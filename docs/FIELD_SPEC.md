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

### 1.1 catalog(商品目录代理,dapr app `catalog`)

**本期定位**:不维护本地商品表,**只代理 cube-gateway `/v1/load` 返回 product 维度数据**。

`GET /products` 请求/响应字段(`data[]` 各元素)来自 cube `product` model:

| 字段 | 类型 | 单位 | 来源 |
|---|---|---|---|
| `id` | string | - | cube `product.id` |
| `name` | string | - | cube `product.name` |
| `category_id` | string | - | cube `product.category_id` |
| `supplier_id` | string | - | cube `product.supplier_id` |
| `status` | string | - | cube `product.status` |
| `created_at` | datetime | - | cube `product.created_at` |
| `barcode` | string | - | **本期 = item_no**(cube sixun-models/product 无 barcode 字段;对齐 collect-ai 业务字段) |

> cube `product` 不含 `barcode / spec / unit / origin / brand / shelf_life_days` 等扩展字段;
> 这些字段**只在本系统展示时按需从思迅数据库单独查**(本期不实现,
> 留 entry point 在 catalog 服务后续扩展)。

### 1.2 inventory(实时库存代理,dapr app `inventory`)

**本期定位**:不维护本地库存表,**只代理 cube-gateway `/v1/load` 返回 stock 维度数据**。

`GET /stock` 请求/响应字段来自 cube `stock` model:

| 字段 | 类型 | 单位 | 来源 |
|---|---|---|---|
| `product_id` | string | - | cube `stock.product_id` |
| `branch_id` | string | - | cube `stock.branch_id` |
| `quantity` | decimal | - | cube `stock.total_quantity` |
| `avg_cost_yuan` | decimal | yuan | cube `stock.avg_cost` |
| `updated_at` | datetime | - | cube `stock.updated_at` |

> 注:思迅源已含完整批次 / 保质期 / 出入库流水信息,本系统只取汇总的
> `(product, branch) → quantity` 维度,本系统**不建 `inventory.batches` 表**。

### 1.3 pos(销售单据,dapr app `pos`)

`pos.sales_order` / `pos.sale_lines` / `pos.payments`,字段从原 §5 销售明细 + §1 采购入库头部结构移植,字段名沿用 §0 命名约定。

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

### 1.8 master-data / pricing / sales-agg / erp-connector / llm-gw

字段按各自职责定义,沿用 §0 命名约定,清单在各自 dapr app 落地时再补。
**FIELD_SPEC.md 不重复列举这些——只覆盖跟 cube 有映射的"商品/库存/销售/供应商"4 个核心实体 + 本期重点的 stocktake**。

| 服务 | 本期关键表 / 字段 |
|---|---|
| `master-data` | `stores` / `employees`(本地维护);供应商/客户走 cube `supplier` 转发,不维护本地表 |
| `pricing` | `price_lists` / `promotions`(本地维护,本系统自营 POS 用) |
| `sales-agg` | `sales_view_minute` / `sales_view_daily`(本地宽表,聚合 POS + erp-connector) |
| `erp-connector` | `erp_sales_raw` / `sync_logs`(拉 cube 数据落库,供 sales-agg) |
| `llm-gw` | `llm_call_logs`(LLM 调用留痕) |

---

## §2 cube sixun 族统一 schema(来源侧)

来源:`F:\go\src\github.com\YunBright\cube\sixun-models\<entity>\schema.yaml`。
FIELD_SPEC §3 字段映射只引用这些 cube dimension/measure。

| cube model | 关键 dimension | 关键 measure |
|---|---|---|
| `product` | id, name, category_id, supplier_id, status, created_at | count, avg_price_yuan, total_cost_yuan |
| `sale_detail` | id, flow_no, branch_id, product_id, sold_at, order_status(S/R) | count, total_qty, total_amount_yuan, net_amount_yuan |
| `stock` | product_id, branch_id, updated_at | total_quantity, total_value_yuan, low_stock_count |
| `supplier` | id, name, type(0=供应商/1=客户), contact, phone, email, address, status | count, total_credit_yuan |
| `category` | id, name, parent_id | count |

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