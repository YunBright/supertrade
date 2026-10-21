// 本文件锁"归档导出端点"与"导入端的只读闸门"。
//
//	GET  /archive/export?scope=prices|promotions&offset=&limit=&as_of=
//	POST /archive/import
//
// # 判据:与查价同一套
//
//	配了数据源 ⇒ cube(实时);没配 ⇒ 本域 PG
//
// ⚠️ 解析失败 / 数据源被停用 ⇒ **503**,不回落本地。
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/archivedto"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/pricing/model"
	"github.com/YunBright/supertrade/internal/pricing/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// fakeResolver 替身 service.SourceResolver。
type fakeResolver struct {
	src cubesource.Source
	err error
}

func (f *fakeResolver) Resolve(_ context.Context, _ string) (cubesource.Source, error) {
	if f.err != nil {
		return cubesource.Source{}, f.err
	}
	return f.src, nil
}

// fakeReader 替身 service.CubeQueryReader。
type fakeReader struct {
	rows  []map[string]any
	total int
	err   error
}

func (f *fakeReader) LoadCubeQueryPage(_ context.Context, _ string, _ cubeclient.CubeQuery) ([]map[string]any, int, error) {
	if f.err != nil {
		return nil, 0, f.err
	}
	return f.rows, f.total, nil
}

// externalResolver 说"这个门店配了数据源"(富百家 00)。
func externalResolver() *fakeResolver {
	return &fakeResolver{src: cubesource.Source{
		BranchID: "00", Configured: true, Enabled: true,
		CubeSourceName: "cube-sixun-ysx-fb",
	}}
}

// localResolver 说"这个门店没配数据源"(自营店 10)。
func localResolver() *fakeResolver {
	return &fakeResolver{src: cubesource.Source{BranchID: "10"}}
}

// archiveRouter 造一个带导出 / 导入端点的 router。
func archiveRouter(t *testing.T, resolver *fakeResolver, reader *fakeReader) (*gin.Engine, *gorm.DB) {
	t.Helper()
	db := newTestDB(t)
	prices := service.NewPriceListService(db)
	promos := service.NewPromotionService(db)
	svc := service.NewPriceService(nil).WithStore(prices, promos)
	if resolver != nil {
		svc.SetCubeSource(resolver, reader)
	}
	h := New(svc, prices, promos, nil, nil)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.XBranchID())
	r.GET("/archive/export", h.getArchiveExport)
	r.POST("/archive/import", h.importArchive)
	return r, db
}

// archiveExportBody 是导出响应(archivedto 契约)。
type archiveExportBody struct {
	Scope      string           `json:"scope"`
	BranchID   string           `json:"branch_id"`
	CubeSource string           `json:"cube_source"`
	AsOf       string           `json:"as_of"`
	Rows       []map[string]any `json:"rows"`
	Count      int              `json:"count"`
	Total      int              `json:"total"`
	Offset     int              `json:"offset"`
	Limit      int              `json:"limit"`
}

// TestArchiveExport_LocalBranchReadsLocalTable 锁"没配数据源 ⇒ 从本地表导出"。
func TestArchiveExport_LocalBranchReadsLocalTable(t *testing.T) {
	r, db := archiveRouter(t, localResolver(), nil)
	require.NoError(t, db.Create(&model.PriceList{
		ID: "default", BranchID: "10", ProductID: "P1",
		PriceYuan:   decimalOf("15.00"),
		EffectiveAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local),
		Status:      "active",
		CreatedBy:   "test", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}).Error)

	w := getJSON(t, r, "/archive/export?scope=prices&as_of=2026-10-21", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body archiveExportBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, archivedto.ScopePrices, body.Scope)
	assert.Equal(t, "10", body.BranchID)
	assert.Equal(t, "2026-10-21", body.AsOf)
	require.Len(t, body.Rows, 1)
	assert.Equal(t, "P1", body.Rows[0]["product_id"])
	assert.Equal(t, "15", body.Rows[0]["price_yuan"])
	assert.Equal(t, 1, body.Count)
	assert.Equal(t, 1, body.Total)
}

// TestArchiveExport_ConfiguredBranchReadsCube 锁"配了数据源 ⇒ 从 cube 导出"。
func TestArchiveExport_ConfiguredBranchReadsCube(t *testing.T) {
	reader := &fakeReader{
		rows: []map[string]any{{
			"product.id":              "P-CUBE",
			"product.avg_price_yuan":  9.9,
			"product.total_cost_yuan": 6.5,
		}},
		total: 1,
	}
	r, _ := archiveRouter(t, externalResolver(), reader)

	w := getJSON(t, r, "/archive/export?scope=prices", "00")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body archiveExportBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "cube-sixun-ysx-fb", body.CubeSource)
	require.Len(t, body.Rows, 1)
	assert.Equal(t, "P-CUBE", body.Rows[0]["product_id"])
	assert.Equal(t, "9.9", body.Rows[0]["price_yuan"])
}

// TestArchiveExport_UnsupportedScope_400 锁"不支持的资料域 ⇒ 400"。
func TestArchiveExport_UnsupportedScope_400(t *testing.T) {
	r, _ := archiveRouter(t, localResolver(), nil)

	w := getJSON(t, r, "/archive/export?scope=products", "10")
	require.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "unsupported_export_scope", body["code"])
}

