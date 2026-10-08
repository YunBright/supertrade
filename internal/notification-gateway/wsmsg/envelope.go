// Package wsmsg 定义 WS 客户端 / 服务端交互的消息 envelope。
//
// 与 CloudEvents 1.0 对齐；Flutter 端的 lib/infra/ws/ws_message.dart 一一对应。
// 所有消息（包括 hello / welcome / ping / pong / 业务事件）都遵循同一 envelope，
// 便于多端 SDK 复用同一解析器。
package wsmsg

import (
	"encoding/json"
	"time"
)

// Envelope 是 WS 消息的统一 envelope（CloudEvents 1.0 风格）。
//
// JSON 字段:
//   - type:        CloudEvents `type`（必填）；控制帧用 hello/welcome/ping/pong/...，
//     业务帧用 stocktake.line.added 之类
//   - data:        业务 payload（任意 JSON 兼容对象）
//   - id:          CloudEvents `id`（幂等去重）
//   - time:        CloudEvents `time`（RFC3339 UTC）
//   - source:      CloudEvents `source`（如 "stocktake/ST20260917001"）
//   - subject:     CloudEvents `subject`（如 "stocktake_line/12"）
//   - specversion: 固定 "1.0"
type Envelope struct {
	Type            string          `json:"type"`
	Data            json.RawMessage `json:"data,omitempty"`
	ID              string          `json:"id,omitempty"`
	Time            string          `json:"time,omitempty"`
	Source          string          `json:"source,omitempty"`
	Subject         string          `json:"subject,omitempty"`
	SpecVersion     string          `json:"specversion,omitempty"`
	DataContentType string          `json:"datacontenttype,omitempty"`

	// Topic 是 Dapr pub/sub 在 CloudEvents **之外**附加的业务 topic 名
	// （`{"topic":"auth.user.access_changed", ...}`）。
	//
	// 它是 Dapr 投递形态下唯一可靠的"这条事件是什么"——因为 Dapr 把
	// CloudEvents 的 type 固定写成 [TypeComDaprEventSent]。
	// 见 [NormalizeCloudEventType]。
	Topic string `json:"topic,omitempty"`
}

// TypeComDaprEventSent 是 Dapr pub/sub 投递时写死在 CloudEvents `type` 字段里的值。
//
// ⚠️ **不要**用 `type` 判断业务事件。Dapr 的 pub/sub HTTP 投递形态实测为：
//
//	{
//	  "data":         {"user_id":"..."},
//	  "topic":        "auth.user.access_changed",   ← 真正的 topic
//	  "type":         "com.dapr.event.sent",         ← 固定值，无业务含义
//	  "pubsubname":   "tradewind-pubsub",
//	  "datacontenttype": "application/json",
//	  "specversion":  "1.0"
//	}
//
// 2026-10-08 实测事故：曾按 `type` 做 fanout 分类，于是**所有**事件都落进
// default 分支（按 tenant/branch 过滤），而 pub/sub 事件的 payload 里根本没有
// branch_id/tenant_id —— 结果一条也投不出去。全程零报错：
// sidecar 订阅成功、/dapr/subscribe 200、WS 握手 101、消费组 lag=0，
// 客户端就是收不到。
//
// 这也是为什么 auth 自己的 SSE 一直没暴露这个问题：它按**订阅路由 URL**
// (`/dapr/events/<topic>`) 分发，不看 envelope 的 type。
const TypeComDaprEventSent = "com.dapr.event.sent"

// NormalizeCloudEventType 把 Dapr pub/sub 的投递形态归一化成"type == 业务 topic"。
//
// fallbackTopic 是订阅路由里的 topic（`/events/<topic>` 的最后一段），在 envelope
// 自带 topic 字段缺失时兜底 —— 它由 sidecar 的订阅路由保证与实际 topic 一致。
//
// 优先级：
//  1. type 为空            → 用 envelope.Topic
//  2. type 是 Dapr 固定值   → 用 envelope.Topic
//  3. envelope.Topic 为空   → 用 fallbackTopic
//
// 已经带业务 type 的 envelope（我们自己在 WS 上发的 welcome/ping，以及某些
// 直连投递）原样返回，不做任何改写。
func NormalizeCloudEventType(env Envelope, fallbackTopic string) Envelope {
	if env.Type == "" || env.Type == TypeComDaprEventSent {
		if env.Topic != "" {
			env.Type = env.Topic
		} else if fallbackTopic != "" {
			env.Type = fallbackTopic
		}
	}
	return env
}

// New 构造一个 envelope（自动填 time / specversion / datacontenttype）。
func New(typ string, data any) (Envelope, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		Type:            typ,
		Data:            raw,
		Time:            time.Now().UTC().Format(time.RFC3339),
		SpecVersion:     "1.0",
		DataContentType: "application/json",
	}, nil
}

// MustNew 同 New；出错时 panic（仅用于静态数据）。
func MustNew(typ string, data any) Envelope {
	e, err := New(typ, data)
	if err != nil {
		panic(err)
	}
	return e
}

// 控制帧 type 常量（与 Flutter WsControlTypes 对齐）。
const (
	TypeHello   = "hello"
	TypeWelcome = "welcome"
	TypePing    = "ping"
	TypePong    = "pong"
	TypeAuth    = "auth"
	TypeAuthOK  = "auth_ok"
	TypeError   = "error"
)

// 业务事件 type 常量（与 EVENT-CATALOG.md §2.12~2.14 对齐；与 Flutter WsEventTypes 对齐）。
const (
	// §2.12 stocktake 行级事件
	TypeStocktakeLineAdded   = "stocktake.line.added"
	TypeStocktakeLineUpdated = "stocktake.line.updated"
	TypeStocktakeLineDeleted = "stocktake.line.deleted"

	// §2.13 stocktake 头级事件
	TypeStocktakeHeaderSubmitted = "stocktake.header.submitted"
	TypeStocktakeHeaderApproved  = "stocktake.header.approved"

	// §2.13 stocktake 计划项事件
	TypeStocktakePlanItemAdded = "stocktake.plan_item.added"

	// §2.14 访问维度变更事件(scopes / roles / branches / default_branch 合并)
	TypeUserAccessChanged = "auth.user.access_changed"
)

// 错误码（与 Flutter WsErrorCodes 对齐）。
const (
	ErrAuthExpired      = "auth_expired"
	ErrAudienceMismatch = "audience_mismatch"
	ErrProtocolError    = "protocol_error"
	ErrForbidden        = "forbidden"
)
