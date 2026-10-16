package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// 2026-10-16 生产事故回归。
//
// 症状:GET /catalog/products/search?barcode=<任意条码> 一律返回
//
//	{"code":"cube_error","message":"cubeclient: cube 调用链路不可用: Internal: Bad Request"}
//
// 而同时:curl 直连 cube-router 打**完全相同的 query body** 返回 200 + 真实商品;
// 走 stocktake 的 /stocktake/products/search(同一个 cubeclient、同一个 cube-router)
// 也返回 200。
//
// 根因:cmd/catalog 的 forwardBearerToOutgoing 只把 Authorization 注入
// **request.Context**,漏了 X-Branch-ID。middleware.XBranchID() 写的是 **gin.Context**,
// 两者不是同一个 context。于是 cubeclient.LoadCubeQuery 读不到门店,
// outgoing gRPC metadata 里就没有 x-branch-id,cube-router 收不到 X-Branch-ID,
// 直接返 400 branch_required,dapr sidecar 把这个 400 折叠成 gRPC
// `Internal: Bad Request`,catalog 的错误分类器再当成基础设施故障翻成 502。
//
// 为什么这条测试值得存在:漏注入 WithBranchID **不产生任何编译错误**,
// 也不会让任何已有单测变红 —— 现有测试全部直接构造 cubeclient,没人经过
// cmd/catalog 的中间件。唯一能在部署前抓住它的办法,是断言"这个中间件
// 到底有没有把门店写进 request.Context"。

// runCatalogMiddleware 走真实的中间件组合,返回 handler 看到的 branchID。
func runCatalogMiddleware(t *testing.T, headers map[string]string) string {
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

func TestForwardBearerToOutgoing_PropagatesBranchID(t *testing.T) {
	got := runCatalogMiddleware(t, map[string]string{
		"X-Branch-ID":    "01",
		"Authorization":  "Bearer test-token",
		"Accept":         "application/json",
		"dapr-api-token": "",
	})
	if got != "01" {
		t.Fatalf("request.Context 里必须带上门店,cubeclient 才能把它拼成 outgoing "+
			"metadata 的 x-branch-id;少了它 cube-router 返 400 branch_required,"+
			"而现象是「库存服务暂不可用」。实际 got %q", got)
	}
}

// 多店写法(`*` / `01,02`)必须**原样**透传:cube-router 自己解析语义,
// 在这里改写等于把"多店"变成"不存在的单店"。
func TestForwardBearerToOutgoing_PropagatesMultiBranchVerbatim(t *testing.T) {
	for _, in := range []string{"*", "01,02"} {
		if got := runCatalogMiddleware(t, map[string]string{"X-Branch-ID": in}); got != in {
			t.Fatalf("X-Branch-ID=%q 必须原样透传,实际 got %q", in, got)
		}
	}
}

// 没有 X-Branch-ID 时不应写 key(LoadCubeQuery 会跳过 metadata 注入),
// 但也绝不能写成空串以外的任何东西,更不能 panic。
func TestForwardBearerToOutgoing_NoBranchHeader(t *testing.T) {
	if got := runCatalogMiddleware(t, nil); got != "" {
		t.Fatalf("无 X-Branch-ID 时 branchID 应为空,实际 got %q", got)
	}
}

// gin.Context 与 request.Context 是两个东西:只挂 XBranchID() 不注入 request.Context,
// 就是 2026-10-16 那次故障的现场。这条测试把这条事实钉住。
func TestXBranchID_AloneDoesNotPopulateRequestContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var fromGin, fromReq string
	r := gin.New()
	r.Use(middleware.XBranchID())
	r.GET("/probe", func(c *gin.Context) {
		fromGin = middleware.SingleBranchFromCtx(c)
		fromReq = cubeclient.BranchFromContext(c.Request.Context())
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("X-Branch-ID", "01")
	r.ServeHTTP(httptest.NewRecorder(), req)

	if fromGin != "01" {
		t.Fatalf("XBranchID() 应把门店写进 gin.Context,实际 %q", fromGin)
	}
	if fromReq != "" {
		t.Fatalf("XBranchID() 不写 request.Context(这正是 catalog 漏注入的原因);"+
			"若哪天它开始写了,本用例需要重新评估。实际 %q", fromReq)
	}
}
