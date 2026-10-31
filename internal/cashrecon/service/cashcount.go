// Package service / cashcount.go —— 现金实盘与长短款（FR-04）。
//
// # 口径
//
//	variance = counted_cash - expected_cash
//	variance > 0 ⇒ 长款(over)
//	variance < 0 ⇒ 短款(short)
//
// 只有 |variance| 超过容差才落长短款台账；容差内的差异仍会记录在实盘行上
// （出纳能看到"差了 2 元但在容差内"），只是不生成台账。
//
// # 应收现金(expected_cash)为什么不默认 0
//
//	没填 expected_cash 时由当日收银流水的 cash 净额派生 —— 派生失败(cube 不可用)
//	一律 503,不静默给 0。同一天已有**其它收银员**的实盘时,当日现金流水无法
//	按人拆分 ⇒ 400 要求显式填写,绝不把"不知道"算成 0。
//
// # 唯一键
//
//	ux_cash_count(branch,biz_date,cashier) —— 同一人同一天只录一次;
//	重复录入 409,避免两行实盘各自算一次长短款。
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

	"github.com/YunBright/supertrade/internal/cashrecon/model"
)

// CashCountInput 是实盘录入的入参。
type CashCountInput struct {
	BranchID string
	BizDate  time.Time
	// CashierID 是收银员标识（auth user id / 工号）。
	CashierID string
	// CountedCash 是实盘现金（必填，0 也是合法实盘）。
	CountedCash *decimal.Decimal
	// ExpectedCash 未提供 ⇒ 由当日收银流水派生（见文件头）。
	ExpectedCash *decimal.Decimal
	// Tolerance 未提供 ⇒ 取门店规则 cash_variance_tolerance_yuan。
	Tolerance  *decimal.Decimal
	ReasonCode string
	Actor      string
}

// CashCountResult 是实盘录入的产物（实盘行 + 可能生成的长短款台账）。
type CashCountResult struct {
	Count    model.ReconCashCount     `json:"count"`
	Variance *model.ReconCashVariance `json:"variance"`
}

