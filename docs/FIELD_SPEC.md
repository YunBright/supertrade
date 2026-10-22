# FIELD_SPEC — 本系统 dapr app ↔ cube sixun 族字段映射契约

> 唯一真相源(Single Source of Truth):所有本系统 dapr app 与 cube sixun 族之间
> 的字段对应,必须以本文件为准。新增/删除字段须 PR 同步本文件。
>
> **风格继承**:`F:\go\src\github.com\YunBright\cube\pkg\fieldmapping`
> —— 只做字段名 + 类型 + 单位对齐;不做 enum_map、不做 transform、不做派生字段。

---

## §0 命名约定(全局)

| 类型 | 命名规则 | 示例 |
|---|---|---|
| 主键 | `id`(字符串 UUID,或领域内唯一编码) | `id` = `PI20260917001` |
| 外键 | `<entity>_id` | `supplier_id` / `branch_id` / `product_id` |
| 金额 | `<name>_yuan` 或 `<name>_amount`,类型 `decimal`,单位 `元` | `total_amount_yuan` / `cost_yuan` |
| 数量 | `<name>_qty` / `<name>_quantity`,类型 `decimal` | `inbound_qty` |
| 重量 | `<name>_kg` 或 `<name>_weight_g`,类型 `decimal` | `gross_weight_kg` |
| 时间 | `<name>_at`,类型 `datetime` | `created_at` / `sold_at` |
| 日期 | `<name>_date`,类型 `date` | `purchase_date` |
| 状态 | `status`,类型 `string`,枚举值由 schema 维护 | `draft` / `pending` / `approved` |
| 比率 | `<name>_rate` / `<name>_ratio`,类型 `decimal`,单位 `%` | `gross_profit_rate` |

> **不在 schema 层做枚举归一化**(`class_id '1'→food` 这种事)——
> enum_map 留给 schema.yaml meta + BI 层;FIELD_SPEC 只定义"同一语义字段在不同源里的取值集合是相同集合"。

---

## §1 本系统 dapr app 字段清单(业务侧)

> 来源:`F:\go\src\github.com\YunBright\supertrade` 各 dapr app 内 GORM 模型。
> 下列清单是**目标 schema**(target);它们跟 cube source 的对应关系见 §3。

### 1.1 catalog(商品 / 分类 / 供应商,dapr app `catalog`)

**本期定位(2026-10-20 更新)**:
- **本地维护** `catalog.suppliers` 表(带 `branch_id`,复合主键 `(id, branch_id)`)—— 本系统数据
- **本地维护** `catalog.products` 表(带 `branch_id`)—— **2026-10-20 实装 CRUD**
  (写侧 `POST/PUT/DELETE /products`;读侧一律按「配没配数据源」分流:
   配了 ⇒ 走那个 cube 实例,没配 ⇒ 走本域 PG)
  ⚠️ 旧的本地读侧 `GET /products/local` **已于 2026-10-22 删除**
  (名字承诺"只读本地表",与分流判据两套话 ⇒ 同一个门店两个答案);
  商品列表/搜索统一走 `GET /products` / `GET /products/search`,离线包走 `GET /products/export`
- **本地维护** `catalog.categories` 表(带 `branch_id`)—— **2026-10-20 新增**
  (自营门店没有 cube 源,分类必须本系统持有)
- **不再代理** cube `product` / `category` 转发;cube 转发**仅在 picker 端点**保留

> ⚠️ **价格不在 `catalog.products`**:售价/促销归 **pricing** 服务
> (`price_lists` / `promotions`,带生效期与历史版本)。两处都存价 = 两个真相源。

> ⚠️ **写侧只读闸门(2026-10-22)**:`POST/PUT/DELETE /products`、`/categories`、
> `/suppliers`、`/products/:id/barcodes` 在**配了数据源的门店**一律返
> **409 `member_source_is_external`**(与会员域同名同码)。
> 原因:读侧已按数据源分流 ⇒ 本域 PG 里对有源门店的行**永远没人读**
> ⇒ 不拦就是"写进一行没人看的商品,而前台显示写成功了"。
> 闸门与归档导入路径**复用同一个判据**(`internal/cubesource`),
> 与会员域的 `requireLocalSource` 中间件同款。
> ⚠️ 问不到数据源路由时**放行**(cube-router 抖一下不该挡住自营店建档)。

`catalog.suppliers`(本地表):

| 字段 | 类型 | 单位 | 说明 |
|---|---|---|---|
| `id` | string | - | 供应商 PK(同 ID 可在不同门店出现) |
| `branch_id` | string | - | 门店;复合主键 `(id, branch_id)` |
| `name` | string | - | 供应商名 |
| `type` | string | - | `"0"`=供应商 / `"1"`=客户 |
| `contact` | string | - | 联系人 |
| `phone` | string | - | 电话(索引 `(branch_id, phone)`) |
| `email` | string | - | |
| `address` | string | - | |
| `status` | string | - | `active` / `inactive`(索引 `(branch_id, status)`) |
| `created_by` / `updated_by` | string | - | JWT `sub` |
| `created_at` / `updated_at` | datetime | - | |
| `deleted_at` | datetime | - | 软删(**`gorm.DeletedAt`**) |

`catalog.products`(本地表,2026-10-20 实装 CRUD):

| 字段 | 类型 | 单位 | 说明 |
|---|---|---|---|
| `id` | string | - | 复合主键 `(id, branch_id)`,= 思迅 `item_no`(**唯一**) |
| `branch_id` | string | - | 门店 |
| `name` | string | - | 商品名(索引 `(branch_id, name)`) |
| `category_id` | string | - | 分类(索引 `(branch_id, category_id)`) |
| `supplier_id` | string | - | 主供应商 |
| `sub_code` | string | - | **自编码 / 助记码**(思迅 `item_subno`,索引 `(branch_id, sub_code)`)—— ⚠️ **不唯一** |
| `is_exchange` | boolean | - | **换购品**标记(思迅用品名前缀「(换购)」承载) |
| `is_void` | boolean | - | **停用品**标记(思迅用品名前缀「（停用）」承载) |
| `barcode` | string | - | **主条码**(索引 `(branch_id, barcode)`)—— 全集在 `catalog.product_barcodes` |
| `unit` | string | - | 基本单位(思迅 `unit_no`,值即单位名本身如"提") |
| `spec` | string | - | 规格(思迅 `item_size`;实测有 "1*10" 这类脏值,**原样存**) |
| `status` | string | - | `active` / `inactive` |
| `created_by` / `updated_by` | string | - | JWT `sub`;**批量导入时填 `import`** |
| `created_at` / `updated_at` | datetime | - | |
| `deleted_at` | datetime | - | 软删(**`gorm.DeletedAt`**) |

