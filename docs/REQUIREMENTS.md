# REQUIREMENTS — 需求契约

> **唯一真相源**:所有产品需求 + 非功能需求以本文件为准。任何需求变更须 PR 同步本文件。
>
> **同步约束**:本文件每次修订,必须同步检查以下三处是否一致:
>   - `docs/DESIGN.md`(技术方案)
>   - `docs/FIELD_SPEC.md`(字段契约)
>   - 当前 Sprint 的 task list
>
> **更新方式**:每次需求更新→在本文末尾追加"修订记录"行,不要删除旧记录。

---

## §0 项目目标

零售管家 SuperTrade —— 一个面向社区商超 / 生鲜超市的零售管理后端,
基于 Dapr 微服务架构,以 **生鲜(蔬果/生肉)** 为差异化卖点。

**核心目标**:
1. 把日常零售所需的进销存 + POS 收银跑通闭环
2. 在不增加员工录入负担的前提下,**对生鲜品类给出可信的毛利分析**
3. 把多家 ERP / POS 终端的销售数据**统一汇聚**,供 BI 与跨服务拉取

---

## §1 用户与权限

### 1.1 身份与权限来源

| 项 | 来源 | 备注 |
|---|---|---|
| 用户 / 角色 / 权限 / 登录 | `F:\go\src\github.com\YunBright\auth` 项目 | 已有 `userd` dapr app(JWT 签发 + RBAC) |
| 业务侧解析 claims / 角色 / scope | `github.com/YunBright/authkit`(authkit 公开包) | `claims.GinMiddleware` + `rbac.*` |
| 跨服务查用户信息 | `authkit/userinfo.New("userd")` | 走 dapr invocation |
| 验签 | **应用层 `authkit/authn.Verifier`**(RS256 + JWKS) | 各 dapr app **自己验签**。⚠️ 2026-10-20：原写「dapr middleware.http.bearer(sidecar 验签)，各 dapr app 不重验签」——该 sidecar 组件 2026-10-12 已全网移除；只解 payload 不验签会让伪造签名的 JWT 读到真实数据(生产实证) |

### 1.2 权限模型(本项目约束,2026-09 重构后)

**三元权限模型**:每个业务接口必须满足 `user × branch × scope`:

- `user` —— JWT `sub`(dapr sidecar 已验签)
- `branch` —— 当前操作的门店,从 **`X-Branch-ID` header** 统一解析(由 `pkg/middleware.XBranchID()` 注入 ctx)
- `scope` —— 业务操作动词,采用 `<resource>:<verb>` 命名(≤64 字符),如 `inventory:view` / `inventory:manage` / `supplier:view` / `product:view` / `cube:read`

**`JWT.scopes` 字段已废弃**:auth 在 2026-09-23 改为 `defaultScopes = []`,
所有 `cl.HasScope(...)` / JWT-static scope helper 永远返 false;**不再使用**。
业务侧必须走 `userinfo.GetBranchPermissions(uid, branchID)` 读 per-branch scope 矩阵
(userd 实时返回,缓存到内存 + 失效订阅 `auth.user.access_changed`)。

**统一鉴权中间件**:`authkit/rbac.RequireScopeWithBranch(scope, branchFn, resolver)`,
配套 `authkit/rbac.HasAnyScopeWithBranch(users)` resolver 适配器。
决策矩阵:
- branchFn 取不到 → `400 branch_required`
- resolver 返 error → `503 userd_unavailable`
- resolver 返 false → `403 forbidden`
- 命中 → `c.Next()`

公开接口(`/healthz`、登录回调)不走 dapr bearer 中间件。

### 1.3 数据级权限(分店隔离)

业务接口必须按 `branch_id` 过滤:用户仅可访问其所属分店的数据。
**`authkit` 已提供两套中间件**(本系统直接使用,不二次实现):

| 中间件 | 适用场景 | 来源 |
|---|---|---|
| `rbac.RequireBranch(branchFn, allowedFn)` | path `:branch_id` 与 `claims.AccessibleBranches` 比对(高频 / 静态缓存) | `authkit/rbac/rbac.go` |
| `rbac.RequireScopeWithBranch(scope, branchFn, resolver)` | 在指定 branch 下校验动态 scope(`userinfo.GetBranchPermissions` 查) | `authkit/rbac/scope_branch.go`(2026-09 加) |

**branch 来源优先级**(`branchFn` 由各服务按业务语义实现):
- X-Branch-ID header —— 几乎所有业务端点的统一入口(migration 009 起支持 `VARCHAR(124)` 自编码短码 / 逗号多店 union / `*` 通配展开)
- body `branch_id` —— `POST /stocktake-headers` 用 body 字段(创建时,自编码短码)
- 关联实体的 branch —— `stocktake_lines` 通过 header→hdr→line 反查

**X-Branch-ID 多店 union 语义**(migration 009 起):

| header 值 | middleware 透传给 handler 的 ctx 值 | 业务侧守门 |
|---|---|---|
| `B001`(单值) | `[]string{"B001"}` | 走 `userinfo.GetBranchPermissions(uid, "B001")` 单店 |
| `B001,B002`(逗号多店) | `[]string{"B001","B002"}` | 走 `userinfo.GetBranchPermissionsMulti(uid, []string{"B001","B002"})` union scopes;`branches` 矩阵按 per-store 明细返回 |
| `*`(通配) | JWT.AccessibleBranches 展开的具体 list | 同 multi,user 永远拿不到 `*` 字面值;business service 不感知通配语义 |

> 2026-09 简化:path 不再带 `:branch_id`(`/stock/:branch_id/:product_id` 已并为
> `/stock/:product_id`;`/branches/:branch_id/default-stocktake` 已并为
> `/default-stocktake`)。所有 branch 上下文从 X-Branch-ID header 取,
> middleware 只透传不强制(handler 强制要求时自己挂 `middleware.RequireBranch()`)。

### 1.4 待推 authkit 的开发需求(已落地)

| 需求 | 优先级 | 状态 | 实现位置 |
|---|---|---|---|
| `rbac.RequireBranch(branchFn, allowedFn)` 中间件 | P1 | ✅ 已合并 | `F:\go\src\github.com\YunBright\authkit\rbac\rbac.go` |
| `claims.Claims.BranchID` + `AdditionalBranches` + `GetEffectiveBranches()` / `IsAllowedBranch()` | P2 | ✅ 已合并 | `F:\go\src\github.com\YunBright\authkit\claims\claims.go` |
| `userinfo.GetEffectivePermissions(ctx, userID, tenantID)` + `EffectivePermissions` 类型 | P2 | ✅ 已合并 | `F:\go\src\github.com\YunBright\authkit\userinfo\userinfo.go` |
| `rbac.RequireScopeWithBranch(scope, branchFn, resolver)` 三元权限中间件 | P0 | ✅ 已合并(2026-09) | `F:\go\src\github.com\YunBright\authkit\rbac\scope_branch.go` |
| `rbac.HasAnyScopeWithBranch(users *userinfo.Client) ScopeResolver` resolver 适配器 | P0 | ✅ 已合并(2026-09) | 同上 |
| `userinfo.GetBranchPermissions(ctx, userID, branchID)` per-branch 矩阵 | P0 | ✅ 已合并 | `F:\go\src\github.com\YunBright\authkit\userinfo\userinfo.go` |
| `auth.user.access_changed` 合并事件取代 `permissions_changed` | P0 | ✅ 已合并 | auth 仓 userd |

> **依赖关系**:supertrade 通过 `replace github.com/YunBright/authkit => ../authkit`(go.mod)
> 引用本地开发版;authkit 后续打 tag 后切换为 `require ...@vX.Y.Z`。
> 用户侧(userd)需补 `/internal/users/{id}/permissions` 端点供 `GetEffectivePermissions` 调用
> — 当前端点不存在时返回 `ErrPermissionsUnavailable`,调用方可降级到 `Get + User.Scopes`。

