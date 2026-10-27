package insight

import (
	"context"
	"strings"
	"testing"

	"github.com/YunBright/supertrade/internal/salesinsight/fact"
)

// storeFact 复刻 2026-10-23 生产现场那份周报的事实包：
// 全店销售额 108626.17、上期 118653.51、总毛利 16927.17，
// 品类里有蛋类 4162.4、方便食品 456.4。
func storeFact() fact.Fact {
	f := sampleFact()
	f.Totals = fact.Totals{
		NetAmount: 108626.17, NetQty: 20000,
		RefCost: 91700, RefMargin: 16927.17, RefMarginPct: 0.1558,
		CostCoverage: 1, MarginAvailable: true,
	}
	f.Deltas = fact.Deltas{
		PrevNetAmount: 118653.51, AmountAbs: -10027.34,
		AmountPct: -8.5, HasPrev: true,
	}
	f.Categories = []fact.Category{
		{Name: "蛋类", Amount: 4162.4, AmountShare: 0.0618, Rank: 1,
			PrevAmount: 4534.1, DeltaPct: -8.2, HasPrev: true},
		{Name: "方便食品", Amount: 456.4, AmountShare: 0.0042, Rank: 2,
			PrevAmount: 98.1, DeltaPct: 367.6, HasPrev: true},
	}
	f.TopProducts = []fact.TopProduct{}
	f.Stockouts = []fact.TopProduct{}
	f.Suppliers = []fact.Supplier{}
	f.ExcludedSuppliers = []fact.ExcludedSupplier{}
	f.RuleInsights = fact.RuleInsights{}
	return f
}

// ⚠️ 事实包**不得**下发全店销售总额与总毛利的绝对金额。
//
// 这是整个口径的第一道防线：模型看不到的数字，它写不出来。
// 生产实测（2026-10-23）周报正文出现
// 「销售额108626.17元」「参考毛利16927.17元」——
// 根因就是这两个数**在 prompt 里**。
func TestFactDigestHasNoStoreTotals(t *testing.T) {
	d := FactDigest(storeFact())
	for _, banned := range []string{
		"108626.17", "118653.51", "16927.17", // 精确值
		"108626", "118653", "16927", // 取整形态
	} {
		if strings.Contains(d, banned) {
			t.Errorf("digest 不应包含全店金额 %q:\n%s", banned, d)
		}
	}
	// 百分比必须**保留** —— 它才是店长要的东西。
	//
	// ⚠️ 措辞跟着对比基准走（2026-10-26）：
	//   日报的对比基准是"上周同一天"，说"环比"会让店长以为比的是昨天。
	if !strings.Contains(d, "-8.5%") {
		t.Errorf("digest 必须保留销售额对比百分比:\n%s", d)
	}
	if !strings.Contains(d, "上周同期") {
		t.Errorf("digest 必须点明对比基准的口径（上周同期）:\n%s", d)
	}
	if !strings.Contains(d, "参考毛利率 15.6%") {
		t.Errorf("digest 必须保留毛利率百分比:\n%s", d)
	}
}

// ⚠️ system prompt 必须写明这条口径 —— 否则模型没有理由不写。
func TestSystemPromptForbidsStoreTotals(t *testing.T) {
	for _, kw := range []string{
		"不要报告全店销售总额",
		"百分比",
		"细分项",
		"细分项相加",
	} {
		if !strings.Contains(SystemPrompt, kw) {
			t.Errorf("system prompt 缺少关键约束 %q", kw)
		}
	}
}

// ⚠️ 清洗闸：抹掉全店总额，保留百分比与小类目金额。
func TestRedactStoreTotalsRemovesStoreTotalsOnly(t *testing.T) {
	in := Insight{
		Summary: "销售额108626.17元，环比-8.5%，方便食品增367.6%是唯一增长品类。",
		Highlights: []string{
			"参考毛利16927.17元，毛利率15.6%。",
			"蛋类4162.4元居首，占品类内61.8%，环比-8.2%。",
			"方便食品456.4元，环比+367.6%。",
		},
	}
	RedactStoreTotals(storeFact(), &in)

	if strings.Contains(in.Summary, "108626.17") {
		t.Errorf("全店销售额未被抹掉: %q", in.Summary)
	}
	// ⚠️ 抹掉金额时必须把单位"元"一起带走 ——
	//   "销售额—元"读起来像漏了半个数。
	if strings.Contains(in.Summary, "元") {
		t.Errorf("抹除后不应残留单位: %q", in.Summary)
	}
	if strings.Contains(in.Highlights[0], "16927.17") {
		t.Errorf("总毛利未被抹掉: %q", in.Highlights[0])
	}
	if !strings.Contains(in.Highlights[0], "15.6%") {
		t.Errorf("毛利率百分比应保留: %q", in.Highlights[0])
	}
	if !strings.Contains(in.Highlights[1], "4162.4") {
		t.Errorf("小类目金额应保留: %q", in.Highlights[1])
	}
	if !strings.Contains(in.Highlights[2], "456.4") {
		t.Errorf("小类目金额应保留: %q", in.Highlights[2])
	}
	if !strings.Contains(in.Summary, "-8.5%") || !strings.Contains(in.Summary, "367.6%") {
		t.Errorf("百分比应保留: %q", in.Summary)
	}
}

