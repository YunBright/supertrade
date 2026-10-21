// Package handler 实现 master-data 服务的 HTTP handler。
//
// 路由(nginx 已剥过 /api/v1/master-data/):
//
//	GET  /stores                  门店档案(store:view)
//	PUT  /stores/:branch_id       维护门店档案(store:manage)
//	GET  /employees?branch_id=    收银员档案(employee:view)
//	POST /employees               建收银员档案(employee:manage)
//	PUT  /employees/:id           改收银员档案(employee:manage)
//	POST /employees/:code/verify  越权授权:校验工号+口令,返该店长的 scope
//	GET  /shifts                  班次列表(shift:manage)
//	POST /shifts                  开班(shift:manage)
//	PUT  /shifts/:id              交班(shift:manage)
//
// # ⚠️ /employees/:code/verify **不需要管理 scope**,但**需要登录态**
//
// 它是 POS 越权授权链路的一环(图4/图5):
// 收银员已登录(持 JWT)→ 想做没权限的事 → POS 让**另一个有权限的人**(店长)
// 在终端上输入工号+口令 → 本端点校验口令并返回"这个人在当前门店有哪些 scope"。
//
// ⇒ 调用方是**已登录的收银员**,不是管理员 ⇒ 不能用 employee:manage 守门。
//
// ⚠️ 但它**必须**要求登录态(挂鉴权链内)——
//
//	它验口令 ⇒ 公开就等于给了一个口令爆破接口。
//	方案里写的是"公开(内部)",本实现取"**对内已登录即可**"这个更严的读法:
//	终端在 POS 场景下**总是**带着收银员的登录态,不需要匿名访问。
package handler

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/YunBright/authkit/rbac"
	"github.com/YunBright/authkit/userinfo"
	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/masterdata/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// Handler 是 master-data 的 HTTP handler 聚合。
type Handler struct {
	stores *service.StoreService
	emps   *service.EmployeeService
	shifts *service.ShiftService
	users  *userinfo.Client
	logger *slog.Logger
	// verifier 校验口令并取 scope(调 auth 的 /internal/users/{id}/verify-credentials)。
	//
	// ⚠️ 做成接口是为了能注入 fake:单测不连 auth。
	verifier CredentialVerifier
}

// CredentialVerifier 校验口令并返回该用户在某门店的 scope 集合。
type CredentialVerifier interface {
	VerifyCredentials(ctx context.Context, userID, password, branchID string) (*userinfo.VerifyCredentialsResult, error)
}

// New 构造 Handler。users 为 nil 时带 scope 守门的端点一律 503。
//
// ⚠️ [logger] 必须传 —— **nil 会让 mapErr 的兜底分支 panic**
// (`h.logger.Error(...)` 在 nil *slog.Logger 上 ⇒ nil pointer dereference)。
//
//	症状(2026-10-21 生产实测):授权框里输**错口令**
//	⇒ auth 返 400 ⇒ dapr 折成 InvalidArgument ⇒ authkit 归成泛化 error
//	⇒ 落到 mapErr 的 default 分支 ⇒ **panic** ⇒ 500
//	⇒ dapr 折成 Unknown ⇒ member 归成 503「授权校验暂不可用」
//	⇒ 图4/图5 的**时序整个错位**(该弹「你没有冲减积分的权限！」却弹了"服务不可用")。
//
//	⇒ logger 为 nil 时退回 slog.Default(),**任何情况下都不许 panic**。
func New(stores *service.StoreService, emps *service.EmployeeService, shifts *service.ShiftService,
	users *userinfo.Client, verifier CredentialVerifier, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		stores:   stores,
		emps:     emps,
		shifts:   shifts,
		users:    users,
		verifier: verifier,
		logger:   logger,
	}
}

// RegisterRoutes 挂载全部路由。
func (h *Handler) RegisterRoutes(r gin.IRouter) {
	resolver := rbac.HasAnyScopeWithBranch(h.users)

	r.GET("/stores",
		rbac.RequireScopeWithBranch("store:view", branchFromHeader, resolver),
		h.listStores)
	r.PUT("/stores/:branch_id",
		rbac.RequireScopeWithBranch("store:manage", branchFromHeader, resolver),
		h.upsertStore)

	r.GET("/employees",
		rbac.RequireScopeWithBranch("employee:view", branchFromHeader, resolver),
		h.listEmployees)
	r.POST("/employees",
		rbac.RequireScopeWithBranch("employee:manage", branchFromHeader, resolver),
		h.createEmployee)
	r.PUT("/employees/:id",
		rbac.RequireScopeWithBranch("employee:manage", branchFromHeader, resolver),
		h.updateEmployee)
	// ⚠️ **不挂 scope 守门** —— 它是收银员(非管理员)也会调的越权授权链路。
	//   但它**在鉴权链内**(cmdbootstrap 的全局验签 + RequireAudience),
	//   所以必须带一个有效 token。
	r.POST("/employees/:code/verify", h.verifyEmployee)

	r.GET("/shifts",
		rbac.RequireScopeWithBranch("shift:manage", branchFromHeader, resolver),
		h.listShifts)
	r.POST("/shifts",
		rbac.RequireScopeWithBranch("shift:manage", branchFromHeader, resolver),
		h.openShift)
	r.PUT("/shifts/:id",
		rbac.RequireScopeWithBranch("shift:manage", branchFromHeader, resolver),
		h.closeShift)
}

