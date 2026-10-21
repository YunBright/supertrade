// 本文件锁住 inventory 的**归档端点**（导出 / 导入）与「按数据源分流」的 HTTP 行为。
//
// # 守门链说明
//
// 生产上导出挂 `inventory:view`、导入挂 `inventory:manage`（per-branch 实时解析），
// 需要 userd 才能跑起来，单测里拉不起来。
// 这里只挂 `middleware.XBranchID()`（注入门店上下文），业务行为
// （200 / 400 / 409 / 503）与权限层是**两件事** ——
// 权限层由 deployer 的契约测试 + auth 侧保证。
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	_ "modernc.org/sqlite" // 注册驱动名 "sqlite"(纯 Go 驱动,无需 CGO)

	"github.com/YunBright/supertrade/internal/archivedto"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/inventory/model"
	"github.com/YunBright/supertrade/internal/inventory/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// ---- 测试替身 ----

// fakeResolver 替身 cubesource.Resolver。
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

// fakeReader 替身 service.CubeStockReader。
type fakeReader struct {
	rows  []map[string]any
	total int
}

func (f *fakeReader) LoadCubeQueryPage(_ context.Context, _ string, _ cubeclient.CubeQuery) ([]map[string]any, int, error) {
	return f.rows, f.total, nil
}

func (f *fakeReader) GetStock(_ context.Context, branchID, productID string) (*cubeclient.StockSnapshotDTO, error) {
	return nil, errors.New("not used")
}

// fakeExporter 替身 service.ArchiveExporter(转发 cube-router 的 /v1/export)。
type fakeExporter struct {
	body []byte
	in   service.ExportInput
	// calls 记录转发次数。
	calls int
}

func (f *fakeExporter) ExportRaw(_ context.Context, in service.ExportInput) ([]byte, error) {
	f.calls++
	f.in = in
	return f.body, nil
}

// ---- 脚手架 ----

// newArchiveRouter 起一个带归档端点的路由(不带 rbac 层)。
//
// resolver / reader / exporter 传 nil ⇒ 不装配 ⇒ 一律走本域 PG。
func newArchiveRouter(t *testing.T, svc *service.StockService) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.XBranchID())

	h := New(svc, nil, nil)
	r.GET("/stock-levels", h.listStockLevels)
	r.GET("/archive/export", h.exportArchive)
	r.POST("/archive/import", h.importArchive)
	return r
}

// newArchiveSvc 造一个带 sqlite 的 StockService。
func newArchiveSvc(t *testing.T) *service.StockService {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "inventory.db")
	dialector := sqlite.Dialector{DriverName: "sqlite", DSN: dsn}
	db, err := gorm.Open(dialector, &gorm.Config{})
	require.NoError(t, err, "open sqlite")
	require.NoError(t, db.AutoMigrate(&model.StockLevel{}, &model.StockMove{}), "auto migrate")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	// ⚠️ 必须关连接:Windows 上文件被占用时 t.TempDir() 的清理会失败。
	t.Cleanup(func() { _ = sqlDB.Close() })
	return service.NewStockService(db)
}

// doPostJSONWithBranch 打一个带 JSON body + 门店头的 POST。
func doPostJSONWithBranch(t *testing.T, r *gin.Engine, path, branch string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, json.NewEncoder(&buf).Encode(body), "encode body")
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if branch != "" {
		req.Header.Set("X-Branch-ID", branch)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// externalSource 是"配了数据源"的门店。
func externalSource() cubesource.Source {
	return cubesource.Source{
		BranchID:       "00",
		Configured:     true,
		Enabled:        true,
		CubeSourceName: "cube-sixun-ysx-fb",
	}
}

// ---- 读路径的三态(HTTP 层) ----

// TestHandler_ListStockLevels_SourceUnavailableIs503 锁"问不到数据源 ⇒ 503"。
//
// ⚠️ **不是 200 + 空列表**：那会把一次链路故障显示成「该门店没有库存」。
func TestHandler_ListStockLevels_SourceUnavailableIs503(t *testing.T) {
	svc := newArchiveSvc(t)
	svc.SetCubeSource(&fakeResolver{err: errors.New("cube-router 不可达")}, &fakeReader{})
	r := newArchiveRouter(t, svc)

	w := doGet(t, r, "/stock-levels", "00")
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "cube_unavailable", body["code"])
}

