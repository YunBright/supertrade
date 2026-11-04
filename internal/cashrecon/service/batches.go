// Package service / batches.go —— 对账批次：预览、落库、批次号与汇总回写（计划 §10）。
//
// # preview 与 apply 必须是同一段逻辑
//
//	两者都走 runMatch → engine.Run，差别只在 apply 落库。
//	一旦预览走一条查询、落库走另一条，出纳会先信预览、再被落库结果吓到。
//
// # 落库的幂等口径
//
//	唯一键 (batch_id, dedupe_key)；重复 apply **同一批次**不会产生重复行。
//	OnConflict 只更新**引擎字段**（金额/引用），绝不覆盖 status / resolution /
//	handled_* —— 否则出纳每天的人工处理会被下一次重算冲掉。
//
// # 批次号
//
//	CR-<branch>-<yyyymmdd>-<seq>：出纳口头沟通要对得上号。
package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/YunBright/supertrade/internal/cashrecon/engine"
	"github.com/YunBright/supertrade/internal/cashrecon/model"
)

// ApplyBatchInput 是 `POST /batches` 的入参。
type ApplyBatchInput struct {
	BranchID    string
	BizDateFrom time.Time
	BizDateTo   time.Time
	Actor       string
}

// ApplyBatchResult 是落库回执。
type ApplyBatchResult struct {
	Batch           model.ReconBatch `json:"batch"`
	InsertedMatches int              `json:"inserted_matches"`
	InsertedDiffs   int              `json:"inserted_diffs"`
	// CarriedOver 是本次重算中**保留了上一批人工处理结果**的差异条数。
	CarriedOver int `json:"carried_over"`
}

// RecomputeBatchResult 是重做回执。
type RecomputeBatchResult struct {
	Batch           model.ReconBatch `json:"batch"`
	CashFlowRows    int              `json:"cash_flow_rows"`
	InsertedMatches int              `json:"inserted_matches"`
	InsertedDiffs   int              `json:"inserted_diffs"`
	RemovedMatches  int              `json:"removed_matches"`
	RemovedDiffs    int              `json:"removed_diffs"`
	CarriedOver     int              `json:"carried_over"`
}

// runMatch 组装引擎输入并运行匹配（preview / apply 共用，零写库）。
func (s *Service) runMatch(ctx context.Context, branchID string, from, to time.Time) (engine.Output, error) {
	if err := s.validateRange(branchID, from, to); err != nil {
		return engine.Output{}, err
	}
	rules, err := s.LoadRules(ctx, branchID)
	if err != nil {
		return engine.Output{}, err
	}
	cash, _, err := s.CashFlows(ctx, branchID, from, to, rules)
	if err != nil {
		return engine.Output{}, err
	}
	bank, err := s.bankFlows(ctx, branchID, from, to)
	if err != nil {
		return engine.Output{}, err
	}
	prior, err := s.priorDiffStates(ctx, branchID, from, to)
	if err != nil {
		return engine.Output{}, err
	}
	return engine.Run(engine.Input{
		BranchID:    branchID,
		BizDateFrom: from,
		BizDateTo:   to,
		Cash:        cash,
		Bank:        bank,
		Rules:       engine.RuleSet{ArrivalToleranceDays: rules.ArrivalToleranceDays},
		Prior:       prior,
	}), nil
}

// priorDiffStates 取与本区间重叠的历史批次里**已人工处理**的差异状态。
//
// ⚠️ 只取 settled 状态：open 差异重算后本来就该重新判定，
// 把 open 也塞回去会让"这次新判出来的差异"看起来像上一批遗留的。
func (s *Service) priorDiffStates(ctx context.Context, branchID string, from, to time.Time) ([]engine.DiffState, error) {
	var batchIDs []string
	if err := s.db.WithContext(ctx).Model(&model.ReconBatch{}).
		Where("branch_id = ? AND biz_date_from <= ? AND biz_date_to >= ?",
			branchID, dayOnly(to), dayOnly(from)).
		Pluck("id", &batchIDs).Error; err != nil {
		return nil, err
	}
	if len(batchIDs) == 0 {
		return nil, nil
	}
	var rows []model.ReconDiff
	if err := s.db.WithContext(ctx).
		Where("batch_id IN ? AND status IN ?", batchIDs, model.DiffSettledStatuses).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]engine.DiffState, 0, len(rows))
	for _, r := range rows {
		out = append(out, engine.DiffState{
			DedupeKey:      r.DedupeKey,
			Status:         r.Status,
			ResolutionCode: r.ResolutionCode,
			ResolutionNote: r.ResolutionNote,
			HandledBy:      r.HandledBy,
			HandledAt:      r.HandledAt,
		})
	}
	return out, nil
}

