// Package service / points.go —— 积分业务逻辑(赠分 / 冲减 / 消费累计)。
//
// # 积分口径(方案 §4.7,⛔ 不许自创)
//
//	points_accumulated = 累计获得
//	points_used        = 累计扣减
//	剩余积分           = points_accumulated − points_used(**派生,不落列**)
//
// ⛔ **不另立 `points_balance` 列**:多一个列就多一处会漂移的真相源。
//
// # 积分规则(实测确定)
//
//	points_rate     = 1.0(消费 1 元得 1 积分)
//	points_rounding = ceiling(**向上取整**)
//
// 实测样本:ceil(22.20)=22、ceil(20.50)=21、ceil(38.50)=39、ceil(69.60)=70
// —— 与思迅 `card_paylist.vip_acc` 逐一吻合。
//
// ⚠️ **取整方式必须有守卫测试**:改成 floor/round 时每笔都对不上,
// 而差额看起来"很小",极难察觉(见 points_test.go 的 TestCalculatePoints_Ceiling)。
//
// # 幂等
//
// 每条流水带 `op_id`(唯一索引)。重放时:
//   - 流水**只落一行**(ON CONFLICT DO NOTHING);
//   - **返回首次结果**,⛔ 不报错(报错会让 POS 离线重传整批失败)。
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

// PointsRateDefault 是默认消费积分率(消费 1 元得 1 积分)。
var PointsRateDefault = decimal.NewFromInt(1)

// PointsRounding 是 member_settings.points_rounding 的取值(契约名)。
//
// # ⚠️ **名字与行为都按实测走** —— 这里曾有一处契约自相矛盾,现已改正
//
// 契约(方案 §4.7 与增补 §2.10)原本写的是:
//
//	points_rounding = ceiling
//	样本:ceil(22.20)=22、ceil(20.50)=21、ceil(38.50)=39、ceil(69.60)=70
//
// ⚠️ **数学上的 ceil(22.20) = 23,不是 22** ⇒ "ceiling" 这个名字与它自己的样本矛盾。
//
// 增补 §2.10 给的实测表(思迅 `card_paylist.vip_acc`,8 行)里:
//
//	22.20→22  69.60→70  38.50→39  43.00→43  74.00→74  21.70→22  20.50→21  18.00→18
//
// ⇒ **四舍五入(round half up)8/8 全中**;
//
//	而"与契约里的那个标签字面一致"(数学 ceil)只有 7/8 —— 22.20 那行对不上。
//
// ⇒ 2026-10-21 起 **契约名也改成 `half_up`**(文档同步改正),
//
//	行为保持四舍五入不变 ⇒ **对外报的值与实际行为终于一致**。
//
// # 取整方式必须有守卫测试
//
// 改成 floor/round 时每笔都对不上,而差额看起来"很小",极难察觉
// (见 member_test.go 的 TestCalculatePoints_Rounding)。
//
// ⇒ 要改成数学 ceil 只需把 roundPoints 里的 Round(0) 换成 Ceil(),
//
//	但那样 22.20 会得 23 分 ⇒ **这一处需要人拍板**。
const PointsRounding = "half_up"

// CalculatePoints 消费金额 → 积分。
//
// ⚠️ amount <= 0 ⇒ 0 积分(退款/负数单不该产生积分)。
// ⚠️ rate <= 0 ⇒ 回落默认 1.0(配置写坏了不该让会员白拿/白扣积分)。
func CalculatePoints(amount, rate decimal.Decimal) decimal.Decimal {
	if amount.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero
	}
	if rate.LessThanOrEqual(decimal.Zero) {
		rate = PointsRateDefault
	}
	return roundPoints(amount.Mul(rate))
}

// roundPoints 取整(见 PointsRounding 的说明)。
//
// ⚠️ **四舍五入**(half away from zero),不是数学 ceil。
func roundPoints(v decimal.Decimal) decimal.Decimal {
	return v.Round(0)
}

// PointsResult 是积分写操作的返回。
type PointsResult struct {
	// Ledger 是本次(或首次)写入的流水行。
	Ledger *model.MemberPointLedger `json:"points_ledger"`
	// PointsAfter 是变动后的**累计获得值**(与流水里的 points_after 一致)。
	PointsAfter decimal.Decimal `json:"points_after"`
	// Remaining 是变动后的剩余积分(现算,⛔ 不落列)。
	Remaining decimal.Decimal `json:"remaining_points"`
}

