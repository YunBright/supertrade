package calendar

import (
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(DateLayout, s)
	if err != nil {
		t.Fatalf("解析日期 %q 失败: %v", s, err)
	}
	return v
}

func TestDescribeKinds(t *testing.T) {
	cases := []struct {
		date     string
		kind     DayKind
		restDay  bool
		nameHint string
	}{
		// 普通工作日
		{"2026-10-16", KindWorkday, false, ""},
		// 普通周末
		{"2026-10-17", KindWeekend, true, ""},
		{"2026-10-18", KindWeekend, true, ""},
		// 法定假日（国庆 10/1~10/7）
		{"2026-10-01", KindHoliday, true, "国庆节"},
		{"2026-10-05", KindHoliday, true, "国庆节"},
		// ⚠️ 调休上班日：日历上是周六/周日，但**必须**判为上班
		{"2026-10-10", KindMakeupWorkday, false, "国庆调休"},
		{"2026-09-20", KindMakeupWorkday, false, "国庆调休"},
		{"2026-01-04", KindMakeupWorkday, false, "元旦调休"},
	}
	for _, c := range cases {
		t.Run(c.date, func(t *testing.T) {
			d, err := Describe(c.date)
			if err != nil {
				t.Fatalf("Describe(%q) 失败: %v", c.date, err)
			}
			if d.Kind != c.kind {
				t.Errorf("Kind = %q, 期望 %q", d.Kind, c.kind)
			}
			if d.IsRestDay != c.restDay {
				t.Errorf("IsRestDay = %v, 期望 %v", d.IsRestDay, c.restDay)
			}
			if c.nameHint != "" && d.Name != c.nameHint {
				t.Errorf("Name = %q, 期望 %q", d.Name, c.nameHint)
			}
		})
	}
}

// 调休上班日必须**先于**周末判定。
//
// 这条单测单独存在是因为它是本包最容易回归的地方：一旦有人把
// holidays 检查写在 makeupWorkdays 之后，或干脆把调休表删掉，
// 2026-10-10（周六，国庆调休上班）就会被判成休息日 ——
// 而这一天恰恰在国庆之后，是客流最高的日子之一。
func TestMakeupWorkdayBeatsWeekend(t *testing.T) {
	d, err := Describe("2026-10-10")
	if err != nil {
		t.Fatalf("Describe 失败: %v", err)
	}
	// 先确认它日历上确实是周六 —— 否则这条测试的前提就不成立了
	wd := mustTime(t, "2026-10-10").Weekday()
	if wd != time.Saturday {
		t.Fatalf("前提不成立：2026-10-10 不是周六(是 %v)", wd)
	}
	if d.Kind != KindMakeupWorkday {
		t.Fatalf("2026-10-10 是调休上班日，实际判成 %q", d.Kind)
	}
	if d.IsRestDay {
		t.Fatal("调休上班日不能是休息日")
	}
}

