package service

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// jointRow 造一张联营结算单（LY）。
func jointRow(id, supplierID, sales, deduct string) SettlementRow {
	s, d := dec(sales), dec(deduct)
	return SettlementRow{
		ID: id, SupplierID: supplierID, DocumentType: DocumentTypeJoint,
		ApproveStatus:  "1",
		TotalGoodsYuan: s,
		Joint:          &JointFact{SettlementID: id, SalesYuan: s, DeductYuan: d},
	}
}

// jointItem 造一笔「扣点」收款项。
func jointItem(amount string) model.ApprovalItem {
	return model.ApprovalItem{Direction: ItemDirectionReceipt, ItemType: ReceiptTypeDeduction, AmountYuan: dec(amount)}
}

func TestCheckJoint_NoJointSettlements(t *testing.T) {
	sigs, sum := checkJoint([]SettlementRow{
		{ID: "S1", DocumentType: "DP", TotalGoodsYuan: dec("100")},
	}, nil, func(string) float64 { return 10 }, dec("1"))
	if sum.HasJoint || sum.SettlementCount != 0 {
		t.Fatalf("无联营单时不应有联营汇总: %+v", sum)
	}
	if len(sigs) != 0 {
		t.Fatalf("无联营单时不应产生信号, got %+v", sigs)
	}
}

// hasSignal 判断信号集里是否有某类型某级别的信号。
func hasSignal(sigs []signalDraft, sigType, level string) bool {
	for _, s := range sigs {
		if s.SignalType == sigType && s.Level == level {
			return true
		}
	}
	return false
}

func TestCheckJoint_DeductMissing(t *testing.T) {
	sigs, sum := checkJoint([]SettlementRow{
		jointRow("LY002610130070", "SUP1", "10000", "500"),
	}, nil, func(string) float64 { return 10 }, dec("1"))
	if !sum.HasJoint || sum.SettlementCount != 1 {
		t.Fatalf("应有 1 张联营单: %+v", sum)
	}
	if !sum.DeductRatePercent.Equal(dec("5")) {
		t.Fatalf("扣点率应为 5%%, got %s", sum.DeductRatePercent)
	}
	if !hasSignal(sigs, SigJointDeductMissing, model.SignalBlock) {
		t.Fatalf("应报 joint_deduct_missing(block), got %+v", sigs)
	}
}

func TestCheckJoint_DeductShort(t *testing.T) {
	sigs, _ := checkJoint([]SettlementRow{
		jointRow("LY1", "SUP1", "10000", "500"),
	}, []model.ApprovalItem{jointItem("300")}, func(string) float64 { return 10 }, dec("1"))
	if !hasSignal(sigs, SigJointDeductShort, model.SignalBlock) {
		t.Fatalf("应报 joint_deduct_short(block), got %+v", sigs)
	}
}

func TestCheckJoint_RateLow(t *testing.T) {
	// 扣点已足额申报,但扣点率 5% < 最低 10% ⇒ 只应给 warn。
	sigs, sum := checkJoint([]SettlementRow{
		jointRow("LY1", "SUP1", "10000", "500"),
	}, []model.ApprovalItem{jointItem("500")}, func(string) float64 { return 10 }, dec("1"))
	if len(sigs) != 1 || sigs[0].SignalType != SigJointRateLow || sigs[0].Level != model.SignalWarn {
		t.Fatalf("应报且仅报 joint_rate_low(warn), got %+v", sigs)
	}
	if !sum.DeductReceiptYuan.Equal(dec("500")) {
		t.Fatalf("申报扣点应为 500, got %s", sum.DeductReceiptYuan)
	}
	if sum.MinRatePercent != 10 {
		t.Fatalf("最低扣点率应为 10, got %v", sum.MinRatePercent)
	}
}

func TestCheckJoint_Pass(t *testing.T) {
	// 扣点足额 + 扣点率 10% ≥ 最低 10% ⇒ 无信号。
	sigs, sum := checkJoint([]SettlementRow{
		jointRow("LY1", "SUP1", "10000", "1000"),
	}, []model.ApprovalItem{jointItem("1000")}, func(string) float64 { return 10 }, dec("1"))
	if len(sigs) != 0 {
		t.Fatalf("达标不应产生信号, got %+v", sigs)
	}
	if !sum.DeductRatePercent.Equal(dec("10")) {
		t.Fatalf("扣点率应为 10%%, got %s", sum.DeductRatePercent)
	}
}

func TestCheckJoint_PaymentDirectionDoesNotCount(t *testing.T) {
	// 「扣点」写在**付款项**方向不算申报（必须作为收款项冲减）。
	sigs, _ := checkJoint([]SettlementRow{
		jointRow("LY1", "SUP1", "10000", "500"),
	}, []model.ApprovalItem{{
		Direction: ItemDirectionPayment, ItemType: ReceiptTypeDeduction, AmountYuan: dec("500"),
	}}, func(string) float64 { return 10 }, dec("1"))
	if !hasSignal(sigs, SigJointDeductMissing, model.SignalBlock) {
		t.Fatalf("付款方向的「扣点」不应视为申报, got %+v", sigs)
	}
}

func TestCheckAmounts_JointSkipsLineCheck(t *testing.T) {
	// 联营单没有 PI/RO 明细行 ⇒ 不应报 LineAmountMismatch,但货款要计入合计。
	sigs, amt := checkAmounts([]SettlementRow{
		jointRow("LY1", "SUP1", "10000", "1000"),
	}, nil, dec("9000"), dec("0.01"), dec("1"), decimal.Zero, dec("1000"))
	for _, s := range sigs {
		if s.SignalType == SigLineAmountMismatch {
			t.Fatalf("联营单不应报明细金额勾稽信号, got %+v", s)
		}
	}
	if !amt.SettledTotalYuan.Equal(dec("10000")) {
		t.Fatalf("联营单货款应计入合计, got %s", amt.SettledTotalYuan)
	}
	// 应付 = 10000 − 1000 = 9000，与手工金额一致 ⇒ 不应报 ManualAmountMismatch。
	if !amt.Consistent {
		t.Fatalf("应付 9000 与手工 9000 应一致, got %+v", amt)
	}
}
