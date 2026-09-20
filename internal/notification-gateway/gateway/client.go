// Package gateway 实现 WS Hub / Client / Registry / 路由分发。
//
// 设计要点:
//   - 单条 WS 连接 = 一个 Client；一个用户可在多端登录 → 多 Client
//   - Hub 是所有 Client 的中枢；广播 / 单播都经 Hub
//   - Registry 维护 user_id → []*Client 的索引 + tenant / branch 维度索引
//   - 事件路由：TenantRouter.shouldDeliver 校验后 fanout 到目标 Client
//
// Claims 直接复用 authkit/claims.Claims（cmdbootstrap 已经把 claims.GinMiddleware()
// 装到路由链上,WS handler 从 gin.Context 拿到 claims 即可）。
package gateway

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/YunBright/authkit/claims"
	"github.com/YunBright/supertrade/internal/notification-gateway/wsmsg"
)

// ClaimsAlias 是 authkit.Claims 的本地别名,便于代码可读与后续扩展。
//
// 所有路由判定 / fanout 都用 *authkit.claims.Claims 的方法(HasScope /
// GetEffectiveBranches 等),本类型不增加字段,仅提供类型语义。
type ClaimsAlias = claims.Claims

// Client 单条 WS 连接。
type Client struct {
	hub    *Hub
	conn   *websocket.Conn
	send   chan wsmsg.Envelope
	claims *claims.Claims
	logger *slog.Logger

	mu     sync.Mutex
	closed bool
}

// NewClient 包装一条已升级的 WS 连接。
func NewClient(hub *Hub, conn *websocket.Conn, cl *claims.Claims, logger *slog.Logger) *Client {
	return &Client{
		hub:    hub,
		conn:   conn,
		send:   make(chan wsmsg.Envelope, 64),
		claims: cl,
		logger: logger,
	}
}

// UserID 当前连接的用户 ID(对应 authkit.Claims.Sub)。
func (c *Client) UserID() string {
	if c.claims == nil {
		return ""
	}
	return c.claims.Sub
}

// TenantID 当前租户。
func (c *Client) TenantID() string {
	if c.claims == nil {
		return ""
	}
	return c.claims.TenantID
}

// BranchID 当前 home 门店。
func (c *Client) BranchID() string {
	if c.claims == nil {
		return ""
	}
	return c.claims.BranchID
}

// Claims 浅拷贝（仅用于路由判定；不要写）。
func (c *Client) Claims() *claims.Claims {
	return c.claims
}

// EffectiveBranches home + additional,去重。
func (c *Client) EffectiveBranches() []string {
	if c.claims == nil {
		return nil
	}
	return c.claims.GetEffectiveBranches()
}

// HasScope 判定 scope。
func (c *Client) HasScope(s string) bool {
	if c.claims == nil {
		return false
	}
	return c.claims.HasScope(s)
}

// ReadPump 从 WS 读取客户端帧；仅处理 ping/pong（业务帧暂不接收）。
func (c *Client) ReadPump() {
	defer func() {
		c.hub.unregisterClient(c)
		_ = c.conn.Close()
	}()
	c.conn.SetReadLimit(64 * 1024)
	_ = c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		_ = c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})
	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				c.logger.Warn("ws read error", "user", c.UserID(), "err", err)
			}
			return
		}
		// 忽略客户端业务帧（mobile 端目前只读服务端推送）
	}
}

// WritePump 把 send channel 里的 envelope 推到 WS；30s 心跳 ping。
func (c *Client) WritePump() {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
	}()
	for {
		select {
		case env, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			payload, err := json.Marshal(env)
			if err != nil {
				c.logger.Warn("ws marshal error", "err", err)
				continue
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				c.logger.Warn("ws write error", "user", c.UserID(), "err", err)
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// Send 异步发送；channel 满则丢连接（背压策略）。
func (c *Client) Send(env wsmsg.Envelope) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	select {
	case c.send <- env:
	default:
		c.logger.Warn("ws send buffer full; dropping client", "user", c.UserID())
		c.hub.unregisterClient(c)
	}
}

// Close 主动关闭连接。
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	close(c.send)
}
