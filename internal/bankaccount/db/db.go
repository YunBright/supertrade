// Package db 封装 bank-account 服务的 PostgreSQL 连接。
//
// 与 catalog/db 同模式:生产只支持 PostgreSQL,POSTGRES_DSN 缺失时启动失败。
package db

import (
	"fmt"
	"time"

	"github.com/YunBright/core/testrun"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// OpenPostgres 打开 PostgreSQL(bank-account 服务专用)。
//
// dsn 必须非空。推荐格式:
//
//	postgres://user:pwd@host:5432/supertrade?sslmode=disable
//
// 由 caller 负责 AutoMigrate(...) —— bank-account 拥有 supplier_bank_accounts 表。
func OpenPostgres(dsn string) (*gorm.DB, error) {
	if dsn == "" {
		return nil, fmt.Errorf("bank-account: POSTGRES_DSN 必填(只支持 PostgreSQL,不提供 SQLite fallback)")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("bank-account: open db: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(10)
	sqlDB.SetMaxIdleConns(2)
	sqlDB.SetConnMaxLifetime(time.Hour)
	// 溯源记账到旁表 testrun_rows(见 internal/catalog/db/db.go 的说明)。
	if err := testrun.EnsureTable(db); err != nil {
		return nil, err
	}
	if err := testrun.RegisterGorm(db); err != nil {
		return nil, err
	}
	return db, nil
}
