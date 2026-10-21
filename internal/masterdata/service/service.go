// Package service —— master-data 域的业务逻辑(stores / employees / shifts)。
//
// # 三条业务规则
//
//  1. **门店档案是 auth 的镜像**,不是它的副本真源 ——
//     `pos_enabled` 以 auth.branches 为准,这里只作缓存。
//  2. **收银员的 ID 是 auth user_id**,工号是业务标识。
//     ⇒ 改工号不会丢档案;而查权限必须用 user_id(去 auth 查 per-branch 权限矩阵)。
//  3. **一个收银员同时只能有一个未交班的班次** ——
//     两个未交班的班次会让"本班销售额"算成两倍,长短款立刻离谱。
package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/YunBright/supertrade/internal/masterdata/model"
)

// actor 是系统写入的 created_by / updated_by。
const actor = "system"

// StoreService 是门店业务档案服务。
type StoreService struct {
	db  *gorm.DB
	now func() time.Time
}

// NewStoreService 构造 StoreService。
func NewStoreService(db *gorm.DB) *StoreService {
	return &StoreService{db: db, now: func() time.Time { return time.Now() }}
}

// SetClock 注入时钟(测试用)。
func (s *StoreService) SetClock(fn func() time.Time) {
	if fn != nil {
		s.now = fn
	}
}

// StoreInput 是门店档案的写入输入。
type StoreInput struct {
	BranchID           string
	Code               string
	Name               string
	PosEnabled         bool
	StocktakeCycleDays int
	MemberEnabled      bool
	CounterNos         []string
	Remark             string
	UpdatedBy          string
}

// Upsert 幂等写入门店档案。
//
// ⚠️ upsert 的更新列**不含** created_by / created_at(那是审计线索)。
func (s *StoreService) Upsert(ctx context.Context, in StoreInput) (*model.Store, error) {
	branchID := strings.TrimSpace(in.BranchID)
	if branchID == "" {
		return nil, model.ErrInvalidInput
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: 门店名不能为空", model.ErrInvalidInput)
	}
	cycle := in.StocktakeCycleDays
	if cycle <= 0 {
		cycle = 7
	}
	now := s.now()
	row := model.Store{
		BranchID:           branchID,
		Code:               strings.TrimSpace(in.Code),
		Name:               name,
		PosEnabled:         in.PosEnabled,
		StocktakeCycleDays: cycle,
		MemberEnabled:      in.MemberEnabled,
		CounterNos:         strings.Join(in.CounterNos, ","),
		Remark:             in.Remark,
		UpdatedBy:          firstNonEmpty(in.UpdatedBy, actor),
		UpdatedAt:          now,
	}
	err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "branch_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"code", "name", "pos_enabled", "stocktake_cycle_days",
			"member_enabled", "counter_nos", "remark", "updated_by", "updated_at",
		}),
	}).Create(&row).Error
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, branchID)
}

// Get 取门店档案。
func (s *StoreService) Get(ctx context.Context, branchID string) (*model.Store, error) {
	var row model.Store
	err := s.db.WithContext(ctx).Where("branch_id = ?", branchID).First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, model.ErrStoreNotFound
		}
		return nil, err
	}
	return &row, nil
}

