# 鲜肉「分割基线加入单价」+ 售罄率/日终剩余/建议毛利（2026-10-31）

> owner 需求（2026-10-31 18:26）：
> 1. 售罄率要体现「猪肉台日终盘点、今日有剩余」（旧口径 `sold/purchased` 在生产 10-30 给出 **100.75%**）。
> 2. 统计栏第三行（进货 / 售罄率 / 报损）在**报损前面**加一格「日终剩余（猪肉台日终盘点总值）」。
> 3. 分割基线加入单价：样本以日为单位，从每日各部位**实际销售单价** + **整体毛利率**倒推部位进货单价。
> 4. 被搭赠等**无销售记录**的部位 ⇒ 保持无分割单价基线 ⇒ 退取链 ① 门店整店加权平均价 ② LLM 缓存单价 ③ 兜底 LLM 重新给价。
> 5. 基线有了单价后应用基线，必须**结合单猪采购价按比例缩放** ⇒ 得到建议成本单价。
> 6. 部位明细表：新增「成本单价」列（建议/实际两行）；「单价」→「零售单价」（只留均价）；毛利、毛利率按新算法。
> 7. 循环依赖由我们自行设计，边界清晰：各部位合计价值 **≤ 录入猪的总进货金额**。

---

## 一、口径总表

| 量 | 口径 | 出处 |
|---|---|---|
| 部位进货单价（样本） | `零售均价 ÷ (1 + 整体毛利率)` | `forecast_price.go` / `price_model.go` |
| 建议成本单价 | `单价基线 × (单猪进价 ÷ 参考猪进价)`，逐猪封顶 | `forecast_price.go` |
| 日终剩余（kg） | 猪肉台**柜台实盘**（**不含冷库**） | `cut_margin_dayend.go` |
| 日终剩余（元） | 日终剩余 kg × 折算进货单价 | 同上 |
| 折算进货单价 | `零售均价 ÷ (1 + 今日毛利率)`（owner 亲定，与单价样本**同一条公式**） | 同上 |
| 售罄率 | `销售 ÷ (销售 + 日终剩余)` ⇒ 恒 ≤ 100% | 同上 |
| 建议毛利 | `营收 − 建议成本单价×零售销量 − 统收成本 − 报损成本 − 搭赠成本 + 预期毛利` | 同上 |
| 实际毛利 | `营收 − 销售成本 − 统收成本 − 报损成本 − 搭赠成本 + 预期毛利`（**账不动**） | `cut_margin.go` |

> ⚠️ **三条倒推口径同源**：单价样本倒推、建议成本单价、日终剩余估值都用「零售均价 ÷ (1 + 毛利率)」
> ⇒ 不会出现"两个成本价口径"。

---

## 二、数据层：三张新表

`internal/fresh-meat/model/price_model.go`

| 表 | 粒度 | 作用 |
|---|---|---|
| `pig_cut_price_samples` | 店 × 日 × 部位 | **日样本**：零售均价 + 整体毛利率 ⇒ 倒推的部位进货单价 |
| `pig_cut_price_baselines` | 店 × 部位 | **基线**：按销量加权聚合的部位进货单价 + 参考猪进价（缩放基准） |
| `cut_unit_price_caches` | 店 × 部位 | **LLM 单价缓存**（退取链第②步） |

### ⚠️ `biz_date` 用 `varchar(10)` 而不是 `datatypes.Date`

`datatypes.Date.Value()` **保留原时区** ⇒ 服务按 `bizTZ=Asia/Shanghai` 写的行落库成
`"2026-10-06T00:00:00+08:00"`，别的时区写的落成 `"2026-10-06T00:00:00Z"`
⇒ **同一个业务日的两种形态** ⇒ `ON CONFLICT(branch_id, biz_date, cut_type)` **不触发**
⇒ 转而撞主键 `id`（确定性代理键 ⇒ 必然撞）。

⇒ 用**纯日期字符串**：排序 / 比较 / 唯一键都跨驱动安全（PG 与 SQLite 一致）。

---

## 三、循环依赖的解法（结构性断环）

