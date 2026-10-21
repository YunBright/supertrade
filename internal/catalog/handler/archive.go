// Package handler / archive.go —— 档案导入 HTTP handler。
//
// 路由:
//
//	POST /archive/import
//	  X-Branch-ID: <目标门店>
//	  body: { source_branch_id, mode, scopes[], data{...} }
//
// 守门:`product:manage` scope(per-branch)。
//
// ⚠️ **为什么不用 admin 角色守门**:auth 建服务账号时"不分配任何角色(least privilege)"
// ⇒ 服务账号过不了 `RequireRole("admin")`。而这个端点就是给同步服务账号用的。
//
// ⚠️ **门店只从 header 取**,不从 body 取 —— body 里带一份门店,
// A 店的登录态就能把数据写进 B 店。
package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/catalog/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// importReq POST /archive/import 的入参。
type importReq struct {
	SourceBranchID string             `json:"source_branch_id"`
	Mode           string             `json:"mode"`
	Scopes         []string           `json:"scopes"`
	Data           service.ImportData `json:"data"`
}

// importArchive POST /archive/import
func (h *Handler) importArchive(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
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
	res, err := h.archive.Import(c.Request.Context(), branchID, service.ImportRequest{
		SourceBranchID: req.SourceBranchID,
		Mode:           req.Mode,
		Scopes:         req.Scopes,
		Data:           req.Data,
	})
	if err != nil {
		h.mapErr(c, err)
		return
	}
	inserted, updated, skipped := res.Total()
	c.JSON(http.StatusOK, gin.H{
		"source_branch_id": res.SourceBranchID,
		"target_branch_id": res.TargetBranchID,
		"mode":             res.Mode,
		"by_scope":         res.ByScope,
		"inserted":         inserted,
		"updated":          updated,
		"skipped":          skipped,
		"warnings":         res.Warnings,
		"errors":           res.Errors,
	})
}
