package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cashrecon/model"
)

// newPaymentsTestService 在测试库里补建跨域月度总表（只读的来源表）。
func newPaymentsTestService(t *testing.T) (*Service, *gorm.DB) {
	t.Helper()
	db := newTestDB(t)
	if err := db.AutoMigrate(&model.CrossMonthlyItem{}); err != nil {
		t.Fatalf("automigrate approval_monthly_items: %v", err)
	}
	cfg := DefaultConfig()
	cfg.Clock = func() time.Time { return testClock }
	return NewWithConfig(db, cfg), db
}

// seedMonthlyItems 写入审批月度总表（模拟 settlement-auditor 的数据）。
func seedMonthlyItems(t *testing.T, db *gorm.DB, rows ...model.CrossMonthlyItem) {
	t.Helper()
	for i := range rows {
		if rows[i].ID == "" {
			rows[i].ID = newID()
		}
		if rows[i].CreatedAt.IsZero() {
			rows[i].CreatedAt = testClock
		}
		require.NoError(t, db.Create(&rows[i]).Error)
	}
}

// TestSyncPayments_IdempotentAndFilters 锁住：
// 只同步已付 + 付款项 + 未排除终态；重复同步不新增、不覆盖人工字段。
func TestSyncPayments_IdempotentAndFilters(t *testing.T) {
	svc, db := newPaymentsTestService(t)
	ctx := context.Background()

	paidItemID := newID()
	seedMonthlyItems(t, db,
		model.CrossMonthlyItem{
			ID: paidItemID, BranchID: "00", ApprovalID: "a1", ItemID: "i1",
			Direction: crossDirectionPayment, ItemType: "货款", AmountYuan: dec("100"),
			PayState: crossPayStatePaid, InstanceStatus: "human_approved",
		},
		model.CrossMonthlyItem{
			ID: newID(), BranchID: "00", ApprovalID: "a2", ItemID: "i2",
			Direction: crossDirectionPayment, ItemType: "水电费", AmountYuan: dec("50"),
			PayState: crossPayStatePaid, InstanceStatus: "human_rejected", // 终态：不能进台账
		},
		model.CrossMonthlyItem{
			ID: newID(), BranchID: "00", ApprovalID: "a3", ItemID: "i3",
			Direction: crossDirectionPayment, ItemType: "租金", AmountYuan: dec("20"),
			PayState: crossPayStateUnpaid, InstanceStatus: "human_approved", // 未付：不进台账
		},
		model.CrossMonthlyItem{
			ID: newID(), BranchID: "00", ApprovalID: "a4", ItemID: "i4",
			Direction: "receipt", ItemType: "陈列费", AmountYuan: dec("10"),
			PayState: crossPayStatePaid, InstanceStatus: "human_approved", // 收款项：不是付款
		},
		model.CrossMonthlyItem{
			ID: newID(), BranchID: "01", ApprovalID: "a5", ItemID: "i5",
			Direction: crossDirectionPayment, ItemType: "货款", AmountYuan: dec("30"),
			PayState: crossPayStatePaid, InstanceStatus: "human_approved", // 别家门店
		},
	)

	res, err := svc.SyncPayments(ctx, "00")
	require.NoError(t, err)
	require.Equal(t, 1, res.Scanned, "只有一条同时满足 已付+付款项+非终态+本门店")
	require.Equal(t, 1, res.Inserted)

	var pay model.ReconPaymentRecord
	// source_id 记的是月度总表的 item_id（不是行 ID），与月度总表的幂等键对齐。
	require.NoError(t, db.Where("branch_id = ? AND source = ? AND source_id = ?",
		"00", model.PaymentSourceWecom, "i1").First(&pay).Error)
	require.Equal(t, "货款", pay.SupplierName, "供应商名按 item_type 落到台账")
	require.Equal(t, "100", pay.Amount.String())
	require.False(t, pay.Verified)

	// 人工把台账字段改掉后再次同步：必须原样保留（同步只补缺失行）。
	pay.SupplierName = "真实供应商"
	pay.Verified = true
	require.NoError(t, db.Model(&model.ReconPaymentRecord{}).Where("id = ?", pay.ID).
		Updates(map[string]any{"supplier_name": pay.SupplierName, "verified": pay.Verified}).Error)

	again, err := svc.SyncPayments(ctx, "00")
	require.NoError(t, err)
	require.Equal(t, 1, again.Scanned)
	require.Equal(t, 0, again.Inserted, "已存在的行不得重复插入")

	var after model.ReconPaymentRecord
	require.NoError(t, db.Where("id = ?", pay.ID).First(&after).Error)
	require.Equal(t, "真实供应商", after.SupplierName, "同步不得覆盖人工字段")
	require.True(t, after.Verified)
}

