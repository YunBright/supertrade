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