// RecordCashCount 录入某门店某日某收银员的现金实盘，必要时生成长短款台账。
func (s *Service) RecordCashCount(ctx context.Context, in CashCountInput) (CashCountResult, error) {
	in.BranchID = strings.TrimSpace(in.BranchID)
	in.CashierID = strings.TrimSpace(in.CashierID)
	in.ReasonCode = strings.TrimSpace(in.ReasonCode)
	in.Actor = strings.TrimSpace(in.Actor)
	if in.BranchID == "" || in.CashierID == "" || in.Actor == "" || in.BizDate.IsZero() {
		return CashCountResult{}, model.ErrInvalidInput
	}
	if in.CountedCash == nil || in.ExpectedCash != nil && in.ExpectedCash.IsNegative() {
		return CashCountResult{}, model.ErrInvalidInput
	}
	if len(in.CashierID) > 32 || len(in.ReasonCode) > 32 {
		return CashCountResult{}, fmt.Errorf("%w: cashier_id/reason_code 过长", model.ErrInvalidInput)
	}
	if in.CountedCash.IsNegative() {
		return CashCountResult{}, fmt.Errorf("%w: counted_cash 不能为负", model.ErrInvalidInput)
	}
	if in.Tolerance != nil && in.Tolerance.IsNegative() {
		return CashCountResult{}, fmt.Errorf("%w: tolerance 不能为负", model.ErrInvalidInput)
	}

	day := dayOnly(in.BizDate)
	if err := s.assertNotLocked(ctx, in.BranchID, day, day); err != nil {
		return CashCountResult{}, err
	}

	// 重复录入直接 409：两行实盘会各自算一次长短款。
	var exists int64
	if err := s.db.WithContext(ctx).Model(&model.ReconCashCount{}).
		Where("branch_id = ? AND biz_date = ? AND cashier_id = ?", in.BranchID, day, in.CashierID).
		Count(&exists).Error; err != nil {
		return CashCountResult{}, err
	}
	if exists > 0 {
		return CashCountResult{}, fmt.Errorf("%w: %s %s 已录入实盘", model.ErrAlreadyExists, day.Format("2006-01-02"), in.CashierID)
	}

	expected, err := s.expectedCashOf(ctx, in, day)
	if err != nil {
		return CashCountResult{}, err
	}
	tolerance, err := s.cashToleranceOf(ctx, in)
	if err != nil {
		return CashCountResult{}, err
	}

	variance := (*in.CountedCash).Sub(expected)
	now := s.now()
	count := model.ReconCashCount{
		ID:           newID(),
		BranchID:     in.BranchID,
		BizDate:      day,
		CashierID:    in.CashierID,
		ExpectedCash: expected,
		CountedCash:  *in.CountedCash,
		Variance:     variance,
		Tolerance:    tolerance,
		Status:       model.CashCountStatusOpen,
		ReasonCode:   in.ReasonCode,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	var ledger *model.ReconCashVariance
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&count).Error; err != nil {
			return fmt.Errorf("create recon_cash_counts: %w", err)
		}
		if variance.Abs().Cmp(tolerance) > 0 {
			kind := model.VarianceKindOver
			if variance.IsNegative() {
				kind = model.VarianceKindShort
			}
			row := model.ReconCashVariance{
				ID:         newID(),
				BranchID:   in.BranchID,
				BizDate:    day,
				CashierID:  in.CashierID,
				Kind:       kind,
				Amount:     variance.Abs(),
				ReasonCode: in.ReasonCode,
				Status:     model.CashVarianceStatusOpen,
				CreatedAt:  now,
			}
			if err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{
					{Name: "branch_id"}, {Name: "biz_date"}, {Name: "cashier_id"}, {Name: "kind"},
				},
				DoUpdates: clause.AssignmentColumns([]string{"amount", "reason_code", "status"}),
			}).Create(&row).Error; err != nil {
				return fmt.Errorf("create recon_cash_variances: %w", err)
			}
			ledger = &row
		}
		return s.writeAuditTx(ctx, tx, model.ReconAuditLog{
			BranchID:   in.BranchID,
			ObjectType: "cash_count",
			ObjectID:   count.ID,
			Action:     "create",
			AfterJSON: fmt.Sprintf(`{"biz_date":%q,"cashier_id":%q,"expected_cash":%q,"counted_cash":%q,"variance":%q,"tolerance":%q}`,
				day.Format("2006-01-02"), in.CashierID, expected.String(), in.CountedCash.String(), variance.String(), tolerance.String()),
			Reason:    in.ReasonCode,
			Actor:     in.Actor,
			CreatedAt: now,
		})
	})
	if err != nil {
		return CashCountResult{}, err
	}
	return CashCountResult{Count: count, Variance: ledger}, nil
}

// expectedCashOf 解析应收现金：显式值优先，否则由当日收银流水派生。
func (s *Service) expectedCashOf(ctx context.Context, in CashCountInput, day time.Time) (decimal.Decimal, error) {
	if in.ExpectedCash != nil {
		return *in.ExpectedCash, nil
	}
	var others int64
	if err := s.db.WithContext(ctx).Model(&model.ReconCashCount{}).
		Where("branch_id = ? AND biz_date = ? AND cashier_id <> ?", in.BranchID, day, in.CashierID).
		Count(&others).Error; err != nil {
		return decimal.Zero, err
	}
	if others > 0 {
		// 当日现金流水无法按收银员拆分 ⇒ 必须显式填，不能拿整天的现金净额当个人的应收。
		return decimal.Zero, fmt.Errorf("%w: 同日已有其它收银员实盘,必须显式填写 expected_cash", model.ErrInvalidInput)
	}
	rows, _, err := s.CashDayRows(ctx, in.BranchID, day, day)
	if err != nil {
		return decimal.Zero, err
	}
	expected := decimal.Zero
	for _, r := range rows {
		if r.Method == model.MethodCash {
			expected = expected.Add(r.AmountNet)
		}
	}
	return expected, nil
}

// cashToleranceOf 解析容差：显式值优先，否则取门店规则。
func (s *Service) cashToleranceOf(ctx context.Context, in CashCountInput) (decimal.Decimal, error) {
	if in.Tolerance != nil {
		return *in.Tolerance, nil
	}
	rules, err := s.LoadRules(ctx, in.BranchID)
	if err != nil {
		return decimal.Zero, err
	}
	return rules.CashVarianceTolerance, nil
}

