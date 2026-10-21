// Package service / member.go —— 会员主档业务逻辑。
//
// # 业务规则
//
//   - 主键是 **(id, branch_id)** 复合主键:思迅卡号**非全局唯一**
//     (同一卡号可以出现在不同门店)⇒ 跨店同号是**两张卡**,不是一个。
//   - 软删用 `gorm.DeletedAt` ⇒ 删了查不到(列表 / 详情都自动过滤)。
//   - 写操作(开卡 / 改状态 / 改密 / 录人脸)都带 `op_id` ⇒ **幂等**:
//     同一 op_id 重复提交只落一次,**重放返回首次结果,不报错**。
//   - 每个写操作都在**同一个事务**里写业务行 + 发件箱事件(见 events.go)。
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

// DefaultListLimit 是列表默认条数。
const DefaultListLimit = 50

// MaxListLimit 是列表最大条数。
const MaxListLimit = 200

// OpenCardInput 是开卡(图6)的入参。
type OpenCardInput struct {
	// OpID 是幂等键(必填)。
	OpID string
	// BranchID 是门店(必填)。
	BranchID string
	// ID 是会员号;空 ⇒ 自动生成。
	ID string
	// CardTypeID 是会员类别(必填,须存在)。
	CardTypeID string
	// CardFaceNo 是卡面卡号。
	CardFaceNo string
	// Name 是姓名(必填)。
	Name string
	// Phone / Mobile 是电话 / 手机。
	Phone  string
	Mobile string
	// Gender 是性别。
	Gender string
	// Birthday 是生日。
	Birthday *time.Time
	// CalendarType 是历法(公历/农历)。
	CalendarType string
	// ValidFrom / ValidUntil 是有效期。
	ValidFrom  *time.Time
	ValidUntil *time.Time
	// Favorite 是爱好。
	Favorite string
	// IDCardNo 是身份证。
	IDCardNo string
	// Remark 是备注。
	Remark string
	// FeeAmountYuan 是会员费。
	FeeAmountYuan decimal.Decimal
	// LimitedUseCount 是限用次数(0 = 不限次)。
	LimitedUseCount int
	// Password 是会员口令(明文入参,服务内哈希后存)。
	Password string
	// FaceID 是面部特征。
	FaceID string
	// OpenID 是微信/支付宝 openid。
	OpenID string
	// OperatorID 是操作人(auth user id)。
	OperatorID string
	// AuthorizedBy 是越权授权人;空 = 操作人自己有权限。
	AuthorizedBy string
}

// OpenCard 开卡(图6)。
//
// ⚠️ **幂等**:同一 op_id 重复提交 ⇒ 不再插第二行,返回首次那张卡。
//
// ⚠️ 事件 `member.opened` 与会员行**同事务**写入(见 events.go)。
func (s *Service) OpenCard(ctx context.Context, in OpenCardInput) (*model.Member, error) {
	opID := strings.TrimSpace(in.OpID)
	branchID := strings.TrimSpace(in.BranchID)
	if opID == "" || branchID == "" {
		return nil, fmt.Errorf("%w: op_id / branch_id 必填", model.ErrInvalidInput)
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: 姓名不能为空", model.ErrInvalidInput)
	}
	cardTypeID := strings.TrimSpace(in.CardTypeID)
	if cardTypeID == "" {
		return nil, fmt.Errorf("%w: card_type_id 必填", model.ErrInvalidInput)
	}
	// 会员类别必须存在(否则开出来的卡挂在一个不存在的类别上,POS 下拉里看不到)。
	var mt model.MemberType
	if err := s.db.WithContext(ctx).
		Where("id = ? AND branch_id = ?", cardTypeID, branchID).
		First(&mt).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, model.ErrMemberTypeNotFound
		}
		return nil, err
	}

	now := s.now()
	id := strings.TrimSpace(in.ID)
	if id == "" {
		id = newLedgerID("M", now)
	}
	passwordHash := ""
	if strings.TrimSpace(in.Password) != "" {
		h, err := HashPassword(in.Password)
		if err != nil {
			return nil, fmt.Errorf("%w: 口令哈希失败: %v", model.ErrInvalidInput, err)
		}
		passwordHash = h
	}

	var out *model.Member
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row := &model.Member{
			ID:                 id,
			BranchID:           branchID,
			OpID:               opID,
			CardTypeID:         cardTypeID,
			CardFaceNo:         strings.TrimSpace(in.CardFaceNo),
			Name:               name,
			Phone:              strings.TrimSpace(in.Phone),
			Mobile:             strings.TrimSpace(in.Mobile),
			Gender:             strings.TrimSpace(in.Gender),
			Birthday:           in.Birthday,
			CalendarType:       strings.TrimSpace(in.CalendarType),
			Status:             model.MemberStatusNormal,
			ValidFrom:          in.ValidFrom,
			ValidUntil:         in.ValidUntil,
			Favorite:           strings.TrimSpace(in.Favorite),
			IDCardNo:           strings.TrimSpace(in.IDCardNo),
			Remark:             strings.TrimSpace(in.Remark),
			FeeAmountYuan:      in.FeeAmountYuan,
			LimitedUseCount:    in.LimitedUseCount,
			PointsAccumulated:  decimal.Zero,
			PointsUsed:         decimal.Zero,
			BalanceYuan:        decimal.Zero,
			ConsumedAmountYuan: decimal.Zero,
			AuthorizedBy:       strings.TrimSpace(in.AuthorizedBy),
			PasswordHash:       passwordHash,
			FaceID:             strings.TrimSpace(in.FaceID),
			OpenID:             strings.TrimSpace(in.OpenID),
			CreatedBy:          firstNonEmpty(in.OperatorID, actor),
			UpdatedBy:          firstNonEmpty(in.OperatorID, actor),
			CreatedAt:          now,
			UpdatedAt:          now,
		}
		// op_id 幂等:靠 members 上的 op_id 唯一索引 + DO NOTHING。
		res := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "op_id"}},
			DoNothing: true,
		}).Create(row)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			// 重放:同一 op_id 已经开过卡 ⇒ 取回首次那张卡,不报错。
			existing, err := s.memberByOpID(tx, branchID, opID)
			if err != nil {
				return err
			}
			out = existing
			return nil
		}
		if err := s.enqueueMemberOpened(tx, row, now); err != nil {
			return err
		}
		out = row
		return nil
	})
	if err != nil {
		return nil, err
	}
	fillDerived(out)
	return out, nil
}

