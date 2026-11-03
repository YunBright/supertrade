# 鲜肉 · 「录入外采」设计与口径（2026-11-03）

> 线1：**鲜肉 → 每日作业 → 录入猪 → 录入外采 → 搬上台**
> owner 原话：「在每日作业录入猪和搬上台间前增加一项录入外采,针对单独部位进行额外采购,
> 如 11 月 1 日采购了 20 斤货号 01401 对应部位肋排/排骨。
> 11-1 日的毛利等都将需要触发重算,进货成本变高了,采样可以等我手动来重采。」

---

## 0. 结论先写

| 问题 | 结论 |
|---|---|
| 外采是什么 | 门店**额外采购某一个部位**的货（**已经切好的**，不是进猪、不是分割） |
| 成本从哪来 | **门店给的单价**（不是猪成本摊出来的）⇒ 单价 0 会让这批货**零成本**、毛利虚高 ⇒ **必须 > 0** |
| 货号怎么定部位 | 后端从 `branch_cut_mappings` **反查** ⇒ 门店只交货号，**不会挑错部位**；没配映射 ⇒ 400，**不猜** |
| 货进哪儿 | **直接上猪肉台**（位置表 `counter` += qty）⇒ 录完就能卖，**不用再搬一次台**（owner 2026-11-03 拍板） |
| 卖掉的怎么算 | **成本进销售成本** ⇒ 当天毛利下降（owner:「进货成本变高了」）；没卖掉的留在期末存货里 |
| 会不会算成假盘亏 | **不会** —— 外采**不进冷库链**（链上没这一项）⇒ 卖不完的随日终盘点进冷库，卖完的进不了冷库 |
| 跟进猪的口径一样吗 | **不一样** —— 进猪不动；外采**单列一栏**（`external_purchase_kg` / `external_purchase_yuan`） |
| 重算谁 | 录/改/撤 ⇒ **当天起**的链重算（当天与往后的毛利、进货成本跟着变） |
| 采样重采吗 | **不自动重采**（owner:「采样可以等我手动来重采」） |

---

## 1. 数据落地长什么样

新表 `external_purchases`：

```
id                = SEP-{branch}-{yyyymmdd}-{sku}-{seq}   ← 确定性代理主键
branch_id         = '00'
biz_date          = 2026-11-01                              ← 归属营业日
cube_product_id   = '01401'                                 ← 货号（门店按货号录）
cut_type          = 'rib'                                   ← 从映射表反查后冗余存一份
qty_kg            = 10                                      ← 单位统一 kg
unit_price_yuan   = 24                                      ← 元/kg
total_amount_yuan = 240                                     ← 服务端算，不可填
note              = '供应商 X 送的'
```

⚠️ **主键要确定性** ⇒ 溯源记账（`core/purge`）按 INSERT 里的主键值删行 ⇒ upsert 时
GORM 保留**旧行**的主键 ⇒ 主键必须是**算出来的**，不是随机的。

⚠️ **部位冗余存一份**：成本链是**部位级**的（`service/cut_cost.go`），而货号↔部位映射
以后可能被改 ⇒ 存一份当时的部位，报表口径稳定。

### 1.1 同时落三处（一个事务）

| 落点 | 落什么 | 为什么 |
|---|---|---|
| `external_purchases` | 事实行 | 事实来源 |
| `sku_stock_positions` | `counter` += qty | 外采的货**直接上猪肉台**（owner 2026-11-03 拍板）⇒ 录完就能卖、**不用再搬一次台** |
| `carryover_records`（结转记录） | `source='external'`、`location='counter'`、归属快照**空**、成本快照 = 门店给的单价 | 进 FIFO 队列 ⇒ 大宗能领用；零售归摊**不落到猪身上** |

⚠️ **位置口径改过一次**（2026-11-03 owner 拍板）：
原先实现是「**先进冷库**（`cold_store`）+ 再搬一次台」⇒ 门店漏搬 ⇒ 冷库链虚高 ⇒ **假盘亏**
⇒ 外采是**可选动作**，不该埋「漏一步就亏钱」的坑 ⇒ 改成「直接上猪肉台」。