// branchFromHeader 取 X-Branch-ID(单店)。
func branchFromHeader(c *gin.Context) (string, bool) {
	b := middleware.SingleBranchFromCtx(c)
	if b == "" {
		return "", false
	}
	return b, true
}

// listStores GET /stores
func (h *Handler) listStores(c *gin.Context) {
	rows, err := h.stores.List(c.Request.Context())
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"stores": rows, "count": len(rows)})
}

// storeReq 是门店档案的写入请求体。
type storeReq struct {
	Code               string   `json:"code"`
	Name               string   `json:"name"`
	PosEnabled         bool     `json:"pos_enabled"`
	StocktakeCycleDays int      `json:"stocktake_cycle_days"`
	MemberEnabled      bool     `json:"member_enabled"`
	CounterNos         []string `json:"counter_nos"`
	Remark             string   `json:"remark"`
}

// upsertStore PUT /stores/:branch_id
//
// ⚠️ 门店 id **只从路径取**,不从 body 取 ——
//
//	body 里带一份门店,就能把 A 店的设置写进 B 店。
func (h *Handler) upsertStore(c *gin.Context) {
	branchID := strings.TrimSpace(c.Param("branch_id"))
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "bad_request", "message": "branch_id 不能为空",
		})
		return
	}
	var req storeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "bad_request", "message": err.Error(),
		})
		return
	}
	row, err := h.stores.Upsert(c.Request.Context(), service.StoreInput{
		BranchID:           branchID,
		Code:               req.Code,
		Name:               req.Name,
		PosEnabled:         req.PosEnabled,
		StocktakeCycleDays: req.StocktakeCycleDays,
		MemberEnabled:      req.MemberEnabled,
		CounterNos:         req.CounterNos,
		Remark:             req.Remark,
		UpdatedBy:          callerSub(c),
	})
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, row)
}

// listEmployees GET /employees?branch_id=
//
// ⚠️ **门店以 header(X-Branch-ID)为准,query 不能越权** ——
//
//	scope 守门已经按 header 的那家店判过了(用户必须**在那家店**有 employee:view)。
//	若再让 query 里的 branch_id 覆盖它 ⇒ 一个只有 A 店权限的账号
//	传 `?branch_id=B` 就能读到 B 店的收银员 ⇒ **越权读,且零报错**。
//
//	⇒ query 为空 ⇒ 用 header 的店(不再返回全部门店);
//	⇒ query 与 header 不一致 ⇒ 403。
//
//	这同时满足业务语义:**收银员是门店固定的**,
//	管理端要管别家店的收银员,就换一个 X-Branch-ID 过去。
func (h *Handler) listEmployees(c *gin.Context) {
	headerBranch := strings.TrimSpace(middleware.SingleBranchFromCtx(c))
	queryBranch := strings.TrimSpace(c.Query("branch_id"))
	if queryBranch != "" && queryBranch != headerBranch {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"code": "forbidden", "message": "只能查看当前门店的收银员",
		})
		return
	}
	rows, err := h.emps.List(c.Request.Context(), headerBranch)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"employees": rows, "count": len(rows)})
}

// employeeReq 是收银员档案的写入请求体。
type employeeReq struct {
	// AuthUserID 是 auth.users.id(**不是工号**)。
	AuthUserID string `json:"auth_user_id"`
	// EmployeeCode 是工号。
	EmployeeCode string `json:"employee_code"`
	Name         string `json:"name"`
	Role         string `json:"role"`
	Status       string `json:"status"`
	// DiscountRate 是折扣权限字符串(如 "0.95")。
	//
	// ⚠️ 用**字符串**而不是 number:JSON number 到了 Dart 侧会变 double,
	// 0.95 → 0.9499999999999996 ⇒ 折扣判定出错。
	DiscountRate string `json:"discount_rate"`
	CounterNo    string `json:"counter_no"`
	Remark       string `json:"remark"`
}

