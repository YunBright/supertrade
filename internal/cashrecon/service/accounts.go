// Package service / accounts.go —— 账户台账（FR，计划 §10.1）。
//
// 三种账户：
//
//	cash     现金（**内置一行**，出纳不能建也不能停用）
//	bank     银行账户（开户行 + 账号后四位）
//	channel  结算渠道（微信 / 支付宝 / 美团 / 抖音 …，account_key = 渠道编码）
//
// # 为什么现金账户由系统内置
//
// 现金是"钱箱"，每个门店必然有且只有一个；让出纳手建就会造出
// 两个现金账户（各录一半期初），而期末快照按 account_key 分组 ⇒ 两条现金行、
// 长短款对不上。⇒ 由 EnsureCashAccount 在首次访问时自动补齐。
package service

import (
	"context"
	"fmt"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/YunBright/supertrade/internal/cashrecon/model"
)

const (
	// cashAccountKey 是内置现金账户的键。
	cashAccountKey = "cash"
	// cashAccountName 是内置现金账户的显示名。
	cashAccountName = "现金"
	// defaultCurrency 是本期唯一支持的币种（外币折算不在本期范围）。
	defaultCurrency = "CNY"
)

// validAccountType 判断账户类型是否合法（现金只能由系统内置）。
func validAccountType(t string) bool {
	switch t {
	case model.AccountTypeCash, model.AccountTypeBank, model.AccountTypeChannel:
		return true
	}
	return false
}

// EnsureCashAccount 确保门店的现金账户存在（幂等）。
func (s *Service) EnsureCashAccount(ctx context.Context, branchID string) (model.ReconAccount, error) {
	if branchID == "" {
		return model.ReconAccount{}, model.ErrInvalidInput
	}
	row, ok, err := s.getAccount(ctx, branchID, model.AccountTypeCash, cashAccountKey)
	if err != nil {
		return model.ReconAccount{}, err
	}
	if ok {
		return row, nil
	}
	now := s.now()
	row = model.ReconAccount{
		ID:          newID(),
		BranchID:    branchID,
		AccountType: model.AccountTypeCash,
		AccountKey:  cashAccountKey,
		Name:        cashAccountName,
		Currency:    defaultCurrency,
		IsActive:    true,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	err = s.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "branch_id"}, {Name: "account_type"}, {Name: "account_key"}},
			DoNothing: true,
		}).Create(&row).Error
	if err != nil {
		return model.ReconAccount{}, err
	}
	// 并发下可能被别的请求先插进去 ⇒ 以库里那行为准。
	got, ok, err := s.getAccount(ctx, branchID, model.AccountTypeCash, cashAccountKey)
	if err != nil {
		return model.ReconAccount{}, err
	}
	if !ok {
		return model.ReconAccount{}, fmt.Errorf("cash-recon: 现金账户初始化失败")
	}
	return got, nil
}

// getAccount 按 (type, key) 取账户。
func (s *Service) getAccount(ctx context.Context, branchID, accountType, accountKey string) (model.ReconAccount, bool, error) {
	var row model.ReconAccount
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND account_type = ? AND account_key = ?", branchID, accountType, accountKey).
		First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return model.ReconAccount{}, false, nil
	}
	if err != nil {
		return model.ReconAccount{}, false, err
	}
	return row, true, nil
}