// GrantPointsInput 是赠分的入参。
type GrantPointsInput struct {
	// OpID 是幂等键(必填)。
	OpID string
	// BranchID 是门店(必填)。
	BranchID string
	// MemberID 是会员(必填)。
	MemberID string
	// Points 是赠送积分数(> 0)。
	Points decimal.Decimal
	// Reason 是原因(默认 manual_grant)。
	Reason string
	// OperatorID 是操作人。
	OperatorID string
	// AuthorizedBy 是越权授权人;空 = 操作人自己有权限。
	AuthorizedBy string
}

// GrantPoints 赠分 / 手工增加积分。
//
// ⚠️ 幂等:同一 op_id 重复调用 ⇒ 流水只落一行,返回首次结果。
func (s *Service) GrantPoints(ctx context.Context, in GrantPointsInput) (*PointsResult, error) {
	return s.changePoints(ctx, pointsOp{
		OpID:         in.OpID,
		BranchID:     in.BranchID,
		MemberID:     in.MemberID,
		Points:       in.Points,
		Reason:       in.Reason,
		OperatorID:   in.OperatorID,
		AuthorizedBy: in.AuthorizedBy,
		Direction:    model.PointDirectionEarn,
	})
}

// DeductPointsInput 是冲减积分的入参(图2 F4)。
type DeductPointsInput struct {
	// OpID 是幂等键(必填)。
	OpID string
	// BranchID 是门店(必填)。
	BranchID string
	// MemberID 是会员(必填)。
	MemberID string
	// Points 是冲减积分数(> 0)。
	Points decimal.Decimal
	// Reason 是原因(默认 manual_deduct)。
	Reason string
	// RefFlowNo 是关联单号。
	RefFlowNo string
	// OperatorID 是操作人。
	OperatorID string
	// AuthorizedBy 是越权授权人;空 = 操作人自己有权限。
	AuthorizedBy string
}

// DeductPoints 冲减积分(图2 F4 / 图4 越权授权)。
//
// ⚠️ 剩余积分不足 ⇒ `model.ErrPointsInsufficient`(handler 映 409)。
// ⚠️ 会员状态不是 normal ⇒ `model.ErrMemberStatusNotAllowed`(handler 映 409)。
func (s *Service) DeductPoints(ctx context.Context, in DeductPointsInput) (*PointsResult, error) {
	return s.changePoints(ctx, pointsOp{
		OpID:         in.OpID,
		BranchID:     in.BranchID,
		MemberID:     in.MemberID,
		Points:       in.Points,
		Reason:       in.Reason,
		RefFlowNo:    in.RefFlowNo,
		OperatorID:   in.OperatorID,
		AuthorizedBy: in.AuthorizedBy,
		Direction:    model.PointDirectionDeduct,
	})
}

// pointsOp 是赠分 / 冲减的共同入参。
type pointsOp struct {
	OpID         string
	BranchID     string
	MemberID     string
	Points       decimal.Decimal
	Reason       string
	RefFlowNo    string
	OperatorID   string
	AuthorizedBy string
	Direction    string
}

