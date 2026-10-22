// 本文件锁住「商品档案(图1)」读路径的「门店 → 数据源」分流与响应形状。
//
// # 为什么要有这组用例
//
// PLAN §0 把这条纪律写成全域规则:
//
//	配了源 ⇒ 走 cube;没配源 ⇒ 走本域 PG;问不到 ⇒ **503**,不回落本地。
//
// ⚠️ "问不到 ⇒ 空列表"是最坏的一种失败:它把一次链路故障显示成
// "这个门店没有商品",而根因与结论完全无关(2026-10-21 生产事故的形状)。
//
// # ⚠️ "两条路径的响应结构必须逐字相同"
//
// 本文件用**同一组断言**跑两条路径:不是"各测各的",而是
// 把两条路径产出的 item 序列化后**比较键集合**——
// 少一个字段(比如本地路径少了 member_price_yuan)就会红。
package service_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/YunBright/supertrade/internal/catalog/model"
	"github.com/YunBright/supertrade/internal/catalog/service"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubeclient/cubeclientfake"
	"github.com/YunBright/supertrade/internal/cuberead/cubereadfake"
)

// ---- 测试替身 ----
//
// ⚠️ fakeSource / configuredSource / localSource / branchCtx **复用**
// source_test.go 里已有的那几份(同一个包 service_test)。
// 再定义一份就会出现"两个同名替身,后来者用错了还以为测的是另一个"。

// newArchiveDB 构造**文件型** SQLite(同 internal/pos/handler 的做法)。
func newArchiveDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "catalog.db")
	dialector := sqlite.Dialector{DriverName: "sqlite", DSN: dsn}
	db, err := gorm.Open(dialector, &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.Supplier{}, &model.Product{}, &model.Category{},
		&model.ProductBranchPrice{}, &model.ProductSupplierPrice{},
	), "auto migrate")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// seedLocalArchive 灌本域 PG 的门店 10 数据。
func seedLocalArchive(t *testing.T, db *gorm.DB) {
	t.Helper()
	now := time.Now()
	rows := []any{
		&model.Category{ID: "C1", BranchID: "10", Name: "纸品清洁", Status: "active", CreatedBy: "t", CreatedAt: now, UpdatedAt: now},
		&model.Category{ID: "C1-01", BranchID: "10", Name: "抽纸", ParentID: "C1", Status: "active", CreatedBy: "t", CreatedAt: now, UpdatedAt: now},
		&model.Supplier{ID: "S1", BranchID: "10", Name: "某某商行", Type: "0", Status: "active", CreatedBy: "t", CreatedAt: now, UpdatedAt: now},
		&model.Product{ID: "LOCAL-1", BranchID: "10", Name: "本地抽纸", CategoryID: "C1-01", SupplierID: "S1", SubCode: "SC1", Unit: "提", Status: "active", CreatedBy: "t", CreatedAt: now, UpdatedAt: now},
		&model.Product{ID: "LOCAL-2", BranchID: "10", Name: "本地饮料", CategoryID: "C1", SupplierID: "S1", Status: "active", CreatedBy: "t", CreatedAt: now, UpdatedAt: now},
	}
	for _, r := range rows {
		require.NoError(t, db.Create(r).Error, "seed")
	}
}

// cubeProductRows 是 cube `product` model 的 canned 行。
func cubeProductRows() []map[string]any {
	return []map[string]any{
		{
			"product.id":           "00001",
			"product.name":         "小白菜",
			"product.sub_code":     "00001",
			"product.category_id":  "010201",
			"product.supplier_id":  "019",
			"product.unit":         "kg",
			"product.spec":         "500g",
			"product.status":       "1",
			"product.brand_name":   "其它品牌",
		},
	}
}

// newArchiveSvc 构造一个**带数据源解析器 + 本地库 + cube 读端**的服务。
//
// ⚠️ 三个都装:只装一个 ⇒ 走哪条路就成了巧合,而不是被测出来的行为。
func newArchiveSvc(t *testing.T, src *fakeSource) (*service.CatalogService, *gorm.DB, *cubereadfake.Client) {
	t.Helper()
	db := newArchiveDB(t)
	reader := cubereadfake.New()
	reader.Handler = func(model string, q cubeclient.CubeQuery) ([]map[string]any, int, error) {
		switch model {
		case "product":
			rows := cubeProductRows()
			return rows, len(rows), nil
		case "branch_price":
			return []map[string]any{
				{
					"branch_price.item_no":            "00001",
					"branch_price.cost_yuan":           "4.96",
					"branch_price.wholesale_yuan":      "3.60",
					"branch_price.retail_yuan":         "4.76",
					"branch_price.member_price_yuan":   nil,
				},
			}, 1, nil
		case "supplier_item":
			return []map[string]any{
				{
					"supplier_item.item_no":               "00001",
					"supplier_item.branch_no":             "00",
					"supplier_item.supplier_id":           "019",
					"supplier_item.sale_way":              "A",
					"supplier_item.appointed_price_yuan":  "0",
					"supplier_item.top_price_yuan":        "8.00",
					"supplier_item.bottom_price_yuan":     "1.40",
					"supplier_item.last_price_yuan":       "4.76",
					"supplier_item.contract_date":         "2026-10-24",
				},
			}, 1, nil
		}
		return nil, 0, nil
	}
	svc := service.NewCatalogService(cubeclientfake.New(), nil)
	svc.SetSourceResolver(src)
	svc.SetLocalDB(db)
	svc.SetCubeReader(reader)
	return svc, db, reader
}

