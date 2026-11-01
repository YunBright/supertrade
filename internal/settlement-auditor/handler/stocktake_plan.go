// Package handler / stocktake_plan.go —— 手动生成「计划盘点单」端点。
package handler

import (
	"net/http"

	"github.com/YunBright/authkit/claims"
	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/pkg/middleware"
)

// GenerateStocktakePlan POST /settlement-approvals/:id/stocktake-plan
//
// 2026-11-01 用户口径：warn 级盘点类信号（数量偏离 / 无销售进货）
// 不再由流水线自动建计划盘点单，改由审批详情页由人触发。
// block 信号仍自动建单；本端点只在仅 warn 的场景下使用。
//
// 权限：scope settlement:audit（与提交/重跑同级，店长可操作）。
//
// ⚠️ 终态审批的 applicant_bearer 已清空，stocktake 建单要向 cube 拉商品信息
// ⇒ 必须把**当前调用者**的 Authorization 透传下去（见 service.createStocktakePlan）。
func (h *Handler) GenerateStocktakePlan(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "settlement:audit") {
		return
	}
	operator := ""
	if cl, ok := claims.FromContext(c.Request.Context()); ok && cl != nil {
		operator = cl.Sub
	}
	headerID, created, err := h.svc.GenerateStocktakePlan(
		c.Request.Context(), branchID, c.Param("id"), operator,
		c.Request.Header.Get("Authorization"))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "header_id": headerID, "created": created})
}
