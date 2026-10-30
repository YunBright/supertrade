# 鲜肉分割「样本库」规格（owner 2026-10-30 重新定义模型）

> 一句话：**「统计回归基线」是派生结果，底座是「样本库」。**
> 门店操作的是**样本**（加入 / 删除），基线由样本库**重算**得到，自动应用到新录入猪的建议分割。

---

## 0. 为什么改（owner 原话与纠正）

owner 2026-10-30：

> 「可能我理解错了，我现在知道了，"统计回归基线"是每个分店只有一个列表，而我错认为基线是样本。
> 那么实际是我们需要一个管理样本的功能，**按一天生成基线是错的**，其实是**按一天加入样本**。
> 如果每天有两头猪，那么就有两个重量样本。"按一天加入样本"需要实现的是往统计回归基线添加样本的功能，
> 如果是同一天的同一个重量，则从当天日终盘点-搬台+销售…计算出样本后覆盖旧的；
> 如果不是同一种重量则新加一个样本，我也可以选择**删除某天某个重量的样本**（如果觉得它不准，分割不合理）。
> **基线列表的重算功能就是把样本库重新按照算法算一遍产生新的统计回归基线**，
> 而该统计回归基线**自动应用到新录入猪的建议分割**去。」

⇒ 原来的「按一天生成基线」把**样本采集**和**基线拟合**混成了一个动作，
且「生成」产出的基线只覆盖那一天的分层、还带着 `manual_generated` 豁免 ⇒ 语义错、覆盖不全。

---

## 1. 模型

```
                    ┌──────────────────────────────┐
   按一天加入样本 ──▶ │  样本库（pig_cut_estimates）  │ ◀── 删除样本（打墓碑）
   （跑引擎落估计）  │  一头猪 × 一个部位 = 一条样本   │      excluded_at
                    └───────────────┬──────────────┘
                                    │ 重算（全量拟合）
                                    ▼
                    ┌──────────────────────────────┐
                    │  统计回归基线                 │
                    │  (体重段 × 部位) × n/分位数   │  ──▶ 新录入猪的建议分割
                    │  pig_forecast_baselines       │      (p50 优先，LLM 兜底)
                    └──────────────────────────────┘
```

**底座改了什么**

| | 改前 | 改后 |
|---|---|---|
| 一等公民 | 基线行 | **样本** |
| 采集动作 | 「按一天**生成基线**」（采集+拟合混在一起） | 「按一天**加入样本**」（只采集） |
| 拟合动作 | 自动重算（跳过 `manual_generated` 行） | **重算**（从样本库全量拟合，覆盖所有分层） |
| 可用门槛 | `n ≥ 5` 或 `manual_generated` | **`n ≥ 1`**（有样本就算用） |
| 样本身份 | 一条样本 = 一个 (猪, 部位) 观察 | 不变，但**计数按猪**：同段两头猪 = `n = 2` |
| 删除粒度 | 只能删一条基线行 | **按天+体重段整组删** / **按单头猪删** |
| 删除方式 | 物理删 | **打墓碑**（`excluded_at`），物理删会被引擎重跑复活 |

---

## 2. 口径（owner 已拍板的三条 + 推导出的细则）

### 2.1 样本身份 = 一头猪一个样本

- 主键 = (店, 猪, 部位) ⇒ 同一头猪同一部位**一条**记录。
- **样本数按猪计**：同一天同一体重段进了两头猪 ⇒ 该分层 `n = 2`。
  - ⚠️ 若按「体重」合并，两个样本会互相覆盖 ⇒ `n` 永远卡在 1 ⇒ 基线永远「不够样本」⇒ 死锁。
- 分层 = **体重段(50kg 一段) × 部位**（与既有口径一致）。

### 2.2 可用门槛 = 有样本就算用（n ≥ 1）

- `MinForecastSamples` 由 5 改为 **1**。
- ⇒ `manual_generated` 的**豁免机制退役**（不再需要绕过门槛）。
- ⇒ `manual_generated` 列**保留**，语义改为「**曾手工触发过**」的可追溯标记（重算时不清零）。
- ⚠️ **行为变更（有意）**：重算**不再跳过** `manual_generated` 的行
  —— 基线是派生结果 ⇒ 重算就该把它算回去（旧的「手动行整层跳过」保护已废）。
  ⇒ 手工生成过的行**照样**被重算/借用覆盖，但 `manual_generated` + `generated_from_date` 保留（可追溯）。
