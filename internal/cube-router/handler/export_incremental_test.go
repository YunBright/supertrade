// Package handler —— 增量导出过滤的单元测试。
//
// # 测什么(对着症状写)
//
//	① 带 since ⇒ cube 查询加 `变动时间 gt <since>` 过滤
//	② 不带 since ⇒ 不加该过滤(全量)
//	③ 没声明"变动时间成员"的 scope ⇒ **即使**带 since 也不加过滤
//	④ 导出端的成员名与 archivedto.ChangedMembers **逐字一致**
//
// # 为什么③是最要紧的一条
//
//	"给没声明的 scope 乱加时间过滤"⇒ **永远导不出行**(而且不报错)。
//	症状是"导出成功 0 行",而报告与日志都看不出异常。
//
// # 为什么④是跨仓的守卫
//
//	cube-router(导出端)与 cubesync(编排端)**必须**对"哪些 scope 能增量"
//	有同一份认知。两边各写一份时,改了一边不会编译报错 ⇒
//	症状是"编排端推进了水位,而导出端没加过滤" ⇒ **漏数据且零报错**。
package handler

import (
	"testing"

	"github.com/YunBright/supertrade/internal/archivedto"
)

// TestExportScope_ChangedMemberMatchesSharedTable 锁"导出端与编排端同一份认知"。
func TestExportScope_ChangedMemberMatchesSharedTable(t *testing.T) {
	for name, sc := range exportScopes {
		want := archivedto.ChangedMembers[name]
		if sc.ChangedMember != want {
			t.Errorf("scope %q 的变动成员应为 %q,实际 %q"+
				"(两边各写一份时,改了一边不会编译报错 ⇒ 水位被推进而过滤没加 ⇒ 漏数据)",
				name, want, sc.ChangedMember)
		}
	}
	// 反向:能力表里声明能增量的 scope,导出端必须**真的**声明了成员。
	for scope, member := range archivedto.ChangedMembers {
		sc, ok := exportScopes[scope]
		if !ok {
			t.Errorf("能力表里的 scope %q 在导出端不存在", scope)
			continue
		}
		if sc.ChangedMember != member {
			t.Errorf("scope %q 的变动成员应为 %q,实际 %q", scope, member, sc.ChangedMember)
		}
	}
}

// TestExportScopeFilters_SinceProducesGtFilter 锁"带 since ⇒ 加 gt 过滤"。
func TestExportScopeFilters_SinceProducesGtFilter(t *testing.T) {
	sc := exportScopes[archivedto.ScopeStock]
	filters := exportScopeFilters(sc, "2026-10-22", "2026-10-22 10:00:00")

	var found bool
	for _, f := range filters {
		if f.Member == sc.ChangedMember {
			found = true
			if f.Operator != "gt" {
				t.Errorf("变动过滤的算子应为 gt,实际 %q", f.Operator)
			}
			if len(f.Values) != 1 || f.Values[0] != "2026-10-22 10:00:00" {
				t.Errorf("变动过滤的值应为水位本身,实际 %v", f.Values)
			}
		}
	}
	if !found {
		t.Errorf("带 since 时必须加 %q 的 gt 过滤,实际 filters=%v", sc.ChangedMember, filters)
	}
}

// TestExportScopeFilters_NoSinceNoFilter 锁"不带 since ⇒ 不加变动过滤"。
func TestExportScopeFilters_NoSinceNoFilter(t *testing.T) {
	sc := exportScopes[archivedto.ScopeStock]
	filters := exportScopeFilters(sc, "2026-10-22", "")
	for _, f := range filters {
		if f.Member == sc.ChangedMember {
			t.Errorf("不带 since 时不该加变动过滤,实际 filters=%v", filters)
		}
	}
}

// TestExportScopeFilters_ScopeWithoutChangedMemberIgnoresSince 锁"没声明就不加过滤"。
//
// ⚠️ 症状(若乱加):那个 scope **永远导不出行**,而且不报错。
func TestExportScopeFilters_ScopeWithoutChangedMemberIgnoresSince(t *testing.T) {
	sc := exportScopes[archivedto.ScopeProducts]
	if sc.ChangedMember != "" {
		t.Skip("products 已声明变动成员,本用例的前提不成立")
	}
	filters := exportScopeFilters(sc, "2026-10-22", "2026-10-22 10:00:00")
	for _, f := range filters {
		if f.Operator == "gt" {
			t.Errorf("没声明变动成员的 scope 不该加 gt 过滤,实际 filters=%v", filters)
		}
	}
}

// TestExportScopeFilters_PromotionsKeepsDateRange 锁"时效过滤与变动过滤是两回事"。
//
// 促销是"在基准日**有效**"(inDateRange),库存是"在基准日**变过**"(gt)
// ⇒ 两套过滤不能互相顶替。
func TestExportScopeFilters_PromotionsKeepsDateRange(t *testing.T) {
	sc := exportScopes[archivedto.ScopePromotions]
	filters := exportScopeFilters(sc, "2026-10-22", "")
	var inRange int
	for _, f := range filters {
		if f.Operator == "inDateRange" {
			inRange++
		}
	}
	if inRange != 2 {
		t.Errorf("促销必须保留两个 inDateRange 过滤,实际 %d 个: %v", inRange, filters)
	}
}
