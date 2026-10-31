package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/cashrecon/model"
)

// TestEnsureCashAccount_IsBuiltIn 锁住"每个门店都有且只有一个现金账户"。
func TestEnsureCashAccount_IsBuiltIn(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	first, err := svc.EnsureCashAccount(ctx, "00")
	require.NoError(t, err)
	require.Equal(t, model.AccountTypeCash, first.AccountType)
	require.Equal(t, cashAccountKey, first.AccountKey)
	require.Equal(t, "现金", first.Name)

	second, err := svc.EnsureCashAccount(ctx, "00")
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID, "重复调用必须幂等")

	accounts, err := svc.ListAccounts(ctx, "00", false)
	require.NoError(t, err)
	require.Len(t, accounts, 1)
}

// TestCreateAccount_RejectsCashAndForeignCurrency 锁住建账边界。
func TestCreateAccount_RejectsCashAndForeignCurrency(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	_, err := svc.CreateAccount(ctx, CreateAccountInput{
		BranchID: "00", AccountType: model.AccountTypeCash, AccountKey: "cash2", Actor: "u1",
	})
	require.ErrorIs(t, err, model.ErrInvalidInput)

	_, err = svc.CreateAccount(ctx, CreateAccountInput{
		BranchID: "00", AccountType: model.AccountTypeBank, AccountKey: "boc-8899", Currency: "USD", Actor: "u1",
	})
	require.ErrorIs(t, err, model.ErrInvalidInput)

	row, err := svc.CreateAccount(ctx, CreateAccountInput{
		BranchID: "00", AccountType: model.AccountTypeBank, AccountKey: "boc-8899", Name: "中国银行 8899", Actor: "u1",
	})
	require.NoError(t, err)
	require.Equal(t, "CNY", row.Currency)
	require.True(t, row.IsActive)

	_, err = svc.CreateAccount(ctx, CreateAccountInput{
		BranchID: "00", AccountType: model.AccountTypeBank, AccountKey: "boc-8899", Actor: "u1",
	})
	require.ErrorIs(t, err, model.ErrAlreadyExists)
}

// TestCreateOpenings_RequiresEveryActiveAccount 锁住"缺任一启用账户 ⇒ 400，不部分成功"。
func TestCreateOpenings_RequiresEveryActiveAccount(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	_, err := svc.CreateAccount(ctx, CreateAccountInput{
		BranchID: "00", AccountType: model.AccountTypeBank, AccountKey: "boc-8899", Actor: "u1",
	})
	require.NoError(t, err)

	_, err = svc.CreateOpenings(ctx, CreateOpeningsInput{
		BranchID:      "00",
		EffectiveDate: day(2026, 10, 1),
		Items: []OpeningItemInput{
			{AccountType: model.AccountTypeBank, AccountKey: "boc-8899", Amount: dec("1000")},
		},
		Actor: "u1",
	})
	require.True(t, errors.Is(err, model.ErrOpeningItemsIncomplete), "现金账户也要建账: %v", err)

	// 未知账户 / 重复账户 ⇒ 400。
	_, err = svc.CreateOpenings(ctx, CreateOpeningsInput{
		BranchID:      "00",
		EffectiveDate: day(2026, 10, 1),
		Items: []OpeningItemInput{
			{AccountType: model.AccountTypeCash, AccountKey: cashAccountKey, Amount: dec("100")},
			{AccountType: model.AccountTypeBank, AccountKey: "nope", Amount: dec("1")},
		},
		Actor: "u1",
	})
	require.ErrorIs(t, err, model.ErrInvalidInput)

	// 补齐两个账户 ⇒ 成功，且 source=manual（首次建账）。
	rows, err := svc.CreateOpenings(ctx, CreateOpeningsInput{
		BranchID:      "00",
		EffectiveDate: day(2026, 10, 1),
		Items: []OpeningItemInput{
			{AccountType: model.AccountTypeCash, AccountKey: cashAccountKey, Amount: dec("500")},
			{AccountType: model.AccountTypeBank, AccountKey: "boc-8899", Amount: dec("1000")},
		},
		Actor: "u1",
	})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, r := range rows {
		require.Equal(t, model.OpeningSourceManual, r.Source)
	}
}

// TestCreateOpenings_LaterDateIsAdjust 锁住 append-only 修正语义。
func TestCreateOpenings_LaterDateIsAdjust(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, err := svc.EnsureCashAccount(ctx, "00")
	require.NoError(t, err)

	_, err = svc.CreateOpenings(ctx, CreateOpeningsInput{
		BranchID:      "00",
		EffectiveDate: day(2026, 10, 1),
		Items:         []OpeningItemInput{{AccountType: model.AccountTypeCash, AccountKey: cashAccountKey, Amount: dec("500")}},
		Actor:         "u1",
	})
	require.NoError(t, err)

	rows, err := svc.CreateOpenings(ctx, CreateOpeningsInput{
		BranchID:      "00",
		EffectiveDate: day(2026, 11, 1),
		Items:         []OpeningItemInput{{AccountType: model.AccountTypeCash, AccountKey: cashAccountKey, Amount: dec("800")}},
		Note:          "上月短款补记",
		Actor:         "u1",
	})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, model.OpeningSourceAdjust, rows[0].Source)

	row, ok, err := svc.ResolveOpening(ctx, "00", model.AccountTypeCash, cashAccountKey, day(2026, 11, 5))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "800", row.Amount.String())

	// 10-05 仍解析到最早那条（后续调整不能改写历史）。
	old, ok, err := svc.ResolveOpening(ctx, "00", model.AccountTypeCash, cashAccountKey, day(2026, 10, 5))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "500", old.Amount.String())
}

