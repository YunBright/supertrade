// Package service / checks.go —— 确定性规则核对引擎(FR-2.2/FR-2.3)。
//
// 每个 check 是纯函数:输入 cube 行 + 参数,输出 []model.AuditSignal 草稿
// (ID/ApprovalID/CreatedAt 由 pipeline 落库前补),便于单测。
//
// 信号级别语义:
//   - info:提示(如非采购结算单),不阻断;
//   - warn:偏离超阈值,交给 LLM 综合研判;
//   - block:硬异常(单据缺失/未审核/金额勾稽不上),规则降级路径直接拒绝。
package service

import (
	"encoding/json"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// 信号类型。
const (
	SigSettlementMissing     = "settlement_missing"      // 结算单不存在
	SigSettlementNotApproved = "settlement_not_approved" // 结算单未审核
	SigNoPurchaseLines       = "no_purchase_lines"       // 非采购结算单(无 PI/RO 行)
	SigSheetMissing          = "sheet_missing"           // 关联采购单不存在
	SigSheetNotApproved      = "sheet_not_approved"      // 采购单未审核
	SigSupplierMismatch      = "supplier_mismatch"       // 采购单与结算单供应商不一致
	SigLineAmountMismatch    = "line_amount_mismatch"    // Σ明细 vs 结算单金额勾稽不上
	SigManualAmountMismatch  = "manual_amount_mismatch"  // 结算总额 vs 手工累加金额不符
	SigPurchaseWithoutSales  = "purchase_without_sales"  // 窗口内无销售却有进货
	SigQtyDeviation          = "qty_deviation"           // 采购量/销售量偏离超阈值
)

// signalDraft 是落库前的信号草稿。
type signalDraft struct {
	SettlementID string
	ProductID    string
	SignalType   string
	Level        string
	Message      string
	Evidence     any
}

func newSignal(level, sigType, settlementID, productID, msg string, evidence any) signalDraft {
	return signalDraft{
		SettlementID: settlementID,
		ProductID:    productID,
		SignalType:   sigType,
		Level:        level,
		Message:      msg,
		Evidence:     evidence,
	}
}

// hasBlock 判断信号集里是否有 block。
func hasBlock(drafts []signalDraft) bool {
	for _, d := range drafts {
		if d.Level == model.SignalBlock {
			return true
		}
	}
	return false
}

// marshalEvidence 序列化证据(失败兜底为字符串)。
func marshalEvidence(ev any) []byte {
	if ev == nil {
		return nil
	}
	b, err := json.Marshal(ev)
	if err != nil {
		b, _ = json.Marshal(map[string]any{"raw": fmt.Sprint(ev)})
	}
	return b
}

// ---- check 1: 结算单存在性 + 审核状态 ----

// checkSettlements 核对期望的结算单是否全部存在且已审核。
func checkSettlements(expectedIDs []string, found []SettlementRow) []signalDraft {
	byID := make(map[string]SettlementRow, len(found))
	for _, r := range found {
		byID[r.ID] = r
	}
	var out []signalDraft
	for _, id := range expectedIDs {
		r, ok := byID[id]
		if !ok {
			out = append(out, newSignal(model.SignalBlock, SigSettlementMissing, id, "",
				fmt.Sprintf("结算单 %s 在 sixun 中不存在", id),
				map[string]any{"settlement_id": id}))
			continue
		}
		if r.ApproveStatus != "1" {
			out = append(out, newSignal(model.SignalBlock, SigSettlementNotApproved, id, "",
				fmt.Sprintf("结算单 %s 未审核(approve_status=%s)", id, r.ApproveStatus),
				map[string]any{"settlement_id": id, "approve_status": r.ApproveStatus}))
		}
	}
	return out
}

// ---- check 2: 采购单存在性/审核状态/供应商一致性 ----

// checkSheets 核对逐张拆出的采购单。settlementSupplier 提供结算单 → 供应商映射。
func checkSheets(lines []SettlementLineRow, sheets []PurchaseSheetRow, settlementSupplier map[string]string) []signalDraft {
	byID := make(map[string]PurchaseSheetRow, len(sheets))
	for _, r := range sheets {
		byID[r.ID] = r
	}
	var out []signalDraft
	for _, ln := range lines {
		sh, ok := byID[ln.VoucherID]
		if !ok {
			out = append(out, newSignal(model.SignalBlock, SigSheetMissing, ln.SettlementID, "",
				fmt.Sprintf("结算单 %s 关联的采购单 %s 不存在", ln.SettlementID, ln.VoucherID),
				map[string]any{"voucher_id": ln.VoucherID, "source_doc_type": ln.SourceDocType}))
			continue
		}
		if sh.ApproveStatus != "1" {
			out = append(out, newSignal(model.SignalBlock, SigSheetNotApproved, ln.SettlementID, "",
				fmt.Sprintf("采购单 %s 未审核(approve_status=%s)", ln.VoucherID, sh.ApproveStatus),
				map[string]any{"voucher_id": ln.VoucherID, "approve_status": sh.ApproveStatus}))
		}
		if want := settlementSupplier[ln.SettlementID]; want != "" && sh.SupplierID != want {
			out = append(out, newSignal(model.SignalBlock, SigSupplierMismatch, ln.SettlementID, "",
				fmt.Sprintf("采购单 %s 供应商 %s 与结算单供应商 %s 不一致", ln.VoucherID, sh.SupplierID, want),
				map[string]any{"voucher_id": ln.VoucherID, "sheet_supplier": sh.SupplierID, "settlement_supplier": want}))
		}
	}
	return out
}

// ---- check 3: 金额勾稽 ----

// checkAmounts 三层勾稽:
//
//	a) 每张结算单:Σ(PI/RO 行 total_goods_yuan) vs settlement.total_goods_yuan;
//	b) 全部结算单货款合计 vs 申请人手工累加金额;
//	容差 cfg.AmountToleranceYuan(默认 0.01)。
func checkAmounts(settlements []SettlementRow, linesBySettlement map[string][]SettlementLineRow, manualAmount, tolerance decimal.Decimal) []signalDraft {
	var out []signalDraft
	grandTotal := decimal.Zero
	for _, st := range settlements {
		lineSum := decimal.Zero
		for _, ln := range linesBySettlement[st.ID] {
			lineSum = lineSum.Add(ln.TotalGoodsYuan)
		}
		grandTotal = grandTotal.Add(st.TotalGoodsYuan)
		diff := lineSum.Sub(st.TotalGoodsYuan).Abs()
		if diff.GreaterThan(tolerance) {
			out = append(out, newSignal(model.SignalBlock, SigLineAmountMismatch, st.ID, "",
				fmt.Sprintf("结算单 %s 金额勾稽不符:明细合计 %s ≠ 单头货款 %s(差 %s)",
					st.ID, lineSum.StringFixed(2), st.TotalGoodsYuan.StringFixed(2), diff.StringFixed(2)),
				map[string]any{
					"settlement_id": st.ID,
					"line_sum":      lineSum.String(),
					"header_goods":  st.TotalGoodsYuan.String(),
					"diff":          diff.String(),
				}))
		}
	}
	diff := grandTotal.Sub(manualAmount).Abs()
	if diff.GreaterThan(tolerance) {
		out = append(out, newSignal(model.SignalBlock, SigManualAmountMismatch, "", "",
			fmt.Sprintf("手工累加付款金额 %s 与结算单货款合计 %s 不符(差 %s)",
				manualAmount.StringFixed(2), grandTotal.StringFixed(2), diff.StringFixed(2)),
			map[string]any{
				"manual_amount": manualAmount.String(),
				"settled_total": grandTotal.String(),
				"diff":          diff.String(),
			}))
	}
	return out
}

// ---- check 4: 数量信号(采购量 vs 销售/库存) ----

// checkQuantity 对每个进货商品比较窗口采购量与窗口销量:
//
//   - 窗口销量 == 0 且进货 > 0 → block(purchase_without_sales);
//   - 进货 > 销量 × factor×2   → block(qty_deviation);
//   - 进货 > 销量 × factor     → warn(qty_deviation)。
//
// received 只统计 PI(进货);RO(退货)按负量冲减由 caller 聚合时处理
// (退货量大是另一类信号,本期不做)。
func checkQuantity(receivedByProduct map[string]decimal.Decimal, salesByProduct map[string]SaleRow, factor float64) []signalDraft {
	var out []signalDraft
	f := decimal.NewFromFloat(factor)
	f2 := decimal.NewFromFloat(factor * 2)
	for pid, recv := range receivedByProduct {
		if !recv.GreaterThan(decimal.Zero) {
			continue
		}
		sale := salesByProduct[pid]
		sold := sale.NetSaleQuantity
		if !sold.GreaterThan(decimal.Zero) {
			out = append(out, newSignal(model.SignalBlock, SigPurchaseWithoutSales, "", pid,
				fmt.Sprintf("商品 %s 窗口内无销售记录却进货 %s", pid, recv.String()),
				map[string]any{"product_id": pid, "received": recv.String(), "sold": "0"}))
			continue
		}
		ratio := recv.Div(sold)
		switch {
		case recv.GreaterThan(sold.Mul(f2)):
			out = append(out, newSignal(model.SignalBlock, SigQtyDeviation, "", pid,
				fmt.Sprintf("商品 %s 窗口进货 %s 达销量 %s 的 %s 倍(>%.0f×)", pid, recv.String(), sold.String(), ratio.StringFixed(1), factor*2),
				map[string]any{"product_id": pid, "received": recv.String(), "sold": sold.String(), "ratio": ratio.String()}))
		case recv.GreaterThan(sold.Mul(f)):
			out = append(out, newSignal(model.SignalWarn, SigQtyDeviation, "", pid,
				fmt.Sprintf("商品 %s 窗口进货 %s 为销量 %s 的 %s 倍(>%.0f×)", pid, recv.String(), sold.String(), ratio.StringFixed(1), factor),
				map[string]any{"product_id": pid, "received": recv.String(), "sold": sold.String(), "ratio": ratio.String()}))
		}
	}
	return out
}
