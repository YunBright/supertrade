package service

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

// TestBuildPurchaseBasis_MultiDayDailyAverage 是 2026-11-01 用户口径的回归锁：
//
//	10 天合并付款（香蕉-00518 累计 220、合理备货量 49.304）不能拿 220 去比，
//	必须拿 220/10=22 的日均去比 —— 否则备货倍数虚高、误拒单。
func TestBuildPurchaseBasis_MultiDayDailyAverage(t *testing.T) {
	received := map[string]decimal.Decimal{"P1": dec("220")}
	purchase := buildPurchaseBasis(received, nil,
		map[string][]string{"P1": {"PI-1", "PI-2", "PI-3"}},
		map[string]string{
			"PI-1": "2026-09-18",
			"PI-2": "2026-09-22",
			"PI-3": "2026-09-27",
		})
	b := purchase["P1"]
	if b.Days != 10 {
		t.Fatalf("Days = %d, want 10(2026-09-18~2026-09-27 含首尾)", b.Days)
	}
	if !b.NetQty.Equal(dec("220")) {
		t.Errorf("NetQty = %s, want 220", b.NetQty)
	}
	if !b.CompareQty().Equal(dec("22")) {
		t.Errorf("日均采购量 = %s, want 22", b.CompareQty())
	}
	if b.FirstDate != "2026-09-18" || b.LastDate != "2026-09-27" {
		t.Errorf("首尾收货日 = %s/%s, want 2026-09-18/2026-09-27", b.FirstDate, b.LastDate)
	}
}

// TestBuildPurchaseBasis_OneBuyOneReturnIsSingleDay 钉住「一日进货、一日退货」不是多日。
//
// 那一天进货量本身就是「一次备货量」，一刀切除以 2 会把真实过量藏起来。
func TestBuildPurchaseBasis_OneBuyOneReturnIsSingleDay(t *testing.T) {
	received := map[string]decimal.Decimal{"P1": dec("60")}
	returned := map[string]decimal.Decimal{"P1": dec("10")}
	purchase := buildPurchaseBasis(received, returned,
		// 退货单不在 productSheets 里(sheetDates 的键是该商品的 PI 单)——
		// 即便调用方把退货日也塞进来，退货日也不该参与天数。
		map[string][]string{"P1": {"PI-1"}},
		map[string]string{"PI-1": "2026-10-01"})
	b := purchase["P1"]
	if b.Days != 1 {
		t.Fatalf("Days = %d, want 1(单日进货 + 单日退货不算多日)", b.Days)
	}
	if !b.NetQty.Equal(dec("50")) {
		t.Errorf("净进货量 = %s, want 60-10=50", b.NetQty)
	}
	if !b.CompareQty().Equal(dec("50")) {
		t.Errorf("比较量 = %s, want 50(单日不除天数)", b.CompareQty())
	}
	if !b.ReturnedQty.Equal(dec("10")) {
		t.Errorf("退货量 = %s, want 10(回显用)", b.ReturnedQty)
	}
}

// TestBuildPurchaseBasis_ReturnDeducted 对应图上第二单：水果木瓜有采购退货单，
// 实际采购量必须减去退货数量。
func TestBuildPurchaseBasis_ReturnDeducted(t *testing.T) {
	received := map[string]decimal.Decimal{"P1": dec("48.75")}
	returned := map[string]decimal.Decimal{"P1": dec("30")}
	purchase := buildPurchaseBasis(received, returned,
		map[string][]string{"P1": {"PI-1", "PI-2"}},
		map[string]string{"PI-1": "2026-09-25", "PI-2": "2026-10-01"}) // 7 天
	b := purchase["P1"]
	if !b.NetQty.Equal(dec("18.75")) {
		t.Fatalf("净进货量 = %s, want 48.75-30=18.75", b.NetQty)
	}
	if b.Days != 7 {
		t.Fatalf("Days = %d, want 7", b.Days)
	}
	want := dec("18.75").Div(decimal.NewFromInt(7)).Round(3)
	if !b.CompareQty().Equal(want) {
		t.Errorf("日均采购量 = %s, want %s", b.CompareQty(), want)
	}
}

