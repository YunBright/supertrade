package service

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/cashrecon/model"
)

// seedPeriodFixture 准备「现金 + 一个渠道账户」的期初与流水。
//
//	现金期初 1000，渠道期初 0；当日现金收款 200 / 微信收款 300；
//	渠道到账 300（手续费 0）；现金实盘 195（短款 5）。
func seedPeriodFixture(t *testing.T, svc *Service) string {
	t.Helper()
	ctx := context.Background()

	_, err := svc.CreateAccount(ctx, CreateAccountInput{
		BranchID: "00", AccountType: model.AccountTypeChannel, AccountKey: "wechat", Name: "微信", Actor: "u1",
	})
	require.NoError(t, err)
	_, err = svc.CreateOpenings(ctx, CreateOpeningsInput{
		BranchID:      "00",
		EffectiveDate: day(2026, 10, 1),
		Items: []OpeningItemInput{
			{AccountType: model.AccountTypeCash, AccountKey: cashAccountKey, Amount: dec("1000")},
			{AccountType: model.AccountTypeChannel, AccountKey: "wechat", Amount: dec("0")},
		},
		Actor: "u1",
	})
	require.NoError(t, err)

	insertPayFlows(t, svc.db, map[string]any{
		"branch_id": "00", "pay_way": "RMB", "pay_amount": "200", "exchange_rate": "1",
		"paid_at": "2026-10-01 12:00:00",
	}, map[string]any{
		"branch_id": "00", "pay_way": "SWX", "pay_amount": "300", "exchange_rate": "1",
		"paid_at": "2026-10-01 12:00:00",
	})

	// 渠道到账 300（channel = 账户 key）。
	require.NoError(t, svc.db.Create(&model.ReconBankFlow{
		ID: newID(), BranchID: "00", Channel: "wechat",
		ValueDate: day(2026, 10, 1), Amount: dec("300"), Fee: dec("0"),
		DedupeKey: "wechat-20261001-300", CreatedAt: testClock,
	}).Error)

	// 实盘必须在冻结前录入（冻结后写操作 409）。
	_, err = svc.RecordCashCount(ctx, CashCountInput{
		BranchID: "00", BizDate: day(2026, 10, 1), CashierID: "c1",
		CountedCash:  ptrDec("195"),
		ExpectedCash: ptrDec("200"),
		Tolerance:    ptrDec("0"), // 默认容差 5 会把短款 5 吞掉；夹具显式置 0 以落台账
		Actor:        "u1",
	})
	require.NoError(t, err)

	res, err := svc.ApplyBatch(ctx, ApplyBatchInput{
		BranchID: "00", BizDateFrom: day(2026, 10, 1), BizDateTo: day(2026, 10, 1), Actor: "u1",
	})
	require.NoError(t, err)
	return res.Batch.ID
}

// ptrDec 返回 decimal 指针（实盘/容差入参用）。
func ptrDec(v string) *decimal.Decimal {
	d := dec(v)
	return &d
}

// TestFreeze_MissingOpeningBlocks 缺期初 ⇒ 带缺失清单的 409 语义。
func TestFreeze_MissingOpeningBlocks(t *testing.T) {
	svc, db := newTestService(t)
	svc.SetCashSource(localSource(), nil)
	ctx := context.Background()
	insertPayFlows(t, db) // 本地路径需要 pay_flows 表（即使为空）

	res, err := svc.ApplyBatch(ctx, ApplyBatchInput{
		BranchID: "00", BizDateFrom: day(2026, 10, 1), BizDateTo: day(2026, 10, 1), Actor: "u1",
	})
	require.NoError(t, err)

	_, err = svc.FreezeBatch(ctx, "00", res.Batch.ID, "u1")
	require.Error(t, err)
	require.True(t, errors.Is(err, model.ErrOpeningBalanceMissing))
	var om *OpeningMissingError
	require.True(t, errors.As(err, &om), "必须带缺失账户清单: %v", err)
	require.NotEmpty(t, om.Missing)
	require.Equal(t, model.AccountTypeCash, om.Missing[0].AccountType)
}

// TestComplete_RequiresFreeze 未冻结不能结账。
func TestComplete_RequiresFreeze(t *testing.T) {
	svc, db := newTestService(t)
	svc.SetCashSource(localSource(), nil)
	ctx := context.Background()
	insertPayFlows(t, db)

	res, err := svc.ApplyBatch(ctx, ApplyBatchInput{
		BranchID: "00", BizDateFrom: day(2026, 10, 1), BizDateTo: day(2026, 10, 1), Actor: "u1",
	})
	require.NoError(t, err)

	_, err = svc.CompleteBatch(ctx, "00", res.Batch.ID, "u1")
	require.ErrorIs(t, err, model.ErrInvalidInput)
}

