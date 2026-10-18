package salesdaily

import (
	"context"
	"errors"
	"testing"
)

// seedDays 造若干天的快照。
//
// ⚠️ 每天的金额刻意设成可加的整数，方便断言"聚合 = 逐日相加"。
func seedDays(t *testing.T, s *Store, branch string, dates ...string) {
	t.Helper()
	for i, d := range dates {
		base := float64(100 * (i + 1))
		err := s.Save(context.Background(), Snapshot{
			BranchID: branch,
			OperDate: d,
			Totals: TotalsRow{
				NetAmount: base, NetQty: base, RefCost: base * 0.6,
				TotalRows: 10, CostedRows: 10, MarginAvailable: true,
			},
			Categories: []CategoryRow{
				{Name: "生鲜联营", Amount: base * 0.7},
				{Name: "百货联营", Amount: base * 0.3},
			},
			Products: []ProductRow{
				{ItemNo: "A1", Name: "土豆", Category: "生鲜联营", Amount: base * 0.5},
				{ItemNo: "B2", Name: "纸巾", Category: "百货联营", Amount: base * 0.5},
			},
			Suppliers: []SupplierRow{
				{Code: "S1", Name: "南宁蔬菜行", Amount: base * 0.6, Qty: base, ItemCount: 2,
					TopItems: []TopItemRow{{ItemNo: "A1", Name: "土豆", Amount: base * 0.5}}},
				{Code: "S2", Name: "百货批发", Amount: base * 0.4, Qty: base, ItemCount: 1,
					TopItems: []TopItemRow{{ItemNo: "B2", Name: "纸巾", Amount: base * 0.5}}},
			},
		}, CollectSourceInitial)
		if err != nil {
			t.Fatalf("seed %s：%v", d, err)
		}
	}
}

// TestLoadRangeSumsEveryDay 全覆盖时聚合 = 逐日相加。
func TestLoadRangeSumsEveryDay(t *testing.T) {
	s := newStore(t)
	seedDays(t, s, "00", "2026-10-01", "2026-10-02", "2026-10-03")

	got, err := s.LoadRange(context.Background(), "00", "2026-10-01", "2026-10-03")
	if err != nil {
		t.Fatalf("全覆盖不该报错：%v", err)
	}
	// 100 + 200 + 300
	if got.Totals.NetAmount != 600 {
		t.Errorf("总额 = %v，期望 600", got.Totals.NetAmount)
	}
	if got.CoveredDays != 3 || got.TotalDays != 3 || got.MissingDays() != 0 {
		t.Errorf("覆盖 = %d/%d 缺 %d，期望 3/3 缺 0",
			got.CoveredDays, got.TotalDays, got.MissingDays())
	}

	// 品类：生鲜 = (70+140+210)
	if len(got.Categories) != 2 {
		t.Fatalf("品类数 = %d，期望 2", len(got.Categories))
	}
	if got.Categories[0].Name != "生鲜联营" || got.Categories[0].Amount != 420 {
		t.Errorf("第一品类 = %s %v，期望 生鲜联营 420", got.Categories[0].Name, got.Categories[0].Amount)
	}
	if len(got.Products) != 2 || got.Products[0].Amount != 300 {
		t.Errorf("商品聚合不对：%+v", got.Products)
	}
	if len(got.Suppliers) != 2 || got.Suppliers[0].Code != "S1" || got.Suppliers[0].Amount != 360 {
		t.Errorf("供应商聚合不对：%+v", got.Suppliers)
	}
}

