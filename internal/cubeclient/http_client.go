// internal/cubeclient/http_client.go
//
// HTTPCubeClient 经 Dapr service invocation 调 cube-gateway /v1/load 端点,
// 拿 cube sixun 真实数据。
//
// 推荐配置:
//   DAPR_ENDPOINT = "http://localhost:3500"   // dapr sidecar 默认
//   CUBE_APP_ID   = "cube-gateway"            // 走 gateway 路由到具体 cube app
//                    = "sixun-hbposv7"          // 直连具体实例,跳过 gateway
//
// 协议参考:F:\go\src\github.com\YunBright\cube\pkg\cubequery\query.go
//   请求:{ query: { measures, dimensions, filters, limit } }
//   响应:{ data: [{ "<model>.<field>": value, ... }] }
//
// JWT 透传约定:
//   cube-gateway 的 dapr-sidecar 配了 middleware.http.bearer,要求请求带 Authorization: Bearer <token>。
//   stocktake 等 supertrade dapr app 经 dapr service invocation 直连 cube-gateway 时,
//   dapr 默认**不**透传 Authorization 头(nginx 路径下走 userd-sidecar → cube-gateway-sidecar
//   才会自动 forward)。所以 stocktake 的 handler 必须从 gin request 提取 JWT,塞进 context,
//   LoadCubeQuery 从 context 读出,加到 outgoing dapr invocation 上。
//   调用模式 (在 handler 里):
//     ctx := authctx.WithBearer(c.Request.Context(), c.Request.Header.Get("Authorization"))
//     appSvc.SearchProducts(ctx, ...)
//   LoadCubeQuery 内部自动 ctx.Value(bearerCtxKey) 取出并加 header。
package cubeclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// bearerCtxKey 是 context.Value 的 key,值是 "Bearer <token>" 完整字符串(可空)。
//
// 命名带 package 名("cubeclient/")避免与其他包 ctx key 冲突。
type bearerCtxKey struct{}

// WithBearer 把 JWT 透传到下游 cube 调用。
//
// handler 收到 gin request 时,调用一次 WithBearer 把 Authorization header
// (含 "Bearer " 前缀)塞进 ctx,后续 cubeclient 调用会自动 forward。
// 空字符串表示"无 token"(e.g. 内部定时任务),LoadCubeQuery 会跳过 header 注入。
func WithBearer(ctx context.Context, bearer string) context.Context {
	return context.WithValue(ctx, bearerCtxKey{}, bearer)
}

