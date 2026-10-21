package service_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	_ "modernc.org/sqlite" // 注册驱动名 "sqlite"(纯 Go 驱动,无需 CGO)

	"github.com/YunBright/supertrade/internal/inventory/model"
	"github.com/YunBright/supertrade/internal/inventory/service"
)

// newTestDB 构造 SQLite 内存 DB + AutoMigrate。
//
// ⚠️ 用**文件型** sqlite(而不是 ":memory:")—— 内存库在
// `SetMaxOpenConns(1)` 之外的连接池配置下,每个连接会拿到**不同的库**,
// 于是"建表用的连接"与"查询用的连接"看到的不是同一个库,
// 症状是"表不存在"这种完全无法定位的错。
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "inventory.db")
	dialector := sqlite.Dialector{DriverName: "sqlite", DSN: dsn}
	db, err := gorm.Open(dialector, &gorm.Config{})
	require.NoError(t, err, "open sqlite")
	require.NoError(t, db.AutoMigrate(&model.StockLevel{}, &model.StockMove{}), "auto migrate")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	// ⚠️ 必须关连接:Windows 上文件被占用时 t.TempDir() 的清理会失败
	// (报 "The process cannot access the file because it is being used by another process")。
	// t.Cleanup 是 LIFO ⇒ 本回调先于 TempDir 的清理执行。
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// dec 造一个 decimal。
func dec(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

// salePaidPayload 是 pos 的 sale.paid 载荷(契约见方案 v2 §6.5)。
func salePaidPayload(opID string, lines ...service.SalePaidLine) service.SalePaidPayload {
	return service.SalePaidPayload{
		OpID:            opID,
		SaleID:          "SO20261021000001",
		BranchID:        "10",
		OperatorID:      "u-1",
		TotalAmountYuan: dec("22.20"),
		PaidAt:          "2026-10-21T12:00:00+08:00",
		OrderStatus:     "S",
		Lines:           lines,
	}
}

// line 造一条商品行。
func line(lineID, productID, qty, unitPrice, amount string) service.SalePaidLine {
	return service.SalePaidLine{
		LineID:        lineID,
		ProductID:     productID,
		Qty:           dec(qty),
		UnitPriceYuan: dec(unitPrice),
		AmountYuan:    dec(amount),
	}
}

// countMoves 数 stock_moves 行数。
func countMoves(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Model(&model.StockMove{}).Count(&n).Error)
	return n
}

// countLevels 数 stock_levels 行数。
func countLevels(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Model(&model.StockLevel{}).Count(&n).Error)
	return n
}

// ---- 幂等守卫(最重要) ----

// TestApplySalePaid_Idempotent 是**幂等守卫测试**:
// 同一 op_id 投递两次 ⇒ stock_moves 只有 1 行、stock_levels.qty 只扣一次。
//
// 判据对着症状写(用户看到的库存数字),不是对着内部实现。
func TestApplySalePaid_Idempotent(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)
	ctx := context.Background()

	p := salePaidPayload("op-1", line("L1", "00000297", "2", "11.10", "22.20"))

	res, err := svc.ApplySalePaid(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Applied, "首次投递应写入 1 条流水")
	assert.Equal(t, 0, res.Skipped)

	res, err = svc.ApplySalePaid(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Applied, "重复投递不应再写流水")
	assert.Equal(t, 1, res.Skipped, "重复投递应被唯一索引挡下")

	assert.Equal(t, int64(1), countMoves(t, db), "stock_moves 只应有 1 行")
	assert.Equal(t, int64(1), countLevels(t, db), "stock_levels 只应有 1 行")

	lvl, err := svc.GetLevel(ctx, "10", "00000297")
	require.NoError(t, err)
	assert.True(t, lvl.Qty.Equal(dec("-2")),
		"库存应只扣一次(-2),实际 %s", lvl.Qty.String())

	// 第三次投递(模拟 dapr 退避重试)同样不能多扣。
	_, err = svc.ApplySalePaid(ctx, p)
	require.NoError(t, err)
	lvl, err = svc.GetLevel(ctx, "10", "00000297")
	require.NoError(t, err)
	assert.True(t, lvl.Qty.Equal(dec("-2")), "重复投递后库存仍应是 -2,实际 %s", lvl.Qty.String())
	assert.Equal(t, int64(1), countMoves(t, db), "stock_moves 仍应是 1 行")
}