// ⚠️ 本包的**核心**防回归测试：三种周期各自的锚点。
//
//	日报：今日 vs 昨日
//	周报：上周一~上周日 vs 上上周一~上上周日
//	月报：上月1号~上月末 vs 上上月1号~上上月末
//
// ⚠️ 三者**不共用**一个锚点，这是有意的：
//
//	日报 22:50 推是营业日中段，店长看的是「今天怎么样」；
//	周报在**周一**推 —— 若取"本周一~今天"就只有 1 天，那不是周报；
//	月报在**每月 1 号**推 —— 同理，取"本月1号~今天"只有 1 天。
//
// ⇒ 周报/月报必须取**上一个完整周期**。这与 Flutter 端文案逐字一致
//（`ai_report_rules.dart`：「每天推送当天汇总」/「每周一推送上周汇总」
// /「每月 1 日推送上月汇总」）。
//
// 如果上期退化成自然上期，在周中/月中就会拿 4 天比 7 天，
// 金额必然显示"大幅下滑"，而那只是"本周还没过完"。
// 这个错误每次都像坏消息，但毫无经营含义。
//
// 日报退化成"昨天"会让 22:50 在 D 推送的报告变成 D-1，而店长看的就是 D 当下 ——
// 那条 bug 在 2026-10-19 现场实测：用户看到「现在是 2026-10-19 23:30，
// 但日报显示 2026-10-18」，根因就是这里。
func TestResolvePeriodUsesSameLengthComparison(t *testing.T) {
	cases := []struct {
		name     string
		kind     PeriodKind
		now      string
		wantCur  [2]string
		wantPrev [2]string
	}{
		{
			// 22:50 推送 = 当日营业中段，「今日」反映当下实况。
			// 对比基准是「上周同一天」（2026-10-26 改）：
			// 同一个星期几 ⇒ 客流基准一致，涨跌才反映经营变化。
			// 生产日报自己就打出了警告「本期周日对比上期周六，日环比受星期差异影响」。
			name: "日报今日(22:50推送) vs 上周同一天", kind: PeriodDaily, now: "2026-10-16",
			wantCur:  [2]string{"2026-10-16", "2026-10-16"},
			wantPrev: [2]string{"2026-10-09", "2026-10-09"},
		},
		{
			// 2026-10-16 是周五 ⇒ 本周日 10-11，上周日 10-04。
			// 本期 = 上周日(10-04)~上周六(10-10) = 完整 7 天
			// 上期 = 上上周日(09-27)~上上周六(10-03) = 完整 7 天 ✅
			name: "周报取上一个完整周而非本周至今", kind: PeriodWeekly, now: "2026-10-16",
			wantCur:  [2]string{"2026-10-04", "2026-10-10"},
			wantPrev: [2]string{"2026-09-27", "2026-10-03"},
		},
		{
			// 本期 = 上月1号(09-01)~上月末(09-30)
			// 上期 = 上上月1号(08-01)~上上月末(08-31)
			name: "月报取上一个完整月", kind: PeriodMonthly, now: "2026-10-16",
			wantCur:  [2]string{"2026-09-01", "2026-09-30"},
			wantPrev: [2]string{"2026-08-01", "2026-08-31"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := ResolvePeriod(c.kind, mustTime(t, c.now))
			if err != nil {
				t.Fatalf("ResolvePeriod 失败: %v", err)
			}
			gotCur := [2]string{Format(p.Cur.Start), Format(p.Cur.End)}
			gotPrev := [2]string{Format(p.Prev.Start), Format(p.Prev.End)}
			if gotCur != c.wantCur {
				t.Errorf("本期 = %v, 期望 %v", gotCur, c.wantCur)
			}
			if gotPrev != c.wantPrev {
				t.Errorf("上期 = %v, 期望 %v", gotPrev, c.wantPrev)
			}
			// 同期口径的定义性质：两期天数必须相等。
			//
			// ⚠️ **月报例外**：2月28天 vs 1月31天 是真实口径差异，
			// 由 calendar.Compare 判 limited 并在卡片上说明，不在这里抹平。
			if c.kind != PeriodMonthly && p.Cur.Days() != p.Prev.Days() {
				t.Errorf("同期口径下两期天数应相等，实际 %d vs %d",
					p.Cur.Days(), p.Prev.Days())
			}
		})
	}
}

