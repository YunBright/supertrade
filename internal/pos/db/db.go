// Package db 封装 pos 服务的 PostgreSQL 连接。
//
// 与 pricing/db.go / masterdata/db.go 同模式:生产只支持 PostgreSQL,
// POSTGRES_DSN 缺失时启动失败;连接池默认值一致。
//
// ⚠️ pos **需要**数据库(它拥有 sales_orders / sale_lines / payments 三张表,
// 以及事务性发件箱 outbox 表)—— 收银事实的**唯一落库处**。
package db

import (
	"fmt"
	"time"

	"github.com/YunBright/core/testrun"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// OpenPostgres 打开 PostgreSQL(pos 服务专用)。
//
// dsn 必须非空。推荐格式:
//
//	postgres://user:pwd@host:5432/supertrade?sslmode=disable
//
// 由 caller 负责 AutoMigrate(...) —— pos 拥有 sales_orders + sale_lines +
// payments + outbox 四张表。
func OpenPostgres(dsn string) (*gorm.DB, error) {
	if dsn == "" {
		return nil, fmt.Errorf("pos: POSTGRES_DSN 必填(只支持 PostgreSQL,不提供 SQLite fallback)")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("pos: open db: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(10)
	sqlDB.SetMaxIdleConns(2)
	sqlDB.SetConnMaxLifetime(time.Hour)
	// 溯源记账到旁表 testrun_rows(core/testrun),见 internal/catalog/db.go 的说明。
	if err := testrun.EnsureTable(db); err != nil {
		return nil, err
	}
	if err := testrun.RegisterGorm(db); err != nil {
		return nil, err
	}
	return db, nil
}