// TestHandler_ListStockLevels_SourceDisabledIs503 锁"数据源被停用 ⇒ 503"。
func TestHandler_ListStockLevels_SourceDisabledIs503(t *testing.T) {
	svc := newArchiveSvc(t)
	disabled := externalSource()
	disabled.Enabled = false
	svc.SetCubeSource(&fakeResolver{src: disabled}, &fakeReader{})
	r := newArchiveRouter(t, svc)

	w := doGet(t, r, "/stock-levels", "00")
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "cube_source_disabled", body["code"])
}

// TestHandler_ListStockLevels_CubeBranchReadsFromCube 锁"有源 ⇒ 走 cube"。
func TestHandler_ListStockLevels_CubeBranchReadsFromCube(t *testing.T) {
	svc := newArchiveSvc(t)
	svc.SetCubeSource(&fakeResolver{src: externalSource()}, &fakeReader{
		rows: []map[string]any{{
			"stock.product_id":     "P001",
			"stock.total_quantity": 261.0,
			"stock.avg_cost":       4.96,
		}},
		total: 1,
	})
	r := newArchiveRouter(t, svc)

	w := doGet(t, r, "/stock-levels", "00")
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		StockLevels []model.StockLevel `json:"stock_levels"`
		Total       int64              `json:"total"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.StockLevels, 1)
	assert.Equal(t, "P001", resp.StockLevels[0].ProductID)
	assert.Equal(t, int64(1), resp.Total)
}

// ---- 导出端点 ----

// TestHandler_ExportArchive_Local 锁"没配数据源 ⇒ 从本域导出(archivedto 契约)"。
func TestHandler_ExportArchive_Local(t *testing.T) {
	svc := newArchiveSvc(t)
	// 本域有一行库存(用导入端点写进去:没配源 ⇒ 放行)。
	w := doPostJSONWithBranch(t, newArchiveRouter(t, svc), "/archive/import", "10", map[string]any{
		"source_branch_id": "00",
		"mode":             "overwrite",
		"data": map[string]any{
			"stocks": []map[string]string{{
				"product_id": "P001", "qty": "261", "avg_cost_yuan": "4.96",
			}},
		},
	})
	require.Equal(t, http.StatusOK, w.Code)

	r := newArchiveRouter(t, svc)
	w = doGet(t, r, "/archive/export?scope=stock", "10")
	require.Equal(t, http.StatusOK, w.Code)

	var page struct {
		Scope  string           `json:"scope"`
		Branch string           `json:"branch_id"`
		Rows   []map[string]any `json:"rows"`
		Count  int              `json:"count"`
		Total  int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page))
	assert.Equal(t, archivedto.ScopeStock, page.Scope)
	assert.Equal(t, "10", page.Branch)
	assert.Equal(t, 1, page.Count)
	assert.Equal(t, 1, page.Total)
	require.Len(t, page.Rows, 1)
	assert.Equal(t, "P001", page.Rows[0]["product_id"])
	assert.Equal(t, "261", page.Rows[0]["qty"], "数量在 JSON 里是字符串")
}

// TestHandler_ExportArchive_ExternalForwardsToCubeRouter 锁"有源 ⇒ 转发(body 原样)"。
func TestHandler_ExportArchive_ExternalForwardsToCubeRouter(t *testing.T) {
	svc := newArchiveSvc(t)
	body := []byte(`{"scope":"stock","branch_id":"00","cube_source":"cube-sixun-ysx-fb",` +
		`"as_of":"2026-10-21","rows":[],"count":0,"total":0,"offset":0,"limit":5000}`)
	exp := &fakeExporter{body: body}
	svc.SetCubeSource(&fakeResolver{src: externalSource()}, &fakeReader{})
	svc.SetArchiveExporter(exp)
	r := newArchiveRouter(t, svc)

	w := doGet(t, r, "/archive/export?scope=stock&offset=10&limit=5", "00")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, exp.calls, "有源 ⇒ 必须转发给 cube-router")
	assert.Equal(t, archivedto.ScopeStock, exp.in.Scope)
	assert.Equal(t, 10, exp.in.Offset)
	assert.Equal(t, 5, exp.in.Limit)
	assert.Equal(t, string(body), w.Body.String(), "响应体必须原样透传")
}

// TestHandler_ExportArchive_ScopeRequired 锁"scope 必填"。
func TestHandler_ExportArchive_ScopeRequired(t *testing.T) {
	svc := newArchiveSvc(t)
	r := newArchiveRouter(t, svc)

	w := doGet(t, r, "/archive/export", "10")
	require.Equal(t, http.StatusBadRequest, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "unsupported_export_scope", body["code"])
}

// TestHandler_ExportArchive_UnsupportedScope 锁"scope 不归本服务 ⇒ 400"。
func TestHandler_ExportArchive_UnsupportedScope(t *testing.T) {
	svc := newArchiveSvc(t)
	r := newArchiveRouter(t, svc)

	w := doGet(t, r, "/archive/export?scope=products", "10")
	require.Equal(t, http.StatusBadRequest, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "unsupported_export_scope", body["code"])
}

// TestHandler_ExportArchive_BranchRequired 锁"门店只从 header 取"。
func TestHandler_ExportArchive_BranchRequired(t *testing.T) {
	svc := newArchiveSvc(t)
	r := newArchiveRouter(t, svc)

	w := doGet(t, r, "/archive/export?scope=stock", "")
	require.Equal(t, http.StatusBadRequest, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "branch_required", body["code"])
}

// ---- 导入端点的只读闸门 ----

// TestHandler_ImportArchive_ExternalSourceIs409 锁"有源门店 ⇒ 409(一行都不写)"。
func TestHandler_ImportArchive_ExternalSourceIs409(t *testing.T) {
	svc := newArchiveSvc(t)
	svc.SetCubeSource(&fakeResolver{src: externalSource()}, &fakeReader{})
	r := newArchiveRouter(t, svc)

	w := doPostJSONWithBranch(t, r, "/archive/import", "00", map[string]any{
		"source_branch_id": "00",
		"mode":             "overwrite",
		"data": map[string]any{
			"stocks": []map[string]string{{
				"product_id": "P001", "qty": "261", "avg_cost_yuan": "4.96",
			}},
		},
	})
	require.Equal(t, http.StatusConflict, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "member_source_is_external", body["code"])
	// ⚠️ **message 不暴露内部 app-id**。
	msg, _ := body["message"].(string)
	assert.NotContains(t, msg, "cube-sixun-ysx-fb")
}

// TestHandler_ImportArchive_LocalIsOK 锁"没配数据源 ⇒ 导入照常"。
func TestHandler_ImportArchive_LocalIsOK(t *testing.T) {
	svc := newArchiveSvc(t)
	svc.SetCubeSource(&fakeResolver{src: cubesource.Source{BranchID: "10"}}, &fakeReader{})
	r := newArchiveRouter(t, svc)

	w := doPostJSONWithBranch(t, r, "/archive/import", "10", map[string]any{
		"source_branch_id": "00",
		"mode":             "overwrite",
		"data": map[string]any{
			"stocks": []map[string]string{{
				"product_id": "P001", "qty": "261", "avg_cost_yuan": "4.96",
			}},
		},
	})
	require.Equal(t, http.StatusOK, w.Code)
}
