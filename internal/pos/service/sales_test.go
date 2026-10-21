package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
	_ "modernc.org/sqlite" // 注册驱动名 "sqlite"(纯 Go 驱动,无需 CGO)

	"github.com/YunBright/supertrade/internal/pos/model"
	"github.com/YunBright/supertrade/internal/pos/service"
	"github.com/YunBright/supertrade/pkg/outbox"
)

// newTestDB 构造**文件型** SQLite + AutoMigrate。
//
// ⚠️ 必须是文件型(不是 :memory:)——
//
//	`:memory:` 的每个**连接**都是独立的库 ⇒ 并发事务会看到不同的库,
//	"并发开单"那种用例会假绿(每个 goroutine 各自建了一套表)。
//	文件型 + MaxOpenConns(1) ⇒ 所有连接看到同一个库。
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "pos.db")
	dialector := sqlite.Dialector{DriverName: "sqlite", DSN: dsn}
	// ⚠️ Silent logger:findByOpID 的"查不到"是**正常路径**(幂等快速路径),
	//   默认 logger 会把它打成红色 ERROR,刷屏且误导。
	db, err := gorm.Open(dialector, &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	require.NoError(t, err, "open sqlite")
	require.NoError(t, db.AutoMigrate(
		&model.SalesOrder{},
		&model.SaleLine{},
		&model.Payment{},
		&outbox.Outbox{},
	), "auto migrate")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	// ⚠️ 必须关连接 —— 否则 TempDir 清理时文件被占用(Windows 上会直接判测试失败)。
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// validInput 造一份自洽的开单入参(合计 = 明细之和;找零 = 实收 - 合计)。
func validInput(opID string) service.CreateSaleInput {
	return service.CreateSaleInput{
		OpID:       opID,
		BranchID:   "10",
		MemberID:   "M0001",
		ShiftID:    "SH20261021000001",
		OperatorID: "u-1",
		Lines: []service.SaleLineInput{
			{
				ProductID:          "00000297",
				ProductName:        "可口可乐",
				ProductNo:          "P0001",
				Barcode:            "6900000000001",
				Unit:               "瓶",
				Qty:                decimal.NewFromInt(2),
				OriginalPriceYuan:  decimal.NewFromInt(12),
				UnitPriceYuan:      decimal.RequireFromString("11.10"),
				DiscountAmountYuan: decimal.RequireFromString("1.80"),
				AmountYuan:         decimal.RequireFromString("22.20"),
			},
		},
		Payments: []service.PaymentInput{
			{
				PayWay:              "RMB",
				AmountYuan:          decimal.RequireFromString("22.20"),
				ExchangeRate:        decimal.NewFromInt(1),
				ConvertedAmountYuan: decimal.RequireFromString("22.20"),
			},
		},
		TotalAmountYuan:    decimal.RequireFromString("22.20"),
		DiscountAmountYuan: decimal.RequireFromString("1.80"),
		PaidAmountYuan:     decimal.RequireFromString("50.00"),
		ChangeAmountYuan:   decimal.RequireFromString("27.80"),
		CreatedBy:          "tester",
	}
}

// countRows 数某张表的行数。
func countRows(t *testing.T, db *gorm.DB, model any) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Model(model).Count(&n).Error)
	return n
}

// outboxRows 读出发件箱全部行。
func outboxRows(t *testing.T, db *gorm.DB) []outbox.Outbox {
	t.Helper()
	var rows []outbox.Outbox
	require.NoError(t, db.Order("id asc").Find(&rows).Error)
	return rows
}

// ---- 开单:落库 + 事件 ----

