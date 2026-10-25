package service_test

// 2026-10-24:**营业日状态机 + 按日结余 + 搬台流水 + 盘点口径拆分**的回归锁。
//
// 门店口径(用户原话,别改回去):
//
//	「整个鲜肉板块都是围绕如何计算**每日真实毛利**展开的。倘若不知道前天不做日终盘点,
//	  那就没办法从冷库搬上台继续售卖;倘若没录入今天的猪,那也无法售卖。
//	  补录是为了避免错过日期无法正确填补空缺而导致**整条时间线错乱,毛利就错完了**。」
//	「**不补录昨天,就无法录今天。**」
//	「盘点盘的是猪肉台,不盘冷库;冷库里的没搬出来的一直留在冷库,
//	  可以通过大宗结算直接出冷库。」
//
// ⇒ 六条不变量(每条一个用例):
//
//	① 前一营业日未盘点 ⇒ 录猪被拒(并说清要补哪天);
//	② 同一归属日重复盘点 ⇒ 只有一张单(且营业日状态 = 已盘点);
//	③ 补录往日的盘点 ⇒ 那天的结余/成本快照**重算**,今天的不被写脏;
//	④ 两种搬台方式(整袋搬 / 按 SKU+kg 搬)**都留流水**;
//	⑤ 搬台支持**归属日**(补录往期的搬台);
//	⑥ 盘点**实盘只报柜台**,冷库余额**另列**。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/shopspring/decimal"
)

// fmBizSetup 起服务 + 部位映射(belly → P-BELLY),时钟停在 10-01。
func fmBizSetup(t *testing.T) (*service.Service, context.Context, string) {
	t.Helper()
	svc, _ := setupTestService(t)
	ctx := context.Background()
	svc.SetClock(fmClock(fmDay1))
	if _, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-BELLY"}); err != nil {
		t.Fatalf("CreateBranchCutMapping: %v", err)
	}
	return svc, ctx, "P-BELLY"
}

// fmBizSeedDay1 铺好 10-01:进猪 + 分割 10kg + 日终盘点(柜台实盘 10kg)。
//
// ⇒ 10-01 日终:冷库 10kg、柜台 0kg。
func fmBizSeedDay1(t *testing.T, svc *service.Service, ctx context.Context, sku string) {
	t.Helper()
	pigID := fmSeedPigAt(t, svc, "EB-1", "50", fmDay1)
	fmSeedPigCut(t, svc, pigID, sku, "10")
	_, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("10")},
		},
	}, "u-1")
	mustNoErr(t, err, "10-01 盘点")
}

// fmPositions 查某店某 SKU 的位置余额(冷库 / 柜台)。
func fmPositions(t *testing.T, branchID, sku string) (cold, counter decimal.Decimal) {
	t.Helper()
	var rows []model.SkuStockPosition
	if err := testDBSingleton.Where("branch_id = ? AND cube_product_id = ?", branchID, sku).
		Find(&rows).Error; err != nil {
		t.Fatalf("query sku_stock_positions: %v", err)
	}
	cold, counter = decimal.Zero, decimal.Zero
	for _, r := range rows {
		switch r.Location {
		case model.StockLocationColdStore:
			cold = cold.Add(r.QtyKg)
		case model.StockLocationCounter:
			counter = counter.Add(r.QtyKg)
		}
	}
	return cold, counter
}

// fmDailySnapshot 查某店某营业日某**部位**的**结余快照**(审计结转的产物)。
//
// ⚠️ 用前缀匹配:gorm.io/datatypes 的 Date 落库保留时区表示
// (2026-10-02T00:00:00+08:00)⇒ 等值比较会因构造方式不同而落空。
func fmDailySnapshot(t *testing.T, branchID, bizDate, cutType string) (model.CutStockDaily, bool) {
	t.Helper()
	var rec model.CutStockDaily
	err := testDBSingleton.Where("branch_id = ? AND cut_type = ? AND biz_date LIKE ?",
		branchID, cutType, bizDate+"%").First(&rec).Error
	if err != nil {
		return rec, false
	}
	return rec, true
}

// fmCostDailyClosing 查某店某营业日某 SKU 的**成本日快照期末量**(全店)。
func fmCostDailyClosing(t *testing.T, branchID, bizDate, sku string) (decimal.Decimal, bool) {
	t.Helper()
	var rec model.SkuCostDaily
	err := testDBSingleton.Where("branch_id = ? AND cube_product_id = ? AND biz_date LIKE ?",
		branchID, sku, bizDate+"%").First(&rec).Error
	if err != nil {
		return decimal.Zero, false
	}
	return rec.ClosingKg, true
}

