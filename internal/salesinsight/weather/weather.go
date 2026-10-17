// Package weather 提供天气上下文，供日报解释客流波动。
//
// # 设计原则：**天气是增强项，永远不是必需项**
//
// 天气影响零售是真的（下雨客流降、生鲜受损；高温利好冷饮），
// 但它绝不能成为日报能否生成的前置条件。所以本包的契约是：
//
//	拿不到天气 ⇒ 报告照出，只是少一段解释。
//
// # 为什么用 dapr binding 而不是直接 HTTP
//
// dapr 提供 bindings 抽象（`weather` 绑定），可以在不改代码的前提下
// 切换供应商、接自建代理、甚至换成本地气象站数据。
// 但本环境**尚未配置**该绑定，因此默认实现是"不可用"——
// 见 unavailableProvider 的注释：它返回一个明确的降级原因，
// 而不是假装天气是"晴"。
//
// ⚠️ 那个区别很重要：如果降级时返回"晴朗"，LLM 会拿它当真实事实去
//
//	解释"为什么生鲜涨了"，产出看似合理的假因果。
package weather

import (
	"context"
	"strings"
)

// Info 是某一天的天气上下文。
type Info struct {
	// Available 表示这份天气是否可信。
	//
	// ⚠️ 它必须与 Summary 分开：Available=false 时 Summary **必须为空串**。
	//   两者都填、或降级时填个"晴"，下游（尤其是 LLM）会把它当事实用。
	Available bool `json:"available"`
	// Summary 是给店长/LLM 看的一句话（如 "中雨，全天 6 小时降水"）。
	Summary string `json:"summary,omitempty"`
	// DegradedReason 说明为什么拿不到，是**给运维看的**，
	// 不直接展示给店长。
	DegradedReason string `json:"degraded_reason,omitempty"`
}

// unavailable 是"拿不到天气"时的 Info。
func unavailable(reason string) Info {
	return Info{Available: false, DegradedReason: reason}
}

// Provider 是天气数据源。
type Provider interface {
	// Fetch 取某天的天气。date 为 YYYY-MM-DD。
	//
	// ⚠️ 返回的 Info.Available=false 时，Summary 必须是空串 ——
	//   这是本接口最重要的不变量，见 package 注释。
	Fetch(ctx context.Context, date string, location Location) (Info, error)
}

// Location 是门店位置。
//
// # 为什么坐标是配置而不是硬编码
//
// 硬编码坐标的代价是：换一个城市部署时，天气会一直取错地点的值，
// 而且**完全静默** —— 报告显示"晴"，实际那边在下雨。
// 所以坐标必须显式配置，且必须有城市名作为可读校验。
type Location struct {
	// Name 是门店所在城市名，用于展示与人工核对。
	Name string
	Lat  float64
	Lon  float64
}

// Valid 报告位置是否可用于查询。
//
// ⚠️ 中国境内经度约 73~135、纬度约 3~54。
//
//	超出这个范围基本可以断定是配置写错了（比如把 0/0 当默认值）。
//	0/0 是几内亚湾，天气 API 会返回"那里的天气"且**不报错**。
func (l Location) Valid() bool {
	if strings.TrimSpace(l.Name) == "" {
		return false
	}
	if l.Lat < 3 || l.Lat > 54 || l.Lon < 73 || l.Lon > 135 {
		return false
	}
	return true
}

// unavailableProvider 是默认实现：本环境未配置天气数据源。
//
// # 它返回什么、不返回什么
//
// 返回 Available=false + **明确的 DegradedReason**，而不是一个假的晴天。
// 理由见 package 注释：假的"晴"会被 LLM 当事实使用，
// 产出"因为天气好所以生鲜卖得好"这类无法证伪的因果叙述 ——
// 那比不解释危害大得多，因为它会让店长相信一个错误结论。
type unavailableProvider struct {
	reason string
}

// NewUnavailableProvider 构造一个明确不可用的天气源。
//
// reason 会出现在后台的推理审计里，便于判断"天气缺失"是不是
// 长期状态（值得去配）还是偶发。
func NewUnavailableProvider(reason string) Provider {
	if strings.TrimSpace(reason) == "" {
		reason = "未配置天气数据源（本环境无 weather binding）"
	}
	return unavailableProvider{reason: reason}
}

func (p unavailableProvider) Fetch(context.Context, string, Location) (Info, error) {
	return unavailable(p.reason), nil
}
