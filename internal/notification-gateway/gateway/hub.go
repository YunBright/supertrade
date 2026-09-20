// Package gateway 实现 WS Hub / Client / Registry / 路由分发。
//
// 设计要点:
//   - 单条 WS 连接 = 一个 Client；一个用户可在多端登录 → 多 Client
//   - Hub 是所有 Client 的中枢；广播 / 单播都经 Hub
//   - Registry 维护 user_id → []*Client 的索引 + tenant / branch 维度索引
//   - 事件路由：TenantRouter.shouldDeliver 校验后 fanout 到目标 Client
package gateway

import (
	"log/slog"
	"sync"

	"github.com/YunBright/supertrade/internal/notification-gateway/wsmsg"
)

// Hub 是所有 Client 的中枢。
//
// 职责：
//   - 维护 Client 池（register / unregister）
//   - 提供 Fanout 方法（向一组 Client 投递 envelope）
//   - 关闭时通知所有 Client
type Hub struct {
	clients    map[*Client]struct{}
	register   chan *Client
	unregister chan *Client
	broadcast  chan broadcastJob
	done       chan struct{}

	logger *slog.Logger

	mu       sync.RWMutex
	byUser   map[string]map[*Client]struct{}
	byTenant map[string]map[*Client]struct{}
}

// broadcastJob 一次 fanout 任务。
type broadcastJob struct {
	envelope wsmsg.Envelope
	target   func(*Client) bool
}

// NewHub 构造一个 Hub；调用 Run 启动 goroutine。
func NewHub(logger *slog.Logger) *Hub {
	return &Hub{
		clients:    make(map[*Client]struct{}),
		register:   make(chan *Client, 64),
		unregister: make(chan *Client, 64),
		broadcast:  make(chan broadcastJob, 1024),
		done:       make(chan struct{}),
		logger:     logger,
		byUser:     make(map[string]map[*Client]struct{}),
		byTenant:   make(map[string]map[*Client]struct{}),
	}
}

// Run 阻塞直到 done 被 close；驱动 register / unregister / broadcast。
func (h *Hub) Run() {
	for {
		select {
		case c := <-h.register:
			h.mu.Lock()
			h.clients[c] = struct{}{}
			if c.Claims() != nil {
				if h.byUser[c.UserID()] == nil {
					h.byUser[c.UserID()] = map[*Client]struct{}{}
				}
				h.byUser[c.UserID()][c] = struct{}{}
				if h.byTenant[c.TenantID()] == nil {
					h.byTenant[c.TenantID()] = map[*Client]struct{}{}
				}
				h.byTenant[c.TenantID()][c] = struct{}{}
			}
			h.mu.Unlock()
			h.logger.Info("ws client registered", "user", c.UserID(), "tenant", c.TenantID())

		case c := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[c]; ok {
				delete(h.clients, c)
				if c.Claims() != nil {
					if set, ok := h.byUser[c.UserID()]; ok {
						delete(set, c)
						if len(set) == 0 {
							delete(h.byUser, c.UserID())
						}
					}
					if set, ok := h.byTenant[c.TenantID()]; ok {
						delete(set, c)
						if len(set) == 0 {
							delete(h.byTenant, c.TenantID())
						}
					}
				}
				c.Close()
			}
			h.mu.Unlock()
			h.logger.Info("ws client unregistered", "user", c.UserID())

		case job := <-h.broadcast:
			h.mu.RLock()
			targets := make([]*Client, 0, len(h.clients))
			for c := range h.clients {
				if job.target(c) {
					targets = append(targets, c)
				}
			}
			h.mu.RUnlock()
			for _, c := range targets {
				c.Send(job.envelope)
			}

		case <-h.done:
			h.mu.Lock()
			for c := range h.clients {
				c.Close()
			}
			h.mu.Unlock()
			return
		}
	}
}

// Shutdown 停止 Hub。
func (h *Hub) Shutdown() {
	close(h.done)
}

// RegisterClient 注册一个 Client（非阻塞，channel 满则记录 warning）。
func (h *Hub) RegisterClient(c *Client) {
	select {
	case h.register <- c:
	default:
		h.logger.Warn("hub register channel full; dropping client", "user", c.UserID())
	}
}

// unregisterClient 内部使用：Client.ReadPump / Send 失败时调用。
func (h *Hub) unregisterClient(c *Client) {
	select {
	case h.unregister <- c:
	default:
		c.Close()
	}
}

// Fanout 把 envelope 投递到所有满足 target 的客户端。
//
// target 是路由过滤器；返回 true 表示该 client 应收到此消息。
// 通常由 TenantRouter.shouldDeliver 提供。
func (h *Hub) Fanout(env wsmsg.Envelope, target func(*Client) bool) {
	select {
	case h.broadcast <- broadcastJob{envelope: env, target: target}:
	default:
		h.logger.Warn("hub broadcast channel full; dropping event", "type", env.Type)
	}
}

// ByUser 取该用户的所有连接。
func (h *Hub) ByUser(userID string) []*Client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	set := h.byUser[userID]
	out := make([]*Client, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	return out
}

// Stats 返回当前 client 数（用于 /healthz / metrics）。
func (h *Hub) Stats() (clients, users, tenants int) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients), len(h.byUser), len(h.byTenant)
}
