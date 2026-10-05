// Package service - dapr_publisher.go
//
// DaprPublisher 实现 service.Publisher,经 dapr/go-sdk 的 client.PublishEvent
// 把事件发给 pubsub component(默认 Redis Streams;notification-gateway 通过
// /dapr/subscribe 订阅)。
//
// 失败处理:
//   - dapr.NewClient() 失败 → 启动期 fail-fast:stocktake 必须有 publisher
//     才能消费 auth.user.access_changed,静默禁用会导致 scope 缓存不一致。
//   - Publish 失败 → 返 error,service.publish 内部 log;dapr sidecar 内部
//     重试机制独立。
package service

import (
	"context"
	"os"

	dapr "github.com/dapr/go-sdk/client"
)

// DaprPublisher service.Publisher 的 dapr-sdk 实现。
//
// 持有 dapr.Client (gRPC SDK 接口) + pubsub component name。
type DaprPublisher struct {
	client dapr.Client
	pubsub string // pubsub component name,默认 "pubsub"
}

// NewDaprPublisherFromEnv 构造 publisher(失败则返 error,caller 应启动失败)。
//
//	DAPR_PUBSUB = "pubsub"   // 默认
func NewDaprPublisherFromEnv() (*DaprPublisher, error) {
	cli, err := dapr.NewClient()
	if err != nil {
		return nil, err
	}
	ps := os.Getenv("DAPR_PUBSUB")
	if ps == "" {
		ps = "pubsub"
	}
	return &DaprPublisher{client: cli, pubsub: ps}, nil
}

// Publish 把 data 发到 pubsub/topic。SDK 自动包 CloudEvents 1.0 envelope。
func (p *DaprPublisher) Publish(ctx context.Context, topic string, data any) error {
	return p.client.PublishEvent(ctx, p.pubsub, topic, data)
}