// seedPaymentFlow 写入一条银行流水（核销的对手方）。
func seedPaymentFlow(t *testing.T, db *gorm.DB, channel, amount, fee string) model.ReconBankFlow {
	t.Helper()
	flow := model.ReconBankFlow{
		ID: newID(), BranchID: "00", Channel: channel, ValueDate: day(2026, 10, 30),
		Amount: dec(amount), Fee: dec(fee), DedupeKey: newID(), CreatedAt: testClock,
	}
	require.NoError(t, db.Create(&flow).Error)
	return flow
}

// TestVerifyPayment_MatchesGrossAmountAndChannel 锁住：
// 金额按「流水 + 手续费」毛额比对，渠道等价放行，核销不动银行流水。
func TestVerifyPayment_MatchesGrossAmountAndChannel(t *testing.T) {
	svc, db := newPaymentsTestService(t)
	ctx := context.Background()

	pay, err := svc.RegisterPayment(ctx, RegisterPaymentInput{
		BranchID: "00", SupplierName: "供应商A", Amount: dec("100"),
		Method: model.MethodBankCard, Actor: "u1",
	})
	require.NoError(t, err)

	flow := seedPaymentFlow(t, db, "bank", "99.99", "0.01")
	require.Equal(t, "99.99", flow.Amount.String())

	verified, err := svc.VerifyPayment(ctx, VerifyPaymentInput{
		BranchID: "00", PaymentID: pay.ID, BankFlowID: flow.ID, Actor: "u2",
	})
	require.NoError(t, err)
	require.True(t, verified.Verified)
	require.Equal(t, flow.ID, verified.BankFlowID)
	require.Equal(t, "u2", verified.VerifiedBy)

	// 银行流水本身不得被改（导入即事实）。
	var after model.ReconBankFlow
	require.NoError(t, db.Where("id = ?", flow.ID).First(&after).Error)
	require.Equal(t, "99.99", after.Amount.String())
	require.Equal(t, "0.01", after.Fee.String(), "手续费必须单列，不能被吞掉凑平")

	// 已核销再核销 ⇒ 400。
	_, err = svc.VerifyPayment(ctx, VerifyPaymentInput{
		BranchID: "00", PaymentID: pay.ID, BankFlowID: flow.ID, Actor: "u2",
	})
	require.ErrorIs(t, err, model.ErrInvalidInput)
}

// TestVerifyPayment_RejectsAmountAndChannelMismatch 锁住核销的两道闸。
func TestVerifyPayment_RejectsAmountAndChannelMismatch(t *testing.T) {
	svc, db := newPaymentsTestService(t)
	ctx := context.Background()

	pay, err := svc.RegisterPayment(ctx, RegisterPaymentInput{
		BranchID: "00", SupplierName: "供应商A", Amount: dec("100"),
		Method: model.MethodWechat, Actor: "u1",
	})
	require.NoError(t, err)

	// 差额 0.02 > 0.01 ⇒ 拒绝。
	badAmount := seedPaymentFlow(t, db, "wechat", "99.98", "0")
	_, err = svc.VerifyPayment(ctx, VerifyPaymentInput{
		BranchID: "00", PaymentID: pay.ID, BankFlowID: badAmount.ID, Actor: "u2",
	})
	require.ErrorIs(t, err, model.ErrInvalidInput)

	// 金额一致但渠道不一致 ⇒ 拒绝。
	badChannel := seedPaymentFlow(t, db, "bank", "100", "0")
	_, err = svc.VerifyPayment(ctx, VerifyPaymentInput{
		BranchID: "00", PaymentID: pay.ID, BankFlowID: badChannel.ID, Actor: "u2",
	})
	require.ErrorIs(t, err, model.ErrInvalidInput)

	// 跨门店核销 ⇒ 404（流水与付款都必须属于本门店）。
	_, err = svc.VerifyPayment(ctx, VerifyPaymentInput{
		BranchID: "01", PaymentID: pay.ID, BankFlowID: badChannel.ID, Actor: "u2",
	})
	require.ErrorIs(t, err, model.ErrNotFound)

	// 别家门店的流水也拿不到。
	other := model.ReconBankFlow{
		ID: newID(), BranchID: "01", Channel: "wechat", ValueDate: day(2026, 10, 30),
		Amount: dec("100"), DedupeKey: newID(), CreatedAt: testClock,
	}
	require.NoError(t, db.Create(&other).Error)
	_, err = svc.VerifyPayment(ctx, VerifyPaymentInput{
		BranchID: "00", PaymentID: pay.ID, BankFlowID: other.ID, Actor: "u2",
	})
	require.ErrorIs(t, err, model.ErrNotFound)
}