// memberByOpID 按 op_id 取会员(开卡幂等重放路径)。
func (s *Service) memberByOpID(tx *gorm.DB, branchID, opID string) (*model.Member, error) {
	var row model.Member
	err := tx.Where("branch_id = ? AND op_id = ?", branchID, opID).First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, model.ErrMemberNotFound
		}
		return nil, err
	}
	fillDerived(&row)
	return &row, nil
}

// Get 取会员详情(图3)。
func (s *Service) Get(ctx context.Context, branchID, memberID string) (*model.Member, error) {
	return s.getMember(ctx, branchID, memberID)
}

// ListMembersInput 是会员查询入参。
type ListMembersInput struct {
	// BranchID 是门店(必填)。
	BranchID string
	// Field 是查询字段(白名单):card_id / name / phone / mobile / remark / id_card_no。
	Field string
	// Q 是查询值(模糊)。
	Q string
	// Limit 是条数上限(<=0 用默认)。
	Limit int
	// Offset 是**翻页起点**(跳过的行数)。
	//
	// ⚠️ 为什么必须有它:一个门店导入后有 4128 个会员,
	//   而 Limit 上限是 200 ⇒ 不给 offset 就**只能看到前 200 条**,
	//   而且响应里没有"总数"⇒ 前端会以为"这个店就这么点会员"(静默截断)。
	Offset int
}

// ListMembers 查会员列表(图3 的"查询条件"下拉 + 输入框)。
//
// ⚠️ **只认白名单字段**(model.QueryFieldColumn)—— 把 query 直接拼进 SQL
// 是注入面,而这里根本不需要灵活性。
//
// ⚠️ 本方法**不返回总数**:要总数用 ListMembersPage(见下)。
func (s *Service) ListMembers(ctx context.Context, in ListMembersInput) ([]model.Member, error) {
	rows, _, err := s.ListMembersPage(ctx, in)
	return rows, err
}

