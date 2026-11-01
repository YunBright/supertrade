// Package service / llm.go —— LLM 综合研判(FR-2.4)。
//
// 设计(仿 fresh-meat llm.go):runAudit 调 verdictFn(ctx, AuditInput),
// 成功则落 llm_verdicts(source=llm)并按 Conclusion 决定放行/拒绝;
// 失败/解析失败由 caller 降级 defaultVerdictFn(source=rule_fallback)。
//
// 默认 VerdictFn 是规则降级:有 block 信号 → reject,否则 pass。
// 生产注入 DaprConversationVerdictFn(dapr Conversation API,仿 fresh-meat)。
// ConvName 默认 "conversation";env SETTLEMENT_AUDITOR_CONVERSATION 覆盖。
// LLMTimeout 默认 30s;env SETTLEMENT_AUDITOR_LLM_TIMEOUT 覆盖。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	dapr "github.com/dapr/go-sdk/client"
	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// 结论取值。
const (
	ConclusionPass   = "pass"
	ConclusionReject = "reject"
)

// StocktakeSuggestion LLM 建议的针对性盘点范围。
type StocktakeSuggestion struct {
	Scope string   `json:"scope"` // category / supplier / product
	IDs   []string `json:"ids"`
}

// AuditInput 是 VerdictFn 的输入(审批快照 + cube 数据 + 规则信号)。
type AuditInput struct {
	Approval    *model.ApprovalInstance
	Settlements []SettlementRow
	// Lines 按结算单分组的 PI/RO 行。
	Lines map[string][]SettlementLineRow
	// Sheets 全部关联采购单头。
	Sheets []PurchaseSheetRow
	// Signals 规则核对引擎产出的信号(供 LLM 参考,不重复发现)。
	Signals []signalDraft
	// Amount 是规则引擎算好的金额勾稽结论。
	//
	// ⚠️ **它存在的唯一理由是"别让 LLM 自己重算金额"**:
	// 2026-10-22 生产实踩,LLM 拿 user prompt 里的显示值自己相加,
	// 把 0.0054 元的差额算成"差 0.01"并当成 block 理由拒了单。
	// 金额结论只有一个口径:引擎算完给 LLM。
	Amount *AmountCheck
	// Items 是本次审批的**全部**收付款明细行(货款行在前,手工行在后)。
	//
	// 用户口径(2026-10-25):「所有收付款项都列给机器审核」。
	Items []ItemRow
	// Totals 是金额汇总(货款/付款项/收款项/应付)。
	Totals ItemTotals
	// Allowance 是收款项给审核的放宽额度(陈列费/堆头费/补差价)。
	Allowance Allowance
	// Margins 是引擎算好的**毛利汇总**(2026-10-30 用户口径)。
	//
	// ⚠️ 与金额同理:**总括数字由引擎给** —— LLM 自己把逐条信号里的
	// 负毛利相加会造出假合计(而且信号里只列了最亏的几个)。
	// 用户要求结论"不能缺少总和",所以这里必须给「负毛利商品数 + 合计金额」。
	Margins MarginSummary
	// Stocktakes 是本次审批关联且**已审核通过**的盘点单。
	//
	// 有它 ⇒ 库存已被盘点核对过 ⇒ 数量类信号(无销售记录/数量偏离)
	// 不作为拒绝理由(与陈列费/堆头费的放宽口径同构,见 allowance.go)。
	Stocktakes []StocktakeVerification
	// Stocking 是本次涉及的**商品组**的备货事实(2026-11-01 用户口径)。
	//
	// 每个组给:本次进货、组内库存、补货后合计、组销量与组盘损(有货天数/日均消耗)、
	// 可售上限天数与量、补货后可售天数、一次进货下限、单次进货中位数、
	// 组内 180 天无销售时的分类。
	// ⚠️ 只放汇总事实,不放逐日曲线(几十个组 × 上百天会撑爆 prompt)。
	Stocking []StockingGroupFact
	// Joint 是联营( LY )结算单的扣点核对事实(2026-10-31 用户口径)。
	//
	// 联营单的"货款"是**毛销售额**,扣点是**收款项**单独冲减 ⇒
	// 应付 = 销售额 − 扣点。LLM 要判断"扣点是否随单申报、扣点率是否达标"。
	Joint JointSummary
	// Dx 是代销( DX )结算单的毛利核对事实(2026-11-01 用户口径)。
	//
	// 代销的毛利隐含在"零售额 − 结算额"的价差里 ⇒ LLM 要判断
	// "整体实现毛利率是否达到最低要求(默认 10%)"。
	Dx DxSummary
}

// MarginSummary 是引擎口径的毛利汇总(两个口径分开统计)。
type MarginSummary struct {
	// GrossNegCount / GrossNegYuan 是窗口毛利为负的商品数与合计负毛利。
	GrossNegCount int
	GrossNegYuan  decimal.Decimal
	// RealizedNegCount / RealizedNegYuan 是实现毛利为负的商品数与合计负毛利。
	RealizedNegCount int
	RealizedNegYuan  decimal.Decimal
}

