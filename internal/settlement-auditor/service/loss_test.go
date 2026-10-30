package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/stocktake/model"
	"github.com/YunBright/supertrade/internal/stocktake/testdb"
)

// fakeResolver 是 cubesource.Resolver 的测试替身。
type fakeResolver struct {
	src cubesource.Source
	err error
}

func (r fakeResolver) Resolve(_ context.Context, _ string) (cubesource.Source, error) {
	return r.src, r.err
}

// TestAggregateLoss_PerDirection 锁盘损口径(2026-10-31 用户拍板):
// JO 报损单**全部**计入;OO 库存调整单**只算减少**('+' 是盘盈,算进去会把偏离"解释"掉)。
func TestAggregateLoss_PerDirection(t *testing.T) {
	rows := []LossRow{
		{ProductID: "P1", DocumentType: "JO", Direction: "-", Quantity: dec("8.43")},
		{ProductID: "P1", DocumentType: "OO", Direction: "-", Quantity: dec("1.91")},
		{ProductID: "P1", DocumentType: "OO", Direction: "+", Quantity: dec("100")}, // 盘盈:不计
		{ProductID: "P1", DocumentType: "KB", Direction: "-", Quantity: dec("50")},  // 扣补:不计
		{ProductID: "P2", DocumentType: "JO", Direction: "+", Quantity: dec("2")},   // 报损单方向异常:仍算(JO 全部)
		{ProductID: "", DocumentType: "JO", Direction: "-", Quantity: dec("9")},     // 空货号:丢
		{ProductID: "P3", DocumentType: "JO", Direction: "-", Quantity: dec("0")},   // 0:丢
	}
	got := aggregateLoss(rows)
	if got["P1"].String() != "10.34" {
		t.Errorf("P1 盘损 = %s, want 10.34(8.43 JO + 1.91 OO 减少;盘盈与扣补不计)", got["P1"].String())
	}
	if got["P2"].String() != "2" {
		t.Errorf("P2 盘损 = %s, want 2", got["P2"].String())
	}
	if _, ok := got["P3"]; ok {
		t.Errorf("零数量不应进结果: %v", got)
	}
	if _, ok := got[""]; ok {
		t.Errorf("空货号不应进结果: %v", got)
	}
}

// TestLossService_CubePath 有源门店走 cube(用的是同一份 aggregateLoss)。
func TestLossService_CubePath(t *testing.T) {
	cube := &fakeCube{losses: []LossRow{
		{ProductID: "PR1", DocumentType: "JO", Direction: "-", Quantity: dec("8.43")},
		{ProductID: "PR1", DocumentType: "OO", Direction: "+", Quantity: dec("5")},
	}}
	svc := NewLossService(fakeResolver{src: cubesource.Source{Configured: true, Enabled: true}}, cube, nil)

	got, err := svc.SumLossByProduct(context.Background(), "B1", []string{"PR1"}, "2026-08-01", "2026-11-01")
	if err != nil {
		t.Fatalf("SumLossByProduct: %v", err)
	}
	if got["PR1"].String() != "8.43" {
		t.Errorf("盘损 = %s, want 8.43", got["PR1"].String())
	}
	// 有源门店**不得**回落本域 PG(即使 db 为 nil 也不会走到本地路径)。
}

// TestLossService_CubeMissingReader 锁"配了源却没装 cube 读端 ⇒ 大声失败,不回落本地"。
func TestLossService_CubeMissingReader(t *testing.T) {
	svc := NewLossService(fakeResolver{src: cubesource.Source{Configured: true, Enabled: true}}, nil, nil)
	if _, err := svc.SumLossByProduct(context.Background(), "B1", []string{"PR1"}, "a", "b"); !errors.Is(err, ErrCubeUnavailable) {
		t.Errorf("err = %v, want ErrCubeUnavailable", err)
	}
}

// TestLossService_DisabledSource 锁"数据源被停用 ⇒ 报错,不回落本地"(与读端点同一判据)。
func TestLossService_DisabledSource(t *testing.T) {
	svc := NewLossService(fakeResolver{src: cubesource.Source{Configured: true, Enabled: false}}, &fakeCube{}, nil)
	_, err := svc.SumLossByProduct(context.Background(), "B1", []string{"PR1"}, "a", "b")
	if !errors.Is(err, cubesource.ErrSourceDisabled) {
		t.Errorf("err = %v, want ErrSourceDisabled", err)
	}
}

