package service_test

// 2026-10-25：**期初生效日当天也录猪** ⇒ 成本链会不会算错？
//
// # 场景（门店真实用法）
//
//	期初结转：belly 100kg @ 18 元（生效日 2026-10-01）⇒ 链的起点
//	**同一天（10-01）**又录了 1 头猪 40kg @ 20 元（成本 800），
//	分割出 40kg belly（当天切好的直接上猪肉台），
//	把冷库的 20kg 搬上猪肉台（搬的是期初那批货），
//	日终盘点：柜台实盘 60kg（搬上台 20 + 当天切 40）。
//
// # 进出逻辑（链的口径）
//
//	冷库(10-01 日终) = 期初 100 + 柜台实盘 60 − 搬台 20 = **140**
//	  ⇒ 物理：冷库 80（100−20）+ 柜台 60 = 140（夜里柜台搬回冷库）
//
//	成本引擎：期初 100kg/1800 元 + 产出 40kg/800 元 = 可用 140kg/2600 元
//	  ⇒ 加权单价 2600/140；期末 140kg ⇒ 成本恒等式差额 = 0
//
// # 断言（四条）
//
//	① 10-01 的**期初** = 门店录的那组数（100kg / 1800 元）—— 不是 0；
//	② 10-01 的**产出** = 当天那头猪（40kg / 800 元）—— 不与期初混在一起；
//	③ 10-01 的**期末** = 链算的日终（140kg）⇒ 成本恒等式差额 = 0；
//	④ 10-02 的**期初** = 10-01 的期末（140kg / 2600 元）⇒ **链式接续，期初没丢**。

import (
	"strings"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/shopspring/decimal"
)

// TestOpeningAndPigsSameDay_CostChainStaysConsistent 锁"期初当天也录猪 ⇒ 不算错"。
func TestOpeningAndPigsSameDay_CostChainStaysConsistent(t *testing.T) {
	svc, ctx, sku := fmBizSetup(t)
	day1 := fmDay1                     // 2026-10-01（期初生效日 = 第一期营业日）
	day2 := fmDay1.Add(24 * time.Hour) // 2026-10-02

	// ---- 期初结转：belly 100kg @ 18（生效日 10-01）----
	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-01",
			ColdStoreKg: d("100"), UnitPriceYuan: d("18")},
	}); err != nil {
		t.Fatalf("SaveStockOpenings: %v", err)
	}

	// ---- 同一天：录 1 头猪 40kg @ 20，切 40kg belly ----
	pig, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-1", GrossWeightKg: d("40"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: day1,
	}, "u-1")
	mustNoErr(t, err, "RecordWholePig")
	if _, err := svc.RecordPigCut(ctx, "S001", model.RecordPigCutInput{
		PigID: pig.ID, CutType: model.CutBelly, Barcode: "BC-1",
		WeightKg: d("40"), ExpiresAt: day1.Add(48 * time.Hour), CubeProductID: sku,
	}, "u-1"); err != nil {
		t.Fatalf("RecordPigCut: %v", err)
	}

	// ---- 把冷库的 20kg（期初那批）搬上猪肉台 ----
	if _, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		TakenDate: "2026-10-01",
		Lines:     []service.CounterLoadLine{{CubeProductID: sku, QtyKg: d("20")}},
	}, "u-1"); err != nil {
		t.Fatalf("CounterLoad: %v", err)
	}

	// ---- 日终盘点：柜台实盘 60kg（搬上台 20 + 当天切 40）----
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		TakenDate:  "2026-10-01",
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("60")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("RecordPorkCutsStocktake: %v", err)
	}

	// ---- ① ② ③ 10-01 当天的成本明细 ----
	rep1, err := service.New(testDBSingleton).ComputeGrossMargin(ctx, "S001", day1)
	mustNoErr(t, err, "ComputeGrossMargin(10-01)")
	row1 := fmRowForSKU(t, rep1, sku)

	if !row1.OpeningKg.Equal(d("100")) {
		t.Errorf("★ 10-01 的期初 kg 应是门店录的 100(链的起点), got %s", row1.OpeningKg)
	}
	if !row1.OpeningAmount.Equal(d("1800")) {
		t.Errorf("★ 10-01 的期初金额应是 100×18=1800, got %s —— "+
			"0 ⇒ 那批货零成本 ⇒ 当天毛利虚高", row1.OpeningAmount)
	}
	if !row1.ProducedKg.Equal(d("40")) {
		t.Errorf("10-01 的产出 kg 应是当天那头猪切出的 40, got %s", row1.ProducedKg)
	}
	if !row1.ProducedAmount.Equal(d("800")) {
		t.Errorf("10-01 的产出金额应是那头猪的成本 40×20=800, got %s", row1.ProducedAmount)
	}
	// 期末 = 链的日终 = 期初 100 + 柜台实盘 60 − 搬台 20 = 140。
	if !row1.ClosingKg.Equal(d("140")) {
		t.Errorf("★ 10-01 的期末应是 140kg(100+60−20), got %s", row1.ClosingKg)
	}
	// 成本恒等式：期初 1800 + 产出 800 = 期末 2600 ⇒ 差额 0。
	if !rep1.Reconciliation.DiffYuan.Equal(decimal.Zero) {
		t.Errorf("★ 10-01 的成本恒等式差额应是 0, got %s", rep1.Reconciliation.DiffYuan)
	}

	// ---- ④ 10-02 的期初 = 10-01 的期末（链式接续，期初没丢）----
	svc.SetClock(fmClock(day2))
	rep2, err := service.New(testDBSingleton).ComputeGrossMargin(ctx, "S001", day2)
	mustNoErr(t, err, "ComputeGrossMargin(10-02)")
	row2 := fmRowForSKU(t, rep2, sku)
	if !row2.OpeningKg.Equal(d("140")) {
		t.Errorf("★ 10-02 的期初 kg 应接上 10-01 的期末 140, got %s —— "+
			"链断了(期初/产出在次日丢了)", row2.OpeningKg)
	}
	if !row2.OpeningAmount.Equal(d("2600")) {
		t.Errorf("★ 10-02 的期初金额应接上 2600(1800+800), got %s", row2.OpeningAmount)
	}
}