// fmMarginClosing 查某营业日的**实时算**期末结余(per-SKU):柜台 + 冷库。
//
// ⚠️ 审计前**不快照** ⇒ 结余是实时算的 ⇒ 从每日毛利的 per_sku 里读。
func fmMarginClosing(t *testing.T, branchID string, day time.Time, sku string) (counter, cold decimal.Decimal) {
	t.Helper()
	rep, err := service.New(testDBSingleton).ComputeGrossMargin(context.Background(), branchID, day)
	mustNoErr(t, err, "ComputeGrossMargin")
	counter, cold = decimal.Zero, decimal.Zero
	for _, r := range rep.PerSku {
		if r.CubeProductID != sku {
			continue
		}
		counter, cold = r.CounterKg, r.ColdStoreKg
	}
	return counter, cold
}

// ---- ① 前一营业日未盘点 ⇒ 录猪被拒 ----

// TestBizDay_RecordPigRejectedWhenPrevDayNotStocktaken 锁 ①。
//
// 「**不补录昨天,就无法录今天。**」⇒ 10-01 盘过了、10-02 没盘 ⇒ 10-03 录猪被拒,
// 且报错要**说清要补哪天**(10-02)。
func TestBizDay_RecordPigRejectedWhenPrevDayNotStocktaken(t *testing.T) {
	svc, ctx, sku := fmBizSetup(t)
	fmBizSeedDay1(t, svc, ctx, sku)

	// 10-03 进猪 ⇒ 10-02 没盘点 ⇒ 必须被拒。
	day3 := fmDay1.Add(48 * time.Hour)
	svc.SetClock(fmClock(day3))
	_, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-3", GrossWeightKg: d("180"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: day3,
	}, "u-1")
	if err == nil {
		t.Fatalf("10-02 未盘点时录 10-03 的猪应被拒(不补录昨天就无法录今天)")
	}
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Errorf("应是输入校验错(400),got %v", err)
	}
	if !strings.Contains(err.Error(), "2026-10-02") {
		t.Errorf("报错要说清要补哪天(应含 2026-10-02),got %q", err.Error())
	}

	// 补录 10-02 的盘点之后 ⇒ 10-03 就能录猪了。
	_, err = svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		TakenDate:  "2026-10-02",
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("6")},
		},
	}, "u-1")
	mustNoErr(t, err, "补录 10-02 盘点")
	if _, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-3", GrossWeightKg: d("180"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: day3,
	}, "u-1"); err != nil {
		t.Fatalf("补录 10-02 之后应能录 10-03 的猪:%v", err)
	}
}

// ---- ①b. 期初结转的生效日 = 链的起点 ⇒ 那天不需要盘点单 ----

// TestBizDay_OpeningDateNeedsNoStocktake 锁 ①的**例外②**(2026-10-25 门店反馈)。
//
// 门店场景：上线前冷库里的货用**期初结转**记一笔(生效日 10-01)，
// **10-02 才开始第一期营业** ⇒ 10-01 那天没营业、哪来的盘点单？
// 但期初就是那天的基线(链的起点) ⇒ **10-02 必须能录猪**。
//
// ⚠️ 只豁免"**恰好是生效日的那一天**"：10-02 之后仍然要 10-02 的盘点
//   (链是逐日推的,漏一天后面全歪)。
func TestBizDay_OpeningDateNeedsNoStocktake(t *testing.T) {
	svc, ctx, _ := fmBizSetup(t)

	// ⚠️ 先在 **09-30** 盘过一次(上线前最后一次盘)⇒ lastComplete = 09-30
	//    ⇒ 没有豁免的话,录 10-02 的猪会报"10-01 未盘点"（门��的第一期就被挡住）。
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		TakenDate:  "2026-09-30",
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: "P-BELLY", ActualRemainKg: d("5")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("09-30 盘点: %v", err)
	}

	// 期初：五花 100kg @ 18 元(生效日 10-01 = 链的起点)。
	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-01",
			ColdStoreKg: d("100"), UnitPriceYuan: d("18")},
	}); err != nil {
		t.Fatalf("SaveStockOpenings: %v", err)
	}

	// 10-02 = 门店的第一期 ⇒ 录猪必须放行(10-01 是期初生效日,不是营业日)。
	day2 := fmDay1.Add(24 * time.Hour)
	svc.SetClock(fmClock(day2))
	if _, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-D2", GrossWeightKg: d("180"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: day2,
	}, "u-1"); err != nil {
		t.Fatalf("★ 期初生效日(10-01)就是那天的基线 ⇒ 10-02 必须能录猪: %v", err)
	}

	// ⚠️ 但 10-03 仍然要 10-02 的盘点。
	day3 := fmDay1.Add(48 * time.Hour)
	svc.SetClock(fmClock(day3))
	_, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-D3", GrossWeightKg: d("180"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: day3,
	}, "u-1")
	if err == nil {
		t.Fatalf("★ 10-02 没盘点 ⇒ 10-03 录猪必须被拒(只有期初生效日那天被豁免)")
	}
	if !strings.Contains(err.Error(), "2026-10-02") {
		t.Errorf("报错要点名要补哪天(2026-10-02), got %v", err)
	}
}

