// internal/cubeclient/dapr_client.go
//
// DaprCubeClient 经 github.com/dapr/go-sdk 的 dapr.Client 调 cube /v1/load。
//
// 推荐配置 (2026-09 PR 5 后,SDK 模式):
//   CUBE_APP_ID = "cube-router"        // 走 cube-router 多源路由 (默认)
//              = "cube-gateway"        // 直连 cube-gateway (历史 fallback)
//              = "sixun-hbposv7"       // 直连具体实例,跳过 gateway
//
// 协议参考:F:\go\src\github.com\YunBright\cube\pkg\cubequery\query.go
//   请求:{ measures, dimensions, filters, limit }
//   响应:{ data: [{ "<model>.<field>": value, ... }] }
//
// JWT 透传约定:
//   cube-gateway 的 dapr-sidecar 配了 middleware.http.bearer,要求请求带 Authorization: Bearer <token>。
//   stocktake 等 supertrade dapr app 经 dapr service invocation 直连 cube-gateway 时,
//   把 JWT 塞进 outgoing gRPC metadata "authorization";SDK 把它转成 outgoing HTTP
//   Authorization 头给目标 app。
//   调用模式 (在 handler 里):
//     ctx := authctx.WithBearer(c.Request.Context(), c.Request.Header.Get("Authorization"))
//     appSvc.SearchProducts(ctx, ...)
//   LoadCubeQuery 内部自动 ctx.Value(bearerCtxKey) 取出并塞 gRPC metadata。
//
// 2026-09 重构对比旧 http_client.go:
//   - 不再读 DAPR_ENDPOINT(SDK 自动从 DAPR_GRPC_PORT 拿 :50001)
//   - 不再手拼 /v1.0/invoke/<app-id>/method/v1/load URL
//   - 不再 http.Client + req.Header.Set("Authorization", ...)
//   - 由 dapr.Client.InvokeMethodWithContent 完成所有上述工作
package cubeclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	dapr "github.com/dapr/go-sdk/client"
	"github.com/shopspring/decimal"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// bearerCtxKey 是 context.Value 的 key,值是 "Bearer <token>" 完整字符串(可空)。
//
// 命名带 package 名("cubeclient/")避免与其他包 ctx key 冲突。
type bearerCtxKey struct{}

// branchCtxKey 是 context.Value 的 key,值是 X-Branch-ID header 原值(支持 `*` / `01,02` 多店)。
type branchCtxKey struct{}

// WithBearer 把 JWT 透传到下游 cube 调用。
//
// handler 收到 gin request 时,调用一次 WithBearer 把 Authorization header
// (含 "Bearer " 前缀)塞进 ctx,后续 cubeclient 调用会自动 forward。
// 空字符串表示"无 token"(e.g. 内部定时任务),LoadCubeQuery 会跳过 metadata 注入。
func WithBearer(ctx context.Context, bearer string) context.Context {
	return context.WithValue(ctx, bearerCtxKey{}, bearer)
}

// WithBranchID 把 X-Branch-ID header 透传到下游 cube 调用。
//
// 支持 `*`(全部 accessible branches) / `01,02`(多店逗号分隔),按字面值传给
// 下游 server 自己解析 / 校验。
// 空字符串表示"无 branch"(单店内部查询,不需要 cube 走门店过滤),LoadCubeQuery 会跳过 metadata 注入。
func WithBranchID(ctx context.Context, branchID string) context.Context {
	return context.WithValue(ctx, branchCtxKey{}, branchID)
}

// WithBranchIDs migration 009 起的 []string 多店版本。
//
// 等价于 WithBranchID(ctx, strings.Join(branchIDs, ","));空 slice → 不设 key
// (下游走全矩阵,与 WithBranchID(ctx, "") 一致)。
//
// 调用方:handler 拿到的 []string(如 middleware.BranchIDsFromContext)直接传。
func WithBranchIDs(ctx context.Context, branchIDs []string) context.Context {
	if len(branchIDs) == 0 {
		return ctx
	}
	return context.WithValue(ctx, branchCtxKey{}, strings.Join(branchIDs, ","))
}

// bearerFromCtx 读 ctx 里的 bearer(没有 → "")。
func bearerFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(bearerCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// branchFromCtx 读 ctx 里的 branchID(没有 → "")。
func branchFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(branchCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// DaprCubeClient 是 cube-gateway / cube-router 的 client (dapr SDK 封装)。
//
// 持有 dapr.Client (gRPC SDK 接口) + appID + timeout。
// timeout 是 SDK 调用的本端超时(SDK 内部还有 default 5s 超时;
// 短超时优先)。
type DaprCubeClient struct {
	dapr    dapr.Client
	appID   string // "cube-router" / "cube-gateway" / 具体 instance
	timeout time.Duration
}

// NewDaprCubeClient 构造 dapr cube client。
//
// daprClient 通常是 dapr.NewClient() 的返回值;测试可注入 fake (实现 dapr.Client 接口)。
// appID 例:"cube-router"(默认,经 cube-router 多源路由) / "cube-gateway"(直连)
// / "sixun-hbposv7"(具体实例)。
func NewDaprCubeClient(daprClient dapr.Client, appID string) *DaprCubeClient {
	return &DaprCubeClient{
		dapr:    daprClient,
		appID:   appID,
		timeout: 10 * time.Second,
	}
}

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
// 错误映射:
//   - gRPC NotFound → ErrCubeNotFound(各 GetX 方法 wrap 成具体 error)
//   - 其它错误 → 普通 wrap error
func (c *DaprCubeClient) LoadCubeQuery(ctx context.Context, modelName string, q CubeQuery) ([]map[string]any, error) {
	body := map[string]any{
		"measures":   q.Measures,
		"dimensions": q.Dimensions,
		"filters":    q.Filters,
	}
	if q.Limit > 0 {
		body["limit"] = q.Limit
	}
	b, _ := json.Marshal(body)

	// JWT 透传:从 ctx 拿 bearer,塞 outgoing gRPC metadata。
	// dapr sidecar 把 outgoing gRPC metadata keys 转 outgoing HTTP headers 给目标 app。
	if bearer := bearerFromCtx(ctx); bearer != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", bearer)
	}
	// X-Branch-ID 透传:让 cube 知道走哪个门店的数据(`*` / `01,02` 字面值原样传)。
	if bid := branchFromCtx(ctx); bid != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-branch-id", bid)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.dapr.InvokeMethodWithContent(ctx, c.appID, "v1/load", "POST",
		&dapr.DataContent{ContentType: "application/json", Data: b})
	if err != nil {
		if st, ok := status.FromError(err); ok && st.Code().String() == "NotFound" {
			return nil, ErrCubeNotFound
		}
		return nil, fmt.Errorf("cube dapr: %w", err)
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, fmt.Errorf("cube decode: %w", err)
	}
	return out.Data, nil
}

// ---- Client interface 实现 ----

// GetProduct 查 cube product(按 item_no)。
func (c *DaprCubeClient) GetProduct(ctx context.Context, productID string) (*ProductDTO, error) {
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
func (c *DaprCubeClient) GetStock(ctx context.Context, branchID, productID string) (*StockSnapshotDTO, error) {
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
func (c *DaprCubeClient) SearchProductsByBarcode(ctx context.Context, barcode, branchID string, limit int) ([]ProductWithStock, error) {
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
func (c *DaprCubeClient) SearchSuppliers(ctx context.Context, query string, limit int) ([]SupplierDTO, error) {
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