---

## 2. 怎么进成本链

移动加权平均（见 `service/moving_average.go` 的包注释）：

```
可售数量 = 期初 + 本期入库
可售金额 = 期初金额 + 本期入库金额
本期销售成本 = 可售金额 − 期末存货金额 − 统收成本 − 报损成本
```

⇒ 外采就是「本期入库」的**另一个来源**（与猪的分割产出并列）：

```
本期入库量   += 外采 kg(按部位)
本期入库金额 += 外采金额(kg × 门店给的采购单价)
```

⇒ **卖掉的那些外采** ⇒ 成本进销售成本 ⇒ **毛利下降**；
　**还没卖掉的** ⇒ 留在期末存货里 ⇒ 不影响当天毛利。

### 2.1 ⚠️ 注入点必须在 `applySplitWeights` 之后

`service/cut_cost.go` 的 **8c-2**：

```go
prodKg, prodAmount := applySplitWeights(split, partProducedKg, partProducedAmount)
// ← 8c-2 外采在这里叠加
for c, v := range extProducedKg   { prodKg[c] = prodKg[c].Add(v) }
for c, v := range extProducedAmount { prodAmount[c] = prodAmount[c].Add(v) }
```

⚠️ **为什么不能放前面**：`applySplitWeights` 把入库量整体**替换成分割权重 W**、
并把入库金额按 W 的**比例重分** ⇒ 外采放进去会被 W 顶掉（kg 丢了）、
金额会被摊到别的部位。

⚠️ `cutList` 也要加外采的部位 —— 那天只外采、没进猪也没卖时，它是当天**唯一**有货的部位。

### 2.2 ⚠️ 补充口径：外采按「当天进、**当天出**」（2026-11-04）

owner 2026-11-04 报：11-02 外采了 11.4kg 排骨并**全部卖掉**，
看板却提示「肋排/排骨 账面比实盘多 **11.40 kg**」（点「确认搭赠」= 白扣 **241 元**毛利）。

**根因**：外采进了「本期入库」（`availKg`），但**思迅日汇总里没有对应的卖出**
⇒ 账面应有永远等不到那个出口 ⇒ 缺口**恰好等于当天外采量**
（10-30 12.25 / 10-31 11.0 / 11-01 9.7 / 11-02 11.4，四天全中）。

**修法**（`service/cut_cost.go`）：外采**自己带一个出口** ——

```go
externalOutKg := extProducedKg[c]
expectedCloseKg := availKg.
    Sub(partSoldKg[c]).Sub(partBulkKg[c]).Sub(partWasteKg[c]).Sub(partGiftKg[c]).
    Sub(externalOutKg)     // ← 当天进、当天出(pass-through)
```

⇒ 账面应有里**不留**它 ⇒ 缺口归零 ⇒ 不再误报搭赠。

⚠️ **不动的部分**：**成本照样进销售成本**（owner：「进货成本变高了」）、收入/毛利口径不动。
- 有盘点单的日子：期末量 = 实盘 ⇒ 销售成本**逐位不变**；
- 没盘点单的日子：期末量 = 账面推演 ⇒ 外采不再留在存货里 ⇒ 成本**全额**进当天销售成本 ⇒ 毛利**下降**（更保守）。

⚠️ **推不出来时的代价**：当天有外采的部位**当天不再报搭赠**（漏报优于误报 ——
误报会让门店白扣毛利）。不推断「外采已售（POS 未登记）」，也不新建交互让门店选「卖掉了」
（系统无法区分「外采已售」与「搭赠/盘亏」）。

详见 [FRESH-MEAT-COUNTER-DERIVE-2026-11-04.md](./FRESH-MEAT-COUNTER-DERIVE-2026-11-04.md)。

---

## 3. 冷库链：**外采不进链**

⚠️ 链不记分割产出（`service/daily_balance.go` 的 `giftOut` 注释：链永远 ≥0）
⇒ **外采也不进链**（owner 2026-11-03 拍板）：外采录完**直接上猪肉台**、不占冷库。

