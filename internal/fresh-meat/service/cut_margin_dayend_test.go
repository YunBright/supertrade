package service_test

// 回归锁:**参考口径**(2026-11-01 owner 口径切换 = B 口径)。
//
// # 口径(B)
//
//	成本单价_c = **建议成本单价**(基线单价 × 今天这头猪的进价折算)
//	参考毛利_c = 营收_c − 成本单价_c×销量_c − 搭赠成本_c − 报损成本_c
//	全店参考毛利 = **Σ 部位参考毛利**(头与部位同源)
//
// # 四条不能违反的性质(每条都有判别力用例)
//
//	① **没有日终盘点 ⇒ 参考毛利照样有值**(owner 2026-11-01 拍板换口径的原因)
//	   ⇒ 用例:TestReferenceMarginWithoutStocktakeStillHasValue
//	② **header = 各部位参考毛利之和**(B 口径同源)
//	   ⇒ 用例:TestReferenceHeaderEqualsSumOfParts
//	③ **当天没进货 ⇒ 参考毛利给不出(nil)**,绝不回落成 0/100%
//	   ⇒ 用例:TestReferenceMarginNeedsPurchase
//	④ **出成率 > 100% ⇒ 有问题**(owner:「它大于100%,就知道有问题,就需要查」)
//	   ⇒ 用例:TestYieldRateOver100MeansProblem
//
// # 为什么日终剩余的金额用**采购均价**
//
//	旧口径「零售均价 ÷ (1 + 毛利率)」里含 m ⇒ 拿它估值期末库存 ⇒ **成环**。
//	采购均价不含 m ⇒ 天然无环 ⇒ 才能当 m 的输入。
//
// ⚠️ 日终剩余/售罄率**仍然依赖日终实盘**(它们的分母是期末实盘)⇒ 没盘点时仍是「—」。

import (
	"testing"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/shopspring/decimal"
)

// fmDayDec 把字面量转成 decimal(测试里不用 decimal.NewFromFloat,避免浮点噪声)。
func fmDayDec(f float64) decimal.Decimal {
	return decimal.NewFromFloat(f)
}

// fmDayEndRow 造一条部位行。
func fmDayEndRow(cut model.CutType, soldKg, revenue, salesCost, remainKg float64,
	dataSource string) service.CutDailyCost {
	return service.CutDailyCost{
		BranchID:        "00",
		CutType:         cut,
		DataSource:      dataSource,
		SoldKg:          fmDayDec(soldKg),
		RevenueYuan:     fmDayDec(revenue),
		SalesCostYuan:   fmDayDec(salesCost),
		CounterActualKg: fmDayDec(remainKg),
	}
}

// fmDayEndRowWithSuggested 造一条**带建议成本单价**的部位行(B 口径的正统输入)。
//
// ⚠️ B 口径下,部位成本单价 = 建议成本单价(基线 × 今天进价折算)⇒ 没有它就落兜底。
func fmDayEndRowWithSuggested(cut model.CutType, soldKg, revenue, salesCost, remainKg float64,
	dataSource string, suggestedUnit string,
) service.CutDailyCost {
	row := fmDayEndRow(cut, soldKg, revenue, salesCost, remainKg, dataSource)
	v := decimal.RequireFromString(suggestedUnit)
	row.SuggestedCostUnitPriceYuan = &v
	row.SuggestedCostPriceSource = "baseline"
	return row
}

// fmDayEndFind 按部位找行。
func fmDayEndFind(t *testing.T, rows []service.PerCutGrossMargin, cut model.CutType) *service.PerCutGrossMargin {
	t.Helper()
	for i := range rows {
		if rows[i].CutType == cut {
			return &rows[i]
		}
	}
	t.Fatalf("部位 %s 不在 per_cut", cut)
	return nil
}

// fmRefTotals 造参考口径输入:买 100kg 花 1400 元(均价 14),期末台面剩 20kg。
func fmRefTotals() service.ReferenceDayTotals {
	return service.ReferenceDayTotalsForTest("100", "1400", "20", true)
}