func TestCreate_WritesOrderLinesPaymentsAndOutbox(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	in := validInput("op-1")
	order, replayed, err := svc.Create(context.Background(), in)
	require.NoError(t, err)
	assert.False(t, replayed, "首次提交不是重放")
	assert.Equal(t, "10", order.BranchID)
	assert.Equal(t, "M0001", order.MemberID)
	assert.Equal(t, "paid", order.Status)

	assert.EqualValues(t, 1, countRows(t, db, &model.SalesOrder{}))
	assert.EqualValues(t, 1, countRows(t, db, &model.SaleLine{}))
	assert.EqualValues(t, 1, countRows(t, db, &model.Payment{}))

	rows := outboxRows(t, db)
	require.Len(t, rows, 1, "必须正好入队一条事件")
	assert.Equal(t, service.TopicSalePaid, rows[0].Topic)
	assert.Equal(t, "pos:op-1", rows[0].ID, "outbox 行 id 带服务命名空间(同库多服务共用一张表)")
	assert.Equal(t, "10", rows[0].BranchID)

	var ev service.SalePaidEvent
	require.NoError(t, json.Unmarshal([]byte(rows[0].Payload), &ev))
	assert.Equal(t, "op-1", ev.OpID, "payload 里的 op_id 必须与请求一致")
	assert.Equal(t, order.ID, ev.SaleID)
	assert.Equal(t, "10", ev.BranchID)
	assert.Equal(t, "M0001", ev.MemberID)
	assert.Equal(t, "u-1", ev.OperatorID)
	assert.Equal(t, "22.20", ev.TotalAmountYuan)
	assert.Equal(t, service.OrderStatusSale, ev.OrderStatus)
	require.Len(t, ev.Lines, 1)
	assert.Equal(t, order.ID+"-L1", ev.Lines[0].LineID)
	assert.Equal(t, "00000297", ev.Lines[0].ProductID)
	assert.Equal(t, "2", ev.Lines[0].Qty)
	assert.Equal(t, "11.10", ev.Lines[0].UnitPriceYuan)
	assert.Equal(t, "22.20", ev.Lines[0].AmountYuan)
	// paid_at 必须是 RFC3339 且带 +08:00。
	_, err = time.Parse(time.RFC3339, ev.PaidAt)
	require.NoError(t, err, "paid_at 必须是 RFC3339:%s", ev.PaidAt)
	assert.Contains(t, ev.PaidAt, "+08:00")
}

func TestCreate_AmountsAreDecimalNotFloat(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	_, _, err := svc.Create(context.Background(), validInput("op-1"))
	require.NoError(t, err)

	var order model.SalesOrder
	require.NoError(t, db.First(&order).Error)
	assert.Equal(t, "22.2000", order.TotalAmountYuan.StringFixed(4), "DB 里是 numeric(18,4)")
	assert.True(t, order.TotalAmountYuan.Equal(decimal.RequireFromString("22.2")))

	// JSON 里金额必须是**字符串**(Dart 侧不能变 double)。
	raw, err := json.Marshal(order)
	require.NoError(t, err)
	var generic map[string]any
	require.NoError(t, json.Unmarshal(raw, &generic))
	assert.IsType(t, "", generic["total_amount_yuan"], "金额在 JSON 里必须是字符串")
}

// ---- op_id 幂等 ----

func TestCreate_SameOpID_ReplaysFirstSale(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	in := validInput("op-dup")
	first, replayed, err := svc.Create(context.Background(), in)
	require.NoError(t, err)
	assert.False(t, replayed)

	second, replayed2, err := svc.Create(context.Background(), in)
	require.NoError(t, err, "重放**不报错**")
	assert.True(t, replayed2, "重放必须被标记")
	assert.Equal(t, first.ID, second.ID, "重放返回首次那张单(同一个 sale_id)")

	assert.EqualValues(t, 1, countRows(t, db, &model.SalesOrder{}), "重放不得新建单")
	assert.EqualValues(t, 1, countRows(t, db, &model.SaleLine{}))
	assert.EqualValues(t, 1, countRows(t, db, &model.Payment{}))
	assert.Len(t, outboxRows(t, db), 1, "重放不得重复发事件")
}

// TestCreate_SameOpID_DifferentContent_StillReplaysFirstSale 钉死"首次结果优先"。
func TestCreate_SameOpID_DifferentContent_StillReplaysFirstSale(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	first, _, err := svc.Create(context.Background(), validInput("op-dup"))
	require.NoError(t, err)

	other := validInput("op-dup")
	other.MemberID = "M9999"
	other.PaidAmountYuan = decimal.RequireFromString("22.20")
	other.ChangeAmountYuan = decimal.Zero
	second, replayed, err := svc.Create(context.Background(), other)
	require.NoError(t, err)
	assert.True(t, replayed)
	assert.Equal(t, first.ID, second.ID)
	assert.Equal(t, "M0001", second.MemberID, "返回的是**首次**那张单的内容")
}

