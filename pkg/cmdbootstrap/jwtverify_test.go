package cmdbootstrap_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/YunBright/authkit/authn"
	"github.com/YunBright/authkit/claims"
	"github.com/YunBright/authkit/jwks"
	"github.com/YunBright/authkit/jwt"
	"github.com/gin-gonic/gin"
	"github.com/go-jose/go-jose/v4"

	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
)

// 这些用例锁的是**装配不变量**,不是验签算法本身(那部分在 authn 包里)。
//
// 背景:2026-10-12 生产实证,伪造签名的 JWT 打 /api/v1/settlement-auditor/…
// 返 200 + 真实结算数据。修法是应用层验签(authkit/authn)。
// 于是"engine 到底挂了哪个中间件"成了新的、且极易在重构中被静默改掉的不变量 ——
// 它不会让任何单测变红,只会让漏洞复活。

const testIssuer = "https://yb.test.local"

// mintForgedJWT 造一个 **payload 合法但签名是垃圾** 的 token。
//
// 这正是攻击者能构造的最强形态:claims / aud / exp 全部合法,
// 只有 signature 段是编的。解 payload 的中间件(claims.GinMiddleware)
// 会放它过关,验签的中间件必须拒。
func mintForgedJWT(t *testing.T, aud []string) string {
	t.Helper()
	hdr := map[string]any{"alg": "RS256", "typ": "JWT", "kid": "k1"}
	pl := map[string]any{
		"sub": "00000000-0000-0000-0000-000000000001",
		"iss": testIssuer,
		"aud": aud,
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
	b64 := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return b64(hdr) + "." + b64(pl) + "." + base64.RawURLEncoding.EncodeToString([]byte("not-a-signature"))
}

// newTestIssuer 走**生产同一条**签发路径(authkit/jwt.Issuer + jwks.Set),
// 返回签发器和对应的公钥集。
//
// 刻意不用 go-jose 手搓 token:那会与生产签发逻辑分叉,而分叉出来的
// "真实 token"可能因为签发细节不同而验不过,变成一条永远红的假阳性。
func newTestIssuer(t *testing.T) (*jwt.Issuer, *jwks.Set) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	set := jwks.NewSet()
	if err := set.Add(jwks.NewKeyFromRSA("k1", jose.RS256, priv, time.Time{}, time.Time{})); err != nil {
		t.Fatalf("set.Add: %v", err)
	}
	iss, err := jwt.NewIssuer(jwt.IssuerConfig{
		Keys:       set,
		Issuer:     testIssuer,
		DefaultTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	return iss, set
}

// newVerifier 用公钥集装配一个真验签器。
func newVerifier(t *testing.T, set *jwks.Set) *authn.Verifier {
	t.Helper()
	// 走一遍 JWKS 文档的序列化/反序列化,模拟生产"公钥经 dapr state 传递"
	// 的那一跳 —— StateSource 做的就是这件事。
	raw, err := set.MarshalBytes(time.Now())
	if err != nil {
		t.Fatalf("MarshalBytes: %v", err)
	}
	parsed, err := jwks.ParseDocument(raw)
	if err != nil {
		t.Fatalf("ParseDocument: %v", err)
	}
	v, err := authn.NewVerifier(authn.VerifierConfig{
		Source: authn.NewStaticSource(parsed),
		Issuer: testIssuer,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

// withVerifier 临时装配验签器并在用例结束后复原(全局单例,不能并发跑)。
func withVerifier(t *testing.T, set *jwks.Set) {
	t.Helper()
	cmdbootstrap.SetAuthVerifier(newVerifier(t, set))
	t.Cleanup(func() { cmdbootstrap.SetAuthVerifier(nil) })
}

// TestNewRouterRefusesToBuildWithoutVerifier 钉住"没有验签器就不许建 engine"。
//
// 反证:把 NewRouter 的 default 分支改回 r.Use(claims.GinMiddleware()),
// 这条会立刻红。任何一次"为了让测试好过"而加回 fallback 的改动都会被抓住。
func TestNewRouterRefusesToBuildWithoutVerifier(t *testing.T) {
	cmdbootstrap.SetAuthVerifier(nil)
	t.Cleanup(func() { cmdbootstrap.SetAuthVerifier(nil) })

	defer func() {
		if recover() == nil {
			t.Fatal("未装配验签器时 NewRouter 应当 panic —— 它意味着 engine 会在" +
				"解析即信任的状态下运行(本次要修的漏洞本身)")
		}
	}()
	_ = cmdbootstrap.NewRouter(cmdbootstrap.Options{AppID: "x", Audience: "x"})
}

// TestNewRouterRejectsForgedTokenOnceVerifierIsWired 是**反向**证明:
// 装配了真验签器之后,伪造签名不再能拿到 claims。
//
// 这条与上一条成对。只锁上一条的话,"装配了验签器但中间件挂错"仍然能过;
// 只锁这一条的话,"压根没装配"反而没人管。
func TestNewRouterRejectsForgedTokenOnceVerifierIsWired(t *testing.T) {
	iss, set := newTestIssuer(t)
	withVerifier(t, set)

	gin.SetMode(gin.TestMode)
	opts := cmdbootstrap.Options{
		AppID:    "settlement-auditor",
		Audience: "settlement-auditor",
		Register: func(r *gin.Engine) {
			r.GET("/settlement-approvals", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"ok": true})
			})
		},
	}
	r := cmdbootstrap.NewRouter(opts)
	cmdbootstrap.MountBusiness(r, opts)

	real, err := iss.Sign(claims.Claims{Sub: "00000000-0000-0000-0000-000000000001"},
		jwt.WithAudience("settlement-auditor"))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	otherAud, err := iss.Sign(claims.Claims{Sub: "00000000-0000-0000-0000-000000000001"},
		jwt.WithAudience("userd"))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	tests := []struct {
		name       string
		token      string
		wantStatus int
		wantBody   string
	}{
		{
			name:       "伪造签名(payload 合法)",
			token:      mintForgedJWT(t, []string{"settlement-auditor"}),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "真实签名",
			token:      real,
			wantStatus: http.StatusOK,
		},
		{
			// aud 不含本服务返 401 invalid_audience(而不是 403):
			// 对调用方而言"这个 token 不该给我"与"这个 token 是假的"同样是
			// "别进来",status code 一致、错误信封里的 code 才区分两者。
			name:       "真实签名但 aud 不含本服务",
			token:      otherAud,
			wantStatus: http.StatusUnauthorized,
			wantBody:   "invalid_audience",
		},
		{
			name:       "无 Authorization 头",
			token:      "",
			wantStatus: http.StatusUnauthorized,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/settlement-approvals", nil)
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tt.wantStatus, w.Body.String())
			}
			if tt.wantStatus != http.StatusOK && strings.Contains(w.Body.String(), `"ok":true`) {
				t.Fatalf("被拒的请求却读到了业务数据: %s", w.Body.String())
			}
			if tt.wantBody != "" && !strings.Contains(w.Body.String(), tt.wantBody) {
				t.Fatalf("body 缺少 %q: %s", tt.wantBody, w.Body.String())
			}
		})
	}
}

