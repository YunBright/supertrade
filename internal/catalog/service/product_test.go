package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/catalog/model"
	"github.com/YunBright/supertrade/internal/catalog/service"
)

func TestProductService_CreateAndGet(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewProductService(db)

	row, err := svc.Create(context.Background(), service.CreateProductInput{
		ID: "6957583900828", BranchID: "10", Name: "凤派XL码抽纸8包",
		CategoryID: "0102", SupplierID: "SUP-001", Barcode: "6957583900828",
		Unit: "提", Spec: "1*8*10", CreatedBy: "admin",
	})
	require.NoError(t, err)
	assert.Equal(t, "active", row.Status, "默认 status=active")
	assert.Equal(t, "6957583900828", row.ID)
	assert.Equal(t, "10", row.BranchID)

	got, err := svc.Get(context.Background(), "10", "6957583900828")
	require.NoError(t, err)
	assert.Equal(t, "凤派XL码抽纸8包", got.Name)
	assert.Equal(t, "提", got.Unit)
}

func TestProductService_Create_RequiresIDAndName(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewProductService(db)

	// 少 id / 少 name / 空名字(带空格)都要被拒。
	for _, in := range []service.CreateProductInput{
		{BranchID: "10", Name: "X"},
		{ID: "P1", Name: "X"},
		{ID: "P1", BranchID: "10", Name: "   "},
	} {
		_, err := svc.Create(context.Background(), in)
		require.Error(t, err)
		assert.True(t, errors.Is(err, model.ErrInvalidInput), "应返 ErrInvalidInput,实际 %v", err)
	}
}

// TestProductService_List_StableOrderAcrossPages 锁"分页排序有**稳定 tiebreaker**"。
//
// ⚠️ 客户端是**分页拉全量**(27565 商品 = 138 页)⇒ 排序必须是**全序**。
//
//	只按 name 排序时,同名商品之间的先后由 DB 决定 ⇒ 不稳定
//	⇒ 两次拉同一份数据可能得到不同顺序 ⇒ 客户端会**重复或跳过**某些行,
//	   而且**零报错** —— 症状是"本地商品库少了几个商品",极难查。
//
// # ⚠️ 为什么**倒序**插入
//
//	测试必须能在"少了 id tiebreaker"时**真的红**。
//	若正序插入,SQLite 的默认顺序(插入序/rowid)恰好就是升序 id
//	⇒ 去掉 tiebreaker 也不会红 ⇒ 测试形同虚设。
//	倒序插入后:插入序 = P5,P4,P3,P2,P1,而正确结果必须是 P1..P5。
func TestProductService_List_StableOrderAcrossPages(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewProductService(db)
	ctx := context.Background()

	// 造 5 个**同名**商品,并且**倒序**插入(见上面的说明)。
	for _, id := range []string{"P5", "P4", "P3", "P2", "P1"} {
		_, err := svc.Create(ctx, service.CreateProductInput{
			ID: id, BranchID: "10", Name: "同名商品", CreatedBy: "admin",
		})
		require.NoError(t, err)
	}

	// 一次取全量 ⇒ 同名的必须按 **id 升序**(即排序键是 (name, id))。
	rows, total, err := svc.List(ctx, "10", service.LocalProductQuery{PageSize: 10})
	require.NoError(t, err)
	require.Equal(t, int64(5), total)
	var ids []string
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	assert.Equal(t, []string{"P1", "P2", "P3", "P4", "P5"}, ids,
		"同名商品必须按 id 升序(排序键要有稳定 tiebreaker)")

	// 分两页拉 ⇒ 收集到的 id 必须**不重不漏**。
	seen := map[string]int{}
	for page := 1; page <= 3; page++ {
		rows, _, err := svc.List(ctx, "10", service.LocalProductQuery{Page: page, PageSize: 2})
		require.NoError(t, err)
		for _, r := range rows {
			seen[r.ID]++
		}
	}
	assert.Len(t, seen, 5, "翻页必须不重不漏,实际拿到 %v", seen)
	for id, n := range seen {
		assert.Equal(t, 1, n, "商品 %s 出现了 %d 次(分页顺序不稳定)", id, n)
	}
}

func TestProductService_Create_DuplicateFails(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewProductService(db)

	_, err := svc.Create(context.Background(), service.CreateProductInput{
		ID: "P1", BranchID: "10", Name: "A", CreatedBy: "admin",
	})
	require.NoError(t, err)

	_, err = svc.Create(context.Background(), service.CreateProductInput{
		ID: "P1", BranchID: "10", Name: "B", CreatedBy: "admin",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrProductAlreadyExists))
}

