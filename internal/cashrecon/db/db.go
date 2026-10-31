// Package db 封装 cash-recon 服务的 PostgreSQL 连接。
//
// 与 bank-account / catalog 同模式:生产只支持 PostgreSQL,POSTGRES_DSN 缺失时启动失败。
package db

import (
	"fmt"
	"time"

	"github.com/YunBright/core/testrun"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// OpenPostgres 打开 PostgreSQL(cash-recon 服务专用)。
//
// dsn 必须非空。DSN 缺失时**拒绝启动**:缺库却照常起来,症状是
// "对账批次永远为空"或"账户台账建不了",而真因不在任何日志里。
//
// 由 caller 负责 AutoMigrate(...) —— cash-recon 拥有 recon_* 共 17 张表。
func OpenPostgres(dsn string) (*gorm.DB, error) {
	if dsn == "" {
		return nil, fmt.Errorf("cash-recon: POSTGRES_DSN 必填(只支持 PostgreSQL,不提供 SQLite fallback)")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("cash-recon: open db: %w", err)
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