// ApplyBatch 生成并落库一个对账批次（draft）。
func (s *Service) ApplyBatch(ctx context.Context, in ApplyBatchInput) (ApplyBatchResult, error) {
	in.BranchID = strings.TrimSpace(in.BranchID)
	in.Actor = strings.TrimSpace(in.Actor)
	if in.Actor == "" {
		return ApplyBatchResult{}, model.ErrInvalidInput
	}
	if err := s.validateRange(in.BranchID, in.BizDateFrom, in.BizDateTo); err != nil {
		return ApplyBatchResult{}, err
	}
	from, to := dayOnly(in.BizDateFrom), dayOnly(in.BizDateTo)
	// 冻结区间内不允许重算：否则快照之后还能改写匹配与差异。
	if err := s.assertNotLocked(ctx, in.BranchID, from, to); err != nil {
		return ApplyBatchResult{}, err
	}
	// 落库前清掉收银流水缓存：预览时读到的可能是 5 分钟前的 cube 结果。
	s.InvalidateCashCache(in.BranchID)
	out, err := s.runMatch(ctx, in.BranchID, from, to)
	if err != nil {
		return ApplyBatchResult{}, err
	}

	now := s.now()
	batch := model.ReconBatch{
		ID:          newID(),
		BranchID:    in.BranchID,
		Kind:        model.BatchKindNormal,
		BizDateFrom: from,
		BizDateTo:   to,
		Status:      model.BatchStatusDraft,
		MatchRate:   out.Summary.MatchRate,
		DiffCount:   out.Summary.DiffCount,
		CashTotal:   out.Summary.CashTotal,
		BankTotal:   out.Summary.BankTotal,
		FeeTotal:    out.Summary.FeeTotal,
		CreatedBy:   in.Actor,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	var insertedMatches, insertedDiffs int
	var carried []engine.Diff
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		no, err := nextBatchNoTx(ctx, tx, in.BranchID, now)
		if err != nil {
			return err
		}
		batch.BatchNo = no
		if err := tx.WithContext(ctx).Create(&batch).Error; err != nil {
			return err
		}

		if len(out.Matches) > 0 {
			rows := make([]model.ReconMatch, 0, len(out.Matches))
			for _, m := range out.Matches {
				rows = append(rows, model.ReconMatch{
					ID:             newID(),
					BatchID:        batch.ID,
					BranchID:       in.BranchID,
					Channel:        m.Channel,
					BizDate:        dayOnly(m.BizDate),
					BankFlowID:     m.BankFlowID,
					CashKey:        m.CashKey,
					ExpectedAmount: m.ExpectedAmount,
					ActualAmount:   m.ActualAmount,
					Fee:            m.Fee,
					Kind:           m.Kind,
					DedupeKey:      m.DedupeKey,
					CreatedAt:      now,
				})
			}
			res := tx.WithContext(ctx).Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "batch_id"}, {Name: "dedupe_key"}},
				DoUpdates: clause.AssignmentColumns([]string{
					"channel", "biz_date", "bank_flow_id", "cash_key",
					"expected_amount", "actual_amount", "fee", "kind",
				}),
			}).CreateInBatches(rows, 500)
			if res.Error != nil {
				return res.Error
			}
			insertedMatches = int(res.RowsAffected)
		}

		if len(out.Diffs) > 0 {
			rows := make([]model.ReconDiff, 0, len(out.Diffs))
			for _, d := range out.Diffs {
				if d.CarriedOver {
					carried = append(carried, d)
				}
				rows = append(rows, model.ReconDiff{
					ID:             newID(),
					BatchID:        batch.ID,
					BranchID:       in.BranchID,
					Kind:           d.Kind,
					Channel:        d.Channel,
					CashRef:        d.CashRef,
					BankRef:        d.BankRef,
					Expected:       d.Expected,
					Actual:         d.Actual,
					DiffAmount:     d.DiffAmount,
					Status:         d.Status,
					ResolutionCode: d.ResolutionCode,
					ResolutionNote: d.ResolutionNote,
					HandledBy:      d.HandledBy,
					HandledAt:      d.HandledAt,
					DedupeKey:      d.DedupeKey,
					CreatedAt:      now,
					UpdatedAt:      now,
				})
			}
			res := tx.WithContext(ctx).Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "batch_id"}, {Name: "dedupe_key"}},
				// ⚠️ 只更新引擎字段：status / resolution / handled_* 是人工结果，重算不许覆盖。
				DoUpdates: clause.AssignmentColumns([]string{
					"kind", "channel", "cash_ref", "bank_ref",
					"expected", "actual", "diff_amount", "updated_at",
				}),
			}).CreateInBatches(rows, 500)
			if res.Error != nil {
				return res.Error
			}
			insertedDiffs = int(res.RowsAffected)
		}

		if err := writeCarriedAuditsTx(ctx, tx, in.BranchID, batch.ID, carried, now); err != nil {
			return err
		}

		return tx.WithContext(ctx).Model(&model.ReconBatch{}).
			Where("id = ?", batch.ID).
			Updates(map[string]any{
				"match_rate": out.Summary.MatchRate,
				"diff_count": out.Summary.DiffCount,
				"cash_total": out.Summary.CashTotal,
				"bank_total": out.Summary.BankTotal,
				"fee_total":  out.Summary.FeeTotal,
				"updated_at": now,
			}).Error
	})
	if err != nil {
		return ApplyBatchResult{}, err
	}

	return ApplyBatchResult{
		Batch:           batch,
		InsertedMatches: insertedMatches,
		InsertedDiffs:   insertedDiffs,
		CarriedOver:     len(carried),
	}, nil
}

