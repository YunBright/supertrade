// 本文件锁住 catalog 的 HTTP 层「门店 → 数据源」分流。
//
// # 三态必须能从 HTTP 上看出来
//
//	配了源   ⇒ 200 + **cube 的数据**
//	没配源   ⇒ 200 + **本域 PG 的数据**
//	问不到   ⇒ 503 cube_unavailable(不是空列表!)
//	被停用   ⇒ 503 cube_source_disabled
//
// ⚠️ "没配源"那条断言的是**数据来源**(真的返回了本地商品),
// 不是只断言状态码 —— 只断言 200 的话,一个恒返空列表的实现也能过。
//
// # 导入端点的只读闸门
//
//	有源门店 ⇒ 409 member_source_is_external(与会员域同名同码)
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	_ "modernc.org/sqlite" // 注册驱动名 "sqlite"(纯 Go 驱动,无需 CGO)

	"github.com/YunBright/supertrade/internal/archivedto"
	"github.com/YunBright/supertrade/internal/catalog/model"
	"github.com/YunBright/supertrade/internal/catalog/service"
	"github.com/YunBright/supertrade/internal/cubeclient/cubeclientfake"
	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// ---- 测试替身 ----

// fakeSource 替身 cubesource.Resolver。
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

// configuredSource 是"配了数据源"的门店。
func configuredSource() cubesource.Source {
	return cubesource.Source{
		BranchID:       "00",
		Configured:     true,
		Enabled:        true,
		CubeSourceName: "cube-sixun-ysx-fb",
	}
}

// localSource 是"没配数据源"的门店(自营店 10)。
func localSource() cubesource.Source {
	return cubesource.Source{BranchID: "10"}
}

// newSourceTestDB 造一个内存 DB(四张表齐全)。
func newSourceTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dialector := sqlite.Dialector{DriverName: "sqlite", DSN: ":memory:?_pragma=foreign_keys(1)"}
	db, err := gorm.Open(dialector, &gorm.Config{})
	require.NoError(t, err, "open sqlite")
	require.NoError(t, db.AutoMigrate(
		&model.Supplier{}, &model.Product{}, &model.Category{}, &model.ProductBarcode{},
	), "auto migrate")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return db
}

// newSourceTestRouter 造一个带数据源解析器 + 本地库的路由。
//
// ⚠️ 解析器与本地库**两个都装** —— 只装一个的话,
// "走哪条路"就成了巧合而不是被测出来的行为。
func newSourceTestRouter(t *testing.T, src *fakeSource) *gin.Engine {
	t.Helper()
	return newSourceTestRouterWith(t, src, nil)
}

// fakeExporter 替身 service.ArchiveExporter(上游导出端点)。
type fakeExporter struct {
	rows map[string][]map[string]any
	// scopes 记录被请求过的 scope。
	scopes []string
}

func (f *fakeExporter) Export(_ context.Context, _, _, scope string, offset, limit int) (*service.ExportPage, error) {
	f.scopes = append(f.scopes, scope)
	rows := f.rows[scope]
	if offset >= len(rows) {
		rows = nil
	} else {
		rows = rows[offset:]
	}
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return &service.ExportPage{
		ExportPage: archivedto.ExportPage{
			Scope:      scope,
			BranchID:   "00",
			CubeSource: "cube-sixun-ysx-fb",
			Rows:       rows,
			Count:      len(rows),
			Total:      len(rows),
			Offset:     offset,
			Limit:      limit,
		},
		AsOf: "2026-10-22",
	}, nil
}

// newSourceTestRouterWith 造一个带数据源解析器 + 本地库(+ 可选上游导出端点)的路由。
func newSourceTestRouterWith(t *testing.T, src *fakeSource, exporter service.ArchiveExporter) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.XBranchID())
	db := newSourceTestDB(t)
	h := New(
		service.NewSupplierService(db),
		service.NewProductService(db),
		service.NewCategoryService(db),
		service.NewProductBarcodeService(db),
		service.NewArchiveService(db),
		cubeclientfake.New(), nil, nil)
	h.SetSourceResolver(src)
	h.SetLocalDB(db)
	if exporter != nil {
		h.SetArchiveExporter(exporter)
	}
	h.RegisterProductPickerRoutes(r, func(scope string) []gin.HandlerFunc {
		return []gin.HandlerFunc{h.requireSingleBranch()}
	})
	// 写侧只用来灌种子数据(不挂 rbac 层,与既有本地档案测试同款)。
	r.POST("/products", h.createProduct)
	r.GET("/products/export", h.exportLocalProducts)
	r.POST("/archive/import", h.importArchive)
	r.GET("/archive/export", h.exportArchive)
	return r
}