// TestAllowUnverifiedOnlyAppliesToClaimsAndStillChecksAudience 钉住显式降级的边界:
// 声明 AllowUnverified 只影响**验签**,audience 校验照旧。
//
// 否则"测试用的降级开关"会顺带把 audience 也放掉,变成一个真的权限洞。
func TestAllowUnverifiedOnlyAppliesToClaimsAndStillChecksAudience(t *testing.T) {
	cmdbootstrap.SetAuthVerifier(nil)
	t.Cleanup(func() { cmdbootstrap.SetAuthVerifier(nil) })

	gin.SetMode(gin.TestMode)
	opts := cmdbootstrap.Options{
		AppID:           "x",
		Audience:        "x",
		AllowUnverified: true,
		Register: func(r *gin.Engine) {
			r.GET("/p", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
		},
	}
	r := cmdbootstrap.NewRouter(opts)
	cmdbootstrap.MountBusiness(r, opts)

	// 伪造签名 + 正确 aud → 放行(这正是 AllowUnverified 的含义)
	req := httptest.NewRequest(http.MethodGet, "/p", nil)
	req.Header.Set("Authorization", "Bearer "+mintForgedJWT(t, []string{"x"}))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("AllowUnverified 下应放行, got %d: %s", w.Code, w.Body.String())
	}

	// aud 不含本服务 → 仍被拒(AllowUnverified 不该顺带放掉 audience)
	req = httptest.NewRequest(http.MethodGet, "/p", nil)
	req.Header.Set("Authorization", "Bearer "+mintForgedJWT(t, []string{"other"}))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "invalid_audience") {
		t.Fatalf("aud 仍应被校验, got %d: %s", w.Code, w.Body.String())
	}
}

