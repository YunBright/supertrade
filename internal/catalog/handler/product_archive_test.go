// 本文件锁住「商品档案(图1)」的 HTTP 层:状态码、响应形状、错误映射,
// 以及**过滤真的收窄命中数**。
//
// ⚠️ "查询端点返回了数据"不等于"查询条件生效了"——
// 一个被静默忽略的过滤器会返回"看起来正常"的完整列表。
//
// ⚠️ 本文件是 `package handler`(与同目录既有测试同包)⇒ 复用
// source_test.go 里的 fakeSource / configuredSource / localSource。
package handler

import (
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

	"github.com/YunBright/supertrade/internal/catalog/model"
	"github.com/YunBright/supertrade/internal/catalog/service"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubeclient/cubeclientfake"
	"github.com/YunBright/supertrade/internal/cuberead/cubereadfake"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// newArchiveDB 构造**文件型** SQLite。
func newArchiveDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "catalog.db")
	dialector := sqlite.Dialector{DriverName: "sqlite", DSN: dsn}
	db, err := gorm.Open(dialector, &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.Supplier{}, &model.Product{}, &model.Category{},
		&model.ProductBranchPrice{}, &model.ProductSupplierPrice{},
	), "auto migrate")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// newArchiveRouter 造一个带数据源解析器 + 本地库 + cube 读端的路由。
//
// ⚠️ **不挂 RBAC**:RBAC 需要真 userd;本文件守的是"错误 → 状态码"的映射。
func newArchiveRouter(t *testing.T, src *fakeSource) (*gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.XBranchID())
	// ⚠️ **必须**把 X-Branch-ID 注入 **request.Context**:
	// middleware.XBranchID() 只写 gin.Context,而商品档案的 service
	// 是从 ctx 读门店的(cubeclient.BranchFromContext)。
	// 生产里这一步由 cmd/catalog/main.go 的 forwardBearerToOutgoing 做。
	r.Use(injectBranchIntoContext)
	db := newArchiveDB(t)
	reader := cubereadfake.New()
	reader.Handler = func(m string, q cubeclient.CubeQuery) ([]map[string]any, int, error) {
		switch m {
		case "product":
			return []map[string]any{{
				"product.id": "00001", "product.name": "小白菜", "product.sub_code": "00001",
				"product.category_id": "010201", "product.supplier_id": "019",
				"product.unit": "kg", "product.spec": "500g", "product.status": "1",
				"product.brand_name": "其它品牌",
			}}, 1, nil
		case "branch_price":
			return []map[string]any{{
				"branch_price.item_no": "00001", "branch_price.cost_yuan": "4.96",
				"branch_price.wholesale_yuan": "3.60", "branch_price.retail_yuan": "4.76",
			}}, 1, nil
		case "supplier_item":
			return []map[string]any{{
				"supplier_item.item_no": "00001", "supplier_item.branch_no": "00",
				"supplier_item.supplier_id": "019", "supplier_item.sale_way": "A",
				"supplier_item.top_price_yuan": "8.00",
			}}, 1, nil
		}
		return nil, 0, nil
	}
	h := New(
		service.NewSupplierService(db), service.NewProductService(db),
		service.NewCategoryService(db), service.NewProductBarcodeService(db),
		service.NewArchiveService(db), cubeclientfake.New(), nil, nil)
	h.SetSourceResolver(src)
	h.SetLocalDB(db)
	h.SetCubeReader(reader)
	r.GET("/product-archive", h.listProductArchive)
	r.GET("/product-archive/:id/supplier-prices", h.getProductSupplierPrices)
	return r, db
}

// injectBranchIntoContext 把 X-Branch-ID 写进 request.Context。
func injectBranchIntoContext(c *gin.Context) {
	if b := c.Request.Header.Get("X-Branch-ID"); b != "" {
		c.Request = c.Request.WithContext(cubeclient.WithBranchID(c.Request.Context(), b))
	}
	c.Next()
}

