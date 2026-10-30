# 鲜肉分割基线：估计样本通道（A 方案）—— 2026-10-30

> 触发：owner 2026-10-30 拍板 **A 方案** ——「倒推值按当日各猪毛重比例拆到每头猪，
> 作为**估计样本**单独标记喂分割建议基线；实录样本 ≥5 的部位仍用实录 p50，不够才用估计 p50。」
> 追加硬要求：「**每个门店样本要单独**」⇒ 每店生成自己的基线，
> 并**预留**多门店分级基线对比 + 跨门店选用的**数据模型**（具体实现列为待办）。

---

## 1. 背景：为什么门店的「建议分割」会偏

建议分割 `S_c` = 统计基线 p50 优先、LLM 兜底（`cut_split_algo.go::loadSuggestedSplitKg`）。
而统计基线的样本来自 `pig_cuts` —— **门店手工补录的分割实录**。

生产实测（店 00）：`pig_cuts` = **0 行**、`pig_forecast_baselines` = **0 行**
⇒ 基线永远不够样本 ⇒ 建议分割**全部**来自 LLM
⇒ 那是**语言先验**（通用出肉率），不是这家店的真实出肉率
⇒ 建议分割系统性偏 ⇒ 入库量 `W_c` 偏 ⇒ 销售成本偏 ⇒ 毛利偏。

## 2. A 方案：用日终倒推值当估计样本

门店不补录，但每天都有**日终实盘** ⇒ 全店恒等式能解出「今日新分割」：

```
A_c = 全店期末结余 + 销售 + 统收 + 报损 − 全店期初
```

⇒ 这个 `A_c` 是**部位级**的当日总量 ⇒ 按当日各猪的**毛重比例**摊到每头猪
⇒ 每头猪每个部位一条**估计样本** ⇒ 喂基线 ⇒ 建议分割开始反映**本店真实出肉率**。

### 三条硬规矩（写在代码注释里，也写进了单测）

| 规矩 | 为什么 |
|---|---|
| ① **只信「有完整日终盘点」的倒推** | 没有完整盘点时倒推走的是「台面流量近似」（搬上台是老货 ⇒ 只能扣掉估计）⇒ 噪声太大 ⇒ 会把基线教歪 |
| ② **只喂 derived（倒推）部位** | 有 `pig_cuts` 实录的部位，实录本身就是更准的样本 ⇒ 估计样本只补「没有实录」的部位 ⇒ 两个池子互补、不打架 |
| ③ **独立通道**（不写进 `pig_cuts`） | `pig_cuts` 同时是「按猪摊成本」的输入 + 删猪时的「有补录」锁 ⇒ 混进去会污染 FIFO 摊销 + 锁死删猪 |

### 为什么按**毛重**摊

- ⚠️ 不能按「建议分割」摊 ⇒ **循环论证**（建议会复制自己）。
- ⚠️ 也不能按「上次实录」摊 ⇒ 那本来就是另一个池子。
- ⇒ 毛重是**唯一独立于本算法的体量信号** ⇒ 大猪出得多、小猪出得少。

**尾差处理**：除法有 16 位精度 ⇒ 各份直接相加可能差 1e-16
⇒ **最大毛重那头的份额 = 总量 − 其余各份之和**（十进制减法是精确的）
⇒ Σ 恰好等于 `A_c`（基线样本不能凭空多/少）。

## 3. 数据模型

| 表 | 作用 | 关键列 |
|---|---|---|
| `pig_cut_estimates`（新） | 估计样本（每头猪每个部位一条） | 唯一键 `(branch_id, pig_id, cut_type)`；`weight_kg` / `weight_band` / `biz_date` / `source='stocktake_derived'` |
| `pig_forecast_baseline_borrows`（新，**只建表不实现**） | 跨门店选用基线的预留 | `branch_id`（借用方）/ `weight_band` / `cut_type` / `source_branch_id`（被借方）/ `enabled` |
| `pig_forecast_baselines`（加 4 列） | 基线 + 留痕 | `recorded_sample_count` / `estimated_sample_count` / `sample_source` / `source_branch_id` |

**主键**：三张表都是**单列代理主键**（确定性哈希 / 业务键拼装）
⇒ 能进 `PurgeRegistry` ⇒ 契约测试在生产留下的行**清得掉**。

> ⚠️ 顺带修掉的陈旧注释：`purge.go` 里原写「**不登记** `pig_forecast_baselines`：
> 主键是复合的（体重段×部位），purge 按单列主键定位 ⇒ 报"模型没有主键"」
> —— 那是**代理主键改造之前**的情况，现在该表已有单列 `id` ⇒ 登记上。