// ---- 事务原子性 ----

// TestCreate_OutboxWriteFails_RollsBackEverything 是"事件写不进去 ⇒ 业务也
// 不落"的守卫测试。
//
// ⇒ 没有它,一个"只写业务不写事件"的实现会全绿,而生产上丢一条 sale.paid
// = 库存不扣 + 积分不加,且**没有任何日志说这件事**。
func TestCreate_OutboxWriteFails_RollsBackEverything(t *testing.T) {
	db := newTestDB(t)
	// 让 outbox 的 INSERT 失败(模拟发件箱写不进去)。
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register(
		"test:fail_outbox_insert",
		func(tx *gorm.DB) {
			if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "outbox" {
				tx.AddError(errors.New("boom: 发件箱写不进去"))
			}
		},
	))

	svc := service.NewSalesService(db)
	_, _, err := svc.Create(context.Background(), validInput("op-fail"))
	require.Error(t, err, "发件箱写失败必须让整笔开单失败")

	assert.EqualValues(t, 0, countRows(t, db, &model.SalesOrder{}), "业务行不得落下")
	assert.EqualValues(t, 0, countRows(t, db, &model.SaleLine{}), "明细不得落下")
	assert.EqualValues(t, 0, countRows(t, db, &model.Payment{}), "收付不得落下")
	assert.EqualValues(t, 0, countRows(t, db, &outbox.Outbox{}), "outbox 表里查不到")
}

// ---- 单号 ----

func TestCreate_SaleIDFormat(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	order, _, err := svc.Create(context.Background(), validInput("op-1"))
	require.NoError(t, err)
	assert.Regexp(t, `^SO\d{8}\d{6}$`, order.ID, "单号格式 SO<yyyymmdd><6 位序号>")
	assert.Equal(t, order.ID[:10], "SO"+order.ID[2:10], "前缀是 SO + yyyymmdd")
}

func TestCreate_ConcurrentSaleIDsAreUnique(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	const n = 12
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		ids  []string
		errs []error
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			in := validInput(fmt.Sprintf("op-conc-%d", i))
			order, _, err := svc.Create(context.Background(), in)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			ids = append(ids, order.ID)
		}(i)
	}
	wg.Wait()

	require.Empty(t, errs, "并发开单不应报错:%v", errs)
	require.Len(t, ids, n)
	seen := map[string]bool{}
	for _, id := range ids {
		assert.False(t, seen[id], "单号撞了:%s", id)
		seen[id] = true
	}
	assert.EqualValues(t, n, countRows(t, db, &model.SalesOrder{}), "每笔开单各落一张单")
}

// TestSaleIDGen_ConcurrentSameProcessUnique 直测"并发开单 ⇒ 单号不撞"。
//
// ⚠️ 每个 goroutine 必须用**不同的 op_id** ——
//
//	同一个 op_id 走的是幂等路径,返回**同一张单**(同一个 sale_id),
//	那是正确行为,不是撞号。
func TestSaleIDGen_ConcurrentSameProcessUnique(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	const n = 30
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		ids  []string
		errs []error
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			in := validInput(fmt.Sprintf("op-gen-%d", i))
			order, _, err := svc.Create(context.Background(), in)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			ids = append(ids, order.ID)
		}(i)
	}
	wg.Wait()

	require.Empty(t, errs, "并发开单不应报错:%v", errs)
	require.Len(t, ids, n)
	seen := map[string]bool{}
	for _, id := range ids {
		assert.False(t, seen[id], "单号撞了:%s", id)
		seen[id] = true
	}
}

// ---- 找零与金额自洽 ----

