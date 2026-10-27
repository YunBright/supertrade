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

// ok7d 造一份"7 天预报"的响应体（只关心给定的几天）。
func ok7d(days ...string) string {
	return `{"code":"200","daily":[` + strings.Join(days, ",") + `]}`
}

// 正常路径：按日预报可用。
func TestQWeatherDailySummary(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(ok7d(
			`{"fxDate":"2026-10-26","textDay":"小雨","textNight":"阴","precip":"3.2","tempMax":"26","tempMin":"19"}`,
			`{"fxDate":"2026-10-27","textDay":"晴","textNight":"晴","precip":"0.0","tempMax":"31","tempMin":"19"}`,
		)))
	})

	p := &QWeatherProvider{BaseURL: srv.URL, Key: "k", HTTP: srv.Client()}
	got, err := p.Daily(context.Background(), "2026-10-26", validLoc())
	if err != nil {
		t.Fatalf("不应返回 error（失败应降级而不是报错）: %v", err)
	}
	if !got.Available {
		t.Fatal("按日预报可用时应 Available=true")
	}
	if got.Date != "2026-10-26" {
		t.Errorf("Date = %q，期望 2026-10-26", got.Date)
	}
	if got.Text != "小雨" {
		t.Errorf("Text = %q，期望 小雨（应优先取白天现象）", got.Text)
	}
	if got.PrecipMM != 3.2 {
		t.Errorf("PrecipMM = %v，期望 3.2", got.PrecipMM)
	}
	if !got.IsRainy() {
		t.Error("3.2 毫米应判为有雨")
	}
	if !strings.Contains(got.Summary, "2026-10-26") {
		t.Errorf("摘要必须带绝对日期，否则会被读成别的日子: %q", got.Summary)
	}
	if !strings.Contains(got.Summary, "小雨") || !strings.Contains(got.Summary, "3.2 毫米") {
		t.Errorf("摘要应含天气现象与降水量: %q", got.Summary)
	}
}

// ⚠️ 核心不变量：**任何失败都必须降级，且 Summary 必须为空**。
//
// 逐条覆盖：HTTP 错误 / code 非 200 / 非法 JSON / 空列表 / key 缺失 /
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
					w.Write([]byte(`{"code":"402","daily":[]}`))
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
			name: "code=200 但 daily 为空",
			srv: func(t *testing.T) *httptest.Server {
				return newServer(t, func(w http.ResponseWriter, r *http.Request) {
					w.Write([]byte(`{"code":"200","daily":[]}`))
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
			got, err := p.Daily(context.Background(), "2026-10-26", c.loc)

			if err != nil {
				t.Fatalf("失败必须降级为 Daily，不该返回 error: %v", err)
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

// ⚠️ 找不到那一天时**必须降级**，绝不能回退到"最近的一天"。
//
// 这是本包最不能犯的错：回退会让"明天的天气"变成"今天的天气"——
// 店长会照着错误的天气改备货。
func TestDateNotInForecastDegradesInsteadOfFallingBack(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(ok7d(
			`{"fxDate":"2026-10-26","textDay":"晴","precip":"0.0"}`,
			`{"fxDate":"2026-10-27","textDay":"晴","precip":"0.0"}`,
		)))
	})
	p := &QWeatherProvider{BaseURL: srv.URL, Key: "k", HTTP: srv.Client()}
	got, err := p.Daily(context.Background(), "2026-11-30", validLoc())
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if got.Available {
		t.Fatal("预报里没有这一天时必须不可用，绝不能回退到别的日子")
	}
	if got.Summary != "" {
		t.Errorf("不可用时 Summary 必须为空串，实际 %q", got.Summary)
	}
	if !strings.Contains(got.DegradedReason, "2026-11-30") {
		t.Errorf("降级原因应点名缺的是哪一天: %q", got.DegradedReason)
	}
}

// ⚠️ 无降水与"有降水"的判定**只能**看 precip。
//
// "阴"的降水量可以是 0；而量级（1mm 与 30mm）是两回事。
func TestPrecipDrivesRainJudgment(t *testing.T) {
	cases := []struct {
		name      string
		day       string
		wantRain  bool
		wantMM    float64
	}{
		{"晴且 0 毫米", `{"fxDate":"2026-10-26","textDay":"晴","precip":"0.0"}`, false, 0},
		{"阴但 0 毫米", `{"fxDate":"2026-10-26","textDay":"阴","precip":"0.0"}`, false, 0},
		{"小雨 1.5 毫米", `{"fxDate":"2026-10-26","textDay":"小雨","precip":"1.5"}`, true, 1.5},
		{"大雨 28.0 毫米", `{"fxDate":"2026-10-26","textDay":"大雨","precip":"28.0"}`, true, 28},
		{"precip 字段缺失", `{"fxDate":"2026-10-26","textDay":"晴"}`, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(ok7d(c.day)))
			})
			p := &QWeatherProvider{BaseURL: srv.URL, Key: "k", HTTP: srv.Client()}
			got, _ := p.Daily(context.Background(), "2026-10-26", validLoc())
			if !got.Available {
				t.Fatalf("应可用，实际不可用: %q", got.DegradedReason)
			}
			if got.IsRainy() != c.wantRain {
				t.Errorf("IsRainy() = %v，期望 %v（precip=%v）", got.IsRainy(), c.wantRain, got.PrecipMM)
			}
			if got.PrecipMM != c.wantMM {
				t.Errorf("PrecipMM = %v，期望 %v", got.PrecipMM, c.wantMM)
			}
		})
	}
}

// ⚠️ 夜间现象不能顶替白天现象。
//
// 备货决策发生在白天营业时段，"晴转多云"的夜间对店长基本无用。
func TestPrefersDaytimeText(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(ok7d(
			`{"fxDate":"2026-10-26","textDay":"","textNight":"雷阵雨","precip":"5.0"}`,
		)))
	})
	p := &QWeatherProvider{BaseURL: srv.URL, Key: "k", HTTP: srv.Client()}
	got, _ := p.Daily(context.Background(), "2026-10-26", validLoc())
	if got.Text != "雷阵雨" {
		t.Errorf("白天现象为空时应回落到夜间现象，实际 %q", got.Text)
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
		w.Write([]byte(ok7d(`{"fxDate":"2026-10-26","textDay":"晴","precip":"0.0"}`)))
	})
	p := &QWeatherProvider{BaseURL: srv.URL, Key: "k", HTTP: srv.Client()}
	if _, err := p.Daily(context.Background(), "2026-10-26", Location{Name: "岑溪", Lat: 24.7, Lon: 111.0}); err != nil {
		t.Fatalf("Daily 失败: %v", err)
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
	got, _ := p.Daily(context.Background(), "2026-10-26", validLoc())
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
		{"-1.0", -1}, // 负值原样返回（由调用方的阈值过滤）
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

// ⚠️ 不可用 provider 也必须守住"Available=false + Summary 空"的不变量。
func TestUnavailableProviderKeepsInvariant(t *testing.T) {
	p := NewUnavailableProvider("测试原因")
	got, err := p.Daily(context.Background(), "2026-10-26", validLoc())
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if got.Available {
		t.Error("不可用时 Available 必须为 false")
	}
	if got.Summary != "" {
		t.Errorf("不可用时 Summary 必须为空串，实际 %q", got.Summary)
	}
	if got.DegradedReason == "" {
		t.Error("不可用时必须给出降级原因")
	}
}
