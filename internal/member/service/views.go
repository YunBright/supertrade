// Package service / views.go —— 会员的派生视图(消费汇总 / 充值卡 / 储值卡 / 上一单)。
//
// # 边界:会员域**不复制销售数据**
//
// 图7「上一单」要显示"商品销售流水 + 收银流水"两张表,那些数据归 **pos** 服务。
// 会员域只有"这一笔消费给该会员加了多少积分"的凭证(积分流水里的 ref_flow_no = 销售单号)。
//
// ⇒ 本视图返回会员域**真正拥有**的部分,并在 `source` / `note` 里说清数据来源,
//
//	⛔ 不用假数据把两张表填满(那会让现场以为"上一单查不到"是会员域的 bug)。
//
// # 图2 F3 / F7
//
// 「查询充值卡信息」与「查询一卡通信息」在思迅里是两张独立的卡,
// 在本域里**同源于会员卡**:充值卡 = 储值视角,一卡通 = 卡 + 积分 + 折扣视角。
package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/member/model"
)

// ConsumeSummary 是会员消费汇总(GET /members/{id}/consume)。
type ConsumeSummary struct {
	// MemberID 是会员号。
	MemberID string `json:"member_id"`
	// ConsumedAmountYuan 是累计消费金额。
	ConsumedAmountYuan decimal.Decimal `json:"consumed_amount_yuan"`
	// ConsumedCount 是消费次数。
	ConsumedCount int `json:"consumed_count"`
	// BalanceYuan 是零钱包余额。
	BalanceYuan decimal.Decimal `json:"balance_yuan"`
	// Points 是积分汇总。
	Points *PointsSummary `json:"points"`
	// LastConsumeAt 是最近一次消费时刻。
	LastConsumeAt string `json:"last_consume_at,omitempty"`
}

// ConsumeSummaryOf 汇总某会员的消费。
func (s *Service) ConsumeSummaryOf(ctx context.Context, branchID, memberID string) (*ConsumeSummary, error) {
	member, err := s.getMemberAny(ctx, branchID, memberID)
	if err != nil {
		return nil, err
	}
	out := &ConsumeSummary{
		MemberID:           member.ID,
		ConsumedAmountYuan: member.ConsumedAmountYuan,
		ConsumedCount:      member.ConsumedCount,
		BalanceYuan:        member.BalanceYuan,
		Points: &PointsSummary{
			Accumulated: member.PointsAccumulated,
			Used:        member.PointsUsed,
			Remaining:   member.RemainingPoints,
		},
	}
	var last model.MemberPointLedger
	err = s.db.WithContext(ctx).
		Where("branch_id = ? AND member_id = ? AND reason = ?", branchID, memberID, model.PointReasonSale).
		Order("operated_at DESC").
		First(&last).Error
	if err == nil {
		out.LastConsumeAt = last.OperatedAt.Format(time.RFC3339)
	}
	return out, nil
}

// RechargeCardView 是充值卡信息(图2 F3)。
type RechargeCardView struct {
	// MemberID 是会员号(= 充值卡号)。
	MemberID string `json:"member_id"`
	// CardFaceNo 是卡面卡号。
	CardFaceNo string `json:"card_face_no"`
	// CardTypeID / CardTypeName 是会员类别。
	CardTypeID   string `json:"card_type_id"`
	CardTypeName string `json:"card_type_name"`
	// Status 是卡状态。
	Status string `json:"status"`
	// BalanceYuan 是当前储值余额。
	BalanceYuan decimal.Decimal `json:"balance_yuan"`
	// RechargedYuan 是累计充值。
	RechargedYuan decimal.Decimal `json:"recharged_yuan"`
	// RefundedYuan 是累计退储。
	RefundedYuan decimal.Decimal `json:"refunded_yuan"`
	// ValidFrom / ValidUntil 是有效期。
	ValidFrom  string `json:"valid_from,omitempty"`
	ValidUntil string `json:"valid_until,omitempty"`
	// LastRechargeAt 是最近一次充值时刻。
	LastRechargeAt string `json:"last_recharge_at,omitempty"`
}

// RechargeCardOf 取某会员的充值卡信息(图2 F3)。
func (s *Service) RechargeCardOf(ctx context.Context, branchID, memberID string) (*RechargeCardView, error) {
	member, err := s.getMemberAny(ctx, branchID, memberID)
	if err != nil {
		return nil, err
	}
	out := &RechargeCardView{
		MemberID:    member.ID,
		CardFaceNo:  member.CardFaceNo,
		CardTypeID:  member.CardTypeID,
		Status:      member.Status,
		BalanceYuan: member.BalanceYuan,
	}
	if mt, err := s.GetType(ctx, branchID, member.CardTypeID); err == nil {
		out.CardTypeName = mt.Name
	}
	if member.ValidFrom != nil {
		out.ValidFrom = member.ValidFrom.Format(time.RFC3339)
	}
	if member.ValidUntil != nil {
		out.ValidUntil = member.ValidUntil.Format(time.RFC3339)
	}
	acc, err := s.AccountSummaryOf(ctx, branchID, memberID)
	if err != nil {
		return nil, err
	}
	out.RechargedYuan = acc.RechargedYuan
	out.RefundedYuan = acc.RefundedYuan
	var last model.MemberAccountLedger
	err = s.db.WithContext(ctx).
		Where("branch_id = ? AND member_id = ? AND direction = ?", branchID, memberID, model.AccountDirectionRecharge).
		Order("operated_at DESC").
		First(&last).Error
	if err == nil {
		out.LastRechargeAt = last.OperatedAt.Format(time.RFC3339)
	}
	return out, nil
}