---

## §2 POS 基础功能(标准零售)

| 模块 | 子功能 | 关键单据 | 主服务 |
|---|---|---|---|
| 库存 | 入库 / 出库 / 调拨 / 报损 / 批次 / 保质期 / 库存查询 | inventory batches + stock_moves | inventory |
| **商品目录** | **本地 supplier / product 表(suppliers/products)+ cube 兜底** | **suppliers / products(本地)** + cube | **catalog** |
| 销售 | 开单 / 挂单 / 改价 / 优惠 / 抹零 / 退货 / 反结账 / 班次结账 | pos sales_orders + sale_lines + payments | pos |
| 采购 | 询价 / 订货 / 收货 / 退货 / 供应商对账 / 账期 | procurement purchase_orders + grns | procurement |
| 盘点 | 全盘 / 抽盘 / 差异调整 | stocktake tasks | stocktake |
| 定价 | 基础价 / 促销价 / 会员价 / 时段价 / 限购 + 改价审批 | pricing price_lists + promotions | pricing |
| 主数据 | 门店 / 员工 / 班次(供应商 / 客户已在 catalog 维护) | master-data tables | master-data |
| 报表 | 销售 / 毛利 / 库存 / 损耗 / 供应商对账 | 走 cube | bi-gateway + sales-agg |
| **Cube 多源路由** | **按 X-Branch-ID 路由到不同 cube 实例(sixun-hbposv7 / sixun-ysx)** | **branch_cube_sources** | **cube-router** |

**字段定义统一走**:`docs/FIELD_SPEC.md` §1

### 2.1 盘点管理(stocktake) —— 本期重点:实时盘点(营业中)

> **核心语义**:**营业中盘点(非锁库)** —— 盘点过程中 POS 正常销售,不影响业务;
> 录入每行时按条码**实时拉 cube sixun 库存作快照**,然后录入实盘,差异 = 实盘 - 快照。
> **盘点单不能跨店**,每个盘点单只盘点一家分店。

| 子功能 | 服务 | 说明 |
|---|---|---|
| 盘点表 CURD | stocktake | 单据头(`stocktake_headers`):创建 / 查询 / 改状态 / 提交 / 审核 |
| 盘点明细 CURD | stocktake | 单据行(`stocktake_lines`):**按条码逐行录入** / 改 actual_qty / 删 |
| **生成盘点差异表** | stocktake | 任意时刻可调 `/diff-report`,基于已录入行的 `actual_qty - book_qty` 实时聚合 |
| 商品信息查询 | stocktake → cube-gateway | 录入页输入条码时,调 cube 查 product(`measures: product.*`) |
| **实时库存快照** | stocktake → cube-gateway | **每行录入时刻实时拉** cube `stock.total_quantity` 作 `book_qty` |
| 审计 / 导出 | stocktake | `operator_id` 全留痕;导出 CSV/XLSX(后续) |

#### 2.1.1 单据头 `stocktake_headers`

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | string | 主键,如 `ST20260917001` |
| `branch_id` | string | **盘点门店(整单锁定,不可改)** |
| `count_date` | date | 盘点日期 |
| `status` | enum | `counting` / `adjusted` / `approved` |
| `type` | enum | `general`(普通全盘) / `produce`(蔬果联动 fresh-produce) |
| `operator_id` | string | 录入员 |
| `auditor_id` | string | 审核员(进入 approved 时填) |
| `total_diff_qty` | decimal | 差异数量合计(提交时计算) |
| `total_diff_amount_yuan` | decimal | 差异金额合计(提交时计算) |
| `remark` | string | |

#### 2.1.2 单据行 `stocktake_lines`(按条码逐行录入)

> **重要语义**:`book_qty` 是**录入该行时刻从 cube sixun 拉的库存快照**,**不是**统一快照;
> 不同行的快照时间不同(同店同商品可能在不同时间被盘两次,产生两个快照)。

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | string | PK(UUID) |
| `header_id` | string | 关联 stocktake_headers.id |
| `product_id` | string | cube product.id(思迅 item_no 类型,见 §7.5.1) |
| `product_name` | string | 录入时冗余(防 cube 后改失真) |
| `unit` | string | 冗余 |
| `spec` | string | 冗余 |
| `book_qty` | decimal | **该行录入时刻的 cube 库存快照** |
| `book_qty_at` | datetime | **快照时间**(cube 返回的 `updated_at`) |
| `actual_qty` | decimal | 实盘录入(counting 状态可改) |
| `diff_qty` | decimal | **`actual_qty - book_qty`**(录入时算) |
| `avg_cost_yuan` | decimal | 快照时从 cube `stock.avg_cost` 拉 |
| `diff_amount_yuan` | decimal | **`diff_qty × avg_cost_yuan`**(录入时算) |
| `diff_reason` | enum | `loss` / `overage` / `damage` / `wrong_unit` / `other`(可空) |
| `created_at` | datetime | 行创建时间 |
| `updated_at` | datetime | |

#### 2.1.3 差异表 `stocktake_diff_report`(实时生成,任意状态可调)

差异表**不落库**,`GET /diff-report?header_id=...` 实时聚合:

```jsonc
{
  "header_id": "ST20260917001",
  "branch_id": "S001",
  "count_date": "2026-09-17",
  "status":    "counting",         // 当前状态;counting/adjusted/approved 都可生成
  "summary": {
    "total_lines":       3,        // 当前已录入行数
    "total_diff_qty":    -3.0,     // ∑ diff_qty
    "total_diff_amount_yuan": -75.0,
    "loss_lines":         2,        // diff_qty < 0
    "overage_lines":     1,        // diff_qty > 0
    "no_diff_lines":     0         // diff_qty = 0
  },
  "by_reason": [
    { "reason": "loss", "lines": 2, "qty": -5.0, "amount_yuan": -125.0 },
    { "reason": "overage", "lines": 1, "qty": 2.0, "amount_yuan": 50.0 }
  ],
  "lines": [
    {
      "product_id": "P-1001",
      "product_name": "可口可乐 330ml",
      "book_qty": 100.0,
      "book_qty_at": "2026-09-17T10:30:15Z",
      "actual_qty": 95.0,
      "diff_qty": -5.0,
      "avg_cost_yuan": 25.0,
      "diff_amount_yuan": -125.0,
      "diff_reason": "loss"
    }
  ],
  "generated_at": "2026-09-17T10:35:00Z"
}
```

#### 2.1.4 商品信息 / 实时库存数据来源

**关键**:录入每行都**实时拉**(不是创建表时一次性预填,见 §2.1.5)。

```
营业员/店长         stocktake 服务            cube-gateway            cube product/stock
    │ 扫条码         │                         │                       │
    ├───────────────▶│ POST /v1/load product   │                       │
    │                 │ (按 barcode 查 product) │ ────────────────────▶│
    │                 │ ◀────── {id,name,...}    │ ◀────────────────────│
    │                 │                         │                       │
    │                 │ POST /v1/load stock     │                       │
    │                 │ (按 branch+product 查)   │ ────────────────────▶│
    │                 │ ◀──── {qty,avg_cost,at}  │ ◀────────────────────│
    │                 │                         │                       │
    │ 输实盘数 95     │                         │                       │
    ├───────────────▶│                         │                       │
    │ ◀ diff 立即算  │                         │                       │
```

#### 2.1.4.1 商品搜索接口契约(`/api/v1/products/search`)

