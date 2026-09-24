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
| 验签 | **dapr middleware.http.bearer**(sidecar 验签) | 各 dapr app **不重验签** |

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
- path `:branch_id` —— `/stock/:branch_id/:product_id` / `/branches/:branch_id/...`
- X-Branch-ID header —— catalog / cube-router / stocktake 大多数端点
- body `branch_id` —— `POST /stocktake-headers` 用 body 字段
- 关联实体的 branch —— `stocktake_lines` 通过 header→hdr→line 反查

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

`llm-gw` 失败 / 超时(>30s)时:
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