- ⚠️ 副作用（有意）：只要有 1 条样本，该分层就用基线、不再回退 LLM ⇒
  **样本质量的责任落到「删除样本」这个动作上**（owner 原话：「如果觉得它不准，分割不合理」）。

### 2.3 拟合口径 = 合并观察（实录 ∪ 估计）

- 两个池子：
  - **实录** = `pig_cuts`（门店手工补录的分割）
  - **估计** = `pig_cut_estimates`（引擎按全店恒等式倒推、按毛重摊到每头猪）
- 合并规则：**按 (猪, 部位) 去重，实录优先**
  - 同一头猪同一部位，实录与估计代表**同一件事** ⇒ 同时计入 = 重复计样本 ⇒ 均值被拉偏。
  - 实录更准 ⇒ 实录优先，估计只补实录没有的部位。
- 分桶：按体重段分桶 ⇒ 每桶一组样本值。
- `sample_source` 新增 **`mixed`**（两个池子都有贡献时）：
  - `recorded` / `estimated` / `mixed`。

### 2.4 删除 = 打墓碑（`excluded_at`）

- `pig_cut_estimates` 加一列 `excluded_at`（可空 timestamp）+ `excluded_reason`。
- **不能物理删**：引擎重跑那一天的成本链会把样本**重新写回来** ⇒ 物理删 = 删除无效。
- 墓碑行：
  - **不参与拟合**（读样本时过滤掉）；
  - **不被重建覆盖**（重建那天的样本时跳过墓碑行，也不清除墓碑）；
  - 可**恢复**（owner 未明确要求，但墓碑语义天然要求可逆 —— 误删要能救回来）。

### 2.5 样本库的边界

- 样本库 = **`pig_cut_estimates`**（引擎倒推的观察，带 `biz_date` + `weight_band`）。
- `pig_cuts`（手工补录）**继续参与拟合**，但**不在样本库里删**
  —— 删它会动 FIFO 摊销与「有补录锁死删猪」的逻辑 ⇒ 成本链会被污染。

### 2.6 加入样本 ≠ 重算

- 「按一天加入样本」= 让引擎跑那一天 ⇒ 落样本。
- **不自动重算**（重算是另一个明确动作）。
- 前端提示：「已加入 N 条样本，点「重算」生效」。

### 2.7 重算 = 从样本库全量拟合

- 覆盖**所有**分层（含原来 `manual_generated` 的行）。
- 借用逻辑不变：本店该分层**自己一个样本都没有** ⇒ 借别店的（被借店也没有 ⇒ 不借，回退 LLM）。
- **样本被删光 ⇒ 该层基线行随之删除**（否则脏数字会一直给出错建议）。
  - 判据：本次没写出来、但原来 `sample_count > 0` 的**自产**行 ⇒ 删。
  - `sample_count = 0` 的历史行（迁移默认值）不动（本来不可用，留着无害）。

---

## 3. API

> 出参一律 **snake_case**；时间用 RFC3339；日期 `YYYY-MM-DD`。
> 错误语义沿用既有约定：`ErrInvalidInput` → 400、`ErrCubeUnavailable` → 503、权限类原样上抛。

### 3.1 `GET /forecast-samples` —— 列样本库

Query：`date`（可选）、`weight_band`（可选）、`include_excluded`（可选，默认 `false`）

```jsonc
{
  "branch_id": "00",
  "sample_count": 48,            // 未被排除的样本条数（猪 × 部位）
  "excluded_sample_count": 0,
  "pig_count": 2,
  "recorded_sample_count": 0,    // 本店实录样本条数（不在库里，但照样参与拟合）
  "groups": [
    {
      "date": "2026-10-29",
      "weight_band": "100-149",
      "pig_count": 1,
      "sample_count": 24,
      "excluded_sample_count": 0,
      "pigs": [
        {
          "pig_id": "WP20261029xxxxxxxx",
          "ear_tag": "A12",              // 猪已删则为空串
          "gross_weight_kg": 100.2,       // 猪已删则 0
          "weight_band": "100-149",
          "date": "2026-10-29",
          "sample_source": "estimated",  // estimated | recorded | mixed
          "excluded": false,
          "cuts": [
            { "cut_type": "belly", "weight_kg": 13.944, "sample_source": "estimated", "excluded": false }
          ]
        }
      ]
    }
  ]
}
```

