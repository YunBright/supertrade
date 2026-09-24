# DESIGN — 微服务架构与技术方案

> **本文件与 `docs/REQUIREMENTS.md`、`docs/FIELD_SPEC.md` 必须保持一致**。
> 任何修改其中之一,另两份需在同一 PR 同步。

---

## §0 总览

- **技术栈**:Go 1.26+ / Gin / GORM / PostgreSQL / Redis
- **微服务编排**:Dapr(本地 `dapr init` + `dapr run`,无 docker)
- **身份 / 权限**:`F:\go\src\github.com\YunBright\auth`(userd) + `github.com/YunBright/authkit`
- **ERP 数据源**:`F:\go\src\github.com\YunBright\cube`(只读,不复制其语义层)
- **LLM 接入**:`llm-gw` 统一调用智谱 / DeepSeek(本地 `pkg/llm` 可直调,后续接 llm-gw)

### 三大原则

1. **事件驱动**:跨服务状态变更一律走 Dapr pub/sub,无强同步事务
2. **数据契约唯一**:`docs/FIELD_SPEC.md` 字段映射 + `docs/REQUIREMENTS.md` 业务规则
3. **基础能力不重复造**:**身份/权限完全委托 auth + authkit**(本系统只写业务代码)

---

## §1 微服务拆分(16 个 dapr app)

```
                      ┌──────────────────────────────────────────┐
                      │       外部 / 前端 / BI 工具接入            │
                      │   pos-gateway (Gin, BFF)                 │
                      │   bi-gateway   (调用 cube-gateway /v1/load) │
                      └────────────────┬─────────────────────────┘ │
        ┌──────────────────────────────┼──────────────────────────────┐
        │                              │                              │
   ┌────▼─────┐  ┌──────────────┐  ┌────▼─────┐  ┌──────────────┐  ┌───▼────┐
   │ catalog │  │  inventory   │  │   pos    │  │ procurement  │  │ pricing│
   │   商品    │  │   库存/批次  │  │  收银     │  │   采购       │  │  定价   │
   └──────────┘  └──────────────┘  └──────────┘  └──────────────┘  └───────┘
        │                              │                              │
        │       ┌──────────────────────┼──────────────────────┐       │
        │       │                      │                      │       │
        │  ┌────▼─────┐  ┌─────────┐  ┌▼──────────┐  ┌───────▼──┐    │
        │  │  fresh-  │  │ fresh-  │  │ stocktake │  │ erp-     │    │
        │  │ produce  │  │  meat   │  │   盘点    │  │ connector│    │
        │  │ 生鲜蔬果 │  │  生肉   │  └───────────┘  │  (cube)  │    │
        │  └──────────┘  └────┬────┘                  └───┬───────┘    │
        │                     │ LLM                      │            │
        │                     ▼                          ▼            │
        │               ┌───────────┐            ┌───────────────┐    │
        │               │  llm-gw   │            │  sales-agg    │    │
        │               │ 智谱/DS │            │ 多端销售聚合  │    │
        │               └───────────┘            └───────────────┘    │
        │                                                          │
        └──────────────────────┬───────────────────────────────────┘
                               │
                  ┌────────────▼────────────┐
                  │ master-data  主数据     │
                  │ notification 通知       │
                  └───────────────────────────┘

身份/权限:外部 YunBright/auth 项目的 userd(JWT 签发) + authkit 公开包(claims + rbac + userinfo)
```

| # | dapr app-id | 职责 | 主要数据 | 关键依赖 |
|---|---|---|---|---|
| 1 | **pos-gateway** | BFF,前端对接,聚合调用 | 路由 | Dapr invoke |
| 2 | **catalog** | **本地 suppliers/products 表(带 branch_id)+ cube 兜底**(REQUIREMENTS §7.5) | suppliers, products(本地) | cube-gateway(兜底), userd(scope) |
| 3 | **inventory** | **代理 cube `stock` 读取,本系统不维护库存**(REQUIREMENTS §7.5) | — | cube-gateway(经 cube-router) |
| 4 | **procurement** | 采购订单、收货、退货、账期(本系统自营) | purchase_orders, grns | cube-gateway(查 SKU/供应商) |
| 5 | **pos** | 销售开单、收款、改价、退货、班次(本系统自营) | sales_orders, sale_lines, payments | cube-gateway(查 SKU) |
| 6 | **pricing** | 售价/促销/会员/改价审批(本系统自营) | price_lists, promotions | cube-gateway |
| 7 | **fresh-produce** ⭐ | 蔬果盘点驱动毛利(REQUIREMENTS §3) | produce_stocktake, waste_logs | cube-gateway, stocktake |
| 8 | **fresh-meat** ⭐ | 生肉整猪 + LLM 分割(REQUIREMENTS §4) | whole_pig, pig_cuts, pork_cuts_stocktake | llm-gw, cube-gateway |
| 9 | **stocktake** ⭐ | **盘点表 + 盘点明细 CURD + 差异表生成**(REQUIREMENTS §2.1,本期重点) | stocktake_headers, stocktake_lines | cube-gateway |
| 10 | **erp-connector** | 拉 cube /v1/load(REQUIREMENTS §5) | erp_sales_raw, sync_logs | cube-gateway |
| 11 | **sales-agg** | 聚合 POS + erp-connector,供 BI / 跨服务拉 | sales_view | pos pub/sub, erp-connector |
| 12 | **llm-gw** | LLM 路由 + prompt 模板 + 缓存 + 降级 | llm_call_logs | 智谱 / DeepSeek |
| 13 | **master-data** | 门店/员工/班次(**供应商/客户走 catalog 本地表**) | stores, employees | cube-gateway |
| 14 | **bi-gateway** | BI 出口,调 cube-gateway + 本系统聚合 | — | cube-gateway |
| 15 | **notification** | 企微 / 钉钉 / 短信通知 | notification_logs | — |
| 16 | **cube-router** ⭐ | **按 X-Branch-ID 路由 POST /v1/load 到正确 cube 实例(sixun-hbposv7 / sixun-ysx)** | branch_cube_sources | dapr invoke → cube 实例, userd(scope) |

