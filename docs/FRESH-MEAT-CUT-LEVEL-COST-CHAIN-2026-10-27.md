# 鲜肉毛利：成本链改「部位级」——解决方案与改造清单

> 日期：2026-10-27
> 触发：门店反馈 2026-10-02 店 00 猪蹄行「进货 4.7 / 销售 16.9 / 应有 +5.0」自相矛盾。
> 状态：**待实施**（本文件是施工图 + 口径决议记录）。

---

## 1. 结论先说

**问题不是算错，是两套粒度在打架：**

| 口径 | 现状 | 后果 |
|---|---|---|
| 库存 / 结余 / 盘点 / 期初结转 / 大宗 | **已经是部位级**（`cut_stock_daily` / `sku_stock_openings` / `loadDailyTerms`） | —— |
| 成本链（移动加权 / 对账 / 审计冻结） | **SKU 级**（`sku_cost_daily` / `sku_cost_states` / `computeSkuDailyCosts`） | 部位级的量被摊到 SKU 主键上 ⇒ 另一侧变零存量 |

两个具体的坏点：

1. `overlayStockOpenings`（`daily_balance.go`）把**部位**的期初结转 **100% 记到该部位的「主 SKU」**
   （`cube_product_id` 最小那条），并把同部位**其它 SKU 的期初清零**
   ⇒ 多货号部位（猪蹄 = 猪脚 01411 / 猪手 01412）里，**猪手账面库存恒为 0**。
2. `expectedCloseKg` 负数裁 0 ⇒ 账面没货却卖出的货，「应有」被**抹掉**；`movingAvg = 0`
   ⇒ **销售成本 0 ⇒ 毛利率 100%**。

**2026-10-02 店 00 实测后果**：
5 个 SKU 账面 0 却卖出（营收 431.2 元按 100% 毛利入账 = 全店营收 12.4%）；
12 个部位账面短卖 41.4 kg（≈733 元成本没入账）；30 个部位里 14 个「应有」与恒等式对不上。

**上游**：店 00 的成本链基线只有一条 —— `sku_stock_openings` 仅「猪蹄 9.23kg @ 8元/kg，生效 2026-10-01」；
店 00 从 9 月至今**没有任何盘点单** ⇒ 其余 29 个部位期初为 0。

---

## 2. 目标模型（一句话）

> **库存 / 结余 / 成本链 = 部位（cut_type）；SKU 只承载「卖了多少、卖了多少钱」。**

这与代码里门店早就写下的口径一致（`model.go` / `cut_stock_daily` / `sku_stock_openings` 的注释）：

> 「鲜肉只管部位数据，不管 SKU；SKU 映射只是为了把销售、数量等数据聚合到其部位……
>   日终盘点也是记录部位。库存，大宗结算都是部位。」

### 部位级引擎的计算式

```
期初(部位)   = 上一营业日的部位级成本快照结余
              → 回退: closingForDay(D−1) 部位冷库结余 × 部位移动加权单价
              → 回退: sku_stock_openings 期初结转（**按部位直接用，不再落 SKU**）
入库(部位)   = 今日分割产出 kg（Σ 各 SKU produced）+ 猪成本按部位分摊额
可售金额     = 期初金额 + 入库金额
可售数量     = 期初 kg + 入库 kg
移动加权单价 = 可售金额 / 可售数量
期末(部位)   = 有盘点 → closingForDay(D)（柜台 + 冷库，部位级）
              无盘点 → 期初 + 入库 − 销售 − 统收 − 报损   ← **不裁 0**
期末金额     = 期末 kg × 移动加权单价
销售成本     = 可售金额 − 期末金额 − 统收成本 − 报损成本  ← **不裁 0**
毛利         = 收入 − 销售成本 − 统收成本 − 报损成本 + 预期毛利
```

⚠️ **两处「不裁 0」是本次改造的核心**：
- 期末为负 ⇒ 该部位**账面短卖**（卖超了库存）⇒ 成本照摊 ⇒ 毛利不再虚高；
- 销售成本为负 ⇒ 不可能发生（期末 ≤ 可售金额时），若发生说明数据有问题 ⇒ 记日志 + 归零。

### SKU 级只剩什么

| 字段 | 部位级引擎给 | SKU 级怎么来 |
|---|---|---|
| 销售 kg / 营收 | ✅ 部位汇总 | **按 SKU 原样**（cube 销售本来就是 SKU 级） |
| 统收 / 报损 kg + 金额 | ✅ 部位汇总 | 部位汇总（不再按 SKU 拆） |
| 入库（分割产出）kg | ✅ 部位汇总 | **按 SKU 原样**（猪分割本来就落到货号） |
| 销售成本 | ✅ 部位 | **按部位移动加权单价 × 该 SKU 销售 kg** 分摊（供下钻） |
| 期初 / 期末 kg | ✅ 部位 | **不再返 SKU 值**（SKU 粒度不存在） |