> ⚠️ **不存价格**:见上面的说明。POS 扫码要价走 pricing。
>
> ⚠️ **没有 `origin_branch_id`**:基础档案的**来源**不记录在业务字段上。
> 数据可以从任何地方进来(cube 导出、Excel 导入、手工建档)——
> 归属哪个门店由 `branch_id` 决定,**谁建的**由 `created_by` 决定
> (`import` / `admin` / 某个收银员工号)。多一个"来源门店"字段只会
> 让人误以为"数据只能从某个地方来"。

> ⭐ **三种编码,三种语义,三种基数**(思迅实测 2026-10-21,别再混了):
>
> | 概念 | 源列 | 基数 | 唯一? |
> |---|---|---|---|
> | **货号** | `t_bd_item_info.item_no` | 27,565 个商品 | ✅ 唯一(PK) |
> | **自编码 / 助记码** | `t_bd_item_info.item_subno` | 27,547 个对应 1 个货号,**9 个对应 2 个货号** | ❌ **不唯一** |
> | **商品条码** | `t_bd_item_barcode.item_barcode` | 153 个条码**各自只对应 1 个货号**;而**一个货号有 1~7 个条码** | ❌ 1 商品 : N 条码 |
>
> - `item_subno` 与 `t_bd_item_barcode.item_barcode` **零交集**(实测 JOIN = 0 行)⇒ **两张表是完全不同的东西**。
> - 源库自己只给 `item_subno` 建了**非唯一**索引(`i_bd_item_info_1`)⇒ 1:N 是设计,不是脏数据。
> - `item_subno` **覆盖率 100%**(27,565 个商品全有)。
>
> ⭐ **那 9 组"一个自编码两个货号"全是「正常品 + 换购品」配对**(实测):
> `6904406383122` → `02000110` 远航九江双蒸酒精品500ml(16.50 元)
> / `02000111` **(换购)**远航九江双蒸酒精品500ml(5.00 元)。
> 另 8 组全是槟榔/酒类(口味王、湘潭铺子、和成天下…)。
>
> ⇒ ⭐ **扫一个编码可能命中多个商品** ⇒ 查商品的端点**必须返回候选列表**,
> 收银员要能选。`GET /products/by-barcode/:code` 返回 `{products:[…], count:N}`。
>
> ⭐ **换购/停用靠品名前缀承载,必须解析成字段**:实测品名含「换购」19 个、
> 以「（停用）」开头 17 个、带括号前缀共 62 个;而换购品的 `status` 也是 `1`(=在售),
> 售价多为 `.0000` 或象征价(1 元 / 15 元)⇒ **不单独售卖**,是"搭售 / 积分换购"的载体。
> ⇒ 不解析成字段的话,POS 界面上会出现「(换购)远航九江…」这种脏品名,
> 而且**没法按"是否换购 / 是否停用"过滤**(图1 商品选择器就有「是否积分」列)。
> ⇒ `catalog.products` 默认**不返回**换购品 / 停用品,需要时显式传 `include_special=true`。

`catalog.product_barcodes`(本地表,2026-10-21 新增):

| 字段 | 类型 | 单位 | 说明 |
|---|---|---|---|
| `product_id` | string | - | 复合主键 `(product_id, branch_id, barcode)` |
| `branch_id` | string | - | 门店 |
| `barcode` | string | - | 条码(索引 `(branch_id, barcode)`——**扫码查商品的主路径**) |
| `is_primary` | boolean | - | 是否主条码(= `products.barcode` 冗余的那条);一个商品**至多一条** |
| `label` | string | - | 条码说明(思迅 `other1`,实测"草莓味"/"桔子味") |
| `created_by` | string | - | JWT `sub` |
| `created_at` / `updated_at` | datetime | - | |
| `deleted_at` | datetime | - | 软删(**`gorm.DeletedAt`**) |

> ⭐ **为什么必须 1:N**:思迅的**商品条码**在独立表 `t_bd_item_barcode`,
> 实测**每个 `item_barcode` 只对应 1 个 `item_no`**,而**一个 `item_no` 可有 1~7 个条码**
> (分布:1 个 87 个、2 个 11 个、3 个 7 个、5 个 2 个、6 个 1 个、7 个 1 个)
> ⇒ 1 商品 : N 条码。
>
> ⚠️ **别与"自编码"混**:自编码是 `t_bd_item_info.item_subno`(见上一节的表),
> 它与 `t_bd_item_barcode.item_barcode` **零交集**(实测 JOIN = 0 行)——
> **两张表是完全不同的东西**。
>
> ⚠️ **替换语义**:导入一个商品时,它的条码集合是**权威的**——
> 源库删掉的条码要跟着消失(增量式"只加不减"会留下**幽灵条码**,扫出来是错商品)。

`catalog.categories`(本地表,2026-10-20 新增):

| 字段 | 类型 | 单位 | 说明 |
|---|---|---|---|
| `id` | string | - | 复合主键 `(id, branch_id)`,= 思迅 `t_bd_item_cls.item_clsno` |
| `branch_id` | string | - | 门店 |
| `name` | string | - | 分类名(索引 `(branch_id, name)`) |
| `parent_id` | string | - | 父分类(索引 `(branch_id, parent_id)`)—— **导入必须带** |
| `sort_order` | integer | - | 同级展示顺序 |
| `status` | string | - | `active` / `inactive` |
| `created_by` / `updated_by` | string | - | JWT `sub`;**批量导入时填 `import`** |
| `created_at` / `updated_at` | datetime | - | |
| `deleted_at` | datetime | - | 软删(**`gorm.DeletedAt`**) |

> ⚠️ **父子关系成环会在写入时被拒**(`category_cycle` → 400):分类是树,
> 成环 ⇒ 建树时无限递归且**不报错**。父节点在库里查不到时该节点当**顶级**处理
> (与 cube 侧 `buildCategoryTree` 同一套口径)。

### 1.1.1 `GET /products/search` 响应字段(catalog 本期实装)

响应结构(沿用 §2.1.4.1 接口契约):