> **身份/权限不归本系统**:`auth` 项目的 userd 提供 JWT 签发 + 用户/角色/权限 CRUD;
> 本系统每个 dapr app 通过 `authkit` 中间件鉴权。**不写 iam 服务**。

### 1.1 本期数据归属速查(REQUIREMENTS §7.5)

| 数据 | 归属 | 服务 |
|---|---|---|
| **SKU / 商品** | **catalog 本地表 + cube 兜底** | `catalog.GET /products/search` 等 |
| 实时库存 | cube-gateway 经 cube-router 转发 | `inventory.GET /stock/:branch_id/:product_id` |
| **供应商 / 客户** | **catalog 本地表 suppliers(type=0/1)+ cube 兜底** | `catalog.GET /suppliers` 等 |
| 销售明细(思迅) | erp-connector 定期拉 → `erp_sales_raw` | `erp-connector`(订阅 `erp.sale.ingested`) |
| **盘点表 + 盘点明细** | **本系统自维护** | `stocktake`(本期重点) |
| POS 开单(本系统) | 本系统自维护 | `pos` |
| 采购收货(本系统) | 本系统自维护 | `procurement` |
| 蔬果 / 生肉特有表 | 本系统自维护 | `fresh-produce` / `fresh-meat` |
| 门店 / 员工 / 班次 | 本系统自维护 | `master-data` |
| **branch ↔ cube 映射** | **本系统自维护** | **cube-router / admin / branch-cube-sources** |

---

## §2 Dapr 组件与本地启动

### 2.1 本地启动(`dapr init` + `dapr run`,无 docker)

```bash
# 一次性:dapr standalone + redis + 默认 components
dapr init
# 启单 dapr app
dapr run --app-id catalog --app-port 8080 -- go run ./cmd/catalog
```

> 详见 `docs/RUNBOOK.md` 中的"本地启动顺序"小节(后续补;无 make 文件)。

### 2.2 Dapr 组件(每 dapr app 共享 `~/.dapr/components/`)

```yaml
state.redis.yaml      # state store:购物车/分布式锁/会话缓存
pubsub.redis.yaml     # pub/sub: 领域事件总线
bindings.cron.yaml    # cron: 每日盘点提醒 / erp 轮询
secret.local.yaml     # secret: LLM API Key 等(本地 file,生产换 kubernetes)
```

### 2.3 关键事件 topic

| topic | 发布方 | 订阅方 | payload 关键字段 |
|---|---|---|---|
| `inventory.stock.moved` | inventory | fresh-produce, fresh-meat, sales-agg | sku, qty, move_type, ref_no |
| `purchase.completed` | procurement | inventory, fresh-produce | grn_id, lines[] |
| `sale.completed` | pos | inventory, fresh-meat, sales-agg | sale_id, branch_id, lines[] |
| `stocktake.completed` | stocktake | inventory, fresh-produce, fresh-meat | task_id, type |
| `pig.arrived` | fresh-meat | llm-gw(异步分析) | pig_id, gross_weight_kg |
| `pig.stocktaken` | fresh-meat | llm-gw(异步分析) | stocktake_id, variance_kg |
| `pig.analysis.completed` | fresh-meat | notification, dashboard | pig_id, analysis_json |
| `erp.sale.ingested` | erp-connector | sales-agg | source, batch_id, count |
| `sale.aggregated` | sales-agg | bi-gateway + 其它 dapr app | branch_id, period, kpis |

### 2.4 Dapr 调用关系(节选)

```
pos-gateway ─invoke─▶ pos ─invoke─▶ inventory (锁库/释放)
                       └─pub─▶ sale.completed ─▶ inventory 扣减
                                             ─▶ fresh-meat 落 line_sales_by_pig
                                             ─▶ sales-agg 聚合

fresh-meat ─invoke─▶ llm-gw (ChatCompletion, json_mode)
erp-connector ─invoke─▶ cube-gateway (/v1/load)
bi-gateway ─invoke─▶ cube-gateway (BI 走 cube 自家的,不经本系统 cube 兼容层)
```

---

## §3 auth + authkit 集成

### 3.1 本系统每个 dapr app 的 handler 链(2026-09 重构后)

每个 dapr app 的请求链(由 `pkg/cmdbootstrap` + `pkg/middleware` + `authkit/rbac` 组合):

```go
// cmd/catalog/main.go 示意(2026-09 重构后)
import (
    "github.com/YunBright/authkit/claims"
    "github.com/YunBright/authkit/rbac"
    "github.com/YunBright/authkit/userinfo"
    "github.com/YunBright/supertrade/pkg/middleware"
)

users := userinfo.New("userd")
r := gin.New()
r.Use(claims.GinMiddleware())              // 1. 解析 claims 到 ctx(Dapr sidecar 已验签)
r.Use(rbac.RequireAudience("catalog"))     // 2. aud 必须包含本服务
r.Use(middleware.XBranchID())              // 3. 解析 X-Branch-ID header 到 ctx(全局挂)

// 三元权限中间件:每个业务端点都挂
suppliers := r.Group("/suppliers")
suppliers.GET("",
    rbac.RequireScopeWithBranch("supplier:view",
        middleware.BranchFromCtx,                  // branchFn
        rbac.HasAnyScopeWithBranch(users)),        // userinfo per-branch 矩阵
    supplierHandler.List,
)
suppliers.POST("",
    rbac.RequireScopeWithBranch("supplier:manage",
        middleware.BranchFromCtx,
        rbac.HasAnyScopeWithBranch(users)),
    supplierHandler.Create,
)
```