// 有完整日终盘点 ⇒ 日终剩余(kg/金额)与售罄率都有值,且售罄率 ≤ 1。
func TestDayEndRemainAndSellThrough(t *testing.T) {
	rows, total := service.RollUpPerCutCutsForTest([]service.CutDailyCost{
		fmDayEndRow(model.CutLegRear, 80, 1600, 1200, 20, "actual"),
	}, nil, fmRefTotals())

	row := fmDayEndFind(t, rows, model.CutLegRear)
	if row.DayEndRemainKg == nil {
		t.Fatal("有完整盘点 ⇒ 日终剩余必须有值")
	}
	if !row.DayEndRemainKg.Equal(fmDayDec(20)) {
		t.Fatalf("日终剩余 kg = %s,期望 20", row.DayEndRemainKg.String())
	}
	// ⚠️ 金额 = 剩余 kg × **采购均价**(1400 ÷ 100 = 14)⇒ 20 × 14 = 280
	if row.DayEndRemainYuan == nil {
		t.Fatal("有采购均价 ⇒ 日终剩余金额必须有值")
	}
	if !row.DayEndRemainYuan.Equal(fmDayDec(280)) {
		t.Fatalf("日终剩余金额 = %s,期望 280(采购均价 14)", row.DayEndRemainYuan.String())
	}
	// 售罄率 = 80 ÷ (80 + 20) = 0.8
	if row.SellThroughRate == nil {
		t.Fatal("有盘点 ⇒ 售罄率必须有值")
	}
	if got := *row.SellThroughRate; got < 0.7999 || got > 0.8001 {
		t.Fatalf("售罄率 = %v,期望 0.8", got)
	}
	// 全店合计
	if total.DayEndRemainKg == nil || !total.DayEndRemainKg.Equal(fmDayDec(20)) {
		t.Fatalf("全店日终剩余 kg = %v,期望 20", total.DayEndRemainKg)
	}
	if total.DayEndRemainYuan == nil || !total.DayEndRemainYuan.Equal(fmDayDec(280)) {
		t.Fatalf("全店日终剩余金额 = %v,期望 280", total.DayEndRemainYuan)
	}
	if total.SellThroughRate == nil || *total.SellThroughRate > 1 {
		t.Fatalf("全店售罄率 = %v,必须落在 [0,1]", total.SellThroughRate)
	}
}

// 没有完整日终盘点 ⇒ 日终剩余与售罄率都给不出(前端「—」),**不是 0、不是 100%**。
func TestDayEndNoStocktakeGivesNoSellThrough(t *testing.T) {
	rows, total := service.RollUpPerCutCutsForTest([]service.CutDailyCost{
		fmDayEndRow(model.CutLegRear, 80, 1600, 1200, 0, "estimated"),
	}, nil, service.ReferenceDayTotalsForTest("100", "1400", "0", false))

	row := fmDayEndFind(t, rows, model.CutLegRear)
	if row.DayEndRemainKg != nil {
		t.Fatalf("没有盘点 ⇒ 日终剩余必须是 nil,实际 %s", row.DayEndRemainKg.String())
	}
	if row.DayEndRemainYuan != nil {
		t.Fatalf("没有盘点 ⇒ 日终剩余金额必须是 nil")
	}
	if row.SellThroughRate != nil {
		t.Fatalf("没有盘点 ⇒ 售罄率必须是 nil(不能回落成 100%%),实际 %v", *row.SellThroughRate)
	}
	if total.SellThroughRate != nil {
		t.Fatalf("没有盘点 ⇒ 全店售罄率必须是 nil")
	}
	// ⚠️ 全店日终剩余也必须是 nil（**不是 0**）
	//   ⇒ 0 会被门店误读成"柜台上一点货都没有"。
	if total.DayEndRemainKg != nil {
		t.Fatalf("没有盘点 ⇒ 全店日终剩余必须是 nil,实际 %s", total.DayEndRemainKg.String())
	}
	if total.DayEndRemainYuan != nil {
		t.Fatalf("没有盘点 ⇒ 全店日终剩余金额必须是 nil")
	}
}

