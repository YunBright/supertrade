// Package cmdbootstrap —— 访问日志中间件的契约守卫(内部测试包)。
//
// # 为什么要有这个文件
//
// 访问日志的失败模式**全是静默的**:
//
//   - 路径记成原始 URL ⇒ query 里的 token 被整份贴进工单/群聊(泄漏);
//   - 探活 / dapr 内部路由也记 ⇒ 真正的业务请求被刷掉,日志失去意义;
//   - 字段少了 sub / branch ⇒ 多店场景下"谁在哪家店踩到的"无从查起。
//
// 而这三种都不会让任何一条业务用例变红 ⇒ 必须有断言盯着。
//
// ⚠️ 本文件断言的是**日志里到底有什么**,不是"中间件被调用了"。
// 前者才是排障时真正依赖的东西。
//
// ⚠️ 用内部测试包(package cmdbootstrap)而不是 cmdbootstrap_test:
//
//	accessLog / errorLog / recoveryMiddleware 是**未导出**的,
//	要单独挂到裸 engine 上测(不经过鉴权 ⇒ 状态码可控),必须能直接调到它们。
package cmdbootstrap

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// accessLogTestEngine 装一个只挂访问日志的 engine,返回 engine 与日志 buffer。
func accessLogTestEngine(t *testing.T) (*gin.Engine, *bytes.Buffer) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	buf := &bytes.Buffer{}
	r := gin.New()
	r.Use(accessLog(slog.New(slog.NewJSONHandler(buf, nil))))
	return r, buf
}

// accessLogLines 抽出 buffer 里的**访问日志**行(message = "http access")。
//
// ⚠️ 只认 message 是访问日志的那几行 ——
//
//	同一个 buffer 里可能还有 business error / panic recovered 行
//	(那是另外两个中间件的职责),混进来会让"有几行访问日志"的断言失真。
func accessLogLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("访问日志不是合法 JSON: %v(line=%s)", err, line)
		}
		if m["msg"] != accessLogMessage {
			continue
		}
		out = append(out, m)
	}
	return out
}

// TestAccessLog_BusinessRequestIsLogged 锁"业务请求要留痕,且字段齐全"。
//
// ⚠️ 字段少了 sub / branch ⇒ 多店场景下"谁在哪家店踩到的"查不出来,
// 而这正是排障时第一时间要问的问题。
func TestAccessLog_BusinessRequestIsLogged(t *testing.T) {
	r, buf := accessLogTestEngine(t)
	r.GET("/biz/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest(http.MethodGet, "/biz/ping", nil)
	req.Header.Set(branchHeader, "10")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	lines := accessLogLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("业务请求应留 1 行访问日志,实际 %d 行: %s", len(lines), buf.String())
	}
	got := lines[0]
	for _, key := range []string{
		"method", "path", "status", "duration_ms", "ip", "bytes", "sub", "branch",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("访问日志缺字段 %q: %v", key, got)
		}
	}
	if got["method"] != http.MethodGet {
		t.Errorf("method = %v, want GET", got["method"])
	}
	if got["path"] != "/biz/ping" {
		t.Errorf("path = %v, want /biz/ping", got["path"])
	}
	if got["status"] != float64(http.StatusOK) {
		t.Errorf("status = %v, want 200", got["status"])
	}
	if got["branch"] != "10" {
		t.Errorf("branch = %v, want 10", got["branch"])
	}
	if _, ok := got["duration_ms"].(float64); !ok {
		t.Errorf("duration_ms 应是数字: %v", got["duration_ms"])
	}
	if _, ok := got["ip"].(string); !ok {
		t.Errorf("ip 应是字符串: %v", got["ip"])
	}
	if b, ok := got["bytes"].(float64); !ok || b <= 0 {
		t.Errorf("bytes 应是响应体字节数(>0): %v", got["bytes"])
	}
}

// TestAccessLog_HealthzIsNotLogged 锁"探活不记日志"。
//
// ⚠️ 探活每 3 秒一次 ⇒ 记进访问日志会把真正的业务请求**刷掉**,
// 而"日志里少了业务请求"这种故障没人会发现。
//
// ⇒ 走**真实装配**(NewRouter):/healthz 的跳过是注册时登记的,
//
//	不是某个中间件里硬编码的一张路径表。
func TestAccessLog_HealthzIsNotLogged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	buf := &bytes.Buffer{}
	r := NewRouter(Options{
		AppID:           "probe",
		Audience:        "probe",
		AllowUnverified: true,
		Logger:          slog.New(slog.NewJSONHandler(buf, nil)),
	})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if lines := accessLogLines(t, buf); len(lines) != 0 {
		t.Errorf("/healthz 不该记访问日志,实际: %s", buf.String())
	}
}

// TestAccessLog_DaprPathsAreNotLogged 锁"dapr 内部路由不记日志"。
//
// ⚠️ sidecar 每 10 分钟重注册 job、CloudEvents 持续推送 ⇒
//
//	这些路径打进访问日志就是把业务请求淹掉。
//
// ⇒ 走**真实装配**:跳过集合跟着 RegisterDapr **实际注册**的路由走,
//
//	而不是一份硬编码的路径表(各服务注册的路径本来就不一样)。
func TestAccessLog_DaprPathsAreNotLogged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	buf := &bytes.Buffer{}
	r := NewRouter(Options{
		AppID:           "probe",
		Audience:        "probe",
		AllowUnverified: true,
		Logger:          slog.New(slog.NewJSONHandler(buf, nil)),
		RegisterDapr: func(r *gin.Engine) {
			r.GET("/dapr/subscribe", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"topics": []string{}})
			})
			r.POST("/events/:topic", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"ok": true})
			})
		},
	})

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/dapr/subscribe"},
		{http.MethodPost, "/events/sale.paid"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
	}

	if lines := accessLogLines(t, buf); len(lines) != 0 {
		t.Errorf("dapr 内部路由不该记访问日志,实际: %s", buf.String())
	}
}

