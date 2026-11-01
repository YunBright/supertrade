package service_test

// 回归锁:**长期毛利(全店口径)**(2026-11-01 owner 拍板:专门建一个长期毛利页)。
//
// # 口径(owner 亲定 + 推论)
//
//	期间毛利 = 期间营收 − 期间采购 + (期末冷库量 − 期初冷库量) × 期间采购均价
//
// # 为什么单日必须配一个长期口径
//
//	单日参考口径: m = (营收 − 当日采购 + 期末**猪肉台**剩余×采购均价) / 营收
//	⚠️ **期初台面 = 0**(夜里柜台清空、货回冷库)⇒ 单日**故意不含冷库**
//	⇒ 单日毛利偏高(今天卖昨天结转的货、那笔成本昨天已扣)⇒ **跨期才平**。
//	⇒ 这一页把冷库接回来,让期间数自洽。
//
// # 三条不能违反的性质(每条都有判别力用例)
//
//	① **期间毛利 = 期间营收 − 期间采购 + 冷库变动的计价**(桥接项必须能自己验算)
//	   ⇒ 用例:TestLongTermMargin_PeriodFormulaBridge
//	② **全店结余 = 冷库量**(冷库的日终余额**已经含**夜里从柜台搬回去的那部分)
//	   ⇒ 不能再加一次台面实盘 ⇒ 用例:TestLongTermMargin_StockIsColdStoreOnly
//	③ **没有完整日终盘点 ⇒ 期间毛利不完整**,必须显式提示(绝不让 0 冒充"算得出来")
//	   ⇒ 用例:TestLongTermMargin_MissingStocktakeIsFlagged

import (
	"strings"
	"testing"

	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/shopspring/decimal"
)

// ltmDec 把字面量转成 decimal。
func ltmDec(f float64) decimal.Decimal { return decimal.NewFromFloat(f) }

// ltmDay 造一行[LongTermMarginDay]。
//
// ⚠️ HasPurchase 由 purchasedKg 推出:没进货的那天**毛利算不出来**
//   (公式会退化成 100%,见 cost_reference.go)⇒ 不进"每日之和"。
func ltmDay(date string, revenue, purchasedKg, purchasedYuan, counterKg, coldKg, gm float64,
	hasStocktake bool) service.LongTermMarginDay {
	return service.LongTermMarginDay{
		Date:            date,
		RevenueYuan:     ltmDec(revenue),
		PurchasedKg:     ltmDec(purchasedKg),
		PurchasedYuan:   ltmDec(purchasedYuan),
		PurchaseAvgYuan: ltmDec(10), // 本用例统一 10 元/kg,便于口算
		CounterStockKg:  ltmDec(counterKg),
		ColdStoreKg:     ltmDec(coldKg),
		HasStocktake:    hasStocktake,
		HasPurchase:     purchasedKg > 0,
		GrossMarginYuan: ltmDec(gm),
	}
}

// ① 期间毛利必须能自己验算:营收 − 采购 + 冷库变动的计价。
//
// 场景(物理自洽):两天都按 10 元/kg 进货;第一天天台面剩 20kg ⇒ 夜里搬进冷库
// ⇒ 期末冷库 20kg、期初 0kg ⇒ 冷库变动 +20kg ⇒ 计价 +200 元。
func TestLongTermMargin_PeriodFormulaBridge(t *testing.T) {
	rows := []service.LongTermMarginDay{
		ltmDay("2026-10-29", 1000, 100, 1000, 20, 20, 200, true),
		ltmDay("2026-10-30", 2000, 200, 2000, 0, 20, 0, true),
	}
	rep := service.BuildLongTermMarginReportForTest("00", "2026-10-29", "2026-10-30", rows,
		ltmDec(0), ltmDec(20))

	tot := rep.Total
	if !tot.RevenueYuan.Equal(ltmDec(3000)) {
		t.Fatalf("期间营收 = %s,期望 3000", tot.RevenueYuan.String())
	}
	if !tot.PurchasedYuan.Equal(ltmDec(3000)) {
		t.Fatalf("期间采购 = %s,期望 3000", tot.PurchasedYuan.String())
	}
	if !tot.PurchaseAvgYuan.Equal(ltmDec(10)) {
		t.Fatalf("期间采购均价 = %s,期望 10", tot.PurchaseAvgYuan.String())
	}
	// 冷库变动 +20kg × 10 = +200 元。
	if !tot.ColdStoreDeltaKg.Equal(ltmDec(20)) {
		t.Fatalf("冷库变动 = %s kg,期望 20", tot.ColdStoreDeltaKg.String())
	}
	if !tot.ColdStoreValueDeltaYuan.Equal(ltmDec(200)) {
		t.Fatalf("冷库变动计价 = %s,期望 200", tot.ColdStoreValueDeltaYuan.String())
	}
	// 期间毛利 = 3000 − 3000 + 200 = 200。
	if !tot.GrossMarginYuan.Equal(ltmDec(200)) {
		t.Fatalf("期间毛利 = %s,期望 200", tot.GrossMarginYuan.String())
	}
	// 桥接必须闭合(店长自己能验算)。
	bridge := tot.RevenueYuan.Sub(tot.PurchasedYuan).Add(tot.ColdStoreValueDeltaYuan)
	if !bridge.Equal(tot.GrossMarginYuan) {
		t.Fatalf("桥接不闭合:%s ≠ %s", bridge.String(), tot.GrossMarginYuan.String())
	}
	// 毛利率 = 200 / 3000。
	if got := tot.GrossMarginPct; got < 6.66 || got > 6.67 {
		t.Fatalf("期间毛利率 = %v,期望 ≈6.67", got)
	}
}

