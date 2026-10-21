// 本文件锁住 catalog **本地档案**(products / categories)的 HTTP 层行为。
//
// # 为什么单独立一个文件
//
// 既有端点(商品选择控件 + cube 兜底)由 catalog_test.go 锁。
// 本地档案是 2026-10-20 新增的**写侧 + 本地读侧**,与 cube 那套并存,
// 断言点完全不同(尤其下面那条**路由优先级**)。
//
// # 守门链说明
//
// 生产上这些端点挂 `rbac.RequireScopeWithBranch("product:manage", ...)`,
// 需要 userd 才能跑起来,单测里拉不起来。
// 这里只挂"严格单店"(与既有 picker 测试同款),业务行为(201/409/404/204/400)
// 与权限层是**两件事** —— 权限层由 deployer 的契约测试 + auth 侧保证。
package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	_ "modernc.org/sqlite" // 注册驱动名 "sqlite"(纯 Go 驱动,无需 CGO)

	"github.com/YunBright/supertrade/internal/catalog/model"
	"github.com/YunBright/supertrade/internal/catalog/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// newLocalTestRouter 挂本地档案的端点(不带 rbac 层)。
func newLocalTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.XBranchID())

	dialector := sqlite.Dialector{DriverName: "sqlite", DSN: ":memory:?_pragma=foreign_keys(1)"}
	db, err := gorm.Open(dialector, &gorm.Config{})
	require.NoError(t, err, "open sqlite")
	require.NoError(t, db.AutoMigrate(&model.Supplier{}, &model.Product{}, &model.Category{}, &model.ProductBarcode{}), "auto migrate")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	h := New(
		service.NewSupplierService(db),
		service.NewProductService(db),
		service.NewCategoryService(db),
		service.NewProductBarcodeService(db),
		service.NewArchiveService(db),
		nil, nil, nil,
	)

	// 与生产同款的路径集合。注意 :id 先注册、/local 后注册,
	// **注册顺序反过来也要成立**(见下面的路由优先级测试)。
	r.GET("/products/:id", h.getProduct)
	r.POST("/products", h.createProduct)
	r.PUT("/products/:id", h.updateProduct)
	r.DELETE("/products/:id", h.deleteProduct)
	r.GET("/products/local", h.listLocalProducts)
	r.GET("/products/export", h.exportLocalProducts)
	r.GET("/products/by-barcode/:code", h.getProductByBarcode)
	r.GET("/products/:id/barcodes", h.listProductBarcodes)
	r.POST("/products/:id/barcodes", h.createProductBarcode)
	r.DELETE("/products/:id/barcodes/:barcode", h.deleteProductBarcode)

	r.GET("/categories/:id", h.getCategory)
	r.POST("/categories", h.createCategory)
	r.PUT("/categories/:id", h.updateCategory)
	r.DELETE("/categories/:id", h.deleteCategory)
	r.GET("/categories/local", h.listLocalCategories)

	return r
}

