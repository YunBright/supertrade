package service_test

// 回归锁:**参考口径**(2026-11-01 owner 终稿)。
//
// # 口径(owner 亲定)
//
//	m       = (营收 − 当日采购 + 期末猪肉台剩余×采购均价) / 营收
//	成本单价_c = 零售均价_c ÷ (1 + m)
//	参考毛利_c = 营收_c − 成本单价_c×销量_c − 搭赠成本_c − 报损成本_c
//
// # 三条不能违反的性质(每条都有判别力用例)
//
//	① **header ≠ 各部位参考毛利之和**(owner:「header不需要与部位同口径」)
//	   ⇒ 用例:TestReferenceHeaderNotEqualSumOfParts
//	② **没有完整日终盘点 ⇒ 参考毛利给不出(nil)**,绝不回落成 0
//	   ⇒ 用例:TestReferenceMarginNeedsStocktake
//	③ **出成率 > 100% ⇒ 有问题**(owner:「它大于100%,就知道有问题,就需要查」)
//	   ⇒ 用例:TestYieldRateOver100MeansProblem
//
// # 为什么日终剩余的金额用**采购均价**
//
//	旧口径「零售均价 ÷ (1 + 毛利率)」里含 m ⇒ 拿它估值期末库存 ⇒ **成环**。
//	采购均价不含 m ⇒ 天然无环 ⇒ 才能当 m 的输入。

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

// 参考毛利率 m = (营收 − 采购 + 期末剩余×采购均价) / 营收。
//
// ⚠️ 判别力:把「+期末剩余成本」这一项去掉 ⇒ m 会从 30% 变成 12.5% ⇒ 断言变红。
func TestReferenceMarginRate(t *testing.T) {
	_, total := service.RollUpPerCutCutsForTest([]service.CutDailyCost{
		fmDayEndRow(model.CutLegRear, 80, 1600, 1200, 20, "actual"),
	}, nil, fmRefTotals())

	if total.ReferenceMarginRate == nil {
		t.Fatal("有完整盘点 ⇒ 参考毛利率必须有值")
	}
	// m = (1600 − 1400 + 20×14) / 1600 = 480 / 1600 = 0.30
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

// 部位成本单价 = 零售均价 ÷ (1 + m);参考毛利 = 营收 − 单价×销量。
func TestReferenceCutUnitPriceAndMargin(t *testing.T) {
	rows, _ := service.RollUpPerCutCutsForTest([]service.CutDailyCost{
		fmDayEndRow(model.CutLegRear, 80, 1600, 1200, 20, "actual"),
	}, nil, fmRefTotals())

	row := fmDayEndFind(t, rows, model.CutLegRear)
	if row.ReferenceCostUnitPriceYuan == nil {
		t.Fatal("有 m ⇒ 部位成本单价必须有值")
	}
	// 单价 = (1600 ÷ 80) ÷ (1 + 0.30) = 20 ÷ 1.3 = 15.3846…
	want := fmDayDec(20).Div(fmDayDec(1.3))
	if !row.ReferenceCostUnitPriceYuan.Equal(want) {
		t.Fatalf("部位成本单价 = %s,期望 %s", row.ReferenceCostUnitPriceYuan.String(), want.String())
	}
	if row.ReferenceCostPriceSource != "retail_backout" {
		t.Fatalf("成本单价来源 = %q,期望 retail_backout", row.ReferenceCostPriceSource)
	}
	if row.ReferenceGrossMarginYuan == nil {
		t.Fatal("有成本单价 ⇒ 参考毛利必须有值")
	}
	// 参考毛利 = 1600 − (20/1.3) × 80 = 1600 − 1230.769… = 369.2307…
	wantGm := fmDayDec(1600).Sub(fmDayDec(20).Div(fmDayDec(1.3)).Mul(fmDayDec(80)))
	if !row.ReferenceGrossMarginYuan.Equal(wantGm) {
		t.Fatalf("部位参考毛利 = %s,期望 %s", row.ReferenceGrossMarginYuan.String(), wantGm.String())
	}
}

// **header ≠ 各部位参考毛利之和**(owner:「header不需要与部位同口径」)。
//
// ⚠️ 判别力:把 header 改成"部位加总"⇒ 本用例必红(611.11 ≠ 880)。
func TestReferenceHeaderNotEqualSumOfParts(t *testing.T) {
	rows, total := service.RollUpPerCutCutsForTest([]service.CutDailyCost{
		fmDayEndRow(model.CutLegRear, 80, 1600, 1200, 0, "actual"),
		fmDayEndRow(model.CutBelly, 20, 400, 300, 0, "actual"),
	}, nil, fmRefTotals())

	var sum decimal.Decimal
	for _, r := range rows {
		if r.ReferenceGrossMarginYuan != nil {
			sum = sum.Add(*r.ReferenceGrossMarginYuan)
		}
	}
	if total.ReferenceGrossMarginYuan == nil {
		t.Fatal("有完整盘点 ⇒ 全店参考毛利必须有值")
	}
	if sum.Equal(*total.ReferenceGrossMarginYuan) {
		t.Fatalf("★ header 必须**不等于**部位之和(两者口径不同)—— 两者都是 %s,"+
			"说明有人把 header 改成了部位加总", sum.String())
	}
	// 全店口径:营收 2000 − 采购 1400 + 剩余 280 = 880
	if !total.ReferenceGrossMarginYuan.Equal(fmDayDec(880)) {
		t.Fatalf("全店参考毛利 = %s,期望 880", total.ReferenceGrossMarginYuan.String())
	}
}

// 没有完整日终盘点 ⇒ 参考毛利**给不出**(nil),**绝不回落成 0**。
func TestReferenceMarginNeedsStocktake(t *testing.T) {
	rows, total := service.RollUpPerCutCutsForTest([]service.CutDailyCost{
		fmDayEndRow(model.CutLegRear, 80, 1600, 1200, 0, "estimated"),
	}, nil, service.ReferenceDayTotalsForTest("100", "1400", "0", false))

	row := fmDayEndFind(t, rows, model.CutLegRear)
	if row.ReferenceGrossMarginYuan != nil {
		t.Fatalf("没有盘点 ⇒ 部位参考毛利必须是 nil,实际 %s", row.ReferenceGrossMarginYuan.String())
	}
	if row.ReferenceCostUnitPriceYuan != nil {
		t.Fatalf("没有盘点 ⇒ 部位成本单价必须是 nil")
	}
	if total.ReferenceGrossMarginYuan != nil {
		t.Fatalf("没有盘点 ⇒ 全店参考毛利必须是 nil(不是 0!),实际 %s",
			total.ReferenceGrossMarginYuan.String())
	}
	if total.ReferenceMarginRate != nil {
		t.Fatalf("没有盘点 ⇒ 参考毛利率必须是 nil")
	}
}

// 当天**没有进货** ⇒ 参考毛利**给不出**(nil),**绝不回落成 100%**。
//
// # 为什么这条必须锁(2026-11-01)
//
//	没有进货 ⇒ 没有采购均价 ⇒ 期末剩余的成本估不出来;
//	而公式会退化成 m = (营收 − 0 + 剩余×0) / 营收 = **100%**
//	⇒ 门店看到「参考毛利 = 营收、毛利率 100%」会以为赚疯了。
//	⇒ 而那种日子通常只是**还没录猪** ⇒ 正确表现是「—」(算不出来)。
//
// ⚠️ 判别力:去掉 referenceMarginRate 里的 PurchasedKg 判据
//   ⇒ 本用例必红(总额会变成 100%)。
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
