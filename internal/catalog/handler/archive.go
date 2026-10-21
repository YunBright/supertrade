// Package handler / archive.go —— 档案导入 / 导出 HTTP handler。
//
// 路由:
//
//	POST /archive/import
//	  X-Branch-ID: <目标门店>
//	  body: { source_branch_id, mode, scopes[], data{...} }
//	GET  /archive/export?scope=&offset=&limit=
//	  X-Branch-ID: <被导出的门店>
//
// 守门:`product:manage` scope(导入)/ `product:view` scope(导出)。
//
// ⚠️ **为什么不用 admin 角色守门**:auth 建服务账号时"不分配任何角色(least privilege)"
// ⇒ 服务账号过不了 `RequireRole("admin")`。而这个端点就是给同步服务账号用的。
//
// ⚠️ **门店只从 header 取**,不从 body 取 —— body 里带一份门店,
// A 店的登录态就能把数据写进 B 店。
package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/catalog/model"
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

// exportArchive GET /archive/export?scope=&offset=&limit=
//
// # 输出 archivedto 契约(与上游导出端点**同一份字段名**)
//
//	{ scope, branch_id, cube_source, as_of, rows[], count, total, offset, limit }
//
// ⚠️ **数据来源按门店分流**(见 service/source.go):
//
//	配了源 ⇒ 转发上游导出端点(body 透传)
//	没配源 ⇒ 从本域 PG 读,组装成同一份契约
//
// ⚠️ **配了源但上游没有 ⇒ 导出空数据,不回落本地**:
// 回落 = 把"外部系统里没有这份档案"说成"本地也没有"。
func (h *Handler) exportArchive(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "branch_required",
			"message": "X-Branch-ID header 必填",
		})
		return
	}
	scope := strings.TrimSpace(c.Query("scope"))
	if scope == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "bad_request",
			"message": "scope 必填(categories / suppliers / products / item_barcodes)",
		})
		return
	}
	offset, limit := parseExportPaging(c)
	page, err := h.archive.Export(c.Request.Context(), service.ExportInput{
		BranchID: branchID,
		// ⚠️ 调用者的 Authorization **原样**转给上游:
		// 上游按它做 per-branch scope 守门(cube:read)。
		Bearer: c.Request.Header.Get("Authorization"),
		Scope:  scope,
		Offset: offset,
		Limit:  limit,
	})
	if err != nil {
		h.mapArchiveErr(c, err)
		return
	}
	c.JSON(http.StatusOK, page)
}

// parseExportPaging 解析导出端点的分页参数。
//
// ⚠️ 上限与上游导出端点一致(10000):再大就撑爆 sidecar 的内存
// (门店 00 实测 27565 个商品 ⇒ 一次拉完会让 sidecar 内存爆)。
func parseExportPaging(c *gin.Context) (offset, limit int) {
	offset, _ = strconv.Atoi(c.Query("offset"))
	if offset < 0 {
		offset = 0
	}
	limit, _ = strconv.Atoi(c.Query("limit"))
	if limit <= 0 {
		limit = service.ExportDefaultLimit
	}
	if limit > service.ExportMaxLimit {
		limit = service.ExportMaxLimit
	}
	return offset, limit
}

// mapArchiveErr 把档案端点的错误映到 HTTP。
func (h *Handler) mapArchiveErr(c *gin.Context, err error) {
	if h.writeSourceErr(c, err) {
		return
	}
	if errors.Is(err, service.ErrUnsupportedExportScope) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "unsupported_export_scope",
			"message": err.Error(),
		})
		return
	}
	if errors.Is(err, model.ErrInvalidInput) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "bad_request",
			"message": err.Error(),
		})
		return
	}
	h.mapErr(c, err)
}