| 字段 | 类型 | 权限过滤 | 说明 |
|---|---|---|---|
| `barcode` | string | - | 商品条码(本期 = item_no) |
| `product_id` | string | - | = cube `product.id` |
| `product_name` | string | - | cube `product.name` |
| `category` | string | - | cube `category.name` |
| `brand` | string | - | 扩展字段(本期 mock) |
| `unit` | string | - | 基本单位 |
| `price` | decimal | - | 零售价(本期 mock) |
| `stock_qty` | decimal | `inventory:view` | cube `stock.total_quantity`(经 cube-router) |
| `avg_cost_yuan` | decimal | `inventory:view` | cube `stock.avg_cost` |
| `supplier_id` | string | `supplier:view` | 主供应商 ID |
| `supplier_name` | string | `supplier:view` | cube `supplier.name` |

权限过滤规则:`rbac.RequireScopeWithBranch("product:view", branchFromHeader, resolver)` 中间件负责 per-branch 校验;
`stock_qty` / `avg_cost_yuan` 字段额外需要 `inventory:view` scope;
`supplier_id` / `supplier_name` 字段额外需要 `supplier:view` scope。

### 1.2 inventory(库存域,dapr app `inventory`)

**2026-10-21 起定位变了**：不只代理 cube,**还拥有两张表**并**订阅 `sale.paid`**。

⚠️ 变化的原因：自营门店（门店 10）**没有 cube 源**（`branch_cube_sources` 只有 00 / 01）
⇒ 门店 10 的库存只能落在自己的库里；且卖货扣库存由**事件**驱动，不是同步调用。

`inventory.stock_levels`（**当前库存快照**，按 `(branch_id, product_id)` 唯一）：

| 字段 | 类型 | 单位 | 来源 |
|---|---|---|---|
| `branch_id` | string | - | `X-Branch-ID` |
| `product_id` | string | - | catalog `products.id`（货号） |
| `qty` | decimal | - | 出入库累计（**可负**：无库存快照时卖货会扣成负数） |
| `avg_cost_yuan` | decimal | yuan | 成本档案（**当前恒为 0**，见下） |
| `server_version` | bigint | - | 同步水位 |

`inventory.stock_moves`（**流水，append-only**）：

| 字段 | 类型 | 单位 | 说明 |
|---|---|---|---|
| `id` | string | - | 流水号 |
| `branch_id` / `product_id` | string | - | 门店 / 商品 |
| `move_type` | string | - | `sale` / `refund` / `stocktake` / `adjust` / `in` / `out` |
| `qty` | decimal | - | **负 = 出库**（销售），正 = 入库（退货） |
| `qty_after` | decimal | - | 变动后库存 |
| `unit_cost_yuan` / `amount_yuan` | decimal | yuan | 金额（**字符串**） |
| `ref_type` / `ref_id` | string | - | **`ref_id` = `sale.paid` 的 `op_id`** |
| `occurred_at` | datetime | - | 发生时刻 |
| `operator_id` / `authorized_by` | string | - | 操作人 / 越权授权人 |

⚠️ **唯一索引 `(branch_id, product_id, ref_type, ref_id)`** —— 这就是"同一笔销售重复投递只落一行"的保证。
⚠️ `avg_cost_yuan` **当前恒为 0**（没有成本档案来源）⇒ 接入 `purchase.completed` 后应改用加权成本。

端点：

| 端点 | scope | 说明 |
|---|---|---|
| `GET /stock-levels?branch_id=&product_id=&keyword=&page=&page_size=` | `inventory:view` | 库存快照 |
| `GET /stock-levels/:product_id` | `inventory:view` | 单个商品库存 |
| `GET /stock-moves?branch_id=&product_id=&move_type=&occurred_from=&occurred_to=` | `inventory:view` | 流水 |
| `GET /dapr/subscribe` | 免鉴权窗口 | 订阅 `sale.paid` |
| `POST /events/sale.paid` | 免鉴权窗口 | 扣库存 / 退货加回 |

### 1.2.1 cube-router(cube 多源路由,dapr app `cube-router`)

**本期定位(2026-09 新增)**:按 X-Branch-ID 路由 `POST /v1/load` 到正确 cube 实例,统一收口 stocktake / catalog / inventory / erp-connector / bi-gateway 等所有需要 cube 的服务。

`branch_cube_sources`(本地表):

| 字段 | 类型 | 单位 | 说明 |
|---|---|---|---|
| `branch_id` | string | - | PK —— 门店 ID |
| `cube_source_name` | string | - | cube 实例 app-id(如 `sixun-hbposv7` / `sixun-ysx`);索引 `idx_cube_source` |
| `enabled` | bool | - | 是否启用,默认 `true` |
| `created_by` / `updated_by` | string | - | admin 用户的 JWT `sub` |
| `created_at` / `updated_at` | datetime | - | |

**路由逻辑**(`POST /v1/load` handler):

1. `rbac.RequireScopeWithBranch("cube:read", branchFromHeader, resolver)` 中间件验三元权限
2. 从 ctx 拿 `branch_id` → 查 `branch_cube_sources`(内存 cache + 60s TTL + singleflight)
3. 拿不到 / `enabled=false` → 503 `cube_source_not_configured` / `cube_source_disabled`
4. 拿 `cube_source_name` → 走 `dapr.Client.InvokeMethodWithContent(ctx, cubeSourceName, "v1/load", "POST", ...)` 转发
5. JWT 透传:`metadata.AppendToOutgoingContext(ctx, "authorization", bearer)` → sidecar 转 outgoing HTTP Authorization
6. 透传 response body

### 1.3 pos(销售单据,dapr app `pos`)

**2026-10-21 起实装**：`sales_orders` / `sale_lines` / `payments` + **发件箱**（`pkg/outbox`）。

`pos.sales_orders`：

| 字段 | 类型 | 单位 | 说明 |
|---|---|---|---|
| `id` | string | - | 单号 `SO<yyyymmdd><6位seq>`（**当天单调递增、不撞**） |
| `branch_id` | string | - | 门店（**只从 header 取**，body 里带一份就能跨店写） |
| `member_id` | string | - | 会员号（空 = 非会员单） |
| `shift_id` | string | - | 班次 |
| `operator_id` | string | - | 操作人（收银员） |
| `authorized_by` | string | - | ★ 越权授权人（图4/图5 的审计） |
| `total_amount_yuan` / `discount_amount_yuan` / `paid_amount_yuan` / `change_amount_yuan` | decimal | yuan | **字符串**（JSON 金额一律字符串） |
| `status` | string | - | `draft` / `paid` / `returned` / `void` |
| `sold_at` | datetime | - | 销售时间 |
| `op_id` | string | - | ★ **幂等键**（uuid，唯一索引） |
| `server_version` | bigint | - | 同步水位 |