// ⚠️ 兜底：上期遗留摘要里的**旧**总额也要抹掉。
//
// 它不在本期黑名单里（本期是 108626.17，那条是 60585.35），
// 靠的是「销售额 + 紧跟数字」这条词规则。
func TestRedactStoreTotalsRemovesLegacyTotal(t *testing.T) {
	in := Insight{Summary: "上期结论写销售额60585.35元，与本期引用的118653.51元不一致。"}
	RedactStoreTotals(storeFact(), &in)
	if strings.Contains(in.Summary, "60585.35") {
		t.Errorf("上期遗留总额未被抹掉: %q", in.Summary)
	}
	if strings.Contains(in.Summary, "118653.51") {
		t.Errorf("上期总额未被抹掉: %q", in.Summary)
	}
}

// ⚠️ 词规则不能误伤两样东西：小类目的销售额、毛利率。
func TestRedactStoreTotalsKeepsCategorySalesAndMarginPct(t *testing.T) {
	in := Insight{Summary: "蛋类销售额4162.4元居首，毛利率15.6%。"}
	RedactStoreTotals(storeFact(), &in)
	if !strings.Contains(in.Summary, "4162.4") {
		t.Errorf("小类目销售额应保留: %q", in.Summary)
	}
	if !strings.Contains(in.Summary, "15.6%") {
		t.Errorf("毛利率应保留: %q", in.Summary)
	}
}

// 日期不能被误伤：小门店的销售额可能恰好是 "2026"。
func TestRedactStoreTotalsKeepsDates(t *testing.T) {
	f := storeFact()
	f.Totals.NetAmount = 2026 // 极小门店：年份本身就是一个合法金额
	in := Insight{Summary: "统计区间 2026-10-15，环比-8.5%。"}
	RedactStoreTotals(f, &in)
	if !strings.Contains(in.Summary, "2026-10-15") {
		t.Errorf("日期不应被抹掉: %q", in.Summary)
	}
}

// ⚠️ 百分比即使数值碰巧等于某个全店金额，也不能被抹掉。
func TestRedactStoreTotalsKeepsPercentMatchingAmount(t *testing.T) {
	f := storeFact()
	f.Totals.NetAmount = 15.6 // 极小门店
	in := Insight{Summary: "蛋类占比15.6%，环比-8.5%。"}
	RedactStoreTotals(f, &in)
	if !strings.Contains(in.Summary, "15.6%") {
		t.Errorf("百分比不应被抹掉: %q", in.Summary)
	}
}

// ⚠️ "总销售额"这类**永远**指全店的词，优先级高于细分项白名单。
//
// 模型说"总销售额4162.4元"时，哪怕 4162.4 恰好是"蛋类"的金额，
// 这句话**仍然在报全店总额**（而且报错了）。
func TestRedactStoreTotalsAlwaysTermWins(t *testing.T) {
	in := Insight{Summary: "总销售额4162.4元，蛋类4162.4元居首。"}
	RedactStoreTotals(storeFact(), &in)
	if strings.Contains(in.Summary, "总销售额4162.4") {
		t.Errorf("全店口径的金额应被抹掉: %q", in.Summary)
	}
	if !strings.Contains(in.Summary, "蛋类4162.4") {
		t.Errorf("小类目金额应保留: %q", in.Summary)
	}
}

// ⚠️ 规则降级路径（LLM 挂掉时走的那条）同样不许出现全店总额。
//
// 降级不是"放松版"：那张卡片照样要守同一条口径。
//
// ⚠️ 这里断言的是**逐字**的摘要，而不是"里面没有那个数字"：
// 只查数字的话，"规则版写了总额、清洗闸事后抹掉"也会算通过 ——
// 那样就钉不住"规则版自己就不该写"这一层。
func TestRuleInsightsHaveNoStoreTotal(t *testing.T) {
	in := RuleInsightsFromFact(storeFact(), "")
	if in.Summary != "销售额环比 -8.5%" {
		t.Errorf("规则版摘要 = %q, 期望 %q", in.Summary, "销售额环比 -8.5%")
	}
	if strings.Contains(in.Summary, "108626.17") {
		t.Errorf("规则版摘要不应出现全店销售额: %q", in.Summary)
	}
	if strings.Contains(in.Summary, "元") {
		t.Errorf("规则版摘要不应出现全店金额: %q", in.Summary)
	}
}

// ⚠️ LLM 路径也必须过清洗闸 —— prompt 里已经没有全店金额了，
// 但模型仍可能自己凑一个出来。
func TestAnalyzeRedactsStoreTotalsFromLLM(t *testing.T) {
	e := NewEngine(true, 0, func(ctx context.Context, c Conversation) (ConversationResult, error) {
		return ConversationResult{Content: `{"summary":"销售额108626.17元，环比-8.5%",` +
			`"highlights":["蛋类4162.4元居首"],"actions":[],"warnings":[]}`}, nil
	})
	in, _ := e.Analyze(context.Background(), AnalyzeInput{Fact: storeFact()})
	if strings.Contains(in.Summary, "108626.17") {
		t.Errorf("LLM 路径未过清洗闸: %q", in.Summary)
	}
	if !strings.Contains(in.Highlights[0], "4162.4") {
		t.Errorf("小类目金额应保留: %q", in.Highlights[0])
	}
}