// ---- ② 同一归属日重复盘点 ⇒ 只有一张单 ----

// TestBizDay_SameBizDateStocktakeKeepsOne 锁 ②。
//
// ⇒ 同一 (门店, 归属日) 只有一张盘点单(确定性主键 + upsert);
// ⇒ 营业日状态被推进到「已盘点」。
func TestBizDay_SameBizDateStocktakeKeepsOne(t *testing.T) {
	svc, ctx, sku := fmBizSetup(t)

	submit := func(kg string) {
		t.Helper()
		_, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
			IsComplete: true,
			Cuts: []model.CutInput{
				{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d(kg)},
			},
		}, "u-1")
		mustNoErr(t, err, "盘点")
	}
	submit("10")
	submit("8")

	var n int64
	if err := testDBSingleton.Model(&model.PorkCutsStocktake{}).
		Where("branch_id = ?", "S001").Count(&n).Error; err != nil {
		t.Fatalf("count stocktakes: %v", err)
	}
	if n != 1 {
		t.Fatalf("同一归属日重复盘点应只有 1 张单,got %d", n)
	}

	// 营业日状态:已盘点。
	bd, err := svc.GetBusinessDay(ctx, "S001", fmDay1)
	mustNoErr(t, err, "GetBusinessDay")
	if bd.Status != service.BizDayStatusStocktaken {
		t.Errorf("营业日状态应是 %q,got %q", service.BizDayStatusStocktaken, bd.Status)
	}
	if !bd.HasStocktake {
		t.Errorf("营业日应标记 has_stocktake=true")
	}
}

// ---- ③ 补录往日的盘点 ⇒ 那天的结余/成本快照重算,今天的不被写脏 ----

// TestBizDay_BackfillRecomputesThatDayOnly 锁 ③。
//
// # 场景
//
//	10-01:进猪 + 分割 10kg + 盘点(柜台实盘 10kg)⇒ 冷库 10kg
//	10-02:搬上台 4kg ⇒ 冷库 6kg、柜台 4kg
//	10-03(今天):**补录 10-02 的盘点**(柜台实盘 2kg)
//
// # 期望
//
//	10-02 的结余快照:柜台 2kg、冷库 6kg、全店 8kg
//	  (日终冷库 = 上期冷库 10 + 本期结转 2 − 本期搬台 4 = 8)
//	10-02 的成本日快照:期末 = 8kg(全店)
//	**今天的位置余额不被写脏**:仍是冷库 6kg、柜台 4kg
//	  (补录只改"那天的快照",不改"当前余额")
//
// # 再补录一次(柜台实盘改成 3kg)⇒ 那天的快照被**覆盖重算**(全店 9kg)。
func TestBizDay_BackfillRecomputesThatDayOnly(t *testing.T) {
	svc, ctx, sku := fmBizSetup(t)
	fmBizSeedDay1(t, svc, ctx, sku)

	// 10-02:搬上台 4kg(归属日 10-02)。
	svc.SetClock(fmClock(fmDay2))
	_, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		Lines: []service.CounterLoadLine{{CubeProductID: sku, QtyKg: d("4")}},
	}, "u-1")
	mustNoErr(t, err, "10-02 搬台")

	coldBefore, counterBefore := fmPositions(t, "S001", sku)
	if !coldBefore.Equal(d("6")) || !counterBefore.Equal(d("4")) {
		t.Fatalf("搬台后应是冷库 6 / 柜台 4,got %s / %s", coldBefore, counterBefore)
	}

	// 10-03(今天):补录 10-02 的盘点。
	day3 := fmDay1.Add(48 * time.Hour)
	svc.SetClock(fmClock(day3))
	backfill := func(kg string) {
		t.Helper()
		_, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
			IsComplete: true,
			TakenDate:  "2026-10-02",
			Cuts: []model.CutInput{
				{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d(kg)},
			},
		}, "u-1")
		mustNoErr(t, err, "补录 10-02 盘点")
	}
	backfill("2")

	// 审计前**不快照** ⇒ 结余是实时算的:10-02 日终柜台 0(夜里搬进冷库)、
	// 冷库 = 上期 10 + 结转 2 − 搬台 4 = 8。
	counter, cold := fmMarginClosing(t, "S001", fmDay2, sku)
	if !counter.IsZero() {
		t.Errorf("10-02 日终柜台应是 0(夜里过夜搬运),got %s", counter)
	}
	if !cold.Equal(d("8")) {
		t.Errorf("10-02 冷库余额应是 8(上期 10 + 结转 2 − 搬台 4),got %s", cold)
	}

	// 10-02 的成本日快照:期末 = 全店 8kg。
	closing, ok := fmCostDailyClosing(t, "S001", "2026-10-02", sku)
	if !ok {
		t.Fatalf("补录后应重算 10-02 的成本日快照")
	}
	if !closing.Equal(d("8")) {
		t.Errorf("10-02 成本快照期末应是 8,got %s", closing)
	}

	// **今天的位置余额不被写脏**。
	coldAfter, counterAfter := fmPositions(t, "S001", sku)
	if !coldAfter.Equal(coldBefore) || !counterAfter.Equal(counterBefore) {
		t.Errorf("补录往日不该改今天的当前余额:冷库 %s→%s 柜台 %s→%s",
			coldBefore, coldAfter, counterBefore, counterAfter)
	}

	// 再补录一次(柜台实盘 3kg)⇒ 那天的结余被**重算**(冷库 9)。
	backfill("3")
	counter2, cold2 := fmMarginClosing(t, "S001", fmDay2, sku)
	if !counter2.IsZero() || !cold2.Equal(d("9")) {
		t.Errorf("重复补录后 10-02 应是柜台 0 / 冷库 9,got %s / %s", counter2, cold2)
	}
	closing2, ok := fmCostDailyClosing(t, "S001", "2026-10-02", sku)
	if !ok || !closing2.Equal(d("9")) {
		t.Errorf("重复补录后 10-02 成本快照期末应是 9,got %s(ok=%v)", closing2, ok)
	}
}

