// Package service / payables.go —— 供应商应付台账（FR-08，计划 §11）。
//
// # 归组口径
//
//	审批项**没有结构化的供应商字段**(approval_items 只有 item_type 文本与 remark),
//	⇒ 本期按 `item_type` 文本归组;付款台账按 `supplier_name` 归组
//	(自动同步时 supplier_name 填的就是 item_type,人工登记时才可能是真供应商名)。
//	两边的键都是文本:对不上时表现为两行,而不是错误合并。
//
// # 只读
//
//	本文件只 SELECT approval_monthly_items 与 recon_payment_records,
//	不写跨域表(见 model/crossdomain.go)。
package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/cashrecon/model"
)

// SupplierPayable 是按文本归组的一行应付。
type SupplierPayable struct {
	Supplier string `json:"supplier"`
	// UnpaidAmount / UnpaidCount 来自审批月度总表（pay_state=unpaid）。
	UnpaidAmount decimal.Decimal `json:"unpaid_amount"`
	UnpaidCount  int             `json:"unpaid_count"`
	// PaidAmount / PaidCount 来自付款台账（含未核销的手工登记）。
	PaidAmount decimal.Decimal `json:"paid_amount"`
	PaidCount  int             `json:"paid_count"`
}

// PayablesResult 是应付台账响应。
type PayablesResult struct {
	BranchID    string            `json:"branch_id"`
	Items       []SupplierPayable `json:"items"`
	TotalUnpaid decimal.Decimal   `json:"total_unpaid"`
	TotalPaid   decimal.Decimal   `json:"total_paid"`
}

// SupplierPayables 汇总某门店的供应商应付（未付 + 已付，按文本归组）。
func (s *Service) SupplierPayables(ctx context.Context, branchID string) (PayablesResult, error) {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		return PayablesResult{}, model.ErrInvalidInput
	}
	out := PayablesResult{BranchID: branchID, Items: make([]SupplierPayable, 0)}
	byKey := map[string]*SupplierPayable{}

	// ---- 未付：审批月度总表 ----
	var unpaid []model.CrossMonthlyItem
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND direction = ? AND pay_state = ? AND instance_status NOT IN ?",
			branchID, crossDirectionPayment, crossPayStateUnpaid, crossExcludedInstanceStatuses).
		Find(&unpaid).Error
	if err != nil {
		return PayablesResult{}, fmt.Errorf("read approval_monthly_items: %w", err)
	}
	for _, it := range unpaid {
		key := payableKey(it.ItemType)
		row := ensurePayable(byKey, key)
		row.UnpaidAmount = row.UnpaidAmount.Add(it.AmountYuan)
		row.UnpaidCount++
		out.TotalUnpaid = out.TotalUnpaid.Add(it.AmountYuan)
	}

	// ---- 已付：付款台账 ----
	var paid []model.ReconPaymentRecord
	if err := s.db.WithContext(ctx).Where("branch_id = ?", branchID).Find(&paid).Error; err != nil {
		return PayablesResult{}, fmt.Errorf("read recon_payment_records: %w", err)
	}
	for _, p := range paid {
		row := ensurePayable(byKey, payableKey(p.SupplierName))
		row.PaidAmount = row.PaidAmount.Add(p.Amount)
		row.PaidCount++
		out.TotalPaid = out.TotalPaid.Add(p.Amount)
	}

	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		out.Items = append(out.Items, *byKey[k])
	}
	return out, nil
}

// payableKey 规整归组键（空文本统一成"未命名"）。
func payableKey(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "未命名"
	}
	return v
}

// ensurePayable 取得（必要时创建）一行的聚合器。
func ensurePayable(byKey map[string]*SupplierPayable, key string) *SupplierPayable {
	row, ok := byKey[key]
	if !ok {
		row = &SupplierPayable{Supplier: key}
		byKey[key] = row
	}
	return row
}
