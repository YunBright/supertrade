// Package handler —— 采购收货域(图5)的 HTTP handler。
//
// 路由(nginx 已剥过 /api/v1/procurement/):
//
//	GET /purchase-sheets?from=&to=&branch_id=&supplier_id=&approve_status=
//	                   &order_no=&document_type=&created_by=&amount_gte=
//	                   &page=1&page_size=50
//	    采购单列表(procurement:view,per branch)
//	GET /purchase-sheets/:sheet_no
//	    采购单详情(头 + 明细)(procurement:view,per branch)
//
// # ⚠️ scope 是 `procurement:view`(**新 scope**)
//
// PLAN §3.6:新 scope 需要在 auth 仓的 seed 里存在,否则一律 403 fail-closed。
// 本轮这些端点在 auth 补 seed 之前对所有人 403 —— 这是**有意的**。
//
// # ⚠️ branch 一律走 X-Branch-ID header(路径里没有 :branch_id)。
//
// 查询参数里的 `branch_id` 是**源库**的分店号(思迅 branch_no),
// 与 X-Branch-ID 是两套无关编码。
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
	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/procurement/model"
	"github.com/YunBright/supertrade/internal/procurement/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// Handler 是采购收货域的 handler。
type Handler struct {
	svc    *service.Service
	users  *userinfo.Client
	logger *slog.Logger
}

// New 构造 Handler。
func New(svc *service.Service, users *userinfo.Client, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{svc: svc, users: users, logger: logger}
}

// RegisterRoutes 挂载路由(nginx 已剥过 /api/v1/procurement/)。
func (h *Handler) RegisterRoutes(r gin.IRouter) {
	resolver := rbac.HasAnyScopeWithBranch(h.users)
	r.GET("/purchase-sheets",
		rbac.RequireScopeWithBranch("procurement:view", branchFromHeader, resolver),
		h.listSheets)
	r.GET("/purchase-sheets/:sheet_no",
		rbac.RequireScopeWithBranch("procurement:view", branchFromHeader, resolver),
		h.getSheet)
}

// branchFromHeader 取 X-Branch-ID(单店)。
func branchFromHeader(c *gin.Context) (string, bool) {
	b := middleware.SingleBranchFromCtx(c)
	if b == "" {
		return "", false
	}
	return b, true
}

// listSheets GET /purchase-sheets
func (h *Handler) listSheets(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	q := service.Query{
		From:          c.Query("from"),
		To:            c.Query("to"),
		BranchID:      c.Query("branch_id"),
		SupplierID:    c.Query("supplier_id"),
		ApproveStatus: c.Query("approve_status"),
		OrderNo:       c.Query("order_no"),
		DocumentType:  c.Query("document_type"),
		// ⚠️ 前端已经在发 created_by(制单人)⇒ 后端必须真的过滤,
		// 否则"点了筛选没反应"与"参数被忽略"长得一模一样。
		CreatedBy: c.Query("created_by"),
		Page:          1,
		PageSize:      service.DefaultPageSize,
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
	// ⚠️ amount_gte(单据金额下限)是**可选数值参数**,但**非法值必须 400**。
	//
	// 为什么不能"解析失败就当没传":
	//   "点了筛选没反应"与"参数被忽略"长得一模一样 —— 用户会以为功能坏了,
	//   而真相是后端把非法值悄悄丢了。
	//   而且它是**金额**:把 "abc" 当成"不过滤"会返回**全量单据**,
	//   用户以为"金额≥ 的单据就这么几条",实际看到的是全部。
	// ⇒ 诚实报错(400),让调用方知道这个参数没被接受。
	if v := strings.TrimSpace(c.Query("amount_gte")); v != "" {
		d, err := decimal.NewFromString(v)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"code": "bad_request", "message": "amount_gte 必须是合法的金额数字",
			})
			return
		}
		q.AmountGte = &d
	}
	page, err := h.svc.List(c.Request.Context(), branchID, q)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, page)
}

// getSheet GET /purchase-sheets/:sheet_no
func (h *Handler) getSheet(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	sheetNo := strings.TrimSpace(c.Param("sheet_no"))
	if sheetNo == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "bad_request", "message": "单据号 sheet_no 必填",
		})
		return
	}
	detail, err := h.svc.Get(c.Request.Context(), branchID, sheetNo)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, detail)
}

// fail 把错误映射为 HTTP 状态。
//
// ⚠️ **403 / 401 与 503 必须分开**(理由同 transaction / catalog)。
func (h *Handler) fail(c *gin.Context, err error) {
	switch {
	case errors.Is(err, model.ErrPurchaseSheetNotFound):
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"code": "purchase_sheet_not_found", "message": err.Error(),
		})
	case errors.Is(err, model.ErrCubeSourceUnavailable):
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code": "cube_unavailable", "message": "数据源路由暂不可用,请稍后重试",
		})
	case errors.Is(err, model.ErrCubeSourceDisabled):
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code": "cube_source_disabled", "message": "该门店的数据源已停用",
		})
	case errors.Is(err, model.ErrLocalStoreNotReady):
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code": "not_ready", "message": err.Error(),
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
		h.logger.Error("procurement handler error", "err", err, "path", c.Request.URL.Path)
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
			"code": "internal_error", "message": "系统繁忙,请稍后重试",
		})
	}
}