- `sample_source` = `mixed` ⇒ 这头猪**同时**有门店手工补录的实录（那部分**删不掉**，删它会动 FIFO 成本）。

### 3.2 `POST /forecast-samples/collect` —— 按一天加入样本

Body：`{ "date": "2026-10-29", "source_branch_id": "" }`

- `source_branch_id` 空 = 本店；给了 = **替别店**跑那一天，样本落在**别店自己的**样本库
  （样本归猪的门店 ⇒ 不能把别店的猪写进本店样本库）。
- 流程：完整性校验（录入 / 搬台 / 销售 / 盘点，**跟毛利看板同一条销售读路径**）
  → 跑那天的引擎 → 落估计样本（跳过墓碑行）。
- 响应：

```jsonc
{
  "date": "2026-10-29",
  "source_branch_id": "",
  "written": 48,            // 落库的样本条数
  "skipped_excluded": 0,    // 因已被排除而没写的条数
  "pig_count": 2,
  "group_count": 2,
  "message": "已加入 48 条样本（2 头猪），点「重算」后生效"
}
```

- 缺项 ⇒ **400 点名缺哪一项**（沿用 `dayDataGaps`）。
- 引擎跑不了（cube 读失败）⇒ **503 上抛**，不回落成「那天没样本」。
- 幂等：重复调用覆盖同 (猪, 部位) 行；**不复活墓碑**。

### 3.3 `DELETE /forecast-samples` —— 删除样本（打墓碑）

Query（二选一，缺一 ⇒ 400）：

| 参数 | 语义 |
|---|---|
| `date` + `weight_band` | **整组**删：那天该体重段的**所有**猪的所有部位样本 |
| `pig_id` | **单头猪**删：该猪的所有部位样本 |

响应：`{ "excluded": 24, "message": "已删除 24 条样本，点「重算」后生效" }`

### 3.4 `POST /forecast-samples/restore` —— 恢复被删的样本

Body：`{ "date": "2026-10-29", "weight_band": "", "pig_id": "" }`（`date` 或 `pig_id` 至少给一个）

响应：`{ "restored": 24 }`

### 3.5 `POST /forecast-baselines/recompute` —— 重算（改语义）

响应：`{ "baselines": 48 }`
（`baselines` 字段名沿用，前端已经在读。）

### 3.6 `DELETE /forecast-baselines` —— 删单条基线行（保留）

⚠️ **不再在 UI 上暴露**：基线是**派生结果**，删行之后「重算」会把它原样算回来 ⇒ 按钮是个假动作。
要剔除某层数据 ⇒ 去**样本库**删样本。
（端点保留为 API 级低层操作，契约测试继续覆盖。）

### 3.7 `POST /forecast-baselines/generate` —— **下线（410 Gone）**

- 语义已被「加入样本 + 重算」两步取代 ⇒ 留着就是**错误的心智模型**。
- 前端与契约测试同步改。
- ⚠️ **返回 410 而不是 404**：404 会让人以为是 URL 写错了；410 + 一句"该用哪个端点"才是
  「这条语义被换掉了」。响应体：`{"code":"endpoint_removed","message":"「按一天生成基线」已下线：请改用「按一天加入样本」(POST /forecast-samples/collect) + 「重算」(POST /forecast-baselines/recompute)"}`

---

## 4. 数据模型变更

### 4.1 `pig_cut_estimates` 加两列

| 列 | 类型 | 说明 |
|---|---|---|
| `excluded_at` | `timestamptz` 可空 | 墓碑时间；非空 = 已删除，不参与拟合 |
| `excluded_reason` | `varchar(32)` | 留痕（`manual`） |

- AutoMigrate 会自动加列（可空列 ⇒ 老行不受影响）。
- ⚠️ **不新建表** ⇒ 无需登记 PurgeRegistry。

### 4.2 `pig_forecast_baselines` 不改结构

- `manual_generated` 语义变更为「曾手工触发」标记（重算时**保留**）。
- `sample_source` 新增取值 `mixed`。

---

## 5. 前端

