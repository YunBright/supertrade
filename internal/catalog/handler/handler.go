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
//	POST   /products                             新建商品(product:manage)—— 本地表
//	PUT    /products/:id                         改商品(product:manage)—— 本地表
//	DELETE /products/:id                         软删商品(product:manage)—— 本地表
//	GET    /products/local                        本地表商品列表(product:view)—— 分页
//	GET    /products/export                       离线商品库(product:view)—— 不分页,含换购/停用
//	GET    /categories?with_counts=               分类字典(supplier:view,商品选择控件)
//	GET    /categories/local                      本地表分类列表(product:view)
//	GET    /categories/:id                        查单个分类(product:view)
//	POST   /categories                            新建分类(product:manage)
//	PUT    /categories/:id                        改分类(product:manage)
//	DELETE /categories/:id                        软删分类(product:manage)
//
// 中间件:
//   - middleware.XBranchID() 全局挂(由 cmd/catalog/main.go::registerRoutes)
//   - 业务端点全部走 rbac.RequireScopeWithBranch(userd per-branch)
//   - 商品选择控件的 4 条端点额外走"严格单店"守门(见 catalog.go)
//
// 错误映射:
//   - model.ErrSupplierNotFound        → 404
//   - model.ErrSupplierAlreadyExists   → 409
//   - model.ErrProductNotFound         → 404
//   - model.ErrProductAlreadyExists    → 409
//   - model.ErrCategoryNotFound        → 404
//   - model.ErrCategoryAlreadyExists   → 409
//   - model.ErrCategoryCycle           → 400
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
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/catalog/model"
	"github.com/YunBright/supertrade/internal/catalog/service"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// Handler 是 catalog 服务的 HTTP handler 聚合。
type Handler struct {
	suppliers  *service.SupplierService
	products   *service.ProductService
	categories *service.CategoryService
	barcodes   *service.ProductBarcodeService
	archive    *service.ArchiveService
	catalog    *service.CatalogService
	users      *userinfo.Client // per-branch scope 守门用
	logger     *slog.Logger
}

// New 构造 Handler。
//
// cubeClient 可以为 nil(本地表已有完整数据时),searchProducts 在 cubeClient
// == nil 时返 503 cube_unavailable。
// users 用于业务端点 per-branch 守门;传 nil 时所有业务端点 503 userd_unavailable。
func New(suppliers *service.SupplierService, products *service.ProductService,
	categories *service.CategoryService, barcodes *service.ProductBarcodeService,
	archive *service.ArchiveService,
	cube cubeclient.Client, users *userinfo.Client, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		suppliers:  suppliers,
		products:   products,
		categories: categories,
		barcodes:   barcodes,
		archive:    archive,
		catalog:    service.NewCatalogService(cube, logger),
		users:      users,
		logger:     logger,
	}
}

// SetSourceResolver 装配数据源解析器(「这个门店配了哪个数据源」)。
//
// ⚠️ 生产**必须**装配(cmd/catalog/main.go 里装 cubesource.CachedResolver):
// 没装 ⇒ picker 的读端点一律走 cube ⇒ 自营店(门店 10)又变回
// 2026-10-21 那个症状(cube 404,而本地明明有 27565 个商品)。
func (h *Handler) SetSourceResolver(r service.SourceResolver) {
	h.catalog.SetSourceResolver(r)
	h.archive.SetSourceResolver(r)
}

// SetLocalDB 装配本域 PG(没配数据源的门店走本地时用)。
func (h *Handler) SetLocalDB(db *gorm.DB) {
	h.catalog.SetLocalDB(db)
}

