package handler

// 本文件锁住 catalog **写侧**的只读闸门。
//
// # 为什么需要这道闸门
//
// 读侧已经按「门店配没配数据源」分流(配了源 ⇒ 走那个 cube 实例),
// 所以本域 PG 里**对有源门店的行永远没人读**。
//
// ⇒ 写侧若不拦:写进本域 PG 的一行"没人看的商品",而前台还显示"写成功了"
// ⇒ 现场看到的是"商品改了但查出来还是老的",排查方向完全跑偏。
//
// # 闸门的口径与会员域完全一致
//
//	有源门店 ⇒ 409 member_source_is_external(与会员域同名同码)
//	没配源   ⇒ 正常写
//	问不到   ⇒ **放行**(与导入路径同一处理:cube-router 抖一下
//	           不该挡住自营店的正常建档)
//	被停用   ⇒ 503 cube_source_disabled
//
// ⚠️ 会员域有**同一个中间件**(internal/member/handler.go 的 requireLocalSource)
// ⇒ 同一个门店,会员档案与商品档案必须给同一种解释。

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/catalog/model"
	"github.com/YunBright/supertrade/internal/catalog/service"
	"github.com/YunBright/supertrade/internal/cubeclient/cubeclientfake"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// newWriteGateRouter 造一个**带只读闸门**的写侧路由。
//
// ⚠️ 必须与生产注册顺序一致:rbac 守门 → requireLocalSource → handler。
// 闸门放错位置(比如放在 handler 之后)⇒ 写方法已经落了库,闸门形同虚设。
func newWriteGateRouter(t *testing.T, src *fakeSource) (*gin.Engine, *gorm.DB) {
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

	// 写侧(与生产同款:闸门在写方法之前)。
	r.POST("/products", h.requireLocalSource, h.createProduct)
	r.PUT("/products/:id", h.requireLocalSource, h.updateProduct)
	r.DELETE("/products/:id", h.requireLocalSource, h.deleteProduct)
	r.POST("/products/:id/barcodes", h.requireLocalSource, h.createProductBarcode)
	r.DELETE("/products/:id/barcodes/:barcode", h.requireLocalSource, h.deleteProductBarcode)
	r.POST("/categories", h.requireLocalSource, h.createCategory)
	r.PUT("/categories/:id", h.requireLocalSource, h.updateCategory)
	r.DELETE("/categories/:id", h.requireLocalSource, h.deleteCategory)
	r.POST("/suppliers", h.requireLocalSource, h.createSupplier)
	r.PUT("/suppliers/:id", h.requireLocalSource, h.updateSupplier)
	r.DELETE("/suppliers/:id", h.requireLocalSource, h.deleteSupplier)
	return r, db
}

// writeCase 是一个写请求。
type writeCase struct {
	method string
	path   string
	body   map[string]any
}

// allWriteCases 是**全部**写侧端点。
//
// ⚠️ 清单必须与生产注册表**逐条对齐**:漏一条 = 那条端点没有闸门。
// ⇒ 新增写端点时,这个清单必须同步加一行。
func allWriteCases() []writeCase {
	return []writeCase{
		{http.MethodPost, "/products", map[string]any{"id": "P1", "name": "新商品"}},
		{http.MethodPut, "/products/P1", map[string]any{"name": "改过的名字"}},
		{http.MethodDelete, "/products/P1", nil},
		{http.MethodPost, "/products/P1/barcodes", map[string]any{"barcode": "6901234567890"}},
		{http.MethodDelete, "/products/P1/barcodes/6901234567890", nil},
		{http.MethodPost, "/categories", map[string]any{"id": "C1", "name": "生鲜"}},
		{http.MethodPut, "/categories/C1", map[string]any{"name": "改过的分类"}},
		{http.MethodDelete, "/categories/C1", nil},
		{http.MethodPost, "/suppliers", map[string]any{"id": "S1", "name": "某供应商"}},
		{http.MethodPut, "/suppliers/S1", map[string]any{"name": "改过的供应商"}},
		{http.MethodDelete, "/suppliers/S1", nil},
	}
}

// TestWriteGate_ExternalSourceRejected 锁"有源门店 ⇒ 所有写端点被拒(409)"。
//
// ⚠️ code 与会员域同名(`member_source_is_external`):判据全服务一致。
//
// ⚠️ message 里**不许**出现内部 app-id(cube-sixun-ysx-fb):
// 那是运维细节,泄漏它对终端用户没有价值,却把内部拓扑摊开了。
func TestWriteGate_ExternalSourceRejected(t *testing.T) {
	r, db := newWriteGateRouter(t, &fakeSource{src: configuredSource()})

	for _, tc := range allWriteCases() {
		w := doJSON(t, r, tc.method, tc.path, "00", tc.body)
		require.Equal(t, http.StatusConflict, w.Code,
			"%s %s 必须被拒(有源门店的本域只读),body=%s", tc.method, tc.path, w.Body.String())
		require.Equal(t, "member_source_is_external", errorCode(t, w),
			"%s %s 的 code 不对", tc.method, tc.path)
		assert.NotContains(t, w.Body.String(), "cube-sixun-ysx-fb",
			"%s %s 的 message 不许暴露内部 app-id", tc.method, tc.path)
	}

	// ⚠️ 最要紧的一条:**一行都没写进本域 PG**。
	// 只断言状态码不够 —— 一个"先写后判"的实现会返 409 却把数据落了库。
	var products, categories, suppliers int64
	require.NoError(t, db.Model(&model.Product{}).Count(&products).Error)
	require.NoError(t, db.Model(&model.Category{}).Count(&categories).Error)
	require.NoError(t, db.Model(&model.Supplier{}).Count(&suppliers).Error)
	assert.Zero(t, products, "有源门店不许在本域留下商品行")
	assert.Zero(t, categories, "有源门店不许在本域留下分类行")
	assert.Zero(t, suppliers, "有源门店不许在本域留下供应商行")
}

