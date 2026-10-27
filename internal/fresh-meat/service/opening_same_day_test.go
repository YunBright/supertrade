package service_test

// 2026-10-25：**期初生效日 = 链的起点 = 基线日 ⇒ 它不是营业日**。
//
// # 口径（门店反馈后的定案）
//
//	门店的用法：上线前冷库里的货用**期初结转**记一笔（基线日 = 上线**前一天**），
//	**从次日开始第一期营业**。
//
//	⇒ 基线日本身**不能录猪/搬台**（也不该在日期控件上被选到）：
//	   那天的"日终"就等于期初本身，一旦有进出，链的第一天就不再是那批期初货
//	   ⇒ 成本链的第一天对不上 ⇒ **毛利算不准**。
//
// # 场景（门店真实用法）
//
//	期初结转：belly 100kg @ 18（生效日 2026-10-01 = 基线日）
//	**2026-10-02 才是第一期营业**：
//	  录 1 头猪 40kg @ 20（成本 800），切 40kg belly；
//	  把冷库的 20kg 搬上猪肉台；日终盘点柜台实盘 60kg。
//
// # 进出逻辑（链的口径）
//
//	冷库(10-02 日终) = 期初 100 + 柜台实盘 60 − 搬台 20 = **140**
//	  ⇒ 物理：冷库 80（100−20）+ 柜台 60 = 140（夜里柜台搬回冷库）
//
//	成本引擎：期初 100kg/1800 元 + 产出 40kg/800 元 = 可用 140kg/2600 元
//	  ⇒ 加权单价 2600/140；期末 140kg ⇒ 成本恒等式差额 = 0
//
// # 断言
//
//	① 10-02 的**期初** = 门店录的那组数（100kg / 1800 元）—— 不是 0；
//	② 10-02 的**产出** = 当天那头猪（40kg / 800 元）—— 不与期初混在一起；
//	③ 10-02 的**期末** = 链算的日终（140kg）⇒ 成本恒等式差额 = 0；
//	④ 10-03 的**期初** = 10-02 的期末（140kg / 2600 元）⇒ **链式接续，期初没丢**；
//	⑤ **基线日（10-01）录猪 ⇒ 400**（并说清"请从 10-02 开始"）。

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/shopspring/decimal"
)

