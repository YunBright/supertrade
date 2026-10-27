// Package handler / reports.go —— 图7 的销售报表 HTTP 端点。
//
// 路由(nginx 剥掉 /api/v1/sales-agg/ 前缀后):
//
//	GET /reports/sales/menu-items   11 个菜单项(sales:view,per branch)
//	GET /reports/sales/by-code?code=&from=&to=&branch_id=&product_id=
//	                             &category_id=&supplier_id=&sale_way=
//	                             &brand_name=&page=1&page_size=100
//	    按 code 取一张报表(sales:view,per branch)
//
// ⚠️ 这两条端点必须挂 rbac.RequireScopeWithBranch(sales:view)。已有的 GET /reports/sales 未挂,不在本次改动范围(改动会变更已上线行为)。
//
// # ⚠️ menu-items 也要挂守门
//
// 菜单本身不含业务数据,但它决定了前端能看哪些报表 ⇒ 挂在同一个
// scope 下,避免"没权限的人看到菜单、点进去才 403"。
package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/YunBright/authkit/rbac"
	"github.com/YunBright/authkit/userinfo"
	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/salesagg/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// ReportsHandler 是图7 报表的 handler。
type ReportsHandler struct {
	svc    *service.ReportsService
	users  *userinfo.Client
	logger *slog.Logger
}

// NewReportsHandler 构造 ReportsHandler。
func NewReportsHandler(svc *service.ReportsService, users *userinfo.Client, logger *slog.Logger) *ReportsHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &ReportsHandler{svc: svc, users: users, logger: logger}
}

// RegisterRoutes 挂载路由。
func (h *ReportsHandler) RegisterRoutes(r gin.IRouter) {
	resolver := rbac.HasAnyScopeWithBranch(h.users)
	r.GET("/reports/sales/menu-items",
		rbac.RequireScopeWithBranch("sales:view", branchFromHeader, resolver),
		h.menuItems)
	r.GET("/reports/sales/by-code",
		rbac.RequireScopeWithBranch("sales:view", branchFromHeader, resolver),
		h.byCode)
}

// branchFromHeader 取 X-Branch-ID(单店)。
func branchFromHeader(c *gin.Context) (string, bool) {
	b := middleware.SingleBranchFromCtx(c)
	if b == "" {
		return "", false
	}
	return b, true
}

// menuItems GET /reports/sales/menu-items
func (h *ReportsHandler) menuItems(c *gin.Context) {
	c.JSON(http.StatusOK, service.Menu())
}

// byCode GET /reports/sales/by-code
func (h *ReportsHandler) byCode(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	code := strings.TrimSpace(c.Query("code"))
	if code == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "bad_request",
			"message": "code 查询参数必填(可取值见 GET /reports/sales/menu-items)",
		})
		return
	}
	q := service.ReportQuery{
		Code:       code,
		From:       c.Query("from"),
		To:         c.Query("to"),
		BranchID:   c.Query("branch_id"),
		ProductID:  c.Query("product_id"),
		CategoryID: c.Query("category_id"),
		SupplierID: c.Query("supplier_id"),
		SaleWay:    c.Query("sale_way"),
		BrandName:  c.Query("brand_name"),
		Page:       1,
		PageSize:   100,
	}
	if v := c.Query("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			q.Page = n
		}
	}
	if v := c.Query("page_size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			q.PageSize = n
		}
	}
	rep, err := h.svc.Run(c.Request.Context(), branchID, q)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, rep)
}

// fail 把错误映射为 HTTP 状态。
//
// ⚠️ **403 / 401 与 503 必须分开**(理由同 transaction / procurement)。
func (h *ReportsHandler) fail(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrUnknownReportCode):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "invalid_report_code",
			"message": err.Error(),
		})
	case errors.Is(err, service.ErrReportSourceUnavailable):
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code": "cube_unavailable", "message": "数据源路由暂不可用,请稍后重试",
		})
	case errors.Is(err, service.ErrReportSourceDisabled):
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code": "cube_source_disabled", "message": "该门店的数据源已停用",
		})
	case errors.Is(err, cubeclient.ErrCubeForbidden):
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"code": "forbidden", "message": "没有该门店的数据读权限",
		})
	case errors.Is(err, cubeclient.ErrCubeAuthRejected):
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"code": "unauthorized", "message": "登录已失效,请重新登录",
		})
	case errors.Is(err, cubeclient.ErrCubeBadRequest):
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code": "cube_unavailable", "message": "数据源暂不可用(查询契约不匹配)",
		})
	case errors.Is(err, cubeclient.ErrCubeUnavailable):
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code": "cube_unavailable", "message": "数据源暂不可用,请稍后重试",
		})
	default:
		h.logger.Error("salesagg reports handler error", "err", err, "path", c.Request.URL.Path)
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
			"code": "internal_error", "message": "系统繁忙,请稍后重试",
		})
	}
}
