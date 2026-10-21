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
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
	_ "modernc.org/sqlite"

	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/masterdata/handler"
	"github.com/YunBright/supertrade/internal/masterdata/model"
	"github.com/YunBright/supertrade/internal/masterdata/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// ---- test doubles ----

// fakeSourceResolver 是 cubesource.Resolver 的 fake。
//
// ⚠️ 只实现"这个门店配了哪个数据源"这一个能力 ——
//	归档端点**不查业务数据**,所以不需要别的 fake。
type fakeSourceResolver struct {
	src cubesource.Source
	err error
}

func (f fakeSourceResolver) Resolve(_ context.Context, _ string) (cubesource.Source, error) {
	if f.err != nil {
		return cubesource.Source{}, f.err
	}
	return f.src, nil
}

// localSource 是"自营店"(没配数据源)。
func localSource() fakeSourceResolver {
	return fakeSourceResolver{src: cubesource.Source{BranchID: "10", Configured: false, Enabled: false}}
}

// externalSource 是"配了外部数据源且启用"。
func externalSource() fakeSourceResolver {
	return fakeSourceResolver{src: cubesource.Source{
		BranchID: "10", Configured: true, Enabled: true, CubeSourceName: "cube-sixun-ysx-fb",
	}}
}

// ---- fixtures ----

func newArchiveDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Dialector{DriverName: "sqlite", DSN: ":memory:"},
		&gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Store{}, &model.Employee{}, &model.Shift{}))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// seedBranch10 造一行门店档案 + 一个收银员 + 一个班次。
