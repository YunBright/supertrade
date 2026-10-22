package service

import (
	"testing"
	"time"
)

// TestDayStartEnd_AcceptsBothDateAndRFC3339 锁住"时间边界归一"这条契约。
//
// ⚠️ 2026-10-23 生产实测:前端(Flutter)发的是 **RFC3339 UTC**
// (DateX.toIsoUtc ⇒ "2026-10-21T16:00:00Z"),而源库(思迅 MSSQL)存的是
// **本地墙钟**(datetime,不带时区,值形如 "2026-10-22 21:54:52")。
// ⇒ 不归一就是**差 8 小时**,流水被切掉一整天。
func TestDayStartEnd_AcceptsBothDateAndRFC3339(t *testing.T) {
	SetBizLoc(time.FixedZone("CST", 8*3600))
	defer SetBizLoc(mustLoadBizLoc("Asia/Shanghai"))

	cases := []struct {
		name        string
		in          string
		wantStart   string
		wantEnd     string
	}{
		{
			name:      "只有日期 ⇒ 补当天首末刻",
			in:        "2026-10-22",
			wantStart: "2026-10-22 00:00:00",
			wantEnd:   "2026-10-22 23:59:59",
		},
		{
			name:      "已经是墙钟格式 ⇒ 原样",
			in:        "2026-10-22 08:30:00",
			wantStart: "2026-10-22 08:30:00",
			wantEnd:   "2026-10-22 08:30:00",
		},
		{
			// 2026-10-21T16:00:00Z == 2026-10-22 00:00:00 +08:00
			name:      "RFC3339 UTC ⇒ 转成业务时区墙钟",
			in:        "2026-10-21T16:00:00Z",
			wantStart: "2026-10-22 00:00:00",
			wantEnd:   "2026-10-22 00:00:00",
		},
		{
			name:      "RFC3339 带偏移 ⇒ 同样按业务时区归一",
			in:        "2026-10-22T09:15:00+08:00",
			wantStart: "2026-10-22 09:15:00",
			wantEnd:   "2026-10-22 09:15:00",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dayStart(tc.in); got != tc.wantStart {
				t.Fatalf("dayStart(%q) = %q, want %q", tc.in, got, tc.wantStart)
			}
			if got := dayEnd(tc.in); got != tc.wantEnd {
				t.Fatalf("dayEnd(%q) = %q, want %q", tc.in, got, tc.wantEnd)
			}
		})
	}
}

// TestNormalizeToWallClock_KeepsUnparsable 锁住"解析不出来就原样返回"。
//
// ⚠️ 不能在这里悄悄改写成一个**猜出来的**值 ——
// 猜错的话症状是"流水少了/多了",而且没有任何报错。
func TestNormalizeToWallClock_KeepsUnparsable(t *testing.T) {
	SetBizLoc(time.FixedZone("CST", 8*3600))
	defer SetBizLoc(mustLoadBizLoc("Asia/Shanghai"))

	const in = "not-a-date"
	if got := normalizeToWallClock(in); got != in {
		t.Fatalf("normalizeToWallClock(%q) = %q, want原样", in, got)
	}
	if got := normalizeToWallClock(""); got != "" {
		t.Fatalf("normalizeToWallClock(\"\") = %q, want空", got)
	}
}

// TestQueries_NoRangeDefaultsToToday 钉住"不给区间 ⇒ 补今天"。
//
// ⚠️ 为什么要有这个默认(2026-10-23 生产实测):
//
//	流水表是**逐笔**的(t_rm_saleflow 330 万行 / t_rm_payflow 114 万行),
//	不带区间就是**全表扫 + 排序** ⇒ 必然撞 cube 的 10s 超时 ⇒
//	端点返 503「数据源暂不可用」。
//
//	⇒ 而 503 的文案完全指不到真因("查询太大"被说成"数据源挂了"),
//	  排查会去查 cube 的健康度 —— 真因只是"没给时间区间"。
//
// ⇒ 默认"今天"(与思迅老系统一致:图2 的默认就是 00:00 → 23:59)。
// ⇒ **两条路径都补**(本域 PG 与 cube):只给 cube 补 ⇒
//	同一个查询在两个门店给出不同答案,而"换个门店就换个答案"是本仓最忌讳的问题。
func TestQueries_NoRangeDefaultsToToday(t *testing.T) {
	SetBizLoc(time.FixedZone("CST", 8*3600))
	defer SetBizLoc(mustLoadBizLoc("Asia/Shanghai"))

	today := time.Now().In(bizLoc).Format("2006-01-02")

	sq := SaleQuery{}.normalized()
	if sq.From != today || sq.To != today {
		t.Fatalf("SaleQuery.normalized() 区间 = (%q,%q), want (%q,%q)", sq.From, sq.To, today, today)
	}
	pq := PayQuery{}.normalized()
	if pq.From != today || pq.To != today {
		t.Fatalf("PayQuery.normalized() 区间 = (%q,%q), want (%q,%q)", pq.From, pq.To, today, today)
	}

	// 给了任意一端 ⇒ **不动**(半开区间是有意义的用法)。
	sq2 := SaleQuery{From: "2026-01-01"}.normalized()
	if sq2.From != "2026-01-01" || sq2.To != "" {
		t.Fatalf("给了 from 时不该补区间, got (%q,%q)", sq2.From, sq2.To)
	}
}
