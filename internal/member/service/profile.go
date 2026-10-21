// Package service / profile.go —— 会员**档案**的局部更新(改身份字段)。
//
// # ⚠️ 这个端点**只能改身份字段**
//
//	积分 / 储值 / 消费 / 状态是本域 POS 业务的事实源
//	(sale.paid 订阅给会员加积分、手工赠分/充值也改它们)⇒
//	它们**只能**走专用的积分/储值/状态端点。
//
//	⇒ 本文件里**根本没有**这些字段(不是"handler 拦一下",是**类型上就没有**)
//	⇒ handler 额外把 body 里出现的这些列**直接 400**(而不是静默忽略):
//	  静默忽略的形态是"前端以为改了积分,其实没改",没有任何人会发现。
//
// # ⚠️ 幂等:带 op_id
//
//	同 op_id 重复提交 ⇒ 只生效一次(靠发件箱行判定,见 opApplied),
//	**重放返回首次的结果,不报错**。
//
// # ⚠️ 局部更新语义:指针 = 未传就保持原值
//
//	照抄 UpsertSettings / UpsertType 的范式 ——
//	管理端可能只改一个字段,全量替换会把没改的清掉。
package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/member/model"
)

// UpdateProfileInput 是"改会员档案"的入参。
//
// ⚠️ **指针 = 未传就保持原值**(局部更新语义)。
//
// ⛔ **没有**积分/储值/消费/状态字段 —— 类型上就不给改它们的机会。
type UpdateProfileInput struct {
	// OpID 是幂等键(必填)。
	OpID string
	// BranchID 是门店(来自 X-Branch-ID)。
	BranchID string
	// MemberID 是会员号(来自路径)。
	MemberID string
	// Name 是姓名。
	Name *string
	// Phone / Mobile 是电话 / 手机。
	Phone  *string
	Mobile *string
	// Gender 是性别。
	Gender *string
	// Birthday 是生日。
	Birthday *time.Time
	// Favorite 是爱好。
	Favorite *string
	// Remark 是备注。
	Remark *string
	// IDCardNo 是身份证。
	IDCardNo *string
	// ValidFrom / ValidUntil 是有效期。
	ValidFrom  *time.Time
	ValidUntil *time.Time
	// CardTypeID 是会员类别(必须存在且属于同一门店)。
	CardTypeID *string
	// CardFaceNo 是卡面卡号。
	CardFaceNo *string
	// OperatorID 是操作人。
	OperatorID string
	// AuthorizedBy 是越权授权人(审计)。
	AuthorizedBy string
}

// UpdateProfile 局部更新会员档案。
//
// ⚠️ 幂等:同一 op_id 重复提交 ⇒ 只生效一次,重放返回当前值、不报错。
func (s *Service) UpdateProfile(ctx context.Context, in UpdateProfileInput) (*model.Member, error) {
	opID := strings.TrimSpace(in.OpID)
	branchID := strings.TrimSpace(in.BranchID)
	if opID == "" || branchID == "" {
		return nil, fmt.Errorf("%w: op_id / branch_id 必填", model.ErrInvalidInput)
	}
	member, err := s.getMember(ctx, branchID, in.MemberID)
	if err != nil {
		return nil, err
	}

	updates := map[string]any{}
	if in.Name != nil {
		updates["name"] = strings.TrimSpace(*in.Name)
	}
	if in.Phone != nil {
		updates["phone"] = strings.TrimSpace(*in.Phone)
	}
	if in.Mobile != nil {
		updates["mobile"] = strings.TrimSpace(*in.Mobile)
	}
	if in.Gender != nil {
		updates["gender"] = strings.TrimSpace(*in.Gender)
	}
	if in.Birthday != nil {
		updates["birthday"] = *in.Birthday
	}
	if in.Favorite != nil {
		updates["favorite"] = strings.TrimSpace(*in.Favorite)
	}
	if in.Remark != nil {
		updates["remark"] = strings.TrimSpace(*in.Remark)
	}
	if in.IDCardNo != nil {
		updates["id_card_no"] = strings.TrimSpace(*in.IDCardNo)
	}
	if in.ValidFrom != nil {
		updates["valid_from"] = *in.ValidFrom
	}
	if in.ValidUntil != nil {
		updates["valid_until"] = *in.ValidUntil
	}
	if in.CardFaceNo != nil {
		updates["card_face_no"] = strings.TrimSpace(*in.CardFaceNo)
	}
	if in.CardTypeID != nil {
		ct := strings.TrimSpace(*in.CardTypeID)
		if ct != "" {
			// ⚠️ 类别必须**存在且属于同一门店** —— 否则改完的卡挂在别的店的类别上,
			//   POS 的类别下拉里根本看不到它(症状:会员"消失"了)。
			if _, err := s.GetType(ctx, branchID, ct); err != nil {
				return nil, err
			}
			updates["card_type_id"] = ct
		}
	}

	if len(updates) == 0 {
		// 什么都没传 ⇒ 直接返回当前值(不写库、不发事件)。
		return member, nil
	}

	now := s.now()
	changed := make([]string, 0, len(updates))
	for k := range updates {
		changed = append(changed, k)
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 幂等:同一 op_id 已经应用过 ⇒ 什么都不做(重放返回当前值)。
		applied, err := s.opApplied(tx, opID)
		if err != nil {
			return err
		}
		if applied {
			return nil
		}
		updates["authorized_by"] = strings.TrimSpace(in.AuthorizedBy)
		updates["updated_by"] = firstNonEmpty(in.OperatorID, actor)
		updates["updated_at"] = now
		res := tx.Model(&model.Member{}).
			Where("id = ? AND branch_id = ?", member.ID, member.BranchID).
			Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return model.ErrMemberNotFound
		}
		return s.enqueueMemberUpdated(tx, opID, member.ID, member.BranchID, changed)
	})
	if err != nil {
		return nil, err
	}
	return s.getMember(ctx, branchID, member.ID)
}
