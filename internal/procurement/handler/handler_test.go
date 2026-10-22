// 本文件锁住「采购收货域」的 HTTP 层:状态码、响应形状、错误映射,
// 以及**过滤真的收窄命中数**。
//
// ⚠️ "查询端点返回了数据"不等于"查询条件生效了"——
// 一个被静默忽略的过滤器会返回"看起来正常"的完整列表。
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
	"github.com/YunBright/supertrade/internal/procurement/model"
	"github.com/YunBright/supertrade/internal/procurement/service"
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

// newRouter 造一个带数据源解析器 + 本地库的路由(不挂 RBAC:需要真 userd)。
func newRouter(t *testing.T, src *fakeSource) (*gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.XBranchID())
	db := newTestDB(t)
	reader := cubereadfake.New()
	reader.Handler = func(m string, q cubeclient.CubeQuery) ([]map[string]any, int, error) {
		switch m {
		case "purchase_sheet":
			return []map[string]any{{
				"purchase_sheet.id": "PI-1", "purchase_sheet.supplier_id": "084",
				"purchase_sheet.document_type": "PI", "purchase_sheet.approve_status": "1",
				"purchase_sheet.branch_id": "0001", "purchase_sheet.currency": "RMB",
				"purchase_sheet.created_at": "10 22 2026 12:00AM",
				"purchase_sheet.total_amount_yuan": "241",
				"purchase_sheet.total_net_amount_yuan": "241",
			}}, 1, nil
		case "purchase_sheet_line":
			return []map[string]any{{
				"purchase_sheet_line.id": "1", "purchase_sheet_line.sheet_id": "PI-1",
				"purchase_sheet_line.product_id": "000552",
				"purchase_sheet_line.total_amount_yuan": "122",
				"purchase_sheet_line.total_received_quantity": "19.6",
				"purchase_sheet_line.total_order_quantity": "19.6",
				"purchase_sheet_line.avg_valid_price_yuan": "6.22",
			}}, 1, nil
		case "supplier":
			return []map[string]any{{"supplier.id": "084", "supplier.name": "水果(自营)"}}, 1, nil
		case "product":
			return []map[string]any{{"product.id": "000552", "product.name": "凯特芒", "product.unit": "kg"}}, 1, nil
		}
		return nil, 0, nil
	}
	svc := service.New(db, nil)
	svc.SetCubeSource(src, reader)
	h := New(svc, nil, nil)
	r.GET("/purchase-sheets", h.listSheets)
	r.GET("/purchase-sheets/:sheet_no", h.getSheet)
	return r, db
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
			Currency: "CNY", PayWay: "CASH", CreatedAt2: created, UpdatedAt2: created},
		&model.PurchaseSheet{SheetNo: "RO-1", BranchID: "10", SourceBranch: "0001",
			SupplierID: "085", DocumentType: "RO", ApproveStatus: "draft",
			CreatedAt: &created, CreatedBy: "3001",
			TotalAmountYuan: decimal.NewFromInt(100), TotalNetAmountYuan: decimal.NewFromInt(-100),
			Currency: "CNY", CreatedAt2: created, UpdatedAt2: created},
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

func do(r *gin.Engine, path, branch string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if branch != "" {
		req.Header.Set("X-Branch-ID", branch)
	}
	r.ServeHTTP(w, req)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &m))
	return m
}

// ---- 端点 ----

// TestListSheets_LocalReturnsRows 锁"没配数据源 ⇒ 200 + 本地数据"。
func TestListSheets_LocalReturnsRows(t *testing.T) {
	r, db := newRouter(t, &fakeSource{src: localSource()})
	seedLocal(t, db)

	w := do(r, "/purchase-sheets?page=1&page_size=50", "10")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	m := decode(t, w)
	assert.Equal(t, float64(2), m["total"])
	assert.Equal(t, "local", m["data_source"])
	items := m["items"].([]any)
	require.Len(t, items, 2)
	row := items[0].(map[string]any)
	assert.Equal(t, "PI-1", row["sheet_no"])
	// ⚠️ 金额是**字符串**。
	assert.Equal(t, "241.00", row["total_amount_yuan"])
	assert.Equal(t, "241.00", row["total_net_amount_yuan"])
	// ⚠️ 审核状态是**翻译后**的。
	assert.Equal(t, "approved", row["approve_status"])
}

// TestListSheets_FilterNarrowsCount 锁"过滤真的收窄命中数"。
func TestListSheets_FilterNarrowsCount(t *testing.T) {
	r, db := newRouter(t, &fakeSource{src: localSource()})
	seedLocal(t, db)

	all := decode(t, do(r, "/purchase-sheets", "10"))
	require.Equal(t, float64(2), all["total"])

	byNo := decode(t, do(r, "/purchase-sheets?order_no=PI-1", "10"))
	assert.Equal(t, float64(1), byNo["total"], "单号过滤必须真的收窄命中数")

	bySupplier := decode(t, do(r, "/purchase-sheets?supplier_id=084", "10"))
	assert.Equal(t, float64(1), bySupplier["total"], "供应商过滤必须真的收窄命中数")

	byStatus := decode(t, do(r, "/purchase-sheets?approve_status=approved", "10"))
	assert.Equal(t, float64(1), byStatus["total"], "审核状态过滤必须真的收窄命中数")

	none := decode(t, do(r, "/purchase-sheets?supplier_id=NOPE", "10"))
	assert.Equal(t, float64(0), none["total"], "不存在的条件 ⇒ 命中 0 行")
}

// TestGetSheet_LocalReturnsDetail 锁"详情 = 头 + 明细"。
func TestGetSheet_LocalReturnsDetail(t *testing.T) {
	r, db := newRouter(t, &fakeSource{src: localSource()})
	seedLocal(t, db)

	w := do(r, "/purchase-sheets/PI-1", "10")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	m := decode(t, w)
	assert.Equal(t, "PI-1", m["sheet_no"])
	lines, ok := m["lines"].([]any)
	require.True(t, ok)
	require.Len(t, lines, 1)
	line := lines[0].(map[string]any)
	assert.Equal(t, float64(1), line["line_no"])
	assert.Equal(t, "000552", line["product_id"])
	assert.Equal(t, "19.60", line["received_qty"])
	assert.Equal(t, "122.00", line["amount_yuan"])
}

// TestGetSheet_NotFoundIs404 锁"查不到 ⇒ 404"。
func TestGetSheet_NotFoundIs404(t *testing.T) {
	r, _ := newRouter(t, &fakeSource{src: localSource()})
	w := do(r, "/purchase-sheets/NOPE", "10")
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	assert.Equal(t, "purchase_sheet_not_found", decode(t, w)["code"])
}

// TestListSheets_ResolveFailureIs503 锁"问不到数据源 ⇒ 503(**不是**空列表)"。
func TestListSheets_ResolveFailureIs503(t *testing.T) {
	r, db := newRouter(t, &fakeSource{err: errors.New("cube-router 不可达")})
	seedLocal(t, db)

	w := do(r, "/purchase-sheets", "10")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Equal(t, "cube_unavailable", decode(t, w)["code"])
}

// TestListSheets_SourceDisabledIs503 锁"数据源被停用 ⇒ 503"。
func TestListSheets_SourceDisabledIs503(t *testing.T) {
	disabled := configuredSource()
	disabled.Enabled = false
	r, db := newRouter(t, &fakeSource{src: disabled})
	seedLocal(t, db)

	w := do(r, "/purchase-sheets", "10")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Equal(t, "cube_source_disabled", decode(t, w)["code"])
}
