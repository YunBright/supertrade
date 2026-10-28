// Package handler / admin_approve.go —— 管理员人工放行端点。
package handler

import (
	"net/http"

	"github.com/YunBright/authkit/claims"
	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/pkg/middleware"
)

// RoleAdmin 是"管理员"角色名（auth 的 roles 表字面量）。
const RoleAdmin = "admin"

// AdminApprove POST /settlement-approvals/:id/admin-approve
//
// 2026-10-28 用户口径：「在审批详情页针对被机器拒绝的增加『管理员』审批按钮」。
//
// # 权限（**双重**守门）
//
//	scope = settlement:audit（与提交/重跑同级）
//	角色  = admin
//
//	⚠️ **只守 scope 不够**：merchant（店长）持有 settlement:audit
//	（见 auth 迁移 001 的 merchant 清单）⇒ 店长能自己放行自己提交的���
//	⇒ 这等于"店长可以绕过机器审核"，与整个流程定制的初衷相反。
//
//	⇒ 角色校验放在**服务端**（不是只靠前端隐藏按钮）。
func (h *Handler) AdminApprove(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "settlement:audit") {
		return
	}
	cl, _ := claims.FromContext(c.Request.Context())
	if cl == nil || !cl.HasAnyRole(RoleAdmin) {
		writeError(c, http.StatusForbidden, "forbidden", "人工放行仅限管理员")
		return
	}
	if err := h.svc.AdminApprove(c.Request.Context(), branchID, c.Param("id"), cl.Sub); err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "id": c.Param("id")})
}
