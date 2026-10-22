// Package cubesync —— 增量同步的单元测试。
//
// # 测什么(对着症状写)
//
//	① 增量轮次**带**水位;全量轮次**不带**
//	② 增量轮次 0 行 = "没有变动"(**不是**失败);全量轮次 0 行 = 失败
//	③ 导入失败 ⇒ 水位**不动**(下次重来)
//	④ 不在白名单的 scope 一律全量
//	⑤ 定期全量校准:距上次全量太久的那一轮无视水位
//
// # 为什么②最重要
//
//	源库实测:库存 23837 行,**一天只变 91 行**
//	⇒ 绝大多数夜晚增量窗口里就是 **0 行**。
//	若把 0 行当失败 ⇒ 每晚同步都红 ⇒
//	症状是"同步总失败",而真相是"今天确实没变动"。
//
// # 为什么③最重要
//
//	水位推进 = "我导到这里了"。导入失败却推进了水位
//	⇒ 那批数据**永远不会被再导**(下次只导"之后变过的")
//	⇒ **永久漏数据**,而且全程零报错。
package cubesync

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/archivedto"
)

// incrClock 是增量测试用的固定时钟。
func incrClock() time.Time {
	return time.Date(2026, 10, 22, 12, 0, 0, 0, time.Local)
}

// newIncrSyncer 造一个**可控时钟**的 Syncer。
func newIncrSyncer(cfg Config, ex Exporter, im Importer) *Syncer {
	s := NewSyncer(cfg, fakeToken{token: "T"}, ex, []Owner{
		{Importer: im, Scopes: DefaultCatalogScopes()},
		{Importer: im, Scopes: DefaultPricingScopes()},
		{Importer: im, Scopes: DefaultInventoryScopes()},
		{Importer: im, Scopes: DefaultMemberScopes()},
	}, nil)
	s.chunkSize = 100
	s.now = func() time.Time { return incrClock() }
	s.watermarks = NewWatermarkStore(cfg.WatermarkFile,
		cfg.WatermarkOverlapSeconds, cfg.WatermarkMaxAgeDays, s.now)
	return s
}

// incrCfg 造一份"库存走增量"的配置。
func incrCfg(t *testing.T, scopes []string) Config {
	t.Helper()
	return Config{
		SourceBranchID: "00", TargetBranchID: "10", Mode: "overwrite",
		Scopes:               scopes,
		IncrementalScopes:    []string{archivedto.ScopeStock},
		WatermarkFile:        filepath.Join(t.TempDir(), "watermark.json"),
		WatermarkOverlapSeconds: 5,
		WatermarkMaxAgeDays:  7,
		PageSize:             100,
	}
}

// stockRows 造 N 行库存导出数据。
func stockRows(n int) []map[string]any {
	out := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, map[string]any{
			"product_id": "P" + string(rune('A'+i)), "qty": "10", "avg_cost_yuan": "7.10",
		})
	}
	return out
}

// TestSync_IncrementalPassesWatermark 锁"第二轮必须带上水位"。
//
// 症状(如果不带):每一轮都是全量 ⇒ 增量形同虚设,
// 而报告与日志里**看不出任何区别**。
func TestSync_IncrementalPassesWatermark(t *testing.T) {
	ex := &fakeExporter{pages: map[string][]map[string]any{
		archivedto.ScopeStock: stockRows(3),
	}}
	im := &fakeImporter{}
	cfg := incrCfg(t, []string{archivedto.ScopeStock})

	s := newIncrSyncer(cfg, ex, im)

	// 第一轮:没有水位 ⇒ 全量 ⇒ 不带 since。
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatalf("第一轮同步失败: %v", err)
	}
	require1(t, len(ex.calls) == 1, "第一轮应只调一次导出")
	if got := ex.calls[0].Since; got != "" {
		t.Errorf("第一轮(无水位)不该带 since,实际 %q", got)
	}

	// 第二轮:有水位 ⇒ 必须带上。
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatalf("第二轮同步失败: %v", err)
	}
	require1(t, len(ex.calls) == 2, "第二轮应再调一次导出")
	since := ex.calls[1].Since
	if since == "" {
		t.Fatal("第二轮必须带水位(since),否则增量没生效")
	}
	// 水位必须落在固定时钟附近(减掉 overlap)。
	want := incrClock().Add(-5 * time.Second).Format(archivedto.SinceLayout)
	if since != want {
		t.Errorf("水位应为 %q,实际 %q", want, since)
	}
}

