// Package service / config_write.go —— 门店会员配置 + 会员类别的**写侧**。
//
// ⚠️ **配置类写操作不走图4/图5 的越权授权链路** ——
//
//	会员域的交易类写操作(开卡/冲减积分/充值…)走 service.Authorize,
//	因为那是"收银员在终端上想干没权限的事",POS 需要弹授权框(图4)。
//	而**配置类**写操作(改门店会员参数 / 改会员类别)是**管理员后台动作**,
//	用标准 rbac.RequireScopeWithBranch 守门即可(无权限 ⇒ 403 forbidden)。
//
// ⚠️ **门店永远来自 X-Branch-ID**,body 里不带 branch_id ——
//
//	⇒ 天然满足"会员类别 / 会员参数按门店维护",管理端也必须显式带上操作门店。
//
// ⚠️ **金额与比率一律 string** —— JSON number 到了客户端会变成 double。
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm/clause"

	"github.com/YunBright/supertrade/internal/member/model"
)

// SettingsInput 是门店会员配置的写侧入参。
//
// ⚠️ **指针 = 未传就保持原值**(局部更新语义)。
//
//	管理端的配置页可能只改一个开关 ⇒ 全量替换会把没改的字段清掉。
type SettingsInput struct {
	// BranchID 是门店(来自 X-Branch-ID)。
	BranchID string
	// MembershipEnabled 是"该门店是否启用会员"。
	MembershipEnabled *bool
	// PointsRate 是消费积分率(1.0 = 消费 1 元得 1 积分)。
	PointsRate *decimal.Decimal
	// PointsDeductRate 是积分抵扣率(1 积分抵多少元)。
	PointsDeductRate *decimal.Decimal
	// PasswordRequired 是"开卡是否必须设密码"。
	PasswordRequired *bool
	// FaceRequired 是"开卡是否必须录人脸"。
	FaceRequired *bool
	// MaxFacePerMember 是每个会员最多录几个人脸特征。
	MaxFacePerMember *int
	// UpdatedBy 是操作人(审计)。
	UpdatedBy string
}

// UpsertSettings 写门店会员配置(按 branch_id 幂等 upsert)。
//
// ⚠️ **points_rate 必须 > 0** —— 读侧 `GetSettings` 的口径是
//
//	`row.PointsRate > 0 才生效,否则回落默认值 1`
//
//	⇒ 写 0 进去会被读成 1("我明明设了不积分")⇒ 直接在写侧拒掉(400)。
func (s *Service) UpsertSettings(ctx context.Context, in SettingsInput) (*SettingsView, error) {
	branchID := strings.TrimSpace(in.BranchID)
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", model.ErrInvalidInput)
	}
	if in.PointsRate != nil && !in.PointsRate.GreaterThan(decimal.Zero) {
		return nil, fmt.Errorf("%w: points_rate 必须 > 0", model.ErrInvalidInput)
	}
	if in.PointsDeductRate != nil && in.PointsDeductRate.IsNegative() {
		return nil, fmt.Errorf("%w: points_deduct_rate 不能为负", model.ErrInvalidInput)
	}
	if in.MaxFacePerMember != nil && *in.MaxFacePerMember < 0 {
		return nil, fmt.Errorf("%w: max_face_per_member 不能为负", model.ErrInvalidInput)
	}

	// 取现值(没有行 ⇒ 默认值)⇒ 合并 ⇒ 落库。
	cur, err := s.GetSettings(ctx, branchID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	row := model.MemberSettings{
		BranchID:          branchID,
		MembershipEnabled: cur.MembershipEnabled,
		PointsRate:        cur.PointsRate,
		PointsDeductRate:  cur.PointsDeductRate,
		PasswordRequired:  cur.PasswordRequired,
		FaceRequired:      cur.FaceRequired,
		MaxFacePerMember:  cur.MaxFacePerMember,
		UpdatedBy:         strings.TrimSpace(in.UpdatedBy),
		UpdatedAt:         now,
	}
	if in.MembershipEnabled != nil {
		row.MembershipEnabled = *in.MembershipEnabled
	}
	if in.PointsRate != nil {
		row.PointsRate = *in.PointsRate
	}
	if in.PointsDeductRate != nil {
		row.PointsDeductRate = *in.PointsDeductRate
	}
	if in.PasswordRequired != nil {
		row.PasswordRequired = *in.PasswordRequired
	}
	if in.FaceRequired != nil {
		row.FaceRequired = *in.FaceRequired
	}
	if in.MaxFacePerMember != nil {
		row.MaxFacePerMember = *in.MaxFacePerMember
	}

	err = s.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "branch_id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"membership_enabled", "points_rate", "points_deduct_rate",
				"password_required", "face_required", "max_face_per_member",
				"updated_by", "updated_at",
			}),
		}).
		Create(&row).Error
	if err != nil {
		return nil, err
	}
	// ⚠️ **GORM 会省略带 default 标签的零值字段** ——
	//
	//	GORM 的 INSERT 构造里,`HasDefaultValue` 的字段**一律不进列清单**
	//	(让数据库填默认值)⇒ 写 `max_face_per_member=0`("不限制")
	//	在**首次插入**时落库变成 1 ⇒ 用户设了"不限制"却看到"只能录 1 个"。
	//
	//	⚠️ 不能靠改 model 的 default 标签绕开(会触发 SQLite 的表重建式迁移,
	//	生产上有风险)⇒ 这里在 upsert 之后**补一条显式 UPDATE**
	//	(UPDATE 路径一定会写这一列)。
	if in.MaxFacePerMember != nil && *in.MaxFacePerMember == 0 {
		err = s.db.WithContext(ctx).Model(&model.MemberSettings{}).
			Where("branch_id = ?", branchID).
			Update("max_face_per_member", 0).Error
		if err != nil {
			return nil, err
		}
	}
	return s.GetSettings(ctx, branchID)
}

