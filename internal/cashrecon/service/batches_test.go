package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cashrecon/model"
)

// seedBankFlow 写入一条银行/渠道到账流水（channel = 标准 method，便于匹配）。
func seedBankFlow(t *testing.T, db *gorm.DB, branch, channel string, d time.Time, amount, fee string) model.ReconBankFlow {
	t.Helper()
	row := model.ReconBankFlow{
		ID:        newID(),
		BranchID:  branch,
		Channel:   channel,
		ValueDate: d,
		Amount:    dec(amount),
		Fee:       dec(fee),
		DedupeKey: fmt.Sprintf("%s|%s|%s|%s|%s", channel, d.Format("2006-01-02"), amount, fee, newID()),
		CreatedAt: testClock,
	}
	require.NoError(t, db.Create(&row).Error)
	return row
}

// TestApplyBatch_MatchesAndDiffs 锁住"同一天生成匹配与差异，汇总=明细"。
func TestApplyBatch_MatchesAndDiffs(t *testing.T) {
	svc, db := newTestService(t)
	svc.SetCashSource(localSource(), nil)
	ctx := context.Background()

	insertPayFlows(t, db,
		map[string]any{"branch_id": "00", "pay_way": "SWX", "pay_amount": "100", "exchange_rate": "1", "paid_at": "2026-10-30 10:00:00"},
		map[string]any{"branch_id": "00", "pay_way": "SWX", "pay_amount": "200", "exchange_rate": "1", "paid_at": "2026-10-31 10:00:00"},
	)
	seedBankFlow(t, db, "00", model.MethodWechat, day(2026, 10, 30), "100", "0")

	res, err := svc.ApplyBatch(ctx, ApplyBatchInput{
		BranchID: "00", BizDateFrom: day(2026, 10, 30), BizDateTo: day(2026, 10, 31), Actor: "u1",
	})
	require.NoError(t, err)
	require.Equal(t, model.BatchStatusDraft, res.Batch.Status)
	require.Regexp(t, `^CR-00-\d{8}-\d{2}$`, res.Batch.BatchNo)
	require.Equal(t, 1, res.InsertedMatches)
	require.Equal(t, 1, res.InsertedDiffs)
	require.Equal(t, "0.5", res.Batch.MatchRate.String())

	// 30 日精确匹配；31 日无到账 ⇒ time_lag。
	require.Equal(t, "300", res.Batch.CashTotal.String())
	require.Equal(t, "100", res.Batch.BankTotal.String())
	require.Len(t, mustDiffs(t, db, res.Batch.ID), 1)
	require.Equal(t, model.DiffKindTimeLag, mustDiffs(t, db, res.Batch.ID)[0].Kind)
}

// TestApplyBatch_RepeatApplyKeepsHandledStatus 锁住"重算不冲掉人工处理结果"。
func TestApplyBatch_RepeatApplyKeepsHandledStatus(t *testing.T) {
	svc, db := newTestService(t)
	svc.SetCashSource(localSource(), nil)
	ctx := context.Background()

	insertPayFlows(t, db,
		map[string]any{"branch_id": "00", "pay_way": "SWX", "pay_amount": "200", "exchange_rate": "1", "paid_at": "2026-10-31 10:00:00"},
	)
	in := ApplyBatchInput{BranchID: "00", BizDateFrom: day(2026, 10, 31), BizDateTo: day(2026, 10, 31), Actor: "u1"}

	first, err := svc.ApplyBatch(ctx, in)
	require.NoError(t, err)
	diffs := mustDiffs(t, db, first.Batch.ID)
	require.Len(t, diffs, 1)
	_, err = svc.ActDiff(ctx, ActDiffInput{
		BranchID: "00", DiffID: diffs[0].ID, Action: DiffActionReason, Reason: "渠道结算延迟", Actor: "u1",
	})
	require.NoError(t, err)

	// 同一区间再算一次 ⇒ 新批次，但那条差异沿用 reasoned。
	second, err := svc.ApplyBatch(ctx, in)
	require.NoError(t, err)
	require.Equal(t, 1, second.CarriedOver)
	recomputed := mustDiffs(t, db, second.Batch.ID)
	require.Len(t, recomputed, 1)
	require.Equal(t, model.DiffStatusReasoned, recomputed[0].Status)
	require.Equal(t, "渠道结算延迟", recomputed[0].ResolutionNote)
	require.Equal(t, "u1", recomputed[0].HandledBy)

	// 重复 apply 不产生重复行：每个批次内 dedupe_key 唯一。
	rows := mustDiffs(t, db, second.Batch.ID)
	keys := map[string]bool{}
	for _, r := range rows {
		require.False(t, keys[r.DedupeKey], "批内 dedupe_key 必须唯一")
		keys[r.DedupeKey] = true
	}
}

