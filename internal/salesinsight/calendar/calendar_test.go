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

// ⚠️ 本包的**核心**防回归测试：周报/月报的上期必须是"同期口径"。
//
// 如果上期退化成自然上期（上周一~上周日 / 上月1号~上月底），
// 在周中/月中就会拿 4 天比 7 天，金额必然显示"大幅下滑"，
// 而那只是"本周还没过完"。这个错误每次都像坏消息，但毫无经营含义。
func TestResolvePeriodUsesSameLengthComparison(t *testing.T) {
	cases := []struct {
		name     string
		kind     PeriodKind
		now      string
		wantCur  [2]string
		wantPrev [2]string
	}{
		{
			name: "日报取昨天比前天", kind: PeriodDaily, now: "2026-10-16",
			wantCur: [2]string{"2026-10-15", "2026-10-15"},
			// 10-15 是昨天，10-14 是前天
			wantPrev: [2]string{"2026-10-14", "2026-10-14"},
		},
		{
			// 周四：本周一(10-12)~今天(10-16) = 5 天
			// 同期：上周一(10-05)~上周四(10-09) = 5 天 ✅
			// 若退化成自然上期(10-05~10-11)就是 7 天 → 金额假跌 29%
			name: "周报周四用上周同期而非自然上期", kind: PeriodWeekly, now: "2026-10-16",
			wantCur:  [2]string{"2026-10-12", "2026-10-16"},
			wantPrev: [2]string{"2026-10-05", "2026-10-09"},
		},
		{
			name: "月报月中用上月同日而非上月整月", kind: PeriodMonthly, now: "2026-10-16",
			wantCur:  [2]string{"2026-10-01", "2026-10-16"},
			// 10-16 上月同日 = 09-16，不是 09-30
			wantPrev: [2]string{"2026-09-01", "2026-09-16"},
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
				t.Errorf("上期 = %v, 期望 %v（自然上期会让金额假跌）", gotPrev, c.wantPrev)
			}
			// 同期口径的定义性质：两期天数必须相等
			if p.Cur.Days() != p.Prev.Days() {
				t.Errorf("同期口径下两期天数应相等，实际 %d vs %d",
					p.Cur.Days(), p.Prev.Days())
			}
		})
	}
}

// 上月同日不能溢出到本月。
//
// 3月31日报月报时，"上月同日"是 2月31日（不存在），
// time 的规范化会把它变成 3月3日 —— 上期区间凭空多出两天，
// 于是月报显示"环比上涨"，纯属算错。
func TestMonthlyPrevDoesNotOverflowIntoNextMonth(t *testing.T) {
	p, err := ResolvePeriod(PeriodMonthly, mustTime(t, "2026-03-31"))
	if err != nil {
		t.Fatalf("ResolvePeriod 失败: %v", err)
	}
	if got := Format(p.Prev.End); got != "2026-02-28" {
		t.Fatalf("3月31日的上月同日应为 2026-02-28（2月最后一天），实际 %s", got)
	}
	if p.Prev.End.After(p.Prev.Start.AddDate(0, 1, -1)) {
		t.Fatal("上期区间越出了上个月")
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
