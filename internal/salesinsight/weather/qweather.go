package weather

import (
	"context"
	"encoding/json"
	"errors"
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
//   - 有**实况天气**与**分钟级降水**两个端点：实况给可读描述（现象/温度），
//     分钟级降水给"接下来会不会下雨、什么时候停"。
//   - 免费额度对单店日报足够（每天 1~2 次调用）。
//   - 有稳定的 JSON 契约与明确的 code 字段（错误码可判定）。
//
// # ⚠️ 时间口径：**报告期是过去，分钟级降水是未来**
//
// 这是本包最容易被忽略、也最危险的一处语义错配。
//
//	日报统计的是**昨天**（10-16 的日报在 10-17 生成），
//	而 /v7/minutely/5m 给的是**从现在起未来 2 小时**（实测 09:10 生成时，
//	返回窗口是 09:10~11:05）。
//
//	所以降水数据**永远不可能**落在报告期里 —— 早先基于
//	/v7/weather/24hour 的 `describeRainfall(date)` 按日期过滤的逻辑
//	在 minutely 下没有任何意义（该端点对本订阅已是 404，2027-06-01 停服）。
//
//	因此这里的处理是：
//	  - 实况摘要 → "现在晴，26℃"（明确的现在时）
//	  - 降水摘要 → "未来两小时无降水"（**直接用官方 summary 原文**）
//	  - 两者都带时间锚点，绝不让 LLM 把未来 2 小时当成昨天的实况。
//
//	宁可不解释，也不要一个"看起来合理、实则完全错误"的时间归因 ——
//	店长会照着"昨天没下雨所以生鲜卖得好"去砍品。
//
// # 最重要的不变量（不要破坏）
//
//	**任何失败、超时、配额不足、解析异常，都必须返回 Available=false
//	 且 Summary 为空串。**
//
// 绝不返回"默认晴天"。理由见本文件顶部的 package 注释：
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
	// ⚠️ 每次 Fetch 会对它发起 **2 次** HTTP 请求（now + minutely），
	//   所以配额是按"次请求"而不是"次报告"消耗的。
	//   单店单日约 2 次（日报）+ 2 次（周报）/月报，远低于 1500；
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

// nowResponse 是 /v7/weather/now 的响应（只取用到的字段）。
type nowResponse struct {
	Code string `json:"code"`
	Now  struct {
		Text      string `json:"text"` // 天气现象，如 "小雨"
		Temp      string `json:"temp"` // 温度（摄氏度）
		FeelsLike string `json:"feelsLike"`
		Humidity  string `json:"humidity"`
		WindDir   string `json:"windDir"`
		WindScale string `json:"windScale"`
		Precip    string `json:"precip"` // 降水量(mm)
	} `json:"now"`
}

// minutelyResponse 是 /v7/minutely/5m 的响应。
//
// # 实测（2026-10-17 09:10，生产订阅）
//
//	{"code":"200","updateTime":"2026-10-17T09:10+08:00","fxLink":"https://www.qweather.com",
//	 "summary":"未来两小时无降水",
//	 "minutely":[{"fxTime":"2026-10-17T09:10+08:00","precip":"0.00","type":"rain"}, ... 共 24 条]}
//
// # ⚠️ 两个只有实测才能发现的陷阱
//
//  1. **`type` 恒为 "rain"，哪怕完全没有降水。**
//     无降水时它照样是 `rain`（见上面的实测输出）。
//     → 所以判定"有没有雨"**必须**看 `precip > 0`，
//     用 type 判定会把晴天说成有雨，这是会误导店长的假事实。
//
//  2. **fxTime 带 +08:00 时区后缀**（`2026-10-17T09:10+08:00`）。
//     小时位置仍在 [11:13)，但营业时段判定必须按**东八区本地时间**，
//     不能把带偏移的时间戳当成 UTC 解析后再取小时。
type minutelyResponse struct {
	Code     string `json:"code"`
	Summary  string `json:"summary"` // 分钟降水描述，如 "未来两小时无降水"
	Minutely []struct {
		FXTime string `json:"fxTime"` // 预报时间，如 2026-10-17T09:10+08:00
		Precip string `json:"precip"` // 该 5 分钟累计降水量(mm)，字符串
		Type   string `json:"type"`   // rain / snow（⚠️ 无降水时也是 rain，不能据此判定）
	} `json:"minutely"`
}

// Fetch 实现 Provider。
//
// # 两个端点各自回答什么
//
//	实况   → "现在怎么样"（天气现象、温度、风）—— 生成时刻的实况
//	分钟降水 → "接下来会不会下雨" —— **未来 2 小时**，不是报告期
//
// # 为什么 date 参数在这里**不参与**降水计算
//
// ⚠️ 这是从 24hour 迁移到 minutely 后最关键的语义变化：
//
//	调用方传进来的 date 是**报告期首日**（通常是昨天），
//	而 minutely 的窗口永远是"从现在起未来 2 小时" —— 两者永不相交。
//	继续按 date 过滤只会让降水统计恒为空，然后**悄悄**得出
//	"营业时段无降水"这种看起来正常的结论。
//
//	→ date 只用于日志/调试语义的完整性，降水一律按"未来 2 小时"表述，
//	  并在摘要里带明确时间锚点，让 LLM 不可能把它读成昨天的实况。
func (p *QWeatherProvider) Fetch(ctx context.Context, date string, loc Location) (Info, error) {
	if p == nil || strings.TrimSpace(p.Key) == "" {
		return unavailable("天气数据源未配置（缺少 API key）"), nil
	}
	if !loc.Valid() {
		return unavailable("门店位置未配置或超出中国境内范围，无法定位天气"), nil
	}

	// ---- 实况 ----
	//
	// ⚠️ 实况失败即整体降级，因为它拿不到就没什么可拼的了。
	//
	// 配额耗尽会走到这里（实况本身也要消耗配额），而守卫按"不超发"原则
	// 在发请求前就拦下 —— 所以此时天气**整段消失**。
	//
	// 这是刻意的取舍：配额保护的意义就是"不浪费额度"，
	// 而不是一个"尽量用完额度"的优化目标。降级原因里带上了
	// 已用/上限数字，运维能直接判断是等次日恢复还是去提额。
	now, err := p.fetchNow(ctx, loc)
	if err != nil {
		return unavailable("天气查询失败：" + err.Error()), nil
	}

	// ---- 分钟级降水（未来 2 小时）----
	//
	// ⚠️ 分钟降水失败**不**让整份天气不可用：
	//   "当前晴，26℃" 仍然是有用的上下文，只是没有临近降水。
	//   把两者绑在一起会让一次接口抖动丢掉全部天气信息。
	//
	// ⚠️ 失败原因只进 DegradedReason（给运维），**绝不**进 Summary：
	//   摘要里出现"降水查询失败"会让 LLM 试图解释一个不存在的天气维度。
	summary := "当前" + describeNow(now)
	info := Info{Available: true, Summary: summary}
	if m, err := p.fetchMinutely(ctx, loc); err == nil {
		if s := describeMinutely(m); s != "" {
			info.Summary += "；" + s
		}
	} else if errors.Is(err, ErrQuotaExceeded) {
		// 配额耗尽**单独标记** —— 它不是"这次接口抖动"，
		// 而是当天剩余所有报告都会失败，运维必须能一眼区分。
		info.DegradedReason = err.Error()
	} else {
		// 保留原始判据：这样运维能从 facts 包判断"降水长期缺失"还是"偶尔抖动"。
		info.DegradedReason = "分钟级降水查询失败：" + err.Error()
	}

	return info, nil
}

// fetchNow 查实况。
func (p *QWeatherProvider) fetchNow(ctx context.Context, loc Location) (nowResponse, error) {
	var out nowResponse
	q := url.Values{}
	q.Set("location", coord(loc))
	q.Set("key", p.Key)
	if err := p.get(ctx, "/v7/weather/now", q, &out); err != nil {
		return out, err
	}
	if out.Code != "200" {
		return out, fmt.Errorf("和风天气返回 code=%s", out.Code)
	}
	if strings.TrimSpace(out.Now.Text) == "" {
		return out, fmt.Errorf("和风天气返回了空的天气现象")
	}
	return out, nil
}

// fetchMinutely 查分钟级降水（未来 2 小时）。
func (p *QWeatherProvider) fetchMinutely(ctx context.Context, loc Location) (minutelyResponse, error) {
	var out minutelyResponse
	q := url.Values{}
	q.Set("location", coord(loc))
	q.Set("key", p.Key)
	if err := p.get(ctx, "/v7/minutely/5m", q, &out); err != nil {
		return out, err
	}
	if out.Code != "200" {
		return out, fmt.Errorf("和风天气分钟级降水返回 code=%s", out.Code)
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
// ⚠️ 配额检查放在**这里**（而不是 Fetch 入口）是有意的：
//
//	Fetch 一次会发两个请求（now + minutely），配额是按**请求**计的。
//	若在 Fetch 入口检查一次，那第二次请求就绕过了守卫 ——
//	而这恰好是最容易漏的地方：守卫看着"生效"，实际发出量是预期的 2 倍。
//
//	放在真正发起前的那一行，是唯一结构上不可能绕过的地方。
func (p *QWeatherProvider) get(ctx context.Context, path string, q url.Values, out any) error {
	base := strings.TrimRight(p.BaseURL, "/")
	if base == "" {
		base = DefaultQWeatherBaseURL
	}
	// ⚠️ 先扣配额再发请求：配额按"发起"计，
	//   一个超时/500 的请求同样消耗额度。
	//   记"成功"会在网络故障时把真实消耗低估 2~3 倍。
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

// describeNow 把实况转成一句人话。
func describeNow(n nowResponse) string {
	var parts []string
	if t := strings.TrimSpace(n.Now.Text); t != "" {
		parts = append(parts, t)
	}
	if t := strings.TrimSpace(n.Now.Temp); t != "" {
		parts = append(parts, t+"℃")
	}
	if t := strings.TrimSpace(n.Now.WindDir); t != "" {
		scale := strings.TrimSpace(n.Now.WindScale)
		if scale != "" {
			parts = append(parts, t+"风"+scale+"级")
		} else {
			parts = append(parts, t+"风")
		}
	}
	if len(parts) == 0 {
		return "天气数据不完整"
	}
	return strings.Join(parts, "，")
}

// describeMinutely 把分钟级降水转成一句带时间锚点的人话。
//
// # 为什么优先用官方 summary 原文
//
//	和风自己就把"未来两小时无降水"/"95分钟后雨就停了"算好了，
//	而这类措辞（尤其"什么时候开始/结束"）由我们重拼极易出错：
//	窗口跨小时边界时，"还有多久"要按 5 分钟步长换算，
//	说错一个数就是一条看似合理的假事实。
//
//	→ 有 summary 就直接用；没有才回落到我们自己的统计。
//
// # ⚠️ 绝不能按报告期过滤（date）
//
//	minutely 的窗口恒为"从现在起未来 2 小时"，与报告期（昨天）不相交。
//	如果在这里按 date 过滤，结果恒为空 → 回落逻辑会输出"未来两小时无降水"
//	—— 那是一句**彻头彻尾的假话**（可能是订阅不含、也可能是解析失败）。
//
// # 有降水时补充累计量
//
//	summary 只说"有雨"，店长还需要知道量级（1mm 和 30mm 是两回事）。
//	所以有降水时用 minutely[] 累加 precip（每 5 分钟累计量）补一个总数。
func describeMinutely(m minutelyResponse) string {
	summary := strings.TrimSpace(m.Summary)
	total, rainy := 0.0, 0
	for _, it := range m.Minutely {
		// ⚠️ 判定降水**只能**用 precip，不能用 type ——
		//   实测无降水时 type 依然是 "rain"。
		if p := parseFloat(it.Precip); p > 0 {
			total += p
			rainy++
		}
	}

	switch {
	case summary != "" && rainy == 0:
		// 官方已明说无降水，且数据侧一致（24 条全为 0）→ 直接用原文。
		return summary
	case summary != "":
		// 有降水：官方措辞 + 我们算的量级。
		if total > 0 {
			return fmt.Sprintf("%s（未来两小时预计累计降水约 %.1f 毫米）", summary, total)
		}
		return summary
	case rainy > 0:
		// summary 缺失但数据有降水：仍然**不能说**"无降水"。
		return fmt.Sprintf("未来两小时有降水（预计累计约 %.1f 毫米）", total)
	default:
		// ⚠️ summary 空 + 全 0：这是"**不确定**"，不是"确定无降水"。
		//   返回空串 → 摘要里完全不提降水，而不是谎称无降水。
		return ""
	}
}

func parseFloat(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}
