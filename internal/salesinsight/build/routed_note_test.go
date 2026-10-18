package build

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/YunBright/supertrade/internal/salesdaily"
)

// historyLoader 在 fakeLoader 之上补上 FirstCollectedDate，
// 用来验证「历史没攒够」与「中间漏采」的区分。
type historyLoader struct {
	*fakeLoader
	first string
	ok    bool
}

func (h *historyLoader) FirstCollectedDate(context.Context, string) (string, bool, error) {
	if !h.ok {
		return "", false, nil
	}
	return h.first, true, nil
}

// TestFallbackNoteDistinguishesUnaccumulatedHistory ⭐
//
// 缺失的日子都在首次采集日**之前** ⇒ "历史还没攒够"，该等。
// 两种情况在 ListMetas 里长得一模一样，而处置完全相反：
// 不分开的话运营会去查采集配置，而真正的问题只是"上周才上线"。
func TestFallbackNoteDistinguishesUnaccumulatedHistory(t *testing.T) {
	base := &fakeLoader{
		facts: map[string]salesdaily.RangeFacts{
			"2026-10-01~2026-10-18": {
				BranchID: "00", From: "2026-10-01", To: "2026-10-18",
				CoveredDays: 1, TotalDays: 18,
			},
		},
		errs: map[string]error{
			"2026-10-01~2026-10-18": wrapIncomplete(errors.New("缺 17 天")),
		},
	}
	s := newRoutedSource(&historyLoader{fakeLoader: base, first: "2026-10-18", ok: true}, &countingCube{})
	// ⚠️ 必须先真的查一次：决策是**查询发生时**才产生的，
	//   reportLines 只汇报已发生的决策，不主动触发。
	_ = s.decide(context.Background(), "00", "2026-10-01", "2026-10-18")
	lines := s.reportLines()
	if len(lines) != 1 {
		t.Fatalf("应产生一条回退说明，实际 %v", lines)
	}
	if !strings.Contains(lines[0], "2026-10-18 起才开始采集落库") {
		t.Errorf("应说明从哪天起才开始落库，实际 %q", lines[0])
	}
	if strings.Contains(lines[0], "只覆盖了") && !strings.Contains(lines[0], "此前的数据不在库里") {
		t.Errorf("缺少历史不在库里的解释，运营会误以为是采集漏了：%q", lines[0])
	}
}

// TestFallbackNoteReportsGapAfterFirstCollection 中间/末尾缺天 ⇒ 查采集日志。
//
// ⚠️ 这一条与上一条**必须**不同：文案不能含糊，
//
//	否则两种情况都变成同一句"可能采集失败"，
//	于是在"其实只是还没上线"的时候误导运维去查 job。
func TestFallbackNoteReportsGapAfterFirstCollection(t *testing.T) {
	base := &fakeLoader{
		facts: map[string]salesdaily.RangeFacts{
			"2026-10-01~2026-10-18": {
				BranchID: "00", From: "2026-10-01", To: "2026-10-18",
				CoveredDays: 16, TotalDays: 18,
			},
		},
		errs: map[string]error{
			"2026-10-01~2026-10-18": wrapIncomplete(errors.New("缺 2 天")),
		},
	}
	s := newRoutedSource(&historyLoader{fakeLoader: base, first: "2026-10-01", ok: true}, &countingCube{})
	_ = s.decide(context.Background(), "00", "2026-10-01", "2026-10-18")
	lines := s.reportLines()
	if len(lines) != 1 {
		t.Fatalf("应产生一条回退说明，实际 %v", lines)
	}
	if !strings.Contains(lines[0], "16/18") {
		t.Errorf("应报出覆盖天数 16/18，实际 %q", lines[0])
	}
	if strings.Contains(lines[0], "才开始采集落库") {
		t.Errorf("不该说历史没攒够 —— 首次采集日早于区间起点：%q", lines[0])
	}
}