// TypeInput 是会员类别的写侧入参。
//
// ⚠️ 同 SettingsInput:**指针 = 未传就保持原值**。
type TypeInput struct {
	// ID 是类别 id(与 BranchID 组成复合主键)。
	ID string
	// BranchID 是门店(来自 X-Branch-ID)。
	BranchID string
	// Name 是类别名。
	Name string
	// DiscountRate 是会员折扣(0.95 = 95 折;1 = 不打折)。
	DiscountRate *decimal.Decimal
	// FeeAmountYuan 是会员费。
	FeeAmountYuan *decimal.Decimal
	// LimitedUseCount 是限用次数(0 = 不限次)。
	LimitedUseCount *int
	// AllowStoredValue 是"是否储值卡"。
	AllowStoredValue *bool
	// AllowPointsPay 是"是否可用积分抵扣"。
	AllowPointsPay *bool
	// Priority 是排序权重(小的在前)。
	Priority *int
	// Status 是 active / inactive。
	Status *string
	// CreatedBy / UpdatedBy 是审计。
	CreatedBy string
	UpdatedBy string
}

// 会员类别的 status 取值。
const (
	// MemberTypeStatusActive 是启用。
	MemberTypeStatusActive = "active"
	// MemberTypeStatusInactive 是停用(软删,历史单据仍能对上)。
	MemberTypeStatusInactive = "inactive"
)

