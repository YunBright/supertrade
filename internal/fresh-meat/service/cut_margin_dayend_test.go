package service_test

// 回归锁:**售罄率 / 日终剩余 / 建议毛利**三项新口径(2026-10-31 owner)。
//
// # ① 售罄率为什么必须改
//
// 旧口径 = sold / purchased ⇒ 生产实测(店 00 2026-10-30)= **100.75%**。
// 根因:**销售**里含冷库卖出去的货,而**当天进货**里没有那部分
// ⇒ 分子含了分母之外的货 ⇒ 比率可以 >1。
// ⇒ 新口径 = 销售 ÷ (销售 + **猪肉台日终剩余**)⇒ 恒 ≤ 100%。
//
// ⚠️ 判别力:本文件里「售罄率 ≤ 1」这条断言,
// 在把分母换回 purchased 时**必然变红**(100.75% 的形状)。
//
// # ② 日终剩余的折算口径
//
// owner 亲定:**零售均价 ÷ (1 + 今日毛利率)** ⇒ 倒推进货单价 × 剩余 kg。
// ⚠️ 与单价日样本的倒推**同一条公式** ⇒ 两边不会各算各的。
//
// # ③ 建议毛利
//
// 与实际毛利**逐项平行**,只差「零售货的成本口径」:
//	建议毛利 = 营收 − 建议成本单价×零售销量 − 统收成本 − 报损成本 − 搭赠成本 + 预期毛利
//
// ⚠️ 取不到建议成本单价 ⇒ **算不出来**(nil)⇒ 不用实际成本冒充。

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

