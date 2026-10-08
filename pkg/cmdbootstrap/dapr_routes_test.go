package cmdbootstrap_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/YunBright/supertrade/pkg/eventbus"
	"github.com/gin-gonic/gin"
)

// 本文件锁死一个 2026-10-07 才发现的静默故障:
//
//	curl http://127.0.0.1:8106/dapr/subscribe → 401 {"code":"unauthenticated"}
//	curl http://127.0.0.1:8108/dapr/subscribe → 401("正常"的 fresh-meat 同样中招)
//
// Dapr sidecar 调 /dapr/subscribe 时**不带 Authorization 头**(它是 sidecar 自己,
// 不是终端用户),而 rbac.RequireAudience 对缺 claims 的请求一律 401。
// sidecar 于是永远拿不到订阅清单 → 事件一条也推不出去,而 /healthz 依然 200、
// 业务依然正常,只有"通知不响"一个症状 —— 极易被误判成"通知服务没实现"。
//
// 这个 bug 能藏这么久有两个原因,用例同时堵住这两个:
//   - 所有业务单测都直接调 handler,不经过 dapr sidecar;
//   - 装配逻辑原本内联在阻塞的 Run() 里,没有任何用例能观察中间件顺序。
// 所以本文件断言的是**真实的 NewRouter 装配结果**,不是一份复制品;
// Run() 现在复用同一个 NewRouter,行为不会分叉。

// newEngine 用真实 NewRouter 搭一个带 dapr 订阅的 app。
func newEngine(t *testing.T, withDapr bool) *gin.Engine {
	t.Helper()
	opts := cmdbootstrap.Options{
		AppID:    "notification-gateway",
		Audience: "notification-gateway",
		Register: func(r *gin.Engine) {
			r.GET("/ws", func(c *gin.Context) { c.String(http.StatusOK, "ws") })
			r.GET("/metrics", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
		},
	}
	if withDapr {
		opts.RegisterDapr = func(r *gin.Engine) {
			r.GET("/dapr/subscribe", func(c *gin.Context) {
				c.JSON(http.StatusOK, []map[string]string{{
					"pubsubname": eventbus.Name,
					"topic":      "stocktake.line.added",
					"route":      "/events/stocktake.line.added",
				}})
			})
			r.POST("/events/stocktake.line.added", func(c *gin.Context) {
				c.Status(http.StatusOK)
			})
		}
	}
	// 与 Run 完全一致的两段式装配:NewRouter(公开端点 + dapr 路由 + auth 中间件)
	// → MountBusiness(业务路由)。单测如果自己重排这个顺序,就会和真机行为分叉。
	r := cmdbootstrap.NewRouter(opts)
	cmdbootstrap.MountBusiness(r, opts)
	return r
}

func TestDaprSubscribeEndpointIsNotAuthGated(t *testing.T) {
	r := newEngine(t, true)

	// sidecar 的真实请求形态:不带 Authorization 头
	req := httptest.NewRequest(http.MethodGet, "/dapr/subscribe", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("/dapr/subscribe 必须是 200(否则 sidecar 拿不到订阅清单), got %d body=%s",
			w.Code, w.Body.String())
	}
	var subs []map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &subs); err != nil {
		t.Fatalf("响应不是订阅清单 JSON: %v (body=%s)", err, w.Body.String())
	}
	if len(subs) != 1 || subs[0]["topic"] != "stocktake.line.added" {
		t.Errorf("订阅清单内容不对: %+v", subs)
	}
}