```
链：期初 + 大宗进 − 搬上台 − 大宗出库 + 盘点结转 − 搭赠 = 期末(冷库)
```

⇒ 外采的卖剩部分怎么进冷库？**靠盘点**：盘点盘的是**猪肉台**（含外采没卖完的那部分）
⇒ 盘点结转项把它带回冷库 ⇒ 盘点夜 `applyOvernightMove` 再把位置搬过去
⇒ 部位卖完（盘点 0）⇒ 进不了冷库 ✔ 与 owner 的描述逐字吻合：

> 「跟录猪一样,卖不完会在日终盘点体现,随日终盘点一起进冷库,
>   部位卖完,日终盘点为 0,就进不到冷库了。」

⚠️ 与「进猪」唯一的差别：进猪的分割产出**不写位置表**；外采**要**写（记在猪肉台）
⇒ 门店在「库存」页看得见这批货。

---

## 4. FIFO 归摊：外采**不落到猪身上**

外采没有产出猪 ⇒ 它的收入不能摊给某头猪（那会让单猪毛利失真）
⇒ 外采自己登记一行结转记录（`source='external'`，归属快照为空）
⇒ FIFO 领用它时收入记「**未归属**」（与期初结转同一处理，见 `service/pig_fifo.go`）。

⚠️ **同时：日终盘点的结转记录要扣掉外采的那部分**
（盘点盘的是**猪肉台**上的货，其中可能就有外采没卖完的）
⇒ 不扣 ⇒ 同一批货在 FIFO 队列里出现两次 ⇒ **收入被摊两遍**。

⚠️ **FIFO 消费顺序**：外采的行**先于**盘点结转的行被领用
⇒ 否则同一天的两行按 id 排序 ⇒ 外采的收入可能摊到猪身上。

---

## 5. 看板口径

owner 拍板：「进猪口径不动,新增独立『外采进货』字段、界面合计显示」

`GET /gross-margin` 的 `total` 多两个字段：

```json
{
  "total": {
    "purchased_kg": "227.1",           ← 进猪（一个字没动）
    "purchased_yuan": "3315.66",
    "external_purchase_kg": "10",      ← 新增：外采进货
    "external_purchase_yuan": "240"    ← 新增：外采进货金额
  }
}
```

⚠️ **不并进** `purchased_kg` / `purchased_yuan`
⇒ 合并了门店就看不出"今天额外买了某部位的货、花了多少钱"。

### 5.1 ⚠️ 口径提醒：参考毛利（B 口径）里**怎么**算外采的成本

owner 说「进货成本变高了」⇒ 这句话在界面上**由「外采进货」栏兑现**（¥240 摆在那里）。

⚠️ 但要讲清楚三件事，免得店长以为页面算错了：

| 口径 | 外采的成本算进去了吗 | 页面上看得到吗 |
|---|---|---|
| **账面口径**（移动加权链） | **算** —— 卖掉的那些外采，成本进销售成本 ⇒ 毛利下降 | ❌ 账面口径已于 2026-11-01 退休，页面不呈现 |
| **参考口径**（B：各部位参考毛利之和） | **算** —— 部位成本单价**按量加权**（见 §11）⇒ 外采的成本进了当天的参考毛利 | ✅ 这就是页面上那个「毛利 / 毛利率」；来源标记变成 `baseline_external` / `purchase_avg_external` |
| **进货合计**（Σ 本期入库量） | **算** —— 外采进了本期入库量 ⇒ 「进货」合计含外采 | ✅ 「进货」格 + 「外采进货」格（其中外采多少） |
| **长期毛利页**（全店口径） | **算** —— 「期间采购 / 采购均价」**含外采**（见 §11）⇒ 期间毛利也扣外采成本 | ✅ 「其中外采」单列（kg / 元） |

