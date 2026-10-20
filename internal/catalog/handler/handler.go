// Package handler 实现 catalog 服务的 HTTP handler。
//
// 路由(nginx 已剥过 /api/v1/catalog/):
//
//	GET    /suppliers?q=&limit=                  供应商列表(supplier:view,per branch)
//	GET    /suppliers/summary                     供应商摘要(supplier:view,商品选择控件)
//	GET    /suppliers/:id                        查单个供应商(supplier:view)
//	POST   /suppliers                            新增供应商(supplier:manage)
//	PUT    /suppliers/:id                        改供应商(supplier:manage)
//	DELETE /suppliers/:id                        软删供应商(supplier:manage)
//	GET    /products?category_id=&page=           商品列表(product:view,商品选择控件)
//	GET    /products/search?barcode=             按条码查商品(product:view,老路径)
//	GET    /products/search?q=&page=             关键字搜商品(product:view,商品选择控件)
//	GET    /products/:id                         查单个商品(product:view)
//	GET    /categories?with_counts=              分类字典(supplier:view,商品选择控件)
//
// 中间件:
//   - middleware.XBranchID() 全局挂(由 cmd/catalog/main.go::registerRoutes)
//   - 业务端点全部走 rbac.RequireScopeWithBranch(userd per-branch)
//   - 商品选择控件的 4 条端点额外走"严格单店"守门(见 catalog.go)
//
// 错误映射:
//   - model.ErrSupplierNotFound        → 404
//   - model.ErrSupplierAlreadyExists   → 409
//   - model.ErrInvalidInput            → 400
//   - 其它                              → 500
package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/YunBright/authkit/claims"
	"github.com/YunBright/authkit/rbac"
	"github.com/YunBright/authkit/userinfo"
	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/catalog/model"
	"github.com/YunBright/supertrade/internal/catalog/service"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// Handler 是 catalog 服务的 HTTP handler 聚合。
type Handler struct {
	suppliers *service.SupplierService
	products  *service.ProductService
	catalog   *service.CatalogService
	cube      cubeclient.Client // 兜底,本地表无数据时转发 cube
	users     *userinfo.Client  // per-branch scope 守门用
	logger    *slog.Logger
}

// New 构造 Handler。
//
// cubeClient 可以为 nil(本地表已有完整数据时),searchProducts 在 cubeClient
// == nil 时返 503 cube_unavailable。
// users 用于业务端点 per-branch 守门;传 nil 时所有业务端点 503 userd_unavailable。
func New(suppliers *service.SupplierService, products *service.ProductService, cube cubeclient.Client, users *userinfo.Client, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		suppliers: suppliers,
		products:  products,
		catalog:   service.NewCatalogService(cube, logger),
		cube:      cube,
		users:     users,
		logger:    logger,
	}
}

// branchFromHeader 拿当前请求的 X-Branch-ID header(单店场景)。
//
// 多店 header(`*` / `01,02`)时只取第一项;catalog 端点是单店 CRUD,多店场景
// 由 stocktake / inventory 等聚合端点自己处理,不在此 helper 解决。
//
// ""(header 未传)→ 返 ("", false) → RequireScopeWithBranch 按 400 branch_required 拒。
func branchFromHeader(c *gin.Context) (string, bool) {
	b := middleware.SingleBranchFromCtx(c)
	if b == "" {
		return "", false
	}
	return b, true
}

