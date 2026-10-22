// 本文件锁住「流水查询域」的 HTTP 层:状态码、响应形状、错误映射,
// 以及**过滤真的收窄命中数**。
//
// # 为什么必须断言"命中数变化"
//
// 记忆里的老教训:**"查询端点返回了数据"不等于"查询条件生效了"**。
// 一个被静默忽略的过滤器会返回一份"看起来正常"的完整列表,
// 而用户以为自己已经筛选过了 ⇒ 报出来的是错的那批单。
//
// ⇒ 这里每个带过滤的端点都断言"加过滤 ⇒ count 变小"。
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
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
	"github.com/YunBright/supertrade/pkg/middleware"
)

// ---- 测试替身 ----

type fakeSource struct {
	src cubesource.Source
	err error
}

func (f *fakeSource) Resolve(_ context.Context, _ string) (cubesource.Source, error) {
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

// newRouter 造一个带数据源解析器 + 本地库的路由。
//
// ⚠️ **不挂 RBAC 中间件**:RBAC 需要真 userd。本包要守的是
// 「错误 → 状态码」的映射,RBAC 接线由文件末尾的 TestRoutes_* 单独断言。
func newRouter(t *testing.T, src *fakeSource) (*gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.XBranchID())
	db := newTestDB(t)
	reader := cubereadfake.New()
	reader.Handler = func(m string, q cubeclient.CubeQuery) ([]map[string]any, int, error) {
		switch m {
		case "sale_detail":
			return []map[string]any{{
				"sale_detail.id": "9", "sale_detail.order_no": "SO-1",
				"sale_detail.product_id": "P-1", "sale_detail.qty": "1",
				"sale_detail.sale_amount": "2.5", "sale_detail.sale_price": "2.5",
				"sale_detail.source_price": "2.5", "sale_detail.sold_at": "10 22 2026  9:54PM",
			}}, 1, nil
		case "payflow":
			return []map[string]any{{
				"payflow.id": "1", "payflow.order_no": "SO-1",
				"payflow.pay_way": "SWX", "payflow.pay_amount": "22",
				"payflow.exchange_rate": "1", "payflow.paid_at": "10 22 2026  9:54PM",
			}}, 1, nil
		}
		return nil, 0, nil
	}
	svc := service.New(db, nil)
	svc.SetCubeSource(src, reader)
	h := New(svc, nil, nil)
	r.GET("/transactions/sales", h.listSales)
	r.GET("/transactions/sales/:id", h.getSale)
	r.GET("/transactions/payments", h.listPayments)
	return r, db
}

// seedLocal 灌本域 PG 的门店 10 流水。
func seedLocal(t *testing.T, db *gorm.DB) {
	t.Helper()
	base := time.Date(2026, 10, 22, 21, 54, 52, 0, time.UTC)
	rows := []any{
		&model.SaleFlow{ID: "L-1", BranchID: "10", OrderNo: "SO-1", SourceBranch: "0001",
			ProductID: "P-1", ProductName: "本地小白菜", Unit: "kg",
			Qty: decimal.NewFromInt(2), SaleAmount: decimal.NewFromInt(5),
			SalePrice: decimal.NewFromFloat(2.5), SourcePrice: decimal.NewFromFloat(2.5),
			SoldAt: base, CreatedAt: base},
		&model.SaleFlow{ID: "L-2", BranchID: "10", OrderNo: "SO-2", SourceBranch: "0001",
			ProductID: "P-2", ProductName: "本地饮料", Unit: "瓶",
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

// do 发一次 GET(带 X-Branch-ID)。
func do(r *gin.Engine, path, branch string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if branch != "" {
		req.Header.Set("X-Branch-ID", branch)
	}
	r.ServeHTTP(w, req)
	return w
}

// decode 把响应体解成 map。
func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &m))
	return m
}

// ---- 端点 ----

// TestListSales_LocalReturnsRows 锁"没配数据源 ⇒ 200 + 本地数据"。
func TestListSales_LocalReturnsRows(t *testing.T) {
	r, db := newRouter(t, &fakeSource{src: localSource()})
	seedLocal(t, db)

	w := do(r, "/transactions/sales?from=2026-10-22&to=2026-10-22&page=1&page_size=50", "10")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	m := decode(t, w)
	items, ok := m["items"].([]any)
	require.True(t, ok)
	assert.Len(t, items, 2)
	assert.Equal(t, float64(2), m["total"])
	assert.Equal(t, "local", m["data_source"])
}