// TestApplyBatch_RejectsFrozenPeriod 锁住冻结区间不允许重算。
func TestApplyBatch_RejectsFrozenPeriod(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	require.NoError(t, db.Create(&model.ReconPeriodLock{
		ID: newID(), BranchID: "00", BizDateFrom: day(2026, 10, 1), BizDateTo: day(2026, 10, 31),
		BatchID: "b1", LockedAt: testClock,
	}).Error)

	_, err := svc.ApplyBatch(ctx, ApplyBatchInput{
		BranchID: "00", BizDateFrom: day(2026, 10, 30), BizDateTo: day(2026, 10, 31), Actor: "u1",
	})
	require.ErrorIs(t, err, model.ErrPeriodFrozen)
}

// TestActDiff_RequiresReasonAndWritesAudit 锁住"人工动作必填原因 + append-only 审计"。
func TestActDiff_RequiresReasonAndWritesAudit(t *testing.T) {
	svc, db := newTestService(t)
	svc.SetCashSource(localSource(), nil)
	ctx := context.Background()

	insertPayFlows(t, db,
		map[string]any{"branch_id": "00", "pay_way": "LEP", "pay_amount": "50", "exchange_rate": "1", "paid_at": "2026-10-31 10:00:00"},
	)
	res, err := svc.ApplyBatch(ctx, ApplyBatchInput{
		BranchID: "00", BizDateFrom: day(2026, 10, 31), BizDateTo: day(2026, 10, 31), Actor: "u1",
	})
	require.NoError(t, err)
	diffs := mustDiffs(t, db, res.Batch.ID)
	require.Len(t, diffs, 1, "未映射支付方式 ⇒ unmapped_pay_way 差异")

	_, err = svc.ActDiff(ctx, ActDiffInput{BranchID: "00", DiffID: diffs[0].ID, Action: DiffActionMatch, Actor: "u1"})
	require.ErrorIs(t, err, model.ErrInvalidInput, "空原因必须拒绝")

	row, err := svc.ActDiff(ctx, ActDiffInput{
		BranchID: "00", DiffID: diffs[0].ID, Action: DiffActionEscalate, Reason: "疑似重复结算", Actor: "u2",
	})
	require.NoError(t, err)
	require.Equal(t, model.DiffStatusEscalated, row.Status)
	require.Equal(t, "escalated", row.ResolutionCode)
	require.Equal(t, "u2", row.HandledBy)
	require.NotNil(t, row.HandledAt)

	detail, err := svc.GetDiff(ctx, "00", diffs[0].ID)
	require.NoError(t, err)
	require.Len(t, detail.AuditLogs, 1)
	require.Equal(t, DiffActionEscalate, detail.AuditLogs[0].Action)
	require.Equal(t, "疑似重复结算", detail.AuditLogs[0].Reason)

	// 再次动作 ⇒ 审计追加，不覆盖。
	_, err = svc.ActDiff(ctx, ActDiffInput{
		BranchID: "00", DiffID: diffs[0].ID, Action: DiffActionReason, Reason: "已核实，平台补差", Actor: "u3",
	})
	require.NoError(t, err)
	detail, err = svc.GetDiff(ctx, "00", diffs[0].ID)
	require.NoError(t, err)
	require.Len(t, detail.AuditLogs, 2)
	require.Equal(t, model.DiffStatusReasoned, detail.Diff.Status)
}

