// Package handler 实现 member 服务的 HTTP handler。
//
// 路由(nginx 已剥过 /api/v1/member/):
//
//	GET  /permissions                     当前工号在本门店的 scope 集合(member:view)
//	GET  /config                          门店会员配置(member:view)
//	PUT  /config                          写门店会员配置(member:config)
//	GET  /types                           会员类别(member:view)
//	POST /types                           建会员类别(member:config)
//	PUT  /types/:id                       改会员类别(member:config)
//	DELETE /types/:id                     删会员类别(软删)(member:config)
//	GET  /payment-ways                    付款方式字典(member:view)
//	GET  /members?field=&q=&limit=&offset=  会员查询(member:view;响应含 count + total)
//	GET  /members/export                   离线会员包(member:view)—— 不分页,全店
//	GET  /members/:id                     会员卡信息(图3)(member:view)
//	GET  /members/:id/points              积分汇总 + 流水(member:view)
//	GET  /members/:id/account             储值余额 + 流水(member:view)
//	GET  /members/:id/consume             消费汇总(member:view)
//	GET  /members/:id/last-order          上一单(图7)(member:view)
//	GET  /members/:id/recharge-card       充值卡信息(图2 F3)(member:view)
//	GET  /members/:id/stored-value-card   一卡通/储值卡信息(图2 F7)(member:view)
//	POST /members                         开卡(member:card-open)
//	PUT  /members/:id                     局部改会员档案(身份字段)(走图4/图5 授权链路)
//	DELETE /members/:id                   软删会员(member:config)
//	POST /import                          导入会员档案(归档同步)(member:config)
//	POST /members/:id/points/deduct       冲减积分(member:point-deduct)
//	POST /members/:id/points/grant        赠分(member:point-manage)
//	POST /members/:id/account/recharge     储值充值(member:account-recharge)
//	POST /members/:id/account/refund       退储(member:account-recharge)
//	POST /members/:id/status              冻结/解冻/挂失/解挂(member:status-manage)
//	POST /members/:id/password            改密/重置(member:password-manage)
//	POST /members/:id/face                人脸登记(member:face-manage)
//
// # ⚠️ 读端点与写端点的守门方式**不同**(这是会员域的核心不变量)
//
// 读端点用 `rbac.RequireScopeWithBranch(scope, branchFromHeader, resolver)`
// —— 无权限 ⇒ 403 forbidden。
//
// 写端点**不能**用它:会员域要区分两种 403(图4 / 图5):
//
//	403 member_permission_required  当前收银员没有<操作名>权限，请输入授权
//	                                (+ required_scope)⇒ POS 弹授权框
//	403 member_permission_denied    你没有<操作名>的权限！⇒ POS 弹图5 的 alert
//
// ⇒ 写端点走 service.Authorize(见 service/authorization.go)。
package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/YunBright/authkit/rbac"
	"github.com/YunBright/authkit/userinfo"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/member/model"
	"github.com/YunBright/supertrade/internal/member/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// Handler 是 member 服务的 HTTP handler 聚合。
type Handler struct {
	svc    *service.Service
	users  *userinfo.Client
	logger *slog.Logger
}

// New 构造 Handler。
//
// ⚠️ users 为 nil 时:读端点由 rbac 中间件返 403(forbidden),
// 写端点由 service.Authorize 返 403 member_permission_required —— 都是 fail-closed。
func New(svc *service.Service, users *userinfo.Client, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{svc: svc, users: users, logger: logger}
}