func TestProductService_Create_SameID_DifferentBranch_OK(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewProductService(db)

	_, err := svc.Create(context.Background(), service.CreateProductInput{
		ID: "P1", BranchID: "10", Name: "门店10商品", CreatedBy: "admin",
	})
	require.NoError(t, err)

	_, err = svc.Create(context.Background(), service.CreateProductInput{
		ID: "P1", BranchID: "11", Name: "门店11商品", CreatedBy: "admin",
	})
	require.NoError(t, err, "同 id 不同门店应各自独立")

	// 读的时候必须按门店隔离:拿 10 查不到 11 那条。
	got, err := svc.Get(context.Background(), "10", "P1")
	require.NoError(t, err)
	assert.Equal(t, "门店10商品", got.Name)
}

func TestProductService_List_FiltersAndPaginates(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewProductService(db)

	seed := []service.CreateProductInput{
		{ID: "P1", BranchID: "10", Name: "抽纸", Barcode: "6957583900828", CategoryID: "C1", SupplierID: "S1", CreatedBy: "admin"},
		{ID: "P2", BranchID: "10", Name: "牛奶", Barcode: "6907992103686", CategoryID: "C2", SupplierID: "S1", CreatedBy: "admin"},
		{ID: "P3", BranchID: "10", Name: "面包", Barcode: "6900067311092", CategoryID: "C1", SupplierID: "S2", CreatedBy: "admin"},
		{ID: "P9", BranchID: "99", Name: "别家店的", Barcode: "9999", CreatedBy: "admin"},
	}
	for _, in := range seed {
		_, err := svc.Create(context.Background(), in)
		require.NoError(t, err)
	}

	// 关键词命中名称。
	rows, total, err := svc.List(context.Background(), "10", service.LocalProductQuery{Keyword: "抽纸"})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	assert.Equal(t, "P1", rows[0].ID)

	// 关键词命中条码(扫条码是最高频入口)。
	rows, total, err = svc.List(context.Background(), "10", service.LocalProductQuery{Keyword: "6907992103686"})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	assert.Equal(t, "P2", rows[0].ID)

	// 分类过滤。
	rows, total, err = svc.List(context.Background(), "10", service.LocalProductQuery{CategoryID: "C1"})
	require.NoError(t, err)
	assert.Equal(t, int64(2), total)
	assert.Len(t, rows, 2)

	// 供应商过滤。
	rows, total, err = svc.List(context.Background(), "10", service.LocalProductQuery{SupplierID: "S2"})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	assert.Equal(t, "P3", rows[0].ID)

	// 门店隔离:别家店的那条不能出现。
	rows, total, err = svc.List(context.Background(), "10", service.LocalProductQuery{})
	require.NoError(t, err)
	assert.Equal(t, int64(3), total, "不能把别的门店的商品算进来")
	assert.Len(t, rows, 3)

	// 分页:第 2 页每页 2 条 → 剩 1 条,total 仍是 3。
	rows, total, err = svc.List(context.Background(), "10", service.LocalProductQuery{Page: 2, PageSize: 2})
	require.NoError(t, err)
	assert.Equal(t, int64(3), total, "total 是过滤后的总数,不是本页条数")
	assert.Len(t, rows, 1)
}

// 自编码 → 候选商品(1 个自编码对应 2 个货号,思迅实测的「正常品 + 换购品」配对)。
func TestProductService_FindCandidatesByCode_SubCodeHitsTwoProducts(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewProductService(db)

	// 照思迅实测:6904406383122 → 02000110(正常品) / 02000111(换购品)。
	for _, in := range []service.CreateProductInput{
		{ID: "02000110", BranchID: "10", Name: "远航九江双蒸酒精品500ml", SubCode: "6904406383122", CreatedBy: "import"},
		{ID: "02000111", BranchID: "10", Name: "(换购)远航九江双蒸酒精品500ml", SubCode: "6904406383122", IsExchange: true, CreatedBy: "import"},
	} {
		_, err := svc.Create(context.Background(), in)
		require.NoError(t, err)
	}

	// 扫自编码 ⇒ **两个候选**。
	rows, err := svc.FindCandidatesByCode(context.Background(), "10", "6904406383122")
	require.NoError(t, err)
	require.Len(t, rows, 2, "一个自编码可能对应多个商品(正常品 + 换购品)")

	// 扫货号 ⇒ 只有一个。
	rows, err = svc.FindCandidatesByCode(context.Background(), "10", "02000111")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "02000111", rows[0].ID)
	assert.True(t, rows[0].IsExchange, "换购标记必须解析成字段,不能靠品名匹配")
}

