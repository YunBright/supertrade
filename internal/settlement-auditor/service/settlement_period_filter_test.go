package service

import (
	"strings"
	"testing"
)

// TestSettlementPeriodFilters_UsesPeriodEndNotSettledAt 钉住账期区间打在**哪一列**。
//
// 为什么这条必须放在这里而不是消费端:缺陷的形态是"筛了另一列",
// 而那一列在源库里几乎全是 NULL(ysx 实测 4379 张里只有 1 张有 settle_date),
// 所以**任何断言"过滤条件有没有带上"的测试都会绿** —— 过滤带了,只是带错了列。
// 唯一能观测到错误的位置就是构造 filter 的这一行。
//
// 期望值写死字面量,不从常量拼接 —— 否则把列名改回 settled_at 时断言会跟着变。
func TestSettlementPeriodFilters_UsesPeriodEndNotSettledAt(t *testing.T) {
	got := settlementPeriodFilters("2026-09-01", "2026-10-01")
	if len(got) != 2 {
		t.Fatalf("应有 2 条(下界 + 上界),got %d: %#v", len(got), got)
	}
	want := []struct{ member, operator, value string }{
		{"settlement.period_end", "gte", "2026-09-01"},
		{"settlement.period_end", "lt", "2026-10-01"},
	}
	for i, w := range want {
		if got[i].Member != w.member {
			t.Errorf("第 %d 条 member = %q,应为 %q", i, got[i].Member, w.member)
		}
		if got[i].Operator != w.operator {
			t.Errorf("第 %d 条 operator = %q,应为 %q", i, got[i].Operator, w.operator)
		}
		if len(got[i].Values) != 1 || got[i].Values[0] != w.value {
			t.Errorf("第 %d 条 values = %#v,应为 [%s]", i, got[i].Values, w.value)
		}
	}
}

// TestSettlementPeriodFilters_NeverMentionsSettledAt 是上一条的反向断言。
//
// 单独写一条,是为了让"误用结算日"这个具体错误在失败信息里直说,
// 而不是混在"member 不等于 period_end"里让人自己去猜。
func TestSettlementPeriodFilters_NeverMentionsSettledAt(t *testing.T) {
	for _, c := range []struct{ from, to string }{
		{"2026-09-01", "2026-10-01"},
		{"2026-09-01", ""},
		{"", "2026-10-01"},
		{"", ""},
	} {
		for _, f := range settlementPeriodFilters(c.from, c.to) {
			if strings.Contains(f.Member, "settled_at") {
				t.Errorf("账期过滤不得碰 settled_at(结算日),实际 member = %q", f.Member)
			}
		}
	}
}

// TestSettlementPeriodFilters_HalfOpenBounds 锁住区间是 [from, to) 半开。
//
// 闭区间会多吃一天 —— 实测 2026-09 窗口:
// 闭区间 [09-01, 09-30] 113 张;若误用 to=10-01 的闭区间会变成 122 张,
// 多出来的 9 张正是 10-01 当天的单。这不是洁癖,是财务口径。
func TestSettlementPeriodFilters_HalfOpenBounds(t *testing.T) {
	got := settlementPeriodFilters("2026-09-01", "2026-10-01")
	if got[0].Operator != "gte" {
		t.Errorf("下界应用 >=(闭下界),实际 %q", got[0].Operator)
	}
	// 上界必须是 lt,不是 lte —— 否则 [from,to) 退化成 [from,to]。
	if got[1].Operator != "lt" {
		t.Errorf("上界应用 <(半开),实际 %q", got[1].Operator)
	}
}

// TestSettlementPeriodFilters_EmptyBoundsProduceNoFilter 空串 = 不限。
//
// 这条不是废话:若空串也产出一条 filter,就会给每次不带账期的查询
// 挂上一个值恒为空的谓词,既拖慢查询又让"没筛选"与"筛了但没命中"分不清。
func TestSettlementPeriodFilters_EmptyBoundsProduceNoFilter(t *testing.T) {
	if got := settlementPeriodFilters("", ""); len(got) != 0 {
		t.Errorf("两端都空时不应产生任何 filter,got %#v", got)
	}
	if got := settlementPeriodFilters("2026-09-01", ""); len(got) != 1 {
		t.Errorf("只给下界时应只有 1 条,got %#v", got)
	}
	if got := settlementPeriodFilters("", "2026-10-01"); len(got) != 1 {
		t.Errorf("只给上界时应只有 1 条,got %#v", got)
	}
}