> 本期新增 —— 跟 scan.html 后端(`collect-ai` `SearchProducts`)字段对齐,前端**可零改动接入**。
>
> **归属变更**:该端点已从 stocktake 迁移到 **catalog 服务**(`cmd/catalog/main.go`),
> 权限守门由 stocktake handler 内部 `hasScope` JWT-static helper 改为 catalog 的
> `rbac.RequireScopeWithBranch("product:view", branchFromHeader, resolver)` 中间件。
> stocktake 仍保留 `/products/search` 转发逻辑(走 cube-router 兜底),但客户端应优先调 catalog。

**端点**:`GET /api/v1/catalog/products/search?barcode=xxx`(branch 由 `X-Branch-ID` header 传入)

**业务字段**(响应中的 `products[]` 每个元素):

| 字段 | 类型 | 权限 | 说明 |
|---|---|---|---|
| `barcode` | string | 公开 | 商品条码(**本期 = item_no**,cube sixun 标准 product schema 无 barcode 字段,后续 cube 仓库扩展后启用真 barcode) |
| `product_id` | string | 公开 | cube `product.id` = 思迅 `item_no` |
| `product_name` | string | 公开 | 商品名 |
| `category` | string | 公开 | 分类名(cube `product.category_id` → 名称) |
| `brand` | string | 公开 | 品牌(扩展字段) |
| `unit` | string | 公开 | 基本单位 |
| `price` | decimal | 公开 | 零售价(扩展字段,本期 mock) |
| `stock_qty` | decimal | **`inventory:view`** | 该门店 cube `stock.total_quantity` |
| `avg_cost_yuan` | decimal | **`inventory:view`** | cube `stock.avg_cost` |
| `supplier_id` | string | **`supplier:view`** | 主供应商 ID |
| `supplier_name` | string | **`supplier:view`** | 供应商名(cube `supplier.name`) |

**权限过滤**(跟 collect-ai SearchProducts 对齐):
- 无 `inventory:view` → **不查不返回** `stock_qty` / `avg_cost_yuan`(也告诉前端)
- 无 `supplier:view` → **不查不返回** `supplier_id` / `supplier_name`
- 其它字段不受权限控制
- `meta.inv_viewable` / `meta.supplier_viewable` 告诉前端哪些被过滤

**barcode 长度策略**(跟 collect-ai 对齐):
- `≥13 位`:**精确匹配**,limit 自动取 1
- `5~12 位`:**后缀匹配** `LIKE '%X'`,limit 留给前端(默认 10)
- `<5 位`:**不查**,返回空(防全表扫)

**响应结构**:

```jsonc
{
  "products": [
    {
      "barcode": "6901234567890",
      "product_id": "P-1001",
      "product_name": "可口可乐 330ml",
      "category": "饮料",
      "brand": "可口可乐",
      "unit": "瓶",
      "price": 2.5,
      "stock_qty": 100,
      "avg_cost_yuan": 2.5,
      "supplier_id": "SUP-001",
      "supplier_name": "可口可乐华南"
    }
  ],
  "count": 1,
  "meta": {
    "inv_viewable": true,
    "supplier_viewable": true,
    "barcode_query": "6901234567890"
  }
}
```

#### 2.1.5 业务规则(实时盘点,营业中)

- **盘点单不跨店**:`header.branch_id` 创建后不可改;录入每行必须 `line.product_id` 属于该店所在 cube stock 维度。
- **非锁库(营业中)**:POS 正常销售,不影响;
- **不预填 SKU**:创建 header 时**只创建空单**;录入第一行时拉 cube;
- **每行独立快照**:`book_qty` 是录入该行的瞬间 cube 库存快照;
  同一商品在不同时间录入会产生两行,各有各的快照。
- **整单差异表实时生成**:`counting` / `adjusted` / `approved` 任意状态都允许;
  - `counting`:总差异随录入逐行变化
  - `adjusted`:冻结,不再录入
  - `approved`:不可改单据
- 状态机:
  ```
  counting ─submit─▶ adjusted ─approve─▶ approved(不可改)
  counting/adjusted ─rollback(管理员)─▶ 撤销到上一状态
  ```
- 删除明细限制:`counting` 状态可删;`adjusted` / `approved` 不可删。
- **跨店阻断校验**:`CreateHeader / AddLine` 时强制校验 `product_id` 的 cube 库存必须存在于 `branch_id`
  (cube `stock` 维度 `(product_id, branch_id)` 唯一存在,否则 400 错误)。

#### 2.1.6 与 §7.5 数据归属原则的协同

- 本系统 PG **不建** `product` 表 / `stock` 表(§7.5)。
- 但 `stocktake_lines.product_id` 必须与 cube `product.id` **类型一致**(思迅 `item_no`,string)
  —— 这是"统一数据模型但不建表"的契约:
  `FIELD_SPEC §1.1 catalog product` 字段定义约束了 cube → 本系统字段对应关系,
  盘点明细的 `product_id` 直接用 `item_no` 类型(string),无需任何 ID 转换。
```

具体 cube query 走 `docs/FIELD_SPEC.md` §3 mapping-* + 在 `pkg/cubeclient` 包内统一封装(后续 Phase)。

#### 2.1.5 业务规则

- `counting` 状态:可改明细、删行
- `adjusted` 状态:不可改明细(冻结差异快照)
- `approved` 状态:不可改单据头(生成 diff report 后 BI/采购/财务可引用)
- 创建盘点表时,**按 product 全集快照预填行**(只填 product_id / book_qty,actual_qty 留空)
- 录入实盘后,自动算 diff_qty + diff_amount_yuan(不需要前端算)

---

## §3 生鲜蔬果(fresh-produce)—— 盘点驱动毛利

### 3.1 业务规则

- **不要求每日盘点**:盘点周期由门店管理员在 `master-data.branch.stocktake_cycle_days` 配置,蔬果默认 7 天;可手动触发临时盘点
- **必录**:采购、销售、盘点
- **可录(非必录)**:显性报损 —— 有数据时 BI 显示,缺数据时不阻塞
- **毛利计算节点**:**每次盘点完成时**异步触发,非实时销售时算

### 3.2 数据模型核心(详见 FIELD_SPEC §1.5)

- `fresh_produce.produce_stocktake`(盘点)
- `fresh_produce.waste_logs`(显性报损,可空)
- `fresh_produce.profit_periods`(盘点节点产出:期初/期末/本期毛利/本期损耗)

### 3.3 毛利公式

```
期初成本 C0 = 上一周期 profit_periods.last_profit_period.remaining_value_yuan
本期采购成本 C_in   = Σ purchase_cost_yuan (该周期内)
销售出库收入 C_rev  = Σ pos.sale_lines.amount_yuan (该周期内,显性报损不计入)
期末盘点价值 C_end = 本次盘点 sum(gross_weight_kg × current_market_price_yuan)
─────────────────────────────────────────────────
本期毛利 = C_rev - (C0 + C_in - C_end)
本期损耗(元) = (C0 + C_in - C_rev) - C_end   // 等于 0 表示无损
本期隐性损耗率 = 本期损耗 / (C0 + C_in)
```

### 3.4 LLM 介入路径(预留)

- **预留查询方法**:`fresh-produce.GET /llm/loss-context?sku_id=&cycle_id=`
  返回该 SKU 该周期的完整数据(采购/销售/盘点/显性报损),供后续 BI 或外部 LLM 服务拉取
- **本 Phase 不集成 LLM**:只暴露数据 API;真正的 LLM 调用在 BI 侧或后续 Phase

### 3.5 显性报损数据可用性降级

- 有报损数据 → BI 报损面板显示数字 + 隐性损耗反推
- 无报损数据 → BI 报损面板显示"未录入",毛利公式仍按隐性损耗计算

---

## §4 生鲜肉(fresh-meat)—— 整猪 + 早盘 LLM 推演 + 整店按部位盘点(可选)

### 4.1 业务时间线

```
06:00 早盘 ─┬─ 录入整猪 N 条(each:耳标 + 毛重 + 单价 + 到货时间)
           │           └─ 调 LLM:基于这头猪的重量 + 历史数据
           │              → 输出"预期分割建议"(五花 kg / 里脊 kg / 排骨 kg ...)
           │              → 写 whole_pig.llm_advice_json
           │
           │  白天 ─ 员工手动分割 + 部分单品补录条码(pig_cuts)
           │           pos 销售事件 → fresh-meat 落 line_sales_by_pig
           │