> Flutter 规约：widget ≤ 200 行、screen ≤ 800 行、单函数 ≤ 80 行；
> `flutter analyze lib test` 0 warning；`flutter test` 全绿；**不主动构建 APK**；
> 用户可见文案禁 Markdown 粗体（用「」）。

### 5.1 基线页（`fresh_meat_baseline_screen.dart`）结构调整

```
┌ 基线维护 ────────────────────────────────┐
│ [提示卡]                                   │
│ [样本库卡片]   ← 新增：列表 / 加入 / 删除  │
│ [基线列表卡片] ← 保留：只读 + 「重算」按钮  │
│ [多门店对比]                               │
│ [跨店选用]                                 │
└───────────────────────────────────────────┘
```

⚠️ **不加新路由 / 不动 `app/router.dart`**（他人未提交改动）⇒ 样本库做成基线页内的**卡片**。

### 5.2 样本库卡片

- 按 **(天 × 体重段)** 分组展示：日期、体重段、猪数、样本条数。
- 每组两个动作：
  - 「删除这组」⇒ `DELETE /forecast-samples?date=&weight_band=` ⇒ 弹确认框（说清后果：重算后该组不再参与拟合）
  - 「删除这头猪」⇒ `DELETE /forecast-samples?pig_id=`
- 顶部动作：「按一天加入样本」⇒ 日期选择 + `POST /forecast-samples/collect`
  ⇒ 成功提示「已加入 N 条样本，点「重算」后生效」。
- 已删除的样本默认**不显示**；卡片上给一个「显示已删除」开关（`include_excluded=true`），
  命中已删除的行给「恢复」按钮（`POST /forecast-samples/restore`）。

### 5.3 基线列表卡片

- 保留「重算」按钮（= 从样本库全量拟合）。
- 行：去掉「删除」按钮（见 3.6）。
- 「样本数」旁加**弱提示角标**：`n < 5` ⇒ 「样本少（n=1）」。
  ⚠️ 这是**提示不是拦截**（门槛已是 n ≥ 1）。
- `sample_source` 枚举加 `mixed`（前端 `ForecastSampleSource`）。

### 5.4 前端模型

- `kMinForecastSamples` 由 5 改为 **1**（对齐后端）。
- 新增：`ForecastSampleGroup` / `ForecastSamplePig` / `ForecastSampleCut` / `ForecastSampleCollectResult`。
- `ForecastBaselineGenerateInput` / `ForecastBaselineGenerateResult` **删除**（端点移除）。

---

## 6. 契约测试（deployer）

新增：
1. `POST /forecast-samples/collect` ⇒ 400 点名缺项 / 200 落样本。
2. `GET /forecast-samples` ⇒ 分组结构正确（天 / 体重段 / 猪 / 部位）。
3. `DELETE /forecast-samples`（整组 + 单猪）⇒ 墓碑生效；`GET` 不再返回（默认不含已删）。
4. 删除后 `POST /forecast-baselines/recompute` ⇒ 该层基线**不复活**（判别力：先删再重算，样本数不反弹）。
5. `POST /forecast-samples/restore` ⇒ 恢复后重算又生效。

改写：
6. 移除 `POST /forecast-baselines/generate` 的断言（端点移除 ⇒ 断言改成 404）。
7. 基线可用门槛：n ≥ 1 的分层 `usable = true`。

---

## 7. 验收标准

- [ ] 后端单测全绿（含判别力自检：撤掉任一修复 ⇒ 对应用例红）。
- [ ] 契约测试全绿。
- [ ] 生产回验：店 00「加入样本 2026-10-29」→「重算」⇒ 基线 `sample_count` 变化 ⇒ **新录入猪的建议分割取自基线 p50**（不再是 LLM）。
- [ ] 生产回验：删除 10-29 某组样本 ⇒ 重算 ⇒ 该层基线消失/样本数下降 ⇒ 再恢复 ⇒ 复原。

---

## 8. 不在本轮范围

- 存量快照重物化（`POST /cost-chain/materialize`）—— 生产写操作，**待 owner 批准**。
  ⇒ 影响：**历史日**（链没跑过的）没有估计样本 ⇒ 重算覆盖不到历史。
- 10-28 期初各部位冷库 kg + 进价的补齐 —— 门店动作。
- 前端分割提示文案口径 + kg 显示精度 —— owner 明确暂缓。
- 跨店借用的两条边界口径 —— 待 owner 拍板。