// ListAccounts 返回门店账户台账（默认只返回启用的）。
func (s *Service) ListAccounts(ctx context.Context, branchID string, includeInactive bool) ([]model.ReconAccount, error) {
	if branchID == "" {
		return nil, model.ErrInvalidInput
	}
	if _, err := s.EnsureCashAccount(ctx, branchID); err != nil {
		return nil, err
	}
	q := s.db.WithContext(ctx).Where("branch_id = ?", branchID)
	if !includeInactive {
		q = q.Where("is_active = ?", true)
	}
	var rows []model.ReconAccount
	if err := q.Order("sort_order, account_type, account_key").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// CreateAccountInput 是新增账户的入参。
type CreateAccountInput struct {
	BranchID    string
	AccountType string
	AccountKey  string
	Name        string
	Currency    string
	SortOrder   int
	Note        string
	Actor       string
}

// CreateAccount 新增银行 / 渠道账户（现金账户只能由系统内置）。
func (s *Service) CreateAccount(ctx context.Context, in CreateAccountInput) (model.ReconAccount, error) {
	in.BranchID = strings.TrimSpace(in.BranchID)
	in.AccountType = strings.TrimSpace(in.AccountType)
	in.AccountKey = strings.TrimSpace(in.AccountKey)
	in.Name = strings.TrimSpace(in.Name)
	in.Currency = strings.TrimSpace(in.Currency)
	if in.BranchID == "" || in.AccountKey == "" {
		return model.ReconAccount{}, model.ErrInvalidInput
	}
	if !validAccountType(in.AccountType) {
		return model.ReconAccount{}, fmt.Errorf("%w: account_type 必须是 cash/bank/channel", model.ErrInvalidInput)
	}
	if in.AccountType == model.AccountTypeCash {
		// 现金账户由 EnsureCashAccount 内置，手工创建只会造出第二个钱箱。
		return model.ReconAccount{}, fmt.Errorf("%w: 现金账户由系统内置,不能手工创建", model.ErrInvalidInput)
	}
	if len(in.AccountKey) > 64 || len(in.Name) > 128 {
		return model.ReconAccount{}, fmt.Errorf("%w: account_key/name 过长", model.ErrInvalidInput)
	}
	if in.Currency == "" {
		in.Currency = defaultCurrency
	}
	if in.Currency != defaultCurrency {
		return model.ReconAccount{}, fmt.Errorf("%w: 本期只支持 CNY 账户", model.ErrInvalidInput)
	}
	if in.Name == "" {
		in.Name = in.AccountKey
	}

	if _, ok, err := s.getAccount(ctx, in.BranchID, in.AccountType, in.AccountKey); err != nil {
		return model.ReconAccount{}, err
	} else if ok {
		return model.ReconAccount{}, fmt.Errorf("%w: 账户 %s/%s 已存在", model.ErrAlreadyExists, in.AccountType, in.AccountKey)
	}

	now := s.now()
	row := model.ReconAccount{
		ID:          newID(),
		BranchID:    in.BranchID,
		AccountType: in.AccountType,
		AccountKey:  in.AccountKey,
		Name:        in.Name,
		Currency:    in.Currency,
		IsActive:    true,
		SortOrder:   in.SortOrder,
		Note:        strings.TrimSpace(in.Note),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return model.ReconAccount{}, err
	}
	_ = s.writeAudit(ctx, model.ReconAuditLog{
		BranchID:   row.BranchID,
		ObjectType: "account",
		ObjectID:   row.ID,
		Action:     "create",
		AfterJSON:  fmt.Sprintf(`{"account_type":%q,"account_key":%q,"name":%q}`, row.AccountType, row.AccountKey, row.Name),
		Actor:      in.Actor,
		CreatedAt:  now,
	})
	return row, nil
}

// UpdateAccountInput 是账户变更的入参（指针 = 未提供）。
type UpdateAccountInput struct {
	BranchID  string
	ID        string
	Name      *string
	IsActive  *bool
	SortOrder *int
	Note      *string
	Actor     string
}

// UpdateAccount 修改账户名称 / 启停 / 排序 / 备注。
//
// ⚠️ 现金账户**不允许停用**：停用后它不再参与期初校验，期末快照会缺掉
// 现金行 —— 而现金恰恰是短款最常发生的账户。
func (s *Service) UpdateAccount(ctx context.Context, in UpdateAccountInput) (model.ReconAccount, error) {
	in.BranchID = strings.TrimSpace(in.BranchID)
	in.ID = strings.TrimSpace(in.ID)
	if in.BranchID == "" || in.ID == "" {
		return model.ReconAccount{}, model.ErrInvalidInput
	}
	var row model.ReconAccount
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND id = ?", in.BranchID, in.ID).
		First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return model.ReconAccount{}, model.ErrNotFound
	}
	if err != nil {
		return model.ReconAccount{}, err
	}
	if row.AccountType == model.AccountTypeCash && in.IsActive != nil && !*in.IsActive {
		return model.ReconAccount{}, fmt.Errorf("%w: 现金账户不允许停用", model.ErrInvalidInput)
	}

	updates := map[string]any{"updated_at": s.now()}
	if in.Name != nil {
		if n := strings.TrimSpace(*in.Name); n != "" {
			if len(n) > 128 {
				return model.ReconAccount{}, fmt.Errorf("%w: name 过长", model.ErrInvalidInput)
			}
			updates["name"] = n
			row.Name = n
		}
	}
	if in.IsActive != nil {
		updates["is_active"] = *in.IsActive
		row.IsActive = *in.IsActive
	}
	if in.SortOrder != nil {
		updates["sort_order"] = *in.SortOrder
		row.SortOrder = *in.SortOrder
	}
	if in.Note != nil {
		if n := strings.TrimSpace(*in.Note); len(n) > 512 {
			return model.ReconAccount{}, fmt.Errorf("%w: note 过长", model.ErrInvalidInput)
		} else {
			updates["note"] = n
			row.Note = n
		}
	}
	if err := s.db.WithContext(ctx).Model(&model.ReconAccount{}).
		Where("branch_id = ? AND id = ?", in.BranchID, in.ID).
		Updates(updates).Error; err != nil {
		return model.ReconAccount{}, err
	}
	_ = s.writeAudit(ctx, model.ReconAuditLog{
		BranchID:   row.BranchID,
		ObjectType: "account",
		ObjectID:   row.ID,
		Action:     "update",
		AfterJSON:  fmt.Sprintf(`{"name":%q,"is_active":%t,"sort_order":%d}`, row.Name, row.IsActive, row.SortOrder),
		Actor:      in.Actor,
		CreatedAt:  s.now(),
	})
	return row, nil
}
