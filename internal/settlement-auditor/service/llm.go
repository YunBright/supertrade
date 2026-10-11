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
}

// VerdictResult 是 VerdictFn 的返回值。
type VerdictResult struct {
	Conclusion       string               `json:"conclusion"` // pass / reject
	Reason           string               `json:"reason"`
	Confidence       decimal.Decimal      `json:"confidence"`
	SuggestStocktake *StocktakeSuggestion `json:"suggest_stocktake,omitempty"`
	// RawJSON 原始输出(落 llm_verdicts.raw_json)。
	RawJSON []byte `json:"-"`
	// Source = llm / rule_fallback。
	Source string `json:"-"`
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
			"conclusion":       ConclusionReject,
			"reason":           reason,
			"confidence":       1,
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
		"conclusion":       ConclusionPass,
		"reason":           reason,
		"confidence":       0.9,
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
func buildAuditSystemPrompt() string {
	return `你是商场采购结算付款审核员。给你一次付款申请的数据:勾选结算单、
每张结算单拆出的采购进货(PI)/退货(RO)单、采购单头、规则核对引擎已发现的信号、
以及窗口期商品销售汇总。你要判断这次付款是否应当放行进入人工终审。

判断要点:
- 规则引擎已标 block 的信号(单据缺失/未审核/金额勾稽不上/无销售进货/数量严重偏离)
  通常应 reject,并在 reason 中引用最关键的几条;
- 只有 warn 信号时,结合销售/供应商/商品常识判断是否人为失误或套利嫌疑,
  无明显问题则 pass,把 warn 摘要写进 reason 供人工终审关注;
- 若发现某类商品/某供应商/个别商品数据可疑,建议针对性盘点:
  suggest_stocktake.scope 取 category|supplier|product,ids 填商品/分类/供应商 id 列表;
  无建议则省略该字段或置 null。

只输出 JSON,不要 markdown,不要解释:
{"conclusion":"pass|reject","reason":"中文,200字内","confidence":0.0~1.0,
"suggest_stocktake":{"scope":"category|supplier|product","ids":["..."]}}`
}

// buildAuditUserPrompt 拼数据快照(控制长度:信号最多 20 条,结算单最多 20 张)。
func buildAuditUserPrompt(in *AuditInput) string {
	var sb strings.Builder
	ap := in.Approval
	fmt.Fprintf(&sb, "付款申请:approval_id=%s, 手工累加付款金额=%s 元, 备注=%s\n",
		ap.ID, ap.ManualAmountYuan.StringFixed(2), ap.Remark)

	fmt.Fprintf(&sb, "勾选结算单 %d 张:\n", len(in.Settlements))
	for i, st := range in.Settlements {
		if i >= 20 {
			fmt.Fprintf(&sb, "  ... 其余 %d 张略\n", len(in.Settlements)-20)
			break
		}
		fmt.Fprintf(&sb, "  - %s 供应商=%s 类型=%s 货款=%s 账期=%s~%s\n",
			st.ID, st.SupplierID, st.DocumentType, st.TotalGoodsYuan.StringFixed(2), st.PeriodStart, st.PeriodEnd)
		if lns := in.Lines[st.ID]; len(lns) > 0 {
			for j, ln := range lns {
				if j >= 10 {
					fmt.Fprintf(&sb, "    ... 其余 %d 行略\n", len(lns)-10)
					break
				}
				fmt.Fprintf(&sb, "    · %s %s 货款=%s\n", ln.SourceDocType, ln.VoucherID, ln.TotalGoodsYuan.StringFixed(2))
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