// 换购品 / 停用品 默认**不返回**(混在普通商品里会让收银员扫到"看不见价格"的怪商品)。
func TestProductService_List_HidesSpecialByDefault(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewProductService(db)

	for _, in := range []service.CreateProductInput{
		{ID: "P1", BranchID: "10", Name: "普通品", CreatedBy: "admin"},
		{ID: "P2", BranchID: "10", Name: "(换购)某品", IsExchange: true, CreatedBy: "import"},
		{ID: "P3", BranchID: "10", Name: "（停用）某品", IsVoid: true, CreatedBy: "import"},
	} {
		_, err := svc.Create(context.Background(), in)
		require.NoError(t, err)
	}

	_, total, err := svc.List(context.Background(), "10", service.LocalProductQuery{})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total, "默认只返普通品")

	// 显式打开才带上。
	_, total, err = svc.List(context.Background(), "10", service.LocalProductQuery{IncludeSpecial: true})
	require.NoError(t, err)
	assert.Equal(t, int64(3), total)
}

// 关键词命中四样:货号 / 自编码 / 名称 / 条码(思迅搜索框「输入编码、名称或助记码」)。
func TestProductService_List_KeywordHitsAllFour(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewProductService(db)

	_, err := svc.Create(context.Background(), service.CreateProductInput{
		ID: "P1", BranchID: "10", Name: "抽纸", SubCode: "6904406383122",
		Barcode: "6957583900828", CreatedBy: "admin",
	})
	require.NoError(t, err)

	for _, kw := range []string{"P1", "6904406383122", "抽纸", "6957583900828"} {
		_, total, err := svc.List(context.Background(), "10", service.LocalProductQuery{Keyword: kw})
		require.NoError(t, err)
		assert.Equal(t, int64(1), total, "关键词 %q 应命中", kw)
	}
}

// 条码反查:条码在 product_barcodes 表上(1 商品 : N 条码)⇒ 也要能查到。
func TestProductService_FindCandidatesByCode_ThroughBarcodeTable(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewProductService(db)
	bars := service.NewProductBarcodeService(db)

	_, err := svc.Create(context.Background(), service.CreateProductInput{
		ID: "P1", BranchID: "10", Name: "酸奶", CreatedBy: "admin",
	})
	require.NoError(t, err)
	_, err = bars.Upsert(context.Background(), service.BarcodeInput{
		ProductID: "P1", BranchID: "10", Barcode: "6908946288185", Label: "桔子味",
	})
	require.NoError(t, err)

	rows, err := svc.FindCandidatesByCode(context.Background(), "10", "6908946288185")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "P1", rows[0].ID)

	// 别的门店查不到。
	rows, err = svc.FindCandidatesByCode(context.Background(), "11", "6908946288185")
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestProductService_Update_Partial(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewProductService(db)

	_, err := svc.Create(context.Background(), service.CreateProductInput{
		ID: "P1", BranchID: "10", Name: "旧名", Unit: "提", CreatedBy: "admin",
	})
	require.NoError(t, err)

	newName := "新名"
	got, err := svc.Update(context.Background(), "10", "P1", service.UpdateProductInput{
		Name: &newName, UpdatedBy: "editor",
	})
	require.NoError(t, err)
	assert.Equal(t, "新名", got.Name)
	assert.Equal(t, "提", got.Unit, "没传的字段不能被清空")
	assert.Equal(t, "editor", got.UpdatedBy)

	// 改不存在的 → ErrProductNotFound。
	_, err = svc.Update(context.Background(), "10", "NOPE", service.UpdateProductInput{Name: &newName})
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrProductNotFound))
}

func TestProductService_Delete_SoftDeletesAndHides(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewProductService(db)

	_, err := svc.Create(context.Background(), service.CreateProductInput{
		ID: "P1", BranchID: "10", Name: "抽纸", CreatedBy: "admin",
	})
	require.NoError(t, err)

	require.NoError(t, svc.Delete(context.Background(), "10", "P1"))

	// 软删之后查不到,而且**列表里也不该出现**。
	_, err = svc.Get(context.Background(), "10", "P1")
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrProductNotFound))

	rows, total, err := svc.List(context.Background(), "10", service.LocalProductQuery{})
	require.NoError(t, err)
	assert.Equal(t, int64(0), total, "软删的行不能出现在列表里")
	assert.Empty(t, rows)

	// 删不存在的 → ErrProductNotFound。
	err = svc.Delete(context.Background(), "10", "P1")
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrProductNotFound))
}