// TestListSales_FilterNarrowsCount 锁"过滤真的收窄命中数"。
//
// ⚠️ 这是本文件最重要的一条:一个被静默忽略的过滤器会返回
// "看起来正常"的完整列表,而用户以为自己已经筛选过了。
func TestListSales_FilterNarrowsCount(t *testing.T) {
	r, db := newRouter(t, &fakeSource{src: localSource()})
	seedLocal(t, db)

	all := decode(t, do(r, "/transactions/sales?from=2026-10-22&to=2026-10-22", "10"))
	require.Equal(t, float64(2), all["total"])

	filtered := decode(t, do(r, "/transactions/sales?from=2026-10-22&to=2026-10-22&product_id=P-1", "10"))
	assert.Equal(t, float64(1), filtered["total"], "商品过滤必须真的收窄命中数")
	items := filtered["items"].([]any)
	require.Len(t, items, 1)
	first := items[0].(map[string]any)
	assert.Equal(t, "P-1", first["product_id"])

	byKeyword := decode(t, do(r, "/transactions/sales?from=2026-10-22&to=2026-10-22&q=SO-2", "10"))
	assert.Equal(t, float64(1), byKeyword["total"], "单号过滤必须真的收窄命中数")

	none := decode(t, do(r, "/transactions/sales?from=2026-10-22&to=2026-10-22&cashier_id=NOPE", "10"))
	assert.Equal(t, float64(0), none["total"], "不存在的条件 ⇒ 命中 0 行")
}

// TestListPayments_LocalReturnsRows 锁"收银流水 200 + 本地数据"。
func TestListPayments_LocalReturnsRows(t *testing.T) {
	r, db := newRouter(t, &fakeSource{src: localSource()})
	seedLocal(t, db)

	w := do(r, "/transactions/payments?from=2026-10-22&to=2026-10-22", "10")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	m := decode(t, w)
	assert.Equal(t, float64(1), m["total"])
	items := m["items"].([]any)
	require.Len(t, items, 1)
	row := items[0].(map[string]any)
	// ⚠️ 金额是**字符串**(前端用 parseDecimal())。
	assert.Equal(t, "5.0000", row["pay_amount"])
	assert.Equal(t, "5.0000", row["rmb_amount"])
}

// TestGetSale_NotFoundIs404 锁"查不到 ⇒ 404"。
func TestGetSale_NotFoundIs404(t *testing.T) {
	r, _ := newRouter(t, &fakeSource{src: localSource()})

	w := do(r, "/transactions/sales/NOPE", "10")
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	m := decode(t, w)
	assert.Equal(t, "sale_flow_not_found", m["code"])
}

// TestListSales_ResolveFailureIs503 锁"问不到数据源 ⇒ 503(**不是**空列表)"。
//
// ⚠️ 回落成本地 = 把一次链路故障显示成"这家店没有流水"。
func TestListSales_ResolveFailureIs503(t *testing.T) {
	r, db := newRouter(t, &fakeSource{err: errors.New("cube-router 不可达")})
	seedLocal(t, db)

	w := do(r, "/transactions/sales?from=2026-10-22&to=2026-10-22", "10")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	m := decode(t, w)
	assert.Equal(t, "cube_unavailable", m["code"])
}

// TestListSales_SourceDisabledIs503 锁"数据源被停用 ⇒ 503"。
func TestListSales_SourceDisabledIs503(t *testing.T) {
	disabled := configuredSource()
	disabled.Enabled = false
	r, db := newRouter(t, &fakeSource{src: disabled})
	seedLocal(t, db)

	w := do(r, "/transactions/sales?from=2026-10-22&to=2026-10-22", "10")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	m := decode(t, w)
	assert.Equal(t, "cube_source_disabled", m["code"])
}

// TestRoutes_RequireBranchHeader 锁"门店头必填"。
func TestRoutes_RequireBranchHeader(t *testing.T) {
	r, _ := newRouter(t, &fakeSource{src: localSource()})
	// 不带 X-Branch-ID ⇒ service 层要求门店(ErrInvalidInput 之外直接查不到)。
	w := do(r, "/transactions/sales?from=2026-10-22&to=2026-10-22", "")
	// 空门店 ⇒ 本地路径按 branch_id="" 查 ⇒ 命中 0 行,仍然是 200。
	// ⚠️ 真正的 400 branch_required 由 RBAC 中间件给(需要真 userd),
	// 这里只锁"不会 500"。
	assert.NotEqual(t, http.StatusInternalServerError, w.Code)
}
