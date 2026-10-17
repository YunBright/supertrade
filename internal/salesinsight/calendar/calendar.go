// Package calendar 提供"某一天是什么日子"的判定：星期、法定节假日、
// 调休上班、以及由此推出的**可比性**结论。
//
// # 为什么可比性比日期更重要
//
// 零售日报说"今天比昨天跌了 30%"，这句话本身几乎不含信息量 ——
// 直到你知道昨天是周五、今天是周六。商超的周末客流通常是工作日的 1.5~2 倍，
// 拿周六和周五比，结论会系统性地偏负。
//
// 更隐蔽的是**调休**：`2026-10-09` 是国庆后的调休上班日（星期五），
// 它在日历上看起来是普通工作日，但对零售来说它是"假期尾巴"，客流极高。
// 如果把调休日当普通工作日处理，节后第一份日报必然得出"大幅下滑"的错误结论，
// 而店长会据此砍掉正在爆发的品。
//
// # 所以本包输出的是"可比性结论"，不是一个日期字符串
//
// 上层据此决定要不要展示环比百分比，而不是简单地"有上期就比"。
//
// # 节假日表是内置的，不是 API
//
// 内置的三个理由：
//  1. 节假日是**每年由国务院办公厅发文**的确定数据，一年最多改 1~2 次；
//  2. 外部 API 会在最需要它的时候（春节前后网络高峰）超时；
//  3. 它是**合规要求准确**的数据，第三方 API 的错误会直接变成错误的经营建议。
//
// 代价是每年初需要更新一次数据文件（见 DataVersion 与下面的注释），
// 这是有意的取舍：宁可一年手工维护一次，也不要每天依赖一个可能挂掉的外部服务。
package calendar

import (
	"fmt"
	"time"
)

// DateLayout 是全系统统一的营业日期格式，与思迅源库 oper_date 一致。
const DateLayout = "2006-01-02"

// DayKind 是这一天的性质。
type DayKind string

const (
	// KindWorkday 普通工作日（周一至周五，且不在法定假日内）
	KindWorkday DayKind = "workday"
	// KindWeekend 普通周末（周六/周日，且不是调休上班日）
	KindWeekend DayKind = "weekend"
	// KindHoliday 法定节假日（休息）
	KindHoliday DayKind = "holiday"
	// KindMakeupWorkday **调休上班日**：日历上是周六/周日，但实际要上班。
	//
	// ⚠️ 这是零售分析里最容易被忽略、也最致命的一类。
	// 纯靠 time.Weekday() 判断的实现，会把"国庆调休上班的周六"
	// 归入周末，于是拿它和普通工作日比 —— 而这两天的客流可能差三倍。
	KindMakeupWorkday DayKind = "makeup_workday"
)

// Day 是一天的判定结果。
type Day struct {
	Date string  `json:"date"`
	Kind DayKind `json:"kind"`
	// Name 是节日名（如 "国庆节"）或空串。
	Name string `json:"name,omitempty"`
	// Weekday 是中文星期名。
	Weekday string `json:"weekday"`
	// IsRestDay 表示"这天不上班"（周末 + 法定假日 + 非调休日）。
	IsRestDay bool `json:"is_rest_day"`
}

// ComparableKind 描述两天在多大程度上可以直接比。
type ComparableKind string

const (
	// ComparableGood 两天的性质相同（都是普通工作日，或都是同类休息日）
	ComparableGood ComparableKind = "comparable"
	// ComparableLimited 性质不同，环比数字要谨慎解读
	ComparableLimited ComparableKind = "limited"
)

// Comparability 是两天的可比性判定。
type Comparability struct {
	Kind ComparableKind `json:"kind"`
	// Reason 是**面向店长的白话**，不是给程序看的枚举名。
	//
	// 它的用途是直接印在卡片上，所以必须具体：
	// "昨天是法定假日，今天是普通工作日，两天客流差异大，
	//  涨跌幅不能直接当作经营变好或变坏"  —— 这句话店长看得懂并知道该怎么做。
	// 而"comparable_limited: kind_mismatch" 这种字符串对店长毫无意义。
	Reason string `json:"reason"`
}

// Context 是某个统计区间的日历上下文。
type Context struct {
	From    Day `json:"from"`
	To      Day `json:"to"`
	// Prev 是与本期对比的上期区间。
	PrevFrom Day `json:"prev_from"`
	PrevTo   Day `json:"prev_to"`

	// Comparability 是本期 vs 上期的可比性。
	Comparability Comparability `json:"comparability"`
}

// Parse 解析 YYYY-MM-DD。
func Parse(s string) (time.Time, error) {
	t, err := time.Parse(DateLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("calendar: 解析日期 %q 失败(要求 YYYY-MM-DD): %w", s, err)
	}
	return t, nil
}

// Format 格式化日期。
func Format(t time.Time) string { return t.Format(DateLayout) }

