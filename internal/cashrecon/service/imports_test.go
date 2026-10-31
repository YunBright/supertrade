package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/cashrecon/model"
)

const bankCSV = `channel,value_date,amount,fee,external_id
wechat,2026-10-30,100.00,0.50,EX001
bank_card,2026-10-30,200.00,0,EX002
wechat,2026-10-29,50.00,0,EX003
`

// TestImportBankCSV_Idempotent 锁住"重复导入只新增 0 行"。
func TestImportBankCSV_Idempotent(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()

	res, err := svc.ImportBankFlows(ctx, ImportBankInput{
		BranchID: "00", FileName: "2026-10.csv", CSV: bankCSV, Actor: "u1",
	})
	require.NoError(t, err)
	require.Equal(t, 3, res.Inserted)
	require.Equal(t, 0, res.Skipped)
	require.Equal(t, 0, res.Failed)
	require.Equal(t, 3, res.Import.RowOK)
	require.Equal(t, 3, res.Import.RowTotal)

	again, err := svc.ImportBankFlows(ctx, ImportBankInput{
		BranchID: "00", FileName: "2026-10.csv", CSV: bankCSV, Actor: "u1",
	})
	require.NoError(t, err)
	require.Equal(t, 0, again.Inserted, "同一份文件重复导入不得新增")
	require.Equal(t, 3, again.Skipped)

	var count int64
	require.NoError(t, db.Model(&model.ReconBankFlow{}).Where("branch_id = ?", "00").Count(&count).Error)
	require.Equal(t, int64(3), count)

	// 与产生扣款的手续费单列：amount=100、fee=0.5（不允许凑平成 99.5）。
	var flow model.ReconBankFlow
	require.NoError(t, db.Where("branch_id = ? AND external_id = ?", "00", "EX001").First(&flow).Error)
	require.Equal(t, "100", flow.Amount.String())
	require.Equal(t, "0.5", flow.Fee.String())
}

// TestImportBankCSV_RejectsMissingHeader 锁住表头契约。
func TestImportBankCSV_RejectsMissingHeader(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.ImportBankFlows(context.Background(), ImportBankInput{
		BranchID: "00", CSV: "日期,金额\n2026-10-30,100\n", Actor: "u1",
	})
	require.ErrorIs(t, err, model.ErrInvalidInput)
}

// TestImportBankCSV_RowErrorsCounted 锁住"坏行逐条计数，好行照常入库"。
func TestImportBankCSV_RowErrorsCounted(t *testing.T) {
	svc, _ := newTestService(t)
	csv := `channel,value_date,amount
wechat,2026-10-30,100.00
bank_card,2026/13/45,200.00
`
	res, err := svc.ImportBankFlows(context.Background(), ImportBankInput{
		BranchID: "00", CSV: csv, Actor: "u1",
	})
	require.NoError(t, err)
	require.Equal(t, 1, res.Inserted)
	require.Equal(t, 1, res.Failed)
	require.NotEmpty(t, res.Errors)
}

// TestImportBankJSONRows 锁住 JSON rows 路径（Flutter 侧解析后直接提交）。
func TestImportBankJSONRows(t *testing.T) {
	svc, _ := newTestService(t)
	res, err := svc.ImportBankFlows(context.Background(), ImportBankInput{
		BranchID: "00",
		Rows: []ImportBankRow{
			{Channel: "wechat", ValueDate: "2026-10-30", Amount: dec("88.88"), Fee: dec("0.12")},
			{Channel: "wechat", ValueDate: "2026/10/30", Amount: dec("88.88"), Fee: dec("0.12")},
		},
		Actor: "u1",
	})
	require.NoError(t, err)
	// 日期写法不同但归一化后同一条 ⇒ 只有第一行入库。
	require.Equal(t, 1, res.Inserted)
	require.Equal(t, 1, res.Skipped)
}

// TestImportBank_RejectsFrozenPeriod 锁住冻结区间整批 409。
func TestImportBank_RejectsFrozenPeriod(t *testing.T) {
	svc, db := newTestService(t)
	require.NoError(t, db.Create(&model.ReconPeriodLock{
		ID: newID(), BranchID: "00", BizDateFrom: day(2026, 10, 1), BizDateTo: day(2026, 10, 31),
		BatchID: "b1", LockedAt: testClock,
	}).Error)

	_, err := svc.ImportBankFlows(context.Background(), ImportBankInput{
		BranchID: "00", CSV: bankCSV, Actor: "u1",
	})
	require.ErrorIs(t, err, model.ErrPeriodFrozen)

	var count int64
	require.NoError(t, db.Model(&model.ReconBankFlow{}).Where("branch_id = ?", "00").Count(&count).Error)
	require.Zero(t, count, "冻结区间内不允许写入任何流水")
}

// TestListImports_ReturnsLedger 锁住导入台账可回查。
func TestListImports_ReturnsLedger(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	res, err := svc.ImportBankFlows(ctx, ImportBankInput{BranchID: "00", CSV: bankCSV, Actor: "u1"})
	require.NoError(t, err)

	rows, err := svc.ListImports(ctx, "00", 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	got, err := svc.GetImport(ctx, "00", res.Import.ID)
	require.NoError(t, err)
	require.Equal(t, res.Import.ID, got.ID)

	_, err = svc.GetImport(ctx, "01", res.Import.ID)
	require.ErrorIs(t, err, model.ErrNotFound, "跨门店不可读")
}
