// 本文件锁住「采购收货域」的「门店 → 数据源」分流、响应形状与过滤口径。
//
// # 为什么要有这组用例
//
// 同 catalog / transaction:读端点必须按"配没配数据源"分流,
// 且**问不到 ⇒ 报错**,不许回落成本地(回落 = 把一次链路故障显示成
// "这家店没有采购单")。
//
// # ⚠️ 额外锁一条:**净额必须用 total_net_amount_yuan**
//
// 采购侧 t_pm_sheet_master 的 RO 退货单是**正数**,结算侧是**负数**
// ⇒ 直接对比会差一个符号、约 346 万(2026-10-10 实测)。
package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	_ "modernc.org/sqlite" // 注册驱动名 "sqlite"(纯 Go 驱动,无需 CGO)

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cuberead/cubereadfake"
	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/procurement/model"
	"github.com/YunBright/supertrade/internal/procurement/service"
)

// ---- 测试替身 ----

type fakeSource struct {
	src   cubesource.Source
	err   error
	calls int
}

func (f *fakeSource) Resolve(_ context.Context, branchID string) (cubesource.Source, error) {
	f.calls++
	if f.err != nil {
		return cubesource.Source{}, f.err
	}
	return f.src, nil
}

func configuredSource() cubesource.Source {
	return cubesource.Source{BranchID: "00", Configured: true, Enabled: true}
}

func localSource() cubesource.Source {
	return cubesource.Source{BranchID: "10"}
}

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "procurement.db")
	dialector := sqlite.Dialector{DriverName: "sqlite", DSN: dsn}
	db, err := gorm.Open(dialector, &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.PurchaseSheet{}, &model.PurchaseSheetLineRow{}), "auto migrate")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// newSvc 构造服务(解析器 + 本地库 + cube 读端三件都装)。
func newSvc(t *testing.T, src *fakeSource) (*service.Service, *gorm.DB, *cubereadfake.Client) {
	t.Helper()
	db := newTestDB(t)
	reader := cubereadfake.New()
	reader.Handler = func(m string, q cubeclient.CubeQuery) ([]map[string]any, int, error) {
		switch m {
		case "purchase_sheet":
			// ⚠️ 按查询里的单据号过滤(模拟 cube 的 filter 语义)——
			// 否则"查一个不存在的单号"也会返回行,404 用例就测不出来。
			for _, f := range q.Filters {
				if f.Member != "purchase_sheet.id" {
					continue
				}
				want := ""
				if len(f.Values) > 0 {
					want, _ = f.Values[0].(string)
				}
				if want != "" && want != "PI002610224944" && want != "RO002610224944" {
					return nil, 0, nil
				}
			}
			rows := []map[string]any{
				{
					"purchase_sheet.id":                 "PI002610224944",
					"purchase_sheet.supplier_id":        "084",
					"purchase_sheet.document_type":      "PI",
					"purchase_sheet.approve_status":     "1",
					"purchase_sheet.order_status":       "finished",
					"purchase_sheet.branch_id":          "0001",
					"purchase_sheet.pay_way":            "CASH",
					"purchase_sheet.currency":           "RMB",
					"purchase_sheet.created_at":         "10 22 2026 12:00AM",
					"purchase_sheet.finished_at":        "10 22 2026 12:00AM",
					"purchase_sheet.operator_id":        "3001",
					"purchase_sheet.confirmer":          "3001",
					"purchase_sheet.remark":             "",
					"purchase_sheet.total_amount_yuan":    "241",
					"purchase_sheet.total_net_amount_yuan": "241",
				},
				{
					"purchase_sheet.id":                 "RO002610224944",
					"purchase_sheet.supplier_id":        "084",
					"purchase_sheet.document_type":      "RO",
					"purchase_sheet.approve_status":     "0",
					"purchase_sheet.order_status":       "finished",
					"purchase_sheet.branch_id":          "0001",
					"purchase_sheet.pay_way":            "CASH",
					"purchase_sheet.currency":           "RMB",
					"purchase_sheet.created_at":         "10 22 2026 12:00AM",
					"purchase_sheet.finished_at":        "",
					"purchase_sheet.operator_id":        "3001",
					"purchase_sheet.confirmer":          "",
					"purchase_sheet.remark":             "",
					"purchase_sheet.total_amount_yuan":    "100",
					"purchase_sheet.total_net_amount_yuan": "-100",
				},
			}
			return rows, len(rows), nil
		case "purchase_sheet_line":
			rows := []map[string]any{
				{
					"purchase_sheet_line.id":                     "1",
					"purchase_sheet_line.sheet_id":               "PI002610224944",
					"purchase_sheet_line.product_id":             "000552",
					"purchase_sheet_line.total_amount_yuan":      "122",
					"purchase_sheet_line.total_received_quantity": "19.6",
					"purchase_sheet_line.total_order_quantity":   "19.6",
					"purchase_sheet_line.avg_valid_price_yuan":   "6.22",
				},
			}
			return rows, len(rows), nil
		case "supplier":
			return []map[string]any{
				{"supplier.id": "084", "supplier.name": "水果(自营)"},
			}, 1, nil
		case "product":
			return []map[string]any{
				{"product.id": "000552", "product.name": "凯特芒", "product.unit": "kg"},
			}, 1, nil
		}
		return nil, 0, nil
	}
	svc := service.New(db, nil)
	svc.SetCubeSource(src, reader)
	return svc, db, reader
}

