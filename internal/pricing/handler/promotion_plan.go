// Package handler / promotion_plan.go —— 促销方案(图4)的 HTTP 端点。
//
// 路由(nginx 已剥过 /api/v1/pricing/):
//
//	GET /promotion-plans?from=&to=&plan_no=&plan_name=&approve_status=
//	                  &created_by=&member_level=&exclude_expired=true
//	                  &sort=&order=&page=&page_size=
//	    促销方案列表(pricing:view,per branch)
//
//	    sort / order 是**排序**:sort 取白名单字段
//	    (created_at / plan_no / plan_name / begin_date / end_date /
//	    created_by / approved_at / stopped_at),order 取 asc / desc。
//	    不传 ⇒ 默认「制单日期倒序」。非法取值 ⇒ **400**(不静默回落)。
//	GET /promotion-plans/:plan_no
//	    促销方案详情(pricing:view,per branch)
//
// # ⚠️ 本轮**只读**
//
// PLAN §3.3 明写「只读(列表 + 详情)」⇒ 这里不注册任何 POST / PUT / DELETE。
//
// # ⚠️ 分流判据与 pricing 的其它端点**完全相同**
// (internal/cubesource:配了源 ⇒ cube;没配 ⇒ 本域 PG;问不到 ⇒ 503)。
package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/pricing/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// listPromotionPlans GET /promotion-plans
func (h *Handler) listPromotionPlans(c *gin.Context) {
	if h.plans == nil {
		// ⚠️ 装配漏了 ⇒ **503**,不是空列表:
		// 空列表会被读成"这家店没有促销方案"。
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code": "not_ready", "message": "促销方案服务未装配",
		})
		return
	}
	branchID := middleware.SingleBranchFromCtx(c)
	q := service.PromotionPlanQuery{
		From:          strings.TrimSpace(c.Query("from")),
		To:            strings.TrimSpace(c.Query("to")),
		PlanNo:        strings.TrimSpace(c.Query("plan_no")),
		PlanName:      strings.TrimSpace(c.Query("plan_name")),
		ApproveStatus: strings.TrimSpace(c.Query("approve_status")),
		CreatedBy:     strings.TrimSpace(c.Query("created_by")),
		MemberLevel:   strings.TrimSpace(c.Query("member_level")),
		// ⚠️ 排序:空 = 后端默认「制单日期倒序」;非法字段/方向 ⇒ **400**
		// (静默回落 = 用户点了排序、列表没变、零提示)。
		Sort:  strings.TrimSpace(c.Query("sort")),
		Order: strings.TrimSpace(c.Query("order")),
		// ⚠️ exclude_expired 默认 **true**:促销方案是时效数据,
		// 2021 年的方案和今天的方案混在一张列表里没法用。
		// 显式传 false 才放开。
		ExcludeExpired: true,
		Page:           1,
		PageSize:       50,
	}
	// ⚠️ exclude_expired 默认 **true**:促销方案是时效数据,
	// 2021 年的方案和今天的方案混在一张列表里没法用。
	// 显式传 false 才放开。
	if v := c.Query("exclude_expired"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			q.ExcludeExpired = b
		}
	}
	if v := c.Query("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			q.Page = n
		}
	}
	if v := c.Query("page_size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			q.PageSize = n
		}
	}
	page, err := h.plans.List(c.Request.Context(), branchID, q)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, page)
}

// getPromotionPlan GET /promotion-plans/:plan_no
//
// ⚠️ 响应体**就是那一行**(PLAN §3.3 的"列表行"结构),
// 不额外包一层 {"plan": {...}} —— 前端按固定字段解析,少一层包装少一处可能漂移。
func (h *Handler) getPromotionPlan(c *gin.Context) {
	if h.plans == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code": "not_ready", "message": "促销方案服务未装配",
		})
		return
	}
	branchID := middleware.SingleBranchFromCtx(c)
	planNo := strings.TrimSpace(c.Param("plan_no"))
	if planNo == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "bad_request", "message": "方案编号 plan_no 必填",
		})
		return
	}
	item, err := h.plans.Get(c.Request.Context(), branchID, planNo)
	if err != nil {
		if errors.Is(err, service.ErrPromotionPlanNotFound) {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
				"code": "promotion_plan_not_found", "message": err.Error(),
			})
			return
		}
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, item)
}