// ---- ④ 两种搬台方式都留流水 ----

// TestBizDay_CounterLoadLeavesLedgerForBothModes 锁 ④。
//
// 「按标签整袋搬」与「按 SKU + kg 搬」**都要留流水**
// ⇒ 否则休市判定与按日结余都会误判(之前按 SKU+kg 搬完全不留痕)。
func TestBizDay_CounterLoadLeavesLedgerForBothModes(t *testing.T) {
	svc, ctx, sku := fmBizSetup(t)

	// 10-01:盘点两行 —— 一袋带标签号(20kg)+ 一行不装袋的散货(10kg)⇒ 冷库 30kg。
	_, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("20"), Label: "BAG-9"},
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("10")},
		},
	}, "u-1")
	mustNoErr(t, err, "10-01 盘点")

	// 10-02:两种搬法各来一次(整袋搬 20kg + 按 SKU+kg 搬 2kg)。
	svc.SetClock(fmClock(fmDay2))
	_, err = svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		Lines: []service.CounterLoadLine{
			{Label: "BAG-9"},
			{CubeProductID: sku, QtyKg: d("2")},
		},
	}, "u-1")
	mustNoErr(t, err, "10-02 搬台")

	view, err := svc.ListCounterLoads(ctx, "S001", fmDay2)
	mustNoErr(t, err, "ListCounterLoads")
	if len(view.Lines) != 2 {
		t.Fatalf("两种搬法应留 2 条流水,got %d: %+v", len(view.Lines), view.Lines)
	}
	bySource := map[string]service.CounterLoadLineView{}
	for _, l := range view.Lines {
		bySource[l.Source] = l
	}
	bag, ok := bySource[service.CounterLoadSourceBag]
	if !ok {
		t.Fatalf("应有整袋搬(source=bag)的流水: %+v", view.Lines)
	}
	if bag.Label != "BAG-9" || !bag.QtyKg.Equal(d("20")) {
		t.Errorf("整袋搬流水不对: %+v", bag)
	}
	if bag.BizDate != "2026-10-02" {
		t.Errorf("流水归属日应是 2026-10-02,got %s", bag.BizDate)
	}
	skuLine, ok := bySource[service.CounterLoadSourceSKU]
	if !ok {
		t.Fatalf("应有按 SKU+kg 搬(source=sku)的流水: %+v", view.Lines)
	}
	if !skuLine.QtyKg.Equal(d("2")) || skuLine.CubeProductID != sku {
		t.Errorf("按 SKU 搬的流水不对: %+v", skuLine)
	}
	if !view.TotalKg.Equal(d("22")) {
		t.Errorf("当天搬台总量应是 22kg,got %s", view.TotalKg)
	}
	if !view.HasCounterLoad {
		t.Errorf("当天应标记 has_counter_load=true")
	}
}

