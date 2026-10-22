// 本文件锁住「流水查询域」的「门店 → 数据源」分流、响应形状与过滤口径。
//
// # 为什么要有这组用例
//
// 2026-10-21 生产事故的形状:读端点只走一条路 ⇒ 没配数据源的门店
// 直接 404 / 空列表,而它本地明明有数据。
//
// ⇒ 三态必须被测出来:
//
//	配了源 ⇒ 走 cube
//	没配源 ⇒ 走本域 PG(**真的返回本地流水**,不是只断言状态码)
//	问不到 ⇒ 报错(不是空列表!)
//
// ⚠️ "问不到 ⇒ 空列表"是最坏的一种失败:它把一次链路故障
// 显示成"这个门店没有流水",而根因与结论完全无关。
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
	"github.com/YunBright/supertrade/internal/transaction/model"
	"github.com/YunBright/supertrade/internal/transaction/service"
)

// seedDay 是本地种子数据的时间(与 seedLocal 的 base 同一天)。
//
// ⚠️ 流水端点在**完全不给时间区间**时会补"今天"(见 service.defaultDayRange)——
// 因为逐笔流水表是 330 万 / 114 万行,不带区间就会全表扫 ⇒ 撞 10s 超时。
// ⇒ 这里的用例**显式**给出区间,断言的是"指定区间 ⇒ 命中这些行"。
const seedDay = "2026-10-22"

// ---- 测试替身 ----

// fakeSource 替身 cubesource.Resolver(三态:配了 / 没配 / 问不到)。
type fakeSource struct {
	src cubesource.Source
	err error
	calls int
	branch string
}

func (f *fakeSource) Resolve(_ context.Context, branchID string) (cubesource.Source, error) {
	f.calls++
	f.branch = branchID
	if f.err != nil {
		return cubesource.Source{}, f.err
	}
	return f.src, nil
}

func configuredSource() cubesource.Source {
	return cubesource.Source{BranchID: "00", Configured: true, Enabled: true, CubeSourceName: "cube-sixun-ysx-fb"}
}

func localSource() cubesource.Source {
	return cubesource.Source{BranchID: "10"}
}

// newTestDB 构造**文件型** SQLite(同 internal/pos/handler 的做法)。
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "transaction.db")
	dialector := sqlite.Dialector{DriverName: "sqlite", DSN: dsn}
	db, err := gorm.Open(dialector, &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.SaleFlow{}, &model.PayFlow{}), "auto migrate")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// newSvc 构造一个**带数据源解析器 + 本地库 + cube 读端**的服务。
func newSvc(t *testing.T, src *fakeSource) (*service.Service, *gorm.DB, *cubereadfake.Client) {
	t.Helper()
	db := newTestDB(t)
	reader := cubereadfake.New()
	reader.Handler = func(m string, q cubeclient.CubeQuery) ([]map[string]any, int, error) {
		switch m {
		case "sale_detail":
			rows := []map[string]any{
				{
					"sale_detail.id":             "9",
					"sale_detail.order_no":       "06202610220435",
					"sale_detail.random_order_no": "06429097872618",
					"sale_detail.branch_no":      "0001",
					"sale_detail.product_id":     "6911754103296",
					"sale_detail.source_price":   "2.5",
					"sale_detail.sale_price":     "2.5",
					"sale_detail.qty":            "1",
					"sale_detail.sale_amount":    "2.5",
					"sale_detail.sale_way":       "A",
					"sale_detail.cashier_id":     "1012",
					"sale_detail.salesman_id":    "9999",
					"sale_detail.counter_no":     "9999",
					"sale_detail.sold_at":        "10 22 2026  9:54PM",
					"sale_detail.online_flag":    "0",
					"sale_detail.discount_amount": "0",
					"product.sub_code":           "00001",
					"product.name":               "小白菜",
					"product.spec":               "500g",
					"product.unit":               "kg",
					"product.brand_name":         "其它品牌",
				},
			}
			return rows, len(rows), nil
		case "payflow":
			rows := []map[string]any{
				{
					"payflow.id":              "1",
					"payflow.order_no":        "06202610220435",
					"payflow.random_order_no": "06429097872618",
					"payflow.branch_no":       "0001",
					"payflow.sale_way":        "A",
					"payflow.pay_way":         "SWX",
					"payflow.card_no":         "",
					"payflow.vip_no":          "",
					"payflow.exchange_rate":   "1",
					"payflow.pay_amount":      "22",
					"payflow.sale_amount":     "22",
					"payflow.paid_at":         "10 22 2026  9:54PM",
					"payflow.cashier_id":      "1012",
					"payflow.salesman_id":     "9999",
					"payflow.counter_no":      "9999",
				},
			}
			return rows, len(rows), nil
		}
		return nil, 0, nil
	}
	svc := service.New(db, nil)
	svc.SetCubeSource(src, reader)
	return svc, db, reader
}