// TestVoidOpening_FallsBackToPrevious 锁住"作废后回退上一条，而不是变成 0"。
func TestVoidOpening_FallsBackToPrevious(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	_, err := svc.EnsureCashAccount(ctx, "00")
	require.NoError(t, err)
	_, err = svc.CreateOpenings(ctx, CreateOpeningsInput{
		BranchID: "00", EffectiveDate: day(2026, 10, 1),
		Items: []OpeningItemInput{{AccountType: model.AccountTypeCash, AccountKey: cashAccountKey, Amount: dec("500")}},
		Actor: "u1",
	})
	require.NoError(t, err)
	rows, err := svc.CreateOpenings(ctx, CreateOpeningsInput{
		BranchID: "00", EffectiveDate: day(2026, 11, 1),
		Items: []OpeningItemInput{{AccountType: model.AccountTypeCash, AccountKey: cashAccountKey, Amount: dec("800")}},
		Actor: "u1",
	})
	require.NoError(t, err)

	_, err = svc.VoidOpening(ctx, "00", rows[0].ID, "u1", "录错了")
	require.NoError(t, err)

	row, ok, err := svc.ResolveOpening(ctx, "00", model.AccountTypeCash, cashAccountKey, day(2026, 11, 5))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "500", row.Amount.String())

	// 重复作废 ⇒ 400（留痕只写一次）。
	_, err = svc.VoidOpening(ctx, "00", rows[0].ID, "u1", "再作废一次")
	require.ErrorIs(t, err, model.ErrInvalidInput)
}

// TestOpeningView_GapWarns 锁住"本期期初 vs 上期期末"的勾稽警告（不阻断）。
func TestOpeningView_GapWarns(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()

	_, err := svc.EnsureCashAccount(ctx, "00")
	require.NoError(t, err)
	_, err = svc.CreateOpenings(ctx, CreateOpeningsInput{
		BranchID: "00", EffectiveDate: day(2026, 10, 1),
		Items: []OpeningItemInput{{AccountType: model.AccountTypeCash, AccountKey: cashAccountKey, Amount: dec("500")}},
		Actor: "u1",
	})
	require.NoError(t, err)

	// 上期期末 480（与 500 差 20）—— 银行侧未入账时这种差额是正常的。
	require.NoError(t, db.Create(&model.ReconPeriodSnapshot{
		ID: newID(), BranchID: "00", PeriodStart: day(2026, 10, 1), PeriodEnd: day(2026, 10, 31),
		AccountType: model.AccountTypeCash, AccountKey: cashAccountKey,
		Opening: dec("0"), OpeningSource: model.OpeningSourceManual,
		Inflow: dec("500"), Outflow: dec("20"), Closing: dec("480"),
		CreatedAt: testClock,
	}).Error)

	// 11-01 起算：期初解析到 10-01 那条，上期期末是 10-31 的快照。
	view, err := svc.OpeningView(ctx, "00", day(2026, 11, 1))
	require.NoError(t, err)
	require.Equal(t, 0, view.Missing)
	require.Len(t, view.Items, 1)
	item := view.Items[0]
	require.NotNil(t, item.Opening)
	require.Equal(t, "500", item.Opening.String())
	require.NotNil(t, item.PreviousClosing)
	require.Equal(t, "480", item.PreviousClosing.String())
	require.NotNil(t, item.Gap)
	require.Equal(t, "20", item.Gap.String())
	require.Equal(t, "opening_gap", item.GapWarning)
}

// TestMissingOpenings_AndLock 锁住前置校验与冻结拦截。
func TestMissingOpenings_AndLock(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()

	missing, err := svc.MissingOpenings(ctx, "00", day(2026, 10, 1))
	require.NoError(t, err)
	require.Len(t, missing, 1, "现金账户没建账 ⇒ 缺失")
	require.Equal(t, cashAccountKey, missing[0].AccountKey)

	_, err = svc.CreateOpenings(ctx, CreateOpeningsInput{
		BranchID: "00", EffectiveDate: day(2026, 10, 1),
		Items: []OpeningItemInput{{AccountType: model.AccountTypeCash, AccountKey: cashAccountKey, Amount: dec("500")}},
		Actor: "u1",
	})
	require.NoError(t, err)
	missing, err = svc.MissingOpenings(ctx, "00", day(2026, 10, 1))
	require.NoError(t, err)
	require.Empty(t, missing)
	_, err = svc.AssertOpeningBalancesComplete(ctx, "00", day(2026, 10, 1))
	require.NoError(t, err)

	// 已冻结区间 ⇒ 不允许再动期初。
	require.NoError(t, db.Create(&model.ReconPeriodLock{
		ID: newID(), BranchID: "00", BizDateFrom: day(2026, 9, 1), BizDateTo: day(2026, 9, 30),
		BatchID: "b1", LockedAt: testClock,
	}).Error)
	_, err = svc.CreateOpenings(ctx, CreateOpeningsInput{
		BranchID: "00", EffectiveDate: day(2026, 9, 15),
		Items: []OpeningItemInput{{AccountType: model.AccountTypeCash, AccountKey: cashAccountKey, Amount: dec("1")}},
		Actor: "u1",
	})
	require.ErrorIs(t, err, model.ErrPeriodFrozen)
}

// TestAssertOpeningBalancesComplete_ReportsMissing 锁住 409 的缺失清单。
func TestAssertOpeningBalancesComplete_ReportsMissing(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	missing, err := svc.AssertOpeningBalancesComplete(ctx, "00", day(2026, 10, 1))
	require.ErrorIs(t, err, model.ErrOpeningBalanceMissing)
	require.Len(t, missing, 1)
}