**问题**：建议成本单价依赖单价基线；单价基线依赖当日销售价与当日毛利率；
当日毛利率又依赖当日成本 ⇒ 如果用当天的数据算当天的建议，就是**自证**。

**解法**：`loadPriceBaselinesAsOf` 只取 **`biz_date < 当日`** 的样本。

```
D 日的建议成本单价 ← 只用 < D 日的样本 ⇒ 天然滞后一天 ⇒ 绝不自证
D 日结束后采样的 D 日样本            ← 供 D+1 日使用
```

物化表 `pig_cut_price_baselines` **只给门店看**，不参与计算（避免"人改了表 → 数就变"的隐性回环）。

### 边界守卫（owner 给的两条边界）

| 边界 | 实现 | 位置 |
|---|---|---|
| 重量：各部位建议分割之和 ≤ 猪重 | `scaleSplitToPigWeight` **只往下缩** | `cut_split_algo.go` |
| 价值：Σ(建议成本单价 × 建议分割 kg) ≤ 猪总进货金额 | 逐猪封顶，**按比例只往下缩** | `forecast_price.go` |

> ⚠️ 两条守卫都是「**只往下缩**」：宁可低估，不可虚高。

---

## 四、退取链（owner 指定的顺序）

```
① 门店整店加权平均价  = 该店全部有效单价样本的倒推进货单价，按销量加权
② LLM 缓存单价        = cut_unit_price_caches（LLM 生成分割建议时顺带给的部位单价）
③ LLM 重新给价        = 显式端点触发（POST /cut-unit-prices/refresh）
   ⚠️ 不在报表路径同步调 LLM —— 避免 30s 超时
```

三步都走不通 ⇒ 该部位**没有建议成本单价** ⇒ 建议毛利**算不出来**（显示「—」），
**不用实际销售成本冒充**（那会让「建议」和「实际」长得一样 ⇒ 这一列白做）。

---

## 五、新增出参（毛利看板）

### 部位行 `per_cut[]`

| 键 | 类型 | 含义 |
|---|---|---|
| `day_end_remain_kg` | decimal? | 猪肉台日终剩余（kg）；**没完整盘点 ⇒ 键缺失** |
| `day_end_remain_yuan` | decimal? | 日终剩余总值（元）；折算价取不到 ⇒ 键缺失 |
| `sell_through_rate` | float? | 售罄率（0~1）；没完整盘点 ⇒ 键缺失 |
| `suggested_cost_unit_price_yuan` | decimal? | 建议成本单价（上一轮已加） |
| `suggested_cost_price_source` | string | `baseline` / `store_avg` / `llm_cache` / `none` |
| `actual_cost_unit_price_yuan` | decimal? | 实际成本单价 = 销售成本 ÷ 总销量 |
| `suggested_gross_margin_yuan` | decimal? | 建议毛利 |
| `suggested_gross_margin_pct` | float? | 建议毛利率 |

### 全店合计 `total`

| 键 | 类型 | 含义 |
|---|---|---|
| `day_end_remain_kg` / `day_end_remain_yuan` | decimal | 各部位之和 |
| `sell_through_rate` | float? | 全店售罄率 |
| `suggested_gross_margin_yuan` / `suggested_gross_margin_pct` | decimal?/float? | 全店建议毛利 / 毛利率 |
| `suggested_cost_partial` | bool | **有部位取不到价** ⇒ 建议毛利是**不完整**的 |

> ⚠️ `decimal.Decimal` 是 struct ⇒ `encoding/json` 的 `omitempty` 对它**无效**
> ⇒ **可空字段一律用指针** ⇒ 键缺失才是真的"没有这个数"。

---

## 六、前端改动（`F:\flutter\tradewind_app`）

| 位置 | 改动 |
|---|---|
| `margin_summary.dart` | 统计栏第三行：`进货 / 售罄率 / **日终剩余** / 报损`；日终剩余显示**金额**（kg 放副标题）；**报损改金额**（kg 放副标题） |
| `gross_margin_table.dart` | 「成本单价」列（建议/实际两行）+「单价」→「零售单价」（只留均价）；**毛利、毛利率两行**（上行建议、下行实际） |
| `gross_margin_cards.dart` | 窄屏卡片同步：毛利/毛利率两行 |
| `fresh_meat_gross_margin.dart` | 新字段解析；**售罄率不再前端自算**（后端给） |

