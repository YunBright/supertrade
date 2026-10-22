// 本文件锁住「商品条码**只有一处**」这条不变量。
//
// # 不变量
//
//	一个商品的**所有**条码都只存在于 `product_barcodes` 表(1 商品 : N 条码),
//	**主表 `products` 上没有条码字段**。
//
//	这与思迅同款:`t_bd_item_info`(主表)**没有 barcode 列**,
//	条码全在独立表 `t_bd_item_barcode`。
//
// # 为什么要有这条不变量
//
//	旧设计在主表上留了一个 `products.barcode` 冗余列("主条码"),
//	实测是个**双写的僵尸列**:
//
//	- 档案导入不写它 ⇒ 同步来的 27567 个商品这一列**全空**;
//	- 只有手工建档会写它;
//	- 搜索条件读它 ⇒ 对同步来的商品**恒不命中**(死路);
//	- 响应层不用它(`ProductItem.Barcode` 恒等于 id)。
//
//	⇒ 即"同一条条码有两个可能的存放处,其中一个永远是空的"
//	—— 那正是"两处不一致时以本表为准"注释担心的那种不一致。
//
// # 反射断言为什么必要
//
//	删掉 model 字段后,编译器就挡住了"再写它"这件事;
//	但**反射断言**挡住的是"哪天又把它加回来"。
//	⇒ 这条不变量要被**两道锁**守着:编译器 + 这条测试。
package service_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/catalog/model"
	"github.com/YunBright/supertrade/internal/catalog/service"
)

// TestProductModel_HasNoBarcodeColumn 锁"主表**没有**条码字段"。
//
// ⚠️ 这是**反射**断言:编译器只能挡住"现在用",挡不住"哪天又加回来"。
func TestProductModel_HasNoBarcodeColumn(t *testing.T) {
	typ := reflect.TypeOf(model.Product{})
	if f, ok := typ.FieldByName("Barcode"); ok {
		t.Fatalf("主表 model 不该再有 Barcode 字段(现在有:%s)—— "+
			"条码只由 product_barcodes 表表达(1 商品 : N 条码,思迅同款)。"+
			"主表再放一个 ⇒ 同一条条码有两个可能的存放处,而导入不写它 ⇒ 永远是空的",
			f.Type)
	}
}

// TestProductService_Create_BarcodeGoesToSubTable 锁"建档带的条码落进子表"。
func TestProductService_Create_BarcodeGoesToSubTable(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewProductService(db)

	_, err := svc.Create(context.Background(), service.CreateProductInput{
		ID: "P1", BranchID: "10", Name: "酸奶", Barcode: "6908946288185",
		CreatedBy: "admin",
	})
	require.NoError(t, err)

	// 条码在子表里。
	rows, err := service.NewProductBarcodeService(db).ListByProduct(context.Background(), "10", "P1")
	require.NoError(t, err)
	require.Len(t, rows, 1, "建档带的条码应落成子表的一行")
	assert.Equal(t, "6908946288185", rows[0].Barcode)
	assert.Equal(t, "P1", rows[0].ProductID)
}

// TestProductService_Update_BarcodeGoesToSubTable 锁"改档带的条码也落进子表"。
//
// ⚠️ 旧设计是写主表列 ⇒ 与子表**两处不一致**。
//   现在只有子表一处 ⇒ 不可能不一致。
func TestProductService_Update_BarcodeGoesToSubTable(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewProductService(db)

	_, err := svc.Create(context.Background(), service.CreateProductInput{
		ID: "P1", BranchID: "10", Name: "酸奶", CreatedBy: "admin",
	})
	require.NoError(t, err)

	bc := "6908946288185"
	_, err = svc.Update(context.Background(), "10", "P1", service.UpdateProductInput{
		Barcode: &bc, UpdatedBy: "admin",
	})
	require.NoError(t, err)

	rows, err := service.NewProductBarcodeService(db).ListByProduct(context.Background(), "10", "P1")
	require.NoError(t, err)
	require.Len(t, rows, 1, "改档带的条码应落成子表的一行")
	assert.Equal(t, "6908946288185", rows[0].Barcode)
}

// TestProductService_List_KeywordHitsBarcodeInSubTable 锁"按条码能列出商品"。
//
// ⚠️ 这一路以前匹配的是主表的空列 ⇒ **恒不命中**。
func TestProductService_List_KeywordHitsBarcodeInSubTable(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewProductService(db)

	_, err := svc.Create(context.Background(), service.CreateProductInput{
		ID: "P1", BranchID: "10", Name: "酸奶", Barcode: "6908946288185",
		CreatedBy: "admin",
	})
	require.NoError(t, err)

	_, total, err := svc.List(context.Background(), "10", service.LocalProductQuery{
		Keyword: "6908946288185",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total, "按条码应命中 1 条")
}

// TestProductService_FindCandidatesByCode_HitsBarcodeInSubTable 锁"按条码反查商品"。
func TestProductService_FindCandidatesByCode_HitsBarcodeInSubTable(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewProductService(db)

	_, err := svc.Create(context.Background(), service.CreateProductInput{
		ID: "P1", BranchID: "10", Name: "酸奶", Barcode: "6908946288185",
		CreatedBy: "admin",
	})
	require.NoError(t, err)

	rows, err := svc.FindCandidatesByCode(context.Background(), "10", "6908946288185")
	require.NoError(t, err)
	require.Len(t, rows, 1, "按条码反查应命中 1 个货号")
	assert.Equal(t, "P1", rows[0].ID)
}

// TestProductService_BarcodeNotSharedAcrossBranches 锁"条码不跨店"。
func TestProductService_BarcodeNotSharedAcrossBranches(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewProductService(db)

	_, err := svc.Create(context.Background(), service.CreateProductInput{
		ID: "P1", BranchID: "10", Name: "酸奶", Barcode: "6908946288185",
		CreatedBy: "admin",
	})
	require.NoError(t, err)

	rows, err := svc.FindCandidatesByCode(context.Background(), "11", "6908946288185")
	require.NoError(t, err)
	assert.Empty(t, rows, "别的店不该按同一个条码查到货")
}

// compileTimeAssert 保证 db 变量被用到(避免 import 漂移)。
var _ *gorm.DB