func TestCreate_ChangeAmountMustEqualPaidMinusTotal(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	in := validInput("op-change")
	in.ChangeAmountYuan = decimal.RequireFromString("0")
	_, _, err := svc.Create(context.Background(), in)
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrAmountMismatch), "找零不自洽必须是 ErrAmountMismatch")

	// 正确的找零能过。
	ok := validInput("op-change-ok")
	order, _, err := svc.Create(context.Background(), ok)
	require.NoError(t, err)
	assert.Equal(t, "27.80", order.ChangeAmountYuan.StringFixed(2))
	assert.True(t, order.ChangeAmountYuan.Equal(
		order.PaidAmountYuan.Sub(order.TotalAmountYuan)),
		"找零 = 实收 - 合计")
}

func TestCreate_TotalMustEqualSumOfLines(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	in := validInput("op-total")
	in.TotalAmountYuan = decimal.RequireFromString("20.00")
	in.ChangeAmountYuan = decimal.RequireFromString("30.00")
	_, _, err := svc.Create(context.Background(), in)
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrAmountMismatch))
}

func TestCreate_NegativeChangeRejected(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	in := validInput("op-neg")
	in.PaidAmountYuan = decimal.RequireFromString("10.00")
	in.ChangeAmountYuan = decimal.RequireFromString("-12.20")
	_, _, err := svc.Create(context.Background(), in)
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrAmountMismatch))
}

// ---- 错误路径 ----

func TestCreate_EmptyLinesRejected(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	in := validInput("op-nolines")
	in.Lines = nil
	_, _, err := svc.Create(context.Background(), in)
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrInvalidInput))
	assert.EqualValues(t, 0, countRows(t, db, &model.SalesOrder{}))
}

func TestCreate_MissingOpIDRejected(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	in := validInput("")
	_, _, err := svc.Create(context.Background(), in)
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrInvalidInput))
}

func TestGet_NotFound(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	_, _, _, err := svc.Get(context.Background(), "10", "SO20261021000001")
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrSaleNotFound))
}

func TestGet_BranchIsolation(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	order, _, err := svc.Create(context.Background(), validInput("op-1"))
	require.NoError(t, err)

	_, _, _, err = svc.Get(context.Background(), "01", order.ID)
	require.Error(t, err, "跨店查单必须查不到")
	assert.True(t, errors.Is(err, model.ErrSaleNotFound))
}

// ---- 列表 ----

func TestList_FiltersByBranchAndPaging(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	for i := 0; i < 3; i++ {
		_, _, err := svc.Create(context.Background(), validInput(fmt.Sprintf("op-%d", i)))
		require.NoError(t, err)
	}
	// 另一家门店的单。
	other := validInput("op-other")
	other.BranchID = "01"
	_, _, err := svc.Create(context.Background(), other)
	require.NoError(t, err)

	rows, total, err := svc.List(context.Background(), service.ListFilter{BranchID: "10", Page: 1, PageSize: 2})
	require.NoError(t, err)
	assert.Len(t, rows, 2)
	assert.EqualValues(t, 3, total)
	for _, r := range rows {
		assert.Equal(t, "10", r.BranchID)
	}
}

// ---- 改价 ----

func TestPriceOverride_UpdatesLineAndOrderAudit(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	order, _, err := svc.Create(context.Background(), validInput("op-1"))
	require.NoError(t, err)

	updated, err := svc.PriceOverride(context.Background(), service.PriceOverrideInput{
		SaleID:        order.ID,
		LineID:        order.ID + "-L1",
		BranchID:      "10",
		UnitPriceYuan: decimal.RequireFromString("9.90"),
		AuthorizedBy:  "boss-1",
		UpdatedBy:     "tester",
	})
	require.NoError(t, err)
	assert.Equal(t, "boss-1", updated.AuthorizedBy, "越权授权人必须落库")
	assert.Equal(t, "19.80", updated.TotalAmountYuan.StringFixed(2), "合计按新单价重算")

	var line model.SaleLine
	require.NoError(t, db.Where("id = ?", order.ID+"-L1").First(&line).Error)
	assert.Equal(t, "9.90", line.UnitPriceYuan.StringFixed(2))
	assert.Equal(t, "19.80", line.AmountYuan.StringFixed(2))

	// 改价**不发事件**(否则下游会重复扣库存 / 重复加积分)。
	assert.Len(t, outboxRows(t, db), 1)
}