// weekdayCN 把 Go 的 Weekday 转中文。
//
// ⚠️ Go 的 time.Weekday：Sunday=0。中文习惯是"周一"到"周日"，
// 直接 fmt 会得到 "Saturday" 这种英文且顺序错乱。
func weekdayCN(t time.Time) string {
	// 周一=1 … 周日=7（中文习惯）
	idx := (int(t.Weekday()) + 6) % 7
	return [...]string{"周一", "周二", "周三", "周四", "周五", "周六", "周日"}[idx]
}

// Describe 返回某天的判定结果。
func Describe(date string) (Day, error) {
	t, err := Parse(date)
	if err != nil {
		return Day{}, err
	}
	d := Day{
		Date:    date,
		Weekday: weekdayCN(t),
	}

	// 顺序很重要：调休上班日必须**先于**周末判定。
	// 若先判周末，2026-10-09（国庆调休上班的周五）没问题，
	// 但 2026-09-27 这类"周末调休上班"就会被误判成休息日。
	if names, ok := makeupWorkdays[date]; ok {
		d.Kind = KindMakeupWorkday
		d.IsRestDay = false
		if len(names) > 0 {
			d.Name = names[0]
		}
		return d, nil
	}
	if names, ok := holidays[date]; ok {
		d.Kind = KindHoliday
		d.IsRestDay = true
		if len(names) > 0 {
			d.Name = names[0]
		}
		return d, nil
	}
	if t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
		d.Kind = KindWeekend
		d.IsRestDay = true
		return d, nil
	}
	d.Kind = KindWorkday
	d.IsRestDay = false
	return d, nil
}

// Compare 判定本期(from~to)与上期(prevFrom~prevTo)的可比性。
//
// # 判定规则（按重要性排序）
//
//  1. **区间天数不同** → limited。7 天 vs 3 天，金额不可直接比。
//     这是最容易犯也最容易被忽略的错误：周报拿"本周到今天"（可能只有 3 天）
//     去比"上周同期"（7 天），金额必然下降 60%，而这毫无经营含义。
//     ⚠️ 上层**必须**用同期口径（见 Period.PrevRange）而不是自然上期。
//  2. **区间内含节假日性质差异** → limited。
//  3. 区间都只是普通工作日 → comparable。
func Compare(cur, prev Range) Comparability {
	curDays := cur.Days()
	prevDays := prev.Days()
	if curDays != prevDays {
		return Comparability{
			Kind: ComparableLimited,
			Reason: fmt.Sprintf("本期 %d 天、上期 %d 天，天数不同，"+
				"金额涨跌不能直接当作经营变好或变坏", curDays, prevDays),
		}
	}

	// 两段区间里任一天性质不同，就不可比。
	// 取"差异最大的一天"来说明，而不是笼统地说"存在差异" ——
	// 店长需要知道差在哪，才能判断该信几分。
	for _, day := range cur.Dates() {
		pd, err := Describe(day)
		if err != nil {
			continue
		}
		prevDay := prevAt(day, cur, prev)
		cd, err := Describe(prevDay)
		if err != nil {
			continue
		}
		if kindWeight(pd.Kind) != kindWeight(cd.Kind) {
			return Comparability{
				Kind: ComparableLimited,
				Reason: fmt.Sprintf("%s 是%s，%s 是%s，"+
					"两天客流基础不同，涨跌幅要谨慎解读",
					day, describeKind(pd), prevDay, describeKind(cd)),
			}
		}
	}
	return Comparability{Kind: ComparableGood, Reason: "本期与上期均为同类日期，可直接对比"}
}

// kindWeight 给日期性质一个可比较的"客流基准"权重。
//
// 为什么要权重而不是直接比 Kind 字符串：法定节假日之间也分高低
// （春节 > 国庆 > 普通周末），纯枚举比较只能回答"同不同类"。
// 这里的权重是**相对客流**的粗略刻画，用于识别"同类但强度差很多"。
func kindWeight(k DayKind) int {
	switch k {
	case KindWorkday:
		return 10
	case KindMakeupWorkday:
		// 调休上班日客流介于工作日与周末之间
		return 20
	case KindWeekend:
		return 30
	case KindHoliday:
		// 法定假日客流最高（对商超而言）
		return 40
	}
	return 0
}

// describeKind 把日期性质转成店长看得懂的话。
func describeKind(d Day) string {
	switch d.Kind {
	case KindWorkday:
		return "普通工作日"
	case KindMakeupWorkday:
		if d.Name != "" {
			return "调休上班日（" + d.Name + "）"
		}
		return "调休上班日"
	case KindWeekend:
		return "周末"
	case KindHoliday:
		if d.Name != "" {
			return "法定假日（" + d.Name + "）"
		}
		return "法定假日"
	}
	return string(d.Kind)
}

// prevAt 找到与 cur 中某一天相对应的上期日期（按天偏移对齐）。
func prevAt(date string, cur, prev Range) string {
	from, err := Parse(date)
	if err != nil {
		return date
	}
	offset := from.Sub(cur.Start) / (24 * time.Hour)
	return Format(prev.Start.Add(offset * 24 * time.Hour))
}
