// internal/cubeclient/dapr_client.go
//
// DaprCubeClient 经 github.com/dapr/go-sdk 的 dapr.Client 调 cube /v1/load。
//
// 推荐配置:
//
//	CUBE_APP_ID = "cube-router"        // 走 cube-router 多源路由 (默认)
//	           = "cube-gateway"        // 直连 cube-gateway (历史 fallback)
//	           = "sixun-hbposv7"       // 直连具体实例,跳过 gateway
//
// 协议参考:F:\go\src\github.com\YunBright\cube\pkg\cubequery\query.go
//
//	请求:{ measures, dimensions, filters, limit }
//	响应:{ data: [{ "<model>.<field>": value, ... }] }
//
// JWT 透传约定:
//
//	cube-gateway 的 dapr-sidecar 配了 middleware.http.bearer,要求请求带 Authorization: Bearer <token>。
//	stocktake 等 supertrade dapr app 经 dapr service invocation 直连 cube-gateway 时,
//	把 JWT 塞进 outgoing gRPC metadata "authorization";SDK 把它转成 outgoing HTTP
//	Authorization 头给目标 app。
//	调用模式 (在 handler 里):
//	  ctx := authctx.WithBearer(c.Request.Context(), c.Request.Header.Get("Authorization"))
//	  appSvc.SearchProducts(ctx, ...)
//	LoadCubeQuery 内部自动 ctx.Value(bearerCtxKey) 取出并塞 gRPC metadata。
package cubeclient

