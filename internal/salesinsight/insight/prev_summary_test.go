package insight

import (
	"context"
	"strings"
	"testing"
)

// ⚠️ 区间对不上的上期摘要必须**整份丢弃**。
//
// 2026-10-23 生产实测：周报正文出现
// 「上期结论写销售额60585.35元，与本期引用的118653.51元不一致」。
// 真相是那份"上一期周报"是在**周期口径改动之前**生成的，
// 它与本期的"上期"根本不是同一段 —— 类型相同 ≠ 区间相同。
func TestEngineDropsMismatchedPrevSummary(t *testing.T) {
	e := NewEngine(true, 0, func(ctx context.Context, s, u string) (string, int, int, error) {
		if strings.Contains(u, "旧周期结论") {
			t.Errorf("区间不匹配的上期摘要不应进入 prompt:\n%s", u)
		}
		return `{"summary":"x"}`, 0, 0, nil
	})
	// sampleFact 的上期是 2026-10-14；这里给一份别的区间。
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
	e := NewEngine(true, 0, func(ctx context.Context, s, u string) (string, int, int, error) {
		return `{"summary":"x"}`, 0, 0, nil
	})
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
	f := sampleFact() // PrevFrom/PrevTo = 2026-10-14

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
