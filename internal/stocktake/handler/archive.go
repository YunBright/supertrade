// Package handler / archive.go —— stocktake 的**归档导出 + 批量导入**端点。
//
// 路由(nginx 已剥过 /api/v1/stocktake/):
//
//	GET  /archive/export?scope=headers|lines|line_operations|plan_items
//	POST /archive/import
//
// # 守卫 scope
//
//	导出 inventory:view(仓管 view) / 导入 inventory:manage(仓管)
//	—— 与本域既有端点同一档(列表/详情用 view,写用 manage)。
//
// # ⚠️ 导入 = **数据恢复**,不是业务重放
//
//	只往本地表里写行:不调 AddLine / Submit / Approve,
//	不写操作历史、不重算差异、不发 dapr 事件、不推进状态机。
package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/YunBright/authkit/rbac"
	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/archiveroute"
	"github.com/YunBright/supertrade/internal/stocktake/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// ArchiveHandler 是 stocktake 的归档导出 / 导入 handler。
type ArchiveHandler struct {
	archive *service.ArchiveService
	// resolver 是 per-branch scope 解析器。
	resolver rbac.ScopeResolver
}

// NewArchiveHandler 构造 ArchiveHandler。
func NewArchiveHandler(archive *service.ArchiveService, resolver rbac.ScopeResolver) *ArchiveHandler {
	return &ArchiveHandler{archive: archive, resolver: resolver}
}

// RegisterRoutes 挂载归档端点。
func (h *ArchiveHandler) RegisterRoutes(r gin.IRouter) {
	r.GET("/archive/export",
		rbac.RequireScopeWithBranch(service.ScopeInventoryView, archiveBranchFromHeader, h.resolver),
		h.exportArchive)
	r.POST("/archive/import",
		rbac.RequireScopeWithBranch(service.ScopeInventoryManage, archiveBranchFromHeader, h.resolver),
		h.importArchive)
}

// archiveBranchFromHeader 取 X-Branch-ID(单店)。
func archiveBranchFromHeader(c *gin.Context) (string, bool) {
	b := strings.TrimSpace(middleware.SingleBranchFromCtx(c))
	if b == "" {
		return "", false
	}
	return b, true
}

// exportArchive GET /archive/export?scope=
func (h *ArchiveHandler) exportArchive(c *gin.Context) {
	branchID := strings.TrimSpace(middleware.SingleBranchFromCtx(c))
	if branchID == "" {
		writeError(c, http.StatusBadRequest, "branch_required", "X-Branch-ID header 必填")
		return
	}
	scope := strings.TrimSpace(c.Query("scope"))
	if scope == "" {
		writeError(c, http.StatusBadRequest, "bad_request", "scope 必填")
		return
	}
	res, err := h.archive.Export(c.Request.Context(), branchID, scope)
	if err != nil {
		mapArchiveErr(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// importArchive POST /archive/import
func (h *ArchiveHandler) importArchive(c *gin.Context) {
	branchID := strings.TrimSpace(middleware.SingleBranchFromCtx(c))
	if branchID == "" {
		writeError(c, http.StatusBadRequest, "branch_required", "X-Branch-ID header 必填")
		return
	}
	var req service.ImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	res, err := h.archive.Import(c.Request.Context(), branchID, req)
	if err != nil {
		mapArchiveErr(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// mapArchiveErr 把归档端点的错误映射为 HTTP 状态码 + JSON body。
func mapArchiveErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrInvalidStatus):
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, archiveroute.ErrExternalReadOnly):
		// 409:状态冲突(数据在外部系统里,不是权限问题)。
		// ⚠️ message **不暴露**内部 app-id。
		writeError(c, http.StatusConflict, "member_source_is_external",
			"该门店的盘点数据来自外部数据源,请在原系统操作")
	case errors.Is(err, archiveroute.ErrForbidden):
		writeError(c, http.StatusForbidden, "forbidden", "没有该门店的数据源读权限")
	case errors.Is(err, archiveroute.ErrAuthRejected):
		writeError(c, http.StatusUnauthorized, "unauthorized", "登录已失效,请重新登录")
	case errors.Is(err, archiveroute.ErrDisabled):
		writeError(c, http.StatusServiceUnavailable, "cube_source_disabled", "该门店的数据源已停用")
	case errors.Is(err, archiveroute.ErrUnavailable):
		// ⚠️ 503 而不是空列表:问不到路由是一次链路故障。
		writeError(c, http.StatusServiceUnavailable, "cube_unavailable", "数据源路由暂不可用,请稍后重试")
	default:
		writeError(c, http.StatusInternalServerError, "internal_error", err.Error())
	}
}