func seedBranch10(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(t, db.Create(&model.Store{
		BranchID: "10", Code: "S10", Name: "云商自营店", PosEnabled: true,
		StocktakeCycleDays: 7, MemberEnabled: true, CounterNos: "1,2",
	}).Error)
	require.NoError(t, db.Create(&model.Employee{
		ID: "auth-u-1", BranchID: "10", EmployeeCode: "E001", Name: "张三",
		Role: "cashier", Status: "active", DiscountRate: decimal.NewFromInt(1),
	}).Error)
	require.NoError(t, db.Create(&model.Shift{
		ID: "SH20260101000000", BranchID: "10", EmployeeID: "auth-u-1",
		CounterNo: "1", Status: "open", OpeningAmountYuan: decimal.NewFromInt(100),
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
		return scope == service.ScopeStoreView || scope == service.ScopeStoreManage, nil
	})
	handler.NewArchiveHandler(archive, allow, nil).RegisterRoutes(r)
	return r
}

func doArchive(t *testing.T, r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Branch-ID", "10")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// ---- 用例 ----

// TestArchiveExport_NoSource_ReturnsLocalRows 锁"没配数据源 ⇒ 导本地"。
func TestArchiveExport_NoSource_ReturnsLocalRows(t *testing.T) {
	db := newArchiveDB(t)
	seedBranch10(t, db)
	r := newArchiveRouter(t, db, localSource())

	w := doArchive(t, r, http.MethodGet, "/archive/export?scope=stores", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var got service.ExportResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, service.ScopeStores, got.Scope)
	assert.Equal(t, "10", got.BranchID)
	assert.False(t, got.External, "没配源 ⇒ 不是外部模式")
	assert.Equal(t, 1, got.Count)
	require.Len(t, got.Stores, 1)
	assert.Equal(t, "云商自营店", got.Stores[0].Name)
}

// TestArchiveExport_SourceConfigured_ReturnsEmpty 锁"配了源 ⇒ 导出**空**"。
//
// ⚠️ 关键断言:本地明明有 1 行门店档案,导出结果**仍然必须是空的**
//	——回落本地 = 把"数据在外部系统里"显示成"本地的旧数据",那是假数据。
func TestArchiveExport_SourceConfigured_ReturnsEmpty(t *testing.T) {
	db := newArchiveDB(t)
	seedBranch10(t, db)
	r := newArchiveRouter(t, db, externalSource())

	w := doArchive(t, r, http.MethodGet, "/archive/export?scope=stores", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var got service.ExportResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.True(t, got.External, "配了源 ⇒ external=true")
	assert.Equal(t, 0, got.Count)
	assert.Empty(t, got.Stores, "配了源 ⇒ 不许回落本地")
	// ⚠️ 数组必须**存在**(空数组),不是缺省 —— 缺省是契约事故。
	assert.Contains(t, w.Body.String(), `"stores":[]`)
}

// TestArchiveExport_ResolveFailure_503 锁"问不到路由 ⇒ 503,不许回落本地"。
func TestArchiveExport_ResolveFailure_503(t *testing.T) {
	db := newArchiveDB(t)
	seedBranch10(t, db)
	r := newArchiveRouter(t, db, fakeSourceResolver{err: assertErr{}})

	w := doArchive(t, r, http.MethodGet, "/archive/export?scope=stores", "")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "cube_unavailable")
}

// TestArchiveImport_SourceConfigured_409 锁"配了源 ⇒ 导入 409 只读"。
func TestArchiveImport_SourceConfigured_409(t *testing.T) {
	db := newArchiveDB(t)
	seedBranch10(t, db)
	r := newArchiveRouter(t, db, externalSource())

	body := `{"scope":"stores","stores":[{"branch_id":"10","name":"试图写回"}]}`
	w := doArchive(t, r, http.MethodPost, "/archive/import", body)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "member_source_is_external")
	// ⚠️ message 不得暴露内部 app-id。
	assert.NotContains(t, w.Body.String(), "cube-sixun-ysx-fb")

	// ⚠️ 拒绝 ⇒ 一行都不能写进去。
	var n int64
	require.NoError(t, db.Model(&model.Store{}).Where("branch_id = ?", "10").Count(&n).Error)
	assert.Equal(t, int64(1), n, "409 之后本地数据不许被改动")
}

// TestArchiveImport_Local_WritesAndIsIdempotent 锁"没配源 ⇒ 写本地 + 幂等"。
func TestArchiveImport_Local_WritesAndIsIdempotent(t *testing.T) {
	db := newArchiveDB(t)
	r := newArchiveRouter(t, db, localSource())

	body := `{"scope":"employees","employees":[` +
		`{"id":"auth-u-9","employee_code":"E009","name":"李四","role":"supervisor","status":"active","discount_rate":"0.95"}]}`
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
	require.NoError(t, db.Model(&model.Employee{}).Count(&n).Error)
	assert.Equal(t, int64(1), n)
}

// TestArchiveExportRows_NoCredentialColumns 锁"导出行里没有凭据列"。
//
// ⚠️ 本域 employees 表**本来就没有**口令哈希 / API key / 人脸特征
//	(身份与令牌在 auth.users)⇒ 没有可导出的敏感列。
//	这个测试是为了将来有人往 employees 加了 password_hash 之类的列时能变红。
func TestArchiveExportRows_NoCredentialColumns(t *testing.T) {
	banned := []string{"password", "passwd", "secret", "token", "credential", "face", "api_key", "apikey", "salt", "hash"}
	check := func(t *testing.T, name string, raw map[string]any) {
		t.Helper()
		for k := range raw {
			low := strings.ToLower(k)
			for _, b := range banned {
				assert.NotContains(t, low, b, "%s 的导出行不得含凭据列 %q", name, k)
			}
		}
	}
	row := service.EmployeeRow{ID: "u", EmployeeCode: "E", Name: "N", Role: "cashier",
		Status: "active", DiscountRate: "1", CounterNo: "1"}
	raw, err := json.Marshal(row)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	check(t, "EmployeeRow", m)

	raw, err = json.Marshal(service.StoreRow{BranchID: "10", Name: "N"})
	require.NoError(t, err)
	m = nil
	require.NoError(t, json.Unmarshal(raw, &m))
	check(t, "StoreRow", m)

	raw, err = json.Marshal(service.ShiftRow{ID: "S", EmployeeID: "u"})
	require.NoError(t, err)
	m = nil
	require.NoError(t, json.Unmarshal(raw, &m))
	check(t, "ShiftRow", m)
}

// assertErr 是一个普通 error(fake resolver 用)。
type assertErr struct{}

func (assertErr) Error() string { return "cube-router 问不到" }