// UpsertType 写会员类别(按 (id, branch_id) 幂等 upsert)。
func (s *Service) UpsertType(ctx context.Context, in TypeInput) (*model.MemberType, error) {
	branchID := strings.TrimSpace(in.BranchID)
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", model.ErrInvalidInput)
	}
	typeID := strings.TrimSpace(in.ID)
	name := strings.TrimSpace(in.Name)
	if typeID == "" {
		return nil, fmt.Errorf("%w: id 必填", model.ErrInvalidInput)
	}
	// ⚠️ **name 只在"新建"时必填** ——
	//
	//	更新时允许只传要改的字段(不传 name ⇒ 保持原名),
	//	否则"只改折扣"这种局部更新会被 400 拒掉。
	var cur *model.MemberType
	if existing, err := s.GetType(ctx, branchID, typeID); err == nil && existing != nil {
		cur = existing
	} else if !errors.Is(err, model.ErrMemberTypeNotFound) {
		return nil, err
	}
	if name == "" {
		if cur == nil {
			return nil, fmt.Errorf("%w: name 必填", model.ErrInvalidInput)
		}
		name = cur.Name
	}
	if in.DiscountRate != nil && (in.DiscountRate.IsZero() || in.DiscountRate.GreaterThan(decimal.NewFromInt(1))) {
		// ⚠️ 下界是**严格大于 0** ——
		//
		//	`discount_rate` 列有 default=1,而 GORM 会**省略**带 default 标签的
		//	零值字段 ⇒ 写 0 在首次插入时会落库成 1(折扣被悄悄改掉)。
		//	⇒ 直接在写侧拒掉 0(0 折本身也没有业务含义)。
		return nil, fmt.Errorf("%w: discount_rate 必须在 (0,1] 之间", model.ErrInvalidInput)
	}
	if in.FeeAmountYuan != nil && in.FeeAmountYuan.IsNegative() {
		return nil, fmt.Errorf("%w: fee_amount_yuan 不能为负", model.ErrInvalidInput)
	}
	if in.LimitedUseCount != nil && *in.LimitedUseCount < 0 {
		return nil, fmt.Errorf("%w: limited_use_count 不能为负", model.ErrInvalidInput)
	}
	if in.Priority != nil && *in.Priority < 0 {
		return nil, fmt.Errorf("%w: priority 不能为负", model.ErrInvalidInput)
	}
	status := MemberTypeStatusActive
	if in.Status != nil {
		status = strings.TrimSpace(*in.Status)
	}
	if status != MemberTypeStatusActive && status != MemberTypeStatusInactive {
		return nil, fmt.Errorf("%w: status 只能是 active / inactive", model.ErrInvalidInput)
	}

	now := time.Now()
	row := model.MemberType{
		ID:               typeID,
		BranchID:         branchID,
		Name:             name,
		DiscountRate:     decimal.NewFromInt(1),
		FeeAmountYuan:    decimal.Zero,
		LimitedUseCount:  0,
		AllowStoredValue: false,
		AllowPointsPay:   false,
		Priority:         0,
		Status:           status,
		CreatedBy:        strings.TrimSpace(in.CreatedBy),
		UpdatedBy:        strings.TrimSpace(in.UpdatedBy),
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	// 已存在 ⇒ 先取现值做合并(局部更新)。
	if cur != nil {
		row.DiscountRate = cur.DiscountRate
		row.FeeAmountYuan = cur.FeeAmountYuan
		row.LimitedUseCount = cur.LimitedUseCount
		row.AllowStoredValue = cur.AllowStoredValue
		row.AllowPointsPay = cur.AllowPointsPay
		row.Priority = cur.Priority
		// ⚠️ **status 也要一起合并** ——
		//
		//	漏了它 ⇒ "只改折扣、不传 status"会把状态**静默重置成 active**
		//	(用户明明把类别停用了,一次改名就复活了)。
		row.Status = cur.Status
		row.CreatedAt = cur.CreatedAt
		row.CreatedBy = cur.CreatedBy
	}
	if in.DiscountRate != nil {
		row.DiscountRate = *in.DiscountRate
	}
	if in.FeeAmountYuan != nil {
		row.FeeAmountYuan = *in.FeeAmountYuan
	}
	if in.LimitedUseCount != nil {
		row.LimitedUseCount = *in.LimitedUseCount
	}
	if in.AllowStoredValue != nil {
		row.AllowStoredValue = *in.AllowStoredValue
	}
	if in.AllowPointsPay != nil {
		row.AllowPointsPay = *in.AllowPointsPay
	}
	if in.Priority != nil {
		row.Priority = *in.Priority
	}
	if in.Status != nil {
		row.Status = status
	}

	err := s.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "id"}, {Name: "branch_id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"name", "discount_rate", "fee_amount_yuan", "limited_use_count",
				"allow_stored_value", "allow_points_pay", "priority", "status",
				"updated_by", "updated_at",
			}),
		}).
		// ⚠️ Select("*")：同 UpsertSettings ——
		// GORM 会省略带 default 标签的零值字段 ⇒ 写 0 会被数据库默认值覆盖。
		Select("*").
		Create(&row).Error
	if err != nil {
		return nil, err
	}
	return s.GetType(ctx, branchID, typeID)
}

// DeleteType 删会员类别(**软删**)。
//
// ⚠️ **被引用的类别不能删** —— 否则 POS 的"会员类别"下拉里会突然少一项,
// 已经开出来的卡就选不回来了。
//
//	⇒ 先查该门店下还有没有正常会员在用这个类别 ⇒ 有就 409,让管理员先改会员的类别。
func (s *Service) DeleteType(ctx context.Context, branchID, typeID string) error {
	branchID = strings.TrimSpace(branchID)
	typeID = strings.TrimSpace(typeID)
	if branchID == "" || typeID == "" {
		return fmt.Errorf("%w: branch_id 与 type_id 必填", model.ErrInvalidInput)
	}
	if _, err := s.GetType(ctx, branchID, typeID); err != nil {
		return err
	}
	var used int64
	err := s.db.WithContext(ctx).
		Model(&model.Member{}).
		Where("branch_id = ? AND card_type_id = ? AND status <> ?",
			branchID, typeID, model.MemberStatusCancelled).
		Count(&used).Error
	if err != nil {
		return err
	}
	if used > 0 {
		return fmt.Errorf("%w:还有 %d 张卡在用", model.ErrMemberTypeInUse, used)
	}
	// GORM 的 DeletedAt ⇒ Delete 是软删(历史单据仍能对上类别)。
	return s.db.WithContext(ctx).
		Where("id = ? AND branch_id = ?", typeID, branchID).
		Delete(&model.MemberType{}).Error
}
