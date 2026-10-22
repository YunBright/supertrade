package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/pos/model"
	"github.com/YunBright/supertrade/internal/pos/service"
)

// 两行单的开单入参(金额互不相同,便于断言"只退了一行")。
//
//	行1 可口可乐  qty 2  amount 22.20
//	行2 酸奶      qty 1  amount  8.80
//	合计 31.00
func twoLineInput(opID string) service.CreateSaleInput {
	return service.CreateSaleInput{
		OpID:       opID,
		BranchID:   "10",
		MemberID:   "M0001",
		OperatorID: "u-1",
		Lines: []service.SaleLineInput{
			{
				ProductID: "00000297", ProductName: "可口可乐", ProductNo: "P0001",
				Barcode: "6900000000001", Unit: "瓶",
				Qty: decimal.NewFromInt(2), OriginalPriceYuan: decimal.NewFromInt(12),
				UnitPriceYuan: decimal.RequireFromString("11.10"),
				DiscountAmountYuan: decimal.RequireFromString("1.80"),
				AmountYuan:        decimal.RequireFromString("22.20"),
			},
			{
				ProductID: "00000003", ProductName: "酸奶", ProductNo: "P0002",
				Barcode: "6900000000002", Unit: "杯",
				Qty: decimal.NewFromInt(1), OriginalPriceYuan: decimal.NewFromInt(10),
				UnitPriceYuan: decimal.RequireFromString("8.80"),
				AmountYuan:    decimal.RequireFromString("8.80"),
			},
		},
		Payments: []service.PaymentInput{
			{
				PayWay: "RMB", AmountYuan: decimal.RequireFromString("31.00"),
				ExchangeRate: decimal.NewFromInt(1), ConvertedAmountYuan: decimal.RequireFromString("31.00"),
			},
		},
		TotalAmountYuan:  decimal.RequireFromString("31.00"),
		PaidAmountYuan:   decimal.RequireFromString("31.00"),
		ChangeAmountYuan: decimal.Zero,
		CreatedBy:        "tester",
	}
}

// refundEvent 读出最后一条 outbox 事件并解成 SalePaidEvent。
func refundEvent(t *testing.T, db *gorm.DB) service.SalePaidEvent {
	t.Helper()
	rows := outboxRows(t, db)
	require.NotEmpty(t, rows)
	var ev service.SalePaidEvent
	require.NoError(t, json.Unmarshal([]byte(rows[len(rows)-1].Payload), &ev))
	return ev
}

// ---- 行级部分退货(2026-10-22)----

// TestRefund_PartialLines_OnlyRefundsSelectedLines 锁"只退指定的行"。
//
// ⚠️ 这是**行级部分退货**的核心断言:
//   事件里**只能有被退的行** —— 多了就会让库存把没退的商品也回补一次。
func TestRefund_PartialLines_OnlyRefundsSelectedLines(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	order, _, err := svc.Create(context.Background(), twoLineInput("op-1"))
	require.NoError(t, err)

	// 行 id = <sale_id>-L<n> ⇒ 第 2 行是 L2。
	refunded, replayed, err := svc.Refund(context.Background(), service.RefundInput{
		OpID: "op-refund-1", SaleID: order.ID, BranchID: "10",
		OperatorID: "u-2", CreatedBy: "tester",
		Lines: []service.RefundLineInput{{LineID: order.ID + "-L2"}},
	})
	require.NoError(t, err)
	assert.False(t, replayed)
	assert.Equal(t, service.StatusPartiallyReturned, refunded.Status,
		"只退了 2 行中的一行 ⇒ 不能是 returned(那张单还没退完)")

	ev := refundEvent(t, db)
	assert.Equal(t, service.OrderStatusRefund, ev.OrderStatus)
	require.Len(t, ev.Lines, 1, "事件里**只能有被退的那一行**")
	assert.Equal(t, order.ID+"-L2", ev.Lines[0].LineID)
	assert.Equal(t, "00000003", ev.Lines[0].ProductID)
	assert.Equal(t, "-1", ev.Lines[0].Qty, "退 1 件 ⇒ 数量取负")
	assert.Equal(t, "-8.80", ev.Lines[0].AmountYuan)
	assert.Equal(t, "-8.80", ev.TotalAmountYuan, "本次退款额(不是原单合计)")

	// 已退量落库(超退守卫的依据)。
	var lines []model.SaleLine
	require.NoError(t, db.Where("sale_id = ?", order.ID).Order("id asc").Find(&lines).Error)
	require.Len(t, lines, 2)
	assert.True(t, lines[0].ReturnedQty.IsZero(), "没退的行不该有已退量")
	assert.Equal(t, "1", lines[1].ReturnedQty.String())
	assert.Equal(t, "8.80", lines[1].ReturnedAmountYuan.StringFixed(2))
}

