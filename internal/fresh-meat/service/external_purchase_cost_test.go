package service_test

// 2026-11-03:外采进**成本口径**的回归锁(owner 拍板)。
//
// # owner 的两条要求
//
//	① 长期毛利的「期间采购」**要含外采**。
//	② 参考毛利要**反映外采成本**:不是「直接换」成本单价,而是**求均值**;
//	   而且这个均价要**能被价格基线采样**采到。
//
// # 三条不变量
//
//	① 参考毛利的**部位成本单价** = (建议成本单价 × 猪分割产出 + 外采价 × 外采量)
//	   ÷ 总量 ⇒ 按量加权(不是直接换成外采价)。
//	② 长期毛利的**期间采购 / 采购均价**含外采(进猪 + 外采 ⇒ 总金额 ÷ 总重量)。
//	③ **单价基线采样**:样本值 = 倒推单价与外采价按量加权,且样本带 external_kg
//	   ⇒ 聚合权重 = 销量 + 外采量 ⇒ 外采的真实进货价进得了基线。
//
// ⚠️ 判别力:把加权去掉(直接用外采价 / 直接用倒推价)⇒ ①③ 的断言立刻红。

import (
	"testing"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/shopspring/decimal"
)

// fmExtSamples 查某店某天某部位的单价样本。
func fmExtSamples(t *testing.T, branchID, bizDate string, cut model.CutType) []model.PigCutPriceSample {
	t.Helper()
	var rows []model.PigCutPriceSample
	if err := testDBSingleton.Where("branch_id = ? AND biz_date = ? AND cut_type = ?",
		branchID, bizDate, string(cut)).Find(&rows).Error; err != nil {
		t.Fatalf("query pig_cut_price_samples: %v", err)
	}
	return rows
}

// ---- ① 参考毛利的部位成本单价:按量加权 ----

