package salesdaily

import (
	"context"
	"errors"
	"testing"
	"time"

	// modernc 是纯 Go 的 SQLite（无 cgo），gorm 的 sqlite.Dialector
	// 只认 database/sql 注册名 —— 必须空白导入注册驱动。
	_ "modernc.org/sqlite"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	dialector := sqlite.Dialector{
		DriverName: "sqlite",
		DSN:        ":memory:?_pragma=foreign_keys(1)&_time_format=sqlite",
	}
	db, err := gorm.Open(dialector, &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(Models()...); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("sql db: %v", err)
	}
	// ":memory:" 库每个连接是独立实例，>1 时建表与查询可能落在不同连接上，
	// 表现为随机的「表不存在」。
	sqlDB.SetMaxOpenConns(1)
	fixed, _ := time.Parse(time.RFC3339, "2026-10-17T22:45:00+08:00")
	return NewStore(db, func() time.Time { return fixed })
}

func sampleSnapshot(date string) Snapshot {
	return Snapshot{
		BranchID: "00", OperDate: date,
		Totals: TotalsRow{
			NetAmount: 13003.8, NetQty: 1400, RefCost: 10403.04,
			RefMargin: 2600.76, RefMarginPct: 0.2, CostCoverage: 1,
			MarginAvailable: true, TotalRows: 1400, CostedRows: 1400,
		},
		Categories: []CategoryRow{
			{Name: "蛋类", Amount: 746.3, Qty: 100, Cost: 600, CostedRow: 100, TotalRow: 100},
			{Name: "干货/大米", Amount: 586.8, Qty: 90, Cost: 470, CostedRow: 90, TotalRow: 90},
		},
		Suppliers: []SupplierRow{{
			Code: "29", Name: "猪肉自营", Amount: 3378.3, Qty: 200, ItemCount: 3,
			TopItems: []TopItemRow{{ItemNo: "A", Name: "五花肉", Amount: 2000, Qty: 120}},
		}},
		Products: []ProductRow{
			{ItemNo: "A", Name: "五花肉", Category: "猪肉", Amount: 2000, Qty: 120},
		},
	}
}

func TestSaveGetRoundTrip(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Save(ctx, sampleSnapshot("2026-10-16"), CollectSourceInitial); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := st.Get(ctx, "00", "2026-10-16")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if d := got.Totals.NetAmount - 13003.8; d > 0.01 || d < -0.01 {
		t.Errorf("总额 = %v，期望 13003.8", got.Totals.NetAmount)
	}
	if !got.Totals.MarginAvailable {
		t.Error("覆盖率 100% 时毛利应可用")
	}
	if len(got.Categories) != 2 {
		t.Fatalf("品类 = %d 条，期望 2", len(got.Categories))
	}
	if got.Categories[0].Name != "蛋类" || got.Categories[0].Amount != 746.3 {
		t.Errorf("品类应按金额倒序且内容正确：%+v", got.Categories)
	}
	if len(got.Suppliers) != 1 || got.Suppliers[0].Code != "29" {
		t.Fatalf("供应商 = %+v", got.Suppliers)
	}
	if len(got.Suppliers[0].TopItems) != 1 || got.Suppliers[0].TopItems[0].ItemNo != "A" {
		t.Errorf("供应商商品明细未还原：%+v", got.Suppliers[0].TopItems)
	}
	if len(got.Products) != 1 || got.Products[0].ItemNo != "A" {
		t.Errorf("商品 = %+v", got.Products)
	}
}

// TestSaveTwice_第二次消失的品类必须真的消失 是整日替换的核心判别力。
//
// 只 upsert 不删除的实现会让它**红**：那个今天已经不在供应商列表里的
// 品类会永远留在库里，一路混进月报 —— 而症状是"有个早就没卖的品类
// 天天出现"，零报错、极难反推。
func TestSaveTwice_第二次消失的品类必须真的消失(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	first := sampleSnapshot("2026-10-16")
	if err := st.Save(ctx, first, CollectSourceInitial); err != nil {
		t.Fatalf("首次 Save: %v", err)
	}

	// 第二次采集（校正）："干货/大米"当天不再有货
	second := first
	second.Categories = []CategoryRow{
		{Name: "蛋类", Amount: 800, Qty: 110, Cost: 640, CostedRow: 110, TotalRow: 110},
	}
	second.Suppliers = nil
	second.Products = nil
	second.Totals.NetAmount = 800
	if err := st.Save(ctx, second, CollectSourceCorrected); err != nil {
		t.Fatalf("校正 Save: %v", err)
	}

	got, err := st.Get(ctx, "00", "2026-10-16")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Categories) != 1 || got.Categories[0].Name != "蛋类" {
		t.Errorf("校正后不应残留已消失的品类；实际 = %+v", got.Categories)
	}
	if got.Totals.NetAmount != 800 {
		t.Errorf("总额应被覆盖为 800，实际 %v", got.Totals.NetAmount)
	}
	if len(got.Suppliers) != 0 {
		t.Errorf("供应商应被清空，实际 %+v", got.Suppliers)
	}
	if len(got.Products) != 0 {
		t.Errorf("商品应被清空，实际 %+v", got.Products)
	}

	// 行数必须仍是 1（不是追加出一行互相打架的数字）
	var n int64
	st.db.Model(&DailyTotals{}).Count(&n)
	if n != 1 {
		t.Errorf("sales_daily_totals 应只有 1 行，实际 %d 行 —— 重复采集在追加而不是覆盖", n)
	}
}

