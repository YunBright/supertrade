// 本文件锁住 catalog 读路径的「门店 → 数据源」分流。
//
// # 为什么要有这组用例
//
// 2026-10-21 生产实测:商品选择控件(picker)的 4 条读端点**只走 cube**
// ⇒ 自营店(门店 10,`branch_cube_sources` 里没有它)调它直接返
// `{"code":"cube_error","message":"cubeclient: cube 404 not found"}`,
// 而门店 10 的本地表里明明有 27565 个商品。
//
// ⇒ 三态必须被测出来:
//
//	配了源 ⇒ 走 cube
//	没配源 ⇒ 走本域 PG(**真的返回本地商品**,不是只断言状态码)
//	问不到 ⇒ 503(不是空列表!)
//
// ⚠️ "问不到 ⇒ 空列表"是最坏的一种失败:它把一次链路故障
// 显示成"该门店没有商品",而根因与结论完全无关。
package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/catalog/model"
	"github.com/YunBright/supertrade/internal/catalog/service"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubeclient/cubeclientfake"
	"github.com/YunBright/supertrade/internal/cubesource"
)

// ---- 测试替身 ----

// fakeSource 替身 cubesource.Resolver。
type fakeSource struct {
	src cubesource.Source
	err error
	// calls 记录解析次数;branch 记录被解析的门店。
	calls  int
	branch string
}

func (f *fakeSource) Resolve(_ context.Context, branchID string) (cubesource.Source, error) {
	f.calls++
	f.branch = branchID
	if f.err != nil {
		return cubesource.Source{}, f.err
	}
	return f.src, nil
}

// configuredSource 是"配了数据源"的门店(富百家 / 佳乐那类)。
func configuredSource() cubesource.Source {
	return cubesource.Source{
		BranchID:       "00",
		Configured:     true,
		Enabled:        true,
		CubeSourceName: "cube-sixun-ysx-fb",
	}
}

// localSource 是"没配数据源"的门店(自营店 10)。
func localSource() cubesource.Source {
	return cubesource.Source{BranchID: "10"}
}

// newSourceSvc 构造一个**带数据源解析器 + 本地库**的 picker 服务。
//
// ⚠️ 解析器 + 本地库**两个都装**:只装一个 ⇒ 走哪条路就成了巧合,
// 而不是被测出来的行为。
func newSourceSvc(t *testing.T, src *fakeSource) (*service.CatalogService, *gorm.DB) {
	t.Helper()
	db := newTestDB(t)
	svc := service.NewCatalogService(cubeclientfake.New(), nil)
	svc.SetSourceResolver(src)
	svc.SetLocalDB(db)
	return svc, db
}

// branchCtx 造一个带门店的 ctx。
//
// ⚠️ 门店走 ctx(cubeclient.WithBranchID):picker 的端点由
// requireSingleBranch 写入**已校验过的**单店 ⇒ 本地路径与 cube 路径
// 永远看同一个门店。
func branchCtx(branchID string) context.Context {
	return cubeclient.WithBranchID(context.Background(), branchID)
}

// seedLocalCatalog 灌本域 PG 的门店 10 数据。
func seedLocalCatalog(t *testing.T, db *gorm.DB) {
	t.Helper()
	now := time.Now()
	rows := []any{
		&model.Category{ID: "C1", BranchID: "10", Name: "纸品清洁", Status: "active", CreatedBy: "test", CreatedAt: now, UpdatedAt: now},
		&model.Category{ID: "C1-01", BranchID: "10", Name: "抽纸", ParentID: "C1", Status: "active", CreatedBy: "test", CreatedAt: now, UpdatedAt: now},
		&model.Supplier{ID: "S1", BranchID: "10", Name: "某某商行", Type: "0", Status: "active", CreatedBy: "test", CreatedAt: now, UpdatedAt: now},
		&model.Product{ID: "LOCAL-1", BranchID: "10", Name: "本地抽纸", CategoryID: "C1-01", SupplierID: "S1", SubCode: "SC1", Unit: "提", Status: "1", CreatedBy: "test", CreatedAt: now, UpdatedAt: now},
		&model.Product{ID: "LOCAL-2", BranchID: "10", Name: "本地饮料", CategoryID: "C1", SupplierID: "S1", Status: "1", CreatedBy: "test", CreatedAt: now, UpdatedAt: now},
	}
	for _, r := range rows {
		require.NoError(t, db.Create(r).Error, "seed")
	}
}

