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

	// 与生产同款的路径集合。注意 :id 先注册、/export 后注册,
	// **注册顺序反过来也要成立**(见下面的路由优先级测试)。
	//
	// ⚠️ 旧的 /products/local、/categories/local 已删除(2026-10-22):
	// 名字承诺"只读本地表",而全域判据是"配没配数据源"
	// ⇒ 见 TestDeletedLocalEndpoints_AreGone(锁住"别把它们加回来")。
	r.GET("/products/:id", h.getProduct)
	r.POST("/products", h.createProduct)
	r.PUT("/products/:id", h.updateProduct)
	r.DELETE("/products/:id", h.deleteProduct)
	r.GET("/products/export", h.exportLocalProducts)
	r.GET("/products/by-barcode/:code", h.getProductByBarcode)
	r.GET("/products/:id/barcodes", h.listProductBarcodes)
	r.POST("/products/:id/barcodes", h.createProductBarcode)
	r.DELETE("/products/:id/barcodes/:barcode", h.deleteProductBarcode)

	r.GET("/categories/:id", h.getCategory)
	r.POST("/categories", h.createCategory)
	r.PUT("/categories/:id", h.updateCategory)
	r.DELETE("/categories/:id", h.deleteCategory)

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

// ---- 旧端点已删除(2026-10-22) ----

// TestDeletedLocalEndpoints_AreGone 锁住「/products/local、/categories/local 已删除」。
//
// # 为什么要有这条测试
//
// 这两条端点的名字承诺"只读本地表",而全域判据是**配没配数据源**
// (配了 ⇒ 走 cube,没配 ⇒ 走本域 PG)
// ⇒ 名字与语义两套话:同一个门店在后台管理页与 POS 收银页会看到**两个答案**
// (门店 00 的本地副本已被清空 ⇒ 一边是全的、一边是空的)。
//
// ⇒ 删除后**必须**404,不能"悄悄恢复"。
// 恢复的诱惑很大:代码里已经有一份能跑的实现,加回一行路由即可。
//
// # 为什么会落到 503 / 404 而不是"路由不存在"
//
// `/products/local` 现在落到参数段 `/products/:id`(id="local")
// ⇒ getProduct 按数据源分流:没配源的门店查本域 ⇒ 404 product_not_found;
// 配了源的门店打 cube ⇒ 单测里 cube 未装配 ⇒ 503。
// `/categories/local` 同理落到 `/categories/:id` ⇒ 404 category_not_found。
//
// ⇒ 断言**不看状态码**(它随门店/装配变),只断言两件事:
//
//	① 路由表里**没有**这两条路径(精确);
//	② 请求这两个路径**拿不到本地表的商品**(行为)。
func TestDeletedLocalEndpoints_AreGone(t *testing.T) {
	r := newLocalTestRouter(t)

	// ① 路由表里没有这两条路径。
	registered := map[string]bool{}
	for _, route := range r.Routes() {
		registered[route.Path] = true
	}
	for _, path := range []string{"/products/local", "/categories/local"} {
		require.False(t, registered[path], "%s 必须从路由表里消失", path)
	}

	// ② 就算请求它,也拿不到本地表的商品。
	w := doJSON(t, r, http.MethodPost, "/products", "10", map[string]any{"id": "P1", "name": "本地商品X"})
	require.Equal(t, http.StatusCreated, w.Code)

	for _, path := range []string{"/products/local", "/categories/local"} {
		w = do(t, r, path, "10")
		require.NotEqual(t, http.StatusOK, w.Code,
			"%s 必须已删除(不能返回本地表数据),body=%s", path, w.Body.String())
		require.NotContains(t, w.Body.String(), "本地商品X",
			"%s 不得返回本地表的商品", path)
	}
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

// ---- 分页参数 ----
//
// ⚠️ "page_size 越界必须 400(不是静默截断 + 回显请求值)"这条守卫
// **没有随旧端点一起消失** —— 它现在挂在商品选择控件那两条端点上
// (`GET /products` / `GET /products/search`,见 catalog.go 的 parsePageQuery):
//
//	TestParsePageQuery_RejectsInvalidPagination  越界 → 400 bad_request
//	TestListProducts_EchoesPagination            回显**生效值**(归一化之后)
//
// ⚠️ 为什么这条守卫要紧:客户端是**分页拉全量**(27565 商品 = 138 页)
// ⇒ 每一页算错都**漏商品**,而且**零报错**。
// 之前就是"静默截断 + 回显请求值":客户端请求 page_size=500、服务端只给 200 条,
// 响应里却写 page_size=500 ⇒ 客户端以为这页有 500 条。