func branchCtx(branchID string) context.Context {
	return cubeclient.WithBranchID(context.Background(), branchID)
}

// seedLocal 灌本域 PG 的门店 10 采购单。
func seedLocal(t *testing.T, db *gorm.DB) {
	t.Helper()
	created := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	rows := []any{
		&model.PurchaseSheet{SheetNo: "PI-1", BranchID: "10", SourceBranch: "0001",
			SupplierID: "084", SupplierName: "水果(自营)", DocumentType: "PI",
			ApproveStatus: "approved", OrderStatus: "finished",
			CreatedAt: &created, CreatedBy: "3001",
			TotalAmountYuan: decimal.NewFromInt(241), TotalNetAmountYuan: decimal.NewFromInt(241),
			Currency: "CNY", PayWay: "CASH",
			CreatedAt2: created, UpdatedAt2: created},
		&model.PurchaseSheet{SheetNo: "RO-1", BranchID: "10", SourceBranch: "0001",
			SupplierID: "084", DocumentType: "RO", ApproveStatus: "draft",
			CreatedAt: &created, CreatedBy: "3001",
			TotalAmountYuan: decimal.NewFromInt(100), TotalNetAmountYuan: decimal.NewFromInt(-100),
			Currency: "CNY",
			CreatedAt2: created, UpdatedAt2: created},
		&model.PurchaseSheetLineRow{SheetID: "PI-1", LineNo: 1, BranchID: "10",
			ProductID: "000552", ProductName: "凯特芒", Unit: "kg",
			OrderQty: decimal.NewFromFloat(19.6), ReceivedQty: decimal.NewFromFloat(19.6),
			ValidPriceYuan: decimal.NewFromFloat(6.22), AmountYuan: decimal.NewFromInt(122),
			CreatedAt2: created, UpdatedAt2: created},
	}
	for _, r := range rows {
		require.NoError(t, db.Create(r).Error, "seed")
	}
}

// ---- 三态 ----