// TestApplySalePaid_StockLevelCreatedWhenMissing 快照行不存在时自动建行。
func TestApplySalePaid_StockLevelCreatedWhenMissing(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)
	ctx := context.Background()

	// 先确认它不存在
	_, err := svc.GetLevel(ctx, "10", "00000297")
	assert.ErrorIs(t, err, model.ErrStockLevelNotFound)

	_, err = svc.ApplySalePaid(ctx, salePaidPayload("op-1",
		line("L1", "00000297", "3", "5.00", "15.00")))
	require.NoError(t, err)

	assert.Equal(t, int64(1), countLevels(t, db), "应自动建出 1 行快照")
	lvl, err := svc.GetLevel(ctx, "10", "00000297")
	require.NoError(t, err)
	assert.True(t, lvl.Qty.Equal(dec("-3")), "库存应为 -3,实际 %s", lvl.Qty.String())
	assert.Equal(t, int64(1), lvl.ServerVersion, "server_version 应 +1")
}

// TestApplySalePaid_Refund 加回库存(order_status=R)。
//
// ⚠️ 上游 pos 在退款事件里**已经把 qty 与金额取负**(qty=-2),
//
//	所以本服务统一按 "qty = qty - 行qty" 处理 ⇒ -2 ⇒ 库存 +2。
func TestApplySalePaid_Refund(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)
	ctx := context.Background()

	// 先卖 2 件。
	_, err := svc.ApplySalePaid(ctx, salePaidPayload("op-1",
		line("L1", "00000297", "2", "11.10", "22.20")))
	require.NoError(t, err)

	lvl, err := svc.GetLevel(ctx, "10", "00000297")
	require.NoError(t, err)
	assert.True(t, lvl.Qty.Equal(dec("-2")), "卖出后应为 -2,实际 %s", lvl.Qty.String())

	// 再退 2 件(上游已取负)。
	refund := salePaidPayload("op-2", line("L1", "00000297", "-2", "-11.10", "-22.20"))
	refund.OrderStatus = "R"
	res, err := svc.ApplySalePaid(ctx, refund)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Applied)

	lvl, err = svc.GetLevel(ctx, "10", "00000297")
	require.NoError(t, err)
	assert.True(t, lvl.Qty.Equal(dec("0")), "退货后应回到 0,实际 %s", lvl.Qty.String())

	// 流水方向:销售 -2、退货 +2。
	var moves []model.StockMove
	require.NoError(t, db.Order("occurred_at asc").Find(&moves).Error)
	require.Len(t, moves, 2)
	assert.Equal(t, "sale", moves[0].MoveType)
	assert.True(t, moves[0].Qty.Equal(dec("-2")), "销售流水 qty 应为 -2")
	assert.Equal(t, "refund", moves[1].MoveType)
	assert.True(t, moves[1].Qty.Equal(dec("2")), "退货流水 qty 应为 +2")
	assert.True(t, moves[1].QtyAfter.Equal(dec("0")), "退货后 qty_after 应为 0")

	// 退货重复投递也不能多加一次库存。
	_, err = svc.ApplySalePaid(ctx, refund)
	require.NoError(t, err)
	lvl, err = svc.GetLevel(ctx, "10", "00000297")
	require.NoError(t, err)
	assert.True(t, lvl.Qty.Equal(dec("0")), "重复退货后库存仍应是 0,实际 %s", lvl.Qty.String())
	assert.Equal(t, int64(2), countMoves(t, db), "流水仍应是 2 行")
}

// TestApplySalePaid_MultipleProducts 一笔单 2 个商品 ⇒ 2 条流水 + 2 个快照。
func TestApplySalePaid_MultipleProducts(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)
	ctx := context.Background()

	p := salePaidPayload("op-1",
		line("L1", "00000297", "2", "11.10", "22.20"),
		line("L2", "00000301", "3", "2.50", "7.50"))
	res, err := svc.ApplySalePaid(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Applied)
	assert.Equal(t, 2, res.Groups)

	assert.Equal(t, int64(2), countMoves(t, db), "应有 2 条流水")
	assert.Equal(t, int64(2), countLevels(t, db), "应有 2 行快照")

	l1, err := svc.GetLevel(ctx, "10", "00000297")
	require.NoError(t, err)
	assert.True(t, l1.Qty.Equal(dec("-2")), "商品1 库存应为 -2")
	l2, err := svc.GetLevel(ctx, "10", "00000301")
	require.NoError(t, err)
	assert.True(t, l2.Qty.Equal(dec("-3")), "商品2 库存应为 -3")
}