## 4. 基线口径（选池）

```
recN ≥ MinForecastSamples(5) ⇒ 用实录池，sample_source='recorded'
否则 estN ≥ 5               ⇒ 用估计池，sample_source='estimated'
否则                        ⇒ 取样本多的那个留痕，但**不可用**（回退 LLM）
```

- `sample_count` = **实际采用**的那个池子的样本数（= 可用性判据 `Usable()`）。
- `recorded_sample_count` / `estimated_sample_count` = 两个池子**各自**的样本数（留痕）。

## 5. 门店隔离（owner 硬要求）

| 层 | 做法 |
|---|---|
| 估计样本表 | 唯一键含 `branch_id`；写、读都按店过滤 |
| 实录样本 | `pig_cuts` JOIN `whole_pigs` 时**两边都按 `branch_id` 过滤**（数据脏了也不会把别店的猪算进来） |
| 基线 | `pig_forecast_baselines` 早已按 `branch_id` 分组 ⇒ 沿用并加固 |

## 6. 反馈回路风险与限制（已知、受控）

估计值来自恒等式（间接受 W/S 影响）⇒ 用三条限制压住：
**实录优先 + 只信完整盘点 + `sample_source` 留痕**。
跨店对比 / 选用是下一步（**待办**）。

## 7. 上线影响：生产数字**不变**

生产实测：店 00 只有 **2 头猪**（10-29）⇒ 每个 `(体重段, 部位)` 的估计样本数 = **2 < 5**
⇒ 估计池**暂不可用** ⇒ 建议分割 / 毛利数字**不会变**（仍走 LLM 通用先验）。

⇒ 等门店每天有猪、攒够 5 个样本，建议分割才会切到本店基线 —— **这一步会改变毛利数字，是预期行为**。

基线重算保持**手动**端点（`POST /forecast-baselines/recompute`），**不自动重算**
⇒ 避免生产数字意外变动。

## 8. 守卫清单

| 守卫 | 位置 |
|---|---|
| 倒推值按毛重比例摊 + Σ 守恒 | `forecast_estimates_test.go::TestForecastEstimates_SplitByGrossWeight` |
| 没有完整盘点 ⇒ 不产样本（**两个分支**：无盘点单 / 半盘） | `TestForecastEstimates_NoCompleteStocktake_NoSamples` |
| 有实录的部位不产生估计样本 | `TestForecastEstimates_RecordedCutNotEstimated` |
| 估计样本不写进 `pig_cuts` | `TestForecastEstimates_DoesNotTouchPigCuts` |
| 实录 ≥5 用实录 | `TestForecastBaselines_RecordedWinsOverEstimated` |
| 实录不够用估计 | `TestForecastBaselines_EstimatedFallback` |
| **门店隔离**（A 店的样本不进 B 店的基线） | `TestForecastBaselines_StoreIsolation` |
| 幂等 | `TestForecastEstimates_Idempotent` |
| 读取侧按店过滤 | `TestForecastBaselines_ListIsStoreScoped` |
| **线上**：新列出参 + 门店隔离 | `deployer/tests/contract/fresh_meat_forecast_estimate_test.go::TestFreshMeat_ForecastBaselines_DeclaresSamplePools` |
| **线上**：估计通道端到端（一次性测试门店） | 同上 `::TestFreshMeat_ForecastBaselines_EstimateChannelEndToEnd` |

### 判别力自检（做过）

- 撤掉 `r.DataSource != dataSourceActual` 过滤 ⇒ 「无完整盘点」用例两个分支各产出 1 行样本 ⇒ **立刻红**。
- 撤掉估计样本的 `branch_id` 过滤 ⇒ 「门店隔离」用例报 `S002 的基线只该用它自己的 1 条估计样本, got 6` ⇒ **立刻红**。
- 交换「实录优先 / 估计优先」顺序 ⇒ 报 `sample_source="estimated" + p50 应是实录的 10` ⇒ **立刻红**。

## 9. 待办（owner 通知后再做）

1. **多门店分级基线对比** —— 把各店的基线（分层 × 部位 × p50）横向对比，看哪家店的出肉率偏。
2. **跨门店选用** —— 本店某分层样本不够时，借用别店的基线（`pig_forecast_baseline_borrows` 已建表，
   `pig_forecast_baselines.source_branch_id` 已留好「实际采用的样本来源门店」）。

⚠️ 这两项**本轮只留数据模型，不实现**（owner 明确要求）。