// List 列出全部门店档案。
func (s *StoreService) List(ctx context.Context) ([]model.Store, error) {
	var rows []model.Store
	err := s.db.WithContext(ctx).Order("branch_id asc").Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// EmployeeService 是收银员业务档案服务。
type EmployeeService struct {
	db  *gorm.DB
	now func() time.Time
}

// NewEmployeeService 构造 EmployeeService。
func NewEmployeeService(db *gorm.DB) *EmployeeService {
	return &EmployeeService{db: db, now: func() time.Time { return time.Now() }}
}

// SetClock 注入时钟(测试用)。
func (s *EmployeeService) SetClock(fn func() time.Time) {
	if fn != nil {
		s.now = fn
	}
}

// EmployeeInput 是收银员档案的写入输入。
type EmployeeInput struct {
	// AuthUserID 是 auth.users.id(不是工号)。
	AuthUserID string
	BranchID   string
	// EmployeeCode 是工号(门店内唯一)。
	EmployeeCode string
	Name         string
	// Role 是 cashier / supervisor / manager;空 = cashier。
	Role string
	// Status 是 active / inactive;空 = active。
	Status string
	// DiscountRate 是折扣权限(0.95 = 95 折);零值 = 不限(1.00)。
	DiscountRate decimal.Decimal
	CounterNo    string
	Remark       string
	UpdatedBy    string
}

// Upsert 幂等写入收银员档案。
func (s *EmployeeService) Upsert(ctx context.Context, in EmployeeInput) (*model.Employee, error) {
	userID := strings.TrimSpace(in.AuthUserID)
	branchID := strings.TrimSpace(in.BranchID)
	code := strings.TrimSpace(in.EmployeeCode)
	if userID == "" || branchID == "" || code == "" {
		return nil, fmt.Errorf("%w: auth_user_id / branch_id / employee_code 必填", model.ErrInvalidInput)
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: 姓名不能为空", model.ErrInvalidInput)
	}
	role := strings.TrimSpace(in.Role)
	if role == "" {
		role = RoleCashier
	}
	switch role {
	case RoleCashier, RoleSupervisor, RoleManager:
	default:
		return nil, fmt.Errorf("%w: role=%q 非法(只能是 %s / %s / %s)",
			model.ErrInvalidInput, role, RoleCashier, RoleSupervisor, RoleManager)
	}
	status := strings.TrimSpace(in.Status)
	if status == "" {
		status = EmployeeStatusActive
	}
	switch status {
	case EmployeeStatusActive, EmployeeStatusInactive:
	default:
		return nil, fmt.Errorf("%w: status=%q 非法(只能是 %s / %s)",
			model.ErrInvalidInput, status, EmployeeStatusActive, EmployeeStatusInactive)
	}
	// ⚠️ **同店同工号不能重复** ——
	//
	//	DB 层没有唯一约束(`idx_employee_branch_code` 实际是 employee_code
	//	**单列非唯一**索引,branch_id 根本没挂上去),
	//	而收银员是**门店内唯一**的 ⇒ 在 service 层拦。
	//
	//	⚠️ 同一个 auth user 在**别的门店**用同一工号是允许的
	//	(那是另一个人事档案,`employees` 复合主键含 branch_id)。
	var dup int64
	dupErr := s.db.WithContext(ctx).
		Model(&model.Employee{}).
		Where("branch_id = ? AND employee_code = ? AND id <> ?", branchID, code, userID).
		Count(&dup).Error
	if dupErr != nil {
		return nil, dupErr
	}
	if dup > 0 {
		return nil, fmt.Errorf("%w: 该门店已有工号 %s 的收银员", model.ErrEmployeeCodeConflict, code)
	}
	rate := in.DiscountRate
	if rate.IsZero() {
		rate = decimal.NewFromInt(1)
	}
	now := s.now()
	row := model.Employee{
		ID:           userID,
		BranchID:     branchID,
		EmployeeCode: code,
		Name:         name,
		Role:         role,
		Status:       status,
		DiscountRate: rate,
		CounterNo:    strings.TrimSpace(in.CounterNo),
		Remark:       in.Remark,
		CreatedBy:    firstNonEmpty(in.UpdatedBy, actor),
		UpdatedBy:    firstNonEmpty(in.UpdatedBy, actor),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}, {Name: "branch_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"employee_code", "name", "role", "status", "discount_rate",
			"counter_no", "remark", "updated_by", "updated_at",
		}),
	}).Create(&row).Error
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, userID, branchID)
}

// Get 取收银员档案。
func (s *EmployeeService) Get(ctx context.Context, id, branchID string) (*model.Employee, error) {
	var row model.Employee
	err := s.db.WithContext(ctx).
		Where("id = ? AND branch_id = ?", id, branchID).
		First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, model.ErrEmployeeNotFound
		}
		return nil, err
	}
	return &row, nil
}

// GetByCode 按工号取收银员档案(门店内)。
func (s *EmployeeService) GetByCode(ctx context.Context, branchID, code string) (*model.Employee, error) {
	var row model.Employee
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND employee_code = ?", branchID, strings.TrimSpace(code)).
		First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, model.ErrEmployeeCodeNotFound
		}
		return nil, err
	}
	return &row, nil
}

