package weather

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// QWeatherProvider 是基于和风天气（QWeather）开发服务的实现。
//
// # 为什么选它
//
//   - 有**按日预报**端点（/v7/weather/7d）：一次请求拿到未来 7 天
//     每一天的天气现象与**日降水量**，正好覆盖日报需要的两个场景
//     （今天为什么卖得差 / 明天要不要改备货）。
//   - 免费额度对单店日报足够（每天 1~2 次调用）。
//   - 有稳定的 JSON 契约与明确的 code 字段（错误码可判定）。
//
// # ⚠️ 为什么不再用分钟级降水（2026-10-26）
//
//	原实现查 /v7/minutely/5m，把"未来两小时"的降水摘要写进事实包。
//	日报在 22:50 生成 ⇒ 那个窗口是 22:50~00:50，描述的是
//	**今天营业结束之后**的天气，却会被 LLM 当成今天的实况写进结论
//	（生产日报实际出现过"80分钟后开始下小雨"）。
//
//	⇒ 换成按日预报：每条结果都带绝对日期与日降水量，
//	  今天的与明天的互不串味，也不存在"未来两小时"这种与报告期无关的时间锚。
//
// # 最重要的不变量（不要破坏）
//
//	**任何失败、超时、配额不足、解析异常，都必须返回 Available=false
//	 且 Summary 为空串。**
//
// 绝不返回"默认晴天"。理由见 weather.go 顶部的 package 注释：
// 假的"晴"会被 LLM 当作事实，产出"因为天气好所以生鲜卖得好"
// 这类无法证伪的因果叙述 —— 那比不解释危害大得多。
//
// # key 的存放
//
// 从 env WEATHER_QWEATHER_KEY 读，**不写进代码或仓库**。
// 部署时由 systemd EnvironmentFile 提供。
type QWeatherProvider struct {
	// BaseURL 是 API 根地址。做成字段是为了单测可注入 httptest 服务器。
	BaseURL string
	// Key 是和风天气的 API key。
	Key string
	// HTTP 是客户端。Timeout 不可为 0 —— 天气服务慢时会拖垮整个日报生成。
	HTTP *http.Client
	// Quota 是每日调用配额守卫。nil = 不限制（仅测试/本地）。
	//
	// ⚠️ 每次 Daily 只发 **1** 次 HTTP 请求（7d 端点一次覆盖 7 天），
	//   所以配额是按"次请求"而不是"次报告"消耗的。
	//   单店单日约 0~2 次（模型决定是否调用工具），远低于 1500；
	//   真正的风险是重试风暴 —— 守卫的作用是在风暴里**先降级而不是先耗尽**。
	Quota *Quota
}

// DefaultQWeatherBaseURL 是和风天气的**回退**根地址。
//
// ⚠️ 它在 2026-10-17 实测中对生产那个 key **不可用**：
//
//	`devapi.qweather.com` / `api.qweather.com` / `geoapi.qweather.com`
//	一律 403 `invalid-host`（"An invalid or unauthorized API Host"）。
//
//	这个错误**不是 key 错了**，而是和风按订阅套餐分配 host：
//	新账号拿到的是形如 `xxxx.re.qweatherapi.com` 的专属 host。
//	→ 生产必须用 env `WEATHER_QWEATHER_BASE_URL` 显式指定（见 cmd/sales-insight）。
//
//	保留它作为回退只为让本地开发在无 env 时有个可用的默认值，
//	**不要**把任何真实 host 硬编码进这里 —— 换账号就失效。
const DefaultQWeatherBaseURL = "https://devapi.qweather.com"

// DefaultQWeatherTimeout 是单次天气查询超时。
//
// 取 6s：天气是**增强项**，它的延迟必须远小于日报生成的整体预算
// （LLM 单次可达 45s）。6s 拿不到就直接降级，
// 不让一个可有可无的信息拖慢推送。
const DefaultQWeatherTimeout = 6 * time.Second

// EnvQWeatherBaseURL 是覆盖 base URL 的环境变量名。
const EnvQWeatherBaseURL = "WEATHER_QWEATHER_BASE_URL"