`pos.sale_lines`：`id` / `sale_id` / `branch_id` / `product_id` / `product_name` / `product_no` /
`barcode` / `unit` / `qty` / `original_price_yuan` / `unit_price_yuan` / `discount_amount_yuan` /
`amount_yuan` / `is_exchange` / `is_void`

`pos.payments`：`id` / `sale_id` / `branch_id` / `pay_way`（`RMB`/`SWX`/`SZF`/`SAV`/`JFP`/`EKT`）/
`amount_yuan` / `exchange_rate` / `converted_amount_yuan` / `ref_flow_no`

⚠️ **金额自洽强约束**：`total_amount_yuan` 必须 == Σ 明细 `amount_yuan`，
`change_amount_yuan` 必须 == `paid − total` 且 ≥ 0，否则 `409 amount_mismatch`。

端点：

| 端点 | scope | 说明 |
|---|---|---|
| `POST /sales` | `pos:sale` | 开单/收款（**`op_id` 幂等**：重放返回首次那张单，`replayed:true`） |
| `GET /sales` | `pos:sale` | 列表（分页） |
| `GET /sales/:id` | `pos:sale` | 查单（头 + 行 + 收付） |
| `POST /sales/:id/refund` | `pos:refund` | 退货（事件 `order_status="R"`，金额取负） |
| `POST /sales/:id/lines/:line_id/price-override` | `pos:price-override` | 改价（无权限 ⇒ `403 pos_permission_required` + `required_scope`） |

### 1.4 procurement(采购,dapr app `procurement`)

`procurement.purchase_orders` / `procurement.grns`(Goods Received Notes)。字段沿用原 §1 采购入库 + 原 §6 采购结算,但合并为单据头/行结构。

### 1.5 fresh-produce(蔬果,dapr app `fresh-produce`)

| 字段 | 类型 | 单位 | 说明 |
|---|---|---|---|
| `id` | string | - | 盘点单据 PK |
| `sku_id` | string | - | 蔬果 SKU |
| `branch_id` | string | - | 分店 |
| `gross_weight_kg` | decimal | kg | 实盘毛重 |
| `remaining_value_yuan` | decimal | yuan | 期末库存估值 |
| `taken_at` | datetime | - | 盘点时间 |
| `taken_by` | string | - | 盘点操作员 user_id |
| `cycle_id` | string | - | 所属盘点周期(同周期多次盘点共享) |
| `created_at` | datetime | - | |

显性报损(可选录入,字段独立表 `fresh_produce.waste_logs`):

| 字段 | 类型 | 单位 | 说明 |
|---|---|---|---|
| `id` | string | - | |
| `sku_id` | string | - | |
| `branch_id` | string | - | |
| `waste_qty` | decimal | kg | 报损重量 |
| `waste_reason` | enum | - | `rot` / `damage` / `expired` / `other` |
| `waste_at` | datetime | - | |
| `recorded_by` | string | - | |

### 1.6 fresh-meat(生肉,dapr app `fresh-meat`)

整猪录入(早盘,一头一行):

| 字段 | 类型 | 单位 | 说明 |
|---|---|---|---|
| `id` | string | - | 整猪 PK |
| `ear_tag` | string | - | 耳标号 |
| `gross_weight_kg` | decimal | kg | 整猪毛重 |
| `purchase_unit_price_yuan` | decimal | yuan/kg | 采购单价 |
| `purchase_cost_yuan` | decimal | yuan | 采购总额 |
| `supplier_id` | string | - | |
| `branch_id` | string | - | |
| `arrived_at` | datetime | - | 到货时间(早盘) |
| `recorded_by` | string | - | 录入员 user_id |
| `llm_advice_json` | jsonb | - | 早盘 LLM 预期分割建议(五花 kg / 里脊 kg / ...) |
| `created_at` | datetime | - | |

单品补录(部分追加条码):

| 字段 | 类型 | 单位 | 说明 |
|---|---|---|---|
| `id` | string | - | |
| `pig_id` | string | - | 关联 whole_pig.id |
| `cut_type` | enum | - | `belly` / `tenderloin` / `rib` / `leg_front` / `leg_rear` / `trotter` / `liver` |
| `barcode` | string | - | 单品条码 |
| `weight_kg` | decimal | kg | 单品重量 |
| `expires_at` | datetime | - | 保质期 |
| `created_at` | datetime | - | |

日终盘点(**整店按部位**,非整只猪;**可选**,缺失不阻断销售):

| 字段 | 类型 | 单位 | 说明 |
|---|---|---|---|
| `id` | string | - | |
| `branch_id` | string | - | 门店 PK |
| `taken_at` | datetime | - | 盘点时间 |
| `taken_by` | string | - | 盘点员 user_id |
| `is_complete` | bool | - | 当日是否全店盘点(true=已盘全店 / false=部分盘或未盘);BI 据此显式标注"已盘点"或"未盘点,数据为推演" |
| `cuts` | jsonb | - | 数组,每个元素:`{cut_type, actual_remain_kg, expected_remain_kg, variance_kg}` |
| `llm_review_json` | jsonb | - | 日终 LLM 反推 + 明日分割建议(仅 is_complete=true 时写入) |
| `created_at` | datetime | - | |

> **粒度**:门店 + cut_type(五花 / 里脊 / 排骨 / 前后腿 / 猪蹄 / 猪肝)。
> **不盘点不阻断**:当日未录入盘点,作为"次日开盘库存"继续销售,当日 fresh-meat 毛利按 llm 推演 / 历史均值估算,sales-agg 在 BI 标注推演来源。

### 1.7 stocktake(盘点,dapr app `stocktake`,本期重点)

**本期定位**:**实时盘点(营业中、非锁库)**,维护 `stocktake_headers` + `stocktake_lines` 两表,
`book_qty` / `book_qty_at` / `avg_cost_yuan` / `product_name` 在**每行录入时刻从 cube 拉取并冗余落库**(防 cube 改后盘点数据失真)。
**盘点单不跨店**:每行校验 cube `stock` 在该 `branch_id` 下存在。

`stocktake_headers`(单据头):

