// service/allowance_test.go —— 收款项放宽额度的单测（2026-10-25 用户口径）。
//
// 锁住两件事：
//  1. 有陈列费/堆头费 ⇒ 「进货可以多些」（数量偏离不再阻断）；
//  2. 有补差价 ⇒ 允许毛利降低，额度 = 补差价总额。
package service

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// 有陈列费 ⇒ 数量偏离的 block 降为 warn（用户举例:30.9 倍那单应当通过）。
func TestApplyReceiptAllowance_DowngradesQtyDeviation(t *testing.T) {
	drafts := []signalDraft{
		newSignal(model.SignalBlock, SigQtyDeviation, "", "P1",
			"商品 伊利金典纯牛奶250ml-6907992507095 本次进货 216,为 2026-08-26 采购入库以来销量 7 的 30.9 倍", nil),
	}
	al := Allowance{DisplayYuan: dec("300")}
	out := applyReceiptAllowance(drafts, al)
	if out[0].Level != model.SignalWarn {
		t.Fatalf("级别 = %s, want warn(有陈列费⇒进货可以多些)", out[0].Level)
	}
	if !strings.Contains(out[0].Message, "陈列费/堆头费") {
		t.Fatalf("消息里要写明放宽依据: %q", out[0].Message)
	}
	// 入参不被改(调用方可能还拿着它)。
	if drafts[0].Level != model.SignalBlock {
		t.Fatal("不该原地改入参切片")
	}
}

// 没有陈列费/堆头费 ⇒ 信号原样(维持原有判断规则)。
func TestApplyReceiptAllowance_NoAllowance(t *testing.T) {
	drafts := []signalDraft{
		newSignal(model.SignalBlock, SigQtyDeviation, "", "P1", "进货 216,销量 7", nil),
	}
	out := applyReceiptAllowance(drafts, Allowance{})
	if out[0].Level != model.SignalBlock {
		t.Fatalf("级别 = %s, want block(无放宽额度)", out[0].Level)
	}
}

// 「窗口内完全无销售」也放宽:供应商付了陈列费/堆头费,门店就是有意多备货/铺货。
// (系统本来就允许"申请人的备注"解释数量类信号 ⇒ 有结构的收款给出同样口径。)
func TestApplyReceiptAllowance_DowngradesNoSales(t *testing.T) {
	drafts := []signalDraft{
		newSignal(model.SignalBlock, SigPurchaseWithoutSales, "", "P1", "无销售记录", nil),
	}
	out := applyReceiptAllowance(drafts, Allowance{PromotionYuan: dec("100")})
	if out[0].Level != model.SignalWarn {
		t.Fatalf("级别 = %s, want warn(有堆头费⇒备货铺货是正常的)", out[0].Level)
	}
	if !strings.Contains(out[0].Message, "进货可以多些") {
		t.Fatalf("消息里要写明放宽依据: %q", out[0].Message)
	}
}

// 其它 block 信号(单据缺失等)不受影响。
func TestApplyReceiptAllowance_KeepsOtherBlocks(t *testing.T) {
	drafts := []signalDraft{
		newSignal(model.SignalBlock, SigSettlementMissing, "S1", "", "结算单不存在", nil),
	}
	out := applyReceiptAllowance(drafts, Allowance{DisplayYuan: dec("300")})
	if out[0].Level != model.SignalBlock {
		t.Fatalf("级别 = %s, want block", out[0].Level)
	}
}

// ---- 毛利额度 ----

func TestEstimateGrossMargin(t *testing.T) {
	cost := map[string]decimal.Decimal{
		"P1": dec("10"), // 进价 10
	}
	sales := map[string]SaleRow{
		// 卖 10 件、收入 150 ⇒ 毛利 50
		"P1": {ProductID: "P1", NetSaleQuantity: dec("10"), NetSaleAmountYuan: dec("150")},
		// 没有进货均价 ⇒ 算不出成本，不计入
		"P2": {ProductID: "P2", NetSaleQuantity: dec("5"), NetSaleAmountYuan: dec("100")},
	}
	gm := estimateGrossMargin(cost, sales)
	if !gm.Calculated {
		t.Fatal("应算得出毛利")
	}
	if gm.AmountYuan.String() != "50" {
		t.Fatalf("毛利 = %s, want 50", gm.AmountYuan.String())
	}
}

// 毛利缺口 ≤ 补差价 ⇒ info（按口径允许毛利降低）。
func TestCheckGrossMargin_CoveredByPriceDiff(t *testing.T) {
	gm := GrossMargin{AmountYuan: dec("-100"), Calculated: true}
	sigs := checkGrossMarginWithAllowance(gm, Allowance{PriceDiffYuan: dec("150")})
	if len(sigs) != 1 {
		t.Fatalf("信号数 = %d, want 1", len(sigs))
	}
	if sigs[0].Level != model.SignalInfo {
		t.Fatalf("级别 = %s, want info(缺口被补差价覆盖)", sigs[0].Level)
	}
	if sigs[0].SignalType != SigGrossMargin {
		t.Fatalf("信号类型 = %s, want %s", sigs[0].SignalType, SigGrossMargin)
	}
}

// 毛利缺口 > 补差价 ⇒ warn（提醒人工终审，仍不阻断）。
func TestCheckGrossMargin_ExceedsPriceDiff(t *testing.T) {
	gm := GrossMargin{AmountYuan: dec("-300"), Calculated: true}
	sigs := checkGrossMarginWithAllowance(gm, Allowance{PriceDiffYuan: dec("150")})
	if len(sigs) != 1 {
		t.Fatalf("信号数 = %d, want 1", len(sigs))
	}
	if sigs[0].Level != model.SignalWarn {
		t.Fatalf("级别 = %s, want warn", sigs[0].Level)
	}
	if !strings.Contains(sigs[0].Message, "超过补差价") {
		t.Fatalf("消息 = %q", sigs[0].Message)
	}
}

// 没有补差价 ⇒ 不产生毛利信号（不制造新噪音，维持原有判断）。
func TestCheckGrossMargin_NoPriceDiff(t *testing.T) {
	gm := GrossMargin{AmountYuan: dec("-300"), Calculated: true}
	if sigs := checkGrossMarginWithAllowance(gm, Allowance{}); len(sigs) != 0 {
		t.Fatalf("没有补差价却产生了 %d 条信号: %+v", len(sigs), sigs)
	}
}

// 毛利为正 ⇒ 不产生信号。
func TestCheckGrossMargin_Positive(t *testing.T) {
	gm := GrossMargin{AmountYuan: dec("50"), Calculated: true}
	if sigs := checkGrossMarginWithAllowance(gm, Allowance{PriceDiffYuan: dec("150")}); len(sigs) != 0 {
		t.Fatalf("毛利为正却产生了信号: %+v", sigs)
	}
}

// 进货均价：Σ金额 ÷ Σ收货量。
func TestUnitCostByProduct(t *testing.T) {
	cost := unitCostByProduct([]SheetLineRow{
		{ProductID: "P1", TotalAmountYuan: dec("100"), ReceivedQuantity: dec("10")},
		{ProductID: "P1", TotalAmountYuan: dec("50"), ReceivedQuantity: dec("5")},
		{ProductID: "P2", TotalAmountYuan: dec("80"), ReceivedQuantity: dec("0")},
	})
	if got := cost["P1"].String(); got != "10" {
		t.Fatalf("P1 均价 = %s, want 10", got)
	}
	if _, ok := cost["P2"]; ok {
		t.Fatal("收货量为 0 的商品不该有均价")
	}
}