// seedLocalProducts 灌门店 10 的本地商品。
func seedLocalProducts(t *testing.T, r *gin.Engine) {
	t.Helper()
	w := doJSON(t, r, http.MethodPost, "/products", "10", map[string]any{
		"id": "LOCAL-1", "name": "本地抽纸", "category_id": "C1", "supplier_id": "S1",
	})
	require.Equal(t, http.StatusCreated, w.Code, "body=%s", w.Body.String())
}

// productIDs 取响应里的商品 id 集合。
func productIDs(t *testing.T, w *httptest.ResponseRecorder) map[string]bool {
	t.Helper()
	var body struct {
		Products []struct {
			ID string `json:"id"`
		} `json:"products"`
		Total int `json:"total"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), "body=%s", w.Body.String())
	ids := map[string]bool{}
	for _, p := range body.Products {
		ids[p.ID] = true
	}
	return ids
}

// ---- picker 的三态 ----

// TestPicker_ConfiguredSourceReadsCube 锁"配了数据源 ⇒ 走 cube"。
func TestPicker_ConfiguredSourceReadsCube(t *testing.T) {
	r := newSourceTestRouter(t, &fakeSource{src: configuredSource()})
	seedLocalProducts(t, r)

	w := do(t, r, "/products", "00")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	ids := productIDs(t, w)
	assert.True(t, ids["P-1001"], "必须来自 cube")
	assert.False(t, ids["LOCAL-1"], "**不该**返回本地商品")
}

// TestPicker_NoSourceReadsLocal 锁"没配数据源 ⇒ 走本域 PG"。
//
// ⚠️ 这是 2026-10-21 生产故障的那条路径:
// 自营店 10 调 picker 之前恒报 `cube 404 not found`,
// 而它的本地表里明明有商品。
func TestPicker_NoSourceReadsLocal(t *testing.T) {
	r := newSourceTestRouter(t, &fakeSource{src: localSource()})
	seedLocalProducts(t, r)

	w := do(t, r, "/products", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	ids := productIDs(t, w)
	assert.True(t, ids["LOCAL-1"], "必须真的返回本地商品")
	assert.False(t, ids["P-1001"], "**不该**打 cube")
}

// TestPicker_ResolveFailure_503 锁"问不到数据源 ⇒ 503(不是空列表)"。
func TestPicker_ResolveFailure_503(t *testing.T) {
	r := newSourceTestRouter(t, &fakeSource{err: errors.New("cube-router 不可达")})
	seedLocalProducts(t, r)

	for _, path := range pickerPaths {
		w := do(t, r, path, "10")
		require.Equal(t, http.StatusServiceUnavailable, w.Code,
			"path=%s body=%s", path, w.Body.String())
		require.Equal(t, "cube_unavailable", errorCode(t, w), "path=%s", path)
	}
}

// TestPicker_DisabledSource_503 锁"数据源被停用 ⇒ 503"。
func TestPicker_DisabledSource_503(t *testing.T) {
	disabled := configuredSource()
	disabled.Enabled = false
	r := newSourceTestRouter(t, &fakeSource{src: disabled})

	w := do(t, r, "/products", "00")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, "body=%s", w.Body.String())
	require.Equal(t, "cube_source_disabled", errorCode(t, w))
}

// ---- 导入闸门 ----

// TestArchiveImport_ExternalSource_409 锁"有源门店 ⇒ 导入被拒"。
//
// ⚠️ code 与会员域同名(`member_source_is_external`):判据全服务一致。
func TestArchiveImport_ExternalSource_409(t *testing.T) {
	r := newSourceTestRouter(t, &fakeSource{src: configuredSource()})

	w := doJSON(t, r, http.MethodPost, "/archive/import", "00", map[string]any{
		"source_branch_id": "00",
		"mode":             "incremental",
		"data": map[string]any{
			"products": []map[string]any{{"id": "X1", "name": "外部商品"}},
		},
	})
	require.Equal(t, http.StatusConflict, w.Code, "body=%s", w.Body.String())
	require.Equal(t, "member_source_is_external", errorCode(t, w))
	assert.NotContains(t, w.Body.String(), "cube-sixun-ysx-fb",
		"message 里不许暴露内部 app-id")
}

// TestArchiveImport_NoSource_OK 锁"没配数据源的门店 ⇒ 正常导入"。
func TestArchiveImport_NoSource_OK(t *testing.T) {
	r := newSourceTestRouter(t, &fakeSource{src: localSource()})

	w := doJSON(t, r, http.MethodPost, "/archive/import", "10", map[string]any{
		"source_branch_id": "00",
		"mode":             "incremental",
		"data": map[string]any{
			"products": []map[string]any{{"id": "X1", "name": "导入商品"}},
		},
	})
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
}

// ---- 导出端口 ----

// TestArchiveExport_NoSourceReadsLocal 锁"没配数据源 ⇒ 导出本域 PG 的档案"。
//
// ⚠️ 输出必须是 **archivedto 契约**(字段名与上游导出端点逐字一致)。
func TestArchiveExport_NoSourceReadsLocal(t *testing.T) {
	r := newSourceTestRouter(t, &fakeSource{src: localSource()})
	seedLocalProducts(t, r)

	w := do(t, r, "/archive/export?scope=products", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body struct {
		Scope    string           `json:"scope"`
		BranchID string           `json:"branch_id"`
		Rows     []map[string]any `json:"rows"`
		Count    int              `json:"count"`
		Total    int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), "body=%s", w.Body.String())
	assert.Equal(t, "products", body.Scope)
	assert.Equal(t, "10", body.BranchID)
	require.Len(t, body.Rows, 1)
	assert.Equal(t, "LOCAL-1", body.Rows[0]["id"])
	assert.Equal(t, "本地抽纸", body.Rows[0]["name"])
	assert.Equal(t, 1, body.Total)

	// ⚠️ **字段集必须与上游导出端点逐字一致**(archivedto 契约):
	// 编排方(cubesync)按这些名字读行,差一个字符就是"整列读不到",零报错。
	var raw map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	assert.ElementsMatch(t, []string{
		"scope", "branch_id", "cube_source", "as_of",
		"rows", "count", "total", "offset", "limit",
	}, keysOf(raw), "导出响应的字段集必须与 archivedto 契约一致")
}

// keysOf 取一个 JSON 对象的键集合。
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestArchiveExport_UnsupportedScope_400 锁"本域不导的资料域 ⇒ 400"。
//
// ⚠️ 本域只有四张表;售价 / 促销 / 库存 / 会员归别的服务。
// 导出一堆空行会被下游当成"这个门店真的没有"。
func TestArchiveExport_UnsupportedScope_400(t *testing.T) {
	r := newSourceTestRouter(t, &fakeSource{src: localSource()})

	w := do(t, r, "/archive/export?scope=prices", "10")
	require.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	require.Equal(t, "unsupported_export_scope", errorCode(t, w))
}

// TestArchiveExport_MissingScope_400 锁"scope 必填"。
func TestArchiveExport_MissingScope_400(t *testing.T) {
	r := newSourceTestRouter(t, &fakeSource{src: localSource()})

	w := do(t, r, "/archive/export", "10")
	require.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	require.Equal(t, "bad_request", errorCode(t, w))
}

// TestProductsExport_ConfiguredSourceReadsUpstream 锁"离线商品库也按数据源切"。
//
// ⚠️ **路径与响应形状都不许变**(前端 / POS 离线包在用)⇒
// 字段名必须与本地路径**逐字一致**,只有数据来源不同。
func TestProductsExport_ConfiguredSourceReadsUpstream(t *testing.T) {
	exp := &fakeExporter{rows: map[string][]map[string]any{
		archivedto.ScopeProducts: {
			{
				"id": "EXT-1", "name": "外部商品", "category_id": "C1",
				"supplier_id": "S1", "unit": "提", "spec": "1*8",
				"sub_code": "SC1", "status": "1",
				"price_yuan": "9.90", "cost_yuan": "5.00",
			},
		},
		archivedto.ScopeItemBarcodes: {
			{"barcode": "6908946288183", "product_id": "EXT-1", "label": "口味"},
		},
	}}
	r := newSourceTestRouterWith(t, &fakeSource{src: configuredSource()}, exp)
	seedLocalProducts(t, r)

	w := do(t, r, "/products/export", "00")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body struct {
		BranchID string           `json:"branch_id"`
		Products []map[string]any `json:"products"`
		Count    int              `json:"count"`
		Total    int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), "body=%s", w.Body.String())
	require.Len(t, body.Products, 1, "必须来自上游导出端点,body=%s", w.Body.String())
	assert.Equal(t, "EXT-1", body.Products[0]["id"])
	assert.Equal(t, "外部商品", body.Products[0]["name"])
	assert.Equal(t, "00", body.BranchID)

	// ⚠️ 字段名必须与本地路径**逐字一致**(既有断言 TestExportProducts_FieldNamesAreExact
	// 锁的就是这一组)—— 差一个字符就是客户端"整列读不到",而且零报错。
	want := []string{
		"id", "name", "sub_code", "barcode", "barcodes", "unit", "spec",
		"category_id", "supplier_id", "status", "updated_at",
	}
	got := make([]string, 0, len(body.Products[0]))
	for k := range body.Products[0] {
		got = append(got, k)
	}
	assert.ElementsMatch(t, want, got, "上游路径的字段名必须与本地路径逐字一致")

	// barcode 回落成 id(上游的 product 模型没有 barcode 列)。
	assert.Equal(t, "EXT-1", body.Products[0]["barcode"])
	// 子表条码 + 回落 id(去重)。
	assert.ElementsMatch(t, []any{"6908946288183", "EXT-1"}, body.Products[0]["barcodes"])
}
