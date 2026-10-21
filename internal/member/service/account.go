// Package service / account.go —— 储值(零钱包)业务逻辑。
//
// # 业务规则
//
//   - 余额只有一个真相源:`members.balance_yuan`。
//   - 每一笔变动都写 `member_account_ledger`(append-only),
//     `op_id` 唯一索引 ⇒ **幂等**:重复提交只落一行,**返回首次结果,不报错**。
//   - 退储金额 > 余额 ⇒ `model.ErrAccountInsufficient`(handler 映 409)。
//   - 冻结 / 挂失 / 注销的卡不允许动余额。
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/YunBright/supertrade/internal/member/model"
)

// AccountResult 是储值写操作的返回。
type AccountResult struct {
	// Ledger 是本次(或首次)写入的流水行。
	Ledger *model.MemberAccountLedger `json:"account_ledger"`
	// BalanceAfter 是变动后的余额。
	BalanceAfter decimal.Decimal `json:"balance_after_yuan"`
}

// AccountOpInput 是储值充值 / 退储的入参。
type AccountOpInput struct {
	// OpID 是幂等键(必填)。
	OpID string
	// BranchID 是门店(必填)。
	BranchID string
	// MemberID 是会员(必填)。
	MemberID string
	// AmountYuan 是金额(> 0)。
	AmountYuan decimal.Decimal
	// Direction 是 recharge / refund。
	Direction string
	// Reason 是原因(默认与方向同名)。
	Reason string
	// OperatorID 是操作人。
	OperatorID string
	// AuthorizedBy 是越权授权人;空 = 操作人自己有权限。
	AuthorizedBy string
}

// Recharge 储值充值。
//
// ⚠️ 幂等:同一 op_id 重复调用 ⇒ 只落一行,返回首次结果。
func (s *Service) Recharge(ctx context.Context, in AccountOpInput) (*AccountResult, error) {
	in.Direction = model.AccountDirectionRecharge
	return s.changeAccount(ctx, in)
}

// Refund 退储。
//
// ⚠️ 余额不足 ⇒ `model.ErrAccountInsufficient`(handler 映 409)。
func (s *Service) Refund(ctx context.Context, in AccountOpInput) (*AccountResult, error) {
	in.Direction = model.AccountDirectionRefund
	return s.changeAccount(ctx, in)
}

// changeAccount 是充值 / 退储的共同实现。
func (s *Service) changeAccount(ctx context.Context, in AccountOpInput) (*AccountResult, error) {
	opID := strings.TrimSpace(in.OpID)
	branchID := strings.TrimSpace(in.BranchID)
	memberID := strings.TrimSpace(in.MemberID)
	if opID == "" || branchID == "" || memberID == "" {
		return nil, fmt.Errorf("%w: op_id / branch_id / member_id 必填", model.ErrInvalidInput)
	}
	if in.AmountYuan.LessThanOrEqual(decimal.Zero) {
		return nil, fmt.Errorf("%w: 金额必须大于 0", model.ErrInvalidInput)
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		reason = in.Direction
	}

	member, err := s.getMember(ctx, branchID, memberID)
	if err != nil {
		return nil, err
	}
	if member.Status != model.MemberStatusNormal {
		return nil, model.ErrMemberStatusNotAllowed
	}
	balanceAfter := member.BalanceYuan.Add(in.AmountYuan)
	if in.Direction == model.AccountDirectionRefund {
		if member.BalanceYuan.LessThan(in.AmountYuan) {
			return nil, model.ErrAccountInsufficient
		}
		balanceAfter = member.BalanceYuan.Sub(in.AmountYuan)
	}

	now := s.now()
	var out *AccountResult
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row := &model.MemberAccountLedger{
			ID:               newLedgerID("AL", now),
			BranchID:         branchID,
			MemberID:         memberID,
			OpID:             opID,
			Direction:        in.Direction,
			AmountYuan:       in.AmountYuan,
			BalanceAfterYuan: balanceAfter,
			Reason:           reason,
			OperatorID:       firstNonEmpty(in.OperatorID, actor),
			AuthorizedBy:     strings.TrimSpace(in.AuthorizedBy),
			OperatedAt:       now,
			CreatedAt:        now,
		}
		res := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "op_id"}},
			DoNothing: true,
		}).Create(row)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			// 重放:取回首次那行,返回**首次结果**,不动余额。
			existing, err := s.accountLedgerByOpID(tx, opID)
			if err != nil {
				return err
			}
			out = &AccountResult{Ledger: existing, BalanceAfter: existing.BalanceAfterYuan}
			return nil
		}
		upd := tx.Model(&model.Member{}).
			Where("id = ? AND branch_id = ?", memberID, branchID).
			Updates(map[string]any{
				"balance_yuan": balanceAfter,
				"updated_by":   firstNonEmpty(in.OperatorID, actor),
				"updated_at":   now,
			})
		if upd.Error != nil {
			return upd.Error
		}
		if upd.RowsAffected == 0 {
			return model.ErrMemberNotFound
		}
		out = &AccountResult{Ledger: row, BalanceAfter: balanceAfter}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// accountLedgerByOpID 按 op_id 取储值流水(幂等重放路径)。
func (s *Service) accountLedgerByOpID(tx *gorm.DB, opID string) (*model.MemberAccountLedger, error) {
	var row model.MemberAccountLedger
	if err := tx.Where("op_id = ?", opID).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, model.ErrMemberNotFound
		}
		return nil, err
	}
	return &row, nil
}

// ListAccountInput 是储值流水查询入参。
type ListAccountInput struct {
	BranchID string
	MemberID string
	// Limit 是条数上限(<=0 用默认)。
	Limit int
}

// ListAccount 列储值流水(倒序)。
func (s *Service) ListAccount(ctx context.Context, in ListAccountInput) ([]model.MemberAccountLedger, error) {
	branchID := strings.TrimSpace(in.BranchID)
	memberID := strings.TrimSpace(in.MemberID)
	if branchID == "" || memberID == "" {
		return nil, fmt.Errorf("%w: branch_id / member_id 必填", model.ErrInvalidInput)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	var rows []model.MemberAccountLedger
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND member_id = ?", branchID, memberID).
		Order("operated_at DESC, id DESC").
		Limit(limit).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// AccountSummary 是储值汇总(GET /members/{id}/account)。
type AccountSummary struct {
	// BalanceYuan 是当前余额。
	BalanceYuan decimal.Decimal `json:"balance_yuan"`
	// RechargedYuan 是累计充值。
	RechargedYuan decimal.Decimal `json:"recharged_yuan"`
	// RefundedYuan 是累计退储。
	RefundedYuan decimal.Decimal `json:"refunded_yuan"`
}

// AccountSummaryOf 汇总某会员的储值。
func (s *Service) AccountSummaryOf(ctx context.Context, branchID, memberID string) (*AccountSummary, error) {
	member, err := s.getMember(ctx, branchID, memberID)
	if err != nil {
		return nil, err
	}
	out := &AccountSummary{BalanceYuan: member.BalanceYuan}
	var rows []model.MemberAccountLedger
	err = s.db.WithContext(ctx).
		Where("branch_id = ? AND member_id = ?", branchID, memberID).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		switch r.Direction {
		case model.AccountDirectionRecharge:
			out.RechargedYuan = out.RechargedYuan.Add(r.AmountYuan)
		case model.AccountDirectionRefund:
			out.RefundedYuan = out.RefundedYuan.Add(r.AmountYuan)
		}
	}
	return out, nil
}
