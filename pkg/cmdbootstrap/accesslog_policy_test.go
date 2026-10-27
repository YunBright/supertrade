// 访问日志 / 业务错误日志 / panic 兜底的**行为守卫**。
//
// 与 accesslog_test.go(内部包)配对:那边锁字段形状与跳过集合,这边锁**在真实装配下**
// 的策略——状态码到日志级别的映射、c.Error 的业务错误行、panic 兜底的敏感信息与状态码。
//
// ⚠️ 本仓不写契约 / e2e 测试:走公网 nginx 入口的 contract case 在 ../deployer 仓
// (tests/contract/)。本文件是**进程内单元测试**,不启 sidecar、不连外部依赖。
//
// ⚠️ 用外部测试包(package cmdbootstrap_test)而不是内部包:这里要按**调用方视角**
// 驱动 NewRouter + MountBusiness 的真实装配,而不是直接挂未导出的中间件。
package cmdbootstrap_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/gin-gonic/gin"
)

const accessLogMsg = "http access"

// logBuffer 是并发安全的日志缓冲。
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// logRecord 是日志行(JSON)的解析结果。
type logRecord struct {
	Level      string `json:"level"`
	Msg        string `json:"msg"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Status     int    `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	IP         string `json:"ip"`
	Bytes      int64  `json:"bytes"`
	Sub        string `json:"sub"`
	Branch     string `json:"branch"`
	Code       string `json:"code"`
	Message    string `json:"message"`
}

// logRecords 把日志缓冲解析成记录行。
func logRecords(t *testing.T, buf *logBuffer) []logRecord {
	t.Helper()
	var out []logRecord
	for _, line := range strings.Split(buf.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec logRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("日志行不是 JSON: %v (line=%s)", err, line)
		}
		out = append(out, rec)
	}
	return out
}

// accessRecords 只挑访问日志行。
func accessRecords(t *testing.T, buf *logBuffer) []logRecord {
	t.Helper()
	var out []logRecord
	for _, rec := range logRecords(t, buf) {
		if rec.Msg == accessLogMsg {
			out = append(out, rec)
		}
	}
	return out
}

// newPolicyEngine 搭一个带日志注入的 engine。
//
// ⚠️ 走**真实的 NewRouter + MountBusiness**:自己重排中间件顺序的话,
// 断言的就不是真机的装配结果了。
//
// ⚠️ AllowUnverified:本组用例断言的是**日志形状**,不需要真实验签器;
// 显式声明跳过,而不是靠"没装配就 fallback"蒙混过去。
func newPolicyEngine(t *testing.T, buf *logBuffer,
	register func(r *gin.Engine), registerDapr func(r *gin.Engine)) *gin.Engine {
	t.Helper()
	opts := cmdbootstrap.Options{
		AppID:    "pos",
		Audience: "pos",
		// 注入 logger(而不是 slog.SetDefault)—— 全局污染会影响别的测试。
		Logger:          slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		AllowUnverified: true,
		Register:        register,
		RegisterDapr:    registerDapr,
	}
	r := cmdbootstrap.NewRouter(opts)
	cmdbootstrap.MountBusiness(r, opts)
	return r
}

// fakeJWT 造一个"只解 payload 不验签"的 token(claims.GinMiddleware 能解析)。
//
// payload 形如 {"sub":"user-1","aud":["pos"]}。
func fakeJWT(sub string, aud []string) string {
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, err := json.Marshal(map[string]any{"sub": sub, "aud": aud})
	if err != nil {
		panic(err)
	}
	return header + "." + enc.EncodeToString(payload) + ".sig"
}

// TestPolicyAccessLog_FieldsComeFromClaims 锁"业务请求 ⇒ 恰好一条访问日志,字段齐全"。
//
// ⚠️ sub 必须取自 claims.sub(不是自己解析 token):解析 token 意味着日志路径上
// 多一份"读 secret"的逻辑。branch 取 header 原值。
func TestPolicyAccessLog_FieldsComeFromClaims(t *testing.T) {
	buf := &logBuffer{}
	r := newPolicyEngine(t, buf, func(r *gin.Engine) {
		r.GET("/sales", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"ok": true})
		})
	}, nil)

	req := httptest.NewRequest(http.MethodGet, "/sales", nil)
	req.Header.Set("Authorization", "Bearer "+fakeJWT("user-1", []string{"pos"}))
	req.Header.Set("X-Branch-ID", "branch-9")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("/sales 应 200, got %d body=%s", w.Code, w.Body.String())
	}

	recs := accessRecords(t, buf)
	if len(recs) != 1 {
		t.Fatalf("应有且仅有 1 条访问日志,实际 %d 条:\n%s", len(recs), buf.String())
	}
	rec := recs[0]

	if rec.Level != "INFO" {
		t.Errorf("正常请求级别应为 INFO,实际 %q", rec.Level)
	}
	if rec.Method != http.MethodGet {
		t.Errorf("method 应为 GET,实际 %q", rec.Method)
	}
	if rec.Path != "/sales" {
		t.Errorf("path 应为 /sales,实际 %q", rec.Path)
	}
	if rec.Status != http.StatusOK {
		t.Errorf("status 应为 200,实际 %d", rec.Status)
	}
	if rec.DurationMS < 0 {
		t.Errorf("duration_ms 不该为负,实际 %d", rec.DurationMS)
	}
	if rec.IP == "" {
		t.Error("ip 不该为空")
	}
	if rec.Bytes <= 0 {
		t.Errorf("bytes 应大于 0,实际 %d", rec.Bytes)
	}
	if rec.Sub != "user-1" {
		t.Errorf("sub 应取 claims.sub=user-1,实际 %q", rec.Sub)
	}
	if rec.Branch != "branch-9" {
		t.Errorf("branch 应取 X-Branch-ID=branch-9,实际 %q", rec.Branch)
	}
}