// ---- 三态 ----

// TestListProductArchive_ConfiguredSourceReadsCube 锁"配了数据源 ⇒ 走 cube"。
func TestListProductArchive_ConfiguredSourceReadsCube(t *testing.T) {
	src := &fakeSource{src: configuredSource()}
	svc, db, reader := newArchiveSvc(t, src)
	seedLocalArchive(t, db)

	page, err := svc.ListProductArchive(branchCtx("00"), service.ProductArchiveQuery{Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	assert.Equal(t, "00001", page.Items[0].ID, "必须来自 cube")
	assert.Equal(t, "cube", page.DataSource)
	assert.Equal(t, 1, src.calls, "每次读都要问一次数据源")
	assert.Equal(t, "00", src.branch, "问的必须是当前请求的门店")
	q, ok := reader.LastQuery("product")
	require.True(t, ok)
	// 没给关键字 ⇒ 没有关键字过滤(但查询本身必须真的发出去)。
	assert.False(t, q.HasFilter("product.id"))
	assert.Contains(t, q.Query.Dimensions, "product.brand_name",
		"商品档案页必须带上品牌维度")
}

// TestListProductArchive_NoSourceReadsLocal 锁"没配数据源 ⇒ 走本域 PG"。
//
// ⚠️ 断言的是**数据来源**(真的返回了本地商品),不是状态码。
func TestListProductArchive_NoSourceReadsLocal(t *testing.T) {
	src := &fakeSource{src: localSource()}
	svc, db, reader := newArchiveSvc(t, src)
	seedLocalArchive(t, db)

	page, err := svc.ListProductArchive(branchCtx("10"), service.ProductArchiveQuery{Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	assert.Equal(t, "local", page.DataSource)
	assert.Empty(t, reader.Queries(), "本地路径**不该**发 cube 查询")

	byID := map[string]service.ProductArchiveItem{}
	for _, it := range page.Items {
		byID[it.ID] = it
	}
	local, ok := byID["LOCAL-1"]
	require.True(t, ok, "本地商品必须在结果里")
	assert.Equal(t, "本地抽纸", local.Name)
	require.NotNil(t, local.CategoryName)
	assert.Equal(t, "抽纸", *local.CategoryName)
	require.NotNil(t, local.SupplierName)
	assert.Equal(t, "某某商行", *local.SupplierName)
	// ⚠️ 本域不存价 ⇒ null(未维护 ≠ 0 元)。
	assert.Nil(t, local.CostYuan)
	assert.Nil(t, local.RetailYuan)
	assert.Nil(t, local.MemberPriceYuan)
	// ⚠️ 本域没有品牌列 ⇒ null。
	assert.Nil(t, local.BrandName)
	assert.Equal(t, 2, page.Total)
}

// TestListProductArchive_ResolveFailureIsNotEmptyList 锁"问不到 ⇒ 报错,不是空列表"。
func TestListProductArchive_ResolveFailureIsNotEmptyList(t *testing.T) {
	src := &fakeSource{err: errors.New("cube-router 不可达")}
	svc, db, _ := newArchiveSvc(t, src)
	seedLocalArchive(t, db)

	_, err := svc.ListProductArchive(branchCtx("10"), service.ProductArchiveQuery{Page: 1, PageSize: 50})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCubeSourceUnavailable)
}

// TestListProductArchive_SourceDisabledIsNotLocalFallback 锁"数据源被停用 ⇒ 报错"。
func TestListProductArchive_SourceDisabledIsNotLocalFallback(t *testing.T) {
	disabled := configuredSource()
	disabled.Enabled = false
	src := &fakeSource{src: disabled}
	svc, db, _ := newArchiveSvc(t, src)
	seedLocalArchive(t, db)

	_, err := svc.ListProductArchive(branchCtx("00"), service.ProductArchiveQuery{Page: 1, PageSize: 50})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCubeSourceDisabled)
}

// ---- 两条路径的响应结构必须逐字相同 ----

// TestProductArchive_LocalAndCubeHaveSameShape 锁"本地路径与 cube 路径结构相同"。
//
// ⚠️ 做法:把两条路径产出的第一条 item 各自序列化成 map,比较**键集合**。
// 少一个字段(比如本地路径少了 member_price_yuan)就会红。
func TestProductArchive_LocalAndCubeHaveSameShape(t *testing.T) {
	// cube 路径
	cubeSvc, _, _ := newArchiveSvc(t, &fakeSource{src: configuredSource()})
	cubePage, err := cubeSvc.ListProductArchive(branchCtx("00"), service.ProductArchiveQuery{Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.Len(t, cubePage.Items, 1)

	// 本地路径
	localSvc, db, _ := newArchiveSvc(t, &fakeSource{src: localSource()})
	seedLocalArchive(t, db)
	localPage, err := localSvc.ListProductArchive(branchCtx("10"), service.ProductArchiveQuery{Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.NotEmpty(t, localPage.Items)

	cubeKeys := keysOf(t, cubePage.Items[0])
	localKeys := keysOf(t, localPage.Items[0])
	assert.Equal(t, cubeKeys, localKeys, "两条路径的 item 字段集合必须一致")

	// 信封也一样。
	assert.Equal(t, keysOf(t, *cubePage), keysOf(t, *localPage), "信封字段集合必须一致")
}

// keysOf 把任意结构体序列化后取键集合(排序)。
func keysOf(t *testing.T, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return keys
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ---- 过滤真的生效 ----

// TestListProductArchive_LocalFilterNarrowsResult 锁"过滤真的收窄结果"。
//
// ⚠️ 只断言"200 + 有字段"**抓不到**一个被静默忽略的过滤器 ——
// 这里断言命中数从 2 变成 1。
func TestListProductArchive_LocalFilterNarrowsResult(t *testing.T) {
	src := &fakeSource{src: localSource()}
	svc, db, _ := newArchiveSvc(t, src)
	seedLocalArchive(t, db)

	all, err := svc.ListProductArchive(branchCtx("10"), service.ProductArchiveQuery{Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.Equal(t, 2, all.Total)

	// 关键字过滤。
	kw, err := svc.ListProductArchive(branchCtx("10"), service.ProductArchiveQuery{
		Keyword: "本地抽", Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, kw.Total, "关键字必须真的收窄命中数")
	assert.Equal(t, "LOCAL-1", kw.Items[0].ID)

	// 分类过滤走**子树**(C1 + 它的子孙 C1-01)。
	cat, err := svc.ListProductArchive(branchCtx("10"), service.ProductArchiveQuery{
		CategoryID: "C1", Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, cat.Total, "C1 + 子分类 C1-01 的商品都要")

	// 供应商过滤。
	sup, err := svc.ListProductArchive(branchCtx("10"), service.ProductArchiveQuery{
		SupplierID: "S1", Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, sup.Total)

	// 不存在的分类 ⇒ 命中 0 行(**不是**"不过滤")。
	none, err := svc.ListProductArchive(branchCtx("10"), service.ProductArchiveQuery{
		CategoryID: "NOPE", Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, none.Total)
}

// TestListProductArchive_CubeQueryCarriesFilters 锁"cube 查询真的带了过滤条件"。
//
// ⚠️ cube 侧不返回"过滤后的条数"给这个替身 ⇒ 只能断言"过滤进了 query"。
// 这是 cube 集成层唯一能证明"过滤器没被吞掉"的地方。
func TestListProductArchive_CubeQueryCarriesFilters(t *testing.T) {
	src := &fakeSource{src: configuredSource()}
	svc, _, reader := newArchiveSvc(t, src)

	_, err := svc.ListProductArchive(branchCtx("00"), service.ProductArchiveQuery{
		Keyword: "白菜", SupplierID: "019", Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	q, ok := reader.LastQuery("product")
	require.True(t, ok)
	assert.True(t, q.HasFilter("product.id"), "关键字过滤必须进 query")
	assert.True(t, q.HasFilter("product.name"), "名称过滤必须进 query")
	assert.True(t, q.HasFilter("product.supplier_id"), "供应商过滤必须进 query")
	assert.Equal(t, []any{"019"}, q.FilterValues("product.supplier_id"))
}

// ---- 供货信息 ----

// TestListSupplierPrices_ThreeStates 锁"供货信息"的三态。
func TestListSupplierPrices_ThreeStates(t *testing.T) {
	// 配了源 ⇒ cube
	cubeSvc, _, _ := newArchiveSvc(t, &fakeSource{src: configuredSource()})
	list, err := cubeSvc.ListSupplierPrices(branchCtx("00"), "00001")
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	assert.Equal(t, "cube", list.DataSource)
	assert.Equal(t, "8.0000", *list.Items[0].TopPriceYuan)
	// ⚠️ 约定价源库给了 0 ⇒ 是 0.0000(不是 null)。
	require.NotNil(t, list.Items[0].AppointedPriceYuan)
	assert.Equal(t, "0.0000", *list.Items[0].AppointedPriceYuan)

	// 没配源 ⇒ 本地(本轮无写端点 ⇒ 诚实的空)
	localSvc, db, _ := newArchiveSvc(t, &fakeSource{src: localSource()})
	seedLocalArchive(t, db)
	localList, err := localSvc.ListSupplierPrices(branchCtx("10"), "LOCAL-1")
	require.NoError(t, err)
	assert.Equal(t, "local", localList.DataSource)
	assert.Empty(t, localList.Items)

	// 问不到 ⇒ 503
	failSvc, _, _ := newArchiveSvc(t, &fakeSource{err: errors.New("cube-router 不可达")})
	_, err = failSvc.ListSupplierPrices(branchCtx("10"), "LOCAL-1")
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCubeSourceUnavailable)
}
