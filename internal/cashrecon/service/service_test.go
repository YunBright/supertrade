package service

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	_ "modernc.org/sqlite" // 纯 Go 驱动（无 CGO）

	"github.com/YunBright/supertrade/internal/cashrecon/model"
	"github.com/YunBright/supertrade/internal/cubesource"
)

// testClock 固定时钟（让"今天"可断言）。
var testClock = time.Date(2026, 10, 31, 3, 0, 0, 0, time.UTC)

// newTestDB 构造 SQLite 内存库并迁移 cash-recon 的表。
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Dialector{DriverName: "sqlite", DSN: ":memory:"}, &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	err = db.AutoMigrate(
		&model.ReconBatch{},
		&model.ReconCashFlow{},
		&model.ReconBankFlow{},
		&model.ReconImport{},
		&model.ReconMatch{},
		&model.ReconDiff{},
		&model.ReconCashCount{},
		&model.ReconCashVariance{},
		&model.ReconAuditLog{},
		&model.ReconRule{},
		&model.ReconPeriodSnapshot{},
		&model.ReconPeriodLock{},
		&model.ReconAccount{},
		&model.ReconOpeningBalance{},
	)
	if err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// newTestService 构造带固定时钟的 service。
func newTestService(t *testing.T) (*Service, *gorm.DB) {
	t.Helper()
	db := newTestDB(t)
	cfg := DefaultConfig()
	cfg.Clock = func() time.Time { return testClock }
	return NewWithConfig(db, cfg), db
}

// fakeResolver 是 cubesource.Resolver 的测试替身。
type fakeResolver struct {
	src cubesource.Source
	err error
}

func (f fakeResolver) Resolve(context.Context, string) (cubesource.Source, error) {
	return f.src, f.err
}

// cubeSource 返回"配了且启用"的解析结果。
func cubeSource() fakeResolver {
	return fakeResolver{src: cubesource.Source{
		Configured: true, Enabled: true, CubeSourceName: "cube-sixun-ysx-fb",
	}}
}

// localSource 返回"没配数据源"（自营店）的解析结果。
func localSource() fakeResolver {
	return fakeResolver{src: cubesource.Source{Configured: false}}
}

// day 构造日历日（UTC 零点）。
func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// dec 构造 decimal。
func dec(v string) decimal.Decimal {
	return decimal.RequireFromString(v)
}

// insertPayFlows 建 transaction 域的 pay_flows 表并写入流水（本地路径测试用）。
func insertPayFlows(t *testing.T, db *gorm.DB, rows ...map[string]any) {
	t.Helper()
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS pay_flows (
		branch_id varchar(32), pay_way varchar(16),
		pay_amount numeric, exchange_rate numeric, paid_at datetime)`).Error; err != nil {
		t.Fatalf("create pay_flows: %v", err)
	}
	for _, r := range rows {
		err := db.Exec(`INSERT INTO pay_flows (branch_id, pay_way, pay_amount, exchange_rate, paid_at)
			VALUES (?, ?, ?, ?, ?)`,
			r["branch_id"], r["pay_way"], r["pay_amount"], r["exchange_rate"], r["paid_at"]).Error
		require.NoError(t, err)
	}
}
