// Package handler —— member 服务的错误映射与入参解析。
//
// # 错误信封
//
// 统一 `{code, message}`(照抄 internal/stocktake/handler/handler.go 的 errBody)。
// ⛔ 不引共享 apierror 包 —— 仓里**没有**那个包,各服务各写一份。
package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/YunBright/authkit/claims"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/member/model"
	"github.com/YunBright/supertrade/internal/member/service"
)

// errBody 是统一错误信封。
type errBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// RequiredScope 是缺少的 scope(仅 member_permission_required 带)。
	RequiredScope string `json:"required_scope,omitempty"`
}

// writeError 写错误信封。
func writeError(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, errBody{Code: code, Message: msg})
}

// mapErr 把 service / model 错误映射为 HTTP 状态码 + JSON body。
//
// ⚠️ code 是**按资源分的**:member_not_found / member_points_insufficient …
//
//	前端 error_mapper 按这些字符串判定,别改成通用的 not_found / conflict。
func mapErr(c *gin.Context, err error) {
	// 越权授权链路的两种 403(带 required_scope / 图5 文案)—— 单独一个 case。
	var pe *service.PermissionError
	if errors.As(err, &pe) {
		c.AbortWithStatusJSON(http.StatusForbidden, errBody{
			Code:          pe.Code,
			Message:       pe.Message,
			RequiredScope: pe.RequiredScope,
		})
		return
	}
	switch {
	case errors.Is(err, model.ErrMemberNotFound):
		writeError(c, http.StatusNotFound, "member_not_found", "会员不存在")
	case errors.Is(err, model.ErrMemberAlreadyExists):
		writeError(c, http.StatusConflict, "member_already_exists", "会员已存在")
	case errors.Is(err, model.ErrMemberTypeNotFound):
		writeError(c, http.StatusNotFound, "member_type_not_found", "会员类别不存在")
	case errors.Is(err, model.ErrMemberTypeAlreadyExists):
		writeError(c, http.StatusConflict, "member_type_already_exists", "会员类别已存在")
	case errors.Is(err, model.ErrMemberTypeInUse):
		writeError(c, http.StatusConflict, "member_type_in_use", err.Error())
	case errors.Is(err, model.ErrPointsInsufficient):
		// 409:状态冲突(改参数没用,只能充值或调整)。
		writeError(c, http.StatusConflict, "member_points_insufficient", "剩余积分不足")
	case errors.Is(err, model.ErrAccountInsufficient):
		writeError(c, http.StatusConflict, "member_account_insufficient", "储值余额不足")
	case errors.Is(err, model.ErrMemberStatusNotAllowed):
		// 409:卡被冻结/挂失/注销 ⇒ 这类操作当前不允许。
		writeError(c, http.StatusConflict, "member_status_not_allowed", "会员当前状态不允许该操作")
	case errors.Is(err, model.ErrInvalidQueryField):
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, model.ErrInvalidInput):
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, model.ErrCubeSourceUnavailable):
		// ⚠️ 503 而不是回落成空列表:这是链路故障(问不到路由),
		// 回落会让用户看到"该门店还没有会员档案"——与故障无关的结论。
		writeError(c, http.StatusServiceUnavailable, "cube_unavailable", "数据源路由暂不可用，请稍后重试")
	case errors.Is(err, model.ErrCubeSourceDisabled):
		writeError(c, http.StatusServiceUnavailable, "cube_source_disabled", "该门店的数据源已停用")
	case errors.Is(err, model.ErrExternalSourceReadOnly):
		// 409:状态冲突(这个门店的账在外部系统里,不是权限问题)。
		writeError(c, http.StatusConflict, "member_source_is_external", err.Error())
	case errors.Is(err, cubeclient.ErrCubeForbidden):
		writeError(c, http.StatusForbidden, "forbidden", "没有该门店的数据读权限")
	case errors.Is(err, cubeclient.ErrCubeAuthRejected):
		writeError(c, http.StatusUnauthorized, "unauthorized", "登录已失效，请重新登录")
	case errors.Is(err, cubeclient.ErrCubeBadRequest):
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, cubeclient.ErrCubeUnavailable):
		writeError(c, http.StatusServiceUnavailable, "cube_unavailable", "数据源暂不可用，请稍后重试")
	case errors.Is(err, service.ErrInvalidOldPassword):
		writeError(c, http.StatusForbidden, "member_password_invalid", "原口令不正确")
	case errors.Is(err, service.ErrCredentialsInvalid):
		// 授权不通过 ⇒ 图5 的 alert 文案。
		writeError(c, http.StatusForbidden, service.CodePermissionDenied, "你没有该操作的权限!")
	case errors.Is(err, service.ErrVerifierUnavailable):
		// 503:授权链路本身不可用(机器身份没配 / master-data 调不通)——
		// ⛔ **不是** 403:那会让现场以为用户输错了口令。
		writeError(c, http.StatusServiceUnavailable, "member_authorizer_unavailable", "授权校验暂不可用,请稍后重试")
	default:
		slog.Error("member handler error", "err", err, "path", c.Request.URL.Path)
		c.AbortWithStatusJSON(http.StatusInternalServerError, errBody{
			Code:    "internal_error",
			Message: "系统繁忙,请稍后重试",
		})
	}
}

// callerSub 取当前调用人的 sub(填 created_by / updated_by / operator_id)。
//
// ⚠️ 拿不到时兜底 "system"(照抄 internal/catalog/handler/handler.go)。
func callerSub(c *gin.Context) string {
	if cl, ok := claims.FromContext(c.Request.Context()); ok && cl != nil && cl.Sub != "" {
		return cl.Sub
	}
	return "system"
}

// parseAmount 解析金额字符串。
//
// ⚠️ **空串 = 0**;非法串也当 0,但**记日志** ——
//
//	金额非法是调用方的 bug,静默当 0 会让"充值 100 元"变成"充值 0 元"。
//
// 判据:金额一律用**字符串**传输(不用 JSON number)——
//
//	到了 Dart 侧 jsonDecode 会把 number 变成 double。
func parseAmount(s string) decimal.Decimal {
	t := strings.TrimSpace(s)
	if t == "" {
		return decimal.Zero
	}
	d, err := decimal.NewFromString(t)
	if err != nil {
		slog.Warn("member: 金额字符串解析失败,按 0 处理", "value", t, "err", err)
		return decimal.Zero
	}
	return d
}
