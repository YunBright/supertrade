// Package handler 实现 bank-account 服务的 HTTP handler(Gin)。
//
// 端点(nginx 已剥过 /api/v1/bank-accounts/):
//
//	GET    /supplier-bank-accounts              列表(?supplier_id&only_default)  scope settlement:view
//	POST   /supplier-bank-accounts              新建                              scope supplier:manage
//	GET    /supplier-bank-accounts/:id          详情                              scope settlement:view
//	PUT    /supplier-bank-accounts/:id          修改                              scope supplier:manage
//	DELETE /supplier-bank-accounts/:id          软删                              scope supplier:manage
//	POST   /supplier-bank-accounts/:id/default  设为该供应商的默认收款账号        scope supplier:manage
//
// # 为什么读用 settlement:view、写用 supplier:manage
//
// ⚠️ **这是有意的不对称**，写在这里以免后人"统一"成同一个 scope：
//
//	读(settlement:view) → 店长在 H5「申请付款审批」页要能挑收款账号。
//	    merchant 角色**持有** settlement:view ⇒ 门店能自助付款。
//	    ⚠️ merchant **没有** supplier:view(001 迁移的注释写明 merchant
//	    没有 supplier:view / product:view)⇒ 用 supplier:view 守读端点
//	    会让店长在付款页直接 403 ⇒ 功能做不了。
//
//	写(supplier:manage) → **故意不给 merchant**。
//	    收款账号是"钱付给谁"的最终落点 ⇒ 能改它 = 能改收款人。
//	    这条能力必须收紧到 admin(或经 user_branch_permissions 逐店显式授权)。
//	    merchant 拿到它 = 任何店长都能把供应商收款账号改成自己的户名。
//
// ⇒ 结论：**能读 ≠ 能改**。读是日常付款流程，写是资金风险动作。
//
// # 鉴权链路
//
//	rbac.RequireScopeWithBranch(scope, branchFromHeader, resolver)
//	  - branchFn 取 X-Branch-ID(缺 → 400 branch_required)
//	  - claims 缺失 → 401 unauthenticated
//	  - resolver 报错(userd 不可用) → 503 userd_unavailable
//	  - 该 branch 下无此 scope → 403 forbidden
//
// 错误映射:ErrNotFound→404,ErrAlreadyExists→409,
// ErrInvalidInput→400。
package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/YunBright/authkit/claims"
	"github.com/YunBright/authkit/rbac"
	"github.com/YunBright/authkit/userinfo"
	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/bankaccount/model"
	"github.com/YunBright/supertrade/internal/bankaccount/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// 端点使用的 scope。
const (
	// ScopeView 是读端点的 scope(店长在付款审批页要用)。
	ScopeView = "settlement:view"
	// ScopeManage 是写端点的 scope(**merchant 不持有** ⇒ 收款账号只能由 admin 维护)。
	ScopeManage = "supplier:manage"
)

// Handler 是 bank-account 的 HTTP handler 聚合。
type Handler struct {
	svc *service.Service
	// users 是 userd 客户端(per-branch effective scopes 校验)。
	// nil ⇒ 带 scope 守门的端点一律 503。
	users *userinfo.Client
}

// New 构造 handler。
//
// users 为 nil 时带 scope 守门的端点一律 503(fail-closed)——
// 与 master-data 同款。
func New(svc *service.Service, users *userinfo.Client) *Handler {
	return &Handler{svc: svc, users: users}
}

// branchFromHeader 取目标 branch_id。
func branchFromHeader(c *gin.Context) (string, bool) {
	b := middleware.SingleBranchFromCtx(c)
	if b == "" {
		return "", false
	}
	return b, true
}

// RegisterRoutes 注册业务路由。
//
// X-Branch-ID 由 cmd main 的 middleware.XBranchID() 注入 ctx;
// 这里每路由挂 RequireBranch 拦截缺失。
func (h *Handler) RegisterRoutes(r gin.IRouter) {
	requireBranch := middleware.RequireBranch()
	resolver := rbac.HasAnyScopeWithBranch(h.users)

	r.GET("/supplier-bank-accounts",
		requireBranch,
		rbac.RequireScopeWithBranch(ScopeView, branchFromHeader, resolver),
		h.List)
	r.POST("/supplier-bank-accounts",
		requireBranch,
		rbac.RequireScopeWithBranch(ScopeManage, branchFromHeader, resolver),
		h.Create)
	// ⚠️ 静态段必须注册在 /:id 之前(gin 里静态段优先于参数段,
	// 但显式写出来更安全 —— 见 settlement-auditor 的同类注释)。
	r.POST("/supplier-bank-accounts/:id/default",
		requireBranch,
		rbac.RequireScopeWithBranch(ScopeManage, branchFromHeader, resolver),
		h.SetDefault)
	r.GET("/supplier-bank-accounts/:id",
		requireBranch,
		rbac.RequireScopeWithBranch(ScopeView, branchFromHeader, resolver),
		h.Get)
	r.PUT("/supplier-bank-accounts/:id",
		requireBranch,
		rbac.RequireScopeWithBranch(ScopeManage, branchFromHeader, resolver),
		h.Update)
	r.DELETE("/supplier-bank-accounts/:id",
		requireBranch,
		rbac.RequireScopeWithBranch(ScopeManage, branchFromHeader, resolver),
		h.Delete)
}