// 月报的本期/上期都必须是**完整月**，且不得溢出到相邻月。
//
// 3月31日报月报时，本期 = 2月1号~2月28日（2月短月），
// 上期 = 1月1号~1月31日。
//
// ⚠️ 这条同时钉住"上月末"的取法：上上月末必须是 1月31日
//（1月是31天），错一个月就整月错。
func TestMonthlyPeriodsAreWholeMonths(t *testing.T) {
	p, err := ResolvePeriod(PeriodMonthly, mustTime(t, "2026-03-31"))
	if err != nil {
		t.Fatalf("ResolvePeriod 失败: %v", err)
	}
	if got := Format(p.Cur.Start); got != "2026-02-01" {
		t.Errorf("本期起点 = %s, 期望 2026-02-01（上月1号）", got)
	}
	if got := Format(p.Cur.End); got != "2026-02-28" {
		t.Errorf("本期终点 = %s, 期望 2026-02-28（2月最后一天）", got)
	}
	if got := Format(p.Prev.Start); got != "2026-01-01" {
		t.Errorf("上期起点 = %s, 期望 2026-01-01（上上月1号）", got)
	}
	if got := Format(p.Prev.End); got != "2026-01-31" {
		t.Errorf("上期终点 = %s, 期望 2026-01-31（上上月末，1月是31天）", got)
	}
	// 本期必须**整月落在 2 月内**，不能溢出到 3 月。
	if p.Cur.End.After(p.Cur.Start.AddDate(0, 1, -1)) {
		t.Fatal("本期区间越出了上个月")
	}
	if p.Prev.End.After(p.Prev.Start.AddDate(0, 1, -1)) {
		t.Fatal("上期区间越出了上上个月")
	}
}

// 周报必须是**完整 7 天**，且落在上一个完整周里。
//
// 2026-10-16 是周五 ⇒ 上周日 10-04 ~ 上周六 10-10（周日为一周之始，
// 与思迅老系统及 App 端的 date_range_preset 同一口径）。
//
// ⚠️ 钉的是「上周」而不是「本周」：周报在**周一**推，
// 若取"本周一到今天"就只有 1 天，那不是周报（2026-10-23 现场实测的 bug）。
func TestWeeklyPeriodIsWholePreviousWeek(t *testing.T) {
	p, err := ResolvePeriod(PeriodWeekly, mustTime(t, "2026-10-16"))
	if err != nil {
		t.Fatalf("ResolvePeriod 失败: %v", err)
	}
	if got := Format(p.Cur.Start); got != "2026-10-04" {
		t.Errorf("本期起点 = %s, 期望 2026-10-04（上周日）", got)
	}
	if got := Format(p.Cur.End); got != "2026-10-10" {
		t.Errorf("本期终点 = %s, 期望 2026-10-10（上周六）", got)
	}
	if n := p.Cur.Days(); n != 7 {
		t.Errorf("周报本期必须是完整 7 天，实际 %d 天", n)
	}
	if n := p.Prev.Days(); n != 7 {
		t.Errorf("周报上期必须是完整 7 天，实际 %d 天", n)
	}
	if wd := p.Cur.Start.Weekday(); wd != time.Sunday {
		t.Errorf("周报本期起点应是周日，实际 %v", wd)
	}
	if wd := p.Cur.End.Weekday(); wd != time.Saturday {
		t.Errorf("周报本期终点应是周六，实际 %v", wd)
	}
}

func TestCompareDetectsDayCountMismatch(t *testing.T) {
	// 4 天 vs 7 天：即使日期性质一致，也**不可比**
	cur := Range{Start: mustTime(t, "2026-10-12"), End: mustTime(t, "2026-10-15")}
	prev := Range{Start: mustTime(t, "2026-10-05"), End: mustTime(t, "2026-10-11")}
	got := Compare(cur, prev)
	if got.Kind != ComparableLimited {
		t.Fatalf("天数不同时应判为 limited，实际 %q", got.Kind)
	}
	if got.Reason == "" {
		t.Fatal("可比性受限必须给出白话理由，卡片要直接印给店长看")
	}
}

func TestCompareDetectsHolidayMismatch(t *testing.T) {
	// 10-08(周四,节后首个工作日) vs 10-01(周四,国庆首日)
	// 天数相同但性质截然不同 → 不可比
	cur := Range{Start: mustTime(t, "2026-10-08"), End: mustTime(t, "2026-10-08")}
	prev := Range{Start: mustTime(t, "2026-10-01"), End: mustTime(t, "2026-10-01")}
	got := Compare(cur, prev)
	if got.Kind != ComparableLimited {
		t.Fatalf("假日 vs 工作日应判为 limited，实际 %q", got.Kind)
	}
}

