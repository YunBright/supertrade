package service_test

// 本文件锁的是一条**静默**缺陷(2026-10-23 生产实测踩到):
// 溯源记账的 gorm 回调读 `Statement.Context` 里的 run_id,
// 而 `s.db` 自带的 context 是 context.Background()。
// 于是 service 里任何 `s.db.Create(...)`——**哪怕方法签名已经收了 ctx**——
// 都会让回调第一行拿到空串直接 return:
//
//	行照样写进生产库,旁表一条账都不记,收尾 sweep 永远找不到它。
//
// 而 sweep 报的是 "OK"。**没有任何一行报错。**
//
// # 生产上的样子(2026-10-23)
//
// 契约测试打生产,连跑 3 次 ⇒ 生产库里躺着 3 头整猪 + 3 条分割补录 + 3 张盘点单,
// 全部是测试数据,全部清不掉(旁表里没有它们的账)。
// ⇒ 残留会一天天变多,而报告显示"清理成功"。
//
// # 判据
//
// 凡是"把请求上下文带进 DB 调用"这类修复,测试必须断言**带上了之后的结果**
// (账记上了),而不是断言代码里写了 WithContext——后者挡不住下一个调用点再漏一次。

import (
	"context"
	"testing"
	"time"

	"github.com/YunBright/core/testrun"
	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/YunBright/supertrade/internal/fresh-meat/testdb"
	"gorm.io/gorm"
)

const propagationRunID = "ctr-20261023-235959-prop01"

// newLedgerService 造一个带溯源记账的 service
// (生产 OpenPostgres 里 EnsureTable + RegisterGorm 的等价物)。
func newLedgerService(t *testing.T) (*service.Service, *gorm.DB) {
	t.Helper()
	db, err := testdb.OpenSQLite(":memory:?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := testrun.EnsureTable(db); err != nil {
		t.Fatalf("ensure testrun_rows: %v", err)
	}
	if err := db.AutoMigrate(
		&model.WholePig{},
		&model.PigCut{},
		&model.PorkCutsStocktake{},
		&model.LineSalesByPig{},
		&model.BranchCutMapping{},
		&model.WasteLog{},
		&model.SkuStockPosition{},
		&model.SkuCostState{},
		&model.SkuCostDaily{},
		&model.PromoSku{},
		&model.CarryoverRecord{},
		&model.BulkProduct{},
		&model.BulkProduction{},
		&model.BulkSettlement{},
		&model.BulkSettlementItem{},
		&model.BulkSettlementAllocation{},
		&model.PigForecastBaseline{},
		// 2026-10-24:营业日状态机 + 搬台流水 + 按营业日的结余快照 + 期初结转 + 审计结转。
		&model.BusinessDay{},
		&model.CounterLoadRecord{},
		&model.CutStockDaily{},
		&model.SkuStockOpening{},
		&model.BizAudit{},
		&model.CutPresale{},
		&model.PigPresaleMargin{},
		&model.CutPresaleEdit{},
		// 2026-10-27:成本链改部位级(部位级成本日快照 + 部位级成本状态)。
		&model.CutCostDaily{},
		&model.CutCostState{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := testrun.RegisterGorm(db); err != nil {
		t.Fatalf("register gorm callbacks: %v", err)
	}
	svc := service.New(db)
	bizLoc := time.FixedZone("Asia/Shanghai", 8*3600)
	svc.SetClock(func() time.Time { return time.Date(2026, 10, 23, 14, 15, 0, 0, bizLoc) })
	return svc, db
}

// assertRecorded 断言"这张表的这一行"在旁表里有一笔账,且记的是**它自己的主键**。
func assertRecorded(t *testing.T, db *gorm.DB, table, rowID string) {
	t.Helper()
	n, err := testrun.RecordedRows(db, propagationRunID, table)
	if err != nil {
		t.Fatalf("RecordedRows(%s): %v", table, err)
	}
	if n != 1 {
		t.Fatalf("%s 记账条数 = %d, 期望 1。"+
			"为 0 说明 ctx 没传到 DB 调用 —— 行写进了库但清理永远找不到它。", table, n)
	}
	var got []string
	if err := db.Model(&testrun.Row{}).
		Where("run_id = ? AND table_name = ?", propagationRunID, table).
		Pluck("row_id", &got).Error; err != nil {
		t.Fatalf("pluck row_id: %v", err)
	}
	if len(got) != 1 || got[0] != rowID {
		t.Fatalf("%s 记账的 row_id = %v, 期望 [%s]", table, got, rowID)
	}
}

// TestService_RecordWholePig_RecordsProvenance —— 进猪必须进旁表。
//
// 反证:把 `s.db.WithContext(ctx).Create(pig)` 退回 `s.db.Create(pig)`,
// 本测试立刻红(其它 service 测试仍全绿 —— 它们只断言业务结果,不关心溯源)。
func TestService_RecordWholePig_RecordsProvenance(t *testing.T) {
	svc, db := newLedgerService(t)

	pig, err := svc.RecordWholePig(
		testrun.WithRunID(context.Background(), propagationRunID),
		"S001",
		model.RecordWholePigInput{
			EarTag: "EB-PROG", GrossWeightKg: d("50"), PurchaseUnitPriceYuan: d("20"),
			SupplierID: "SUP-1", ArrivedAt: time.Date(2026, 10, 23, 6, 0, 0, 0, time.UTC),
		}, "u-1")
	if err != nil {
		t.Fatalf("RecordWholePig: %v", err)
	}
	assertRecorded(t, db, "whole_pigs", pig.ID)
}

// TestService_RecordPigCut_RecordsProvenance —— 分割补录必须进旁表。
func TestService_RecordPigCut_RecordsProvenance(t *testing.T) {
	svc, db := newLedgerService(t)
	ctx := testrun.WithRunID(context.Background(), propagationRunID)

	pig, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-PROG2", GrossWeightKg: d("50"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: time.Date(2026, 10, 23, 6, 0, 0, 0, time.UTC),
	}, "u-1")
	if err != nil {
		t.Fatalf("RecordWholePig: %v", err)
	}
	// 补录要求该部位已配 cube 映射(业务前置)。
	if _, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-2001"}); err != nil {
		t.Fatalf("CreateBranchCutMapping: %v", err)
	}
	cut, err := svc.RecordPigCut(ctx, "S001", model.RecordPigCutInput{
		PigID: pig.ID, CutType: model.CutBelly, Barcode: "B-PROG",
		WeightKg: d("10"), ExpiresAt: time.Date(2026, 10, 25, 6, 0, 0, 0, time.UTC),
		CubeProductID: "P-2001",
	}, "u-1")
	if err != nil {
		t.Fatalf("RecordPigCut: %v", err)
	}
	assertRecorded(t, db, "pig_cuts", cut.ID)
}