import (
	"log/slog"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	dapr "github.com/dapr/go-sdk/client"
	"github.com/shopspring/decimal"
	"google.golang.org/grpc/metadata"
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

// BranchFromContext 是 [branchFromCtx] 的导出读侧。
//
// 存在的理由:调用方(cmd/*/main.go 里的透传中间件)只能**写** WithBranchID,
// 却无法**读**,于是没法给自己的中间件写回归测试 —— 而 2026-10-16 那次生产
// 故障正是"中间件忘了写"造成的:漏注入没有任何编译错误、没有单测变红,
// 只有线上所有条码查询报「库存服务暂不可用」。
// 有了读侧,"这个中间件到底有没有把门店写进 ctx"才是一条可以断言的事实。
func BranchFromContext(ctx context.Context) string { return branchFromCtx(ctx) }

// daprInvoker 是 DaprCubeClient 对 dapr SDK 的**全部**依赖面。
//
// 为什么收窄:go-sdk v1.10.1 只带 actor/mock,**没有 client/mock**,
// 而 dapr.Client 有几十个方法 —— 单测想捕获"到底往 cube 发了什么 query"
// 就得手写整个接口,成本高到没人愿意写,于是 GetProduct 的 Dimensions
// 只能靠肉眼 review 保护(2026-10-09 加 product.unit 时就是如此:
// 把它删掉不会有任何测试变红,只会让 wx-h5 静默退回显示「件」)。
//
// 收窄后 dapr.Client 自动满足本接口(结构化子集),所有现有调用方零改动。
type daprInvoker interface {
	InvokeMethodWithContent(ctx context.Context, appID, methodName, verb string,
		content *dapr.DataContent) ([]byte, error)
}

// DaprCubeClient 是 cube-gateway / cube-router 的 client (dapr SDK 封装)。
//
// 持有 daprInvoker (dapr.Client 的结构化子集) + appID + timeout。
// timeout 是 SDK 调用的本端超时(SDK 内部还有 default 5s 超时;
// 短超时优先)。
type DaprCubeClient struct {
	dapr      daprInvoker
	appID     string
	queryPath string // 该 app-id 上的查询方法路径,随 appID 变化
	timeout   time.Duration
	// barcodes 是"商品条码全表"的短 TTL 快照(见 item_barcode.go)。
	barcodes *itemBarcodeCache
	// log 是该 client 的 logger(nil ⇒ 用 slog.Default())。
	//
	// ⚠️ 只给"降级路径"用:条形码快照拉不到时不能把整个搜索带崩,
	//   但**也不能静默** —— 静默降级会让"扫条码搜不到"变成一个没人知道的原因。
	log *slog.Logger
}

// SetLogger 给 client 设 logger(可选;不设就用 slog.Default())。
func (c *DaprCubeClient) SetLogger(l *slog.Logger) {
	if l != nil {
		c.log = l
	}
}

// NewDaprCubeClient 构造 dapr cube client。
//
// daprClient 通常是 dapr.NewClient() 的返回值(它自动满足 daprInvoker 这个
// 结构化子集,所有现有调用方无需改动);测试可注入只实现了
// InvokeMethodWithContent 的窄 fake,见 query_shape_test.go。
//
// appID 与 queryPath **必须成对给**,它们描述同一个 dapr app 的地址:
//
//	appID                     queryPath
//	-----------------------  ---------------
//	supertrade-cube-router   v1/load        ← 默认,唯一支持 per-branch 路由
//	cube-sixun-ysx-fb        query          ← 直连语义层 app,绕过 branch 隔离
//
// 传错组合(例如拿 supertrade-cube-router 配 query)会稳定 404,而 404 与
// "商品不存在" 无法区分 —— 这正是 2026-10-08 那次故障被显示成
// 「本门店没有条码 X 的商品」的机制。参见 errors.go 的注释表。
func NewDaprCubeClient(daprClient daprInvoker, appID, queryPath string) *DaprCubeClient {
	if queryPath == "" {
		queryPath = DefaultCubeQueryPathName
	}
	return &DaprCubeClient{
		dapr:      daprClient,
		appID:     appID,
		queryPath: queryPath,
		// ⚠️ 10s 太紧(2026-10-23 实测):逐笔流水表 330 万行,一条"按天 + 关联商品档案"的
		// live 查询在 cube 侧就要 1.4s 原始 SQL + cube app 自身开销 + 两跳 dapr 转发
		// ⇒ 端到端 5~6s。**并发几个请求同时打过来时就会撞 10s ⇒ 端点返 503**,
		// 而那个 503 的文案是「数据源暂不可用」⇒ 会被误读成"cube 挂了"。
		//
		// ⇒ 放宽到 30s:慢查询照样能出结果,而不是变成一个误导性的 503。
		// (真正的 cube 故障仍会按 gRPC code 分类成 503/403/401,不受影响。)
		timeout: 30 * time.Second,
		barcodes:  newItemBarcodeCache(itemBarcodeTTL, nil),
	}
}

// CubeQuery 是 cube /v1/load body 的最小子集。
//
// # 2026-10-20 扩展:offset / order / total
//
// 之前只有 measures / dimensions / filters / limit,而 cube 的分页有两条硬约束:
//
//  1. **offset 必须配 order**。offset>0 且没给 order 时 cube 直接 400:
//     「offset=2 需要显式 order:分页必须建立在稳定排序上,否则每页可能重复或漏行」。
//     没有 order 的 limit 是**任意截取**,翻页会重复 / 漏行。
//  2. **不给 total 时响应里的 total 恒为 0**。total=true 才返回"不受
//     limit/offset 影响的总行数"(响应顶层 total)。
//
// ⇒ 之前这个结构体**根本表达不了分页**,只能"一次全取"。
type CubeQuery struct {
	Measures   []string
	Dimensions []string
	Filters    []CubeFilter
	Limit      int
	// Offset 是偏移量。**必须同时给 Order**,否则 cube 返 400。
	Offset int
	// Order 是排序。id 必须写全限定名("<model>.<ref>")且**同时在本次 SELECT 里**
	// (ORDER BY 只能引用输出列,否则 GROUP BY 查询会在 DB 层报 "not a grouped column")。
	Order []CubeOrder
	// Total 为 true 时 cube 返回不受 limit/offset 影响的总行数(响应顶层 total)。
	Total bool
}

// CubeOrder 是 cube 的排序项。
//
// ID 写全限定名("product.id"),Order 取 asc / desc(大小写不敏感,省略按 asc)。
type CubeOrder struct {
	ID    string `json:"id"`
	Order string `json:"order"`
}

// CubeFilter 是 where 条件。
type CubeFilter struct {
	Member   string `json:"member"`
	Operator string `json:"operator"` // equals / contains / in ...
	Values   []any  `json:"values"`
}

// cube 的 filter 算子名(拼错会得到 400 `unsupported operator`,不会静默返回空)。
//
// 这里只把**用到的**列成常量:算子名是**跨仓契约**(cube 编译成 SQL 语义),
// 散落在调用点写裸字符串时,拼错了只有到运行时才知道 —— 而那时已经在生产上了。
const (
	// OperatorEquals 等值匹配。
	OperatorEquals = "equals"
	// OperatorContains 子串匹配,**大小写敏感**。
	//
	// ⚠️ DuckDB 的 LIKE 大小写敏感(实测 "Face" 命中、"face" 不命中),
	// 所以商品名里有拉丁品牌名(洁柔Face / C&S)时,用户打出小写就搜不到。
	// 要大小写无关用 [OperatorContainsCI]。
	OperatorContains = "contains"
	// OperatorContainsCI 子串匹配,**大小写无关**(2026-10-20 cube 新增)。
	//
	// cube 侧把列与模式都转小写再比;模式在**编译期** lower,值仍走参数绑定。
	// 只影响 ASCII 字母,中文与数字原样通过。
	OperatorContainsCI = "containsCI"
	// OperatorIn 集合成员(2026-10-22 新增)。
	//
	// ⚠️ cube 的 `in` **至少要一个值**(空集合直接 400)⇒ 空集合要用
	//   `equals` + 一个不可能为真的哨兵值表达(见 emptySetSentinel)。
	OperatorIn = "in"
)

// invoke 发一次 POST 调用(带 JSON body),返回原始响应体。
//
// ⚠️ 与 loadCubeQuery 的差别:那个方法把 body 固定成 cube 查询的形状
// (measures / dimensions / filters),而这里要发的是**别的端点**
// (如 cube-router 的 /v1/branch-source)⇒ 不能复用它的 body 构造。
//
// JWT / X-Branch-ID 的透传与 loadCubeQuery **完全一致**
// (否则被调方拿不到调用者身份 ⇒ scope 守门会 403)。
func (c *DaprCubeClient) invoke(ctx context.Context, method string, body any) ([]byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("cubeclient: 序列化请求体失败: %w", err)
	}
	if bearer := bearerFromCtx(ctx); bearer != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", bearer)
	}
	if bid := branchFromCtx(ctx); bid != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-branch-id", bid)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.dapr.InvokeMethodWithContent(ctx, c.appID, method, "POST",
		&dapr.DataContent{ContentType: "application/json", Data: b})
	if err != nil {
		return nil, classifyCubeError(err)
	}
	return resp, nil
}