// createEmployee POST /employees
func (h *Handler) createEmployee(c *gin.Context) {
	var req employeeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "bad_request", "message": err.Error(),
		})
		return
	}
	branchID := middleware.SingleBranchFromCtx(c)
	rate, err := parseRate(req.DiscountRate)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	row, err := h.emps.Upsert(c.Request.Context(), service.EmployeeInput{
		AuthUserID:   req.AuthUserID,
		BranchID:     branchID,
		EmployeeCode: req.EmployeeCode,
		Name:         req.Name,
		Role:         req.Role,
		Status:       req.Status,
		DiscountRate: rate,
		CounterNo:    req.CounterNo,
		Remark:       req.Remark,
		UpdatedBy:    callerSub(c),
	})
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, row)
}

// updateEmployee PUT /employees/:id
func (h *Handler) updateEmployee(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "bad_request", "message": "id 不能为空",
		})
		return
	}
	var req employeeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "bad_request", "message": err.Error(),
		})
		return
	}
	branchID := middleware.SingleBranchFromCtx(c)
	rate, err := parseRate(req.DiscountRate)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	row, err := h.emps.Upsert(c.Request.Context(), service.EmployeeInput{
		AuthUserID:   id,
		BranchID:     branchID,
		EmployeeCode: req.EmployeeCode,
		Name:         req.Name,
		Role:         req.Role,
		Status:       req.Status,
		DiscountRate: rate,
		CounterNo:    req.CounterNo,
		Remark:       req.Remark,
		UpdatedBy:    callerSub(c),
	})
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, row)
}

// verifyReq 是越权授权的请求体。
type verifyReq struct {
	Password string `json:"password" binding:"required"`
}

// verifyEmployee POST /employees/:code/verify
//
// 返回:该店长在**当前门店**的 scope 集合。
//
// ⚠️ 失败一律 4xx,**不**返 200 + {verified:false} ——
//
//	那会让"忘了看返回值"的调用点默认放行(见 auth 侧的同款说明)。
func (h *Handler) verifyEmployee(c *gin.Context) {
	code := strings.TrimSpace(c.Param("code"))
	branchID := middleware.SingleBranchFromCtx(c)
	if code == "" || branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "bad_request", "message": "工号与门店都不能为空",
		})
		return
	}
	var req verifyReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "bad_request", "message": err.Error(),
		})
		return
	}
	if h.verifier == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code": "not_ready", "message": "凭据校验未装配",
		})
		return
	}
	// ① 工号 → auth user_id。
	emp, err := h.emps.GetByCode(c.Request.Context(), branchID, code)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	// ② 校验口令 + 取该店长的 scope。
	res, err := h.verifier.VerifyCredentials(c.Request.Context(), emp.ID, req.Password, branchID)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"verified":      res.Verified,
		"branch_id":     res.BranchID,
		"employee_code": emp.EmployeeCode,
		"name":          emp.Name,
		"role":          emp.Role,
		"scopes":        res.Scopes,
	})
}

// listShifts GET /shifts
func (h *Handler) listShifts(c *gin.Context) {
	rows, err := h.shifts.ListOpen(c.Request.Context(), c.Query("branch_id"))
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"shifts": rows, "count": len(rows)})
}

// openShiftReq 是开班的请求体。
type openShiftReq struct {
	EmployeeID string `json:"employee_id" binding:"required"`
	CounterNo  string `json:"counter_no"`
	// OpeningAmountYuan 是备用金(字符串)。
	OpeningAmountYuan string `json:"opening_amount_yuan"`
	Remark            string `json:"remark"`
}

// openShift POST /shifts
func (h *Handler) openShift(c *gin.Context) {
	var req openShiftReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "bad_request", "message": err.Error(),
		})
		return
	}
	branchID := middleware.SingleBranchFromCtx(c)
	row, err := h.shifts.Open(c.Request.Context(), service.OpenShiftInput{
		BranchID:          branchID,
		EmployeeID:        req.EmployeeID,
		CounterNo:         req.CounterNo,
		OpeningAmountYuan: parseAmount(req.OpeningAmountYuan),
		Remark:            req.Remark,
		UpdatedBy:         callerSub(c),
	})
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, row)
}

// closeShiftReq 是交班的请求体。
type closeShiftReq struct {
	// ClosingAmountYuan 是实收(含备用金)(字符串)。
	ClosingAmountYuan string `json:"closing_amount_yuan"`
	Remark            string `json:"remark"`
}

// closeShift PUT /shifts/:id
func (h *Handler) closeShift(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "bad_request", "message": "id 不能为空",
		})
		return
	}
	var req closeShiftReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "bad_request", "message": err.Error(),
		})
		return
	}
	row, err := h.shifts.Close(c.Request.Context(), id, parseAmount(req.ClosingAmountYuan), req.Remark, callerSub(c))
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, row)
}
