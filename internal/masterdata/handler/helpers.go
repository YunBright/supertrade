// Package handler —— master-data 的错误映射与金额解析。
package handler

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/YunBright/authkit/claims"
	"github.com/YunBright/authkit/userinfo"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/masterdata/model"
)

// mapErr 把 service 错误映射为 HTTP 状态码 + JSON body。
//
// ⚠️ code 的命名与 catalog / pricing 保持同一套,前端 error_mapper 不用分服务处理。
func (h *Handler) mapErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, model.ErrEmployeeCodeNotFound):
		// 工号不存在 ⇒ 401(凭据不成立),**不是** 403。
		//
		// ⚠️ 与"口令错"同码是**故意的**(防枚举):调用方无法区分
		// "工号不存在"与"口令错了"。
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"code": "invalid_credentials", "message": "工号或口令错误",
		})
	case errors.Is(err, model.ErrEmployeeNotFound):
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"code": "employee_not_found", "message": "收银员不存在",
		})
	case errors.Is(err, model.ErrEmployeeCodeConflict):
		// 409:该门店已有同工号的收银员(工号是**门店内唯一**的)。
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"code": "employee_code_conflict", "message": err.Error(),
		})
	case errors.Is(err, model.ErrStoreNotFound):
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"code": "store_not_found", "message": "门店不存在",
		})
	case errors.Is(err, model.ErrShiftNotFound):
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"code": "shift_not_found", "message": "班次不存在",
		})
	case errors.Is(err, model.ErrShiftAlreadyOpen):
		// 409:状态冲突(这个收银员已经有一个未交班的班次了)。
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"code": "shift_already_open", "message": "该收银员已有未交班的班次",
		})
	case errors.Is(err, model.ErrShiftClosed):
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"code": "shift_closed", "message": "班次已交班,不可重复交班",
		})
	case errors.Is(err, model.ErrInvalidInput):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "bad_request", "message": err.Error(),
		})
	case errors.Is(err, userinfo.ErrNotBoundToBranch):
		// 403:他没绑定这家店。
		//
		// ⚠️ 与"口令错"(401)**必须分开** ——
		//   "他没绑定这家店"与"口令错了"要给的引导完全不同。
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"code": "not_bound_to_branch", "message": "该收银员未绑定此门店",
		})
	case errors.Is(err, userinfo.ErrCredentialsInvalid):
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"code": "invalid_credentials", "message": "工号或口令错误",
		})
	case errors.Is(err, userinfo.ErrMachineIdentityRejected):
		// 401:是**我们**的机器身份被拒(没配服务账号 / secret 错了),
		// 不是用户输错了 ⇒ 与 invalid_credentials 分开,否则现场会去查用户的口令。
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"code": "machine_identity_rejected", "message": "服务账号不可用,请联系管理员",
		})
	default:
		h.logger.Error("master-data 未映射错误", "err", err)
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
			"code": "internal_error", "message": "系统繁忙,请稍后重试",
		})
	}
}

// callerSub 取当前调用人的 sub(填 created_by / updated_by)。
func callerSub(c *gin.Context) string {
	if cl, ok := claims.FromContext(c.Request.Context()); ok && cl != nil && cl.Sub != "" {
		return cl.Sub
	}
	return "system"
}

// parseAmount 解析金额字符串。
//
// ⚠️ **空串 = 0**;非法串也当 0 ——
//
//	理由:金额非法是**调用方**的 bug(它拼错了字符串),
//	而这里已经过了 HTTP 层的绑定校验 ⇒ 真出现非法串说明前端发了个坏值。
//	静默当 0 会让"折扣 0.95"变成"折扣 0"(打折权限失效)⇒ 所以**记日志**。
//
// 判据:金额一律用**字符串**传输(见 employeeReq.DiscountRate 的注释),
// 不用 JSON number —— 到了 Dart 侧会被 jsonDecode 变成 double。
func parseAmount(s string) decimal.Decimal {
	t := strings.TrimSpace(s)
	if t == "" {
		return decimal.Zero
	}
	d, err := decimal.NewFromString(t)
	if err != nil {
		slog.Warn("master-data: 金额字符串解析失败,按 0 处理", "value", t, "err", err)
		return decimal.Zero
	}
	return d
}

// parseRate 解析折扣率字符串(0.95 = 95 折)。
//
// ⚠️ **非法串必须报错,不能静默当 1** ——
//
//	旧写法:非法串 → parseAmount 记日志后当 0 → 这里再把 0 转成 1
//	⇒ 调用方传了 "abc" 会**静默拿到"不打折"**,而且**不报错**。
//	折扣率直接决定"能打几折"⇒ 静默改成 1 = 折扣权限失效。
//
//	⇒ 空串 = 不传 ⇒ 用默认 1(正常路径);
//	  非空但解析不了 ⇒ 返错误,让调用方知道自己传错了。
func parseRate(s string) (decimal.Decimal, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return decimal.NewFromInt(1), nil
	}
	d, err := decimal.NewFromString(t)
	if err != nil {
		return decimal.Zero, fmt.Errorf("%w: discount_rate 不是合法数字: %q", model.ErrInvalidInput, t)
	}
	if d.IsNegative() || d.GreaterThan(decimal.NewFromInt(1)) {
		return decimal.Zero, fmt.Errorf("%w: discount_rate 必须在 [0,1] 之间: %q", model.ErrInvalidInput, t)
	}
	return d, nil
}
