// Package handler / proxy.go —— /v1/load 转发实现。
//
// 流程:
//  1. middleware.XBranchID() 已注入 ctx branch UUID(无 header → 400)
//  2. svc.ResolveCubeSource(branchID) → cube_source_name(可能 404 / 503)
//  3. 透传 gin request body → dapr service invocation → 透传 response
//
// JWT 透传:cube-gateway 的 dapr-sidecar bearer middleware 要求
// "Authorization: Bearer <token>" header,这里直接从 gin request 复制到
// outgoing dapr invocation request。
//
// 为什么不复用 cubeclient.HTTPCubeClient.LoadCubeQuery:
//   - 它要求 CubeQuery 结构(measures/dimensions 字段),会改写 body shape
//   - cube /v1/load 实际接受任意 JSON(measures/dimensions/filters/limit/...)
//   - 这里保留调用方原 body,避免协议耦合与字段丢失
package handler

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/pkg/middleware"
)

// httpClientTimeout 是转发到 cube 的 HTTP 超时。
const httpClientTimeout = 30 * time.Second

// proxyLoad 转发 POST /v1/load 到对应 cube instance。
func (h *Handler) proxyLoad(c *gin.Context) {
	branchID := middleware.BranchFromCtx(c)
	if branchID == nil {
		// 中间件应该已阻断了非法 header;此处兜底。
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "missing_branch_id",
			"message": "X-Branch-ID header 必填",
		})
		return
	}

	// 解析 cube source(可能 ErrNotFound / ErrDisabled / DB error)
	cubeSourceName, _, err := h.svc.ResolveCubeSource(c.Request.Context(), branchID.String())
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

// rawForward 直接 POST body 给 dapr service invocation,透传 response。
func (h *Handler) rawForward(c *gin.Context, cubeSourceName string, body []byte) {
	url := fmt.Sprintf("%s/v1.0/invoke/%s/method/v1/load", h.daprEndpoint, cubeSourceName)
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
			"code":    "internal_error",
			"message": "construct request: " + err.Error(),
		})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer := c.Request.Header.Get("Authorization"); bearer != "" {
		req.Header.Set("Authorization", bearer)
	}

	httpClient := &http.Client{Timeout: httpClientTimeout}
	resp, err := httpClient.Do(req)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{
			"code":    "cube_unavailable",
			"message": err.Error(),
		})
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	c.Data(resp.StatusCode, "application/json", respBody)
}