**关键变化(2026-09 重构后)**:
- ❌ **移除** `rbac.RequireScope("catalog.read")` 这类 JWT-static scope 中间件
  (auth 在 2026-09-23 把 JWT `scopes` 字段清空,static helper 永远 false)
- ✅ **新增** `rbac.RequireScopeWithBranch(scope, branchFn, resolver)` —— 三元 `user × branch × scope`
- ✅ **新增** `rbac.HasAnyScopeWithBranch(users *userinfo.Client)` resolver 适配器
  (内部调 `users.GetBranchPermissions(ctx, sub, branchID)` 读 userd per-branch 矩阵)
- ✅ **统一** branch 来源:`pkg/middleware.XBranchID()` 把 `X-Branch-ID` header 注入 ctx
  (catalog / cube-router / stocktake 大多数端点统一走 header;path `:branch_id` 用 `c.Param`,
   body `branch_id` 用 `ShouldBindJSON` 后字段)

### 3.2 分店隔离(数据级权限)

`authkit` 0.2+ 已提供 `rbac.RequireBranch(branchFn, allowedFn)` 中间件 + `claims.GetEffectiveBranches()` /
`IsAllowedBranch()` helper。本系统直接使用:

```go
branches := r.Group("/branches/:branch_id")
branches.GET("/sales",
    rbac.RequireBranch(
        func(c *gin.Context) (string, bool) {
            id := c.Param("branch_id")
            return id, id != ""
        },
        func(c *gin.Context) ([]string, bool) {
            cl, ok := claims.FromContext(c.Request.Context())
            if !ok {
                return nil, false
            }
            return cl.GetEffectiveBranches(), true
        },
    ),
    handler.ListSales,
)
```

> 静态版从 claims 读(轻量、零额外调用);
> 动态版从 userinfo.GetEffectivePermissions 读(支持权限变更即时生效),具体见 §3.4。

### 3.3 一次拿全用户有效权限

```go
users := userinfo.New("userd")
p, err := users.GetEffectivePermissions(ctx, claims.MustFromContext(ctx).Sub, tenantID)
if errors.Is(err, userinfo.ErrPermissionsUnavailable) {
    // userd 暂未提供此端点,降级到 Get + User.Scopes 自行聚合
    u, _ := users.Get(ctx, sub)
    p = &userinfo.EffectivePermissions{Scopes: u.Scopes, Roles: u.Roles}
}
```

> 调用方应缓存 `(userID, tenantID) → Scopes` 以减少 userd 压力。

### 3.4 authkit 集成清单(2026-09-24 已全部合并)

REQUIREMENTS §1.4 列的 6 项 authkit 需求已全部合并:
- `rbac.RequireBranch(branchFn, allowedFn)` —— 静态版(高频 / JWT snapshot)
- `rbac.RequireScopeWithBranch(scope, branchFn, resolver)` —— 三元权限核心(走 userd per-branch)
- `rbac.HasAnyScopeWithBranch(users)` —— resolver 适配器
- `userinfo.GetBranchPermissions(ctx, uid, branchID)` —— per-branch 矩阵
- `claims.GetEffectiveBranches()` / `IsAllowedBranch()` —— JWT snapshot helpers
- `auth.user.access_changed` 合并事件 —— 权限失效广播,前端订阅重新签 token

---

## §4 数据模型关键表(每 dapr app 一个 schema)

> 字段定义以 `docs/FIELD_SPEC.md` §1 为准。
> **本期变化**:SKU / 分类 / 实时库存 / 供应商 / 客户 / 销售明细(思迅源) **不维护本地表**,
> 全部从 cube-gateway 转发(见 REQUIREMENTS §7.5)。
> 以下仅列出**本系统自己拥有的表**(非 cube 已有)。

```
procurement.purchase_orders / grns
pos.sales_orders        / pos.sale_lines / pos.payments
pricing.price_lists     / promotions
fresh_produce.produce_stocktake
fresh_produce.waste_logs            (可选录入)
fresh_produce.profit_periods        (盘点节点产出)
fresh_meat.whole_pig                (早盘录入,一头一行)
fresh_meat.pig_cuts                 (部分追加条码)
fresh_meat.pork_cuts_stocktake      (整店按部位盘点,可选)
fresh_meat.line_sales_by_pig        (按猪聚合的销售)
fresh_meat.llm_runs                 (LLM 调用留痕)
stocktake.stocktake_headers         (盘点单据头)
stocktake.stocktake_lines           (盘点单据行,录入实盘)
erp_connector.erp_sales_raw         / sync_logs   (cube 拉取的思迅销售明细缓存)
sales_agg.sales_view_minute / sales_view_daily
master_data.stores / employees      (cube 无,本系统维护)
```

> **stocktake 表**(本期重点):见 §4.5;其余 cube 已有数据(products / stock / suppliers)
> 本系统**不创建表**,由 catalog / inventory / master-data 服务按需通过 Dapr service invocation
> 调 cube-gateway `/v1/load` 取。