// TestAccessLog_BusinessRouteIsLoggedThroughRealWiring 锁"真实装配下业务路由仍然留痕"。
//
// ⚠️ 与上面两条配对:跳过逻辑不能把**业务路由**一起跳掉
// (那会让访问日志变成"只有探活没有业务"的空壳)。
//
// ⚠️ 这里不校验状态码:请求没带 token ⇒ 被鉴权挡成 401。
// 但**正是这些 401/403 也必须有访问日志** ——
// "客户端说没收到数据、后端说没收到请求"这类问题只能靠这些行定位。
func TestAccessLog_BusinessRouteIsLoggedThroughRealWiring(t *testing.T) {
	gin.SetMode(gin.TestMode)
	buf := &bytes.Buffer{}
	opts := Options{
		AppID:           "probe",
		Audience:        "probe",
		AllowUnverified: true,
		Logger:          slog.New(slog.NewJSONHandler(buf, nil)),
		RegisterDapr: func(r *gin.Engine) {
			r.GET("/dapr/subscribe", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"topics": []string{}})
			})
		},
		Register: func(r *gin.Engine) {
			r.GET("/biz/ping", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"ok": true})
			})
		},
	}
	r := NewRouter(opts)
	MountBusiness(r, opts)

	req := httptest.NewRequest(http.MethodGet, "/biz/ping", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	lines := accessLogLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("业务请求应留 1 行访问日志,实际 %d 行: %s", len(lines), buf.String())
	}
	if lines[0]["path"] != "/biz/ping" {
		t.Errorf("path = %v, want /biz/ping", lines[0]["path"])
	}
}

// TestAccessLog_QueryStringIsNotLogged 锁"路径里不含 query"。
//
// ⚠️ query 里常有 token / 签名 / 手机号,而访问日志经常被**整份**贴进工单或群聊。
//
// ⇒ 路径取路由模板(c.FullPath()),query 一个字都不打。
func TestAccessLog_QueryStringIsNotLogged(t *testing.T) {
	r, buf := accessLogTestEngine(t)
	r.GET("/biz/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	const secret = "super-secret-token"
	req := httptest.NewRequest(http.MethodGet, "/biz/ping?token="+secret+"&page=2", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	lines := accessLogLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("应留 1 行访问日志,实际 %d 行: %s", len(lines), buf.String())
	}
	if lines[0]["path"] != "/biz/ping" {
		t.Errorf("path = %v, want /biz/ping(路由模板,不含 query)", lines[0]["path"])
	}
	if strings.Contains(buf.String(), secret) {
		t.Errorf("访问日志里不该出现 query 里的 token: %s", buf.String())
	}
}

// TestAccessLog_ResponseHasNoToken 锁"响应里不含 token"。
//
// ⚠️ 中间件只读不写响应体 —— 但它拿到了整条请求(含 query 与 header),
// 一旦有人在这里"顺手回显一下请求内容",token 就会进响应体
// ⇒ 进浏览器历史 / 代理缓存 / 前端截图。
func TestAccessLog_ResponseHasNoToken(t *testing.T) {
	r, buf := accessLogTestEngine(t)
	r.GET("/biz/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	const secret = "super-secret-token"
	req := httptest.NewRequest(http.MethodGet, "/biz/ping?token="+secret, nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Cookie", "session="+secret)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if strings.Contains(w.Body.String(), secret) {
		t.Errorf("响应体里不该出现 token: %s", w.Body.String())
	}
	if strings.Contains(buf.String(), secret) {
		t.Errorf("访问日志里不该出现 token: %s", buf.String())
	}
}

// TestAccessLog_UnmatchedRouteFallsBackToURLPath 锁"404 也要留痕"。
//
// ⚠️ 路由没匹配上时 c.FullPath() 为空 ⇒ 退回 URL.Path(仍不含 query)。
// 不退回就只剩空串,现场根本看不出"客户端调了哪个地址"。
func TestAccessLog_UnmatchedRouteFallsBackToURLPath(t *testing.T) {
	r, buf := accessLogTestEngine(t)

	req := httptest.NewRequest(http.MethodGet, "/not/exists?token=abc", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	lines := accessLogLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("404 也该留 1 行访问日志,实际 %d 行: %s", len(lines), buf.String())
	}
	if lines[0]["path"] != "/not/exists" {
		t.Errorf("path = %v, want /not/exists", lines[0]["path"])
	}
	if lines[0]["status"] != float64(http.StatusNotFound) {
		t.Errorf("status = %v, want 404", lines[0]["status"])
	}
	if strings.Contains(buf.String(), "token") {
		t.Errorf("兜底路径也不该含 query: %s", buf.String())
	}
}

// TestAccessLog_SkipRegistryFollowsRegisteredRoutes 锁"跳过集合跟着实际注册走"。
//
// ⚠️ 硬编码一张路径表("/dapr/*"、"/events/*"……)迟早和实际注册的对不上 ——
//
//	各服务注册的 dapr 内部端点本来就不一样(有的还有 /job/*),
//	而"日志里少了一类请求"这种故障**没人会发现**。
func TestAccessLog_SkipRegistryFollowsRegisteredRoutes(t *testing.T) {
	RegisterAccessLogSkipPaths("/events/:topic")

	r, buf := accessLogTestEngine(t)
	r.POST("/events/:topic", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest(http.MethodPost, "/events/sale.paid", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if lines := accessLogLines(t, buf); len(lines) != 0 {
		t.Errorf("登记进跳过集合的路径不该记访问日志,实际: %s", buf.String())
	}
}
