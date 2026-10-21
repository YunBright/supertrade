// Package handler / catalog.go —— 商品选择控件(ProductPicker)的 HTTP handler。
//
// # 端点
//
//	GET /products?category_id=&supplier_id=&page=&page_size=   商品列表
//	GET /products/search?q=&...&page=&page_size=               关键字搜索
//	GET /products/search?barcode=                              老路径(兼容)
//	GET /categories?with_counts=                               分类字典
//	GET /suppliers/summary                                     供应商摘要
//
// # 门店口径:必须**单一具体门店**
//
// picker 一次只服务一家门店("这家店在卖什么"),所以 X-Branch-ID 必须是一个
// 具体门店:
//
//	缺失 / 空    → 400 branch_required
//	"*"          → 400 invalid_branch_scope
//	"00,01"      → 400 invalid_branch_scope
//
// ⚠️ 不能用 middleware.SingleBranchFromCtx 做这个校验:它对 "*" 返回 "*" 本身、
// 对 "00,01" 返回**第一项** "00"。用它做严格单店校验,多店请求会**静默按 00 查**
// —— 放行了不该放行的请求,而调用方以为拿到的是两家店的并集。
//
// # 老路径(?barcode=)不受严格单店约束
//
// `GET /products/search?barcode=` 是**另一个端点**(Flutter 的 POP 价签流 /
// 老 catalog 仓库在用),它历史上就接受多店 / 通配(Flutter 的 BranchInterceptor
// 在没选门店时会发 "*")。给它加上严格单店会打断线上流程,所以这里保持原样。
package handler

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/catalog/model"
	"github.com/YunBright/supertrade/internal/catalog/service"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// ctxCatalogBranch 是 gin.Context 里存"已解析的单店 branch"的 key。
const ctxCatalogBranch = "catalog.branch_id"

// singleBranchOf 从 ctx 取**单一具体门店**。
//
// 返回 (branchID, "") = 合法;
// ( "", "branch_required")      = 没传 / 传空 X-Branch-ID;
// ( "", "invalid_branch_scope") = 通配("*")或多店("00,01")。
//
// # 为什么不能直接用 middleware.SingleBranchFromCtx
//
// 它对 "*" 返回 "*" 本身、对 "00,01" 返回**第一项** "00"。
// 用它做严格单店校验,多店请求会**静默按 00 查** —— 放行了不该放行的请求,
// 而调用方以为拿到的是两家店的并集。
func singleBranchOf(c *gin.Context) (string, string) {
	bs := middleware.BranchFromCtx(c)
	if len(bs) == 0 {
		return "", "branch_required"
	}
	if len(bs) != 1 {
		return "", "invalid_branch_scope"
	}
	if bs[0] == "*" {
		return "", "invalid_branch_scope"
	}
	return bs[0], ""
}

// requireSingleBranch 强制"单一具体门店"。
//
// # 为什么必须排在 rbac 守门**之前**
//
// rbac.RequireScopeWithBranch 拿 branch 去 userd 查 per-branch 权限矩阵:
// "*" 这种值在 userd 里查不到行 → 403 forbidden。于是本该是
// 400 invalid_branch_scope 的请求被报成"没权限",排查方向完全跑偏。
//
// # 为什么它还要把门店写回 request.Context
//
// 真正决定"路由到哪家门店的 cube"的是 outgoing metadata 里的 x-branch-id,
// 而它来自 cmd/catalog/main.go 的 forwardBearerToOutgoing —— 那里写的是
// **header 原值**。于是 " 00 "(带空格)这种 header 会被解析成 "00" 却把
// " 00 " 发给 cube-router,查不到源 → 404。
//
// 这里把**已校验过**的门店写回 request.Context,保证"我们校验的门店"与
// "cube 收到的门店"永远是同一个值。
func (h *Handler) requireSingleBranch() gin.HandlerFunc {
	return func(c *gin.Context) {
		if isLegacyBarcodeRequest(c) {
			// 老路径保持原样(见包注释)。
			return
		}
		branchID, code := singleBranchOf(c)
		if code != "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"code":    code,
				"message": branchScopeMessage(code),
			})
			return
		}
		c.Set(ctxCatalogBranch, branchID)
		c.Request = c.Request.WithContext(cubeclient.WithBranchID(c.Request.Context(), branchID))
	}
}