// ListMembersPage 查会员列表**并给出总命中行数**。
//
// # ⚠️ 为什么必须有 total
//
//	Limit 上限 200(MaxListLimit)⇒ 一个 4128 人的门店,前端必须能翻页。
//	而只返"本次返回几行"的话,前端分页器**不知道有没有下一页**
//	⇒ 症状是"会员档案页只有 200 条,收银员以为这个店就这么点会员"——
//	静默截断比报错危险。
//
// # ⚠️ 计数与列表**必须用同一组条件**(门店 + field + q)
//
//	条件不一致 ⇒ total 与实际能翻到的条数对不上,
//	前端就会"翻到最后一页还有下一页"(或反过来)。
//
// # ⚠️ field 不在白名单时**同样 400**
//
//	否则 total 会绕过白名单:列表 400、计数却成功 ⇒ 两条路径的语义不一致。
func (s *Service) ListMembersPage(ctx context.Context, in ListMembersInput) ([]model.Member, int64, error) {
	branchID := strings.TrimSpace(in.BranchID)
	if branchID == "" {
		return nil, 0, fmt.Errorf("%w: branch_id 必填", model.ErrInvalidInput)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	offset := in.Offset
	if offset < 0 {
		offset = 0
	}

	// ⚠️ **两条独立的查询链** —— 复用同一条链时,Count 留下的 SELECT
	//   会污染后面的 Find(症状:列表恒为空,且不报错)。
	countQ, err := applyMemberFilter(s.db.WithContext(ctx).Model(&model.Member{}),
		branchID, in.Field, in.Q)
	if err != nil {
		return nil, 0, err
	}
	var total int64
	if err := countQ.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	rowsQ, err := applyMemberFilter(s.db.WithContext(ctx).Model(&model.Member{}),
		branchID, in.Field, in.Q)
	if err != nil {
		return nil, 0, err
	}
	var rows []model.Member
	// ⚠️ 排序键必须是**全序**:updated_at 会并列,id 在本门店内唯一 ⇒
	//   (updated_at DESC, id ASC) 是全序 ⇒ 翻页不会重复/漏行。
	if err := rowsQ.Order("updated_at DESC, id ASC").Limit(limit).Offset(offset).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	for i := range rows {
		fillDerived(&rows[i])
	}
	return rows, total, nil
}

// applyMemberFilter 把"门店 + 查询字段 + 查询值"的条件加到链上。
//
// ⚠️ **列表与计数共用它** —— 两边各写一份条件时,改了一边 total 就与列表对不上。
func applyMemberFilter(q *gorm.DB, branchID, field, val string) (*gorm.DB, error) {
	q = q.Where("branch_id = ?", branchID)
	field = strings.TrimSpace(field)
	if field == "" {
		return q, nil
	}
	col, ok := model.QueryFieldColumn(field)
	if !ok {
		return nil, fmt.Errorf("%w: field=%q 不是允许的查询字段", model.ErrInvalidQueryField, field)
	}
	if v := strings.TrimSpace(val); v != "" {
		q = q.Where(col+" LIKE ?", "%"+v+"%")
	}
	return q, nil
}

// UpdateStatusInput 是改会员状态的入参(冻结/解冻/挂失/解挂/注销)。
type UpdateStatusInput struct {
	OpID     string
	BranchID string
	MemberID string
	// Status 是目标状态。
	Status string
	// OperatorID 是操作人。
	OperatorID string
	// AuthorizedBy 是越权授权人。
	AuthorizedBy string
}

// UpdateStatus 改会员状态。
//
// ⚠️ 幂等:同一 op_id 重复提交 ⇒ 状态不变、不报错(返回当前状态)。
func (s *Service) UpdateStatus(ctx context.Context, in UpdateStatusInput) (*model.Member, error) {
	opID := strings.TrimSpace(in.OpID)
	branchID := strings.TrimSpace(in.BranchID)
	if opID == "" || branchID == "" {
		return nil, fmt.Errorf("%w: op_id / branch_id 必填", model.ErrInvalidInput)
	}
	status := strings.TrimSpace(in.Status)
	if !model.ValidMemberStatus(status) {
		return nil, fmt.Errorf("%w: status=%q 非法", model.ErrInvalidInput, status)
	}
	member, err := s.getMember(ctx, branchID, in.MemberID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 幂等:同一 op_id 已经应用过 ⇒ 什么都不做(重放返回当前状态)。
		applied, err := s.opApplied(tx, opID)
		if err != nil {
			return err
		}
		if applied {
			return nil
		}
		res := tx.Model(&model.Member{}).
			Where("id = ? AND branch_id = ?", member.ID, member.BranchID).
			Updates(map[string]any{
				"status":        status,
				"authorized_by": strings.TrimSpace(in.AuthorizedBy),
				"updated_by":    firstNonEmpty(in.OperatorID, actor),
				"updated_at":    now,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return model.ErrMemberNotFound
		}
		return s.enqueueMemberUpdated(tx, opID, member.ID, member.BranchID, []string{"status"})
	})
	if err != nil {
		return nil, err
	}
	return s.getMember(ctx, branchID, member.ID)
}

// UpdatePasswordInput 是改密/重置口令的入参。
type UpdatePasswordInput struct {
	OpID     string
	BranchID string
	MemberID string
	// Password 是新口令(明文入参,服务内哈希后存)。
	Password string
	// OldPassword 是原口令;**重置**时可空,改密时必填。
	OldPassword string
	// OperatorID 是操作人。
	OperatorID string
	// AuthorizedBy 是越权授权人。
	AuthorizedBy string
}

// UpdatePassword 改密 / 重置口令(图6 的 密码 / 确认密码 / 原密码)。
//
// ⚠️ 原口令错 ⇒ 403(不是 404 / 500):调用方需要区分"口令错"与"会员不存在"。
func (s *Service) UpdatePassword(ctx context.Context, in UpdatePasswordInput) (*model.Member, error) {
	opID := strings.TrimSpace(in.OpID)
	branchID := strings.TrimSpace(in.BranchID)
	if opID == "" || branchID == "" {
		return nil, fmt.Errorf("%w: op_id / branch_id 必填", model.ErrInvalidInput)
	}
	if strings.TrimSpace(in.Password) == "" {
		return nil, fmt.Errorf("%w: 新口令不能为空", model.ErrInvalidInput)
	}
	member, err := s.getMember(ctx, branchID, in.MemberID)
	if err != nil {
		return nil, err
	}
	// 改密(有原口令)必须校验原口令;重置(无原口令)走 member:password-manage 权限。
	if old := strings.TrimSpace(in.OldPassword); old != "" {
		ok, err := VerifyPassword(old, member.PasswordHash)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, ErrInvalidOldPassword
		}
	}
	hash, err := HashPassword(in.Password)
	if err != nil {
		return nil, fmt.Errorf("%w: 口令哈希失败: %v", model.ErrInvalidInput, err)
	}
	now := s.now()
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 幂等:同一 op_id 已经应用过 ⇒ 什么都不做(重放返回当前状态)。
		applied, err := s.opApplied(tx, opID)
		if err != nil {
			return err
		}
		if applied {
			return nil
		}
		res := tx.Model(&model.Member{}).
			Where("id = ? AND branch_id = ?", member.ID, member.BranchID).
			Updates(map[string]any{
				"password_hash": hash,
				"authorized_by": strings.TrimSpace(in.AuthorizedBy),
				"updated_by":    firstNonEmpty(in.OperatorID, actor),
				"updated_at":    now,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return model.ErrMemberNotFound
		}
		// ⚠️ 密码本身**不进事件 payload**(changed_fields 只写字段名)。
		return s.enqueueMemberUpdated(tx, opID, member.ID, member.BranchID, []string{"password_hash"})
	})
	if err != nil {
		return nil, err
	}
	return s.getMember(ctx, branchID, member.ID)
}