// TestRegisterPayment_ValidationAndFrozen 锁住手工登记校验与冻结 409。
func TestRegisterPayment_ValidationAndFrozen(t *testing.T) {
	svc, db := newPaymentsTestService(t)
	ctx := context.Background()

	_, err := svc.RegisterPayment(ctx, RegisterPaymentInput{
		BranchID: "00", Amount: dec("0"), Actor: "u1",
	})
	require.ErrorIs(t, err, model.ErrInvalidInput, "金额必须为正")

	_, err = svc.RegisterPayment(ctx, RegisterPaymentInput{
		BranchID: "00", Amount: dec("1"), Method: model.MethodUnmapped, Actor: "u1",
	})
	require.ErrorIs(t, err, model.ErrInvalidInput, "未映射不是付款渠道")

	require.NoError(t, db.Create(&model.ReconPeriodLock{
		ID: newID(), BranchID: "00", BizDateFrom: day(2026, 10, 1), BizDateTo: day(2026, 10, 31),
		BatchID: "b1", LockedAt: testClock,
	}).Error)
	d := day(2026, 10, 30)
	_, err = svc.RegisterPayment(ctx, RegisterPaymentInput{
		BranchID: "00", Amount: dec("10"), Method: model.MethodCash,
		PayDate: &d, Actor: "u1",
	})
	require.ErrorIs(t, err, model.ErrPeriodFrozen)

	var n int64
	require.NoError(t, db.Model(&model.ReconPaymentRecord{}).Count(&n).Error)
	require.Zero(t, n)
}

// TestListPayments_Filters 锁住 source / verified 过滤与门店隔离。
func TestListPayments_Filters(t *testing.T) {
	svc, _ := newPaymentsTestService(t)
	ctx := context.Background()

	m1, err := svc.RegisterPayment(ctx, RegisterPaymentInput{
		BranchID: "00", SupplierName: "A", Amount: dec("10"), Actor: "u1",
	})
	require.NoError(t, err)
	_, err = svc.RegisterPayment(ctx, RegisterPaymentInput{
		BranchID: "00", SupplierName: "B", Amount: dec("20"), Actor: "u1",
	})
	require.NoError(t, err)
	_, err = svc.RegisterPayment(ctx, RegisterPaymentInput{
		BranchID: "01", SupplierName: "C", Amount: dec("30"), Actor: "u1",
	})
	require.NoError(t, err)

	rows, total, err := svc.ListPayments(ctx, PaymentQuery{BranchID: "00"})
	require.NoError(t, err)
	require.Equal(t, int64(2), total)
	require.Len(t, rows, 2)

	rows, total, err = svc.ListPayments(ctx, PaymentQuery{BranchID: "00", Source: model.PaymentSourceWecom})
	require.NoError(t, err)
	require.Zero(t, total)
	require.Empty(t, rows)

	_, _, err = svc.ListPayments(ctx, PaymentQuery{BranchID: "00", Source: "nonsense"})
	require.ErrorIs(t, err, model.ErrInvalidInput)

	// 跨门店不可读。
	_, err = svc.GetPayment(ctx, "01", m1.ID)
	require.ErrorIs(t, err, model.ErrNotFound)
}