---

## 3. 持久化：新增两张部位级表

| 表 | 作用 | 唯一键 |
|---|---|---|
| `cut_cost_daily` | **部位级成本日快照** —— 链的载体（承接 `sku_cost_daily` 的职责） | (branch_id, biz_date, cut_type) |
| `cut_cost_states` | 部位级「当前成本状态」（qty / amount / moving_avg） | (branch_id, cut_type)，代理主键 `surrogateID("CCS", …)` |

`sku_cost_daily` / `sku_cost_states`：**停止写入，历史保留只读**（不删、不迁移）。

> ⚠️ 为什么新建表而不是在 `sku_cost_daily` 里塞 `cut:belly` 这类伪 SKU：
> 本仓约定「一张表一个粒度」（`cut_stock_daily` 部位 / `sku_cost_daily` SKU）。
> 混粒度会让后来读表的人踩同一个坑 —— 这次的坑就是这么来的。

---

## 4. 改造清单（"不符合"的地方逐条处置）

| # | 现状 | 处置 |
|---|---|---|
| 1 | `overlayStockOpenings` 把部位期初落「主 SKU」 | **删掉落 SKU 逻辑**；期初直接按部位用 |
| 2 | `splitCutClosing` 按冷库余额占比把部位结余拆 SKU | **删掉**（结余不再需要 SKU 版） |
| 3 | `loadSkuOpening`（SKU 期初三段回退） | 换成 `loadCutOpening`（部位级三段回退） |
| 4 | `computeSkuDailyCosts` | 拆成 `computeCutDailyCosts`（部位引擎）+ SKU 层只出销售/营收/分摊成本 |
| 5 | `persistSkuCostDaily` | 改写 `cut_cost_daily` + `cut_cost_states` |
| 6 | `recomputeCostSnapshots` | 改重算部位级快照 |
| 7 | `bulkUnitCost` / `bulkUnitCostMap`（查 SKU 成本状态） | 改按**部位**查 `cut_cost_states`（大宗本来就按部位记 ✓） |
| 8 | `biz_audit.go` 清零逻辑（按 SKU 查 `sku_cost_daily`） | 改按部位查 `cut_cost_daily` |
| 9 | `gross_margin.go` 的 `rollUpPerCut`（Σ SKU → 部位） | **不再汇总**；`per_cut` 直接就是部位引擎的结果 |
| 10 | `loadPresaleUnits`（SKU 预期售价） | 部位重估：部位预期售价 = 各 SKU 预期售价**按当日营收占比**加权 |
| 11 | 盘点页「应盘哪些 SKU」 | 改成**部位**：部位有期初或产出 ⇒ 该部位下**所有** SKU 都要盘（避免分摊值把某个货号漏掉） |
| 12 | 长期毛利 `/gross-margin/period?group_by=sku` | SKU 粒度 = 销售/营收按 SKU + 成本按部位单价分摊；文档写明 |
| 13 | `per_sku` 接口字段 | `opening_kg` / `closing_kg` **不再返**（键消失）⇒ 前端显示「—」；新增 `cost_yuan`（分摊成本） |
| 14 | Flutter `SkuGrossMargin` | `openingKg`/`closingKg` 改 nullable；`countableSkuIds` 改用 `per_cut` |
| 15 | Flutter 盘点页 | 「应有」按部位显示（部位为主 + SKU 子项），与毛利表同一套粒度 |
| 16 | `handler/purge.go` / `migrate.go` | 登记新表 + 代理主键 DDL |

---

## 5. 口径决议（本次已定）

| 决议 | 内容 | 依据 |
|---|---|---|
| **R1 历史重算** | 真实门店 00/01/10 **无审计冻结、无盘点单、无预售** ⇒ 历史可全部按新口径重算 | 生产实测：`biz_audits` / `cut_stock_daily` / `pork_cuts_stocktakes` 只有 `TST-*` 契约测试门店的行 |
| **R2 盘点录入粒度** | 盘点**仍按 SKU 录**（仓管按货号称重），服务端**汇总到部位**喂给成本链；盘点页的「应有」改**部位级** | 盘点是物理动作，按货号称重是事实；改部位级会改变门店操作 |
| **R3 应盘集合** | 部位有期初或产出 ⇒ 该部位下**所有** SKU 都要盘 | 用分摊值判定会漏盘（没分到期初的货号会被跳过） |
| **R4 预售重估** | 部位预期售价 = 各 SKU 预期售价**按当日营收占比**加权 | 营收占比是 SKU 粒度里最可靠的量（销售是实打实的） |
| **R5 `per_sku` 语义** | 只保留销售 / 营收 / 入库 / 分摊成本；期初、期末**不返** | SKU 粒度的库存是**假的**，返回就是继续制造同一个坑 |
| **R6 毛利会变** | 改造后毛利**会下降**（成本开始入账）⇒ 这是**变准**，上线时必须同步给门店 | 2026-10-02 店 00 约 733 元成本此前没入账 |

