// Package handler / proxy.go —— /v1/load 转发实现。
//
// 流程:
//  1. middleware.XBranchID() 已注入 ctx branch UUID(无 header → 400)
//  2. svc.ResolveCubeSource(branchID) → cube_source_name(可能 404 / 503)
//  3. 透传 gin request body → dapr service invocation → 透传 response
//
// 为什么不复用 cubeclient.DaprCubeClient.LoadCubeQuery:
//   - 它要求 CubeQuery 结构(measures/dimensions 字段),会改写 body shape
//   - 这里保留调用方原 body,避免协议耦合与字段丢失
//
// 出口方法名是 query 而不是 v1/load:
//   cube 侧 cube app 只注册了 POST /query + GET /healthz;cube-gateway 也只注册
//   /register /unregister /v1/source/:source/load /v1/sources /healthz,并且
//   gateway 调上游同样走 /query。整条链路上没有任何服务提供 v1/load,
//   原先转发 v1/load 会稳定拿到 404。
package handler

import (
	"context"
	"io"
	"net/http"
	"time"

	dapr "github.com/dapr/go-sdk/client"
	"github.com/gin-gonic/gin"
	"google.golang.org/grpc/metadata"

	"github.com/YunBright/supertrade/pkg/middleware"
)

// invokeTimeout 是转发到 cube 的 gRPC 调用超时。
const invokeTimeout = 30 * time.Second

// proxyLoad 转发 POST /v1/load 到对应 cube instance。
func (h *Handler) proxyLoad(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		// 中间件应该已阻断了非法 header;此处兜底。
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "branch_required",
			"message": "X-Branch-ID header 必填",
		})
		return
	}

	// 解析 cube source(可能 ErrNotFound / ErrDisabled / DB error)
	cubeSourceName, _, err := h.svc.ResolveCubeSource(c.Request.Context(), branchID)
	if err != nil {
		h.mapErr(c, err)
		return
	}

	// 读 body(转发需要原样 byte slice)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "bad_request",
			"message": "read body: " + err.Error(),
		})
		return
	}
	defer c.Request.Body.Close()

	h.rawForward(c, cubeSourceName, body)
}

// rawForward 经 dapr SDK 把 body 转发到目标 cube instance,透传 response。
func (h *Handler) rawForward(c *gin.Context, cubeSourceName string, body []byte) {
	ctx := c.Request.Context()
	// JWT 透传:把 gin request 的 Authorization 头塞 outgoing gRPC metadata。
	// dapr sidecar 把 outgoing gRPC metadata keys 转 outgoing HTTP headers 给目标 app。
	if bearer := c.Request.Header.Get("Authorization"); bearer != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", bearer)
	}
	// X-Branch-ID 透传:cube-router → cube 必须知道走哪个门店数据;
	// sidecar 把 outgoing metadata "x-branch-id" → outgoing HTTP "X-Branch-ID"。
	if bid := c.Request.Header.Get("X-Branch-ID"); bid != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-branch-id", bid)
	}
	ctx, cancel := context.WithTimeout(ctx, invokeTimeout)
	defer cancel()

	resp, err := h.daprClient.InvokeMethodWithContent(ctx, cubeSourceName, cubeQueryMethod, "POST",
		&dapr.DataContent{ContentType: "application/json", Data: body})
	if err != nil {
		// dapr SDK 把 sidecar 端 HTTP 4xx/5xx 转 gRPC status error。
		//
		// 关键:404 必须显式区分。cube app 返回 404 = "这个 query 查不到数据"是**业务结论**,
		// 但 "方法名写错 / source 没注册 / 路由没命中" 也是 404,且属于基础设施故障。
		// 两者混在一起,上层就无法区分"商品不存在"和"链路坏了"——
		// 原先的错误坍缩链就是:
		//   cube 404 → gRPC NotFound → ErrCubeNotFound → ErrProductNotFound → 200 [] →
		//   Flutter 显示「本门店没有条码 XXX 的商品」
		// 把基础设施故障显示成业务结论,是比 502 更糟的失败模式。
		code, status, msg := classifyInvokeError(err)
		c.AbortWithStatusJSON(status, gin.H{
			"code":    code,
			"message": msg,
		})
		return
	}
	c.Data(http.StatusOK, "application/json", resp)
}