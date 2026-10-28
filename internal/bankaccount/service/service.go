// Package service —— 供应商收款银行账号的读写逻辑。
//
// # 与其它服务的**故意不同**：账号数据永远查本地 PG
//
//	catalog   → 有源查 cube、无源查本地 pg
//	本服务     → **不管有无 cube 源，一律查本地 pg**
//
// 理由：收款账号是本系统自己产生的业务事实（"我们付钱到哪个账户"），
// 不是外部 ERP 的镜像 ⇒ 必须落在本域 PG，不能跟着 cube 走。
//
// # supplier_id 的命名空间由**唯一判据**决定
//
//	有 cube 源   → cube 源的 supplier id
//	无 cube 源   → pg 本地表 suppliers 的 supplier id
//
// 判据实现见 internal/cubesource（catalog / inventory / member 共用同一份）。
// 本服务**不自己**判断"有没有 cube 源"——
// 判据散落在多个服务里就会出现"同一门店在不同服务里 id 空间不一样"，
// 而排查时每个服务看起来都"有道理"（这正是 cubesource 包存在的理由）。
//
// # 但本服务**不查 cube 拿供应商名**
//
// ⚠️ 这是一个刻意的取舍，写在这里以免后人"顺手优化"：
//
//	要拿到"这个 supplier_id 叫什么名字"，有源门店得问 cube。
//	但本服务的读路径契约是"一律查本地 pg" ⇒ 若为了取名去问 cube，
//	就等于把"账号数据本地"这个前提又破掉了（且 cube 不可用时账号列表整个挂）。
//
//	⇒ 账号表**冗余存一份 supplier_name 快照**（建/改账号时由调用方一起给）。
//	名字是"建账号那一刻的显示值"，不是外键 —— 与
//	settlement-auditor 的 approval_settlements.SupplierName 同款处理
//	（那边也是"读时补上、不落库"的反面：这里故意落库，因为本服务不 JOIN）。
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/bankaccount/model"
)

// 业务错误 sentinel(handler 层映 4xx / 5xx)。
var (
	// ErrNotFound:按 (id, branch_id) 未命中。
	ErrNotFound = errors.New("bankaccount: 收款账号不存在")
	// ErrAlreadyExists:同 (branch_id, supplier_id, account_no) 已存在。
	ErrAlreadyExists = errors.New("bankaccount: 该供应商已存在相同账号")
	// ErrInvalidInput:必填字段缺失或非法。
	ErrInvalidInput = errors.New("bankaccount: 入参非法")
)

// maxRemarkLen 是备注最大长度(与 DB 列宽一致)。
const maxRemarkLen = 512

// Service 是收款账号的业务逻辑。
type Service struct {
	db  *gorm.DB
	now func() time.Time
}

// New 构造 Service。
func New(db *gorm.DB) *Service {
	return &Service{db: db, now: func() time.Time { return time.Now() }}
}

// SetClock 注入时间(测试用)。
func (s *Service) SetClock(fn func() time.Time) {
	if fn != nil {
		s.now = fn
	}
}

// AccountInput 是新建/修改收款账号的入参。
//
// ⚠️ **SupplierName 是调用方给的快照**，不是服务端 JOIN 出来的
// (见包注释"本服务不查 cube 拿供应商名")。
type AccountInput struct {
	// SupplierID 是所属供应商 id。命名空间按"有无 cube 源"决定。
	SupplierID string
	// SupplierName 是供应商名快照(展示用)。
	SupplierName string
	// BankName 是开户行。
	BankName string
	// AccountNo 是银行账号。
	AccountNo string
	// AccountName 是户名。
	AccountName string
	// IsDefault 是否为该供应商的默认收款账号。
	IsDefault bool
	// Remark 是备注。
	Remark string
}

// validate 校验入参。
func (in AccountInput) validate() error {
	if strings.TrimSpace(in.SupplierID) == "" {
		return fmt.Errorf("%w: supplier_id 必填", ErrInvalidInput)
	}
	if strings.TrimSpace(in.BankName) == "" {
		return fmt.Errorf("%w: bank_name(开户行)必填", ErrInvalidInput)
	}
	if strings.TrimSpace(in.AccountNo) == "" {
		return fmt.Errorf("%w: account_no(账号)必填", ErrInvalidInput)
	}
	if strings.TrimSpace(in.AccountName) == "" {
		return fmt.Errorf("%w: account_name(户名)必填", ErrInvalidInput)
	}
	if len(in.Remark) > maxRemarkLen {
		return fmt.Errorf("%w: remark 超长(>%d)", ErrInvalidInput, maxRemarkLen)
	}
	return nil
}

