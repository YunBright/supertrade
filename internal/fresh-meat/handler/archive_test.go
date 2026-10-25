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
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/fresh-meat/handler"
	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/YunBright/supertrade/internal/fresh-meat/testdb"
	"github.com/YunBright/supertrade/pkg/middleware"
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
	db, err := testdb.OpenSQLite(":memory:?_pragma=foreign_keys(1)")
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.WholePig{}, &model.PigCut{}, &model.PorkCutsStocktake{},
		&model.LineSalesByPig{}, &model.BranchCutMapping{}, &model.WasteLog{},
		&model.SkuStockPosition{}, &model.SkuCostState{}, &model.SkuCostDaily{},
		&model.PromoSku{},
		&model.BulkProduct{}, &model.BulkProduction{}, &model.BulkSettlement{},
		&model.BulkSettlementAllocation{}, &model.PigForecastBaseline{},
	))
	return db
}

// seedArchiveData 造一头猪 + 一个部位 + 一条报损。
func seedArchiveData(t *testing.T, db *gorm.DB) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, db.Create(&model.WholePig{
		ID: "PIG-1", EarTag: "ET-1", GrossWeightKg: decimal.NewFromInt(100),
		PurchaseUnitPriceYuan: decimal.NewFromInt(15), PurchaseCostYuan: decimal.NewFromInt(1500),
		SupplierID: "SUP-1", BranchID: archiveBranch, ArrivedAt: now, RecordedBy: "u-1",
		DataSource:    model.DataSourceStub,
		LLMAdviceJSON: datatypes.JSON([]byte(`{"raw":"LLM-PROMPT-SECRET"}`)),
	}).Error)
	require.NoError(t, db.Create(&model.PigCut{
		ID: "CUT-1", PigID: "PIG-1", CutType: model.CutType("五花"), BranchID: archiveBranch,
		Barcode: "6900000000000", WeightKg: decimal.NewFromInt(20), ExpiresAt: now.Add(48 * time.Hour),
	}).Error)
	require.NoError(t, db.Create(&model.WasteLog{
		ID: "W-1", BranchID: archiveBranch, PigID: "PIG-1", CutType: model.CutType("五花"),
		QtyKg: decimal.NewFromInt(2), Reason: "切坏", RecordedAt: now, OperatorID: "u-1",
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
		return scope == service.ScopeFreshmeatView || scope == service.ScopeFreshmeatWrite, nil
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
	seedArchiveData(t, db)
	r := newArchiveRouter(t, db, archiveLocalSource())

	w := doArchive(t, r, http.MethodGet, "/archive/export?scope=whole_pigs", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var got service.ExportResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, service.ScopeWholePigs, got.Scope)
	assert.False(t, got.External)
	assert.Equal(t, 1, got.Count)
	require.Len(t, got.WholePigs, 1)
	assert.Equal(t, "PIG-1", got.WholePigs[0].ID)
	assert.Equal(t, "100", got.WholePigs[0].GrossWeightKg)
}

// TestArchiveExport_SourceConfigured_ReturnsEmpty 锁"配了源 ⇒ 导出**空**"。
func TestArchiveExport_SourceConfigured_ReturnsEmpty(t *testing.T) {
	db := newArchiveDB(t)
	seedArchiveData(t, db)
	r := newArchiveRouter(t, db, archiveExternalSource())

	for _, scope := range []string{
		service.ScopeWholePigs, service.ScopePigCuts, service.ScopePorkCutsStocktakes,
		service.ScopeLineSalesByPigs, service.ScopeBranchCutMappings, service.ScopeWasteLogs,
	} {
		w := doArchive(t, r, http.MethodGet, "/archive/export?scope="+scope, "")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var got service.ExportResult
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		assert.True(t, got.External, scope)
		assert.Equal(t, 0, got.Count, scope)
		assert.Empty(t, got.WholePigs, scope)
		assert.Empty(t, got.PigCuts, scope)
		assert.Empty(t, got.PorkCutsStocktakes, scope)
		assert.Empty(t, got.LineSalesByPigs, scope)
		assert.Empty(t, got.BranchCutMappings, scope)
		assert.Empty(t, got.WasteLogs, scope)
	}
}

// TestArchiveExport_ResolveFailure_503 锁"问不到路由 ⇒ 503"。
func TestArchiveExport_ResolveFailure_503(t *testing.T) {
	db := newArchiveDB(t)
	seedArchiveData(t, db)
	r := newArchiveRouter(t, db, archiveSourceResolver{err: archiveResolveError{}})

	w := doArchive(t, r, http.MethodGet, "/archive/export?scope=whole_pigs", "")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "cube_unavailable")
}

// TestArchiveImport_SourceConfigured_409 锁"配了源 ⇒ 导入 409 只读"。
func TestArchiveImport_SourceConfigured_409(t *testing.T) {
	db := newArchiveDB(t)
	seedArchiveData(t, db)
	r := newArchiveRouter(t, db, archiveExternalSource())

	body := `{"scope":"whole_pigs","whole_pigs":[{"id":"PIG-NEW","ear_tag":"ET-NEW"}]}`
	w := doArchive(t, r, http.MethodPost, "/archive/import", body)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "member_source_is_external")
	assert.NotContains(t, w.Body.String(), "cube-sixun-ysx-fb")

	var n int64
	require.NoError(t, db.Model(&model.WholePig{}).Count(&n).Error)
	assert.Equal(t, int64(1), n, "409 之后本地数据不许被改动")
}