// TestRefund_PartialRefundsProportionally 锁"退款额按**比例**摊到各支付方式"。
//
// ⚠️ 退 8.80 / 31.00 ⇒ 退款 8.80(单支付方式时就是全额)。
//   多支付方式时按比例分摊,最后一条吸收分位误差 ⇒ Σ 精确 = 退款额。
func TestRefund_PartialRefundsProportionally(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	in := twoLineInput("op-1")
	in.Payments = []service.PaymentInput{
		{PayWay: "RMB", AmountYuan: decimal.RequireFromString("20.00"),
			ExchangeRate: decimal.NewFromInt(1), ConvertedAmountYuan: decimal.RequireFromString("20.00")},
		{PayWay: "SWX", AmountYuan: decimal.RequireFromString("11.00"),
			ExchangeRate: decimal.NewFromInt(1), ConvertedAmountYuan: decimal.RequireFromString("11.00")},
	}
	order, _, err := svc.Create(context.Background(), in)
	require.NoError(t, err)

	_, _, err = svc.Refund(context.Background(), service.RefundInput{
		OpID: "op-refund-1", SaleID: order.ID, BranchID: "10", CreatedBy: "tester",
		Lines: []service.RefundLineInput{{LineID: order.ID + "-L2"}},
	})
	require.NoError(t, err)

	var pays []model.Payment
	require.NoError(t, db.Where("sale_id = ? AND id LIKE ?", order.ID, order.ID+"-RF%").
		Order("id asc").Find(&pays).Error)
	require.Len(t, pays, 2)
	// 8.80 / 31.00 ⇒ RMB 退 5.68(20×8.8/31),SWX 退 3.12(11×8.8/31)。
	assert.Equal(t, "-5.68", pays[0].AmountYuan.StringFixed(2))
	assert.Equal(t, "-3.12", pays[1].AmountYuan.StringFixed(2))
	sum := pays[0].AmountYuan.Add(pays[1].AmountYuan).Abs()
	assert.Equal(t, "8.80", sum.StringFixed(2), "Σ 退款额必须精确 = 本次退款额")
}

// TestRefund_ExceedsSoldRejected 锁"**超退被拒**"。
//
// ⚠️ 少了这条守卫,"退两次同一行"⇒ 库存凭空多出两件 + 退款额大于应收。
func TestRefund_ExceedsSoldRejected(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	order, _, err := svc.Create(context.Background(), twoLineInput("op-1"))
	require.NoError(t, err)

	// 行 1 只有 2 件,退 3 件 ⇒ 超退。
	_, _, err = svc.Refund(context.Background(), service.RefundInput{
		OpID: "op-refund-1", SaleID: order.ID, BranchID: "10", CreatedBy: "tester",
		Lines: []service.RefundLineInput{{LineID: order.ID + "-L1", Qty: decimal.NewFromInt(3)}},
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrRefundExceedsSold),
		"超退必须是 ErrRefundExceedsSold,实际: %v", err)
}