// NormalizeBaseURL 归一化 host 配置：补 https://、去尾斜杠。
//
// ⚠️ 为什么必须归一而不是原样用：运维在 env 里写
//
//	`n93qquf5ha.re.qweatherapi.com`（不带 scheme）是极常见的写法，
//	而它直接拼进 URL 会变成 `n93qquf5ha.re.qweatherapi.com/v7/...`，
//	变成一个**相对 URL**，Go 的 http 会报
//	`unsupported protocol scheme` —— 错误文案完全指不到"少了 https://"。
//
//	留空返回回退值（见 DefaultQWeatherBaseURL 的警告）。
func NormalizeBaseURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return DefaultQWeatherBaseURL
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	return strings.TrimRight(s, "/")
}

// NewQWeatherProvider 构造。key 为空返回 nil（调用方据此降级）。
func NewQWeatherProvider(key string) *QWeatherProvider {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	return &QWeatherProvider{
		BaseURL: DefaultQWeatherBaseURL,
		Key:     key,
		HTTP:    &http.Client{Timeout: DefaultQWeatherTimeout},
	}
}

// WithBaseURL 设置 base URL（已归一）。返回自身便于链式调用。
func (p *QWeatherProvider) WithBaseURL(raw string) *QWeatherProvider {
	if p != nil {
		p.BaseURL = NormalizeBaseURL(raw)
	}
	return p
}

// dailyResponse 是 /v7/weather/7d 的响应（只取用到的字段）。
//
// # 实测（2026-10-26 21:00，生产订阅）
//
//	{"code":"200","updateTime":"2026-10-26T21:00+08:00",
//	 "daily":[{"fxDate":"2026-10-26","textDay":"晴","textNight":"晴",
//	           "precip":"0.0","tempMax":"31","tempMin":"19", ...}, ... 共 7 条]}
//
// # ⚠️ 关于 precip
//
//	它是**日降水量**（毫米），"0.0" 表示当天无降水。
//	判定"有没有雨"只能用它，不能用 textDay —— "阴"的 precip 可能是 0，
//	而量级（1mm 与 30mm）才是店长做备货决策需要的。
type dailyResponse struct {
	Code  string       `json:"code"`
	Daily []dailyEntry `json:"daily"`
}

// dailyEntry 是 7 天预报里的一天。
type dailyEntry struct {
	FXDate    string `json:"fxDate"`    // 预报日期，如 2026-10-26
	TextDay   string `json:"textDay"`   // 白天天气现象
	TextNight string `json:"textNight"` // 夜间天气现象
	Precip    string `json:"precip"`    // 日降水量(mm)，字符串
	TempMax   string `json:"tempMax"`   // 最高气温
	TempMin   string `json:"tempMin"`   // 最低气温
}

// Daily 实现 Provider。
//
// # 一次请求覆盖 7 天
//
//	/v7/weather/7d 一次返回今天起 7 天，所以"今天"与"明天"两次查询
//	各自发一次请求（各 1 次配额），而不是各查各的端点。
//
// # 找不到那一天时怎么办
//
//	降级为"不可用"，**不回退到最近的一天**。
//	回退会让"明天的天气"变成"今天的天气"——
//	那是本包最不能犯的错（店长会照着错误的天气改备货）。
func (p *QWeatherProvider) Daily(ctx context.Context, date string, loc Location) (Daily, error) {
	if p == nil || strings.TrimSpace(p.Key) == "" {
		return Daily{Available: false, DegradedReason: "天气数据源未配置（缺少 API key）"}, nil
	}
	if !loc.Valid() {
		return Daily{Available: false, DegradedReason: "门店位置未配置或超出中国境内范围，无法定位天气"}, nil
	}

	resp, err := p.fetchDaily(ctx, loc)
	if err != nil {
		return Daily{Available: false, DegradedReason: "天气查询失败：" + err.Error()}, nil
	}

	day, ok := findDay(resp, date)
	if !ok {
		return Daily{
			Available:      false,
			Date:           date,
			DegradedReason: fmt.Sprintf("天气数据里没有 %s 这一天（预报只覆盖今明等 7 天）", date),
		}, nil
	}

	out := Daily{
		Available: true,
		Date:      day.FXDate,
		Text:      pickText(day),
		PrecipMM:  parseFloat(day.Precip),
		TempMinC:  parseFloat(day.TempMin),
		TempMaxC:  parseFloat(day.TempMax),
	}
	out.Summary = describeDaily(out)
	return out, nil
}

