package calendar

import (
	"fmt"
	"time"
)

// Range 是一个闭区间 [Start, End]。
type Range struct {
	Start time.Time
	End   time.Time
}

// Days 返回区间包含的自然天数（含首尾）。
//
// ⚠️ 负数天数会被夹成 0：Range 若被错误地构造成 End<Start
// （比如调用方把 from/to 传反了），返回负数会让下游所有占比计算翻成负的，
// 而报表上显示的百分比会是"看起来合理"的负数。
// 夹成 0 让它变成"空区间"，至少在大盘上可辨认。
func (r Range) Days() int {
	if r.End.Before(r.Start) {
		return 0
	}
	return int(r.End.Sub(r.Start)/(24*time.Hour)) + 1
}

// Dates 返回区间内的每一天（按天，从 Start 到 End）。
func (r Range) Dates() []string {
	n := r.Days()
	if n == 0 {
		return nil
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Format(r.Start.AddDate(0, 0, i)))
	}
	return out
}

func (r Range) String() string { return Format(r.Start) + "~" + Format(r.End) }

// PeriodKind 是报表周期。
type PeriodKind string

const (
	PeriodDaily   PeriodKind = "daily"
	PeriodWeekly  PeriodKind = "weekly"
	PeriodMonthly PeriodKind = "monthly"
)

// Period 是一个报表周期及其上期（同期口径）。
type Period struct {
	Kind PeriodKind
	Cur  Range
	Prev Range
}

// Label 是给店长看的周期名。
func (p Period) Label() string {
	switch p.Kind {
	case PeriodDaily:
		return "日报"
	case PeriodWeekly:
		return "周报"
	case PeriodMonthly:
		return "月报"
	}
	return string(p.Kind)
}

// ResolvePeriod 按周期解析本期与上期区间。
//
// # 三种周期各自的锚点（**互不相同**，这是有意的）
//
//	日报：今日 vs 昨日
//	周报：上周一~上周日 vs 上上周一~上上周日
//	月报：上月1号~上月末 vs 上上月1号~上上月月末
//
// ⚠️ **不要**把它们统一成"本周期至今"。原因：
//
//	日报 22:50 推是**营业日中段**，店长要的是「今天怎么样、现在补什么」，
//	所以取今日；而周报在**周一**推、月报在**每月 1 号**推 ——
//	那两天推"本周一~今天 / 本月1号~今天"分别是 **1 天**，
//	一份只覆盖 1 天的"周报"没有任何内容。
//
// ⇒ 周报/月报取**上一个完整周期**，日报取**当日实况**。
// 这与 Flutter 端的文案逐字一致（`ai_report_rules.dart`）：
//
//	「每天推送当天汇总」/「每周一推送上周汇总」/「每月 1 日推送上月汇总」
//
// ⚠️ **上期一律取"同期口径"**（相邻的上一段），不是自然上期：
//
//	周报：上周 7 天 vs 上上周 7 天（长度相同，可直接比）
//	月报：上月 N 天 vs 上上月 M 天 —— ⚠️ **长度可能不同**
//	（2 月 28 天 vs 1 月 31 天）。这是**真实**的口径差异，
//	必须由 `calendar.Compare` 判成 limited 并在卡片上说明，
//	而不是悄悄折算。
//
// 这个错误极其常见且极具误导性：它每次都"看起来像一个坏消息"，
// 而实际上只是口径错了。店长会照着它砍品。
//
// # ⚠️ **now 必须带营业时区（Asia/Shanghai）**，否则早期早班（0:00~08:00 CST）
//
//	会被 time.Now 默认的 UTC 误算成前一天 —— 例如 0:30 CST = 16:30 UTC，
//	若用 UTC 则 daily 落到「昨日」。
//
// 调用本函数前用 `s.now().Location()` 检查；若不是 `Asia/Shanghai`，
// 应通过 ServiceImpl.WithBizTZ 注入（见 cmd/sales-insight/main.go）。
func ResolvePeriod(kind PeriodKind, now time.Time) (Period, error) {
	d := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch kind {
	case PeriodDaily:
		// today vs yesterday（当日实况：22:50 是营业日中段）
		cur := Range{Start: d, End: d}
		prev := Range{Start: d.AddDate(0, 0, -1), End: d.AddDate(0, 0, -1)}
		return Period{Kind: kind, Cur: cur, Prev: prev}, nil

	case PeriodWeekly:
		// 上周一~上周日（**完整周**），而不是"本周一到今天"。
		//
		// ⚠️ 周报在**周一**推：若取"本周一~今天"就是 1 天，
		// 一份只覆盖 1 天的周报没有任何内容。
		offset := (int(d.Weekday()) + 6) % 7
		thisMonday := d.AddDate(0, 0, -offset)
		lastMonday := thisMonday.AddDate(0, 0, -7)
		cur := Range{Start: lastMonday, End: lastMonday.AddDate(0, 0, 6)}
		// 同期：再往前 7 天，得到"上上周一 ~ 上上周日"
		prev := Range{
			Start: lastMonday.AddDate(0, 0, -7),
			End:   lastMonday.AddDate(0, 0, -1),
		}
		return Period{Kind: kind, Cur: cur, Prev: prev}, nil

	case PeriodMonthly:
		// 上月1号~上月末（**完整月**）。
		//
		// ⚠️ 不用"上月同日"口径，而是整月对整月：
		// 本期 = 上月1号~上月末，上期 = 上上月1号~上上月末。
		// "上月同日"在 3/31 会溢出（2月31日不存在），
		// time 的规范化把它变成 3月3日 —— 上期区间凭空多出几天，
		// 于是月报显示"环比上涨"，纯属算错。
		//
		// ⚠️ 代价：2月(28天) vs 1月(31天) 长度不同 ⇒ Compare 判 limited，
		// 卡片会明说"天数不同"—— 这是**诚实**的口径差异，
		// 比悄悄折算更可信。
		thisFirst := time.Date(d.Year(), d.Month(), 1, 0, 0, 0, 0, d.Location())
		// ⚠️ AddDate(0,-1,0) 用在**1 号**上是安全的：1 号在每个月都存在，
		// 不会像 31 号那样被规范化到下个月。
		lastFirst := thisFirst.AddDate(0, -1, 0)
		lastEnd := thisFirst.AddDate(0, 0, -1)
		prevFirst := lastFirst.AddDate(0, -1, 0)
		prevEnd := lastFirst.AddDate(0, 0, -1)
		cur := Range{Start: lastFirst, End: lastEnd}
		prev := Range{Start: prevFirst, End: prevEnd}
		return Period{Kind: kind, Cur: cur, Prev: prev}, nil
	}
	return Period{}, fmt.Errorf("calendar: 不支持的周期 %q", kind)
}
