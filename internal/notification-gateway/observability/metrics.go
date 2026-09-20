// Package observability 提供轻量 metrics（避免引入 prometheus 客户端依赖）。
//
// 当前只暴露原子计数器；后续可换成 promhttp.Handler。
package observability

import "sync/atomic"

// Metrics 内存计数器；可扩展为 Prom Counter。
type Metrics struct {
	ConnectionsAccepted atomic.Int64
	ConnectionsClosed   atomic.Int64
	MessagesSent        atomic.Int64
	MessagesDropped     atomic.Int64
	AuthFailures        atomic.Int64
	EventsReceived      atomic.Int64
	EventsFannedOut     atomic.Int64
	EventsSkipped       atomic.Int64
}

// Snapshot 返回当前快照。
type Snapshot struct {
	ConnectionsAccepted int64 `json:"connections_accepted"`
	ConnectionsClosed   int64 `json:"connections_closed"`
	MessagesSent        int64 `json:"messages_sent"`
	MessagesDropped     int64 `json:"messages_dropped"`
	AuthFailures        int64 `json:"auth_failures"`
	EventsReceived      int64 `json:"events_received"`
	EventsFannedOut     int64 `json:"events_fanned_out"`
	EventsSkipped       int64 `json:"events_skipped"`
}

// Snapshot 拷贝当前值。
func (m *Metrics) Snapshot() Snapshot {
	return Snapshot{
		ConnectionsAccepted: m.ConnectionsAccepted.Load(),
		ConnectionsClosed:   m.ConnectionsClosed.Load(),
		MessagesSent:        m.MessagesSent.Load(),
		MessagesDropped:     m.MessagesDropped.Load(),
		AuthFailures:        m.AuthFailures.Load(),
		EventsReceived:      m.EventsReceived.Load(),
		EventsFannedOut:     m.EventsFannedOut.Load(),
		EventsSkipped:       m.EventsSkipped.Load(),
	}
}