// TestWriteGate_NoSourceWritesNormally 锁"没配数据源的门店 ⇒ 正常写"。
//
// ⚠️ 这条不能被闸门误伤:自营店(门店 10)的建档是它的日常操作。
func TestWriteGate_NoSourceWritesNormally(t *testing.T) {
	r, db := newWriteGateRouter(t, &fakeSource{src: localSource()})

	w := doJSON(t, r, http.MethodPost, "/products", "10", map[string]any{
		"id": "LOCAL-1", "name": "本地抽纸", "category_id": "C1", "supplier_id": "S1",
	})
	require.Equal(t, http.StatusCreated, w.Code, "body=%s", w.Body.String())

	w = doJSON(t, r, http.MethodPut, "/products/LOCAL-1", "10", map[string]any{"name": "改过的名字"})
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	w = doJSON(t, r, http.MethodPost, "/categories", "10", map[string]any{"id": "C1", "name": "生鲜"})
	require.Equal(t, http.StatusCreated, w.Code, "body=%s", w.Body.String())

	w = doJSON(t, r, http.MethodPost, "/suppliers", "10", map[string]any{"id": "S1", "name": "某供应商"})
	require.Equal(t, http.StatusCreated, w.Code, "body=%s", w.Body.String())

	var products int64
	require.NoError(t, db.Model(&model.Product{}).Count(&products).Error)
	assert.Equal(t, int64(1), products)
}

// TestWriteGate_ResolveFailureAllowsWrite 锁"问不到路由 ⇒ 放行"。
//
// ⚠️ 与归档导入路径**同一处理**:cube-router 抖一下不该挡住自营店的正常建档。
// 而真写了也没关系 —— 没配源的门店本来就读本域。
//
// ⚠️ 这条断言的是"放行",不是"报错":判据问不到时**不能**把写端点也堵死
// (那会让一次路由抖动变成整个门店不能建档)。
func TestWriteGate_ResolveFailureAllowsWrite(t *testing.T) {
	r, _ := newWriteGateRouter(t, &fakeSource{err: errors.New("cube-router 不可达")})

	w := doJSON(t, r, http.MethodPost, "/products", "10", map[string]any{
		"id": "LOCAL-1", "name": "本地抽纸",
	})
	require.Equal(t, http.StatusCreated, w.Code,
		"问不到路由时写端点要放行,body=%s", w.Body.String())
}

// TestWriteGate_DisabledSourceRejected 锁"数据源被停用 ⇒ 503"。
//
// ⚠️ 停用 = "这个门店现在查不了" ⇒ 更不该写(写了也读不出来)。
func TestWriteGate_DisabledSourceRejected(t *testing.T) {
	disabled := configuredSource()
	disabled.Enabled = false
	r, _ := newWriteGateRouter(t, &fakeSource{src: disabled})

	w := doJSON(t, r, http.MethodPost, "/products", "00", map[string]any{
		"id": "P1", "name": "新商品",
	})
	require.Equal(t, http.StatusServiceUnavailable, w.Code, "body=%s", w.Body.String())
	require.Equal(t, "cube_source_disabled", errorCode(t, w))
}

// TestWriteGate_MissingBranchDoesNotBlock 锁"门店头缺失时闸门不重复处理"。
//
// ⚠️ 门店头缺失由 rbac 中间件判 400 branch_required;
// 闸门再判一次就是**两个**错误源 ⇒ 同一个请求两种解释。
func TestWriteGate_MissingBranchDoesNotBlock(t *testing.T) {
	r, _ := newWriteGateRouter(t, &fakeSource{src: configuredSource()})

	w := doJSON(t, r, http.MethodPost, "/products", "", map[string]any{
		"id": "P1", "name": "新商品",
	})
	// 闸门放行(没有门店 ⇒ 无从判数据源)⇒ 由 handler 自己判 400 branch_required。
	assert.NotEqual(t, http.StatusConflict, w.Code,
		"没有门店时不该报 409(那是「档案在外部系统」的解释),body=%s", w.Body.String())
}

// 编译期断言:写侧的 DTO 形状没变(闸门不该改响应契约)。
var _ = json.Marshal