// TestPolicyAccessLog_SecretsNeverLogged 锁"query string 与令牌都不进日志"。
//
// ⚠️ query 里可能有 token / 口令;Authorization / Cookie 更不能打 ——
// 访问日志经常被整份贴进工单或群聊。
func TestPolicyAccessLog_SecretsNeverLogged(t *testing.T) {
	buf := &logBuffer{}
	r := newPolicyEngine(t, buf, func(r *gin.Engine) {
		r.GET("/sales", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"ok": true})
		})
	}, nil)

	const queryToken = "query-secret-token"
	jwt := fakeJWT("user-1", []string{"pos"})
	req := httptest.NewRequest(http.MethodGet, "/sales?token="+queryToken, nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Cookie", "session=cookie-secret")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	recs := accessRecords(t, buf)
	if len(recs) != 1 {
		t.Fatalf("应有且仅有 1 条访问日志,实际 %d 条:\n%s", len(recs), buf.String())
	}
	if recs[0].Path != "/sales" {
		t.Errorf("path 应去掉 query string,实际 %q", recs[0].Path)
	}
	if strings.Contains(recs[0].Path, "?") {
		t.Errorf("path 不该带 query string: %q", recs[0].Path)
	}

	out := buf.String()
	for _, secret := range []string{queryToken, jwt, "cookie-secret"} {
		if strings.Contains(out, secret) {
			t.Errorf("日志里不该出现敏感值 %q:\n%s", secret, out)
		}
	}
}

// TestPolicyAccessLog_UnauthorizedLoggedAtInfo 锁"401 也有日志,且级别是 Info"。
//
// ⚠️ 401 必须在日志里:被鉴权挡掉的请求恰恰是排障时最想看的那批
// ("客户端说没收到数据、后端说没收到请求")。
// ⚠️ 但级别是 Info:越权探测会刷屏,升 Warn/Error 就是噪音。
func TestPolicyAccessLog_UnauthorizedLoggedAtInfo(t *testing.T) {
	buf := &logBuffer{}
	r := newPolicyEngine(t, buf, func(r *gin.Engine) {
		r.GET("/sales", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"ok": true})
		})
	}, nil)

	// 无 token ⇒ RequireAudience 挡下 ⇒ 401。
	req := httptest.NewRequest(http.MethodGet, "/sales", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("无 token 应 401, got %d body=%s", w.Code, w.Body.String())
	}
	recs := accessRecords(t, buf)
	if len(recs) != 1 {
		t.Fatalf("应有且仅有 1 条访问日志,实际 %d 条:\n%s", len(recs), buf.String())
	}
	if recs[0].Status != http.StatusUnauthorized {
		t.Errorf("status 应为 401,实际 %d", recs[0].Status)
	}
	if recs[0].Level != "INFO" {
		t.Errorf("4xx 级别应为 INFO,实际 %q", recs[0].Level)
	}
}

// TestPolicyAccessLog_ServerErrorLoggedAtError 锁"5xx 记 Error"。
func TestPolicyAccessLog_ServerErrorLoggedAtError(t *testing.T) {
	buf := &logBuffer{}
	r := newPolicyEngine(t, buf, func(r *gin.Engine) {
		r.GET("/boom", func(c *gin.Context) {
			c.JSON(http.StatusInternalServerError, gin.H{"code": "internal_error"})
		})
	}, nil)

	// ⚠️ 必须带 token:鉴权中间件在业务路由**之前**,
	// 没 token 会被 RequireAudience 挡成 401,handler 根本不会被执行。
	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	req.Header.Set("Authorization", "Bearer "+fakeJWT("user-1", []string{"pos"}))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("/boom 应 500, got %d", w.Code)
	}
	recs := accessRecords(t, buf)
	if len(recs) != 1 {
		t.Fatalf("应有且仅有 1 条访问日志,实际 %d 条:\n%s", len(recs), buf.String())
	}
	if recs[0].Level != "ERROR" {
		t.Errorf("5xx 级别应为 ERROR,实际 %q", recs[0].Level)
	}
	if recs[0].Status != http.StatusInternalServerError {
		t.Errorf("status 应为 500,实际 %d", recs[0].Status)
	}
}

