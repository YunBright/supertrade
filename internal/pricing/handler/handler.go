// Package handler 实现 pricing 服务的 HTTP handler。
//
// 路由(nginx 已剥过 /api/v1/pricing/):
//
//	GET /quotes?barcode=&on_date=   查条码当前价格结论(pricing:view,per branch)
//	GET /promotions/active?barcode= 同上,只返促销部分(给"只关心特价"的调用方)
//	GET /meta                       价格标签字段名等静态元数据(pricing:view)
//
// 中间件:
//   - middleware.XBranchID() 全局挂(由 cmd/pricing/main.go 注册)
//   - rbac.RequireScopeWithBranch(userd per-branch)
//
// 错误映射(与 catalog 保持同一套 code,前端 error_mapper 不用分服务处理):
//   - service.ErrInvalidBarcode → 400 invalid_barcode
//   - cubeclient.ErrCubeUnavailable / ErrCubeAuthRejected → 503 / 401
//   - cubeclient.ErrCubeForbidden → 403
//   - 其余 → 500
//
// ⚠️ **无生效促销不是错误**:返 200 + {"promo": null}。
//    绝大多数商品本来就没有促销,返 404 会让前端把"正常"显示成"查失败"。
package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/YunBright/authkit/rbac"
	"github.com/YunBright/authkit/userinfo"
	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/pricing/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// Handler 是 pricing 服务的 HTTP handler 聚合。
type Handler struct {
	prices *service.PriceService
	users  *userinfo.Client
	logger *slog.Logger
}

// New 构造 Handler。users 传 nil 时业务端点一律 503 userd_unavailable。
func New(prices *service.PriceService, users *userinfo.Client, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{prices: prices, users: users, logger: logger}
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
	q, err := h.prices.Quote(c.Request.Context(), barcode, c.Query("on_date"))
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, q)
}

// getActivePromo GET /promotions/active?barcode=
//
// 与 /quotes 的差别:只返促销。无促销时返 {"promo": null,**仍是 200**。
func (h *Handler) getActivePromo(c *gin.Context) {
	barcode := c.Query("barcode")
	if barcode == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "missing_barcode", "message": "barcode query 必填",
		})
		return
	}
	q, err := h.prices.Quote(c.Request.Context(), barcode, c.Query("on_date"))
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"barcode": q.Barcode, "promo": q.Promo, "as_of": q.AsOf})
}

// getMeta GET /meta
//
// 静态元数据。存在的意义:价格标签字段名("特价日期/店特日期/活动日期")
// 的枚举由**后端**定义并下发,前端不再自己硬编码一份 ——
// 两份枚举必然漂移,而漂移的表现是"某个选项打出来是空白标签",极难发现。
func (h *Handler) getMeta(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"app":              "pricing",
		"promo_date_labels": service.DefaultPromoDateLabels,
	})
}

// mapErr 把 service / cubeclient 错误映射为 HTTP 状态码 + JSON body。
//
// ⚠️ ErrCubeAuthRejected 与 ErrCubeUnavailable **必须**分开:
//   前者是"你的登录态没了"(401,重新登录能好),后者是"cube 挂了"(503,重试没用)。
// 合并的后果是一次登录态问题被显示成「系统繁忙」,排查全部跑去查 cube 健康度。
func (h *Handler) mapErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrInvalidBarcode):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "invalid_barcode", "message": err.Error(),
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
