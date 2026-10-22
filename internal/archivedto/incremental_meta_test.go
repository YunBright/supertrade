// Package archivedto —— 增量能力的元数据守卫。
//
// # 测什么
//
//	① **能增量**的 scope 与**源数据滞后**的登记必须**一一对应**
//	② 增量白名单里不能出现"不能增量"的 scope
//
// # 为什么①要紧
//
//	`ChangedMembers` 说"这个 scope 能按变动时间过滤",
//	`ChangedMemberLag` 说"它的源数据最多落后多久"。
//
//	**漏登记 lag** ⇒ 那个 scope 的水位按"导出时刻"记
//	⇒ 源是离线快照(cube 的 DuckDB)时,刷新延迟里的变更**永远落在水位之前**
//	⇒ **永久漏数据**,而且报告照样写"成功 N 条"。
//
//	⚠️ 这类缺陷**没有任何报错** ⇒ 只能靠这条测试在配置期拦住。
package archivedto_test

import (
	"testing"

	"github.com/YunBright/supertrade/internal/archivedto"
)

// TestChangedMemberLagCoversEveryIncrementalScope 锁"每个能增量的 scope 都登记了滞后"。
func TestChangedMemberLagCoversEveryIncrementalScope(t *testing.T) {
	for scope := range archivedto.ChangedMembers {
		if _, ok := archivedto.ChangedMemberLag[scope]; !ok {
			t.Errorf("scope %q 在 ChangedMembers 里(能增量),但没在 ChangedMemberLag 里登记源数据滞后 ⇒ "+
				"水位会按'导出时刻'记 ⇒ 源是离线快照时刷新延迟里的变更**永远漏掉**,且零报错", scope)
		}
	}
	// 反向:lag 表里不该有"不能增量"的 scope(那是死配置,会误导排障)。
	for scope := range archivedto.ChangedMemberLag {
		if _, ok := archivedto.ChangedMembers[scope]; !ok {
			t.Errorf("scope %q 在 ChangedMemberLag 里,但它**不能**做增量 ⇒ 死配置", scope)
		}
	}
}

// TestDuckSourceScopesHaveLag 锁"非 live 源的 scope 必须有非零滞后"。
//
// ⚠️ 2026-10-22:cube 去掉 DuckDB 后**所有 model 都是 live**（查询实时透传源库）
//   ⇒ 不再有"快照滞后"这回事 ⇒ 全部登记为 0。
//
// ⇒ 这条测试现在的作用是：**将来若某个 scope 真的引入了缓存/物化**，
//   必须同步在这里登记滞后，否则会**永久漏数据且零报错**。
//
//   判据：滞后 > 0 的 scope **必须**在下面这份"已知有滞后"名单里；
//   反之，名单里的 scope 若滞后变成 0 ⇒ 要么是去掉了滞后（合理），
//   要么是**忘了改这里的语义**（要人来看一眼）。
func TestDuckSourceScopesHaveLag(t *testing.T) {
	// 已知"源有延迟"的 scope(cube 引入了缓存/物化时登记到这里)。
	knownLagged := map[string]bool{}

	for scope, lag := range archivedto.ChangedMemberLag {
		if lag > 0 && !knownLagged[scope] {
			t.Errorf("scope %q 登记了 %d 秒滞后,但它不在'已知有滞后'名单里 ⇒ "+
				"要么是登记错了,要么是引入滞后的原因没记下来(将来会漏数据且零报错)",
				scope, lag)
		}
	}
}

// TestStockIsLiveSourceHasZeroLag 锁"live 源(stock)的滞后是 0"。
//
// ⚠️ 若给它加了大滞后 ⇒ 每轮都会重导一大片历史(白搬)。
func TestStockIsLiveSourceHasZeroLag(t *testing.T) {
	if got := archivedto.ChangedMemberLagOf(archivedto.ScopeStock); got != 0 {
		t.Errorf("stock 是 live 源(直接查思迅源库)⇒ 滞后应为 0,实际 %d 秒", got)
	}
}
