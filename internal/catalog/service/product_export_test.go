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

// TestProductService_ExportAll_KeepsSpecialAndBranchScope 锁"ExportAll 的三条口径"。
//
// ⚠️ 离线商品库缺换购品/停用品是**已知缺口**(见 service/product.go 的 List 注释)
//
//	⇒ 导出必须给全。
//
// ⚠️ 门店维度必须隔离 —— 导出是"给一家店的离线包",
//
//	混进别家店 = 数据越权。
func TestProductService_ExportAll_KeepsSpecialAndBranchScope(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewProductService(db)
	ctx := context.Background()

	for _, in := range []service.CreateProductInput{
		{ID: "P-NORMAL", BranchID: "10", Name: "普通品"},
		{ID: "P-EXCHANGE", BranchID: "10", Name: "换购品", IsExchange: true},
		{ID: "P-VOID", BranchID: "10", Name: "停用品", IsVoid: true},
		{ID: "P-OTHER", BranchID: "01", Name: "别家店的品"},
	} {
		_, err := svc.Create(ctx, in)
		require.NoError(t, err)
	}

	rows, err := svc.ExportAll(ctx, "10")
	require.NoError(t, err)
	assert.Len(t, rows, 3, "换购品/停用品都要给,且不能带别家店的商品")

	// 排序是 (name, id) 全序 ⇒ 同一份数据两次导出结果一致。
	again, err := svc.ExportAll(ctx, "10")
	require.NoError(t, err)
	for i := range rows {
		assert.Equal(t, rows[i].ID, again[i].ID, "排序必须稳定")
	}
}

// TestProductService_ExportAll_RequiresBranch 锁"空门店 ⇒ ErrInvalidInput"。
func TestProductService_ExportAll_RequiresBranch(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewProductService(db)

	_, err := svc.ExportAll(context.Background(), "")
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrInvalidInput), "应返 ErrInvalidInput,实际 %v", err)
}

// TestProductBarcodeService_ListByBranch_GroupsByProduct 锁"按商品归组 + 门店隔离"。
//
// ⚠️ 一个商品 1~7 条码 ⇒ 客户端要一张完整的"码 → 商品"索引。
func TestProductBarcodeService_ListByBranch_GroupsByProduct(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewProductBarcodeService(db)
	ctx := context.Background()

	for _, in := range []service.BarcodeInput{
		{ProductID: "P1", BranchID: "10", Barcode: "6908946288183", IsPrimary: true},
		{ProductID: "P1", BranchID: "10", Barcode: "6908946288185"},
		{ProductID: "P2", BranchID: "10", Barcode: "6957583900828"},
		{ProductID: "P1", BranchID: "01", Barcode: "0000000000000"},
	} {
		_, err := svc.Upsert(ctx, in)
		require.NoError(t, err)
	}

	got, err := svc.ListByBranch(ctx, "10")
	require.NoError(t, err)
	assert.Len(t, got, 2, "只有本门店的两个商品有条码")
	assert.Equal(t, []string{"6908946288183", "6908946288185"}, got["P1"],
		"主条码在前")
	assert.Equal(t, []string{"6957583900828"}, got["P2"])
	assert.NotContains(t, got, "P3", "没有条码的商品不在 map 里(调用方据此回落 id)")
}
