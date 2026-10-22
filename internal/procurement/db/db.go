// Package db 封装 procurement 服务的 PostgreSQL 连接。
//
// 与 catalog/db.go / transaction/db.go 同模式:生产只支持 PostgreSQL,
// POSTGRES_DSN 缺失时启动失败。
package db

import (
	"fmt"
	"time"

	"github.com/YunBright/core/testrun"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// OpenPostgres 打开 PostgreSQL(procurement 服务专用)。
//
// 由 caller 负责 AutoMigrate(...) —— procurement 拥有
// purchase_sheets / purchase_sheet_lines 两张表。
func OpenPostgres(dsn string) (*gorm.DB, error) {
	if dsn == "" {
		return nil, fmt.Errorf("procurement: POSTGRES_DSN 必填(只支持 PostgreSQL,不提供 SQLite fallback)")
	}
	d, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("procurement: open db: %w", err)
	}
	sqlDB, err := d.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(10)
	sqlDB.SetMaxIdleConns(2)
	sqlDB.SetConnMaxLifetime(time.Hour)
	// 溯源记账到旁表 testrun_rows（core/testrun），见 internal/catalog/db.go 的说明。
	if err := testrun.EnsureTable(d); err != nil {
		return nil, err
	}
	if err := testrun.RegisterGorm(d); err != nil {
		return nil, err
	}
	return d, nil
}