| 字段 | 类型 | 单位 | 说明 |
|---|---|---|---|
| `id` | string | - | 主键,`ST<yyyymmdd><seq>` |
| `branch_id` | string | - | **盘点门店,创建后不可改** |
| `count_date` | date | - | 盘点日期 |
| `status` | enum | - | `counting` / `adjusted` / `approved` |
| `type` | enum | - | `general` / `produce` |
| `operator_id` | string | - | 录入员 user_id |
| `auditor_id` | string | - | 审核员 user_id(approved 时填) |
| `total_diff_qty` | decimal | - | 差异数量合计(submit 时计算) |
| `total_diff_amount_yuan` | decimal | yuan | 差异金额合计(submit 时计算) |
| `remark` | string | - | |
| `created_at` | datetime | - | |
| `updated_at` | datetime | - | |

`stocktake_lines`(单据行):**book_qty 是该行录入时刻的快照**

| 字段 | 类型 | 单位 | 说明 |
|---|---|---|---|
| `id` | string | - | PK(UUID) |
| `header_id` | string | - | 关联 stocktake_headers.id |
| `product_id` | string | - | cube product.id(思迅 `item_no`,string) |
| `product_name` | string | - | 录入时冗余 cube product.name |
| `unit` | string | - | 冗余 cube product.unit |
| `spec` | string | - | 冗余 cube product.spec |
| `book_qty` | decimal | - | **该行录入时刻的 cube `stock.total_quantity` 快照** |
| `book_qty_at` | datetime | - | **快照时间**(cube 返回的 `stock.updated_at`) |
| `actual_qty` | decimal | - | 实盘录入(counting 状态可改) |
| `diff_qty` | decimal | - | `actual_qty - book_qty`(录入时算) |
| `avg_cost_yuan` | decimal | yuan | 快照时从 cube `stock.avg_cost` 拉 |
| `diff_amount_yuan` | decimal | yuan | `diff_qty × avg_cost_yuan`(录入时算) |
| `diff_reason` | enum | - | `loss` / `overage` / `damage` / `wrong_unit` / `other`(可空) |
| `created_at` | datetime | - | 行创建时间 |
| `updated_at` | datetime | - | |

差异表(**不落库**,实时生成,见 REQUIREMENTS §2.1.3):

| 字段 | 类型 | 说明 |
|---|---|---|
| `header_id` | string | |
| `branch_id` | string | |
| `count_date` | date | |
| `status` | string | 盘点表当前状态 |
| `summary.total_lines` | int | |
| `summary.total_diff_qty` | decimal | |
| `summary.total_diff_amount_yuan` | decimal | yuan |
| `summary.loss_lines` / `overage_lines` / `no_diff_lines` | int | 三类行数 |
| `by_reason[]` | array | 按 diff_reason 聚合的 (lines, qty, amount_yuan) |
| `lines[]` | array | 仅列有差异的行(完整字段) |
| `generated_at` | datetime | 服务生成时间 |

### 1.8 master-data / pricing / sales-agg / erp-connector

字段按各自职责定义,沿用 §0 命名约定,清单在各自 dapr app 落地时再补。
**FIELD_SPEC.md 不重复列举这些——只覆盖跟 cube 有映射的"商品/库存/销售/供应商"4 个核心实体 + 本期重点的 stocktake**。

| 服务 | 本期关键表 / 字段 |
|---|---|
| `master-data` | `stores` / `employees`(本地维护);供应商/客户已搬到 **catalog 本地表**(`catalog.suppliers`),master-data 不再转发 |
| `pricing` | `price_lists` / `promotions`(本地维护,本系统自营 POS 用)—— 详见 **§1.12** |
| `sales-agg` | `sales_view_minute` / `sales_view_daily`(本地宽表,聚合 POS + erp-connector) |
| `erp-connector` | `erp_sales_raw` / `sync_logs`(拉 cube 数据落库,供 sales-agg) |

### 1.12 pricing(售价 / 促销,dapr app `pricing`)

**2026-10-21 实装。** pricing 从"只读 cube 代理"变成"价格的拥有者",
原因是**自营门店(门店 10)没有 cube 源**(`branch_cube_sources` 实测只有 `00` / `01`)
⇒ 门店 10 的售价/促销没有别的地方可取。

⚠️ **售价/促销不归 catalog** —— `catalog.products` **不存价**(两个真相源)。
`catalog.products` 的"当前值"语义与价格的历史/生效期语义不同。

#### 1.12.1 表 `price_lists`(售价)

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | varchar(64) | **价目表编码**(默认 `default`)。预留"会员价目表""团购价目表";当前只有一套 |
| `branch_id` | varchar(64) | 门店 |
| `product_id` | varchar(64) | 商品货号(思迅 `item_no`,与 `catalog.products.id` 同一套编码) |
| `price_yuan` | numeric(18,4) | **零售价**。0 与"没维护价"是两件事,见下 |
| `member_price_yuan` | numeric(18,4) | 会员价。0 = 没有会员价(不是"会员价 0 元") |
| `cost_yuan` | numeric(18,4) | 成本价。0 = 不知道成本。**只用于毛利估算**,不参与收银计价 |
| `effective_at` | datetime | **开始生效的那天**(日粒度,存当天零点) |
| `expired_at` | datetime | **失效的那天**(日粒度);nil = 长期有效。失效**不删行** |
| `status` | varchar(16) | `active` / `inactive` |
| `created_by` / `updated_by` | varchar(64) | 导入写入为 `import` |
| `created_at` / `updated_at` | datetime | |
| `deleted_at` | datetime | 软删(`gorm.DeletedAt`) |

PK = `(id, branch_id, product_id)`。**一行 = 一个商品在一个价目表里的当前价**。

⚠️ **"没维护价" = 该商品在表里没有行**,而不是"有一行、价是 0"。
POS 据此印「未维护售价」;印 ¥0.00 会被顾客当真价。

⚠️ **日粒度**:价签按天印,查价的基准时刻是"基准日的 00:00:00"。
`effective_at` / `expired_at` 都按天判(失效当天仍算有效)。

#### 1.12.2 表 `promotions`(促销)

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | varchar(64) | **促销单号**(思迅 `plan_no`) |
| `branch_id` | varchar(64) | 门店 |
| `product_id` | varchar(64) | 商品货号 |
| `plan_name` | varchar(255) | 促销单名称(印在价签上用于人工追溯) |
| `price_yuan` | numeric(18,4) | **促销价** |
| `orig_price_yuan` | numeric(18,4) | **原价**;0 = 源库没给 |
| `begin_at` / `end_at` | datetime | 活动起止**日**(日粒度,存当天零点) |
| `status` | varchar(16) | `active` / `inactive` |
| `created_by` / `updated_by` / `created_at` / `updated_at` / `deleted_at` | | 同上 |

PK = `(id, branch_id, product_id)`。一个促销单可覆盖多个商品,所以不能只拿 `plan_no` 当主键。