// TestOpeningBaselineDay_FirstBusinessDayCostChainIsConsistent 锁 ①～④。
func TestOpeningBaselineDay_FirstBusinessDayCostChainIsConsistent(t *testing.T) {
	svc, ctx, sku := fmBizSetup(t)
	day2 := fmDay1.Add(24 * time.Hour) // 2026-10-02（第一期营业日）
	day3 := fmDay1.Add(48 * time.Hour) // 2026-10-03

	// ---- 期初结转：belly 100kg @ 18（生效日 10-01 = 基线日）----
	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-01",
			ColdStoreKg: d("100"), UnitPriceYuan: d("18")},
	}); err != nil {
		t.Fatalf("SaveStockOpenings: %v", err)
	}

	// ---- 10-02：第一期营业 ----
	svc.SetClock(fmClock(day2))
	pig, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-1", GrossWeightKg: d("40"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: day2,
	}, "u-1")
	mustNoErr(t, err, "10-02 录猪")
	if _, err := svc.RecordPigCut(ctx, "S001", model.RecordPigCutInput{
		PigID: pig.ID, CutType: model.CutBelly, Barcode: "BC-1",
		WeightKg: d("40"), ExpiresAt: day2.Add(48 * time.Hour), CubeProductID: sku,
	}, "u-1"); err != nil {
		t.Fatalf("RecordPigCut: %v", err)
	}

	// 把冷库的 20kg（期初那批）搬上猪肉台。
	if _, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		TakenDate: "2026-10-02",
		Lines:     []service.CounterLoadLine{{CubeProductID: sku, QtyKg: d("20")}},
	}, "u-1"); err != nil {
		t.Fatalf("CounterLoad: %v", err)
	}

	// 日终盘点：柜台实盘 60kg（搬上台 20 + 当天切 40）。
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		TakenDate:  "2026-10-02",
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("60")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("RecordPorkCutsStocktake: %v", err)
	}

	// ---- ① ② ③ 10-02 当天的成本明细 ----
	//
	// ⚠️ 2026-10-27:成本链改**部位级** ⇒ 断言从 per_sku(SKU 粒度)改成
	//   **per_cut(部位粒度)**:库存 / 期初 / 期末 / 成本 / 毛利都是部位口径
	//   (per_sku 现在只承载销售 / 营收 / 产出)。
	rep1, err := service.New(testDBSingleton).ComputeGrossMargin(ctx, "S001", day2)
	mustNoErr(t, err, "ComputeGrossMargin(10-02)")
	row1 := fmCutRowFor(t, rep1, model.CutBelly)

	if !row1.OpeningKg.Equal(d("100")) {
		t.Errorf("★ 10-02 的期初 kg 应是门店录的 100(链的起点), got %s", row1.OpeningKg)
	}
	// 期初金额 = 100 × 18 = 1800(部位级 ⇒ 走对账行的期初存货金额)。
	if !rep1.Reconciliation.OpeningInventoryCostYuan.Equal(d("1800")) {
		t.Errorf("★ 10-02 的期初金额应是 100×18=1800, got %s —— "+
			"0 ⇒ 那批货零成本 ⇒ 当天毛利虚高", rep1.Reconciliation.OpeningInventoryCostYuan)
	}
	if !row1.PurchasedKg.Equal(d("40")) {
		t.Errorf("10-02 的产出 kg 应是当天那头猪切出的 40, got %s", row1.PurchasedKg)
	}
	// 产出金额 = 那头猪的成本 40 × 20 = 800。
	if !rep1.Reconciliation.ProducedCostYuan.Equal(d("800")) {
		t.Errorf("10-02 的产出金额应是那头猪的成本 40×20=800, got %s", rep1.Reconciliation.ProducedCostYuan)
	}
	// 账面应有 = 期初 100 + 产出 40 = 140(= 链算的日终 100+60−20)。
	if !row1.ExpectedRemainKg.Equal(d("140")) {
		t.Errorf("★ 10-02 的应有应是 140kg(100+60−20), got %s", row1.ExpectedRemainKg)
	}
	// 实盘(= 按营业日推导的日终结余)也是 140。
	if row1.ActualRemainKg == nil || !row1.ActualRemainKg.Equal(d("140")) {
		t.Errorf("★ 10-02 的实盘剩余应是 140kg, got %v", row1.ActualRemainKg)
	}
	// 成本恒等式：期初 1800 + 产出 800 = 期末 2600 ⇒ 差额 0。
	if !rep1.Reconciliation.DiffYuan.Equal(decimal.Zero) {
		t.Errorf("★ 10-02 的成本恒等式差额应是 0, got %s", rep1.Reconciliation.DiffYuan)
	}

	// ---- ④ 10-03 的期初 = 10-02 的期末（链式接续，期初没丢）----
	svc.SetClock(fmClock(day3))
	rep2, err := service.New(testDBSingleton).ComputeGrossMargin(ctx, "S001", day3)
	mustNoErr(t, err, "ComputeGrossMargin(10-03)")
	row2 := fmCutRowFor(t, rep2, model.CutBelly)
	if !row2.OpeningKg.Equal(d("140")) {
		t.Errorf("★ 10-03 的期初 kg 应接上 10-02 的期末 140, got %s —— "+
			"链断了(期初/产出在次日丢了)", row2.OpeningKg)
	}
	if !rep2.Reconciliation.OpeningInventoryCostYuan.Equal(d("2600")) {
		t.Errorf("★ 10-03 的期初金额应接上 2600(1800+800), got %s",
			rep2.Reconciliation.OpeningInventoryCostYuan)
	}
}

// TestChainStartDayIsNotABusinessDay 锁 ⑤。
//
// **基线日（期初生效日）不是营业日** ⇒ 录猪/搬台一律 400，
// 而且要**说清从哪天开始**（门店照着改就行）。
func TestChainStartDayIsNotABusinessDay(t *testing.T) {
	svc, ctx, _ := fmBizSetup(t)
	day1 := fmDay1

	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-01",
			ColdStoreKg: d("100"), UnitPriceYuan: d("18")},
	}); err != nil {
		t.Fatalf("SaveStockOpenings: %v", err)
	}

	// 基线日当天录猪 ⇒ 拒绝。
	_, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-ON-START", GrossWeightKg: d("180"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: day1,
	}, "u-1")
	if err == nil {
		t.Fatalf("★ 基线日(2026-10-01 = 期初生效日)录猪必须被拒(它不是营业日)")
	}
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Errorf("应是输入校验错(400), got %v", err)
	}
	if !strings.Contains(err.Error(), "2026-10-02") {
		t.Errorf("★ 报错要说清从哪天开始录(2026-10-02), got %v", err)
	}
}

// fmCutRowFor 从每日毛利报告里取某个**部位**的明细行(成本链的计算单元)。
//
// ⚠️ 2026-10-27:成本链改部位级 ⇒ per_sku **不再承载**库存/成本/毛利
//   (那些是部位口径)⇒ 部位口径的断言走这里。
func fmCutRowFor(t *testing.T, rep *service.GrossMarginReport, cut model.CutType) service.PerCutGrossMargin {
	t.Helper()
	for _, r := range rep.PerCut {
		if r.CutType == cut {
			return r
		}
	}
	t.Fatalf("每日毛利里应有 %s 这一行: %+v", cut, rep.PerCut)
	return service.PerCutGrossMargin{}
}