// RegisterRoutes 挂载全部路由。
func (h *Handler) RegisterRoutes(r gin.IRouter) {
	resolver := rbac.HasAnyScopeWithBranch(h.users)
	// service 侧的 per-branch scope 判定器(写端点的授权链路用它)。
	//
	// ⚠️ 只在**没注入**时设默认值:单测会先注入 fake checker。
	if h.svc.ScopeChecker() == nil {
		h.svc.SetScopeChecker(userInfoScopeChecker{users: h.users})
	}

	// ---- 读端点:标准 scope 守门 ----
	//
	// ⚠️ /permissions 是"我的权限"查询:用 member:view 守门即可
	//   (登录用户都能查"我在这个店有什么权限")。
	//   ⚠️ 它**只读**:不改任何状态,也不需要 body。
	r.GET("/permissions",
		rbac.RequireScopeWithBranch(model.ScopeView, branchFromHeader, resolver),
		h.getPermissions)
	r.GET("/config",
		rbac.RequireScopeWithBranch(model.ScopeView, branchFromHeader, resolver),
		h.getConfig)
	r.GET("/types",
		rbac.RequireScopeWithBranch(model.ScopeView, branchFromHeader, resolver),
		h.listTypes)
	r.GET("/payment-ways",
		rbac.RequireScopeWithBranch(model.ScopeView, branchFromHeader, resolver),
		h.listPaymentWays)
	r.GET("/members",
		rbac.RequireScopeWithBranch(model.ScopeView, branchFromHeader, resolver),
		h.listMembers)
	// 离线会员包(一次给全,不分页)。
	//
	// ⚠️ 静态段 `/members/export` 与参数段 `/members/:id` **共存**:
	// gin 的路由树里静态段优先于参数段(与注册顺序无关)——
	// 这条依赖 gin 版本,必须有测试锁住
	// (见 handler/export_test.go 的 TestMembersExportRouteWinsOverParam)。
	r.GET("/members/export",
		rbac.RequireScopeWithBranch(model.ScopeView, branchFromHeader, resolver),
		h.exportMembers)
	r.GET("/members/:id",
		rbac.RequireScopeWithBranch(model.ScopeView, branchFromHeader, resolver),
		h.getMember)
	r.GET("/members/:id/points",
		rbac.RequireScopeWithBranch(model.ScopeView, branchFromHeader, resolver),
		h.getMemberPoints)
	r.GET("/members/:id/account",
		rbac.RequireScopeWithBranch(model.ScopeView, branchFromHeader, resolver),
		h.getMemberAccount)
	r.GET("/members/:id/consume",
		rbac.RequireScopeWithBranch(model.ScopeView, branchFromHeader, resolver),
		h.getMemberConsume)
	r.GET("/members/:id/last-order",
		rbac.RequireScopeWithBranch(model.ScopeView, branchFromHeader, resolver),
		h.getMemberLastOrder)
	r.GET("/members/:id/recharge-card",
		rbac.RequireScopeWithBranch(model.ScopeView, branchFromHeader, resolver),
		h.getRechargeCard)
	r.GET("/members/:id/stored-value-card",
		rbac.RequireScopeWithBranch(model.ScopeView, branchFromHeader, resolver),
		h.getStoredValueCard)

	// ---- 配置类写端点:标准 scope 守门(不走图4/图5 的授权链路) ----
	//
	// ⚠️ 这几个是**管理员后台动作**(改门店会员参数 / 会员类别),
	//   不是"收银员在终端上越权"⇒ 用 rbac 的 403 forbidden 即可,
	//   不必让 POS 弹授权框。
	//
	// ⚠️ 会员类别也归 `member:config` 管(不另开 scope):
	//   auth 的 permissions seed 里 `member:config` 已存在且已给 admin。
	r.PUT("/config",
		rbac.RequireScopeWithBranch(model.ScopeConfig, branchFromHeader, resolver),
		h.updateConfig)
	r.POST("/types",
		rbac.RequireScopeWithBranch(model.ScopeConfig, branchFromHeader, resolver),
		h.requireLocalSource,
		h.createType)
	r.PUT("/types/:id",
		rbac.RequireScopeWithBranch(model.ScopeConfig, branchFromHeader, resolver),
		h.requireLocalSource,
		h.updateType)
	r.DELETE("/types/:id",
		rbac.RequireScopeWithBranch(model.ScopeConfig, branchFromHeader, resolver),
		h.requireLocalSource,
		h.deleteType)

	// ---- 归档导入端点:标准 scope 守门 ----
	//
	// ⚠️ 导入是**运维的后台批量动作**(一次性建档),不是收银员动作
	//   ⇒ 用 member:config(与会员类别/门店会员参数同档),不弹授权框。
	r.POST("/import",
		rbac.RequireScopeWithBranch(model.ScopeConfig, branchFromHeader, resolver),
		h.requireLocalSource,
		h.importMembers)

	// ---- 会员档案的编辑 / 删除 ----
	//
	// ⚠️ PUT /members/:id 是**写端点** ⇒ 走本域的越权授权链路(见下),
	//   不是 rbac.RequireScopeWithBranch(它只会返 403 forbidden,
	//   POS 分不出"要弹授权框"与"授权不通过")。
	r.PUT("/members/:id", h.requireLocalSource, h.updateMember)
	// ⚠️ DELETE 是**管理员后台动作**(同 DELETE /types/:id)⇒ 标准 scope 守门。
	r.DELETE("/members/:id",
		rbac.RequireScopeWithBranch(model.ScopeConfig, branchFromHeader, resolver),
		h.requireLocalSource,
		h.deleteMember)

	// ---- 写端点:会员域自己的授权链路(图4 / 图5) ----
	//
	// ⚠️ **不挂 rbac.RequireScopeWithBranch**:它只会返 403 forbidden,
	//   而 POS 需要区分"要弹授权框"(member_permission_required)
	//   与"授权不通过"(member_permission_denied)。
	//
	// ⚠️ requireLocalSource:该门店的会员档案来自**外部系统**(富百家 / 佳乐)
	//   ⇒ 本域只读,写操作一律拒(409)。
	//   不拒的话:写进本域 PG 的一行,前台显示"写成功了",
	//   而那个门店的事实源是外部 ERP ⇒ 这行数据永远没人看。
	r.POST("/members", h.requireLocalSource, h.openMember)
	r.POST("/members/:id/points/deduct", h.requireLocalSource, h.deductPoints)
	r.POST("/members/:id/points/grant", h.requireLocalSource, h.grantPoints)
	r.POST("/members/:id/account/recharge", h.requireLocalSource, h.rechargeAccount)
	r.POST("/members/:id/account/refund", h.requireLocalSource, h.refundAccount)
	r.POST("/members/:id/status", h.requireLocalSource, h.updateStatus)
	r.POST("/members/:id/password", h.requireLocalSource, h.updatePassword)
	r.POST("/members/:id/face", h.requireLocalSource, h.updateFace)
}

