// Package service - dapr_publisher.go
//
// DaprPublisher 实现 service.Publisher,经 dapr/go-sdk 的 client.PublishEvent
// 把事件发给 pubsub component(默认 Redis Streams;notification-gateway 通过
// /dapr/subscribe 订阅)。
//
// 失败处理:
//   - dapr.NewClient() 失败 → 启动期 fail-fast:fresh-meat 业务不依赖发事件,
//     失败仅 log,不影响早盘录入。
//   - Publish 失败 → 返 error,service.publish 内部 log;dapr sidecar 内部重试机制独立。
package service

import (
	"context"
	"os"

	dapr "github.com/dapr/go-sdk/client"

	"github.com/YunBright/supertrade/pkg/eventbus"
)

// DaprPublisher service.Publisher 的 dapr-sdk 实现。
//
// 持有 dapr.Client (gRPC SDK 接口) + pubsub component name。
type DaprPublisher struct {
	client dapr.Client
	pubsub string // pubsub component name,默认 eventbus.Name
}

// NewDaprPublisherFromEnv 构造 publisher(失败则返 error,caller 应启动失败)。
//
//	DAPR_PUBSUB = "tradewind-pubsub"   // 默认
//
// ⚠️ 必须与订阅侧 component metadata.name 逐字相同,否则事件没有订阅者且
// **完全不报错**。见 pkg/eventbus 的说明。
func NewDaprPublisherFromEnv() (*DaprPublisher, error) {
	cli, err := dapr.NewClient()
	if err != nil {
		return nil, err
	}
	ps := os.Getenv("DAPR_PUBSUB")
	if ps == "" {
		ps = eventbus.Name
	}
	return &DaprPublisher{client: cli, pubsub: ps}, nil
}

// Publish 把 data 发到 pubsub/topic。SDK 自动包 CloudEvents 1.0 envelope。
func (p *DaprPublisher) Publish(ctx context.Context, topic string, data any) error {
	return p.client.PublishEvent(ctx, p.pubsub, topic, data)
}