// errBody 是错误响应体(与 settlement-auditor 同形:code + message)。
type errBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, errBody{Code: code, Message: msg})
}

// mapErr 把 service 错误映成 HTTP 状态码。
func mapErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrNotFound):
		writeError(c, http.StatusNotFound, "bank_account_not_found", err.Error())
	case errors.Is(err, service.ErrAlreadyExists):
		// 409 而不是 400:请求本身合法,是"撞了已有数据" ⇒ 客户端该换个值而不是改参数格式。
		writeError(c, http.StatusConflict, "bank_account_already_exists", err.Error())
	case errors.Is(err, service.ErrInvalidInput):
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
	default:
		writeError(c, http.StatusInternalServerError, "internal_error", err.Error())
	}
}

// branchIDOf 取本次请求的门店 id(X-Branch-ID 由中间件注入 ctx)。
func branchIDOf(c *gin.Context) string {
	if v := middleware.SingleBranchFromCtx(c); v != "" {
		return v
	}
	return strings.TrimSpace(c.GetHeader("X-Branch-ID"))
}

// operatorOf 取操作人 auth user id。
//
// ⚠️ 取 claims.Sub(auth user id)—— **不信任客户端自称的 operator**,
// 否则审计字段等于谁都能填。
func operatorOf(c *gin.Context) string {
	cl, _ := claims.FromContext(c.Request.Context())
	if cl == nil {
		return ""
	}
	return cl.Sub
}

// accountReq 是新建/修改的请求体。
type accountReq struct {
	SupplierID   string `json:"supplier_id"`
	SupplierName string `json:"supplier_name"`
	BankName     string `json:"bank_name"`
	AccountNo    string `json:"account_no"`
	AccountName  string `json:"account_name"`
	IsDefault    bool   `json:"is_default"`
	Remark       string `json:"remark"`
}

// toInput 收敛成 service 层入参。
func (r accountReq) toInput() service.AccountInput {
	return service.AccountInput{
		SupplierID:   r.SupplierID,
		SupplierName: r.SupplierName,
		BankName:     r.BankName,
		AccountNo:    r.AccountNo,
		AccountName:  r.AccountName,
		IsDefault:    r.IsDefault,
		Remark:       r.Remark,
	}
}

// List GET /supplier-bank-accounts?supplier_id&only_default
//
// scope: settlement:view(店长在付款审批页要读)。
func (h *Handler) List(c *gin.Context) {
	branchID := branchIDOf(c)
	onlyDefault, _ := strconv.ParseBool(c.DefaultQuery("only_default", "false"))
	rows, err := h.svc.List(c.Request.Context(), service.ListFilter{
		BranchID:    branchID,
		SupplierID:  c.Query("supplier_id"),
		OnlyDefault: onlyDefault,
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	if rows == nil {
		rows = []model.BankAccount{}
	}
	c.JSON(http.StatusOK, gin.H{"items": rows, "total": len(rows)})
}

// Get GET /supplier-bank-accounts/:id
func (h *Handler) Get(c *gin.Context) {
	row, err := h.svc.Get(c.Request.Context(), branchIDOf(c), c.Param("id"))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, row)
}

// Create POST /supplier-bank-accounts
//
// scope: supplier:manage(**merchant 没有** ⇒ 只有 admin/显式授权的人能建)。
func (h *Handler) Create(c *gin.Context) {
	var req accountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体非法: "+err.Error())
		return
	}
	row, err := h.svc.Create(c.Request.Context(), branchIDOf(c), operatorOf(c), req.toInput())
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, row)
}

// Update PUT /supplier-bank-accounts/:id
func (h *Handler) Update(c *gin.Context) {
	var req accountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体非法: "+err.Error())
		return
	}
	row, err := h.svc.Update(c.Request.Context(), branchIDOf(c), c.Param("id"), operatorOf(c), req.toInput())
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, row)
}

// Delete DELETE /supplier-bank-accounts/:id
func (h *Handler) Delete(c *gin.Context) {
	if err := h.svc.Delete(c.Request.Context(), branchIDOf(c), c.Param("id"), operatorOf(c)); err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// SetDefault POST /supplier-bank-accounts/:id/default
func (h *Handler) SetDefault(c *gin.Context) {
	if err := h.svc.SetDefault(c.Request.Context(), branchIDOf(c), c.Param("id"), operatorOf(c)); err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