// requireLocalSource 拦住"该门店的会员档案来自外部系统"的写操作。
//
// ⚠️ 放在**写端点的第一层**:不然后面那些写方法会在本域 PG 里
// 写一行"没人看的会员",而前台还显示成功。
func (h *Handler) requireLocalSource(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		// 门店头缺失由 rbac 中间件判 400,这里不重复处理。
		c.Next()
		return
	}
	if err := h.svc.EnsureLocalSource(c.Request.Context(), branchID); err != nil {
		mapErr(c, err)
		c.Abort()
		return
	}
	c.Next()
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

// userInfoScopeChecker 把 *userinfo.Client 适配成 service.ScopeChecker。
//
// ⚠️ 走的是 **per-branch 权限矩阵**(userd /internal/users/{id}/permissions
//
//	?branch_id=…),⛔ 不是 JWT 里的 scopes(本体系恒为空数组)。
//
// ⚠️ 用户没绑定这家店 ⇒ userd 返回的矩阵里**没有这一行**
//
//	⇒ 在该店**没有任何 scope**(连全局角色权限也没有)。
type userInfoScopeChecker struct {
	users *userinfo.Client
}

// HasScope 实现 service.ScopeChecker。
func (c userInfoScopeChecker) HasScope(ctx context.Context, userID, branchID, scope string) (bool, error) {
	if c.users == nil {
		return false, errors.New("member: userd 客户端未装配")
	}
	bp, err := c.users.GetBranchPermissions(ctx, userID, branchID)
	if err != nil {
		return false, err
	}
	for _, row := range bp.Branches {
		if branchID != "" && row.BranchID != branchID {
			continue
		}
		for _, s := range row.Scopes {
			if s == scope {
				return true, nil
			}
		}
	}
	return false, nil
}

// ---- 读端点 ----

// getConfig GET /config
func (h *Handler) getConfig(c *gin.Context) {
	view, err := h.svc.GetSettings(c.Request.Context(), middleware.SingleBranchFromCtx(c))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

// listTypes GET /types
func (h *Handler) listTypes(c *gin.Context) {
	rows, err := h.svc.ListTypes(c.Request.Context(), middleware.SingleBranchFromCtx(c))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"member_types": rows, "count": len(rows)})
}

