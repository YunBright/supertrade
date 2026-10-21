// Package handler / archive_export.go —— 价格域的归档导出端点。
//
// 路由:
//
//	GET /archive/export?scope=prices|promotions&offset=&limit=&as_of=yyyy-mm-dd
//	  X-Branch-ID: <目标门店>
//
// # 契约
//
// 输出 **archivedto.ExportPage**(字段名与 cube-router 的 GET /v1/export **逐字相同**):
//
//	scope / branch_id / cube_source / as_of / rows / count / total / offset / limit
//
// ⚠️ 字段名只有一处定义(见 internal/archivedto/dto.go):
// 三方(cube-router / cubesync / pricing)各写一份 struct 时,
// JSON 对不认识的字段**静默忽略** ⇒ "导出了 27565 条、导入后价格全是空的",
// 而且**没有一行报错**。
//
// # 数据来源:与查价**同一套判据**
//
//	门店配了数据源 ⇒ cube(该门店的实时数据)
//	没配数据源       ⇒ 本域 PG
//
// ⚠️ **配了源但源里没有 ⇒ 导出空数据,不许回落本地**:
// 回落 = 把"源库没有"说成"本域有旧数据",而真相是"那边没有"。
//
// ⚠️ 解析失败(问不到路由)/ 数据源被关停 ⇒ **503**,不回落本地。
//
// # 守门
//
// `pricing:view` —— 导出是**读**操作(与 /prices/export 同一档)。
package handler

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/archivedto"
	"github.com/YunBright/supertrade/internal/pricing/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// archiveExportResponse 是导出响应。
//
// ⚠️ 内嵌 archivedto.ExportPage ⇒ 字段名只有一处定义;
// 额外带 as_of(让"导了哪一天的数据"从响应里就能看出来,与 cube-router 一致)。
type archiveExportResponse struct {
	archivedto.ExportPage
	AsOf string `json:"as_of"`
}

// getArchiveExport GET /archive/export?scope=&offset=&limit=&as_of=
func (h *Handler) getArchiveExport(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "branch_required", "message": "X-Branch-ID header 必填",
		})
		return
	}
	scope := strings.TrimSpace(c.Query("scope"))
	if scope == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "missing_scope", "message": "scope query 必填(pricing 域:" +
				strings.Join(service.ExportScopeNames(), ",") + ")",
		})
		return
	}
	// ⚠️ 基准日必须在**取数之前**校验:非法值当场 400 —— 不能静默回落到今天,
	// 否则"这批数据是哪一天的"不可知(促销是时效数据)。
	asOf, err := archivedto.NormalizeAsOf(c.Query("as_of"), time.Now())
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "bad_as_of", "message": "as_of 必须是 yyyy-mm-dd",
		})
		return
	}

	offset, limit := parseExportPage(c.Query("offset"), c.Query("limit"))
	page, err := h.prices.ExportArchive(c.Request.Context(), branchID, scope, offset, limit, asOf)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, archiveExportResponse{ExportPage: *page, AsOf: asOf})
}

// parseExportPage 解析 offset / limit。
//
// ⚠️ 非法值**按缺省处理**(不报错):分页参数是"取多少"而不是"要什么",
// 而调用方(POS 离线包 / 同步器)少传一个参数不该让整次导出失败。
//
// ⚠️ limit 上限是 service.ExportMaxLimit ——
// 一次拉 27565 行会让 sidecar 内存爆,而且 cubeclient 的单次调用只有 10s 超时。
func parseExportPage(rawOffset, rawLimit string) (offset, limit int) {
	offset, _ = strconv.Atoi(strings.TrimSpace(rawOffset))
	if offset < 0 {
		offset = 0
	}
	limit, _ = strconv.Atoi(strings.TrimSpace(rawLimit))
	if limit <= 0 {
		limit = service.ExportDefaultLimit
	}
	if limit > service.ExportMaxLimit {
		limit = service.ExportMaxLimit
	}
	return offset, limit
}
