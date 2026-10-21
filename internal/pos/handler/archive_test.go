package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/YunBright/authkit/claims"
	"github.com/YunBright/authkit/rbac"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
	_ "modernc.org/sqlite"

	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/pos/handler"
	"github.com/YunBright/supertrade/internal/pos/model"
	"github.com/YunBright/supertrade/internal/pos/service"
	"github.com/YunBright/supertrade/pkg/middleware"
	"github.com/YunBright/supertrade/pkg/outbox"
)

// ---- fakes ----

// archiveSourceResolver 是 cubesource.Resolver 的 fake。
type archiveSourceResolver struct {
	src cubesource.Source
	err error
}

func (f archiveSourceResolver) Resolve(_ context.Context, _ string) (cubesource.Source, error) {
	if f.err != nil {
		return cubesource.Source{}, f.err
	}
	return f.src, nil
}

// archiveLocalSource 是"自营店"(没配数据源)。
func archiveLocalSource() archiveSourceResolver {
	return archiveSourceResolver{src: cubesource.Source{BranchID: "10"}}
}

// archiveExternalSource 是"配了外部数据源且启用"。
func archiveExternalSource() archiveSourceResolver {
	return archiveSourceResolver{src: cubesource.Source{
		BranchID: "10", Configured: true, Enabled: true, CubeSourceName: "cube-sixun-ysx-fb",
	}}
}

// archiveResolveError 是"问不到路由"。
type archiveResolveError struct{}

func (archiveResolveError) Error() string { return "cube-router 问不到" }

// ---- 基建 ----

const archiveBranch = "10"

func newArchiveDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Dialector{DriverName: "sqlite", DSN: ":memory:"},
		&gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.SalesOrder{}, &model.SaleLine{}, &model.Payment{}, &outbox.Outbox{},
	))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// seedArchiveSale 造一张销售单 + 一条明细 + 一条收付。
func seedArchiveSale(t *testing.T, db *gorm.DB) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, db.Create(&model.SalesOrder{
		ID: "SO20260101000001", BranchID: archiveBranch, OperatorID: "u-1",
		TotalAmountYuan: decimal.NewFromInt(22), PaidAmountYuan: decimal.NewFromInt(22),
		Status: "paid", SoldAt: now, OpID: "op-1", CreatedBy: "u-1",
	}).Error)
	require.NoError(t, db.Create(&model.SaleLine{
		ID: "SO20260101000001-L1", SaleID: "SO20260101000001", BranchID: archiveBranch,
		ProductID: "P-1", ProductName: "农夫山泉", Unit: "瓶",
		Qty: decimal.NewFromInt(2), UnitPriceYuan: decimal.NewFromInt(11),
		AmountYuan: decimal.NewFromInt(22), CreatedBy: "u-1",
	}).Error)
	require.NoError(t, db.Create(&model.Payment{
		ID: "SO20260101000001-P1", SaleID: "SO20260101000001", BranchID: archiveBranch,
		PayWay: "RMB", AmountYuan: decimal.NewFromInt(22), ExchangeRate: decimal.NewFromInt(1),
		CreatedBy: "u-1",
	}).Error)
}

func newArchiveRouter(t *testing.T, db *gorm.DB, resolver cubesource.Resolver) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.XBranchID())
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(
			claims.WithClaims(c.Request.Context(), &claims.Claims{Sub: "u-1"}))
		c.Next()
	})
	archive := service.NewArchiveService(db)
	archive.SetSourceResolver(resolver)
	allow := rbac.ScopeResolver(func(_ *gin.Context, _, _, scope string) (bool, error) {
		return scope == service.ScopeSale, nil
	})
	handler.NewArchiveHandler(archive, allow).RegisterRoutes(r)
	return r
}

func doArchive(t *testing.T, r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Branch-ID", archiveBranch)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// ---- 用例 ----

// TestArchiveExport_NoSource_ReturnsLocalRows 锁"没配数据源 ⇒ 导本地"。
func TestArchiveExport_NoSource_ReturnsLocalRows(t *testing.T) {
	db := newArchiveDB(t)
	seedArchiveSale(t, db)
	r := newArchiveRouter(t, db, archiveLocalSource())

	w := doArchive(t, r, http.MethodGet, "/archive/export?scope=sales_orders", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var got service.ExportResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, service.ScopeSalesOrders, got.Scope)
	assert.False(t, got.External)
	assert.Equal(t, 1, got.Count)
	require.Len(t, got.SalesOrders, 1)
	assert.Equal(t, "SO20260101000001", got.SalesOrders[0].ID)
	assert.Equal(t, "22", got.SalesOrders[0].TotalAmountYuan)
}

// TestArchiveExport_SourceConfigured_ReturnsEmpty 锁"配了源 ⇒ 导出**空**"。
func TestArchiveExport_SourceConfigured_ReturnsEmpty(t *testing.T) {
	db := newArchiveDB(t)
	seedArchiveSale(t, db)
	r := newArchiveRouter(t, db, archiveExternalSource())

	for _, scope := range []string{
		service.ScopeSalesOrders, service.ScopeSaleLines, service.ScopePayments,
	} {
		w := doArchive(t, r, http.MethodGet, "/archive/export?scope="+scope, "")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var got service.ExportResult
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		assert.True(t, got.External, scope)
		assert.Equal(t, 0, got.Count, scope)
		assert.Empty(t, got.SalesOrders, scope)
		assert.Empty(t, got.SaleLines, scope)
		assert.Empty(t, got.Payments, scope)
	}
}

// TestArchiveExport_ResolveFailure_503 锁"问不到路由 ⇒ 503"。
func TestArchiveExport_ResolveFailure_503(t *testing.T) {
	db := newArchiveDB(t)
	seedArchiveSale(t, db)
	r := newArchiveRouter(t, db, archiveSourceResolver{err: archiveResolveError{}})

	w := doArchive(t, r, http.MethodGet, "/archive/export?scope=sales_orders", "")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "cube_unavailable")
}

