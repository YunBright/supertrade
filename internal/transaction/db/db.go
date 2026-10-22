// Package db 封装 transaction 服务的 PostgreSQL 连接。
//
// 与 catalog/db.go / pos/db.go 同模式:生产只支持 PostgreSQL,
// POSTGRES_DSN 缺失时启动失败(缺数据库却起来 ⇒ 自营店的流水永远查不了,
// 而真因不出现在任何日志里)。
package db

import (
	"fmt"
	"time"

	"github.com/YunBright/core/testrun"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// OpenPostgres 打开 PostgreSQL(transaction 服务专用)。
//
// dsn 必须非空。推荐格式:
//
//	postgres://user:pwd@host:5432/supertrade?sslmode=disable
//
// 由 caller 负责 AutoMigrate(...) —— transaction 拥有 sale_flows / pay_flows 表。
func OpenPostgres(dsn string) (*gorm.DB, error) {
	if dsn == "" {
		return nil, fmt.Errorf("transaction: POSTGRES_DSN 必填(只支持 PostgreSQL,不提供 SQLite fallback)")
	}
	d, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("transaction: open db: %w", err)
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
