// Package weather 提供**按日**天气上下文，供日报解释客流波动。
//
// # 设计原则：天气是增强项，永远不是必需项
//
// 天气影响零售是真的（下雨客流降、生鲜受损；高温利好冷饮），
// 但它绝不能成为日报能否生成的前置条件。所以本包的契约是：
//
//	拿不到天气 ⇒ 报告照出，只是少一段解释。
//
// # ⚠️ 2026-10-26 的口径变更：为什么只剩"按日"这一种查询
//
//	原实现有两条查询：
//	  - /v7/weather/now   → "现在晴，26℃"（实况）
//	  - /v7/minutely/5m   → "未来两小时无降水" / "80分钟后开始下小雨"
//
//	第二条是**报告里出现过假事实的根源**。日报在 22:50 生成，
//	而分钟级降水的窗口恒为"从现在起未来 2 小时"（即 22:50~00:50）——
//	它描述的是**今天营业结束之后**的天气，与"今天卖得怎么样"毫无关系，
//	却会被 LLM 当成今天的实况写进结论（生产日报实际出现过
//	"当前晴，22℃，北风3级；80分钟后开始下小雨"）。
//
//	⇒ 现在只保留**按日**查询（/v7/weather/7d）：
//	  - 某一天的天气现象与日降水量（今天用来看"今天为什么销量低"，
//	    明天用来看"明天要不要多进雨具/少进生鲜"）。
//	  - 每条结果都带**绝对日期**，不可能被读成别的一天。
//
// # 最重要的不变量（不要破坏）
//
//	**任何失败、超时、配额不足、解析异常，都必须返回 Available=false
//	 且 Summary 为空串。**
//
// 绝不返回"默认晴天"。理由见本文件顶部的 package 注释：
// 假的"晴"会被 LLM 当作事实，产出"因为天气好所以生鲜卖得好"
// 这类无法证伪的因果叙述 —— 那比不解释危害大得多。
package weather

import (
	"context"
	"strings"
)

// Daily 是某一天的天气。
type Daily struct {
	// Available 表示这份天气是否可信。
	//
	// ⚠️ 它必须与 Summary 分开：Available=false 时 Summary **必须为空串**。
	//   两者都填、或降级时填个"晴"，下游（尤其是 LLM）会把它当事实用。
	Available bool `json:"available"`
	// Date 是这一天（YYYY-MM-DD）。
	//
	// ⚠️ 它**必须**带上：天气一旦脱离日期就会被读成"今天"，
	//   而"明天有雨"被写成"今天有雨"会让店长做出完全相反的备货决策。
	Date string `json:"date,omitempty"`
	// Text 是天气现象（如 "小雨"、"晴"）。
	Text string `json:"text,omitempty"`
	// PrecipMM 是当日降水量（毫米）。
	//
	// ⚠️ 判定"有没有雨"**只能**看它，不能看 Text：
	//   "阴" 的降水量可能是 0，也可能是 8 毫米；量级才是店长要的信息
	//   （1mm 和 30mm 是两回事）。
	PrecipMM float64 `json:"precip_mm,omitempty"`
	// TempMinC / TempMaxC 是当日最低/最高气温（摄氏度）。
	TempMinC float64 `json:"temp_min_c,omitempty"`
	TempMaxC float64 `json:"temp_max_c,omitempty"`
	// Summary 是给店长/LLM 看的一句话，**必带日期**。
	//
	// 例："2026-10-27 晴，日降水量 0 毫米，气温 19~30℃"
	Summary string `json:"summary,omitempty"`
	// DegradedReason 说明为什么拿不到，是**给运维看的**，
	// 不直接展示给店长。
	DegradedReason string `json:"degraded_reason,omitempty"`
}

// IsRainy 判断这一天是否有可观测降水。
//
// ⚠️ 判据是**降水量**，不是天气现象：
// "阴"/"多云" 的降水量可以是 0，而"小雨"在数值上也可能极小。
// 阈值取 0.1mm —— 小于它等于没有可感知的雨。
func (d Daily) IsRainy() bool {
	return d.Available && d.PrecipMM >= 0.1
}

// Provider 是天气数据源。
type Provider interface {
	// Daily 取某一天的天气。date 为 YYYY-MM-DD。
	//
	// ⚠️ 返回的 Daily.Available=false 时，Summary 必须是空串 ——
	//   这是本接口最重要的不变量，见 package 注释。
	Daily(ctx context.Context, date string, location Location) (Daily, error)
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
		reason = "未配置天气数据源"
	}
	return unavailableProvider{reason: reason}
}

func (p unavailableProvider) Daily(context.Context, string, Location) (Daily, error) {
	return Daily{Available: false, DegradedReason: p.reason}, nil
}