// assertNotLockedByOthers 判断 [from, to] 是否被**其它批次**冻结。
//
// 重做只改写本批次自己的快照，所以它自己的锁不算冲突；只有别的批次锁住
// 同一区间时才拒绝（否则两个批次会各自钉住一套互相矛盾的流水）。
func (s *Service) assertNotLockedByOthers(ctx context.Context, branchID string, from, to time.Time, batchID string) error {
	if branchID == "" || from.IsZero() || to.IsZero() || batchID == "" {
		return model.ErrInvalidInput
	}
	var n int64
	err := s.db.WithContext(ctx).Model(&model.ReconPeriodLock{}).
		Where("branch_id = ? AND biz_date_from <= ? AND biz_date_to >= ? AND batch_id <> ?",
			branchID, dayOnly(to), dayOnly(from), batchID).
		Count(&n).Error
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w: %s ~ %s", model.ErrPeriodFrozen, from.Format("2006-01-02"), to.Format("2006-01-02"))
	}
	return nil
}

// RecomputeBatch 重做批次：按**当前**收银流水重新跑匹配与差异。
//
// 为什么必须有"重做"：收银流水会迟到 / 被修正（cube 补数、导入补录），而
// freeze 已经把流水快照钉住、区间也被冻结锁住 ⇒ 批次的差异停在旧口径，
// 新建批次又会被 assertNotLocked 拒绝。重做用本批次的区间重查流水、重跑引擎，
// 把匹配、差异、现金流快照与批次汇总一起刷新。
//
// 人工结果：差异的 status / resolution / handled_* 不被覆盖（upsert 只写引擎
// 字段，已处理状态还会作为 Prior 带回来）；**本次不再成立的差异行会被删除**，
// 否则"现在已经 0 差异"的批次仍会挂着幽灵差异。
func (s *Service) RecomputeBatch(ctx context.Context, branchID, batchID, actor string) (RecomputeBatchResult, error) {
	branchID = strings.TrimSpace(branchID)
	batchID = strings.TrimSpace(batchID)
	actor = strings.TrimSpace(actor)
	if branchID == "" || batchID == "" || actor == "" {
		return RecomputeBatchResult{}, model.ErrInvalidInput
	}
	batch, err := s.GetBatch(ctx, branchID, batchID)
	if err != nil {
		return RecomputeBatchResult{}, err
	}
	switch batch.Status {
	case model.BatchStatusDraft, model.BatchStatusAuditing, model.BatchStatusFrozen:
		// draft / 审核中都能重做；completed 不可变，调整走 adjust 批次。
	default:
		return RecomputeBatchResult{}, fmt.Errorf("%w: 批次状态 %s 不允许重做（已结账请新建调整批次）",
			model.ErrInvalidInput, batch.Status)
	}
	from, to := dayOnly(batch.BizDateFrom), dayOnly(batch.BizDateTo)
	if err := s.assertNotLockedByOthers(ctx, branchID, from, to, batch.ID); err != nil {
		return RecomputeBatchResult{}, err
	}

	// 重做必须看到**当前**收银流水（同步后缓存可能还是旧的）。
	s.InvalidateCashCache(branchID)
	rules, err := s.LoadRules(ctx, branchID)
	if err != nil {
		return RecomputeBatchResult{}, err
	}
	cash, source, err := s.CashFlows(ctx, branchID, from, to, rules)
	if err != nil {
		return RecomputeBatchResult{}, err
	}
	out, err := s.runMatch(ctx, branchID, from, to)
	if err != nil {
		return RecomputeBatchResult{}, err
	}

	now := s.now()
	res := RecomputeBatchResult{
		Batch:           batch,
		InsertedMatches: len(out.Matches),
		InsertedDiffs:   len(out.Diffs),
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 1) 现金流快照整体替换（重做 = 按当前流水重新审核计算）。
		//    draft 还没冻结，没有快照行，跳过。
		if batch.Status != model.BatchStatusDraft {
			if err := tx.WithContext(ctx).
				Where("batch_id = ?", batch.ID).Delete(&model.ReconCashFlow{}).Error; err != nil {
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
				if err := tx.WithContext(ctx).CreateInBatches(flows, 500).Error; err != nil {
					return err
				}
			}
			res.CashFlowRows = len(cash)
		}

		// 2) 匹配整体替换：matches 全是引擎字段，没有人工结果，删旧写新最干净。
		delMatches := tx.WithContext(ctx).
			Where("batch_id = ?", batch.ID).Delete(&model.ReconMatch{})
		if delMatches.Error != nil {
			return delMatches.Error
		}
		res.RemovedMatches = int(delMatches.RowsAffected)
		if len(out.Matches) > 0 {
			rows := make([]model.ReconMatch, 0, len(out.Matches))
			for _, m := range out.Matches {
				rows = append(rows, model.ReconMatch{
					ID:             newID(),
					BatchID:        batch.ID,
					BranchID:       branchID,
					Channel:        m.Channel,
					BizDate:        dayOnly(m.BizDate),
					BankFlowID:     m.BankFlowID,
					CashKey:        m.CashKey,
					ExpectedAmount: m.ExpectedAmount,
					ActualAmount:   m.ActualAmount,
					Fee:            m.Fee,
					Kind:           m.Kind,
					DedupeKey:      m.DedupeKey,
					CreatedAt:      now,
				})
			}
			if err := tx.WithContext(ctx).CreateInBatches(rows, 500).Error; err != nil {
				return err
			}
		}

		// 3) 差异：只 upsert 引擎字段（人工状态保留），再把本次不再成立的行删掉。
		var carried []engine.Diff
		if len(out.Diffs) > 0 {
			rows := make([]model.ReconDiff, 0, len(out.Diffs))
			for _, d := range out.Diffs {
				if d.CarriedOver {
					carried = append(carried, d)
				}
				rows = append(rows, model.ReconDiff{
					ID:             newID(),
					BatchID:        batch.ID,
					BranchID:       branchID,
					Kind:           d.Kind,
					Channel:        d.Channel,
					CashRef:        d.CashRef,
					BankRef:        d.BankRef,
					Expected:       d.Expected,
					Actual:         d.Actual,
					DiffAmount:     d.DiffAmount,
					Status:         d.Status,
					ResolutionCode: d.ResolutionCode,
					ResolutionNote: d.ResolutionNote,
					HandledBy:      d.HandledBy,
					HandledAt:      d.HandledAt,
					DedupeKey:      d.DedupeKey,
					CreatedAt:      now,
					UpdatedAt:      now,
				})
			}
			if err := tx.WithContext(ctx).Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "batch_id"}, {Name: "dedupe_key"}},
				DoUpdates: clause.AssignmentColumns([]string{
					"kind", "channel", "cash_ref", "bank_ref",
					"expected", "actual", "diff_amount", "updated_at",
				}),
			}).CreateInBatches(rows, 500).Error; err != nil {
				return err
			}
		}
		keys := make([]string, 0, len(out.Diffs))
		for _, d := range out.Diffs {
			keys = append(keys, d.DedupeKey)
		}
		stale := tx.WithContext(ctx).Where("batch_id = ?", batch.ID)
		if len(keys) > 0 {
			stale = stale.Where("dedupe_key NOT IN ?", keys)
		}
		var staleRows []model.ReconDiff
		if err := stale.Find(&staleRows).Error; err != nil {
			return err
		}
		if len(staleRows) > 0 {
			ids := make([]string, 0, len(staleRows))
			for _, r := range staleRows {
				ids = append(ids, r.ID)
				// 曾经人工处理过、现在不再成立的差异：留一条审计再删（审计链不断）。
				if r.Status == model.DiffStatusOpen {
					continue
				}
				if err := tx.WithContext(ctx).Create(&model.ReconAuditLog{
					ID:         newID(),
					BranchID:   branchID,
					ObjectType: "recon_diff",
					ObjectID:   r.ID,
					Action:     "recompute_dropped",
					BeforeJSON: fmt.Sprintf(`{"status":%q,"resolution_code":%q}`, r.Status, r.ResolutionCode),
					Reason:     "重做后该差异不再成立",
					Actor:      actor,
					CreatedAt:  now,
				}).Error; err != nil {
					return err
				}
			}
			if err := tx.WithContext(ctx).Where("id IN ?", ids).Delete(&model.ReconDiff{}).Error; err != nil {
				return err
			}
			res.RemovedDiffs = len(staleRows)
		}

		if err := writeCarriedAuditsTx(ctx, tx, branchID, batch.ID, carried, now); err != nil {
			return err
		}
		res.CarriedOver = len(carried)

		// 4) 汇总回写（批次行本身仍是同一张单，只刷新口径）。
		return tx.WithContext(ctx).Model(&model.ReconBatch{}).
			Where("id = ?", batch.ID).
			Updates(map[string]any{
				"match_rate": out.Summary.MatchRate,
				"diff_count": out.Summary.DiffCount,
				"cash_total": out.Summary.CashTotal,
				"bank_total": out.Summary.BankTotal,
				"fee_total":  out.Summary.FeeTotal,
				"updated_at": now,
			}).Error
	})
	if err != nil {
		return RecomputeBatchResult{}, err
	}

	if err := s.writeAudit(ctx, model.ReconAuditLog{
		BranchID:   branchID,
		ObjectType: "recon_batch",
		ObjectID:   batch.ID,
		Action:     "recompute",
		AfterJSON: fmt.Sprintf(`{"match_rate":%q,"diff_count":%d,"cash_flow_rows":%d,"removed_diffs":%d}`,
			out.Summary.MatchRate.String(), out.Summary.DiffCount, res.CashFlowRows, res.RemovedDiffs),
		Reason:    "重做：按当前收银流水重新跑匹配与差异",
		Actor:     actor,
		CreatedAt: now,
	}); err != nil {
		return RecomputeBatchResult{}, err
	}

	batch.MatchRate = out.Summary.MatchRate
	batch.DiffCount = out.Summary.DiffCount
	batch.CashTotal = out.Summary.CashTotal
	batch.BankTotal = out.Summary.BankTotal
	batch.FeeTotal = out.Summary.FeeTotal
	batch.UpdatedAt = now
	res.Batch = batch
	return res, nil
}

