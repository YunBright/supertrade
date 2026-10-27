package insight

import (
	"context"
	"strings"
	"testing"
)

// ⚠️ **候选列表必须按区间挑，而不是按"最新"**。
//
// # 这条钉住 2026-10-27 的修复
//
// 上期摘要改成**按期次分段存**之后，调用方会把该门店该周期的
// **全部**上期摘要都传过来（每一期一份）。
// 若本包按"取最新的那份"来用，就会取到
// **白天手动生成**留下的那一期 —— 它的区间是**今天**，
// 而本期要的是**昨天**那份。
//
// 症状（原缺陷）：22:50 的定时日报突然"不接昨天的话了"，
// 而且**没有任何报错** —— 只是"没有上一期结论记录"。
//
// 判别力：把 PickPrevSummary 改成"取 candidates[0]"，
// 本用例立刻失败（prompt 里出现的是"今天的半成品"）。
func TestEnginePicksPrevSummaryByRangeNotByNewest(t *testing.T) {
	e := NewEngine(true, 0, func(ctx context.Context, c Conversation) (ConversationResult, error) {
		return ConversationResult{Content: `{"summary":"x"}`}, nil
	})
	// sampleFact 的「上一期报告」= 2026-10-14（昨天）。
	// 候选里"今天这一期"排在**前面**（它才是最近保存的）。
	candidates := []*PrevSummary{
		{From: "2026-10-15", To: "2026-10-15", Summary: "今天的半成品"},
		{From: "2026-10-14", To: "2026-10-14", Summary: "昨天的结论"},
	}

	_, audit := e.Analyze(context.Background(), AnalyzeInput{
		Fact:          sampleFact(),
		PrevSummaries: candidates,
	})
	if !strings.Contains(audit.Prompt, "昨天的结论") {
		t.Errorf("应注入区间匹配的那一份（昨天）:\n%s", audit.Prompt)
	}
	if strings.Contains(audit.Prompt, "今天的半成品") {
		t.Errorf("不得注入区间不匹配的那一份（今天）:\n%s", audit.Prompt)
	}
}

// ⚠️ 候选里**没有区间**的那一份不参与挑选。
//
// PrevSummaryMatches 对"没有区间"的摘要一律放行（老调用方的兼容语义），
// 但用在候选上就变成"任意一期都能认领它" ——
// 于是挑出来的是**别的期**的结论，比没有上期更糟。
func TestEngineIgnoresRangeLessPrevCandidate(t *testing.T) {
	e := NewEngine(true, 0, func(ctx context.Context, c Conversation) (ConversationResult, error) {
		return ConversationResult{Content: `{"summary":"x"}`}, nil
	})
	candidates := []*PrevSummary{
		{Summary: "没有区间的候选"},
		{From: "2026-10-14", To: "2026-10-14", Summary: "昨天的结论"},
	}

	_, audit := e.Analyze(context.Background(), AnalyzeInput{
		Fact:          sampleFact(),
		PrevSummaries: candidates,
	})
	if strings.Contains(audit.Prompt, "没有区间的候选") {
		t.Errorf("没有区间的候选不应被挑中:\n%s", audit.Prompt)
	}
	if !strings.Contains(audit.Prompt, "昨天的结论") {
		t.Errorf("应注入区间匹配的那一份:\n%s", audit.Prompt)
	}
}

// ⚠️ 有候选但一份都不匹配 ⇒ 当作"没有上期"，**不退回**兜底那份。
//
// 兜底那份是"最近保存的"，而它区间对不上正是要拒掉它的理由。
// 退回 = 把错的区间的结论塞回 prompt。
func TestEngineDoesNotFallBackWhenCandidatesMiss(t *testing.T) {
	e := NewEngine(true, 0, func(ctx context.Context, c Conversation) (ConversationResult, error) {
		return ConversationResult{Content: `{"summary":"x"}`}, nil
	})
	fallback := &PrevSummary{From: "2026-10-15", To: "2026-10-15", Summary: "今天的半成品"}
	candidates := []*PrevSummary{
		{From: "2026-10-13", To: "2026-10-13", Summary: "更早的结论"},
	}

	_, audit := e.Analyze(context.Background(), AnalyzeInput{
		Fact:          sampleFact(),
		PrevSummary:   fallback,
		PrevSummaries: candidates,
	})
	if strings.Contains(audit.Prompt, "今天的半成品") {
		t.Errorf("候选都不匹配时不得退回兜底那份:\n%s", audit.Prompt)
	}
}