// branchScopeMessage 给两种 400 一句能直接发给用户看的中文说明。
func branchScopeMessage(code string) string {
	if code == "branch_required" {
		return "X-Branch-ID header 必填,且必须是单个具体门店"
	}
	return "X-Branch-ID 必须是单个具体门店(不接受 * 通配或多店)"
}

// isLegacyBarcodeRequest 判断这次请求走的是老的 ?barcode= 路径。
func isLegacyBarcodeRequest(c *gin.Context) bool {
	barcode, ok := c.GetQuery("barcode")
	return ok && barcode != ""
}

// listProducts GET /products?category_id=&supplier_id=&page=&page_size=
func (h *Handler) listProducts(c *gin.Context) {
	q, ok := parsePageQuery(c, service.DefaultPageSize)
	if !ok {
		return
	}
	q.CategoryID = c.Query("category_id")
	q.SupplierID = c.Query("supplier_id")
	q.SupplierCategoryID = c.Query("supplier_category_id")

	page, err := h.catalog.ListProducts(c.Request.Context(), q)
	if err != nil {
		h.mapCatalogErr(c, err)
		return
	}
	c.JSON(http.StatusOK, page)
}

// searchProducts GET /products/search
//
// 两条路径共用一个 URL:
//
//   - ?barcode=X → **老路径**:保留 product_id / product_name / category /
//     stock_qty / avg_cost_yuan,并叠加新字段(超集)。
//   - ?q=... / 其它 → **新路径**:商品选择控件用,返回
//     {products,count,page,page_size,total}。
//
// ⚠️ 分派依据是"barcode 有没有值",不是"barcode 参数在不在":
// 新契约里 picker 会发 `?q=`(可能为空),若按"参数在不在"分派,空 q 会被
// 误判成老路径并返 400 missing_barcode —— 那是"空关键词 + 全量浏览"的主场景。
func (h *Handler) searchProducts(c *gin.Context) {
	barcode, hasBarcode := c.GetQuery("barcode")
	keyword, hasKeyword := c.GetQuery("q")
	switch {
	case barcode != "":
		h.searchProductsByBarcode(c, barcode)
	case hasKeyword:
		h.searchProductsByKeyword(c, keyword)
	case hasBarcode:
		// 显式传了空 barcode:保持老行为(400 missing_barcode)。
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "missing_barcode",
			"message": "barcode query 必填",
		})
	default:
		h.searchProductsByKeyword(c, "")
	}
}

// searchProductsByKeyword 新路径:关键字搜索(q 为空 = 全量浏览)。
func (h *Handler) searchProductsByKeyword(c *gin.Context, keyword string) {
	q, ok := parsePageQuery(c, service.DefaultSearchPageSize)
	if !ok {
		return
	}
	q.Keyword = keyword
	q.CategoryID = c.Query("category_id")
	q.SupplierID = c.Query("supplier_id")
	q.SupplierCategoryID = c.Query("supplier_category_id")

	page, err := h.catalog.SearchProducts(c.Request.Context(), q)
	if err != nil {
		h.mapCatalogErr(c, err)
		return
	}
	c.JSON(http.StatusOK, page)
}

// searchProductsByBarcode 老路径:按条码查商品(+ 一次库存查询)。
//
// ⚠️ 门店口径保持老行为(SingleBranchFromCtx):这条路径历史上就接受
// 多店 / 通配,改成严格单店会打断 Flutter 的 POP 价签流。
func (h *Handler) searchProductsByBarcode(c *gin.Context, barcode string) {
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "branch_required",
			"message": "X-Branch-ID header 必填",
		})
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	if limit <= 0 {
		limit = 10
	}
	page, err := h.catalog.SearchProductsByBarcode(c.Request.Context(), barcode, branchID, limit)
	if err != nil {
		h.mapCatalogErr(c, err)
		return
	}
	c.JSON(http.StatusOK, page)
}