**约束**:每 dapr app 一个 PostgreSQL schema,跨服务**禁止直连 DB**;
跨域查询走 sales-agg 的视图 API 或同步落本地宽表。

### 4.5 stocktake 服务详情(REQUIREMENTS §2.1,本期重点):实时盘点(营业中)

> **核心语义**:**营业中盘点(非锁库)** —— 盘点不锁库,POS 正常销售;
> 录入每行时按条码**实时拉 cube sixun 库存作快照**,然后录入实盘;
> 整单差异 = `∑(actual - snapshot)`。
> **盘点单不跨店**。

#### 4.5.1 端点列表

| 方法 | 路径 | 作用域 | 说明 |
|---|---|---|---|
| GET | `/healthz` | 公开 | 标准健康检查(cmdbootstrap 注入) |
| GET | `/products` | cube 转发 | 录入页 SKU 列表 → `cube-gateway /v1/load product` |
| GET | `/stock` | cube 转发 | 录入页账面库存 → `cube-gateway /v1/load stock` |
| **GET** | **`/api/v1/products/search`** | **cube 聚合 + 权限过滤** | **扫条码查商品(对齐 scan.html 后端 SearchProducts,REQUIREMENTS §2.1.4.1)** |
| POST | `/stocktake-headers` | 本系统 | **创建空盘点表**(不预填 SKU) |
| GET | `/stocktake-headers/:id` | 本系统 | 查盘点表(含 lines) |
| PATCH | `/stocktake-headers/:id` | 本系统 | 改状态 / 备注 |
| POST | `/stocktake-headers/:id/lines` | 本系统 | **按条码逐行录入**(每行实时拉 cube 快照作 book_qty) |
| PUT | `/stocktake-lines/:id` | 本系统 | 改 actual_qty / diff_reason(重算 diff_qty / diff_amount_yuan) |
| DELETE | `/stocktake-lines/:id` | 本系统 | 删单条明细(counting 状态) |
| GET | `/stocktake-headers/:id/diff-report` | 本系统 | **生成差异表**(任意状态可调) |
| POST | `/stocktake-headers/:id/submit` | 本系统 | `counting → adjusted`(冻结差异) |
| POST | `/stocktake-headers/:id/approve` | 本系统 | `adjusted → approved`(需 auditor_id) |

#### 4.5.2 创建盘点表 = 只创建空 header

```go
// 伪代码(stocktake 服务,POST /stocktake-headers)
func CreateHeader(ctx, input) (*StocktakeHeader, error) {
    h := &StocktakeHeader{
        ID:        nextID("ST"),               // ST<yyyymmdd><seq>
        BranchID:  input.BranchID,             // 整单锁定,创建后不可改
        CountDate: input.CountDate,
        Type:      input.Type,                 // general / produce
        OperatorID: ctx.Claims.Sub,             // 操作员 = 当前登录用户
        Status:    StocktakeStatusCounting,
    }
    if err := DB.Create(h).Error; err != nil {
        return nil, err
    }
    return h, nil
}
```

**关键差异**:**不预填任何行**。营业员扫条码时按需添加。
库存快照发生在添加行的瞬间(§4.5.3),不是创建表时。

#### 4.5.3 按条码录入明细(每行实时拉 cube 库存快照)

```go
// 伪代码(stocktake 服务,POST /stocktake-headers/:id/lines)
func AddLine(ctx, headerID, input AddLineInput) (*StocktakeLine, error) {
    h := DB.GetHeader(headerID)

    // 跨店阻断校验
    if h.Status != StocktakeStatusCounting {
        return nil, ErrInvalidTransition
    }

    // 1. 实时拉 cube:按 product_id 查 product 元信息(只读)
    product, err := cubeClient.GetProduct(ctx, input.ProductID)
    if err != nil {
        return nil, ErrProductNotFound
    }

    // 2. 实时拉 cube:按 (branch_id, product_id) 查 stock 快照
    //    关键:book_qty 是录入该行这一刻的库存快照
    snap, err := cubeClient.GetStock(ctx, h.BranchID, input.ProductID)
    if err != nil {
        return nil, ErrStockNotFound       // 跨店阻断:cube stock 不存在
    }

    // 3. 计算 diff
    diffQty := input.ActualQty - snap.Quantity
    diffAmount := diffQty * snap.AvgCost

    // 4. 落库
    line := &StocktakeLine{
        ID:             uuid.NewString(),
        HeaderID:       h.ID,
        ProductID:      product.ID,         // = 思迅 item_no
        ProductName:    product.Name,       // 冗余防 cube 改
        Unit:           product.Unit,
        Spec:           product.Spec,
        BookQty:        snap.Quantity,      // 该行录入时刻的快照
        BookQtyAt:      snap.UpdatedAt,     // 快照时间
        ActualQty:      input.ActualQty,
        DiffQty:        diffQty,
        AvgCostYuan:    snap.AvgCost,
        DiffAmountYuan: diffAmount,
        DiffReason:     input.DiffReason,
    }
    if err := DB.Create(line).Error; err != nil {
        return nil, err
    }
    return line, nil
}
```

**为什么 book_qty 是每行单独快照而不是统一快照**:
- 营业中盘点,POS 可能在不同时间卖同一商品;
- 不同录入时刻 cube 库存不同;
- 每行快照才能反映"这个时刻录入实盘时,账面是多少"。

#### 4.5.4 差异表生成算法

