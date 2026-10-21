// Package handler / permissions.go —— "我的权限"只读端点。
//
// # 为什么需要它
//
// POS 端要实现「按 F4 会员冲减积分时,若当前收银员没有 `member:point-deduct`
// 权限,就要求输入有权限的收银员工号+口令,临时放行一次」(图4)。
//
// ⚠️ **本体系的令牌里没有 scopes** —— per-branch 权限是**实时**解析的
// (userd /internal/users/{id}/permissions?branch_id=…),
// JWT 里的 scopes 恒为空数组
// ⇒ 客户端**自己算不出**当前收银员有没有某个 scope
// ⇒ 必须有个只读端点告诉它。
//
// # 契约
//
//	GET /permissions        (公网形态 GET /api/v1/member/permissions)
//
//	→ 200 {"branch_id":"10","scopes":["member:view","pos:sale",…]}
//
// ⚠️ 守门用 `member:view`:登录用户都能查"我在这个店有什么权限";
//
//	连 `member:view` 都没有的人(理论上不存在)拿不到这个集合。
//
// ⚠️ **查不到就返回空数组 + warnings**(不 500):
//
//	客户端要能区分"我没有这个权限"与"权限查不到" ——
//	后者**不能**当"没有权限"处理(否则收银员被平白要求授权,真因却是 userd 挂了)。
package handler

import (
	"net/http"
	"sort"
	"strings"

	"github.com/YunBright/authkit/userinfo"
	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/pkg/middleware"
)

// warnScopesUnavailable 是"权限矩阵查不到"的告警码。
const warnScopesUnavailable = "scopes_unavailable"

// permissionsResp 是 GET /permissions 的响应体。
type permissionsResp struct {
	// BranchID 是本次查询的门店(取 X-Branch-ID)。
	BranchID string `json:"branch_id"`
	// Scopes 是该工号在该门店的有效 scope 集合(**可能为空数组**)。
	Scopes []string `json:"scopes"`
	// Warnings 是"数据没拿全"的提示(查不到时含 scopes_unavailable)。
	//
	// ⚠️ 客户端靠它区分"没有这个权限"与"查不到权限"(后者不该弹授权框)。
	Warnings []string `json:"warnings,omitempty"`
}

// getPermissions GET /permissions —— 查"我在当前门店有哪些权限"。
//
// ⚠️ 只读端点:**不改任何状态**,也不需要 body。
func (h *Handler) getPermissions(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	resp := permissionsResp{BranchID: branchID, Scopes: []string{}}

	// ⚠️ users 为 nil ⇒ 查不到 ⇒ 空数组 + warnings(**不 500**)。
	//   (正常情况下守门中间件已经先 403 / 503 了,这里是兜底。)
	if h.users == nil {
		h.logger.Warn("member: userd 客户端未装配,查不到权限矩阵", "branch_id", branchID)
		resp.Warnings = append(resp.Warnings, warnScopesUnavailable)
		c.JSON(http.StatusOK, resp)
		return
	}

	// ⚠️ 用 **callerSub(c)**(当前操作人)当 userID —— 问的是"**我**有什么权限",
	//   不是"某个工号有什么权限"(那是 master-data 的活)。
	bp, err := h.users.GetBranchPermissions(c.Request.Context(), callerSub(c), branchID)
	if err != nil {
		// ⚠️ 查不到 **不是 500**:客户端要能区分"没权限"与"查不到"。
		h.logger.Warn("member: 查权限矩阵失败",
			"err", err, "branch_id", branchID, "user_id", callerSub(c))
		resp.Warnings = append(resp.Warnings, warnScopesUnavailable)
		c.JSON(http.StatusOK, resp)
		return
	}

	resp.Scopes = scopesOfBranch(bp, branchID)
	c.JSON(http.StatusOK, resp)
}

// scopesOfBranch 从 per-branch 矩阵里取出该门店的 scope 集合(去重 + 排序)。
//
// ⚠️ 过滤语义与 rbac.HasAnyScopeWithBranch **逐字一致**(只认 branch_id 对得上的行)
// —— 否则客户端拿到的"我有什么权限"会和服务端守门判定打架
// (客户端以为有权限 → 不弹授权框 → 服务端 403)。
//
// ⚠️ **空行 = 没有权限**(不是"不过滤"):
//
//	userd 对"没绑定这家店"的用户返 scopes=[] ⇒ 这里就是空数组。
func scopesOfBranch(bp *userinfo.BranchPermissions, branchID string) []string {
	out := make([]string, 0, 8)
	if bp == nil {
		return out
	}
	seen := make(map[string]struct{}, 8)
	for _, row := range bp.Branches {
		if branchID != "" && row.BranchID != branchID {
			continue
		}
		for _, s := range row.Scopes {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			if _, ok := seen[s]; ok {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	// 排序:让响应**稳定**(同一份权限每次返回同一个顺序,便于比对与测试)。
	sort.Strings(out)
	return out
}
