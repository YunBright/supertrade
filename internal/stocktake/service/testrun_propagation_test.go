package service_test

import (
	"context"
	"testing"

	"github.com/YunBright/core/testrun"
	"github.com/YunBright/supertrade/internal/cubeclient/cubeclientfake"
	"github.com/YunBright/supertrade/internal/stocktake/model"
	"github.com/YunBright/supertrade/internal/stocktake/service"
	"github.com/YunBright/supertrade/internal/stocktake/testdb"
	"gorm.io/gorm"
)

// 本文件锁的是一条**静默**缺陷：溯源记账的 gorm 回调读 `Statement.Context` 里的
// run_id，而 `s.db` 自带的 context 是 context.Background()。于是 service 里任何
// `s.db.Create(...)`——哪怕方法签名**已经收了 ctx**——都会让回调第一行拿到空串
// 返回：
//
//	行照样写进生产库，旁表一条账都不记，收尾 sweep 永远找不到它。
//
// 而 sweep 报的是 "OK"。**没有任何一行报错。**
//
// 判据：凡是"把请求上下文带进 DB 调用"这类修复，测试必须断言**带上了之后的
// 结果**（账记上了），而不是断言代码里写了 WithContext——后者挡不住下一个
// 调用点再漏一次。

const propagationRunID = "ctr-20261013-135959-prop01"

// newLedgerService 造一个带溯源记账的 service（生产 OpenPostgres 里
// EnsureTable + RegisterGorm 的等价物）。
func newLedgerService(t *testing.T) (*service.Service, *gorm.DB, *cubeclientfake.Client) {
	t.Helper()
	db, err := testdb.OpenSQLite(":memory:?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := testrun.EnsureTable(db); err != nil {
		t.Fatalf("ensure testrun_rows: %v", err)
	}
	if err := db.AutoMigrate(
		&model.StocktakeHeader{},
		&model.StocktakeLine{},
		&model.StocktakeLineOperation{},
		&model.StocktakePlanItem{},
		&model.StocktakeBranchDefault{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := testrun.RegisterGorm(db); err != nil {
		t.Fatalf("register gorm callbacks: %v", err)
	}
	cube := cubeclientfake.New()
	return service.New(db, cube), db, cube
}

// TestService_CreateHeader_RecordsProvenance —— 建盘点单必须进旁表。
//
// 反证：把 service.go 里的 `s.db.WithContext(ctx).Create(h)` 退回
// `s.db.Create(h)`，本测试立刻红（其他 service 测试仍全绿——它们只断言业务
// 结果，不关心溯源）。
func TestService_CreateHeader_RecordsProvenance(t *testing.T) {
	svc, db, _ := newLedgerService(t)

	h, err := svc.CreateHeader(
		testrun.WithRunID(context.Background(), propagationRunID),
		service.CreateHeaderInput{BranchID: "S001", OperatorID: "u-1"})
	if err != nil {
		t.Fatalf("CreateHeader: %v", err)
	}

	n, err := testrun.RecordedRows(db, propagationRunID, "stocktake_headers")
	if err != nil {
		t.Fatalf("RecordedRows: %v", err)
	}
	if n != 1 {
		t.Fatalf("stocktake_headers 记账条数 = %d, 期望 1。"+
			"为 0 说明 ctx 没传到 DB 调用 —— 行写进了库但清理永远找不到它。", n)
	}

	// 记的必须是**这一行**的主键，不能只是一条数量对得上的旁路记录。
	var got []string
	if err := db.Model(&testrun.Row{}).
		Where("run_id = ? AND table_name = ?", propagationRunID, "stocktake_headers").
		Pluck("row_id", &got).Error; err != nil {
		t.Fatalf("pluck row_id: %v", err)
	}
	if len(got) != 1 || got[0] != h.ID {
		t.Fatalf("记账的 row_id = %v, 期望 [%s]", got, h.ID)
	}
}

// TestService_NoRunIDMeansNoLedger —— 生产流量不许产生任何账。
//
// 这是"业务表零改动 + 生产零成本"的另一半保证：没有 RunID 的请求
// 一行都不许记。否则旁表会被真实业务流量撑爆，而它的存在意义
// 恰恰是"这里只记录测试数据"。
func TestService_NoRunIDMeansNoLedger(t *testing.T) {
	svc, db, _ := newLedgerService(t)

	if _, err := svc.CreateHeader(context.Background(),
		service.CreateHeaderInput{BranchID: "S001", OperatorID: "u-1"}); err != nil {
		t.Fatalf("CreateHeader: %v", err)
	}

	var n int64
	if err := db.Model(&testrun.Row{}).Count(&n).Error; err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if n != 0 {
		t.Fatalf("无 RunID 的请求写了 %d 条账，期望 0 条", n)
	}

	// 业务行本身必须照常写入 —— 证明上面不是"因为写失败所以没账"。
	var headers int64
	if err := db.Model(&model.StocktakeHeader{}).Count(&headers).Error; err != nil {
		t.Fatalf("count headers: %v", err)
	}
	if headers != 1 {
		t.Fatalf("stocktake_headers 行数 = %d, 期望 1（无 RunID 不影响业务写入）", headers)
	}
}