```go
func DiffReport(headerID) {
    h := stocktake_headers.Get(headerID)
    lines := stocktake_lines.Where(header_id = headerID)

    // 1. 计算每行的 diff
    for i := range lines {
        lines[i].diff_qty = lines[i].actual_qty - lines[i].book_qty
        lines[i].diff_amount_yuan = lines[i].diff_qty * lines[i].avg_cost_yuan
    }

    // 2. summary
    summary := {
        total_lines: len(lines),
        total_diff_qty: sum(lines.diff_qty),
        total_diff_amount_yuan: sum(lines.diff_amount_yuan),
        loss_lines:    count(diff_qty < 0),
        overage_lines: count(diff_qty > 0),
        no_diff_lines: count(diff_qty == 0),
    }

    // 3. 按 reason 聚合
    byReason := groupBy(lines, diff_reason, {sum(qty), sum(amount_yuan)})

    // 4. 返回结构(不落库)
    return DiffReport{header, summary, byReason, lines}
}
```

#### 4.5.4 cube 客户端(后续 Phase 落地)

封装在 `pkg/cubeclient`,签名:

```go
// pkg/cubeclient/client.go
package cubeclient

type Client interface {
    // 查 product dimension/measure
    QueryProduct(ctx, CubeQuery) ([]ProductRow, error)
    // 查 stock(实时库存)
    QueryStock(ctx, CubeQuery) ([]StockRow, error)
    // 查 supplier
    QuerySupplier(ctx, CubeQuery) ([]SupplierRow, error)
    // 通用 /v1/load 转发
    Load(ctx, query json.RawMessage) (json.RawMessage, error)
}

// 通过 dapr service invocation 调 cube-gateway
// appID = "cube-gateway" 或具体实例 "sixun-hbposv7"
func New(appID string, opts ...Option) *Client
```

> 本期不实现 §4.5.4 的完整封装 —— stocktake / catalog / inventory 等服务先用
> `*_test.go` mock cube server 验证业务流程,真实 dapr invocation 后续 Phase 接入。

---

## §5 蔬果:盘点驱动毛利(REQUIREMENTS §3)

### 5.1 事件流

```
purchase.completed  ──┐
sale.completed         ──┼──▶ fresh-produce(订阅)
waste_log.recorded    ──┘    │
                              ▼
                     produce_stocktake.completed  ──▶ fresh-produce.profit-calculator
                                                        (Dapr binding,每次盘点完成后异步触发)
```

### 5.2 毛利公式

```
本期毛利 = 销售收入 - (期初成本 + 本期采购 - 期末库存估值)
本期损耗(元) = (期初 + 采购 - 销售) - 期末库存
```

公式、字段、显性报损降级策略 详见 `docs/REQUIREMENTS.md` §3.3 §3.5。

### 5.3 LLM 接入预留

`GET /llm/loss-context?sku_id=&cycle_id=` 返回该 SKU 该周期的完整数据
(采购 / 销售 / 盘点 / 显性报损)。本 Phase 只暴露 API,不集成 LLM。

---

## §6 生肉:整猪 + 早盘 LLM + 整店按部位盘点(可选,REQUIREMENTS §4)

### 6.1 早盘:录入即调 LLM

```mermaid
06:00 ─ 员工录入 whole_pig(ear_tag, gross_weight_kg, ...)
        │
        ▼
fresh-meat 服务
        │
        ├─ 写 whole_pig 表
        ├─ 发 pig.arrived(pig_id, gross_weight_kg)
        │
        ▼
   (异步,后台 job)
        │
        ├─ llm-gw.ChatCompletion({
        │     pig_id, gross_weight_kg,
        │     history_pigs (近 30 天 ±10% 重量段),
        │     task: "predict_cuts"
        │   })
        │
        ├─ 写 whole_pig.llm_advice_json
        └─ 发 pig.analysis.completed 事件
```

### 6.2 日终:**整店按部位盘点**(可选,不阻断销售,REPLACE)

**与原设计的关键差异**:
- 盘点对象是**整店各分割部位的重量**(五花 / 里脊 / 排骨 / 前后腿 / 猪蹄 / 猪肝),
  而**不是**整只猪。
- 录入接口:`POST /pork-cuts-stocktake`,body = `{branch_id, cuts:[{cut_type, actual_remain_kg}], is_complete:true|false}`
- 表名:`fresh_meat.pork_cuts_stocktake`(原 `pig_stocktake` 不再使用)
- **不盘点不阻断次日销售** —— 未盘点的店内剩余作为"次日开盘库存"继续销售。

```
                  ┌─ 员工录入 pork_cuts_stocktake(cuts[],is_complete)
                  │
                  ▼
fresh-meat 服务
        │
        ├─ 写 pork_cuts_stocktake 表
        ├─ 计算 expected_remain_kg_by_cut = 入库 - 已销 - 报损(按 cut 维度)
        ├─ variance_kg = actual - expected(逐 cut)
        │
        ├─ llm-gw.ChatCompletion({
        │     stocktake_id, pigs[],
        │     task: "review_cuts"
        │   })
        │   // 每头猪独立反推实际分割比例,
        │   // 标记"分割异常"和"明日分割建议"
        │
        ├─ 发 pork.stocktaken 事件
        │   payload.is_complete = is_complete // 供 sales-agg 标注 BI
        │
        └─ 没录入盘点 → 当日不发 pork.stocktaken 事件
              sales-agg 用 llm 推演 / 历史均值估算当日 fresh-meat 毛利
              BI 页面在次日的鲜猪毛利卡片显示 "⚠ 未盘点,数据为推演"
```

### 6.3 LLM 失败降级

`llm-gw` 失败 / 超时(>30s):
- 早盘建议 → 历史同重量段均值
- 日终反推 → 历史损耗均值
- 不阻塞业务流程,只在 dashboard 显示"分析降级"