// List 列出某门店的收银员(空 branchID = 全部)。
func (s *EmployeeService) List(ctx context.Context, branchID string) ([]model.Employee, error) {
	q := s.db.WithContext(ctx)
	if branchID != "" {
		q = q.Where("branch_id = ?", branchID)
	}
	var rows []model.Employee
	if err := q.Order("branch_id asc, employee_code asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// ShiftService 是班次服务。
type ShiftService struct {
	db  *gorm.DB
	now func() time.Time
}

// NewShiftService 构造 ShiftService。
func NewShiftService(db *gorm.DB) *ShiftService {
	return &ShiftService{db: db, now: func() time.Time { return time.Now() }}
}

// SetClock 注入时钟(测试用)。
func (s *ShiftService) SetClock(fn func() time.Time) {
	if fn != nil {
		s.now = fn
	}
}

// OpenShiftInput 是开班的输入。
type OpenShiftInput struct {
	BranchID   string
	EmployeeID string
	CounterNo  string
	// OpeningAmountYuan 是备用金。
	OpeningAmountYuan decimal.Decimal
	Remark            string
	UpdatedBy         string
}

// Open 开班。
//
// ⚠️ **一个收银员同时只能有一个未交班的班次** ——
//
//	两个未交班的班次会让"本班销售额"算成两倍,长短款立刻离谱。
//	⇒ 这里显式检查并拒绝,而不是靠调用方自觉。
func (s *ShiftService) Open(ctx context.Context, in OpenShiftInput) (*model.Shift, error) {
	branchID := strings.TrimSpace(in.BranchID)
	employeeID := strings.TrimSpace(in.EmployeeID)
	if branchID == "" || employeeID == "" {
		return nil, fmt.Errorf("%w: branch_id / employee_id 必填", model.ErrInvalidInput)
	}
	now := s.now()
	open, err := s.hasOpenShift(ctx, branchID, employeeID)
	if err != nil {
		return nil, err
	}
	if open {
		return nil, model.ErrShiftAlreadyOpen
	}
	row := model.Shift{
		ID:                NewShiftID(now),
		BranchID:          branchID,
		EmployeeID:        employeeID,
		CounterNo:         strings.TrimSpace(in.CounterNo),
		OpenedAt:          now,
		Status:            ShiftStatusOpen,
		OpeningAmountYuan: in.OpeningAmountYuan,
		UpdatedBy:         firstNonEmpty(in.UpdatedBy, actor),
		CreatedBy:         firstNonEmpty(in.UpdatedBy, actor),
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return nil, err
	}
	return &row, nil
}

// Close 交班。
//
// ⚠️ 交班是**幂等友好的**:已交班的班次再交一次会报 ErrShiftClosed,
//
//	而不是把 closing_amount 改掉 —— 交班后再改金额 = 篡改账。
func (s *ShiftService) Close(ctx context.Context, id string, closing decimal.Decimal, remark, updatedBy string) (*model.Shift, error) {
	var row model.Shift
	err := s.db.WithContext(ctx).Where("id = ?", id).First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, model.ErrShiftNotFound
		}
		return nil, err
	}
	if row.Status == ShiftStatusClosed {
		return nil, model.ErrShiftClosed
	}
	now := s.now()
	row.Status = ShiftStatusClosed
	row.ClosedAt = &now
	row.ClosingAmountYuan = closing
	if remark != "" {
		row.Remark = remark
	}
	row.UpdatedBy = firstNonEmpty(updatedBy, actor)
	row.UpdatedAt = now
	if err := s.db.WithContext(ctx).Save(&row).Error; err != nil {
		return nil, err
	}
	return &row, nil
}

// SetExpectedAmount 写班次的应收合计(由 pos 在成交时累计)。
func (s *ShiftService) SetExpectedAmount(ctx context.Context, id string, amount decimal.Decimal, updatedBy string) error {
	return s.db.WithContext(ctx).Model(&model.Shift{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"expected_amount_yuan": amount,
			"updated_by":           firstNonEmpty(updatedBy, actor),
			"updated_at":           s.now(),
		}).Error
}

// Get 取班次。
func (s *ShiftService) Get(ctx context.Context, id string) (*model.Shift, error) {
	var row model.Shift
	err := s.db.WithContext(ctx).Where("id = ?", id).First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, model.ErrShiftNotFound
		}
		return nil, err
	}
	return &row, nil
}

// ListOpen 列出某门店未交班的班次(空 branchID = 全部)。
func (s *ShiftService) ListOpen(ctx context.Context, branchID string) ([]model.Shift, error) {
	q := s.db.WithContext(ctx).Where("status = ?", ShiftStatusOpen)
	if branchID != "" {
		q = q.Where("branch_id = ?", branchID)
	}
	var rows []model.Shift
	if err := q.Order("opened_at desc").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// hasOpenShift 判断该收银员是否有未交班的班次。
func (s *ShiftService) hasOpenShift(ctx context.Context, branchID, employeeID string) (bool, error) {
	var n int64
	err := s.db.WithContext(ctx).Model(&model.Shift{}).
		Where("branch_id = ? AND employee_id = ? AND status = ?",
			branchID, employeeID, ShiftStatusOpen).
		Count(&n).Error
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ---- 常量与工具 ----

// 收银员角色。
const (
	RoleCashier    = "cashier"
	RoleSupervisor = "supervisor"
	RoleManager    = "manager"
)

// 收银员状态。
//
// ⚠️ **只有两个取值** —— 停用就改 status(不物理删),
// 历史班次/流水仍能对上这个人。
const (
	EmployeeStatusActive   = "active"
	EmployeeStatusInactive = "inactive"
)

// 班次状态。
const (
	ShiftStatusOpen   = "open"
	ShiftStatusClosed = "closed"
)

// NewShiftID 生成班次 id:SH<yyyymmddhhmmss><6 位随机十六进制>。
//
// ⚠️ **必须带随机后缀**,不能只用时间戳:
//
//	门店 10 有多个收银台,**两个收银员在同一秒开班是常态**
//	⇒ 只用时间戳会撞主键(UNIQUE constraint failed on shifts.id)。
//	而"主键撞了"发生在开班那一刻,现场看到的是"开班按钮点了报错",
//	而真因(两个收银员同时开班)完全看不出来。
//
// ⇒ 6 位随机(3 字节)⇒ 碰撞概率约 1/1670 万,单店规模下可忽略。
func NewShiftID(t time.Time) string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 读不到随机源(几乎不可能)⇒ 用纳秒兜底。
		return "SH" + t.Format("20060102150405.000000000")
	}
	return "SH" + t.Format("20060102150405") + hex.EncodeToString(b[:])
}

// firstNonEmpty 取第一个非空字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