// fmDayEndRow 造一条部位行(默认：有完整盘点、有建议成本单价)。
func fmDayEndRow(cut model.CutType, soldKg, revenue, salesCost, remainKg float64,
	suggestedPrice *float64, dataSource string) service.CutDailyCost {
	row := service.CutDailyCost{
		BranchID:        "00",
		CutType:         cut,
		DataSource:      dataSource,
		SoldKg:          fmDayDec(soldKg),
		RevenueYuan:     fmDayDec(revenue),
		SalesCostYuan:   fmDayDec(salesCost),
		CounterActualKg: fmDayDec(remainKg),
	}
	if suggestedPrice != nil {
		v := fmDayDec(*suggestedPrice)
		row.SuggestedCostUnitPriceYuan = &v
	}
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

// 有完整日终盘点 ⇒ 日终剩余(kg/金额)与售罄率都有值,且售罄率 ≤ 1。
func TestDayEndRemainAndSellThrough(t *testing.T) {
	price := 14.0
	rows, total := service.RollUpPerCutCutsForTest([]service.CutDailyCost{
		fmDayEndRow(model.CutLegRear, 80, 1600, 1200, 20, &price, "actual"),
	}, nil)

	row := fmDayEndFind(t, rows, model.CutLegRear)
	if row.DayEndRemainKg == nil {
		t.Fatal("有完整盘点 ⇒ 日终剩余必须有值")
	}
	if !row.DayEndRemainKg.Equal(fmDayDec(20)) {
		t.Fatalf("日终剩余 kg = %s,期望 20", row.DayEndRemainKg.String())
	}
	// 折算进货单价 = 零售均价 ÷ (1 + 毛利率) = (1600/80) ÷ (1 + 400/1600) = 20 ÷ 1.25 = 16
	// ⇒ 日终剩余金额 = 20 × 16 = 320
	if row.DayEndRemainYuan == nil {
		t.Fatal("有盘点且能折算价 ⇒ 日终剩余金额必须有值")
	}
	if !row.DayEndRemainYuan.Equal(fmDayDec(320)) {
		t.Fatalf("日终剩余金额 = %s,期望 320", row.DayEndRemainYuan.String())
	}
	// 售罄率 = 80 ÷ (80 + 20) = 0.8
	if row.SellThroughRate == nil {
		t.Fatal("有盘点 ⇒ 售罄率必须有值")
	}
	if got := *row.SellThroughRate; got < 0 || got > 1 {
		t.Fatalf("售罄率 = %v,必须落在 [0,1](旧口径会算出 100.75%%)", got)
	}
	if got := *row.SellThroughRate; got < 0.7999 || got > 0.8001 {
		t.Fatalf("售罄率 = %v,期望 0.8", got)
	}
	// 全店合计
	if total.DayEndRemainKg == nil || !total.DayEndRemainKg.Equal(fmDayDec(20)) {
		t.Fatalf("全店日终剩余 kg = %v,期望 20", total.DayEndRemainKg)
	}
	if total.DayEndRemainYuan == nil || !total.DayEndRemainYuan.Equal(fmDayDec(320)) {
		t.Fatalf("全店日终剩余金额 = %v,期望 320", total.DayEndRemainYuan)
	}
	if total.SellThroughRate == nil || *total.SellThroughRate > 1 {
		t.Fatalf("全店售罄率 = %v,必须落在 [0,1]", total.SellThroughRate)
	}
}

// 没有完整日终盘点 ⇒ 日终剩余与售罄率都给不出(前端「—」),**不是 0、不是 100%**。
func TestDayEndNoStocktakeGivesNoSellThrough(t *testing.T) {
	price := 14.0
	rows, total := service.RollUpPerCutCutsForTest([]service.CutDailyCost{
		fmDayEndRow(model.CutLegRear, 80, 1600, 1200, 0, &price, "estimated"),
	}, nil)

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

// 建议毛利:与实际毛利逐项平行,只差「零售货的成本口径」。
func TestSuggestedGrossMarginParallelToActual(t *testing.T) {
	price := 14.0
	rows, total := service.RollUpPerCutCutsForTest([]service.CutDailyCost{
		fmDayEndRow(model.CutLegRear, 80, 1600, 1200, 20, &price, "actual"),
	}, nil)

	row := fmDayEndFind(t, rows, model.CutLegRear)
	if row.SuggestedGrossMarginYuan == nil {
		t.Fatal("有建议成本单价 ⇒ 建议毛利必须有值")
	}
	// 建议毛利 = 1600 − (14 × 80) = 480(实际毛利 = 1600 − 1200 = 400)
	if !row.SuggestedGrossMarginYuan.Equal(fmDayDec(480)) {
		t.Fatalf("建议毛利 = %s,期望 480", row.SuggestedGrossMarginYuan.String())
	}
	if row.SuggestedGrossMarginPct == nil {
		t.Fatal("有建议毛利 ⇒ 建议毛利率必须有值")
	}
	// 480 ÷ 1600 = 30%
	if got := *row.SuggestedGrossMarginPct; got < 29.99 || got > 30.01 {
		t.Fatalf("建议毛利率 = %v,期望 30", got)
	}
	if total.SuggestedGrossMarginYuan == nil ||
		!total.SuggestedGrossMarginYuan.Equal(fmDayDec(480)) {
		t.Fatalf("全店建议毛利 = %v,期望 480", total.SuggestedGrossMarginYuan)
	}
	if total.SuggestedCostPartial {
		t.Fatal("全部部位都有建议成本单价 ⇒ 不该标 partial")
	}
}

// 卖过但取不到建议成本单价 ⇒ 建议毛利**算不出来**(nil),并标 partial。
//
// ⚠️ 不能用实际销售成本冒充建议成本 ⇒ 否则「建议」和「实际」长得一样 ⇒ 这一列白做。
func TestSuggestedGrossMarginMissingPrice(t *testing.T) {
	rows, total := service.RollUpPerCutCutsForTest([]service.CutDailyCost{
		fmDayEndRow(model.CutLegRear, 80, 1600, 1200, 20, nil, "actual"),
	}, nil)

	row := fmDayEndFind(t, rows, model.CutLegRear)
	if row.SuggestedGrossMarginYuan != nil {
		t.Fatalf("取不到建议成本单价 ⇒ 建议毛利必须是 nil,实际 %s", row.SuggestedGrossMarginYuan.String())
	}
	if !total.SuggestedCostPartial {
		t.Fatal("有部位取不到价 ⇒ 必须标 partial(前端要提示「部分部位未计入」)")
	}
}

// 没卖过且取不到建议成本单价 ⇒ 建议毛利**照样算得出来**(零售成本恒 0)。
func TestSuggestedGrossMarginNoSalesStillComputable(t *testing.T) {
	rows, _ := service.RollUpPerCutCutsForTest([]service.CutDailyCost{
		fmDayEndRow(model.CutLiver, 0, 0, 0, 5, nil, "actual"),
	}, nil)

	row := fmDayEndFind(t, rows, model.CutLiver)
	if row.SuggestedGrossMarginYuan == nil {
		t.Fatal("没卖过 ⇒ 零售成本恒 0 ⇒ 建议毛利算得出来")
	}
	if !row.SuggestedGrossMarginYuan.IsZero() {
		t.Fatalf("没卖过 ⇒ 建议毛利应为 0,实际 %s", row.SuggestedGrossMarginYuan.String())
	}
}