### 6.4 盘点可选 + 不阻断 + BI 标注

- Dapr cron binding 每天 23:00 **提醒**(非阻断)员工盘点。
- 盘点缺失时:**不阻止次日销售**,当日 fresh-meat 毛利按 llm 推演 / 历史均值计算。
- BI 报表须显式标注数据来源:
  - `is_complete=true` → "✓ 已盘点(实测值)"
  - `is_complete=false` 或无盘点 → "⚠ 未盘点,数据为 llm 推演 / 历史均值"
- 销售毛利数字真实性优先于"数字好看"。

---

## §7 Cube ERP 接入(REQUIREMENTS §5)

### 7.1 数据流

```
思迅 DB  ─cube semantic-layer─▶ cube-gateway POST /v1/load
                                       │
                                       │  erp-connector
                                       │    - 定时拉(每 5 分钟)
                                       │    - 差量 + 1h lookback
                                       │    - 哈希幂等
                                       ▼
                PG: erp_connector.erp_sales_raw
                                       │
                                       │  pub/sub: erp.sale.ingested
                                       ▼
                sales-agg
                  ├── sales_view_minute
                  ├── sales_view_daily
                  └── 供其它 dapr app 反查
```

### 7.2 cube-gateway 查询示例(erp-connector 发的 payload)

```jsonc
// POST http://cube-gateway:3500/v1/load
{
  "query": {
    "measures": ["sale_detail.total_amount_yuan", "sale_detail.total_qty"],
    "timeDimensions": [{
      "dimension": "sale_detail.sold_at",
      "dateRange": ["2026-09-17T00:00:00.000", "2026-09-17T23:59:59.999"],
      "granularity": "minute"
    }],
    "dimensions": ["sale_detail.branch_id", "sale_detail.sku_id", "sale_detail.flow_no"],
    "filters": [{ "dimension": "sale_detail.branch_id", "operator": "=", "values": ["S001"] }]
  }
}
```

### 7.3 字段映射

走 `docs/FIELD_SPEC.md` §3 mapping-*.yaml,erp-connector 启动时 Load。

---

## §8 仓库结构

```
F:\go\src\github.com\YunBright\supertrade\
├── cmd/                       # 每个 dapr app 一个 cmd 子目录
│   ├── pos-gateway/           # BFF
│   ├── catalog/
│   ├── inventory/
│   ├── procurement/
│   ├── pos/
│   ├── pricing/
│   ├── fresh-produce/
│   ├── fresh-meat/
│   ├── stocktake/
│   ├── erp-connector/
│   ├── sales-agg/
│   ├── llm-gw/
│   ├── master-data/
│   ├── bi-gateway/
│   └── notification/
├── internal/                  # 每个 cmd 同名子目录
│   ├── catalog/
│   │   ├── handler/  service/  repo/  model/  dto/
│   │   └── *_test.go          # Go 测试
│   └── ...
├── pkg/                       # 共享库(本项目用)
│   ├── daprclient/            # 包装 dapr service invocation / pubsub
│   ├── eventbus/              # 事件 schema + 编解码
│   ├── workspace/             # 分店隔离 helper(临时,后续推 authkit)
│   ├── money/  decimal/  time/ # 通用基础
│   └── llm/                   # 直调 LLM 客户端(后续接 llm-gw)
├── migrations/                # 每个服务一个 schema 目录
│   ├── catalog/        .../001_init.up.sql
│   ├── inventory/      ...
│   └── ...
├── docs/
│   ├── REQUIREMENTS.md        # 需求契约
│   ├── DESIGN.md              # 本文件
│   ├── FIELD_SPEC.md          # 字段映射契约
│   ├── EVENT-CATALOG.md       # 事件 payload schema(后续补)
│   └── RUNBOOK.md             # 本地启动 / 调试手册(后续补)
├── go.mod
├── go.sum
└── README.md
```

> **不写**:`Makefile`、`Dockerfile`、`docker-compose.yml`、`scripts/*.ps1`、`scripts/*.py`

---

## §9 测试约定

- **单元测试 / 集成测试** 全部 Go `*_test.go`,运行 `go test ./...`
- 关键路径必须有表驱动测试 + mock(外部依赖 mock 掉,真实 dapr / cube-gateway 走 e2e test)
- **不写** powershell / python / shell 测试脚本
- **不写** 启动脚本(README 写命令 + dapr run 直接敲)

---

## §10 开发顺序(3 周 Phase 0 → MVP)

**Week 1:骨架 + POS 闭环**
- dapr init + Go module + 15 个 cmd 骨架占位(每个 `main.go` 起 + `/healthz`)
- **本期不做 SKU / 库存 / 供应商的本地 CURD** —— catalog / inventory / master-data 服务
  只暴露 cube 转发接口(`GET /products` / `GET /stock` / `GET /suppliers`)
- 业务 service:stocktake(本期重点,盘点表 + 差异表)/ pos / procurement / pricing
- 能跑通"盘点表创建 → 录入实盘 → 生成差异表"(mock cube 数据)

**Week 2:生鲜 + 盘点打通**
- fresh-produce(蔬果毛利)/ fresh-meat(整猪 + LLM)
- EVENT-CATALOG §12 的 Step 1-3(`pkg/events` + `pkg/daprpubsub` + cmdbootstrap 扩展)
- stocktake 端到端跑通:从 cube 拉 SKU → 录入实盘 → diff_report → approved

**Week 3:事件落地 + BI**
- EVENT-CATALOG §12 Step 4(pos publish sale.completed → inventory subscribe 扣库)
- llm-gw + erp-connector + sales-agg + bi-gateway
- 整猪 → LLM 分割建议 → 日终盘点的端到端 demo