// SetArchiveExporter 装配上游导出端点客户端(cube-router 的 GET /v1/export)。
//
// ⚠️ 没装 ⇒ 配了数据源的门店导出返 503(**不**回落本地)。
func (h *Handler) SetArchiveExporter(e service.ArchiveExporter) {
	h.archive.SetExporter(e)
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

	// 本地商品档案的写侧 + 本地列表。
	//
	// ⚠️ `product:manage` 这个 scope **在 auth 的 seed 里还不存在**
	// (现有的是 product:view / supplier:manage / inventory:* 等)。
	// ⇒ 这些端点在 auth 补上 seed 之前会对所有人返 403(fail-closed)。
	// 这是**已知状态**:auth 的 scope seed 属于 Phase 0(见
	// docs/ARCHITECTURE.md §7)。
	r.POST("/products",
		rbac.RequireScopeWithBranch("product:manage", branchFromHeader, resolver),
		h.createProduct)
	r.PUT("/products/:id",
		rbac.RequireScopeWithBranch("product:manage", branchFromHeader, resolver),
		h.updateProduct)
	r.DELETE("/products/:id",
		rbac.RequireScopeWithBranch("product:manage", branchFromHeader, resolver),
		h.deleteProduct)
	r.GET("/products/local",
		rbac.RequireScopeWithBranch("product:view", branchFromHeader, resolver),
		h.listLocalProducts)
	// 离线商品库(一次给全,不分页,含换购品/停用品)。
	//
	// ⚠️ 静态段 `/products/export` 与参数段 `/products/:id` **共存**:
	// gin 的路由树里静态段优先于参数段(与注册顺序无关)——
	// 这条依赖 gin 版本,必须有测试锁住
	// (见 handler/product_export_test.go 的 TestProductsExportRouteWinsOverParam)。
	r.GET("/products/export",
		rbac.RequireScopeWithBranch("product:view", branchFromHeader, resolver),
		h.exportLocalProducts)

	// 商品条码(1 商品 : N 条码)。
	//
	// ⚠️ `/products/by-barcode/:barcode` 与 `/products/:id` 的**静态段优先级**:
	// gin 的路由树里静态段优先于参数段(与注册顺序无关)——
	// 这条依赖 gin 版本,必须有测试锁住
	// (见 handler/product_test.go 的 TestProductsByBarcodeRouteWinsOverParam)。
	r.GET("/products/by-barcode/:code",
		rbac.RequireScopeWithBranch("product:view", branchFromHeader, resolver),
		h.getProductByBarcode)
	r.GET("/products/:id/barcodes",
		rbac.RequireScopeWithBranch("product:view", branchFromHeader, resolver),
		h.listProductBarcodes)
	r.POST("/products/:id/barcodes",
		rbac.RequireScopeWithBranch("product:manage", branchFromHeader, resolver),
		h.createProductBarcode)
	r.DELETE("/products/:id/barcodes/:barcode",
		rbac.RequireScopeWithBranch("product:manage", branchFromHeader, resolver),
		h.deleteProductBarcode)

	// 档案导入(实现见 archive.go)。
	//
	// ⚠️ 守门是 `product:manage` scope,**不是** admin 角色 ——
	// auth 建服务账号时"不分配任何角色(least privilege)",
	// 服务账号过不了 RequireRole("admin")。而这个端点就是给同步服务账号用的。
	//
	// ⚠️ 门店只从 X-Branch-ID header 取,body 里**不认**门店 ——
	// body 里带一份门店,A 店的登录态就能把数据写进 B 店。
	r.POST("/archive/import",
		rbac.RequireScopeWithBranch("product:manage", branchFromHeader, resolver),
		h.importArchive)
	// 档案导出(archivedto 契约;实现见 archive.go)。
	//
	// ⚠️ 守门是 `product:view` scope —— 导出是**读**操作,
	// 与既有的 `/products/export` 同一档。
	r.GET("/archive/export",
		rbac.RequireScopeWithBranch("product:view", branchFromHeader, resolver),
		h.exportArchive)

	// 分类(本地表)。
	//
	// ⚠️ 分类用 `product:view`/`product:manage` 而不是独立的 category:* ——
	// 分类是"商品档案"域的一部分,少一组 scope 少一轮 auth 改动。
	// `GET /categories`(cube)由 RegisterProductPickerRoutes 注册,不冲突。
	r.GET("/categories/local",
		rbac.RequireScopeWithBranch("product:view", branchFromHeader, resolver),
		h.listLocalCategories)
	r.GET("/categories/:id",
		rbac.RequireScopeWithBranch("product:view", branchFromHeader, resolver),
		h.getCategory)
	r.POST("/categories",
		rbac.RequireScopeWithBranch("product:manage", branchFromHeader, resolver),
		h.createCategory)
	r.PUT("/categories/:id",
		rbac.RequireScopeWithBranch("product:manage", branchFromHeader, resolver),
		h.updateCategory)
	r.DELETE("/categories/:id",
		rbac.RequireScopeWithBranch("product:manage", branchFromHeader, resolver),
		h.deleteCategory)

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
//
// ⚠️ **code 是按资源分的,不是按 HTTP 状态统一**。
// 已有的 `supplier_not_found` / `supplier_already_exists` 是**线上契约**
// (Flutter 端与契约测试都按这两个字符串判定),**不能**改成通用的
// `not_found` / `already_exists` —— 那是静默的契约破坏。
// ⇒ 新增资源(product / category)也各用各的 code,保持同一套风格。
func (h *Handler) mapErr(c *gin.Context, err error) {
	// ⚠️ 「数据源」相关的错误**先**判:它们的 code 与会员域同名同义
	// (见 writeSourceErr 的注释)。
	if h.writeSourceErr(c, err) {
		return
	}
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
	case errors.Is(err, model.ErrProductNotFound):
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"code":    "product_not_found",
			"message": err.Error(),
		})
	case errors.Is(err, model.ErrProductAlreadyExists):
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"code":    "product_already_exists",
			"message": err.Error(),
		})
	case errors.Is(err, model.ErrCategoryNotFound):
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"code":    "category_not_found",
			"message": err.Error(),
		})
	case errors.Is(err, model.ErrBarcodeNotFound):
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"code":    "barcode_not_found",
			"message": err.Error(),
		})
	case errors.Is(err, model.ErrCategoryAlreadyExists):
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"code":    "category_already_exists",
			"message": err.Error(),
		})
	case errors.Is(err, model.ErrCategoryCycle):
		// 成环是**数据问题**(不是权限、不是参数格式)⇒ 400 而不是 500。
		// 挂错父节点会挡住分类树建树(无限递归),必须当场拒掉。
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "category_cycle",
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