22:00 日终(可选)── 盘点 pork_cuts_stocktake:
              // 盘点对象:整店各分割部位的重量,**不是整只猪**
              // 例:五花 8.2kg / 里脊 2.1kg / 排骨 5.4kg / 前后腿 6.8kg
              actual_remain_kg_by_cut = 实盘店内每个 cut_type 总重
              expected_remain_kg_by_cut = 入库 - 已销 - 报损(按 cut 维度)
              // 不盘点不阻止次日销售 — 未盘点部分作为次日库存继续销售,
              //   当日 fresh-meat 毛利按 llm 推演 / 历史均值估算,BI 标注
              调 LLM(若有盘点):用当日销售反推每头猪"实际分割比例"
                     标记"分割异常的猪"与"明日分割建议"
              → 写 pork_cuts_stocktake.llm_review_json
```

### 4.2 LLM 输入数据组装(早盘 + 日终)

每次调 LLM 必须**按单头猪独立组装**输入,不汇总后算比例:

```jsonc
{
  "pig_id": "uuid",
  "ear_tag": "...",
  "gross_weight_kg": 312.5,
  "purchase_unit_price_yuan": 28.0,
  "history_pigs": [
    // 近 30 天相似重量段(±10%)的猪,作为"对照样本"
    {"gross_weight_kg": 305, "actual_cuts": {"belly": 92, "tenderloin": 18, "rib": 35, ...}},
    {"gross_weight_kg": 318, "actual_cuts": {...}}
  ],
  "task": "predict_cuts" | "review_cuts"
}
```

> **不笼统用 650×瘦肉比例** —— 每头独立推演,LLM 输出的每个 cut 重量是这头猪独立的预测值。

### 4.3 LLM 失败降级

`llm-gw` 失败 / 超时(>30s)时(LLM 网关为外部服务,不在本仓):
- 早盘建议降级为"历史同重量段均值"
- 日终反推降级为"历史损耗均值"
- 不阻塞业务流程,只降级分析质量

### 4.4 盘点可选 + 不阻断销售 + BI 标注

`fresh-meat` 盘点**非强制**:

- **不盘点不阻止次日销售** —— 未盘点的店内剩余分割肉作为"次日开盘库存"继续销售。
- 当日 fresh-meat 毛利在 BI 报表上**显式标注**:
  - 当日已盘点 → "✓ 已盘点(实测值)"
  - 当日未盘点 → "⚠ 未盘点(数据为 llm 推演 / 历史均值)"
- BI 不掩盖数据来源 —— 销售毛利数据真实性优先于"毛利数字好看"。
- 用 Dapr cron binding 每天 23:00 提醒员工盘点(非阻断,仅提示)。

### 4.5 盘点表名与粒度(取代 §1.6 的 `pig_stocktake`)

- 表名:`fresh_meat.pork_cuts_stocktake`(整店按部位盘点)
- 粒度:**门店 + cut_type** 一行(不是一头猪一行)
- 关键字段:
  - `id`、`branch_id`、`taken_at`、`taken_by`
  - `cuts`:JSON 数组,每个元素 `{cut_type, actual_remain_kg, expected_remain_kg, variance_kg}`
  - `is_complete` — 当日所有猪只的部位是否都覆盖(用于 BI 标注;true=已盘点全店,false=部分或未盘)
  - `llm_review_json` — LLM 反推(可选)
- 与整猪的关系:当日 sales-agg 反算毛利时,**按 cut 维度聚合**,不按 pig 维度反推整只重量。

### 4.6 部位枚举(cut_type)—— 2026-10-23 扩到 34 类

`cut_type` 是**封闭集合**(未知值被 `RecordPigCut` / `RecordPorkCutsStocktake` 直接拒),
命名对齐**行业标准猪部位名**。原有 12 值的 wire 名**一个都没改** ⇒ 历史数据全部仍然有效。

| 组 | wire | 中文名 |
|---|---|---|
| 整猪 | `whole_pig` | 白条猪/整猪 |
| 分割肉 | `head` | 猪头(头肉) |
| 分割肉 | `head_skin` | 猪头皮 |
| 分割肉 | `ear` | 猪耳 |
| 分割肉 | `tongue` | 猪舌(猪利) |
| 分割肉 | `neck` | 猪颈肉(血脖) |
| 分割肉 | `leg_front` | 前腿肉(猪肩) |
| 分割肉 | `shoulder_blade` | 梅花肉(前夹心) |
| 分割肉 | `belly` | 五花肉 |
| 分割肉 | `rib` | 肋排/排骨 |
| 分割肉 | `tenderloin` | 里脊肉 |
| 分割肉 | `lean` | 瘦肉 |
| 分割肉 | `leg_rear` | 后腿肉 |
| 分割肉 | `trotter` | 猪蹄(前蹄/后蹄) |
| 分割肉 | `tail` | 猪尾(尾骨) |
| 分割肉 | `skin` | 猪皮 |
| 分割肉 | `fat` | 板油/猪油 |
| 分割肉 | `local_pork` | 隔山肉(本地土猪)—— **品种级**,不是解剖部位 |
| 骨 | `bone_shank` | 筒骨(腿骨) |
| 骨 | `bone_scapula` | 扇骨(肩胛骨) |
| 骨 | `bone_spine` | 龙骨(脊骨) |
| 骨 | `bone_rib` | 沙骨(肋骨) |
| 骨 | `bone_head` | 猪头骨 |
| 下水 | `liver` | 猪肝 |
| 下水 | `heart` | 猪心 |
| 下水 | `lung` | 猪肺 |
| 下水 | `kidney` | 猪腰(肾) |
| 下水 | `stomach` | 猪肚(胃) |
| 下水 | `stomach_honeycomb` | 猪小肚(蜂巢肚) |
| 下水 | `intestine` | 大肠(生肠) |
| 下水 | `intestine_head` | 大肠头 |
| 下水 | `intestine_small` | 粉肠(小肠) |
| 下水 | `spleen` | 猪横利(脾) |
| 下水 | `tendon` | 猪鞭 |

**约束**:`local_pork` 是品种级商品,**不进整猪分割推演**(一头普通猪分割不出隔山肉;
混进去只会让 LLM 产生幻觉)。见 `model.CarcassCutTypes`。

**部位 ↔ SKU 映射**:一张 `branch_cut_mappings` 表,唯一约束是
`UNIQUE(branch_id, cube_product_id)` ⇒ **一个 SKU 在一个店只对应一个部位**,
而**一个部位可以挂多个 SKU**(真源库两家店都是常态:
佳乐 belly = 腩肉(五花肉) + 精品五花肉;富百家 rib = 精选腩排 + 精选排骨)。
装配数据与推导过程见 `internal/fresh-meat/seed/cut_mappings.go`。

### 4.7 成本口径与库存位置(2026-10-23 改)

#### 4.7.1 为什么换口径

旧口径把"今天卖的钱"按"今天进场的猪"摊成本:

```
pigs := 当日进场的猪
costForCut[cut] = Σ pig(今天的猪)(pig_cost × 该猪该部位占比)
case purch.IsZero(): ratio = 1        ← 今天没产出该部位 ⇒ 比例按 100%
consumedCost := costForCut[cut].Mul(ratio)   ← 但 costForCut = 0
```

⇒ **今天卖昨天剩的五花肉(今天没进猪 / 今天的猪没出五花)= 成本 0、毛利率 100%。**
不是偶发 —— **只要店里在卖隔夜货就每天都发生**;而且旧口径里**没有"昨天"的概念**。

#### 4.7.2 新口径:SKU 级移动加权平均

```
期初数量 Q0 = 昨日日终实盘(全店)          ← 自动取,零人工
期初金额 V0 = Q0 × 昨日该 SKU 的移动加权单价
本期入库 Q1 = 今日分割产出到该 SKU 的 kg
本期入库 V1 = Σ(今日产出该 SKU 的每头猪的成本分摊额)
可售总金额   = V0 + V1        可售总数量 = Q0 + Q1
本期移动加权单价 = 可售总金额 / 可售总数量
期末数量 Q2 = 今日日终实盘(全店)
期末金额 V2 = Q2 × 本期移动加权单价
本期销售成本 = 可售总金额 − V2 − 统收成本 − 报损成本
本期毛利     = 本期销售收入 − 本期销售成本
```

**猪成本按产出比例逐猪摊**(不按全店池化):一头猪的成本只摊到它自己产出的部位上,
否则"这头猪的五花成本"会串到别的猪的产出上 ⇒ 整猪毛利失真。

#### 4.7.3 库存位置:猪肉台 / 冷库

| 位置 | wire | 含义 |
|---|---|---|
| 猪肉台 | `counter` | 当日在售 |
| 冷库 | `cold_store` | 过夜存放 / 分割副产(板油) / 待统收 |
| 全店 | `all` | = 猪肉台 + 冷库(**查询口径,不是第三个真实位置**) |

**查询只有两层**:默认 `counter`,可选 `all`;**不提供"只查冷库"**。

**位置流水**:
```
搬上台(冷库→柜台)   冷库 −T ; 柜台 +T      ← 全店不变
副产入冷库          冷库 +kg
酒店统收(冷库→出)   冷库 −kg                ← 全店减少(变成收入)
日终过夜(柜台→冷库) 柜台 −P ; 冷库 +P       ← 全店不变
```

⇒ **全店 = 柜台 + 冷库 恒等**;毛利用**全店口径**,转库不影响成本
(成本跟着货走,移动加权单价是全店一个)。

**日终盘点录的是"猪肉台实盘"**(仓管只盘柜台,工作量不变);
**全店实盘 = 柜台实盘 + 冷库余额**,由服务层在保存盘点单时补上。

#### 4.7.4 成本对账行(毛利准不准,一眼可判)

报告带一行 `reconciliation`:

```
期初存货金额 + 本期入库金额 == 销售成本 + 统收成本 + 报损成本 + 期末存货金额
```

差额只允许来自**显式化的损耗**,两者都在 `losses` 里单列:

| 项 | 公式 | 说明 |
|---|---|---|
| 切分损耗 | 进猪毛重 − 产出 kg − 副产 kg | 修边掉料/滴水/血水;以前**完全不在任何表里** |
| 隔夜失重·盘亏 | 昨日全店 − 今日全店 − 今日净变动 | ⚠️ **不可单独测出**(门店按"昨晚称好的重量"上台),只能估 |

⇒ 隔夜失重**不假装能测**:差额显式命名 + 报表给自估偏差百分比
(`近 7 天 (昨日全店−今日全店实盘)/昨日全店`)。

### 4.8 特价档货号(promo_skus,2026-10-23)

**实测结论:特价档是"价格槽位",不是部位。** 成交价逐日变:

```
03001 特价猪肉5元  名义 5 元 → 实际卖 9 元/斤
03002 特价猪肉8元  名义 8 元 → 实际卖 7 元/斤
03007 特价猪肉20元 名义 20 元 → 10-13~10-20 卖 15 元、10-21 卖 20 元
03390 特价猪肉5Y  名义 4 元 → 卖 3.98 / 4.90 / 4.00
```

⇒ **不能静态映射到某个部位**。试过用"当日同价商品"自动反解,匹配上的都是巧合
(8 元/斤 匹配到的是猪头皮 —— 只是因为该店 8 元/斤到处都是)。
但"这些货号是特价"是**稳定**的 ⇒ 登记一次即可。

```
promo_skus(
  id, branch_id, cube_product_id, product_name,
  price_tier_text, note, active, created_at, updated_at
)
UNIQUE(branch_id, cube_product_id)
```

**为什么必须登记**:原来 `resolvePigByCubeProduct` 查不到部位映射就 `continue`
**丢整行** ⇒ 特价销售的**金额和重量一起丢**
(佳乐近 14 个营业日:**成交金额约 5%、成交重量约 10%** 消失;
重量比金额严重 ⇒ kg 少了 ⇒ expected_remain 虚高 + 成本摊销比例偏小
⇒ 部位毛利与整猪毛利**同时失真**)。

**登记后的行为**:销售行不再丢,归到 `promo` 桶(新增枚举值,与 `local_pork` 一样
**不进整猪分割推演**)⇒ 金额与重量进得来,但**不摊到具体部位**
⇒ 毛利报告里单列"特价(未归属)",让这块不精确**可见**而不是静默摊错。

**端点**:`GET /promo-skus` / `POST /promo-skus`(幂等)/ `DELETE /promo-skus/:id`(停用)。
**装配**:佳乐 12 个货号(见 `internal/fresh-meat/seed/promo_skus.go`);
富百家 `010303` 实测**没有**特价档 ⇒ 不登记。
⚠️ 排除 `03005 特价牛肉30Y` —— 那是牛肉,归牛羊肉域。

### 4.9 特价按日缺口分摊(2026-10-23)

特价登记后能进得来,但仍**不知道它卖的是哪个部位的肉**(§4.8:价格槽位,逐日变)。
⇒ 用**账面缺口**反解:

```
gap_c = 期初 + 入库 − 正常销量 − 统收 − 报损 − 期末实盘
```

`gap_c` 为正 ⇒ 该部位有一批货"账面有、实际被特价处理掉了"
⇒ 特价按 `gap_c` 的比例分摊到各部位。

⚠️ **两条口径不能混**:
- **总量取实测**(当天特价档实际成交的 kg / 金额)
- **比例取缺口**(`gap_c`)

因为缺口里混着**没记的损耗** ⇒ 拿缺口当总量会把损耗一起算成特价。
分摊是**纯读路径、不落库** ⇒ 避免"分摊结果被当成下一轮输入"的自反馈。

### 4.10 长期毛利(2026-10-23)

`GET /gross-margin/period?from=&to=&group_by=day|cut|sku`。

单日毛利会被库存波动扭曲(卖得多 ≠ 毛利高,可能卖的是昨天进的便宜陈货);
刚切换到移动加权成本法时单日数值会剧烈波动 ⇒ **只能看长期序列**。

### 4.11 大宗台账:酒店统收 / 板油(2026-10-23)

**门店流程(用户原话,2026-10-23)**:

> 「板油卖不完就日终盘后进入冷库,同时记录分摊成本,次日不再上台销售,
> 累积到收购时间点时录销售出库就可以了。可以只需要结算与结存功能就满足了。」

⇒ 落成三步:**日终盘点 → 自动结转进冷库(记成本)→ 累积 → 卖给酒店(结算出库)**。
⇒ 页面只要「**结存**」与「**结算**」;**进冷库不用手工录**。

#### 表

```
bulk_products       大宗品项(板油 → fat 部位)
                    UNIQUE(branch_id, code) + UNIQUE(branch_id, cut_type)
