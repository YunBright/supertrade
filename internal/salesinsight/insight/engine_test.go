package insight

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/YunBright/supertrade/internal/salesinsight/calendar"
	"github.com/YunBright/supertrade/internal/salesinsight/fact"
)

// sampleFact 造一份覆盖各种状态的事实包。
func sampleFact() fact.Fact {
	return fact.Fact{
		Meta: fact.Meta{
			PeriodKind: "daily", PeriodLabel: "日报",
			From: "2026-10-15", To: "2026-10-15",
			PrevFrom: "2026-10-14", PrevTo: "2026-10-14",
			BranchID: "00_1000003", GeneratedAt: "2026-10-16T10:45:00+08:00",
			CalendarDataVersion: calendar.DataVersion,
		},
		Context: fact.Context{
			From:        calendar.Day{Date: "2026-10-15", Weekday: "周四", Kind: calendar.KindWorkday},
			PrevFrom:    calendar.Day{Date: "2026-10-14", Weekday: "周三", Kind: calendar.KindWorkday},
			Comparability: calendar.Comparability{
				Kind: calendar.ComparableGood, Reason: "本期与上期均为同类日期",
			},
		},
		Totals: fact.Totals{
			NetAmount: 15929.65, NetQty: 1731.93,
			CostCoverage: 0.253, MarginAvailable: false,
		},
		Deltas: fact.Deltas{
			PrevNetAmount: 20000, AmountAbs: -4070.35,
			AmountPct: -20.35, HasPrev: true,
		},
		Categories: []fact.Category{
			{Name: "生鲜联营", Amount: 8000, AmountShare: 0.5, Rank: 1,
				PrevAmount: 7000, DeltaPct: 14.3, HasPrev: true},
			{Name: "百货联营", Amount: 5000, AmountShare: 0.31, Rank: 2,
				PrevAmount: 6000, DeltaPct: -16.7, HasPrev: true},
		},
		TopProducts: []fact.TopProduct{
			{ItemNo: "A", Name: "西红柿", Amount: 300, Qty: 30, Rank: 1,
				PrevAmount: 400, DeltaPct: -25, HasPrev: true,
				StockStatus: fact.StockOut, HasStock: true, CurrentStock: 0, MinStock: 12,
				DeclineCause: fact.CauseStockout,
				DeclineEvidence: "已断货：当前库存 0；上期售出 400.00 元"},
		},
		Stockouts: []fact.TopProduct{
			{ItemNo: "A", Name: "西红柿", Amount: 300, PrevAmount: 400,
				StockStatus: fact.StockOut,
				DeclineEvidence: "已断货：当前库存 0；上期售出 400.00 元"},
		},
		Suppliers: []fact.Supplier{
			{Code: "038", Name: "岑溪市泰鼎粮油食品有限公司",
				Amount: 3000, ProductCount: 5},
		},
		ExcludedSuppliers: []fact.ExcludedSupplier{
			{Code: "019", Name: "自购蔬菜", Reason: "名称含自采标识（自购），属门店自营采购单元",
				Rule: string(fact.ExcludeNameKeyword), Amount: 5000},
		},
		RuleInsights: fact.RuleInsights{
			StockoutProducts:   []string{"西红柿"},
			GrowingCategories:  []string{"生鲜联营"},
			DecliningCategories: []string{"百货联营"},
			FastMovers:         []string{"土豆"},
		},
		DataSufficiency: fact.Sufficiency{
			Sufficient: false,
			Issues:     []string{"成本数据覆盖率仅 25.3%，毛利不可算"},
		},
	}
}

// 正常路径：解析成功即采信，并打上版本号。
func TestAnalyzeSuccessfulLLMPath(t *testing.T) {
	called := 0
	e := NewEngine(true, 0, func(ctx context.Context, sys, user string) (string, int, int, error) {
		called++
		if !strings.Contains(sys, "不要重新计算") {
			t.Error("system prompt 必须包含禁止改数字的硬约束")
		}
		return `{"summary":"西红柿缺货拖累生鲜","highlights":["西红柿缺货","生鲜联营增长"],"actions":["今日补货西红柿"],"warnings":[]}`, 0, 0, nil
	})

	in, audit := e.Analyze(context.Background(), AnalyzeInput{Fact: sampleFact()})

	if called != 1 {
		t.Fatalf("应调用 LLM 一次，实际 %d 次", called)
	}
	if in.GeneratedBy != ByLLM {
		t.Errorf("GeneratedBy = %q, 期望 %q", in.GeneratedBy, ByLLM)
	}
	if audit.Degraded {
		t.Error("不应降级")
	}
	if in.PromptVersion != PromptVersion {
		t.Errorf("PromptVersion = %q, 期望 %q", in.PromptVersion, PromptVersion)
	}
	if in.Summary != "西红柿缺货拖累生鲜" {
		t.Errorf("Summary 解析错误: %q", in.Summary)
	}
}

