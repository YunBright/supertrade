// Package service / opening.go —— 期初余额维护（计划 §10.1）。
//
// # 勾稽式
//
//	期末 = 期初 + 流入 - 流出 - 手续费
//
// 其中**期初是外部事实**（银行对账单余额 / 钱箱现金），系统算不出来。
// ⇒ 期初独立成表、append-only：
//
//	manual  首次建账
//	carry   complete 时自动结转下一期（S2 实现）
//	adjust  人工调整（修正历史**不 UPDATE**）
//	void    作废（保留行 + 留痕，回退到上一条有效记录）
//
// # 铁律：缺期初不默认 0
//
// 把"没维护"显示成 0 元会让资金日报的余额静默算错且没有任何告警。
// ⇒ 冻结前置校验一律 409 `opening_balance_missing`，前端据此跳"期初建账"页。
//
// # 解析规则（写死，全局共用）
//
//	SELECT ... WHERE branch_id=? AND account_type=? AND account_key=?
//	  AND effective_date <= <period_start> AND voided_at IS NULL
//	ORDER BY effective_date DESC, created_at DESC LIMIT 1
package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cashrecon/model"
)

// ResolveOpening 解析某账户在 periodStart 生效的期初（无 ⇒ ok=false）。
func (s *Service) ResolveOpening(
	ctx context.Context, branchID, accountType, accountKey string, periodStart time.Time,
) (model.ReconOpeningBalance, bool, error) {
	if branchID == "" || accountType == "" || accountKey == "" {
		return model.ReconOpeningBalance{}, false, model.ErrInvalidInput
	}
	var row model.ReconOpeningBalance
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND account_type = ? AND account_key = ? AND effective_date <= ? AND voided_at IS NULL",
			branchID, accountType, accountKey, dayOnly(periodStart)).
		Order("effective_date DESC, created_at DESC").
		First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return model.ReconOpeningBalance{}, false, nil
	}
	if err != nil {
		return model.ReconOpeningBalance{}, false, err
	}
	return row, true, nil
}

// AccountRef 是账户的最小标识（缺失清单 / 错误 body 用）。
type AccountRef struct {
	AccountType string `json:"account_type"`
	AccountKey  string `json:"account_key"`
	Name        string `json:"name"`
}

// OpeningItemView 是勾稽视图里的一行。
type OpeningItemView struct {
	AccountRef
	IsActive bool `json:"is_active"`
	// Opening 为空 ⇒ 该账户在 date 前没有有效期初（missing=true）。
	Opening           *decimal.Decimal `json:"opening"`
	OpeningID         string           `json:"opening_id,omitempty"`
	Source            string           `json:"source,omitempty"`
	EffectiveDate     *time.Time       `json:"effective_date,omitempty"`
	PreviousClosing   *decimal.Decimal `json:"previous_closing"`
	PreviousPeriodEnd *time.Time       `json:"previous_period_end,omitempty"`
	// Gap = opening - previous_closing（任一侧缺失 ⇒ null）。
	//
	// ⚠️ 不为 0 只是**警告**，不阻断：银行侧可能有未入账调整。
	Gap        *decimal.Decimal `json:"gap"`
	GapWarning string           `json:"gap_warning,omitempty"`
	Missing    bool             `json:"missing"`
}

// OpeningView 是 `GET /opening-balances?date=` 的响应。
type OpeningView struct {
	BranchID string            `json:"branch_id"`
	Date     string            `json:"date"`
	Items    []OpeningItemView `json:"items"`
	Missing  int               `json:"missing"`
}