// ConfirmCashCount 收银主管确认实盘（有长短款时必须填原因）。
func (s *Service) ConfirmCashCount(ctx context.Context, branchID, id, actor, reasonCode string) (model.ReconCashCount, error) {
	branchID = strings.TrimSpace(branchID)
	id = strings.TrimSpace(id)
	actor = strings.TrimSpace(actor)
	reasonCode = strings.TrimSpace(reasonCode)
	if branchID == "" || id == "" || actor == "" {
		return model.ReconCashCount{}, model.ErrInvalidInput
	}

	now := s.now()
	var out model.ReconCashCount
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row model.ReconCashCount
		err := tx.Where("branch_id = ? AND id = ?", branchID, id).First(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return model.ErrNotFound
		}
		if err != nil {
			return err
		}
		if row.Status == model.CashCountStatusConfirmed {
			return fmt.Errorf("%w: 该实盘已确认", model.ErrInvalidInput)
		}
		if err := s.assertNotLockedTx(tx, branchID, row.BizDate, row.BizDate); err != nil {
			return err
		}

		var varCount int64
		if err := tx.Model(&model.ReconCashVariance{}).
			Where("branch_id = ? AND biz_date = ? AND cashier_id = ?", branchID, row.BizDate, row.CashierID).
			Count(&varCount).Error; err != nil {
			return err
		}
		if varCount > 0 && reasonCode == "" {
			return fmt.Errorf("%w: 存在长短款,确认时必须填写原因", model.ErrInvalidInput)
		}
		if len(reasonCode) > 32 {
			return fmt.Errorf("%w: reason_code 过长", model.ErrInvalidInput)
		}

		if err := tx.Model(&model.ReconCashCount{}).
			Where("branch_id = ? AND id = ?", branchID, id).
			Updates(map[string]any{
				"status":      model.CashCountStatusConfirmed,
				"confirm_by":  actor,
				"confirm_at":  now,
				"reason_code": reasonCode,
				"updated_at":  now,
			}).Error; err != nil {
			return err
		}
		if varCount > 0 {
			if err := tx.Model(&model.ReconCashVariance{}).
				Where("branch_id = ? AND biz_date = ? AND cashier_id = ?", branchID, row.BizDate, row.CashierID).
				Updates(map[string]any{
					"status":      model.CashVarianceStatusConfirmed,
					"reason_code": reasonCode,
				}).Error; err != nil {
				return err
			}
		}

		row.Status = model.CashCountStatusConfirmed
		row.ConfirmBy = actor
		row.ConfirmAt = &now
		row.ReasonCode = reasonCode
		row.UpdatedAt = now
		out = row

		return s.writeAuditTx(ctx, tx, model.ReconAuditLog{
			BranchID:   branchID,
			ObjectType: "cash_count",
			ObjectID:   id,
			Action:     "confirm",
			BeforeJSON: fmt.Sprintf(`{"status":%q}`, model.CashCountStatusOpen),
			AfterJSON:  fmt.Sprintf(`{"status":%q,"reason_code":%q,"variance":%q}`, model.CashCountStatusConfirmed, reasonCode, row.Variance.String()),
			Reason:     reasonCode,
			Actor:      actor,
			CreatedAt:  now,
		})
	})
	if err != nil {
		return model.ReconCashCount{}, err
	}
	return out, nil
}

// assertNotLockedTx 是 assertNotLocked 的事务内版本。
func (s *Service) assertNotLockedTx(tx *gorm.DB, branchID string, from, to time.Time) error {
	var n int64
	err := tx.Model(&model.ReconPeriodLock{}).
		Where("branch_id = ? AND biz_date_from <= ? AND biz_date_to >= ?", branchID, dayOnly(to), dayOnly(from)).
		Count(&n).Error
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w: %s ~ %s", model.ErrPeriodFrozen, from.Format("2006-01-02"), to.Format("2006-01-02"))
	}
	return nil
}

