package gateway

import (
	"encoding/json"
	"strings"

	"github.com/YunBright/supertrade/internal/notification-gateway/wsmsg"
)

// TenantRouter 决定一个事件是否能投递到某个 client。
//
// 关键约束：
//   - tenant_id 不匹配 → 永远不投递
//   - branch_id 必须落在 client 的"有效门店列表"内（home + additional）
//   - 用户级事件（如 auth.user.access_changed）只投递到该 user
//   - tenant_admin 类 scope 拥有者跨门店事件无需校验 branch
//
// 事件 payload 约定：所有 stocktake.* / 业务事件 data 含 branch_id 字段。
// 用户级事件 data 含 user_id 字段。
type TenantRouter struct {
	// tenantAdminScopes：拥有此 scope 的用户被视为租户管理员，
	// 跨门店事件无需校验 branch。
	tenantAdminScopes map[string]bool
}

// NewTenantRouter 构造路由。
func NewTenantRouter() *TenantRouter {
	return &TenantRouter{
		tenantAdminScopes: map[string]bool{
			"tenant.admin": true,
			"tenant:admin": true,
		},
	}
}

// shouldDeliver 判定 client 是否应收到此 envelope。
func (r *TenantRouter) shouldDeliver(env wsmsg.Envelope, c *Client) bool {
	if c.Claims() == nil {
		return false
	}
	switch {
	case strings.HasPrefix(env.Type, "stocktake."):
		return r.matchStocktake(env, c)
	case strings.HasPrefix(env.Type, "auth.user."):
		return r.matchAuthUser(env, c)
	default:
		// 未知事件 → 保守同 tenant 即可
		return c.TenantID() != "" && c.TenantID() == envelopeTenantID(env)
	}
}

// matchStocktake 业务事件路由。
func (r *TenantRouter) matchStocktake(env wsmsg.Envelope, c *Client) bool {
	tenantID := envelopeTenantID(env)
	branchID := envelopeBranchID(env)

	// tenant 不匹配 → 不投递
	if tenantID != "" && tenantID != c.TenantID() {
		return false
	}
	// tenant 管理员放行
	if c.HasScope("tenant.admin") || c.HasScope("tenant:admin") {
		return true
	}
	// 否则 branch 必须落在有效门店
	if branchID == "" {
		// 没指定 branch → 同 tenant 全发（保守）
		return true
	}
	for _, b := range c.EffectiveBranches() {
		if b == branchID {
			return true
		}
	}
	return false
}

// matchAuthUser 用户级事件路由。
func (r *TenantRouter) matchAuthUser(env wsmsg.Envelope, c *Client) bool {
	uid := envelopeUserID(env)
	if uid == "" {
		return false
	}
	return uid == c.UserID()
}

// envelopeTenantID 从 envelope.data 里取 tenant_id 字段（顶层 snake_case）。
func envelopeTenantID(env wsmsg.Envelope) string { return envelopeDataString(env, "tenant_id") }

// envelopeBranchID 从 envelope.data 里取 branch_id 字段。
func envelopeBranchID(env wsmsg.Envelope) string { return envelopeDataString(env, "branch_id") }

// envelopeUserID 从 envelope.data 里取 user_id 字段。
func envelopeUserID(env wsmsg.Envelope) string { return envelopeDataString(env, "user_id") }

// envelopeDataString 通用解 envelope.data 顶层 string 字段。
func envelopeDataString(env wsmsg.Envelope, key string) string {
	if len(env.Data) == 0 {
		return ""
	}
	var data map[string]any
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return ""
	}
	if v, ok := data[key].(string); ok {
		return v
	}
	return ""
}
