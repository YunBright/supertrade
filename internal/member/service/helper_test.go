package service_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	_ "modernc.org/sqlite" // 纯 Go 驱动(无需 CGO)

	"github.com/YunBright/supertrade/internal/member/model"
	"github.com/YunBright/supertrade/internal/member/service"
	"github.com/YunBright/supertrade/pkg/outbox"
)

// newTestDB 构造**文件型** SQLite DB + AutoMigrate。
//
// ⚠️ 用文件型而不是 `:memory:`:内存库在多个连接下是**多个独立库**
//
//	(GORM 的连接池会换连接),而单测里 `SetMaxOpenConns(1)` 只能规避并发,
//	规避不了"连接池复用 + 事务中另开连接"的场景。
//	文件型 + MaxOpenConns(1) ⇒ 事务与业务查询必然落在同一个库上。
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "member.db")
	db, err := gorm.Open(sqlite.Dialector{DriverName: "sqlite", DSN: dsn}, &gorm.Config{})
	require.NoError(t, err, "open sqlite")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	// 关掉连接,否则 TempDir 清理会因文件被占用而失败。
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(
		&model.Member{},
		&model.MemberType{},
		&model.MemberPointLedger{},
		&model.MemberAccountLedger{},
		&model.MemberSettings{},
		&model.PaymentWay{},
		&outbox.Outbox{},
	), "auto migrate")
	return db
}

// newService 构造带固定时钟的 Service(测试用)。
func newService(t *testing.T) (*service.Service, *gorm.DB) {
	t.Helper()
	db := newTestDB(t)
	svc := service.New(db)
	svc.SetClock(func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) })
	return svc, db
}

// seedType 造一个会员类别。
func seedType(t *testing.T, db *gorm.DB, id, branchID, name string) {
	t.Helper()
	require.NoError(t, db.Create(&model.MemberType{
		ID: id, BranchID: branchID, Name: name,
		DiscountRate:  decimal.NewFromFloat(1.0),
		FeeAmountYuan: decimal.Zero,
		Status:        "active",
		CreatedBy:     "test", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}).Error)
}

// seedMember 直接插一行会员(绕过开卡的类别校验)。
func seedMember(t *testing.T, db *gorm.DB, id, branchID string) {
	t.Helper()
	require.NoError(t, db.Create(&model.Member{
		ID: id, BranchID: branchID, CardTypeID: "T1", Name: "张三",
		Status:            model.MemberStatusNormal,
		PointsAccumulated: decimal.Zero, PointsUsed: decimal.Zero,
		BalanceYuan: decimal.Zero, ConsumedAmountYuan: decimal.Zero,
		CreatedBy: "test", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}).Error)
}