// TestSync_IncrementalEmptyIsNotFailure 锁"增量窗口 0 行不是失败"。
//
// ⚠️ 这是最容易被写成"报错"的地方 ——
//	源库一天只变 91 行 ⇒ 大多数夜晚就是 0 行。
//	报错 ⇒ 每晚同步都红 ⇒ 运维会以为同步坏了。
func TestSync_IncrementalEmptyIsNotFailure(t *testing.T) {
	ex := &fakeExporter{pages: map[string][]map[string]any{
		archivedto.ScopeStock: nil,
	}}
	im := &fakeImporter{}
	cfg := incrCfg(t, []string{archivedto.ScopeStock})
	s := newIncrSyncer(cfg, ex, im)

	// 先做一轮有水的(全量),让水位与全量校准时间都落地。
	ex.pages[archivedto.ScopeStock] = stockRows(2)
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatalf("第一轮同步失败: %v", err)
	}
	// 第二轮:窗口里没有变动 ⇒ 0 行。
	ex.pages[archivedto.ScopeStock] = nil
	rep, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("增量轮次 0 行不该报错(那只是'今天没变动'),实际: %v", err)
	}
	require1(t, len(rep.Scopes) == 1, "报告里应有一个 scope")
	if rep.Scopes[0].Exported != 0 {
		t.Errorf("导出行数应为 0,实际 %d", rep.Scopes[0].Exported)
	}
	// 水位照常推进(这一轮查询确实覆盖了那个窗口)。
	if got := s.watermarks.Get(archivedto.ScopeStock); got == "" {
		t.Error("0 行那轮也应推进水位,否则每晚都重查同一个窗口")
	}
}

// TestSync_FullRoundEmptyStillFails 锁"全量轮次 0 行仍然失败"。
//
// 症状(若放过):覆盖模式会把目标门店的库存**清空**,
// 而报告写"成功 0 条" ⇒ 前台库存全变成 0。
func TestSync_FullRoundEmptyStillFails(t *testing.T) {
	ex := &fakeExporter{pages: map[string][]map[string]any{
		archivedto.ScopeStock: nil,
	}}
	im := &fakeImporter{}
	cfg := incrCfg(t, []string{archivedto.ScopeStock})

	// 第一轮就没有水位 ⇒ 全量轮次 ⇒ 0 行必须报错。
	_, err := newIncrSyncer(cfg, ex, im).Sync(context.Background())
	if err == nil {
		t.Fatal("全量轮次的 0 行必须报错(会把目标库存清空)")
	}
	if len(im.snapshot()) != 0 {
		t.Errorf("失败时不该提交任何导入,实际 %d 次", len(im.snapshot()))
	}
}

// TestSync_ImportFailureKeepsWatermark 锁"导入失败 ⇒ 水位不动"。
//
// ⚠️ 这是**永久漏数据**的唯一入口:
//	水位推进了 + 导入失败了 ⇒ 那批数据永远不会再被导。
func TestSync_ImportFailureKeepsWatermark(t *testing.T) {
	ex := &fakeExporter{pages: map[string][]map[string]any{
		archivedto.ScopeStock: stockRows(2),
	}}
	im := &fakeImporter{err: errors.New("下游 500")}
	cfg := incrCfg(t, []string{archivedto.ScopeStock})
	s := newIncrSyncer(cfg, ex, im)

	if _, err := s.Sync(context.Background()); err == nil {
		t.Fatal("导入失败必须报错")
	}
	if got := s.watermarks.Get(archivedto.ScopeStock); got != "" {
		t.Errorf("导入失败时水位必须不动,实际 %q", got)
	}
}