⚠️ **源列语义相反**:思迅 `t_pub_plan_flow.sale_price` = **原价**、`price` = **促销价**。
拿反了价签会印出「原价 ¥3.50 / 促销价 ¥9.90」,**全程不报错**。

⚠️ **同一商品多个生效促销 ⇒ 取促销价最低的那条**
(与 cube `promotion.min_promo_price` 语义一致:用户实际能买到的最低价)。

#### 1.12.3 金额的传输约定(全服务)

- **DB**:`numeric(18,4)`(不是 float/double —— PG 的 double 有二进制误差)。
- **Go**:`shopspring/decimal`。
- **JSON**:**字符串**。JSON number 到了 Dart 侧会被 `jsonDecode` 变成 double,精度当场丢掉
  (9.90 → 9.899999999999999)。
- **导入契约**:见 `internal/archivedto` 的 `ImportPriceRow` / `ImportPromotionRow`。

#### 1.12.4 读路径:本地优先、cube 回落

| 门店 | 判据 | 读哪里 |
|---|---|---|
| `10`(自营) | 本地**有**价格数据 | 本地表。某个商品查不到 = **数据缺口**,显性暴露(不回落) |
| `00` / `01`(过渡期) | 本地**没有**价格数据 | 回落到 cube(`product.avg_price_yuan` / `promotion`) |

⚠️ 回落判据是**按门店**而不是按商品:若按商品判,门店 10 某个没导到价的商品会回落 cube
—— 而门店 10 的 cube 路径必然 503 ⇒ "数据缺口"被翻译成「库存服务暂不可用」,排查方向完全跑偏。

#### 1.12.5 端点

| 端点 | scope | 说明 |
|---|---|---|
| `GET /quotes?barcode=&on_date=` | `pricing:view` | 单条码的价格结论。响应含 `source`(`local` / `cube`) |
| `GET /promotions/active?barcode=` | `pricing:view` | 只返促销部分 |
| `GET /meta` | `pricing:view` | 价签字段名枚举(`promo_date_labels`) |
| `GET /prices?product_ids=` | `pricing:view` | **批量**查售价(POS 一次扫一屏)。判据:配了数据源 ⇒ cube 实时;没配 ⇒ 本域 PG |
| `GET /promotions?product_ids=&on_date=` | `pricing:view` | 批量查当前生效促销(判据同上) |
| `POST /archive/import` | `pricing:manage` | 导入售价/促销(幂等 upsert) |

⚠️ 旧的 `GET /prices/local` / `GET /promotions/local` **已于 2026-10-22 删除**:
名字里的 "local" 承诺"只读本域 PG",而门店 00 在那两条端点上拿到的仍是
**两天前的快照价**,而 `/quotes` 拿的是思迅实时价
⇒ "同一个门店,批量查和单个查的价不一样"。**响应形状逐字不变**(pos-gateway 不用改解析)。

⚠️ `pricing:manage` **已存在**（auth `migrations/003_add_employee_login` 加的 17 条 scope 之一），
写侧端点对**有该 scope 的操作人**放行。

### 1.13 member(会员域,dapr app `member`，2026-10-21 上线)

**六张表**：`members` / `member_types` / `member_point_ledger` / `member_account_ledger` /
`member_settings` / `payment_ways`。

`member.members`（复合主键 **`(id, branch_id)`** —— 思迅卡号**非全局唯一**）：

| 字段 | 类型 | 单位 | 说明 |
|---|---|---|---|
| `id` | string | - | 会员号（思迅 `card_id`，导入时去反引号与前导零） |
| `branch_id` | string | - | 归属门店 |
| `card_type_id` | string | - | → `member_types.id` |
| `card_face_no` | string | - | 卡面卡号（两库列名不同，且 hbposv7 全空 ⇒ 多为空属正常） |
| `name` / `phone` / `mobile` / `gender` | string | - | 基本信息 |
| `birthday` | datetime | - | 生日 |
| `calendar_type` | string | - | `公历` / `农历` |
| `status` | string | - | `normal` / `frozen` / `lost` / `cancelled`（**我们自己的枚举**，不照搬思迅的 0/1/2/3/5） |
| `valid_from` / `valid_until` | datetime | - | 有效期 |
| `favorite` / `id_card_no` / `remark` | string | - | 爱好 / 身份证 / 备注 |
| `fee_amount_yuan` | decimal | yuan | 会员费（**思迅的 `acc_amt` 恒为 0 ⇒ 由我们自己的会员类别配置**） |
| `limited_use_count` | int | - | 限用次数（0 = 不限次；思迅的 `999998` 哨兵导入时转 0） |
| `used_count` | int | - | 已用次数 |
| `points_accumulated` | decimal | - | **累计获得**（思迅 `acc_num` 口径） |
| `points_used` | decimal | - | **累计扣减**（思迅 `dec_num` 口径） |
| `balance_yuan` | decimal | yuan | 零钱包余额（⚠️ 思迅的 `residual_amt` 是**密文/脏值**（实测 `@><:6642675` / `86220..` / `,`，另有 764 行为空）⇒ **解析失败则该行不落库** + warning，⛔ 不静默当 0） |
| `consumed_amount_yuan` / `consumed_count` | decimal / int | yuan | 消费汇总 |
| `password_hash` | string | - | bcrypt（**不存明文**） |
| `face_id` / `open_id` | string | - | 人脸 / 微信 |
| `authorized_by` | string | - | ★ 越权授权人 |
| `op_id` | string | - | ★ **幂等键**（开卡幂等） |
| `server_version` | bigint | - | 同步水位 |

⚠️ **不另立 `points_balance` 列**：剩余积分 = `points_accumulated − points_used`（**派生**）。
多一个列就多一处会漂移的真相源（思迅的口径就是"累计/已用"两列）。

⚠️ **积分规则**：`points_rate = 1.0`（消费 1 元得 1 积分）、`points_rounding = half_up`（**四舍五入**）。
⚠️ 契约原文写的是 `ceiling`，但**数学上 `ceil(22.20)=23` 与思迅那一行的 22 对不上** ⇒ 已按实测改正为四舍五入
（8 行真实流水：四舍五入 8/8、数学 ceil 7/8）。

`member.member_point_ledger`（append-only，`op_id` **唯一索引**）：
`id` / `branch_id` / `member_id` / `op_id` / `direction`(`earn`/`deduct`) / `points` / `points_after` /
`reason`(`sale`/`manual_deduct`/`manual_grant`) / `ref_flow_no` / `operator_id` / `authorized_by` / `operated_at`

