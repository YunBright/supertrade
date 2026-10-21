// Package handler / branch_source.go —— 「这个门店配了哪个数据源」的查询端点。
//
// # 为什么要有这个端点
//
// supertrade 是 ERP，同时也是**别的 ERP 的代理**：某个门店一旦在 cube-router 里
// 配了数据源(cube-<family>-<version>-<sub>)，**这个门店的所有业务查询**
// 都要走那个数据源，而不是本域自己的 PG。
//
// 典型症状(2026-10-21 用户报)：富百家(00) / 佳乐(01) 配了数据源，
// 而 member 服务的会员档案列表只查自己的 PG ⇒ 列表空 ⇒ 前端显示
// 「该门店还没有会员档案」—— 而真相是"这个门店的会员在外部系统里"。
//
// ⇒ 各业务服务在读数据前先问一句"我这个门店有没有配数据源"：
//   - 配了 ⇒ 走 cube(cube-router → 该门店的 cube 实例)
//   - 没配 ⇒ 走本域 PG(自营店)
//
// # 为什么是 POST 而不是 GET
//
// 与既有的 `POST /v1/load` 同风格：调用方统一走 dapr service invocation
// (cubeclient 的 InvokeMethodWithContent 只支持带 body 的调用)。
//
// # 为什么"没配数据源"返 200 而不是 404
//
// 「这个门店没有配数据源」是**正常状态**(自营店就是这样)，不是错误。
// 返 404 会让调用方把"没配"与"路由坏了"混在一起——
// 历史上就因为把"没这条数据"和"链路故障"折叠成同一个错误，
// 让一次基础设施故障被显示成业务结论。
package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/cube-router/model"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// BranchSourcePath 是「门店数据源」查询端点的路径。
const BranchSourcePath = "/v1/branch-source"

// branchSourceResp 是响应体。
//
// ⚠️ configured=false 时**不是错误**：该门店没配数据源 ⇒ 调用方读本域 PG。
type branchSourceResp struct {
	// BranchID 是被查询的门店。
	BranchID string `json:"branch_id"`
	// Configured = 该门店在 branch_cube_sources 里有一行。
	Configured bool `json:"configured"`
	// Enabled = 那一行是否启用(false = 管理员临时关停 ⇒ 应当 503 而不是回落本地)。
	Enabled bool `json:"enabled"`
	// CubeSourceName 是数据源对应的 dapr app-id(没配时为空)。
	CubeSourceName string `json:"cube_source_name"`
}

// getBranchSource POST /v1/branch-source
//
// ⚠️ **不把 enabled=false 当成"没配"**：
// 管理员把某个门店的数据源关停(临时维护)，语义是"这个门店现在查不了"，
// 不是"这个门店没有数据源"⇒ 调用方要报 503，而不是悄悄回落到本地 PG
// (那会让用户看到一份"空列表"，以为是没会员)。
func (h *Handler) getBranchSource(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "branch_required",
			"message": "X-Branch-ID header 必填",
		})
		return
	}

	row, err := h.svc.Get(c.Request.Context(), branchID)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			// 没配数据源 = 正常状态(自营店)⇒ 200 + configured=false。
			c.JSON(http.StatusOK, branchSourceResp{BranchID: branchID})
			return
		}
		h.mapErr(c, err)
		return
	}

	c.JSON(http.StatusOK, branchSourceResp{
		BranchID:       branchID,
		Configured:     true,
		Enabled:        row.Enabled,
		CubeSourceName: row.CubeSourceName,
	})
}