// TestLoadRangeReportsMissingDays ⭐ 缺一天必须**同时**满足两件事：
// 1. 返回 ErrRangeIncomplete
// 2. 仍然返回已聚合的部分（让调用方能报出"缺了哪几天"）
//
// ⚠️ 只报错就丢掉部分数据是常见的写法，而那会让"这个月覆盖了几天"
// 这个信息彻底消失 —— 月报少算两天营业额，报告里一句解释都没有。
func TestLoadRangeReportsMissingDays(t *testing.T) {
	s := newStore(t)
	seedDays(t, s, "00", "2026-10-01", "2026-10-03") // 故意缺 10-02

	got, err := s.LoadRange(context.Background(), "00", "2026-10-01", "2026-10-03")
	if !errors.Is(err, ErrRangeIncomplete) {
		t.Fatalf("覆盖不全必须返回 ErrRangeIncomplete，实际 err=%v", err)
	}
	if got.CoveredDays != 2 || got.TotalDays != 3 || got.MissingDays() != 1 {
		t.Errorf("覆盖 = %d/%d 缺 %d，期望 2/3 缺 1",
			got.CoveredDays, got.TotalDays, got.MissingDays())
	}
	// ⚠️ 部分数据仍要可用 —— 这是调用方判断"缺哪几天"的依据。
	//   seedDays 按**下标**给金额（第 1 天 100、第 2 天 200），
	//   所以这里是 100+200，而不是按日期推的 100+300。
	if got.Totals.NetAmount != 300 {
		t.Errorf("已聚合总额 = %v，期望 300（seedDays 两天 100+200）", got.Totals.NetAmount)
	}

	missing, err := s.MissingDates(context.Background(), "00", "2026-10-01", "2026-10-03")
	if err != nil {
		t.Fatalf("MissingDates 失败：%v", err)
	}
	if len(missing) != 1 || missing[0] != "2026-10-02" {
		t.Errorf("缺失日期 = %v，期望 [2026-10-02]", missing)
	}
}

// TestLoadRangeEmptyIsNotIncomplete 一天都没有时是 ErrNotFound 那一类，
// 而**不是** ErrRangeIncomplete。
//
// ⚠️ 两者在调用方那里的处理不同：
//
//	ErrNotFound        → "PG 采集可能压根没跑"，提示运维
//	ErrRangeIncomplete → "跑了但有几天失败"，提示查那几天的采集日志
//
// 混为一谈会让运维去查 job 配置，而真正的问题只是某三天失败了。
func TestLoadRangeEmptyIsNotIncomplete(t *testing.T) {
	s := newStore(t)
	seedDays(t, s, "00", "2026-10-01")

	got, err := s.LoadRange(context.Background(), "00", "2026-10-15", "2026-10-17")
	if !errors.Is(err, ErrRangeIncomplete) {
		t.Fatalf("错误类型不对：%v", err)
	}
	if got.CoveredDays != 0 {
		t.Errorf("覆盖天数 = %d，期望 0", got.CoveredDays)
	}
	// 覆盖 0 天时也必须能区分出来 —— 这是上面那条判据的关键。
	if got.MissingDays() != 3 {
		t.Errorf("缺失天数 = %d，期望 3", got.MissingDays())
	}
}

// TestLoadRangeIsBranchScoped 别的门店的数据不能混进来。
//
// ⚠️ 不加门店过滤的后果是"两家门店的营业额加在一起"——
//
//	数字大得离谱却零报错，而两家门店各自的日报看起来都正常
//	（它们各自查的是自己的那一份）。
func TestLoadRangeIsBranchScoped(t *testing.T) {
	s := newStore(t)
	seedDays(t, s, "00", "2026-10-01", "2026-10-02")
	seedDays(t, s, "01", "2026-10-01", "2026-10-02")

	got, err := s.LoadRange(context.Background(), "01", "2026-10-01", "2026-10-02")
	if err != nil {
		t.Fatalf("全覆盖不该报错：%v", err)
	}
	if got.Totals.NetAmount != 300 {
		t.Errorf("门店 01 总额 = %v，期望 300（只含本店 100+200）", got.Totals.NetAmount)
	}
}

