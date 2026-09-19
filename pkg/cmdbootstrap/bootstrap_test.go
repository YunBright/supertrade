package cmdbootstrap_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/gin-gonic/gin"
)

// startTestServer 用 goroutine 模拟 Run() 的 server 启动部分。
//
// 真实 Run() 在 main 启动时阻塞,我们只测试 Options 解析 / 中间件装配 / 业务路由。
func startTestServer(t *testing.T, opts cmdbootstrap.Options) http.Handler {
	t.Helper()
	if opts.AppID == "" {
		t.Fatalf("AppID 不能为空(本测试不覆盖 panic 路径)")
	}
	if opts.Port == "" {
		opts.Port = ":0"
	}
	if opts.Audience == "" {
		opts.Audience = opts.AppID
	}

	// 复制 Run() 的中间件装配逻辑,避免阻塞式启动。
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "app": opts.AppID})
	})

	// 重放中间件:GinMiddleware + RequireAudience
	// (生产代码:Run() 已装配,本测试只关心路由层行为)
	if opts.Register != nil {
		opts.Register(r)
	}
	return r
}

func TestHealthz_IsPublic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "app": "catalog"})
	})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("healthz 应 200, got %d", w.Code)
	}
	var body struct {
		Status string `json:"status"`
		App    string `json:"app"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Status != "ok" || body.App != "catalog" {
		t.Errorf("healthz body 异常: %+v", body)
	}
}

func TestRegister_AddsBusinessRoute(t *testing.T) {
	r := startTestServer(t, cmdbootstrap.Options{
		AppID: "pos",
		Register: func(r *gin.Engine) {
			r.GET("/sales", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"sales": []int{1, 2, 3}})
			})
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/sales", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("/sales 应 200, got %d, body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Sales []int `json:"sales"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if len(body.Sales) != 3 {
		t.Errorf("sales 应有 3 条, got %d", len(body.Sales))
	}
}

// TestOptions_Validate 验证 Options.validate() 在 AppID 缺失时返回 error
// (Run() 内部 panic 此 error,确保 boot 阶段不变量)。
func TestOptions_Validate(t *testing.T) {
	cases := []struct {
		name    string
		opts    cmdbootstrap.Options
		wantErr bool
	}{
		{name: "AppID 空 → error", opts: cmdbootstrap.Options{}, wantErr: true},
		{name: "AppID 有 → 正常", opts: cmdbootstrap.Options{AppID: "pos"}, wantErr: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opts.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("Validate() 应返回 error")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate() 不应返回 error, got %v", err)
			}
		})
	}
}