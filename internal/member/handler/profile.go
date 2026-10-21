// Package handler / profile.go —— 会员档案的**编辑**端点与**软删**端点。
//
// 路由(nginx 已剥过 /api/v1/member/)：
//
//	PUT    /members/:id   局部改会员档案(身份字段)(走图4/图5 授权链路)
//	DELETE /members/:id   软删会员(member:config)
//
// # ⚠️ 为什么"改积分"在这个端点上是 400 而不是"被忽略"
//
//	积分 / 储值 / 消费 / 状态是本域 POS 业务的事实源,
//	只能走 POST /members/:id/points/grant 等专用端点(带各自的授权链路)。
//
//	若这里"收下但忽略",症状是"前端以为改了积分,其实没改"——
//	没有任何人会发现(静默忽略比报错危险)。
//	⇒ body 里出现这些列 ⇒ **直接 400**,并告诉调用方该用哪个端点。
package handler

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/member/model"
	"github.com/YunBright/supertrade/internal/member/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// forbiddenProfileFields 是**这个端点不接受**的列(积分/储值/消费/状态)。
//
// ⚠️ 它们是本域 POS 业务的事实源,只能走专用端点。
var forbiddenProfileFields = map[string]string{
	"points_accumulated":   "POST /members/{id}/points/grant 或 /points/deduct",
	"points_used":          "POST /members/{id}/points/deduct",
	"points_balance":       "POST /members/{id}/points/grant 或 /points/deduct",
	"remaining_points":     "POST /members/{id}/points/grant 或 /points/deduct",
	"balance_yuan":         "POST /members/{id}/account/recharge 或 /account/refund",
	"consumed_amount_yuan": "由销售流水累积,不能直接改",
	"consumed_count":       "由销售流水累积,不能直接改",
	"used_count":           "由使用次数累积,不能直接改",
	"status":               "POST /members/{id}/status",
}

// updateProfileReq 是 PUT /members/:id 的请求体。
//
// ⚠️ **指针 = 未传就保持原值**(局部更新语义,同 UpsertSettings / UpsertType)。
//
// ⚠️ **禁改的列不在这个结构体里** —— 类型上就不给改它们的机会;
//
//	handler 另外扫一遍原始 body,出现即 400(见 forbiddenProfileFields)。
type updateProfileReq struct {
	// OpID 是幂等键(必填)。
	OpID string `json:"op_id"`
	// Name 是姓名。
	Name *string `json:"name"`
	// Phone / Mobile 是电话 / 手机。
	Phone  *string `json:"phone"`
	Mobile *string `json:"mobile"`
	// Gender 是性别。
	Gender *string `json:"gender"`
	// Birthday 是生日(RFC3339)。空串 = 未传(保持原值)。
	Birthday string `json:"birthday"`
	// Favorite 是爱好。
	Favorite *string `json:"favorite"`
	// Remark 是备注。
	Remark *string `json:"remark"`
	// IDCardNo 是身份证。
	IDCardNo *string `json:"id_card_no"`
	// ValidFrom / ValidUntil 是有效期(RFC3339)。
	ValidFrom  string `json:"valid_from"`
	ValidUntil string `json:"valid_until"`
	// CardTypeID 是会员类别(必须存在且属于同一门店)。
	CardTypeID *string `json:"card_type_id"`
	// CardFaceNo 是卡面卡号。
	CardFaceNo *string `json:"card_face_no"`
	// AuthorizedBy 是越权授权人。
	AuthorizedBy string `json:"authorized_by"`
	// Authorization 是工号+口令(图4)。
	Authorization *authBlock `json:"authorization"`
}

// updateMember PUT /members/:id
//
// 守门:走本域的越权授权链路(service.Authorize),scope = member:config。
//
// ⚠️ 为什么用 member:config:改档案是**后台维护动作**(同会员类别/门店会员参数),
//
//	而 auth 的 seed 里 member:config 已存在且 admin 在门店10 已有
//	⇒ 不用改 auth 仓、不用加迁移。
func (h *Handler) updateMember(c *gin.Context) {
	// ⚠️ 先读原始 body:要同时做两件事 ——
	//   ① 反序列化进结构体;② 扫一遍"禁改列"是否出现。
	//   结构体里没有这些列 ⇒ 只看结构体是**发现不了**它们被传了。
	body, err := c.GetRawData()
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	var probe map[string]json.RawMessage
	if len(body) > 0 {
		if err := json.Unmarshal(body, &probe); err != nil {
			writeError(c, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
	}
	for field, hint := range forbiddenProfileFields {
		if _, ok := probe[field]; ok {
			writeError(c, http.StatusBadRequest, "bad_request",
				"该端点不允许修改 "+field+"("+hint+")")
			return
		}
	}

	var req updateProfileReq
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(c, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
	}

	authorizedBy, ok := h.authorize(c, model.ScopeConfig, "编辑会员档案", req.AuthorizedBy, req.Authorization)
	if !ok {
		return
	}
	row, err := h.svc.UpdateProfile(c.Request.Context(), service.UpdateProfileInput{
		OpID:         strings.TrimSpace(req.OpID),
		BranchID:     middleware.SingleBranchFromCtx(c),
		MemberID:     strings.TrimSpace(c.Param("id")),
		Name:         req.Name,
		Phone:        req.Phone,
		Mobile:       req.Mobile,
		Gender:       req.Gender,
		Birthday:     parseTimePtr(req.Birthday),
		Favorite:     req.Favorite,
		Remark:       req.Remark,
		IDCardNo:     req.IDCardNo,
		ValidFrom:    parseTimePtr(req.ValidFrom),
		ValidUntil:   parseTimePtr(req.ValidUntil),
		CardTypeID:   req.CardTypeID,
		CardFaceNo:   req.CardFaceNo,
		OperatorID:   callerSub(c),
		AuthorizedBy: authorizedBy,
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"member": row})
}

// deleteMember DELETE /members/:id
//
// ⚠️ **软删**(gorm.DeletedAt):行还在表里,但之后查不到
//
//	(历史单据仍能对上这张卡)。
//
// 守门:`member:config` scope(per-branch)—— 与 DELETE /types/:id 同档:
// 这是**管理员后台动作**,不是"收银员在终端上越权"⇒ 不必让 POS 弹授权框。
func (h *Handler) deleteMember(c *gin.Context) {
	branchID := strings.TrimSpace(middleware.SingleBranchFromCtx(c))
	if branchID == "" {
		writeError(c, http.StatusBadRequest, "branch_required", "X-Branch-ID header 必填")
		return
	}
	memberID := strings.TrimSpace(c.Param("id"))
	if err := h.svc.Delete(c.Request.Context(), branchID, memberID, callerSub(c)); err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"member_id": memberID,
		"deleted":   true,
	})
}
