// Package service / period.go —— 审核结算：冻结（freeze）与期末快照（complete）（计划 §10）。
//
// # freeze 与 complete 的区别
//
//	freeze   = 审核计算：重新查收银流水并**落快照表**，再锁住区间 —— 之后任何
//	           能改流水金额的写操作（导入 / 收款 / 实盘 / 付款）都 409 period_frozen。
//	complete = 期末快照：按「期末 = 期初 + 流入 - 流出 - 手续费」逐账户结账，
//	           现金账户期末取**实盘**，并把期末自动结转为下一期期初（source=carry）。
//
// # 两条铁律
//
//  1. 缺期初 / 缺实盘一律 409，**不默认 0**（默认 0 会让资金日报静默算错）。
//  2. `completed` 不可变：重入 complete 是幂等返回既有快照，不重写金额；
//     后续调整只能新建 `kind=adjust` 批次。
//
// # 为什么冻结要写 recon_cash_flows
//
// 平时收银流水是实时查 cube 的（可能迟到 / 被修正）。审核计算必须把**当时的**
// 结果钉住 —— 否则 complete 时的实时值已与 freeze 时不同，快照和差异就永久对不上。
package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/YunBright/supertrade/internal/cashrecon/model"
)

// OpeningMissingError 携带缺失账户清单（409 body 直接给前端跳"期初建账"页）。
//
// Unwrap 回 model.ErrOpeningBalanceMissing，handler 仍可用 errors.Is 判态。
type OpeningMissingError struct {
	Missing []AccountRef
}

// Error 实现 error。
func (e *OpeningMissingError) Error() string {
	return fmt.Sprintf("%s: 缺少 %d 个账户的期初", model.ErrOpeningBalanceMissing.Error(), len(e.Missing))
}

// Unwrap 让 errors.Is(err, model.ErrOpeningBalanceMissing) 成立。
func (e *OpeningMissingError) Unwrap() error { return model.ErrOpeningBalanceMissing }

// assertOpenings 是 freeze / complete 的期初前置校验（缺 ⇒ 带清单的 409）。
func (s *Service) assertOpenings(ctx context.Context, branchID string, periodStart time.Time) error {
	missing, err := s.MissingOpenings(ctx, branchID, periodStart)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return &OpeningMissingError{Missing: missing}
	}
	return nil
}

// FreezeResult 是 freeze 的回执。
type FreezeResult struct {
	Batch model.ReconBatch `json:"batch"`
	// CashFlowRows 是本次写入（或已存在）的收银流水快照行数。
	CashFlowRows int `json:"cash_flow_rows"`
	// DataSource ∈ cube / local（快照来源，前端据此提示口径）。
	DataSource string `json:"data_source"`
	// AlreadyFrozen 表示本次是幂等重入（未重复写行）。
	AlreadyFrozen bool `json:"already_frozen"`
}