> ⚠️ **售罄率不再前端自己算**：旧口径在前端是 `sold / purchased`
> ⇒ 生产 10-30 出现 100.75%。口径是后端的职责，前端只负责把它显出来。

---

## 七、端点

| 方法 | 路径 | 作用 |
|---|---|---|
| GET | `/forecast-price-baselines` | 列单价基线 |
| POST | `/forecast-price-baselines/recompute` | 重算单价基线（幂等） |
| GET | `/forecast-price-samples` | 列单价日样本 |
| POST | `/forecast-price-samples/collect` | 采集某天的单价日样本 |
| DELETE | `/forecast-price-samples` | 删样本（**墓碑**，不物理删） |
| POST | `/cut-unit-prices/refresh` | 兜底：重新问 LLM 要部位单价 |
| GET | `/forecast-price-preview` | **预览**：按基线算建议分割 + 建议成本单价（只读） |
| POST | `/whole-pigs/:pig_id/apply-baseline` | **重算**：已录的猪切成「只用基线」+ 重算成本链 |

---

## 七之二、「按基线预览 / 重算」通道（owner 2026-10-31 20:36 追加）

> owner：「我打算用 29 日的 2 个分割样本、1 个 7.3 元/斤(14.6 元/kg)的 29 日猪单价样本
>   倒推出的『分割基线 + 部位进货单价基线』应用到 30 日去，
>   需要你在录入猪的时候提供使用基线重算建议分割和建议成本单价的功能
>   （目前只能靠删除和重录实现）。我这样才能看到效果。」

### 为什么要有这条通道

原来只有「**录猪时选**建议生成方式」（`advice_mode`）⇒ 录错了就只能**删了重录**
⇒ 会丢掉当天的补录与销售归属 ⇒ 门店不愿意用「只用基线」这个模式。

⇒ 这里补两个动作：**预览**（还没录猪时先看）与**重算**（已录的猪就地改口径）。

### `GET /forecast-price-preview?date=&weight_kg=&pig_price_yuan=`

**只读**，不写任何库。返回**扁平**结构：

| 键 | 类型 | 含义 |
|---|---|---|
| `date` | string | 归属营业日 |
| `weight_kg` / `pig_price_yuan` | decimal | 这头猪的毛重 / 进价 |
| `pig_total_cost_yuan` | decimal | 猪的总进货金额（= 毛重 × 进价） |
| `total_split_kg` | decimal | 各部位建议分割之和 |
| `total_value_yuan` | decimal | 各部位「建议成本单价 × 建议分割 kg」之和 |
| `capped` | bool | **true** ⇒ 合计价值超了猪的总进货金额，已按比例**只往下缩** |
| `rows[]` | array | 逐部位：`cut_type` / `suggested_split_kg` / `suggested_cost_unit_price_yuan`（**键可能缺失**）/ `cost_price_source` |

> ⚠️ **与毛利看板同源**：预览走的是 `loadSuggestedSplitPerPig` + `suggestedCutUnitPrices`
> ⇒ 和看板「成本单价」列的建议行**同一个数** ⇒ 门店在两个页面不会看到两个数。
>
> ⚠️ **预览一律基线口径**（`advice_mode=baseline`）⇒ 不取 LLM 兜底
> ⇒ 门店看到的就是"**纯基线**会给什么"，不会被 AI 的建议搅混。

### `POST /whole-pigs/:pig_id/apply-baseline`

body `{"mode":"baseline"}`（可省；`"auto"` 改回自动）。

| 键 | 类型 | 含义 |
|---|---|---|
| `pig_id` / `advice_mode` / `rematerialized_days` | string/string/int | 被改的猪 / 改后的模式 / 重算了几天成本链 |
| 其余 | 同预览 | 与预览**同一套结构**（扁平） |

### ⚠️ 三条语义（容易踩）

