// Package cubesync —— 增量水位存储的单元测试。
//
// # 测什么(对着症状写)
//
//	① 水位文件丢了/坏了 ⇒ 必须退化成全量(**绝不猜一个时间**)
//	② 水位推进 ⇒ 下一次导出必须带上它
//	③ 定期全量校准 ⇒ 距上次全量太久的那一轮必须无视水位
//	④ scope 之间隔离 ⇒ 库存的水位不能影响商品
//
// # 为什么"绝不猜时间"是硬要求
//
//	猜**太新** ⇒ 漏数据(永久漂移,而且没有任何报错);
//	猜**太旧** ⇒ 多导(幂等,无害)。
//	⇒ 宁可多导一万倍,也不能漏一行。
package cubesync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/archivedto"
)

// fixedNow 是测试用的固定时刻(2026-10-22 12:00:00 本地)。
func fixedNow() time.Time {
	return time.Date(2026, 10, 22, 12, 0, 0, 0, time.Local)
}

// newTestWatermark 造一个水位存储(临时文件 + 固定时钟)。
func newTestWatermark(t *testing.T, overlapSec, maxAgeDays int) (*WatermarkStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "watermark.json")
	return NewWatermarkStore(path, overlapSec, maxAgeDays, func() time.Time { return fixedNow() }), path
}

// TestWatermark_MissingFileMeansNoWatermark 锁"文件不存在 ⇒ 没有水位"。
//
// 症状(如果猜一个时间):水位凭空出现 ⇒ 那一段窗口的数据**永远导不到**。
func TestWatermark_MissingFileMeansNoWatermark(t *testing.T) {
	w, _ := newTestWatermark(t, 5, 7)
	if got := w.Get(archivedto.ScopeStock); got != "" {
		t.Errorf("文件不存在时水位必须为空,实际 %q", got)
	}
	// 从没做过全量校准 ⇒ 这一轮必须全量。
	if !w.ShouldFullRefresh(archivedto.ScopeStock) {
		t.Error("从没全量校准过 ⇒ 必须要求全量")
	}
}

