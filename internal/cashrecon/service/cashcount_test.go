package service

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/cashrecon/model"
)

// decPtr 取 decimal 指针（用于"必填/可选"的区分）。
func decPtr(v string) *decimal.Decimal {
	d := dec(v)
	return &d
}

// TestRecordCashCount_DerivesExpectedAndCreatesVariance 锁住：
// 未填 expected_cash ⇒ 由当日 cash 净额派生；超容差 ⇒ 生成短款台账。
func TestRecordCashCount_DerivesExpectedAndCreatesVariance(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	insertPayFlows(t, db,
		map[string]any{"branch_id": "00", "pay_way": "RMB", "pay_amount": 100.0, "exchange_rate": 1.0, "paid_at": "2026-10-30 12:00:00"},
		map[string]any{"branch_id": "00", "pay_way": "SWX", "pay_amount": 50.0, "exchange_rate": 1.0, "paid_at": "2026-10-30 13:00:00"},
	)

	res, err := svc.RecordCashCount(ctx, CashCountInput{
		BranchID:    "00",
		BizDate:     day(2026, 10, 30),
		CashierID:   "c1",
		CountedCash: decPtr("94"),
		Actor:       "u1",
	})
	require.NoError(t, err)
	require.Equal(t, "100", res.Count.ExpectedCash.String(), "应收现金只算 cash 净额，不含微信")
	require.Equal(t, "-6", res.Count.Variance.String())
	require.Equal(t, model.CashCountStatusOpen, res.Count.Status)
	require.NotNil(t, res.Variance)
	require.Equal(t, model.VarianceKindShort, res.Variance.Kind)
	require.Equal(t, "6", res.Variance.Amount.String())
	require.Equal(t, model.CashVarianceStatusOpen, res.Variance.Status)

	// 有长短款时确认必须填原因。
	_, err = svc.ConfirmCashCount(ctx, "00", res.Count.ID, "u2", "")
	require.ErrorIs(t, err, model.ErrInvalidInput)

	confirmed, err := svc.ConfirmCashCount(ctx, "00", res.Count.ID, "u2", "short_change")
	require.NoError(t, err)
	require.Equal(t, model.CashCountStatusConfirmed, confirmed.Status)
	require.Equal(t, "u2", confirmed.ConfirmBy)
	require.NotNil(t, confirmed.ConfirmAt)

	var v model.ReconCashVariance
	require.NoError(t, db.Where("branch_id = ? AND cashier_id = ?", "00", "c1").First(&v).Error)
	require.Equal(t, model.CashVarianceStatusConfirmed, v.Status, "确认后长短款台账同步状态")
	require.Equal(t, "short_change", v.ReasonCode)
}

// TestRecordCashCount_WithinTolerance_NoLedger 锁住容差内不生成台账。
func TestRecordCashCount_WithinTolerance_NoLedger(t *testing.T) {
	svc, db := newTestService(t)
	insertPayFlows(t, db,
		map[string]any{"branch_id": "00", "pay_way": "RMB", "pay_amount": 100.0, "exchange_rate": 1.0, "paid_at": "2026-10-30 12:00:00"},
	)
	res, err := svc.RecordCashCount(context.Background(), CashCountInput{
		BranchID: "00", BizDate: day(2026, 10, 30), CashierID: "c1",
		CountedCash: decPtr("97"), Tolerance: decPtr("5"), Actor: "u1",
	})
	require.NoError(t, err)
	require.Equal(t, "-3", res.Count.Variance.String())
	require.Nil(t, res.Variance)

	var n int64
	require.NoError(t, db.Model(&model.ReconCashVariance{}).Count(&n).Error)
	require.Zero(t, n)
}

// TestRecordCashCount_RequiresExplicitExpectedForSecondCashier 锁住：
// 同日已有其它收银员实盘 ⇒ 必须显式填 expected_cash（不能拿整天现金净额当个人应收）。
func TestRecordCashCount_RequiresExplicitExpectedForSecondCashier(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	_, err := svc.RecordCashCount(ctx, CashCountInput{
		BranchID: "00", BizDate: day(2026, 10, 30), CashierID: "c1",
		CountedCash: decPtr("100"), ExpectedCash: decPtr("100"), Actor: "u1",
	})
	require.NoError(t, err)

	_, err = svc.RecordCashCount(ctx, CashCountInput{
		BranchID: "00", BizDate: day(2026, 10, 30), CashierID: "c2",
		CountedCash: decPtr("50"), Actor: "u1",
	})
	require.ErrorIs(t, err, model.ErrInvalidInput)

	_, err = svc.RecordCashCount(ctx, CashCountInput{
		BranchID: "00", BizDate: day(2026, 10, 30), CashierID: "c2",
		CountedCash: decPtr("50"), ExpectedCash: decPtr("50"), Actor: "u1",
	})
	require.NoError(t, err)
}

