// Package service - dapr_publisher.go
//
// DaprPublisher 实现 service.Publisher,经 dapr/go-sdk 的 client.PublishEvent
// 把事件发给 pubsub component(默认 Redis Streams;notification-gateway 通过
// /dapr/subscribe 订阅)。
//
// 2026-09 切到 dapr/go-sdk:不再读 DAPR_ENDPOINT,SDK 自动从 DAPR_GRPC_PORT
// (默认 :50001) 拿 sidecar 地址;sidecar 自动跨主机寻址 + 跨 app-id 转发。
//
// 失败处理:
//   - dapr.NewClient() 失败 → 启动期 fail-fast(用户决策,2026-09-24):
//     stocktake 必须有 publisher 才能消费 auth.user.access_changed,静默禁用
//     会导致 scope 缓存不一致,应早暴露。
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
//
// dapr.NewClient() 自动从 DAPR_GRPC_PORT 拿 sidecar 地址(默认 :50001),
// 无需 DAPR_ENDPOINT 环境变量。
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