// branchCtx 造一个带门店的 ctx。
func branchCtx(branchID string) context.Context {
	return cubeclient.WithBranchID(context.Background(), branchID)
}

// seedLocal 灌本域 PG 的门店 10 流水。
func seedLocal(t *testing.T, db *gorm.DB) {
	t.Helper()
	base := time.Date(2026, 10, 22, 21, 54, 52, 0, time.UTC)
	rows := []any{
		&model.SaleFlow{ID: "L-1", BranchID: "10", OrderNo: "SO-1", SourceBranch: "0001",
			ProductID: "P-1", CategoryID: "C1", ProductName: "本地小白菜", Unit: "kg",
			Qty: decimal.NewFromFloat(2), SaleAmount: decimal.NewFromFloat(5),
			SalePrice: decimal.NewFromFloat(2.5), SourcePrice: decimal.NewFromFloat(2.5),
			SoldAt: base, CreatedAt: base},
		&model.SaleFlow{ID: "L-2", BranchID: "10", OrderNo: "SO-2", SourceBranch: "0001",
			ProductID: "P-2", CategoryID: "C2", ProductName: "本地饮料", Unit: "瓶",
			Qty: decimal.NewFromInt(1), SaleAmount: decimal.NewFromInt(3),
			SalePrice: decimal.NewFromInt(3), SourcePrice: decimal.NewFromInt(3),
			SoldAt: base, CreatedAt: base},
		&model.PayFlow{ID: "PF-1", BranchID: "10", OrderNo: "SO-1", SourceBranch: "0001",
			PayWay: "SWX", SaleAmount: decimal.NewFromInt(5), ExchangeRate: decimal.NewFromInt(1),
			PayAmount: decimal.NewFromInt(5), PaidAt: base, CreatedAt: base},
	}
	for _, r := range rows {
		require.NoError(t, db.Create(r).Error, "seed")
	}
}

// ---- 三态 ----

