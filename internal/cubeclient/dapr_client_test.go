package cubeclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	dapr "github.com/dapr/go-sdk/client"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/shopspring/decimal"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeDaprClient 是 dapr.Client 的最小 mock 实现。
//
// 嵌入 dapr.Client(nil impl):其它方法被调用会 panic,提示测试未覆写。
// 用法:把 Invoke 字段设成你想要的 closure,即可拦截 InvokeMethodWithContent。
//
// 不再走 httptest.Server mock 整个 sidecar gRPC 协议 —— SDK 已暴露
// dapr.Client interface,这是更轻量的 mock 边界。
type fakeDaprClient struct {
	dapr.Client
	Invoke func(ctx context.Context, appID, method, verb string,
		content *dapr.DataContent) ([]byte, error)
}

// InvokeMethodWithContent 覆写 SDK 的接口方法,转给 Invoke 字段。
func (f *fakeDaprClient) InvokeMethodWithContent(
	ctx context.Context, appID, method, verb string, content *dapr.DataContent,
) ([]byte, error) {
	return f.Invoke(ctx, appID, method, verb, content)
}

// fakeResp 构造一个 mock cube 响应。
//
// code 是 HTTP 状态码;dapr sidecar 把它转 gRPC status code 后返给 SDK。
func fakeResp(code int, data []map[string]any) ([]byte, error) {
	if code == 404 {
		// gRPC 错误 (dapr 把 HTTP 404 转 codes.NotFound)
		return nil, status.Error(codes.NotFound, "not found")
	}
	if code >= 400 {
		return nil, status.Error(codes.Internal, "cube error")
	}
	body, _ := json.Marshal(map[string]any{"data": data})
	return body, nil
}

// newFakeClient 构造一个用 fn 模拟响应的 DaprCubeClient。
//
// fn 签名:(ctx, appID, method, verb, content) → (HTTP status, data, error)
//   - HTTP status 200/404:触发 normal flow
//   - 其它 status:返 wrap 后的 error
func newFakeClient(fn func(ctx context.Context, appID, method, verb string, content *dapr.DataContent) (int, []map[string]any, error)) *cubeclient.DaprCubeClient {
	fake := &fakeDaprClient{
		Invoke: func(ctx context.Context, appID, method, verb string, content *dapr.DataContent) ([]byte, error) {
			st, data, _ := fn(ctx, appID, method, verb, content)
			return fakeResp(st, data)
		},
	}
	return cubeclient.NewDaprCubeClient(fake, "cube-gateway")
}

// ---- GetProduct ----

