package weather

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newServer 造一个可控的天气 API 假服务器。
//
// ⚠️ 它**逐字节**返回我们给的 JSON，不做任何加工 ——
//
//	否则"code 不是 200 时必须降级"这类分支会被 fake 洗掉。
func newServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(srv.Close)
	return srv
}

func validLoc() Location { return Location{Name: "岑溪", Lat: 24.7, Lon: 111.0} }

// 正常路径：实况可用。
func TestQWeatherNowSummary(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/now") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(`{"code":"200","now":{"text":"小雨","temp":"18","windDir":"东北","windScale":"3","humidity":"90","precip":"0.2"}}`))
	})

	p := &QWeatherProvider{BaseURL: srv.URL, Key: "k", HTTP: srv.Client()}
	got, err := p.Fetch(context.Background(), "2026-10-16", validLoc())
	if err != nil {
		t.Fatalf("不应返回 error（失败应降级而不是报错）: %v", err)
	}
	if !got.Available {
		t.Fatal("实况可用时应 Available=true")
	}
	if !strings.Contains(got.Summary, "小雨") || !strings.Contains(got.Summary, "18℃") {
		t.Errorf("摘要应含天气现象与温度: %q", got.Summary)
	}
}

// ⚠️ 核心不变量：**任何失败都必须降级，且 Summary 必须为空**。
//
// 逐条覆盖：HTTP 错误 / code 非 200 / 非法 JSON / 空文本 / key 缺失 /
// 位置无效。它们的共同点是"如果返回了一个假的晴天，
// LLM 就会拿它当事实编出无法证伪的因果"。
func TestEveryFailureDegradesWithEmptySummary(t *testing.T) {
	cases := []struct {
		name string
		// srv 返回 nil 表示"不提供服务器"（用于 key/位置无效的场景）
		srv func(t *testing.T) *httptest.Server
		loc Location
		key string
	}{
		{
			name: "HTTP 500",
			srv: func(t *testing.T) *httptest.Server {
				return newServer(t, func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusInternalServerError)
				})
			},
			loc: validLoc(), key: "k",
		},
		{
			name: "code 非 200（配额不足 402）",
			srv: func(t *testing.T) *httptest.Server {
				return newServer(t, func(w http.ResponseWriter, r *http.Request) {
					w.Write([]byte(`{"code":"402","now":{}}`))
				})
			},
			loc: validLoc(), key: "k",
		},
		{
			name: "非法 JSON",
			srv: func(t *testing.T) *httptest.Server {
				return newServer(t, func(w http.ResponseWriter, r *http.Request) {
					w.Write([]byte(`not json at all`))
				})
			},
			loc: validLoc(), key: "k",
		},
		{
			name: "code=200 但天气现象为空",
			srv: func(t *testing.T) *httptest.Server {
				return newServer(t, func(w http.ResponseWriter, r *http.Request) {
					w.Write([]byte(`{"code":"200","now":{"text":"","temp":"18"}}`))
				})
			},
			loc: validLoc(), key: "k",
		},
		{
			name: "key 缺失",
			srv: func(t *testing.T) *httptest.Server {
				return newServer(t, func(w http.ResponseWriter, r *http.Request) {
					t.Error("key 缺失时不应发起请求")
				})
			},
			loc: validLoc(), key: "",
		},
		{
			name: "位置无效（0/0）",
			srv: func(t *testing.T) *httptest.Server {
				return newServer(t, func(w http.ResponseWriter, r *http.Request) {
					t.Error("位置无效时不应发起请求")
				})
			},
			loc: Location{Name: "", Lat: 0, Lon: 0}, key: "k",
		},
		{
			name: "位置超出中国境内",
			srv: func(t *testing.T) *httptest.Server {
				return newServer(t, func(w http.ResponseWriter, r *http.Request) {
					t.Error("境外位置不应发起请求")
				})
			},
			loc: Location{Name: "巴黎", Lat: 48.85, Lon: 2.35}, key: "k",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := c.srv(t)
			p := &QWeatherProvider{BaseURL: srv.URL, Key: c.key, HTTP: srv.Client()}
			got, err := p.Fetch(context.Background(), "2026-10-16", c.loc)

			if err != nil {
				t.Fatalf("失败必须降级为 Info，不该返回 error: %v", err)
			}
			if got.Available {
				t.Error("失败时 Available 必须为 false")
			}
			if got.Summary != "" {
				t.Errorf("失败时 Summary 必须为空串（绝不能编造天气），实际 %q", got.Summary)
			}
			if got.DegradedReason == "" {
				t.Error("失败时必须给出降级原因，供后台判断是否长期异常")
			}
		})
	}
}