// RegisterRoutes 把所有路由挂到 r(nginx 已剥过 /api/v1/catalog/)。
//
// 每个业务端点都挂 rbac.RequireScopeWithBranch(scope, branchFromHeader, resolver);
// X-Branch-ID 由 cmd/catalog/main.go 全局 middleware.XBranchID() 注入。
func (h *Handler) RegisterRoutes(r gin.IRouter) {
	resolver := rbac.HasAnyScopeWithBranch(h.users)

	// 供应商
	r.GET("/suppliers",
		rbac.RequireScopeWithBranch("supplier:view", branchFromHeader, resolver),
		h.listSuppliers)
	r.GET("/suppliers/:id",
		rbac.RequireScopeWithBranch("supplier:view", branchFromHeader, resolver),
		h.getSupplier)
	r.POST("/suppliers",
		rbac.RequireScopeWithBranch("supplier:manage", branchFromHeader, resolver),
		h.createSupplier)
	r.PUT("/suppliers/:id",
		rbac.RequireScopeWithBranch("supplier:manage", branchFromHeader, resolver),
		h.updateSupplier)
	r.DELETE("/suppliers/:id",
		rbac.RequireScopeWithBranch("supplier:manage", branchFromHeader, resolver),
		h.deleteSupplier)

	// 商品
	//
	// ⚠️ `/products/search` **不在这里**注册:它同时承载老路径(?barcode=)与
	// 新路径(?q=,商品选择控件),由 RegisterProductPickerRoutes 统一注册 ——
	// 同一条路径注册两次 gin 会直接 panic("handlers are already registered")。
	r.GET("/products/:id",
		rbac.RequireScopeWithBranch("product:view", branchFromHeader, resolver),
		h.getProduct)

	// 商品选择控件(ProductPicker)的 4 条端点。
	//
	// 它们的守门链比上面多一条"严格单店":picker 一次只服务一家门店,
	// 通配 / 多店必须 400(而不是像 SingleBranchFromCtx 那样静默取第一项)。
	h.RegisterProductPickerRoutes(r, func(scope string) []gin.HandlerFunc {
		return []gin.HandlerFunc{
			h.requireSingleBranch(),
			rbac.RequireScopeWithBranch(scope, branchFromHeader, resolver),
		}
	})
}

// RegisterProductPickerRoutes 挂商品选择控件的 4 条路由:
//
//	GET /products          商品列表
//	GET /products/search   关键字搜索(与老路径 ?barcode= 共用 URL)
//	GET /categories        分类字典
//	GET /suppliers/summary 供应商摘要
//
// guard 由调用方给(生产是"严格单店 + scope 守门"两条链)。
//
// ⚠️ `/suppliers/summary` 与 `/suppliers/:id` **必须并存**:只注册 :id 时,
// `/suppliers/summary` 会被 :id 吃掉并返 404 supplier_not_found(实测)。
// gin 的路由树里静态段优先于参数段(与注册顺序无关),但这条依赖 gin 版本 ——
// 必须实测,不能假设(见 handler_test.go 的 TestSuppliersSummaryRouteWinsOverParam)。
//
// 单独暴露是为了让**路由本身**能在没有 userd 的情况下被验证 ——
// "/suppliers/summary 与 /suppliers/:id 谁先命中"是纯路由问题,
// 不该被权限依赖挡住(见 handler_test.go)。
func (h *Handler) RegisterProductPickerRoutes(r gin.IRouter, guard func(scope string) []gin.HandlerFunc) {
	r.GET("/products",
		append(guard("product:view"), h.listProducts)...)
	r.GET("/products/search",
		append(guard("product:view"), h.searchProducts)...)
	r.GET("/categories",
		append(guard("supplier:view"), h.listCategories)...)
	r.GET("/suppliers/summary",
		append(guard("supplier:view"), h.listSuppliersSummary)...)
}

// ---- helpers ----

// callerSub 拿当前 caller 的 sub(从 JWT claims)。
//
// admin 端点已被 rbac.RequireRole("admin") 守门(后续 PR 3),claims 必然存在;
// 拿不到时兜底 "system"。
func callerSub(c *gin.Context) string {
	cl, _ := claims.FromContext(c.Request.Context())
	if cl == nil || cl.Sub == "" {
		return "system"
	}
	return cl.Sub
}

// mapErr 把 service / model 错误映射为 HTTP 状态码 + JSON body。
func (h *Handler) mapErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, model.ErrSupplierNotFound):
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"code":    "supplier_not_found",
			"message": err.Error(),
		})
	case errors.Is(err, model.ErrSupplierAlreadyExists):
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"code":    "supplier_already_exists",
			"message": err.Error(),
		})
	case errors.Is(err, model.ErrInvalidInput):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "bad_request",
			"message": err.Error(),
		})
	default:
		h.logger.Error("catalog handler error", "err", err, "path", c.Request.URL.Path)
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
			"code":    "internal_error",
			"message": err.Error(),
		})
	}
}