// TestOpeningBaselineDay_NextDayOpeningKeepsOpeningCost 锁"纯基线日的次日，期初在成本链里"。
//
// # 场景
//
//	期初 100kg @ 18（生效日 10-01），**10-01 什么都没做**（纯基线日：链的起点）
//	⇒ **10-02 才是第一期营业**
//
// # 断言
//
//	10-02 的**期初** = 100kg / 1800 元（期初那批货）
//	⇒ 不补这一条 ⇒ 上线前那批货的成本在 10-02 **消失** ⇒ **毛利虚高**。
func TestOpeningBaselineDay_NextDayOpeningKeepsOpeningCost(t *testing.T) {
	svc, ctx, sku := fmBizSetup(t)
	day2 := fmDay1.Add(24 * time.Hour)

	// 期初（10-01），那天**什么都不做**（纯基线日）。
	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-01",
			ColdStoreKg: d("100"), UnitPriceYuan: d("18")},
	}); err != nil {
		t.Fatalf("SaveStockOpenings: %v", err)
	}

	// 10-02：第一期营业（录一头猪 + 切 40kg）。
	pig, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-1", GrossWeightKg: d("40"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: day2,
	}, "u-1")
	mustNoErr(t, err, "10-02 录猪（纯基线日的次日应当放行）")
	if _, err := svc.RecordPigCut(ctx, "S001", model.RecordPigCutInput{
		PigID: pig.ID, CutType: model.CutBelly, Barcode: "BC-2",
		WeightKg: d("40"), ExpiresAt: day2.Add(48 * time.Hour), CubeProductID: sku,
	}, "u-1"); err != nil {
		t.Fatalf("RecordPigCut: %v", err)
	}

	rep, err := service.New(testDBSingleton).ComputeGrossMargin(ctx, "S001", day2)
	mustNoErr(t, err, "ComputeGrossMargin(10-02)")
	row := fmRowForSKU(t, rep, sku)
	if !row.OpeningKg.Equal(d("100")) {
		t.Errorf("★ 10-02 的期初 kg 应是上线前那批货的 100, got %s —— "+
			"期初在成本链上消失了 ⇒ 毛利虚高", row.OpeningKg)
	}
	if !row.OpeningAmount.Equal(d("1800")) {
		t.Errorf("★ 10-02 的期初金额应是 100×18=1800, got %s", row.OpeningAmount)
	}
}

// fmRowForSKU 从每日毛利报告里取某个 SKU 的明细行。
func fmRowForSKU(t *testing.T, rep *service.GrossMarginReport, sku string) service.SkuDailyCost {
	t.Helper()
	for _, r := range rep.PerSku {
		if r.CubeProductID == sku {
			return r
		}
	}
	t.Fatalf("每日毛利里应有 %s 这一行: %+v", sku, rep.PerSku)
	return service.SkuDailyCost{}
}

// TestOpeningSameDay_NoStocktakeBlocksNextDay 锁"链断不了"。
//
// # 为什么要有这条
//
// 成本链的接续靠**上一营业日的日终快照**（sku_cost_daily）⇒ 而快照只在**盘点**时落。
// ⇒ 门店如果**期初当天不盘点** ⇒ 次日的期初就没有（回退到 0）⇒ 期初那批货的
//   成本在次日**消失** ⇒ 次日毛利虚高。
//
// ⇒ 但**跨日顺序校验**先挡住了：d0 没盘点 ⇒ d0+1 录猪/搬台被拒
//   （"d0 未盘点，请先补录 d0 的盘点"）⇒ 门店只能补录 d0 的盘点
// ⇒ 链的起点那天**必然**有快照 ⇒ **链断不了**。
func TestOpeningSameDay_NoStocktakeBlocksNextDay(t *testing.T) {
	svc, ctx, sku := fmBizSetup(t)
	day1 := fmDay1
	day2 := fmDay1.Add(24 * time.Hour)

	// 期初 + 当天录猪，但**故意不盘点**。
	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-01",
			ColdStoreKg: d("100"), UnitPriceYuan: d("18")},
	}); err != nil {
		t.Fatalf("SaveStockOpenings: %v", err)
	}
	if _, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-1", GrossWeightKg: d("40"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: day1,
	}, "u-1"); err != nil {
		t.Fatalf("RecordWholePig: %v", err)
	}

	// 10-02 录猪 ⇒ 必须被拒（10-01 没盘点）⇒ 链断不了。
	svc.SetClock(fmClock(day2))
	_, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-2", GrossWeightKg: d("40"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: day2,
	}, "u-1")
	if err == nil {
		t.Fatalf("★ 10-01 没盘点 ⇒ 10-02 录猪必须被拒（否则次日没有期初 ⇒ 成本链断）")
	}
	if !strings.Contains(err.Error(), "2026-10-01") {
		t.Errorf("★ 报错要点名要补哪天(2026-10-01), got %v", err)
	}
	_ = sku
}
