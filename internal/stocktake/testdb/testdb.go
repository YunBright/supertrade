// Package testdb 提供 stocktake 服务测试用的 SQLite in-memory 打开函数。
//
// 本包**仅供 _test.go 使用**,但因为 Go 的 _test.go 文件不能跨包 export 函数,
// 这里把它放到独立子包。生产代码(go build)不引用此包,可在 code review 校验。
//
// 数据库方言差异:
//   - SQLite(in-memory,本包)用于单测速度快
//   - PostgreSQL(internal/stocktake.OpenPostgres)用于生产 / 真机 / e2e
//   - 关键 SQL 兼容性以 PG e2e 为准
package testdb

import (
	"fmt"

	_ "modernc.org/sqlite" // 注册驱动名 "sqlite"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// OpenSQLite 打开 SQLite in-memory 数据库(测试用)。
//
// dsn 例:":memory:?_pragma=foreign_keys(1)"。
//
// 自动注入关键 pragma:
//   - _pragma=foreign_keys(1)  启用外键约束(GORM 需要)
//   - _time_format=sqlite      时间存为 INTEGER(unix seconds),与 GORM time.Time 兼容
func OpenSQLite(dsn string) (*gorm.DB, error) {
	sep := "?"
	if contains(dsn, "?") {
		sep = "&"
	}
	if !contains(dsn, "_time_format") {
		dsn = dsn + sep + "_time_format=sqlite"
	}
	dialector := sqlite.Dialector{DriverName: "sqlite", DSN: dsn}
	db, err := gorm.Open(dialector, &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1) // SQLite 单写
	return db, nil
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}