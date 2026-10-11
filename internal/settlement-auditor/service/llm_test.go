package service

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

func TestParseVerdictJSON_OK(t *testing.T) {
	raw := `{"conclusion":"pass","reason":"数据一致","confidence":0.8,"suggest_stocktake":{"scope":"product","ids":["P1","P2"]}}`
	v, err := parseVerdictJSON([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v.Conclusion != ConclusionPass {
		t.Errorf("conclusion = %s, want pass", v.Conclusion)
	}
	if !v.Confidence.Equal(decimal.NewFromFloat(0.8)) {
		t.Errorf("confidence = %s, want 0.8", v.Confidence)
	}
	if v.Source != model.VerdictSourceLLM {
		t.Errorf("source = %s, want llm", v.Source)
	}
	if v.SuggestStocktake == nil || v.SuggestStocktake.Scope != "product" || len(v.SuggestStocktake.IDs) != 2 {
		t.Errorf("suggest_stocktake 解析错误: %+v", v.SuggestStocktake)
	}
	if string(v.RawJSON) != raw {
		t.Errorf("RawJSON 应保留原文")
	}
}

func TestParseVerdictJSON_RejectNoSuggest(t *testing.T) {
	v, err := parseVerdictJSON([]byte(`{"conclusion":"reject","reason":"金额勾稽不上","confidence":0.95}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v.Conclusion != ConclusionReject {
		t.Errorf("conclusion = %s, want reject", v.Conclusion)
	}
	if v.SuggestStocktake != nil {
		t.Errorf("无建议时 SuggestStocktake 应为 nil, got %+v", v.SuggestStocktake)
	}
}

func TestParseVerdictJSON_InvalidConclusion(t *testing.T) {
	if _, err := parseVerdictJSON([]byte(`{"conclusion":"maybe","reason":"x"}`)); err == nil {
		t.Error("非法 conclusion 应报错")
	}
}

func TestParseVerdictJSON_EmptyReason(t *testing.T) {
	if _, err := parseVerdictJSON([]byte(`{"conclusion":"pass"}`)); err == nil {
		t.Error("空 reason 应报错")
	}
}

func TestParseVerdictJSON_BadJSON(t *testing.T) {
	if _, err := parseVerdictJSON([]byte(`{not json`)); err == nil {
		t.Error("坏 JSON 应报错")
	}
}

func TestParseVerdictJSON_ConfidenceClamped(t *testing.T) {
	v, err := parseVerdictJSON([]byte(`{"conclusion":"pass","reason":"ok","confidence":1.5}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !v.Confidence.Equal(decimal.NewFromFloat(0.5)) {
		t.Errorf("越界 confidence 应回落 0.5, got %s", v.Confidence)
	}
}

func TestParseVerdictJSON_SuggestSanitized(t *testing.T) {
	// scope 非法 → 建议整体丢弃
	v, err := parseVerdictJSON([]byte(`{"conclusion":"pass","reason":"ok","confidence":0.5,"suggest_stocktake":{"scope":"weird","ids":["x"]}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v.SuggestStocktake != nil {
		t.Errorf("非法 scope 应丢弃建议, got %+v", v.SuggestStocktake)
	}
	// ids 空 → 建议整体丢弃
	v, err = parseVerdictJSON([]byte(`{"conclusion":"pass","reason":"ok","confidence":0.5,"suggest_stocktake":{"scope":"supplier","ids":[]}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v.SuggestStocktake != nil {
		t.Errorf("空 ids 应丢弃建议, got %+v", v.SuggestStocktake)
	}
}

func TestStripCodeFence(t *testing.T) {
	cases := []struct{ in, want string }{
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
		{"```{\"a\":1}```", `{"a":1}`},
		{`{"a":1}`, `{"a":1}`},
	}
	for _, c := range cases {
		if got := stripCodeFence(c.in); got != c.want {
			t.Errorf("stripCodeFence(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDefaultVerdictFn_BlockReject(t *testing.T) {
	in := &AuditInput{Signals: []signalDraft{
		{Level: model.SignalWarn, Message: "警告1"},
		{Level: model.SignalBlock, Message: "阻断1"},
		{Level: model.SignalBlock, Message: "阻断2"},
	}}
	v, err := defaultVerdictFn(t.Context(), in)
	if err != nil {
		t.Fatalf("defaultVerdictFn: %v", err)
	}
	if v.Conclusion != ConclusionReject {
		t.Errorf("conclusion = %s, want reject", v.Conclusion)
	}
	if !strings.Contains(v.Reason, "阻断1") || !strings.Contains(v.Reason, "阻断2") {
		t.Errorf("reason 应拼接 block 消息, got %q", v.Reason)
	}
	if v.Source != model.VerdictSourceRuleFallback {
		t.Errorf("source = %s, want rule_fallback", v.Source)
	}
	if len(v.RawJSON) == 0 {
		t.Error("RawJSON 应非空")
	}
}

func TestDefaultVerdictFn_WarnOnlyPass(t *testing.T) {
	in := &AuditInput{Signals: []signalDraft{{Level: model.SignalWarn, Message: "警告1"}}}
	v, err := defaultVerdictFn(t.Context(), in)
	if err != nil {
		t.Fatalf("defaultVerdictFn: %v", err)
	}
	if v.Conclusion != ConclusionPass {
		t.Errorf("conclusion = %s, want pass", v.Conclusion)
	}
	if !strings.Contains(v.Reason, "1 条警告") {
		t.Errorf("reason 应含警告计数, got %q", v.Reason)
	}
}

func TestDefaultVerdictFn_NoSignalsPass(t *testing.T) {
	v, err := defaultVerdictFn(t.Context(), &AuditInput{})
	if err != nil {
		t.Fatalf("defaultVerdictFn: %v", err)
	}
	if v.Conclusion != ConclusionPass || !strings.Contains(v.Reason, "无阻断信号") {
		t.Errorf("无信号应 pass, got %s/%q", v.Conclusion, v.Reason)
	}
}

func TestDefaultVerdictFn_NilInput(t *testing.T) {
	if _, err := defaultVerdictFn(t.Context(), nil); err == nil {
		t.Error("nil 输入应报错")
	}
}

// 数量类异常信号必须在规则降级路径派生 product 盘点建议(LLM 不可用时闭环不断)。
func TestStocktakeSuggestionFromSignals(t *testing.T) {
	cases := []struct {
		name    string
		signals []signalDraft
		wantIDs []string
	}{
		{
			name:    "无信号",
			signals: nil,
			wantIDs: nil,
		},
		{
			name: "非数量类 block 不产建议(单据缺失/金额勾稽盘点解决不了)",
			signals: []signalDraft{
				{Level: model.SignalBlock, SignalType: SigSheetMissing, ProductID: "PR1", Message: "m"},
				{Level: model.SignalBlock, SignalType: SigLineAmountMismatch, ProductID: "PR2", Message: "m"},
			},
			wantIDs: nil,
		},
		{
			name: "无销售进货 block + 数量偏离 warn,去重保序",
			signals: []signalDraft{
				{Level: model.SignalBlock, SignalType: SigPurchaseWithoutSales, ProductID: "PRA", Message: "m"},
				{Level: model.SignalWarn, SignalType: SigQtyDeviation, ProductID: "PRB", Message: "m"},
				{Level: model.SignalBlock, SignalType: SigQtyDeviation, ProductID: "PRA", Message: "m2"},
				{Level: model.SignalBlock, SignalType: SigQtyDeviation, ProductID: "", Message: "无商品 id 忽略"},
			},
			wantIDs: []string{"PRA", "PRB"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stocktakeSuggestionFromSignals(tc.signals)
			if tc.wantIDs == nil {
				if got != nil {
					t.Fatalf("应无建议, got %+v", got)
				}
				return
			}
			if got == nil || got.Scope != "product" {
				t.Fatalf("应产 product 建议, got %+v", got)
			}
			if len(got.IDs) != len(tc.wantIDs) {
				t.Fatalf("ids = %v, want %v", got.IDs, tc.wantIDs)
			}
			for i, id := range tc.wantIDs {
				if got.IDs[i] != id {
					t.Fatalf("ids = %v, want %v(保序去重)", got.IDs, tc.wantIDs)
				}
			}
		})
	}
}

// reject 与 warn-only pass 两条路径都要把建议挂到 verdict 上。
func TestDefaultVerdictFn_StocktakeSuggestionAttached(t *testing.T) {
	reject, err := defaultVerdictFn(t.Context(), &AuditInput{Signals: []signalDraft{
		{Level: model.SignalBlock, SignalType: SigPurchaseWithoutSales, ProductID: "PR1", Message: "无销售进货"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if reject.Conclusion != ConclusionReject || reject.SuggestStocktake == nil ||
		len(reject.SuggestStocktake.IDs) != 1 || reject.SuggestStocktake.IDs[0] != "PR1" {
		t.Errorf("reject 路径应带 PR1 建议: %+v", reject.SuggestStocktake)
	}

	pass, err := defaultVerdictFn(t.Context(), &AuditInput{Signals: []signalDraft{
		{Level: model.SignalWarn, SignalType: SigQtyDeviation, ProductID: "PR2", Message: "偏离"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if pass.Conclusion != ConclusionPass || pass.SuggestStocktake == nil ||
		len(pass.SuggestStocktake.IDs) != 1 || pass.SuggestStocktake.IDs[0] != "PR2" {
		t.Errorf("warn pass 路径应带 PR2 建议: %+v", pass.SuggestStocktake)
	}
}

func TestBuildAuditUserPrompt_Truncates(t *testing.T) {
	// 构造超量数据验证截断保护(不 panic 且长度受控)
	ap := &model.ApprovalInstance{ID: "ap-1", Remark: "r"}
	var settlements []SettlementRow
	for i := 0; i < 30; i++ {
		settlements = append(settlements, SettlementRow{ID: strings.Repeat("S", 3) + string(rune('0'+i%10))})
	}
	var signals []signalDraft
	for i := 0; i < 30; i++ {
		signals = append(signals, signalDraft{Level: model.SignalWarn, SignalType: "x", Message: "m"})
	}
	out := buildAuditUserPrompt(&AuditInput{Approval: ap, Settlements: settlements, Signals: signals})
	if !strings.Contains(out, "其余 10 张略") {
		t.Error("结算单超 20 应截断提示")
	}
	if !strings.Contains(out, "其余 10 条略") {
		t.Error("信号超 20 应截断提示")
	}
}