// TestSync_NonIncrementalScopeNeverGetsSince 锁"白名单外一律全量"。
//
// 症状(若串了):商品只导"最近变过的一部分"⇒ **漏商品**,而报告说"成功"。
func TestSync_NonIncrementalScopeNeverGetsSince(t *testing.T) {
	ex := &fakeExporter{pages: map[string][]map[string]any{
		archivedto.ScopeProducts: {productRow("P1", "商品")},
		archivedto.ScopeStock:    stockRows(1),
	}}
	im := &fakeImporter{}
	cfg := incrCfg(t, []string{archivedto.ScopeProducts, archivedto.ScopeStock})
	s := newIncrSyncer(cfg, ex, im)

	for i := 0; i < 2; i++ {
		if _, err := s.Sync(context.Background()); err != nil {
			t.Fatalf("第 %d 轮同步失败: %v", i+1, err)
		}
	}
	for _, c := range ex.calls {
		if c.Scope == archivedto.ScopeProducts && c.Since != "" {
			t.Errorf("商品不在增量白名单,不该带 since,实际 %q", c.Since)
		}
	}
}

// TestSync_PeriodicFullRefresh 锁"定期全量校准"。
//
// 症状(若不校准):增量漏掉的行**永远补不回来** ⇒ 库存慢慢偏掉。
func TestSync_PeriodicFullRefresh(t *testing.T) {
	ex := &fakeExporter{pages: map[string][]map[string]any{
		archivedto.ScopeStock: stockRows(2),
	}}
	im := &fakeImporter{}
	cfg := incrCfg(t, []string{archivedto.ScopeStock})
	s := newIncrSyncer(cfg, ex, im)

	// 第一轮:全量,并记下全量校准时间。
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatalf("第一轮同步失败: %v", err)
	}
	// 第二轮:刚校准过 ⇒ 增量。
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatalf("第二轮同步失败: %v", err)
	}
	if len(ex.calls) < 2 || ex.calls[1].Since == "" {
		t.Fatal("刚校准过的第二轮应是增量(必须带 since)")
	}

	// 把时钟往前推 8 天(超过 maxAgeDays=7)⇒ 必须强制全量。
	s.now = func() time.Time { return incrClock().Add(8 * 24 * time.Hour) }
	s.watermarks = NewWatermarkStore(cfg.WatermarkFile,
		cfg.WatermarkOverlapSeconds, cfg.WatermarkMaxAgeDays, s.now)
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatalf("第三轮同步失败: %v", err)
	}
	last := ex.calls[len(ex.calls)-1]
	if last.Since != "" {
		t.Errorf("距上次全量超过 7 天 ⇒ 这一轮必须全量(不带 since),实际带 %q", last.Since)
	}
}

// TestSync_ForceFullIgnoresWatermark 锁"人工的强制全量无视水位"。
//
// ⚠️ 场景:换门店 / 重建目标库之后,增量**导不出基础数据**
// (它只导"变过的行")⇒ 必须能显式跑一次全量。
func TestSync_ForceFullIgnoresWatermark(t *testing.T) {
	ex := &fakeExporter{pages: map[string][]map[string]any{
		archivedto.ScopeStock: stockRows(2),
	}}
	im := &fakeImporter{}
	cfg := incrCfg(t, []string{archivedto.ScopeStock})
	s := newIncrSyncer(cfg, ex, im)

	// 第一轮:全量(建立水位与全量校准时间)。
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatalf("第一轮同步失败: %v", err)
	}
	// 第二轮:正常增量 ⇒ 带 since。
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatalf("第二轮同步失败: %v", err)
	}
	if len(ex.calls) < 2 || ex.calls[1].Since == "" {
		t.Fatal("第二轮应是增量(必须带 since)")
	}
	// 第三轮:强制全量 ⇒ **不带** since。
	cfg.ForceFull = true
	s = newIncrSyncer(cfg, ex, im)
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatalf("强制全量同步失败: %v", err)
	}
	last := ex.calls[len(ex.calls)-1]
	if last.Since != "" {
		t.Errorf("强制全量时不该带 since,实际 %q", last.Since)
	}
}