⇒ 录完一笔外采后，页面上**会变的是**：
「进货」合计变大、「外采进货」栏出现、**参考毛利率会动**（外采的成本按量摊进了部位成本单价）、
长期毛利的「期间采购 / 采购均价」也含外采。

⚠️ 「进货」合计与「外采进货」两栏**永远分开**（不并进 `purchased_kg`/`purchased_yuan`）
⇒ 门店始终看得见「其中外采多少」。

---

## 6. 端点

| 方法 | 路径 | scope | 说明 |
|---|---|---|---|
| GET | `/external-purchases?date=YYYY-MM-DD` | `freshmeat:view` | 某营业日的外采清单（+ 合计） |
| POST | `/external-purchases` | `freshmeat:write` | 录一笔 |
| PUT | `/external-purchases/:id` | `freshmeat:write` | 改一笔（重量/单价/备注） |
| DELETE | `/external-purchases/:id` | `freshmeat:write` | 撤一笔（位置余额与成本一起回退） |

⚠️ 单位统一 **kg / 元每kg**（前端按门店习惯可按「斤」录，**换算在前端做**）。

⚠️ **营业日标志不加**：外采是**可选**动作（多数日子没有）⇒ 不改 `hasBizDayActions`、
不进状态阶梯 ⇒ 前端靠外采清单判定。

---

## 7. 校验（录不进去的四种）

| 情形 | 结果 | 为什么 |
|---|---|---|
| `qty_kg <= 0` | 400 | 没有重量 |
| `unit_price_yuan <= 0` | 400 | **零成本 ⇒ 毛利虚高** |
| 货号没配部位映射 | 400 | **猜错部位 ⇒ 成本摊到错的部位上** |
| 归属日在链起点之前 / 前一营业日没盘点 | 400 | 与录猪、搬台同一套纪律（见 `business_day.go`） |

---

## 8. 重算

录 / 改 / 撤 ⇒ 事务外调 `recomputeCostSnapshots`（那天起的结余快照 + 整条链物化）
⇒ 当天与往后的毛利、进货成本跟着变。

⚠️ **失败不阻断录入**：外采是「事实录入」，成功就成功；
派生数据没跟上只影响「看数字」，下次任何写动作都会自愈（与录猪同一套纪律）。

⚠️ 毛利看板是**实时算**的，但链锚点读 `cut_cost_daily` 快照
⇒ **必须重物化**，否则往后日期走旧锚点。

---

## 9. 前端（每日作业第 ② 步）

| 位置 | 做什么 |
|---|---|
| `lib/domain/fresh_meat/fresh_meat_external_purchase.dart` | 模型 + `PurchaseUnit`（斤/kg 换算） |
| `lib/application/fresh_meat/fresh_meat_external_purchase_controller.dart` | 清单 + 录/改/撤 |
| `lib/ui/fresh_meat/daily/daily_step_external_purchase*.dart` | 录入页（表单 / 清单 / 写操作 / 弹层 / 单位换算） |
| `lib/ui/fresh_meat/widgets/margin_stock_rows.dart` | 看板的「外采进货」格 |

⚠️ **步骤栏里「录入外采」是可选动作** ⇒ 没有外采显示「**无**」（不是「未做」）
⇒ 否则门店会以为漏做了。

⚠️ **切单位时已填的数要跟着换算** —— 否则实际重量会差一倍（10kg 显示成 10 斤 = 5kg）。

⚠️ **窄屏（360px）的 chip 行放不下 4 个步骤** ⇒ 「④ 日终盘点」在横向滚动区之外
⇒ 测试要 `ensureVisible` 之后再点。

---

## 10. 2026-11-03 第二次拍板：外采怎么进毛利口径 + 结转记录表改名

owner 一次拍板四件事，本节记**最终口径**（上一版 §5.1 说的「参考口径不含外采」已作废）。

### 10.1 长期毛利的「期间采购」**含外采**

owner 原话：「1. 要含」

```
期间采购量   = 进猪 kg + 外采 kg
期间采购金额 = 进猪金额 + 外采金额
期间采购均价 = 总金额 ÷ 总重量        ← 即 owner 说的「求均值」
```