func TestDaprCubeClient_GetProduct_OK(t *testing.T) {
	c := newFakeClient(func(_ context.Context, appID, method, verb string, content *dapr.DataContent) (int, []map[string]any, error) {
		if appID != "cube-gateway" {
			t.Errorf("appID = %q, want cube-gateway", appID)
		}
		if method != "v1/load" || verb != "POST" {
			t.Errorf("method=%q verb=%q, want v1/load POST", method, verb)
		}
		if content == nil || content.ContentType != "application/json" {
			t.Errorf("content 应为 application/json, got %+v", content)
		}
		// 校验 query body 含 product filter
		var body map[string]any
		if err := json.Unmarshal(content.Data, &body); err != nil {
			t.Fatalf("unmarshal content: %v", err)
		}
		if dims, _ := body["dimensions"].([]any); len(dims) == 0 {
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
		}, nil
	})

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

func TestDaprCubeClient_GetProduct_NotFound(t *testing.T) {
	c := newFakeClient(func(_ context.Context, _, _, _ string, _ *dapr.DataContent) (int, []map[string]any, error) {
		return 404, nil, nil
	})
	_, err := c.GetProduct(context.Background(), "P-9999")
	if !errors.Is(err, cubeclient.ErrProductNotFound) {
		t.Errorf("应报 ErrProductNotFound, got %v", err)
	}
}

func TestDaprCubeClient_GetProduct_EmptyData(t *testing.T) {
	c := newFakeClient(func(_ context.Context, _, _, _ string, _ *dapr.DataContent) (int, []map[string]any, error) {
		return 200, nil, nil
	})
	_, err := c.GetProduct(context.Background(), "P-9999")
	if !errors.Is(err, cubeclient.ErrProductNotFound) {
		t.Errorf("空 data 应报 ErrProductNotFound, got %v", err)
	}
}

// ---- GetStock ----

func TestDaprCubeClient_GetStock_OK(t *testing.T) {
	c := newFakeClient(func(_ context.Context, _, _, _ string, _ *dapr.DataContent) (int, []map[string]any, error) {
		return 200, []map[string]any{
			{
				"stock.product_id":     "P-1001",
				"stock.branch_id":      "S001",
				"stock.total_quantity": 100,
				"stock.avg_cost":       2.5,
			},
		}, nil
	})

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

func TestDaprCubeClient_GetStock_CrossBranchBlocked(t *testing.T) {
	c := newFakeClient(func(_ context.Context, _, _, _ string, _ *dapr.DataContent) (int, []map[string]any, error) {
		return 404, nil, nil
	})
	_, err := c.GetStock(context.Background(), "S002", "P-1003")
	if !errors.Is(err, cubeclient.ErrStockNotFound) {
		t.Errorf("跨店阻断应报 ErrStockNotFound, got %v", err)
	}
}

// ---- SearchProductsByBarcode ----

func TestDaprCubeClient_SearchProductsByBarcode_TooShort(t *testing.T) {
	called := false
	c := newFakeClient(func(_ context.Context, _, _, _ string, _ *dapr.DataContent) (int, []map[string]any, error) {
		called = true
		return 200, nil, nil
	})
	out, err := c.SearchProductsByBarcode(context.Background(), "12", "S001", 10)
	if err != nil {
		t.Fatalf("SearchProductsByBarcode: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("<3 位应返空, got %d", len(out))
	}
	if called {
		t.Errorf("<3 位不应发 cube 请求")
	}
}

func TestDaprCubeClient_SearchProductsByBarcode_Exact13(t *testing.T) {
	c := newFakeClient(func(_ context.Context, _, _, _ string, content *dapr.DataContent) (int, []map[string]any, error) {
		// 根据 query.dimensions 区分 product vs stock
		var body map[string]any
		if err := json.Unmarshal(content.Data, &body); err != nil {
			return 500, nil, nil
		}
		dims, _ := body["dimensions"].([]any)
		if len(dims) == 0 {
			return 200, nil, nil
		}
		firstDim, _ := dims[0].(string)
		if firstDim == "stock.product_id" {
			return 200, []map[string]any{
				{
					"stock.product_id":     "6901234567890",
					"stock.branch_id":      "S001",
					"stock.total_quantity": 100,
					"stock.avg_cost":       2.5,
				},
			}, nil
		}
		return 200, []map[string]any{
			{
				"product.id":          "6901234567890",
				"product.name":        "可口可乐 330ml",
				"product.category_id": "CAT-01",
				"product.supplier_id": "SUP-001",
				"product.status":      "active",
			},
		}, nil
	})

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

func TestDaprCubeClient_SearchProductsByBarcode_NotFound(t *testing.T) {
	c := newFakeClient(func(_ context.Context, _, _, _ string, _ *dapr.DataContent) (int, []map[string]any, error) {
		return 404, nil, nil
	})
	out, err := c.SearchProductsByBarcode(context.Background(), "9999999999999", "S001", 1)
	if err != nil {
		t.Errorf("未找到应返空而非 error: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("应返空, got %d", len(out))
	}
}

// ---- SearchSuppliers ----

func TestDaprCubeClient_SearchSuppliers_ByID(t *testing.T) {
	c := newFakeClient(func(_ context.Context, _, _, _ string, content *dapr.DataContent) (int, []map[string]any, error) {
		var body map[string]any
		_ = json.Unmarshal(content.Data, &body)
		filters, _ := body["filters"].([]any)
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
		}, nil
	})

	out, err := c.SearchSuppliers(context.Background(), "SUP-001", 0)
	if err != nil {
		t.Fatalf("SearchSuppliers: %v", err)
	}
	if len(out) != 1 || out[0].Name != "可口可乐华南" {
		t.Errorf("结果错: %+v", out)
	}
}

func TestDaprCubeClient_SearchSuppliers_ByName(t *testing.T) {
	c := newFakeClient(func(_ context.Context, _, _, _ string, _ *dapr.DataContent) (int, []map[string]any, error) {
		return 200, []map[string]any{
			{"supplier.id": "SUP-001", "supplier.name": "可口可乐华南", "supplier.type": "0"},
		}, nil
	})
	out, err := c.SearchSuppliers(context.Background(), "可口可乐", 10)
	if err != nil {
		t.Fatalf("SearchSuppliers: %v", err)
	}
	if len(out) != 1 {
		t.Errorf("应 1 条, got %d", len(out))
	}
}

func TestDaprCubeClient_SearchSuppliers_All(t *testing.T) {
	c := newFakeClient(func(_ context.Context, _, _, _ string, content *dapr.DataContent) (int, []map[string]any, error) {
		var body map[string]any
		_ = json.Unmarshal(content.Data, &body)
		filters, _ := body["filters"].([]any)
		if len(filters) > 0 {
			t.Errorf("空 query 不应有 filters, got %v", filters)
		}
		return 200, []map[string]any{
			{"supplier.id": "SUP-001", "supplier.name": "A", "supplier.type": "0"},
			{"supplier.id": "SUP-002", "supplier.name": "B", "supplier.type": "0"},
		}, nil
	})
	out, err := c.SearchSuppliers(context.Background(), "", 0)
	if err != nil {
		t.Fatalf("SearchSuppliers: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("应 2 条, got %d", len(out))
	}
}

// ---- HTTP 错误 ----

func TestDaprCubeClient_LoadCubeQuery_500ReturnsError(t *testing.T) {
	c := newFakeClient(func(_ context.Context, _, _, _ string, _ *dapr.DataContent) (int, []map[string]any, error) {
		return 500, nil, nil
	})
	_, err := c.GetProduct(context.Background(), "P-1001")
	if err == nil {
		t.Errorf("500 应返 error")
	}
}

// ---- ParseDecimal ----

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