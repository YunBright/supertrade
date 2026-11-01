# 结算付款审批：收付款明细（2026-10-25）

> 需求原文（用户口径）：「付款审批」增加填入区别于 ERP 系统结算单的其它收付款项。
> 分**收款项**、**付款项**两种表，表格方式添加、修改、删除明细。
> 类型与备注**皆以文本保存**；供应商结算单归为付款类型「货款」。
> 所有收付款总和为正数或 0，收款项**冲减**付款金额。
> 机器审核维持原有判断规则，并新增两条放宽口径（陈列费/堆头费 → 进货可以多些；补差价 → 允许毛利降低）。

---

## 1. 业务形态

「付款审批」不再只有"付供应商货款"这一种。门店实际还会发生：

| 方向 | 类型（下拉选项） |
| --- | --- |
| 付款项 | 水电费、租金、物业费、垃圾处理费、软件服务费、运费、肉类加工费、设备维护费、耗材、招待费、燃气费、电信费、福利、工资、其它 |
| 收款项 | 陈列费、堆头费、补差价、兑换、纸皮回收、扣点、节庆费、合同工本费、POP费、条幅费、投流费、其它 |
| 货款 | **派生**：勾选的供应商结算单（不是手工行） |

- 「其它」是**可改文字**的选项：用户可以把"其它"两字改成自定义文字（如"临期处理费"）。
- 类型与备注都是**文本**：新增类型不用改表、不迁移；后端按**文本精确匹配**识别陈列费/堆头费/补差价
  （匹配不上就不给放宽额度 —— 宁可少放宽，不误放宽）。

## 2. 金额口径

```
应付金额 = 货款 + 付款项合计 − 收款项合计      （且不为负）
```

- 净额为负（纯收款项，如只有「纸皮回收」）⇒ **应付按 0 计**，不拦下这一单
  （用户口径：「付款审批可以只有'货款'，只有'纸皮回收'」）。
- 手工累加金额（`manual_amount_yuan`）比对的是**应付金额**，不是货款合计。
- 校验容差不变：**付款取整容差 1 元**（`数值容差 0.01,付款取整容差 1`）。

| 口径 | 位置 |
| --- | --- |
| 前端计算 | `wx-h5/src/pages/settlement-items.js` → `computeTotals` / `computePayableCents` |
| 后端计算 | `supertrade/internal/settlement-auditor/service/items.go` → `computeTotals` |
| 引擎勾稽 | `service/checks.go` → `checkAmounts`（比 `PayableYuan`） |

## 3. 数据模型

### 3.1 新表 `approval_items`

| 列 | 类型 | 说明 |
| --- | --- | --- |
| `id` | varchar(36) | 主键 |
| `approval_id` | varchar(32) | 审批实例 id |
| `branch_id` / `applicant_user_id` | varchar | 冗余（与实例一致，便于按人/店筛） |
| `direction` | varchar(16) | `payment` / `receipt` |
| `item_type` | varchar(64) | **类型文本**（"陈列费"/自定义文字…） |
| `remark` | varchar(256) | 备注（所属日期等补充信息） |
| `amount_yuan` | numeric(14,2) | 金额（元），恒 > 0 |
| `created_at` | timestamptz | |

由 `cmd/settlement-auditor/main.go` 的 `AutoMigrate` 建表（生产已建）。

### 3.2 为什么**货款不落这张表**

货款的事实源是 `approval_settlements`（cube 快照）。再抄一份进明细表就会出现**两个金额口径**，
迟早对不上 ⇒ 明细表只存人填的行，货款行在**读取/审核时派生**（`service/items.go::goodsItemRows`）。

### 3.3 为什么类型/备注是文本而不是枚举

用户明确要求「两者皆保存为文本」，且「其它」可改成任意文字 ⇒ 用枚举/码表就得先建码表才能存一个字。
文本列 ⇒ 新增类型不改表、不迁移。

## 4. 接口契约

### 4.1 提交 `POST /settlement-approvals`

```jsonc
{
  "applicant_wecom_id": "...",
  "settlement_ids": ["CP001"],          // 可以为空（纯收款项）
  "manual_amount_yuan": "1100.00",      // 应付金额；纯收款时为 "0"
  "remark": "9月货款",
  "flow_selections": { "approver": [], "handler": [], "cc": [] },
  "receipt_items": [                    // 手工收款项
    { "type": "陈列费", "remark": "2026-09", "amount_yuan": "200.00" }
  ],
  "payment_items": [                    // 手工付款项
    { "type": "水电费", "remark": "2026-08", "amount_yuan": "300.00" }
  ],
  "attachments": [ /* 附件，字段见 docs/settlement-approval-attachment.md */ ]
}
```

- 金额一律**十进制字符串**（JSON number 会走 float64）。
- `settlement_ids` 允许为空 —— 前提是填了收/付款明细；两者都没有 ⇒ 400。
- `manual_amount_yuan` 允许为 **0**（纯收款）；负数 ⇒ 400。