// FreezeBatch 执行审核计算：快照收银流水 + 锁区间 + 批次进入 auditing。
//
// 前置：批次 status=draft；区间未被其它批次锁定；每个启用账户都有期初。
func (s *Service) FreezeBatch(ctx context.Context, branchID, batchID, actor string) (FreezeResult, error) {
	branchID = strings.TrimSpace(branchID)
	batchID = strings.TrimSpace(batchID)
	actor = strings.TrimSpace(actor)
	if branchID == "" || batchID == "" || actor == "" {
		return FreezeResult{}, model.ErrInvalidInput
	}

	batch, err := s.GetBatch(ctx, branchID, batchID)
	if err != nil {
		return FreezeResult{}, err
	}
	switch batch.Status {
	case model.BatchStatusDraft:
		// 继续
	case model.BatchStatusAuditing, model.BatchStatusFrozen:
		// 幂等重入：不重复写快照行，也不重复加锁。
		var n int64
		if err := s.db.WithContext(ctx).Model(&model.ReconCashFlow{}).
			Where("batch_id = ?", batch.ID).Count(&n).Error; err != nil {
			return FreezeResult{}, err
		}
		return FreezeResult{Batch: batch, CashFlowRows: int(n), AlreadyFrozen: true}, nil
	default:
		return FreezeResult{}, fmt.Errorf("%w: 批次状态 %s 不允许冻结", model.ErrInvalidInput, batch.Status)
	}

	from, to := dayOnly(batch.BizDateFrom), dayOnly(batch.BizDateTo)
	if err := s.assertNotLocked(ctx, branchID, from, to); err != nil {
		return FreezeResult{}, err
	}
	if err := s.assertOpenings(ctx, branchID, from); err != nil {
		return FreezeResult{}, err
	}

	// 审核计算必须看到**当前**的收银流水（缓存可能是 5 分钟前的）。
	s.InvalidateCashCache(branchID)
	rules, err := s.LoadRules(ctx, branchID)
	if err != nil {
		return FreezeResult{}, err
	}
	cash, source, err := s.CashFlows(ctx, branchID, from, to, rules)
	if err != nil {
		return FreezeResult{}, err
	}

	now := s.now()
	var rows int
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 事务内再判一次：并发下另一个请求可能刚锁住同一区间。
		if err := s.assertNotLockedTx(tx, branchID, from, to); err != nil {
			return err
		}

		if len(cash) > 0 {
			flows := make([]model.ReconCashFlow, 0, len(cash))
			for _, c := range cash {
				flows = append(flows, model.ReconCashFlow{
					ID:         newID(),
					BatchID:    batch.ID,
					BranchID:   branchID,
					BizDate:    dayOnly(c.BizDate),
					PayWay:     c.PayWay,
					Method:     c.Method,
					AmountNet:  c.AmountNet,
					TxnCount:   c.TxnCount,
					Source:     source,
					SnapshotAt: now,
				})
			}
			res := tx.WithContext(ctx).Clauses(clause.OnConflict{
				Columns: []clause.Column{
					{Name: "batch_id"}, {Name: "biz_date"}, {Name: "pay_way"},
				},
				DoNothing: true,
			}).CreateInBatches(flows, 500)
			if res.Error != nil {
				return res.Error
			}
			rows = int(res.RowsAffected)
		}

		var lockCount int64
		if err := tx.WithContext(ctx).Model(&model.ReconPeriodLock{}).
			Where("batch_id = ?", batch.ID).Count(&lockCount).Error; err != nil {
			return err
		}
		if lockCount == 0 {
			if err := tx.WithContext(ctx).Create(&model.ReconPeriodLock{
				ID:          newID(),
				BranchID:    branchID,
				BizDateFrom: from,
				BizDateTo:   to,
				BatchID:     batch.ID,
				LockedAt:    now,
			}).Error; err != nil {
				return err
			}
		}

		if err := tx.WithContext(ctx).Model(&model.ReconBatch{}).
			Where("id = ? AND status = ?", batch.ID, model.BatchStatusDraft).
			Updates(map[string]any{
				"status":     model.BatchStatusAuditing,
				"frozen_at":  now,
				"updated_at": now,
			}).Error; err != nil {
			return err
		}

		return s.writeAuditTx(ctx, tx, model.ReconAuditLog{
			BranchID:   branchID,
			ObjectType: "recon_batch",
			ObjectID:   batch.ID,
			Action:     "freeze",
			AfterJSON: fmt.Sprintf(`{"status":%q,"from":%q,"to":%q,"cash_flow_rows":%d,"data_source":%q}`,
				model.BatchStatusAuditing, from.Format("2006-01-02"), to.Format("2006-01-02"), rows, source),
			Reason:    "审核计算：快照收银流水并冻结区间",
			Actor:     actor,
			CreatedAt: now,
		})
	})
	if err != nil {
		return FreezeResult{}, err
	}

	batch.Status = model.BatchStatusAuditing
	batch.FrozenAt = &now
	batch.UpdatedAt = now
	return FreezeResult{Batch: batch, CashFlowRows: rows, DataSource: source}, nil
}

// CompleteResult 是 complete 的回执。
type CompleteResult struct {
	Batch model.ReconBatch `json:"batch"`
	// Snapshots 是本期快照行（每个启用账户一行）。
	Snapshots []model.ReconPeriodSnapshot `json:"snapshots"`
	// CarriedOpenings 是自动结转的下一期期初条数。
	CarriedOpenings int `json:"carried_openings"`
	// CashClosing 是现金账户期末（= 实盘）。
	CashClosing decimal.Decimal `json:"cash_closing"`
	// AlreadyCompleted 表示本次是幂等重入（未重写快照）。
	AlreadyCompleted bool `json:"already_completed"`
}