// listPaymentWays GET /payment-ways
func (h *Handler) listPaymentWays(c *gin.Context) {
	rows, err := h.svc.ListPaymentWays(c.Request.Context(), middleware.SingleBranchFromCtx(c))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"payment_ways": rows, "count": len(rows)})
}

// listMembers GET /members?field=&q=&limit=&offset=&sort=&order=
//
// ⚠️ 响应里**同时**给 count 与 total:
//
//	count = 本次返回的行数(旧语义,不变)
//	total = 该门店 + 该查询条件下的**总命中行数**
//
// ⇒ 门店10 导入后有 4128 个会员,而 limit 上限是 200
// ⇒ 只给 count 的话前端会以为"这个店就这么点会员"(静默截断)。
//
// ⚠️ sort / order 是**服务端排序**(不是前端排):
//
//	前端排只能排当前这一页 ⇒ "按积分排序"翻到第 2 页就乱序了。
//	排序列只认白名单(model.MemberSortColumn)，非法值 ⇒ 400。
func (h *Handler) listMembers(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	if offset < 0 {
		offset = 0
	}
	rows, total, err := h.svc.ListMembersPage(c.Request.Context(), service.ListMembersInput{
		BranchID: middleware.SingleBranchFromCtx(c),
		Field:    c.Query("field"),
		Q:        c.Query("q"),
		Limit:    limit,
		Offset:   offset,
		Sort:     c.Query("sort"),
		Order:    c.Query("order"),
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"members": rows,
		"count":   len(rows),
		"total":   total,
	})
}

// exportMemberDTO 是离线会员包里的一行。
//
// # ⚠️ 字段名与 model.Member 的 json tag **逐字对齐**
//
//	id / branch_id / name / card_face_no / card_type_id / phone / mobile /
//	status / points_accumulated / points_used / remaining_points /
//	balance_yuan / updated_at
//
// 两处用同一个词的地方含义必须一样,否则客户端按 model.Member 的字段名
// 解析这个响应会静默取到空值。
//
// # ⚠️ 金额/积分一律 decimal.Decimal ⇒ JSON **字符串**
//
// JSON number 到了 Dart 侧会被 jsonDecode 变成 double,精度当场丢掉;
// 字符串则原样落地。
type exportMemberDTO struct {
	ID string `json:"id"`
	// BranchID 是门店(复合主键的第二列,卡号**非全局唯一**)。
	BranchID string `json:"branch_id"`
	// Name 是姓名。
	Name string `json:"name"`
	// CardFaceNo 是卡面卡号。
	CardFaceNo string `json:"card_face_no"`
	// CardTypeID 是会员类别 → member_types.id。
	CardTypeID string `json:"card_type_id"`
	// Phone 是电话号码。
	Phone string `json:"phone"`
	// Mobile 是手机号码。
	Mobile string `json:"mobile"`
	// Status 是状态:normal / frozen / lost / cancelled。
	Status string `json:"status"`
	// PointsAccumulated 是**累计获得**积分。
	PointsAccumulated decimal.Decimal `json:"points_accumulated"`
	// PointsUsed 是**累计扣减**积分。
	PointsUsed decimal.Decimal `json:"points_used"`
	// RemainingPoints 是**剩余积分(派生)**= 累计获得 − 累计扣减。
	//
	// ⚠️ 由 service 的 fillDerived 填(gorm:"-" 不落库)——
	// 忘了填 ⇒ 客户端拿到的全是 0 ⇒ 算积分、抵扣积分全错。
	RemainingPoints decimal.Decimal `json:"remaining_points"`
	// BalanceYuan 是零钱包余额(储值卡余额)。
	BalanceYuan decimal.Decimal `json:"balance_yuan"`
	// UpdatedAt 是最后修改时刻(RFC3339,与 model.Member 的 JSON 编码一致)。
	UpdatedAt time.Time `json:"updated_at"`
}

