// Package handler 实现 pricing 服务的 HTTP handler。
//
// 路由(nginx 已剥过 /api/v1/pricing/):
//
//	GET  /quotes?barcode=&on_date=   查条码当前价格结论(pricing:view,per branch)
//	GET  /promotions/active?barcode=  同上,只返促销部分(给"只关心特价"的调用方)
//	GET  /meta                        价格标签字段名等静态元数据(pricing:view)
//	GET  /prices?product_ids=   批量查售价(pricing:view)
//	                              判据:配了数据源走 cube、没配走本地
//	GET  /promotions?...        批量查当前生效促销(pricing:view)
//	                              判据同上
//	GET  /prices/export?on_date=      离线价格包(pricing:view)—— 不分页,全店
//	GET  /archive/export?scope=       归档导出(archivedto 契约)(pricing:view)
//	POST /archive/import               导入售价/促销(pricing:manage,per branch)
//
// 中间件:
//   - middleware.XBranchID() 全局挂(由 cmd/pricing/main.go 注册)
//   - rbac.RequireScopeWithBranch(userd per-branch)
//
// 错误映射(与 catalog 保持同一套 code,前端 error_mapper 不用分服务处理):
//   - service.ErrInvalidBarcode → 400 invalid_barcode
//   - model.ErrInvalidInput     → 400 bad_request
//   - model.ErrUnsupportedExportScope → 400 unsupported_export_scope
//   - model.ErrCubeSourceUnavailable → 503 cube_unavailable
//   - model.ErrCubeSourceDisabled    → 503 cube_source_disabled
//   - model.ErrExternalSourceReadOnly → 409 member_source_is_external
//   - cubeclient.ErrCubeUnavailable / ErrCubeAuthRejected → 503 / 401
//   - cubeclient.ErrCubeForbidden → 403
//   - 其余 → 500
//
// ⚠️ **无生效促销不是错误**:返 200 + {"promo": null}。
//
//	绝大多数商品本来就没有促销,返 404 会让前端把"正常"显示成"查失败"。
//
// ⚠️ `pricing:manage` 目前在 auth 的 seed 里**不存在** ⇒ 写侧端点对所有人 403
// (fail-closed)。权限点没 seed 就放行 = 任何有 pricing:view 的人都能改价。
package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/YunBright/authkit/rbac"
	"github.com/YunBright/authkit/userinfo"
	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/pricing/model"
	"github.com/YunBright/supertrade/internal/pricing/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// Handler 是 pricing 服务的 HTTP handler 聚合。
type Handler struct {
	prices *service.PriceService
	// priceLists / promos 是本地表服务(写侧 + 批量读侧用)。
	priceLists *service.PriceListService
	promos     *service.PromotionService
	// plans 是促销**方案头**的读服务(图4)。
	plans  *service.PromotionPlanService
	users  *userinfo.Client
	logger *slog.Logger
}

// New 构造 Handler。users 传 nil 时业务端点一律 503 userd_unavailable。
//
// priceLists / promos 传 nil 时:读侧走 cube 回落,写侧端点返 503 not_ready。
func New(prices *service.PriceService, priceLists *service.PriceListService, promos *service.PromotionService, users *userinfo.Client, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		prices:     prices,
		priceLists: priceLists,
		promos:     promos,
		users:      users,
		logger:     logger,
	}
}

// SetPromotionPlans 装配促销方案头服务(图4)。
//
// ⚠️ 没装配 ⇒ `/promotion-plans` 返 503 not_ready(装配错误要大声,
// 不能表现成"这家店没有促销方案")。
func (h *Handler) SetPromotionPlans(plans *service.PromotionPlanService) {
	h.plans = plans
}

// RegisterRoutes 挂载全部路由(nginx 已剥过 /api/v1/pricing/)。
func (h *Handler) RegisterRoutes(r gin.IRouter) {
	resolver := rbac.HasAnyScopeWithBranch(h.users)

	r.GET("/quotes",
		rbac.RequireScopeWithBranch("pricing:view", branchFromHeader, resolver),
		h.getQuote)
	r.GET("/promotions/active",
		rbac.RequireScopeWithBranch("pricing:view", branchFromHeader, resolver),
		h.getActivePromo)
	r.GET("/meta",
		rbac.RequireScopeWithBranch("pricing:view", branchFromHeader, resolver),
		h.getMeta)
	// 本地批量读(POS 一次扫一屏商品 ⇒ 一次请求拿一批价)。
	r.GET("/prices",
		rbac.RequireScopeWithBranch("pricing:view", branchFromHeader, resolver),
		h.getPrices)
	r.GET("/promotions",
		rbac.RequireScopeWithBranch("pricing:view", branchFromHeader, resolver),
		h.getPromotions)
	// 离线价格包(一次给全:默认价目表的生效零售价 + 该基准日生效的促销)。
	//
	// ⚠️ **路径与响应形状不许变**(前端 / POS 离线包在用)——
	// 只有**数据来源**按"配没配数据源"切(有源 ⇒ cube 实时,没源 ⇒ 本地 PG)。
	r.GET("/prices/export",
		rbac.RequireScopeWithBranch("pricing:view", branchFromHeader, resolver),
		h.exportLocalPrices)
	// 归档导出(archivedto 契约):与 cube-router 的 /v1/export **同一份字段名**。
	//
	// ⚠️ 守门是 `pricing:view` —— 导出是**读**操作。
	r.GET("/archive/export",
		rbac.RequireScopeWithBranch("pricing:view", branchFromHeader, resolver),
		h.getArchiveExport)
	// 促销方案(图4,**只读**)。
	//
	// ⚠️ 与 `/promotions`(促销**行**)是两个东西:
	//   /promotions     批量查某批商品当前生效的促销(给 POS / 价签用)
	//   /promotion-plans 查促销**方案单**(图4 的列表 + 详情)
	r.GET("/promotion-plans",
		rbac.RequireScopeWithBranch("pricing:view", branchFromHeader, resolver),
		h.listPromotionPlans)
	r.GET("/promotion-plans/:plan_no",
		rbac.RequireScopeWithBranch("pricing:view", branchFromHeader, resolver),
		h.getPromotionPlan)
	// 写侧(导入)。scope 是 pricing:manage(见包注释:目前未 seed ⇒ 403)。
	r.POST("/archive/import",
		rbac.RequireScopeWithBranch("pricing:manage", branchFromHeader, resolver),
		h.importArchive)
}