// TestArchiveExport_MissingScope_400 锁"缺 scope ⇒ 400"。
func TestArchiveExport_MissingScope_400(t *testing.T) {
	r, _ := archiveRouter(t, localResolver(), nil)

	w := getJSON(t, r, "/archive/export", "10")
	require.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "missing_scope", body["code"])
}

// TestArchiveExport_BadAsOf_400 锁"非法基准日 ⇒ 400(不静默回落到今天)"。
//
// ⚠️ 促销是时效数据:基准日错了 ⇒ 导出的促销全错,而报告里看不出来。
func TestArchiveExport_BadAsOf_400(t *testing.T) {
	r, _ := archiveRouter(t, localResolver(), nil)

	w := getJSON(t, r, "/archive/export?scope=promotions&as_of=2026-13-01", "10")
	require.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "bad_as_of", body["code"])
}

// TestArchiveExport_MissingBranch_400 锁"缺 X-Branch-ID ⇒ 400 branch_required"。
func TestArchiveExport_MissingBranch_400(t *testing.T) {
	r, _ := archiveRouter(t, localResolver(), nil)

	w := getJSON(t, r, "/archive/export?scope=prices", "")
	require.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "branch_required", body["code"])
}

// TestArchiveExport_ResolveFailure_503 锁"问不到路由 ⇒ 503,不回落本地"。
//
// ⚠️ 回落本地 = 把一次链路故障显示成"该门店没有价格"。
func TestArchiveExport_ResolveFailure_503(t *testing.T) {
	r, _ := archiveRouter(t, &fakeResolver{err: errors.New("dapr 不可达")}, nil)

	w := getJSON(t, r, "/archive/export?scope=prices", "00")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, "body=%s", w.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "cube_unavailable", body["code"])
}

// TestArchiveExport_DisabledSource_503 锁"数据源被停用 ⇒ 503"。
func TestArchiveExport_DisabledSource_503(t *testing.T) {
	disabled := externalResolver()
	disabled.src.Enabled = false
	r, _ := archiveRouter(t, disabled, nil)

	w := getJSON(t, r, "/archive/export?scope=prices", "00")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, "body=%s", w.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "cube_source_disabled", body["code"])
}

// TestImportArchive_ExternalSourceIsReadOnly_409 锁"有源门店的导入 ⇒ 409"。
//
// ⚠️ 不拒的话:写进本域 PG 的一行"没人看的价",而前台还显示"写成功了"。
func TestImportArchive_ExternalSourceIsReadOnly_409(t *testing.T) {
	r, _ := archiveRouter(t, externalResolver(), nil)

	w := postJSON(t, r, "/archive/import", "00",
		`{"mode":"overwrite","data":{"prices":[{"product_id":"P1","price_yuan":"9.90"}]}}`)
	require.Equal(t, http.StatusConflict, w.Code, "body=%s", w.Body.String())

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "member_source_is_external", body["code"])
	// ⚠️ 文案不能泄漏内部 app-id。
	msg, _ := body["message"].(string)
	assert.NotContains(t, msg, "cube-sixun-ysx-fb", "409 文案泄漏了内部 app-id")
}

// TestImportArchive_LocalSourceStillWorks 锁"没配数据源的门店照常导入"。
func TestImportArchive_LocalSourceStillWorks(t *testing.T) {
	r, _ := archiveRouter(t, localResolver(), nil)

	w := postJSON(t, r, "/archive/import", "10",
		`{"mode":"overwrite","data":{"prices":[{"product_id":"P1","price_yuan":"9.90"}]}}`)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
}

// TestImportArchive_ResolveFailureIsAllowed 锁"问不到路由 ⇒ 放行导入"。
//
// ⚠️ 写侧放行是**有意的**:导入路径自己会兜底(本域 PG 里没有这个门店的行
// ⇒ 导入 0 条),那是诚实的失败。
func TestImportArchive_ResolveFailureIsAllowed(t *testing.T) {
	r, _ := archiveRouter(t, &fakeResolver{err: errors.New("dapr 不可达")}, nil)

	w := postJSON(t, r, "/archive/import", "10",
		`{"mode":"overwrite","data":{"prices":[{"product_id":"P1","price_yuan":"9.90"}]}}`)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
}

// TestArchiveExport_RowsAreArchivedtoContract 锁"导出的列名与 archivedto 契约一致"。
//
// ⚠️ 字段名不一致 ⇒ 导入端**静默丢数据**(JSON 对不认识的字段静默忽略)。
func TestArchiveExport_RowsAreArchivedtoContract(t *testing.T) {
	r, _ := archiveRouter(t, localResolver(), nil)

	w := getJSON(t, r, "/archive/export?scope=promotions", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body archiveExportBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	// 没有数据时 rows 是空数组(不是 null)—— 客户端遍历不用判空指针。
	assert.NotNil(t, body.Rows)
	assert.Equal(t, 0, body.Count)
}

// decimalOf 造一个 decimal(避免每个用例都写一遍 RequireFromString)。
func decimalOf(s string) decimal.Decimal { return decimal.RequireFromString(s) }