// TestBuildPurchaseBasis_ReturnExceedsReceived 退货大于进货 ⇒ 净量归零，不比负数。
func TestBuildPurchaseBasis_ReturnExceedsReceived(t *testing.T) {
	received := map[string]decimal.Decimal{"P1": dec("10")}
	returned := map[string]decimal.Decimal{"P1": dec("25")}
	purchase := buildPurchaseBasis(received, returned,
		map[string][]string{"P1": {"PI-1"}}, map[string]string{"PI-1": "2026-09-18"})
	b := purchase["P1"]
	if b.NetQty.IsNegative() || !b.NetQty.IsZero() {
		t.Errorf("NetQty = %s, want 0(负数按 0)", b.NetQty)
	}
	if !b.CompareQty().IsZero() {
		t.Errorf("比较量 = %s, want 0", b.CompareQty())
	}
}

// TestBuildPurchaseBasis_SameDayMultipleSheets 同日多张单 ⇒ 单日口径。
func TestBuildPurchaseBasis_SameDayMultipleSheets(t *testing.T) {
	received := map[string]decimal.Decimal{"P1": dec("40")}
	purchase := buildPurchaseBasis(received, nil,
		map[string][]string{"P1": {"PI-1", "PI-2"}},
		map[string]string{"PI-1": "2026-09-18", "PI-2": "2026-09-18"})
	b := purchase["P1"]
	if b.Days != 1 {
		t.Fatalf("Days = %d, want 1(同一天两张单仍是单次收货)", b.Days)
	}
	if !b.CompareQty().Equal(dec("40")) {
		t.Errorf("比较量 = %s, want 40", b.CompareQty())
	}
}

// TestBuildPurchaseBasis_MissingSheetDate 收货日期解析不出来 ⇒ 按单日口径，不瞎除。
func TestBuildPurchaseBasis_MissingSheetDate(t *testing.T) {
	received := map[string]decimal.Decimal{"P1": dec("30")}
	purchase := buildPurchaseBasis(received, nil,
		map[string][]string{"P1": {"PI-1", "PI-2"}},
		map[string]string{"PI-1": "2026-09-18"}) // PI-2 无收货日
	b := purchase["P1"]
	if b.Days != 1 {
		t.Fatalf("Days = %d, want 1(收货日不全时不判定多日)", b.Days)
	}
	if !b.CompareQty().Equal(dec("30")) {
		t.Errorf("比较量 = %s, want 30", b.CompareQty())
	}
}

// TestCheckQuantity_MultiDayUsesDailyAverage 是端到端口径锁：
// 10 天累计进货 220（日均 22） vs 合理备货量 49.304 ⇒ 不应出过量信号。
func TestCheckQuantity_MultiDayUsesDailyAverage(t *testing.T) {
	purchase := map[string]purchaseBasis{
		"PR1": {NetQty: dec("220"), Days: 10, FirstDate: "2026-09-18", LastDate: "2026-09-27"},
	}
	core := decimal.NewFromInt(10)
	stocking := map[string]StockingFact{"PR1": {
		ProductID:     "PR1",
		ProductName:   "香蕉-00518",
		Multiplier:    dec("1.43"),
		PeriodDays:    7,
		ReceivedQty:   dec("220"),
		PurchaseDays:  10,
		DailyAvgQty:   dec("22"),
		FirstDate:     "2026-09-18",
		LastDate:      "2026-09-27",
		ReasonableQty: dec("49.304"),
	}}
	sigs := checkQuantity(map[string]decimal.Decimal{"PR1": dec("22")},
		map[string]SaleRow{"PR1": {ProductID: "PR1", NetSaleQuantity: core}},
		nil, 3, QuantityContext{Stocking: stocking, Purchase: purchase})
	if len(sigs) != 0 {
		t.Fatalf("日均 22 略低于合理备货量 49.304 ⇒ 不应出信号, got %v", sigs)
	}

	// 反向：仍按累计 220 比 ⇒ 会误判为 block(4.5 倍)。确认我们没有这么算。
	sigs = checkQuantity(map[string]decimal.Decimal{"PR1": dec("220")},
		map[string]SaleRow{"PR1": {ProductID: "PR1", NetSaleQuantity: core}},
		nil, 3, QuantityContext{Stocking: stocking, Purchase: purchase})
	if len(sigs) != 1 || sigs[0].SignalType != SigQtyDeviation {
		t.Fatalf("对照组(传累计值)应 block, got %v", sigs)
	}
	if !containsAll(sigs[0].Message, "日均进货", "2026-09-18", "累计 220") {
		t.Errorf("多日信号文案必须说明日均口径与收货区间: %s", sigs[0].Message)
	}
}

// containsAll 判断 s 是否包含全部子串(测试辅助)。
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