// TestLossService_LocalPG 无源门店查本域 PG 的盘亏行,并锁住"哪些行不算证据"。
func TestLossService_LocalPG(t *testing.T) {
	db := openStocktakeDB(t)
	countDate := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)

	// ① 已审核、普通盘、盘亏 ⇒ 计入(2.5 + 1.0)
	seedStocktake(t, db, model.StocktakeHeader{
		ID: "ST-A", BranchID: "B1", CountDate: countDate,
		Status: model.StatusApproved, Type: model.TypeGeneral, OperatorID: "u",
	}, []model.StocktakeLine{
		{ProductID: "P1", DiffQty: dec("-2.5")},
		{ProductID: "P1", DiffQty: dec("-1.0")},
		{ProductID: "P2", DiffQty: dec("3.0")},  // 盘盈:不计
		{ProductID: "P3", DiffQty: dec("-7.0")}, // 别的商品:不在查询范围
	})
	// ② 未审核 ⇒ 不计
	seedStocktake(t, db, model.StocktakeHeader{
		ID: "ST-B", BranchID: "B1", CountDate: countDate,
		Status: model.StatusCounting, Type: model.TypeGeneral, OperatorID: "u",
	}, []model.StocktakeLine{{ProductID: "P1", DiffQty: dec("-9.0")}})
	// ③ 复盘点单 ⇒ 不计(与原盘是同一批货的第二次清点,算两遍会把损耗放大)
	seedStocktake(t, db, model.StocktakeHeader{
		ID: "ST-C", BranchID: "B1", CountDate: countDate,
		Status: model.StatusApproved, Type: model.TypeRecheck, OperatorID: "u",
	}, []model.StocktakeLine{{ProductID: "P1", DiffQty: dec("-9.0")}})
	// ④ 别的门店 ⇒ 不计
	seedStocktake(t, db, model.StocktakeHeader{
		ID: "ST-D", BranchID: "B2", CountDate: countDate,
		Status: model.StatusApproved, Type: model.TypeGeneral, OperatorID: "u",
	}, []model.StocktakeLine{{ProductID: "P1", DiffQty: dec("-9.0")}})
	// ⑤ 窗口外 ⇒ 不计
	seedStocktake(t, db, model.StocktakeHeader{
		ID: "ST-E", BranchID: "B1", CountDate: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Status: model.StatusApproved, Type: model.TypeGeneral, OperatorID: "u",
	}, []model.StocktakeLine{{ProductID: "P1", DiffQty: dec("-9.0")}})

	// ⑥ 软删的行 ⇒ 不计(删掉 -2.5 那条,只剩 -1.0)
	deleted := model.StocktakeLine{ID: "ST-A-L0"}
	if err := db.Delete(&deleted).Error; err != nil {
		t.Fatalf("软删测试行: %v", err)
	}

	svc := NewLossService(nil, nil, db) // nil resolver ⇒ 按"没配源"走本地
	got, err := svc.SumLossByProduct(context.Background(), "B1", []string{"P1", "P2"}, "2026-08-01", "2026-11-01")
	if err != nil {
		t.Fatalf("SumLossByProduct: %v", err)
	}
	if got["P1"].String() != "1" {
		t.Errorf("P1 盘损 = %s, want 1(2.5 的行被软删后只剩 1.0)", got["P1"].String())
	}
	if _, ok := got["P2"]; ok {
		t.Errorf("盘盈行不应计入: %v", got)
	}
}

// TestLossService_LocalPG_NoDB 无源门店但 PG 未装配 ⇒ 报错(不是静默空结果)。
func TestLossService_LocalPG_NoDB(t *testing.T) {
	svc := NewLossService(nil, nil, nil)
	if _, err := svc.SumLossByProduct(context.Background(), "B1", []string{"P1"}, "a", "b"); err == nil {
		t.Error("db 未装配应报错")
	}
}

// ---- 测试基建 ----

func openStocktakeDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := testdb.OpenSQLite(":memory:?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.StocktakeHeader{}, &model.StocktakeLine{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// seedStocktake 落一张盘点单头 + 若干行(只填盘损用例关心的列)。
func seedStocktake(t *testing.T, db *gorm.DB, h model.StocktakeHeader, lines []model.StocktakeLine) model.StocktakeHeader {
	t.Helper()
	if err := db.Create(&h).Error; err != nil {
		t.Fatalf("create header %s: %v", h.ID, err)
	}
	for i := range lines {
		ln := lines[i]
		ln.ID = h.ID + "-L" + decimal.NewFromInt(int64(i)).String()
		ln.HeaderID = h.ID
		ln.ActualQty = ln.DiffQty
		ln.BookQty = decimal.Zero
		ln.BookQtyAt = h.CountDate
		ln.AvgCostYuan = decimal.Zero
		ln.DiffAmountYuan = decimal.Zero
		if err := db.Create(&ln).Error; err != nil {
			t.Fatalf("create line: %v", err)
		}
	}
	return h
}
