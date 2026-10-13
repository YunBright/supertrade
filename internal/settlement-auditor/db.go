// internal/settlement-auditor/db.go
//
// 生产代码只支持 PostgreSQL。SQLite 仅在 _test.go 内用于单元测试。
// 启动期必须设置 POSTGRES_DSN;缺失则 OpenPostgres 返回明确 error,启动失败。
package settlementauditor

import (
	"fmt"
	"time"

	"github.com/YunBright/core/testrun"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// OpenPostgres 打开 PostgreSQL(settlement-auditor 服务专用)。
//
// dsn 必须非空。推荐格式:
//
//	postgres://user:pwd@host:5432/supertrade?sslmode=disable
//
// 连接池默认值:MaxOpenConns=10,MaxIdleConns=2,ConnMaxLifetime=1h。
// 由 caller 负责调 AutoMigrate(...)。
func OpenPostgres(dsn string) (*gorm.DB, error) {
	if dsn == "" {
		return nil, fmt.Errorf("settlement-auditor: POSTGRES_DSN 必填(只支持 PostgreSQL,不提供 SQLite fallback)")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("settlement-auditor: open db: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(10)
	sqlDB.SetMaxIdleConns(2)
	sqlDB.SetConnMaxLifetime(time.Hour)
	// 溯源记账到旁表 testrun_rows（core/testrun），见 internal/stocktake/db.go 的说明。
	if err := testrun.EnsureTable(db); err != nil {
		return nil, err
	}
	if err := testrun.RegisterGorm(db); err != nil {
		return nil, err
	}
	return db, nil
}