// ListFilter 是账号列表的筛选条件。
type ListFilter struct {
	// BranchID 是门店 id(必填)——账号是门店级数据。
	BranchID string
	// SupplierID 非空时只返回该供应商的账号。
	SupplierID string
	// OnlyDefault 为 true 时只返回默认账号。
	OnlyDefault bool
}

// List 返回某门店的收款账号列表。
//
// ⚠️ **一律查本地 pg**，不查 cube(见包注释)。
//
// 排序：默认账号优先，其次按开户行 + 账号 ⇒ 前端"下拉选一个付款账号"
// 的默认项自然排在最前。
func (s *Service) List(ctx context.Context, f ListFilter) ([]model.BankAccount, error) {
	if strings.TrimSpace(f.BranchID) == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	q := s.db.WithContext(ctx).Model(&model.BankAccount{}).
		Where("branch_id = ? AND deleted_at IS NULL", f.BranchID)
	if id := strings.TrimSpace(f.SupplierID); id != "" {
		q = q.Where("supplier_id = ?", id)
	}
	if f.OnlyDefault {
		q = q.Where("is_default = ?", true)
	}
	var rows []model.BankAccount
	if err := q.Order("is_default DESC, bank_name ASC, account_no ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list bank accounts: %w", err)
	}
	return rows, nil
}

// Get 按 id 取单个账号(限门店)。
func (s *Service) Get(ctx context.Context, branchID, id string) (*model.BankAccount, error) {
	if strings.TrimSpace(branchID) == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("%w: id 必填", ErrInvalidInput)
	}
	var row model.BankAccount
	err := s.db.WithContext(ctx).
		Where("id = ? AND branch_id = ? AND deleted_at IS NULL", id, branchID).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get bank account: %w", err)
	}
	return &row, nil
}

// Create 新建收款账号。
//
// ⚠️ **默认账号的唯一性靠事务保证**：同一 (branch_id, supplier_id) 下
// 最多一条 is_default=true ⇒ 设默认时先把该供应商其它账号的 default 置 false。
// 不这样做就会出现"两个默认账号"，前端下拉选中哪一个全靠运气。
func (s *Service) Create(ctx context.Context, branchID, operator string, in AccountInput) (*model.BankAccount, error) {
	if strings.TrimSpace(branchID) == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	now := s.now()
	row := &model.BankAccount{
		ID:           uuid.NewString(),
		BranchID:     branchID,
		SupplierID:   strings.TrimSpace(in.SupplierID),
		SupplierName: strings.TrimSpace(in.SupplierName),
		BankName:     strings.TrimSpace(in.BankName),
		AccountNo:    strings.TrimSpace(in.AccountNo),
		AccountName:  strings.TrimSpace(in.AccountName),
		IsDefault:    in.IsDefault,
		Remark:       strings.TrimSpace(in.Remark),
		CreatedBy:    operator,
		UpdatedBy:    operator,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 同 (branch, supplier, account_no) 已存在 ⇒ 拒。
		var dup int64
		if err := tx.Model(&model.BankAccount{}).
			Where("branch_id = ? AND supplier_id = ? AND account_no = ? AND deleted_at IS NULL",
				branchID, row.SupplierID, row.AccountNo).
			Count(&dup).Error; err != nil {
			return err
		}
		if dup > 0 {
			return ErrAlreadyExists
		}
		if row.IsDefault {
			if err := clearOtherDefaults(tx, branchID, row.SupplierID, ""); err != nil {
				return err
			}
		}
		return tx.Create(row).Error
	})
	if err != nil {
		if errors.Is(err, ErrAlreadyExists) {
			return nil, err
		}
		return nil, fmt.Errorf("create bank account: %w", err)
	}
	return row, nil
}

// Update 修改收款账号。
//
// ⚠️ **改 supplier_id 是允许的** —— 账号可能挂错供应商，店长要能改回来。
// 但改了之后要重新检查"同 (branch, supplier, account_no) 唯一"。
func (s *Service) Update(ctx context.Context, branchID, id, operator string, in AccountInput) (*model.BankAccount, error) {
	if strings.TrimSpace(branchID) == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("%w: id 必填", ErrInvalidInput)
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	now := s.now()
	var out model.BankAccount
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row model.BankAccount
		if err := tx.Where("id = ? AND branch_id = ? AND deleted_at IS NULL", id, branchID).
			First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		supplierID := strings.TrimSpace(in.SupplierID)
		accountNo := strings.TrimSpace(in.AccountNo)
		// 唯一性自检(排除自身)。
		var dup int64
		if err := tx.Model(&model.BankAccount{}).
			Where("branch_id = ? AND supplier_id = ? AND account_no = ? AND id <> ? AND deleted_at IS NULL",
				branchID, supplierID, accountNo, id).
			Count(&dup).Error; err != nil {
			return err
		}
		if dup > 0 {
			return ErrAlreadyExists
		}
		row.SupplierID = supplierID
		row.SupplierName = strings.TrimSpace(in.SupplierName)
		row.BankName = strings.TrimSpace(in.BankName)
		row.AccountNo = accountNo
		row.AccountName = strings.TrimSpace(in.AccountName)
		row.IsDefault = in.IsDefault
		row.Remark = strings.TrimSpace(in.Remark)
		row.UpdatedBy = operator
		row.UpdatedAt = now
		if row.IsDefault {
			if err := clearOtherDefaults(tx, branchID, row.SupplierID, id); err != nil {
				return err
			}
		}
		if err := tx.Save(&row).Error; err != nil {
			return err
		}
		out = row
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrAlreadyExists) {
			return nil, err
		}
		return nil, fmt.Errorf("update bank account: %w", err)
	}
	return &out, nil
}