⚠️ `points_after` 记**变动后累计获得值**（扣减时累计获得不变，剩余积分现算）。

`member.member_account_ledger`（append-only，`op_id` 唯一索引）：
`id` / `branch_id` / `member_id` / `op_id` / `direction`(`recharge`/`refund`) / `amount_yuan` /
`balance_after_yuan` / `reason` / `ref_flow_no` / `operator_id` / `authorized_by` / `operated_at`

`member.member_types`：`id` / `branch_id` / `name` / `discount_rate` / `fee_amount_yuan` /
`limited_use_count` / `allow_stored_value` / `allow_points_pay` / `priority` / `status` / 审计列

`member.member_settings`（**按门店**一行）：`branch_id` / `membership_enabled` / `points_rate` /
`points_deduct_rate` / `password_required` / `face_required` / `max_face_per_member` / `updated_by` / `updated_at`

`member.payment_ways`：`code` / `name` / `is_integral` / `exchange_rate`（`JFP`=积分支付，`is_integral=1`）

端点（scope 见方案 §6.2）：

| 端点 | scope |
|---|---|
| `GET /config` `/types` `/payment-ways` `/members` `/members/{id}` `/members/{id}/points` `/members/{id}/account` `/members/{id}/consume` `/members/{id}/last-order` `/members/{id}/recharge-card` `/members/{id}/stored-value-card` | `member:view` |
| `POST /members` | `member:card-open` |
| `PUT /members/{id}` | 走越权授权链路（`service.Authorize`），scope `member:config`；**只改身份字段**（姓名/电话/性别/生日/爱好/备注/身份证/有效期/卡面号/类别），⛔ 积分/储值/消费/状态列出现在 body 里 ⇒ **400** |
| `DELETE /members/{id}` | `member:config`（**软删**，`gorm.DeletedAt`） |
| `POST /import` | `member:config`（会员档案导入，body = `archivedto.ImportRequest`，响应 = `archivedto.ImportResult`） |
| `POST /members/{id}/points/deduct` | `member:point-deduct` |
| `POST /members/{id}/points/grant` | `member:point-manage` |
| `POST /members/{id}/account/recharge` `/refund` | `member:account-recharge` |
| `POST /members/{id}/status` | `member:status-manage` |
| `POST /members/{id}/password` | `member:password-manage` |
| `POST /members/{id}/face` | `member:face-manage` |
| `GET /dapr/subscribe` / `POST /events/sale.paid` | 免鉴权窗口（订阅 `sale.paid` 累计积分） |

⚠️ **写端点故意不挂 `rbac.RequireScopeWithBranch`** —— 它只会返 `forbidden`，
会员域要区分**图4**（`member_permission_required` + `required_scope`，弹授权框）与
**图5**（`member_permission_denied`「你没有冲减积分的权限！」）。

⚠️ **只带 `authorized_by` 不再放行**（2026-10-21 安全修复）：
旧实现里"只带 authorized_by"直接放行 ⇒ 任何调用方只要在 body 里填一个工号，
就能在**没有任何权限**的情况下开卡/赠分/充值/改状态。
⇒ 现在：调用方**自己**没有该 scope 时，必须带 `authorization`（工号 + 口令）让 member 验；
调用方自己**有**该 scope 时（`member:*` 在本门店已授权），`authorized_by` 只作为审计信息透传。

#### 1.13.1 会员档案导入（`POST /import`）

数据流：`cube-sync` → `cube-router`（导出 scope=`members` / `member_types`）→ `member`（`POST /import`）。

- **目标门店只从 `X-Branch-ID` 取**（body 里的门店不读）。
- 契约：body = `archivedto.ImportRequest`，响应 = `archivedto.ImportResult`（`by_scope` 两行：`member_types` / `members`）。
- 导出侧**原样带出**思迅的**源值**（`status` 带 card_status 原值、`limited_use_count` 带 `use_num` 原值、`balance_yuan` 带 `residual_amt` 原值）⇒ **语义归一化在导入端做**。
- 思迅 → 本域映射：`card_status` `0/1/2/3/5` → `normal/frozen/lost/frozen/cancelled`
  （⚠️ `3` **无权威文档** ⇒ 保守按 `frozen`；`frozen` 可解冻，而 `normal` 等于放开一张本该停用的卡）；
  `use_num` `999998`/`999999` → `0`（不限次）；`card_type` `0/1/2` → 类别名 `会员卡`/`储值卡`/`VIP储值卡`。
- 会员类别**按名字**对齐本域 `member_types.name`（两边 id 体系不同）⇒ **不自动建类别**（类别是门店自己维护的字典）。
- ⚠️ **默认 `incremental`**：新会员用思迅值初始化；**已存在**的会员**只更新身份字段**，
  ⛔ 不覆盖 `points_accumulated` / `points_used` / `balance_yuan` / `consumed_*` / `status` / `used_count`
  （那些是本域 POS 业务的事实源）。
  `overwrite` 才连财务列一起覆盖（**必须由运维显式选择**）。

### 1.14 pos-gateway(POS BFF，dapr app `pos-gateway`，2026-10-21 上线)

**不拥有任何表**（纯聚合层）⇒ unit 里没有 `POSTGRES_DSN`。

| 端点 | scope | 聚合的下游 |
|---|---|---|
| `GET /home` | `pos:sale` | master-data `/stores` `/shifts` `/employees` + member `/config` `/payment-ways` |
| `GET /config` | `pos:sale` | 同上 + member `/types` |
| `GET /members/{id}/last-order` | `member:view` | member `/members/{id}` + `/last-order` + pos `/sales/{id}` |
| `GET /products/search?q=` | `product:view` | catalog **`/products/search`**(商品选择控件那条,已按「配没配数据源」分流:配了 ⇒ cube,没配 ⇒ 本域 PG) |
| `GET /prices?product_ids=` | `pricing:view` | pricing `/prices` `/promotions`(同样按数据源分流) |
| `POST /sales` | `pos:sale` | pos `/sales`（**body 一字不改转发**，`op_id` 原样透传） |

⚠️ **降级语义**：某个下游取不到 ⇒ 该项 `null` + 响应里带 `warnings[]`，其余照常 200；
但**鉴权失败（401/403）必须整体失败**（不能把 401 吞成"部分成功"）。

⚠️ **内部路径不带 `/v1/`** —— nginx 只剥一层前缀 ⇒ 前端调 `/api/v1/pos-gateway/home`。

---