// TestFallbackNoteWorksWithoutHistoryCapability 读不到首次采集日时
// 仍要给出可用文案，不能整条丢失。
//
// ⚠️ 降级路径不报错，但**必须仍然说话** ——
// 悄悄回退而不留痕，正是这次改造要消灭的失败形态。
func TestFallbackNoteWorksWithoutHistoryCapability(t *testing.T) {
	base := &fakeLoader{
		facts: map[string]salesdaily.RangeFacts{
			"2026-10-01~2026-10-18": {
				BranchID: "00", From: "2026-10-01", To: "2026-10-18",
				CoveredDays: 16, TotalDays: 18,
			},
		},
		errs: map[string]error{
			"2026-10-01~2026-10-18": wrapIncomplete(errors.New("缺 2 天")),
		},
	}
	// fakeLoader 没有 FirstCollectedDate ⇒ 走"能力缺省"分支。
	s := newRoutedSource(base, &countingCube{})
	_ = s.decide(context.Background(), "00", "2026-10-01", "2026-10-18")
	lines := s.reportLines()
	if len(lines) != 1 || !strings.Contains(lines[0], "16/18") {
		t.Errorf("缺诊断能力时也必须有可读文案，实际 %v", lines)
	}
}

// TestNoSnapshotEverSaysWhenCollectionStarts 一行都没有时要说清"从哪天起有"。
func TestNoSnapshotEverSaysWhenCollectionStarts(t *testing.T) {
	base := &fakeLoader{
		facts: map[string]salesdaily.RangeFacts{
			"2026-10-18~2026-10-18": {BranchID: "00", From: "2026-10-18", To: "2026-10-18"},
		},
		errs: map[string]error{"2026-10-18~2026-10-18": errors.New("PG 里没有这个区间")},
	}
	s := newRoutedSource(&historyLoader{fakeLoader: base, first: "2026-10-20", ok: true}, &countingCube{})
	_ = s.decide(context.Background(), "00", "2026-10-18", "2026-10-18")
	lines := s.reportLines()
	if len(lines) != 1 || !strings.Contains(lines[0], "2026-10-20") {
		t.Errorf("应报出该门店最早的采集日，实际 %v", lines)
	}
}

// TestPGNoteMentionsCorrectedDays ⭐ 走 PG 时也要说清"有几天被次日校正过"。
//
// ⚠️ "被校正过的数字"与"从未被校正的数字"在数值上**完全一样** ——
//
//	而思迅日结晚于收档时刻是常态，店长需要知道
//	"这个月的数字里有几天是事后补的"，否则会把补出来的数
//	当成当天真实发生的情况。
func TestPGNoteMentionsCorrectedDays(t *testing.T) {
	loader := &fakeLoader{facts: map[string]salesdaily.RangeFacts{
		"2026-10-10~2026-10-16": {
			BranchID: "00", From: "2026-10-10", To: "2026-10-16",
			Totals:        salesdaily.TotalsRow{NetAmount: 700},
			Categories:    []salesdaily.CategoryRow{{Name: "A", Amount: 700}},
			CoveredDays:   7,
			TotalDays:     7,
			CorrectedDays: 5,
		},
	}}
	s := newRoutedSource(loader, &countingCube{})
	ctx := context.Background()
	if _, err := s.TotalsSales(ctx, "00", "2026-10-10", "2026-10-16"); err != nil {
		t.Fatalf("totals：%v", err)
	}
	lines := s.reportLines()
	if len(lines) != 1 {
		t.Fatalf("应产生一条说明，实际 %v", lines)
	}
	if !strings.Contains(lines[0], "5 天是次日校正后定稿的") {
		t.Errorf("应报出 5 天被校正，实际 %q", lines[0])
	}
}

// TestPGNoteSilentForSingleDay 单日不啰嗦。
//
// ⚠️ 每天一句"这天被校正过"是噪声，会把真正要看的告警淹掉。
func TestPGNoteSilentForSingleDay(t *testing.T) {
	loader := &fakeLoader{facts: map[string]salesdaily.RangeFacts{
		"2026-10-16~2026-10-16": {
			BranchID: "00", From: "2026-10-16", To: "2026-10-16",
			Totals:      salesdaily.TotalsRow{NetAmount: 500},
			Categories:  []salesdaily.CategoryRow{{Name: "A", Amount: 500}},
			CoveredDays: 1, TotalDays: 1, CorrectedDays: 1,
		},
	}}
	s := newRoutedSource(loader, &countingCube{})
	if _, err := s.TotalsSales(context.Background(), "00", "2026-10-16", "2026-10-16"); err != nil {
		t.Fatalf("totals：%v", err)
	}
	if lines := s.reportLines(); len(lines) != 0 {
		t.Errorf("单日不该产生说明（噪声），实际 %v", lines)
	}
}