// ⚠️ prompt 必须携带"缺货不是滞销"的硬约束。
//
// 这是整个产品最重要的一条 prompt 约束：把缺货说成滞销会让店长
// 砍掉一个爆款。测试把它钉死，防止将来改写 prompt 时无意删掉。
func TestSystemPromptForbidsMislabelingStockout(t *testing.T) {
	for _, kw := range []string{
		"缺货",          // 必须点名这个场景
		"滞销",          // 必须点名不能说的词
		"不要重新计算",   // 铁律一
		"不要编造",      // 铁律二
		"JSON",          // 输出格式
	} {
		if !strings.Contains(SystemPrompt, kw) {
			t.Errorf("system prompt 缺少关键约束 %q", kw)
		}
	}
}

// 毛利不可算时，事实包必须明确告诉 LLM 别编毛利率。
func TestFactDigestTellsLLMMarginUnavailable(t *testing.T) {
	d := FactDigest(sampleFact())
	if !strings.Contains(d, "毛利率不可计算") && !strings.Contains(d, "请勿给出毛利率") {
		t.Fatalf("毛利不可算时 digest 必须明令禁止给出毛利率:\n%s", d)
	}
	// 且不能出现任何"参考毛利 X 元"（那会诱导模型引用不存在的数）
	if strings.Contains(d, "参考毛利 0.00") {
		t.Errorf("毛利不可算时不应出现参考毛利数字:\n%s", d)
	}
}

// digest 必须把缺货商品单独标出来，并带上判定证据。
func TestFactDigestHighlightsStockoutWithEvidence(t *testing.T) {
	d := FactDigest(sampleFact())
	if !strings.Contains(d, "需要补货的商品") {
		t.Fatalf("digest 应包含补货清单:\n%s", d)
	}
	if !strings.Contains(d, "西红柿") || !strings.Contains(d, "已断货") {
		t.Fatalf("补货项应带判定证据:\n%s", d)
	}
}

// 自用供应商的排除信息必须出现在 digest 里。
//
// ⚠️ 理由：若不告诉 LLM 存在自采渠道，它会把"乐园公司直供的生鲜"
// 理解成"某供应商供货能力强"，产出错误的供应商结论。
func TestFactDigestCarriesExcludedSuppliers(t *testing.T) {
	d := FactDigest(sampleFact())
	if !strings.Contains(d, "自购蔬菜") || !strings.Contains(d, "已排除的渠道") {
		t.Fatalf("digest 必须告知被排除的自采渠道:\n%s", d)
	}
}

// ⚠️ 没有上期摘要时，必须**明确告诉模型**不要编造上期情况。
//
// 否则模型看到环比数字就会自己编一个"上期下滑"的叙事。
func TestBuildPromptWithoutPrevSummarySaysSoExplicitly(t *testing.T) {
	p := BuildUserPrompt(sampleFact(), nil)
	if !strings.Contains(p, "没有上一期的结论记录") {
		t.Fatalf("缺上期摘要时必须明说:\n%s", p)
	}
	if !strings.Contains(p, "不要编造上期情况") && !strings.Contains(p, "请勿编造上期情况") {
		t.Fatalf("缺上期摘要时必须禁止编造:\n%s", p)
	}
}

func TestBuildPromptWithPrevSummaryInjectsIt(t *testing.T) {
	prev := FromInsight("daily", "2026-10-14", "2026-10-14", Insight{
		Summary:    "西红柿缺货",
		Highlights: []string{"百货联营下滑", "土豆增长"},
		Actions:    []string{"补货西红柿"}, // 不应被注入
		Warnings:   []string{"成本不足"},     // 不应被注入
	})
	p := BuildUserPrompt(sampleFact(), prev)
	if !strings.Contains(p, "西红柿缺货") {
		t.Fatalf("上期摘要应被注入:\n%s", p)
	}
	if strings.Contains(p, "补货西红柿") {
		t.Errorf("上期**动作**不应注入（本期特有，会导致重复建议）:\n%s", p)
	}
	if strings.Contains(p, "成本不足") {
		t.Errorf("上期**警告**不应注入（与本期数据无关，只增噪声）:\n%s", p)
	}
}

