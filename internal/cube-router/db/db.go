// Package db 封装 cube-router 服务的 PostgreSQL 连接。
//
// 与 stocktake/db.go 同样的模式:生产只支持 PostgreSQL,缺失 POSTGRES_DSN
// 时启动失败;连接池默认值与 stocktake 保持一致(MaxOpen=10, MaxIdle=2,
// ConnMaxLifetime=1h)。
package db

import (
	"fmt"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// OpenPostgres 打开 PostgreSQL(cube-router 服务专用)。
//
// dsn 必须非空。推荐格式:
//
//	postgres://user:pwd@host:5432/supertrade?sslmode=disable
//
// 由 caller 负责 AutoMigrate(...) —— cube-router 仅有 branch_cube_sources
// 一张表。
func OpenPostgres(dsn string) (*gorm.DB, error) {
	if dsn == "" {
		return nil, fmt.Errorf("cube_router: POSTGRES_DSN 必填(只支持 PostgreSQL,不提供 SQLite fallback)")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("cube_router: open db: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(10)
	sqlDB.SetMaxIdleConns(2)
	sqlDB.SetConnMaxLifetime(time.Hour)
	return db, nil
}