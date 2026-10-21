// Package handler / archive.go —— 库存快照的**导入**端点（一次性归档同步用）。
//
// 路由(nginx 已剥过 /api/v1/inventory/)：
//
//	POST /archive/import   导入库存快照(inventory:manage)
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

// importArchive POST /archive/import
//
// 守门:`inventory:manage` scope(per-branch)。
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