// findDay 在 7 天列表里找到指定日期。
func findDay(r dailyResponse, date string) (dailyEntry, bool) {
	for _, d := range r.Daily {
		if d.FXDate == date {
			return d, true
		}
	}
	return dailyEntry{}, false
}

// pickText 选一个天气现象。
//
// ⚠️ 优先用白天的：备货决策发生在白天营业时段，
// 夜间现象（"晴转多云"的夜间）对店长基本无用。
func pickText(d dailyEntry) string {
	if t := strings.TrimSpace(d.TextDay); t != "" {
		return t
	}
	return strings.TrimSpace(d.TextNight)
}

// describeDaily 把一天拼成一句**带日期**的人话。
//
// ⚠️ 日期必须在最前面：一旦脱离日期，
// "晴，日降水量 0 毫米"就可能被读成"今天"或"任何一天"。
func describeDaily(d Daily) string {
	parts := []string{d.Date}
	if t := strings.TrimSpace(d.Text); t != "" {
		parts = append(parts, t)
	} else {
		parts = append(parts, "天气现象未知")
	}
	parts = append(parts, fmt.Sprintf("日降水量 %.1f 毫米", d.PrecipMM))
	if d.TempMinC != 0 || d.TempMaxC != 0 {
		parts = append(parts, fmt.Sprintf("气温 %.0f~%.0f℃", d.TempMinC, d.TempMaxC))
	}
	return strings.Join(parts, "，")
}

// fetchDaily 查 7 天预报。
func (p *QWeatherProvider) fetchDaily(ctx context.Context, loc Location) (dailyResponse, error) {
	var out dailyResponse
	q := url.Values{}
	q.Set("location", coord(loc))
	q.Set("key", p.Key)
	if err := p.get(ctx, "/v7/weather/7d", q, &out); err != nil {
		return out, err
	}
	if out.Code != "200" {
		return out, fmt.Errorf("和风天气返回 code=%s", out.Code)
	}
	if len(out.Daily) == 0 {
		return out, fmt.Errorf("和风天气返回了空的日预报列表")
	}
	return out, nil
}

// coord 拼坐标串。QWeather 用 "经度,纬度"（**经度在前**）。
//
// ⚠️ 顺序写反不会报错，只会返回**几百公里外**的天气 ——
//
//	而报告显示的天气现象看起来完全正常。
//	这是本函数唯一的存在理由（除了拼串本身）。
func coord(loc Location) string {
	return fmt.Sprintf("%s,%s",
		strconv.FormatFloat(loc.Lon, 'f', 4, 64),
		strconv.FormatFloat(loc.Lat, 'f', 4, 64))
}

// get 发一次 GET 并解析 JSON。
//
// ⚠️ 配额检查放在**这里**（而不是 Daily 入口）是有意的：
//
//	配额是按"请求"计的，检查必须落在真正发起前的那一行，
//	那是唯一结构上不可能绕过的地方。
//
// ⚠️ 先扣配额再发请求：配额按"发起"计，
//   一个超时/500 的请求同样消耗额度。
//   记"成功"会在网络故障时把真实消耗低估 2~3 倍。
func (p *QWeatherProvider) get(ctx context.Context, path string, q url.Values, out any) error {
	base := strings.TrimRight(p.BaseURL, "/")
	if base == "" {
		base = DefaultQWeatherBaseURL
	}
	if ok, err := p.Quota.AllowContext(ctx); !ok {
		return err
	}
	full := base + path + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return err
	}
	client := p.HTTP
	if client == nil {
		client = &http.Client{Timeout: DefaultQWeatherTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// ⚠️ 限制读取上限 1MB：异常响应体不该把内存吃光。
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("解析响应失败: %w", err)
	}
	return nil
}

// parseFloat 解析字符串数字，失败返回 0。
func parseFloat(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}