// CompleteBatch 结账：写期末快照 + 自动结转下期期初。
//
// 前置：批次已 freeze（auditing）；每个启用账户都能解析到期初；现金账户有当期实盘。
func (s *Service) CompleteBatch(ctx context.Context, branchID, batchID, actor string) (CompleteResult, error) {
	branchID = strings.TrimSpace(branchID)
	batchID = strings.TrimSpace(batchID)
	actor = strings.TrimSpace(actor)
	if branchID == "" || batchID == "" || actor == "" {
		return CompleteResult{}, model.ErrInvalidInput
	}

	batch, err := s.GetBatch(ctx, branchID, batchID)
	if err != nil {
		return CompleteResult{}, err
	}
	if batch.Status == model.BatchStatusCompleted {
		// 幂等重入：completed 不可变，原样返回既有快照。
		var rows []model.ReconPeriodSnapshot
		if err := s.db.WithContext(ctx).
			Where("branch_id = ? AND source_batch_id = ?", branchID, batch.ID).
			Order("account_type, account_key").Find(&rows).Error; err != nil {
			return CompleteResult{}, err
		}
		cashClosing := decimal.Zero
		for _, r := range rows {
			if r.AccountType == model.AccountTypeCash {
				cashClosing = r.Closing
			}
		}
		return CompleteResult{Batch: batch, Snapshots: rows, CashClosing: cashClosing, AlreadyCompleted: true}, nil
	}
	if batch.Status != model.BatchStatusAuditing && batch.Status != model.BatchStatusFrozen {
		return CompleteResult{}, fmt.Errorf("%w: 批次必须先冻结才能结账", model.ErrInvalidInput)
	}

	from, to := dayOnly(batch.BizDateFrom), dayOnly(batch.BizDateTo)
	if err := s.assertOpenings(ctx, branchID, from); err != nil {
		return CompleteResult{}, err
	}
	// 现金期末取实盘 ⇒ 期末日必须有实盘（不默认 0）。
	if err := s.AssertCashCountExists(ctx, branchID, to); err != nil {
		return CompleteResult{}, err
	}

	accounts, err := s.ListAccounts(ctx, branchID, false)
	if err != nil {
		return CompleteResult{}, err
	}

	// 收银流水（冻结时已钉住）：按标准 method 汇总流入/流出。
	cashByMethod, err := s.flowSumsByMethod(ctx, batch.ID)
	if err != nil {
		return CompleteResult{}, err
	}
	// 银行/渠道到账：按 channel 汇总（account_key = 渠道编码）。
	bankByChannel, err := s.bankSumsByChannel(ctx, branchID, from, to)
	if err != nil {
		return CompleteResult{}, err
	}
	// 其他收款（已确认）计入对应账户流入。
	receiptByMethod, err := s.receiptSumsByMethod(ctx, branchID, from, to)
	if err != nil {
		return CompleteResult{}, err
	}
	countedCash, err := s.countedCashAt(ctx, branchID, to)
	if err != nil {
		return CompleteResult{}, err
	}

	now := s.now()
	// 计算阶段：**事务外**读库（opening / 上期期末 / 凭证明细），
	// 收集成待写列表；事务内只写。否则事务持有连接时再走 s.db 会自锁。
	snapshots := make([]model.ReconPeriodSnapshot, 0, len(accounts))
	carries := make([]model.ReconOpeningBalance, 0, len(accounts))
	for _, a := range accounts {
		opening, ok, err := s.ResolveOpening(ctx, branchID, a.AccountType, a.AccountKey, from)
		if err != nil {
			return CompleteResult{}, err
		}
		if !ok {
			return CompleteResult{}, &OpeningMissingError{Missing: []AccountRef{{
				AccountType: a.AccountType, AccountKey: a.AccountKey, Name: a.Name,
			}}}
		}

		inflow, outflow, fee := decimal.Zero, decimal.Zero, decimal.Zero
		switch a.AccountType {
		case model.AccountTypeCash:
			if sums, ok := cashByMethod[model.MethodCash]; ok {
				inflow = inflow.Add(sums.Inflow)
				outflow = outflow.Add(sums.Outflow)
			}
			inflow = inflow.Add(receiptByMethod[model.MethodCash])
		case model.AccountTypeBank, model.AccountTypeChannel:
			if sums, ok := bankByChannel[a.AccountKey]; ok {
				inflow = inflow.Add(sums.Inflow)
				outflow = outflow.Add(sums.Outflow)
				fee = fee.Add(sums.Fee)
			}
			inflow = inflow.Add(receiptByMethod[a.AccountKey])
		}

		adjustment := decimal.Zero
		prevClosing, hasPrev, err := s.previousClosing(ctx, branchID, a.AccountType, a.AccountKey, from)
		if err != nil {
			return CompleteResult{}, err
		}
		if hasPrev && opening.Source == model.OpeningSourceAdjust {
			// carry 的金额 = 上期期末，差额恒为 0；adjust 才是人工调整额。
			adjustment = opening.Amount.Sub(prevClosing)
		}

		closing := opening.Amount.Add(inflow).Sub(outflow).Sub(fee)
		if a.AccountType == model.AccountTypeCash {
			// 现金期末 = 实盘（长短款由此产生，快照不再二次修正）。
			closing = countedCash
		}

		snapshots = append(snapshots, model.ReconPeriodSnapshot{
			ID:               newID(),
			BranchID:         branchID,
			PeriodStart:      from,
			PeriodEnd:        to,
			AccountType:      a.AccountType,
			AccountKey:       a.AccountKey,
			Opening:          opening.Amount,
			OpeningSource:    opening.Source,
			AdjustmentAmount: adjustment,
			Inflow:           inflow,
			Outflow:          outflow,
			Fee:              fee,
			Closing:          closing,
			SourceBatchID:    batch.ID,
			CreatedAt:        now,
		})
		// 自动结转：下期期初 = 本期期末（effective_date = period_end + 1）。
		carries = append(carries, model.ReconOpeningBalance{
			ID:            newID(),
			BranchID:      branchID,
			AccountType:   a.AccountType,
			AccountKey:    a.AccountKey,
			EffectiveDate: to.AddDate(0, 0, 1),
			Amount:        closing,
			Source:        model.OpeningSourceCarry,
			BatchID:       batch.ID,
			Note:          fmt.Sprintf("期末自动结转（%s 结账）", to.Format("2006-01-02")),
			CreatedBy:     actor,
			CreatedAt:     now,
		})
	}

	carried := 0
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, row := range snapshots {
			snap := row
			res := tx.WithContext(ctx).Clauses(clause.OnConflict{
				Columns: []clause.Column{
					{Name: "branch_id"}, {Name: "period_start"},
					{Name: "account_type"}, {Name: "account_key"},
				},
				DoUpdates: clause.AssignmentColumns([]string{
					"period_end", "opening", "opening_source", "adjustment_amount",
					"inflow", "outflow", "fee", "closing", "source_batch_id",
				}),
			}).Create(&snap)
			if res.Error != nil {
				return res.Error
			}
		}
		for _, carry := range carries {
			c := carry
			res := tx.WithContext(ctx).Clauses(clause.OnConflict{
				Columns: []clause.Column{
					{Name: "branch_id"}, {Name: "account_type"}, {Name: "account_key"},
					{Name: "effective_date"}, {Name: "source"},
				},
				DoNothing: true,
			}).Create(&c)
			if res.Error != nil {
				return res.Error
			}
			carried += int(res.RowsAffected)
		}

		if err := tx.WithContext(ctx).Model(&model.ReconBatch{}).
			Where("id = ?", batch.ID).
			Updates(map[string]any{
				"status":       model.BatchStatusCompleted,
				"completed_at": now,
				"updated_at":   now,
			}).Error; err != nil {
			return err
		}

		return s.writeAuditTx(ctx, tx, model.ReconAuditLog{
			BranchID:   branchID,
			ObjectType: "recon_batch",
			ObjectID:   batch.ID,
			Action:     "complete",
			AfterJSON: fmt.Sprintf(`{"status":%q,"from":%q,"to":%q,"accounts":%d,"carried_openings":%d}`,
				model.BatchStatusCompleted, from.Format("2006-01-02"), to.Format("2006-01-02"), len(snapshots), carried),
			Reason:    "期末快照并自动结转下期期初",
			Actor:     actor,
			CreatedAt: now,
		})
	})
	if err != nil {
		return CompleteResult{}, err
	}

	batch.Status = model.BatchStatusCompleted
	batch.CompletedAt = &now
	batch.UpdatedAt = now
	// 回读快照：upsert 可能命中既有行，返回库里的真实值才是"期末快照"本身。
	var stored []model.ReconPeriodSnapshot
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND source_batch_id = ? AND period_start = ?", branchID, batch.ID, from).
		Order("account_type, account_key").Find(&stored).Error; err != nil {
		return CompleteResult{}, err
	}
	cashClosing := decimal.Zero
	for _, r := range stored {
		if r.AccountType == model.AccountTypeCash {
			cashClosing = r.Closing
		}
	}
	return CompleteResult{Batch: batch, Snapshots: stored, CarriedOpenings: carried, CashClosing: cashClosing}, nil
}

