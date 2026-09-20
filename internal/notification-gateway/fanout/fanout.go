// Package fanout 提供事件分类 + 路由策略选择。
//
// events router 在收到 Dapr pub/sub 消息后，先用 Classify 决定走哪种 fanout
// 策略（按用户 / 按租户+门店 / 全局），再调用 Registry 对应方法。
package fanout

import "github.com/YunBright/supertrade/internal/notification-gateway/wsmsg"

// Strategy 表示 fanout 路由策略。
type Strategy int

const (
	// StrategyTenantByBranch 业务事件（stocktake.*）→ tenant + branch 过滤
	StrategyTenantByBranch Strategy = iota
	// StrategyUser 用户级事件（auth.user.*）→ 单 user 推送
	StrategyUser
	// StrategyGlobal 系统级 / 公告
	StrategyGlobal
)

// Classify 根据 envelope.Type 决定策略。
func Classify(env wsmsg.Envelope) Strategy {
	switch {
	case hasPrefix(env.Type, "auth.user."):
		return StrategyUser
	case hasPrefix(env.Type, "system."), env.Type == "broadcast":
		return StrategyGlobal
	default:
		return StrategyTenantByBranch
	}
}

func hasPrefix(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	return s[:len(prefix)] == prefix
}