carryover_records   **结转记录**:进冷库的批次 + FIFO 队列 + 猪归属快照
                    ⚠️ 2026-10-25 起它是大宗进/出冷库**唯一**的记录
                    ⚠️ 2026-11-03 由 `stock_bags` 改名(旧名只来自已下线的旧玩法)
bulk_productions    进冷库台账 —— **2026-10-25 起停写**(表保留,只读历史)
bulk_settlements    统收结算**表头**(单号/结算日期/对方/总金额/备注)
bulk_settlement_items  结算**明细**(品项/数量/单价/金额)
bulk_settlement_allocations  结算收入按**结转记录**摊回猪(carryover_id)
```

⚠️ **大宗 FIFO 队列 = 结转记录队列(2026-10-25 口径修正)**:
旧实现里,大宗结算吃 `bulk_productions` 台账、零售/搬台吃**结转记录**
⇒ 同一批冷库货有**两份"还剩多少"的账**
⇒ 结算按台账扣、零售按结转记录扣 ⇒ **同一批货能被卖两遍**。
⇒ 现在**只有一个队列**(`carryover_records`):
- 结算按产生日 FIFO 领用**结转记录**,领用就扣 `consumed_kg`
  ⇒ 与零售**共用同一个消耗水位** ⇒ 不可能卖两遍;
- 猪归属用**同一套**:结转记录的 `PigAlloc` 归属快照 → 没有才按当日各猪毛重兜底;
- 入冷库成本单价钉在结转记录上(`unit_cost_yuan`)⇒ 压几天后结算,成本仍是入库那天的口径。

`carryover_records.source`:`stocktake`(日终盘点登记,主路径)/
`bulk_manual`(大宗手工兜底)。两者**互斥**(当天已有盘点结转时再手工录 ⇒ 400)
⇒ 杜绝"同一个数字记两处"。
⚠️ 手工兜底那一行**也在 FIFO 队列里**(冷库里有货、账上也得有,否则结不出去)。

⚠️ **大宗品项按部位选、不可重复**(门店要求,2026-10-23):
`UNIQUE(branch_id, cut_type)` ⇒ 同一部位只能登记**一个**大宗品项。
少了这条 ⇒ 同一部位挂"板油"与"猪油"两个品项 ⇒ 结存与结算各算一半、说不清。
⇒ 品项的增删**在页面上做**(门店自己管),不再是只读的装配配置。

⚠️ **结算改成表头 + 明细**(门店要求,2026-10-23):
表头 = 结算日期 / 对方 / 备注 / 总金额;明细 = 大宗品项 / 结算数量 / 单价。
⇒ 一次可以把板油和水油**一起**交给酒店,金额归到同一张单上。
⚠️ 老字段(product_code / from_date / to_date / qty_kg / unit_price_yuan / amount_yuan)
是明细粒度,已**废弃**:改成**可空**而不是 DROP 列 ——
GORM 的 AutoMigrate 不会删列,而 NOT NULL 又会让新 INSERT 缺列直接失败。

#### 三条口径,各管一件事

| 口径 | 来源 | 用途 |
|---|---|---|
| **库存 / 结存** | 位置表**部位主 SKU 的冷库余额** | 物理真相 |
| **归属 / 批次** | `carryover_records`(**结转记录**) | FIFO ⇒ 结算与零售都归到产出它的那头猪 |
| **收入 / 成本** | `line_sales_by_pigs`,`cube_product_id` = **部位主 SKU** | 移动加权自动匹配成本 |

⇒ 结存页的 `balance_kg` 口径 = **还能领用多少**(队列剩余),
与结算 FIFO 吃的是**同一个数** ⇒ 门店在结算页填的量不会超过它。

⚠️ **库存与收入必须记在真实部位 SKU 上,不能用 `bulk:<code>` 伪 SKU**
(2026-10-23 修的真 bug,实测):
伪 SKU 不在部位映射里 ⇒ **没有入库金额** ⇒ 移动加权单价恒为 0
⇒ **统收成本 = 0** ⇒ 卖 10kg 板油收 60 元,毛利显示 **+60 元(毛利率 100%)**,
而真实成本 1000 元、毛利应是 **−940 元**。

⚠️ **毛利公式必须把统收成本与报损成本算进损益**(2026-10-23 同批修):
`gm = 收入 − 销售成本 − 统收成本 − 报损成本`。
原来 `gm = 收入 − 销售成本`,而销售成本里**已经把统收/报损成本减掉了**
⇒ 统收收入与报损都成了"零成本收入" ⇒ 毛利虚高。
(连带修掉一条**旧测试锁错了值**:报损的 64 元成本原来根本没进损益。)

⚠️ **板油必须挂 `fat` 部位** —— 大宗收入要落回产出它的部位,否则那批 kg 永远是 -100%。
⚠️ **录入不指定猪**(门店决策:最省事),猪归属在**结算时**按产出日各猪毛重比例分摊。
⚠️ **收入记在产出日**(权责发生制 + 事后定价回填),不是结算日
⇒ 单猪毛利在正确的日子闭合。

**端点**:`GET/POST /bulk-products` / `GET/POST /bulk-productions` /
`GET/POST /bulk-settlements` / `GET /bulk-balances`。
⚠️ 同 `(branch_id, code)` 再登记 ⇒ **409 冲突**(`code: "conflict"`),
不是 500 —— code 是业务标识(板油=lard),静默覆盖会让历史结算的部位归属前后不一致。
⚠️ `GET /bulk-balances` 给**三条**:台账结余 / 冷库实际余额 / 两者差额
(`ledger_vs_cold_diff_kg`)。差额不为 0 要**显式报出来** —— 藏起来就变成"库存说不清"。

#### ⚠️ 待门店确认:大宗线跟的是"水油"还是"板油"(2026-10-23 实测)

佳乐 `1201` 猪肉类目里,两条线完全不同:

| SKU | 名称 | 节奏 | 量 | 价 | 近 2 个月 |
|---|---|---|---|---|---|
| `00032` | 猪板油 | **每天零售** | 2~25 kg/天 | 2.98 元 | 666 kg / ¥1,985 |
| `03808` | **水油** | **每 9 天整一笔** | 400~870 kg/笔 | 1.80 元 | 4,006 kg / **¥7,210** |

(富百家 `01445 猪油` 同样是**每天零售** ~9.35 kg/天 @6.00 元。)

⇒ 「每 9 天一批 400~870kg」正是**大宗批发**的节奏 ——
对应用户说的「累积到收购时间点时录销售出库」。

#### ✅ 2026-10-23 门店确认:水油是**分割副产品**

⇒ 归**鲜肉域**的大宗线,不是外购油脂商品。

⇒ 再查一步发现:它**走的是普通收银台**(柜台 9999,固定几个操作员),
不是账外统收 ⇒ **它是正常的 POS 零售 SKU**,只是**单笔几百公斤**。

⇒ **这带来一个必须解决的问题**:一笔 400~870kg 的 POS 行**不可能带 pig_id**
(它跨很多头猪)⇒ 若按普通零售行处理,`resolvePigByCubeProduct`
会把它**整笔**归到"当日某一头猪"头上 ⇒ 那头猪的毛利凭空多出几百公斤的收入
(实测一笔 ¥1,123),而成本只属于它自己 ⇒ **整猪毛利失真**。

⇒ **口径(2026-10-25 改:一律走手工统收结算)**:
>
> 2026-10-23 的口径是「**收入由 POS 自动采信**」⇒ 2026-10-25 **推翻**:
> 自动采信已随「POS 自动采信成结算单」一起删除(大宗结算**手工登记**),
> 而 03808 那笔若是按零售行进来 ⇒ 仍会被整笔归到"当日某一头猪"⇒ 失真照旧。
> ⇒ **两条路互斥,且只认手工那条**。

| 环节 | 做法 |
|---|---|
| **收入** | **不在 POS 自动采信** ⇒ 门店在「**统收结算**」手工登记 |
| **POS 那一行** | **不按零售算**（不进 `line_sales_by_pigs`、不吃 FIFO）⇒ 见 `bulk_products.pos_cube_product_id` |
| **猪归属** | 结算时按**结转记录的 FIFO**摊到产出它的那几头猪（与零售同一套队列） |
| **冷库出库** | 结算时**领用冷库重量**（结转记录队列的 `consumed_kg`）⇒ 与零售共用同一个消耗水位 |
| **成本** | fat 部位的移动加权单价（补录选对 SKU 才有入库金额） |
| **人工录入** | ✅ **要**在「统收结算」里录 ⇒ 这是**唯一**的收入入口（漏了会少收入） |

⇒ 落地:
- 部位映射:`03808 水油 → fat`（**保留**：分割补录要用它记水油的产量与成本）
- 大宗品项:「**一个部位**」(`cut=fat`，code/name/unit 服务端自动填，**门店不填 POS 货号**)
  - ⚠️ 但装配会写 `pos_cube_product_id = '03808'` ⇒ 语义是
    **「这条货号不走零售」的标记**（不是"自动采信"）
- 「板油」**不再退役**：`00032 猪板油`是**零售货**（每天 2~25kg）⇒ 照常按零售算；
  `03808 水油`是**攒着批量卖酒店**的 ⇒ 走手工统收。两条线**同一个部位、不同货号**。

⚠️ **安全网**:漏登记时，那批货还挂在冷库里 ⇒ 结存页的「**可领用**」一直显示它
⇒ 门店看结存就知道「还有 N kg 没结」。

⚠️ 富百家(00)的 `01445 猪油`是**每天零售 ~9.35kg** ⇒ **没有大宗线**
⇒ 不装 `pos_cube_product_id` ⇒ 它的 POS 销售照常按零售算。

### 4.12 分割重量统计基线(2026-10-23)

"300kg 的猪能出多少五花"是**数值回归**问题 ⇒ 用真实补录样本拟合统计基线
(体重段 50kg 一段 × 部位 → n/mean/p10/p50/p90),
`MinForecastSamples = 5`,样本不足才降级到 LLM。

⇒ **统计基线优先于 LLM**:数值回归不是 LLM 的强项,基线更准更便宜;
且生产实测 LLM 经常返空(`history_avg` 空历史 ⇒ `cuts: []`)。

### 4.13 通用盘点排除猪肉(2026-10-23)

猪肉归 fresh-meat 域 ⇒ **通用盘点服务端拒绝**(400 + `code: "excluded_scope"`),
不靠前端过滤(会被绕开,且失败静默)。

⚠️ **按货号排除,不按类目**:佳乐 `1201`「猪肉部」353 个 SKU **混有**
羊肉/牛肉/鸡/预制菜 ⇒ 按类目排除会连带挡掉门店真实库存。
排除清单从 fresh-meat 的部位映射**派生**(单一事实源),
并由 fresh-meat 在映射增删改时**推送**到 `stocktake_excluded_scopes`。

---

## §5 Cube ERP 接入(erp-connector)

### 5.1 集成边界

- **不复制 cube 语义层**——cube 是另一仓库( `F:\go\src\github.com\YunBright\cube`)
- **不直连思迅 DB**——只能通过 cube-gateway HTTP `POST /v1/load` 拉
- **单向拉取**(本系统 → cube):cube 不调本系统
- **本系统不修改 cube 数据**:任何写入走本系统自己的 pos/procurement

### 5.2 数据流(详见 DESIGN §7)

```
思迅 DB ─cube semantic-layer─▶ cube-gateway /v1/load
                                    │
                                    │  erp-connector
                                    │    - 定时拉(每 5 分钟)
                                    │    - 差量 + 1h lookback 防漏
                                    │    - 哈希幂等
                                    ▼
                本系统 PG: erp_sales_raw
                                    │
                                    │  pub/sub: erp.sale.ingested
                                    ▼
                sales-agg ────▶ BI 拉取 + 其它 dapr app 反查
