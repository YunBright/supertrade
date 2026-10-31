// Package handler / monthly.go —— 企微审批月度总表的内部维护端点（计划 §9）。
//
// # 为什么是「内部」端点
//
// 重建会重算整个门店的月度行，是运维动作（改正则/补日期写法后跑一次），
// 不是店长的日常操作。挂在 RegisterDapr 的免鉴权窗口（同 purge），
// 公网不可达靠 nginx 的 `location ^~ .../internal/ { return 404; }`。
//
// ⚠️ 必须带 `confirm=true`：不带就 400。这是防误触的最后一道闸——
// 重建本身幂等，但一次几十万行的全量重算不该被"手滑点一下"触发。
package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// RegisterMonthlyInternalRoutes 挂月度总表重建端点。
//
//	POST /internal/monthly-items/rebuild?branch_id=<id>&confirm=true
func (h *Handler) RegisterMonthlyInternalRoutes(r gin.IRouter) {
	r.POST("/internal/monthly-items/rebuild", h.RebuildMonthlyItems)
}

// RebuildMonthlyItems 按当前解析口径重建某门店的月度总表派生列。
func (h *Handler) RebuildMonthlyItems(c *gin.Context) {
	if strings.TrimSpace(c.Query("confirm")) != "true" {
		writeError(c, http.StatusBadRequest, "confirm_required",
			"重建会重算该门店全部月度行，需显式 confirm=true")
		return
	}
	branchID := strings.TrimSpace(c.Query("branch_id"))
	if branchID == "" {
		writeError(c, http.StatusBadRequest, "branch_required", "branch_id 必填")
		return
	}
	n, err := h.svc.RebuildMonthlyItems(c.Request.Context(), branchID)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "branch_id": branchID, "rebuilt": n})
}