// 反向用例：同性质同天数必须判为可比 —— 否则每份日报都会挂一句无意义的警告。
//
// ⚠️ 日期是刻意选的：上周必须避开 10-01~10-07 国庆，
// 否则"同性质"的前提不成立（10-06 是假日，不是工作日）。
func TestCompareSameKindIsComparable(t *testing.T) {
	cur := Range{Start: mustTime(t, "2026-10-13"), End: mustTime(t, "2026-10-14")}
	prev := Range{Start: mustTime(t, "2026-10-20"), End: mustTime(t, "2026-10-21")}
	// 先自证前提，避免用例会因节假日表更新而悄悄失去意义
	for _, d := range prev.Dates() {
		got, _ := Describe(d)
		if got.Kind != KindWorkday {
			t.Fatalf("测试前提失效：%s 是 %q，不是普通工作日", d, got.Kind)
		}
	}
	got := Compare(cur, prev)
	if got.Kind != ComparableGood {
		t.Fatalf("同为普通工作日应判为 comparable，实际 %q (%s)", got.Kind, got.Reason)
	}
}

// 周末 vs 周末应判为可比：周六比周六是合理的对比。
//
// ⚠️ 上期刻意避开 10-10（国庆调休上班日）—— 它在日历上是周六，
// 但**不是休息日**。这正是本表存在的意义：朴素的 Weekday 判断
// 会把这两者当成同类，从而得出"可比"的错误结论。
func TestCompareWeekendToWeekendIsComparable(t *testing.T) {
	cur := Range{Start: mustTime(t, "2026-10-17"), End: mustTime(t, "2026-10-18")}
	prev := Range{Start: mustTime(t, "2026-10-24"), End: mustTime(t, "2026-10-25")}
	got := Compare(cur, prev)
	if got.Kind != ComparableGood {
		t.Fatalf("同为周末应判为 comparable，实际 %q (%s)", got.Kind, got.Reason)
	}
}

// ⚠️ 反证：**调休上班日 ≠ 休息日**，哪怕日历上同为周六。
//
// 这条是本包最重要的一条。它与上一条构成对照：
//   上期用 10-24/25(真周末) → 可比
//   上期用 10-10(调休上班)   → 不可比
// 只用 time.Weekday() 的实现会让这两种情况都判成"可比"。
func TestMakeupWorkdayIsNotComparableToRestWeekend(t *testing.T) {
	cur := Range{Start: mustTime(t, "2026-10-17"), End: mustTime(t, "2026-10-17")}
	prev := Range{Start: mustTime(t, "2026-10-10"), End: mustTime(t, "2026-10-10")}
	got := Compare(cur, prev)
	if got.Kind != ComparableLimited {
		t.Fatalf("周六比调休上班的周六应判为 limited，实际 %q (%s)", got.Kind, got.Reason)
	}
}

// 区间构造反了不能让天数变负 —— 负数会让下游占比计算翻负，
// 报表上会出现"看起来合理"的负百分比。
func TestRangeDaysNeverNegative(t *testing.T) {
	r := Range{Start: mustTime(t, "2026-10-15"), End: mustTime(t, "2026-10-12")}
	if n := r.Days(); n != 0 {
		t.Fatalf("End<Start 时天数应为 0，实际 %d", n)
	}
	if d := r.Dates(); d != nil {
		t.Fatalf("空区间不应产生日期列表，实际 %v", d)
	}
}