> **约束**(USER 已确认):
> - 步骤 1(跑单 dapr app)✅ 已验证
> - 步骤 2(进真表业务 handler)❌ 暂不做,继续走 mock 数据
> - 步骤 3(推 authkit tag)⏸ 等整个服务网格测试后再做
> - 步骤 4(EVENT-CATALOG 代码落地)📝 §12 已给出具体落地方案,等业务 handler 就绪后再实施

---

## §11 关键风险与对策

| 风险 | 对策 |
|---|---|
| 蔬果盘点周期不一,毛利节点难对齐 | profit_periods 表按"上次盘点 → 本次盘点"切片,与周期配置解耦 |
| LLM 慢 / 费用 | llm-gw 缓存 + 异步触发 + 失败降级到历史均值 |
| Cube 拉数慢 / 漏数 | erp-connector 多拉 1h lookback + 哈希幂等 + 缺失告警;**catalog / inventory / stocktake 等转发服务加短期 cache** 兜底(本期不实现,后续 Phase) |
| Cube 不可达 | stocktake 创建表时降级:book_qty 留空,前端手填,后续 cube 恢复后回填(本期不实现,记 issue) |
| 微服务多,本地起不来 | `dapr run --app-id <app>` 单跑任一服务,不影响其它 |
| 库存销售一致性 | **最终一致**:outbox pattern,关键路径本地事务 + 异步事件 |
| 分店隔离漏写 | handler 层强制走 `rbac.RequireBranch(...)`(authkit 已合并)+ PR review 把关 |
| 跨服务调用 cube 性能 | `pkg/cubeclient` 内部加 batch + redis 短期缓存(本期不实现) |

---

## §12 EVENT-CATALOG 落地说明(对 §4 第 4 点)

> `docs/EVENT-CATALOG.md` 已经定义 11 个 topic 的 schema,代码侧需要 4 步把契约接上。
> 本节是后续 Phase 的具体落地路径,**本期不做**(USER 已确认 1/2/3 已验证 / 不做 / 推 tag 后再做,
> 第 4 点是要说明落地方案)。

### 12.1 总体架构

```
publisher (e.g. pos)
   │
   │  dapr client.PublishEvent("pubsub", "sale.completed", envelope)
   ▼
dapr sidecar of pos
   │
   │  redis pub/sub (or kafka)
   ▼
dapr sidecar of subscriber (e.g. inventory)
   │
   │  POST http://inventory:8080/events/sale-completed  (CloudEvents envelope)
   ▼
inventory service handler
   │
   ▼
eventsaleCompletedHandler(env data) → 业务逻辑
```

### 12.2 落地步骤(4 步)

#### Step 1:`pkg/events` 包 —— envelope 编解码

**目的**:统一所有 topic 的 publish / parse / validate。

```go
// pkg/events/envelope.go
package events

// Envelope is the CloudEvents 1.0 wrapper used for all dapr pub/sub events.
// 见 docs/EVENT-CATALOG.md §0.1
type Envelope struct {
    SpecVersion     string          `json:"specversion"`
    Type            string          `json:"type"`           // e.g. "com.yunbright.supertrade.sale.completed"
    Source          string          `json:"source"`         // e.g. "pos/SO20260917001"
    ID              string          `json:"id"`             // UUID,幂等
    Time            string          `json:"time"`           // RFC3339 UTC
    DataContentType string          `json:"datacontenttype"` // "application/json"
    Subject         string          `json:"subject,omitempty"`
    Data            json.RawMessage `json:"data"`           // topic-specific payload
}

// TypePrefix 统一前缀,降低 topic 名拼写错概率。
const TypePrefix = "com.yunbright.supertrade."

// MustMarshal envelope to JSON bytes.
func (e Envelope) MustMarshal() []byte

// Parse envelope from request body.
//   - 校验 SpecVersion == "1.0"
//   - 校验 Type 必填
//   - 校验 ID 必填
func Parse(r *http.Request) (Envelope, error)

// ParseData 反序列化 data 到具体 topic 的 payload。
//   payload 必须是非 nil 指针,反序列化失败返回 error。
func ParseData[T any](e Envelope) (T, error)
```

#### Step 2:`pkg/daprpubsub` 包 —— dapr 客户端 + 重试 + DLQ

**目的**:封装 dapr publish,失败重试,死信入 `<topic>.dlq`。

```go
// pkg/daprpubsub/publisher.go
package daprpubsub

type Publisher struct {
    daprClient client.Client   // dapr go-sdk client
    pubsub     string          // pubsub name, e.g. "pubsub"
    appID      string          // 当前 app-id,用作 source 前缀
}

func NewPublisher(c client.Client, pubsub, appID string) *Publisher

// Publish 序列化 envelope + 调 dapr.PublishEvent + 写 dlq。
//   - envelope.Type 必填
//   - envelope.ID 必填(无则生成)
//   - 失败自动重试 3 次(指数退避)
//   - 3 次失败后写 <topic>.dlq 并返回 error
func (p *Publisher) Publish(ctx context.Context, topic string, data any, opts ...PublishOption) error

type PublishOption func(*publishOpts)
func WithSubject(s string) PublishOption
func WithMetadata(k, v string) PublishOption
```

#### Step 3:`cmdbootstrap` 扩展 —— `/dapr/subscribe` 自动注册

**目的**:每个 cmd 在 Options 里声明订阅列表,启动时自动暴露 `/dapr/subscribe` + 注册 topic handler。

