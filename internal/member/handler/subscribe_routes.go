// Package handler / subscribe_routes.go —— Dapr pub/sub 订阅。
//
// Dapr sidecar 调 GET /dapr/subscribe 拿订阅清单,然后对每个订阅 topic
// POST /events/<topic> 推送 CloudEvents。
//
// 本服务订阅 1 个 topic:
//
//	sale.paid —— 销售成交 ⇒ 累计会员积分(幂等)
//
// ⚠️ 这两个端点**必须在 auth 中间件之前注册**:
//
//	sidecar 调它们**不带 Authorization 头**(它是 sidecar 自己,不是终端用户),
//	带上鉴权中间件就会 401 ⇒ sidecar 永远拿不到订阅清单 ⇒ 整条 pubsub 链路静默失效。
//	(2026-10-07 生产实测 fresh-meat / stocktake 都中招过。)
//	⇒ 必须走 cmdbootstrap.Options.RegisterDapr。
package handler

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/member/service"
	"github.com/YunBright/supertrade/pkg/eventbus"
)

// SubscribeTopics 是本服务订阅的 Dapr topic 列表。
//
// 加新 topic:① 这里加一行 ② dispatchSubscribe 加一个 case
// ③ 同步 docs/EVENT-CATALOG.md。
var SubscribeTopics = []string{
	service.TopicSalePaid,
}

// Subscription 是 /dapr/subscribe 返回结构。
type Subscription struct {
	PubsubName string   `json:"pubsubname"`
	Topic      string   `json:"topic"`
	Route      string   `json:"route"`
	Metadata   struct{} `json:"metadata,omitempty"`
}

// RegisterSubscribeRoutes 把 Dapr 订阅端点挂到 gin engine。
//
//	GET  /dapr/subscribe  → 订阅清单
//	POST /events/<topic>  → 事件分发
func (h *Handler) RegisterSubscribeRoutes(r *gin.Engine, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	r.GET("/dapr/subscribe", h.subscribeList)
	for _, t := range SubscribeTopics {
		r.POST("/events/"+t, h.makeTopicHandler(t, logger))
	}
}

// subscribeList 返回 /dapr/subscribe 注册清单。
func (h *Handler) subscribeList(c *gin.Context) {
	pubsub := c.Query("pubsubname")
	if pubsub == "" {
		pubsub = eventbus.Name
	}
	out := make([]Subscription, 0, len(SubscribeTopics))
	for _, t := range SubscribeTopics {
		out = append(out, Subscription{
			PubsubName: pubsub,
			Topic:      t,
			Route:      "/events/" + t,
		})
	}
	c.JSON(http.StatusOK, out)
}

// makeTopicHandler 单 topic 处理器。
func (h *Handler) makeTopicHandler(topic string, logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		h.dispatchSubscribe(c, topic, logger)
	}
}

// dispatchSubscribe 解 CloudEvents envelope → 分发到对应 service 方法。
//
// ⚠️ 任何 error 都返 200 + log warn,**不返 5xx** ——
//
//	dapr 会按状态码重试,5xx 会让这条事件无限重试,把整条 bus 卡住。
func (h *Handler) dispatchSubscribe(c *gin.Context, topic string, logger *slog.Logger) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		logger.Warn("subscribe: read body", "topic", topic, "err", err)
		c.Status(http.StatusBadRequest)
		return
	}
	defer c.Request.Body.Close()

	// 解 envelope(CloudEvents 风格);失败时降级从 raw body 直接拿字段。
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(body, &env)

	switch topic {
	case service.TopicSalePaid:
		h.onSalePaid(c, env.Data, body, logger)
	default:
		logger.Warn("subscribe: unknown topic", "topic", topic)
		c.Status(http.StatusOK)
	}
}

// onSalePaid 解 envelope → 调 svc.OnSalePaid 累计积分。
func (h *Handler) onSalePaid(c *gin.Context, data json.RawMessage, rawBody []byte, logger *slog.Logger) {
	var payload service.SalePaidPayload
	if len(data) > 0 {
		if err := json.Unmarshal(data, &payload); err != nil {
			logger.Warn("subscribe: parse sale.paid envelope data", "err", err)
		}
	} else {
		// 兜底:dapr 直接投递原始 JSON,无 envelope 包裹。
		_ = json.Unmarshal(rawBody, &payload)
	}
	if payload.BranchID == "" || payload.OpID == "" {
		logger.Warn("subscribe: sale.paid 缺 branch_id / op_id", "body", string(rawBody))
		c.Status(http.StatusNoContent) // 200/204 都表示 ack
		return
	}
	res, err := h.svc.OnSalePaid(c.Request.Context(), payload)
	if err != nil {
		logger.Warn("subscribe: OnSalePaid",
			"branch_id", payload.BranchID,
			"op_id", payload.OpID,
			"err", err)
		c.Status(http.StatusOK)
		return
	}
	logger.Info("subscribe: OnSalePaid ok",
		"branch_id", payload.BranchID,
		"op_id", payload.OpID,
		"member_id", payload.MemberID,
		"points", res.Points.String(),
		"skipped", res.Skipped,
		"replayed", res.Replayed)
	c.Status(http.StatusOK)
}
