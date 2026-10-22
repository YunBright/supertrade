// 本文件锁住「商品查询必须能按**条形码**命中」这条契约。
//
// # 思迅的商品标识体系(四种,唯一性各不相同)
//
//	| 方式     | 字段             | 唯一性            | 落点              |
//	|----------|------------------|-------------------|-------------------|
//	| 商品码   | item_no          | **唯一**          | products.id       |
//	| 助记码   | item_subno       | **不唯一**(1:N)   | products.sub_code |
//	| 条形码   | t_bd_item_barcode | **一品多码**(1:N) | product_barcodes  |
//	| 商品名称 | item_name        | 不唯一            | products.name     |
//
// ⇒ 四种方式**都要**能查到;不唯一的方式要能返回**候选列表**。
//
// # 为什么条形码要单独用子查询
//
//	`products.barcode` 只是"主条码"**冗余列**,生产实测**大面积为空**
//	(门店 10:27568 行全空)—— 领域事实是"思迅 item_no 就是条码"。
//	真正的商品条码在 `t_bd_item_barcode` ⇒ 同步后落在 `product_barcodes` 表。
//
//	⇒ 只在 `products` 表上做 LIKE 是**扫不到条形码**的:
//	  2026-10-22 生产实测,商品 `02000089` 的条形码 `6970367140630`
//	  在 `products/search` 里 count=0 ⇒ **POS 上扫这个码搜不出来**。
//
// # 两条"搜索"路径必须同口径
//
//	`CatalogService.SearchProducts`(POS 走的 ?q= 路径)与
//	`ProductService.List`(域内商品列表)都必须命中四种。
//	⇒ 历史上它们就不一致(一个查条码表、一个只查冗余列),
//	  而**只断言"端点在、字段对"**的契约测试完全看不出来。
package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/catalog/model"
	"github.com/YunBright/supertrade/internal/catalog/service"
)

// seedBarcode 在 product_barcodes 上登记一条条形码。
func seedBarcode(t *testing.T, db *gorm.DB, productID, branchID, barcode string) {
	t.Helper()
	_, err := service.NewProductBarcodeService(db).Upsert(context.Background(), service.BarcodeInput{
		ProductID: productID, BranchID: branchID, Barcode: barcode, Label: "测试口味",
	})
	require.NoError(t, err)
}

// TestSearchProducts_LocalPath_KeywordHitsBarcodeTable 锁"**POS 走的那条路径**按条形码能命中"。
//
// ⚠️ 走的是**没配数据源**的门店(10)⇒ 落 `localProductPage`(本域 PG)。
func TestSearchProducts_LocalPath_KeywordHitsBarcodeTable(t *testing.T) {
	src := &fakeSource{src: localSource()}
	svc, db := newSourceSvc(t, src)
	seedLocalCatalog(t, db)
	// 条形码在**另一张表**上;products.barcode 留空(生产实况)。
	seedBarcode(t, db, "LOCAL-1", "10", "6970367140630")

	// 商品码。
	byID, err := svc.SearchProducts(branchCtx("10"), service.ProductListQuery{
		Keyword: "LOCAL-1", Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, byID.Total, "按商品码应命中 1 条")

	// 助记码。
	bySub, err := svc.SearchProducts(branchCtx("10"), service.ProductListQuery{
		Keyword: "SC1", Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, bySub.Total, "按助记码应命中 1 条")

	// 名称。
	byName, err := svc.SearchProducts(branchCtx("10"), service.ProductListQuery{
		Keyword: "本地抽纸", Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, byName.Total, "按名称应命中 1 条")

	// **条形码** —— 修复前这里是 0。
	byBarcode, err := svc.SearchProducts(branchCtx("10"), service.ProductListQuery{
		Keyword: "6970367140630", Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	require.Equal(t, 1, byBarcode.Total,
		"按条形码应命中 1 条(修复前是 0 ⇒ POS 上扫这个码搜不出来)")
	require.Len(t, byBarcode.Products, 1)
	assert.Equal(t, "LOCAL-1", byBarcode.Products[0].ID)
}

// TestSearchProducts_LocalPath_BarcodeKeywordStillMatchesOtherFields 锁
// "加了条形码子查询**没有**破坏原来的三种命中"。
//
// ⚠️ 子查询是用 **OR** 拼进同一条 Where 的 ——
//   如果写成两条独立的 Where,会变成 AND(货号命中 **且** 条形码命中)⇒ 全查不到。
func TestSearchProducts_LocalPath_BarcodeKeywordStillMatchesOtherFields(t *testing.T) {
	src := &fakeSource{src: localSource()}
	svc, db := newSourceSvc(t, src)
	seedLocalCatalog(t, db)
	seedBarcode(t, db, "LOCAL-1", "10", "6970367140630")

	for _, kw := range []string{"LOCAL-1", "SC1", "本地抽纸", "6970367140630"} {
		page, err := svc.SearchProducts(branchCtx("10"), service.ProductListQuery{
			Keyword: kw, Page: 1, PageSize: 50,
		})
		require.NoError(t, err)
		assert.Equal(t, 1, page.Total, "关键词 %q 应命中 1 条", kw)
	}
}

// TestSearchProducts_LocalPath_BarcodeIsBranchScoped 锁"条形码反查**不跨店**"。
func TestSearchProducts_LocalPath_BarcodeIsBranchScoped(t *testing.T) {
	src := &fakeSource{src: localSource()}
	svc, db := newSourceSvc(t, src)
	seedLocalCatalog(t, db)
	seedBarcode(t, db, "LOCAL-1", "10", "6970367140630")
	// 别的店也有一条**同条码**的商品(不该被门店 10 的查询捞到)。
	require.NoError(t, db.Create(&model.Product{
		ID: "OTHER-1", BranchID: "11", Name: "别店商品", Status: "1",
		CreatedBy: "test",
	}).Error)
	seedBarcode(t, db, "OTHER-1", "11", "6970367140630")

	page, err := svc.SearchProducts(branchCtx("10"), service.ProductListQuery{
		Keyword: "6970367140630", Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	require.Len(t, page.Products, 1, "只能捞到本店的商品")
	assert.Equal(t, "LOCAL-1", page.Products[0].ID)
}

// TestProductService_List_KeywordHitsBarcodeTable 锁"**域内商品列表**也按条形码命中"。
//
// ⚠️ 它原来只匹配 `products.barcode`(=主条码冗余列,生产大面积为空)
//   ⇒ "四种都命中"在生产上是**空转**的(第 4 种永远不生效)。
func TestProductService_List_KeywordHitsBarcodeTable(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewProductService(db)
	_, err := svc.Create(context.Background(), service.CreateProductInput{
		ID: "P1", BranchID: "10", Name: "酸奶", SubCode: "6908946288183",
		CreatedBy: "admin",
	})
	require.NoError(t, err)
	seedBarcode(t, db, "P1", "10", "6908946288185")

	for _, kw := range []string{"P1", "6908946288183", "酸奶", "6908946288185"} {
		_, total, err := svc.List(context.Background(), "10", service.LocalProductQuery{Keyword: kw})
		require.NoError(t, err)
		assert.Equal(t, int64(1), total, "关键词 %q 应命中", kw)
	}
}
