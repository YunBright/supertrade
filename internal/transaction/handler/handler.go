// Package handler —— 流水查询域(图2 / 图3 / 图6)的 HTTP handler。
//
// 路由(nginx 已剥过 /api/v1/transaction/):
//
//	GET /transactions/sales?q=&from=&to=&branch_id=&product_id=&category_id=
//	                  &sale_way=&cashier_id=&salesman_id=&counter_no=
//	                  &page=1&page_size=50
//	    商品销售流水列表(transaction:view,per branch)
//	GET /transactions/sales/:id
//	    单条商品销售流水(transaction:view,per branch)
//	GET /transactions/payments?q=&from=&to=&branch_id=&pay_way=&cashier_id=
//	                    &card_no=&vip_no=&page=1&page_size=50
//	    收银流水列表(transaction:view,per branch)
//
// # ⚠️ scope 是 `transaction:view`(**新 scope**)
//
// PLAN §3.6 写明:`transaction:view`(新)/ `procurement:view`(新)/
// `sales:view`(新) 需要在 auth 仓的 seed 里存在,否则一律 403 fail-closed。
//
// ⇒ auth 补 seed 之前这些端点对所有人 403。这是**有意的**:
// 权限点没 seed 就放行 = 任何登录用户都能看全店流水。
//
// # ⚠️ branch 一律走 X-Branch-ID header
//
// 路径里**没有** :branch_id(全局约定)。
//
// ⚠️ 查询参数里的 `branch_id` 是**源库**的分店号(思迅 branch_no),
// 与 X-Branch-ID 是两套无关编码(见 model.SaleFlowItem.BranchID 的注释)。
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
	"github.com/YunBright/supertrade/internal/transaction/model"
	"github.com/YunBright/supertrade/internal/transaction/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// Handler 是流水查询域的 handler。
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

// RegisterRoutes 挂载路由(nginx 已剥过 /api/v1/transaction/)。
func (h *Handler) RegisterRoutes(r gin.IRouter) {
	resolver := rbac.HasAnyScopeWithBranch(h.users)
	r.GET("/transactions/sales",
		rbac.RequireScopeWithBranch("transaction:view", branchFromHeader, resolver),
		h.listSales)
	r.GET("/transactions/sales/:id",
		rbac.RequireScopeWithBranch("transaction:view", branchFromHeader, resolver),
		h.getSale)
	r.GET("/transactions/payments",
		rbac.RequireScopeWithBranch("transaction:view", branchFromHeader, resolver),
		h.listPayments)
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

// listSales GET /transactions/sales
func (h *Handler) listSales(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	q := service.SaleQuery{
		Q:          c.Query("q"),
		From:       c.Query("from"),
		To:         c.Query("to"),
		BranchID:   c.Query("branch_id"),
		ProductID:  c.Query("product_id"),
		CategoryID: c.Query("category_id"),
		SaleWay:    c.Query("sale_way"),
		CashierID:  c.Query("cashier_id"),
		SalesmanID: c.Query("salesman_id"),
		CounterNo:  c.Query("counter_no"),
		Page:       1,
		PageSize:   service.DefaultPageSize,
	}
	q.Page, q.PageSize = pageArgs(c, q.Page, q.PageSize)
	page, err := h.svc.ListSales(c.Request.Context(), branchID, q)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, page)
}

// getSale GET /transactions/sales/:id
func (h *Handler) getSale(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "bad_request", "message": "流水 id 必填",
		})
		return
	}
	item, err := h.svc.GetSale(c.Request.Context(), branchID, id)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, item)
}

// listPayments GET /transactions/payments
func (h *Handler) listPayments(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	q := service.PayQuery{
		Q:         c.Query("q"),
		From:      c.Query("from"),
		To:        c.Query("to"),
		BranchID:  c.Query("branch_id"),
		PayWay:    c.Query("pay_way"),
		CashierID: c.Query("cashier_id"),
		CardNo:    c.Query("card_no"),
		VipNo:     c.Query("vip_no"),
		Page:      1,
		PageSize:  service.DefaultPageSize,
	}
	q.Page, q.PageSize = pageArgs(c, q.Page, q.PageSize)
	page, err := h.svc.ListPayments(c.Request.Context(), branchID, q)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, page)
}

// pageArgs 解析 page / page_size(越界值退回默认值,不当 400)。
//
// ⚠️ 与 catalog / pricing 的同一取舍:page_size 是**性能护栏**,
// 不是业务参数 ⇒ 越界时夹到合法区间,而不是拒绝整个请求。
func pageArgs(c *gin.Context, page, pageSize int) (int, int) {
	if v := c.Query("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	if v := c.Query("page_size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			pageSize = n
		}
	}
	return page, pageSize
}

// fail 把错误映射为 HTTP 状态。
//
// ⚠️ **403 / 401 与 503 必须分开**:
//
//	403 = "这个调用者没有该门店的读权限"
//	503 = "数据源路由问不到 / 数据源被停用"
//
// 混在一起 ⇒ 用户看到"暂不可用"就去查数据源健康度,
// 而真正的原因是权限。
func (h *Handler) fail(c *gin.Context, err error) {
	switch {
	case errors.Is(err, model.ErrSaleFlowNotFound):
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"code": "sale_flow_not_found", "message": err.Error(),
		})
	case errors.Is(err, model.ErrPayFlowNotFound):
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"code": "pay_flow_not_found", "message": err.Error(),
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
		// ⚠️ cube 侧没有这个 model / 维度 ⇒ **服务端**的契约不一致 ⇒ 503。
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code": "cube_unavailable", "message": "数据源暂不可用(查询契约不匹配)",
		})
	case errors.Is(err, cubeclient.ErrCubeUnavailable):
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code": "cube_unavailable", "message": "数据源暂不可用,请稍后重试",
		})
	default:
		h.logger.Error("transaction handler error", "err", err, "path", c.Request.URL.Path)
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
			"code": "internal_error", "message": "系统繁忙,请稍后重试",
		})
	}
}