// TestRefund_PartialThenRemainder_Completes 锁"部分退完还能退剩下的,最后置 returned"。
func TestRefund_PartialThenRemainder_Completes(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	order, _, err := svc.Create(context.Background(), twoLineInput("op-1"))
	require.NoError(t, err)

	// 先退行 1 的一件(共 2 件)。
	_, _, err = svc.Refund(context.Background(), service.RefundInput{
		OpID: "op-refund-1", SaleID: order.ID, BranchID: "10", CreatedBy: "tester",
		Lines: []service.RefundLineInput{{LineID: order.ID + "-L1", Qty: decimal.NewFromInt(1)}},
	})
	require.NoError(t, err)

	// 再退行 1 的剩下 1 件 + 行 2 ⇒ 全部退完。
	refunded, _, err := svc.Refund(context.Background(), service.RefundInput{
		OpID: "op-refund-2", SaleID: order.ID, BranchID: "10", CreatedBy: "tester",
		Lines: []service.RefundLineInput{
			{LineID: order.ID + "-L1", Qty: decimal.NewFromInt(1)},
			{LineID: order.ID + "-L2"},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, service.StatusReturned, refunded.Status, "全部退完 ⇒ returned")

	// 已退量 = 原数量。
	var lines []model.SaleLine
	require.NoError(t, db.Where("sale_id = ?", order.ID).Order("id asc").Find(&lines).Error)
	require.Len(t, lines, 2)
	assert.Equal(t, "2", lines[0].ReturnedQty.String())
	assert.Equal(t, "22.20", lines[0].ReturnedAmountYuan.StringFixed(2))
	assert.Equal(t, "1", lines[1].ReturnedQty.String())
	assert.Equal(t, "8.80", lines[1].ReturnedAmountYuan.StringFixed(2))
}

// TestRefund_LineNotInSale 锁"要退的行不属于这张单 ⇒ 404 语义的错误"。
func TestRefund_LineNotInSale(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	order, _, err := svc.Create(context.Background(), twoLineInput("op-1"))
	require.NoError(t, err)

	_, _, err = svc.Refund(context.Background(), service.RefundInput{
		OpID: "op-refund-1", SaleID: order.ID, BranchID: "10", CreatedBy: "tester",
		Lines: []service.RefundLineInput{{LineID: "SO-other-L1"}},
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrLineNotFoundInSale),
		"必须是 ErrLineNotFoundInSale,实际: %v", err)
}

// ---- 挂账(实收不足)----

// TestCreate_CreditPaymentAllowsShortPaid 锁"实收不足 + 显式挂账收付 ⇒ 放行"。
//
// ⚠️ 以前这条被当成"找零为负"直接拒(409)⇒ 现实收银里的挂账单**无法表达**。
func TestCreate_CreditPaymentAllowsShortPaid(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	in := validInput("op-credit-1")
	// 应收 22.20,实收 20.00 ⇒ 欠 2.20。
	in.PaidAmountYuan = decimal.RequireFromString("20.00")
	in.ChangeAmountYuan = decimal.RequireFromString("-2.20")
	in.Payments = []service.PaymentInput{
		{PayWay: "RMB", AmountYuan: decimal.RequireFromString("20.00"),
			ExchangeRate: decimal.NewFromInt(1), ConvertedAmountYuan: decimal.RequireFromString("20.00")},
		{PayWay: service.PayWayCredit, AmountYuan: decimal.RequireFromString("2.20"),
			ExchangeRate: decimal.NewFromInt(1), ConvertedAmountYuan: decimal.RequireFromString("2.20")},
	}

	order, _, err := svc.Create(context.Background(), in)
	require.NoError(t, err, "显式给出挂账收付 ⇒ 放行")
	assert.Equal(t, "20.00", order.PaidAmountYuan.StringFixed(2), "实收就是实收(不含挂账)")
	assert.Equal(t, "-2.20", order.ChangeAmountYuan.StringFixed(2), "负找零 = 欠款")

	var pays []model.Payment
	require.NoError(t, db.Where("sale_id = ?", order.ID).Order("id asc").Find(&pays).Error)
	require.Len(t, pays, 2)
	assert.Equal(t, service.PayWayCredit, pays[1].PayWay)
	assert.Equal(t, "2.20", pays[1].AmountYuan.StringFixed(2))
}

// TestCreate_ShortPaidWithoutCreditRejected 锁"实收不足但**没给挂账收付** ⇒ 拒"。
//
// ⚠️ 这是关键:否则"实收不足"就是**沉默的负找零** ——
//   单子看起来收了钱、实际少收,账上没有任何东西说明差在哪。
func TestCreate_ShortPaidWithoutCreditRejected(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	in := validInput("op-short-1")
	in.PaidAmountYuan = decimal.RequireFromString("20.00")
	in.ChangeAmountYuan = decimal.RequireFromString("-2.20")
	in.Payments = []service.PaymentInput{
		{PayWay: "RMB", AmountYuan: decimal.RequireFromString("20.00"),
			ExchangeRate: decimal.NewFromInt(1), ConvertedAmountYuan: decimal.RequireFromString("20.00")},
	}

	_, _, err := svc.Create(context.Background(), in)
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrAmountMismatch),
		"必须是 ErrAmountMismatch,实际: %v", err)
}

// TestCreate_CreditAmountMustMatchDebt 锁"挂账额必须**精确**等于欠款"。
func TestCreate_CreditAmountMustMatchDebt(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	in := validInput("op-short-2")
	in.PaidAmountYuan = decimal.RequireFromString("20.00")
	in.ChangeAmountYuan = decimal.RequireFromString("-2.20")
	in.Payments = []service.PaymentInput{
		{PayWay: "RMB", AmountYuan: decimal.RequireFromString("20.00"),
			ExchangeRate: decimal.NewFromInt(1), ConvertedAmountYuan: decimal.RequireFromString("20.00")},
		// 挂账写成 1.00 ≠ 欠款 2.20。
		{PayWay: service.PayWayCredit, AmountYuan: decimal.RequireFromString("1.00"),
			ExchangeRate: decimal.NewFromInt(1), ConvertedAmountYuan: decimal.RequireFromString("1.00")},
	}

	_, _, err := svc.Create(context.Background(), in)
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrAmountMismatch),
		"挂账额对不上必须拒,实际: %v", err)
}