// bearerFromCtx 读 ctx 里的 bearer(没有 → "")。
func bearerFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(bearerCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// HTTPCubeClient 是 cube-gateway 的 client(dapr service invocation 封装)。
type HTTPCubeClient struct {
	daprEndpoint string // "http://localhost:3500"
	appID        string // "cube-gateway" 或具体 instance
	httpClient   *http.Client
}

// NewHTTPCubeClient 构造 HTTP cube client。
//
// daprEndpoint 例:"http://localhost:3500"
// appID 例:"cube-gateway" 或 "sixun-hbposv7"
func NewHTTPCubeClient(daprEndpoint, appID string) *HTTPCubeClient {
	return &HTTPCubeClient{
		daprEndpoint: daprEndpoint,
		appID:        appID,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
	}
}

// SetHTTPClient 注入自定义 http.Client(测试用)。
func (c *HTTPCubeClient) SetHTTPClient(h *http.Client) { c.httpClient = h }

// CubeQuery 是 cube /v1/load body 的最小子集。
type CubeQuery struct {
	Measures   []string
	Dimensions []string
	Filters    []CubeFilter
	Limit      int
}

// CubeFilter 是 where 条件。
type CubeFilter struct {
	Member   string `json:"member"`
	Operator string `json:"operator"` // equals / contains / in ...
	Values   []any  `json:"values"`
}

// LoadCubeQuery 调 cube /v1/load,返回 data[] 数组。
//
// 每行 key 形如 "<model>.<field>",例如 "product.id"、"stock.total_quantity"。
//
// 请求体是 cube /v1/load 期望的扁平 JSON(measures/dimensions/filters/limit 在顶层,
// 不是包在 "query" 字段里 — cube 的 cubequery.Parse 直接解析顶层字段)。
//
// HTTP 错误映射:
//   - 404 → ErrCubeNotFound(各 GetX 方法 wrap 成具体 error)
//   - 其它 ≥400 → 普通 error 含状态码
func (c *HTTPCubeClient) LoadCubeQuery(ctx context.Context, modelName string, q CubeQuery) ([]map[string]any, error) {
	body := map[string]any{
		"measures":   q.Measures,
		"dimensions": q.Dimensions,
		"filters":    q.Filters,
	}
	if q.Limit > 0 {
		body["limit"] = q.Limit
	}
	b, _ := json.Marshal(body)

	url := fmt.Sprintf("%s/v1.0/invoke/%s/method/v1/load", c.daprEndpoint, c.appID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("cube http: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// 透传调用方的 JWT (cube-gateway 的 dapr-sidecar bearer middleware 必需要)。
	// 没设过 WithBearer 时 bearerFromCtx 返 "" → 跳过,行为兼容。
	if bearer := bearerFromCtx(ctx); bearer != "" {
		req.Header.Set("Authorization", bearer)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cube http: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrCubeNotFound
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("cube http %d: %s", resp.StatusCode, string(respBody))
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("cube decode: %w", err)
	}
	return out.Data, nil
}

// ---- Client interface 实现 ----

// GetProduct 查 cube product(按 item_no)。
func (c *HTTPCubeClient) GetProduct(ctx context.Context, productID string) (*ProductDTO, error) {
	data, err := c.LoadCubeQuery(ctx, "product", CubeQuery{
		Measures:   []string{"product.count"},
		Dimensions: []string{"product.id", "product.name", "product.category_id",
			"product.supplier_id", "product.status"},
		Filters: []CubeFilter{
			{Member: "product.id", Operator: "equals", Values: []any{productID}},
		},
		Limit: 1,
	})
	if err != nil {
		if errors.Is(err, ErrCubeNotFound) {
			return nil, fmt.Errorf("%w: product_id=%s", ErrProductNotFound, productID)
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: product_id=%s", ErrProductNotFound, productID)
	}
	row := data[0]
	return &ProductDTO{
		ID:         asStr(row["product.id"]),
		Name:       asStr(row["product.name"]),
		CategoryID: asStr(row["product.category_id"]),
		SupplierID: asStr(row["product.supplier_id"]),
		Status:     asStr(row["product.status"]),
		// Barcode: cube product schema 本期不含,留空;后续 cube 仓库扩展后补
	}, nil
}

// GetStock 查 cube stock(按 branch_id + product_id)。
//
// cube stock.total_quantity / cube stock.avg_cost。
func (c *HTTPCubeClient) GetStock(ctx context.Context, branchID, productID string) (*StockSnapshotDTO, error) {
	data, err := c.LoadCubeQuery(ctx, "stock", CubeQuery{
		Dimensions: []string{"stock.product_id", "stock.branch_id"},
		Measures:   []string{"stock.total_quantity", "stock.avg_cost"},
		Filters: []CubeFilter{
			{Member: "stock.branch_id", Operator: "equals", Values: []any{branchID}},
			{Member: "stock.product_id", Operator: "equals", Values: []any{productID}},
		},
		Limit: 1,
	})
	if err != nil {
		if errors.Is(err, ErrCubeNotFound) {
			return nil, fmt.Errorf("%w: branch_id=%s product_id=%s",
				ErrStockNotFound, branchID, productID)
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: branch_id=%s product_id=%s",
			ErrStockNotFound, branchID, productID)
	}
	row := data[0]
	return &StockSnapshotDTO{
		ProductID:   asStr(row["stock.product_id"]),
		BranchID:    asStr(row["stock.branch_id"]),
		Quantity:    asDecimal(row["stock.total_quantity"]),
		AvgCostYuan: asDecimal(row["stock.avg_cost"]),
		UpdatedAt:   time.Now().UTC(), // cube stock.updated_at 本期未拉(简化)
	}, nil
}

// SearchProductsByBarcode 按 barcode 查商品,合并该门店 stock。
//
// 把 barcode 当 item_no 精确查(cube product.id = 思迅 item_no,
// 条码和 item_no 在思迅系统里通常是同一字段)。
// 长度 < 3 时返空(防全表扫 + 防无效输入)。
//
// 注:本期 cube sixun-models/product 不含 barcode dimension,
// 条码模糊查询需要 cube 仓库扩展,后续 Phase 跟进。
func (c *HTTPCubeClient) SearchProductsByBarcode(ctx context.Context, barcode, branchID string, limit int) ([]ProductWithStock, error) {
	if len(barcode) < 3 {
		return []ProductWithStock{}, nil
	}
	// 精确查(把 barcode 当 item_no)
	p, err := c.GetProduct(ctx, barcode)
	if err != nil {
		if errors.Is(err, ErrProductNotFound) {
			return []ProductWithStock{}, nil
		}
		return nil, err
	}
	out := []ProductWithStock{{Product: p}}
	if branchID != "" {
		s, err := c.GetStock(ctx, branchID, barcode)
		if err == nil {
			out[0].Stock = s
		}
		// 跨店阻断:stock 不存在不报错(GetStock 已 wrap ErrStockNotFound),只不返 stock 字段
	}
	return out, nil
}

// SearchSuppliers 按 ID 精确 或 name 模糊查供应商。
//
// query 以 "SUP-" 开头 → 走 id equals;
// 否则走 name contains。
func (c *HTTPCubeClient) SearchSuppliers(ctx context.Context, query string, limit int) ([]SupplierDTO, error) {
	cq := CubeQuery{
		Measures:   []string{"supplier.count"},
		Dimensions: []string{"supplier.id", "supplier.name", "supplier.type",
			"supplier.contact", "supplier.phone"},
	}
	if query != "" {
		if strings.HasPrefix(query, "SUP-") {
			cq.Filters = []CubeFilter{
				{Member: "supplier.id", Operator: "equals", Values: []any{query}},
			}
			cq.Limit = 1
		} else {
			cq.Filters = []CubeFilter{
				{Member: "supplier.name", Operator: "contains", Values: []any{query}},
			}
			if limit > 0 {
				cq.Limit = limit
			}
		}
	}
	data, err := c.LoadCubeQuery(ctx, "supplier", cq)
	if err != nil {
		return nil, err
	}
	out := make([]SupplierDTO, 0, len(data))
	for _, row := range data {
		out = append(out, SupplierDTO{
			ID:      asStr(row["supplier.id"]),
			Name:    asStr(row["supplier.name"]),
			Type:    asStr(row["supplier.type"]),
			Contact: asStr(row["supplier.contact"]),
			Phone:   asStr(row["supplier.phone"]),
		})
	}
	return out, nil
}

// ---- helpers ----

func asStr(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func asDecimal(v any) decimal.Decimal {
	return ParseDecimal(v)
}

// ParseDecimal 把 cube 返回的任意数值字段转为 decimal.Decimal(导出,测试可见)。
func ParseDecimal(v any) decimal.Decimal {
	if v == nil {
		return decimal.Zero
	}
	switch x := v.(type) {
	case float64:
		return decimal.NewFromFloat(x)
	case float32:
		return decimal.NewFromFloat32(x)
	case int:
		return decimal.NewFromInt(int64(x))
	case int64:
		return decimal.NewFromInt(x)
	case string:
		d, _ := decimal.NewFromString(x)
		return d
	case json.Number:
		d, _ := decimal.NewFromString(x.String())
		return d
	}
	return decimal.Zero
}