// TestService_CreateBranchCutMapping_RecordsProvenance —— 部位映射必须进旁表。
func TestService_CreateBranchCutMapping_RecordsProvenance(t *testing.T) {
	svc, db := newLedgerService(t)

	m, err := svc.CreateBranchCutMapping(
		testrun.WithRunID(context.Background(), propagationRunID),
		"S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-2001"})
	if err != nil {
		t.Fatalf("CreateBranchCutMapping: %v", err)
	}
	assertRecorded(t, db, "branch_cut_mappings", m.ID)
}

// TestService_RecordPorkCutsStocktake_RecordsProvenance —— 日终盘点必须进旁表。
func TestService_RecordPorkCutsStocktake_RecordsProvenance(t *testing.T) {
	svc, db := newLedgerService(t)
	ctx := testrun.WithRunID(context.Background(), propagationRunID)

	_, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-2001"})
	if err != nil {
		t.Fatalf("CreateBranchCutMapping: %v", err)
	}
	st, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: "P-2001", ActualRemainKg: d("10")},
		},
	}, "u-1")
	if err != nil {
		t.Fatalf("RecordPorkCutsStocktake: %v", err)
	}
	assertRecorded(t, db, "pork_cuts_stocktakes", st.ID)
}

// TestService_StockPosition_RecordsProvenance —— 库存位置必须进旁表。
//
// ⚠️ 这条专门盯 `addPosition`:它有一个调用方传的是 `s.db`(不带 ctx),
// 而溯源回调读的是 `Statement.Context` ⇒ 那条写入**没有 run_id**
// ⇒ 旁表不记账 ⇒ sweep 清不掉 ⇒ 生产库一天天攒"0 kg 的幽灵行",
// 而 sweep 报的是 OK。
func TestService_StockPosition_RecordsProvenance(t *testing.T) {
	svc, db := newLedgerService(t)
	ctx := testrun.WithRunID(context.Background(), propagationRunID)

	if _, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-2001"}); err != nil {
		t.Fatalf("CreateBranchCutMapping: %v", err)
	}
	// 日终盘点 ⇒ 过夜搬运把柜台实盘搬进冷库 ⇒ 写一行 sku_stock_positions
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: "P-2001", ActualRemainKg: d("10")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("RecordPorkCutsStocktake: %v", err)
	}
	n, err := testrun.RecordedRows(db, propagationRunID, "sku_stock_positions")
	if err != nil {
		t.Fatalf("RecordedRows: %v", err)
	}
	if n != 1 {
		t.Fatalf("sku_stock_positions 记账条数 = %d,期望 1。"+
			"为 0 说明 addPosition 没把 ctx 带进 DB 调用 ⇒ 这行永远清不掉。", n)
	}
}

// TestService_NoRunIDMeansNoLedger —— 生产流量不许产生任何账。
//
// 这是"业务表零改动 + 生产零成本"的另一半保证:没有 RunID 的请求一行都不许记。
// 否则旁表会被真实业务流量撑爆,而它的存在意义恰恰是"这里只记录测试数据"。
func TestService_NoRunIDMeansNoLedger(t *testing.T) {
	svc, db := newLedgerService(t)

	if _, err := svc.RecordWholePig(context.Background(), "S001", model.RecordWholePigInput{
		EarTag: "EB-NORUN", GrossWeightKg: d("50"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: time.Date(2026, 10, 23, 6, 0, 0, 0, time.UTC),
	}, "u-1"); err != nil {
		t.Fatalf("RecordWholePig: %v", err)
	}

	var n int64
	if err := db.Model(&testrun.Row{}).Count(&n).Error; err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if n != 0 {
		t.Fatalf("无 RunID 的请求写了 %d 条账,期望 0 条", n)
	}
	// 业务行本身必须照常写入 —— 证明上面不是"因为写失败所以没账"。
	var pigs int64
	if err := db.Model(&model.WholePig{}).Count(&pigs).Error; err != nil {
		t.Fatalf("count pigs: %v", err)
	}
	if pigs != 1 {
		t.Fatalf("whole_pigs 行数 = %d,期望 1(无 RunID 不影响业务写入)", pigs)
	}
}