// OpeningView 返回 date 起算的"本期期初 vs 上期期末"勾稽视图。
func (s *Service) OpeningView(ctx context.Context, branchID string, date time.Time) (OpeningView, error) {
	if branchID == "" || date.IsZero() {
		return OpeningView{}, model.ErrInvalidInput
	}
	accounts, err := s.ListAccounts(ctx, branchID, true)
	if err != nil {
		return OpeningView{}, err
	}
	day := dayOnly(date)
	out := OpeningView{BranchID: branchID, Date: day.Format("2006-01-02"), Items: make([]OpeningItemView, 0, len(accounts))}
	for _, a := range accounts {
		item, err := s.openingItemView(ctx, a, day)
		if err != nil {
			return OpeningView{}, err
		}
		if item.Missing && a.IsActive {
			out.Missing++
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// openingItemView 组装单账户的勾稽行。
func (s *Service) openingItemView(ctx context.Context, a model.ReconAccount, day time.Time) (OpeningItemView, error) {
	item := OpeningItemView{
		AccountRef: AccountRef{AccountType: a.AccountType, AccountKey: a.AccountKey, Name: a.Name},
		IsActive:   a.IsActive,
	}
	row, ok, err := s.ResolveOpening(ctx, a.BranchID, a.AccountType, a.AccountKey, day)
	if err != nil {
		return OpeningItemView{}, err
	}
	if ok {
		amount := row.Amount
		eff := dayOnly(row.EffectiveDate)
		item.Opening = &amount
		item.OpeningID = row.ID
		item.Source = row.Source
		item.EffectiveDate = &eff
	} else {
		item.Missing = true
	}

	var prev model.ReconPeriodSnapshot
	err = s.db.WithContext(ctx).
		Where("branch_id = ? AND account_type = ? AND account_key = ? AND period_end < ?",
			a.BranchID, a.AccountType, a.AccountKey, day).
		Order("period_end DESC").
		First(&prev).Error
	switch {
	case err == gorm.ErrRecordNotFound:
	case err != nil:
		return OpeningItemView{}, err
	default:
		closing := prev.Closing
		pe := dayOnly(prev.PeriodEnd)
		item.PreviousClosing = &closing
		item.PreviousPeriodEnd = &pe
	}

	if item.Opening != nil && item.PreviousClosing != nil {
		gap := item.Opening.Sub(*item.PreviousClosing)
		item.Gap = &gap
		if !gap.IsZero() {
			if item.Source == model.OpeningSourceAdjust {
				item.GapWarning = "adjust"
			} else {
				item.GapWarning = "opening_gap"
			}
		}
	}
	return item, nil
}

// MissingOpenings 列出 periodStart 前没有有效期初的**启用**账户。
func (s *Service) MissingOpenings(ctx context.Context, branchID string, periodStart time.Time) ([]AccountRef, error) {
	if branchID == "" || periodStart.IsZero() {
		return nil, model.ErrInvalidInput
	}
	accounts, err := s.ListAccounts(ctx, branchID, false)
	if err != nil {
		return nil, err
	}
	missing := make([]AccountRef, 0)
	for _, a := range accounts {
		_, ok, err := s.ResolveOpening(ctx, a.BranchID, a.AccountType, a.AccountKey, periodStart)
		if err != nil {
			return nil, err
		}
		if !ok {
			missing = append(missing, AccountRef{AccountType: a.AccountType, AccountKey: a.AccountKey, Name: a.Name})
		}
	}
	return missing, nil
}

// AssertOpeningBalancesComplete 是 freeze / complete 的前置校验（S2 调用）。
//
// 返回缺失账户清单；有缺失 ⇒ 同时返回 ErrOpeningBalanceMissing（body 带清单）。
func (s *Service) AssertOpeningBalancesComplete(ctx context.Context, branchID string, periodStart time.Time) ([]AccountRef, error) {
	missing, err := s.MissingOpenings(ctx, branchID, periodStart)
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		return missing, fmt.Errorf("%w: 缺少 %d 个账户的期初", model.ErrOpeningBalanceMissing, len(missing))
	}
	return nil, nil
}

// OpeningItemInput 是建账入参里的一行。
type OpeningItemInput struct {
	AccountType string          `json:"account_type"`
	AccountKey  string          `json:"account_key"`
	Amount      decimal.Decimal `json:"amount"`
}

// CreateOpeningsInput 是 `POST /opening-balances` 的入参。
type CreateOpeningsInput struct {
	BranchID      string
	EffectiveDate time.Time
	Items         []OpeningItemInput
	Note          string
	Actor         string
}

// CreateOpenings 批量建账 / 调整期初（一个事务，**不部分成功**）。
//
// 语义：
//   - 任一**启用**账户缺项 ⇒ 400 opening_items_incomplete（列出缺失）。
//   - 该账户在 effective_date 之前已有有效期初，或当天已有记录 ⇒ 记 source=adjust，
//     否则记 source=manual（首次建账）。
//   - 当天的 adjust 若已存在 ⇒ 先作废旧的再写新的（唯一索引 ux_opening 允许
//     manual/carry/adjust 同日各一条，重复 adjust 只能靠作废腾位）。
func (s *Service) CreateOpenings(ctx context.Context, in CreateOpeningsInput) ([]model.ReconOpeningBalance, error) {
	in.BranchID = strings.TrimSpace(in.BranchID)
	in.Actor = strings.TrimSpace(in.Actor)
	in.Note = strings.TrimSpace(in.Note)
	if in.BranchID == "" || in.Actor == "" || in.EffectiveDate.IsZero() || len(in.Items) == 0 {
		return nil, model.ErrInvalidInput
	}
	if len(in.Note) > 512 {
		return nil, fmt.Errorf("%w: note 过长", model.ErrInvalidInput)
	}
	eff := dayOnly(in.EffectiveDate)

	accounts, err := s.ListAccounts(ctx, in.BranchID, false) // 只取启用账户
	if err != nil {
		return nil, err
	}
	active := make(map[string]model.ReconAccount, len(accounts))
	for _, a := range accounts {
		active[a.AccountType+"|"+a.AccountKey] = a
	}

	// 入参规整：去空白、去重、未知账户直接拒绝。
	items := make([]OpeningItemInput, 0, len(in.Items))
	seen := map[string]bool{}
	for _, it := range in.Items {
		it.AccountType = strings.TrimSpace(it.AccountType)
		it.AccountKey = strings.TrimSpace(it.AccountKey)
		k := it.AccountType + "|" + it.AccountKey
		if seen[k] {
			return nil, fmt.Errorf("%w: 账户 %s/%s 重复", model.ErrInvalidInput, it.AccountType, it.AccountKey)
		}
		seen[k] = true
		if _, ok := active[k]; !ok {
			return nil, fmt.Errorf("%w: 账户 %s/%s 不存在或已停用", model.ErrInvalidInput, it.AccountType, it.AccountKey)
		}
		items = append(items, it)
	}

	missing := make([]string, 0)
	for k, a := range active {
		if !seen[k] {
			missing = append(missing, a.AccountType+"/"+a.AccountKey)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: 缺少账户 %s", model.ErrOpeningItemsIncomplete, strings.Join(missing, ", "))
	}

	if err := s.assertNotLocked(ctx, in.BranchID, eff, eff); err != nil {
		return nil, err
	}

	now := s.now()
	var created []model.ReconOpeningBalance
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, it := range items {
			row, err := s.writeOpeningTx(ctx, tx, in.BranchID, it, eff, in.Actor, in.Note, now)
			if err != nil {
				return err
			}
			created = append(created, row)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// writeOpeningTx 在事务内写一条期初（必要时先作废同日 adjust）。
func (s *Service) writeOpeningTx(
	ctx context.Context, tx *gorm.DB, branchID string, it OpeningItemInput,
	eff time.Time, actor, note string, now time.Time,
) (model.ReconOpeningBalance, error) {
	source := model.OpeningSourceManual

	// 同账户同日是否已有有效记录？
	var sameDay []model.ReconOpeningBalance
	if err := tx.WithContext(ctx).
		Where("branch_id = ? AND account_type = ? AND account_key = ? AND effective_date = ? AND voided_at IS NULL",
			branchID, it.AccountType, it.AccountKey, eff).
		Find(&sameDay).Error; err != nil {
		return model.ReconOpeningBalance{}, err
	}
	for _, prev := range sameDay {
		source = model.OpeningSourceAdjust
		if prev.Source != model.OpeningSourceAdjust {
			continue
		}
		// 同日已有 adjust ⇒ 作废它（append-only 的"修正"路径，保留行）。
		if err := tx.WithContext(ctx).Model(&model.ReconOpeningBalance{}).
			Where("id = ?", prev.ID).
			Updates(map[string]any{
				"voided_at":   now,
				"voided_by":   actor,
				"void_reason": "被同一天的新调整覆盖",
			}).Error; err != nil {
			return model.ReconOpeningBalance{}, err
		}
	}

	// 早于当天的有效期初 ⇒ 这也是调整（不是首次建账）。
	if source == model.OpeningSourceManual {
		var cnt int64
		if err := tx.WithContext(ctx).Model(&model.ReconOpeningBalance{}).
			Where("branch_id = ? AND account_type = ? AND account_key = ? AND effective_date < ? AND voided_at IS NULL",
				branchID, it.AccountType, it.AccountKey, eff).
			Count(&cnt).Error; err != nil {
			return model.ReconOpeningBalance{}, err
		}
		if cnt > 0 {
			source = model.OpeningSourceAdjust
		}
	}

	row := model.ReconOpeningBalance{
		ID:            newID(),
		BranchID:      branchID,
		AccountType:   it.AccountType,
		AccountKey:    it.AccountKey,
		EffectiveDate: eff,
		Amount:        it.Amount,
		Source:        source,
		Note:          note,
		CreatedBy:     actor,
		CreatedAt:     now,
	}
	if err := tx.WithContext(ctx).Create(&row).Error; err != nil {
		return model.ReconOpeningBalance{}, err
	}
	_ = s.writeAuditTx(ctx, tx, model.ReconAuditLog{
		BranchID:   branchID,
		ObjectType: "opening_balance",
		ObjectID:   row.ID,
		Action:     "create",
		AfterJSON:  fmt.Sprintf(`{"account_type":%q,"account_key":%q,"amount":%q,"source":%q,"effective_date":%q}`, row.AccountType, row.AccountKey, row.Amount.String(), row.Source, eff.Format("2006-01-02")),
		Reason:     note,
		Actor:      actor,
		CreatedAt:  now,
	})
	return row, nil
}

// writeAuditTx 是 writeAudit 的事务内版本（审计失败只告警，不阻断业务）。
func (s *Service) writeAuditTx(ctx context.Context, tx *gorm.DB, entry model.ReconAuditLog) error {
	if entry.ID == "" {
		entry.ID = newID()
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = s.now()
	}
	if err := tx.WithContext(ctx).Create(&entry).Error; err != nil {
		s.cfg.Logger.Warn("cash-recon: 审计日志写入失败", "object_type", entry.ObjectType,
			"object_id", entry.ObjectID, "action", entry.Action, "err", err)
	}
	return nil
}

// VoidOpening 作废一条期初记录（保留行 + 留痕）。
func (s *Service) VoidOpening(ctx context.Context, branchID, id, actor, reason string) (model.ReconOpeningBalance, error) {
	branchID = strings.TrimSpace(branchID)
	id = strings.TrimSpace(id)
	actor = strings.TrimSpace(actor)
	reason = strings.TrimSpace(reason)
	if branchID == "" || id == "" || actor == "" {
		return model.ReconOpeningBalance{}, model.ErrInvalidInput
	}
	var row model.ReconOpeningBalance
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND id = ?", branchID, id).
		First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return model.ReconOpeningBalance{}, model.ErrNotFound
	}
	if err != nil {
		return model.ReconOpeningBalance{}, err
	}
	if row.VoidedAt != nil {
		return model.ReconOpeningBalance{}, fmt.Errorf("%w: 该期初记录已作废", model.ErrInvalidInput)
	}

	now := s.now()
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.ReconOpeningBalance{}).
			Where("branch_id = ? AND id = ?", branchID, id).
			Updates(map[string]any{
				"voided_at":   now,
				"voided_by":   actor,
				"void_reason": reason,
			}).Error; err != nil {
			return err
		}
		return s.writeAuditTx(ctx, tx, model.ReconAuditLog{
			BranchID:   branchID,
			ObjectType: "opening_balance",
			ObjectID:   id,
			Action:     "void",
			BeforeJSON: fmt.Sprintf(`{"amount":%q,"source":%q}`, row.Amount.String(), row.Source),
			Reason:     reason,
			Actor:      actor,
			CreatedAt:  now,
		})
	})
	if err != nil {
		return model.ReconOpeningBalance{}, err
	}
	row.VoidedAt = &now
	row.VoidedBy = actor
	row.VoidReason = reason
	return row, nil
}