```

### 5.3 字段映射

走 `docs/FIELD_SPEC.md` §3 mapping-*.yaml,erp-connector 启动时 Load。

### 5.4 多源 ERP 支持

- 同 store 可挂多个 cube app(`sixun-hbposv7`、`sixun-ysx`)
- 同一 model 不同实例的 mapping.yaml `target` 必须一致(FIELD_SPEC §4.4)
- erp-connector 内部按 `(source_name, model)` 维度缓存 + 拉取

---

## §6 报表与 BI(bi-gateway)

| 报表 | 数据源 | 触发 |
|---|---|---|
| 销售看板(分钟/小时/日) | sales-agg | 实时 |
| 单品毛利(普通商品) | sales-agg × inventory | 日终 |
| **生鲜蔬果毛利(盘点驱动)** | fresh-produce | 每次盘点 |
| **生鲜肉毛利(LLM 增强)** | fresh-meat | 日终 |
| 供应商对账 | procurement + cube supplier | 日终 |
| 库存预警 | inventory | 实时 |
| 损耗分析 | fresh-produce + fresh-meat waste_logs | 日终 |
| 多端销售对比 | sales-agg(自营 + erp-connector 拉取) | 实时 |

---

## §7 非功能需求

| 维度 | 要求 |
|---|---|
| 性能 | POS 开单 P95 < 500ms;BI 看板 P95 < 2s |
| 可用性 | 关键路径(POS / 库存扣减)允许短暂降级,不可丢单 |
| 一致性 | **最终一致**:跨服务走 pub/sub + outbox pattern |
| 安全 | JWT RS256(dapr sidecar 验签);SQL 全部参数化;敏感字段加密 |
| 审计 | 单据变更(`operator_id` / `created_at` / `updated_at`)全留痕 |
| 可观测 | slog + dapr metrics(默认 `/metrics` 端点) |
| 开发 | Go 1.26+,GORM,所有测试用 `*_test.go`(不写 powershell / python 测试) |

### 7.5 本期数据归属原则(数据模型存在,product/stock 表不建 PG,只读 cube + 转发)

**核心约束**:
- 本系统的**数据模型仍存在**(FIELD_SPEC §1 列出的字段定义、ID 类型、命名约定全部有效),
- 但**不在 PostgreSQL 建** `product` / `category` / `stock` / `supplier` 等思迅已覆盖的主数据表,
- 只用来**约束本系统 PG 表(如 `stocktake_lines.product_id`)与 cube 字段的类型一致性**,
- 真正的数据通过 **cube sixun 模型**(六讯思迅 `t_im_branch_stock` 等)**读取**。

#### 7.5.0 数据模型存在但不建表的理由

- **ID 类型必须一致**:本系统 `stocktake_lines.product_id` 必须与 cube `product.id` 同类型(思迅 `item_no`,string),
  否则关联不上。**`FIELD_SPEC §1.1 catalog product` 的字段定义就是类型契约**。
- **不复制数据**:cube 已经是单一真相源,本系统再 cache 一份会引入不一致风险。
- **接口透传**:catalog / inventory / master-data 服务的 GET 接口直接转发 cube `/v1/load` 的响应,
  payload 的字段名 / 类型沿用 cube schema,**不做字段名重命名**(避免前端混用)。

| 数据 | 思迅是否有 | 本系统 PG 是否建表 | 数据模型字段定义 | 谁提供真实数据 |
|---|---|---|---|---|
| **SKU / 商品(`product`)** | ✅ cube product model | ✅ **catalog.products(本地,带 branch_id)** | FIELD_SPEC §1.1(类型契约) | catalog 本地表 + cube 兜底(cube-router 转发) |
| 分类(`category`) | ✅ cube category model | ❌ **不建** | FIELD_SPEC §1.1 同上 | cube-gateway(`/v1/load` 经 cube-router 转发) |
| **供应商(`supplier`,type=0)** | ✅ cube supplier model | ✅ **catalog.suppliers(本地,带 branch_id)** | FIELD_SPEC §1.1 同上 | catalog 本地表 + cube 兜底 |
| **客户(`supplier`,type=1)** | ✅ cube supplier model | ✅ **catalog.suppliers(本地,type=1)** | FIELD_SPEC §1.1 同上 | catalog 本地表 + cube 兜底 |
| 实时库存(`stock`) | ✅ cube stock model | ❌ **不建** | FIELD_SPEC §1.2 同上 | cube-gateway(`/v1/load` 经 cube-router 按 X-Branch-ID 路由) |
| 销售明细(`sale_detail`) | ✅ cube sale_detail model | ❌ **不建** | FIELD_SPEC §3.2(mapping) | erp-connector 拉后落 `erp_sales_raw` |
| **门店 / 员工 / 班次** | ❌ cube 无 | ✅ **建** | FIELD_SPEC §1.8 | master-data 服务 |
| **盘点表 / 盘点明细** | ❌ cube 无 | ✅ **建** | FIELD_SPEC §1.7 | stocktake 服务 |
| **POS 开单(本系统自营)** | ❌ cube 无(本系统的) | ✅ **建** | FIELD_SPEC §1.3 | pos 服务 |
| **采购收货(本系统自营)** | ❌ cube 无(本系统的) | ✅ **建** | FIELD_SPEC §1.4 | procurement 服务 |
| **生鲜蔬果特有表** | ❌ cube 无 | ✅ **建** | FIELD_SPEC §1.5 | fresh-produce 服务 |
| **生肉特有表** | ❌ cube 无 | ✅ **建** | FIELD_SPEC §1.6 | fresh-meat 服务 |
| **促销 / 改价** | 思迅有(外部 POS) | ✅ **建**(自营 POS 用) | FIELD_SPEC §1.8 | pricing 服务 |

#### 7.5.1 "数据模型存在" 的代码语义

```go
// 内 internal/stocktake/model/product.go(或不建,直接引用 cube 字段)
// 本系统**不创建** GORM 表,但 Go struct 类型仍需要用于响应/契约:

