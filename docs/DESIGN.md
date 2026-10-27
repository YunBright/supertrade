# DESIGN — 微服务架构与技术方案

> **本文件与 `docs/REQUIREMENTS.md`、`docs/FIELD_SPEC.md` 必须保持一致**。
> 任何修改其中之一,另两份需在同一 PR 同步。

---

## §0 总览

- **技术栈**:Go 1.26+ / Gin / GORM / PostgreSQL / Redis
- **微服务编排**:Dapr(本地 `dapr init` + `dapr run`,无 docker)
- **身份 / 权限**:`F:\go\src\github.com\YunBright\auth`(userd) + `github.com/YunBright/authkit`
- **ERP 数据源**:`F:\go\src\github.com\YunBright\cube`(只读,不复制其语义层)
- **LLM 接入**:外部服务(本仓不维护);fresh-meat 通过事件 `pig.arrived` / `pork.cuts.stocktaken` 把数据交给外部 LLM 网关,反推结果回写 `whole_pig.llm_advice_json`

### 三大原则

1. **事件驱动**:跨服务状态变更一律走 Dapr pub/sub,无强同步事务
2. **数据契约唯一**:`docs/FIELD_SPEC.md` 字段映射 + `docs/REQUIREMENTS.md` 业务规则
3. **基础能力不重复造**:**身份/权限完全委托 auth + authkit**(本系统只写业务代码)

---

## §1 微服务拆分

> app 清单的真源是 `.claude/rules/00-app-catalog.md`(aud 短名 / dapr app-id / 公网前缀 三列)。
> 本表只讲**职责与数据归属**;路由真源是各 `internal/<svc>/handler/handler.go::RegisterRoutes`。

| # | cmd | dapr app-id | 职责 | 主要数据 | 关键依赖 |
|---|---|---|---|---|---|
| 1 | **stocktake** ⭐ | supertrade-stocktake | 盘点单 / 明细 / 差异报告 | stocktake_headers, stocktake_lines | cube-router, userd |
| 2 | **catalog** | supertrade-catalog | 本地 suppliers/products + cube 兜底 + 商品选择控件读端点 | suppliers, products | cube-router, userd |
| 3 | **inventory** | supertrade-inventory | 库存读(cube 转发)+ 库存流水 / 出入库单 | stock_levels, stock_moves | cube-router |
| 4 | **cube-router** ⭐ | supertrade-cube-router | 按 X-Branch-ID 路由 POST /v1/load 到正确 cube 实例 | branch_cube_sources | dapr invoke → cube 实例, userd |
| 5 | **master-data** | supertrade-master-data | 门店 / 员工 / 班次 | stores, employees, shifts | userd |
| 6 | **pos** | supertrade-pos | 销售开单 / 收款 / 改价 / 退货 | sales_orders, sale_lines, payments | cube-router |
| 7 | **pos-gateway** | supertrade-pos-gateway | BFF,前端对接,聚合调用 | 路由 | dapr invoke |
| 8 | **member** | supertrade-member | 会员档案 / 积分 / 账户 / 门店会员配置 | members | userd |
| 9 | **pricing** | supertrade-pricing | 售价 / 促销 / 促销方案 / 报价 | price_lists, promotions, promotion_plans | cube-router |
| 10 | **procurement** | supertrade-procurement | 采购收货单(只读) | purchase_sheets | cube-router |
| 11 | **transaction** | supertrade-transaction | 商品销售流水 / 收银流水查询 | — | cube-router |
| 12 | **fresh-meat** ⭐ | supertrade-fresh-meat | 生肉整猪 + 分割 + **部位级**成本链 / 毛利 + 大宗台账 | whole_pig, pig_cuts, cut_cost_daily, bulk_settlements | cube-router, 外部 LLM |
| 13 | **settlement-auditor** | supertrade-settlement-auditor | 结算付款审批 + 机器审核 + 附件 | approvals, approval_items | stocktake, 外部 LLM |
| 14 | **sales-agg** | supertrade-sales-agg | 销售事实落库 + 销售报表 + 门店对比 | sales_daily, reports | cube-router |
| 15 | **sales-insight** | supertrade-sales-insight | AI 销售日报 / 周报 / 月报 | reports | sales-agg, 外部 LLM |
| 16 | **notification-gateway** | supertrade-notification-gateway | WebSocket 推送网关 | — | pubsub |

**不经公网**(Dapr job / 内部调用):`cube-sync`(档案同步)、`cube-async`(异步 cube 作业)。

