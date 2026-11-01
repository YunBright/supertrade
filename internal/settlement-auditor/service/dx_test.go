package service

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// dxRow 造一张代销结算单（DX）。
func dxRow(id, supplierID, retail, settle string) SettlementRow {
	rt, st := dec(retail), dec(settle)
	return SettlementRow{
		ID: id, SupplierID: supplierID, DocumentType: DocumentTypeDelivery,
		ApproveStatus:  "1",
		TotalGoodsYuan: st,
		Dx:             &DxFact{SettlementID: id, RetailYuan: rt, SettleYuan: st},
	}
}

func TestCheckDx_NoDxSettlements(t *testing.T) {
	sigs, sum := checkDx([]SettlementRow{
		{ID: "S1", DocumentType: "DP", TotalGoodsYuan: dec("100")},
	})
	if sum.HasDx || sum.SettlementCount != 0 {
		t.Fatalf("无代销单时不应有代销汇总: %+v", sum)
	}
	if len(sigs) != 0 {
		t.Fatalf("无代销单时不应产生信号, got %+v", sigs)
	}
}

func TestCheckDx_MarginPass(t *testing.T) {
	// 零售 1503 / 结算 1202.4 ⇒ 毛利 300.6，毛利率 20% ≥ 10% ⇒ 无信号。
	sigs, sum := checkDx([]SettlementRow{
		dxRow("DX002610190001", "SUP1", "1503", "1202.4"),
	})
	if len(sigs) != 0 {
		t.Fatalf("达标不应产生信号, got %+v", sigs)
	}
	if !sum.RetailYuan.Equal(dec("1503")) || !sum.SettleYuan.Equal(dec("1202.4")) {
		t.Fatalf("零售/结算额汇总不对: %+v", sum)
	}
	if !sum.MarginYuan.Equal(dec("300.6")) {
		t.Fatalf("毛利应为 300.6, got %s", sum.MarginYuan)
	}
	if !sum.MarginRatePercent.Equal(dec("20")) {
		t.Fatalf("毛利率应为 20%%, got %s", sum.MarginRatePercent)
	}
}

func TestCheckDx_MarginLow(t *testing.T) {
	// 零售 1000 / 结算 950 ⇒ 毛利率 5% < 10% ⇒ 只给 warn。
	sigs, sum := checkDx([]SettlementRow{
		dxRow("DX1", "SUP1", "1000", "950"),
	})
	if len(sigs) != 1 || sigs[0].SignalType != SigDxMarginLow || sigs[0].Level != model.SignalWarn {
		t.Fatalf("应报且仅报 dx_margin_low(warn), got %+v", sigs)
	}
	if !sum.MarginRatePercent.Equal(dec("5")) {
		t.Fatalf("毛利率应为 5%%, got %s", sum.MarginRatePercent)
	}
}

func TestCheckDx_MarginUnknown(t *testing.T) {
	// 取不到明细（Dx 为 nil）⇒ warn dx_margin_unknown，不参与合计。
	sigs, sum := checkDx([]SettlementRow{
		{ID: "DX1", SupplierID: "SUP1", DocumentType: DocumentTypeDelivery, ApproveStatus: "1"},
	})
	if sum.UnknownCount != 1 || sum.SettlementCount != 1 {
		t.Fatalf("应有 1 张未知毛利的代销单: %+v", sum)
	}
	if !hasSignal(sigs, SigDxMarginUnknown, model.SignalWarn) {
		t.Fatalf("应报 dx_margin_unknown(warn), got %+v", sigs)
	}
	if !sum.RetailYuan.IsZero() || !sum.SettleYuan.IsZero() {
		t.Fatalf("未知毛利的单不应计入合计: %+v", sum)
	}
}

func TestCheckDx_SupplierAggregated(t *testing.T) {
	// 同一供应商两张单：合计零售 2000 / 结算 1800 ⇒ 毛利率 10% ≥ 10% ⇒ 无信号。
	// （逐张 5% 但汇总达标 —— 口径是**整体实现毛利**，不是逐单）
	sigs, sum := checkDx([]SettlementRow{
		dxRow("DX1", "SUP1", "1000", "950"),
		dxRow("DX2", "SUP1", "1000", "850"),
	})
	if len(sigs) != 0 {
		t.Fatalf("整体毛利率 10%% 达标不应产生信号, got %+v", sigs)
	}
	if len(sum.Suppliers) != 1 {
		t.Fatalf("应聚合为 1 个供应商, got %+v", sum.Suppliers)
	}
	if got := len(sum.Suppliers[0].SettlementIDs); got != 2 {
		t.Fatalf("供应商下应有 2 张单, got %d", got)
	}
}

func TestWriteDxFacts(t *testing.T) {
	var sb strings.Builder
	writeDxFacts(&sb, DxSummary{}) // 无代销单 ⇒ 不写任何内容
	if sb.Len() != 0 {
		t.Fatalf("无代销单不应写提示词, got %q", sb.String())
	}
	writeDxFacts(&sb, DxSummary{
		HasDx: true, SettlementCount: 1, UnknownCount: 1,
		RetailYuan: dec("1503"), SettleYuan: dec("1202.4"),
		MarginYuan: dec("300.6"), MarginRatePercent: dec("20"),
		Suppliers: []DxSupplierFact{{
			SupplierID: "SUP1", SettlementIDs: []string{"DX1"},
			RetailYuan: dec("1503"), SettleYuan: dec("1202.4"),
			MarginYuan: dec("300.6"), MarginRatePercent: dec("20"), MinRatePercent: 10,
		}},
	})
	out := sb.String()
	if !strings.Contains(out, "代销毛利(引擎口径)") || !strings.Contains(out, "无法核算毛利") {
		t.Fatalf("提示词内容不对: %q", out)
	}
}

func TestCheckAmounts_DxSkipsLineCheck(t *testing.T) {
	// 代销单没有 PI/RO 明细行 ⇒ 不应报 LineAmountMismatch，但结算额要计入合计。
	sigs, amt := checkAmounts([]SettlementRow{
		dxRow("DX1", "SUP1", "1503", "1202.4"),
	}, nil, dec("1202.4"), dec("0.01"), dec("1"), decimal.Zero, decimal.Zero)
	for _, s := range sigs {
		if s.SignalType == SigLineAmountMismatch {
			t.Fatalf("代销单不应报明细金额勾稽信号, got %+v", s)
		}
	}
	if !amt.SettledTotalYuan.Equal(dec("1202.4")) {
		t.Fatalf("代销结算额应计入合计, got %s", amt.SettledTotalYuan)
	}
	if !amt.Consistent {
		t.Fatalf("应付 1202.4 与手工 1202.4 应一致, got %+v", amt)
	}
}
