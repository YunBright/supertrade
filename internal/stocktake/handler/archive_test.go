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
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/stocktake/handler"
	"github.com/YunBright/supertrade/internal/stocktake/model"
	"github.com/YunBright/supertrade/internal/stocktake/service"
	"github.com/YunBright/supertrade/internal/stocktake/testdb"
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
		&model.StocktakeHeader{}, &model.StocktakeLine{},
		&model.StocktakeLineOperation{}, &model.StocktakePlanItem{},
	))
	return db
}

// seedArchiveData 造一张盘点单 + 一条明细 + 一条操作历史 + 一条计划商品。
func seedArchiveData(t *testing.T, db *gorm.DB) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, db.Create(&model.StocktakeHeader{
		ID: "ST20260101001", BranchID: archiveBranch, CountDate: now,
		Status: model.StatusCounting, Type: model.TypeGeneral, OperatorID: "u-1",
		TotalDiffQty: decimal.NewFromInt(2), TotalDiffAmountYuan: decimal.NewFromInt(20),
	}).Error)
	require.NoError(t, db.Create(&model.StocktakeLine{
		ID: "ST20260101001-L1", HeaderID: "ST20260101001", ProductID: "P-1",
		ProductName: "农夫山泉", Unit: "瓶", BookQty: decimal.NewFromInt(10),
		BookQtyAt: now, ActualQty: decimal.NewFromInt(8), DiffQty: decimal.NewFromInt(-2),
		AvgCostYuan: decimal.NewFromInt(2), DiffAmountYuan: decimal.NewFromInt(-4),
		DiffReason: model.ReasonLoss,
	}).Error)
	require.NoError(t, db.Create(&model.StocktakeLineOperation{
		ID: "OP-1", HeaderID: "ST20260101001", LineID: "ST20260101001-L1",
		OpType: model.OpCreate, ActorID: "u-1", ActorName: "张三",
		PrevQty: decimal.Zero, NewQty: decimal.NewFromInt(8), QtyDelta: decimal.NewFromInt(8),
		OpAt: now, Method: model.MethodScan,
	}).Error)
	require.NoError(t, db.Create(&model.StocktakePlanItem{
		ID: "PI-1", HeaderID: "ST20260101001", ProductID: "P-1",
		ProductName: "农夫山泉", Unit: "瓶", Barcode: "6900000000000", SortOrder: 1,
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
		return scope == service.ScopeInventoryView || scope == service.ScopeInventoryManage, nil
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

	w := doArchive(t, r, http.MethodGet, "/archive/export?scope=headers", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var got service.ExportResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, service.ScopeHeaders, got.Scope)
	assert.False(t, got.External)
	assert.Equal(t, 1, got.Count)
	require.Len(t, got.Headers, 1)
	assert.Equal(t, "ST20260101001", got.Headers[0].ID)

	// 从表(明细 / 操作历史 / 计划商品)也必须能导出来。
	w = doArchive(t, r, http.MethodGet, "/archive/export?scope=lines", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, 1, got.Count)
	require.Len(t, got.Lines, 1)
	assert.Equal(t, "农夫山泉", got.Lines[0].ProductName)
}

// TestArchiveExport_SourceConfigured_ReturnsEmpty 锁"配了源 ⇒ 导出**空**"。
func TestArchiveExport_SourceConfigured_ReturnsEmpty(t *testing.T) {
	db := newArchiveDB(t)
	seedArchiveData(t, db)
	r := newArchiveRouter(t, db, archiveExternalSource())

	for _, scope := range []string{
		service.ScopeHeaders, service.ScopeLines,
		service.ScopeLineOperations, service.ScopePlanItems,
	} {
		w := doArchive(t, r, http.MethodGet, "/archive/export?scope="+scope, "")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var got service.ExportResult
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		assert.True(t, got.External, scope)
		assert.Equal(t, 0, got.Count, scope)
		assert.Empty(t, got.Headers, scope)
		assert.Empty(t, got.Lines, scope)
		assert.Empty(t, got.LineOperations, scope)
		assert.Empty(t, got.PlanItems, scope)
	}
}

// TestArchiveExport_ResolveFailure_503 锁"问不到路由 ⇒ 503"。
func TestArchiveExport_ResolveFailure_503(t *testing.T) {
	db := newArchiveDB(t)
	seedArchiveData(t, db)
	r := newArchiveRouter(t, db, archiveSourceResolver{err: archiveResolveError{}})

	w := doArchive(t, r, http.MethodGet, "/archive/export?scope=headers", "")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "cube_unavailable")
}

// TestArchiveImport_SourceConfigured_409 锁"配了源 ⇒ 导入 409 只读"。
func TestArchiveImport_SourceConfigured_409(t *testing.T) {
	db := newArchiveDB(t)
	seedArchiveData(t, db)
	r := newArchiveRouter(t, db, archiveExternalSource())

	body := `{"scope":"headers","headers":[{"id":"ST-NEW","count_date":"2026-01-01"}]}`
	w := doArchive(t, r, http.MethodPost, "/archive/import", body)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "member_source_is_external")
	assert.NotContains(t, w.Body.String(), "cube-sixun-ysx-fb")

	var n int64
	require.NoError(t, db.Model(&model.StocktakeHeader{}).Count(&n).Error)
	assert.Equal(t, int64(1), n, "409 之后本地数据不许被改动")
}

// TestArchiveImport_Local_WritesAndIsIdempotent 锁"没配源 ⇒ 写本地 + 幂等"。
func TestArchiveImport_Local_WritesAndIsIdempotent(t *testing.T) {
	db := newArchiveDB(t)
	r := newArchiveRouter(t, db, archiveLocalSource())

	body := `{"scope":"headers","headers":[` +
		`{"id":"ST-NEW","count_date":"2026-01-01","status":"counting","type":"general",` +
		`"operator_id":"u-1","total_diff_qty":"1.5","total_diff_amount_yuan":"3.25"}]}`
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
	require.NoError(t, db.Model(&model.StocktakeHeader{}).Count(&n).Error)
	assert.Equal(t, int64(1), n)
}

// TestArchiveImport_DoesNotWriteLineOperations 锁"导入明细**不写操作历史**"。
//
// ⚠️ 这是「只写数据不触发领域副作用」的一条具体断言:
//
//	AddLine 会写 stocktake_line_operations 并发 dapr 事件;
//	导入不许做这件事(那是"业务重放",不是"数据恢复")。
func TestArchiveImport_DoesNotWriteLineOperations(t *testing.T) {
	db := newArchiveDB(t)
	seedArchiveData(t, db)
	r := newArchiveRouter(t, db, archiveLocalSource())

	body := `{"scope":"lines","lines":[` +
		`{"id":"ST20260101001-L2","header_id":"ST20260101001","product_id":"P-2",` +
		`"product_name":"怡宝","book_qty":"5","book_qty_at":"2026-01-01T00:00:00Z",` +
		`"actual_qty":"4","diff_qty":"-1"}]}`
	w := doArchive(t, r, http.MethodPost, "/archive/import", body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// 明细写进去了。
	var lines int64
	require.NoError(t, db.Model(&model.StocktakeLine{}).Count(&lines).Error)
	assert.Equal(t, int64(2), lines)

	// ⚠️ 操作历史**没有**多出来(导入不写操作历史)。
	var ops int64
	require.NoError(t, db.Model(&model.StocktakeLineOperation{}).Count(&ops).Error)
	assert.Equal(t, int64(1), ops, "导入只写数据,不许产生领域副作用(操作历史)")
}
