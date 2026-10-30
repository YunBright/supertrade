// 2026-10-30:锁「链的起点 = **期初生效日**」+「**锚点必须晚于基线日**」。
//
// # 为什么锁这两条(生产实测:店 00 / 2026-10-29)
//
//	门店口径(owner 2026-10-30 原话):「我把 2026-10-28 作为期初,
//	2026-10-29 作为第一天正常开始使用这套鲜肉子系统」。
//
//	⇒ 10-28 之前物化的旧成本快照是**上线前的账**,不在新链里。
//	⇒ 但引擎的链起点取的是 min(期初生效日, 最早快照日) = 10-01
//	  ⇒ 链被拉回上线前;更要命的是**锚点**会把 10-28 那天的快照接给 10-29
//	  ⇒ 负链值(猪耳 −1.279 / 猪肺 −17.64 / 猪手 −9.2 kg)被当成 10-29 的期初
//	  ⇒ 门店看到"期初为什么有负数"。
//
// # 两条规则
//
//	① 链的起点 = **期初生效日**(门店定义的基线);只有没有期初结转的店
//	   才退回"最早快照日"。
//	② **锚点必须晚于基线日** —— 基线日当天必须由链自己算(它的期初 = 门店录的值),
//	   不能用快照接续(否则基线日之前物化的旧快照会又接回链里)。
package service_test

import (
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/shopspring/decimal"
	"gorm.io/datatypes"
)

// fmChainSeedSnapshot 插一条成本日快照(模拟"上线前/旧数据状态物化出来的快照")。
//
// ⚠ 这类快照是**链的载体**(= 上一营业日的部位结余)⇒ 它一旦被当成锚点,
//
//	它的期末就会变成次日的期初 ⇒ 里面的脏值会一路传下去。
//
// ⚠️ **时区必须与生产一致**:服务默认 bizTZ = Asia/Shanghai,
//
//	而 gorm.io/datatypes 的 Date 在 SQLite 里**保留时区表示**
//	(业务时区落库成 "2026-10-06T00:00:00+08:00"、UTC 落库成 "…T00:00:00Z")
//	⇒ 两种形态对 (branch_id, biz_date, cut_type) 唯一索引来说**是两个不同的键**
//	⇒ 用 UTC 造的行与引擎写的行对不上(upsert 会撞主键)。
//	⇒ 这里一律用**业务时区**造,和引擎自己写的形态一致。
func fmChainSeedSnapshot(t *testing.T, branchID, bizDate string, cut model.CutType,
	closingKg, closingAmt, unit string) {
	t.Helper()
	day, err := time.ParseInLocation("2006-01-02", bizDate, fmLoc)
	mustNoErr(t, err, "parse bizDate")
	mustNoErr(t, testDBSingleton.Create(&model.CutCostDaily{
		ID:                service.CutCostDailyID(branchID, bizDate, string(cut)),
		BranchID:          branchID,
		BizDate:           datatypes.Date(day),
		CutType:           cut,
		OpeningKg:         decimal.Zero,
		OpeningAmount:     decimal.Zero,
		ClosingKg:         d(closingKg),
		ClosingAmount:     d(closingAmt),
		MovingAvgUnitCost: d(unit),
	}).Error, "插成本日快照")
}