// nextBatchNoTx 生成 CR-<branch>-<yyyymmdd>-<seq>（同店同日递增）。
//
// 并发下两个请求可能算出同一个 seq ⇒ 唯一索引 ux_batch_branch_no 兜底，
// 失败方拿到 already_exists 而不是两条同号批次。
func nextBatchNoTx(ctx context.Context, tx *gorm.DB, branchID string, now time.Time) (string, error) {
	prefix := fmt.Sprintf("CR-%s-%s-", branchID, now.Format("20060102"))
	var n int64
	if err := tx.WithContext(ctx).Model(&model.ReconBatch{}).
		Where("branch_id = ? AND batch_no LIKE ?", branchID, prefix+"%").
		Count(&n).Error; err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%02d", prefix, n+1), nil
}

// writeCarriedAuditsTx 为"沿用了人工处理结果"的差异写审计（append-only）。
func writeCarriedAuditsTx(ctx context.Context, tx *gorm.DB, branchID, batchID string, carried []engine.Diff, now time.Time) error {
	if len(carried) == 0 {
		return nil
	}
	keys := make([]string, 0, len(carried))
	for _, d := range carried {
		keys = append(keys, d.DedupeKey)
	}
	var rows []model.ReconDiff
	if err := tx.WithContext(ctx).
		Where("batch_id = ? AND dedupe_key IN ?", batchID, keys).
		Find(&rows).Error; err != nil {
		return err
	}
	byKey := make(map[string]model.ReconDiff, len(rows))
	for _, r := range rows {
		byKey[r.DedupeKey] = r
	}
	for _, d := range carried {
		row, ok := byKey[d.DedupeKey]
		if !ok {
			continue
		}
		if err := tx.WithContext(ctx).Create(&model.ReconAuditLog{
			ID:         newID(),
			BranchID:   branchID,
			ObjectType: "recon_diff",
			ObjectID:   row.ID,
			Action:     "carried_over",
			AfterJSON: fmt.Sprintf(`{"status":%q,"resolution_code":%q}`,
				d.Status, d.ResolutionCode),
			Reason:    "重算保留上一批人工处理结果",
			Actor:     "system",
			CreatedAt: now,
		}).Error; err != nil {
			return err
		}
	}
	return nil
}
