// Package handler / subscribe_routes.go —— Dapr pub/sub 订阅。
//
// Dapr sidecar 调 GET /dapr/subscribe 拿订阅清单,然后对每个订阅 topic
// POST /events/<topic> 推送 CloudEvents。
//
// 本服务订阅 1 个 topic:
//
//	sale.paid:POS 推送的收款完成事件 ⇒ 幂等扣减库存
//	(写 stock_moves + 更新 stock_levels,见 service/subscribe.go)
//
// # ⚠️ 这两个端点必须注册在 **auth 中间件之前**
//
// 通过 `cmdbootstrap.Options.RegisterDapr` 注册 —— sidecar 调它们**不带**
// Authorization 头(它是 sidecar 自己,不是终端用户),
// 而 RequireAudience 对缺 claims 的请求一律 401。
//
// ⇒ 整条 pubsub 链路静默失效(订阅建不起来,但 /healthz 仍 200、业务看着正常)。
//
// ⚠️ 开这个窗口的代价:**任何能 TCP 连到 app 端口的人都能 POST 伪造事件**。
//
//	当前部署依赖"内网可信"这一前提(与 fresh-meat / stocktake 同款,已被接受)。
package handler

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/inventory/model"
	"github.com/YunBright/supertrade/internal/inventory/service"
	"github.com/YunBright/supertrade/pkg/eventbus"
)

// SubscribeTopics 是本服务订阅的 Dapr topic 列表。
//
// 加新 topic:
//  1. 在这里加一行
//  2. 在 dispatchSubscribe 的 switch 加一个 case
//  3. 同步 docs/EVENT-CATALOG.md
var SubscribeTopics = []string{
	"sale.paid",
}

// Subscription 是 /dapr/subscribe 返回结构。
type Subscription struct {
	PubsubName string   `json:"pubsubname"`
	Topic      string   `json:"topic"`
	Route      string   `json:"route"`
	Metadata   struct{} `json:"metadata,omitempty"`
}

// RegisterSubscribeRoutes 把 Dapr 订阅相关端点挂到 gin engine。
//
//	GET  /dapr/subscribe        → Subscribe(订阅清单)
//	POST /events/<topic>        → 事件分发
//
// ⚠️ 必须由 `cmdbootstrap.Options.RegisterDapr` 调用(而不是 Options.Register):
//
//	RegisterDapr 在 auth 中间件**之前**执行,Register 在之后。
func (h *Handler) RegisterSubscribeRoutes(r *gin.Engine, logger *slog.Logger) {
	if logger == nil {
		logger = h.logger
	}
	r.GET("/dapr/subscribe", h.subscribeList)
	for _, t := range SubscribeTopics {
		path := "/events/" + t
		r.POST(path, h.makeTopicHandler(t, logger))
	}
}

// subscribeList 返回 /dapr/subscribe 注册清单。
//
// ⚠️ pubsubname **必须**是 eventbus.Name(全平台唯一总线名):
//
//	名字与发布侧不一致 = 两条互不相通的 bus ——
//	sidecar 照常加载 component、/dapr/subscribe 照常返 200、连接照常建立,
//	但事件**永远送不到**,且全程零报错。
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
// 错误处理(与 fresh-meat 相反,这里是**该重试的就重试**):
//
//   - 载荷非法(缺 branch_id / lines 空)→ 400
//     dapr 会重试几次后进 DLQ —— 这类错误重试也不会好,但它**必须被看见**
//     (静默 200 会让"库存没扣"这件事彻底无痕)。
//   - DB 错误 → 500,让 dapr 重试(瞬时故障重试是有意义的)。
func (h *Handler) dispatchSubscribe(c *gin.Context, topic string, logger *slog.Logger) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		logger.Warn("subscribe: read body", "topic", topic, "err", err)
		c.AbortWithStatusJSON(http.StatusBadRequest, errBody{
			Code:    "bad_request",
			Message: "读取请求体失败",
		})
		return
	}
	defer c.Request.Body.Close()

	// 解 envelope(CloudEvents 风格);失败时降级从 raw body 直接拿字段。
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(body, &env)

	switch topic {
	case "sale.paid":
		h.onSalePaid(c, env.Data, body, logger)
	default:
		logger.Warn("subscribe: unknown topic", "topic", topic)
		c.Status(http.StatusOK)
	}
}

// onSalePaid 解 envelope → 调 svc.ApplySalePaid 幂等扣减库存。
func (h *Handler) onSalePaid(c *gin.Context, data json.RawMessage, rawBody []byte, logger *slog.Logger) {
	var payload service.SalePaidPayload
	if len(data) > 0 {
		if err := json.Unmarshal(data, &payload); err != nil {
			logger.Warn("subscribe: parse sale.paid envelope data", "err", err)
			c.AbortWithStatusJSON(http.StatusBadRequest, errBody{
				Code:    "bad_request",
				Message: "事件体解析失败: " + err.Error(),
			})
			return
		}
	} else {
		// 兜底:dapr 直接投递原始 JSON,无 envelope 包裹
		if err := json.Unmarshal(rawBody, &payload); err != nil {
			logger.Warn("subscribe: parse sale.paid raw body", "err", err)
			c.AbortWithStatusJSON(http.StatusBadRequest, errBody{
				Code:    "bad_request",
				Message: "事件体解析失败: " + err.Error(),
			})
			return
		}
	}

	result, err := h.stock.ApplySalePaid(c.Request.Context(), payload)
	if err != nil {
		logger.Warn("subscribe: ApplySalePaid", "op_id", payload.OpID, "err", err)
		// 入参非法 → 400(重试无用,但要留下痕迹);其它 → 500(让 dapr 重试)。
		if isInvalidInput(err) {
			c.AbortWithStatusJSON(http.StatusBadRequest, errBody{
				Code:    "bad_request",
				Message: err.Error(),
			})
			return
		}
		c.AbortWithStatusJSON(http.StatusInternalServerError, errBody{
			Code:    "internal_error",
			Message: "处理 sale.paid 失败",
		})
		return
	}
	logger.Info("subscribe: ApplySalePaid ok",
		"op_id", payload.OpID,
		"sale_id", payload.SaleID,
		"branch_id", payload.BranchID,
		"groups", result.Groups,
		"applied", result.Applied,
		"skipped", result.Skipped)
	c.Status(http.StatusOK)
}

// isInvalidInput 判断是不是"载荷非法"(这类错误重试无用)。
func isInvalidInput(err error) bool {
	return errors.Is(err, model.ErrInvalidInput)
}
