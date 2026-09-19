package cubeclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/shopspring/decimal"
)

// startMockDapr 启动 mock dapr sidecar,转发 /v1/load 请求到 handler。
//
// mock server 接收 cube query JSON,返回指定 data 行。
func startMockDapr(t *testing.T, handler func(query map[string]any) (status int, data []map[string]any)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 只匹配 cube-gateway 的 /v1/load(经 dapr invocation 转发)
		// URL 形如 /v1.0/invoke/cube-gateway/method/v1/load
		if r.URL.Path != "/v1.0/invoke/cube-gateway/method/v1/load" {
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query map[string]any `json:"query"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		status, data := handler(req.Query)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status < 400 {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestClient(t *testing.T, srv *httptest.Server) *cubeclient.HTTPCubeClient {
	t.Helper()
	c := cubeclient.NewHTTPCubeClient(srv.URL, "cube-gateway")
	return c
}

// ---- GetProduct ----

func TestHTTPCubeClient_GetProduct_OK(t *testing.T) {
	srv := startMockDapr(t, func(q map[string]any) (int, []map[string]any) {
		// 校验 query 结构
		if q["dimensions"] == nil {
			t.Errorf("query.dimensions 应非空")
		}
		return 200, []map[string]any{
			{
				"product.id":          "P-1001",
				"product.name":        "可口可乐 330ml",
				"product.category_id": "CAT-01",
				"product.supplier_id": "SUP-001",
				"product.status":      "active",
			},
		}
	})
	c := newTestClient(t, srv)

	p, err := c.GetProduct(context.Background(), "P-1001")
	if err != nil {
		t.Fatalf("GetProduct: %v", err)
	}
	if p.ID != "P-1001" || p.Name != "可口可乐 330ml" {
		t.Errorf("product = %+v", p)
	}
	if p.SupplierID != "SUP-001" {
		t.Errorf("supplier_id = %q", p.SupplierID)
	}
}

func TestHTTPCubeClient_GetProduct_NotFound(t *testing.T) {
	srv := startMockDapr(t, func(q map[string]any) (int, []map[string]any) {
		return 404, nil // cube 找不到 → 404
	})
	c := newTestClient(t, srv)

	_, err := c.GetProduct(context.Background(), "P-9999")
	if !errors.Is(err, cubeclient.ErrProductNotFound) {
		t.Errorf("应报 ErrProductNotFound, got %v", err)
	}
}

func TestHTTPCubeClient_GetProduct_EmptyData(t *testing.T) {
	// cube 返 200 但 data 为空(无错误)→ 视为 NotFound
	srv := startMockDapr(t, func(q map[string]any) (int, []map[string]any) {
		return 200, nil
	})
	c := newTestClient(t, srv)

	_, err := c.GetProduct(context.Background(), "P-9999")
	if !errors.Is(err, cubeclient.ErrProductNotFound) {
		t.Errorf("空 data 应报 ErrProductNotFound, got %v", err)
	}
}

// ---- GetStock ----

func TestHTTPCubeClient_GetStock_OK(t *testing.T) {
	srv := startMockDapr(t, func(q map[string]any) (int, []map[string]any) {
		return 200, []map[string]any{
			{
				"stock.product_id":     "P-1001",
				"stock.branch_id":      "S001",
				"stock.total_quantity": 100,
				"stock.avg_cost":       2.5,
			},
		}
	})
	c := newTestClient(t, srv)

	s, err := c.GetStock(context.Background(), "S001", "P-1001")
	if err != nil {
		t.Fatalf("GetStock: %v", err)
	}
	if !s.Quantity.Equal(decimal.NewFromInt(100)) {
		t.Errorf("quantity = %s, want 100", s.Quantity)
	}
	if !s.AvgCostYuan.Equal(decimal.NewFromFloat(2.5)) {
		t.Errorf("avg_cost = %s, want 2.5", s.AvgCostYuan)
	}
}

func TestHTTPCubeClient_GetStock_CrossBranchBlocked(t *testing.T) {
	srv := startMockDapr(t, func(q map[string]any) (int, []map[string]any) {
		return 404, nil
	})
	c := newTestClient(t, srv)

	_, err := c.GetStock(context.Background(), "S002", "P-1003")
	if !errors.Is(err, cubeclient.ErrStockNotFound) {
		t.Errorf("跨店阻断应报 ErrStockNotFound, got %v", err)
	}
}

// ---- SearchProductsByBarcode ----

func TestHTTPCubeClient_SearchProductsByBarcode_TooShort(t *testing.T) {
	c := cubeclient.NewHTTPCubeClient("http://nope", "cube-gateway")
	out, err := c.SearchProductsByBarcode(context.Background(), "123", "S001", 10)
	if err != nil {
		t.Fatalf("SearchProductsByBarcode: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("<5 位应返空, got %d", len(out))
	}
}

func TestHTTPCubeClient_SearchProductsByBarcode_Exact13(t *testing.T) {
	srv := startMockDapr(t, func(q map[string]any) (int, []map[string]any) {
		// 根据 query.dimensions 区分 product vs stock
		dims, _ := q["dimensions"].([]any)
		if len(dims) == 0 {
			return 200, nil
		}
		firstDim, _ := dims[0].(string)
		if firstDim == "stock.product_id" {
			// stock query
			return 200, []map[string]any{
				{
					"stock.product_id":     "6901234567890",
					"stock.branch_id":      "S001",
					"stock.total_quantity": 100,
					"stock.avg_cost":       2.5,
				},
			}
		}
		// product query
		return 200, []map[string]any{
			{
				"product.id":          "6901234567890",
				"product.name":        "可口可乐 330ml",
				"product.category_id": "CAT-01",
				"product.supplier_id": "SUP-001",
				"product.status":      "active",
			},
		}
	})
	c := newTestClient(t, srv)

	out, err := c.SearchProductsByBarcode(context.Background(), "6901234567890", "S001", 1)
	if err != nil {
		t.Fatalf("SearchProductsByBarcode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("精确匹配应 1 条, got %d", len(out))
	}
	if out[0].Product == nil || out[0].Product.ID != "6901234567890" {
		t.Errorf("product 缺失")
	}
	if out[0].Stock == nil || !out[0].Stock.Quantity.Equal(decimal.NewFromInt(100)) {
		t.Errorf("stock 缺失或数量错: %+v", out[0].Stock)
	}
}

func TestHTTPCubeClient_SearchProductsByBarcode_NotFound(t *testing.T) {
	srv := startMockDapr(t, func(q map[string]any) (int, []map[string]any) {
		return 404, nil
	})
	c := newTestClient(t, srv)

	out, err := c.SearchProductsByBarcode(context.Background(), "9999999999999", "S001", 1)
	if err != nil {
		t.Errorf("未找到应返空而非 error: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("应返空, got %d", len(out))
	}
}

func TestHTTPCubeClient_SearchProductsByBarcode_5to12ReturnsEmpty(t *testing.T) {
	// 5~12 位:cube product schema 本期无 barcode 字段,无法后缀查 → 返空
	// 即使 mock server 有数据,客户端也不发请求
	srv := startMockDapr(t, func(q map[string]any) (int, []map[string]any) {
		t.Errorf("5~12 位不应发 cube 请求")
		return 200, nil
	})
	c := newTestClient(t, srv)

	out, err := c.SearchProductsByBarcode(context.Background(), "67890", "S001", 10)
	if err != nil {
		t.Fatalf("SearchProductsByBarcode: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("5~12 位应返空, got %d", len(out))
	}
}

// ---- SearchSuppliers ----

func TestHTTPCubeClient_SearchSuppliers_ByID(t *testing.T) {
	srv := startMockDapr(t, func(q map[string]any) (int, []map[string]any) {
		// 期望 filters: supplier.id equals
		filters, _ := q["filters"].([]any)
		if len(filters) == 0 {
			t.Errorf("filters 应非空")
		}
		return 200, []map[string]any{
			{
				"supplier.id":      "SUP-001",
				"supplier.name":    "可口可乐华南",
				"supplier.type":    "0",
				"supplier.contact": "张经理",
				"supplier.phone":   "13800000001",
			},
		}
	})
	c := newTestClient(t, srv)

	out, err := c.SearchSuppliers(context.Background(), "SUP-001", 0)
	if err != nil {
		t.Fatalf("SearchSuppliers: %v", err)
	}
	if len(out) != 1 || out[0].Name != "可口可乐华南" {
		t.Errorf("结果错: %+v", out)
	}
}

func TestHTTPCubeClient_SearchSuppliers_ByName(t *testing.T) {
	srv := startMockDapr(t, func(q map[string]any) (int, []map[string]any) {
		// 期望 filters: supplier.name contains
		return 200, []map[string]any{
			{"supplier.id": "SUP-001", "supplier.name": "可口可乐华南", "supplier.type": "0"},
		}
	})
	c := newTestClient(t, srv)

	out, err := c.SearchSuppliers(context.Background(), "可口可乐", 10)
	if err != nil {
		t.Fatalf("SearchSuppliers: %v", err)
	}
	if len(out) != 1 {
		t.Errorf("应 1 条, got %d", len(out))
	}
}

func TestHTTPCubeClient_SearchSuppliers_All(t *testing.T) {
	srv := startMockDapr(t, func(q map[string]any) (int, []map[string]any) {
		// 无 filters
		filters, _ := q["filters"].([]any)
		if len(filters) > 0 {
			t.Errorf("空 query 不应有 filters, got %v", filters)
		}
		return 200, []map[string]any{
			{"supplier.id": "SUP-001", "supplier.name": "A", "supplier.type": "0"},
			{"supplier.id": "SUP-002", "supplier.name": "B", "supplier.type": "0"},
		}
	})
	c := newTestClient(t, srv)

	out, err := c.SearchSuppliers(context.Background(), "", 0)
	if err != nil {
		t.Fatalf("SearchSuppliers: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("应 2 条, got %d", len(out))
	}
}

// ---- HTTP 错误 ----

func TestHTTPCubeClient_LoadCubeQuery_500ReturnsError(t *testing.T) {
	srv := startMockDapr(t, func(q map[string]any) (int, []map[string]any) {
		return 500, nil
	})
	c := newTestClient(t, srv)

	_, err := c.GetProduct(context.Background(), "P-1001")
	if err == nil {
		t.Errorf("500 应返 error")
	}
}

// 验证 ParseDecimal 对不同数值类型的解析
func TestParseDecimal(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{float64(100), "100"},
		{int(50), "50"},
		{int64(75), "75"},
		{"12.5", "12.5"},
		{json.Number("99.9"), "99.9"},
		{nil, "0"},
	}
	for _, tc := range cases {
		got := cubeclient.ParseDecimal(tc.in)
		if got.String() != tc.want {
			t.Errorf("ParseDecimal(%v) = %s, want %s", tc.in, got.String(), tc.want)
		}
	}
}
