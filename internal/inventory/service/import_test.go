package service_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/YunBright/supertrade/internal/archivedto"
	"github.com/YunBright/supertrade/internal/inventory/model"
	"github.com/YunBright/supertrade/internal/inventory/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	_ "modernc.org/sqlite" // pure-Go driver (no CGO)
)

func newImportDB(t *testing.T) *gorm.DB {
	t.Helper()
	// ⚠️ **文件型** SQLite：内存库在多个连接下是**多个独立库**
	// (GORM 的连接池会换连接)，单测里 `SetMaxOpenConns(1)` 只能规避并发，
	// 规避不了"连接池复用 + 事务中另开连接"的场景。
	dsn := filepath.Join(t.TempDir(), "inventory.db")
	db, err := gorm.Open(sqlite.Dialector{DriverName: "sqlite", DSN: dsn}, &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.StockLevel{}, &model.StockMove{}))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	// ⚠️ 必须关连接 —— 否则 TempDir 清理时文件被占用（Windows 上会直接判测试失败）。
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func stockRows(q string, qty string, cost string) archivedto.ImportData {
	return archivedto.ImportData{
		Stocks: []archivedto.ImportStockRow{{
			ProductID:   q,
			Qty:         qty,
			AvgCostYuan: cost,
		}},
	}
}

// TestImportStock_WritesSnapshot —— 导入一次 ⇒ 库存快照落库。
func TestImportStock_WritesSnapshot(t *testing.T) {
	db := newImportDB(t)
	svc := service.NewStockService(db)

	rep, err := svc.ImportStock(context.Background(), service.StockImportInput{
		SourceBranchID: "00",
		TargetBranchID: "10",
		Mode:           "overwrite",
		Data:           stockRows("6957583900828", "261", "4.96"),
	})
	require.NoError(t, err)
	require.Len(t, rep.ByScope, 1)
	assert.Equal(t, archivedto.ScopeStock, rep.ByScope[0].Scope)
	assert.Equal(t, 1, rep.ByScope[0].Inserted)

	var level model.StockLevel
	require.NoError(t, db.First(&level, "branch_id = ? AND product_id = ?",
		"10", "6957583900828").Error)
	assert.Equal(t, "261", level.Qty.String())
	assert.Equal(t, "4.96", level.AvgCostYuan.String())
}

// TestImportStock_IsIdempotent —— 同一份快照导两次 ⇒ 只有一行，第二次是 updated。
//
// ⚠️ 这条锁的是"重复同步不产生第二行"。
//
//	库存快照被导两次 ⇒ 若是 append 语义 ⇒ 库存翻倍 ⇒ **卖货时库存算错**。
func TestImportStock_IsIdempotent(t *testing.T) {
	db := newImportDB(t)
	svc := service.NewStockService(db)

	in := service.StockImportInput{
		SourceBranchID: "00",
		TargetBranchID: "10",
		Mode:           "overwrite",
		Data:           stockRows("6957583900828", "261", "4.96"),
	}
	_, err := svc.ImportStock(context.Background(), in)
	require.NoError(t, err)
	rep, err := svc.ImportStock(context.Background(), in)
	require.NoError(t, err)

	var n int64
	require.NoError(t, db.Model(&model.StockLevel{}).Count(&n).Error)
	assert.EqualValues(t, 1, n, "重复导入同一份快照只能有一行")
	assert.Equal(t, 1, rep.ByScope[0].Updated, "第二次应算 updated 而不是 inserted")
	assert.Equal(t, 0, rep.ByScope[0].Inserted)
}

// TestImportStock_OverwritesQuantity —— 快照覆盖 ⇒ 数量变成**新值**（不是累加）。
//
// ⚠️ 这是"库存只同步**同步时刻**的值"的语义：
//
//	第二次同步的库存是 250 ⇒ 库里必须是 250，**不是** 261+250。
func TestImportStock_OverwritesQuantity(t *testing.T) {
	db := newImportDB(t)
	svc := service.NewStockService(db)

	_, err := svc.ImportStock(context.Background(), service.StockImportInput{
		SourceBranchID: "00", TargetBranchID: "10", Mode: "overwrite",
		Data: stockRows("6957583900828", "261", "4.96"),
	})
	require.NoError(t, err)

	_, err = svc.ImportStock(context.Background(), service.StockImportInput{
		SourceBranchID: "00", TargetBranchID: "10", Mode: "overwrite",
		Data: stockRows("6957583900828", "250", "5.10"),
	})
	require.NoError(t, err)

	var level model.StockLevel
	require.NoError(t, db.First(&level, "branch_id = ? AND product_id = ?",
		"10", "6957583900828").Error)
	assert.Equal(t, "250", level.Qty.String(), "快照覆盖不是累加")
	// ⚠️ decimal 会归一化 scale（"5.10" → 5.1）⇒ 断言按归一化后的值。
	//   （生产是 Postgres 的 numeric(18,4)，那边按 scale 4 存。）
	assert.Equal(t, "5.1", level.AvgCostYuan.String())
}

// TestImportStock_DoesNotTouchOtherBranch —— 别的门店的库存不动。
func TestImportStock_DoesNotTouchOtherBranch(t *testing.T) {
	db := newImportDB(t)
	svc := service.NewStockService(db)

	_, err := svc.ImportStock(context.Background(), service.StockImportInput{
		SourceBranchID: "00", TargetBranchID: "00", Mode: "overwrite",
		Data: stockRows("6957583900828", "100", "1.00"),
	})
	require.NoError(t, err)

	_, err = svc.ImportStock(context.Background(), service.StockImportInput{
		SourceBranchID: "00", TargetBranchID: "10", Mode: "overwrite",
		Data: stockRows("6957583900828", "261", "4.96"),
	})
	require.NoError(t, err)

	var b00 model.StockLevel
	require.NoError(t, db.First(&b00, "branch_id = ?", "00").Error)
	assert.Equal(t, "100", b00.Qty.String(), "门店 00 的库存不该被门店 10 的导入改动")
}

// TestImportStock_SkipsInvalidRows —— 缺 product_id / 数量非法 ⇒ 跳过，不炸。
func TestImportStock_SkipsInvalidRows(t *testing.T) {
	db := newImportDB(t)
	svc := service.NewStockService(db)

	rep, err := svc.ImportStock(context.Background(), service.StockImportInput{
		SourceBranchID: "00", TargetBranchID: "10", Mode: "overwrite",
		Data: archivedto.ImportData{Stocks: []archivedto.ImportStockRow{
			{ProductID: "", Qty: "10"},
			{ProductID: "p2", Qty: "abc"},
			{ProductID: "p3", Qty: "5"},
		}},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, rep.ByScope[0].Skipped)
	assert.Equal(t, 1, rep.ByScope[0].Inserted)

	var n int64
	require.NoError(t, db.Model(&model.StockLevel{}).Count(&n).Error)
	assert.EqualValues(t, 1, n)
}

// TestImportStock_RejectsMissingTargetBranch —— 没有目标门店 ⇒ 报错。
func TestImportStock_RejectsMissingTargetBranch(t *testing.T) {
	db := newImportDB(t)
	svc := service.NewStockService(db)

	_, err := svc.ImportStock(context.Background(), service.StockImportInput{
		SourceBranchID: "00", TargetBranchID: "", Mode: "overwrite",
		Data: stockRows("6957583900828", "261", "4.96"),
	})
	require.Error(t, err)
}
