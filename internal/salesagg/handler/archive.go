// Package handler / archive.go —— sales-agg 的**归档导出 + 批量导入**端点。
//
// 路由(nginx 已剥过 /api/v1/sales-agg/):
//
//	GET  /archive/export?scope=compare_templates
//	POST /archive/import
//
// # 守卫 scope
//
//	导出 inventory:view / 导入 inventory:manage
//	—— per-branch 守门(与本域既有的"对比模板只认 token claims"不同,
//	归档端点**额外**按门店收口:见 internal/salesagg/compare/archive.go 的文件头)。
//
// # ⚠️ 导入 = **数据恢复**,不是业务重放
//
//	只写 sales_compare_templates 一张表:不重算对比数据、不调 cube 取数、不发事件。
package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/YunBright/authkit/rbac"
	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/archiveroute"
	"github.com/YunBright/supertrade/internal/salesagg/compare"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// ArchiveHandler 是 sales-agg 的归档导出 / 导入 handler。
type ArchiveHandler struct {
	archive *compare.ArchiveService
	// resolver 是 per-branch scope 解析器。
	resolver rbac.ScopeResolver
}

// NewArchiveHandler 构造 ArchiveHandler。
func NewArchiveHandler(archive *compare.ArchiveService, resolver rbac.ScopeResolver) *ArchiveHandler {
	return &ArchiveHandler{archive: archive, resolver: resolver}
}

// RegisterRoutes 挂载归档端点。
func (h *ArchiveHandler) RegisterRoutes(r *gin.Engine) {
	r.GET("/archive/export",
		rbac.RequireScopeWithBranch(compare.ScopeInventoryView, archiveBranchFromHeader, h.resolver),
		h.exportArchive)
	r.POST("/archive/import",
		rbac.RequireScopeWithBranch(compare.ScopeInventoryManage, archiveBranchFromHeader, h.resolver),
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
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "branch_required", "message": "X-Branch-ID header 必填",
		})
		return
	}
	scope := strings.TrimSpace(c.Query("scope"))
	if scope == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "bad_request", "message": "scope 必填",
		})
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
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "branch_required", "message": "X-Branch-ID header 必填",
		})
		return
	}
	var req compare.ImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "compare_template_invalid", "message": err.Error(),
		})
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
	case errors.Is(err, compare.ErrInvalid):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "compare_template_invalid", "message": err.Error(),
		})
	case errors.Is(err, archiveroute.ErrExternalReadOnly):
		// 409:状态冲突(数据在外部系统里,不是权限问题)。
		// ⚠️ message **不暴露**内部 app-id。
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"code":    "member_source_is_external",
			"message": "该门店的对比模板来自外部数据源,请在原系统操作",
		})
	case errors.Is(err, archiveroute.ErrForbidden):
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"code": "forbidden", "message": "没有该门店的数据源读权限",
		})
	case errors.Is(err, archiveroute.ErrAuthRejected):
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"code": "unauthorized", "message": "登录已失效,请重新登录",
		})
	case errors.Is(err, archiveroute.ErrDisabled):
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code": "cube_source_disabled", "message": "该门店的数据源已停用",
		})
	case errors.Is(err, archiveroute.ErrUnavailable):
		// ⚠️ 503 而不是空列表:问不到路由是一次链路故障。
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code": "cube_unavailable", "message": "数据源路由暂不可用,请稍后重试",
		})
	default:
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
			"code": "internal", "message": err.Error(),
		})
	}
}