// flowSum 是某维度的流入 / 流出汇总（流出为正数绝对值）。
type flowSum struct {
	Inflow  decimal.Decimal
	Outflow decimal.Decimal
	Fee     decimal.Decimal
}

// flowSumsByMethod 汇总冻结快照里的收银流水（按标准 method）。
func (s *Service) flowSumsByMethod(ctx context.Context, batchID string) (map[string]flowSum, error) {
	var rows []struct {
		Method  string          `gorm:"column:method"`
		Inflow  decimal.Decimal `gorm:"column:inflow"`
		Outflow decimal.Decimal `gorm:"column:outflow"`
	}
	err := s.db.WithContext(ctx).Model(&model.ReconCashFlow{}).
		Select(`method,
			SUM(CASE WHEN amount_net > 0 THEN amount_net ELSE 0 END) AS inflow,
			SUM(CASE WHEN amount_net < 0 THEN -amount_net ELSE 0 END) AS outflow`).
		Where("batch_id = ?", batchID).
		Group("method").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[string]flowSum, len(rows))
	for _, r := range rows {
		out[r.Method] = flowSum{Inflow: r.Inflow, Outflow: r.Outflow}
	}
	return out, nil
}

// bankSumsByChannel 汇总区间内银行 / 渠道流水（按 channel）。
func (s *Service) bankSumsByChannel(ctx context.Context, branchID string, from, to time.Time) (map[string]flowSum, error) {
	var rows []struct {
		Channel string          `gorm:"column:channel"`
		Inflow  decimal.Decimal `gorm:"column:inflow"`
		Outflow decimal.Decimal `gorm:"column:outflow"`
		Fee     decimal.Decimal `gorm:"column:fee"`
	}
	err := s.db.WithContext(ctx).Model(&model.ReconBankFlow{}).
		Select(`channel,
			SUM(CASE WHEN amount > 0 THEN amount ELSE 0 END) AS inflow,
			SUM(CASE WHEN amount < 0 THEN -amount ELSE 0 END) AS outflow,
			COALESCE(SUM(fee), 0) AS fee`).
		Where("branch_id = ? AND value_date >= ? AND value_date <= ?", branchID, from, to).
		Group("channel").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[string]flowSum, len(rows))
	for _, r := range rows {
		out[r.Channel] = flowSum{Inflow: r.Inflow, Outflow: r.Outflow, Fee: r.Fee}
	}
	return out, nil
}