// TestRecordCashCount_FrozenAndDuplicate 锁住冻结 409 与重复录入 409。
func TestRecordCashCount_FrozenAndDuplicate(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	in := CashCountInput{
		BranchID: "00", BizDate: day(2026, 10, 30), CashierID: "c1",
		CountedCash: decPtr("0"), ExpectedCash: decPtr("0"), Actor: "u1",
	}
	_, err := svc.RecordCashCount(ctx, in)
	require.NoError(t, err)

	_, err = svc.RecordCashCount(ctx, in)
	require.ErrorIs(t, err, model.ErrAlreadyExists, "同日同人重复实盘必须 409")

	require.NoError(t, db.Create(&model.ReconPeriodLock{
		ID: newID(), BranchID: "00", BizDateFrom: day(2026, 10, 1), BizDateTo: day(2026, 10, 31),
		BatchID: "b1", LockedAt: testClock,
	}).Error)
	_, err = svc.RecordCashCount(ctx, CashCountInput{
		BranchID: "00", BizDate: day(2026, 10, 30), CashierID: "c2",
		CountedCash: decPtr("0"), ExpectedCash: decPtr("0"), Actor: "u1",
	})
	require.ErrorIs(t, err, model.ErrPeriodFrozen)
}

// TestCashVarianceSummary_ByCashier 锁住按收银员汇总（长款 - 短款 = 净额）。
func TestCashVarianceSummary_ByCashier(t *testing.T) {
	svc, db := newTestService(t)
	now := testClock
	rows := []model.ReconCashVariance{
		{ID: newID(), BranchID: "00", BizDate: day(2026, 10, 28), CashierID: "c1",
			Kind: model.VarianceKindShort, Amount: dec("4"), Status: model.CashVarianceStatusOpen, CreatedAt: now},
		{ID: newID(), BranchID: "00", BizDate: day(2026, 10, 29), CashierID: "c1",
			Kind: model.VarianceKindOver, Amount: dec("2"), Status: model.CashVarianceStatusConfirmed, CreatedAt: now},
		{ID: newID(), BranchID: "00", BizDate: day(2026, 10, 29), CashierID: "c2",
			Kind: model.VarianceKindShort, Amount: dec("3"), Status: model.CashVarianceStatusOpen, CreatedAt: now},
	}
	require.NoError(t, db.Create(&rows).Error)

	sum, err := svc.CashVarianceSummary(context.Background(), "00", day(2026, 10, 28), day(2026, 10, 30), "")
	require.NoError(t, err)
	require.Len(t, sum, 2)
	require.Equal(t, "c1", sum[0].CashierID)
	require.Equal(t, "2", sum[0].OverAmount.String())
	require.Equal(t, "4", sum[0].ShortAmount.String())
	require.Equal(t, "-2", sum[0].NetAmount.String())
	require.Equal(t, 2, sum[0].Count)
	require.Equal(t, 1, sum[0].OpenCount)
	require.Equal(t, "c2", sum[1].CashierID)
	require.Equal(t, "-3", sum[1].NetAmount.String(), "短款净额应为负数")
}

// TestAssertCashCountExists 锁住缺实盘 ⇒ 409 cash_count_missing。
func TestAssertCashCountExists(t *testing.T) {
	svc, _ := newTestService(t)
	err := svc.AssertCashCountExists(context.Background(), "00", day(2026, 10, 30))
	require.ErrorIs(t, err, model.ErrCashCountMissing)

	_, err = svc.RecordCashCount(context.Background(), CashCountInput{
		BranchID: "00", BizDate: day(2026, 10, 30), CashierID: "c1",
		CountedCash: decPtr("0"), ExpectedCash: decPtr("0"), Actor: "u1",
	})
	require.NoError(t, err)
	require.NoError(t, svc.AssertCashCountExists(context.Background(), "00", day(2026, 10, 30)))
}
