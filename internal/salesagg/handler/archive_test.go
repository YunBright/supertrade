package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YunBright/authkit/claims"
	"github.com/YunBright/authkit/rbac"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	_ "modernc.org/sqlite"

	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/salesagg/compare"
	"github.com/YunBright/supertrade/internal/salesagg/handler"
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

func newArchiveStore(t *testing.T) *compare.Store {
	t.Helper()
	dialector := sqlite.Dialector{
		DriverName: "sqlite",
		DSN:        ":memory:?_pragma=foreign_keys(1)&_time_format=sqlite",
	}
	db, err := gorm.Open(dialector, &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&compare.Template{}))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return compare.NewStore(db)
}

// seedArchiveTemplate 造一个含当前门店的模板 + 一个不含当前门店的模板。
func seedArchiveTemplate(t *testing.T, store *compare.Store) {
	t.Helper()
	mine, err := compare.TemplateFromInput(compare.SaveInput{
		Name:      "本店对比",
		Dimension: compare.DimCategory,
		Stores:    []string{archiveBranch, "11"},
		Rows: []compare.RowJSON{{
			Key:   "r1",
			Label: "销售额",
			Items: map[string]*string{archiveBranch: strPtr("A"), "11": strPtr("B")},
		}},
	})
	require.NoError(t, err)
	require.NoError(t, store.Create(context.Background(), mine))

	other, err := compare.TemplateFromInput(compare.SaveInput{
		Name:      "别店对比",
		Dimension: compare.DimCategory,
		Stores:    []string{"11", "12"},
		Rows: []compare.RowJSON{{
			Key:   "r1",
			Label: "销售额",
			Items: map[string]*string{"11": strPtr("B"), "12": strPtr("C")},
		}},
	})
	require.NoError(t, err)
	require.NoError(t, store.Create(context.Background(), other))
}

func strPtr(s string) *string { return &s }

func newArchiveRouter(t *testing.T, store *compare.Store, resolver cubesource.Resolver) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.XBranchID())
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(
			claims.WithClaims(c.Request.Context(), &claims.Claims{Sub: "u-1"}))
		c.Next()
	})
	archive := compare.NewArchiveService(store)
	archive.SetSourceResolver(resolver)
	allow := rbac.ScopeResolver(func(_ *gin.Context, _, _, scope string) (bool, error) {
		return scope == compare.ScopeInventoryView || scope == compare.ScopeInventoryManage, nil
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
	store := newArchiveStore(t)
	seedArchiveTemplate(t, store)
	r := newArchiveRouter(t, store, archiveLocalSource())

	w := doArchive(t, r, http.MethodGet, "/archive/export?scope=compare_templates", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var got compare.ExportResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, compare.ScopeCompareTemplates, got.Scope)
	assert.False(t, got.External)
	assert.Equal(t, 1, got.Count)
	require.Len(t, got.CompareTemplates, 1)
	assert.Equal(t, "本店对比", got.CompareTemplates[0].Name)
	assert.Equal(t, []string{archiveBranch, "11"}, got.CompareTemplates[0].Stores)
}

// TestArchiveExport_SourceConfigured_ReturnsEmpty 锁"配了源 ⇒ 导出**空**"。
func TestArchiveExport_SourceConfigured_ReturnsEmpty(t *testing.T) {
	store := newArchiveStore(t)
	seedArchiveTemplate(t, store)
	r := newArchiveRouter(t, store, archiveExternalSource())

	w := doArchive(t, r, http.MethodGet, "/archive/export?scope=compare_templates", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var got compare.ExportResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.True(t, got.External)
	assert.Equal(t, 0, got.Count)
	assert.Empty(t, got.CompareTemplates)
}

// TestArchiveExport_ResolveFailure_503 锁"问不到路由 ⇒ 503"。
func TestArchiveExport_ResolveFailure_503(t *testing.T) {
	store := newArchiveStore(t)
	seedArchiveTemplate(t, store)
	r := newArchiveRouter(t, store, archiveSourceResolver{err: archiveResolveError{}})

	w := doArchive(t, r, http.MethodGet, "/archive/export?scope=compare_templates", "")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "cube_unavailable")
}

// TestArchiveImport_SourceConfigured_409 锁"配了源 ⇒ 导入 409 只读"。
func TestArchiveImport_SourceConfigured_409(t *testing.T) {
	store := newArchiveStore(t)
	seedArchiveTemplate(t, store)
	r := newArchiveRouter(t, store, archiveExternalSource())

	body := `{"scope":"compare_templates","compare_templates":[{"id":"tpl-new","name":"试图写回",` +
		`"dimension":"category","stores":["10","11"],"rows":[{"key":"r1","label":"销售额","items":{}}]}]}`
	w := doArchive(t, r, http.MethodPost, "/archive/import", body)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "member_source_is_external")
	assert.NotContains(t, w.Body.String(), "cube-sixun-ysx-fb")

	all, err := store.List(context.Background())
	require.NoError(t, err)
	assert.Len(t, all, 2, "409 之后本地数据不许被改动")
}

// TestArchiveImport_Local_WritesAndIsIdempotent 锁"没配源 ⇒ 写本地 + 幂等"。
func TestArchiveImport_Local_WritesAndIsIdempotent(t *testing.T) {
	store := newArchiveStore(t)
	r := newArchiveRouter(t, store, archiveLocalSource())

	body := `{"scope":"compare_templates","compare_templates":[{"id":"tpl-new","name":"新模板",` +
		`"dimension":"category","stores":["10","11"],` +
		`"rows":[{"key":"r1","label":"销售额","items":{"10":"A","11":"B"}}]}]}`
	w := doArchive(t, r, http.MethodPost, "/archive/import", body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var rep compare.ImportResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rep))
	assert.Equal(t, 1, rep.Received)
	assert.Equal(t, 1, rep.Inserted)
	assert.Equal(t, 0, rep.Updated)

	// 幂等:再导一次 ⇒ updated=1,行数不变。
	w2 := doArchive(t, r, http.MethodPost, "/archive/import", body)
	require.Equal(t, http.StatusOK, w2.Code, w2.Body.String())
	var rep2 compare.ImportResult
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &rep2))
	assert.Equal(t, 0, rep2.Inserted)
	assert.Equal(t, 1, rep2.Updated)

	all, err := store.List(context.Background())
	require.NoError(t, err)
	assert.Len(t, all, 1)
}

// TestArchiveImport_SkipsOtherBranchTemplates 锁"跨店模板不导入"。
//
// ⚠️ 调用方只有当前门店的权限,不该改写一份别的门店的全局模板。
func TestArchiveImport_SkipsOtherBranchTemplates(t *testing.T) {
	store := newArchiveStore(t)
	r := newArchiveRouter(t, store, archiveLocalSource())

	body := `{"scope":"compare_templates","compare_templates":[{"id":"tpl-other","name":"别店模板",` +
		`"dimension":"category","stores":["11","12"],` +
		`"rows":[{"key":"r1","label":"销售额","items":{"11":"B","12":"C"}}]}]}`
	w := doArchive(t, r, http.MethodPost, "/archive/import", body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var rep compare.ImportResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rep))
	assert.Equal(t, 0, rep.Inserted)
	assert.Equal(t, 1, rep.Skipped)
	assert.NotEmpty(t, rep.Warnings)

	all, err := store.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, all, "跨店模板不许被写进来")
}