// exportMembers GET /members/export —— 离线会员包(一次给全)。
//
// # ⚠️ 与 listMembers 的差别
//
//	listMembers  limit 上限 200(MaxListLimit)⇒ 4128 人的门店要 21 次往返
//	exportMembers **不限条数**,一次给全
//
// # ⚠️ 门店只从 X-Branch-ID header 取
//
// query / body 里**不认**门店 —— 否则 A 店的登录态能把 B 店的会员导走。
func (h *Handler) exportMembers(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		writeError(c, http.StatusBadRequest, "branch_required", "X-Branch-ID header 必填")
		return
	}
	rows, err := h.svc.ExportMembers(c.Request.Context(), branchID)
	if err != nil {
		mapErr(c, err)
		return
	}
	out := make([]exportMemberDTO, 0, len(rows))
	for _, m := range rows {
		out = append(out, exportMemberDTO{
			ID:                m.ID,
			BranchID:          m.BranchID,
			Name:              m.Name,
			CardFaceNo:        m.CardFaceNo,
			CardTypeID:        m.CardTypeID,
			Phone:             m.Phone,
			Mobile:            m.Mobile,
			Status:            m.Status,
			PointsAccumulated: m.PointsAccumulated,
			PointsUsed:        m.PointsUsed,
			RemainingPoints:   m.RemainingPoints,
			BalanceYuan:       m.BalanceYuan,
			UpdatedAt:         m.UpdatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"branch_id": branchID,
		"members":   out,
		"count":     len(out),
		"total":     len(out),
	})
}

// getMember GET /members/:id(图3)
func (h *Handler) getMember(c *gin.Context) {
	row, err := h.svc.Get(c.Request.Context(), middleware.SingleBranchFromCtx(c), c.Param("id"))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, row)
}

// getMemberPoints GET /members/:id/points
func (h *Handler) getMemberPoints(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	memberID := c.Param("id")
	summary, err := h.svc.PointsSummaryOf(c.Request.Context(), branchID, memberID)
	if err != nil {
		mapErr(c, err)
		return
	}
	rows, err := h.svc.ListPoints(c.Request.Context(), service.ListPointsInput{
		BranchID: branchID,
		MemberID: memberID,
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"points":        summary,
		"points_ledger": rows,
		"count":         len(rows),
	})
}

// getMemberAccount GET /members/:id/account
func (h *Handler) getMemberAccount(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	memberID := c.Param("id")
	summary, err := h.svc.AccountSummaryOf(c.Request.Context(), branchID, memberID)
	if err != nil {
		mapErr(c, err)
		return
	}
	rows, err := h.svc.ListAccount(c.Request.Context(), service.ListAccountInput{
		BranchID: branchID,
		MemberID: memberID,
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"account":        summary,
		"account_ledger": rows,
		"count":          len(rows),
	})
}

