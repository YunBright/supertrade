// Package handler 实现 pos 服务的 HTTP handler。
//
// 路由(nginx 已剥过 /api/v1/pos/):
//
//	POST /sales                                开单/收款(pos:sale)
//	GET  /sales                                销售单列表(pos:sale)
//	GET  /sales/:id                            查单(pos:sale)
//	POST /sales/:id/refund                     退货(pos:refund)
//	POST /sales/:id/lines/:line_id/price-override  改价(pos:price-override)
//
// # 门店从哪来
//
// ⚠️ **门店只从 X-Branch-ID header 取**(scope 守门用的也是它)。
//
//	body 里带一份 branch_id 就能把 A 店的单写进 B 店 ⇒ body 的 branch_id
//	只做**一致性校验**(与 header 不一致 ⇒ 400 branch_mismatch),不作为真源。
//
// # 金额一律字符串
//
// 请求/响应里的金额都是**字符串**("22.20"),不是 JSON number ——
// JSON number 到 Dart 侧会变 double。
package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/YunBright/authkit/claims"
	"github.com/YunBright/authkit/rbac"
	"github.com/YunBright/authkit/userinfo"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/pos/model"
	"github.com/YunBright/supertrade/internal/pos/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// pos 域的 scope(方案 §7.1)。
const (
	// ScopeSale 是收银(开单 / 查单 / 列表)。
	ScopeSale = "pos:sale"
	// ScopeRefund 是退货。
	ScopeRefund = "pos:refund"
	// ScopePriceOverride 是改价。
	ScopePriceOverride = "pos:price-override"
)

// Handler 是 pos 服务的 HTTP handler 聚合。
type Handler struct {
	sales *service.SalesService
	// resolver 是 per-branch scope 解析器(userd 权限矩阵)。
	resolver rbac.ScopeResolver
	logger   *slog.Logger
}

// New 构造 Handler(users 用于 per-branch scope 守门;传 nil 时守门一律 403)。
func New(sales *service.SalesService, users *userinfo.Client, logger *slog.Logger) *Handler {
	return NewWithResolver(sales, rbac.HasAnyScopeWithBranch(users), logger)
}

// NewWithResolver 构造 Handler 并显式注入 scope 解析器(单测用 fake)。
func NewWithResolver(sales *service.SalesService, resolver rbac.ScopeResolver, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{sales: sales, resolver: resolver, logger: logger}
}

// RegisterRoutes 挂载全部路由。
//
// ⚠️ 改价端点**不挂** rbac.RequireScopeWithBranch ——
//
//	那个中间件无 scope 时返的是通用 403 {code:"forbidden"},
//	而收银现场需要的是"请输入授权"这条**可引导**的 403:
//	{code:"pos_permission_required", message:"当前收银员没有改价权限，请输入授权",
//	 required_scope:"pos:price-override"}
//	⇒ 改价的 scope 检查放在 handler 里(因为要看 body 里的 authorized_by)。
//
//	该端点仍然在**鉴权链内**(cmdbootstrap 的全局验签 + RequireAudience)。
func (h *Handler) RegisterRoutes(r gin.IRouter) {
	r.POST("/sales",
		rbac.RequireScopeWithBranch(ScopeSale, branchFromHeader, h.resolver),
		h.createSale)
	r.GET("/sales",
		rbac.RequireScopeWithBranch(ScopeSale, branchFromHeader, h.resolver),
		h.listSales)
	r.GET("/sales/:id",
		rbac.RequireScopeWithBranch(ScopeSale, branchFromHeader, h.resolver),
		h.getSale)
	r.POST("/sales/:id/refund",
		rbac.RequireScopeWithBranch(ScopeRefund, branchFromHeader, h.resolver),
		h.refundSale)
	r.POST("/sales/:id/lines/:line_id/price-override",
		h.priceOverride)
}

// ---- helpers ----

// branchFromHeader 拿当前请求的 X-Branch-ID header(单店场景)。
//
// ""(header 未传)→ 返 ("", false) → RequireScopeWithBranch 按 400 branch_required 拒。
func branchFromHeader(c *gin.Context) (string, bool) {
	b := middleware.SingleBranchFromCtx(c)
	if b == "" {
		return "", false
	}
	return b, true
}

// callerSub 拿当前 caller 的 sub(填 created_by / updated_by);拿不到兜底 "system"。
func callerSub(c *gin.Context) string {
	cl, _ := claims.FromContext(c.Request.Context())
	if cl == nil || cl.Sub == "" {
		return "system"
	}
	return cl.Sub
}

// hasScope 判断当前调用人在该门店是否拥有 scope。
//
// ⚠️ 返回 error 时(权限服务不可用)必须**拒绝**(503),不能当成"有权限"。
func (h *Handler) hasScope(c *gin.Context, branchID, scope string) (bool, error) {
	cl, ok := claims.FromContext(c.Request.Context())
	if !ok || cl == nil {
		return false, errUnauthenticated
	}
	if h.resolver == nil {
		return false, nil
	}
	return h.resolver(c, cl.Sub, branchID, scope)
}

// errUnauthenticated 表示请求没有可用的 claims。
var errUnauthenticated = errors.New("pos: 缺少已签 token")

// parseAmount 解析金额字符串(**严格**:非法值直接报错,不静默当 0)。
//
// ⚠️ 与 master-data 的宽松版不同:收银金额非法就是**账实不符**的入口,
//
//	静默当 0 会让"折扣 0"变成"白送"。⇒ 解析失败一律 400。
func parseAmount(field, s string) (decimal.Decimal, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return decimal.Zero, nil
	}
	d, err := decimal.NewFromString(t)
	if err != nil {
		return decimal.Zero, &amountParseError{field: field, value: t, err: err}
	}
	return d, nil
}

// amountParseError 是金额字符串解析失败。
type amountParseError struct {
	field string
	value string
	err   error
}

func (e *amountParseError) Error() string {
	return "pos: " + e.field + " 不是合法金额:" + e.value
}

func (e *amountParseError) Unwrap() error { return e.err }

// ---- 错误响应 ----

// errBody 是统一的错误信封。
type errBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// permRequiredBody 是"需要授权"的 403(比通用 403 更可引导)。
type permRequiredBody struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	RequiredScope string `json:"required_scope"`
}

func writeError(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, errBody{Code: code, Message: msg})
}

// mapErr 把 service 错误映射为 HTTP 状态码 + 错误信封。
//
// ⚠️ code **按资源/语义分**,不按 HTTP 状态统一(与 catalog / master-data 同风格)。
func (h *Handler) mapErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, model.ErrSaleNotFound):
		writeError(c, http.StatusNotFound, "sale_not_found", err.Error())
	case errors.Is(err, model.ErrLineNotFound):
		writeError(c, http.StatusNotFound, "sale_line_not_found", err.Error())
	case errors.Is(err, model.ErrSaleAlreadyReturned):
		writeError(c, http.StatusConflict, "sale_already_returned", err.Error())
	case errors.Is(err, model.ErrAmountMismatch):
		// 409:金额不自洽是**状态冲突**(改参数重来一次也不会好),
		// 不是请求格式错(那才是 400)。
		writeError(c, http.StatusConflict, "amount_mismatch", err.Error())
	case errors.Is(err, model.ErrInvalidInput):
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, errUnauthenticated):
		writeError(c, http.StatusUnauthorized, "unauthenticated", "缺少已签 token")
	default:
		h.logger.Error("pos handler error", "err", err, "path", c.Request.URL.Path)
		writeError(c, http.StatusInternalServerError, "internal_error", err.Error())
	}
}
