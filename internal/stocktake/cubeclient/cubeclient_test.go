//go:build ignore
// +build ignore

// MOVED -> ../cubeclient/ (same basename). 本文件已废弃,忽略 build。

package cubeclient_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/stocktake/cubeclient"
	"github.com/shopspring/decimal"
)

func TestInMemoryClient_GetProduct_OK(t *testing.T) {
	c := cubeclient.NewInMemoryClient()
	p, err := c.GetProduct(context.Background(), "P-1001")
	if err != nil {
		t.Fatalf("GetProduct: %v", err)
	}
	if p.ID != "P-1001" {
		t.Errorf("id = %q", p.ID)
	}
	if p.Name != "可口可乐 330ml" {
		t.Errorf("name = %q", p.Name)
	}
	if p.Barcode != "6901234567890" {
		t.Errorf("barcode = %q", p.Barcode)
	}
}

func TestInMemoryClient_GetProduct_NotFound(t *testing.T) {
	c := cubeclient.NewInMemoryClient()
	_, err := c.GetProduct(context.Background(), "P-9999")
	if !errors.Is(err, cubeclient.ErrProductNotFound) {
		t.Errorf("err 应为 ErrProductNotFound, got %v", err)
	}
}

func TestInMemoryClient_GetStock_OK(t *testing.T) {
	c := cubeclient.NewInMemoryClient()
	// 注入固定时钟断言快照时间
	fixed := time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC)
	c.SetClock(func() time.Time { return fixed })

	snap, err := c.GetStock(context.Background(), "S001", "P-1001")
	if err != nil {
		t.Fatalf("GetStock: %v", err)
	}
	if !snap.Quantity.Equal(decimal.NewFromInt(100)) {
		t.Errorf("quantity = %s, want 100", snap.Quantity)
	}
	if !snap.AvgCostYuan.Equal(decimal.NewFromFloat(2.5)) {
		t.Errorf("avg_cost_yuan = %s, want 2.5", snap.AvgCostYuan)
	}
	if !snap.UpdatedAt.Equal(fixed) {
		t.Errorf("updated_at 应等于注入时钟, got %v", snap.UpdatedAt)
	}
	if snap.BranchID != "S001" || snap.ProductID != "P-1001" {
		t.Errorf("branch/product ID 不匹配")
	}
}

// 跨店阻断:S002 盘点,但 P-1003 不在 S002 → ErrStockNotFound
func TestInMemoryClient_GetStock_CrossBranch(t *testing.T) {
	c := cubeclient.NewInMemoryClient()
	_, err := c.GetStock(context.Background(), "S002", "P-1003")
	if !errors.Is(err, cubeclient.ErrStockNotFound) {
		t.Errorf("跨店 stock 应报 ErrStockNotFound, got %v", err)
	}
}

func TestInMemoryClient_UpsertStock_ReflectsNewValue(t *testing.T) {
	c := cubeclient.NewInMemoryClient()
	c.UpsertStock("S001", "P-1001", decimal.NewFromInt(80), decimal.NewFromFloat(2.5))
	snap, err := c.GetStock(context.Background(), "S001", "P-1001")
	if err != nil {
		t.Fatalf("GetStock: %v", err)
	}
	if !snap.Quantity.Equal(decimal.NewFromInt(80)) {
		t.Errorf("quantity 应 80, got %s", snap.Quantity)
	}
}

// ---- SearchProductsByBarcode ----

func TestInMemoryClient_SearchProductsByBarcode_TooShort(t *testing.T) {
	c := cubeclient.NewInMemoryClient()
	out, err := c.SearchProductsByBarcode(context.Background(), "123", "S001", 10)
	if err != nil {
		t.Fatalf("SearchProductsByBarcode: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("<5 位应返回空, got %d", len(out))
	}
}

func TestInMemoryClient_SearchProductsByBarcode_Exact13(t *testing.T) {
	// ≥13 位精确匹配:13 位的 "6901234567890" 是 P-1001 的 barcode
	c := cubeclient.NewInMemoryClient()
	out, err := c.SearchProductsByBarcode(context.Background(), "6901234567890", "S001", 10)
	if err != nil {
		t.Fatalf("SearchProductsByBarcode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("精确匹配应 1 条, got %d", len(out))
	}
	if out[0].Product.ID != "P-1001" {
		t.Errorf("id = %q, want P-1001", out[0].Product.ID)
	}
	if out[0].Stock == nil {
		t.Errorf("Stock 应有 S001 的快照")
	} else if !out[0].Stock.Quantity.Equal(decimal.NewFromInt(100)) {
		t.Errorf("stock_qty = %s, want 100", out[0].Stock.Quantity)
	}
}

func TestInMemoryClient_SearchProductsByBarcode_Suffix5to12(t *testing.T) {
	// 5~12 位后缀匹配:"67890" 是 P-1001 (6901234567890) 的后缀
	c := cubeclient.NewInMemoryClient()
	out, err := c.SearchProductsByBarcode(context.Background(), "67890", "S001", 10)
	if err != nil {
		t.Fatalf("SearchProductsByBarcode: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("后缀匹配应至少 1 条")
	}
	found := false
	for _, r := range out {
		if r.Product.ID == "P-1001" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("应命中 P-1001, got %d 条", len(out))
	}
}

func TestInMemoryClient_SearchProductsByBarcode_NoStockBranch(t *testing.T) {
	// 商品存在但该门店无 stock → Product 有,Stock 为 nil
	c := cubeclient.NewInMemoryClient()
	out, err := c.SearchProductsByBarcode(context.Background(), "6901234567890", "S999", 1)
	if err != nil {
		t.Fatalf("SearchProductsByBarcode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("应 1 条, got %d", len(out))
	}
	if out[0].Stock != nil {
		t.Errorf("无 stock 门店 Stock 应 nil, got %+v", out[0].Stock)
	}
}

// ---- SearchSuppliers ----

func TestInMemoryClient_SearchSuppliers_Empty(t *testing.T) {
	c := cubeclient.NewInMemoryClient()
	out, err := c.SearchSuppliers(context.Background(), "", 0)
	if err != nil {
		t.Fatalf("SearchSuppliers: %v", err)
	}
	if len(out) < 3 {
		t.Errorf("空 query 应返所有供应商, got %d", len(out))
	}
}

func TestInMemoryClient_SearchSuppliers_ByName(t *testing.T) {
	c := cubeclient.NewInMemoryClient()
	out, err := c.SearchSuppliers(context.Background(), "可口可乐", 0)
	if err != nil {
		t.Fatalf("SearchSuppliers: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("name 含'可口可乐'应 1 条, got %d", len(out))
	}
	if out[0].ID != "SUP-001" {
		t.Errorf("id = %q, want SUP-001", out[0].ID)
	}
}

func TestInMemoryClient_SearchSuppliers_ByID(t *testing.T) {
	c := cubeclient.NewInMemoryClient()
	out, err := c.SearchSuppliers(context.Background(), "SUP-101", 0)
	if err != nil {
		t.Fatalf("SearchSuppliers: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("精确 ID 应 1 条, got %d", len(out))
	}
	if out[0].Name != "鲜肉供应商" {
		t.Errorf("name = %q, want 鲜肉供应商", out[0].Name)
	}
}