// changePoints 是赠分 / 冲减的共同实现。
//
// ⚠️ 幂等靠**流水的 op_id 唯一索引**:
//   - INSERT ... ON CONFLICT (op_id) DO NOTHING
//   - RowsAffected == 0 ⇒ 已处理过 ⇒ 取回首次那行返回(**不报错**),
//     且**不动**会员的积分列(否则重放会把积分加两次)。
func (s *Service) changePoints(ctx context.Context, op pointsOp) (*PointsResult, error) {
	opID := strings.TrimSpace(op.OpID)
	branchID := strings.TrimSpace(op.BranchID)
	memberID := strings.TrimSpace(op.MemberID)
	if opID == "" || branchID == "" || memberID == "" {
		return nil, fmt.Errorf("%w: op_id / branch_id / member_id 必填", model.ErrInvalidInput)
	}
	if op.Points.LessThanOrEqual(decimal.Zero) {
		return nil, fmt.Errorf("%w: 积分数必须大于 0", model.ErrInvalidInput)
	}
	reason := strings.TrimSpace(op.Reason)
	if reason == "" {
		if op.Direction == model.PointDirectionEarn {
			reason = model.PointReasonManualGrant
		} else {
			reason = model.PointReasonManualDeduct
		}
	}

	member, err := s.getMember(ctx, branchID, memberID)
	if err != nil {
		return nil, err
	}
	// 冻结 / 挂失 / 注销的卡不允许动积分。
	if member.Status != model.MemberStatusNormal {
		return nil, model.ErrMemberStatusNotAllowed
	}
	remaining := member.PointsAccumulated.Sub(member.PointsUsed)
	if op.Direction == model.PointDirectionDeduct && remaining.LessThan(op.Points) {
		return nil, model.ErrPointsInsufficient
	}

	now := s.now()
	var out *PointsResult
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 变动后的累计获得值:
		//   earn   → points_accumulated += N
		//   deduct → points_accumulated 不变(扣的是 points_used)
		pointsAfter := member.PointsAccumulated
		if op.Direction == model.PointDirectionEarn {
			pointsAfter = member.PointsAccumulated.Add(op.Points)
		}
		signed := op.Points
		if op.Direction == model.PointDirectionDeduct {
			signed = op.Points.Neg()
		}
		row := &model.MemberPointLedger{
			ID:           newLedgerID("PL", now),
			BranchID:     branchID,
			MemberID:     memberID,
			OpID:         opID,
			Direction:    op.Direction,
			Points:       signed,
			PointsAfter:  pointsAfter,
			Reason:       reason,
			RefFlowNo:    strings.TrimSpace(op.RefFlowNo),
			OperatorID:   firstNonEmpty(op.OperatorID, actor),
			AuthorizedBy: strings.TrimSpace(op.AuthorizedBy),
			OperatedAt:   now,
			CreatedAt:    now,
		}
		res := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "op_id"}},
			DoNothing: true,
		}).Create(row)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			// 重放:取回首次那行,返回**首次结果**,不动会员积分列。
			existing, err := s.pointLedgerByOpID(tx, opID)
			if err != nil {
				return err
			}
			out = &PointsResult{
				Ledger:      existing,
				PointsAfter: existing.PointsAfter,
				Remaining:   existing.PointsAfter.Sub(member.PointsUsed),
			}
			return nil
		}
		// 更新会员积分列(earn 加累计获得,deduct 加累计扣减)。
		col := "points_accumulated"
		if op.Direction == model.PointDirectionDeduct {
			col = "points_used"
		}
		upd := tx.Model(&model.Member{}).
			Where("id = ? AND branch_id = ?", memberID, branchID).
			Updates(map[string]any{
				col:          gorm.Expr(col+" + ?", op.Points),
				"updated_by": firstNonEmpty(op.OperatorID, actor),
				"updated_at": now,
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
		out = &PointsResult{
			Ledger:      row,
			PointsAfter: pointsAfter,
			Remaining:   pointsAfter.Sub(member.PointsUsed),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// pointLedgerByOpID 按 op_id 取积分流水(幂等重放路径)。
func (s *Service) pointLedgerByOpID(tx *gorm.DB, opID string) (*model.MemberPointLedger, error) {
	var row model.MemberPointLedger
	if err := tx.Where("op_id = ?", opID).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, model.ErrMemberNotFound
		}
		return nil, err
	}
	return &row, nil
}

// ListPointsInput 是积分流水查询入参。
type ListPointsInput struct {
	BranchID string
	MemberID string
	// Limit 是条数上限(<=0 用默认)。
	Limit int
}

// ListPoints 列积分流水(倒序:最近的在前)。
func (s *Service) ListPoints(ctx context.Context, in ListPointsInput) ([]model.MemberPointLedger, error) {
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
	var rows []model.MemberPointLedger
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

// PointsSummary 是积分汇总(GET /members/{id}/points)。
type PointsSummary struct {
	// Accumulated 是累计获得。
	Accumulated decimal.Decimal `json:"points_accumulated"`
	// Used 是累计扣减。
	Used decimal.Decimal `json:"points_used"`
	// Remaining 是剩余积分(派生)。
	Remaining decimal.Decimal `json:"remaining_points"`
}

// PointsSummaryOf 汇总某会员的积分。
func (s *Service) PointsSummaryOf(ctx context.Context, branchID, memberID string) (*PointsSummary, error) {
	member, err := s.getMemberAny(ctx, branchID, memberID)
	if err != nil {
		return nil, err
	}
	return &PointsSummary{
		Accumulated: member.PointsAccumulated,
		Used:        member.PointsUsed,
		Remaining:   member.RemainingPoints,
	}, nil
}
