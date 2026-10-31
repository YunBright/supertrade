# 搭赠（买肉送猪肝这类「有成本、无收入、没过称、没过账」的出库）

> owner 2026-10-31 拍板口径。
> 生产现场：店 00 / 2026-10-30 / 猪肝 3.030kg。

---

## 1. 问题：搭赠在系统里"消失"了

门店把猪肝当**搭赠**送给顾客 —— **没过称、也没过账**：

| 事实 | 证据（生产实测，店 00，已查证） |
|---|---|
| 10-29 切出猪肝 3.030kg（移动加权单价 13.1689） | `cut_cost_daily` ⇒ 期末 3.030kg / 39.90 元 |
| 10-30 猪肝**没卖出去** | `line_sales_by_pigs` 里 liver 销售行 = **0 条** |
| 10-30 猪肝**没报损** | `waste_logs` 店 00 = **0 行** |
| 10-30 盘点猪肝 = **0 kg** | `pork_cuts_stocktakes` 10-30 归属日 34 行，liver 行 = 0/0 |

⇒ 这 3.03kg **从系统里彻底消失**：没有销售、没有报损、没有搭赠。

后果（三条，全部是真的）：

1. **销售成本凭空 39.90 元**，但猪肝营收 = 0 ⇒ 报表上像"卖猪肝亏了 39.9 元"；
2. **缺口被当成盘亏** ⇒ 门店以为自己盘错了 / 丢货了；
3. **进短卖** ⇒ 描红 + **审计结转被拒**（误伤门店）。

---

## 2. 口径（owner 2026-10-31 拍板）

### 2.1 搭赠是链上的**第四个出口**

```
链上出口 = 销售 + 统收 + 报损 + 搭赠
```

⇒ 搭赠**从可售量里扣掉**（否则账面推演比实盘多 ⇒ 永远被当成盘亏）。

⚠️ **不混进报损**：混进去会污染「切分损耗 / 隔夜失重」统计、让报损口径失真。
⇒ 落库走 `waste_logs` **加一列** `waste_type`（`loss` 报损 / `gift` 搭赠），老数据 default `'loss'` ⇒ 加列安全。

### 2.2 成本**扣毛利、单列一行**

```
毛利 = 营收 − 销售成本 − 统收成本 − 报损成本 − 搭赠成本
```

⚠️ **销售成本只含"真卖掉的货"的成本** ⇒ 搭赠成本**不在**销售成本里。

### 2.3 重量**没过称** ⇒ 倒推建议值 + 门店确认

```
缺口 = 账面应有(全店) − 实盘(柜台实盘 + 冷库量)
     = 搭赠 + 真盘亏        ← 两者混在一起 ⇒ 必须门店确认
```

- 系统给**建议值**（走同一条链 ⇒ 与报表数字永远对得上）；
- 门店点**确认** ⇒ 记一条 `waste_logs(waste_type='gift')` ⇒ **缺口归零、不再提示**；
- 确实丢了/损了 ⇒ **不确认** ⇒ 缺口留在账上（走原有的盘亏/短卖处理）。

### 2.4 对账恒等式

```
期初存货 + 本期入库 + 短卖成本 == 销售成本 + 统收 + 报损 + 搭赠成本 + 期末存货
```

⚠️ **搭赠分量必须落在流出侧**（放流入侧会变 2× 搭赠成本）。
⚠️ **短卖成本必须在流入侧补回来**（它已经含在销售成本里）。

### 2.5 搭赠要**扣冷库余额链**

冷库余额链**只跟物理流水**（期初结转 / 盘点结转 / 搬上台 / 大宗）——
搭赠是物理出库 ⇒ 必须扣（**裁 0**，遵循 R1「链上永远 ≥0」）。

⚠️ 不扣 ⇒ 那批送出去的货一直留在链上 ⇒ 次日期初虚高 ⇒ **成本链被污染**。

### 2.6 搭赠计入当天**总流出**

⇒ 短卖 / 盘亏反映的是**真实缺口**（总量够时仍然是 0）。

⚠️ **不要说"搭赠不算短卖"**：把 5kg 搭赠藏起来反而是**漏报**。
真实缺口本来就该在短卖里 —— 只是**归因**从"盘亏"改成"搭赠"，**总毛利不变**。

---

## 3. API 契约

### 3.1 查建议值

```http
GET /api/v1/fresh-meat/gift-suggestions?date=YYYY-MM-DD
```

