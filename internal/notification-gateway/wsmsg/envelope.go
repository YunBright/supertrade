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
//                  业务帧用 stocktake.line.added 之类
//   - data:        业务 payload（任意 JSON 兼容对象）
//   - id:          CloudEvents `id`（幂等去重）
//   - time:        CloudEvents `time`（RFC3339 UTC）
//   - source:      CloudEvents `source`（如 "stocktake/ST20260917001"）
//   - subject:     CloudEvents `subject`（如 "stocktake_line/12"）
//   - specversion: 固定 "1.0"
type Envelope struct {
	Type           string          `json:"type"`
	Data           json.RawMessage `json:"data,omitempty"`
	ID             string          `json:"id,omitempty"`
	Time           string          `json:"time,omitempty"`
	Source         string          `json:"source,omitempty"`
	Subject        string          `json:"subject,omitempty"`
	SpecVersion    string          `json:"specversion,omitempty"`
	DataContentType string         `json:"datacontenttype,omitempty"`
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

	// §2.14 权限变更事件
	TypeUserPermissionsChanged = "auth.user.permissions_changed"
)

// 错误码（与 Flutter WsErrorCodes 对齐）。
const (
	ErrAuthExpired     = "auth_expired"
	ErrAudienceMismatch = "audience_mismatch"
	ErrProtocolError   = "protocol_error"
	ErrForbidden       = "forbidden"
)