// TestApplySalePaid_SameProductTwiceInOneSale 同一笔单里同一商品出现两次。
//
// ⚠️ 唯一索引是 (branch_id, product_id, ref_type, ref_id) ⇒
//
//	不聚合的话第二行会被 DoNothing 静默丢掉 ⇒ **数量就少了**。
//	聚合后应落 1 行流水、数量是 2 行之和。
func TestApplySalePaid_SameProductTwiceInOneSale(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)
	ctx := context.Background()

	p := salePaidPayload("op-1",
		line("L1", "00000297", "2", "11.10", "22.20"),
		line("L2", "00000297", "3", "11.10", "33.30"))
	res, err := svc.ApplySalePaid(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Applied, "同一商品只应落 1 行流水")
	assert.Equal(t, int64(1), countMoves(t, db))

	lvl, err := svc.GetLevel(ctx, "10", "00000297")
	require.NoError(t, err)
	assert.True(t, lvl.Qty.Equal(dec("-5")), "库存应为 -2+-3=-5,实际 %s", lvl.Qty.String())
}

// ---- 错误路径 ----

// TestApplySalePaid_InvalidPayload 载荷非法 ⇒ 报错且**不落任何行**。
func TestApplySalePaid_InvalidPayload(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)
	ctx := context.Background()

	cases := []struct {
		name string
		p    service.SalePaidPayload
	}{
		{"缺 branch_id", salePaidPayload("op-1", line("L1", "00000297", "2", "11.10", "22.20"))},
		{"缺 op_id", salePaidPayload("", line("L1", "00000297", "2", "11.10", "22.20"))},
		{"lines 为空", salePaidPayload("op-1")},
	}
	// 缺 branch_id / op_id 单独构造
	cases[0].p.BranchID = ""
	cases[1].p.OpID = ""

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.ApplySalePaid(ctx, tc.p)
			require.Error(t, err)
			assert.ErrorIs(t, err, model.ErrInvalidInput)
		})
	}

	// ⛔ 一行都不能落。
	assert.Equal(t, int64(0), countMoves(t, db), "非法载荷不应留下流水")
	assert.Equal(t, int64(0), countLevels(t, db), "非法载荷不应留下快照")
}

// TestApplySalePaid_EmptyProductID 缺 product_id ⇒ 报错(不静默丢行)。
func TestApplySalePaid_EmptyProductID(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)

	_, err := svc.ApplySalePaid(context.Background(),
		salePaidPayload("op-1", line("L1", "", "2", "11.10", "22.20")))
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrInvalidInput)
	assert.Equal(t, int64(0), countMoves(t, db))
}

// TestApplySalePaid_UnknownOrderStatus 未知 order_status ⇒ 报错(不猜方向)。
func TestApplySalePaid_UnknownOrderStatus(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)

	p := salePaidPayload("op-1", line("L1", "00000297", "2", "11.10", "22.20"))
	p.OrderStatus = "X"
	_, err := svc.ApplySalePaid(context.Background(), p)
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrInvalidInput)
	assert.Equal(t, int64(0), countMoves(t, db))
}

// TestApplySalePaid_OtherBranchIsolation 不同门店互不影响。
func TestApplySalePaid_OtherBranchIsolation(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)
	ctx := context.Background()

	p := salePaidPayload("op-1", line("L1", "00000297", "2", "11.10", "22.20"))
	_, err := svc.ApplySalePaid(ctx, p)
	require.NoError(t, err)

	other := salePaidPayload("op-2", line("L1", "00000297", "5", "11.10", "55.50"))
	other.BranchID = "20"
	_, err = svc.ApplySalePaid(ctx, other)
	require.NoError(t, err)

	l1, err := svc.GetLevel(ctx, "10", "00000297")
	require.NoError(t, err)
	assert.True(t, l1.Qty.Equal(dec("-2")), "门店 10 不受影响")
	l2, err := svc.GetLevel(ctx, "20", "00000297")
	require.NoError(t, err)
	assert.True(t, l2.Qty.Equal(dec("-5")), "门店 20 扣 5")
}