// LoadCubeQuery 调 cube /v1/load,返回 data[] 数组。
//
// 每行 key 形如 "<model>.<field>",例如 "product.id"、"stock.total_quantity"。
//
// 错误映射:
//   - gRPC NotFound → ErrCubeNotFound(各 GetX 方法 wrap 成具体 error)
//   - 其它错误 → 普通 wrap error
//
// 响应顶层的 total 本方法**不返回** —— 需要它的调用方用 LoadCubeQueryPage。
func (c *DaprCubeClient) LoadCubeQuery(ctx context.Context, modelName string, q CubeQuery) ([]map[string]any, error) {
	data, _, err := c.loadCubeQuery(ctx, modelName, q)
	return data, err
}

// LoadCubeQueryPage 调 cube /v1/load,返回 data[] **和**响应顶层的 total。
//
// total 只有在 CubeQuery.Total = true 时才是真值(不给时 cube 恒返 0 ——
// 那是"没问",不是"总共 0 条")。
func (c *DaprCubeClient) LoadCubeQueryPage(ctx context.Context, modelName string, q CubeQuery) ([]map[string]any, int, error) {
	return c.loadCubeQuery(ctx, modelName, q)
}

// loadCubeQuery 是两个 Load* 方法的共同实现。
//
// body 的字段是**按需**加的:offset / order / total 只在真的要用时才写进去,
// 这样既有调用方(GetProduct / GetStock / SearchSuppliers / GetActivePromotion
// 以及 salesagg / salesinsight / settlement-auditor 的查询)发出去的 body
// 与升级前**逐字节相同**,不会因为"多了一个字段"改变下游行为。
func (c *DaprCubeClient) loadCubeQuery(ctx context.Context, modelName string, q CubeQuery) ([]map[string]any, int, error) {
	body := map[string]any{
		"measures":   q.Measures,
		"dimensions": q.Dimensions,
		"filters":    q.Filters,
	}
	if q.Limit > 0 {
		body["limit"] = q.Limit
	}
	if q.Offset > 0 {
		body["offset"] = q.Offset
	}
	if len(q.Order) > 0 {
		body["order"] = q.Order
	}
	if q.Total {
		body["total"] = true
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

	resp, err := c.dapr.InvokeMethodWithContent(ctx, c.appID, c.queryPath, "POST",
		&dapr.DataContent{ContentType: "application/json", Data: b})
	if err != nil {
		return nil, 0, classifyCubeError(err)
	}
	var out struct {
		Data []map[string]any `json:"data"`
		// Total 是"不受 limit/offset 影响的总行数"。
		//
		// ⚠️ 不给 total:true 时 cube 恒返 0 —— 那是"没问",不是"总共 0 条"。
		// 调用方必须自己知道有没有问。
		Total int `json:"total"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, 0, fmt.Errorf("cube decode: %w", err)
	}
	return out.Data, out.Total, nil
}

// ---- Client interface 实现 ----

// GetProduct 查 cube product(按 item_no)。
func (c *DaprCubeClient) GetProduct(ctx context.Context, productID string) (*ProductDTO, error) {
	data, err := c.LoadCubeQuery(ctx, "product", CubeQuery{
		// 2026-10-08 加 avg_price_yuan:之前只查 product.count,导致
		// SearchProductRow.Price 永远是 nil → wx-h5 显示「商品当前售价 -」。
		//
		// 为什么用 AVG 而不是新加一个 price 维度:
		//   - price_yuan 列**已经在 cube 的 product 表里**(mapping sale_price→price_yuan),
		//     且 schema 已有 avg/min/max_price_yuan 三个 measure,不用改 cube 也不用重新拉数据;
		//   - 这里按 product.id 精确命中**单行**,AVG/MIN/MAX 在单行上恒等于该值,
		//     所以 AVG(price_yuan) 就是"这个商品的售价",不存在聚合失真;
		//   - 真要一个 price 维度就得改 sixun-models/product/schema.yaml(共享 schema,
		//     影响 ysx + hbposv7 两个 family),为同一件事付两倍代价,不划算。
		Measures: []string{"product.count", "product.avg_price_yuan"},
		Dimensions: []string{"product.id", "product.name", "product.category_id",
			"product.supplier_id", "product.unit", "product.status"},
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
		// Unit: cube product.unit(2026-10-09 起由 t_bd_item_info.unit_no 映射而来)。
		// cube 侧 dimension 与 ysx / hbposv7 两个 family 的 mapping 已同时补齐,
		// 所以这里不再恒为空。仍保留空值:老数据 / 该商品未维护单位时 wx-h5 回退显示「件」。
		Unit:   asStr(row["product.unit"]),
		Status: asStr(row["product.status"]),
		// PriceYuan 走指针:nil = cube 没给售价(旧数据 / 该商品未维护价),
		// 与"售价为 0"是两件事,前端要能区分(0 元商品 vs 没维护价)。
		PriceYuan: asDecimalPtr(row["product.avg_price_yuan"]),
		// Barcode = product.id(2026-10-15 修正)。
		//
		// 此前这里**恒为空**,而 catalog 的 /products/search 直接把它当响应字段
		// 返回 —— 于是该字段对每个商品都是 ""。前端扫码拿到的商品没有条码,
		// 只能回显自己输入的那串;一旦走了列表选择而非扫码,就彻底对不上号。
		//
		// 依据是本包 SearchProductsByBarcode 里那条已确认的领域事实:
		// **item_no 就是条码**(实测 6957583900828 → item_no 同值)。
		// 所以 barcode 与 id 同源,填上它不会引入新的假设。
		Barcode: productID,
	}, nil
}

// GetStock 查该门店 cube 实例里的商品库存快照(按 product_id)。
//
// ⚠️ 这里**刻意不加 branch 过滤**,原因是一次真实故障(2026-10-08):
//
//	cube 的 stock.branch_id 来自思迅 t_im_branch_stock.branch_no,实测值 "0001";
//	supertrade 的 branch_id 是 "00"。两者是**完全无关的两套编码**
//	(业务方确认:思迅 branch_no 与 branch_id 没有任何联系,必须忽略)。
//	原先把 "00" 塞进 stock.branch_id 过滤,结果永远匹配不上 ——
//	每个商品都报「该分店下的商品 stock 不存在(疑似跨店盘点)」,
//	而实际该商品在本店有 261 件库存。
//
// 门店隔离**不靠这个维度**,而靠 X-Branch-ID 路由链:
//
//	X-Branch-ID → cube-router → branch_cube_sources(branch_id → cube_source_name)
//	            → 该门店专属的 cube 实例 → 其 DuckDB 即该门店的数据
//
// 一个 cube 实例 = 一家门店(cube wire source id = [family]-[version]-[store])。
// 所以到达这里的查询天然只含本店数据,再按 branch_no 过滤既多余又会失配。
// 商品查询(GetProduct / SearchProductsByBarcode)本来就没有门店过滤,stock 保持一致。
//
// "跨店阻断"仍然成立:本店 cube 里没有该商品的库存记录时,照样返回 ErrStockNotFound。
func (c *DaprCubeClient) GetStock(ctx context.Context, branchID, productID string) (*StockSnapshotDTO, error) {
	data, err := c.LoadCubeQuery(ctx, "stock", CubeQuery{
		Dimensions: []string{"stock.product_id"},
		Measures:   []string{"stock.total_quantity", "stock.avg_cost"},
		Filters: []CubeFilter{
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
		ProductID: asStr(row["stock.product_id"]),
		// 填 supertrade 的 branch_id(路由键),不是 cube 返回的 stock.branch_id ——
		// 后者是思迅 branch_no,与本系统门店无关,填进去只会误导排查。
		BranchID:    branchID,
		Quantity:    asDecimal(row["stock.total_quantity"]),
		AvgCostYuan: asDecimal(row["stock.avg_cost"]),
		UpdatedAt:   time.Now().UTC(), // cube stock.updated_at 本期未拉(简化)
	}, nil
}

// SearchProductsByBarcode 按 barcode 查商品,合并该门店 stock。
//
// 把 barcode 当 item_no 精确查(cube product.id = 思迅 item_no)。
// 长度 < 3 时返空(防全表扫 + 防无效输入)。
//
// 领域事实(2026-10-08 与业务方确认 + 思迅 hbposv10 实测,勿再推翻):
//
//  1. **item_no 就是条码。** 门店实际扫出来的东西绝大多数直接等于 item_no,
//     包括部分生鲜的自建短码。例:6957583900828 →
//     t_bd_item_info.item_no = "6957583900828",item_name = "凤派XL码抽纸8包"。
//     它是商品编码,不是 EAN 条码 —— 早期把它当成"缺 barcode 维度"是误判。
//  2. t_bd_item_barcode.item_barcode 是**一品多码**补充表,不是主码表。
//     实测全库仅 152 行 / 覆盖 108 个商品(占 27299 个商品的 0.4%),
//     一个商品可以挂多条(示例:02000089 有 3 条)。
//     因此它**不能**用来替代 id 维度,也不该被平铺进 product ——
//     1:N 平铺会把商品行放大约 1.4 倍,污染 count / avg_price_yuan 这类 measure。
//
// 结论:本期保持"barcode == item_no"的单查语义。
// 日后若真要支持一品多码检索,正确形态是独立 barcode model,
// 而不是往 product 上加维度 —— 且 cube 查询引擎目前尚未消费 cross-model join
// (Joins 仅在 cubeschema 里被解析),该能力落地前做不了。
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
		// ⚠️ 这里只吞"确实没有这个商品"。链路故障(Unimplemented / Unavailable /
		// 超时)必须原样上抛 —— 吞掉它就等于把一次 cube 故障报告成
		//「本门店没有条码 XXX 的商品」,现场无法区分"商品没入库"和"系统挂了"。
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
		Measures: []string{"supplier.count"},
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

// GetActivePromotion 查 itemNo 在 onDate 当天(或今天)生效的促销,返回**最低价**那一条。
//
// # 为什么时间窗用 inDateRange 而不是 gte / lte(踩过的坑)
//
// begin_date / end_date 是真 datetime。cube 的 gte / lte 编译成
//
//	RTRIM(col) >= ?      ← 字符串比较
//
// 对 datetime 做字符串比较恒不匹配,**返回 0 行且不报错**。
// inDateRange 走的是另一条路,它会把列 CAST 成 DATE 再比:
//
//	CAST(RTRIM(begin_date) AS DATE) >= @p1
//
// 2026-10-15 真机实测:同一张表同一批数据,gte/lte 返 0 行、inDateRange 返 731 行。
//
// "活动期包含今天"要拆成两个半开判断,因为 inDateRange 是**闭区间**:
//
//	begin_date <= today   →  inDateRange ['1900-01-01', today]
//	end_date   >= today   →  inDateRange [today, '2999-12-31']
//
// 下界用 1900-01-01 / 上界用 2999-12-31 只是"够宽的哨兵",不代表业务边界。
//
// # 为什么在 Go 里挑最低价而不是让 cube 挑
//
// cube 侧 GROUP BY 已含 plan_no / begin_day / end_day,所以**每个促销单一行**,
// 每行各自带 min_promo_price(单行上 MIN 恒等于该值)。
// 同一商品被多单覆盖时(实测存在),哪个单生效是业务规则,所以在 Go 里排序取最小。
func (c *DaprCubeClient) GetActivePromotion(ctx context.Context, itemNo, onDate string) (*PromotionDTO, error) {
	day := onDate
	if day == "" {
		day = time.Now().Format("2006-01-02")
	}
	data, err := c.LoadCubeQuery(ctx, "promotion", CubeQuery{
		// min_promo_price 是 measure —— cube 不给 measure 包 RTRIM(否则破坏聚合),
		// 所以它是**真的 number**,不像维度那样以字符串回来。
		Measures: []string{"promotion.count", "promotion.min_promo_price"},
		Dimensions: []string{
			"promotion.item_no", "promotion.plan_no", "promotion.plan_name",
			"promotion.orig_price", "promotion.begin_day", "promotion.end_day",
		},
		Filters: []CubeFilter{
			{Member: "promotion.item_no", Operator: "equals", Values: []any{itemNo}},
			{Member: "promotion.begin_date", Operator: "inDateRange",
				Values: []any{"1900-01-01", day}},
			{Member: "promotion.end_date", Operator: "inDateRange",
				Values: []any{day, "2999-12-31"}},
		},
		// 多促销单时全取回来在 Go 里挑,给足余量。
		Limit: 20,
	})
	if err != nil {
		// ⚠️ 只把"确实没有该 model 的数据"降级成无促销;链路故障原样上抛。
		if errors.Is(err, ErrCubeNotFound) {
			return nil, nil
		}
		return nil, err
	}

	var best *PromotionDTO
	for _, row := range data {
		cand := &PromotionDTO{
			ItemNo:    asStr(row["promotion.item_no"]),
			PlanNo:    asStr(row["promotion.plan_no"]),
			PlanName:  asStr(row["promotion.plan_name"]),
			Price:     asDecimal(row["promotion.min_promo_price"]),
			OrigPrice: asDecimalPtr(row["promotion.orig_price"]),
			BeginDay:  asStr(row["promotion.begin_day"]),
			EndDay:    asStr(row["promotion.end_day"]),
		}
		// 促销价 <= 0 的单等于"没打折",不进候选 —— 否则一条 0 元促销
		// 会盖掉真正有效的促销,并让价签印出 ¥0.00。
		if cand.Price.LessThanOrEqual(decimal.Zero) {
			continue
		}
		if best == nil || cand.Price.LessThan(best.Price) {
			best = cand
		}
	}
	// 没有候选 = 该商品当前没有生效促销。这是正常业务结论,不是错误。
	return best, nil
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

// asInt 把 cube 返回的计数字段转成 int。
//
// 为什么需要它:duckdb 驱动返回的数值类型不统一(int64 / float64 / []byte /
// json.Number 都见过),直接 type assert 会静默拿到 0 —— 而 0 在"按分类统计
// 商品数"这种场景里意味着"这个分类有 0 个商品",是个**看起来正常的错数**。
func asInt(v any) int {
	switch x := v.(type) {
	case nil:
		return 0
	case int:
		return x
	case int32:
		return int(x)
	case int64:
		return int(x)
	case float64:
		return int(x)
	case json.Number:
		i, err := x.Int64()
		if err != nil {
			return 0
		}
		return int(i)
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(x))
		if err != nil {
			return 0
		}
		return i
	}
	return int(ParseDecimal(v).IntPart())
}

// asDecimalPtr 与 asDecimal 的区别:缺失(nil)时返回 nil 而不是 decimal.Zero。
//
// 用于「售价」这类**缺失有语义**的字段 —— 商品没维护价格(nil)与
// 商品确实是 0 元(Zero)在前端要显示成不同的话,合并成 Zero 就丢了这个信息。
func asDecimalPtr(v any) *decimal.Decimal {
	if v == nil {
		return nil
	}
	d := ParseDecimal(v)
	return &d
}

// asInt64 把 cube 返回的整数字段转成 int64。
//
// ⚠️ 与 asInt 同款理由(驱动返回的类型不统一),但**计数类**字段要 int64:
// 会员的消费次数 / 限用次数在思迅是 int,超过 int32 的库(实测 999999 哨兵)
// 用 int32 会溢出成负数 —— 那是一个"看起来正常"的错数。
func asInt64(v any) int64 {
	switch x := v.(type) {
	case nil:
		return 0
	case int:
		return int64(x)
	case int32:
		return int64(x)
	case int64:
		return x
	case float64:
		return int64(x)
	case json.Number:
		i, err := x.Int64()
		if err != nil {
			return ParseDecimal(v).IntPart()
		}
		return i
	case string:
		i, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		if err != nil {
			return ParseDecimal(v).IntPart()
		}
		return i
	}
	return ParseDecimal(v).IntPart()
}

// asTimePtr 把 cube 返回的时间字段转成 *time.Time(nil = 没维护)。
//
// ⚠️ 源库的时间列有三种形态:时间戳字符串 / RFC3339 / 空串 ⇒ 全收。
func asTimePtr(v any) *time.Time {
	if v == nil {
		return nil
	}
	s := strings.TrimSpace(asStr(v))
	if s == "" {
		return nil
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999999",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return &t
		}
	}
	return nil
}

// ParseDecimal 把 cube 返回的任意数值字段转为 decimal.Decimal(导出,测试可见)。
//
// ⚠️ **读不懂的值一律当 0** —— 调用方若需要区分"0"与"读不懂",
// 必须用 ParseDecimalOK(见下)。把脏值静默当 0,会让"售价 0 元"混进
// 商品档案,而 POS 会照印出去。
func ParseDecimal(v any) decimal.Decimal {
	d, _ := ParseDecimalOK(v)
	return d
}

// ParseDecimalOK 与 ParseDecimal 同语义,但**把"读不懂"与"0"区分开**。
//
// # 为什么需要它
//
// MSSQL live 源(思迅直查)的 decimal 列经 go-mssqldb 以 []byte 返回,
// encoding/json 把 []byte 序列化成 **base64**:
//
//	product.avg_price_yuan = "MTIuMDAwMDAw"  → base64 → "12.000000"
//
// ⇒ 导出端若只按"字面数字"解析,就会把 base64 串**原样带出**
// ⇒ 导入端解析失败 ⇒ 那一行被当成"没维护价"跳过
// ⇒ **全量价格一行都没落库,而报告只写"skipped 27569"**,零报错。
//
// # 判据
//
//  1. nil        ⇒ (0, false)—— 调用方据此输出 null 而不是 "0"
//     ("没维护价"与"价是 0"必须能区分)
//  2. 明文数字   ⇒ (该数, true)
//  3. base64 串  ⇒ 解码后是数字 ⇒ (该数, true)
//  4. 其它       ⇒ (0, false)—— **绝不**静默当 0
func ParseDecimalOK(v any) (decimal.Decimal, bool) {
	if v == nil {
		return decimal.Zero, false
	}
	switch x := v.(type) {
	case float64:
		return decimal.NewFromFloat(x), true
	case float32:
		return decimal.NewFromFloat32(x), true
	case int:
		return decimal.NewFromInt(int64(x)), true
	case int64:
		return decimal.NewFromInt(x), true
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return decimal.Zero, false
		}
		if d, err := decimal.NewFromString(s); err == nil {
			return d, true
		}
		// MSSQL live 源(sixun 直查,如 settlement/stock/sale_day/product)的 decimal 经
		// go-mssqldb 以 []byte 返回,encoding/json 把 []byte 序列化成 base64
		// (如 SUM(sheet_amt) "NDY0OTIuMDIwMA==" = "46492.0200")。
		// 直接 NewFromString 失败时按 base64 解码再试;纯数字串在第一分支就
		// 命中,不会被误当 base64。
		if raw, berr := base64.StdEncoding.DecodeString(s); berr == nil {
			if t := strings.TrimSpace(string(raw)); t != "" {
				if d, nerr := decimal.NewFromString(t); nerr == nil {
					return d, true
				}
			}
		}
		return decimal.Zero, false
	case json.Number:
		d, err := decimal.NewFromString(x.String())
		if err != nil {
			return decimal.Zero, false
		}
		return d, true
	}
	return decimal.Zero, false
}
