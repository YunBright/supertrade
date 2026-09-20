package gateway

import "github.com/YunBright/supertrade/internal/notification-gateway/wsmsg"

// Registry 把 Hub 暴露为按 user/tenant 维度的查询接口。
//
// 设计：Registry 不持有状态 —— 状态都在 Hub 里。
// 提供更语义化的查询 API（FanoutToUser / FanoutToTenant），便于 events 包调用。
type Registry struct {
	hub    *Hub
	router *TenantRouter
}

// NewRegistry 用 Hub 与 TenantRouter 构造 Registry。
func NewRegistry(hub *Hub, router *TenantRouter) *Registry {
	return &Registry{hub: hub, router: router}
}

// FanoutToUser 给指定用户的所有连接投递 envelope。
func (r *Registry) FanoutToUser(userID string, env wsmsg.Envelope) {
	target := func(c *Client) bool {
		return c.UserID() == userID
	}
	r.hub.Fanout(env, target)
}

// FanoutToTenant 给同一租户下所有满足 router 判定的连接投递。
//
// router.shouldDeliver 用于判定分支是否落在该 client 的有效门店内。
func (r *Registry) FanoutToTenant(env wsmsg.Envelope) {
	target := func(c *Client) bool {
		return r.router.shouldDeliver(env, c)
	}
	r.hub.Fanout(env, target)
}

// FanoutAll 全局广播（极少用；如系统公告）。
func (r *Registry) FanoutAll(env wsmsg.Envelope) {
	r.hub.Fanout(env, func(*Client) bool { return true })
}

// Hub 暴露底层 Hub（仅用于 /healthz 统计）。
func (r *Registry) Hub() *Hub {
	return r.hub
}
