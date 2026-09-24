// Package events 处理 Dapr pub/sub 订阅。
//
// Dapr sidecar 会：
//   - 调 GET /dapr/subscribe 拿订阅清单
//   - 对每个订阅 topic POST /events/<topic> 推送 CloudEvents
//
// 我们把所有 topic 收口到 dispatch()：先解 CloudEvents envelope，再按
// fanout.Classify 决定路由策略，最后调用 Registry.FanoutToUser/Tenant/All。
package events

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/notification-gateway/fanout"
	"github.com/YunBright/supertrade/internal/notification-gateway/gateway"
	"github.com/YunBright/supertrade/internal/notification-gateway/observability"
	"github.com/YunBright/supertrade/internal/notification-gateway/wsmsg"
)

// Topics 是本服务订阅的全部 Dapr pub/sub topic。
//
// 这些 topic 名严格对齐 EVENT-CATALOG.md §2.12~2.14：
//   - stocktake.line.added / updated / deleted   → §2.12
//   - stocktake.header.submitted / approved      → §2.13
//   - stocktake.plan_item.added                  → §2.13
//   - auth.user.access_changed                   → §2.14
//
// Topic 命名约定：<domain>.<entity>.<verb>（小写 + 点号），与 CloudEvents type 对齐。
//
// Phase 3:auth.user.permissions_changed 合并为 auth.user.access_changed(payload
// 同时含 changed_scopes / changed_roles / changed_branches / changed_default_branch
// / snapshot_version)。订阅清单必须同步切换,否则前端收不到事件。
var Topics = []string{
	"stocktake.line.added",
	"stocktake.line.updated",
	"stocktake.line.deleted",
	"stocktake.header.submitted",
	"stocktake.header.approved",
	"stocktake.plan_item.added",
	"auth.user.access_changed",
}

// Subscription 是 /dapr/subscribe 返回结构。
type Subscription struct {
	PubsubName string   `json:"pubsubname"`
	Topic      string   `json:"topic"`
	Route      string   `json:"route"`
	Metadata   struct{} `json:"metadata,omitempty"`
}

// Handler 是 /events/<topic> 的统一处理器。
type Handler struct {
	registry *gateway.Registry
	metrics  *observability.Metrics
	logger   *slog.Logger
}

// NewHandler 构造 Handler。
func NewHandler(reg *gateway.Registry, m *observability.Metrics, logger *slog.Logger) *Handler {
	return &Handler{registry: reg, metrics: m, logger: logger}
}

// RegisterRoutes 把所有 topic handler 挂到 gin engine。
//
// /dapr/subscribe → Subscribe
// /events/<topic> → dispatch
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	r.GET("/dapr/subscribe", h.Subscribe)
	for _, t := range Topics {
		path := "/events/" + t
		r.POST(path, h.makeTopicHandler(t))
	}
}

// Subscribe 返回 /dapr/subscribe 注册清单。
func (h *Handler) Subscribe(c *gin.Context) {
	pubsub := c.Query("pubsubname")
	if pubsub == "" {
		pubsub = "pubsub"
	}
	out := make([]Subscription, 0, len(Topics))
	for _, t := range Topics {
		s := Subscription{PubsubName: pubsub, Topic: t, Route: "/events/" + t}
		out = append(out, s)
	}
	c.JSON(http.StatusOK, out)
}

// makeTopicHandler 单个 topic 处理器（共用 dispatch 逻辑）。
func (h *Handler) makeTopicHandler(topic string) gin.HandlerFunc {
	return func(c *gin.Context) {
		h.dispatch(c, topic)
	}
}

// dispatch 解 CloudEvents envelope → fanout → registry.Fanout*。
func (h *Handler) dispatch(c *gin.Context, topic string) {
	h.metrics.EventsReceived.Add(1)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		h.logger.Warn("events: read body", "topic", topic, "err", err)
		c.String(http.StatusBadRequest, "read body: %v", err)
		return
	}
	defer c.Request.Body.Close()

	var env wsmsg.Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		// 兼容 Dapr 原始 payload：直接从 topic 推断 type
		env = wsmsg.Envelope{Type: topic, Data: body}
	}
	if env.Type == "" {
		env.Type = topic
	}

	strategy := fanout.Classify(env)
	switch strategy {
	case fanout.StrategyUser:
		uid := envelopeUserID(env)
		if uid == "" {
			h.metrics.EventsSkipped.Add(1)
			h.logger.Warn("events: auth.user.access_changed missing user_id", "topic", topic)
			c.Status(http.StatusNoContent)
			return
		}
		h.registry.FanoutToUser(uid, env)
	case fanout.StrategyGlobal:
		h.registry.FanoutAll(env)
	default:
		h.registry.FanoutToTenant(env)
	}
	h.metrics.EventsFannedOut.Add(1)
	c.Status(http.StatusOK)
}

// envelopeUserID 解 envelope.Data 顶层 user_id。
func envelopeUserID(env wsmsg.Envelope) string {
	if len(env.Data) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(env.Data, &m); err != nil {
		return ""
	}
	if s, ok := m["user_id"].(string); ok {
		return s
	}
	return ""
}
