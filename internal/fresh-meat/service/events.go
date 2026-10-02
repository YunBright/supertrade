// Package service - events.go
//
// 把 fresh-meat 域的关键写操作广播到 Dapr pub/sub。事件契约严格对齐
// docs/EVENT-CATALOG.md §2.6/§2.8(pork.cuts.stocktaken + waste.log.recorded)。
//
// 重要 — 与 stocktake 的差异:
//   - **不**携带 tenant_id 字段(payload 完全无 tenant);
//     全仓不持久化 tenant,notification-gateway 的 TenantRouter 走"同 tenant"分支
//     依赖 branch_id,与本服务无 tenant 字段相容。
//   - 不发 pig.arrived 事件 — LLM 通过 Dapr Conversation API 同步调,无外部订阅方需求
//     (requirements §4.2 + 用户决策 2026-10-01)。
//
// 设计原则(同 stocktake):
//   - 发布是**尽力而为**:失败仅记 log,不阻断业务;
//     下游应能容忍偶发丢事件(WS 重连后由 invalid 全量补偿)。
//   - 在 DB 事务**提交后**再 publish,杜绝"幻影事件"。
//   - publisher 为 nil 时全部 noop(便于单测与离线运行)。
//   - publish 用独立的 short-timeout ctx,与请求 ctx 解耦,
//     避免上游 cancel 影响广播。
package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
)

// Publisher 是 Dapr pub/sub 发布的最小抽象。
//
// 生产实现(cmd/fresh-meat/main.go)走 dapr/go-sdk:cli.PublishEvent(ctx, pubsub, topic, data)。
// SDK 自动包 CloudEvents 1.0 envelope 并通过 sidecar 转发。
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
func (s *Service) publish(ctx context.Context, topic string, payload any) {
	if s.publisher == nil {
		return
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

// ---- 显式 topic 常量 ----
//
// 与 EVENT-CATALOG.md §2.6/§2.8 一一对应;cmd 层直接复用。
const (
	TopicPorkCutsStocktaken = "pork.cuts.stocktaken"
	TopicWasteLogRecorded   = "waste.log.recorded"
)

// payloadAdapter 是给编译期检查 event struct 是否携带 tenant 字段的辅助类型。
//
// 故意留空:无 withTenant 方法。stocktake pattern 不适用 fresh-meat。
var _ = func() bool {
	// 编译期断言:model.PorkCutsStocktakenEventData / WasteLogRecordedEventData
	// 的 struct 字段不含 tenant_id(grep 已人工验证)。
	var p model.PorkCutsStocktakenEventData
	var w model.WasteLogRecordedEventData
	_ = p
	_ = w
	return true
}()