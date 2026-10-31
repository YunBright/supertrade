package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/cashrecon/model"
)

// TestCreateReceipt_ConfirmedOnCreate 锁住"录入即确认 + 只收正数 + 渠道白名单"。
func TestCreateReceipt_ConfirmedOnCreate(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()

	row, err := svc.CreateReceipt(ctx, CreateReceiptInput{
		BranchID: "00", BizDate: day(2026, 10, 30), Amount: dec("88.88"),
		Method: model.MethodCash, Payer: "废品站", Reason: "纸皮变卖", Actor: "u1",
	})
	require.NoError(t, err)
	require.Equal(t, model.ReceiptStatusConfirmed, row.Status, "人工录入即确认")
	require.Equal(t, model.ReceiptSourceManual, row.Source)
	require.Equal(t, "u1", row.ConfirmedBy)
	require.Equal(t, "88.88", row.Amount.String())

	var audit int64
	require.NoError(t, db.Model(&model.ReconAuditLog{}).
		Where("branch_id = ? AND object_id = ? AND action = ?", "00", row.ID, "create").
		Count(&audit).Error)
	require.Equal(t, int64(1), audit, "录入必须留审计")

	// 金额必须为正。
	_, err = svc.CreateReceipt(ctx, CreateReceiptInput{
		BranchID: "00", BizDate: day(2026, 10, 30), Amount: dec("0"),
		Method: model.MethodCash, Actor: "u1",
	})
	require.ErrorIs(t, err, model.ErrInvalidInput)

	// 找零/未映射不是收款渠道。
	for _, m := range []string{model.MethodChange, model.MethodUnmapped, "nonsense"} {
		_, err = svc.CreateReceipt(ctx, CreateReceiptInput{
			BranchID: "00", BizDate: day(2026, 10, 30), Amount: dec("1"),
			Method: m, Actor: "u1",
		})
		require.ErrorIs(t, err, model.ErrInvalidInput, "method=%s 必须拒绝", m)
	}
}

// TestCreateReceipt_FrozenPeriod 锁住冻结区间 409 且不落库。
func TestCreateReceipt_FrozenPeriod(t *testing.T) {
	svc, db := newTestService(t)
	require.NoError(t, db.Create(&model.ReconPeriodLock{
		ID: newID(), BranchID: "00", BizDateFrom: day(2026, 10, 1), BizDateTo: day(2026, 10, 31),
		BatchID: "b1", LockedAt: testClock,
	}).Error)

	_, err := svc.CreateReceipt(context.Background(), CreateReceiptInput{
		BranchID: "00", BizDate: day(2026, 10, 30), Amount: dec("10"),
		Method: model.MethodWechat, Actor: "u1",
	})
	require.ErrorIs(t, err, model.ErrPeriodFrozen)

	var n int64
	require.NoError(t, db.Model(&model.ReconOtherReceipt{}).Count(&n).Error)
	require.Zero(t, n)
}

// TestListReceipts_FilterAndIsolation 锁住区间/渠道过滤与门店隔离。
func TestListReceipts_FilterAndIsolation(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, err := svc.CreateReceipt(ctx, CreateReceiptInput{
		BranchID: "00", BizDate: day(2026, 10, 29), Amount: dec("20"),
		Method: model.MethodCash, Actor: "u1",
	})
	require.NoError(t, err)
	_, err = svc.CreateReceipt(ctx, CreateReceiptInput{
		BranchID: "00", BizDate: day(2026, 10, 30), Amount: dec("30"),
		Method: model.MethodWechat, Actor: "u1",
	})
	require.NoError(t, err)
	_, err = svc.CreateReceipt(ctx, CreateReceiptInput{
		BranchID: "01", BizDate: day(2026, 10, 30), Amount: dec("40"),
		Method: model.MethodCash, Actor: "u1",
	})
	require.NoError(t, err)

	rows, total, err := svc.ListReceipts(ctx, "00", day(2026, 10, 27), day(2026, 10, 31), "", 50, 0)
	require.NoError(t, err)
	require.Equal(t, int64(2), total)
	require.Len(t, rows, 2)

	rows, total, err = svc.ListReceipts(ctx, "00", day(2026, 10, 27), day(2026, 10, 31), model.MethodCash, 50, 0)
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Equal(t, "cash", rows[0].Method)

	// 区间越界仍然按 31 天上限拒绝。
	_, _, err = svc.ListReceipts(ctx, "00", day(2026, 10, 1), day(2026, 11, 2), "", 50, 0)
	require.ErrorIs(t, err, model.ErrRangeTooLarge)

	// 跨门店不可读。
	_, err = svc.GetReceipt(ctx, "01", rows[0].ID)
	require.ErrorIs(t, err, model.ErrNotFound)
}