// TestCutChain_AnchorOnBaselineDay_NotUsed 锁「**锚点必须晚于基线日**」。
//
// # 场景(复刻生产)
//
//	期初(基线日) = 2026-10-05:门店录 belly 100kg @ 18 元。
//	基线日**当天**有一份快照(在旧数据状态下物化的)⇒ 期末是脏值:
//	  closing_kg = −9.2(账面短卖)/ closing_amount = −146.51。
//	10-06 卖 10kg @ 180 元。
//
// # 期望
//
//	10-06 的期初 = **100kg / 1800 元**(门店录的期初)
//	⇒ **不是** −9.2kg(基线日那份脏快照的期末)。
//
// # 判别力
//
//	去掉"锚点必须晚于基线日"这条 ⇒ 基线日的快照被当成锚点
//	⇒ 10-06 的期初变成 −9.2kg ⇒ 本用例立刻红。
func TestCutChain_AnchorOnBaselineDay_NotUsed(t *testing.T) {
	baseDay := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	nextDay := baseDay.AddDate(0, 0, 1) // 2026-10-06

	svc, ctx := fmChainSetup(t, map[string][]map[string]any{
		"2026-10-06": {fmCubeSalesRow("P-BELLY", "10", "180")},
	})
	// ⚠️ 时钟要先拨到目标日之后:期初结转不许填**未来**日期。
	svc.SetClock(fmClock(nextDay))
	// 门店的期初结转(基线日 = 2026-10-05)。
	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-05",
			ColdStoreKg: d("100"), UnitPriceYuan: d("18")},
	}); err != nil {
		t.Fatalf("SaveStockOpenings: %v", err)
	}
	// ⚠ 基线日当天有一份**脏快照**(旧数据状态物化的)⇒ 期末是负的账面短卖。
	fmChainSeedSnapshot(t, "S001", "2026-10-05", model.CutBelly, "-9.2", "-146.51", "15.9249")

	svc.SetClock(fmClock(nextDay))
	rep, err := svc.ComputeGrossMargin(ctx, "S001", nextDay)
	mustNoErr(t, err, "ComputeGrossMargin")
	row := fmCutRowFor(t, rep, model.CutBelly)

	if !row.OpeningKg.Equal(d("100")) {
		t.Errorf("★ 10-06 的期初应是门店录的 100kg(基线日的脏快照**不能**当锚点), got %s",
			row.OpeningKg)
	}
	if !rep.Reconciliation.OpeningInventoryCostYuan.Equal(d("1800")) {
		t.Errorf("★ 10-06 的期初金额应是 1800 元(100kg × 18 元), got %s",
			rep.Reconciliation.OpeningInventoryCostYuan)
	}
}

// TestCutChain_BaselineIsChainStart_NotEarlierSnapshot 锁「链的起点 = **期初生效日**」。
//
// # 场景
//
//	期初(基线日) = 2026-10-05:门店录 belly 100kg @ 18 元。
//	**基线日之前**(10-02)有一份脏快照(期末 −9.2kg)+ 当天有销售 10kg。
//
// # 期望
//
//	10-06 的期初 = 100kg(基线日之后的链,基线日当天由链自己算)
//	⇒ **不是** 10-02 那天脏快照带过来的负链值。
//
// # 判别力
//
//	链起点取 min(期初生效日, 最早快照日) = 10-02 ⇒ 链被拉回上线前
//	⇒ 基线日的期初被上游的负链值污染 ⇒ 本用例红。
func TestCutChain_BaselineIsChainStart_NotEarlierSnapshot(t *testing.T) {
	baseDay := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	nextDay := baseDay.AddDate(0, 0, 1) // 2026-10-06

	svc, ctx := fmChainSetup(t, map[string][]map[string]any{
		"2026-10-02": {fmCubeSalesRow("P-BELLY", "10", "180")},
		"2026-10-06": {fmCubeSalesRow("P-BELLY", "10", "180")},
	})
	// ⚠️ 时钟要先拨到目标日之后:期初结转不许填**未来**日期。
	svc.SetClock(fmClock(nextDay))
	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-05",
			ColdStoreKg: d("100"), UnitPriceYuan: d("18")},
	}); err != nil {
		t.Fatalf("SaveStockOpenings: %v", err)
	}
	// 上线前的脏快照(10-02)⇒ 期末是负的账面短卖。
	fmChainSeedSnapshot(t, "S001", "2026-10-02", model.CutBelly, "-9.2", "-146.51", "15.9249")

	svc.SetClock(fmClock(nextDay))
	rep, err := svc.ComputeGrossMargin(ctx, "S001", nextDay)
	mustNoErr(t, err, "ComputeGrossMargin")
	row := fmCutRowFor(t, rep, model.CutBelly)

	if !row.OpeningKg.Equal(d("100")) {
		t.Errorf("★ 链的起点应是期初生效日 10-05 ⇒ 10-06 的期初 = 100kg, got %s",
			row.OpeningKg)
	}
}
