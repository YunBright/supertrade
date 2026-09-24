// Package handler 实现 cube-router 的 HTTP handler。
//
// 路由(/v1/load 由 cmdbootstrap 之外的子路由组注册;admin 由 RequireRole 守门):
//
//	POST /v1/load                         业务端点 —— cube:read scope(走 userd per-branch)
//	GET  /admin/branch-cube-sources       管理员:列出全部映射
//	POST /admin/branch-cube-sources       管理员:新增映射
//	PUT  /admin/branch-cube-sources/:id   管理员:修改映射
//	DELETE /admin/branch-cube-sources/:id 管理员:删除映射
//
// 错误映射:
//   - model.ErrNotFound     → 404 cube_source_not_configured
//   - model.ErrDisabled     → 503 cube_source_disabled
//   - model.ErrAlreadyExists → 409 conflict
//   - 其它 DB / cube 错误   → 500 internal_error
package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/YunBright/authkit/claims"
	"github.com/YunBright/authkit/rbac"
	"github.com/YunBright/authkit/userinfo"
	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/cube-router/model"
	"github.com/YunBright/supertrade/internal/cube-router/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// Handler 是 cube-router 的 HTTP handler。
//
// 持有 svc(业务逻辑)+ logger(可选)+ daprEndpoint(Dapr sidecar 地址,用于拼
// 出 cube instance 的 invocation URL)+ users(userd 客户端,scope 守门用)。
type Handler struct {
	svc          *service.Service
	users        *userinfo.Client
	daprEndpoint string // 默认 "http://localhost:3500"
	logger       *slog.Logger
}

// New 构造 Handler。
//
// daprEndpoint 默认 "http://localhost:3500",由 caller 用 DAPR_ENDPOINT 注入。
// users 用于 /v1/load 的 cube:read per-branch 守门;传 nil 时该端点返 503
// userd_unavailable(避免绕过 scope 校验)。
// logger == nil → 用 slog.Default()。
func New(svc *service.Service, users *userinfo.Client, daprEndpoint string, logger *slog.Logger) *Handler {
	if daprEndpoint == "" {
		daprEndpoint = "http://localhost:3500"
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		svc:          svc,
		users:        users,
		daprEndpoint: daprEndpoint,
		logger:       logger,
	}
}

// branchFromHeaderFromMiddleware 把 middleware.XBranchID 注入的 *uuid.UUID
// 转成 string(供 rbac.RequireScopeWithBranch 的 BranchContextFn 用)。
//
// nil(未传 header)→ 返 ("", false) → 中间件按 400 branch_required 拒。
func branchFromHeaderFromMiddleware(c *gin.Context) (string, bool) {
	id := middleware.BranchFromCtx(c)
	if id == nil {
		return "", false
	}
	return id.String(), true
}

// RegisterRoutes 把所有路由挂到 r(nginx 已剥过 /api/v1/cube-router/)。
//
// 中间件链:
//   - rbac.RequireAudience("cube-router") 由 cmdbootstrap 全局挂;此处不重复
//   - 业务端点 /v1/load: middleware.XBranchID() + rbac.RequireScopeWithBranch("cube:read")
//   - admin/*: rbac.RequireRole("admin")
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	// 业务端点(需要 X-Branch-ID header + cube:read per-branch scope)
	resolver := rbac.HasAnyScopeWithBranch(h.users)
	biz := r.Group("/")
	biz.Use(middleware.XBranchID())
	biz.POST("/v1/load",
		rbac.RequireScopeWithBranch("cube:read", branchFromHeaderFromMiddleware, resolver),
		h.handleLoad,
	)

	// 管理端点(仅 admin 角色)
	admin := r.Group("/admin")
	admin.Use(rbac.RequireRole("admin"))
	admin.GET("/branch-cube-sources", h.listSources)
	admin.POST("/branch-cube-sources", h.createSource)
	admin.GET("/branch-cube-sources/:branch_id", h.getSource)
	admin.PUT("/branch-cube-sources/:branch_id", h.updateSource)
	admin.DELETE("/branch-cube-sources/:branch_id", h.deleteSource)
}

// handleLoad 转发 POST /v1/load 到对应 cube instance。
//
// 路径 /v1/load 在 handler/proxy.go 中实现(独立的 forwarder 文件,便于阅读)。
func (h *Handler) handleLoad(c *gin.Context) {
	h.proxyLoad(c)
}

// ---- admin endpoints (admin.go) ----

type createReq struct {
	BranchID       string `json:"branch_id" binding:"required"`
	CubeSourceName string `json:"cube_source_name" binding:"required"`
	Enabled        *bool  `json:"enabled"` // pointer 区分"未设"
}

type updateReq struct {
	CubeSourceName string `json:"cube_source_name" binding:"required"`
	Enabled        bool   `json:"enabled"`
}

// listSources GET /admin/branch-cube-sources
func (h *Handler) listSources(c *gin.Context) {
	rows, err := h.svc.List(c.Request.Context())
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"sources": rows,
		"count":   len(rows),
	})
}

// createSource POST /admin/branch-cube-sources
func (h *Handler) createSource(c *gin.Context) {
	var req createReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "bad_request",
			"message": err.Error(),
		})
		return
	}
	createdBy := callerSub(c)
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	row, err := h.svc.Create(c.Request.Context(), model.BranchCubeSource{
		BranchID:       req.BranchID,
		CubeSourceName: req.CubeSourceName,
		Enabled:        enabled,
		CreatedBy:      createdBy,
	})
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, row)
}

// getSource GET /admin/branch-cube-sources/:branch_id
func (h *Handler) getSource(c *gin.Context) {
	branchID := c.Param("branch_id")
	row, err := h.svc.Get(c.Request.Context(), branchID)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, row)
}

// updateSource PUT /admin/branch-cube-sources/:branch_id
func (h *Handler) updateSource(c *gin.Context) {
	branchID := c.Param("branch_id")
	var req updateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "bad_request",
			"message": err.Error(),
		})
		return
	}
	row, err := h.svc.Update(c.Request.Context(), branchID, req.CubeSourceName, req.Enabled, callerSub(c))
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, row)
}

// deleteSource DELETE /admin/branch-cube-sources/:branch_id
func (h *Handler) deleteSource(c *gin.Context) {
	branchID := c.Param("branch_id")
	if err := h.svc.Delete(c.Request.Context(), branchID); err != nil {
		h.mapErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// ---- helpers ----

// callerSub 拿当前 caller 的 sub(从 JWT claims;失败返 "system")。
//
// admin 端点已被 rbac.RequireRole("admin") 守门,claims 必然存在;
// 这里只在解析失败时兜底,正常情况下拿真实 sub 做审计。
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
	case errors.Is(err, model.ErrNotFound):
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"code":    "cube_source_not_configured",
			"message": "branch 未配置 cube 源",
		})
	case errors.Is(err, model.ErrAlreadyExists):
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"code":    "conflict",
			"message": err.Error(),
		})
	case errors.Is(err, model.ErrDisabled):
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code":    "cube_source_disabled",
			"message": "branch cube 源已禁用",
		})
	default:
		h.logger.Error("cube_router handler error", "err", err, "path", c.Request.URL.Path)
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
			"code":    "internal_error",
			"message": err.Error(),
		})
	}
}