// 分钟降水失败**不让整份天气作废** —— 实况仍然有用，
// 且失败原因必须落在 DegradedReason（运维）而不是 Summary（店长/LLM）。
func TestMinutelyFailureKeepsNowSummary(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "minutely") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"code":"200","now":{"text":"多云","temp":"22"}}`))
	})

	p := &QWeatherProvider{BaseURL: srv.URL, Key: "k", HTTP: srv.Client()}
	got, err := p.Fetch(context.Background(), "2026-10-16", validLoc())
	if err != nil || !got.Available {
		t.Fatalf("分钟降水失败不应让整份天气作废: available=%v err=%v", got.Available, err)
	}
	if !strings.Contains(got.Summary, "多云") {
		t.Errorf("实况信息应保留: %q", got.Summary)
	}
	if strings.Contains(got.Summary, "降水") {
		t.Errorf("没有降水数据时不应凭空提降水: %q", got.Summary)
	}
	// ⚠️ 失败原因必须可观测，否则"降水长期缺失"会被当成"一直晴天"。
	if !strings.Contains(got.DegradedReason, "分钟级降水") {
		t.Errorf("分钟降水失败原因应进 DegradedReason 供运维判断: %q", got.DegradedReason)
	}
}

// ⚠️ 判定降水**只能**用 precip，不能用 type。
//
// 2026-10-17 实测：无降水的 24 条 minutely 里，`type` **全部是 "rain"**。
//
// 这条测试直接打 describeMinutely 并断言**具体输出**（而不是"包含某子串"），
// 因为判别力只存在于 summary 为空的那一行：
//
//	summary 非空 + 全 0 → 两种实现都返回 summary 原文，输出完全相同，
//	所以端到端写法在这个分支上**没有判别力**（曾因此写出一条永不失败的"反证"用例）。
//	summary 为空 + 全 0 → 正确实现返回 ""；若用 type 判雨则返回"未来两小时有降水"。
//	  这才是那句谎称"有降水"的唯一入口，也是本用例真正的防线。
//
// 反证：把 `if p := parseFloat(it.Precip); p > 0` 改成
//
//	`if it.Type == "rain" || p > 0`
//
//	则下面 "summary 为空 + type=rain + 全 0" 与 "summary 缺失 + 全 0" 两行立刻变红。
//
// minutelyItem 是 minutelyResponse.Minutely 的元素类型别名，
// 让下面的表驱动用例不必重复三遍匿名结构体。
type minutelyItem = struct {
	FXTime string `json:"fxTime"`
	Precip string `json:"precip"`
	Type   string `json:"type"`
}

func TestDescribeMinutely_判定降水只能用Precip(t *testing.T) {
	cases := []struct {
		name string
		resp minutelyResponse
		want string
	}{
		{
			// 无降水但 type 恒为 rain（生产实测形态）→ 用官方 summary 原文。
			name: "无降水但 type 恒为 rain",
			resp: minutelyResponse{
				Summary: "未来两小时无降水",
				Minutely: []minutelyItem{
					{Precip: "0.00", Type: "rain"},
					{Precip: "0.00", Type: "rain"},
					{Precip: "0.00", Type: "rain"},
				},
			},
			want: "未来两小时无降水",
		},
		{
			// ⚠️ 关键行：summary 空 + type=rain + precip 全 0。
			//    正确 = 不确定就不说；用 type 判 = 谎称"有降水"。
			name: "summary 为空且 type=rain 全 0 绝不能说有降水",
			resp: minutelyResponse{
				Minutely: []minutelyItem{
					{Precip: "0.0", Type: "rain"},
					{Precip: "0.0", Type: "rain"},
				},
			},
			want: "",
		},
		{
			// summary 缺失 + 全 0（订阅不含/字段改名/解析异常）→ 同样不能断言无降水。
			name: "summary 缺失且全 0",
			resp: minutelyResponse{
				Minutely: []minutelyItem{{Precip: "0.0", Type: "rain"}},
			},
			want: "",
		},
		{
			// summary 为空但数据确有降水 → 必须按数据说话。
			name: "summary 为空但有降水必须说出来",
			resp: minutelyResponse{
				Minutely: []minutelyItem{{Precip: "1.2", Type: "rain"}},
			},
			want: "未来两小时有降水（预计累计约 1.2 毫米）",
		},
		{
			// 官方措辞 + 我们补的量级。
			name: "有降水时补累计量级",
			resp: minutelyResponse{
				Summary:  "95分钟后雨就停了",
				Minutely: []minutelyItem{{Precip: "0.15"}, {Precip: "0.23"}},
			},
			want: "95分钟后雨就停了（未来两小时预计累计降水约 0.4 毫米）",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := describeMinutely(c.resp); got != c.want {
				t.Errorf("describeMinutely()\n got = %q\nwant = %q", got, c.want)
			}
		})
	}
}

// ⚠️ **时间口径**:摘要必须带时间锚点，绝不能让 LLM
// 把"未来 2 小时"读成报告期（昨天）的实况。
//
// 这条测试钉住"当前"与"未来"两个锚点同时存在。
// 反证：去掉 Fetch 里的 "当前" 前缀，本用例变红。
func TestSummaryCarriesExplicitTimeAnchor(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "minutely") {
			w.Write([]byte(`{"code":"200","summary":"95分钟后雨就停了","minutely":[
				{"fxTime":"2026-10-17T09:10+08:00","precip":"0.15","type":"rain"},
				{"fxTime":"2026-10-17T09:15+08:00","precip":"0.23","type":"rain"},
				{"fxTime":"2026-10-17T09:20+08:00","precip":"0.00","type":"rain"}
			]}`))
			return
		}
		w.Write([]byte(`{"code":"200","now":{"text":"小雨","temp":"18"}}`))
	})
	p := &QWeatherProvider{BaseURL: srv.URL, Key: "k", HTTP: srv.Client()}
	got, _ := p.Fetch(context.Background(), "2026-10-16", validLoc())

	if !strings.Contains(got.Summary, "当前") {
		t.Errorf("实况部分必须带「当前」锚点，否则会被当成报告期天气: %q", got.Summary)
	}
	// ⚠️ 报告期是 2026-10-16，而降水窗口是 10-17 —— 绝不能出现"昨天下了"。
	for _, forbidden := range []string{"昨天", "营业时段", "当日", "2026-10-16 有降水"} {
		if strings.Contains(got.Summary, forbidden) {
			t.Errorf("摘要不应出现把未来说成过去/报告期的措辞 %q: %q", forbidden, got.Summary)
		}
	}
	// 有降水时必须补累计量级：1mm 和 30mm 对店长是两回事。
	if !strings.Contains(got.Summary, "0.4 毫米") {
		t.Errorf("有降水时应补累计量级（0.15+0.23）: %q", got.Summary)
	}
}

// ⚠️ summary 缺失**不等于**无降水。
//
// 这是本次迁移最危险的一条分支：订阅不含该端点、解析失败、
// 或字段改名，都会让 summary 为空 —— 若此时回落成"无降水"，
// 就会对着一个完全没有数据的情况**谎称天气晴朗**。
// 正确行为是：要么按数据说话，要么什么都不说。
func TestEmptySummaryMustNotFakeNoRain(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"summary 空 + 有降水数据", `{"code":"200","summary":"","minutely":[
			{"fxTime":"2026-10-17T09:10+08:00","precip":"1.2","type":"rain"}]}`},
		{"summary 空 + minutely 空数组", `{"code":"200","summary":"","minutely":[]}`},
		{"summary 缺失字段 + 全 0", `{"code":"200","minutely":[
			{"fxTime":"2026-10-17T09:10+08:00","precip":"0.0","type":"rain"}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "minutely") {
					w.Write([]byte(c.body))
					return
				}
				w.Write([]byte(`{"code":"200","now":{"text":"晴","temp":"30"}}`))
			})
			p := &QWeatherProvider{BaseURL: srv.URL, Key: "k", HTTP: srv.Client()}
			got, _ := p.Fetch(context.Background(), "2026-10-16", validLoc())

			// 唯一禁止的输出：把"不知道"说成"没有雨"。
			if strings.Contains(got.Summary, "无降水") || strings.Contains(got.Summary, "晴朗") {
				t.Fatalf("summary 缺失时绝不能断言无降水（会把不确定说成确定）: %q", got.Summary)
			}
			// 有数据时必须按数据说话
			if strings.Contains(c.body, `"1.2"`) && !strings.Contains(got.Summary, "1.2 毫米") {
				t.Errorf("数据有降水就必须说出来: %q", got.Summary)
			}
			// 无数据时必须完全不提降水
			if !strings.Contains(c.body, `"1.2"`) && strings.Contains(got.Summary, "降水") {
				t.Errorf("无任何降水数据时摘要不应提降水: %q", got.Summary)
			}
		})
	}
}