// TestListDiffs_Filters 锁住筛选与门店隔离。
func TestListDiffs_Filters(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()

	now := testClock
	require.NoError(t, db.Create(&model.ReconDiff{
		ID: newID(), BatchID: "b1", BranchID: "00", Kind: model.DiffKindSingleBank,
		Status: model.DiffStatusOpen, DedupeKey: newID(), CreatedAt: now, UpdatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&model.ReconDiff{
		ID: newID(), BatchID: "b1", BranchID: "00", Kind: model.DiffKindTimeLag,
		Status: model.DiffStatusHung, DedupeKey: newID(), CreatedAt: now, UpdatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&model.ReconDiff{
		ID: newID(), BatchID: "b1", BranchID: "01", Kind: model.DiffKindTimeLag,
		Status: model.DiffStatusOpen, DedupeKey: newID(), CreatedAt: now, UpdatedAt: now,
	}).Error)

	res, err := svc.ListDiffs(ctx, DiffQuery{BranchID: "00"})
	require.NoError(t, err)
	require.Equal(t, int64(2), res.Total)

	res, err = svc.ListDiffs(ctx, DiffQuery{BranchID: "00", Status: model.DiffStatusHung})
	require.NoError(t, err)
	require.Equal(t, int64(1), res.Total)

	_, err = svc.ListDiffs(ctx, DiffQuery{BranchID: "00", Status: "deleted"})
	require.ErrorIs(t, err, model.ErrInvalidInput)

	require.NoError(t, db.Create(&model.ReconPeriodLock{
		ID: newID(), BranchID: "00", BizDateFrom: day(2026, 10, 1), BizDateTo: day(2026, 10, 31),
		BatchID: "b1", LockedAt: now,
	}).Error)
	// 差异动作不因冻结而禁（冻结只挡"改变流水金额"的写操作）。
	first := res.Items[0]
	_, err = svc.ActDiff(ctx, ActDiffInput{
		BranchID: "00", DiffID: first.ID, Action: DiffActionMatch, Reason: "人工匹配", Actor: "u1",
	})
	require.NoError(t, err)
}

// mustDiffs 取某批次的差异行（按 dedupe_key 排序便于断言）。
func mustDiffs(t *testing.T, db *gorm.DB, batchID string) []model.ReconDiff {
	t.Helper()
	var rows []model.ReconDiff
	require.NoError(t, db.Where("batch_id = ?", batchID).Order("dedupe_key").Find(&rows).Error)
	return rows
}

// TestApplyBatch_SampleMatchRateAtLeast90 锁住样本匹配率 ≥ 90%（S2 验收口径）。
func TestApplyBatch_SampleMatchRateAtLeast90(t *testing.T) {
	svc, db := newTestService(t)
	svc.SetCashSource(localSource(), nil)
	ctx := context.Background()

	// 10 天收银流水，前 9 天都有等额到账 ⇒ 9 匹配 + 1 未到账 = 90%。
	for i := 0; i < 10; i++ {
		d := day(2026, 10, 1).AddDate(0, 0, i)
		insertPayFlows(t, db, map[string]any{
			"branch_id": "00", "pay_way": "SWX", "pay_amount": "100", "exchange_rate": "1",
			"paid_at": d.Format("2006-01-02") + " 10:00:00",
		})
		if i < 9 {
			seedBankFlow(t, db, "00", model.MethodWechat, d, "100", "0")
		}
	}

	res, err := svc.ApplyBatch(ctx, ApplyBatchInput{
		BranchID: "00", BizDateFrom: day(2026, 10, 1), BizDateTo: day(2026, 10, 10), Actor: "u1",
	})
	require.NoError(t, err)
	require.Equal(t, 9, res.InsertedMatches)
	require.Equal(t, 1, res.InsertedDiffs)
	require.Equal(t, "0.9", res.Batch.MatchRate.String(), "样本匹配率必须 ≥ 90%")
}

// TestApplyBatch_InvalidRange 锁住入参校验。
func TestApplyBatch_InvalidRange(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.ApplyBatch(context.Background(), ApplyBatchInput{
		BranchID: "00", BizDateFrom: day(2026, 11, 1), BizDateTo: day(2026, 10, 1), Actor: "u1",
	})
	require.True(t, errors.Is(err, model.ErrInvalidInput))
	_, err = svc.ApplyBatch(context.Background(), ApplyBatchInput{
		BranchID: "00", BizDateFrom: day(2026, 1, 1), BizDateTo: day(2026, 12, 31), Actor: "u1",
	})
	require.ErrorIs(t, err, model.ErrRangeTooLarge)
}
