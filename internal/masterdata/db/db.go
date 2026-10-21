// Package db 封装 master-data 服务的 PostgreSQL 连接。
//
// 与 catalog/db.go 同模式:生产只支持 PostgreSQL,POSTGRES_DSN 缺失时启动失败。
//
// ⚠️ 2026-10-21 起 master-data **需要**数据库(它拥有 stores / employees / shifts)。
//
//	此前它是一个 501 占位 cmd,unit 里也没有 POSTGRES_DSN。
package db

import (
	"fmt"
	"time"

	"github.com/YunBright/core/testrun"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// OpenPostgres 打开 PostgreSQL(master-data 服务专用)。
//
// 由 caller 负责 AutoMigrate(...) —— master-data 拥有 stores + employees + shifts 表。
func OpenPostgres(dsn string) (*gorm.DB, error) {
	if dsn == "" {
		return nil, fmt.Errorf("master-data: POSTGRES_DSN 必填(只支持 PostgreSQL,不提供 SQLite fallback)")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("master-data: open db: %w", err)
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