func TestListSheets_ConfiguredSourceReadsCube(t *testing.T) {
	src := &fakeSource{src: configuredSource()}
	svc, db, _ := newSvc(t, src)
	seedLocal(t, db)

	page, err := svc.List(branchCtx("00"), "00", service.Query{Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	assert.Equal(t, "cube", page.DataSource)
	assert.Equal(t, "水果(自营)", *page.Items[0].SupplierName)
	// ⚠️ 审核状态是**翻译后**的,不是源码 "1"。
	assert.Equal(t, "approved", page.Items[0].ApproveStatus)
	assert.Equal(t, "已审核", page.Items[0].ApproveStatusLabel)
	// ⚠️ 币种归一成 CNY(源库是 RMB)。
	assert.Equal(t, "CNY", page.Items[0].Currency)
	// ⚠️ **净额**才是对账口径(退货取负)。
	assert.Equal(t, "-100.00", *page.Items[1].TotalNetAmountYuan)
	assert.Equal(t, "100.00", *page.Items[1].TotalAmountYuan)
}

func TestListSheets_NoSourceReadsLocal(t *testing.T) {
	src := &fakeSource{src: localSource()}
	svc, db, _ := newSvc(t, src)
	seedLocal(t, db)

	page, err := svc.List(branchCtx("10"), "10", service.Query{Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	assert.Equal(t, "local", page.DataSource)
}

func TestListSheets_ResolveFailureIsNotEmptyList(t *testing.T) {
	src := &fakeSource{err: errors.New("cube-router 不可达")}
	svc, db, _ := newSvc(t, src)
	seedLocal(t, db)

	_, err := svc.List(branchCtx("10"), "10", service.Query{Page: 1, PageSize: 50})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCubeSourceUnavailable)
}

func TestListSheets_SourceDisabledIsNotLocalFallback(t *testing.T) {
	disabled := configuredSource()
	disabled.Enabled = false
	src := &fakeSource{src: disabled}
	svc, db, _ := newSvc(t, src)
	seedLocal(t, db)

	_, err := svc.List(branchCtx("10"), "10", service.Query{Page: 1, PageSize: 50})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCubeSourceDisabled)
}

// ---- 过滤真的生效 ----

func TestListSheets_LocalFilterNarrowsResult(t *testing.T) {
	src := &fakeSource{src: localSource()}
	svc, db, _ := newSvc(t, src)
	seedLocal(t, db)

	all, err := svc.List(branchCtx("10"), "10", service.Query{Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.Equal(t, 2, all.Total)

	byNo, err := svc.List(branchCtx("10"), "10", service.Query{OrderNo: "PI-1", Page: 1, PageSize: 50})
	require.NoError(t, err)
	assert.Equal(t, 1, byNo.Total, "单号过滤必须真的收窄命中数")

	byType, err := svc.List(branchCtx("10"), "10", service.Query{DocumentType: "PI", Page: 1, PageSize: 50})
	require.NoError(t, err)
	assert.Equal(t, 1, byType.Total, "单据类型过滤必须真的收窄命中数")

	byStatus, err := svc.List(branchCtx("10"), "10", service.Query{ApproveStatus: "approved", Page: 1, PageSize: 50})
	require.NoError(t, err)
	assert.Equal(t, 1, byStatus.Total, "审核状态过滤必须真的收窄命中数")

	none, err := svc.List(branchCtx("10"), "10", service.Query{SupplierID: "NOPE", Page: 1, PageSize: 50})
	require.NoError(t, err)
	assert.Equal(t, 0, none.Total)
}

func TestListSheets_CubeQueryCarriesFilters(t *testing.T) {
	src := &fakeSource{src: configuredSource()}
	svc, _, reader := newSvc(t, src)

	_, err := svc.List(branchCtx("00"), "00", service.Query{
		SupplierID: "084", OrderNo: "PI-1", ApproveStatus: "approved",
		DocumentType: "PI", BranchID: "0001", Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	q, ok := reader.LastQuery("purchase_sheet")
	require.True(t, ok)
	assert.True(t, q.HasFilter("purchase_sheet.supplier_id"))
	assert.True(t, q.HasFilter("purchase_sheet.id"))
	assert.True(t, q.HasFilter("purchase_sheet.approve_status"))
	assert.True(t, q.HasFilter("purchase_sheet.document_type"))
	assert.True(t, q.HasFilter("purchase_sheet.branch_id"))
	// ⚠️ 状态是译文 ⇒ 必须反查源码集合。
	assert.Equal(t, []any{"1"}, q.FilterValues("purchase_sheet.approve_status"))
}

// ---- 详情 ----

func TestGetSheet_LocalAndCubeHaveSameShape(t *testing.T) {
	cubeSvc, _, _ := newSvc(t, &fakeSource{src: configuredSource()})
	cubeDetail, err := cubeSvc.Get(branchCtx("00"), "00", "PI002610224944")
	require.NoError(t, err)
	require.Len(t, cubeDetail.Lines, 1)

	localSvc, db, _ := newSvc(t, &fakeSource{src: localSource()})
	seedLocal(t, db)
	localDetail, err := localSvc.Get(branchCtx("10"), "10", "PI-1")
	require.NoError(t, err)
	require.Len(t, localDetail.Lines, 1)

	assert.Equal(t, keysOf(t, *cubeDetail), keysOf(t, *localDetail),
		"两条路径的详情字段集合必须一致")
	assert.Equal(t, keysOf(t, cubeDetail.Lines[0]), keysOf(t, localDetail.Lines[0]),
		"两条路径的明细行字段集合必须一致")
	assert.Equal(t, 1, cubeDetail.Lines[0].LineNo)
	assert.Equal(t, 1, localDetail.Lines[0].LineNo)
}

func TestGetSheet_NotFound(t *testing.T) {
	svc, _, _ := newSvc(t, &fakeSource{src: configuredSource()})
	_, err := svc.Get(branchCtx("00"), "00", "NOPE")
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrPurchaseSheetNotFound)
}

// keysOf 把任意结构体序列化后取键集合(排序)。
func keysOf(t *testing.T, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