// UpdateFaceInput 是人脸特征登记的入参(图6 人脸识别[F3])。
type UpdateFaceInput struct {
	OpID     string
	BranchID string
	MemberID string
	// FaceID 是面部特征标识。
	FaceID string
	// OperatorID 是操作人。
	OperatorID string
	// AuthorizedBy 是越权授权人。
	AuthorizedBy string
}

// UpdateFace 登记 / 更新人脸特征。
func (s *Service) UpdateFace(ctx context.Context, in UpdateFaceInput) (*model.Member, error) {
	opID := strings.TrimSpace(in.OpID)
	branchID := strings.TrimSpace(in.BranchID)
	if opID == "" || branchID == "" {
		return nil, fmt.Errorf("%w: op_id / branch_id 必填", model.ErrInvalidInput)
	}
	faceID := strings.TrimSpace(in.FaceID)
	if faceID == "" {
		return nil, fmt.Errorf("%w: face_id 不能为空", model.ErrInvalidInput)
	}
	member, err := s.getMember(ctx, branchID, in.MemberID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 幂等:同一 op_id 已经应用过 ⇒ 什么都不做(重放返回当前状态)。
		applied, err := s.opApplied(tx, opID)
		if err != nil {
			return err
		}
		if applied {
			return nil
		}
		res := tx.Model(&model.Member{}).
			Where("id = ? AND branch_id = ?", member.ID, member.BranchID).
			Updates(map[string]any{
				"face_id":       faceID,
				"authorized_by": strings.TrimSpace(in.AuthorizedBy),
				"updated_by":    firstNonEmpty(in.OperatorID, actor),
				"updated_at":    now,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return model.ErrMemberNotFound
		}
		return s.enqueueMemberUpdated(tx, opID, member.ID, member.BranchID, []string{"face_id"})
	})
	if err != nil {
		return nil, err
	}
	return s.getMember(ctx, branchID, member.ID)
}

// Delete 软删会员(不删行,只置 deleted_at ⇒ 之后查不到)。
//
// ⚠️ 端点表(方案 §6.2)没有列删除端点 ⇒ 本方法只提供业务能力 + 测试覆盖,
//
//	暂不暴露 HTTP 路由。
func (s *Service) Delete(ctx context.Context, branchID, memberID, operatorID string) error {
	now := s.now()
	res := s.db.WithContext(ctx).
		Model(&model.Member{}).
		Where("id = ? AND branch_id = ?", strings.TrimSpace(memberID), strings.TrimSpace(branchID)).
		Updates(map[string]any{
			"deleted_at": now,
			"updated_by": firstNonEmpty(operatorID, actor),
			"updated_at": now,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return model.ErrMemberNotFound
	}
	return nil
}