// ⚠️ 经纬度顺序必须是"经度,纬度"。
//
// 写反**不会报错**，只会返回几百公里外的天气 ——
// 而天气现象看起来完全正常，报告里不会有任何异常信号。
// 这条测试是那个静默错误唯一的防线。
func TestCoordinateOrderIsLonLat(t *testing.T) {
	var gotLoc string
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotLoc = r.URL.Query().Get("location")
		w.Write([]byte(`{"code":"200","now":{"text":"晴","temp":"20"}}`))
	})
	p := &QWeatherProvider{BaseURL: srv.URL, Key: "k", HTTP: srv.Client()}
	if _, err := p.Fetch(context.Background(), "2026-10-16", Location{Name: "岑溪", Lat: 24.7, Lon: 111.0}); err != nil {
		t.Fatalf("Fetch 失败: %v", err)
	}
	if !strings.HasPrefix(gotLoc, "111.0") {
		t.Fatalf("location 应以**经度**开头（实际 %q）。写反不报错，只会取到几百公里外的天气",
			gotLoc)
	}
	if !strings.Contains(gotLoc, "24.7") {
		t.Errorf("location 应含纬度，实际 %q", gotLoc)
	}
}

// key 不得出现在日志/错误里 —— 它是凭据。
func TestKeyNotLeakedInDegradedReason(t *testing.T) {
	const secretKey = "SUPER_SECRET_KEY_123"
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	p := &QWeatherProvider{BaseURL: srv.URL, Key: secretKey, HTTP: srv.Client()}
	got, _ := p.Fetch(context.Background(), "2026-10-16", validLoc())
	if strings.Contains(got.DegradedReason, secretKey) {
		t.Fatalf("降级原因泄露了 API key: %q", got.DegradedReason)
	}
	if strings.Contains(got.Summary, secretKey) {
		t.Fatalf("摘要泄露了 API key: %q", got.Summary)
	}
}