## §2 cube sixun 族统一 schema(来源侧)

来源:`F:\go\src\github.com\YunBright\cube\sixun-models\<entity>\schema.yaml`。
FIELD_SPEC §3 字段映射只引用这些 cube dimension/measure。

| cube model | 关键 dimension | 关键 measure |
|---|---|---|
| `product` | id, name, category_id, supplier_id, status, created_at | count, avg_price_yuan, total_cost_yuan |
| `sale_detail` | id, order_no, random_order_no, branch_no, product_id, qty, **net_qty**, sale_price, sale_amount, sold_at, sale_way, cashier_id, salesman_id, counter_no, online_flag, discount_amount, cost_price, shift_no, pos_id, spec_sheet_no, uptime | count, total_qty, total_amount_yuan, **net_amount_yuan**(净额) |
| `stock` | product_id, branch_id, updated_at | total_quantity, total_value_yuan, low_stock_count |
| `supplier` | id, name, type(0=供应商/1=客户), contact, phone, email, address, status | count, total_credit_yuan |
| `category` | id, name, parent_id | count |

> ⚠️ **`sale_detail` 没有"单据类型(S/R)"维度 —— 这是源库事实,不是漏做**
> (2026-10-23 两个生产源库实测):
>
> - 逐笔流水表 `t_rm_saleflow` 里**没有退货行**:`sale_qnty` / `sale_money` / `sale_price`
>   的**负值行都是 0 行** ⇒ 不能用"负数 = 退货"判(那样恒等于"全是销售")。
> - 退货是**同一行里的另一列**:ysx 的 `ret_qnty`(实测 1939 行非 0,恒等于同行 `sale_qnty`
>   ⇒ 卖 2.545 件、其中 2.545 件被退);hbposv7 **没有**这一列。
> - ⇒ 共享 schema 给不出"S/R"(没有两库共有的可判列)⇒ 该维度**已删除**。
> - ⇒ **净额**由 family 私有的 live 片段算(见 `net_qty` / `net_amount_yuan`):
>   ysx = (sale_qnty − ret_qnty) × sale_price;hbposv7 = sale_qnty × sale_price。
> - ⇒ **单据级退货**(采购退货单)走 `purchase_sheet` 的 `purchase_return` segment;
>   **销售报表的净销售**走 `sales_report`(`t_rm_daysum` 的 `ret_amt`/`ret_qty`)。

---

## §3 本系统 ↔ cube 字段映射(mapping)

> 文件:`mapping-<cube_model>.yaml`(沿用 cube 命名),由 erp-connector 在启动时 Load。
> 同一 model 的多实例(hbposv7/ysx)`target` 必须完全一致——本系统只取统一 schema。

### 3.1 mapping-product.yaml

```yaml
version: 1
model: product
mappings:
  - source: id            target: 立方系统 sku id
    type: string
  - source: name          target: name
    type: string
  - source: category_id   target: category_id
    type: string
  - source: supplier_id   target: supplier_id
    type: string
  - source: status        target: cube_status
    type: string          # enum: active/discontinued 留给 schema.yaml
  - source: created_at    target: cube_created_at
    type: datetime
```

> 注意:cube `product` 不含 `barcode / spec / unit / origin / brand / shelf_life_days`——
> 这些字段**只在本系统维护**,erp-connector 拉不到时不报错,保留本地值。

### 3.2 mapping-sale_detail.yaml

```yaml
version: 1
model: sale_detail
mappings:
  - source: id              target: cube_flow_id
    type: int
  - source: flow_no         target: cube_flow_no
    type: string
  - source: branch_id       target: branch_id
    type: string
  - source: product_id      target: sku_id
    type: string
  - source: sold_at         target: sold_at
    type: datetime
  - source: order_status    target: cube_order_status
    type: string            # S=销售 / R=退货,留给 schema 维护
  - source: quantity        target: cube_qty
    type: decimal
  - source: unit_price      target: cube_unit_price_yuan
    type: decimal
    unit: yuan
```

### 3.3 mapping-stock.yaml

```yaml
version: 1
model: stock
mappings:
  - source: product_id      target: sku_id
    type: string
  - source: branch_id       target: branch_id
    type: string
  - source: updated_at      target: cube_updated_at
    type: datetime
  - source: quantity        target: cube_qty
    type: decimal
  - source: avg_cost        target: avg_cost_yuan
    type: decimal
    unit: yuan
```

### 3.4 mapping-supplier.yaml

```yaml
version: 1
model: supplier
mappings:
  - source: id              target: supplier_id
    type: string
  - source: name            target: name
    type: string
  - source: type            target: cube_type
    type: string            # 0=供应商 / 1=客户,留给 schema
  - source: contact         target: contact
    type: string
  - source: phone           target: phone
    type: string
  - source: email           target: email
    type: string
  - source: address         target: address
    type: string
  - source: status          target: cube_status
    type: string
  - source: modified_at     target: cube_modified_at
    type: datetime
```

### 3.5 mapping-category.yaml

```yaml
version: 1
model: category
mappings:
  - source: id              target: category_id
    type: string
  - source: name            target: name
    type: string
  - source: parent_id       target: parent_id
    type: string
```

---

## §4 字段映射规则(沿用 cube)

来自 `F:\go\src\github.com\YunBright\cube\skills\write-field-mapping\SKILL.md`,本系统同样遵守:

1. **只做**:字段名重命名、类型转换、单位标注
2. **不做**:enum_map、transform(trim/lower/upper)、跨字段派生
3. **目标字段必须在 §1 本系统 schema 中找得到**——否则视为新增字段,需 PR 同步 §1
4. **同一 model 多 erp 实例的 mapping `target` 必须完全一致**——这样 erp-connector 切换数据源时上层无感
5. **单位字段仅作元信息**——SQL 聚合时手动换算(如 `SUM(weight_kg) / 1000`)

### 变更流程

1. 业务字段新增/修改 → 先 PR 更新 §1
2. cube 字段新增/修改 → 先 PR 更新 §2
3. 映射关系变更 → 先 PR 更新 §3 对应 mapping.yaml
4. §1 §2 §3 必须在同一 PR 内一起改,合入前 review

---

## §5 通用字段(跨 dapr app 共享)

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | string | 单据主键 |
| `branch_id` | string | 分店(多门店隔离维度) |
| `operator_id` | string | 制单人 / 收银员 user_id |
| `auditor_id` | string | 审核人 user_id |
| `created_at` | datetime | |
| `updated_at` | datetime | |
| `remark` | string | |