// TestPolicyErrorLog_BusinessErrorLogged 锁"业务错误 ⇒ 一条 ERROR,带 code / message / path / method"。
//
// 触发方式是 gin 的标准做法:handler 调 c.Error(err)。
func TestPolicyErrorLog_BusinessErrorLogged(t *testing.T) {
	buf := &logBuffer{}
	r := newPolicyEngine(t, buf, func(r *gin.Engine) {
		r.GET("/products/:id", func(c *gin.Context) {
			_ = c.Error(errors.New("商品不存在"))
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
				"code":    "product_not_found",
				"message": "商品不存在",
			})
		})
	}, nil)

	req := httptest.NewRequest(http.MethodGet, "/products/P1", nil)
	req.Header.Set("Authorization", "Bearer "+fakeJWT("user-1", []string{"pos"}))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("/products/P1 应 404, got %d", w.Code)
	}

	var biz *logRecord
	for _, rec := range logRecords(t, buf) {
		if rec.Msg == "business error" {
			biz = &rec
			break
		}
	}
	if biz == nil {
		t.Fatalf("应有一条 business error 日志,实际日志:\n%s", buf.String())
	}
	if biz.Level != "ERROR" {
		t.Errorf("业务错误级别应为 ERROR,实际 %q", biz.Level)
	}
	if biz.Message != "商品不存在" {
		t.Errorf("message 应为 %q,实际 %q", "商品不存在", biz.Message)
	}
	if biz.Code == "" {
		t.Error("code 不该为空")
	}
	if biz.Method != http.MethodGet {
		t.Errorf("method 应为 GET,实际 %q", biz.Method)
	}
	// ⚠️ path 是**路由模板**:路径参数(P1)是用户数据,可能混着 token。
	if biz.Path != "/products/:id" {
		t.Errorf("path 应为路由模板 /products/:id,实际 %q", biz.Path)
	}
}

// TestPolicyRecovery_PanicDoesNotLeakSecrets 锁"panic 兜底不打令牌 / Cookie,且状态码是 500"。
//
// ⚠️ gin 自带的 Recovery 会把请求头打到 stderr(DebugMode),
// Authorization 被它 mask 掉,但 **Cookie 没有** —— 那是真实的会话凭据。
// ⇒ 这里断言日志里既没有 Authorization 的值,也没有 Cookie 的值。
func TestPolicyRecovery_PanicDoesNotLeakSecrets(t *testing.T) {
	buf := &logBuffer{}
	r := newPolicyEngine(t, buf, func(r *gin.Engine) {
		r.GET("/panic", func(c *gin.Context) {
			panic("boom")
		})
	}, nil)

	// ⚠️ 必须带 token:鉴权中间件在业务路由**之前**,
	// 没 token 会被 RequireAudience 挡成 401,panic 压根不会发生。
	jwt := fakeJWT("user-1", []string{"pos"})
	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Cookie", "session=panic-cookie-secret")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("/panic 应 500, got %d body=%s", w.Code, w.Body.String())
	}

	out := buf.String()
	for _, secret := range []string{jwt, "panic-cookie-secret"} {
		if strings.Contains(out, secret) {
			t.Errorf("日志里不该出现敏感值 %q:\n%s", secret, out)
		}
	}

	// panic 必须被记录(否则又是一条"后端说没收到请求")。
	found := false
	for _, rec := range logRecords(t, buf) {
		if rec.Msg == "panic recovered" {
			found = true
			if rec.Level != "ERROR" {
				t.Errorf("panic 级别应为 ERROR,实际 %q", rec.Level)
			}
			if rec.Path != "/panic" {
				t.Errorf("panic 日志的 path 应为 /panic,实际 %q", rec.Path)
			}
		}
	}
	if !found {
		t.Fatalf("应有一条 panic recovered 日志,实际:\n%s", out)
	}

	// 访问日志要记到 **500**(不是 panic 之前的 200)——
	// 否则"状态码"这一栏在最需要的时候是错的。
	var access *logRecord
	for _, rec := range accessRecords(t, buf) {
		if rec.Path == "/panic" {
			access = &rec
			break
		}
	}
	if access == nil {
		t.Fatalf("panic 请求也该有访问日志,实际:\n%s", out)
	}
	if access.Status != http.StatusInternalServerError {
		t.Errorf("panic 的访问日志 status 应为 500,实际 %d", access.Status)
	}
}
