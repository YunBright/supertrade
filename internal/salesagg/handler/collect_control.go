package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/salesagg/collect"
)

// CollectControlHandler 是「门店每日销售数据采集」的**开关**端点。
//
// # 为什么这些端点在**鉴权链内**，而采集端点在免鉴权窗口
//
//	采集端点 /sales/daily/collect —— dapr job 回调，**没有**用户 token，
//	                     身份由服务账号承担，所以挂免鉴权窗口。
//	开关端点 /sales/daily/collect/{enable,disable,state} —— 用户在 App 里点，
//	                     身份是**真人**，所以必须走鉴权链。
//
// 两者混在一个窗口里会出大问题：开关端点挂免鉴权窗口 ⇒ 任何人都能
// 给任意门店开/关采集；而采集端点挂鉴权链 ⇒ 每晚 22:45 必然 401。
// 路径前缀刻意相同（都是 /sales/daily/collect），是让"同一个资源的
// 写入口与自动执行入口"在 nginx 与运维眼里一眼可辨。
type CollectControlHandler struct {
	ctl *collect.Control
}

// NewCollectControlHandler 构造。
func NewCollectControlHandler(ctl *collect.Control) *CollectControlHandler {
	return &CollectControlHandler{ctl: ctl}
}

// RegisterRoutes 注册开关端点。
func (h *CollectControlHandler) RegisterRoutes(r gin.IRouter) {
	r.POST("/sales/daily/collect/enable", h.Enable)
	r.POST("/sales/daily/collect/disable", h.Disable)
	r.GET("/sales/daily/collect/state", h.State)
}

// enableReq 是开启请求体。
type enableReq struct {
	// Branch 是门店 id。
	//
	// ⚠️ **只从请求体取**，不从 X-Branch-ID 取 ——
	//	"关的是哪家店"必须由请求显式说明，而 X-Branch-ID 是**列表页筛选**用的
	//	（多店场景下它可能是"当前查看的那家"）。
	//	两者混用会出现"用户想关 A 店，结果关了当前查看的 B 店"。
	Branch string `json:"branch"`
	// Store 是 Branch 的别名（与采集端点一致，兼容两种叫法）。
	Store string `json:"store"`
}

// resolveBranch 合并请求体里的 branch / store。
func resolveBranch(body enableReq) string {
	if v := strings.TrimSpace(body.Branch); v != "" {
		return v
	}
	return strings.TrimSpace(body.Store)
}

// Enable POST /sales/daily/collect/enable
//
// 用**当前用户的 token** 代建服务账号并授权，然后落库并注册定时任务。
//
// ⚠️ 这里不做"用户是不是 admin"的判断 —— 那是 auth 侧的事：
//
//	权限判定必须由**持权方**做（auth 实查调用者在该门店的有效权限），
//	本服务自造一套判断只会出现两套口径，且其中一套必然是漏的。
//	本服务的职责是：把用户的 bearer 原样传下去，并把 auth 的错误**如实**带回。
func (h *CollectControlHandler) Enable(c *gin.Context) {
	if h.ctl == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"code":    "not_ready",
			"message": "采集开关未装配",
		})
		return
	}
	var body enableReq
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"code":    "bad_body",
				"message": "请求体不是合法 JSON：" + err.Error(),
			})
			return
		}
	}
	branch := resolveBranch(body)
	if branch == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    "branch_required",
			"message": "必须指定门店（body 的 branch）",
		})
		return
	}
	st, err := h.ctl.Enable(c.Request.Context(), c.GetHeader("Authorization"), branch, callerSubject(c))
	if err != nil {
		c.JSON(controlStatus(err), gin.H{
			"code":    controlCode(err),
			"message": err.Error(),
			"branch":  branch,
		})
		return
	}
	c.JSON(http.StatusOK, st)
}

// Disable POST /sales/daily/collect/disable
//
// 关停 = 禁用服务账号 + 删掉定时任务，**不删账号、不删凭据** ⇒ 重新开启无损。
func (h *CollectControlHandler) Disable(c *gin.Context) {
	if h.ctl == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"code":    "not_ready",
			"message": "采集开关未装配",
		})
		return
	}
	var body enableReq
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"code":    "bad_body",
				"message": "请求体不是合法 JSON：" + err.Error(),
			})
			return
		}
	}
	branch := resolveBranch(body)
	if branch == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    "branch_required",
			"message": "必须指定门店（body 的 branch）",
		})
		return
	}
	st, err := h.ctl.Disable(c.Request.Context(), c.GetHeader("Authorization"), branch)
	if err != nil {
		c.JSON(controlStatus(err), gin.H{
			"code":    controlCode(err),
			"message": err.Error(),
			"branch":  branch,
		})
		return
	}
	c.JSON(http.StatusOK, st)
}

// State GET /sales/daily/collect/state?branch=00
//
// 查当前状态。前端用它决定开关的初始位置，以及
// "要不要显示「去配置数据源」链接"。
func (h *CollectControlHandler) State(c *gin.Context) {
	if h.ctl == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"code":    "not_ready",
			"message": "采集开关未装配",
		})
		return
	}
	branch := strings.TrimSpace(c.Query("branch"))
	if branch == "" {
		branch = strings.TrimSpace(c.Query("store"))
	}
	if branch == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    "branch_required",
			"message": "必须指定门店（?branch=）",
		})
		return
	}
	st, err := h.ctl.State(c.Request.Context(), branch)
	if err != nil {
		c.JSON(controlStatus(err), gin.H{
			"code":    controlCode(err),
			"message": err.Error(),
			"branch":  branch,
		})
		return
	}
	c.JSON(http.StatusOK, st)
}

// controlStatus 把错误映射成状态码。
//
// # 为什么要把「数据源没配」与「权限不足」分开
//
//	409 + cube_source_not_configured → 前端弹「去配置数据源」链接
//	403 + forbidden                  → 前端弹「你没有权限」+ 重试
//	503 + ...                        → 前端弹「稍后重试」
//
// 三者混成 500 的话，前端只能显示一句"操作失败"，
// 而用户既不知道该去配数据源、也不知道自己缺权限。
func controlStatus(err error) int {
	switch {
	case errors.Is(err, collect.ErrCubeSourceNotConfigured), errors.Is(err, collect.ErrCubeSourceDisabled):
		return http.StatusConflict
	case errors.Is(err, collect.ErrServiceAccountExists):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

// controlCode 给前端一个稳定的分流依据。
//
// ⚠️ 文案（message）随时间会变，**分流逻辑必须只依赖 code**。
func controlCode(err error) string {
	switch {
	case errors.Is(err, collect.ErrCubeSourceNotConfigured):
		return "cube_source_not_configured"
	case errors.Is(err, collect.ErrCubeSourceDisabled):
		return "cube_source_disabled"
	case errors.Is(err, collect.ErrServiceAccountExists):
		return "service_account_exists"
	default:
		return "collect_control_failed"
	}
}

// callerSubject 取调用者标识（仅用于审计与 created_by）。
//
// 取不到就返回空串 —— 它**不参与**权限判定，
// 所以取不到不该让整个操作失败。
func callerSubject(c *gin.Context) string {
	if v := strings.TrimSpace(c.GetHeader("X-User-Id")); v != "" {
		return v
	}
	return ""
}