// receiptSumsByMethod 汇总区间内**已确认**的其他收款（按 method）。
func (s *Service) receiptSumsByMethod(ctx context.Context, branchID string, from, to time.Time) (map[string]decimal.Decimal, error) {
	var rows []struct {
		Method string          `gorm:"column:method"`
		Amount decimal.Decimal `gorm:"column:amount"`
	}
	err := s.db.WithContext(ctx).Model(&model.ReconOtherReceipt{}).
		Select("method, COALESCE(SUM(amount), 0) AS amount").
		Where("branch_id = ? AND biz_date >= ? AND biz_date <= ? AND status = ?",
			branchID, from, to, model.ReceiptStatusConfirmed).
		Group("method").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[string]decimal.Decimal, len(rows))
	for _, r := range rows {
		out[r.Method] = r.Amount
	}
	return out, nil
}

// countedCashAt 汇总某日所有收银员的实盘现金。
func (s *Service) countedCashAt(ctx context.Context, branchID string, day time.Time) (decimal.Decimal, error) {
	var row struct {
		Total decimal.Decimal `gorm:"column:total"`
	}
	err := s.db.WithContext(ctx).Model(&model.ReconCashCount{}).
		Select("COALESCE(SUM(counted_cash), 0) AS total").
		Where("branch_id = ? AND biz_date = ?", branchID, dayOnly(day)).
		Scan(&row).Error
	if err != nil {
		return decimal.Zero, err
	}
	return row.Total, nil
}

// previousClosing 取该账户在 periodStart 之前的最近一期期末。
func (s *Service) previousClosing(ctx context.Context, branchID, accountType, accountKey string, periodStart time.Time) (decimal.Decimal, bool, error) {
	var prev model.ReconPeriodSnapshot
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND account_type = ? AND account_key = ? AND period_end < ?",
			branchID, accountType, accountKey, dayOnly(periodStart)).
		Order("period_end DESC").
		First(&prev).Error
	if err == gorm.ErrRecordNotFound {
		return decimal.Zero, false, nil
	}
	if err != nil {
		return decimal.Zero, false, err
	}
	return prev.Closing, true, nil
}