// seedArchive 灌本域 PG 的门店 10 商品档案。
func seedArchive(t *testing.T, db *gorm.DB) {
	t.Helper()
	now := time.Now()
	rows := []any{
		&model.Category{ID: "C1", BranchID: "10", Name: "纸品清洁", Status: "active", CreatedBy: "t", CreatedAt: now, UpdatedAt: now},
		&model.Category{ID: "C1-01", BranchID: "10", Name: "抽纸", ParentID: "C1", Status: "active", CreatedBy: "t", CreatedAt: now, UpdatedAt: now},
		&model.Supplier{ID: "S1", BranchID: "10", Name: "某某商行", Type: "0", Status: "active", CreatedBy: "t", CreatedAt: now, UpdatedAt: now},
		&model.Product{ID: "LOCAL-1", BranchID: "10", Name: "本地抽纸", CategoryID: "C1-01", SupplierID: "S1", SubCode: "SC1", Unit: "提", Status: "active", CreatedBy: "t", CreatedAt: now, UpdatedAt: now},
		&model.Product{ID: "LOCAL-2", BranchID: "10", Name: "本地饮料", CategoryID: "C1", SupplierID: "S1", Status: "active", CreatedBy: "t", CreatedAt: now, UpdatedAt: now},
	}
	for _, r := range rows {
		require.NoError(t, db.Create(r).Error, "seed")
	}
}

func doArchive(r *gin.Engine, path, branch string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if branch != "" {
		req.Header.Set("X-Branch-ID", branch)
	}
	r.ServeHTTP(w, req)
	return w
}

func decodeArchive(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &m))
	return m
}

// TestProductArchive_LocalReturnsRows 锁"没配数据源 ⇒ 200 + 本地数据"。
func TestProductArchive_LocalReturnsRows(t *testing.T) {
	r, db := newArchiveRouter(t, &fakeSource{src: localSource()})
	seedArchive(t, db)

	w := doArchive(r, "/product-archive?page=1&page_size=50", "10")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	m := decodeArchive(t, w)
	assert.Equal(t, float64(2), m["total"])
	assert.Equal(t, "local", m["data_source"])
	items := m["items"].([]any)
	require.Len(t, items, 2)
	row := items[0].(map[string]any)
	// ⚠️ 没维护的价是 **null**,不是 0。
	assert.Nil(t, row["cost_yuan"])
	assert.Nil(t, row["member_price_yuan"])
}

// TestProductArchive_FilterNarrowsCount 锁"过滤真的收窄命中数"。
func TestProductArchive_FilterNarrowsCount(t *testing.T) {
	r, db := newArchiveRouter(t, &fakeSource{src: localSource()})
	seedArchive(t, db)

	all := decodeArchive(t, doArchive(r, "/product-archive", "10"))
	require.Equal(t, float64(2), all["total"])

	kw := decodeArchive(t, doArchive(r, "/product-archive?q=本地抽", "10"))
	assert.Equal(t, float64(1), kw["total"], "关键字过滤必须真的收窄命中数")

	sup := decodeArchive(t, doArchive(r, "/product-archive?supplier_id=S1", "10"))
	assert.Equal(t, float64(2), sup["total"])

	none := decodeArchive(t, doArchive(r, "/product-archive?supplier_id=NOPE", "10"))
	assert.Equal(t, float64(0), none["total"], "不存在的条件 ⇒ 命中 0 行")
}

// TestProductArchive_ResolveFailureIs503 锁"问不到数据源 ⇒ 503(**不是**空列表)"。
func TestProductArchive_ResolveFailureIs503(t *testing.T) {
	r, db := newArchiveRouter(t, &fakeSource{err: errors.New("cube-router 不可达")})
	seedArchive(t, db)

	w := doArchive(r, "/product-archive", "10")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Equal(t, "cube_unavailable", decodeArchive(t, w)["code"])
}

// TestProductArchive_SourceDisabledIs503 锁"数据源被停用 ⇒ 503"。
func TestProductArchive_SourceDisabledIs503(t *testing.T) {
	disabled := configuredSource()
	disabled.Enabled = false
	r, db := newArchiveRouter(t, &fakeSource{src: disabled})
	seedArchive(t, db)

	w := doArchive(r, "/product-archive", "10")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Equal(t, "cube_source_disabled", decodeArchive(t, w)["code"])
}

// TestSupplierPrices_CubeReturnsRows 锁"配了数据源 ⇒ 供货信息来自 cube"。
func TestSupplierPrices_CubeReturnsRows(t *testing.T) {
	r, _ := newArchiveRouter(t, &fakeSource{src: configuredSource()})

	w := doArchive(r, "/product-archive/00001/supplier-prices", "00")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	m := decodeArchive(t, w)
	assert.Equal(t, "cube", m["data_source"])
	items := m["items"].([]any)
	require.Len(t, items, 1)
	row := items[0].(map[string]any)
	assert.Equal(t, "00001", row["item_no"])
	assert.Equal(t, "8.0000", row["top_price_yuan"])
}