// TestFreezeComplete_FullFlow 冻结 → 区间写操作 409 → 结账快照 → 自动结转 → 幂等。
func TestFreezeComplete_FullFlow(t *testing.T) {
	svc, _ := newTestService(t)
	svc.SetCashSource(localSource(), nil)
	ctx := context.Background()
	batchID := seedPeriodFixture(t, svc)

	// ---- freeze ----
	fr, err := svc.FreezeBatch(ctx, "00", batchID, "u1")
	require.NoError(t, err)
	require.Equal(t, model.BatchStatusAuditing, fr.Batch.Status)
	require.Equal(t, 2, fr.CashFlowRows, "RMB / SWX 两行快照")
	require.Equal(t, CashSourceLocal, fr.DataSource)

	// 幂等重入：不重复写行。
	fr2, err := svc.FreezeBatch(ctx, "00", batchID, "u1")
	require.NoError(t, err)
	require.True(t, fr2.AlreadyFrozen)
	require.Equal(t, 2, fr2.CashFlowRows)

	// 冻结后同区间写操作 409 period_frozen（实盘 / 重算）。
	_, err = svc.RecordCashCount(ctx, CashCountInput{
		BranchID: "00", BizDate: day(2026, 10, 1), CashierID: "c2",
		CountedCash: ptrDec("1"), ExpectedCash: ptrDec("1"), Actor: "u1",
	})
	require.ErrorIs(t, err, model.ErrPeriodFrozen)
	_, err = svc.ApplyBatch(ctx, ApplyBatchInput{
		BranchID: "00", BizDateFrom: day(2026, 10, 1), BizDateTo: day(2026, 10, 1), Actor: "u1",
	})
	require.ErrorIs(t, err, model.ErrPeriodFrozen)

	// ---- complete ----
	cr, err := svc.CompleteBatch(ctx, "00", batchID, "u1")
	require.NoError(t, err)
	require.Equal(t, model.BatchStatusCompleted, cr.Batch.Status)
	require.Len(t, cr.Snapshots, 2)
	require.Equal(t, 2, cr.CarriedOpenings)
	require.Equal(t, "195", cr.CashClosing.String(), "现金期末必须等于实盘")

	cashSnapshot, channelSnapshot := byAccountType(cr.Snapshots)
	require.Equal(t, "1000", cashSnapshot.Opening.String())
	require.Equal(t, "200", cashSnapshot.Inflow.String())
	require.Equal(t, "195", cashSnapshot.Closing.String())
	require.Equal(t, "300", channelSnapshot.Inflow.String())
	require.Equal(t, "300", channelSnapshot.Closing.String())

	// 自动结转：下一期（10-02）期初 = 本期期末。
	for _, accType := range []string{model.AccountTypeCash, model.AccountTypeChannel} {
		key := cashAccountKey
		want := "195"
		if accType == model.AccountTypeChannel {
			key, want = "wechat", "300"
		}
		row, ok, err := svc.ResolveOpening(ctx, "00", accType, key, day(2026, 10, 2))
		require.NoError(t, err)
		require.True(t, ok, "下期期初必须自动结转")
		require.Equal(t, model.OpeningSourceCarry, row.Source)
		require.Equal(t, want, row.Amount.String())
	}

	// completed 不可变：重入返回既有快照，不再结转。
	cr2, err := svc.CompleteBatch(ctx, "00", batchID, "u1")
	require.NoError(t, err)
	require.True(t, cr2.AlreadyCompleted)
	require.Equal(t, cr.Snapshots, cr2.Snapshots)

	var carryCount int64
	require.NoError(t, svc.db.Model(&model.ReconOpeningBalance{}).
		Where("branch_id = ? AND source = ?", "00", model.OpeningSourceCarry).
		Count(&carryCount).Error)
	require.EqualValues(t, 2, carryCount, "结转必须幂等（不重复写）")
}

// TestComplete_MissingCashCount 缺当期实盘 ⇒ 409 cash_count_missing（不默认 0）。
func TestComplete_MissingCashCount(t *testing.T) {
	svc, db := newTestService(t)
	svc.SetCashSource(localSource(), nil)
	ctx := context.Background()
	insertPayFlows(t, db)

	_, err := svc.CreateOpenings(ctx, CreateOpeningsInput{
		BranchID:      "00",
		EffectiveDate: day(2026, 10, 1),
		Items: []OpeningItemInput{
			{AccountType: model.AccountTypeCash, AccountKey: cashAccountKey, Amount: dec("10")},
		},
		Actor: "u1",
	})
	require.NoError(t, err)
	res, err := svc.ApplyBatch(ctx, ApplyBatchInput{
		BranchID: "00", BizDateFrom: day(2026, 10, 1), BizDateTo: day(2026, 10, 1), Actor: "u1",
	})
	require.NoError(t, err)

	fr, err := svc.FreezeBatch(ctx, "00", res.Batch.ID, "u1")
	require.NoError(t, err)
	_, err = svc.CompleteBatch(ctx, "00", fr.Batch.ID, "u1")
	require.ErrorIs(t, err, model.ErrCashCountMissing)
}

