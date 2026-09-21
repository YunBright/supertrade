// Package service - dapr_publisher.go
//
// DaprPublisher 实现 service.Publisher,通过 HTTP POST 把事件发给
// dapr sidecar 的 publish API:
//
//	POST {DAPR_ENDPOINT}/v1.0/publish/pubsub/<topic>
//	Body: { "data": <payload>, "datacontenttype": "application/json",
//	        "type": "<topic>", "id": "<uuid>" }
//
// Dapr sidecar 会把消息封装成 CloudEvents 1.0 envelope 并发布到 pubsub
// 组件(默认 Redis Streams);notification-gateway 通过 /dapr/subscribe
// 自动订阅这些 topic。
//
// 失败处理:任何非 2xx 响应都返回 error;service.publish() 会 log 但不
// 阻塞业务;dapr sidecar 内部也会做重试(默认指数退避 3 次)。
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
)

// DaprPublisher service.Publisher 的 dapr-sidecar 实现。
//
// 简单、单实例、不带熔断;stocktake 是低 QPS 服务,直接 HTTP 调
// localhost:3500 即可。
type DaprPublisher struct {
	endpoint   string       // dapr sidecar base URL,如 http://localhost:3500
	pubsub     string       // pubsub component name,默认 "pubsub"
	httpClient *http.Client // 短超时
}

// NewDaprPublisherFromEnv 从环境变量构造 publisher。
//
//	DAPR_ENDPOINT = "http://localhost:3500"
//	DAPR_PUBSUB   = "pubsub"
//
// 缺省回退到 sidecar 默认值。
func NewDaprPublisherFromEnv() *DaprPublisher {
	ep := os.Getenv("DAPR_ENDPOINT")
	if ep == "" {
		ep = "http://localhost:3001"
	}
	ps := os.Getenv("DAPR_PUBSUB")
	if ps == "" {
		ps = "pubsub"
	}
	return &DaprPublisher{
		endpoint:   ep,
		pubsub:     ps,
		httpClient: &http.Client{Timeout: 3 * time.Second},
	}
}

// Publish 把 data 序列化为 JSON,POST 到 dapr sidecar。
func (p *DaprPublisher) Publish(ctx context.Context, topic string, data any) error {
	body := map[string]any{
		"data":            data,
		"datacontenttype": "application/json",
		"type":            topic,
		"id":              uuid.NewString(),
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal publish body: %w", err)
	}
	url := fmt.Sprintf("%s/v1.0/publish/%s/%s", p.endpoint, p.pubsub, topic)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("publish %s: %w", topic, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("publish %s: status=%d", topic, resp.StatusCode)
	}
	return nil
}
