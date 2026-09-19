// internal/stocktake/db.go
//
// 生产代码只支持 PostgreSQL。SQLite 仅在 _test.go 内用于单元测试(见 db_test.go)。
//
// 启动期必须设置 POSTGRES_DSN(例:postgres://user:pwd@host:5432/supertrade?sslmode=disable)。
// 缺失则 OpenPostgres 返回明确 error,启动失败 —— 不允许任何 fallback。
package stocktake

import (
	"fmt"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// OpenPostgres 打开 PostgreSQL(stocktake 服务专用)。
//
// dsn 必须非空。推荐格式:
//
//	postgres://user:pwd@host:5432/supertrade?sslmode=disable
//
// 连接池默认值:MaxOpenConns=10,MaxIdleConns=2,ConnMaxLifetime=1h。
// 由 caller 负责调 AutoMigrate(...)。
func OpenPostgres(dsn string) (*gorm.DB, error) {
	if dsn == "" {
		return nil, fmt.Errorf("stocktake: POSTGRES_DSN 必填(只支持 PostgreSQL,不提供 SQLite fallback)")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("stocktake: open db: %w", err)
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