// Delete 软删收款账号。
//
// ⚠️ **软删而非物理删**：账号是资金往来凭证的一部分，
// 历史付款审批单可能还引用着它 ⇒ 物理删会让那些单子对不上账。
//
// ⚠️ 软删后**同一账号号不能重建**（唯一索引含 account_no）——
// 故这里做"复活"：若同 (branch, supplier, account_no) 存在已软删的行，
// 把它复活并覆盖字段，而不是报 ErrAlreadyExists。
func (s *Service) Delete(ctx context.Context, branchID, id, operator string) error {
	if strings.TrimSpace(branchID) == "" {
		return fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: id 必填", ErrInvalidInput)
	}
	now := s.now()
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row model.BankAccount
		if err := tx.Where("id = ? AND branch_id = ? AND deleted_at IS NULL", id, branchID).
			First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if err := tx.Model(&model.BankAccount{}).Where("id = ?", id).
			Updates(map[string]any{"deleted_at": now, "updated_by": operator, "updated_at": now}).Error; err != nil {
			return err
		}
		// 删掉的若是默认账号 ⇒ 该供应商就没有默认账号了。
		// ⇒ 把剩下的里 id 最小的一条提为默认，保证"始终有一个默认"这个不变量。
		if row.IsDefault {
			return promoteFallbackDefault(tx, branchID, row.SupplierID)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return err
		}
		return fmt.Errorf("delete bank account: %w", err)
	}
	return nil
}

// SetDefault 把某账号设为该供应商的默认收款账号(其余置否)。
//
// 单独一个端点而不是"PUT 时带 is_default"：
// 前端在列表页点"设为默认"是高频动作，不必回传整份表单。
func (s *Service) SetDefault(ctx context.Context, branchID, id, operator string) error {
	if strings.TrimSpace(branchID) == "" {
		return fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: id 必填", ErrInvalidInput)
	}
	now := s.now()
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row model.BankAccount
		if err := tx.Where("id = ? AND branch_id = ? AND deleted_at IS NULL", id, branchID).
			First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if err := clearOtherDefaults(tx, branchID, row.SupplierID, id); err != nil {
			return err
		}
		if err := tx.Model(&model.BankAccount{}).Where("id = ?", id).
			Updates(map[string]any{"is_default": true, "updated_by": operator, "updated_at": now}).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return err
		}
		return fmt.Errorf("set default bank account: %w", err)
	}
	return nil
}

// clearOtherDefaults 把某供应商下除 keepID 之外的账号全部置为非默认。
//
// keepID 为空 ⇒ 该供应商下**全部**账号置非默认(新建默认账号时用)。
func clearOtherDefaults(tx *gorm.DB, branchID, supplierID, keepID string) error {
	q := tx.Model(&model.BankAccount{}).
		Where("branch_id = ? AND supplier_id = ? AND deleted_at IS NULL", branchID, supplierID)
	if keepID != "" {
		q = q.Where("id <> ?", keepID)
	}
	return q.Updates(map[string]any{"is_default": false}).Error
}

// promoteFallbackDefault 在删掉默认账号后,把剩余账号里 id 最小的一条提为默认。
//
// ⚠️ 选"id 最小"而不是"最近更新的"：id 最小 = 建得最早 = 通常是主账号，
// 语义稳定(每次删默认都提同一个)，不会因为谁碰过它就换来换去。
func promoteFallbackDefault(tx *gorm.DB, branchID, supplierID string) error {
	var fallback model.BankAccount
	err := tx.Where("branch_id = ? AND supplier_id = ? AND deleted_at IS NULL", branchID, supplierID).
		Order("id ASC").First(&fallback).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// 该供应商已无账号 ⇒ 没有默认可言。
		return nil
	}
	if err != nil {
		return err
	}
	return tx.Model(&model.BankAccount{}).Where("id = ?", fallback.ID).
		Updates(map[string]any{"is_default": true}).Error
}