// ---- 三态 ----

// TestListProducts_ConfiguredSourceReadsCube 锁"配了数据源 ⇒ 走 cube"。
func TestListProducts_ConfiguredSourceReadsCube(t *testing.T) {
	src := &fakeSource{src: configuredSource()}
	svc, db := newSourceSvc(t, src)
	seedLocalCatalog(t, db)

	page, err := svc.ListProducts(branchCtx("00"), service.ProductListQuery{Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.NotEmpty(t, page.Products)

	ids := map[string]bool{}
	for _, p := range page.Products {
		ids[p.ID] = true
	}
	assert.True(t, ids["P-1001"], "必须来自 cube(fixture 的商品)")
	assert.False(t, ids["LOCAL-1"], "**不该**返回本地商品")
	assert.Equal(t, 1, src.calls, "每次读都要问一次数据源")
	assert.Equal(t, "00", src.branch, "问的必须是当前请求的门店")
}

// TestListProducts_NoSourceReadsLocal 锁"没配数据源 ⇒ 走本域 PG"。
//
// ⚠️ 断言的是**数据来源**(真的返回了本地商品),不是状态码。
func TestListProducts_NoSourceReadsLocal(t *testing.T) {
	src := &fakeSource{src: localSource()}
	svc, db := newSourceSvc(t, src)
	seedLocalCatalog(t, db)

	page, err := svc.ListProducts(branchCtx("10"), service.ProductListQuery{Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.Len(t, page.Products, 2, "必须真的返回本地商品")

	byID := map[string]service.ProductItem{}
	for _, p := range page.Products {
		byID[p.ID] = p
	}
	local, ok := byID["LOCAL-1"]
	require.True(t, ok, "本地商品必须在结果里")
	assert.Equal(t, "本地抽纸", local.Name)
	assert.Equal(t, "LOCAL-1", local.Barcode, "barcode 必须等于 id(与 cube 路径同款)")
	assert.Equal(t, "C1-01", local.CategoryID)
	require.NotNil(t, local.CategoryName, "本地路径也要 join 分类名")
	assert.Equal(t, "抽纸", *local.CategoryName)
	require.NotNil(t, local.SupplierName, "本地路径也要 join 供应商名")
	assert.Equal(t, "某某商行", *local.SupplierName)
	assert.Nil(t, local.PriceYuan, "本域不存价 ⇒ null(未维护 ≠ 0 元)")
	assert.Equal(t, 2, page.Total, "total 是过滤后的总条数")
}

// TestListProducts_ResolveFailureIsNotEmptyList 锁"问不到数据源 ⇒ 报错,不是空列表"。
func TestListProducts_ResolveFailureIsNotEmptyList(t *testing.T) {
	src := &fakeSource{err: errors.New("cube-router 不可达")}
	svc, db := newSourceSvc(t, src)
	seedLocalCatalog(t, db)

	_, err := svc.ListProducts(branchCtx("10"), service.ProductListQuery{Page: 1, PageSize: 50})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCubeSourceUnavailable)
}

// TestListProducts_SourceDisabledIsNotLocalFallback 锁"数据源被停用 ⇒ 报错"。
func TestListProducts_SourceDisabledIsNotLocalFallback(t *testing.T) {
	disabled := configuredSource()
	disabled.Enabled = false
	src := &fakeSource{src: disabled}
	svc, db := newSourceSvc(t, src)
	seedLocalCatalog(t, db)

	_, err := svc.ListProducts(branchCtx("00"), service.ProductListQuery{Page: 1, PageSize: 50})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCubeSourceDisabled)
}

// TestSearchProducts_NoSourceReadsLocal 锁"没配数据源 ⇒ 本地关键字搜索"。
//
// ⚠️ 关键字命中 id / 自编码 / 名称(大小写无关),与 cube 路径同口径。
func TestSearchProducts_NoSourceReadsLocal(t *testing.T) {
	src := &fakeSource{src: localSource()}
	svc, db := newSourceSvc(t, src)
	seedLocalCatalog(t, db)

	page, err := svc.SearchProducts(branchCtx("10"), service.ProductListQuery{
		Keyword: "本地抽", Page: 1, PageSize: 20,
	})
	require.NoError(t, err)
	require.Len(t, page.Products, 1)
	assert.Equal(t, "LOCAL-1", page.Products[0].ID)
	assert.Equal(t, 1, page.Total)

	// 自编码也要能搜到。
	page, err = svc.SearchProducts(branchCtx("10"), service.ProductListQuery{
		Keyword: "sc1", Page: 1, PageSize: 20,
	})
	require.NoError(t, err)
	require.Len(t, page.Products, 1)
	assert.Equal(t, "LOCAL-1", page.Products[0].ID)
}

// TestListProducts_NoSourceFiltersByCategorySubtree 锁"分类过滤按**子树**"。
//
// ⚠️ 与 cube 路径同一套口径("该分类 + 它的所有子孙分类"),
// 否则侧栏 count 与列表 total 会对不上。
func TestListProducts_NoSourceFiltersByCategorySubtree(t *testing.T) {
	src := &fakeSource{src: localSource()}
	svc, db := newSourceSvc(t, src)
	seedLocalCatalog(t, db)

	page, err := svc.ListProducts(branchCtx("10"), service.ProductListQuery{
		CategoryID: "C1", Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, page.Total, "C1 + 子分类 C1-01 的商品都要")
}

// TestListProducts_NoSourceEmptySupplierCategoryHitsNothing 锁"空集合 ⇒ 命中 0 行"。
//
// ⚠️ 本域没有销售方式 ⇒ 按一个**具体**供应商类别过滤必然是空集。
//
//	空集合当成"不过滤" ⇒ 返回**全店商品**(实测门店 00 是 27565 个),
//	而且**全程不报错** —— 用户点一个空类别看到了整个门店。
func TestListProducts_NoSourceEmptySupplierCategoryHitsNothing(t *testing.T) {
	src := &fakeSource{src: localSource()}
	svc, db := newSourceSvc(t, src)
	seedLocalCatalog(t, db)

	page, err := svc.ListProducts(branchCtx("10"), service.ProductListQuery{
		SupplierCategoryID: "F", Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, page.Total, "空集合必须命中 0 行")
	assert.Empty(t, page.Products)

	// 「未分类」组 = 本域全部供应商 ⇒ 有货。
	page, err = svc.ListProducts(branchCtx("10"), service.ProductListQuery{
		SupplierCategoryID: "__unclassified__", Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, page.Total)
}

// TestListCategories_NoSourceReadsLocal 锁"没配数据源 ⇒ 本地分类树"。
func TestListCategories_NoSourceReadsLocal(t *testing.T) {
	src := &fakeSource{src: localSource()}
	svc, db := newSourceSvc(t, src)
	seedLocalCatalog(t, db)

	cats, err := svc.ListCategories(branchCtx("10"), true)
	require.NoError(t, err)
	require.Len(t, cats.Categories, 1)
	root := cats.Categories[0]
	assert.Equal(t, "C1", root.ID)
	assert.Equal(t, "纸品清洁", root.Name)
	require.NotNil(t, root.Count)
	assert.Equal(t, 2, *root.Count, "count = 本级 + 全部子孙")
	require.Len(t, root.Children, 1)
	assert.Equal(t, "C1-01", root.Children[0].ID)
	assert.Equal(t, 2, cats.Total, "total 是全部节点数")
}

// TestListSuppliers_NoSourceReadsLocal 锁"没配数据源 ⇒ 本地供应商摘要"。
func TestListSuppliers_NoSourceReadsLocal(t *testing.T) {
	src := &fakeSource{src: localSource()}
	svc, db := newSourceSvc(t, src)
	seedLocalCatalog(t, db)

	sups, err := svc.ListSuppliers(branchCtx("10"))
	require.NoError(t, err)
	require.Len(t, sups.Suppliers, 1)
	assert.Equal(t, "S1", sups.Suppliers[0].ID)
	assert.Equal(t, "某某商行", sups.Suppliers[0].Name)
	require.NotNil(t, sups.Suppliers[0].Count)
	assert.Equal(t, 2, *sups.Suppliers[0].Count, "该供应商下的商品数")
	assert.Equal(t, 1, sups.Total)
	// 本域没有销售方式 ⇒ 全部进「未分类」组。
	require.Len(t, sups.SupplierCategories, 1)
	assert.Equal(t, "未分类", sups.SupplierCategories[0].Name)
}

// TestGetProduct_NoSourceReadsLocal 锁"没配数据源 ⇒ 本地商品详情"。
func TestGetProduct_NoSourceReadsLocal(t *testing.T) {
	src := &fakeSource{src: localSource()}
	svc, db := newSourceSvc(t, src)
	seedLocalCatalog(t, db)

	p, err := svc.GetProduct(branchCtx("10"), "10", "LOCAL-1")
	require.NoError(t, err)
	assert.Equal(t, "LOCAL-1", p.ID)
	assert.Equal(t, "本地抽纸", p.Name)
	assert.Equal(t, "LOCAL-1", p.Barcode, "barcode 必须等于 id")
	assert.Equal(t, "提", p.Unit)

	_, err = svc.GetProduct(branchCtx("10"), "10", "NOPE")
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrProductNotFound)
}

// ---- 导入闸门 ----

// TestImport_ExternalSourceIsReadOnly 锁"有源门店 ⇒ 导入被拒(只读闸门)"。
//
// ⚠️ 不拒的话:写进本域 PG 的一行"没人看的商品",而前台还显示"写成功了"。
func TestImport_ExternalSourceIsReadOnly(t *testing.T) {
	src := &fakeSource{src: configuredSource()}
	_, db := newSourceSvc(t, src)
	seedLocalCatalog(t, db)
	arc := service.NewArchiveService(db)
	arc.SetSourceResolver(src)

	_, err := arc.Import(context.Background(), "00", service.ImportRequest{
		SourceBranchID: "00",
		Mode:           service.ModeIncremental,
		Data: service.ImportData{
			Products: []service.ImportProductRow{{ID: "X1", Name: "外部商品"}},
		},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrExternalSourceReadOnly)
	assert.NotContains(t, err.Error(), "cube-sixun-ysx-fb",
		"message 里不许暴露内部 app-id")
}

// TestImport_LocalSourceIsAllowed 锁"没配数据源的门店 ⇒ 正常导入"。
func TestImport_LocalSourceIsAllowed(t *testing.T) {
	src := &fakeSource{src: localSource()}
	_, db := newSourceSvc(t, src)
	arc := service.NewArchiveService(db)
	arc.SetSourceResolver(src)

	res, err := arc.Import(context.Background(), "10", service.ImportRequest{
		SourceBranchID: "00",
		Mode:           service.ModeIncremental,
		Data: service.ImportData{
			Products: []service.ImportProductRow{{ID: "X1", Name: "本地商品"}},
		},
	})
	require.NoError(t, err)
	inserted, _, _ := res.Total()
	assert.Equal(t, 1, inserted)
}

// TestImport_ResolveFailureIsAllowed 锁"问不到路由 ⇒ **放行**导入"。
//
// ⚠️ 读端点不能这么干(问不到就回落本地 = 显示一份空列表),
// 而写端点放行是安全的:导入路径自己会兜底,
// 且 cube-router 挂掉时不会挡住自营店(门店 10)的正常导入。
func TestImport_ResolveFailureIsAllowed(t *testing.T) {
	src := &fakeSource{err: errors.New("cube-router 不可达")}
	_, db := newSourceSvc(t, src)
	arc := service.NewArchiveService(db)
	arc.SetSourceResolver(src)

	res, err := arc.Import(context.Background(), "10", service.ImportRequest{
		SourceBranchID: "00",
		Mode:           service.ModeIncremental,
		Data: service.ImportData{
			Products: []service.ImportProductRow{{ID: "X1", Name: "本地商品"}},
		},
	})
	require.NoError(t, err, "问不到路由时不该挡住正常导入")
	inserted, _, _ := res.Total()
	assert.Equal(t, 1, inserted)
}