// ⚠️ 降级链：LLM 各种失败都必须产出可用的规则版报告。
//
// 这是本产品的可用性底线 —— "群里什么都没发生"比
// "收到一份规则版报告"难查得多。
func TestAnalyzeDegradesGracefully(t *testing.T) {
	cases := []struct {
		name    string
		engine  *Engine
		wantWhy Reason
	}{
		{
			name:    "显式关闭",
			engine:  NewEngine(false, 0, nil),
			wantWhy: ReasonDisabled,
		},
		{
			name:    "无 dapr client",
			engine:  NewEngine(true, 0, nil),
			wantWhy: ReasonNoClient,
		},
		{
			name: "超时",
			engine: NewEngine(true, 0, func(ctx context.Context, s, u string) (string, int, int, error) {
				return "", 0, 0, context.DeadlineExceeded
			}),
			wantWhy: ReasonTimeout,
		},
		{
			name: "调用失败",
			engine: NewEngine(true, 0, func(ctx context.Context, s, u string) (string, int, int, error) {
				return "", 0, 0, errors.New("boom")
			}),
			wantWhy: ReasonCallError,
		},
		{
			name: "返回空",
			engine: NewEngine(true, 0, func(ctx context.Context, s, u string) (string, int, int, error) {
				return "   ", 0, 0, nil
			}),
			wantWhy: ReasonEmptyOutput,
		},
		{
			name: "返回非 JSON",
			engine: NewEngine(true, 0, func(ctx context.Context, s, u string) (string, int, int, error) {
				return "抱歉，我无法完成这个任务", 0, 0, nil
			}),
			wantWhy: ReasonParseFailed,
		},
		{
			name: "返回空对象",
			engine: NewEngine(true, 0, func(ctx context.Context, s, u string) (string, int, int, error) {
				return `{"summary":"","highlights":[],"actions":[]}`, 0, 0, nil
			}),
			wantWhy: ReasonParseFailed,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in, audit := c.engine.Analyze(context.Background(), AnalyzeInput{Fact: sampleFact()})
			if in.GeneratedBy != ByRules {
				t.Errorf("GeneratedBy = %q, 期望 %q", in.GeneratedBy, ByRules)
			}
			if !audit.Degraded {
				t.Error("审计必须标记已降级")
			}
			if audit.DegradedReason != string(c.wantWhy) {
				t.Errorf("降级原因 = %q, 期望 %q", audit.DegradedReason, c.wantWhy)
			}
			// 无论怎么失败，都必须有内容可发。
			if strings.TrimSpace(in.Summary) == "" {
				t.Errorf("降级后仍必须有一句话总结，实际为空")
			}
			// 且必须让店长知道这是规则版。
			if !strings.Contains(strings.Join(in.Warnings, ";"), "AI 解读不可用") {
				t.Errorf("降级后必须在警告里说明, 实际 %v", in.Warnings)
			}
		})
	}
}

// ⚠️ prompt 超长时**不调用** LLM —— 那是一次注定超预算的付费调用。
func TestAnalyzeSkipsLLMWhenPromptTooLong(t *testing.T) {
	called := 0
	e := NewEngine(true, 0, func(ctx context.Context, s, u string) (string, int, int, error) {
		called++
		return `{"summary":"x"}`, 0, 0, nil
	})
	f := sampleFact()
	// 塞一个超长商品名
	f.TopProducts[0].Name = strings.Repeat("长", MaxPromptChars+10)

	in, audit := e.Analyze(context.Background(), AnalyzeInput{Fact: f})
	if called != 0 {
		t.Fatalf("prompt 超长时不应调用 LLM，实际调用了 %d 次", called)
	}
	if in.GeneratedBy != ByRules {
		t.Errorf("应降级为规则版")
	}
	if !strings.Contains(audit.DegradedReason, "prompt 长度") {
		t.Errorf("降级原因应指出 prompt 超长，实际 %q", audit.DegradedReason)
	}
}

// LLM 返回裹了 markdown / 有前后废话时仍要能解析。
func TestParseInsightTolerantFormats(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"裸 JSON", `{"summary":"今天不错"}`, "今天不错"},
		{"markdown 包裹", "```json\n{\"summary\":\"今天不错\"}\n```", "今天不错"},
		{"markdown 无语言标记", "```\n{\"summary\":\"今天不错\"}\n```", "今天不错"},
		{"前后有废话", "好的，分析如下：\n{\"summary\":\"今天不错\"}\n希望对你有帮助！", "今天不错"},
		{"带空格", "  {\"summary\" : \"今天不错\" }  ", "今天不错"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseInsight(c.raw)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if got.Summary != c.want {
				t.Errorf("Summary = %q, 期望 %q", got.Summary, c.want)
			}
		})
	}
}