1. **切「只用基线」会清掉 AI 建议**（`llm_advice_json=nil`、`advice_status=none`、
   `data_source=stub`）⇒ 免得门店以为 AI 建议还在生效。
   ⚠️ 顺带让「盘点预期剩余」的兜底（它读 `llm_advice_json`）不再含 LLM 那份
   ⇒ 与成本链口径一致（都是基线接管）。
2. **切回「自动」时**不**清** `llm_advice_json` —— 新建议是**异步**生成的
   ⇒ 清掉会让这段时间这头猪一条建议都没有 ⇒ 反而制造缺口。
3. **切「只用基线」前必须校验该体重段有可用基线**（`hasUsableBaselineBand`）
   ⇒ baseline 模式 = **基线没覆盖的部位没有建议** ⇒ 若这个体重段压根没基线，
   这头猪就**一条建议都没有** ⇒ 当天产出凭空少一整头猪
   ⇒ 到处是「缺口 / 短卖 / 幻影搭赠」⇒ **400 拦住**并说清"先补基线，或改用自动"。

### 已审计冻结

改建议 ⇒ 改成本链 ⇒ 已结转的期间不能被静默改写 ⇒ 与「补录 / 重新生成建议」同口径
（`assertNotAudited`）。

---

## 七之三、前端（`F:\flutter\tradewind_app`）

| 位置 | 改动 |
|---|---|
| `baseline_price_card.dart` / `baseline_price_row.dart` | 分割基线页新增「**部位进货单价基线**」卡（部位 / 进货单价 / 参考猪进价 / 样本 n / 来源）+「重算」按钮 |
| `pig_create_baseline_preview_card.dart` / `_result.dart` | 录猪表单新增「**按基线预览**建议分割与成本单价」卡（斤→kg 换算统一收敛到 `PigCreateFormState`） |
| `pig_apply_baseline_button.dart` | 已录的猪加「**用基线重算**」按钮（写操作 ⇒ 前置确认弹窗）；已是基线口径 ⇒ 显示「已按基线」且禁用 |

> ⚠️ **取不到价的部位显示「—」**，不是 `0.00` ⇒ 用 `double?` + `parseOptionalDecimal`
> ⇒ 缺键 ≠ 0（0 会被店长读成"这个部位不要钱"）。
>
> ⚠️ **斤→kg 换算只有一份**：表单的 `_submit` 与预览都走 `PigCreateFormState.weightKgValue /
> pigPriceYuanValue` ⇒ 避免两处换算规则漂移。

---

## 八、已知边界与遗留

1. **售罄率的分母只含柜台实盘，不含冷库** —— owner 知情选定。
   ⚠️ 但分子（销售）含冷库卖出去的货 ⇒ 分母与分子**口径略不一致**（这是 owner 的取舍，不是 bug）。
2. **建议毛利只换「零售货的成本口径」** —— 统收成本 / 报损成本 / 搭赠成本仍是**实际数**
   ⇒ 两行只差一处，可横向比。
3. **全店建议毛利在部分部位缺价时是不完整的** ⇒ `suggested_cost_partial=true` ⇒ 前端要提示。
4. **样本要求当天整体毛利率 ∈ (0, 0.95)** ⇒ 越界则**整天不产样本**
   （成本算不出来时倒推出的单价没有意义 ⇒ 不能让垃圾样本把基线教歪）。
5. **冷库余额链不记销售** ⇒ 冷库只进不出 ⇒ 搬上台/结转没记全时，
   **实盘会偏大、倒推的产出会超标** ⇒ 样本与基线质量的真正前提是门店把物理流水记全。
6. **单价样本只从有销量的部位产** ⇒ 搭赠部位永远没有自己的基线 ⇒ 走退取链。
7. **SQLite 单测库里 `decimal(12,4)` 列是 NUMERIC 亲和 ⇒ 存成 float64** ⇒ 读回来与精确值差 ~1e-16
   ⇒ 断言要**按列精度（4 位）比对**（见 `forecast_price_test.go` 的 `priceEq`），
   否则是**假红**（生产 PG 是真 numeric，落库就是 4 位）。
