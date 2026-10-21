// Package handler / archive.go —— master-data 的**归档导出 + 批量导入**端点。
//
// 路由(nginx 已剥过 /api/v1/master-data/):
//
//	GET  /archive/export?scope=stores|employees|shifts   导出(守卫 store:view)
//	POST /archive/import                                 导入(守卫 store:manage)
//
// # ⚠️ 为什么这两个端点也在"有源走 cube、无源走本地"的范式里
//
//	门店档案 / 收银员档案 / 班次**不在 cube 里**
//	⇒ 它们的读端点永远走本地;
//	但导入导出是**跨系统搬运数据**的动作,必须问一句
//	「这个门店配了外部数据源吗」:
//
//	  - 没配 ⇒ 导本地 / 写本地(自营店 10 的常规路径);
//	  - 配了 ⇒ 导出**空** + 导入**409 只读**
//	    (数据在外部系统里,本域那几行是切换前的旧数据)。
//
//	判据只有一份:internal/archiveroute。
//
// # ⚠️ 导入 = **数据恢复**,不是业务重放
//
//	导入只往本地表里写行:不调开班/交班、不推 dapr 事件、
//	不改其它域的余额、不推进任何状态机。
//	详见 internal/masterdata/service/archive.go 的 Import 注释。
package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/YunBright/authkit/rbac"
	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/archiveroute"
	"github.com/YunBright/supertrade/internal/masterdata/model"
	"github.com/YunBright/supertrade/internal/masterdata/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// ArchiveHandler 是 master-data 的归档导出 / 导入 handler。
//
// ⚠️ 与 Handler **分开**:归档是一组独立的端点(独立的契约 + 独立的守卫),
// 塞进主 Handler 会让主 Handler 的构造参数与字段跟着长。
type ArchiveHandler struct {
	archive *service.ArchiveService
	// resolver 是 per-branch scope 解析器(userd 权限矩阵)。
	resolver rbac.ScopeResolver
	logger   *slog.Logger
}

// NewArchiveHandler 构造 ArchiveHandler。
//
// ⚠️ resolver 为 nil ⇒ 守门一律 403(与主 Handler 的口径一致)。
func NewArchiveHandler(archive *service.ArchiveService, resolver rbac.ScopeResolver, logger *slog.Logger) *ArchiveHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &ArchiveHandler{archive: archive, resolver: resolver, logger: logger}
}

// RegisterRoutes 挂载归档端点。
func (h *ArchiveHandler) RegisterRoutes(r gin.IRouter) {
	r.GET("/archive/export",
		rbac.RequireScopeWithBranch(service.ScopeStoreView, branchFromHeader, h.resolver),
		h.exportArchive)
	r.POST("/archive/import",
		rbac.RequireScopeWithBranch(service.ScopeStoreManage, branchFromHeader, h.resolver),
		h.importArchive)
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
			"code": "bad_request",
			"message": "scope 必填(只能是 " + service.ScopeStores + " / " +
				service.ScopeEmployees + " / " + service.ScopeShifts + ")",
		})
		return
	}
	res, err := h.archive.Export(c.Request.Context(), branchID, scope)
	if err != nil {
		h.mapArchiveErr(c, err)
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
	var req service.ImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "bad_request", "message": err.Error(),
		})
		return
	}
	res, err := h.archive.Import(c.Request.Context(), branchID, req)
	if err != nil {
		h.mapArchiveErr(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// mapArchiveErr 把归档端点的错误映射为 HTTP 状态码 + JSON body。
//
// ⚠️ **503 而不是空列表**:问不到数据源是一次链路故障,
//
//	回落成本地 = 把一次故障显示成「该门店没有门店档案」—— 与故障无关的结论。
func (h *ArchiveHandler) mapArchiveErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, model.ErrInvalidInput):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "bad_request", "message": err.Error(),
		})
	case errors.Is(err, archiveroute.ErrExternalReadOnly):
		// 409:状态冲突(这个门店的账在外部系统里,不是权限问题)。
		//
		// ⚠️ code 是**跨服务统一**的 member_source_is_external:
		//   前端 error_mapper 用同一个 code 判"这个门店的数据在外部系统里",
		//   不必为每个服务各认一个字符串。
		//
		// ⚠️ message **不暴露**内部 app-id(cube-sixun-ysx-fb 之类):
		//   那是运维信息,不该出现在给终端用户的报文里。
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"code":    "member_source_is_external",
			"message": "该门店的主数据来自外部数据源,请在原系统操作",
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
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code": "cube_unavailable", "message": "数据源路由暂不可用,请稍后重试",
		})
	default:
		h.logger.Error("master-data 归档端点未映射错误", "err", err)
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
			"code": "internal_error", "message": "系统繁忙,请稍后重试",
		})
	}
}