⇒ 外采的货也是真金白银进的货 ⇒ 不含它 ⇒ 毛利虚高。
⇒ 外采的**明细单列**（`external_kg` / `external_yuan`）⇒ 门店看得见「其中外采多少」。

⚠️ 每日毛利看板的 `total.purchased_*` / `purchase_avg_yuan` **仍保持进猪口径不动**
（owner 上轮拍板）⇒ 外采单列 `external_purchase_*`。**两套口径永远分开。**

### 10.2 参考毛利：部位成本单价**按量加权**（不是直接换成外采价）

owner 原话：「不是直接换，而是**求均值**」⇒ 落成：

```
unit_c = (base_c × pigKg_c + extPrice_c × extKg_c) ÷ (pigKg_c + extKg_c)

base_c     = 建议成本单价（基线折算）；取不到 ⇒ 用**进猪**采购均价
             （⚠️ 进猪口径、不含外采 ⇒ 免得外采被算两遍）
pigKg_c    = 该部位今天的**猪分割产出**（实测 A_c，没有则退建议 S_c）
             ⚠️ **不能用 AdjustedSplitKg(= prodK)** —— prodK 已叠加外采 ⇒ 会算两遍
extPrice_c = 该部位当天外采的加权单价；extKg_c = 外采量
```

⇒ 来源标记新增两个：`baseline_external` / `purchase_avg_external`
（前端 `GrossMarginReferenceCostPriceSource` 同步加了对应文案）。

### 10.3 单价基线采样：两条路都做（owner 拍板「两者都做」）

1. **倒推用的毛利率 m** 改用**含外采**的采购金额与均价（`referenceMarginRate`）；
2. **外采价作为一个样本进基线**：
   - 样本值 = 倒推单价 与 外采价**按量加权**（不是只用倒推价，也不是只用外采价）；
   - 样本新增列 `external_kg`（`pig_cut_price_samples`）；
   - 聚合权重 = **销量 + 外采量**（`aggregatePriceBaselines`）；
   - 「无零售销量但有外采」的部位**也产样本**（值 = 外采价）⇒ 补上了「结构性盲区」。

⚠️ 历史样本 `external_kg = 0` ⇒ 权重与从前逐字一致 ⇒ **历史基线数字不变**。

### 10.4 结转记录表改名 + 彻底去掉旧的批次标记

owner 原话：「这个表包含了没有袋标签的普通搬上台数据？如果是必要的，**要改名，要彻底去掉搬袋标签**」

**实测**：生产 `stock_bags` 共 **194 行** —— source='stocktake' 186 行（其中 location=cold_store 183 / counter 3）
+ source='opening' 8 行 ⇒ **绝大部分是「盘点结转」＝普通搬上台的数据、根本没有批次标记**；
`label` 非空仅 **6 行**且全是契约测试门店（TST-FM-*）造的数据 ⇒ **真实门店零批次标记**。

⇒ 改名 `stock_bags` → **`carryover_records`**（数据原样保留）；
⇒ `label` 列与它的两个索引**彻底删掉**；
⇒ `bulk_settlement_allocations.bag_id` → **`carryover_id`**（数据原样保留）；
⇒ Go 标识符去掉 Bag：`StockBag`→`CarryoverRecord`、`BagSource`→`CarryoverSource`、
   `ListStockBags`→`ListCarryoverRecords`、`/stock/bags`→**`/stock/carryovers`**；
⇒ 前端：`CounterLoadSource` 只剩 `sku`（旧来源显示中性文案「搬台」）、删 `isBag`。

⚠️ **数据删除告知**（owner 要求）：删 `label` 列会连带删掉那 **6 行** label 值
—— **全部是契约测试门店造的测试标签**，真实门店**零业务损失**。其余改动（RENAME 表 / RENAME 列）**全部保留数据**。

⚠️ **迁移顺序**：`ALTER TABLE ... RENAME` 必须在 **AutoMigrate 之前**（`ApplyPreMigrateFixups` ⑥）
⇒ 否则 AutoMigrate 会新建一张空表、194 行老台账留在旧表里 ⇒ FIFO 队列与大宗分摊凭据断链。