// doJSON 打一个带 JSON body 的请求。
func doJSON(t *testing.T, r *gin.Engine, method, path, branch string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body), "encode body")
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if branch != "" {
		req.Header.Set("X-Branch-ID", branch)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// ---- 商品写侧 ----

func TestCreateProduct_201ThenDuplicate409(t *testing.T) {
	r := newLocalTestRouter(t)

	w := doJSON(t, r, http.MethodPost, "/products", "10", map[string]any{
		"id": "6957583900828", "name": "凤派XL码抽纸8包", "barcode": "6957583900828", "unit": "提",
	})
	require.Equal(t, http.StatusCreated, w.Code, "body=%s", w.Body.String())

	// 同 (id, branch_id) 再建一次 → 409。
	w = doJSON(t, r, http.MethodPost, "/products", "10", map[string]any{
		"id": "6957583900828", "name": "重名",
	})
	require.Equal(t, http.StatusConflict, w.Code, "body=%s", w.Body.String())
	require.Equal(t, "product_already_exists", errorCode(t, w))
}

func TestCreateProduct_MissingBranch_400(t *testing.T) {
	r := newLocalTestRouter(t)

	w := doJSON(t, r, http.MethodPost, "/products", "", map[string]any{
		"id": "P1", "name": "X",
	})
	require.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	require.Equal(t, "branch_required", errorCode(t, w))
}

func TestCreateProduct_MissingName_400(t *testing.T) {
	r := newLocalTestRouter(t)

	w := doJSON(t, r, http.MethodPost, "/products", "10", map[string]any{"id": "P1"})
	require.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	require.Equal(t, "bad_request", errorCode(t, w))
}

func TestUpdateProduct_NotFound404(t *testing.T) {
	r := newLocalTestRouter(t)

	w := doJSON(t, r, http.MethodPut, "/products/NOPE", "10", map[string]any{"name": "X"})
	require.Equal(t, http.StatusNotFound, w.Code, "body=%s", w.Body.String())
	require.Equal(t, "product_not_found", errorCode(t, w))
}

func TestDeleteProduct_204Then404(t *testing.T) {
	r := newLocalTestRouter(t)

	w := doJSON(t, r, http.MethodPost, "/products", "10", map[string]any{"id": "P1", "name": "X"})
	require.Equal(t, http.StatusCreated, w.Code)

	w = doJSON(t, r, http.MethodDelete, "/products/P1", "10", nil)
	require.Equal(t, http.StatusNoContent, w.Code, "body=%s", w.Body.String())

	// 再删一次 → 404。
	w = doJSON(t, r, http.MethodDelete, "/products/P1", "10", nil)
	require.Equal(t, http.StatusNotFound, w.Code)
	require.Equal(t, "product_not_found", errorCode(t, w))
}

// ---- 分类写侧 ----

func TestCreateCategory_201ThenDuplicate409(t *testing.T) {
	r := newLocalTestRouter(t)

	w := doJSON(t, r, http.MethodPost, "/categories", "10", map[string]any{
		"id": "01", "name": "生鲜",
	})
	require.Equal(t, http.StatusCreated, w.Code, "body=%s", w.Body.String())

	w = doJSON(t, r, http.MethodPost, "/categories", "10", map[string]any{
		"id": "01", "name": "另一个生鲜",
	})
	require.Equal(t, http.StatusConflict, w.Code)
	require.Equal(t, "category_already_exists", errorCode(t, w))
}

func TestCreateCategory_SelfParent_400(t *testing.T) {
	r := newLocalTestRouter(t)

	w := doJSON(t, r, http.MethodPost, "/categories", "10", map[string]any{
		"id": "01", "name": "生鲜", "parent_id": "01",
	})
	require.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	require.Equal(t, "category_cycle", errorCode(t, w))
}

func TestUpdateCategory_Cycle_400(t *testing.T) {
	r := newLocalTestRouter(t)

	for _, c := range []map[string]any{
		{"id": "01", "name": "生鲜"},
		{"id": "0101", "name": "水果", "parent_id": "01"},
		{"id": "010101", "name": "进口水果", "parent_id": "0101"},
	} {
		w := doJSON(t, r, http.MethodPost, "/categories", "10", c)
		require.Equal(t, http.StatusCreated, w.Code, "body=%s", w.Body.String())
	}

	// 把「生鲜」的父挂到它的孙子下 ⇒ 成环 ⇒ 400 category_cycle。
	w := doJSON(t, r, http.MethodPut, "/categories/01", "10", map[string]any{"parent_id": "010101"})
	require.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	require.Equal(t, "category_cycle", errorCode(t, w))
}

// ---- 路由优先级(与既有 picker 测试同款纪律) ----

// /products/local 必须命中 local 端点,**不能**被 /products/:id 吃掉。
//
// gin 的路由树里静态段优先于参数段 —— 但这条依赖 gin 版本,
// 必须实测(既有 TestSuppliersSummaryRouteWinsOverParam 是同一条纪律)。
//
// ⚠️ 如果这条挂了,症状是"GET /products/local 返回 404 product_not_found",
// 看起来像"数据没导入",实际是路由被吃掉了 —— 排查方向会完全跑偏。
func TestProductsLocalRouteWinsOverParam(t *testing.T) {
	r := newLocalTestRouter(t)

	w := doJSON(t, r, http.MethodPost, "/products", "10", map[string]any{"id": "P1", "name": "X"})
	require.Equal(t, http.StatusCreated, w.Code)

	w = do(t, r, "/products/local", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body struct {
		Products []map[string]any `json:"products"`
		Count    int              `json:"count"`
		Total    int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Products, 1, "命中了错误的 handler:%s", w.Body.String())
	require.Equal(t, 1, body.Total)
}

// /categories/local 同理:不能被 /categories/:id 吃掉。
func TestCategoriesLocalRouteWinsOverParam(t *testing.T) {
	r := newLocalTestRouter(t)

	w := doJSON(t, r, http.MethodPost, "/categories", "10", map[string]any{"id": "01", "name": "生鲜"})
	require.Equal(t, http.StatusCreated, w.Code)

	w = do(t, r, "/categories/local", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body struct {
		Categories []map[string]any `json:"categories"`
		Count      int              `json:"count"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Categories, 1, "命中了错误的 handler:%s", w.Body.String())
}

// 真正带 :id 的路径仍然按 id 处理(找不到 → 404 category_not_found)。
func TestCategoriesByIDRouteStillWorks(t *testing.T) {
	r := newLocalTestRouter(t)

	w := doJSON(t, r, http.MethodPost, "/categories", "10", map[string]any{"id": "01", "name": "生鲜"})
	require.Equal(t, http.StatusCreated, w.Code)

	w = do(t, r, "/categories/01", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	w = do(t, r, "/categories/NOPE", "10")
	require.Equal(t, http.StatusNotFound, w.Code)
	require.Equal(t, "category_not_found", errorCode(t, w))
}

// ---- 分页参数(离线商品库全量拉取的入口) ----

// TestProductsLocal_PageSizeOutOfRange_400 锁"page_size 越界必须 400"。
//
// ⚠️ 之前是"**静默截断** + **回显请求值**"——
//
//	客户端请求 page_size=500,服务端只给 200 条,响应里却写 page_size=500
//	⇒ 客户端以为这一页有 500 条 ⇒ **漏数据,且零报错**。
//
//	(客户端是分页拉全量:27565 商品 = 138 页 ⇒ 每一页算错都丢商品。)
func TestProductsLocal_PageSizeOutOfRange_400(t *testing.T) {
	r := newLocalTestRouter(t)

	// ⚠️ `page_size=0` **不在**这个列表里:0 与"不传"在 query 里无法区分
	//   ⇒ 一律当"不传"⇒ 用默认值(见下面的回显测试)。
	for _, ps := range []string{"501", "-1", "100000"} {
		w := do(t, r, "/products/local?page_size="+ps, "10")
		require.Equal(t, http.StatusBadRequest, w.Code,
			"page_size=%s 必须被拒,body=%s", ps, w.Body.String())
		require.Equal(t, "bad_request", errorCode(t, w))
	}
}

// TestProductsLocal_EchoesEffectivePaging 锁"回显**生效值**(归一化之后)"。
//
// ⚠️ 不传 page/page_size 时回显 0 ⇒ 客户端以为"第 0 页、每页 0 条" ⇒ 翻页逻辑错乱。
func TestProductsLocal_EchoesEffectivePaging(t *testing.T) {
	r := newLocalTestRouter(t)

	w := do(t, r, "/products/local", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body struct {
		Page     int `json:"page"`
		PageSize int `json:"page_size"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, 1, body.Page, "不传 page 必须是第 1 页")
	require.Equal(t, service.DefaultProductPageSize, body.PageSize,
		"不传 page_size 必须回显默认值")
}
