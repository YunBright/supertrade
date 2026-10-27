package insight

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/salesinsight/fact"
	"github.com/YunBright/supertrade/internal/salesinsight/weather"
)

// 工具层：把"外部事实"变成模型**按需调用**的 API。
//
// # 为什么工具而不是"事实包里的一段文本"（2026-10-26）
//
// 原实现把天气**无条件**写进事实包，模型每次都会看到它。
// 后果有两个：
//
//  1. **时间对不上**：日报 22:50 生成，事实包里那段天气描述的是
//     "从现在起未来两小时"（即 22:50~00:50）—— 它讲的是
//     今天营业结束之后的天气，却会被当成"今天的实况"写进结论。
//     生产日报实际出现过「当前晴，22℃，北风3级；80分钟后开始下小雨」。
//  2. **解释成本**：模型必须自己判断"这段天气和今天的销量有没有关系"，
//     而它没有依据 —— 于是它会**硬凑因果**（"因为天气好所以生鲜卖得好"）。
//
// ⇒ 改成工具后，**要不要查天气由模型自己决定**，而且查到的结果
//
//	一定带绝对日期，不会串到别的日子。
//
// 工具按用途分成两个，刻意分开：
//
//	get_today_rainfall    —— 解释**今天**的异常（销量低、雨具涨）
//	get_tomorrow_forecast —— 为**明天/后天**的备货与运营建议提供依据
//
// 合成一个"查天气(日期)"的工具看似更灵活，实际更容易被模型
// 拿错日期（它会自己编一个日期参数），而日期错了整条建议就废了。
const (
	// ToolTodayRainfall 查询报告当天的天气与日降水量。
	ToolTodayRainfall = "get_today_rainfall"
	// ToolTomorrowForecast 查询次日的天气预报与日降水量。
	ToolTomorrowForecast = "get_tomorrow_forecast"
)

// ToolSpec 是暴露给模型的一个工具（函数）的声明。
//
// Parameters 是 JSON Schema。⚠️ 它**必须**是
// `{"type":"object","properties":{}}` 这种形状 ——
// 空的 `{}` 会被和风/DeepSeek 判为非法 schema（实测 400：
// "schema must be a JSON Schema of 'type: \"object\"'"）。
type ToolSpec struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// noArgParameters 是"无参数工具"的 JSON Schema。
func noArgParameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

// WeatherTools 是天气工具的目录 + 执行器。
//
// # 为什么工具定义与执行器放在一起
//
// 工具的**名字与描述**是给模型看的契约，
// 而**执行**要用到"今天是哪天""门店在哪"——
// 两者必须由同一处定义，否则改了名字忘了改执行分支，
// 模型会拿到一个"工具不存在"的静默失败。
type WeatherTools struct {
	// Provider 是天气数据源。nil 时工具不可用（会如实告诉模型）。
	Provider weather.Provider
	// Location 是门店位置。
	Location weather.Location
}

// Specs 返回天气工具的声明。
//
// ⚠️ 描述里必须写清**什么时候该调**、**什么时候不该调**：
// 模型是靠这段描述决定调不调的。
// 描述含糊 ⇒ 模型要么不调（工具白做），
// 要么乱调（把"明天有雨"套到"今天"上）。
func (w WeatherTools) Specs() []ToolSpec {
	return []ToolSpec{
		{
			Name: ToolTodayRainfall,
			Description: "查询**今天（报告期当天）**的天气现象与日降水量（毫米）。" +
				"仅在需要解释今天的客流或品类波动时才调用" +
				"（例如今天销量偏低、雨具销量上涨、生鲜不动）。" +
				"如果今天的数字没有需要用天气解释的异常，就不要调用。" +
				"返回内容带绝对日期与降水量，只能原样引用，不要改写日期。",
			Parameters: noArgParameters(),
		},
		{
			Name: ToolTomorrowForecast,
			Description: "查询**明天**的天气预报（天气现象 + 日降水量毫米）。" +
				"用于生成明天、后天的备货与运营建议" +
				"（例如明天有雨就要多备雨具、少备生鲜）。" +
				"如果建议不需要天气依据，就不要调用。" +
				"返回内容带绝对日期，只能原样引用，不要改写日期。",
			Parameters: noArgParameters(),
		},
	}
}

// Run 执行一次天气工具调用。
//
// # 返回值是**给模型看的文本**，不是结构体
//
// 模型只能看到文本，所以这里必须把"日期 + 现象 + 降水量"写成一句人话，
// 并在**开头就带上日期**——
// 否则模型会把"明天的雨"写成"今天下雨"（它没有别的时间锚）。
//
// # 不可用时**如实说不可用**
//
// 返回"天气数据不可用：<原因>"而不是编一个晴天。
// 模型拿到这句话会放弃用天气解释，而不是产出假因果。
func (w WeatherTools) Run(ctx context.Context, call ToolCall, f fact.Fact) (string, error) {
	if w.Provider == nil {
		return "天气数据不可用：未配置天气数据源。", nil
	}
	if !w.Location.Valid() {
		return "天气数据不可用：门店位置未配置（STORE_CITY/STORE_LAT/STORE_LON）。", nil
	}

	today, tomorrow, err := reportDays(f)
	if err != nil {
		return "天气数据不可用：无法确定报告日期。", nil
	}

	var date, label string
	switch call.Name {
	case ToolTodayRainfall:
		date, label = today, "今天"
	case ToolTomorrowForecast:
		date, label = tomorrow, "明天"
	default:
		return "", fmt.Errorf("insight: 未知天气工具 %q", call.Name)
	}

	d, err := w.Provider.Daily(ctx, date, w.Location)
	if err != nil {
		return fmt.Sprintf("%s（%s）天气查询失败：%v", label, date, err), nil
	}
	if !d.Available {
		reason := d.DegradedReason
		if strings.TrimSpace(reason) == "" {
			reason = "数据源未返回结果"
		}
		return fmt.Sprintf("%s（%s）天气数据不可用：%s", label, date, reason), nil
	}

	// ⚠️ 降水量必须带上：店长要的是量级（1mm 与 30mm 是两回事），
	//   而"小雨"这种现象描述说不出量级。
	extra := ""
	if d.IsRainy() {
		extra = "，有降水"
	} else {
		extra = "，无降水"
	}
	return fmt.Sprintf("%s（%s）%s，日降水量 %.1f 毫米%s。",
		label, d.Date, d.Text, d.PrecipMM, extra), nil
}

// reportDays 推出"今天"与"明天"（YYYY-MM-DD）。
//
// ⚠️ 以事实包的 Meta.Today 为准，**不是**事实包的 From：
//
//	日报的 From 就是今天，两者一致；
//	但周报/月报的 From 是上一周/上个月 ——
//	用 From 去问"今天下雨吗"会问到上周的天气。
func reportDays(f fact.Fact) (today, tomorrow string, err error) {
	today = strings.TrimSpace(f.Meta.Today)
	if today == "" {
		return "", "", fmt.Errorf("insight: 事实包缺少 Meta.Today")
	}
	t, err := time.Parse("2006-01-02", today)
	if err != nil {
		return "", "", fmt.Errorf("insight: 解析 Meta.Today %q 失败: %w", today, err)
	}
	return today, t.AddDate(0, 0, 1).Format("2006-01-02"), nil
}