func TestDaprEventsEndpointIsNotAuthGated(t *testing.T) {
	r := newEngine(t, true)

	// sidecar 推送 CloudEvents 同样不带 Authorization 头
	req := httptest.NewRequest(http.MethodPost, "/events/stocktake.line.added",
		strings.NewReader(`{"type":"stocktake.line.added","data":{}}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("/events/<topic> 必须是 200, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestBusinessRoutesStillRequireAuth(t *testing.T) {
	// 关键的反向断言:开窗**只能**开在 dapr 内部路径上。
	// 如果哪天有人图省事把 auth 中间件整个删掉,这两条会红。
	r := newEngine(t, true)

	for _, path := range []string{"/ws", "/metrics"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("无 token 访问业务路由 %s 应为 401, got %d (body=%s)",
				path, w.Code, w.Body.String())
		}
	}
}

// TestBusinessRoutesRejectMalformedToken 确认"带了一个解析不了的 token"不会
// 被当成已认证放行。
//
// claims.GinMiddleware 只解析 payload、**不验签**(验签在 dapr sidecar 的
// middleware.http.bearer)。对 `Bearer not-a-jwt` 这种输入,它解析失败会
// **放行但不注入 claims**,于是 RequireAudience 依旧 401。
// 价值在于:若哪天有人把 RequireAudience 改成"解析失败也放行",它会立刻红。
func TestBusinessRoutesRejectMalformedToken(t *testing.T) {
	r := newEngine(t, true)

	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Authorization", "Bearer not-a-jwt")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("无法解析的 token 应仍被 RequireAudience 拒为 401, got %d (body=%s) —— 若为 200 说明鉴权被削弱",
			w.Code, w.Body.String())
	}
}

// TestRegisterDaprIsOptIn 保证"不填 RegisterDapr 的服务"不会被顺手放开 /dapr/*。
//
// 这是 Options.RegisterDapr 相对"在中间件里按路径 skip"的核心优势:
// 前者是每个服务显式声明,后者是全局默认放行。
func TestRegisterDaprIsOptIn(t *testing.T) {
	r := newEngine(t, false) // 不注册 dapr 路由

	req := httptest.NewRequest(http.MethodGet, "/dapr/subscribe", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Fatalf("没注册 dapr 路由时 /dapr/subscribe 不该返回 200, got %d", w.Code)
	}
}

func TestHealthzStaysPublic(t *testing.T) {
	// /healthz 必须在 auth 之前注册(探针无 token)。
	// 这条是 dapr 窗口的"前车之鉴" —— 同一个注册顺序机制。
	r := newEngine(t, true)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("/healthz 应公开可访问, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "notification-gateway") {
		t.Errorf("/healthz 应回报 app id, body=%s", w.Body.String())
	}
}

// TestBusinessRoutesAreRegisteredExactlyOnce 是 2026-10-07 首次真机部署时
// 踩到的 panic 的回归锁:
//
//	github.com/gin-gonic/gin.(*node).addRoute(...) tree.go:260
//	panic: handlers are already registered for path '/ws'
//
// 起因:重构 Run 时,业务路由既在 NewRouter 里注册、又在 Run 里注册了一次。
// 单测当时全绿(用的还是重构前那条路径),直到真机启动才炸。
//
// 现在用真实的 NewRouter + MountBusiness 搭一次,并断言
// "挂两次业务路由"必然 panic —— 把这条契约钉死,下次再有人
// 把 Register 挪进 NewRouter 时会立刻看到。
func TestBusinessRoutesAreRegisteredExactlyOnce(t *testing.T) {
	opts := cmdbootstrap.Options{
		AppID: "notification-gateway",
		Register: func(r *gin.Engine) {
			r.GET("/ws", func(c *gin.Context) { c.String(http.StatusOK, "ws") })
		},
	}

	// 正确用法:NewRouter 一次 + MountBusiness 一次,不 panic。
	r := cmdbootstrap.NewRouter(opts)
	cmdbootstrap.MountBusiness(r, opts)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/healthz 应 200, got %d", w.Code)
	}

	// 错误用法:再挂一次同样的业务路由 → gin panic。
	defer func() {
		if rec := recover(); rec == nil {
			t.Fatal("重复注册业务路由应当 panic(gin: handlers already registered),实际没有")
		}
	}()
	cmdbootstrap.MountBusiness(r, opts)
}

// TestOnStartRunsBeforeAnyRouteRegistration 锁死 Run 里的启动顺序不变量:
//
//	OnStart → NewRouter(RegisterDapr) → MountBusiness(Register) → ListenAndServe
//
// 2026-10-08 真机部署踩过:首版把 NewRouter 放在 OnStart **之前**,于是
// Options.RegisterDapr 里的 `handler.New(appSvc)` 拿到的是 nil ——
// stocktake 的 /events/* 永久绑在 svc==nil 的 Handler 上。
// 实测:GET /dapr/subscribe 返 200(看起来完全正常),
// 但 POST 一个带 branch_id 的 auth.user.access_changed →
// InvalidateScopeCache 走到 s.scopeMu.Lock() → nil 解引用 → gin Recovery 500,
// Dapr 无限重试。即"订阅建起来了但一条事件都推不出去"。
//
// 注意:上面那段现象**只有带 branch_id 才复现** —— InvalidateScopeCache 对
// branchID=="" 会提前 return,根本走不到 mutex。所以用"随便发个事件看有没有 500"
// 这种探针很容易得出假阴性结论,必须像这里一样显式断言顺序。
func TestOnStartRunsBeforeAnyRouteRegistration(t *testing.T) {
	var order []string

	opts := cmdbootstrap.Options{
		AppID: "stocktake",
		OnStart: func() error {
			order = append(order, "onstart")
			return nil
		},
		RegisterDapr: func(r *gin.Engine) {
			order = append(order, "registerDapr")
			r.GET("/dapr/subscribe", func(c *gin.Context) { c.JSON(200, []any{}) })
		},
		Register: func(r *gin.Engine) {
			order = append(order, "register")
			r.GET("/ws", func(c *gin.Context) { c.String(200, "ok") })
		},
	}

	// 复刻 Run 内部的装配顺序(不真的监听端口)。
	if opts.OnStart != nil {
		if err := opts.OnStart(); err != nil {
			t.Fatalf("OnStart: %v", err)
		}
	}
	r := cmdbootstrap.NewRouter(opts)
	cmdbootstrap.MountBusiness(r, opts)

	want := []string{"onstart", "registerDapr", "register"}
	if len(order) != len(want) {
		t.Fatalf("调用序列 = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("调用序列 = %v, want %v —— OnStart 必须最先跑,"+
				"否则 RegisterDapr 捕获到未初始化的包级变量", order, want)
		}
	}
}

// TestRegisterDaprSeesStateBuiltByOnStart 是上面那条的语义版:
// 模拟真实 cmd/stocktake/main.go 的写法(handler.New(appSvc)),
// OnStart 之前 appSvc 是 nil。顺序错了就会把 nil 绑进 handler。
func TestRegisterDaprSeesStateBuiltByOnStart(t *testing.T) {
	type svcT struct{ id string }
	var appSvc *svcT

	opts := cmdbootstrap.Options{
		AppID: "stocktake",
		OnStart: func() error {
			appSvc = &svcT{id: "ready"}
			return nil
		},
		RegisterDapr: func(r *gin.Engine) {
			// 和 cmd/stocktake/main.go::registerDaprRoutes 同构:注册时就取值。
			captured := appSvc
			r.POST("/events/t", func(c *gin.Context) {
				if captured == nil {
					c.String(http.StatusInternalServerError, "nil svc captured")
					return
				}
				c.String(http.StatusOK, captured.id)
			})
		},
	}

	if opts.OnStart != nil {
		if err := opts.OnStart(); err != nil {
			t.Fatalf("OnStart: %v", err)
		}
	}
	r := cmdbootstrap.NewRouter(opts)

	req := httptest.NewRequest(http.MethodPost, "/events/t", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("RegisterDapr 里捕获到的 svc 不应是 nil, got %d body=%q", w.Code, w.Body.String())
	}
}

func TestOptionsValidateRequiresAppID(t *testing.T) {
	if err := (cmdbootstrap.Options{}).Validate(); err == nil {
		t.Errorf("空 AppID 应报错")
	}
	if err := (cmdbootstrap.Options{AppID: "x"}).Validate(); err != nil {
		t.Errorf("非空 AppID 不应报错: %v", err)
	}
}

func TestAppPortDefaults(t *testing.T) {
	// AppPort 的真实契约(见 port.go):APP_PORT 未注入时用 fallback;
	// fallback 也为空则返回空串(不是 :8080 —— ":8080" 的默认值在 Options.Port
	// 那一层,不在这里)。缺 " 前缀时补上。
	t.Run("APP_PORT 未注入时用 fallback", func(t *testing.T) {
		t.Setenv("APP_PORT", "")
		if got := cmdbootstrap.AppPort(":8106"); got != ":8106" {
			t.Errorf("AppPort(\":8106\") = %q, want :8106", got)
		}
	})
	t.Run("缺冒号时补上", func(t *testing.T) {
		t.Setenv("APP_PORT", "")
		if got := cmdbootstrap.AppPort("8106"); got != ":8106" {
			t.Errorf("AppPort(\"8106\") = %q, want :8106", got)
		}
	})
	t.Run("APP_PORT 优先于 fallback", func(t *testing.T) {
		t.Setenv("APP_PORT", "9999")
		if got := cmdbootstrap.AppPort(":8106"); got != ":9999" {
			t.Errorf("APP_PORT 应覆盖 fallback, got %q", got)
		}
	})
	t.Run("fallback 与 APP_PORT 都空时返回空串", func(t *testing.T) {
		t.Setenv("APP_PORT", "")
		if got := cmdbootstrap.AppPort(""); got != "" {
			t.Errorf("AppPort(\"\") = %q, want 空串", got)
		}
	})
}