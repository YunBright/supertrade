// Package handler / subscribe.go —— Dapr pub/sub 订阅。
//
// Dapr sidecar 调 GET /dapr/subscribe 拿订阅清单,然后对每个订阅 topic
// POST /events/<topic> 推送 CloudEvents。
//
// 本服务订阅 auth.user.access_changed(合并 events,取代旧的 permissions_changed):
//  - payload 含 user_id(必填)+ branch_id(可选,缺省清空 user 全部缓存)
//  - 收到后调 svc.InvalidateScopeCache(userID, branchID) 让下一次权限校验
//    走 userd 重新拉权限矩阵。
//
// 不订阅 stocktake.line.added 等本服务自己发的事件——dapr sidecar 默认不
// 收自己的发布,避免循环。
package handler

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
)

// SubscribeTopics 是本服务订阅的 Dapr topic 列表。
//
// 加新 topic:
//  1. 在这里加一行
//  2. 在 dispatch() switch 加一个 case(必要时)
//  3. 同步 docs/EVENT-CATALOG.md §2.14
var SubscribeTopics = []string{
	"auth.user.access_changed",
}

// Subscription 是 /dapr/subscribe 返回结构。
type Subscription struct {
	PubsubName string `json:"pubsubname"`
	Topic      string `json:"topic"`
	Route      string `json:"route"`
	Metadata   struct{} `json:"metadata,omitempty"`
}

// RegisterSubscribeRoutes 把 Dapr 订阅相关端点挂到 gin engine。
//
//	GET  /dapr/subscribe        → Subscribe(订阅清单)
//	POST /events/<topic>        → 事件分发
//
// 调用方(参考 cmd/stocktake/main.go)需要在 RegisterRoutes(r gin.IRouter) 之外,
// 单独调 RegisterSubscribeRoutes(r *gin.Engine) —— 因为 /dapr/subscribe 不能
// 挂在业务路由组上,要直接挂在 engine。
func (h *Handler) RegisterSubscribeRoutes(r *gin.Engine, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	r.GET("/dapr/subscribe", h.subscribeList)
	for _, t := range SubscribeTopics {
		path := "/events/" + t
		r.POST(path, h.makeTopicHandler(t, logger))
	}
}

// subscribeList 返回 /dapr/subscribe 注册清单。
func (h *Handler) subscribeList(c *gin.Context) {
	pubsub := c.Query("pubsubname")
	if pubsub == "" {
		pubsub = "pubsub"
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

// dispatchSubscribe 解 CloudEvents envelope → 调 svc.InvalidateScopeCache。
//
// envelope.data 是 JSON object,可能含:
//   - user_id    (必填) — 谁的权限变了
//   - branch_id  (可选) — 哪个分支上的权限变了;缺省清空该 user 全部缓存
//
// 错误处理:dapr sidecar 会按返回 HTTP code 重试(2xx → ack,4xx/5xx → retry);
// 这里任何 error 都返 200 + log warn,避免无限重试。
func (h *Handler) dispatchSubscribe(c *gin.Context, topic string, logger *slog.Logger) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		logger.Warn("subscribe: read body", "topic", topic, "err", err)
		c.String(http.StatusBadRequest, "read body: %v", err)
		return
	}
	defer c.Request.Body.Close()

	// 解 envelope(CloudEvents 风格);失败时降级从 raw body 直接拿字段。
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(body, &env)

	var payload struct {
		UserID   string `json:"user_id"`
		BranchID string `json:"branch_id"`
	}
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &payload); err != nil {
			logger.Warn("subscribe: parse payload", "topic", topic, "err", err)
		}
	} else {
		// 兜底:dapr 直接投递原始 JSON,无 envelope 包裹
		_ = json.Unmarshal(body, &payload)
	}

	if payload.UserID == "" {
		logger.Warn("subscribe: missing user_id", "topic", topic)
		c.Status(http.StatusNoContent) // 200 + 204 都表示 ack
		return
	}

	// 调用 service 失效缓存;branchID 空字符串 → InvalidateScopeCache 内部清空该 user 全部。
	h.svc.InvalidateScopeCache(payload.UserID, payload.BranchID)
	logger.Info("subscribe: scope cache invalidated",
		"topic", topic, "user_id", payload.UserID, "branch_id", payload.BranchID)
	c.Status(http.StatusOK)
}