// ② 全店结余 = 冷库量(冷库日终余额**已含**夜里从柜台搬回去的部分)⇒ 不能再加台面。
//
// 判别力:若有人把台面实盘也加进"全店结余",这两个数就会不等。
func TestLongTermMargin_StockIsColdStoreOnly(t *testing.T) {
	rows := []service.LongTermMarginDay{
		ltmDay("2026-10-29", 1000, 100, 1000, 20, 20, 200, true),
	}
	rep := service.BuildLongTermMarginReportForTest("00", "2026-10-29", "2026-10-29", rows,
		ltmDec(5), ltmDec(20))

	if !rep.Total.OpeningStockKg.Equal(rep.Total.OpeningColdStoreKg) {
		t.Fatalf("期初全店结余 %s ≠ 期初冷库量 %s",
			rep.Total.OpeningStockKg.String(), rep.Total.OpeningColdStoreKg.String())
	}
	if !rep.Total.ClosingStockKg.Equal(rep.Total.ClosingColdStoreKg) {
		t.Fatalf("期末全店结余 %s ≠ 期末冷库量 %s",
			rep.Total.ClosingStockKg.String(), rep.Total.ClosingColdStoreKg.String())
	}
	// ⚠️ 台面实盘 20kg **不在**全店结余里(它已经在冷库的日终余额里)。
	if rep.Total.ClosingStockKg.Equal(ltmDec(40)) {
		t.Fatal("全店结余把台面实盘又加了一遍 ⇒ 重复计库存")
	}
}

// 冷库变了 ⇒ 期间毛利变;每日行**不受影响** ⇒ 证明"跨期才平"这件事由冷库承担。
func TestLongTermMargin_ColdStoreDrivesThePeriodNumber(t *testing.T) {
	rows := []service.LongTermMarginDay{
		ltmDay("2026-10-29", 1000, 100, 1000, 20, 20, 200, true),
	}
	a := service.BuildLongTermMarginReportForTest("00", "2026-10-29", "2026-10-29", rows,
		ltmDec(0), ltmDec(20))
	b := service.BuildLongTermMarginReportForTest("00", "2026-10-29", "2026-10-29", rows,
		ltmDec(10), ltmDec(20))

	if a.Total.GrossMarginYuan.Equal(b.Total.GrossMarginYuan) {
		t.Fatal("期初冷库变了(10kg)⇒ 期间毛利必须变(少了 100 元的库存价值)")
	}
	want := ltmDec(100)
	got := a.Total.GrossMarginYuan.Sub(b.Total.GrossMarginYuan)
	if !got.Equal(want) {
		t.Fatalf("两期毛利差 = %s,期望 %s(10kg × 10 元/kg)", got.String(), want.String())
	}
	// 每日行不因冷库而变。
	if !a.Rows[0].GrossMarginYuan.Equal(b.Rows[0].GrossMarginYuan) {
		t.Fatal("每日行不该受期间冷库影响")
	}
}