```jsonc
{
  "branch_id": "00",
  "date": "2026-10-30",
  "count": 1,
  "suggestions": [
    {
      "cut_type": "liver",
      "cut_label": "猪肝",
      "cube_product_id": "01419",   // 部位主货号（确认时直接带上）
      "expected_kg": 3.03,          // 账面应有（已减掉已确认的搭赠）
      "actual_kg": 0,               // 实盘
      "gap_kg": 3.03,               // 缺口 = 搭赠 + 真盘亏
      "recorded_gift_kg": 0,        // 已确认过的搭赠
      "unit_cost_yuan": 13.1689,
      "suggest_cost_yuan": 39.9,
      "data_source": "actual"       // actual | partial | estimated
    }
  ]
}
```

⚠️ **对象包裹**（与仓里其它列表端点一致）⇒ 前端统一 `_expectMap`。
⚠️ `estimated`（当天没盘点单）⇒ **不产生建议**（没盘过就无从比较）。

### 3.2 确认搭赠（录搭赠）

```http
POST /api/v1/fresh-meat/waste-logs
```

```jsonc
{
  "cut_type": "liver",
  "cube_product_id": "01419",
  "qty_kg": 3.03,
  "reason": "搭赠",
  "waste_type": "gift",
  "recorded_at": "2026-10-30T12:00:00+08:00"
}
```

⚠️ **`recorded_at` 必须是被确认的那一天**（后端按它归属营业日）⇒ 前端**写死 +08:00**，
不能用设备本地时间（设备在别的时区会落到**隔壁一天**）。

服务端容错（**替门店补齐**，否则这行会**静默丢失**）：

| 门店给了 | 服务端行为 |
|---|---|
| 部位 + 货号 | 原样落库 |
| 只有部位 | **自动补该部位主货号** |
| 只有货号 | **反查部位**（冷库链按部位记 ⇒ 没部位扣不掉冷库余额） |
| 都没有 | **400** |

### 3.3 毛利出参新增

| 位置 | 键 | 含义 |
|---|---|---|
| `per_cut[]` | `gift_kg` | 该部位搭赠量（kg） |
| `per_cut[]` | `gift_cost_yuan` | 该部位搭赠成本（元） |
| `total` | `gift_cost_yuan` | 全店搭赠成本（元） |
| `reconciliation` | `gift_cost_yuan` | 对账流出侧的搭赠分量（元） |
| `reconciliation` | `short_sell_yuan` | 计入成本的账面不足金额（元，**已含在销售成本里**） |

---

## 4. 改动清单

### 4.1 后端（`supertrade`）

| 文件 | 改动 |
|---|---|
| `model/model.go` | `WasteLog` 加 `waste_type`；常量 `WasteTypeLoss/WasteTypeGift` + `ValidWasteType()`；`RecordWasteLogInput` 加 `WasteType` |
| `service/service.go` | `RecordWasteLog` 校验 `waste_type`；替门店补货号 / 反查部位；都没给 ⇒ 400 |
| `service/moving_average.go` | `loadSkuFlows` 按 `waste_type` 拆出**独立的 giftKg 桶**；`ReconciliationRow` 加 `gift_cost_yuan` |
| `service/cut_cost.go` | `partGiftKg` 部位桶；`soldTotal` 含搭赠；`expectedCloseKg` 减搭赠；`giftCost`；`salesCost`/`gm` 减搭赠成本；新增 `GiftKg/GiftCostYuan/GiftGapKg/GiftGapActualKg`；对账 `DiffYuan` 右侧减搭赠 |
| `service/cut_margin.go` | 部位行 / 全店合计带 `GiftKg`/`GiftCostYuan`；毛利减搭赠成本 |
| `service/gross_margin.go` | `PerCutGrossMargin` / `TotalGrossMargin` 加搭赠字段 |
| `service/period_margin.go` | 期间毛利成本聚合加 `GiftCostYuan` |
| `service/daily_balance.go` | 冷库余额链新增 `giftOut`（裁 0） |
| `service/gift.go`（新） | `GiftSuggestion` / `GiftSuggestionList` / `ListGiftSuggestions` |
| `handler/handler.go` | `GET /gift-suggestions`（scope `freshmeat:view`） |

### 4.2 前端（`tradewind_app`）

| 文件 | 改动 |
|---|---|
| `domain/fresh_meat/fresh_meat_gross_margin.dart` | 部位行 / 全店加搭赠字段；`ReconciliationRow` 加 `gift_cost_yuan` + `short_sell_yuan`；**修 `inflowYuan`/`outflowYuan`**；`derivedRemainKg` **减搭赠** |
| `domain/fresh_meat/fresh_meat_repository.dart` | `RecordWasteLogInput` 加 `wasteType`；`GiftSuggestion` / `GiftSuggestionList`；`listGiftSuggestions` |
| `infra/fresh_meat/fresh_meat_repository_impl.dart` | 实现 `listGiftSuggestions`（404 ⇒ 空列表，不打挂页面） |
| `application/fresh_meat/fresh_meat_gift_controller.dart`（新） | 建议列表 + 确认搭赠 |
| `ui/fresh_meat/widgets/margin_gift_card.dart`（新） | 搭赠确认卡片 |
| `ui/fresh_meat/widgets/margin_reconciliation_card.dart` | 两侧标签写全短卖 / 搭赠；搭赠成本单列一行 |
| `ui/fresh_meat/widgets/margin_summary.dart` | 毛利卡副标题加「已扣搭赠成本 ¥x」 |
| `ui/fresh_meat/widgets/gross_margin_cards.dart` | 窄屏部位卡加「搭赠」行 |
| `ui/fresh_meat/widgets/gross_margin_table.dart` | 宽屏**不加列**（保持 11 列）；毛利单元格 tooltip 注明已扣搭赠成本 |