// 全店参考毛利 = Σ 部位参考毛利(B 口径);总采/采购均价照样给出来(门店要能自己验算)。
//
// ⚠️ 这条行**没有建议成本单价** ⇒ 成本单价落**全店采购均价兜底**(14 元/kg)
// ⇒ 成本 = 14 × 80 = 1120 ⇒ 毛利 = 1600 − 1120 = 480 ⇒ 毛利率 30%。
//
// ⚠️ 判别力:把兜底价换成"零售均价÷(1+m)倒推"(旧口径)⇒ 单价变成 15.3846 ⇒ 断言变红。
func TestReferenceMarginRate(t *testing.T) {
	_, total := service.RollUpPerCutCutsForTest([]service.CutDailyCost{
		fmDayEndRow(model.CutLegRear, 80, 1600, 1200, 20, "actual"),
	}, nil, fmRefTotals())

	if total.ReferenceMarginRate == nil {
		t.Fatal("有进货 ⇒ 参考毛利率必须有值")
	}
	gotRate, _ := total.ReferenceMarginRate.Float64()
	if gotRate < 0.2999 || gotRate > 0.3001 {
		t.Fatalf("参考毛利率 = %v,期望 0.30", gotRate)
	}
	if total.ReferenceGrossMarginYuan == nil ||
		!total.ReferenceGrossMarginYuan.Equal(fmDayDec(480)) {
		t.Fatalf("全店参考毛利 = %v,期望 480", total.ReferenceGrossMarginYuan)
	}
	if total.ReferenceGrossMarginPct == nil ||
		*total.ReferenceGrossMarginPct < 29.99 || *total.ReferenceGrossMarginPct > 30.01 {
		t.Fatalf("全店参考毛利率 = %v,期望 30", total.ReferenceGrossMarginPct)
	}
	// 门店要能自己验算 ⇒ 输入必须都给出来。
	if !total.PurchasedKg.Equal(fmDayDec(100)) || !total.PurchasedYuan.Equal(fmDayDec(1400)) {
		t.Fatalf("总采必须给出来: %v kg / %v 元", total.PurchasedKg, total.PurchasedYuan)
	}
	if !total.PurchaseAvgYuan.Equal(fmDayDec(14)) {
		t.Fatalf("采购均价 = %v,期望 14", total.PurchaseAvgYuan)
	}
	if !total.ReferenceRemainKg.Equal(fmDayDec(20)) {
		t.Fatalf("期末剩余 kg = %v,期望 20", total.ReferenceRemainKg)
	}
	if !total.ReferenceRemainYuan.Equal(fmDayDec(280)) {
		t.Fatalf("期末剩余成本 = %v,期望 280", total.ReferenceRemainYuan)
	}
}

// 部位成本单价 = **建议成本单价**;参考毛利 = 营收 − 单价×销量。
//
// ⚠️ 判别力:把单价换回"零售均价÷(1+m)倒推"(旧口径)⇒ 单价变 15.3846 ⇒ 断言变红。
func TestReferenceCutUnitPriceAndMargin(t *testing.T) {
	rows, _ := service.RollUpPerCutCutsForTest([]service.CutDailyCost{
		fmDayEndRowWithSuggested(model.CutLegRear, 80, 1600, 1200, 20, "actual", "12"),
	}, nil, fmRefTotals())

	row := fmDayEndFind(t, rows, model.CutLegRear)
	if row.ReferenceCostUnitPriceYuan == nil {
		t.Fatal("有建议成本单价 ⇒ 部位成本单价必须有值")
	}
	want := fmDayDec(12)
	if !row.ReferenceCostUnitPriceYuan.Equal(want) {
		t.Fatalf("部位成本单价 = %s,期望 %s", row.ReferenceCostUnitPriceYuan.String(), want.String())
	}
	if row.ReferenceCostPriceSource != "baseline" {
		t.Fatalf("成本单价来源 = %q,期望 baseline", row.ReferenceCostPriceSource)
	}
	if row.ReferenceGrossMarginYuan == nil {
		t.Fatal("有成本单价 ⇒ 参考毛利必须有值")
	}
	// 参考毛利 = 1600 − 12 × 80 = 640
	wantGm := fmDayDec(1600).Sub(fmDayDec(12).Mul(fmDayDec(80)))
	if !row.ReferenceGrossMarginYuan.Equal(wantGm) {
		t.Fatalf("部位参考毛利 = %s,期望 %s", row.ReferenceGrossMarginYuan.String(), wantGm.String())
	}
}