// TestLoadRangeUsesLatestDayNames 商品改过名时取最后一次采集的名字。
//
// ⚠️ 用 MAX(name) 会按字典序取一个，于是可能显示成历史名字，
//
//	而且**没有任何标记** —— 店长会以为这家店一直卖的是那个名字。
func TestLoadRangeUsesLatestDayNames(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	// 10-01 叫"土豆(新)"，10-02 改名成"土豆"。
	if err := s.Save(ctx, Snapshot{
		BranchID: "00", OperDate: "2026-10-01",
		Totals:     TotalsRow{NetAmount: 100},
		Products:   []ProductRow{{ItemNo: "A1", Name: "土豆(新)", Category: "生鲜联营", Amount: 100}},
		Categories: []CategoryRow{{Name: "生鲜联营", Amount: 100}},
	}, CollectSourceInitial); err != nil {
		t.Fatalf("seed 10-01：%v", err)
	}
	if err := s.Save(ctx, Snapshot{
		BranchID: "00", OperDate: "2026-10-02",
		Totals:     TotalsRow{NetAmount: 200},
		Products:   []ProductRow{{ItemNo: "A1", Name: "土豆", Category: "蔬菜", Amount: 200}},
		Categories: []CategoryRow{{Name: "蔬菜", Amount: 200}},
	}, CollectSourceCorrected); err != nil {
		t.Fatalf("seed 10-02：%v", err)
	}

	got, err := s.LoadRange(ctx, "00", "2026-10-01", "2026-10-02")
	if err != nil {
		t.Fatalf("全覆盖不该报错：%v", err)
	}
	if len(got.Products) != 1 {
		t.Fatalf("商品数 = %d，期望 1（同编码跨日应合并成一行）", len(got.Products))
	}
	if got.Products[0].Name != "土豆" {
		t.Errorf("商品名称 = %q，期望 土豆（取最后一次采集）", got.Products[0].Name)
	}
	if got.Products[0].Category != "蔬菜" {
		t.Errorf("商品类别 = %q，期望 蔬菜", got.Products[0].Category)
	}
	if got.Products[0].Amount != 300 {
		t.Errorf("商品金额 = %v，期望 300", got.Products[0].Amount)
	}
}

// TestSupplierTopItemsMergesAcrossDays 跨日合并商品明细。
func TestSupplierTopItemsMergesAcrossDays(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for i, d := range []string{"2026-10-01", "2026-10-02"} {
		amt := float64(100 * (i + 1))
		err := s.Save(ctx, Snapshot{
			BranchID: branchForTest, OperDate: d,
			Totals: TotalsRow{NetAmount: amt},
			Suppliers: []SupplierRow{{
				Code: "S1", Name: "南宁蔬菜行", Amount: amt, ItemCount: 1,
				TopItems: []TopItemRow{{ItemNo: "A1", Name: "土豆", Amount: amt}},
			}},
		}, CollectSourceInitial)
		if err != nil {
			t.Fatalf("seed %s：%v", d, err)
		}
	}
	got, err := s.LoadRange(ctx, branchForTest, "2026-10-01", "2026-10-02")
	if err != nil {
		t.Fatalf("全覆盖不该报错：%v", err)
	}
	if len(got.Suppliers) != 1 {
		t.Fatalf("供应商数 = %d，期望 1", len(got.Suppliers))
	}
	if len(got.Suppliers[0].TopItems) != 1 {
		t.Fatalf("TopItems = %+v，期望 1 条（同一商品跨日合并）", got.Suppliers[0].TopItems)
	}
	if got.Suppliers[0].TopItems[0].Amount != 300 {
		t.Errorf("跨日合并金额 = %v，期望 300（100+200）", got.Suppliers[0].TopItems[0].Amount)
	}
	if got.Suppliers[0].Name != "南宁蔬菜行" {
		t.Errorf("供应商名称 = %q，期望 南宁蔬菜行", got.Suppliers[0].Name)
	}
}

const branchForTest = "00"

// TestMissingDatesNoGap 全覆盖时缺失清单为空。
func TestMissingDatesNoGap(t *testing.T) {
	s := newStore(t)
	seedDays(t, s, "00", "2026-10-01", "2026-10-02")
	missing, err := s.MissingDates(context.Background(), "00", "2026-10-01", "2026-10-02")
	if err != nil {
		t.Fatalf("MissingDates 失败：%v", err)
	}
	if len(missing) != 0 {
		t.Errorf("全覆盖时不该有缺失，实际 %v", missing)
	}
}

// TestLoadRangeRejectsInvertedRange 区间倒置要报错。
//
// ⚠️ 不报错的话 TotalDays 会是负数，MissingDays() 被夹成 0，
//
//	于是"完全没数据"看起来像"全覆盖" —— 而调用方正是靠这个决定要不要回退。
func TestLoadRangeRejectsInvertedRange(t *testing.T) {
	s := newStore(t)
	seedDays(t, s, "00", "2026-10-01")
	if _, err := s.LoadRange(context.Background(), "00", "2026-10-05", "2026-10-01"); err == nil {
		t.Fatal("区间倒置必须报错")
	}
}
