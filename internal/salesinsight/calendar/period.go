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
// ⚠️ **上期一律取"同期口径"，不是自然上期**。
//
// 这是本函数存在的全部理由：
//
//   - 日报：**今天 vs 昨天**。
//     wechat-bot 默认在 22:50 推送（每门店 SendTime 可配），这是营业日中段、
//     不是结账后 —— 所以日报应当反映「当日实况」，而不是「昨日结账数」。
//     注意今天的销售额是**部分日**的（22:50 抓取 ≈ 88% 完整度，
//     视门店营业时长而定），环比「完整昨天」会出现「跌」的观感。
//     这是产品取舍：22:50 的报告本来就该反映「当日」，
//     等关店后再出一份收口日报是后续工作的方向。
//     ⚠️ **不要**把 today 改成 yesterday 来「解决」这个下跌观感 ——
//     那只会把 bug 重新引入（用户期望是「今日」，不是「昨日」）。
//   - 周报：本周一~今天 vs **上周一~上周同一天**。
//     若取自然上期（上周一~上周日），当今天是周四时就是 4 天 vs 7 天，
//     金额必然跌 43%，而这只是"还没过完周三"。
//   - 月报：本月1号~今天 vs 上月1号~**上月同一天**。
//     同理，否则月中必然显示大幅下滑。
//
// 这个错误极其常见且极具误导性：它每次都"看起来像一个坏消息"，
// 而实际上只是口径错了。店长会照着它砍品。
//
// # ⚠️ **now 必须带营业时区（Asia/Shanghai）**，否则早期早班（0:00~08:00 CST）
//
//	会被 time.Now 默认的 UTC 误算成前一天 —— 例如 0:30 CST = 16:30 UTC，
//	若用 UTC 则 daily 落到「昨日」，再 +1 偏移后今天变成「昨天的昨天」。
//
// 调用本函数前用 `s.now().Location()` 检查；若不是 `Asia/Shanghai`，
// 应通过 ServiceImpl.WithBizTZ 注入（见 cmd/sales-insight/main.go）。
func ResolvePeriod(kind PeriodKind, now time.Time) (Period, error) {
	d := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch kind {
	case PeriodDaily:
		// today vs yesterday（与周报/月报"本期 = 在走周期"口径保持一致）
		cur := Range{Start: d, End: d}
		prev := Range{Start: d.AddDate(0, 0, -1), End: d.AddDate(0, 0, -1)}
		return Period{Kind: kind, Cur: cur, Prev: prev}, nil

	case PeriodWeekly:
		// 周一为一周之始（与 salesagg.ResolveRange 保持一致）
		offset := (int(d.Weekday()) + 6) % 7
		monday := d.AddDate(0, 0, -offset)
		cur := Range{Start: monday, End: d}
		// 同期：往前推 7 天，得到"上周一 ~ 上周同一天"
		prev := Range{
			Start: monday.AddDate(0, 0, -7),
			End:   d.AddDate(0, 0, -7),
		}
		return Period{Kind: kind, Cur: cur, Prev: prev}, nil

	case PeriodMonthly:
		first := time.Date(d.Year(), d.Month(), 1, 0, 0, 0, 0, d.Location())
		cur := Range{Start: first, End: d}
		prevFirst := first.AddDate(0, -1, 0)
		// 上月同一天。上月天数可能少于本月（如 3/31 对 2 月），
		// 所以要夹到上月的最后一天，而不是让时间溢出到下月 ——
		// AddDate 的规范化行为会把 3/31-1月 变成 3/3，那会让上期区间凭空多出几天。
		prevSameDay := first.AddDate(0, -1, d.Day()-1)
		lastOfPrev := prevFirst.AddDate(0, 1, -1)
		if prevSameDay.After(lastOfPrev) {
			prevSameDay = lastOfPrev
		}
		prev := Range{Start: prevFirst, End: prevSameDay}
		return Period{Kind: kind, Cur: cur, Prev: prev}, nil
	}
	return Period{}, fmt.Errorf("calendar: 不支持的周期 %q", kind)
}
