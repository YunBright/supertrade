// Package service / subscribe.go —— 订阅 sale.paid,累计会员积分。
//
// # 为什么是事件而不是直接调用
//
// 方案 §6.6:`sale.paid` 是**库存扣减**与**积分累计**的触发点
// ⇒ 会员与库存都不再走"直接调用",改走事件。
//
// # 幂等(全仓首例,没有先例可抄)
//
// `sale.paid` 的 payload 带 `op_id`;订阅方用它去重:
//
//	INSERT ... ON CONFLICT (op_id) DO NOTHING
//	RowsAffected == 0 ⇒ 这一笔已经处理过 ⇒ **什么都不做**
//
// ⇒ 同一笔销售重复投递(重试 / 重放),`member_point_ledger` 只有 1 行,
//
//	会员积分也只加一次。守卫测试见 subscribe_test.go。
//
// # member_id 为空 ⇒ 不计积分
//
// 普通单(非会员单)不产生积分流水。⚠️ 这不是错误,是常态。
//
// # payload 形状(pos 服务按这个发)
//
//	{
//	  "op_id": "uuid",
//	  "sale_id": "SO20261021000001",
//	  "branch_id": "10",
//	  "member_id": "M0001",
//	  "operator_id": "u-1",
//	  "total_amount_yuan": "22.20",
//	  "paid_at": "2026-10-21T12:00:00+08:00",
//	  "order_status": "S",
//	  "lines": [
//	    {"line_id":"L1","product_id":"00000297","qty":"2",
//	     "unit_price_yuan":"11.10","amount_yuan":"22.20"}
//	  ]
//	}
//
// ⚠️ 金额/数量都是**字符串**(`decimal.Decimal` 的 UnmarshalJSON 兼容
//
//	带引号的数字与裸数字两种形态)。
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/YunBright/supertrade/internal/member/model"
)

// TopicSalePaid 是本服务订阅的 topic。
const TopicSalePaid = "sale.paid"

// SalePaidLine 是 sale.paid 的 lines[] 元素。
type SalePaidLine struct {
	LineID        string          `json:"line_id"`
	ProductID     string          `json:"product_id"`
	Qty           decimal.Decimal `json:"qty"`
	UnitPriceYuan decimal.Decimal `json:"unit_price_yuan"`
	AmountYuan    decimal.Decimal `json:"amount_yuan"`
}

// SalePaidPayload 是 sale.paid 的业务 payload。
type SalePaidPayload struct {
	// OpID 是幂等键(必填)。
	OpID string `json:"op_id"`
	// SaleID 是销售单号。
	SaleID string `json:"sale_id"`
	// BranchID 是门店(必填)。
	BranchID string `json:"branch_id"`
	// MemberID 是会员号;空 = 普通单,不计积分。
	MemberID string `json:"member_id"`
	// OperatorID 是收银员。
	OperatorID string `json:"operator_id"`
	// TotalAmountYuan 是应收合计。
	TotalAmountYuan decimal.Decimal `json:"total_amount_yuan"`
	// PaidAt 是收款时刻。
	PaidAt time.Time `json:"paid_at"`
	// OrderStatus 是 "S"(销售) / "R"(退货)。
	OrderStatus string `json:"order_status"`
	// Lines 是销售明细。
	Lines []SalePaidLine `json:"lines"`
}

// SalePaidResult 是订阅处理的返回(用于日志 / 测试断言)。
type SalePaidResult struct {
	// Skipped 为 true ⇒ 没有计分(普通单 / 会员不存在 / 金额为 0)。
	Skipped bool `json:"skipped"`
	// Points 是本次累计的积分。
	Points decimal.Decimal `json:"points"`
	// Replayed 为 true ⇒ 这一笔之前已经处理过(幂等命中)。
	Replayed bool `json:"replayed"`
}