// TestDaily_SummaryEqualsDetail 日报汇总 = 明细，余额取快照。
func TestDaily_SummaryEqualsDetail(t *testing.T) {
	svc, _ := newTestService(t)
	svc.SetCashSource(localSource(), nil)
	ctx := context.Background()
	batchID := seedPeriodFixture(t, svc)
	_, err := svc.FreezeBatch(ctx, "00", batchID, "u1")
	require.NoError(t, err)
	_, err = svc.CompleteBatch(ctx, "00", batchID, "u1")
	require.NoError(t, err)

	rep, err := svc.Daily(ctx, "00", day(2026, 10, 1))
	require.NoError(t, err)

	sum := dec("0")
	for _, r := range rep.Receipts {
		sum = sum.Add(r.Amount)
	}
	require.Equal(t, rep.ReceiptTotal.String(), sum.String(), "汇总必须等于明细之和")
	require.Equal(t, "200", rep.CashTotal.String())
	require.Equal(t, "300", rep.BankTotal.String())
	require.Equal(t, "5", rep.CashShortAmount.String())
	require.Len(t, rep.Drill.BankFlowIDs, 1, "下钻 id 必须覆盖银行流水")
	require.NotEmpty(t, rep.Balances, "余额列必须取期末快照")
	for _, b := range rep.Balances {
		require.Equal(t, "2026-10-01", b.PeriodStart)
		require.Equal(t, "2026-10-01", b.PeriodEnd)
	}
}

// TestDashboard_DaysValidation 看板只接受 7 / 30，趋势点逐日。
func TestDashboard_DaysValidation(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	_, err := svc.Dashboard(ctx, "00", 14)
	require.ErrorIs(t, err, model.ErrInvalidInput)

	res, err := svc.Dashboard(ctx, "00", 7)
	require.NoError(t, err)
	require.Len(t, res.Trend, 7)
}

// TestExportCSV_BOMAndContent 四类 CSV 可导出，BOM 正确且行数可对账。
func TestExportCSV_BOMAndContent(t *testing.T) {
	svc, db := newTestService(t)
	// 应付导出读跨域只读表 approval_monthly_items（生产由 settlement-auditor 建）。
	if err := db.AutoMigrate(&model.CrossMonthlyItem{}); err != nil {
		t.Fatalf("automigrate approval_monthly_items: %v", err)
	}
	svc.SetCashSource(localSource(), nil)
	ctx := context.Background()
	batchID := seedPeriodFixture(t, svc)
	_, err := svc.FreezeBatch(ctx, "00", batchID, "u1")
	require.NoError(t, err)
	_, err = svc.CompleteBatch(ctx, "00", batchID, "u1")
	require.NoError(t, err)

	from, to := day(2026, 10, 1), day(2026, 10, 1)
	cases := []struct {
		kind       string
		wantRows   int
		wantHeader string
	}{
		{ExportDaily, 2, "period_start"},
		{ExportCashVariance, 1, "biz_date"},
		{ExportPayables, 0, "supplier"},
		{ExportDiffLog, 0, "batch_id"},
	}
	for _, tc := range cases {
		res, err := svc.ExportCSV(ctx, "00", tc.kind, from, to, "u1")
		require.NoError(t, err, tc.kind)
		require.Equal(t, tc.wantRows, res.Rows, tc.kind)
		require.True(t, bytes.HasPrefix(res.Data, []byte("\ufeff")), "%s 必须带 UTF-8 BOM", tc.kind)
		require.Contains(t, string(res.Data), tc.wantHeader, "%s 表头", tc.kind)
	}

	// 导出必须留审计（append-only）。
	var n int64
	require.NoError(t, svc.db.Model(&model.ReconAuditLog{}).
		Where("branch_id = ? AND object_type = ?", "00", "export").Count(&n).Error)
	require.EqualValues(t, len(cases), n)
}

// byAccountType 拆出现金与非现金快照。
func byAccountType(rows []model.ReconPeriodSnapshot) (cash, other model.ReconPeriodSnapshot) {
	for _, r := range rows {
		if r.AccountType == model.AccountTypeCash {
			cash = r
		} else {
			other = r
		}
	}
	return cash, other
}
