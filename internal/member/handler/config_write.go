// Package handler / config_write.go —— 门店会员配置 + 会员类别的**写端点**。
//
// ⚠️ **守门方式与会员域的交易类写端点不同** ——
//
//	交易类(开卡/冲减积分/充值)走 service.Authorize:
//	  403 member_permission_required ⇒ POS 弹授权框(图4)
//	  403 member_permission_denied    ⇒ POS 弹图5 的 alert
//
//	配置类(本文件)走 **rbac.RequireScopeWithBranch**:
//	  无 scope ⇒ 403 forbidden(管理端弹个通用错误即可)
//
//	理由:配置是**管理员后台动作**,不是"收银员在终端上越权"。
//
// ⚠️ **门店永远取 X-Branch-ID**,body 里**没有** branch_id ——
//
//	⇒ 会员类别 / 会员参数天然按门店维护,管理端必须显式带操作门店。
//
// ⚠️ **金额与比率一律 string**(JSON number 到了客户端会变成 double)。
package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/member/model"
	"github.com/YunBright/supertrade/internal/member/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// settingsReq 是 PUT /config 的请求体。
//
// ⚠️ **指针 = 未传就保持原值**(局部更新)。
type settingsReq struct {
	MembershipEnabled *bool   `json:"membership_enabled"`
	PointsRate        *string `json:"points_rate"`
	PointsDeductRate  *string `json:"points_deduct_rate"`
	PasswordRequired  *bool   `json:"password_required"`
	FaceRequired      *bool   `json:"face_required"`
	MaxFacePerMember  *int    `json:"max_face_per_member"`
}

// typeReq 是 POST /types 与 PUT /types/:id 的请求体。
type typeReq struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	DiscountRate     *string `json:"discount_rate"`
	FeeAmountYuan    *string `json:"fee_amount_yuan"`
	LimitedUseCount  *int    `json:"limited_use_count"`
	AllowStoredValue *bool   `json:"allow_stored_value"`
	AllowPointsPay   *bool   `json:"allow_points_pay"`
	Priority         *int    `json:"priority"`
	Status           *string `json:"status"`
}

// ---- PUT /config ----

// updateConfig PUT /config —— 写门店会员配置。
func (h *Handler) updateConfig(c *gin.Context) {
	var req settingsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体格式错误")
		return
	}
	in := service.SettingsInput{
		BranchID:          middleware.SingleBranchFromCtx(c),
		MembershipEnabled: req.MembershipEnabled,
		PasswordRequired:  req.PasswordRequired,
		FaceRequired:      req.FaceRequired,
		MaxFacePerMember:  req.MaxFacePerMember,
		UpdatedBy:         callerSub(c),
	}
	// ⚠️ 比率解析失败 ⇒ 400(不能静默当 0 ——
	//	//	"把积分率设成 0" 与 "没传 points_rate" 完全是两件事)。
	if req.PointsRate != nil {
		d, err := decimal.NewFromString(strings.TrimSpace(*req.PointsRate))
		if err != nil {
			writeError(c, http.StatusBadRequest, "bad_request", "points_rate 不是合法数字")
			return
		}
		in.PointsRate = &d
	}
	if req.PointsDeductRate != nil {
		d, err := decimal.NewFromString(strings.TrimSpace(*req.PointsDeductRate))
		if err != nil {
			writeError(c, http.StatusBadRequest, "bad_request", "points_deduct_rate 不是合法数字")
			return
		}
		in.PointsDeductRate = &d
	}

	view, err := h.svc.UpsertSettings(c.Request.Context(), in)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

// ---- POST /types / PUT /types/:id ----

// createType POST /types —— 新建会员类别。
func (h *Handler) createType(c *gin.Context) {
	var req typeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体格式错误")
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		writeError(c, http.StatusBadRequest, "bad_request", "id 必填")
		return
	}
	row, ok := h.upsertType(c, req, strings.TrimSpace(req.ID))
	if !ok {
		return
	}
	c.JSON(http.StatusCreated, row)
}

// updateType PUT /types/:id —— 改会员类别(幂等 upsert)。
func (h *Handler) updateType(c *gin.Context) {
	var req typeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体格式错误")
		return
	}
	row, ok := h.upsertType(c, req, strings.TrimSpace(c.Param("id")))
	if !ok {
		return
	}
	c.JSON(http.StatusOK, row)
}

// upsertType 是两个写端点共用的落库逻辑(ok=false 表示已写过错误响应)。
func (h *Handler) upsertType(c *gin.Context, req typeReq, typeID string) (*model.MemberType, bool) {
	if typeID == "" {
		writeError(c, http.StatusBadRequest, "bad_request", "id 必填")
		return nil, false
	}
	in := service.TypeInput{
		ID:               typeID,
		BranchID:         middleware.SingleBranchFromCtx(c),
		Name:             strings.TrimSpace(req.Name),
		LimitedUseCount:  req.LimitedUseCount,
		AllowStoredValue: req.AllowStoredValue,
		AllowPointsPay:   req.AllowPointsPay,
		Priority:         req.Priority,
		Status:           req.Status,
		CreatedBy:        callerSub(c),
		UpdatedBy:        callerSub(c),
	}
	if req.DiscountRate != nil {
		d, err := decimal.NewFromString(strings.TrimSpace(*req.DiscountRate))
		if err != nil {
			writeError(c, http.StatusBadRequest, "bad_request", "discount_rate 不是合法数字")
			return nil, false
		}
		in.DiscountRate = &d
	}
	if req.FeeAmountYuan != nil {
		d, err := decimal.NewFromString(strings.TrimSpace(*req.FeeAmountYuan))
		if err != nil {
			writeError(c, http.StatusBadRequest, "bad_request", "fee_amount_yuan 不是合法数字")
			return nil, false
		}
		in.FeeAmountYuan = &d
	}
	row, err := h.svc.UpsertType(c.Request.Context(), in)
	if err != nil {
		mapErr(c, err)
		return nil, false
	}
	return row, true
}

// ---- DELETE /types/:id ----

// deleteType DELETE /types/:id —— 删会员类别(软删)。
func (h *Handler) deleteType(c *gin.Context) {
	err := h.svc.DeleteType(
		c.Request.Context(),
		middleware.SingleBranchFromCtx(c),
		strings.TrimSpace(c.Param("id")),
	)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