// 裁剪必须在解析后执行，且不能把 Summary 裁没。
func TestSanitizeKeepsSummaryButTrimsLists(t *testing.T) {
	in := Insight{
		Summary: strings.Repeat("总", MaxSummaryChars*2),
		Highlights: []string{
			strings.Repeat("亮", MaxHighlightChars*2),
			strings.Repeat("点", MaxHighlightChars*2),
			"", "  ", // 空项应被丢掉
			"第三条",
			"第四条", "第五条", "第六条", // 超出条数上限
		},
	}
	in.Sanitize()
	if in.Summary == "" {
		t.Fatal("Summary 不能被裁成空 —— 它承载核心判断")
	}
	if len([]rune(in.Summary)) > MaxSummaryChars {
		t.Errorf("Summary 应被裁到 %d 字符，实际 %d", MaxSummaryChars, len([]rune(in.Summary)))
	}
	if len(in.Highlights) > MaxItems {
		t.Errorf("要点条数应 ≤ %d，实际 %d", MaxItems, len(in.Highlights))
	}
	for _, h := range in.Highlights {
		if strings.TrimSpace(h) == "" {
			t.Errorf("空项不应保留: %v", in.Highlights)
		}
	}
}

// 总长闸：各字段各自合规，但合计超限时仍要裁。
func TestSanitizeEnforcesTotalLimit(t *testing.T) {
	in := Insight{
		Summary: strings.Repeat("总", MaxSummaryChars),
		Actions: []string{
			strings.Repeat("做", MaxItemChars),
			strings.Repeat("办", MaxItemChars),
			strings.Repeat("行", MaxItemChars),
			strings.Repeat("动", MaxItemChars),
		},
	}
	in.Sanitize()
	total := len([]rune(in.Summary)) + listLen(in.Actions)
	if total > MaxTotalChars {
		t.Fatalf("总长 %d 应被裁到 ≤ %d", total, MaxTotalChars)
	}
}

// 审计里必须保留原始输出 —— 模型升级后解析失败时它是唯一证据。
func TestAuditKeepsRawOutputOnParseFailure(t *testing.T) {
	e := NewEngine(true, 0, func(ctx context.Context, s, u string) (string, int, int, error) {
		return "The model responded with plain text, not JSON", 0, 0, nil
	})
	_, audit := e.Analyze(context.Background(), AnalyzeInput{Fact: sampleFact()})
	if audit.RawOutput == "" {
		t.Fatal("解析失败时必须保留原始输出")
	}
	if !strings.Contains(audit.RawOutput, "plain text") {
		t.Errorf("原始输出应逐字保留，实际 %q", audit.RawOutput)
	}
	if audit.Error == "" {
		t.Error("应记录解析错误原因")
	}
}

// 审计必须带上期摘要，否则后台看到的"上下文"是不完整的。
func TestAuditContextIncludesPrevSummary(t *testing.T) {
	e := NewEngine(true, 0, func(ctx context.Context, s, u string) (string, int, int, error) {
		return `{"summary":"x"}`, 0, 0, nil
	})
	prev := FromInsight("daily", "2026-10-14", "2026-10-14", Insight{Summary: "上期缺货"})
	_, audit := e.Analyze(context.Background(), AnalyzeInput{Fact: sampleFact(), PrevSummary: prev})
	if !strings.Contains(audit.Context, "prev_summary") {
		t.Errorf("审计上下文应包含上期摘要，否则无法还原模型看到什么:\n%s", audit.Context)
	}
}

// 规则版结论必须优先给出"补货"动作。
func TestRuleInsightsPrioritizeStockoutActions(t *testing.T) {
	in := RuleInsightsFromFact(sampleFact(), "")
	if !strings.Contains(in.Summary, "缺货") {
		t.Errorf("规则版摘要应点明缺货，实际 %q", in.Summary)
	}
	found := false
	for _, a := range in.Actions {
		if strings.Contains(a, "补货") && strings.Contains(a, "西红柿") {
			found = true
		}
	}
	if !found {
		t.Errorf("规则版必须给出补货动作，实际 %v", in.Actions)
	}
}

// 可比性受限时，规则版必须警告且不建议下架 —— 否则店长会砍品。
func TestRuleInsightsWarnOnLimitedComparability(t *testing.T) {
	f := sampleFact()
	f.Context.Comparability = calendar.Comparability{
		Kind:   calendar.ComparableLimited,
		Reason: "本期 5 天、上期 7 天，天数不同",
	}
	in := RuleInsightsFromFact(f, "")
	if !strings.Contains(in.Summary, "不可直接比较") {
		t.Errorf("摘要应提示可比性受限，实际 %q", in.Summary)
	}
	found := false
	for _, a := range in.Actions {
		if strings.Contains(a, "暂缓据此下架") {
			found = true
		}
	}
	if !found {
		t.Errorf("可比性受限时不应建议下架，实际 %v", in.Actions)
	}
}

// 没有上期数据时不做环比叙述。
func TestRuleInsightsWithoutPrevDoNotMentionComparison(t *testing.T) {
	f := sampleFact()
	f.Deltas = fact.Deltas{HasPrev: false, PrevNetAmount: 0, AmountPct: 0}
	in := RuleInsightsFromFact(f, "")
	if strings.Contains(in.Summary, "环比") {
		t.Errorf("无上期数据时不应出现环比，实际 %q", in.Summary)
	}
}