// **header = 各部位参考毛利之和**(B 口径:头与部位同源 ⇒ 门店能自己核)。
//
// ⚠️ 判别力:把 header 改回旧口径的「营收 − 采购 + 期末剩余成本」⇒ 本用例必红。
func TestReferenceHeaderEqualsSumOfParts(t *testing.T) {
	rows, total := service.RollUpPerCutCutsForTest([]service.CutDailyCost{
		fmDayEndRowWithSuggested(model.CutLegRear, 80, 1600, 1200, 0, "actual", "12"),
		fmDayEndRowWithSuggested(model.CutBelly, 20, 400, 300, 0, "actual", "10"),
	}, nil, fmRefTotals())

	var sum decimal.Decimal
	for _, r := range rows {
		if r.ReferenceGrossMarginYuan != nil {
			sum = sum.Add(*r.ReferenceGrossMarginYuan)
		}
	}
	if total.ReferenceGrossMarginYuan == nil {
		t.Fatal("有进货 ⇒ 全店参考毛利必须有值")
	}
	if !sum.Equal(*total.ReferenceGrossMarginYuan) {
		t.Fatalf("★ header 必须等于部位之和(同源):部位 %s / 头 %s",
			sum.String(), total.ReferenceGrossMarginYuan.String())
	}
	// 后腿:1600 − 12×80 = 640;五花:400 − 10×20 = 200 ⇒ 合计 840
	if !total.ReferenceGrossMarginYuan.Equal(fmDayDec(840)) {
		t.Fatalf("全店参考毛利 = %s,期望 840", total.ReferenceGrossMarginYuan.String())
	}
}

// **没有日终盘点 ⇒ 参考毛利照样有值**(owner 2026-11-01 换口径的原因)。
//
// ⚠️ 旧口径在这里给的是 nil(整条链算不出来)⇒ 门店看到的是一片「—」。
//
// ⚠️ 判别力:把成本单价换回"依赖 m 的倒推"⇒ 没有 m ⇒ 参考毛利变 nil ⇒ 本用例必红。
func TestReferenceMarginWithoutStocktakeStillHasValue(t *testing.T) {
	rows, total := service.RollUpPerCutCutsForTest([]service.CutDailyCost{
		fmDayEndRowWithSuggested(model.CutLegRear, 80, 1600, 1200, 0, "estimated", "12"),
	}, nil, service.ReferenceDayTotalsForTest("100", "1400", "0", false))

	row := fmDayEndFind(t, rows, model.CutLegRear)
	if row.ReferenceGrossMarginYuan == nil {
		t.Fatal("★ 没有盘点 ⇒ 参考毛利也必须有值(B 口径不需要日终实盘)")
	}
	if row.ReferenceCostUnitPriceYuan == nil {
		t.Fatal("★ 没有盘点 ⇒ 部位成本单价也必须有值")
	}
	if total.ReferenceGrossMarginYuan == nil {
		t.Fatalf("★ 没有盘点 ⇒ 全店参考毛利必须有值,实际 nil")
	}
	if !total.ReferenceGrossMarginYuan.Equal(fmDayDec(640)) {
		t.Fatalf("全店参考毛利 = %s,期望 640", total.ReferenceGrossMarginYuan.String())
	}
	// ⚠️ 但**售罄率**仍然需要日终实盘(分母是期末实盘)⇒ 仍是 nil。
	if total.SellThroughRate != nil {
		t.Fatalf("没有盘点 ⇒ 售罄率仍应是 nil,实际 %v", *total.SellThroughRate)
	}
}

// 参考成本单价必须**优先用建议成本单价**,而不是全店采购均价兜底。
//
// ⚠️ 这是本次口径切换的**核心断言**:owner 的原话是
//
//	「销售重量×建议成本单价不就有了销售成本」
//
// ⇒ 单价取错(拿采购均价顶替)⇒ 毛利会系统性偏 ⇒ 门店看到的是另一个数。
//
// ⚠️ 判别力:把优先级反了(先兜底后建议)⇒ 单价变 14 ⇒ 毛利 640 → 480 ⇒ 断言变红。
func TestReferenceMarginPrefersSuggestedPriceOverPurchaseAvg(t *testing.T) {
	rows, _ := service.RollUpPerCutCutsForTest([]service.CutDailyCost{
		fmDayEndRowWithSuggested(model.CutLegRear, 80, 1600, 1200, 20, "actual", "12"),
	}, nil, fmRefTotals())

	row := fmDayEndFind(t, rows, model.CutLegRear)
	if row.ReferenceCostUnitPriceYuan == nil ||
		!row.ReferenceCostUnitPriceYuan.Equal(fmDayDec(12)) {
		t.Fatalf("★ 成本单价必须用建议成本单价 12,实际 %v", row.ReferenceCostUnitPriceYuan)
	}
	if row.ReferenceCostPriceSource != "baseline" {
		t.Fatalf("★ 来源必须是 baseline(不是 purchase_avg),实际 %q", row.ReferenceCostPriceSource)
	}
	// 采购均价是 14 ⇒ 若错用它,毛利会是 480 而不是 640。
	if !row.ReferenceGrossMarginYuan.Equal(fmDayDec(640)) {
		t.Fatalf("★ 参考毛利 = %s,期望 640(用建议成本单价 12)", row.ReferenceGrossMarginYuan.String())
	}
}

