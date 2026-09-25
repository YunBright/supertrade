// Command notification-gateway 是 TradeWind 后端的实时推送网关。
//
// 职责：
//   - 提供 /ws 升级 WebSocket；客户端携 JWT 接入
//   - 订阅 Dapr pub/sub 业务事件（stocktake.* / auth.user.*）
//   - 通过 Hub / Registry / TenantRouter 把事件 fanout 到对应客户端
//
// 鉴权：JWT 由 sidecar / gateway 验签；本服务只校验 audience 含
// "notification-gateway"（通过 auth.RequireAudience 二次防御）。
// cmdbootstrap 已经把 claims.GinMiddleware() 装到路由链上,
// 所以 /ws handler 可以从 gin.Context 直接拿 *claims.Claims。
//
// 启动：`dapr run --app-id notification-gateway --app-port 8080 -- go run ./cmd/notification-gateway`
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/YunBright/authkit/claims"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/YunBright/supertrade/internal/notification-gateway/auth"
	"github.com/YunBright/supertrade/internal/notification-gateway/events"
	"github.com/YunBright/supertrade/internal/notification-gateway/gateway"
	"github.com/YunBright/supertrade/internal/notification-gateway/observability"
	"github.com/YunBright/supertrade/internal/notification-gateway/wsmsg"
	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
)

const appID = "notification-gateway"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	hub := gateway.NewHub(logger)
	router := gateway.NewTenantRouter()
	registry := gateway.NewRegistry(hub, router)
	metrics := &observability.Metrics{}

	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID:    appID,
		Port:     cmdbootstrap.AppPort(":8101"),
		Audience: auth.ExpectedAudience,
		Logger:   logger,
		OnStart: func() error {
			go hub.Run()
			logger.Info("hub started")
			return nil
		},
		OnStop: func(_ context.Context) error {
			hub.Shutdown()
			return nil
		},
		Register: func(r *gin.Engine) {
			registerRoutes(r, registry, metrics, logger)
		},
	})
}

// registerRoutes 把业务路由挂到 gin engine。
//
// 注意：cmdbootstrap 已注册 /healthz（公开），不需要再注册。
func registerRoutes(r *gin.Engine, registry *gateway.Registry, metrics *observability.Metrics, logger *slog.Logger) {
	// /dapr/subscribe + /events/<topic>
	events.NewHandler(registry, metrics, logger).RegisterRoutes(r)

	// /ws 升级端点（已隐含在 claims.GinMiddleware 之后）
	r.GET("/ws", wsUpgradeHandler(registry, metrics, logger))

	// /metrics（自实现,避免 prometheus 依赖）
	r.GET("/metrics", metricsHandler(metrics))
}

// wsUpgradeHandler 处理 /ws 升级 + 鉴权 + 注册。
func wsUpgradeHandler(registry *gateway.Registry, m *observability.Metrics, logger *slog.Logger) gin.HandlerFunc {
	upgrader := websocket.Upgrader{
		HandshakeTimeout: 10 * time.Second,
		ReadBufferSize:   4096,
		WriteBufferSize:  4096,
		CheckOrigin: func(_ *http.Request) bool {
			// 简化：移动端 / 同源 / 跨域都接受；
			// TODO: 接入 nginx gateway 配置的 origin allowlist
			return true
		},
	}
	return func(c *gin.Context) {
		cl, ok := claims.FromContext(c.Request.Context())
		if !ok || cl == nil {
			m.AuthFailures.Add(1)
			logger.Warn("ws: no claims in context (auth middleware missing?)")
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		if err := auth.RequireAudience(cl); err != nil {
			m.AuthFailures.Add(1)
			logger.Warn("ws: audience mismatch", "user", cl.Sub, "aud", cl.Aud)
			c.AbortWithStatus(http.StatusForbidden)
			return
		}

		conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			logger.Warn("ws upgrade failed", "err", err)
			return
		}
		client := gateway.NewClient(registry.Hub(), conn, cl, logger)
		registry.Hub().RegisterClient(client)
		m.ConnectionsAccepted.Add(1)

		// 发送 welcome
		welcome, _ := wsmsg.New(wsmsg.TypeWelcome, map[string]any{
			"user_id":   cl.Sub,
			"tenant_id": cl.TenantID,
			"server_ts": time.Now().Unix(),
		})
		client.Send(welcome)

		go client.WritePump()
		go func() {
			client.ReadPump()
			m.ConnectionsClosed.Add(1)
		}()
	}
}

// metricsHandler 返回 observability.Metrics 快照。
func metricsHandler(m *observability.Metrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, m.Snapshot())
	}
}