// TestListSales_ConfiguredSourceReadsCube 锁"配了数据源 ⇒ 走 cube"。
func TestListSales_ConfiguredSourceReadsCube(t *testing.T) {
	src := &fakeSource{src: configuredSource()}
	svc, db, _ := newSvc(t, src)
	seedLocal(t, db)

	page, err := svc.ListSales(branchCtx("00"), "00", service.SaleQuery{From: seedDay, To: seedDay, Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	// ⚠️ 列表行的 id = **单号**(唯一可寻址的单位),不是 flow_id ——
	// 实测 t_rm_saleflow 的 flow_id 在 330 万行里只有 **96** 个不同值,
	// 拿它当 id ⇒ 详情端点会命中**别的单**(实测命中 2021 年的另一张单)。
	assert.Equal(t, "06202610220435", page.Items[0].ID, "必须来自 cube")
	assert.Equal(t, "cube", page.DataSource)
	assert.Equal(t, 1, src.calls)
	assert.Equal(t, "00", src.branch)
}

// TestListSales_NoSourceReadsLocal 锁"没配数据源 ⇒ 走本域 PG"。
//
// ⚠️ 断言的是**数据来源**(真的返回了本地流水),不是状态码。
func TestListSales_NoSourceReadsLocal(t *testing.T) {
	src := &fakeSource{src: localSource()}
	svc, db, _ := newSvc(t, src)
	seedLocal(t, db)

	page, err := svc.ListSales(branchCtx("10"), "10", service.SaleQuery{From: seedDay, To: seedDay, Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	assert.Equal(t, "local", page.DataSource)
	assert.Equal(t, 2, page.Total)
}

// TestListSales_ResolveFailureIsNotEmptyList 锁"问不到 ⇒ 报错,不是空列表"。
func TestListSales_ResolveFailureIsNotEmptyList(t *testing.T) {
	src := &fakeSource{err: errors.New("cube-router 不可达")}
	svc, db, _ := newSvc(t, src)
	seedLocal(t, db)

	_, err := svc.ListSales(branchCtx("10"), "10", service.SaleQuery{From: seedDay, To: seedDay, Page: 1, PageSize: 50})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCubeSourceUnavailable)
}

// TestListSales_SourceDisabledIsNotLocalFallback 锁"数据源被停用 ⇒ 报错"。
func TestListSales_SourceDisabledIsNotLocalFallback(t *testing.T) {
	disabled := configuredSource()
	disabled.Enabled = false
	src := &fakeSource{src: disabled}
	svc, db, _ := newSvc(t, src)
	seedLocal(t, db)

	_, err := svc.ListSales(branchCtx("10"), "10", service.SaleQuery{From: seedDay, To: seedDay, Page: 1, PageSize: 50})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCubeSourceDisabled)
}

// ---- 派生列 ----

// TestSaleFlow_DiscountRateIsDerived 锁"折扣度是派生的,且 source_price=0 ⇒ null"。
//
// ⚠️ **绝不编 100%**:源库没有原价时,"零折"是假的。
func TestSaleFlow_DiscountRateIsDerived(t *testing.T) {
	src := &fakeSource{src: localSource()}
	svc, db, _ := newSvc(t, src)
	base := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	require.NoError(t, db.Create(&model.SaleFlow{
		ID: "L-3", BranchID: "10", OrderNo: "SO-3", ProductID: "P-3",
		Qty: decimal.NewFromInt(1), SaleAmount: decimal.NewFromInt(2),
		SalePrice: decimal.NewFromInt(2), SourcePrice: decimal.NewFromInt(0),
		SoldAt: base, CreatedAt: base,
	}).Error)

	page, err := svc.ListSales(branchCtx("10"), "10", service.SaleQuery{From: seedDay, To: seedDay, ProductID: "P-3", Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	assert.Nil(t, page.Items[0].DiscountRate, "source_price=0 ⇒ 折扣度必须是 null")
	// 原价金额 = source_price × qty = 0。
	require.NotNil(t, page.Items[0].SourceAmount)
	assert.Equal(t, "0.0000", *page.Items[0].SourceAmount)

	// 正常行:折扣度 = sale_price / source_price × 100。
	require.NoError(t, db.Create(&model.SaleFlow{
		ID: "L-4", BranchID: "10", OrderNo: "SO-4", ProductID: "P-4",
		Qty: decimal.NewFromInt(2), SaleAmount: decimal.NewFromFloat(1.5),
		SalePrice: decimal.NewFromFloat(0.75), SourcePrice: decimal.NewFromInt(1),
		SoldAt: base, CreatedAt: base,
	}).Error)
	page, err = svc.ListSales(branchCtx("10"), "10", service.SaleQuery{From: seedDay, To: seedDay, ProductID: "P-4", Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.NotNil(t, page.Items[0].DiscountRate)
	assert.Equal(t, "75.00", *page.Items[0].DiscountRate)
	require.NotNil(t, page.Items[0].SourceAmount)
	assert.Equal(t, "2.0000", *page.Items[0].SourceAmount)
}

// ---- 过滤真的生效 ----

// TestListSales_LocalFilterNarrowsResult 锁"过滤真的收窄命中数"。
//
// ⚠️ 只断言"200 + 有字段"**抓不到**一个被静默忽略的过滤器。
func TestListSales_LocalFilterNarrowsResult(t *testing.T) {
	src := &fakeSource{src: localSource()}
	svc, db, _ := newSvc(t, src)
	seedLocal(t, db)

	all, err := svc.ListSales(branchCtx("10"), "10", service.SaleQuery{From: seedDay, To: seedDay, Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.Equal(t, 2, all.Total)

	byProduct, err := svc.ListSales(branchCtx("10"), "10", service.SaleQuery{From: seedDay, To: seedDay, ProductID: "P-1", Page: 1, PageSize: 50})
	require.NoError(t, err)
	assert.Equal(t, 1, byProduct.Total, "商品过滤必须真的收窄命中数")

	byCategory, err := svc.ListSales(branchCtx("10"), "10", service.SaleQuery{From: seedDay, To: seedDay, CategoryID: "C2", Page: 1, PageSize: 50})
	require.NoError(t, err)
	assert.Equal(t, 1, byCategory.Total, "分类过滤必须真的收窄命中数")

	byKeyword, err := svc.ListSales(branchCtx("10"), "10", service.SaleQuery{From: seedDay, To: seedDay, Q: "SO-1", Page: 1, PageSize: 50})
	require.NoError(t, err)
	assert.Equal(t, 1, byKeyword.Total, "单号过滤必须真的收窄命中数")

	none, err := svc.ListSales(branchCtx("10"), "10", service.SaleQuery{From: seedDay, To: seedDay, CashierID: "NOPE", Page: 1, PageSize: 50})
	require.NoError(t, err)
	assert.Equal(t, 0, none.Total, "不存在的条件 ⇒ 命中 0 行(不是不过滤)")
}

// TestListSales_CubeQueryCarriesFilters 锁"cube 查询真的带了过滤条件"。
func TestListSales_CubeQueryCarriesFilters(t *testing.T) {
	src := &fakeSource{src: configuredSource()}
	svc, _, reader := newSvc(t, src)

	_, err := svc.ListSales(branchCtx("00"), "00", service.SaleQuery{From: seedDay, To: seedDay, 
		ProductID: "P-1", CategoryID: "C1", SaleWay: "A", CashierID: "1012",
		CounterNo: "9999", BranchID: "0001", Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	q, ok := reader.LastQuery("sale_detail")
	require.True(t, ok)
	assert.True(t, q.HasFilter("sale_detail.product_id"))
	assert.True(t, q.HasFilter("product.category_id"))
	assert.True(t, q.HasFilter("sale_detail.sale_way"))
	assert.True(t, q.HasFilter("sale_detail.cashier_id"))
	assert.True(t, q.HasFilter("sale_detail.counter_no"))
	assert.True(t, q.HasFilter("sale_detail.branch_no"))
	assert.Equal(t, []any{"P-1"}, q.FilterValues("sale_detail.product_id"))
}

// ---- 两条路径的响应结构必须逐字相同 ----

// TestSaleFlow_LocalAndCubeHaveSameShape 锁"本地路径与 cube 路径结构相同"。
func TestSaleFlow_LocalAndCubeHaveSameShape(t *testing.T) {
	cubeSvc, _, _ := newSvc(t, &fakeSource{src: configuredSource()})
	cubePage, err := cubeSvc.ListSales(branchCtx("00"), "00", service.SaleQuery{From: seedDay, To: seedDay, Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.Len(t, cubePage.Items, 1)

	localSvc, db, _ := newSvc(t, &fakeSource{src: localSource()})
	seedLocal(t, db)
	localPage, err := localSvc.ListSales(branchCtx("10"), "10", service.SaleQuery{From: seedDay, To: seedDay, Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.NotEmpty(t, localPage.Items)

	assert.Equal(t, keysOf(t, cubePage.Items[0]), keysOf(t, localPage.Items[0]),
		"两条路径的 sale 流水行字段集合必须一致")
	assert.Equal(t, keysOf(t, *cubePage), keysOf(t, *localPage),
		"两条路径的信封字段集合必须一致")
}

// TestPayFlow_LocalAndCubeHaveSameShape 锁"收款流水"两条路径结构相同。
func TestPayFlow_LocalAndCubeHaveSameShape(t *testing.T) {
	cubeSvc, _, _ := newSvc(t, &fakeSource{src: configuredSource()})
	cubePage, err := cubeSvc.ListPayments(branchCtx("00"), "00", service.PayQuery{From: seedDay, To: seedDay, Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.Len(t, cubePage.Items, 1)

	localSvc, db, _ := newSvc(t, &fakeSource{src: localSource()})
	seedLocal(t, db)
	localPage, err := localSvc.ListPayments(branchCtx("10"), "10", service.PayQuery{From: seedDay, To: seedDay, Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.Len(t, localPage.Items, 1)

	assert.Equal(t, keysOf(t, cubePage.Items[0]), keysOf(t, localPage.Items[0]),
		"两条路径的 pay 流水行字段集合必须一致")
	assert.Equal(t, keysOf(t, *cubePage), keysOf(t, *localPage),
		"两条路径的信封字段集合必须一致")
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