// TestSync_WatermarkSubtractsSourceLag 锁"水位按 scope 的源数据滞后回退"。
//
// # 症状(如果没减)
//
//	源是 cube 的**离线 DuckDB 快照**(每 30 分钟增量刷新一次):
//	我们导出那一刻,cube 里的数据可能还是 30 分钟前的
//	⇒ 水位按"导出时刻"记 ⇒ 那 30 分钟里的变更**永远落在水位之前**
//	⇒ **永久漏**,且报告照样写"成功 N 条"。
//
// # 现在为什么用 Config.IncrementalLag 注入
//
//	2026-10-22 cube 去 DuckDB 后**所有 model 都是 live**(源数据实时)⇒
//	默认滞后全为 0 ⇒ 拿默认值测不出"回退"这件事。
//	⇒ 用配置覆盖注入一个 24 小时滞后,验证机制**仍然有效**。
//	⇒ 将来某个 scope 真引入缓存时,只要配一行,水位就会自动回退。
func TestSync_WatermarkSubtractsSourceLag(t *testing.T) {
	im := &fakeImporter{}

	// 基线:无滞后(live 源)⇒ 水位 = 起点 − overlap。
	exLive := &fakeExporter{pages: map[string][]map[string]any{
		archivedto.ScopeStock: stockRows(1),
	}}
	cfgLive := incrCfg(t, []string{archivedto.ScopeStock})
	cfgLive.IncrementalScopes = []string{archivedto.ScopeStock}
	sLive := newIncrSyncer(cfgLive, exLive, im)
	if _, err := sLive.Sync(context.Background()); err != nil {
		t.Fatalf("live 源同步失败: %v", err)
	}
	liveWM := sLive.watermarks.Get(archivedto.ScopeStock)

	// 注入 24 小时滞后 ⇒ 水位必须比基线早 24 小时。
	exLag := &fakeExporter{pages: map[string][]map[string]any{
		archivedto.ScopeProducts: {productRow("P1", "商品")},
	}}
	cfgLag := incrCfg(t, []string{archivedto.ScopeProducts})
	cfgLag.IncrementalScopes = []string{archivedto.ScopeProducts}
	cfgLag.IncrementalLag = map[string]int{archivedto.ScopeProducts: 24 * 3600}
	sLag := newIncrSyncer(cfgLag, exLag, im)
	if _, err := sLag.Sync(context.Background()); err != nil {
		t.Fatalf("带滞后的同步失败: %v", err)
	}
	lagWM := sLag.watermarks.Get(archivedto.ScopeProducts)

	ts, err := time.ParseInLocation(archivedto.SinceLayout, liveWM, time.Local)
	if err != nil {
		t.Fatalf("解析 live 水位失败: %v", err)
	}
	tp, err := time.ParseInLocation(archivedto.SinceLayout, lagWM, time.Local)
	if err != nil {
		t.Fatalf("解析带滞后的水位失败: %v", err)
	}
	gap := ts.Sub(tp)
	if gap < 23*time.Hour {
		t.Errorf("有滞后的 scope,水位应比无滞后的早约 24h,实际差 %s", gap)
	}
}