---

## 6. 实施顺序（每步都可独立验证、独立提交）

| 阶段 | 内容 | 验证 |
|---|---|---|
| **P1 引擎** | 新表 + `computeCutDailyCosts` + `loadCutOpening`；`per_cut`/`total`/`reconciliation` 改用部位引擎；`per_sku` 改销售/营收口径 | 单测：多货号部位恒等式闭合、短卖不裁剪、零成本销售归零 |
| **P2 周边** | 快照落库 / 重算 / 大宗单价 / 审计清零 / 盘点应盘集合 / 长期毛利 sku 粒度 | 契约测试：新增「卖超却成本为 0」守卫 + `reconciliation.diff == 0` |
| **P3 清理 + 前端** | 删 `overlayStockOpenings`/`splitCutClosing`/`loadSkuOpening`；Flutter 模型与盘点页 | 全量回归 + 生产只读对比 |

⚠️ **上线节奏**：P1 改完毛利的数字就会变（下降）⇒ 建议 **P1+P2 一起上线**，避免中间态
（部位口径的 per_cut + 旧口径的 per_sku）对不上。

---

## 7. 验证方法（每阶段必做）

1. **服务层单测**（内存 SQLite，不碰生产）：多货号部位的恒等式 `期初+入库−销售−统收−报损=应有`
   逐部位闭合；短卖部位应有为**负**；任何 SKU 都不得出现「卖超却成本 0」。
2. **契约测试**（deployer，一次性测试门店）：新增守卫
   - 任一 `per_cut` 行：`expected_remain_kg` 必须存在（`fresh_meat_actual_remain_test.go` 已有）；
   - 任一 `per_sku` 行：销量 > 0 ⇒ 成本不得恒为 0（除非该部位本期入库为 0 且**期初为 0** ⇒ 那要报警）；
   - `reconciliation.diff_yuan == 0`。
3. **生产只读核对**：用同一份生产 JSON 对比改前 / 改后 `per_cut`；
   **只读、不在真实门店录猪**（审计不可逆）。
4. **门店侧止血**（与改造并行）：在「鲜肉-设置」补**期初结转**（按部位录冷库余额 + 单价，
   生效日与现有猪蹄那行一致 = 2026-10-01）⇒ 部位级链的起点就完整了。
   ⚠️ 改造前补录**只能**改善部位层的应有（因为 SKU 级仍会摊主键）；**改造后补录才真正生效**。

---

## 8. 风险清单

| 风险 | 处置 |
|---|---|
| 毛利数字下降，门店以为"亏了" | R6：上线时给门店一句话说明（"以前虚高，现在是真的"）+ 报表保留「账面短卖」提示 |
| 预售金额变化 | 生产无预售数据 ⇒ 风险≈0；R4 口径写进代码注释 |
| 审计冻结与新口径并存 | R1：当前生产无冻结；代码里保留「审计日之前读冻结快照」的分支不变 |
| 大宗成本单价改按部位 | 大宗本就按部位记（`bulk_settlement_allocations.cut_type`）⇒ 口径更一致 |
| 盘点页 SKU 清单变化 | 门店第一次盘会看到不同清单 ⇒ 上线前告知 |

---

## 9. 链式接续改造（2026-10-27 第二轮）

> 起因：用户判断「长期毛利不能依赖快照表，日终盘点就快照的方式不可取」。
> 物化点决议：**只靠审计结转**（不新增定时任务）。

### 9.1 根因（生产实测，店 00）

`cut_cost_daily`（部位级成本日快照）**只在有盘点单的日子落** ⇒ 零盘点门店的链**每天断一次**
⇒ 每天的期初都回落成「门店期初结转」那点残值 ⇒ 成本塌陷、毛利虚高：

| 营业日 | 销售成本 | 毛利率 | 现象 |
|---|---|---|---|
| 09-01 ~ 09-30 | 恒 0 | 100% | 期间毛利看起来 = 期间营收 |
| 10-01 | 120.41 | 96.84% | 只剩期初结转那 9.23kg 的残值 |
| 10-02 | 3072.17 | 11.73% | 碰巧当天有整猪入库（3169.66） |
| 10-03 ~ 10-27 | ~100 | 97% | 链又断回期初残值 |

⇒ 这不是「延迟」，是**数字系统性失真**。

### 9.2 改法