// 当天**没有进货** ⇒ 参考毛利**给不出**(nil),**绝不回落成 100%**。
//
// # 为什么这条必须锁(2026-11-01)
//
//	没有进货 ⇒ ① 建议成本单价取不到(它要靠当天的猪做进价缩放)
//	② 兜底的全店采购均价也没有(分母是进货重量)⇒ **任何部位都取不到价**
//	⇒ 参考毛利算不出来 ⇒ 只能显示「—」。
//
//	⚠️ 更不能回落成"营收 − 0" = 100% 毛利 ⇒ 门店会以为赚疯了。
//	⇒ 而那种日子通常只是**还没录猪** ⇒ 看板本来就有「今天还没录猪」提示。
//
// ⚠️ 判别力:把"没有价 ⇒ 不给数"改成"给 0 成本"⇒ 本用例必红(总额会变成 1600 = 100%)。
func TestReferenceMarginNeedsPurchase(t *testing.T) {
	rows, total := service.RollUpPerCutCutsForTest([]service.CutDailyCost{
		fmDayEndRow(model.CutLegRear, 80, 1600, 1200, 20, "actual"),
	}, nil, service.ReferenceDayTotalsForTest("0", "0", "20", true))

	row := fmDayEndFind(t, rows, model.CutLegRear)
	if row.ReferenceGrossMarginYuan != nil {
		t.Fatalf("★ 当天没进货 ⇒ 部位参考毛利必须是 nil(不是 0!),实际 %s",
			row.ReferenceGrossMarginYuan.String())
	}
	if total.ReferenceGrossMarginYuan != nil {
		t.Fatalf("★ 当天没进货 ⇒ 全店参考毛利必须是 nil(不是 100%%!),实际 %s",
			total.ReferenceGrossMarginYuan.String())
	}
	if total.ReferenceMarginRate != nil {
		t.Fatalf("当天没进货 ⇒ 参考毛利率必须是 nil(不是 100%%)")
	}
}

// 出成率 = 真实切分产出 ÷ 当日进货毛重;**> 100% ⇒ 有问题,要去查**。
//
// ⚠️ 生产实测(店 00 2026-10-30):106.4% ⇒ 搬台没登记 14.655kg。
func TestYieldRateOver100MeansProblem(t *testing.T) {
	// 正常:台面剩 20 + 卖 80 = 产出 100 = 进货 100 ⇒ 100%
	_, total := service.RollUpPerCutCutsForTest([]service.CutDailyCost{
		fmDayEndRow(model.CutLegRear, 80, 1600, 1200, 20, "actual"),
	}, nil, fmRefTotals())
	if total.YieldRate == nil {
		t.Fatal("有盘点 + 有进货 ⇒ 出成率必须有值")
	}
	if got := *total.YieldRate; got < 0.9999 || got > 1.0001 {
		t.Fatalf("出成率 = %v,期望 1.0", got)
	}

	// 异常:产出 120 > 进货 100 ⇒ 120% ⇒ 门店要去查(搬台没登记 / 盘点漏部位)
	_, total2 := service.RollUpPerCutCutsForTest([]service.CutDailyCost{
		fmDayEndRow(model.CutLegRear, 80, 1600, 1200, 40, "actual"),
	}, nil, fmRefTotals())
	if total2.YieldRate == nil {
		t.Fatal("出成率必须有值")
	}
	if got := *total2.YieldRate; got < 1.1999 || got > 1.2001 {
		t.Fatalf("出成率 = %v,期望 1.2(>100%% ⇒ 要去查)", got)
	}
}

// 没进猪 ⇒ 出成率给不出(分母为 0),**不是 0%**。
func TestYieldRateNeedsPurchase(t *testing.T) {
	_, total := service.RollUpPerCutCutsForTest([]service.CutDailyCost{
		fmDayEndRow(model.CutLegRear, 80, 1600, 1200, 20, "actual"),
	}, nil, service.ReferenceDayTotalsForTest("0", "0", "20", true))
	if total.YieldRate != nil {
		t.Fatalf("当天没进猪 ⇒ 出成率必须是 nil(不是 0%%),实际 %v", *total.YieldRate)
	}
}