// TestSync_WatermarkIsPerScope 锁"水位按 scope 隔离"。
//
// 症状(若串了):库存的水位被商品用上 ⇒ 商品只导一部分 ⇒ 漏商品。
func TestSync_WatermarkIsPerScope(t *testing.T) {
	ex := &fakeExporter{pages: map[string][]map[string]any{
		archivedto.ScopeStock:    stockRows(1),
		archivedto.ScopeProducts: {productRow("P1", "商品")},
	}}
	im := &fakeImporter{}
	cfg := incrCfg(t, []string{archivedto.ScopeStock, archivedto.ScopeProducts})
	s := newIncrSyncer(cfg, ex, im)

	for i := 0; i < 2; i++ {
		if _, err := s.Sync(context.Background()); err != nil {
			t.Fatalf("第 %d 轮同步失败: %v", i+1, err)
		}
	}
	// 第二轮里:只有 stock 带 since。
	for _, c := range ex.calls {
		if c.Scope == archivedto.ScopeProducts && c.Since != "" {
			t.Errorf("商品不该带 since,实际 %q", c.Since)
		}
	}
	if got := s.watermarks.Get(archivedto.ScopeProducts); got != "" {
		t.Errorf("商品不该有水位,实际 %q", got)
	}
	if got := s.watermarks.Get(archivedto.ScopeStock); got == "" {
		t.Error("库存应有水位")
	}
}

// TestSync_DisabledWatermarkMeansFullEveryTime 锁"没配水位文件 ⇒ 每轮全量"。
//
// ⚠️ 这是**安全默认**:上线时"忘了配"的后果是慢,不是漏数据。
func TestSync_DisabledWatermarkMeansFullEveryTime(t *testing.T) {
	ex := &fakeExporter{pages: map[string][]map[string]any{
		archivedto.ScopeStock: stockRows(1),
	}}
	im := &fakeImporter{}
	cfg := incrCfg(t, []string{archivedto.ScopeStock})
	cfg.WatermarkFile = ""
	s := newIncrSyncer(cfg, ex, im)

	for i := 0; i < 2; i++ {
		if _, err := s.Sync(context.Background()); err != nil {
			t.Fatalf("第 %d 轮同步失败: %v", i+1, err)
		}
	}
	for _, c := range ex.calls {
		if c.Since != "" {
			t.Errorf("没配水位文件时不该带 since,实际 %q", c.Since)
		}
	}
}

// TestValidate_RejectsUnsupportedIncrementalScope 锁"配错增量 scope 名 ⇒ 启动失败"。
//
// ⚠️ 症状(若放过):导出端**忽略** since(仍全量导出)
//	⇒ 但水位被推进了 ⇒ 下次只导一部分却当成全量 ⇒ **漏数据且零报错**。
func TestValidate_RejectsUnsupportedIncrementalScope(t *testing.T) {
	c := Config{
		SourceBranchID: "00", TargetBranchID: "10",
		Scopes: []string{"products"}, Mode: "overwrite",
		CronSpec: "03:30", Cron: "0 30 3 * * *",
		ServiceAccountID: "cid", ServiceAccountSecret: "secret",
		JobToken: "tok",
		// ⚠️ promotions 目前**不能**做增量(源表 t_pub_plan_flow 没有该列,
		//   而且它的 overwrite 导入会删行 ⇒ 更不能走增量)。
		IncrementalScopes: []string{archivedto.ScopePromotions},
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("不支持增量的 scope 必须被拒(否则会漏数据)")
	}
	if !containsAll(err.Error(), "promotions", "不支持增量") {
		t.Errorf("错误文案应点明是哪个 scope,实际: %v", err)
	}
	// 而 stock / products / prices 是支持的 ⇒ 同样的配置换成它们就该通过。
	for _, ok := range []string{
		archivedto.ScopeStock, archivedto.ScopeProducts, archivedto.ScopePrices,
	} {
		c.IncrementalScopes = []string{ok}
		if err := c.Validate(); err != nil {
			t.Errorf("%s 是支持的增量 scope,不该报错: %v", ok, err)
		}
	}
}

// require1 断言条件成立。
func require1(t *testing.T, ok bool, msg string) {
	t.Helper()
	if !ok {
		t.Fatal(msg)
	}
}

// containsAll 断言字符串包含全部子串。
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
