// Package handler 实现 pos-gateway 的 HTTP handler(BFF 对前端的那一面)。
//
// 路由(nginx 已剥过 /api/v1/pos-gateway/):
//
//	GET  /home                    收银主页聚合(门店 + 班次 + 收银员 + 会员开关 + 付款方式)
//	GET  /config                  POS 端配置聚合(门店 / 会员开关 / 会员类别 / 付款方式 / 数字键盘)
//	GET  /members/:id/last-order  图7「上一单」一次返回{member, sale_lines[], payments[]}
//	GET  /products/search         商品搜索代理(货号 / 自编码 / 名称 / 条码;可能命中多个候选)
//	GET  /prices                  查价代理(本地售价 + 当前生效促销)
//	POST /sales                   开单代理(转发给 pos,op_id 原样透传)
//
// # ⚠️ 内部路径**不带 /v1/**
//
// nginx 只剥一层前缀(`rewrite ^/api/v1/[^/]+/(.*)$ …`),
// 内部再带 /v1/ 会让前端路径出现**双层 v1**。
// ⇒ 前端调 `/api/v1/pos-gateway/home`。
//
// # ⚠️ 错误信封统一 `{code, message}`
//
// 见 errors.go。
package handler

import (
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/YunBright/authkit/claims"
	"github.com/YunBright/authkit/rbac"
	"github.com/YunBright/authkit/userinfo"
	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/posgateway/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// BFF 端点用的 scope。
//
// ⚠️ 每个端点按它**主要的下游域**取 scope:
//
//	BFF 是代理,权限最终由下游按 per-branch 实时解析,
//	这里守的是"这个人能不能用这个功能"的粗筛。
const (
	// ScopeSale 是收银(开单 / 收银主页 / 配置)。
	ScopeSale = "pos:sale"
	// ScopeMemberView 是会员查询(图7 上一单)。
	ScopeMemberView = "member:view"
	// ScopeProductView 是商品查询。
	ScopeProductView = "product:view"
	// ScopePricingView 是查价。
	ScopePricingView = "pricing:view"
)

// Handler 是 pos-gateway 的 HTTP handler 聚合。
type Handler struct {
	svc      *service.Service
	resolver rbac.ScopeResolver
	logger   *slog.Logger
}

// New 构造 Handler(users 用于 per-branch scope 守门;传 nil 时守门一律 403)。
func New(svc *service.Service, users *userinfo.Client, logger *slog.Logger) *Handler {
	return NewWithResolver(svc, rbac.HasAnyScopeWithBranch(users), logger)
}

// NewWithResolver 构造 Handler 并显式注入 scope 解析器(单测用 fake)。
func NewWithResolver(svc *service.Service, resolver rbac.ScopeResolver, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{svc: svc, resolver: resolver, logger: logger}
}

// RegisterRoutes 挂载全部路由。
//
// ⚠️ 每个业务端点都挂 `rbac.RequireScopeWithBranch` —— **带 branch 维度**。
// 权限是 per-branch 实时解析的:用户没绑这家店 ⇒ 在该店**没有任何 scope**
// (连全局角色权限也没有)⇒ 403。
func (h *Handler) RegisterRoutes(r gin.IRouter) {
	r.GET("/home",
		rbac.RequireScopeWithBranch(ScopeSale, branchFromHeader, h.resolver),
		h.getHome)
	r.GET("/config",
		rbac.RequireScopeWithBranch(ScopeSale, branchFromHeader, h.resolver),
		h.getConfig)
	r.GET("/members/:id/last-order",
		rbac.RequireScopeWithBranch(ScopeMemberView, branchFromHeader, h.resolver),
		h.getLastOrder)
	r.GET("/products/search",
		rbac.RequireScopeWithBranch(ScopeProductView, branchFromHeader, h.resolver),
		h.searchProducts)
	r.GET("/prices",
		rbac.RequireScopeWithBranch(ScopePricingView, branchFromHeader, h.resolver),
		h.getPrices)
	r.POST("/sales",
		rbac.RequireScopeWithBranch(ScopeSale, branchFromHeader, h.resolver),
		h.createSale)
}

// branchFromHeader 取 X-Branch-ID(单店)。
//
// "" ⇒ RequireScopeWithBranch 按 400 branch_required 拒。
func branchFromHeader(c *gin.Context) (string, bool) {
	b := middleware.SingleBranchFromCtx(c)
	if b == "" {
		return "", false
	}
	return b, true
}

// callerSub 拿当前 caller 的 sub(JWT claims);拿不到返 ""。
func callerSub(c *gin.Context) string {
	cl, _ := claims.FromContext(c.Request.Context())
	if cl == nil {
		return ""
	}
	return cl.Sub
}

// getHome GET /home
func (h *Handler) getHome(c *gin.Context) {
	view, err := h.svc.Home(c.Request.Context(), service.HomeInput{
		BranchID:   middleware.SingleBranchFromCtx(c),
		OperatorID: callerSub(c),
		CounterNo:  strings.TrimSpace(c.Query("counter_no")),
	})
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

// getConfig GET /config
func (h *Handler) getConfig(c *gin.Context) {
	view, err := h.svc.Config(c.Request.Context(), middleware.SingleBranchFromCtx(c))
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

// getLastOrder GET /members/:id/last-order(图7)
func (h *Handler) getLastOrder(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		writeError(c, http.StatusBadRequest, "bad_request", "id 不能为空")
		return
	}
	view, err := h.svc.LastOrder(c.Request.Context(), middleware.SingleBranchFromCtx(c), id)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

// searchProducts GET /products/search
//
// ⚠️ 关键字**两个名字都收**（`q` 与 `keyword`）——
// 前端可能按思迅的老习惯发 `q`，也可能发 `keyword`。
// 只认一个 ⇒ 另一个名字的请求**静默变成空搜索**
// （症状：搜任何东西都返回空列表，而请求是 200）。
func (h *Handler) searchProducts(c *gin.Context) {
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	keyword := strings.TrimSpace(c.Query("q"))
	if keyword == "" {
		keyword = strings.TrimSpace(c.Query("keyword"))
	}
	view, err := h.svc.SearchProducts(c.Request.Context(), service.ProductSearchInput{
		BranchID: middleware.SingleBranchFromCtx(c),
		Keyword:  keyword,
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

// getPrices GET /prices?product_ids=a,b,c
func (h *Handler) getPrices(c *gin.Context) {
	ids := splitIDs(c.Query("product_ids"))
	view, err := h.svc.Prices(c.Request.Context(), service.PricesInput{
		BranchID:   middleware.SingleBranchFromCtx(c),
		ProductIDs: ids,
		OnDate:     strings.TrimSpace(c.Query("on_date")),
	})
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

// createSale POST /sales
//
// ⚠️ body **原样**转发给 pos(op_id 必须一字不改 —— 离线重传靠它幂等)。
//
// ⚠️ 响应契约:成功时 pos 的响应体**原样**回传(不重新建模 ⇒ 零漂移),状态码恒 200;
// 下游错误走统一错误信封 {code,message},状态按分类映射(401/403/404/… 原样,其它 ⇒ 502)。
func (h *Handler) createSale(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "读取请求体失败:"+err.Error())
		return
	}
	raw, err := h.svc.CreateSale(c.Request.Context(), middleware.SingleBranchFromCtx(c), body)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	// 原样回传 pos 的响应体(不重新建模 ⇒ 零漂移)。
	c.Data(http.StatusOK, "application/json; charset=utf-8", raw)
}

// splitIDs 解析逗号分隔的 id 列表(去空、去重、保序)。
func splitIDs(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}