1. **链逐日接续**（`service/cut_chain.go` 新增）：今天的期初 = 昨天的期末（**算出来的，不必落库**）。
   - 链**按部位独立**接续；没入链的部位（既无期初结转也无进货）**不接续**
     ⇒ 无基线期间卖出的货不会变成负库存污染后面的日子。
   - 起点 = 最早有基线的营业日（期初结转 / 已落库快照）；基线之前 = 「成本算不出来」。
   - 锚点 = from 之前最近一个已落库快照 ⇒ 命中时 O(1) 快路径。
2. **链带三根支柱**：kg、金额、**移动加权单价**。
   ⚠️ 单价是必需的：短卖时「可售数量 ≤ 0」⇒ 加权单价算不出来 ⇒ 没有它 ⇒ 成本塌成 0 ⇒ 毛利率 100%（第二轮生产实测抓到）。
3. **物化不绑盘点**（`materializeCostChain`）：逐日重算 `fromDate → 今天` 并落 `cut_cost_daily`。
   - 写路径（录猪 / 删猪 / 大宗 / 搬上台 / 期初结转）触发：**后台跑 + 不继承请求 ctx** + 同门店合并（pending）。
   - **审计结转**（物化点）本来就逐日物化整条链：审计前置条件 = 区间内每天都有完整日终盘点 ⇒ 每天都会落 ⇒ 审计过的区间秒回。
4. **「成本算不出来」 ≠ 「成本为 0」**：
   - 引擎：`CutDailyCost.CostUncomputable`。
   - 期间毛利：行级 `cost_incomplete` + 报告级 `cost_incomplete`（宽）/ `days_without_cost_baseline`（严）/ `cost_incomplete_note` + Note。
   - 每日毛利：报告级 `cost_incomplete` + `cost_incomplete_note` + 部位行 `cost_incomplete`。
5. **期初结转生效日 = 链的起点**：门店在那一天重新定义了库存与进价 ⇒ 那天的期初用门店录的值
   （不再被「昨天卖剩的量」覆盖），且门店给的进价优先于历史单价。

### 9.3 新增不变量（已用单测锁住）

| 不变量 | 用例 |
|---|---|
| 次日期初 == 上日期末（kg 与金额**精确相等**） | `TestCutChain_ConsecutiveDays_CarryOpeningExactly` |
| 短卖后成本**不得塌成 0**、对账差额 = 0 | `TestCutChain_ShortSell_UnitCarriesOverSoCostNotZero` |
| 无基线的天必须标注（不算成成本 0） | `TestCutChain_NoBaseline_CostMarkedUncomputable` / `TestGrossMargin_CostIncompleteMarked` |
| 写路径会物化（不绑盘点） | `TestCutChain_WritePathMaterializesChain` |
| 长期毛利 = 每日毛利之和 | `TestCutChain_PeriodMarginEqualsSumOfDaily` / `TestPeriodMargin_AuditMaterializesEveryDay` |
| 报告级 `cost_incomplete`（宽）与 `days_without_cost_baseline`（严）**口径不同**、且文案不得出现「0 天」 | `TestPeriodMargin_PartialCostIncomplete_ReportLevelFlag` |

> ⚠️ **判别力**：摘掉链式接续 ⇒ 链测试立即变红（症状「链断了」）。
> ⚠️ **cube 假身必须按 `oper_date` 返回**：链现在逐日回溯，假身若对任何日期返回同一批销售 ⇒ 销售被重复计入每一天。

### 9.4 契约变化

`GET /fresh-meat/gross-margin/period`：
- 行新增 `cost_incomplete`（bool）
- 报告新增 `cost_incomplete`（bool）—— 口径是「区间里**有任何一天/部位**算不出来」（**宽**）
- 报告新增 `cost_incomplete_note`（string）—— 给门店看的**人话**说明，前端**原样显示**
- 报告新增 `days_without_cost_baseline`（int）—— 口径是「成本**完全**算不出来」的天数（**严**）
- Note 追加「成本算不出来」的说明（⚠️ **按宽/严拆成两种文案**：旧写法在
  `cost_incomplete=true` 但 `days_without_cost_baseline=0` 时会拼出**「其中 0 天成本算不出来」**
  ⇒ 自相矛盾且误导店长，生产实测已复现）

> ⚠️ **区间级提示必须由报告级 `cost_incomplete`（宽口径）驱动**，
> `days_without_cost_baseline` 只作补充 —— 店 00 的 10 月区间正是
> 「部分天/部分部位算不出来」（天数 = 0），按天数驱动 ⇒ **一条提示都不显示**。

`GET /fresh-meat/gross-margin`：
- 报告新增 `cost_incomplete`（bool）、`cost_incomplete_note`（string）
- 部位行新增 `cost_incomplete`（bool）

⇒ 键名不变、都是**可缺省** ⇒ 老客户端不受影响。