**占位 cmd**(未实现,不要往里加业务):`bi-gateway`、`erp-connector`、`fresh-produce`。

> **身份/权限不归本系统**:`auth` 项目的 userd 提供 JWT 签发 + 用户/角色/权限 CRUD;
> 本系统每个 dapr app 通过 `authkit` 中间件鉴权。**不写 iam 服务**。


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
        │                     │ LLM(外部)               │            │
        │                     ▼                          ▼            │
        │             (外部 LLM 网关)            ┌───────────────┐    │
        │            智谱/DeepSeek               │  sales-agg    │    │
        │            不在本仓                    │ 多端销售聚合  │    │
        │                                       └───────────────┘    │
        │                                                          │
        └──────────────────────┬───────────────────────────────────┘
                               │
                  ┌────────────▼────────────┐
                  │ master-data  主数据     │
                  │ notification-gateway  WS│
                  └───────────────────────────┘

身份/权限:外部 YunBright/auth 项目的 userd(JWT 签发) + authkit 公开包(claims + rbac + userinfo)
```

### 1.1 本期数据归属速查(REQUIREMENTS §7.5)

| 数据 | 归属 | 服务 |
|---|---|---|
| **SKU / 商品** | **catalog 本地表 + cube 兜底** | `catalog.GET /products/search` 等 |
| 实时库存 | cube-gateway 经 cube-router 转发 | `inventory.GET /stock/:product_id`(branch 走 `X-Branch-ID` header) |
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

> 详见 `README.md` §3(无 make 文件)。

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
| `pig.arrived` | fresh-meat | (外部 LLM 网关,异步分析) | pig_id, gross_weight_kg |
| `pig.stocktaken` | fresh-meat | (外部 LLM 网关,异步分析) | stocktake_id, variance_kg |
| `pig.analysis.completed` | (外部 LLM 网关) | fresh-meat / dashboard | pig_id, analysis_json |
| `erp.sale.ingested` | erp-connector | sales-agg | source, batch_id, count |
| `sale.aggregated` | sales-agg | bi-gateway + 其它 dapr app | branch_id, period, kpis |

### 2.4 Dapr 调用关系(节选)

```
pos-gateway ─invoke─▶ pos ─invoke─▶ inventory (锁库/释放)
                       └─pub─▶ sale.completed ─▶ inventory 扣减
                                             ─▶ fresh-meat 落 line_sales_by_pig
                                             ─▶ sales-agg 聚合

fresh-meat ─pub─▶ pig.arrived ─▶ 外部 LLM 网关(订阅 + ChatCompletion + 回写)
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
`IsAllowedBranch()` helper。

> 2026-09 简化(migration 009 强化):**所有 path 不再带 `:branch_id`**。branch 全部从
> `X-Branch-ID` header 取(middleware.XBranchID 已注入 `[]string` ctx,handler 调
> `middleware.BranchIDsFromContext(c)`)。跨店用户可用 `X-Branch-ID: B001,B002`(逗号
> 分隔,**多店 union scopes**)或 `X-Branch-ID: *`(通配,**auth 中间件层用
> JWT.AccessibleBranches 展开**;business service 永远拿到具体 branch_id list,不
> 感知通配语义)。
>
> branch ID 形态(migration 009):`VARCHAR(64)` 自编码字符串(`B001` / `01` /
> `S001` 等),**不再要求 UUID**。`X-Branch-ID` 字面值透传到下游 server,server
> 自己 parse / 校验 ≤ 64 字符即可。

```go
// /stock/:product_id —— branch 从 header 取
r.GET("/stock/:product_id",
    rbac.RequireBranch(
        func(c *gin.Context) (string, bool) {
            b := c.GetHeader("X-Branch-ID")
            return b, b != ""
        },
        func(c *gin.Context) ([]string, bool) {
            cl, ok := claims.FromContext(c.Request.Context())
            if !ok { return nil, false }
            return cl.GetEffectiveBranches(), true
        },
    ),
    h.getStock,
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
   (外部 LLM 网关订阅)
        │
        ├─ ChatCompletion({
        │     pig_id, gross_weight_kg,
        │     history_pigs (近 30 天 ±10% 重量段),
        │     task: "predict_cuts"
        │   })
        │
        └─ 写回 whole_pig.llm_advice_json
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
        ├─ 发 pork.cuts.stocktaken 事件 ──▶ (外部 LLM 网关订阅后 ChatCompletion)
        │   payload.is_complete = is_complete // 供 sales-agg 标注 BI
        │
        └─ 没录入盘点 → 当日不发 pork.cuts.stocktaken 事件
              sales-agg 用 llm 推演 / 历史均值估算当日 fresh-meat 毛利
              BI 页面在次日的鲜猪毛利卡片显示 "⚠ 未盘点,数据为推演"
```