```go
// pkg/cmdbootstrap/bootstrap.go(扩展)
type Options struct {
    AppID         string
    // ... 已有字段
    Subscriptions []Subscription   // 新增
}

type Subscription struct {
    Topic   string                       // e.g. "sale.completed"
    Route   string                       // e.g. "/events/sale-completed"
    Handler func(c *gin.Context, env events.Envelope) // 业务 handler
}

// Run() 内部:
//   r.GET("/dapr/subscribe", func(c) {
//       c.JSON(200, []map[string]string{
//           {"pubsubname": "pubsub", "topic": s.Topic, "route": s.Route} for s in opts.Subscriptions,
//       })
//   })
//   for s in opts.Subscriptions:
//       r.POST(s.Route, wrapHandler(s.Handler))
```

#### Step 4:每个 cmd 落地具体订阅

**例**:cmd/inventory 订阅 `sale.completed`,cmd/fresh-meat 订阅 `pig.arrived`。

```go
// cmd/inventory/main.go
func main() {
    cmdbootstrap.Run(cmdbootstrap.Options{
        AppID: "inventory",
        Subscriptions: []cmdbootstrap.Subscription{
            {
                Topic: "sale.completed",
                Route: "/events/sale-completed",
                Handler: handler.OnSaleCompleted,
            },
        },
    })
}

// internal/inventory/events/sale_completed.go
func OnSaleCompleted(c *gin.Context, env events.Envelope) {
    var data saledto.SaleCompleted
    payload, err := events.ParseData[saledto.SaleCompleted](env)
    if err != nil { c.AbortWithStatus(400); return }

    // 幂等:按 envelope.ID 去重
    if idempotency.AlreadyProcessed(env.ID) { c.Status(200); return }

    // 业务:扣减 inventory
    for _, line := range payload.Lines {
        inventory.Deduct(line.SKU, line.Qty, "sale", payload.SaleID)
    }
    c.JSON(200, gin.H{"ok": true})
}
```

### 12.3 落地顺序

| 步骤 | 输出物 | 测试 | 阻塞依赖 |
|---|---|---|---|
| Step 1 | `pkg/events/envelope.go` | `pkg/events/envelope_test.go`:Parse / MustMarshal round-trip | 无 |
| Step 2 | `pkg/daprpubsub/publisher.go` | `pkg/daprpubsub/publisher_test.go`:mock dapr server,验证 retry + DLQ | dapr go-sdk(`github.com/dapr/go-sdk/client`) |
| Step 3 | `pkg/cmdbootstrap` 扩展 | `pkg/cmdbootstrap/subscription_test.go`:验证 `/dapr/subscribe` 返回正确 JSON | Step 1 |
| Step 4 | 各 cmd 的具体订阅 | 单 cmd 端到端:publisher 跑 pub,subscriber 跑收,断言业务效果 | Step 1/2/3 全 |

### 12.4 测试策略

- **单元测试**:用 `httptest.NewServer` 模拟 dapr sidecar,验证 envelope 序列化、handler 注册。
- **端到端**:本地 `dapr run` 起 2 个 app,一个 publish 一个 subscribe,验证事件流转。
- **DLQ 测试**:mock 一个始终返回 5xx 的 sidecar,验证 3 次重试后写 `<topic>.dlq`。

### 12.5 与现有契约的一致性

- **`docs/EVENT-CATALOG.md` §1 topic 索引**:每个 topic 对应一个 Subscription 结构。
- **`docs/EVENT-CATALOG.md` §2 payload schema**:每个 topic 对应一个 dto struct(在 `pkg/events/dto/<topic>.go`)。
- **`pkg/events/TypePrefix` 拼接规则**:`<topic>` ↔ `com.yunbright.supertrade.<topic>` 需在 `events.go` 集中映射,避免散落。

### 12.6 为什么不现在做

- USER 已确认:步骤 1(跑单 app)/ 2(进真表)/ 3(推 authkit tag)都不做,4 要详细说明。
- 当前 cmdbootstrap + 15 个 cmd 骨架已就绪,真实事件流需要在至少 2 个 cmd 有业务 handler 后才有意义。
- **进入条件**:至少 `cmd/inventory` 与 `cmd/pos` 的业务 handler 落地后,Step 1-4 即可一次性串起来。

---

## §13 修订记录

| 日期 | 修订人 | 内容 |
|---|---|---|
| 2026-09-17 | Mavis | 初版,基于需求 v0.1 + auth/authkit/cube 上下文 |
| 2026-09-17 | Mavis | 修订 1:蔬果盘点非必每日;整猪每头独立 LLM;authkit 集成;删 docker/make;FIELD_SPEC 契约 |
| 2026-09-17 | Mavis | 修订 2:authkit 3 项 P1/P2 需求全部合并(RequireBranch / Claims 分店字段 / GetEffectivePermissions) |
| 2026-09-17 | Mavis | 修订 3:生肉盘点粒度改为"整店按部位"(DESIGN §6.2 §6.4),可选,不阻断,BI 标注推演 |
| 2026-09-17 | Mavis | 修订 4:本期只读 cube + 转发(SKU/库存/供应商不维护),§4.5 stocktake 详情,§12 EVENT-CATALOG 落地说明 |
| 2026-09-17 | Mavis | 修订 5:stocktake 实时盘点(营业中、非锁库)+ §4.5.3 按需拉快照 + 盘点单不跨店 |
| 2026-09-17 | Mavis | 修订 6:`/api/v1/products/search` 端点 + 权限过滤 + barcode 长度策略(对齐 scan.html 后端) |