// TestArchiveImport_Local_WritesAndIsIdempotent 锁"没配源 ⇒ 写本地 + 幂等"。
func TestArchiveImport_Local_WritesAndIsIdempotent(t *testing.T) {
	db := newArchiveDB(t)
	r := newArchiveRouter(t, db, archiveLocalSource())

	body := `{"scope":"whole_pigs","whole_pigs":[` +
		`{"id":"PIG-NEW","ear_tag":"ET-NEW","gross_weight_kg":"80.500",` +
		`"purchase_unit_price_yuan":"16.00","purchase_cost_yuan":"1280.00",` +
		`"supplier_id":"SUP-1","arrived_at":"2026-01-01T00:00:00Z","recorded_by":"u-1",` +
		`"data_source":"stub"}]}`
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
	require.NoError(t, db.Model(&model.WholePig{}).Count(&n).Error)
	assert.Equal(t, int64(1), n)
}

// TestArchiveExport_ExcludesLLMAdvice 锁"导出行**不含** LLM 产物"。
//
// ⚠️ llm_advice_json / llm_review_json 是派生的调试产物
//
//	(含原始 prompt 片段、体积不受控)⇒ 不进归档。
func TestArchiveExport_ExcludesLLMAdvice(t *testing.T) {
	db := newArchiveDB(t)
	seedArchiveData(t, db)
	r := newArchiveRouter(t, db, archiveLocalSource())

	w := doArchive(t, r, http.MethodGet, "/archive/export?scope=whole_pigs", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "LLM-PROMPT-SECRET")
	assert.NotContains(t, w.Body.String(), "llm_advice_json")
}

// TestArchiveImport_DoesNotWriteLLMAdvice 锁"导入也不写 LLM 产物"。
func TestArchiveImport_DoesNotWriteLLMAdvice(t *testing.T) {
	db := newArchiveDB(t)
	r := newArchiveRouter(t, db, archiveLocalSource())

	body := `{"scope":"whole_pigs","whole_pigs":[` +
		`{"id":"PIG-NEW","ear_tag":"ET-NEW","arrived_at":"2026-01-01T00:00:00Z"}]}`
	w := doArchive(t, r, http.MethodPost, "/archive/import", body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var got model.WholePig
	require.NoError(t, db.First(&got, "id = ?", "PIG-NEW").Error)
	assert.Empty(t, got.LLMAdviceJSON, "导入不写 LLM 产物")
}