// ⚠️ daily 必须走「今日」语义 —— 22:50 推送时营业日中段。
//
// 反证：把 `d` 改成 `d.AddDate(0, 0, -1)`（即变回「昨日」）→
// 22:50 在 D 推送会显示 D-1 的"完整昨日"销售，
// 用户实测 2026-10-19 看到的正是这个 bug。
//
// 期望值用字面量（"2026-10-16"），而不是由 now 推导，
// 避免「改了 now 测试照样过」的判别力塌陷。
func TestDailyCurIsTodayNotYesterday(t *testing.T) {
	now := time.Date(2026, 10, 16, 22, 50, 0, 0, time.UTC)
	p, err := ResolvePeriod(PeriodDaily, now)
	if err != nil {
		t.Fatalf("ResolvePeriod 失败: %v", err)
	}
	if got := Format(p.Cur.Start); got != "2026-10-16" {
		t.Errorf("daily cur = %q, 期望 2026-10-16（今日，不是 2026-10-15 昨日）", got)
	}
	if got := Format(p.Cur.End); got != "2026-10-16" {
		t.Errorf("daily cur.End = %q, 期望 2026-10-16", got)
	}
	if got := Format(p.Prev.Start); got != "2026-10-09" {
		t.Errorf("daily prev = %q, 期望 2026-10-09（上周同一天）", got)
	}
	// ⚠️ 「上一期报告」仍然是**昨天**（2026-10-15），不是对比基准。
	//
	//	日报的对比基准是 7 天前（避星期差异），
	//	而"上一期报告说了什么"是店长连着读的那一份 —— 必须是昨天。
	//	两者混为一谈 ⇒ 延续性叙述的区间校验会每期失配（静默失效）。
	if got := Format(p.PrevReport.Start); got != "2026-10-15" {
		t.Errorf("daily prevReport = %q, 期望 2026-10-15（昨天）", got)
	}
	if got := p.PrevLabel(); got != "上周同期" {
		t.Errorf("daily PrevLabel = %q, 期望 上周同期", got)
	}
}

// ⚠️ now 的 Location 直接决定「今天是哪一天」 ——
// 早班 0:30 CST = 16:30 UTC（前一日），两条视角下的 daily 必不同。
//
// 这是 2026-10-19 现场 bug 的根因：sales-insight 没注入 Asia/Shanghai，
// time.Now() 默认 UTC，于是 daily 把"今天"算成了 UTC 那天的真实日。
//
// 反证：删掉 `now.Location()`（统一用 time.UTC 算 d.Year/Month/Day）→
// 早班那条路径下 daily 仍然显示「昨日」，
// 即 UTC 视角的"今天"≠ CST 视角的"今天"，店长看到的就是错的日期。
//
// 注意：本测试**只**验证 ResolvePeriod 用 now.Location() 而**不**验证
// 调用方是否注入了 Asia/Shanghai —— 那条契约由 ServiceImpl.WithBizTZ 测试覆盖。
func TestDailyNowBoundaryRespectsTimezone(t *testing.T) {
	cst, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("LoadLocation(Asia/Shanghai) 失败: %v", err)
	}
	// 16:30 UTC 2026-10-15 = 00:30 CST 2026-10-16
	utcNow := time.Date(2026, 10, 15, 16, 30, 0, 0, time.UTC)
	cstNow := utcNow.In(cst)

	pUTC, err := ResolvePeriod(PeriodDaily, utcNow)
	if err != nil {
		t.Fatalf("ResolvePeriod(utcNow) 失败: %v", err)
	}
	if got := Format(pUTC.Cur.Start); got != "2026-10-15" {
		t.Errorf("UTC 视角 daily = %q, 期望 2026-10-15", got)
	}

	pCST, err := ResolvePeriod(PeriodDaily, cstNow)
	if err != nil {
		t.Fatalf("ResolvePeriod(cstNow) 失败: %v", err)
	}
	if got := Format(pCST.Cur.Start); got != "2026-10-16" {
		t.Errorf("CST 视角 daily = %q, 期望 2026-10-16（注入 Asia/Shanghai 才是对的）", got)
	}
}
