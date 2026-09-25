// Package service - events.go
//
// 把 stocktake 域的关键写操作广播到 Dapr pub/sub,供 notification-gateway
// 实时推送给在线客户端。事件契约严格对齐 docs/EVENT-CATALOG.md §2.12~2.13。
//
// 设计原则:
//   - 发布是**尽力而为**:失败仅记 log,不阻断业务;
//     下游应能容忍偶发丢事件(WS 重连后由 invalid 全量补偿)。
//   - 在 DB 事务**提交后**再 publish,杜绝"幻影事件"。
//   - publisher 为 nil 时全部 noop(便于单测与离线运行)。
//   - publish 用独立的 short-timeout ctx,与请求 ctx 解耦,
//     避免上游 cancel 影响广播。
package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/YunBright/authkit/claims"
)

// Publisher 是 Dapr pub/sub 发布的最小抽象。
//
// 生产实现(cmd/stocktake/main.go)走 dapr/go-sdk:cli.PublishEvent(ctx, pubsub, topic, data)。
// SDK 自动包 CloudEvents 1.0 envelope 并通过 sidecar 转发(2026-09 切到 SDK,不走 HTTP POST)。
// 测试可注入 in-memory recorder 验证 publish 调用次数。
type Publisher interface {
	Publish(ctx context.Context, topic string, data any) error
}

// SetPublisher 注入 publisher(nil = 禁用广播,等价于 noop)。
func (s *Service) SetPublisher(p Publisher) {
	s.publisher = p
	if s.pubLogger == nil {
		s.pubLogger = slog.Default()
	}
}

// SetPublisherLogger 注入发布日志(默认 slog.Default())。
func (s *Service) SetPublisherLogger(l *slog.Logger) {
	if l != nil {
		s.pubLogger = l
	}
}

// publish 是内部便捷封装:超时 3s + 错误日志,业务层无须判 err。
//
// 用独立的 background ctx(带 3s 超时)而非调用方 ctx:
//   - 上游 cancel 不应阻断事件广播
//   - 但要避免 publisher hang 死 goroutine
//
// tenant_id 从 ctx 的 claims 里抽(handler 在调 service 前已通过 GinMiddleware 注入);
// 缺 claims 时退化为空字符串,由 notification-gateway 的 TenantRouter 走"同 tenant"分支
// (依赖 branch_id 校验,仍能正确路由)。
func (s *Service) publish(ctx context.Context, topic string, payload any) {
	if s.publisher == nil {
		return
	}
	// 把 tenant_id 注入到 payload(若 payload 是 struct 且含 TenantID 字段为 "" 时)
	if p, ok := payload.(interface{ withTenant(string) }); ok {
		if cl, ok := claims.FromContext(ctx); ok && cl != nil {
			p.withTenant(cl.TenantID)
		}
	}
	pubCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.publisher.Publish(pubCtx, topic, payload); err != nil {
		s.pubLogger.Warn("publish failed",
			"topic", topic,
			"err", err,
		)
	}
}

// marshalOrLog 仅用于构造 payload 失败时记录(理论上 struct → json 不会失败,
// 但保持一致风格)。
func marshalOrLog(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		// 兜底:返回 {} 让下游不 panic
		return json.RawMessage(`{}`)
	}
	return b
}

// ---- §2.12 stocktake.line.* payload ----

// LineEventData §2.12 行级事件 data。
//
// 字段命名严格对齐 EVENT-CATALOG.md;operator_name 用于 Flutter
// 端展示 "张三 改了 五花肉" 这种自然语言提示。
type LineEventData struct {
	HeaderID    string  `json:"header_id"`
	LineID      string  `json:"line_id"`
	BranchID    string  `json:"branch_id"`
	TenantID    string  `json:"tenant_id"`
	ProductID   string  `json:"sku_id"`
	ProductName string  `json:"product_name"`
	Barcode     string  `json:"barcode,omitempty"`
	SystemQty   float64 `json:"system_qty"` // book_qty(JSON 不能直接解 decimal.Decimal)
	ActualQty   float64 `json:"actual_qty"`
	DiffQty     float64 `json:"diff_qty"`
	Unit        string  `json:"unit,omitempty"`
	OperatorID  string  `json:"operator_id"`
	OperatorName string `json:"operator_name,omitempty"`
	OccurredAt  string  `json:"occurred_at"` // RFC3339 UTC
}

// withTenant 实现 payload 的 tenant 注入接口。
func (d *LineEventData) withTenant(t string) { d.TenantID = t }

// LineDeletedEventData §2.12 deleted 事件 data(字段更少)。
type LineDeletedEventData struct {
	HeaderID    string `json:"header_id"`
	LineID      string `json:"line_id"`
	BranchID    string `json:"branch_id"`
	TenantID    string `json:"tenant_id"`
	ProductID   string `json:"sku_id"`
	OperatorID  string `json:"operator_id"`
	OperatorName string `json:"operator_name,omitempty"`
	OccurredAt  string `json:"occurred_at"`
}

// withTenant 实现 payload 的 tenant 注入接口。
func (d *LineDeletedEventData) withTenant(t string) { d.TenantID = t }

// ---- §2.13 stocktake.header.* payload ----

// HeaderEventData §2.13 头级事件 data。
type HeaderEventData struct {
	HeaderID    string `json:"header_id"`
	BranchID    string `json:"branch_id"`
	TenantID    string `json:"tenant_id"`
	Type        string `json:"type"`        // general / produce / plan / recheck
	Status      string `json:"status"`      // counting / adjusted / approved / cancelled
	ParentID    string `json:"parent_id,omitempty"`
	OperatorID  string `json:"operator_id"`
	OperatorName string `json:"operator_name,omitempty"`
	OccurredAt  string `json:"occurred_at"`
}

// withTenant 实现 payload 的 tenant 注入接口。
func (d *HeaderEventData) withTenant(t string) { d.TenantID = t }

// PlanItemEventData §2.13 plan_item.added 事件 data。
type PlanItemEventData struct {
	HeaderID    string `json:"header_id"`
	BranchID    string `json:"branch_id"`
	TenantID    string `json:"tenant_id"`
	ItemsCount  int    `json:"items_count"`
	BatchIndex  int    `json:"batch_index,omitempty"`
	BatchTotal  int    `json:"batch_total,omitempty"`
	OperatorID  string `json:"operator_id"`
	OperatorName string `json:"operator_name,omitempty"`
	OccurredAt  string `json:"occurred_at"`
}

// withTenant 实现 payload 的 tenant 注入接口。
func (d *PlanItemEventData) withTenant(t string) { d.TenantID = t }

// ---- 显式 topic 常量 ----
//
// 与 EVENT-CATALOG.md §2.12~2.13 一一对应;cmd 层直接复用。
const (
	TopicStocktakeLineAdded        = "stocktake.line.added"
	TopicStocktakeLineUpdated      = "stocktake.line.updated"
	TopicStocktakeLineDeleted      = "stocktake.line.deleted"
	TopicStocktakeHeaderSubmitted  = "stocktake.header.submitted"
	TopicStocktakeHeaderApproved   = "stocktake.header.approved"
	TopicStocktakePlanItemAdded    = "stocktake.plan_item.added"
)