// OnSalePaid 处理 sale.paid:写积分流水 + 更新会员积分列。
//
// ⚠️ 幂等:同一 op_id 投递两次 ⇒ 流水只有 1 行、积分只加一次。
//
// ⚠️ 会员不存在 ⇒ **跳过并记日志**,不返错误:
//
//	事件不能因为"会员被删了"就无限重试(那会让整条 bus 卡住)。
func (s *Service) OnSalePaid(ctx context.Context, p SalePaidPayload) (*SalePaidResult, error) {
	opID := strings.TrimSpace(p.OpID)
	branchID := strings.TrimSpace(p.BranchID)
	memberID := strings.TrimSpace(p.MemberID)
	if opID == "" || branchID == "" {
		return nil, fmt.Errorf("%w: op_id / branch_id 必填", model.ErrInvalidInput)
	}
	// 普通单 ⇒ 不计积分。
	if memberID == "" {
		return &SalePaidResult{Skipped: true}, nil
	}
	// 退货(R)⇒ 不累计积分(思迅的退货单也不给积分)。
	if strings.EqualFold(strings.TrimSpace(p.OrderStatus), "R") {
		return &SalePaidResult{Skipped: true}, nil
	}

	// ⚠️ 该门店的会员档案来自**外部系统**(富百家 / 佳乐)⇒ 本域不记积分。
	// 硬写会写进本域 PG 的一行"没人看的会员",而那个门店的账在外部 ERP 里。
	if err := s.EnsureLocalSource(ctx, branchID); err != nil {
		if errors.Is(err, model.ErrExternalSourceReadOnly) {
			return &SalePaidResult{Skipped: true}, nil
		}
		return nil, err
	}

	member, err := s.getMember(ctx, branchID, memberID)
	if err != nil {
		if err == model.ErrMemberNotFound {
			return &SalePaidResult{Skipped: true}, nil
		}
		return nil, err
	}
	rate := s.pointsRateOf(ctx, branchID)
	points := CalculatePoints(p.TotalAmountYuan, rate)
	if points.LessThanOrEqual(decimal.Zero) {
		return &SalePaidResult{Skipped: true}, nil
	}

	now := s.now()
	operatedAt := p.PaidAt
	if operatedAt.IsZero() {
		operatedAt = now
	}
	var out *SalePaidResult
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		pointsAfter := member.PointsAccumulated.Add(points)
		row := &model.MemberPointLedger{
			ID:          newLedgerID("PL", now),
			BranchID:    branchID,
			MemberID:    memberID,
			OpID:        opID,
			Direction:   model.PointDirectionEarn,
			Points:      points,
			PointsAfter: pointsAfter,
			Reason:      model.PointReasonSale,
			RefFlowNo:   strings.TrimSpace(p.SaleID),
			OperatorID:  firstNonEmpty(p.OperatorID, actor),
			OperatedAt:  operatedAt,
			CreatedAt:   now,
		}
		res := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "op_id"}},
			DoNothing: true,
		}).Create(row)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			// 重复投递 ⇒ 什么都不做(幂等)。
			out = &SalePaidResult{Replayed: true, Points: points}
			return nil
		}
		upd := tx.Model(&model.Member{}).
			Where("id = ? AND branch_id = ?", memberID, branchID).
			Updates(map[string]any{
				"points_accumulated":   gorm.Expr("points_accumulated + ?", points),
				"consumed_amount_yuan": gorm.Expr("consumed_amount_yuan + ?", p.TotalAmountYuan),
				"consumed_count":       gorm.Expr("consumed_count + 1"),
				"updated_by":           firstNonEmpty(p.OperatorID, actor),
				"updated_at":           now,
			})
		if upd.Error != nil {
			return upd.Error
		}
		if upd.RowsAffected == 0 {
			return model.ErrMemberNotFound
		}
		if err := s.enqueuePointsChanged(tx, row); err != nil {
			return err
		}
		out = &SalePaidResult{Points: points}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