### 6.3 LLM 失败降级

外部 LLM 网关失败 / 超时(>30s,由外部自行降级):
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
supertrade/
├── cmd/                       # 每个 dapr app 一个 cmd 子目录(22 个:16 公网 + 2 job + 3 占位 + seed CLI)
├── internal/                  # 每个 cmd 同名子目录
│   ├── <svc>/
│   │   ├── handler/           # RegisterRoutes + HTTP 边界(鉴权 / branch / 参数校验)
│   │   ├── service/           # 业务规则
│   │   ├── model/             # GORM 模型
│   │   └── *_test.go          # Go 测试(分模块)
│   └── ...
├── pkg/                       # 本仓共享包
│   ├── cmdbootstrap/          # 启动骨架:装配顺序 / 验签 / 鉴权 / 访问日志 / 优雅退出
│   ├── eventbus/              # pub/sub 组件名(全仓唯一真相)
│   ├── middleware/            # X-Branch-ID 中间件
│   └── outbox/                # 发件箱(事件与业务行同事务落库)
├── docs/                      # 规格与契约(清单见 README §6)
├── dapr/components/           # Dapr 组件定义(pubsub / conversation)
├── .claude/rules/             # 开发规约
├── go.mod
├── go.sum
└── README.md
```

> **不写**:`Makefile`、`Dockerfile`、`docker-compose.yml`、`scripts/*.ps1`、`scripts/*.py`、
> 独立 SQL migration 文件(DB 迁移走 GORM AutoMigrate,服务启动自带)。

---

## §9 测试约定

- **单元测试 / 集成测试** 全部 Go `*_test.go`,运行 `go test ./...`;**按模块分文件夹**。
- 关键路径要有表驱动测试 + fake 依赖(cube 用 `internal/cubeclient/cubeclientfake`)。
- **不写** powershell / python / shell 测试脚本。
- **不写** 启动脚本(README 写命令 + dapr run 直接敲)。
- **契约 / e2e 测试不进本仓** —— 走公网 nginx 入口的 contract case 在 `../deployer/tests/contract/`。
- **不写** httptest 模拟 dapr sidecar —— SDK 走 gRPC over `:50001`,用 fake dapr.Client。

---

## §11 关键风险与对策

| 风险 | 对策 |
|---|---|
| 蔬果盘点周期不一,毛利节点难对齐 | profit_periods 表按"上次盘点 → 本次盘点"切片,与周期配置解耦 |
| LLM 慢 / 费用(外部 LLM 网关) | 异步触发 + 失败降级到历史均值;**事实录入立即返回,派生数据异步生成** |
| Cube 拉数慢 / 漏数 | cube-sync 增量同步(水位线 + since 透传)+ 哈希幂等 + 缺失告警 |
| Cube 不可达 | 转发端点返 503 / 降级路径显式标注 `data_source`,不静默回落本地 |
| 微服务多,本地起不来 | `dapr run --app-id <app>` 单跑任一服务,不影响其它 |
| 库存销售一致性 | **最终一致**:outbox pattern,关键路径本地事务 + 异步事件 |
| 分店隔离漏写 | handler 层强制走 `rbac.RequireBranch(...)` / `RequireScopeWithBranch(...)` + PR review 把关 |
| 跨服务调用 cube 性能 | `internal/cubeclient` 内做 batch + 按基准日过滤时效 scope |

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
| 2026-09-29 | Tinkler | 修订 7:删除占位 cmd `llm-gw`(LLM 网关)和 `notification`(企微/钉钉);§1 微服务从 16 缩到 14;LLM 网关改为外部服务,fresh-meat 通过 `pig.arrived` / `pork.cuts.stocktaken` 事件交接;`pig.analysis.completed` topic 由外部网关负责(详见 EVENT-CATALOG §2.9);同步 .goreleaser.yaml / deploy-supertrade.ps1 / .claude/rules/00-app-catalog.md / README.md |
| 2026-10-27 | Mavis | 修订 8:§1 app 表刷新为当前 16 个公网 app + 2 个 job app;§8 仓库结构对齐现状;§9 测试约定改为"契约 / e2e 在 deployer 仓";删 §10(已完成的 Sprint 计划)与 §12(未按此实现的事件落地方案) |