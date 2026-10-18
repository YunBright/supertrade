package salesdaily

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// OpenDB 打开 PostgreSQL（每日销售事实包专用）。
//
// # ⚠️ DSN 缺失时的行为：报错还是继续？
//
// 这里选择**报错并拒绝启动**，与 internal/salesagg/compare、internal/stocktake 同口径。
//
// 理由是缺失症状极其隐蔽：sales-agg 的采集会每天失败两次，
// 而消费侧因为"PG 里没快照"会**自动回退查 cube** ——
// 于是功能看起来**完全正常**（数字还是对的、卡片照发），
// 唯一线索是日志里一行 ERROR。
//
// 换句话说：静默降级会让"根本没落库"这件事永远不被发现，
// 直到某天有人查 PG 才发现里面是空的。
//
// ⚠️ **两个服务都需要它**：sales-agg 写、sales-insight 读。
//
//	读侧同样 fail-fast，但理由不同：读侧静默降级的话，
//	AI 报告会一直走回退路径，而月报永远拿不到日行 ——
//	表现是"月报数据不对"，排查会去查 LLM 与 prompt。
//
// 连接池默认值与 compare.OpenDB 一致（10/2/1h）。
func OpenDB(dsn string) (*gorm.DB, error) {
	if dsn == "" {
		return nil, errors.New(
			"salesdaily: POSTGRES_DSN 必填（每日销售事实包是 sales-agg 与 sales-insight 的唯一共享数据源；" +
				"缺失时采集持续失败而消费侧静默回退查 cube，两边看起来都正常）")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("salesdaily: open db: %w", err)
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