// StocktakeVerification 是一张**已审核通过**的关联盘点单。
type StocktakeVerification struct {
	HeaderID string
	// Status 是盘点单状态(approved 才进入本结构)。
	Status string
}

// AuditExtras 是流水线可选的补充事实(毛利汇总 / 已盘点)。
//
// ⚠️ 用可变参数传给 decideVerdict / stepLLMVerdict:
// 单测里的旧调用点(不传 extras)行为完全不变。
type AuditExtras struct {
	Margins    MarginSummary
	Stocktakes []StocktakeVerification
	// Stocking 是商品组级备货事实(2026-11-01 用户口径,见 AuditInput.Stocking)。
	Stocking []StockingGroupFact
	// Joint 是联营扣点事实(2026-10-31 用户口径,见 AuditInput.Joint)。
	Joint JointSummary
	// Dx 是代销毛利事实(2026-11-01 用户口径,见 AuditInput.Dx)。
	Dx DxSummary
}

// VerdictResult 是 VerdictFn 的返回值。
type VerdictResult struct {
	Conclusion       string               `json:"conclusion"` // pass / reject
	Reason           string               `json:"reason"`
	Confidence       decimal.Decimal      `json:"confidence"`
	SuggestStocktake *StocktakeSuggestion `json:"suggest_stocktake,omitempty"`
	// RawJSON 原始输出(落 llm_verdicts.raw_json)。
	RawJSON []byte `json:"-"`
	// Source = llm / rule_fallback / flow_config / admin_override。
	Source string `json:"-"`
	// AIConclusionPrefix 是「AI审核结论」控件文案的**首行**（可选）。
	//
	// 2026-10-28 用户口径：管理员人工放行时，控件**第一行**插
	// 「已转人工审核并通过」⇒ 审批人一眼看出这单**没经机器人**，是人放行的。
	//
	// ⚠️ 空 ⇒ 控件文案与改造前完全一致（机器通过 = "机器审核通过:…"）。
	AIConclusionPrefix string `json:"-"`
}

// VerdictFn 是 Service.verdictFn 的签名(单测注入 fake)。
type VerdictFn func(ctx context.Context, in *AuditInput) (*VerdictResult, error)

// ---- 默认 VerdictFn:规则降级 ----

// defaultVerdictFn 不走 LLM:有 block 信号 → reject(拼接 block 消息);
// 否则 pass(reason 注明规则路径,warn 信号数写入 reason)。
//
// 盘点建议不依赖 LLM:数量类异常信号(purchase_without_sales / qty_deviation)
// 本身就携带 ProductID,规则路径直接派生 product scope 建议 —— LLM 不可用时
// "异常→盘点单→任务推送" 闭环不能断(2026-10-11 FR-3 闭环)。
func defaultVerdictFn(_ context.Context, in *AuditInput) (*VerdictResult, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: AuditInput 为 nil", ErrInvalidInput)
	}
	var blocks, warns []string
	for _, d := range in.Signals {
		switch d.Level {
		case model.SignalBlock:
			blocks = append(blocks, d.Message)
		case model.SignalWarn:
			warns = append(warns, d.Message)
		}
	}
	suggestion := stocktakeSuggestionFromSignals(in.Signals)
	if len(blocks) > 0 {
		reason := "规则审核拒绝:" + strings.Join(blocks, ";")
		if len(reason) > 1000 {
			reason = reason[:1000]
		}
		raw, _ := json.Marshal(map[string]any{
			"conclusion":        ConclusionReject,
			"reason":            reason,
			"confidence":        1,
			"suggest_stocktake": suggestion,
		})
		return &VerdictResult{
			Conclusion:       ConclusionReject,
			Reason:           reason,
			Confidence:       decimal.NewFromInt(1),
			SuggestStocktake: suggestion,
			RawJSON:          raw,
			Source:           model.VerdictSourceRuleFallback,
		}, nil
	}
	reason := "规则审核通过:无阻断信号"
	if len(warns) > 0 {
		reason = fmt.Sprintf("规则审核通过:无阻断信号(%d 条警告待人工终审关注)", len(warns))
	}
	raw, _ := json.Marshal(map[string]any{
		"conclusion":        ConclusionPass,
		"reason":            reason,
		"confidence":        0.9,
		"suggest_stocktake": suggestion,
	})
	return &VerdictResult{
		Conclusion:       ConclusionPass,
		Reason:           reason,
		Confidence:       decimal.NewFromFloat(0.9),
		SuggestStocktake: suggestion,
		RawJSON:          raw,
		Source:           model.VerdictSourceRuleFallback,
	}, nil
}

