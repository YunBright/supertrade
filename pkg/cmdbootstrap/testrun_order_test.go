package cmdbootstrap_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YunBright/core/testrun"
	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/gin-gonic/gin"
)

// 本文件钉住 testrun 中间件在 NewRouter 里的**装配位置**。
//
// 为什么位置本身需要测试：
//
// gin 的 `r.Use()` 只对**之后**注册的路由生效。NewRouter 的注册顺序是
//
//	Use(Recovery) → Use(testrun) → /healthz → RegisterDapr → Use(auth) → Register(业务)
//
// 如果有人把 `r.Use(testrun.Middleware())` 挪到 RegisterDapr 或业务路由之后，
// 编译照过、业务照跑、测试照绿 —— 只是**免鉴权窗口造出来的数据不再有溯源标记**，
// 而清理恰恰最需要清理那部分（它是免鉴权的，正常路径管不着）。
// 这是典型的"不变量被静默改掉"，所以必须有断言盯着。

// newProbeEngine 搭一个在 dapr 免鉴权窗口与业务路由里都回显 RunID 的 engine。
func newProbeEngine(t *testing.T) *gin.Engine {
	t.Helper()
	opts := cmdbootstrap.Options{
		AppID:           "probe",
		Audience:        "probe",
		AllowUnverified: true,
		RegisterDapr: func(r *gin.Engine) {
			r.GET("/dapr/subscribe", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"run_id": testrun.Current(c)})
			})
		},
		Register: func(r *gin.Engine) {
			r.GET("/biz", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"run_id": testrun.Current(c)})
			})
		},
	}
	return cmdbootstrap.NewRouter(opts)
}

func probeRunID(t *testing.T, path string) string {
	t.Helper()
	r := newProbeEngine(t)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set(testrun.HeaderName, "ctr-20261013-001122-a1b2c3")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require2xx(t, w)
	var body struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v (body=%s)", path, err, w.Body.String())
	}
	return body.RunID
}

func TestTestrunMiddlewareCoversDaprWindow(t *testing.T) {
	const want = "ctr-20261013-001122-a1b2c3"

	// 免鉴权窗口 —— 最容易被挪位置漏掉的那一段。
	// 它注册在 /healthz 之后，所以这一条同时证明了中间件装在引擎最顶上。
	if got := probeRunID(t, "/dapr/subscribe"); got != want {
		t.Errorf("dapr 免鉴权窗口没拿到 run_id: got %q want %q —— "+
			"testrun.Middleware() 必须注册在 RegisterDapr 之前", got, want)
	}
}

// 为什么不探 /biz：业务路由注册在鉴权中间件之后，探它需要一个真 token，
// 而"装在鉴权之后"并不是我们要防的失败模式。真正会丢的是免鉴权窗口 ——
// 它注册在 auth 之前，一旦 testrun 被挪到它后面，那部分数据就没有溯源标记，
// 而清理恰恰最需要清理免鉴权端点造出来的东西。

func TestTestrunMiddlewareAbsentHeaderStaysEmpty(t *testing.T) {
	r := newProbeEngine(t)
	req := httptest.NewRequest(http.MethodGet, "/dapr/subscribe", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require2xx(t, w)
	// 生产流量不带该头 —— 必须安静放过，不能 400、也不能凭空造一个 id。
	if strings.Contains(w.Body.String(), `"run_id":"c`) {
		t.Errorf("没有该头时不应凭空生成 run_id，body=%s", w.Body.String())
	}
}

func require2xx(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code < 200 || w.Code >= 300 {
		t.Fatalf("期望 2xx，实际 %d，body=%s", w.Code, w.Body.String())
	}
}

func TestTestrunMiddlewareRejectsMalformedHeader(t *testing.T) {
	r := newProbeEngine(t)
	req := httptest.NewRequest(http.MethodGet, "/dapr/subscribe", nil)
	req.Header.Set(testrun.HeaderName, "BAD-RUN-ID")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法 run_id 必须 400，实际 %d —— 静默忽略会让测试配置写错时"+
			"表现为'数据莫名清不掉'，那是最难查的失败形态", w.Code)
	}
}