// TestExternalPurchase_ReferenceUnitPriceBlendsByWeight 锁 ①。
//
// ⚠️ 场景:五花肉建议成本单价 14 元/kg、当天猪分割产出 60kg;
//
//	外采同一部位 10kg @ 24 元/kg ⇒ 单价 = (14×60 + 24×10) ÷ 70。
//
// ⚠️ 判别力:
//   - 去掉加权(直接用建议价)⇒ 单价 = 14 ⇒ 红;
//   - 直接换成外采价 ⇒ 单价 = 24 ⇒ 红。
func TestExternalPurchase_ReferenceUnitPriceBlendsByWeight(t *testing.T) {
	suggested := decimal.RequireFromString("14")
	splitKg := decimal.RequireFromString("60")
	rows, _ := service.RollUpPerCutCutsForTest([]service.CutDailyCost{
		{
			BranchID:                   "00",
			CutType:                    model.CutBelly,
			DataSource:                 "estimated",
			SoldKg:                     decimal.RequireFromString("10"),
			RevenueYuan:                decimal.RequireFromString("200"),
			SuggestedCostUnitPriceYuan: &suggested,
			SuggestedCostPriceSource:   "baseline",
			SuggestedSplitKg:           splitKg,
		},
	}, nil, service.ReferenceDayTotalsWithExternalForTest("100", "1400", "10", "240", "20", true))

	var got *service.PerCutGrossMargin
	for i := range rows {
		if rows[i].CutType == model.CutBelly {
			got = &rows[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("部位 %s 不在 per_cut", model.CutBelly)
	}
	if got.ReferenceCostUnitPriceYuan == nil {
		t.Fatal("★ 有外采时,部位参考成本单价必须给出来(不能是 nil)")
	}
	want := decimal.RequireFromString("14").Mul(splitKg).
		Add(decimal.RequireFromString("24").Mul(decimal.RequireFromString("10"))).
		Div(splitKg.Add(decimal.RequireFromString("10")))
	if !got.ReferenceCostUnitPriceYuan.Round(6).Equal(want.Round(6)) {
		t.Fatalf("★ 部位成本单价应按量加权 = %s,实际 %s", want, got.ReferenceCostUnitPriceYuan)
	}
	if got.ReferenceCostPriceSource != "baseline_external" {
		t.Errorf("来源标记应是 baseline_external(前端要能说清这个价含外采),got %q",
			got.ReferenceCostPriceSource)
	}
}

// ---- ② 长期毛利的期间采购含外采 ----

// TestExternalPurchase_LongTermMarginIncludesExternal 锁 ②。
//
// ⚠️ owner 原话:「长期毛利的期间采购**要含外采**」
// ⇒ 录猪 50kg @ 20(=1000 元)+ 外采 10kg @ 24(=240 元)
// ⇒ 期间采购 = 60kg / 1240 元,均价 = 1240/60;外采单列 10kg / 240 元。
func TestExternalPurchase_LongTermMarginIncludesExternal(t *testing.T) {
	svc, ctx, sku := fmExtSetup(t)

	pigID := fmSeedPigAt(t, svc, "EB-EXT", "50", fmDay1)
	fmSeedPigCut(t, svc, pigID, sku, "10")
	if _, err := svc.RecordExternalPurchase(ctx, "S001", service.ExternalPurchaseInput{
		CubeProductID: sku, QtyKg: d("10"), UnitPriceYuan: d("24"),
	}, "u-1"); err != nil {
		t.Fatalf("RecordExternalPurchase: %v", err)
	}
	// 盘点(实盘 0):让当天的链与毛利算得出来。
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("0")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("10-01 盘点: %v", err)
	}

	rep, err := svc.ComputeLongTermMargin(ctx, "S001", "2026-10-01", "2026-10-01")
	mustNoErr(t, err, "ComputeLongTermMargin")

	if !rep.Total.PurchasedKg.Equal(d("60")) {
		t.Errorf("★ 期间采购重量应是 60kg(进猪 50 + 外采 10),got %s", rep.Total.PurchasedKg)
	}
	if !rep.Total.PurchasedYuan.Equal(d("1240")) {
		t.Errorf("★ 期间采购金额应是 1240 元(进猪 1000 + 外采 240),got %s", rep.Total.PurchasedYuan)
	}
	if !rep.Total.ExternalKg.Equal(d("10")) {
		t.Errorf("期间外采应是 10kg,got %s", rep.Total.ExternalKg)
	}
	if !rep.Total.ExternalYuan.Equal(d("240")) {
		t.Errorf("期间外采金额应是 240 元,got %s", rep.Total.ExternalYuan)
	}
	// 均价 = 总金额 ÷ 总重量(=「求均值」)。
	wantAvg := d("1240").Div(d("60"))
	if !rep.Total.PurchaseAvgYuan.Round(6).Equal(wantAvg.Round(6)) {
		t.Errorf("★ 期间采购均价应是 1240/60 = %s,got %s", wantAvg, rep.Total.PurchaseAvgYuan)
	}
}

// ---- ③ 单价基线采样:外采价进样本 ----

// TestExternalPurchase_PriceSampleIncludesExternal 锁 ③。
//
// ⚠️ 场景:当天该部位零售 100kg / 营收 2000 元(零售均价 20);
//
//	全店采购 = 进猪 1000 + 外采 240 = 1240 ⇒ m = (2000 − 1240) ÷ 2000 = 0.38
//	⇒ 倒推单价 = 20 ÷ 1.38;外采 10kg @ 24 ⇒ 样本值 = 按量加权。
//
// ⚠️ 判别力:
//   - 样本不带 external_kg ⇒ 聚合权重里没有外采 ⇒ 断言 ① 红;
//   - 样本值不用加权(直接用倒推价 / 直接用外采价)⇒ 断言 ② 红。
func TestExternalPurchase_PriceSampleIncludesExternal(t *testing.T) {
	svc, ctx, sku := fmExtSetup(t)

	pigID := fmSeedPigAt(t, svc, "EB-SMP", "50", fmDay1)
	fmSeedPigCut(t, svc, pigID, sku, "10")
	if _, err := svc.RecordExternalPurchase(ctx, "S001", service.ExternalPurchaseInput{
		CubeProductID: sku, QtyKg: d("10"), UnitPriceYuan: d("24"),
	}, "u-1"); err != nil {
		t.Fatalf("RecordExternalPurchase: %v", err)
	}
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("0")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("10-01 盘点: %v", err)
	}

	rows := []service.CutDailyCost{{
		BranchID:    "00",
		CutType:     model.CutBelly,
		DataSource:  "actual",
		SoldKg:      d("100"),
		RevenueYuan: d("2000"),
	}}
	res, err := service.CollectCutPriceSamplesForTest(ctx, svc, "S001", fmDay1, rows)
	mustNoErr(t, err, "CollectCutPriceSamples")
	if res.Written == 0 {
		t.Fatalf("当天应该产出单价样本,got %+v", res)
	}

	samples := fmExtSamples(t, "S001", "2026-10-01", model.CutBelly)
	if len(samples) != 1 {
		t.Fatalf("五花肉应有 1 条单价样本,got %d", len(samples))
	}
	// ① 外采量进了样本(聚合权重 = 销量 + 外采量)。
	if !samples[0].ExternalKg.Equal(d("10")) {
		t.Fatalf("★ 样本的外采量应是 10kg,got %s", samples[0].ExternalKg)
	}
	// ② 样本值 = 倒推单价 与 外采价 按量加权。
	retail := d("2000").Div(d("100"))
	m := d("2000").Sub(d("1240")).Div(d("2000"))
	derived := retail.Div(d("1").Add(m))
	want := derived.Mul(d("100")).Add(d("24").Mul(d("10"))).Div(d("110"))
	if !samples[0].CostPriceYuan.Round(6).Equal(want.Round(6)) {
		t.Fatalf("★ 样本值应按量加权 = %s(倒推 %s 与外采 24),实际 %s", want, derived,
			samples[0].CostPriceYuan)
	}
	if !samples[0].CostPriceYuan.GreaterThan(derived) {
		t.Errorf("样本值必须比倒推单价高(外采价 24 > 倒推 %s),got %s", derived, samples[0].CostPriceYuan)
	}
	if !samples[0].CostPriceYuan.LessThan(d("24")) {
		t.Errorf("样本值必须小于外采价 24(是加权,不是直接换),got %s", samples[0].CostPriceYuan)
	}
}