// ③ 没有完整日终盘点 ⇒ 期间毛利**不完整**,必须显式提示。
func TestLongTermMargin_MissingStocktakeIsFlagged(t *testing.T) {
	rows := []service.LongTermMarginDay{
		ltmDay("2026-10-29", 1000, 100, 1000, 0, 20, 0, false), // 没有盘点
		ltmDay("2026-10-30", 2000, 200, 2000, 0, 20, 0, true),
	}
	rep := service.BuildLongTermMarginReportForTest("00", "2026-10-29", "2026-10-30", rows,
		ltmDec(0), ltmDec(20))

	if !rep.Total.CostIncomplete {
		t.Fatal("有 1 天没有完整盘点 ⇒ 必须标 cost_incomplete")
	}
	if rep.Total.CostIncompleteNote == "" {
		t.Fatal("cost_incomplete ⇒ 必须带一句人话说明")
	}
	if rep.Total.DaysWithoutStocktake != 1 || rep.Total.DaysWithStocktake != 1 {
		t.Fatalf("盘点天数统计错:有=%d 无=%d",
			rep.Total.DaysWithStocktake, rep.Total.DaysWithoutStocktake)
	}
}

// 期间**没有进货** ⇒ 取不到采购均价 ⇒ 冷库变动没计价 ⇒ 必须提示(否则期间毛利虚高)。
func TestLongTermMargin_NoPurchaseMeansNoAvg(t *testing.T) {
	// ⚠️ 没有进货 ⇒ 采购均价也是 0(见 referenceDayTotalsOf:只有 PurchasedKg>0 才算均价)。
	day := ltmDay("2026-10-29", 1000, 0, 0, 0, 20, 1000, true)
	day.PurchaseAvgYuan = decimal.Zero
	rep := service.BuildLongTermMarginReportForTest("00", "2026-10-29", "2026-10-29",
		[]service.LongTermMarginDay{day}, ltmDec(0), ltmDec(20))

	if rep.Total.PurchaseAvgYuan.IsPositive() {
		t.Fatalf("没有进货 ⇒ 采购均价不该为正(%s)", rep.Total.PurchaseAvgYuan.String())
	}
	if !rep.Total.ColdStoreValueDeltaYuan.IsZero() {
		t.Fatalf("取不到采购均价 ⇒ 冷库变动不该计价(%s)", rep.Total.ColdStoreValueDeltaYuan.String())
	}
	if rep.Note == "" {
		t.Fatal("取不到采购均价 ⇒ note 必须说清楚")
	}
}

// 区间里有"**没有进货**"的天 ⇒ 那天的每日毛利算不出来 ⇒ **不进"每日之和"**。
//
// ⚠️ 为什么必须锁:没进货的那天,公式会退化成 m = 100%(见 cost_reference.go)
//   ⇒ 若把它算进每日之和 ⇒ 每日之和虚高 ⇒ 店长拿它跟期间毛利对不上,
//   而差额**看起来**像"口径差" ⇒ 会被误判成 bug。
func TestLongTermMargin_NoPurchaseDayExcludedFromDailySum(t *testing.T) {
	rows := []service.LongTermMarginDay{
		ltmDay("2026-10-29", 1000, 100, 1000, 20, 20, 200, true), // 有进货 ⇒ 毛利 200
		ltmDay("2026-10-30", 2000, 0, 0, 0, 20, 2000, true),      // 没进货 ⇒ 毛利算不出来
	}
	rep := service.BuildLongTermMarginReportForTest("00", "2026-10-29", "2026-10-30", rows,
		ltmDec(0), ltmDec(20))

	if !rep.Total.DailySumGrossMarginYuan.Equal(ltmDec(200)) {
		t.Fatalf("★ 没有进货的那天不该进每日之和,实际 %s",
			rep.Total.DailySumGrossMarginYuan.String())
	}
	if !strings.Contains(rep.Note, "没有进货") {
		t.Fatalf("★ note 必须点名「没有进货」的天: %s", rep.Note)
	}
}

// 冷库量的性质说明必须挂在报告上(它是账面推算值,不是实盘)。
func TestLongTermMargin_ColdStoreNotePresent(t *testing.T) {
	rows := []service.LongTermMarginDay{ltmDay("2026-10-29", 1000, 100, 1000, 20, 20, 200, true)}
	rep := service.BuildLongTermMarginReportForTest("00", "2026-10-29", "2026-10-29", rows,
		ltmDec(0), ltmDec(20))
	if rep.ColdStoreNote == "" {
		t.Fatal("冷库量必须标注「账面推算值」(冷库不在每日盘点范围内)")
	}
	if rep.Note == "" {
		t.Fatal("报告必须带口径说明")
	}
}
