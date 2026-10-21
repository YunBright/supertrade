package handler

// 会员档案的**归档导出**端点（`GET /archive/export`）。
//
// # 范式（2026-10-22 决策）
//
//	配了数据源 ⇒ **原样转发**上游导出（cube-router 的 /v1/export，字段名逐字一致）
//	没配数据源   ⇒ 从本域 PG 导出同一份契约
//	问不到路由   ⇒ 503（**不许**回落本地：回落 = 把一次故障显示成"这个门店没会员"）
//
// # 谁用这个端点
//
//	cube-async（服务层数据搬运）：源门店 00（配了源 ⇒ 走思迅）→ 目标门店 10（本地 PG）。
//
// ⇒ 它是"门店之间搬基础数据"这条链路的**读**端。

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/archivedto"
	"github.com/YunBright/supertrade/internal/member/model"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// getArchiveExport GET /archive/export?scope=&offset=&limit=
//
// 守卫：member:view（导出是读操作，与既有的 /members/export 同档）。
func (h *Handler) getArchiveExport(c *gin.Context) {
	scope := strings.TrimSpace(c.Query("scope"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	limit, _ := strconv.Atoi(c.Query("limit"))

	page, err := h.svc.ExportArchive(
		c.Request.Context(),
		middleware.SingleBranchFromCtx(c),
		c.GetHeader("Authorization"),
		scope,
		offset,
		limit,
	)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, page)
}

// 编译期断言：导出契约的行类型来自 archivedto（导出与导入共用一份字段名）。
var _ = archivedto.ScopeMembers
var _ = model.ScopeView
