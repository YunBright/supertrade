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

// 思迅实测:同一商品有两条条码(口味变体),见 model/product_barcode.go。
func TestProductBarcodeService_Upsert_MultiplePerProduct(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewProductBarcodeService(db)

	_, err := svc.Upsert(context.Background(), service.BarcodeInput{
		ProductID: "P1", BranchID: "10", Barcode: "6908946288183", IsPrimary: true, Label: "草莓味",
	})
	require.NoError(t, err)
	_, err = svc.Upsert(context.Background(), service.BarcodeInput{
		ProductID: "P1", BranchID: "10", Barcode: "6908946288185", Label: "桔子味",
	})
	require.NoError(t, err)

	rows, err := svc.ListByProduct(context.Background(), "10", "P1")
	require.NoError(t, err)
	require.Len(t, rows, 2, "一个商品可以有多个条码")
	// 主条码排前面。
	assert.True(t, rows[0].IsPrimary)
	assert.Equal(t, "6908946288183", rows[0].Barcode)
}

func TestProductBarcodeService_Upsert_Idempotent(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewProductBarcodeService(db)

	_, err := svc.Upsert(context.Background(), service.BarcodeInput{
		ProductID: "P1", BranchID: "10", Barcode: "6908946288183", Label: "草莓味",
	})
	require.NoError(t, err)

	// 同一条码重跑(导入重跑)⇒ 不产生第二行,只更新可变列。
	_, err = svc.Upsert(context.Background(), service.BarcodeInput{
		ProductID: "P1", BranchID: "10", Barcode: "6908946288183", Label: "新口味",
	})
	require.NoError(t, err)

	rows, err := svc.ListByProduct(context.Background(), "10", "P1")
	require.NoError(t, err)
	require.Len(t, rows, 1, "重跑导入不能产生重复条码行")
	assert.Equal(t, "新口味", rows[0].Label)
}

func TestProductBarcodeService_FindByBarcode(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewProductBarcodeService(db)

	_, err := svc.Upsert(context.Background(), service.BarcodeInput{
		ProductID: "P1", BranchID: "10", Barcode: "6908946288183",
	})
	require.NoError(t, err)

	got, err := svc.FindByBarcode(context.Background(), "10", "6908946288183")
	require.NoError(t, err)
	assert.Equal(t, "P1", got)

	// 别的门店扫同一个条码 ⇒ 查不到(门店隔离)。
	_, err = svc.FindByBarcode(context.Background(), "11", "6908946288183")
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrBarcodeNotFound))
}

func TestProductBarcodeService_ReplaceForProduct_RemovesGhosts(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewProductBarcodeService(db)

	for _, b := range []string{"B1", "B2", "B3"} {
		_, err := svc.Upsert(context.Background(), service.BarcodeInput{
			ProductID: "P1", BranchID: "10", Barcode: b,
		})
		require.NoError(t, err)
	}

	// 源库现在只有两条 ⇒ 整体替换,旧的第三条必须消失。
	// (增量式"只加不减"会留下幽灵条码,扫出来是错商品。)
	written, removed, err := svc.ReplaceForProduct(context.Background(), "10", "P1", []service.BarcodeInput{
		{Barcode: "B1", IsPrimary: true},
		{Barcode: "B2"},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, written)
	assert.Equal(t, 1, removed, "不在集合里的条码要被删掉")

	rows, err := svc.ListByProduct(context.Background(), "10", "P1")
	require.NoError(t, err)
	assert.Len(t, rows, 2)
	for _, r := range rows {
		assert.NotEqual(t, "B3", r.Barcode, "幽灵条码不能留在库里")
	}
}

func TestProductBarcodeService_Delete(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewProductBarcodeService(db)

	_, err := svc.Upsert(context.Background(), service.BarcodeInput{
		ProductID: "P1", BranchID: "10", Barcode: "B1",
	})
	require.NoError(t, err)

	require.NoError(t, svc.Delete(context.Background(), "10", "P1", "B1"))

	rows, err := svc.ListByProduct(context.Background(), "10", "P1")
	require.NoError(t, err)
	assert.Empty(t, rows, "软删后不能出现在列表里")

	// 删不存在的 → ErrBarcodeNotFound。
	err = svc.Delete(context.Background(), "10", "P1", "B1")
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrBarcodeNotFound))
}
