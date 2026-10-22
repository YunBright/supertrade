// Package handler / archive.go —— 库存快照的**导入 / 导出**端点（一次性归档同步用）。
//
// 路由(nginx 已剥过 /api/v1/inventory/)：
//
//	GET  /archive/export?scope=stock&offset=&limit=   导出库存快照(inventory:view)
//	POST /archive/import                              导入库存快照(inventory:manage)
//
// # 它与其它服务的导入端点是**同一套契约**
//
// 请求体 = `archivedto.ImportRequest`（导出方 cube-router / 编排方 cubesync /
// 导入方 inventory **共用一份字段名** —— 各写一份的后果是
// 「导入报告显示成功 N 条，但字段全是空的」**且零报错**）。
//
// # ⚠️ **目标门店只从 X-Branch-ID header 取**
//
// body 里带一份门店就能跨店写入 —— 直接 400。
//
// # ⚠️ 时效语义
//
// 导入的是**同步时刻的快照**（不是历史轨迹）：
// cube 的 `stock` model 是 live ⇒ 导出来就是那一刻的库存。
package handler

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/archivedto"
	"github.com/YunBright/supertrade/internal/inventory/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// importReq 是 POST /archive/import 的请求体。
//
// ⚠️ 与 catalog / pricing 的导入端点**同构**（同一份 archivedto 契约）。
type importReq struct {
	// SourceBranchID 是来源门店（仅用于审计与报告）。
	SourceBranchID string `json:"source_branch_id"`
	// Mode 是 incremental / overwrite。
	Mode string `json:"mode"`
	// Scopes 是本次导入的资料域。
	Scopes []string `json:"scopes"`
	// Data 是行数据。
	Data archivedto.ImportData `json:"data"`
}

// exportArchive GET /archive/export?scope=stock&offset=&limit=
//
// # 两条路径（判据见 internal/inventory/service/source.go）
//
//	门店配了数据源 ⇒ **转发** cube-router 的 GET /v1/export?scope=stock
//	                  （响应体**原样**透传；cube 里没有 ⇒ 导出空数据，不回落本地）
//	门店没配数据源   ⇒ 从本域 stock_levels 读，按**同一份契约**组装
//
// ⚠️ 门店只从 X-Branch-ID header 取（query / body 里**不认**门店）。
func (h *Handler) exportArchive(c *gin.Context) {
	branchID, ok := resolveBranch(c)
	if !ok {
		return
	}
	scope := strings.TrimSpace(c.Query("scope"))
	if scope == "" {
		// ⚠️ **不静默兜底成 stock**：cube-router 的同名端点对空 scope 是 400
		//   ⇒ 两边一致，调用方一眼能看出"scope 没传"。
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "unsupported_export_scope",
			"message": "scope 必填;可用:" + archivedto.ScopeStock,
		})
		return
	}
	// ⚠️ 基准日在**转发之前**校验（非法值当场 400，不静默回落到今天）。
	asOf, err := archivedto.NormalizeAsOf(c.Query("as_of"), time.Now())
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "bad_as_of",
			"message": "as_of 必须是 yyyy-mm-dd",
		})
		return
	}
	offset, limit := service.ParseExportPaging(c.Query("offset"), c.Query("limit"))

	// ⚠️ **增量水位**(可选):只导"这个时间点之后变过"的行。
	//   空 = 全量(默认行为不变)。
	//   ⇒ 源库 `t_im_branch_stock.oper_date` 一天只变 91 行(实测 23837 行)
	//   ⇒ 增量导出/写入省掉 99.6%。
	since, err := archivedto.NormalizeSince(c.Query("since"))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "bad_since",
			"message": err.Error(),
		})
		return
	}

	raw, err := h.stock.ExportStock(c.Request.Context(), service.ExportInput{
		BranchID: branchID,
		Scope:    scope,
		Offset:   offset,
		Limit:    limit,
		AsOf:     asOf,
		Since:    since,
		// ⚠️ **JWT 一起透传**：cube-router 的 /v1/export 挂 cube:read scope
		//   （per-branch 实时解析）⇒ 没有它就 401。
		Bearer: c.Request.Header.Get("Authorization"),
	})
	if err != nil {
		h.mapErr(c, err)
		return
	}
	// ⚠️ **原样写出**：转发路径的响应体是 cube-router 生成的
	//   （含 as_of 等字段）⇒ 重新组装会把它们悄悄丢掉。
	c.Data(http.StatusOK, "application/json; charset=utf-8", raw)
}

// importArchive POST /archive/import
//
// 守门:`inventory:manage` scope(per-branch)。
//
// ⚠️ **有源门店只读**（有源 ⇒ 409，code member_source_is_external）：
// 该门店的库存在外部系统里 ⇒ 往本域 PG 写一份"没人看的库存"，
// 而前台还显示"写成功了" ⇒ 比报错更糟。
func (h *Handler) importArchive(c *gin.Context) {
	branchID := strings.TrimSpace(middleware.SingleBranchFromCtx(c))
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "branch_required",
			"message": "X-Branch-ID header 必填",
		})
		return
	}
	var req importReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "bad_request",
			"message": err.Error(),
		})
		return
	}
	rep, err := h.stock.ImportStock(c.Request.Context(), service.StockImportInput{
		SourceBranchID: req.SourceBranchID,
		TargetBranchID: branchID,
		Mode:           req.Mode,
		Data:           req.Data,
	})
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, rep)
}