// TestWatermark_CorruptFileMeansNoWatermark 锁"文件坏了 ⇒ 没有水位"。
//
// ⚠️ 宁可全量也不猜 —— 猜太新的后果是永久漏数据。
func TestWatermark_CorruptFileMeansNoWatermark(t *testing.T) {
	w, path := newTestWatermark(t, 5, 7)
	if err := os.WriteFile(path, []byte("{ 这不是 json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := w.Get(archivedto.ScopeStock); got != "" {
		t.Errorf("文件损坏时水位必须为空,实际 %q", got)
	}
	if !w.ShouldFullRefresh(archivedto.ScopeStock) {
		t.Error("文件损坏 ⇒ 读不到上次校准时间 ⇒ 必须要求全量")
	}
}

// TestWatermark_SetThenGet 锁"写入 ⇒ 读回"。
func TestWatermark_SetThenGet(t *testing.T) {
	w, _ := newTestWatermark(t, 0, 7)
	w.Set(archivedto.ScopeStock, "2026-10-22 10:00:00")
	if got := w.Get(archivedto.ScopeStock); got != "2026-10-22 10:00:00" {
		t.Errorf("水位应读回 %q,实际 %q", "2026-10-22 10:00:00", got)
	}
}

// TestWatermark_ScopesAreIsolated 锁"scope 之间隔离"。
//
// 症状(如果串了):库存的水位被商品用上 ⇒ 商品只导"最近变过的一部分"
// ⇒ **漏商品**,而报告说"成功"。
func TestWatermark_ScopesAreIsolated(t *testing.T) {
	w, _ := newTestWatermark(t, 0, 7)
	w.Set(archivedto.ScopeStock, "2026-10-22 10:00:00")
	if got := w.Get(archivedto.ScopeProducts); got != "" {
		t.Errorf("商品的水位必须为空,实际 %q", got)
	}
}

// TestWatermark_NowBacksOffByOverlap 锁"水位回退 overlap 秒"。
//
// ⚠️ 源库的 oper_date 精确到秒 ⇒ 同一秒内的两次更新,
//   "gt 上次水位"会漏掉后一次 ⇒ 回退几秒 ⇒ 那一秒被重导一次(幂等 ⇒ 无害)。
func TestWatermark_NowBacksOffByOverlap(t *testing.T) {
	w, _ := newTestWatermark(t, 5, 7)
	got := w.Now()
	want := fixedNow().Add(-5 * time.Second).Format(archivedto.SinceLayout)
	if got != want {
		t.Errorf("水位起点应为 %q,实际 %q", want, got)
	}
}

// TestWatermark_NowAtSubtractsSourceLag 锁"水位还要减去**源数据滞后**"。
//
// # 为什么这是"不漏"的关键
//
//	源是 cube 的**离线 DuckDB 快照**(每 30 分钟增量刷新一次)时,
//	导出那一刻它可能还没刷新到最新 ⇒ 水位若按"导出时刻"记,
//	那 30 分钟里的变更就**永远落在水位之前** ⇒ 永久漏,且零报错。
//
//	⇒ 水位 = 导出起点 − 源数据滞后。
func TestWatermark_NowAtSubtractsSourceLag(t *testing.T) {
	w, _ := newTestWatermark(t, 5, 7)
	// live 源(stock,lag=0)⇒ 只回退 overlap。
	want := fixedNow().Add(-5 * time.Second).Format(archivedto.SinceLayout)
	if got := w.NowAt(0); got != want {
		t.Errorf("lag=0 时水位应为 %q,实际 %q", want, got)
	}
	// duck 源(products,lag=24h)⇒ 再回退 24 小时。
	wantDuck := fixedNow().Add(-5*time.Second - 24*time.Hour).Format(archivedto.SinceLayout)
	if got := w.NowAt(24 * 3600); got != wantDuck {
		t.Errorf("lag=24h 时水位应为 %q,实际 %q", wantDuck, got)
	}
	// 负数 lag 必须被夹成 0(不能把水位推到未来 ⇒ 那会让下一轮**全漏**)。
	if got := w.NowAt(-100); got != want {
		t.Errorf("负 lag 必须夹成 0,实际 %q", got)
	}
}

// TestWatermark_ShouldFullRefresh 锁"定期全量校准"。
//
// 症状(如果不校准):增量漏掉的行**永远补不回来**
// ⇒ 库存慢慢偏掉,而且没有任何一行报错。
func TestWatermark_ShouldFullRefresh(t *testing.T) {
	w, _ := newTestWatermark(t, 0, 7)

	// 从没校准过 ⇒ 全量。
	if !w.ShouldFullRefresh(archivedto.ScopeStock) {
		t.Fatal("从没校准过必须要求全量")
	}
	// 刚校准过 ⇒ 增量。
	w.MarkFullSync(archivedto.ScopeStock, fixedNow().Format(archivedto.SinceLayout))
	if w.ShouldFullRefresh(archivedto.ScopeStock) {
		t.Error("刚全量校准过不应要求全量")
	}
	// 8 天前校准过(超过 7 天)⇒ 全量。
	w.MarkFullSync(archivedto.ScopeStock,
		fixedNow().Add(-8*24*time.Hour).Format(archivedto.SinceLayout))
	if !w.ShouldFullRefresh(archivedto.ScopeStock) {
		t.Error("距上次全量超过 7 天必须要求全量")
	}
}

// TestWatermark_MarkFullSyncIsSeparateFromWatermark 锁"两件事分开记"。
//
// ⚠️ 把"全量校准时间"与"水位"混成一件事 ⇒
//   要么校准永远不发生(漏的行永远缺着),
//   要么每轮都被当成全量(增量形同虚设)。
func TestWatermark_MarkFullSyncIsSeparateFromWatermark(t *testing.T) {
	w, path := newTestWatermark(t, 0, 7)
	w.Set(archivedto.ScopeStock, "2026-10-22 10:00:00")
	w.MarkFullSync(archivedto.ScopeStock, "2026-10-22 04:30:00")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "full_sync_at") || !strings.Contains(string(raw), "watermarks") {
		t.Errorf("水位文件必须分开记两个 map,实际内容:\n%s", raw)
	}
	if got := w.Get(archivedto.ScopeStock); got != "2026-10-22 10:00:00" {
		t.Errorf("水位被全量校准时间覆盖了,实际 %q", got)
	}
}

// TestWatermark_DisabledStoreIsAlwaysFull 锁"没配水位文件 ⇒ 全走全量"。
//
// ⚠️ 这是**安全默认**:上线时"忘了配"的后果是慢,不是漏数据。
func TestWatermark_DisabledStoreIsAlwaysFull(t *testing.T) {
	w := NewWatermarkStore("", 5, 7, func() time.Time { return fixedNow() })
	if w.Enabled() {
		t.Error("空路径不该启用水位")
	}
	w.Set(archivedto.ScopeStock, "2026-10-22 10:00:00")
	if got := w.Get(archivedto.ScopeStock); got != "" {
		t.Errorf("未启用时 Get 必须为空,实际 %q", got)
	}
	if !w.ShouldFullRefresh(archivedto.ScopeStock) {
		t.Error("未启用时必须要求全量")
	}
}

// TestWatermark_NoTempFileLeftBehind 锁"原子写不留临时文件"。
//
// 症状:进程被杀留下 .tmp ⇒ 下次读水位读到旧文件(还是能读,但难排障)。
func TestWatermark_NoTempFileLeftBehind(t *testing.T) {
	w, path := newTestWatermark(t, 0, 7)
	w.Set(archivedto.ScopeStock, "2026-10-22 10:00:00")
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("不该留下临时文件 %s", path+".tmp")
	}
}
