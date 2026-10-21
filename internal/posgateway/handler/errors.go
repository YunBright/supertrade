package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/posgateway/service"
)

// errBody 是统一的错误信封 `{code, message}`。
//
// ⚠️ 全仓约定(catalog / pos / member / master-data 都这样):
// 前端按 `code` 分支,不解析 message —— message 会改文案。
type errBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeError 写统一错误信封。
func writeError(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, errBody{Code: code, Message: msg})
}

// mapErr 把 service / 下游错误映射为 HTTP 状态码 + 错误信封。
//
// # ⚠️ 鉴权失败**不降级**
//
// 下游 401 / 403 一律原样上抛(401 / 403),
// 绝不吞成"部分成功" —— 那会让客户端以为拿到了数据,
// 实际是一条越权响应被静默掉了。
func (h *Handler) mapErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrInvalidInput):
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, service.ErrStoreNotFound):
		writeError(c, http.StatusNotFound, "store_not_found", err.Error())
	case errors.Is(err, service.ErrMemberNotFound):
		writeError(c, http.StatusNotFound, "member_not_found", err.Error())
	default:
		if de := service.AsDownstreamError(err); de != nil {
			h.mapDownstreamErr(c, de)
			return
		}
		h.logger.Error("pos-gateway handler error", "err", err, "path", c.Request.URL.Path)
		writeError(c, http.StatusInternalServerError, "internal_error", err.Error())
	}
}

// mapDownstreamErr 把下游错误映射为 HTTP 状态码。
//
// ⚠️ 401 / 403 **必须**与其它状态码分开:
//
//	前者是"你的登录态 / 权限有问题"(重新登录或找人授权能好),
//	后者是"下游服务有问题"(重试可能有用,或者要去看服务)。
//	合并的后果是一次登录态问题被显示成「系统繁忙」,排查全部跑去查下游健康度。
func (h *Handler) mapDownstreamErr(c *gin.Context, de *service.DownstreamError) {
	switch de.Status {
	case http.StatusUnauthorized:
		writeError(c, http.StatusUnauthorized, "unauthenticated", de.Message)
	case http.StatusForbidden:
		writeError(c, http.StatusForbidden, "forbidden", de.Message)
	case http.StatusNotFound:
		writeError(c, http.StatusNotFound, "not_found", de.Message)
	case http.StatusBadRequest:
		writeError(c, http.StatusBadRequest, "bad_request", de.Message)
	case http.StatusConflict:
		writeError(c, http.StatusConflict, "conflict", de.Message)
	case http.StatusTooManyRequests:
		writeError(c, http.StatusTooManyRequests, "too_many_requests", de.Message)
	case http.StatusServiceUnavailable:
		writeError(c, http.StatusServiceUnavailable, "upstream_unavailable", de.Message)
	case http.StatusGatewayTimeout:
		writeError(c, http.StatusGatewayTimeout, "upstream_timeout", de.Message)
	default:
		h.logger.Error("pos-gateway 下游错误", "err", de, "app", de.AppID, "path", de.Path)
		writeError(c, http.StatusBadGateway, "upstream_error", de.Message)
	}
}