---

## 9. 交付与回验（2026-10-30）

### 9.1 代码

| 仓库 | 改动 |
|---|---|
| supertrade | `model.go`（墓碑两列）/ `service/forecast_fit.go`（**新**，合并观察+拟合）/ `service/forecast_samples.go`（**新**，样本库四个动作）/ `service/forecast.go`（重算覆盖全量 + 门槛 1）/ `service/forecast_estimates.go`（重建跳过墓碑）/ `service/forecast_maintain.go`（移除 generate）/ `handler/handler.go`（4 个新端点 + generate → 410） |
| flutter | `domain/fresh_meat/fresh_meat_forecast.dart`（样本库模型 + `mixed` + 门槛 1）/ repository + impl / controller（样本库 controller）/ `ui/fresh_meat/baseline/`（**样本库卡片** 5 个新文件 + 基线行去删除 + 去掉 generate 卡） |
| deployer | `tests/contract/fresh_meat_forecast_samples_test.go`（**新** 4 个用例）+ `fresh_meat_forecast_maintain_test.go`（generate 用例迁出） |

### 9.2 判别力自检（每条修复单独撤掉 ⇒ 对应用例立刻红）

| 撤掉的修复 | 变红的用例 | 症状 |
|---|---|---|
| 重建跳过墓碑行 | `TestTombstone_NotResurrectedByCollect` | `skipped_excluded=0`，样本被复活 |
| 重算清理「样本被删光的层」 | `TestDeleteSamples_Group_ThenRecompute_BaselineGone` | 基线行还在（p50=15 继续给错建议） |
| 合并观察的去重 | `TestRecompute_MergedObservations_RecordedWins` | 样本数 3（同一头猪被算两条） |
| 门槛 n ≥ 1（改回 5） | `TestRecompute_SingleSampleIsUsable` | 有 1 个样本却「不可用」⇒ 死锁 |

### 9.3 生产回验（2026-10-30，店 00）

| 步骤 | 结果 |
|---|---|
| `GET /forecast-samples`（店 00） | `sample_count=48 / pigs=2 / groups=2`（2026-10-29 × {100-149, 50-99}） |
| `POST /forecast-samples/collect`（2026-10-29） | **200** / `written=48` / `skipped_excluded=0` / 「已加入 48 条样本（2 头猪），点「重算」后生效」 |
| `POST /forecast-baselines/recompute` | **200** / `baselines=48` |
| 落库核对（重算**前后**） | 48 行、n 与 p50 **完全一致**（belly 100-149 = 13.944）、`manual_generated=true` 保留为留痕、`sample_source=estimated`、店 01 的 3 行**未被动** |
| `pig_cut_estimates` | 店 00 = 48 行、墓碑 0 行；新增列 `excluded_at` / `excluded_reason` 已建 |
| 服务端日志 | **无 panic** |

### 9.4 契约（生产，一次性测试门店）

- 新增 4 个用例 **全 PASS**：
  - `TestFreshMeat_ForecastSamples_CollectRequiresCompleteDay`（400 点名缺哪一项）
  - `TestFreshMeat_ForecastSamples_CollectListDeleteRestore` ⭐（加入 → 列表 → 重算 → 删除 → 重算（基线消失）→ 再加入（**墓碑不复活**）→ 恢复 → 重算（基线回来））
  - `TestFreshMeat_ForecastSamples_DeleteRequiresTarget`（定位不全 ⇒ 400）
  - `TestFreshMeat_ForecastBaselines_GenerateEndpointGone`（410 + 指向新端点）
- 真实门店的数据一行未碰（写操作全在一次性测试门店里）。
- **全量 fresh-meat 契约回归**（`-run "TestFreshMeat"`，打生产）：**54 PASS / 4 SKIP / 0 FAIL**（exit 0，307.9s）⇒ 本轮口径变更没有带崩任何既有用例。

### 9.5 单测

- 后端：`go test ./internal/fresh-meat/... -count=1` **全绿**；`go vet` 干净；本次新增/改动的文件 **gofmt 干净**。
- 前端：`flutter analyze lib test` → **No issues found**；`flutter test` → **2948 passed / 10 skipped / 0 failed**（新增 20 个）。
- **未构建 APK**（按 owner 规矩）。