// TestSupplierPayables_GroupsAndExcludes 锁住应付聚合口径：
// 未付按 item_type、已付按 supplier_name；终态不进应付；空键归一。
func TestSupplierPayables_GroupsAndExcludes(t *testing.T) {
	svc, db := newPaymentsTestService(t)
	ctx := context.Background()

	seedMonthlyItems(t, db,
		model.CrossMonthlyItem{
			BranchID: "00", ApprovalID: "a1", ItemID: "i1", Direction: crossDirectionPayment,
			ItemType: "货款", AmountYuan: dec("100"), PayState: crossPayStateUnpaid,
			InstanceStatus: "human_approved",
		},
		model.CrossMonthlyItem{
			BranchID: "00", ApprovalID: "a2", ItemID: "i2", Direction: crossDirectionPayment,
			ItemType: "水电费", AmountYuan: dec("50"), PayState: crossPayStateUnpaid,
			InstanceStatus: "human_approved",
		},
		model.CrossMonthlyItem{
			BranchID: "00", ApprovalID: "a3", ItemID: "i3", Direction: crossDirectionPayment,
			ItemType: "货款", AmountYuan: dec("100"), PayState: crossPayStateUnpaid,
			InstanceStatus: "rejected", // 终态：不计入应付
		},
		model.CrossMonthlyItem{
			BranchID: "00", ApprovalID: "a4", ItemID: "i4", Direction: "receipt",
			ItemType: "陈列费", AmountYuan: dec("10"), PayState: crossPayStateUnpaid,
			InstanceStatus: "human_approved", // 收款项：不是应付
		},
	)

	// 已付：一笔同步来的（supplier_name=货款）+ 一笔手工（供应商A）。
	require.NoError(t, db.Create(&model.ReconPaymentRecord{
		ID: newID(), BranchID: "00", Source: model.PaymentSourceWecom, SourceID: newID(),
		SupplierName: "货款", Amount: dec("100"), CreatedAt: testClock, UpdatedAt: testClock,
	}).Error)
	require.NoError(t, db.Create(&model.ReconPaymentRecord{
		ID: newID(), BranchID: "00", Source: model.PaymentSourceManual, SourceID: newID(),
		SupplierName: "供应商A", Amount: dec("20"), CreatedAt: testClock, UpdatedAt: testClock,
	}).Error)

	res, err := svc.SupplierPayables(ctx, "00")
	require.NoError(t, err)
	require.Equal(t, "150", res.TotalUnpaid.String())
	require.Equal(t, "120", res.TotalPaid.String())

	// 键按文本归组：货款 / 水电费 / 供应商A（排序稳定）。
	require.Len(t, res.Items, 3)
	byName := map[string]SupplierPayable{}
	for _, it := range res.Items {
		byName[it.Supplier] = it
	}
	require.Equal(t, "100", byName["货款"].UnpaidAmount.String())
	require.Equal(t, 1, byName["货款"].UnpaidCount, "终态的 100 元不得计入")
	require.Equal(t, "100", byName["货款"].PaidAmount.String())
	require.Equal(t, "50", byName["水电费"].UnpaidAmount.String())
	require.Equal(t, "20", byName["供应商A"].PaidAmount.String())
}

// TestSupplierPayables_BlankKeyIsUnnamed 锁住空文本统一归到「未命名」。
func TestSupplierPayables_BlankKeyIsUnnamed(t *testing.T) {
	svc, db := newPaymentsTestService(t)
	ctx := context.Background()

	require.NoError(t, db.Create(&model.ReconPaymentRecord{
		ID: newID(), BranchID: "00", Source: model.PaymentSourceManual, SourceID: newID(),
		SupplierName: "  ", Amount: dec("10"), CreatedAt: testClock, UpdatedAt: testClock,
	}).Error)

	res, err := svc.SupplierPayables(ctx, "00")
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	require.Equal(t, "未命名", res.Items[0].Supplier)
	require.Equal(t, "10", res.TotalPaid.String())
}

// TestAssertChannelMatches_Aliases 锁住渠道等价表（放行等价、拒绝跨类）。
func TestAssertChannelMatches_Aliases(t *testing.T) {
	require.NoError(t, assertChannelMatches(model.MethodBankCard, "bank"))
	require.NoError(t, assertChannelMatches(model.MethodBankCard, "transfer"))
	require.NoError(t, assertChannelMatches(model.MethodWechat, "wxpay"))
	require.NoError(t, assertChannelMatches(model.MethodCash, "CASH"))
	require.ErrorIs(t, assertChannelMatches(model.MethodWechat, "bank"), model.ErrInvalidInput)
	require.ErrorIs(t, assertChannelMatches(model.MethodCash, "wechat"), model.ErrInvalidInput)
	// 未登记 method 的审批行：只要求流水有渠道。
	require.NoError(t, assertChannelMatches("", "wechat"))
	require.ErrorIs(t, assertChannelMatches("", ""), model.ErrInvalidInput)
}