⚠️ **PG 不会跟着改索引名**（RENAME TABLE / RENAME COLUMN 都不会）⇒ 迁移里额外
`ALTER INDEX ... RENAME TO`（`idx_stock_bags_*` → `idx_carryover_records_*`）并删掉旧的
`idx_bulk_settlement_allocations_bag_id` ⇒ 否则 AutoMigrate 会再建一份 ⇒ **重复索引**。
主键约束也一并改名（`stock_bags_pkey` → `carryover_records_pkey`）⇒ 库里不留旧表名。

✅ **验证方式**：把生产的 `stock_bags` + `bulk_settlement_allocations`
（结构 + 194 行数据）**原样导出**到本地临时 PG，跑**真实的迁移代码**
（`ApplyPreMigrateFixups` → `AutoMigrate` → `ApplySchemaFixups`）⇒
194 行逐行保留、`label` 列消失、`bag_id`→`carryover_id` 数据保留、索引无重复；
**整套再跑一遍结果一致**（幂等）。

---

## 11. 相关提交

| 仓 | 提交 | 内容 |
|---|---|---|
| supertrade | `17f4ad5` | 外采模型 + 端点 + 成本链接入 + 看板字段 + Go 单测 |
| supertrade | `89644f9` | **位置口径改为「直接上猪肉台」**（删冷库链 externalIn + 单测改口径） |
| deployer | `2390f9e` | 端到端契约（打生产） |
| deployer | `48f5253` | 契约加**位置断言**（录完 ⇒ 猪肉台 10kg / 冷库 0kg；撤 ⇒ 回退） |
| tradewind_app | `e17f707` | 每日作业第 ② 步 + 看板外采栏 + 前端单测 |
| tradewind_app | `70e05d6` | 外采文案改成「直接上猪肉台、录完不用再搬台」 |
| **supertrade** | **`5bcfc5f`** | **第二次拍板四条**：长期毛利含外采 + 参考毛利按量加权 + 采样含外采 + `stock_bags`→`carryover_records`（含迁移与新单测） |
| **deployer** | **`b43a213`** | 契约端点改名（`/stock/bags`→`/stock/carryovers`）+ 去掉「袋」措辞 + label 断言改看字段名 |
| **tradewind_app** | **`e894401`** | 长期毛利外采字段 + 参考成本单价两个新来源文案 + 彻底去掉搬袋标签措辞 |

⚠️ `89644f9` 已部署生产（fresh-meat）⇒ 生产库里外采相关行数为 **0**
⇒ 口径变更对**存量数据是 no-op**（旧实现的 `externalIn` 在没有外采单时也是空 map
⇒ 链值逐位相同；位置表一行没动）。

### 11.1 `5bcfc5f` 部署生产后的复核（2026-11-03）

- **迁移实况**（生产库只读复核）：`stock_bags` 已不存在 ⇒ `carryover_records` **194 行逐行保留**；
  `label` 列已删；`bulk_settlement_allocations.bag_id`→`carryover_id`（**6 行非空数据保留**）；
  索引与主键约束全部改名、**无重复索引**；`pig_cut_price_samples.external_kg` 已建。
- **存量数字逐位复核**（部署前后快照对比，10-29 / 10-30 / 10-31 / 11-01 四天）：
  每日看板 `total`、**部位行（含参考成本单价 / 来源 / 参考毛利 / 参考毛利率）**、长期毛利的
  所有既有数字 **逐位相同**；唯一差异是长期毛利每日行**新增**的 `external_kg` / `external_yuan`
  两个字段（值都是 `0` ⇒ 生产暂无外采）⇒ **口径变更对存量数据是 no-op**。
- 生产库 `external_purchases` **0 行**、`carryover_records` 里 `source='external'` **0 行**、
  `pig_cut_price_samples` 里 `external_kg > 0` **0 行** ⇒ 与上面互相印证。