// listCategories GET /categories?with_counts=
func (h *Handler) listCategories(c *gin.Context) {
	withCounts := c.Query("with_counts") == "true"
	out, err := h.catalog.ListCategories(c.Request.Context(), withCounts)
	if err != nil {
		h.mapCatalogErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// listSuppliersSummary GET /suppliers/summary
//
// ⚠️ 这条路由必须与 `/suppliers/:id` **并存**:只注册 :id 时,
// `/suppliers/summary` 会被 :id 吃掉并返 404 supplier_not_found(实测)。
// gin 的路由树里静态段优先于参数段 —— 但这条依赖 gin 版本的行为**必须实测**,
// 不能假设(见 handler_test.go 的 TestSuppliersSummaryRouteWinsOverParam)。
func (h *Handler) listSuppliersSummary(c *gin.Context) {
	out, err := h.catalog.ListSuppliers(c.Request.Context())
	if err != nil {
		h.mapCatalogErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// parsePageQuery 解析并校验分页参数;不合法时直接写 400 并返回 ok=false。
//
// 校验口径(商品选择控件契约):
//
//	page < 1        → 400 bad_request
//	page_size < 1   → 400 bad_request
//	page_size > 200 → 400 bad_request
//
// ⚠️ 上限 200 不是随便定的:page_size 直接落到 cube 的 LIMIT,一个越界值就能
// 让 cube 把 27k 行商品全拉进内存。
func parsePageQuery(c *gin.Context, defPageSize int) (service.ProductListQuery, bool) {
	out := service.ProductListQuery{Page: 1, PageSize: defPageSize}
	if raw := c.Query("page"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			badRequest(c, "page 必须是 >= 1 的整数")
			return service.ProductListQuery{}, false
		}
		out.Page = n
	}
	rawSize := c.Query("page_size")
	if rawSize == "" {
		// 兼容老客户端的 limit= 参数(Flutter 的老 catalog 仓库用它拉列表)。
		// 它不是契约参数,所以**夹到**区间而不是报错 —— 报错会让一个只想
		// "多给点"的老客户端整个用不了。
		if raw := c.Query("limit"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil {
				if n < 1 {
					n = 1
				}
				if n > service.MaxPageSize {
					n = service.MaxPageSize
				}
				out.PageSize = n
			}
		}
		return out, true
	}
	n, err := strconv.Atoi(rawSize)
	if err != nil || n < 1 || n > service.MaxPageSize {
		badRequest(c, "page_size 必须是 1.."+strconv.Itoa(service.MaxPageSize)+" 的整数")
		return service.ProductListQuery{}, false
	}
	out.PageSize = n
	return out, true
}

// badRequest 写统一形态的 400。
func badRequest(c *gin.Context, message string) {
	c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
		"code":    "bad_request",
		"message": message,
	})
}

// mapCatalogErr 把 catalog 查询错误映到 HTTP。
//
//	cube client 未配置 / 问不到数据源路由 → 503 cube_unavailable
//	数据源被停用                        → 503 cube_source_disabled
//	调用者缺该门店读权限                → 403 forbidden
//	登录态失效                          → 401 unauthorized
//	其余(cube 故障 / 查询非法)          → 502 cube_error
//
// ⚠️ 一律**不**把 cube 的 404 说成"没有数据":404 在这条链路上更常见的原因是
// "该门店没配 cube 源"(cube-router 返 cube_source_not_configured),
// 说成"空列表"会把部署缺口伪装成业务结论。
//
// ⚠️ 现在这条链路**先**问"门店配了数据源吗"(见 service/source.go):
// 没配 ⇒ 走本域 PG(自营店 10 的商品在这里),
// 问不到 ⇒ 503(**不**回落本地:回落 = 把一次链路故障显示成"该门店没有商品")。
func (h *Handler) mapCatalogErr(c *gin.Context, err error) {
	if h.writeSourceErr(c, err) {
		return
	}
	if errors.Is(err, service.ErrCubeUnavailable) {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code":    "cube_unavailable",
			"message": err.Error(),
		})
		return
	}
	if errors.Is(err, model.ErrInvalidInput) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "bad_request",
			"message": err.Error(),
		})
		return
	}
	h.logger.Error("catalog 查询失败", "err", err, "path", c.Request.URL.Path)
	c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{
		"code":    "cube_error",
		"message": err.Error(),
	})
}