// TestHealthzStays200AndReportsAuthState 钉住 healthz 的契约。
//
// 关键点:公钥读不到时 auth_ok=false 但 **HTTP 仍 200**。
// 返非 200 会让 sidecar / systemd 反复重启,而重启对"state 暂时不可用"
// 毫无帮助,只会制造重启风暴并掩盖真正的故障面。
func TestHealthzStays200AndReportsAuthState(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 未装配验签器 → 没有 state source → 没有错误 → auth_ok:true
	cmdbootstrap.SetAuthVerifier(nil)
	t.Cleanup(func() { cmdbootstrap.SetAuthVerifier(nil) })
	opts := cmdbootstrap.Options{AppID: "x", AllowUnverified: true}
	r := cmdbootstrap.NewRouter(opts)
	cmdbootstrap.MountBusiness(r, opts)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"auth_ok":true`) {
		t.Fatalf("healthz body = %s, want auth_ok:true", w.Body.String())
	}
}

// TestDaprRoutesStayBeforeAuthMiddleware 复核:装配验签器之后,
// /dapr/subscribe 仍然拿不到 claims 保护(否则 sidecar 拉不到订阅)。
//
// 验签中间件是从"只解 payload"换成"真验签"的,换的过程中很容易顺手把
// 顺序也改了 —— 而那个顺序错了的症状是"事件静默不投递",healthz 照样 200。
func TestDaprRoutesStayBeforeAuthMiddleware(t *testing.T) {
	_, set := newTestIssuer(t)
	withVerifier(t, set)

	gin.SetMode(gin.TestMode)
	opts := cmdbootstrap.Options{
		AppID:    "x",
		Audience: "x",
		RegisterDapr: func(r *gin.Engine) {
			r.GET("/dapr/subscribe", func(c *gin.Context) {
				c.JSON(http.StatusOK, []string{"auth.user.access_changed"})
			})
		},
		Register: func(r *gin.Engine) {
			r.POST("/events/t", func(c *gin.Context) { c.Status(http.StatusOK) })
		},
	}
	r := cmdbootstrap.NewRouter(opts)
	cmdbootstrap.MountBusiness(r, opts)

	// sidecar 不带 Authorization 拉订阅清单,必须 200 而不是 401。
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/dapr/subscribe", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/dapr/subscribe = %d, want 200(sidecar 不带 token); body=%s",
			w.Code, w.Body.String())
	}
}

// TestClaimsMiddlewareStillParsesForgedToken 是对照组:直接用 claims.GinMiddleware 时,
// 伪造 token 的 claims 会被注入 ctx。
//
// 它把"为什么 NewRouter 拒绝在无验签器时 fallback 到 claims.GinMiddleware"
// 这条设计理由钉成可执行的事实,而不是一句注释里的断言。
// 一旦哪天 claims 包收紧了这条行为(开始验签),本用例会红,提醒复核 fallback 决策。
func TestClaimsMiddlewareStillParsesForgedToken(t *testing.T) {
	forged := mintForgedJWT(t, []string{"x"})
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.Use(claims.GinMiddleware())
	var injected bool
	r.GET("/p", func(c *gin.Context) {
		_, injected = claims.FromContext(c.Request.Context())
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/p", nil)
	req.Header.Set("Authorization", "Bearer "+forged)
	r.ServeHTTP(httptest.NewRecorder(), req)

	if !injected {
		t.Fatal("对照组不成立:claims.GinMiddleware 已不再为伪造签名注入 claims。" +
			"若这是有意收紧(claims 包开始验签),请复核 NewRouter 的 fallback 决策与 " +
			"本用例 —— 它守护的是 fallback 的必要性,不是期望行为本身")
	}
}