// getMemberConsume GET /members/:id/consume
func (h *Handler) getMemberConsume(c *gin.Context) {
	view, err := h.svc.ConsumeSummaryOf(c.Request.Context(), middleware.SingleBranchFromCtx(c), c.Param("id"))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

// getMemberLastOrder GET /members/:id/last-order(图7)
func (h *Handler) getMemberLastOrder(c *gin.Context) {
	view, err := h.svc.LastOrderOf(c.Request.Context(), middleware.SingleBranchFromCtx(c), c.Param("id"))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

// getRechargeCard GET /members/:id/recharge-card(图2 F3)
func (h *Handler) getRechargeCard(c *gin.Context) {
	view, err := h.svc.RechargeCardOf(c.Request.Context(), middleware.SingleBranchFromCtx(c), c.Param("id"))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

// getStoredValueCard GET /members/:id/stored-value-card(图2 F7)
func (h *Handler) getStoredValueCard(c *gin.Context) {
	view, err := h.svc.StoredValueCardOf(c.Request.Context(), middleware.SingleBranchFromCtx(c), c.Param("id"))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

// ---- 写端点 ----

// authBlock 是写请求里携带的越权授权信息(图4 的工号 + 口令)。
type authBlock struct {
	EmployeeCode string `json:"employee_code"`
	Password     string `json:"password"`
}

// toService 转成 service.Authorization。
func (a *authBlock) toService() *service.Authorization {
	if a == nil || strings.TrimSpace(a.EmployeeCode) == "" || strings.TrimSpace(a.Password) == "" {
		return nil
	}
	return &service.Authorization{EmployeeCode: a.EmployeeCode, Password: a.Password}
}

// authorize 判定当前操作人能否执行该写操作(会员域的图4 / 图5 链路)。
//
// 返回值:通过 ⇒ (授权人, true);不通过 ⇒ 已写好响应,false。
func (h *Handler) authorize(c *gin.Context, scope, feature string, authorizedBy string, auth *authBlock) (string, bool) {
	res, err := h.svc.Authorize(c.Request.Context(), service.AuthorizeInput{
		OperatorID:    callerSub(c),
		BranchID:      middleware.SingleBranchFromCtx(c),
		Scope:         scope,
		Feature:       feature,
		AuthorizedBy:  authorizedBy,
		Authorization: auth.toService(),
	})
	if err != nil {
		mapErr(c, err)
		return "", false
	}
	return res.AuthorizedBy, true
}

// openMemberReq 是开卡请求体(图6)。
type openMemberReq struct {
	// OpID 是幂等键(必填)。
	OpID string `json:"op_id"`
	// ID 是会员号;空 ⇒ 服务端生成。
	ID string `json:"id"`
	// CardTypeID 是会员类别(必填)。
	CardTypeID string `json:"card_type_id"`
	// CardFaceNo 是卡面卡号。
	CardFaceNo string `json:"card_face_no"`
	// Name 是姓名(必填)。
	Name string `json:"name"`
	// Phone / Mobile 是电话 / 手机。
	Phone  string `json:"phone"`
	Mobile string `json:"mobile"`
	// Gender 是性别。
	Gender string `json:"gender"`
	// Birthday 是生日(RFC3339)。
	Birthday string `json:"birthday"`
	// CalendarType 是历法(公历/农历)。
	CalendarType string `json:"calendar_type"`
	// ValidFrom / ValidUntil 是有效期(RFC3339)。
	ValidFrom  string `json:"valid_from"`
	ValidUntil string `json:"valid_until"`
	// Favorite 是爱好。
	Favorite string `json:"favorite"`
	// IDCardNo 是身份证。
	IDCardNo string `json:"id_card_no"`
	// Remark 是备注。
	Remark string `json:"remark"`
	// FeeAmountYuan 是会员费(字符串)。
	FeeAmountYuan string `json:"fee_amount_yuan"`
	// LimitedUseCount 是限用次数(0 = 不限次)。
	LimitedUseCount int `json:"limited_use_count"`
	// Password 是会员口令(明文,服务内哈希)。
	Password string `json:"password"`
	// FaceID 是面部特征。
	FaceID string `json:"face_id"`
	// OpenID 是微信/支付宝 openid。
	OpenID string `json:"open_id"`
	// AuthorizedBy 是越权授权人。
	AuthorizedBy string `json:"authorized_by"`
	// Authorization 是工号+口令(图4)。
	Authorization *authBlock `json:"authorization"`
}

// openMember POST /members(开卡)
func (h *Handler) openMember(c *gin.Context) {
	var req openMemberReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	authorizedBy, ok := h.authorize(c, model.ScopeCardOpen, "开卡", req.AuthorizedBy, req.Authorization)
	if !ok {
		return
	}
	row, err := h.svc.OpenCard(c.Request.Context(), service.OpenCardInput{
		OpID:            strings.TrimSpace(req.OpID),
		BranchID:        middleware.SingleBranchFromCtx(c),
		ID:              strings.TrimSpace(req.ID),
		CardTypeID:      strings.TrimSpace(req.CardTypeID),
		CardFaceNo:      strings.TrimSpace(req.CardFaceNo),
		Name:            strings.TrimSpace(req.Name),
		Phone:           strings.TrimSpace(req.Phone),
		Mobile:          strings.TrimSpace(req.Mobile),
		Gender:          strings.TrimSpace(req.Gender),
		Birthday:        parseTimePtr(req.Birthday),
		CalendarType:    strings.TrimSpace(req.CalendarType),
		ValidFrom:       parseTimePtr(req.ValidFrom),
		ValidUntil:      parseTimePtr(req.ValidUntil),
		Favorite:        strings.TrimSpace(req.Favorite),
		IDCardNo:        strings.TrimSpace(req.IDCardNo),
		Remark:          strings.TrimSpace(req.Remark),
		FeeAmountYuan:   parseAmount(req.FeeAmountYuan),
		LimitedUseCount: req.LimitedUseCount,
		Password:        req.Password,
		FaceID:          strings.TrimSpace(req.FaceID),
		OpenID:          strings.TrimSpace(req.OpenID),
		OperatorID:      callerSub(c),
		AuthorizedBy:    authorizedBy,
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"member": row})
}

// pointsReq 是积分写请求体(赠分 / 冲减)。
type pointsReq struct {
	// OpID 是幂等键(必填)。
	OpID string `json:"op_id"`
	// Points 是积分数(字符串)。
	Points string `json:"points"`
	// Reason 是原因。
	Reason string `json:"reason"`
	// RefFlowNo 是关联单号。
	RefFlowNo string `json:"ref_flow_no"`
	// AuthorizedBy 是越权授权人。
	AuthorizedBy string `json:"authorized_by"`
	// Authorization 是工号+口令(图4)。
	Authorization *authBlock `json:"authorization"`
}

// deductPoints POST /members/:id/points/deduct(冲减积分,图2 F4)
func (h *Handler) deductPoints(c *gin.Context) {
	var req pointsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	authorizedBy, ok := h.authorize(c, model.ScopePointDeduct, "冲减积分", req.AuthorizedBy, req.Authorization)
	if !ok {
		return
	}
	res, err := h.svc.DeductPoints(c.Request.Context(), service.DeductPointsInput{
		OpID:         strings.TrimSpace(req.OpID),
		BranchID:     middleware.SingleBranchFromCtx(c),
		MemberID:     strings.TrimSpace(c.Param("id")),
		Points:       parseAmount(req.Points),
		Reason:       strings.TrimSpace(req.Reason),
		RefFlowNo:    strings.TrimSpace(req.RefFlowNo),
		OperatorID:   callerSub(c),
		AuthorizedBy: authorizedBy,
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// grantPoints POST /members/:id/points/grant(赠分)
func (h *Handler) grantPoints(c *gin.Context) {
	var req pointsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	authorizedBy, ok := h.authorize(c, model.ScopePointManage, "赠分", req.AuthorizedBy, req.Authorization)
	if !ok {
		return
	}
	res, err := h.svc.GrantPoints(c.Request.Context(), service.GrantPointsInput{
		OpID:         strings.TrimSpace(req.OpID),
		BranchID:     middleware.SingleBranchFromCtx(c),
		MemberID:     strings.TrimSpace(c.Param("id")),
		Points:       parseAmount(req.Points),
		Reason:       strings.TrimSpace(req.Reason),
		OperatorID:   callerSub(c),
		AuthorizedBy: authorizedBy,
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// accountReq 是储值写请求体(充值 / 退储)。
type accountReq struct {
	// OpID 是幂等键(必填)。
	OpID string `json:"op_id"`
	// AmountYuan 是金额(字符串)。
	AmountYuan string `json:"amount_yuan"`
	// Reason 是原因。
	Reason string `json:"reason"`
	// AuthorizedBy 是越权授权人。
	AuthorizedBy string `json:"authorized_by"`
	// Authorization 是工号+口令(图4)。
	Authorization *authBlock `json:"authorization"`
}

// rechargeAccount POST /members/:id/account/recharge(储值充值)
func (h *Handler) rechargeAccount(c *gin.Context) {
	var req accountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	authorizedBy, ok := h.authorize(c, model.ScopeAccountRecharge, "储值充值", req.AuthorizedBy, req.Authorization)
	if !ok {
		return
	}
	res, err := h.svc.Recharge(c.Request.Context(), service.AccountOpInput{
		OpID:         strings.TrimSpace(req.OpID),
		BranchID:     middleware.SingleBranchFromCtx(c),
		MemberID:     strings.TrimSpace(c.Param("id")),
		AmountYuan:   parseAmount(req.AmountYuan),
		Reason:       strings.TrimSpace(req.Reason),
		OperatorID:   callerSub(c),
		AuthorizedBy: authorizedBy,
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// refundAccount POST /members/:id/account/refund(退储)
func (h *Handler) refundAccount(c *gin.Context) {
	var req accountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	authorizedBy, ok := h.authorize(c, model.ScopeAccountRecharge, "退储", req.AuthorizedBy, req.Authorization)
	if !ok {
		return
	}
	res, err := h.svc.Refund(c.Request.Context(), service.AccountOpInput{
		OpID:         strings.TrimSpace(req.OpID),
		BranchID:     middleware.SingleBranchFromCtx(c),
		MemberID:     strings.TrimSpace(c.Param("id")),
		AmountYuan:   parseAmount(req.AmountYuan),
		Reason:       strings.TrimSpace(req.Reason),
		OperatorID:   callerSub(c),
		AuthorizedBy: authorizedBy,
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// statusReq 是改会员状态的请求体。
type statusReq struct {
	// OpID 是幂等键(必填)。
	OpID string `json:"op_id"`
	// Status 是目标状态(normal / frozen / lost / cancelled)。
	Status string `json:"status"`
	// AuthorizedBy 是越权授权人。
	AuthorizedBy string `json:"authorized_by"`
	// Authorization 是工号+口令(图4)。
	Authorization *authBlock `json:"authorization"`
}

// updateStatus POST /members/:id/status(冻结/解冻/挂失/解挂)
func (h *Handler) updateStatus(c *gin.Context) {
	var req statusReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	authorizedBy, ok := h.authorize(c, model.ScopeStatusManage, "冻结/解冻", req.AuthorizedBy, req.Authorization)
	if !ok {
		return
	}
	row, err := h.svc.UpdateStatus(c.Request.Context(), service.UpdateStatusInput{
		OpID:         strings.TrimSpace(req.OpID),
		BranchID:     middleware.SingleBranchFromCtx(c),
		MemberID:     strings.TrimSpace(c.Param("id")),
		Status:       strings.TrimSpace(req.Status),
		OperatorID:   callerSub(c),
		AuthorizedBy: authorizedBy,
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"member": row})
}

// passwordReq 是改密 / 重置口令的请求体。
type passwordReq struct {
	// OpID 是幂等键(必填)。
	OpID string `json:"op_id"`
	// Password 是新口令。
	Password string `json:"password"`
	// OldPassword 是原口令(改密时必填;重置时可空)。
	OldPassword string `json:"old_password"`
	// AuthorizedBy 是越权授权人。
	AuthorizedBy string `json:"authorized_by"`
	// Authorization 是工号+口令(图4)。
	Authorization *authBlock `json:"authorization"`
}

// updatePassword POST /members/:id/password(改密/重置)
func (h *Handler) updatePassword(c *gin.Context) {
	var req passwordReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	authorizedBy, ok := h.authorize(c, model.ScopePasswordManage, "修改密码", req.AuthorizedBy, req.Authorization)
	if !ok {
		return
	}
	row, err := h.svc.UpdatePassword(c.Request.Context(), service.UpdatePasswordInput{
		OpID:         strings.TrimSpace(req.OpID),
		BranchID:     middleware.SingleBranchFromCtx(c),
		MemberID:     strings.TrimSpace(c.Param("id")),
		Password:     req.Password,
		OldPassword:  req.OldPassword,
		OperatorID:   callerSub(c),
		AuthorizedBy: authorizedBy,
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"member": row})
}

// faceReq 是人脸特征登记的请求体。
type faceReq struct {
	// OpID 是幂等键(必填)。
	OpID string `json:"op_id"`
	// FaceID 是面部特征标识。
	FaceID string `json:"face_id"`
	// AuthorizedBy 是越权授权人。
	AuthorizedBy string `json:"authorized_by"`
	// Authorization 是工号+口令(图4)。
	Authorization *authBlock `json:"authorization"`
}

// updateFace POST /members/:id/face(人脸登记)
func (h *Handler) updateFace(c *gin.Context) {
	var req faceReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	authorizedBy, ok := h.authorize(c, model.ScopeFaceManage, "人脸登记", req.AuthorizedBy, req.Authorization)
	if !ok {
		return
	}
	row, err := h.svc.UpdateFace(c.Request.Context(), service.UpdateFaceInput{
		OpID:         strings.TrimSpace(req.OpID),
		BranchID:     middleware.SingleBranchFromCtx(c),
		MemberID:     strings.TrimSpace(c.Param("id")),
		FaceID:       strings.TrimSpace(req.FaceID),
		OperatorID:   callerSub(c),
		AuthorizedBy: authorizedBy,
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"member": row})
}

// parseTimePtr 解析 RFC3339 时间串(空 ⇒ nil)。
func parseTimePtr(s string) *time.Time {
	t := strings.TrimSpace(s)
	if t == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, t)
	if err != nil {
		slog.Warn("member: 时间串解析失败,按空处理", "value", t, "err", err)
		return nil
	}
	return &parsed
}