// stocktakeSuggestionFromSignals 从数量类异常信号派生针对性盘点建议。
//
// 仅两类信号触发(它们的语义就是"库存可能对不上"):
//
//	SigPurchaseWithoutSales(block):窗口内无销售却有进货 → 商品可能压库/虚收
//	SigQtyDeviation(warn/block):进货量相对销量严重偏离
//
// 其它信号(单据缺失/未审核/供应商不符/金额勾稽)是流程或录入问题,
// 盘点解决不了,不产建议。返回 nil 表示无建议;ProductID 去重保序。
func stocktakeSuggestionFromSignals(drafts []signalDraft) *StocktakeSuggestion {
	seen := make(map[string]struct{})
	ids := make([]string, 0)
	for _, d := range drafts {
		if d.ProductID == "" {
			continue
		}
		if d.SignalType != SigPurchaseWithoutSales && d.SignalType != SigQtyDeviation {
			continue
		}
		if _, ok := seen[d.ProductID]; ok {
			continue
		}
		seen[d.ProductID] = struct{}{}
		ids = append(ids, d.ProductID)
	}
	if len(ids) == 0 {
		return nil
	}
	return &StocktakeSuggestion{Scope: "product", IDs: ids}
}

// blockMessages 取所有 block 级信号的可读描述（结论强制用）。
func blockMessages(drafts []signalDraft) []string {
	out := make([]string, 0, len(drafts))
	for _, d := range drafts {
		if d.Level == model.SignalBlock {
			out = append(out, d.Message)
		}
	}
	return out
}

// stocktakeSuggestionFromBlocks 只从**阻断级**数量信号派生盘点建议。
//
// ⚠️ 与 stocktakeSuggestionFromSignals 的差别：后者把 warn 级的 qty_deviation
// 也算进去，会在"机器本可以直接放行"的单子上凭空建盘点任务。
// 2026-11-01 用户口径：只有阻断（需盘点核对）才触发盘点联动。
func stocktakeSuggestionFromBlocks(drafts []signalDraft) *StocktakeSuggestion {
	seen := make(map[string]struct{})
	ids := make([]string, 0)
	for _, d := range drafts {
		if d.Level != model.SignalBlock || d.ProductID == "" {
			continue
		}
		if d.SignalType != SigPurchaseWithoutSales && d.SignalType != SigQtyDeviation {
			continue
		}
		if _, ok := seen[d.ProductID]; ok {
			continue
		}
		seen[d.ProductID] = struct{}{}
		ids = append(ids, d.ProductID)
	}
	if len(ids) == 0 {
		return nil
	}
	return &StocktakeSuggestion{Scope: "product", IDs: ids}
}

// ---- Dapr Conversation API 真实接入 ----