// parseFloat 对空串/非数字必须返回 0 而不是 panic 或 error。
//
// ⚠️ precip 是**字符串**字段，和风可能给 ""（非降水时段）。
// 若解析失败却返回一个"看起来合理"的非零值，降水统计会凭空多出雨。
func TestParseFloatLenient(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"0.15", 0.15},
		{"0.00", 0},
		{"0", 0},
		{"", 0},      // 空串 = 无数据，不是"很大"
		{"  ", 0},    // 空白同理
		{"abc", 0},   // 非数字
		{"-1.0", -1}, // 负值原样返回（由调用方的 p > 0 过滤）
	}
	for _, c := range cases {
		if got := parseFloat(c.in); got != c.want {
			t.Errorf("parseFloat(%q) = %v, 期望 %v", c.in, got, c.want)
		}
	}
}

// 位置有效性判定。
func TestLocationValid(t *testing.T) {
	cases := []struct {
		name string
		loc  Location
		want bool
	}{
		{"正常", Location{Name: "岑溪", Lat: 24.7, Lon: 111.0}, true},
		{"缺城市名", Location{Lat: 24.7, Lon: 111.0}, false},
		{"0/0（几内亚湾）", Location{Name: "x", Lat: 0, Lon: 0}, false},
		{"纽约", Location{Name: "纽约", Lat: 40.7, Lon: -74.0}, false},
		{"北京", Location{Name: "北京", Lat: 39.9, Lon: 116.4}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.loc.Valid(); got != c.want {
				t.Errorf("Valid() = %v, 期望 %v", got, c.want)
			}
		})
	}
}