// TestArchiveImport_SourceConfigured_409 锁"配了源 ⇒ 导入 409 只读"。
func TestArchiveImport_SourceConfigured_409(t *testing.T) {
	db := newArchiveDB(t)
	seedArchiveSale(t, db)
	r := newArchiveRouter(t, db, archiveExternalSource())

	body := `{"scope":"sales_orders","sales_orders":[{"id":"SO-NEW"}]}`
	w := doArchive(t, r, http.MethodPost, "/archive/import", body)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "member_source_is_external")
	assert.NotContains(t, w.Body.String(), "cube-sixun-ysx-fb")

	var n int64
	require.NoError(t, db.Model(&model.SalesOrder{}).Count(&n).Error)
	assert.Equal(t, int64(1), n, "409 之后本地数据不许被改动")
}

// TestArchiveImport_Local_WritesAndIsIdempotent 锁"没配源 ⇒ 写本地 + 幂等"。
func TestArchiveImport_Local_WritesAndIsIdempotent(t *testing.T) {
	db := newArchiveDB(t)
	r := newArchiveRouter(t, db, archiveLocalSource())

	body := `{"scope":"sales_orders","sales_orders":[` +
		`{"id":"SO-NEW","operator_id":"u-1","total_amount_yuan":"22.00",` +
		`"paid_amount_yuan":"22.00","status":"paid","sold_at":"2026-01-01T00:00:00Z",` +
		`"op_id":"op-new","created_by":"u-1"}]}`
	w := doArchive(t, r, http.MethodPost, "/archive/import", body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var rep service.ImportResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rep))
	assert.Equal(t, 1, rep.Received)
	assert.Equal(t, 1, rep.Inserted)
	assert.Equal(t, 0, rep.Updated)

	// 幂等:再导一次 ⇒ updated=1,行数不变。
	w2 := doArchive(t, r, http.MethodPost, "/archive/import", body)
	require.Equal(t, http.StatusOK, w2.Code, w2.Body.String())
	var rep2 service.ImportResult
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &rep2))
	assert.Equal(t, 0, rep2.Inserted)
	assert.Equal(t, 1, rep2.Updated)

	var n int64
	require.NoError(t, db.Model(&model.SalesOrder{}).Count(&n).Error)
	assert.Equal(t, int64(1), n)
}

// TestArchiveImport_DoesNotPublishEvents 锁"导入**不发** sale.paid 事件"。
//
// ⚠️ 这是本域最危险的一条:pos 的一笔销售会写一条 sale.paid,
//
//	下游据此**扣库存**与**加积分**。
//	一次"恢复数据"若发了事件 ⇒ 库存与积分被**再动一次**,
//	而前台看不出来(单据显示正常,只是库存/积分悄悄错一截)。
//
// ⇒ 导入只写业务表,outbox 一行都不写。
func TestArchiveImport_DoesNotPublishEvents(t *testing.T) {
	db := newArchiveDB(t)
	r := newArchiveRouter(t, db, archiveLocalSource())

	body := `{"scope":"sales_orders","sales_orders":[` +
		`{"id":"SO-NEW","operator_id":"u-1","total_amount_yuan":"22.00",` +
		`"status":"paid","sold_at":"2026-01-01T00:00:00Z","op_id":"op-new"}]}`
	w := doArchive(t, r, http.MethodPost, "/archive/import", body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var n int64
	require.NoError(t, db.Model(&model.SalesOrder{}).Count(&n).Error)
	assert.Equal(t, int64(1), n, "单据写进去了")

	// ⚠️ 发件箱必须**空**:一行事件都不许有。
	var events int64
	require.NoError(t, db.Model(&outbox.Outbox{}).Count(&events).Error)
	assert.Equal(t, int64(0), events, "导入只写数据,不许发 sale.paid 事件")
}