// ⚠️ 区间对不上的上期摘要必须**整份丢弃**。
//
// 2026-10-23 生产实测：周报正文出现
// 「上期结论写销售额60585.35元，与本期引用的118653.51元不一致」。
// 真相是那份"上一期周报"是在**周期口径改动之前**生成的，
// 它与本期的"上期"根本不是同一段 —— 类型相同 ≠ 区间相同。
func TestEngineDropsMismatchedPrevSummary(t *testing.T) {
	e := NewEngine(true, 0, func(ctx context.Context, c Conversation) (ConversationResult, error) {
		if strings.Contains(c.Messages[0].Content, "旧周期结论") {
			t.Errorf("区间不匹配的上期摘要不应进入 prompt:\n%s", c.Messages[0].Content)
		}
		return ConversationResult{Content: `{"summary":"x"}`}, nil
	})
	// sampleFact 的「上一期报告」是 2026-10-14；这里给一份别的区间。
	prev := &PrevSummary{From: "2026-10-19", To: "2026-10-19", Summary: "旧周期结论"}

	_, audit := e.Analyze(context.Background(), AnalyzeInput{Fact: sampleFact(), PrevSummary: prev})
	if strings.Contains(audit.Prompt, "旧周期结论") {
		t.Errorf("区间不匹配的上期摘要不应注入 prompt:\n%s", audit.Prompt)
	}
	// ⚠️ 审计也必须与 prompt 一致 ——
	//   "审计说模型看到了摘要，prompt 里却没有" ⇒ 审计是假的。
	if strings.Contains(audit.Context, "prev_summary") {
		t.Errorf("审计里不应记录被丢弃的摘要:\n%s", audit.Context)
	}
}

// 区间对得上的上期摘要照常注入。
func TestEngineKeepsMatchingPrevSummary(t *testing.T) {
	e := NewEngine(true, 0, func(ctx context.Context, c Conversation) (ConversationResult, error) {
		return ConversationResult{Content: `{"summary":"x"}`}, nil
	})
	// ⚠️ 2026-10-26 起匹配的是「**上一期报告**」的区间（昨天），
	//   而不是「对比基准」的区间（上周同一天）——
	//   日报的对比基准改成 7 天前，而上一期报告仍是昨天那份。
	prev := &PrevSummary{From: "2026-10-14", To: "2026-10-14", Summary: "上期缺货"}

	_, audit := e.Analyze(context.Background(), AnalyzeInput{Fact: sampleFact(), PrevSummary: prev})
	if !strings.Contains(audit.Prompt, "上期缺货") {
		t.Errorf("区间匹配的上期摘要应被注入:\n%s", audit.Prompt)
	}
	if !strings.Contains(audit.Context, "prev_summary") {
		t.Errorf("审计上下文应包含上期摘要:\n%s", audit.Context)
	}
}

// PrevSummaryMatches 的判定表。
func TestPrevSummaryMatches(t *testing.T) {
	f := sampleFact() // PrevReportFrom/To = 2026-10-14（上一期报告）

	cases := []struct {
		name string
		prev *PrevSummary
		want bool
	}{
		{"区间一致", &PrevSummary{From: "2026-10-14", To: "2026-10-14"}, true},
		{"区间不同", &PrevSummary{From: "2026-10-19", To: "2026-10-19"}, false},
		{"只有起始日不同", &PrevSummary{From: "2026-10-13", To: "2026-10-14"}, false},
		{"上期是区间（周报）", &PrevSummary{From: "2026-10-05", To: "2026-10-11"}, false},
		{"没给区间则不拦", &PrevSummary{Summary: "只有结论"}, true},
		{"只有起始日也没拦", &PrevSummary{From: "2026-10-14"}, true},
		{"nil 不拦", nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PrevSummaryMatches(f, c.prev); got != c.want {
				t.Errorf("PrevSummaryMatches = %v, 期望 %v", got, c.want)
			}
		})
	}
}

// ⚠️ 事实包没给「上一期报告」区间时，回退到「对比基准」区间。
//
// 这是老数据/旧夹具的兼容路径：回退错了会让延续性叙述静默失效
// （每期都把上期摘要丢掉，而没有任何报错）。
func TestPrevSummaryMatchesFallsBackToBaseline(t *testing.T) {
	f := sampleFact()
	f.Meta.PrevReportFrom = ""
	f.Meta.PrevReportTo = ""

	if !PrevSummaryMatches(f, &PrevSummary{From: "2026-10-08", To: "2026-10-08"}) {
		t.Error("没有 PrevReport 区间时应回退到对比基准区间")
	}
	if PrevSummaryMatches(f, &PrevSummary{From: "2026-10-14", To: "2026-10-14"}) {
		t.Error("回退到对比基准后，上期报告区间不应再匹配")
	}
}
