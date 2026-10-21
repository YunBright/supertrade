// Package service / settings.go —— 门店级会员配置(读侧)。
//
// ⚠️ **配置由 tradewind_app 写,POS 只读** —— 本包只提供读能力,
//
//	不提供写端点(端点表里也没有 `member:config` 的写路由)。
//
// ⚠️ **配置行可能不存在**(门店还没配过会员参数)⇒ 此时返回**默认值**
//
//	而不是 404:POS 打开会员菜单不应该因为"没配参数"就报错。
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/member/model"
)

// SettingsView 是门店会员配置视图(GET /config)。
type SettingsView struct {
	// BranchID 是门店。
	BranchID string `json:"branch_id"`
	// MembershipEnabled 是"该门店是否启用会员"。
	MembershipEnabled bool `json:"membership_enabled"`
	// PointsRate 是消费积分率(1.0 = 消费 1 元得 1 积分)。
	PointsRate decimal.Decimal `json:"points_rate"`
	// PointsDeductRate 是积分抵扣率。
	PointsDeductRate decimal.Decimal `json:"points_deduct_rate"`
	// PointsRounding 是取整方式(固定 ceiling)。
	PointsRounding string `json:"points_rounding"`
	// PasswordRequired 是"开卡是否必须设密码"。
	PasswordRequired bool `json:"password_required"`
	// FaceRequired 是"开卡是否必须录人脸"。
	FaceRequired bool `json:"face_required"`
	// MaxFacePerMember 是每个会员最多录几个人脸特征。
	MaxFacePerMember int `json:"max_face_per_member"`
	// UpdatedAt 是最后修改时刻。
	UpdatedAt string `json:"updated_at,omitempty"`
}

// GetSettings 取门店会员配置(没有配置行 ⇒ 返回默认值)。
func (s *Service) GetSettings(ctx context.Context, branchID string) (*SettingsView, error) {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", model.ErrInvalidInput)
	}
	view := &SettingsView{
		BranchID:         branchID,
		PointsRate:       PointsRateDefault,
		PointsDeductRate: decimal.Zero,
		PointsRounding:   PointsRounding,
		MaxFacePerMember: 1,
	}
	var row model.MemberSettings
	err := s.db.WithContext(ctx).Where("branch_id = ?", branchID).First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 没有配置行 ⇒ 默认值(不报错)。
			return view, nil
		}
		return nil, err
	}
	view.MembershipEnabled = row.MembershipEnabled
	if row.PointsRate.GreaterThan(decimal.Zero) {
		view.PointsRate = row.PointsRate
	}
	view.PointsDeductRate = row.PointsDeductRate
	view.PasswordRequired = row.PasswordRequired
	view.FaceRequired = row.FaceRequired
	// ⚠️ **原样取**，不做 "> 0 才生效" 的过滤 ——
	//
	//	"0 = 不限制录人脸" 是一个**合法配置**,过滤掉会被读成默认 1
	//	⇒ 用户设了"不限制"却看到"只能录 1 个"。
	//
	//	缺配置行时返回的仍是默认值 1(见上面的 view 初始化)。
	view.MaxFacePerMember = row.MaxFacePerMember
	view.UpdatedAt = row.UpdatedAt.Format("2006-01-02T15:04:05Z07:00")
	return view, nil
}