// StocktakeLine.Gorm 模型(本系统建表)
type StocktakeLine struct {
    ID            string
    HeaderID      string
    ProductID     string    // = cube product.id = 思迅 item_no(string)
    ProductName   string    // 录入时从 cube 拉的冗余
    Unit          string    // 同上
    Spec          string    // 同上
    BookQty       decimal.Decimal  // cube 快照
    BookQtyAt     time.Time        // 快照时间
    ActualQty     decimal.Decimal
    DiffQty       decimal.Decimal  // = ActualQty - BookQty
    AvgCostYuan   decimal.Decimal
    DiffAmountYuan decimal.Decimal
    DiffReason    string
    ...
}

// ProductDTO(不建 GORM 表,只作为响应类型,字段约束对齐 cube)
type ProductDTO struct {
    ID         string  `json:"id"`           // = 思迅 item_no
    Name       string  `json:"name"`         // = 思迅 item_name
    CategoryID string  `json:"category_id"`  // = 思迅 category_id
    ...
}
```

> **核心约束**:`StocktakeLine.ProductID` 的类型(string)、取值范围(思迅 item_no)
> 与 `ProductDTO.ID` 完全一致 —— 由 `FIELD_SPEC §1.1` 集中定义,Go 代码两边同步约束。

#### 7.5.1 "只转发"的代码语义

各服务里**不写** SKU 增删改查接口;**只写**透传接口:

```go
// catalog 服务示例(GIN handler)
// 本期:catalog.GET("/products/:id") → 转 cube-gateway /v1/load
// 本期:catalog.POST / PUT / DELETE /products/*  → 不存在,返回 404

