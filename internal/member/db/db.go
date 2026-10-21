// Package db 封装 member 服务的 PostgreSQL 连接。
//
// 与 internal/pricing/db、internal/masterdata/db 同模式:
// 生产只支持 PostgreSQL,POSTGRES_DSN 缺失时启动失败;连接池默认值一致。
//
// ⚠️ member **拥有 6 张表**(members / member_types / member_point_ledger /
// member_account_ledger / member_settings / payment_ways)
// ⇒ 本服务**必须**有数据库,没有"先跑起来再说"的选项。
package db

import (
	"fmt"
	"time"

	"github.com/YunBright/core/testrun"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// OpenPostgres 打开 PostgreSQL(member 服务专用)。
//
// dsn 必须非空。推荐格式:
//
//	postgres://user:pwd@host:5432/supertrade?sslmode=disable
//
// 由 caller 负责 AutoMigrate(...) —— member 拥有上面那 6 张表。
func OpenPostgres(dsn string) (*gorm.DB, error) {
	if dsn == "" {
		return nil, fmt.Errorf("member: POSTGRES_DSN 必填(只支持 PostgreSQL,不提供 SQLite fallback)")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("member: open db: %w", err)
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