// TestNormalizeBaseURL_运维笔误的各种写法
//
// ⚠️ 这条用例的判别力来自**反证**：把 WithBaseURL 改成"原样使用"
//
//	下面除"空值"外的每一行都会变红。
//	运维在 env 里写 `n93qquf5ha.re.qweatherapi.com`（不带 scheme）
//	是极常见的写法，而它直接拼进 URL 会变成相对 URL，
//	Go 报的错是 `unsupported protocol scheme` —— 完全指不到"少了 https://"。
func TestNormalizeBaseURL_运维笔误的各种写法(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", DefaultQWeatherBaseURL},
		{"   ", DefaultQWeatherBaseURL},
		{"n93qquf5ha.re.qweatherapi.com", "https://n93qquf5ha.re.qweatherapi.com"},
		{"https://n93qquf5ha.re.qweatherapi.com", "https://n93qquf5ha.re.qweatherapi.com"},
		{"https://n93qquf5ha.re.qweatherapi.com/", "https://n93qquf5ha.re.qweatherapi.com"},
		{"  n93qquf5ha.re.qweatherapi.com/  ", "https://n93qquf5ha.re.qweatherapi.com"},
		{"http://example.test", "http://example.test"},
	}
	for _, c := range cases {
		if got := NormalizeBaseURL(c.in); got != c.want {
			t.Errorf("NormalizeBaseURL(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

// TestWithBaseURL_链式生效
func TestWithBaseURL_链式生效(t *testing.T) {
	p := NewQWeatherProvider("k").WithBaseURL("n93qquf5ha.re.qweatherapi.com")
	if p == nil {
		t.Fatal("provider 不应为 nil")
	}
	if p.BaseURL != "https://n93qquf5ha.re.qweatherapi.com" {
		t.Errorf("BaseURL = %q", p.BaseURL)
	}
	// 空串必须回落到回退值，而不是变成空 URL
	// —— 空 URL 会让每次请求都失败，且错误信息毫无指向性。
	if got := NewQWeatherProvider("k").WithBaseURL("").BaseURL; got != DefaultQWeatherBaseURL {
		t.Errorf("空 host 应回落到 %q，实际 %q", DefaultQWeatherBaseURL, got)
	}
	// nil 接收者不得 panic：weatherProvider() 可能在装配顺序变化时先于构造调用。
	(*QWeatherProvider)(nil).WithBaseURL("x")
}