// stocktake 服务示例
// 本期:stocktake.GET("/products") → 转 cube-gateway(给前端录入页用)
// 本期:stocktake.GET("/stock?product_id=...&branch_id=...") → 转 cube-gateway(账面数)
// 本期:stocktake.POST /stocktake-headers  → 本系统自己的 stocktake_headers 表
```

#### 7.5.2 暂存 vs 同步

- **思迅侧已有数据**(SKU/库存/销售/供应商):本系统**不缓存**(除非为了性能后续加短期缓存),直接从 cube-gateway 转发。
- **本系统自营数据**(POS 开单 / 采购收货 / 盘点表):本系统**自己 PG 存储**。
- 二者边界清楚,**不允许在同一接口里混拉**(避免数据源不一致)。

#### 7.5.3 后续 Phase 演进(本期不做)

- 若 cube 不可达,加 5 分钟级短期缓存(redis)
- 若思迅某些字段本系统需要写回(如下架、调价),开通双向同步通道(本期不预留)

---

## §8 反需求(明确不做)

- ❌ 开发初期**不写** docker / docker-compose / k8s 部署文件 —— 用 Dapr local(`dapr init` + `dapr run`)
- ❌ 不写 `Makefile` / 启动脚本 —— `go test ./...` + `dapr run` 命令手工敲
- ❌ 不写 powershell / python 测试脚本 —— 全部 Go `*_test.go` + go test
- ❌ 不在 schema 层做枚举归一化(enum_map) —— 留给 schema.yaml meta + BI 层
- ❌ 不复制 cube 的语义层 / 不维护思迅表结构 —— erp-connector 只拉数据
- ❌ **本期不维护 SKU / 分类 / 供应商 / 实时库存**(思迅已有) —— 全部从 cube-gateway 转发,本系统不写 CURD

---

## §9 修订记录

| 日期 | 修订人 | 内容 |
|---|---|---|
| 2026-09-17 | Mavis 起草 | 初版,基于 v0.1 设计对话 |
| 2026-09-17 | Mavis | 修订 2:3 项 authkit 开发需求已落地(REQUIREMENTS §1.4 改为已合并状态) |
| 2026-09-17 | Mavis | 修订 3:生肉盘点粒度改为"整店按部位"(REQUIREMENTS §4.1 §4.4 §4.5),不盘点不阻断,BI 标注推演 |
| 2026-09-17 | Mavis | 修订 4:加 §2.1 盘点管理(stocktake 表 + 差异表)+ §7.5 本期数据归属原则(只读 cube + 转发,SKU/库存/供应商从 cube 转) |
| 2026-09-17 | Mavis | 修订 5:stocktake 实时盘点(营业中)+ §7.5 强化"product 表不建 PG 但数据模型存在" + 盘点单不跨店 |
| 2026-09-17 | Mavis | 修订 6:`/api/v1/products/search` 接口契约(对齐 scan.html 后端 SearchProducts)+ barcode 权限过滤 + barcode 长度策略 |
| 2026-09-24 | Tinkler | 修订 7:branch 重构(REQUIREMENTS §1.2 三元权限 `user × branch × scope` + `rbac.RequireScopeWithBranch`);§1.3 数据级权限改用 authkit 两套中间件;§1.4 加 `RequireScopeWithBranch` / `HasAnyScopeWithBranch` / `GetBranchPermissions` 三条已合并;§2 模块表加 catalog / cube-router 行列(supplier + product 迁 catalog,加 cube-router 行);§2.1.4.1 `/products/search` 归属 catalog + 中间件守门;§7.5 数据归属表 suppliers/products 改 catalog 本地表,stock 走 cube-router;保留全部 12 占位 cmd(用户指令"不用删除占位");go.mod 保留 `replace ../authkit` |