// ---- ⑤ 搬台支持归属日 ----

// TestBizDay_CounterLoadSupportsTakenDate 锁 ⑤。
//
// ⇒ 补录往期的搬台:流水记在**那一天**,冷库从**那一天**扣
//   (不是从今天扣)⇒ 按营业日推导的冷库余额才对得上。
func TestBizDay_CounterLoadSupportsTakenDate(t *testing.T) {
	svc, ctx, sku := fmBizSetup(t)
	fmBizSeedDay1(t, svc, ctx, sku)

	// 10-03(今天):补录 10-02 的搬台 4kg。
	day3 := fmDay1.Add(48 * time.Hour)
	svc.SetClock(fmClock(day3))
	_, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		TakenDate: "2026-10-02",
		Lines:     []service.CounterLoadLine{{CubeProductID: sku, QtyKg: d("4")}},
	}, "u-1")
	mustNoErr(t, err, "补录 10-02 搬台")

	// 流水记在 10-02。
	view, err := svc.ListCounterLoads(ctx, "S001", fmDay2)
	mustNoErr(t, err, "ListCounterLoads(10-02)")
	if len(view.Lines) != 1 {
		t.Fatalf("10-02 应有 1 条搬台流水,got %d", len(view.Lines))
	}
	if view.Lines[0].BizDate != "2026-10-02" {
		t.Errorf("流水归属日应是 2026-10-02,got %s", view.Lines[0].BizDate)
	}

	// 按营业日推导的冷库余额(实时算):10-02 虽没有盘点单,但有搬台流水
	// ⇒ 日终冷库 = 上期 10 − 搬台 4 = 6(补录往期的搬台扣的是**那一天**的冷库)。
	_, cold2 := fmMarginClosing(t, "S001", fmDay2, sku)
	if !cold2.Equal(d("6")) {
		t.Errorf("10-02 冷库余额应是 6(上期 10 − 搬台 4),got %s", cold2)
	}
	// 10-01 的结余不受影响。
	_, cold1 := fmMarginClosing(t, "S001", fmDay1, sku)
	if !cold1.Equal(d("10")) {
		t.Errorf("10-01 冷库余额应是 10,got %s", cold1)
	}

	// 补录往期的搬台 **不许** 把归属日写成今天。
	view3, err := svc.ListCounterLoads(ctx, "S001", day3)
	mustNoErr(t, err, "ListCounterLoads(10-03)")
	if len(view3.Lines) != 0 {
		t.Errorf("10-03 不该有搬台流水(补录记在 10-02),got %d", len(view3.Lines))
	}
}

// ---- ⑥ 盘点实盘只报柜台,冷库另列 ----

// TestBizDay_StocktakeReportsCounterOnlyAndColdSeparately 锁 ⑥。
//
// 「盘点盘的是猪肉台,不盘冷库」⇒
//   - actual_remain_kg = **柜台实盘**(不含冷库);
//   - cold_store_kg = **冷库余额**(单独一个字段)。
func TestBizDay_StocktakeReportsCounterOnlyAndColdSeparately(t *testing.T) {
	svc, ctx, sku := fmBizSetup(t)
	fmBizSeedDay1(t, svc, ctx, sku)

	// 10-02:再盘一次(柜台实盘 3kg);此时冷库有 10kg。
	svc.SetClock(fmClock(fmDay2))
	st, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		TakenDate:  "2026-10-02",
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("3")},
		},
	}, "u-1")
	mustNoErr(t, err, "10-02 盘点")

	var snaps []model.CutSnapshot
	if err := json.Unmarshal(st.Cuts, &snaps); err != nil {
		t.Fatalf("unmarshal cuts: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("cuts 应有 1 行,got %d", len(snaps))
	}
	row := snaps[0]
	if !row.ActualRemainKg.Equal(d("3")) {
		t.Errorf("actual_remain_kg 应是**柜台实盘** 3,got %s(不能含冷库)", row.ActualRemainKg)
	}
	// 冷库余额另列:上期冷库 10kg(10-02 没搬台)− 当天搬台 0 ⇒ 10kg。
	if !row.ColdStoreKg.Equal(d("10")) {
		t.Errorf("cold_store_kg 应是 10(冷库余额单独给出),got %s", row.ColdStoreKg)
	}
	if !row.TotalRemainKg.Equal(d("13")) {
		t.Errorf("total_remain_kg 应是 13(柜台 3 + 冷库 10),got %s", row.TotalRemainKg)
	}
}