### 4.2 详情 `GET /settlement-approvals/:id`

响应新增 `items` 数组（手工收付款明细，字段同 `approval_items`），
「重新发起」用它预填两张明细表。

### 4.3 校验规则（service 层）

| 规则 | 报错文案 |
| --- | --- |
| 类型非空 | `第 N 行收款明细缺类型` |
| 金额 > 0 | `第 N 行付款明细金额必须大于 0` |
| 类型 ≤ 64 字 / 备注 ≤ 256 字 | `第 N 行类型超过 64 字` |
| 单方向 ≤ 20 行 | `收款明细最多 20 行` |

## 5. 机器审核怎么用这些明细

### 5.1 金额勾稽

`checkAmounts` 的第二层从"手工金额 vs 货款合计"改成 **"手工金额 vs 应付金额"**：

```
应付 = 货款(cube) + 付款项(明细行) − 收款项(明细行)，不为负
```

### 5.2 放宽额度（`service/allowance.go`）

| 额度 | 来源类型 | 效果 |
| --- | --- | --- |
| 「进货可以多些」 | 陈列费 + 堆头费 > 0 | **数量类** block（`qty_deviation` 数量偏离、`purchase_without_sales` 自入库后无销售记录）降为 **warn**，消息写明放宽依据 |
| 「允许毛利降低」 | 补差价 > 0 | 算窗口毛利（用本次进货均价估算成本）；缺口 ≤ 补差价 ⇒ info（写明已覆盖）；缺口 > 补差价 ⇒ warn（提示人工终审） |

设计原则（**只放宽既有判断，不新增阻断**）：

- 不新造 block 信号 —— 新 block 会把原本能过的单子挡掉，是更大的行为变更；
- 没有陈列费/堆头费/补差价的单子，**行为与改造前完全一致**（不制造新噪音、不改变既有结论）；
- 放宽后信号**仍是 warn**（不是消失）⇒ 人工终审照旧看得见这些事实；
- 「自入库后无销售记录」也放宽的理由：系统**本来**就允许"申请人的备注"解释数量类信号
  （system prompt:「若备注说明了信号背后的合理原因…可据此认为相应的数量信号不需要拦截」）
  ⇒ 有结构的「陈列费/堆头费收款」给出同样口径是既有口径的延续；
  而用户举例的那笔单（`af9905b6435b485998e38f99f38fa222`）里 7 条 block 全是
  `purchase_without_sales` ⇒ 只放宽数量偏离的话，那笔单仍然过不了。

用户举例（应当通过）：

> 审批 `af9905b6435b485998e38f99f38fa222` 的
> 「伊利金典纯牛奶250ml-6907992507095 本次进货 216，为 2026-08-26 采购入库以来销量 7 的 30.9 倍」
> —— 含陈列费/堆头费时**不作为拒绝理由**。

### 5.3 提示词

- system prompt 新增「收付款明细与放宽口径」一节（陈列费/堆头费 ⇒ 进货可以多些；补差价 ⇒ 允许毛利降低）。
- user prompt 新增「本次收付款明细 N 项」段（方向/类型/备注/金额逐行列出）+「放宽额度」一行。
- 金额结论仍**只由引擎给**（不让 LLM 自己相加，见 `AmountCheck` 注释）。

### 5.4 企微侧

- 备注控件追加摘要：`收付:货款 ¥1000 + 付款项 ¥300 - 收款项 ¥200 = 应付 ¥1100`。
- summary_list 追加三笔账。
- 附件「结算单明细清单」CSV 增加**第二段：收付款明细**（方向/类型/备注/金额/关联单据 + 合计应付）。
  CSV 每行字段数一致（5 列）⇒ Excel 与严格解析器都不会错位。

## 6. 测试

| 用例 | 位置 |
| --- | --- |
| 明细校验/合计/应付净额/放宽额度归类 | `service/items_test.go` |
| 数量偏离降级 / 毛利额度 | `service/allowance_test.go` |
| 提示词列出全部明细 + 放宽口径 | `service/items_test.go::TestBuildAuditUserPrompt_ListsAllItemsAndAllowance` |
| CSV 第二段 | `service/items_test.go::TestBuildSettlementDetailCSV_WithItems` |
| 端到端契约（提交 → 落库 → 详情回读 → 应付额通过审核） | `handler/handler_test.go::TestHandler_Submit_202_WithReceiptPaymentItems` |

## 7. 部署注意

- 结算付款审批是 **settlement-auditor** 这个 app（不是 supertrade 主进程）⇒
  部署命令：`deploy-supertrade.ps1 -Service settlement-auditor`。
- `approval_items` 表由 AutoMigrate 自动建，**不需要**手工 DDL。
- 本次**没有改 wechat-bot**（附件与备注都在 supertrade 侧组装 ⇒ 无跨仓 wire 变更）。