// TestAddAttachment_SizeAndCountLimits 锁住单文件上限与每单 5 个上限。
func TestAddAttachment_SizeAndCountLimits(t *testing.T) {
	db := newTestDB(t)
	cfg := DefaultConfig()
	cfg.Clock = func() time.Time { return testClock }
	cfg.AttachmentMaxBytes = 8
	svc := NewWithConfig(db, cfg)
	ctx := context.Background()

	receipt, err := svc.CreateReceipt(ctx, CreateReceiptInput{
		BranchID: "00", BizDate: day(2026, 10, 30), Amount: dec("10"),
		Method: model.MethodCash, Actor: "u1",
	})
	require.NoError(t, err)

	_, err = svc.AddAttachment(ctx, AddAttachmentInput{
		BranchID: "00", ReceiptID: receipt.ID, FileName: "big.jpg",
		ContentType: "image/jpeg", Data: make([]byte, 9), Actor: "u1",
	})
	require.ErrorIs(t, err, model.ErrInvalidInput, "超过单文件上限必须 400")

	_, err = svc.AddAttachment(ctx, AddAttachmentInput{
		BranchID: "00", ReceiptID: receipt.ID, FileName: "empty.jpg", Actor: "u1",
	})
	require.ErrorIs(t, err, model.ErrInvalidInput, "空附件必须 400")

	for i := 0; i < maxAttachmentsPerReceipt; i++ {
		_, err = svc.AddAttachment(ctx, AddAttachmentInput{
			BranchID: "00", ReceiptID: receipt.ID,
			FileName: "p.jpg", ContentType: "image/jpeg",
			Data: []byte("12345678"), Actor: "u1",
		})
		require.NoError(t, err, "第 %d 个附件应可上传", i+1)
	}
	_, err = svc.AddAttachment(ctx, AddAttachmentInput{
		BranchID: "00", ReceiptID: receipt.ID, FileName: "p6.jpg",
		ContentType: "image/jpeg", Data: []byte("12345678"), Actor: "u1",
	})
	require.ErrorIs(t, err, model.ErrInvalidInput, "第 6 个必须拒绝")

	metas, err := svc.ListAttachments(ctx, "00", receipt.ID)
	require.NoError(t, err)
	require.Len(t, metas, maxAttachmentsPerReceipt)
	for _, m := range metas {
		require.Empty(t, m.Data, "列表不得带二进制")
		require.Equal(t, int64(8), m.SizeBytes)
	}
}

// TestAttachment_CrossBranchAndFrozen 锁住跨门店读取 404 与冻结后禁止补凭证。
func TestAttachment_CrossBranchAndFrozen(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()

	receipt, err := svc.CreateReceipt(ctx, CreateReceiptInput{
		BranchID: "00", BizDate: day(2026, 10, 30), Amount: dec("10"),
		Method: model.MethodCash, Actor: "u1",
	})
	require.NoError(t, err)
	att, err := svc.AddAttachment(ctx, AddAttachmentInput{
		BranchID: "00", ReceiptID: receipt.ID, FileName: "p.jpg",
		ContentType: "image/jpeg", Data: []byte("abc"), Actor: "u1",
	})
	require.NoError(t, err)
	require.Equal(t, []byte("abc"), att.Data, "单笔取回应带二进制")

	// 跨门店下载必须 404（不能靠猜 id 拿到别家凭证）。
	_, err = svc.GetAttachment(ctx, "01", att.ID)
	require.ErrorIs(t, err, model.ErrNotFound)

	// 冻结后不允许再补凭证。
	require.NoError(t, db.Create(&model.ReconPeriodLock{
		ID: newID(), BranchID: "00", BizDateFrom: day(2026, 10, 1), BizDateTo: day(2026, 10, 31),
		BatchID: "b1", LockedAt: testClock,
	}).Error)
	_, err = svc.AddAttachment(ctx, AddAttachmentInput{
		BranchID: "00", ReceiptID: receipt.ID, FileName: "late.jpg",
		ContentType: "image/jpeg", Data: []byte("abc"), Actor: "u1",
	})
	require.ErrorIs(t, err, model.ErrPeriodFrozen)
}