// branchFromHeader 取 X-Branch-ID(单店)。
// 空 → ("", false) → RequireScopeWithBranch 按 400 branch_required 拒。
func branchFromHeader(c *gin.Context) (string, bool) {
	b := middleware.SingleBranchFromCtx(c)
	if b == "" {
		return "", false
	}
	return b, true
}

// getQuote GET /quotes?barcode=&on_date=
//
// 返回 200 + Quote。没维护售价 → retail_price 省略;无生效促销 → promo 省略。
func (h *Handler) getQuote(c *gin.Context) {
	barcode := c.Query("barcode")
	if barcode == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "missing_barcode", "message": "barcode query 必填",
		})
		return
	}
	branchID := middleware.SingleBranchFromCtx(c)
	q, err := h.prices.Quote(c.Request.Context(), branchID, barcode, c.Query("on_date"))
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, q)
}

// getActivePromo GET /promotions/active?barcode=
//
// 与 /quotes 的差别:只返促销部分。
// ⚠️ 无生效促销不是错误:返 200 + {"promo": null}(不是 404)。
func (h *Handler) getActivePromo(c *gin.Context) {
	barcode := c.Query("barcode")
	if barcode == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "missing_barcode", "message": "barcode query 必填",
		})
		return
	}
	branchID := middleware.SingleBranchFromCtx(c)
	q, err := h.prices.Quote(c.Request.Context(), branchID, barcode, c.Query("on_date"))
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"barcode": q.Barcode, "promo": q.Promo, "as_of": q.AsOf, "source": q.Source})
}

// getMeta GET /meta
//
// 静态元数据。存在的意义:价格标签字段名("特价日期/店特日期/活动日期")
// 的枚举由**后端**定义并下发,前端不再自己硬编码一份 ——
// 两份枚举必然漂移,而漂移的表现是"某个选项打出来是空白标签",极难发现。
func (h *Handler) getMeta(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"app":               "pricing",
		"promo_date_labels": service.DefaultPromoDateLabels,
	})
}

// mapErr 把 service / cubeclient 错误映射为 HTTP 状态码 + JSON body。
//
// ⚠️ ErrCubeAuthRejected 与 ErrCubeUnavailable **必须**分开:
//
//	前者是"你的登录态没了"(401,重新登录能好),后者是"cube 挂了"(503,重试没用)。
//
// 合并的后果是一次登录态问题被显示成「系统繁忙」,排查全部跑去查 cube 健康度。
func (h *Handler) mapErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrInvalidBarcode):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "invalid_barcode", "message": err.Error(),
		})
	case errors.Is(err, model.ErrInvalidInput):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "bad_request", "message": err.Error(),
		})
	case errors.Is(err, model.ErrUnsupportedExportScope):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "unsupported_export_scope", "message": err.Error(),
		})
	case errors.Is(err, model.ErrLocalStoreNotReady):
		// 503:部署错误(本地表没装配),不是业务结论。
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code": "not_ready", "message": err.Error(),
		})
	case errors.Is(err, model.ErrCubeSourceUnavailable):
		// ⚠️ 503 而不是回落成一份空结果:这是链路故障(问不到路由),
		// 回落会让用户看到「该门店还没有价格」——与故障无关的结论。
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code": "cube_unavailable", "message": "数据源路由暂不可用，请稍后重试",
		})
	case errors.Is(err, model.ErrCubeSourceDisabled):
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code": "cube_source_disabled", "message": "该门店的数据源已停用",
		})
	case errors.Is(err, model.ErrExternalSourceReadOnly):
		// 409:状态冲突(这个门店的价在外部系统里,不是权限问题)。
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"code": "member_source_is_external", "message": err.Error(),
		})
	case errors.Is(err, service.ErrNoCube):
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code": "cube_unavailable", "message": err.Error(),
		})
	case errors.Is(err, cubeclient.ErrCubeForbidden):
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"code": "forbidden", "message": err.Error(),
		})
	case errors.Is(err, cubeclient.ErrCubeAuthRejected):
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"code": "unauthorized", "message": err.Error(),
		})
	case errors.Is(err, cubeclient.ErrCubeUnavailable):
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code": "cube_unavailable", "message": err.Error(),
		})
	case errors.Is(err, cubeclient.ErrCubeBadRequest):
		// 查询本身不合法 = 调用方 bug,必须暴露成 400,不能折叠成 503。
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "bad_request", "message": err.Error(),
		})
	case errors.Is(err, cubeclient.ErrProductNotFound),
		errors.Is(err, cubeclient.ErrPromotionNotFound):
		// 这里到不了(两个"查不到"都被 service 转成了正常结论),
		// 保留分支是为了将来新增查询路径时不会顺手落到 500。
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"code": "not_found", "message": err.Error(),
		})
	default:
		h.logger.Error("pricing 未映射错误", "err", err)
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
			"code": "internal_error", "message": "系统繁忙，请稍后重试",
		})
	}
}