func TestPriceOverride_UnknownLine(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	order, _, err := svc.Create(context.Background(), validInput("op-1"))
	require.NoError(t, err)

	_, err = svc.PriceOverride(context.Background(), service.PriceOverrideInput{
		SaleID:        order.ID,
		LineID:        "SO20261021000001-L9",
		BranchID:      "10",
		UnitPriceYuan: decimal.RequireFromString("9.90"),
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrLineNotFound))
}

// ---- 退货 ----

func TestRefund_EmitsOrderStatusRWithNegativeAmounts(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	order, _, err := svc.Create(context.Background(), validInput("op-1"))
	require.NoError(t, err)

	refunded, replayed, err := svc.Refund(context.Background(), service.RefundInput{
		OpID:       "op-refund-1",
		SaleID:     order.ID,
		BranchID:   "10",
		OperatorID: "u-2",
		CreatedBy:  "tester",
	})
	require.NoError(t, err)
	assert.False(t, replayed)
	assert.Equal(t, "returned", refunded.Status)

	rows := outboxRows(t, db)
	require.Len(t, rows, 2, "退货也要发一条事件")
	refundRow := rows[1]
	assert.Equal(t, service.TopicSalePaid, refundRow.Topic, "退货复用 sale.paid topic")
	assert.Equal(t, "pos:op-refund-1", refundRow.ID, "outbox 行 id 带服务命名空间")

	var ev service.SalePaidEvent
	require.NoError(t, json.Unmarshal([]byte(refundRow.Payload), &ev))
	assert.Equal(t, service.OrderStatusRefund, ev.OrderStatus)
	assert.Equal(t, order.ID, ev.SaleID)
	assert.Equal(t, "M0001", ev.MemberID, "会员号要带上,下游才知道要冲积分")
	assert.Equal(t, "-22.20", ev.TotalAmountYuan, "退货金额必须为负")
	require.Len(t, ev.Lines, 1)
	assert.Equal(t, "-2", ev.Lines[0].Qty, "退货数量必须为负")
	assert.Equal(t, "-22.20", ev.Lines[0].AmountYuan, "退货行金额必须为负")
	assert.Equal(t, "11.10", ev.Lines[0].UnitPriceYuan, "单价保持正数(金额 = 数量 × 单价)")

	// 退款收付是负数行。
	var pays []model.Payment
	require.NoError(t, db.Where("sale_id = ?", order.ID).Order("id asc").Find(&pays).Error)
	require.Len(t, pays, 2)
	assert.Equal(t, "-22.20", pays[1].AmountYuan.StringFixed(2))
}

func TestRefund_SameOpID_Replays(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	order, _, err := svc.Create(context.Background(), validInput("op-1"))
	require.NoError(t, err)

	in := service.RefundInput{OpID: "op-refund-1", SaleID: order.ID, BranchID: "10", CreatedBy: "tester"}
	_, _, err = svc.Refund(context.Background(), in)
	require.NoError(t, err)

	_, replayed, err := svc.Refund(context.Background(), in)
	require.NoError(t, err, "同 op_id 重放退货**不报错**")
	assert.True(t, replayed)
	assert.Len(t, outboxRows(t, db), 2, "重放不得重复发事件")
}

func TestRefund_AlreadyReturnedRejected(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	order, _, err := svc.Create(context.Background(), validInput("op-1"))
	require.NoError(t, err)
	_, _, err = svc.Refund(context.Background(), service.RefundInput{
		OpID: "op-refund-1", SaleID: order.ID, BranchID: "10", CreatedBy: "tester",
	})
	require.NoError(t, err)

	// 换一个 op_id 再退一次 ⇒ 状态冲突(不能二次退款)。
	_, _, err = svc.Refund(context.Background(), service.RefundInput{
		OpID: "op-refund-2", SaleID: order.ID, BranchID: "10", CreatedBy: "tester",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrSaleAlreadyReturned))
	assert.Len(t, outboxRows(t, db), 2, "二次退款不得发事件")
}

func TestRefund_UnknownSale(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSalesService(db)

	_, _, err := svc.Refund(context.Background(), service.RefundInput{
		OpID: "op-refund-x", SaleID: "SO20261021009999", BranchID: "10", CreatedBy: "tester",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrSaleNotFound))
}
