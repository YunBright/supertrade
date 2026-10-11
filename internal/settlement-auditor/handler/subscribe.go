// Package handler / subscribe.go —— Dapr pub/sub 订阅 + dapr job 端点。
//
// 订阅(RegisterDapr 窗口,auth 中间件之前注册):
//
//   - wecom.approval.status_changed ← wechat-bot(企微审批状态回调)
//   - wecom.approval.command_result ← wechat-bot(命令执行结果)
//   - auth.user.access_changed      ← auth(scope 缓存失效)
//
// dapr job:
//
//	POST /job/settlement-auditor-rescan —— 扫卡死实例重跑(main 启动时注册
//	v1.0-alpha1/jobs/settlement-auditor-rescan,@every 5m)。
//
// 错误处理:任何 error 返 200 + log warn,避免 dapr 无限重试。
package handler

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/settlement-auditor/service"
	"github.com/YunBright/supertrade/pkg/eventbus"
)

// SubscribeTopics 本服务订阅的 topic 列表。
var SubscribeTopics = []string{
	service.TopicWecomStatusChanged,
	service.TopicWecomCommandResult,
	service.TopicAuthAccessChanged,
}

// Subscription 是 /dapr/subscribe 返回结构。
type Subscription struct {
	PubsubName string `json:"pubsubname"`
	Topic      string `json:"topic"`
	Route      string `json:"route"`
}

// RegisterSubscribeRoutes 挂 /dapr/subscribe + /events/<topic> + /job/*。
// 必须在 auth 中间件之前调用(sidecar 调用不带 Authorization 头)。
func (h *Handler) RegisterSubscribeRoutes(r *gin.Engine, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	r.GET("/dapr/subscribe", h.subscribeList)
	for _, t := range SubscribeTopics {
		r.POST("/events/"+t, h.makeTopicHandler(t, logger))
	}
	r.POST("/job/settlement-auditor-rescan", h.rescanJob(logger))
}

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

func (h *Handler) makeTopicHandler(topic string, logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			logger.Warn("subscribe: read body", "topic", topic, "err", err)
			c.Status(http.StatusOK)
			return
		}
		defer c.Request.Body.Close()

		var env struct {
			Data json.RawMessage `json:"data"`
		}
		_ = json.Unmarshal(body, &env)
		data := env.Data
		if len(data) == 0 {
			data = body // 无 envelope 兜底
		}

		switch topic {
		case service.TopicWecomStatusChanged:
			var ev service.WecomStatusChanged
			if err := json.Unmarshal(data, &ev); err != nil {
				logger.Warn("subscribe: parse status_changed", "err", err)
				break
			}
			if err := h.svc.OnStatusChanged(c.Request.Context(), &ev); err != nil {
				logger.Warn("subscribe: OnStatusChanged", "sp_no", ev.SpNo, "err", err)
			}
		case service.TopicWecomCommandResult:
			var res service.WecomCommandResult
			if err := json.Unmarshal(data, &res); err != nil {
				logger.Warn("subscribe: parse command_result", "err", err)
				break
			}
			if err := h.svc.OnCommandResult(c.Request.Context(), &res); err != nil {
				logger.Warn("subscribe: OnCommandResult", "command_id", res.CommandID, "err", err)
			}
		case service.TopicAuthAccessChanged:
			var p struct {
				UserID   string `json:"user_id"`
				BranchID string `json:"branch_id"`
			}
			if err := json.Unmarshal(data, &p); err != nil || p.UserID == "" {
				logger.Warn("subscribe: parse access_changed", "err", err)
				break
			}
			h.svc.InvalidateScopeCache(p.UserID, p.BranchID)
		default:
			logger.Warn("subscribe: unknown topic", "topic", topic)
		}
		c.Status(http.StatusOK)
	}
}

// rescanJob POST /job/settlement-auditor-rescan(dapr job 触发)。
func (h *Handler) rescanJob(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		n, err := h.svc.RescanStuck(c.Request.Context())
		if err != nil {
			logger.Warn("rescan stuck failed", "err", err)
			c.Status(http.StatusOK) // 不返 5xx,避免 dapr job 重试风暴
			return
		}
		if n > 0 {
			logger.Info("rescan stuck triggered", "count", n)
		}
		c.JSON(http.StatusOK, gin.H{"rescanned": n})
	}
}