⚠️ **前端镜像后端口径时的三个坑**（这轮踩到的）：

1. **`derivedRemainKg` 必须减搭赠** —— 后端 `expectedCloseKg = avail − sold − bulk − waste − gift`
   ⇒ 不减 ⇒ 搭赠行**每一行都误报「应有被修正」**（角标刷屏）。
2. **对账两侧都要写全** —— 短卖在流入侧、搭赠在流出侧 ⇒ 少算一项 ⇒
   卡片显示「平」但**两行自己加不上**。
3. **`recorded_at` 写死 +08:00** —— 设备本地时间会落到隔壁一天。

---

## 5. 守卫清单

### 5.1 后端单测（`service/cut_gift_test.go`，6 例）

| 用例 | 锁住什么 |
|---|---|
| `TestGift_NotRecorded_ShowsAsVariance` | 没记搭赠 ⇒ 猪肝 variance −10kg（报盘亏）、销售成本 200、毛利 −200、**对账闭合**、建议 gap=10kg |
| `TestGift_Confirmed_SeparatesCostAndClearsVariance` | 确认后：variance=0、gift_kg=10、gift_cost=200、**销售成本=0**、**总毛利 −200（确认前后总额不变）**、对账闭合、建议消失 |
| `TestGift_GiftIsNotLoss` | 报损进报损桶、搭赠进搭赠桶，**互不串** |
| `TestGift_ShortSellCountsGift` | 卖 12 + 搭赠 5、可售 10 ⇒ 短卖 **7kg**（真实缺口） |
| `TestGift_ColdChainReducesBalance` | 期初 10kg + 搭赠 3kg ⇒ 期末 **7kg** |
| `TestGift_Validation` | 既没部位也没货号 ⇒ 400；`waste_type` 非法 ⇒ 400；只给部位 ⇒ 自动补货号且进链 |

### 5.2 判别力自检（逐条撤掉修复 ⇒ 必须变红）

| 撤掉的修复 | 变红的用例 | 症状 |
|---|---|---|
| 搭赠计入总流出 | `TestGift_ShortSellCountsGift` | 短卖 2 ≠ 7 |
| 销售成本减搭赠成本 | `TestGift_Confirmed_...` | 销售成本 200、毛利 −400、差额 −200 |
| 毛利减搭赠成本 | `TestGift_Confirmed_...` | 毛利 0 ≠ −200 |
| 建议值算缺口 | `TestGift_NotRecorded_...` | 建议列表 0 条 |
| 搭赠扣冷库余额链 | `TestGift_ColdChainReducesBalance` | 期末 10 ≠ 7 |

### 5.3 前端用例（`test/ui/fresh_meat/margin_gift_test.dart`，10 例）

- 毛利出参解析（部位 + 全店）；老版本后端缺字段 ⇒ 0；
- 对账恒等式两侧自洽（`inflow − outflow == diff`）；
- **`derivedRemainKg` 减搭赠** ⇒ 不误报「应有被修正」；
- `giftRecordedAt` 固定 +08:00 ⇒ 任何设备时区都落在当天营业日；
- 卡片渲染 / 点确认（带建议值 + `waste_type=gift` + 货号 + 日期）/ 无建议不占位；
- 对账卡两侧标签写全 + 搭赠成本单列。

### 5.4 契约（`deployer/tests/contract/fresh_meat_gift_test.go`，4 条）

跑在**一次性测试门店**上（真实门店一行不碰）：建议形状 / 确认闭环 / 对账闭合 / 验参。

---

## 6. 已知相邻缺口（不在本轮范围）

- `POST /forecast-baselines/recompute` 只读样本池、**不补样本** ⇒ 历史日（链没跑过的）样本缺失。
- `POST /cost-chain/materialize?from=…` 存量快照重物化**未执行**（非修复必需）。
- 店 00 的 10-28 期初各部位冷库 kg + 进价**只有「后腿 10kg」一行** ⇒ 10-30 的 143kg 短卖里有一部分是「期初没录」造成的（门店动作）。
