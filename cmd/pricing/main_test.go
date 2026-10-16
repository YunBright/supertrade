package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// 2026-10-16 生产事故回归(与 cmd/catalog/main_test.go 同因同源)。
//
// 症状:GET /api/v1/pricing/quotes?barcode=X 一律返
//
//	{"code":"cube_unavailable","message":"cubeclient: cube 调用链路不可用: Internal: Bad Request"}
//
// 而 /meta(不查 cube)正常 200,curl 直连 cube-router 打完全相同的 query body 也 200。
//
// 根因:forwardBearerToOutgoing 只注入 Authorization,漏了 X-Branch-ID。
// middleware.XBranchID() 写的是 gin.Context,cubeclient.LoadCubeQuery 读的是
// request.Context —— 两者不是同一个 context。于是 outgoing gRPC metadata 里没有
// x-branch-id,cube-router 返 400 branch_required,sidecar 折叠成
// gRPC `Internal: Bad Request`,pricing 的错误映射翻成 503 cube_unavailable。
//
// "库存服务暂不可用"这个文案会把排查的人全部引到 cube 健不健康上,
// 而 cube 一直是好的 —— 它只是没收到门店头。
func runPricingMiddleware(t *testing.T, headers map[string]string) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	var got string
	r := gin.New()
	// 与 registerRoutes 完全一致的中间件顺序。
	r.Use(middleware.XBranchID())
	r.Use(forwardBearerToOutgoing())
	r.GET("/probe", func(c *gin.Context) {
		got = cubeclient.BranchFromContext(c.Request.Context())
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("probe 返回 %d,期望 200", w.Code)
	}
	return got
}

func TestPricingForwardBearer_PropagatesBranchID(t *testing.T) {
	got := runPricingMiddleware(t, map[string]string{
		"X-Branch-ID":   "01",
		"Authorization": "Bearer test-token",
	})
	if got != "01" {
		t.Fatalf("request.Context 必须带上门店,cubeclient 才会拼出 outgoing "+
			"metadata 的 x-branch-id;少了它 cube-router 返 400 branch_required,"+
			"对外显示成「库存服务暂不可用」。实际 got %q", got)
	}
}

// 多店写法原样透传:解析语义归 cube-router,这里改写等于凭空造一个不存在的门店。
func TestPricingForwardBearer_PropagatesMultiBranchVerbatim(t *testing.T) {
	for _, in := range []string{"*", "01,02"} {
		if got := runPricingMiddleware(t, map[string]string{"X-Branch-ID": in}); got != in {
			t.Fatalf("X-Branch-ID=%q 必须原样透传,实际 got %q", in, got)
		}
	}
}

func TestPricingForwardBearer_NoBranchHeader(t *testing.T) {
	if got := runPricingMiddleware(t, nil); got != "" {
		t.Fatalf("无 X-Branch-ID 时应为空,实际 got %q", got)
	}
}