// TestSourceRecordsCorrected 钉住"这个数字被校正过"要留下痕迹：
// 被校正过的数字与从未被校正的数字，长得不一样。
func TestSourceRecordsCorrected(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Save(ctx, sampleSnapshot("2026-10-16"), CollectSourceInitial); err != nil {
		t.Fatalf("Save: %v", err)
	}
	metas, err := st.ListMetas(ctx, "00", "2026-10-01", "2026-10-31")
	if err != nil {
		t.Fatalf("ListMetas: %v", err)
	}
	if len(metas) != 1 || metas[0].Source != CollectSourceInitial {
		t.Fatalf("首次采集的来源应是 initial；实际 %+v", metas)
	}
	if err := st.Save(ctx, sampleSnapshot("2026-10-16"), CollectSourceCorrected); err != nil {
		t.Fatalf("校正 Save: %v", err)
	}
	metas, _ = st.ListMetas(ctx, "00", "2026-10-01", "2026-10-31")
	if len(metas) != 1 || metas[0].Source != CollectSourceCorrected {
		t.Errorf("校正后来源应是 corrected；实际 %+v", metas)
	}
}

// TestGetMissingVersusZeroSales 守住一个关键区分：
// "没采集"与"当天零销售"在数据上差一行，而语义完全相反。
func TestGetMissingVersusZeroSales(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	if _, err := st.Get(ctx, "00", "2026-10-15"); !errors.Is(err, ErrNotFound) {
		t.Errorf("未采集的日期必须返回 ErrNotFound，实际 %v", err)
	}

	// 当天确实零销售：会写下一行，但**不是** ErrNotFound
	zero := sampleSnapshot("2026-10-15")
	zero.Totals = TotalsRow{MarginAvailable: false}
	zero.Categories, zero.Suppliers, zero.Products = nil, nil, nil
	if err := st.Save(ctx, zero, CollectSourceInitial); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := st.Get(ctx, "00", "2026-10-15")
	if err != nil {
		t.Fatalf("零销售的日期不应是 ErrNotFound：%v", err)
	}
	if got.Totals.NetAmount != 0 || len(got.Categories) != 0 {
		t.Errorf("零销售快照内容不对：%+v", got.Totals)
	}
}

// TestAggregate_缺口必须被报出来 是月报最关键的一条：
// 采集漏一天就少算一天营业额，而静默少算是最坏的表现。
func TestAggregate_缺口必须被报出来(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for _, d := range []string{"2026-10-01", "2026-10-02", "2026-10-04"} {
		if err := st.Save(ctx, sampleSnapshot(d), CollectSourceInitial); err != nil {
			t.Fatalf("Save %s: %v", d, err)
		}
	}

	agg, err := st.AggregateTotals(ctx, "00", "2026-10-01", "2026-10-04")
	if err != nil {
		t.Fatalf("AggregateTotals: %v", err)
	}
	if agg.CoveredDays != 3 {
		t.Errorf("覆盖天数 = %d，期望 3", agg.CoveredDays)
	}
	if agg.MissingDays != 1 {
		t.Errorf("缺失天数 = %d，期望 1（10-03 没采集）", agg.MissingDays)
	}
	want := 13003.8 * 3
	if d := agg.Totals.NetAmount - want; d > 0.05 || d < -0.05 {
		t.Errorf("聚合总额 = %v，期望 %.1f（**只含已采集的 3 天**）", agg.Totals.NetAmount, want)
	}
}

// TestAggregate_毛利按聚合后算而非日均：日均会让亏损日被盈利日掩盖。
func TestAggregate_毛利按聚合后算而非日均(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	good := sampleSnapshot("2026-10-01") // 赚 2600.76
	bad := sampleSnapshot("2026-10-02")
	bad.Totals.NetAmount = 1000
	bad.Totals.RefCost = 1500 // 亏 500
	for _, s := range []Snapshot{good, bad} {
		if err := st.Save(ctx, s, CollectSourceInitial); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	agg, err := st.AggregateTotals(ctx, "00", "2026-10-01", "2026-10-02")
	if err != nil {
		t.Fatalf("AggregateTotals: %v", err)
	}
	wantMargin := (13003.8 - 10403.04) + (1000 - 1500)
	if d := agg.Totals.RefMargin - wantMargin; d > 0.05 || d < -0.05 {
		t.Errorf("聚合毛利 = %v，期望 %.2f", agg.Totals.RefMargin, wantMargin)
	}
	dayAvg := ((13003.8 - 10403.04) / 2) + ((1000 - 1500) / 2)
	if d := agg.Totals.RefMargin - dayAvg; d < -0.05 && d > 0.05 {
		t.Log("参考：若按日均计算会是", dayAvg)
	}
}

func TestSaveRejectsMissingKey(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for name, s := range map[string]Snapshot{
		"缺 branch": {OperDate: "2026-10-16"},
		"缺 date":   {BranchID: "00"},
	} {
		if err := st.Save(ctx, s, CollectSourceInitial); err == nil {
			t.Errorf("%s 必须被拒，否则会写出一行永远查不到的孤儿数据", name)
		}
	}
}

func TestAggregateRejectsInvertedRange(t *testing.T) {
	st := newStore(t)
	if _, err := st.AggregateTotals(context.Background(), "00", "2026-10-10", "2026-10-01"); err == nil {
		t.Error("区间倒置必须被拒（否则会算出一个负数天数的\"合计\"）")
	}
}