// StoredValueCardView 是一卡通 / 储值卡信息(图2 F7)。
type StoredValueCardView struct {
	// MemberID 是会员号。
	MemberID string `json:"member_id"`
	// CardFaceNo 是卡面卡号。
	CardFaceNo string `json:"card_face_no"`
	// CardTypeID / CardTypeName 是会员类别。
	CardTypeID   string `json:"card_type_id"`
	CardTypeName string `json:"card_type_name"`
	// IsStoredValueCard 是"该类别是否储值卡"。
	IsStoredValueCard bool `json:"is_stored_value_card"`
	// AllowPointsPay 是"该类别是否可用积分抵扣"。
	AllowPointsPay bool `json:"allow_points_pay"`
	// DiscountRate 是会员折扣(0.95 = 95 折)。
	DiscountRate decimal.Decimal `json:"discount_rate"`
	// BalanceYuan 是零钱包余额。
	BalanceYuan decimal.Decimal `json:"balance_yuan"`
	// Points 是积分汇总。
	Points *PointsSummary `json:"points"`
	// Status 是卡状态。
	Status string `json:"status"`
}

// StoredValueCardOf 取某会员的一卡通信息(图2 F7)。
func (s *Service) StoredValueCardOf(ctx context.Context, branchID, memberID string) (*StoredValueCardView, error) {
	member, err := s.getMemberAny(ctx, branchID, memberID)
	if err != nil {
		return nil, err
	}
	out := &StoredValueCardView{
		MemberID:    member.ID,
		CardFaceNo:  member.CardFaceNo,
		CardTypeID:  member.CardTypeID,
		BalanceYuan: member.BalanceYuan,
		Status:      member.Status,
		Points: &PointsSummary{
			Accumulated: member.PointsAccumulated,
			Used:        member.PointsUsed,
			Remaining:   member.RemainingPoints,
		},
	}
	if mt, err := s.GetType(ctx, branchID, member.CardTypeID); err == nil {
		out.CardTypeName = mt.Name
		out.IsStoredValueCard = mt.AllowStoredValue
		out.AllowPointsPay = mt.AllowPointsPay
		out.DiscountRate = mt.DiscountRate
	}
	return out, nil
}

// LastOrderView 是上一单(图7)。
//
// ⚠️ `sale_lines` / `payments` 恒为空:那两张表归 pos 服务(会员域不复制销售数据)。
//
//	会员域能给的是"这一笔消费给该会员加了多少积分"(积分流水里的凭证)。
type LastOrderView struct {
	// MemberID 是会员号。
	MemberID string `json:"member_id"`
	// BranchID 是门店。
	BranchID string `json:"branch_id"`
	// Source 是数据来源(member_local)。
	Source string `json:"source"`
	// LastOrder 是会员域掌握的最近一笔消费凭证(nil = 还没有会员消费)。
	LastOrder *LastOrderSummary `json:"last_order"`
	// SaleLines / Payments 恒为空数组(数据在 pos 服务)。
	SaleLines []any `json:"sale_lines"`
	Payments  []any `json:"payments"`
	// Note 是给调用方的说明。
	Note string `json:"note"`
}

// LastOrderSummary 是会员域掌握的最近一笔消费。
type LastOrderSummary struct {
	// SaleID 是销售单号(积分流水的 ref_flow_no)。
	SaleID string `json:"sale_id"`
	// PaidAt 是收款时刻。
	PaidAt string `json:"paid_at"`
	// PointsEarned 是这笔累计的积分。
	PointsEarned decimal.Decimal `json:"points_earned"`
	// OperatorID 是收银员。
	OperatorID string `json:"operator_id"`
}

// LastOrderOf 取某会员的上一单(图7)。
func (s *Service) LastOrderOf(ctx context.Context, branchID, memberID string) (*LastOrderView, error) {
	branchID = strings.TrimSpace(branchID)
	memberID = strings.TrimSpace(memberID)
	if branchID == "" || memberID == "" {
		return nil, fmt.Errorf("%w: branch_id / member_id 必填", model.ErrInvalidInput)
	}
	// 会员必须存在(否则前端拿到的是"查无此人",而不是"没消费过")。
	if _, err := s.getMemberAny(ctx, branchID, memberID); err != nil {
		return nil, err
	}
	out := &LastOrderView{
		MemberID:  memberID,
		BranchID:  branchID,
		Source:    "member_local",
		SaleLines: []any{},
		Payments:  []any{},
		Note:      "商品销售流水与收银流水归 pos 服务;会员域只保留'这一笔消费加了多少积分'的凭证。",
	}
	var last model.MemberPointLedger
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND member_id = ? AND reason = ?", branchID, memberID, model.PointReasonSale).
		Order("operated_at DESC").
		First(&last).Error
	if err != nil {
		// 没有会员消费 ⇒ last_order = nil(不是错误)。
		return out, nil
	}
	out.LastOrder = &LastOrderSummary{
		SaleID:       last.RefFlowNo,
		PaidAt:       last.OperatedAt.Format(time.RFC3339),
		PointsEarned: last.Points,
		OperatorID:   last.OperatorID,
	}
	return out, nil
}