// DaprConversationVerdictFn 构造生产用 VerdictFn。
//
// 行为:
//   - timeout 内同步调 dapr conversation.<name>/converse(temperature 0.2);
//   - system prompt 要求只输出 JSON:
//     {"conclusion":"pass|reject","reason":"...","confidence":0.0~1.0,
//     "suggest_stocktake":{"scope":"category|supplier|product","ids":[...]}}
//   - 解析失败/conclusion 非法 → 返 error,由 caller 降级 defaultVerdictFn。
func DaprConversationVerdictFn(daprCli dapr.Client, convName string, timeout time.Duration) VerdictFn {
	if convName == "" {
		convName = "conversation"
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return func(ctx context.Context, in *AuditInput) (*VerdictResult, error) {
		if daprCli == nil {
			return nil, errors.New("dapr client not injected")
		}
		if in == nil || in.Approval == nil {
			return nil, fmt.Errorf("%w: AuditInput.Approval 为 nil", ErrInvalidInput)
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		req := dapr.NewConversationRequest(convName, []dapr.ConversationInput{
			{Role: ptrStr("system"), Content: buildAuditSystemPrompt()},
			{Role: ptrStr("user"), Content: buildAuditUserPrompt(in)},
		})
		resp, err := daprCli.ConverseAlpha1(ctx, req, dapr.WithTemperature(0.2))
		if err != nil {
			return nil, fmt.Errorf("dapr converse: %w", err)
		}
		if len(resp.Outputs) == 0 {
			return nil, errors.New("dapr converse: no outputs")
		}
		raw := strings.TrimSpace(resp.Outputs[0].Result)
		if raw == "" {
			return nil, errors.New("dapr converse: empty result")
		}
		return parseVerdictJSON([]byte(stripCodeFence(raw)))
	}
}

// ptrStr 取 string 地址(ConversationInput.Role 用)。
func ptrStr(s string) *string { return &s }

// stripCodeFence 去掉 LLM 可能包裹的 ```json ... ``` 围栏。
func stripCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

// parseVerdictJSON 解析 LLM 输出为 VerdictResult(校验 conclusion 合法性)。
func parseVerdictJSON(raw []byte) (*VerdictResult, error) {
	var parsed struct {
		Conclusion       string               `json:"conclusion"`
		Reason           string               `json:"reason"`
		Confidence       float64              `json:"confidence"`
		SuggestStocktake *StocktakeSuggestion `json:"suggest_stocktake"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("parse LLM output: %w", err)
	}
	if parsed.Conclusion != ConclusionPass && parsed.Conclusion != ConclusionReject {
		return nil, fmt.Errorf("parse LLM output: conclusion 非法 %q", parsed.Conclusion)
	}
	if parsed.Reason == "" {
		return nil, errors.New("parse LLM output: reason 为空")
	}
	conf := decimal.NewFromFloat(parsed.Confidence)
	if conf.LessThan(decimal.Zero) || conf.GreaterThan(decimal.NewFromInt(1)) {
		conf = decimal.NewFromFloat(0.5)
	}
	// suggest_stocktake 基本校验:scope 非法 → 丢弃建议(不阻断主流程)。
	if st := parsed.SuggestStocktake; st != nil {
		switch st.Scope {
		case "category", "supplier", "product":
			if len(st.IDs) == 0 {
				parsed.SuggestStocktake = nil
			}
		default:
			parsed.SuggestStocktake = nil
		}
	}
	return &VerdictResult{
		Conclusion:       parsed.Conclusion,
		Reason:           parsed.Reason,
		Confidence:       conf,
		SuggestStocktake: parsed.SuggestStocktake,
		RawJSON:          raw,
		Source:           model.VerdictSourceLLM,
	}, nil
}

// buildAuditSystemPrompt LLM 角色设定(只输出 JSON)。
//
// # 金额口径这一段是 2026-10-22 生产事故的直接修复
//
// 实测(审批 54bb82421a08480bbcfdacaf6c814b5e):规则引擎算出的真实差额是
// **0.0054 元**(结算单货款合计 8206.1946 vs 手工付款额 8206.20),在容差内,
// 引擎产出 **0 条**金额信号(audit_logs 的 check_amounts 步写明"新增 0 条金额信号")。
//
// 但当时的 system prompt 只说"规则引擎已标 block 的信号(…/金额勾稽不上/…)通常应 reject",
// 没告诉 LLM "金额勾稽这件事引擎已经做完了",于是 LLM 拿着 user prompt 里的
// **显示值**(StringFixed(2) 的 7765.19 + 441.00 = 8206.19)自己加了一遍,
// 得出"手工 8206.20 与合计 8206.19 差 0.01",当成 block 理由把整单拒了。
//
// ⇒ 三条硬规矩(缺一条就会重演):
//  1. 付款按**元**取整:分位一般不实际支付,角位也可能被舍去 ⇒ 分/角级差异不是问题;
//  2. 金额勾稽**以引擎结论为准**,不许自己把结算单金额相加再和手工付款额比;
//  3. 拒绝理由只引用信号列表里的信号,不写引擎没给出的事实。
func buildAuditSystemPrompt() string {
	return `你是商场采购结算付款审核员。给你一次付款申请的数据:勾选结算单、
每张结算单拆出的采购进货(PI)/退货(RO)单、采购单头、规则核对引擎已发现的信号、
以及窗口期商品销售汇总。你要判断这次付款是否应当放行进入人工终审。

判断要点:
- 规则引擎已标 block 的信号(单据缺失/未审核/金额勾稽不上/无销售进货/数量严重偏离)
  通常应 reject,并在 reason 中引用最关键的几条(数量类另见下方「组级备货核对」口径);
- 只有 warn 信号时,结合销售/供应商/商品常识判断是否需要人工复核,
  无明显问题则 pass,把 warn 摘要写进 reason 供人工终审关注;
- 数据里若给出「已关联盘点单且审核通过」,说明库存已经盘点核对过 ⇒
  数量类信号(自入库后无销售记录/进货量偏离销量)**不作为拒绝理由**,应 pass,
  并在 reason 中写「已通过盘点核对」;
- 若发现某类商品/某供应商/个别商品数据需要盘点,建议针对性盘点:
  suggest_stocktake.scope 取 category|supplier|product,ids 填商品/分类/供应商 id 列表;
  无建议则省略该字段或置 null。

联营帐款单与扣点(单据类型 LY,必须遵守):
- 联营是「门店不买货、供应商在店里卖、门店按销售额抽成」:
  联营单的「货款」是**毛销售额**,「扣点」是**收款项** ⇒ 实际应付 = 毛销售额 − 扣点;
- 联营扣点必须随联营帐款单**一起在本笔付款审批里申报「扣点」收款项**;
  引擎给出 joint_deduct_missing / joint_deduct_short(block)通常应 reject,
  并在 reason 写明应扣点多少、本次申报多少;
- 扣点率 = Σ扣点 / Σ销售额,低于该供应商最低扣点率时引擎给 joint_rate_low(warn):
  若申请人备注/合同依据能解释(例如新品扶持期、临时促销),可 pass 并写明依据;
  否则应 reject,并写明哪家供应商扣点率多少、最低要求多少。

措辞(必须遵守,这条是给**发起人**看的):
- 只陈述事实:哪个商品、进了多少、从哪天起没销售记录、金额差多少。
- **不要推断动机、不要给发起人贴负面判断**。禁止出现
  「虚进」「套取付款」「刷单」「人为操作」「舞弊」「风险」这类词,
  也不要把"数据对不上"说成"存在问题" —— 陈述事实即可,判断留给人工终审。
- 例:写「商品 16.8升琅悦泉水桶装水-02000116 自 2026-08-25 采购入库后无销售记录,本次进货 18 桶,
  请核实该商品近期是否有销售」;不要写「存在虚进、滞销或套取付款风险」。
- 提到**供应商**时用数据里给的写法「<供应商名称>-<供应商编号>」(2026-10-29 用户口径),
  **不要只写编号** —— 店长看到「29」无从判断是哪家;名称在前、编号在后,
  既能认出是哪家、又能核对到具体那家(同名供应商很常见)。

结论写法(2026-10-30 用户口径,必须遵守):
- reason 会原样填进企微审批单的「AI审核结论」控件,它与备注、付款金额、
  结算单号在**同一张单**上 ⇒ **不要复述别处已经显示的信息**:
  不写应付金额/取整后金额、不写结算单号与单号明细金额、不逐笔写收付款明细金额;
- 但**不能缺少总和**:必须给出总括数字,例如「实现毛利为负 3 个商品(合计 -88.66 元)」
  「数量异常 N 个商品」;明细只举 1~2 个最有代表性的商品作例证,不要逐条罗列;
- 用完整句子表达(不要写成逗号串);整段控制在 300 字以内,
  宁可少举例子,也不要让句子在中间被截断。

金额口径(必须遵守,违反会误拒单):
- 实际付款按元取整:分位一般不实际支付,角位也可能被舍去。
  ⇒ 分位/角位的差异(小于 1 元)属于正常取整,**不得作为拒绝理由**,一个字都不要提;
- 金额勾稽由规则引擎完成,结论写在本轮数据的「金额勾稽结论」里,直接采信:
  结论为「一致」时,**不要**再自己把结算单金额相加去和手工付款额比较 ——
  你看到的结算单金额是显示值(可能已四舍五入),自己相加会造出几厘的假差异;
- 只有引擎给出了金额勾稽信号(block)才能把金额写进拒绝理由;
  即便如此,差异达到「元」级才值得写,分位差异只字不提;
- 拒绝理由只能引用信号列表里的信号,不要写引擎没有给出的事实。

申请人的备注(重要上下文):
- 备注随付款申请一起上来,是**申请人给出的解释**,要当推理依据用。
- 若备注说明了信号背后的合理原因(例如「该商品为换购品,不产生销量」「试销品」「样品」
  「本次为铺货」),可据此认为相应的数量信号**不需要拦截**,应当 pass,
  并在 reason 里写明依据是申请人的备注(例如「申请人备注:该商品为换购品,不产生销量,本次无销售记录可接受」)。
- 备注与信号无关时,按常规判断;备注只是解释,**不能推翻**单据缺失/未审核/金额勾稽这类硬异常。

收付款明细与放宽口径(2026-10-25 用户口径,**必须遵守**):
- 本次付款审批可能包含**货款**(供应商结算单)之外的收付款项:
  付款项(水电费/租金/物业费/垃圾处理费/软件服务费/其它)与
  收款项(陈列费/堆头费/补差价/兑换/纸皮回收/其它);
  收款项**冲减**付款金额 ⇒ 应付金额 = 货款 + 付款项 − 收款项(不为负)。
- 数据里的「金额勾稽结论」已经算好了应付金额,**直接采信,不要自己重算**。
- 有**陈列费/堆头费**收款时:供应商为陈列/堆头付了费用,本次进货量大于销量属正常铺货,
  ⇒ **进货可以多些**;「数量偏离」(进货量是销量若干倍)与「自入库后无销售记录」类信号
  **不作为拒绝理由**,应当 pass;
- 有**补差价**收款时:允许因进货偏多导致的**毛利降低**,降低幅度小于补差价总额即可,
  ⇒ 毛利相关的疑虑不作为拒绝理由;
- 本单所有收付款项都会列在数据里,金额结论以引擎口径为准,不要自行推断或改写金额。

毛利信号的两个口径(2026-10-29 用户口径,**必须正确区分**):
- 「gross_margin」(**窗口毛利 / 预期毛利**):成本用**库存加权平均成本价**
  ⇒ 回答"这批货整体上赚不赚钱"(含老库存的成本);
- 「realized_margin」(**实现毛利**):成本用**本批次进货均价**,
  量取「本批次进货总量」与「窗口销售总量」的较小者,
  ⇒ 回答"**这批新进的货**按它自己的进价卖出去赚不赚钱"。
- 两者**可能同时出现、且结论相反** —— 老库存成本低会**掩盖**"这次进货进贵了"
  这件事(例:某生鲜的库存加权平均成本 2.82 元 ⇒ 窗口毛利为正;
  但本批次进货均价 4.63 元 > 销售均价 3.56 元 ⇒ 实现毛利为负)。
  ⇒ **不要**因为「gross_margin」为正就忽略「realized_margin」,也不要把两者混为一谈;
- 出现「realized_margin」时,reason 里**只陈述事实**:哪个商品、
  本批次进货均价多少、窗口销售均价多少、按可卖数量算的实现毛利为负多少,请人工终审关注。
  **不要**写成"这单有问题""存在风险""舞弊"这类判断(见上面措辞纪律)。

数量合理性按「组级备货核对」判断(2026-11-01 用户口径,**必须遵守**):
- 一个**商品组** = 组合/拆分商品归并后的整体(整件进货、拆散零售算同一组);
  数据里的库存/销量/盘损/进货都已换算到组基准单位再相加,不要自己再做换算;
- **组消耗 = 组销量 + 组盘损**(报损/盘亏也是货的去向,已审核单据才计入);
  日均消耗 = 组消耗 / 有货天数 —— 数据里「日均消耗」就是按这个口径算好的,直接用;
- 判定只看一个事实:**补货之后这组货够卖多少天**(「补货后可售 X 天」)
  对比「可售上限 C 天」:
  · 补货后可售 ≤ 可售上限:属正常备货,**不作为拒绝理由**;
  · 本次进货 ≤「一次进货下限」:按最小包装进了一次货,同样**不作为拒绝理由**;
  · 超过可售上限:引擎只给 warn(stocking_excess / qty_deviation),写进 reason 作提示,仍应 pass;
  · 超过可售上限 × 倍数、或超出金额 > 300 元:引擎给 qty_deviation(block),可 reject,
    reason 写明本次进货、组内库存、补货后可售天数、可售上限天数;
- **不要**用「这单进了多少 vs 卖了多」当依据 —— 多日合并付款的进货量是累加值,
  数据里会写清累计/天数/日均;数量判断一律以引擎给的「补货后可售天数」为准;
- 组内近 180 天无销售时,引擎按「刚收货 / 首单 / 长期无销售 / 账实缺口」分类:
  只有**长期无销售**(有库存且最后一次进货超过 30 天)才是 block,且阻断条件写明需盘点核对;
  其余三类是 warn,**不作为拒绝理由**;
- 组里有采购行单位与商品档案单位不一致(unconvertible)⇒ 该组本次未做数量比较,
  **不作为拒绝理由**,只陈述事实;
- 「purchase_price_above_history」(进价高于近 180 天均价)、
  「stock_ledger_gap」(进货−销售−盘损−库存对不上)是**提示事实**,不作为拒绝理由。
- 「已扣退货」表示进货量里**已经**减掉了采购退货数量,不要重复扣减。

只输出 JSON,不要 markdown,不要解释:
{"conclusion":"pass|reject","reason":"中文,200字内","confidence":0.0~1.0,
"suggest_stocktake":{"scope":"category|supplier|product","ids":["..."]}}`
}

// buildAuditUserPrompt 拼数据快照(控制长度:信号最多 20 条,结算单最多 20 张)。
//
// # 金额怎么写进去(2026-10-22 事故的直接修复)
//
//  1. **给引擎的结论,不给它自己相加的机会**:第一段就是「金额勾稽结论」——
//     手工付款额、结算单货款合计、差额、结论,由规则引擎算好直接给。
//  2. **金额一律用原始精度打印**(yuanText,不是 StringFixed(2)):
//     显示值四舍五入过一次,LLM 自己相加就会造出假差异
//     (8206.1946 被打成 8206.19,加出 8206.19 vs 8206.20 = "差 0.01")。
func buildAuditUserPrompt(in *AuditInput) string {
	var sb strings.Builder
	ap := in.Approval
	fmt.Fprintf(&sb, "付款申请:approval_id=%s, 手工累加付款金额=%s 元\n",
		ap.ID, yuanText(ap.ManualAmountYuan))
	// 申请人备注 —— **推理上下文**,不是普通字段:申请人可能已经解释了
	// 某个信号背后的原因(换购品/试销品/铺货)。用户口径 2026-10-22:
	// 备注要参与判断,这样第二次发起时机器审核能给出更贴切的结论。
	if remark := strings.TrimSpace(ap.Remark); remark != "" {
		fmt.Fprintf(&sb, "申请人备注(推理依据):%s\n", remark)
	} else {
		sb.WriteString("申请人备注(推理依据):(空)\n")
	}

	// 金额勾稽结论(规则引擎已核对,直接采信,不要自己再把结算单金额相加)。
	if in.Amount != nil {
		verdict := "不一致"
		if in.Amount.Consistent {
			verdict = "一致(差额在付款取整容差内,属正常取整)"
		}
		fmt.Fprintf(&sb, "金额勾稽结论:%s —— 手工付款额 %s 元,应付金额 %s 元(货款 %s + 付款项 %s - 收款项 %s),差额 %s 元,付款取整容差 %s 元。\n",
			verdict, yuanText(in.Amount.ManualAmountYuan), yuanText(in.Amount.PayableYuan),
			yuanText(in.Amount.SettledTotalYuan), yuanText(in.Amount.PaymentTotalYuan),
			yuanText(in.Amount.ReceiptTotalYuan),
			yuanText(in.Amount.DiffYuan), yuanText(in.Amount.ToleranceYuan))
	}

	// 收付款明细(用户口径 2026-10-25:「所有收付款项都列给机器审核」)。
	if len(in.Items) > 0 {
		fmt.Fprintf(&sb, "本次收付款明细 %d 项(货款 %s + 付款项 %s - 收款项 %s = 应付 %s 元):\n",
			len(in.Items), yuanText(in.Totals.GoodsTotalYuan), yuanText(in.Totals.PaymentTotalYuan),
			yuanText(in.Totals.ReceiptTotalYuan), yuanText(in.Totals.PayableYuan))
		for _, r := range in.Items {
			remark := ""
			if r.Remark != "" {
				remark = "(" + r.Remark + ")"
			}
			fmt.Fprintf(&sb, "  - %s %s %s 元%s\n", directionLabel(r.Direction), r.Type, yuanText(r.AmountYuan), remark)
		}
	}
	// 放宽额度(有陈列费/堆头费 ⇒ 进货可以多些;有补差价 ⇒ 允许毛利降低)。
	if in.Allowance.HasDisplayOrPromotion() || in.Allowance.PriceDiffYuan.GreaterThan(decimal.Zero) {
		fmt.Fprintf(&sb, "放宽额度:陈列费 %s 元,堆头费 %s 元(有则进货可以多些,数量偏离不作为拒绝理由);补差价 %s 元(有则允许毛利降低,幅度小于该总额)。\n",
			yuanText(in.Allowance.DisplayYuan), yuanText(in.Allowance.PromotionYuan),
			yuanText(in.Allowance.PriceDiffYuan))
	}
	// 毛利汇总:总括数字由引擎算好(结论里必须给出总和,不要自己把逐条信号相加)。
	if in.Margins.GrossNegCount > 0 || in.Margins.RealizedNegCount > 0 {
		fmt.Fprintf(&sb, "毛利汇总(引擎口径):窗口毛利为负 %d 个商品,合计 %s 元;实现毛利为负 %d 个商品,合计 %s 元。\n",
			in.Margins.GrossNegCount, yuanText(in.Margins.GrossNegYuan),
			in.Margins.RealizedNegCount, yuanText(in.Margins.RealizedNegYuan))
	}
	// 已通过盘点的盘点单:数量类信号不再是阻断项(库存已核对过)。
	if len(in.Stocktakes) > 0 {
		ids := make([]string, 0, len(in.Stocktakes))
		for i, st := range in.Stocktakes {
			if i >= 3 {
				break
			}
			ids = append(ids, st.HeaderID)
		}
		fmt.Fprintf(&sb, "已关联盘点单 %d 张且均审核通过(header_id=%s):库存已盘点核对,数量类信号不作为拒绝理由。\n",
			len(in.Stocktakes), strings.Join(ids, ","))
	}
	// 备货系数(SKU 级事实,2026-10-31 用户口径)。
	//
	// 用来评估「本次进货量是否合理」:合理备货量 = 周期销售数 × 备货倍数,
	// 周期销售数 = 日平均销量 × 备货周期(日均按采样天数口径,见 service/stocking.go)。
	writeStockingFacts(&sb, in.Stocking)
	// 联营扣点(2026-10-31 用户口径):扣点是否随单申报 + 扣点率是否达标。
	writeJointFacts(&sb, in.Joint)
	// 代销毛利(2026-11-01 用户口径):整体实现毛利率是否达标。
	writeDxFacts(&sb, in.Dx)

	fmt.Fprintf(&sb, "勾选结算单 %d 张:\n", len(in.Settlements))
	for i, st := range in.Settlements {
		if i >= 20 {
			fmt.Fprintf(&sb, "  ... 其余 %d 张略\n", len(in.Settlements)-20)
			break
		}
		fmt.Fprintf(&sb, "  - %s 供应商=%s 类型=%s 货款=%s 账期=%s~%s\n",
			st.ID, supplierLabelForPrompt(st.SupplierName, st.SupplierID), st.DocumentType,
			yuanText(st.TotalGoodsYuan), st.PeriodStart, st.PeriodEnd)
		if lns := in.Lines[st.ID]; len(lns) > 0 {
			for j, ln := range lns {
				if j >= 10 {
					fmt.Fprintf(&sb, "    ... 其余 %d 行略\n", len(lns)-10)
					break
				}
				fmt.Fprintf(&sb, "    · %s %s 货款=%s\n", ln.SourceDocType, ln.VoucherID, yuanText(ln.TotalGoodsYuan))
			}
		}
	}

	fmt.Fprintf(&sb, "规则核对信号 %d 条:\n", len(in.Signals))
	for i, d := range in.Signals {
		if i >= 20 {
			fmt.Fprintf(&sb, "  ... 其余 %d 条略\n", len(in.Signals)-20)
			break
		}
		fmt.Fprintf(&sb, "  - [%s][%s] %s\n", d.Level, d.SignalType, d.Message)
	}
	return sb.String()
}

// writeStockingFacts 把**组级**备货事实写进 user prompt（2026-11-01 用户口径）。
//
// 每个商品组一行，最多 20 个（其余省略）：逐组明细过多会冲淡真正需要关注的信号。
func writeStockingFacts(sb *strings.Builder, facts []StockingGroupFact) {
	if len(facts) == 0 {
		return
	}
	fmt.Fprintf(sb, "组级备货核对(共 %d 个商品组;组 = 组合/拆分商品归并,库存/销量/进货已换算到组基准单位):\n",
		len(facts))
	for i, f := range facts {
		if i >= 20 {
			fmt.Fprintf(sb, "  ... 其余 %d 个商品组略\n", len(facts)-20)
			break
		}
		fmt.Fprintf(sb, "  - 商品组 %s:%s;%s", f.Label, groupPurchaseText(f), groupStockText(f))
		if f.CoverageDays.GreaterThan(decimal.Zero) {
			fmt.Fprintf(sb, ";补货后可售 %s 天", f.CoverageDays.StringFixed(1))
		}
		fmt.Fprintf(sb, ";一次进货下限 %s", qtyUnitText(f.FloorQty, f.BaseUnit))
		if f.Unconvertible {
			fmt.Fprintf(sb, ";单位说明:%s(该组本次未做数量比较)", f.UnitNote)
		}
		if text := zeroSalesCaseText(f.ZeroSalesCase); text != "" {
			fmt.Fprintf(sb, ";%s", text)
		}
		fmt.Fprintf(sb, "\n")
	}
}

// zeroSalesCaseText 把「组内 180 天无销量」的分类翻成事实描述（不做判断）。
func zeroSalesCaseText(c string) string {
	switch c {
	case "recent_receipt":
		return "组内近 180 天无销售记录(本次到货不足 7 天)"
	case "new_product":
		return "组内近 180 天无进货也无销售记录(首单)"
	case "long_stock":
		return "组内近 180 天无销售记录,且当前有库存、最后一次进货已超过 30 天"
	case "ledger_gap":
		return "组内近 180 天与近 30 天均无销售记录"
	}
	return ""
}

// marginTotalsSentence 生成引擎口径的**毛利合计句**(结论里必须出现的总和)。
//
// 两个口径分开写:窗口毛利回答"这批货整体赚不赚钱",实现毛利回答
// "这批新进的货按自己的进价卖赚不赚钱" —— 混成一句会让人误读。
func marginTotalsSentence(m MarginSummary) string {
	parts := make([]string, 0, 2)
	if m.GrossNegCount > 0 {
		parts = append(parts, fmt.Sprintf("窗口毛利为负 %d 个商品(合计 %s 元)",
			m.GrossNegCount, yuanText(m.GrossNegYuan)))
	}
	if m.RealizedNegCount > 0 {
		parts = append(parts, fmt.Sprintf("实现毛利为负 %d 个商品(合计 %s 元)",
			m.RealizedNegCount, yuanText(m.RealizedNegYuan)))
	}
	if len(parts) == 0 {
		return ""
	}
	return "毛利合计:" + strings.Join(parts, ",") + "。"
}

// ensureMarginTotals 保证结论里出现毛利合计(缺失才追加,不重复)。
//
// ⚠️ 只在**通过**结论上调用:拒绝结论的重点是阻断原因,不该被总账冲淡;
// 而且拒绝单不会填进「AI审核结论」控件(reject 走通知,不走 applyevent)。
func ensureMarginTotals(reason string, m MarginSummary) string {
	sentence := marginTotalsSentence(m)
	if sentence == "" {
		return reason
	}
	// 已带合计(模型自己写过) ⇒ 不重复追加。
	if strings.Contains(reason, "合计") {
		return reason
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return sentence
	}
	return reason + "\n" + sentence
}

// truncateAtSentence 按**句子边界**截断(中文安全),避免结论在句子中间断掉。
//
// 2026-10-30 用户口径:「避免语义割裂」—— 旧实现按 rune 硬切(200 字),
// 会把「这批货按自己的进价卖出去…」切成半句。现在优先找 ≤max 的最后一个
// 句末标点(。！？;;)或换行;找不到才退回硬截断(并补省略号)。
func truncateAtSentence(s string, max int) string {
	s = strings.TrimSpace(s)
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	// 在 [0,max] 内从后往前找句末标点,且不能切得太短(至少保留 60%)。
	cut := -1
	for i := max - 1; i >= max*6/10 && i >= 0; i-- {
		switch rs[i] {
		case '。', '！', '？', ';', '；', '\n':
			cut = i + 1
		}
		if cut >= 0 {
			break
		}
	}
	if cut > 0 {
		return strings.TrimSpace(string(rs[:cut]))
	}
	return strings.TrimSpace(string(rs[:max])) + "…"
}