// ListCashCounts 返回区间内的实盘列表。
func (s *Service) ListCashCounts(ctx context.Context, branchID string, from, to time.Time, cashierID string, limit, offset int) ([]model.ReconCashCount, int64, error) {
	branchID = strings.TrimSpace(branchID)
	cashierID = strings.TrimSpace(cashierID)
	if err := s.validateRange(branchID, from, to); err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	db := s.db.WithContext(ctx).Model(&model.ReconCashCount{}).
		Where("branch_id = ? AND biz_date >= ? AND biz_date <= ?", branchID, dayOnly(from), dayOnly(to))
	if cashierID != "" {
		db = db.Where("cashier_id = ?", cashierID)
	}
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	rows := make([]model.ReconCashCount, 0, limit)
	if err := db.Order("biz_date DESC, cashier_id").Limit(limit).Offset(offset).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// CashierVarianceSummary 是某收银员的区间长短款汇总。
type CashierVarianceSummary struct {
	CashierID   string          `json:"cashier_id"`
	OverAmount  decimal.Decimal `json:"over_amount"`
	ShortAmount decimal.Decimal `json:"short_amount"`
	// NetAmount = 长款 - 短款（正数 = 净长款）。
	NetAmount decimal.Decimal `json:"net_amount"`
	Count     int             `json:"count"`
	OpenCount int             `json:"open_count"`
}

// CashVarianceSummary 按收银员汇总区间内的长短款（FR-04「可按收银员汇总」）。
func (s *Service) CashVarianceSummary(ctx context.Context, branchID string, from, to time.Time, cashierID string) ([]CashierVarianceSummary, error) {
	branchID = strings.TrimSpace(branchID)
	cashierID = strings.TrimSpace(cashierID)
	if err := s.validateRange(branchID, from, to); err != nil {
		return nil, err
	}
	q := s.db.WithContext(ctx).Model(&model.ReconCashVariance{}).
		Where("branch_id = ? AND biz_date >= ? AND biz_date <= ?", branchID, dayOnly(from), dayOnly(to))
	if cashierID != "" {
		q = q.Where("cashier_id = ?", cashierID)
	}
	var rows []model.ReconCashVariance
	if err := q.Order("cashier_id, biz_date").Find(&rows).Error; err != nil {
		return nil, err
	}

	byCashier := map[string]*CashierVarianceSummary{}
	for _, r := range rows {
		item, ok := byCashier[r.CashierID]
		if !ok {
			item = &CashierVarianceSummary{CashierID: r.CashierID}
			byCashier[r.CashierID] = item
		}
		switch r.Kind {
		case model.VarianceKindOver:
			item.OverAmount = item.OverAmount.Add(r.Amount)
		case model.VarianceKindShort:
			item.ShortAmount = item.ShortAmount.Add(r.Amount)
		}
		item.Count++
		if r.Status == model.CashVarianceStatusOpen {
			item.OpenCount++
		}
	}
	res := make([]CashierVarianceSummary, 0, len(byCashier))
	ids := make([]string, 0, len(byCashier))
	for id := range byCashier {
		ids = append(ids, id)
	}
	sortStrings(ids)
	for _, id := range ids {
		item := byCashier[id]
		item.NetAmount = item.OverAmount.Sub(item.ShortAmount)
		res = append(res, *item)
	}
	return res, nil
}

// AssertCashCountExists 校验某日现金账户有实盘（complete 前置，S4 调用）。
//
// 缺实盘 ⇒ ErrCashCountMissing(409 cash_count_missing)，**不默认有实盘**。
func (s *Service) AssertCashCountExists(ctx context.Context, branchID string, day time.Time) error {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" || day.IsZero() {
		return model.ErrInvalidInput
	}
	var n int64
	if err := s.db.WithContext(ctx).Model(&model.ReconCashCount{}).
		Where("branch_id = ? AND biz_date = ?", branchID, dayOnly(day)).
		Count(&n).Error; err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", model.ErrCashCountMissing, day.Format("2006-01-02"))
	}
	return nil
}

// sortStrings 是稳定插入排序（避免为一个小切片引入额外依赖差异）。
func sortStrings(v []string) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}
