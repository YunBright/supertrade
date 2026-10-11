// Package handler 实现 settlement-auditor 服务的 HTTP handler(Gin)。
//
// 端点:
//
//	POST   /settlement-approvals                 提交付款审批(202,异步审核)   scope settlement:audit
//	GET    /settlement-approvals                 列表(?status&from&to)          scope settlement:view
//	GET    /settlement-approvals/:id             详情(实例+快照+信号+结论+绑定) scope settlement:view
//	GET    /settlement-approvals/:id/logs        推理日志                       scope settlement:view
//	POST   /settlement-approvals/:id/retry       failed 重跑(202)               scope settlement:audit
//	GET    /settlement-approvals/export          打印对账 HTML(?from&to)        scope settlement:view
//	GET    /settlement-approvals/cube/settlements 可勾选结算单(cube 透传)       scope settlement:view
//
// 错误映射:ErrApprovalNotFound→404,ErrInvalidInput/ErrInvalidStatus→400,
// ErrCubeUnavailable→503 cube_unavailable,ErrUserInfoUnavailable→503 userd_unavailable。
package handler

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"time"

	"github.com/YunBright/authkit/claims"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/settlement-auditor/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// Handler 是 settlement-auditor 的 HTTP handler 聚合。
type Handler struct {
	svc *service.Service
}

// New 构造 handler。
func New(svc *service.Service) *Handler {
	return &Handler{svc: svc}
}

// RegisterRoutes 注册业务路由(不接管 /healthz 与 dapr 订阅端点)。
//
// X-Branch-ID 由 cmd main 的 middleware.XBranchID() 注入 ctx;
// 这里每路由挂 RequireBranch 拦截缺失。
func (h *Handler) RegisterRoutes(r gin.IRouter) {
	requireBranch := middleware.RequireBranch()

	r.POST("/settlement-approvals", requireBranch, h.Submit)
	r.GET("/settlement-approvals", requireBranch, h.List)
	r.GET("/settlement-approvals/export", requireBranch, h.Export)
	r.GET("/settlement-approvals/cube/settlements", requireBranch, h.ListCubeSettlements)
	r.GET("/settlement-approvals/approver-preference", requireBranch, h.GetApproverPreference)
	r.GET("/settlement-approvals/:id", requireBranch, h.Get)
	r.GET("/settlement-approvals/:id/logs", requireBranch, h.GetLogs)
	r.POST("/settlement-approvals/:id/retry", requireBranch, h.Retry)
}

// ---- 错误响应 ----

type errBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, errBody{Code: code, Message: msg})
}

func mapErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrApprovalNotFound):
		writeError(c, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, service.ErrInvalidInput), errors.Is(err, service.ErrInvalidStatus):
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, service.ErrCubeUnavailable):
		writeError(c, http.StatusServiceUnavailable, "cube_unavailable", err.Error())
	case errors.Is(err, service.ErrUserInfoUnavailable):
		writeError(c, http.StatusServiceUnavailable, "userd_unavailable", err.Error())
	default:
		writeError(c, http.StatusInternalServerError, "internal_error", err.Error())
	}
}

// requireScope per-branch 守门(仿 fresh-meat)。
func (h *Handler) requireScope(c *gin.Context, branchID, scope string) bool {
	cl, ok := claims.FromContext(c.Request.Context())
	if !ok || cl == nil {
		writeError(c, http.StatusUnauthorized, "unauthenticated", "缺少已签 token")
		return false
	}
	if branchID == "" {
		writeError(c, http.StatusBadRequest, "branch_required", "branch_id 必填")
		return false
	}
	allowed, err := h.svc.HasEffectiveScope(c.Request.Context(), cl.Sub, branchID, scope)
	if err != nil {
		mapErr(c, err)
		return false
	}
	if !allowed {
		writeError(c, http.StatusForbidden, "forbidden", "用户在该 branch 下无 "+scope+" scope")
		return false
	}
	return true
}

// ---- 提交 ----

type submitReq struct {
	ApplicantWecomID string   `json:"applicant_wecom_id" binding:"required"`
	SettlementIDs    []string `json:"settlement_ids" binding:"required"`
	ManualAmountYuan string   `json:"manual_amount_yuan" binding:"required"`
	Remark           string   `json:"remark"`
	// ApproverUserIDs 审批人企微 userid(原生选人器选定,至少 1 人)。
	ApproverUserIDs []string `json:"approver_userids" binding:"required"`
	// CcUserIDs 抄送人(可空)。
	CcUserIDs []string `json:"cc_userids"`
}

// Submit POST /settlement-approvals → 202 {id,status}
func (h *Handler) Submit(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "settlement:audit") {
		return
	}
	var req submitReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体非法: "+err.Error())
		return
	}
	amount, err := decimal.NewFromString(req.ManualAmountYuan)
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "manual_amount_yuan 必须是十进制数字字符串")
		return
	}
	cl, _ := claims.FromContext(c.Request.Context())
	ap, err := h.svc.SubmitApproval(c.Request.Context(), branchID, service.SubmitInput{
		ApplicantUserID:  cl.Sub,
		ApplicantWecomID: req.ApplicantWecomID,
		ApplicantBearer:  c.Request.Header.Get("Authorization"),
		SettlementIDs:    req.SettlementIDs,
		ManualAmountYuan: amount,
		Remark:           req.Remark,
		ApproverUserIDs:  req.ApproverUserIDs,
		CcUserIDs:        req.CcUserIDs,
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"id": ap.ID, "status": ap.Status})
}

// GetApproverPreference GET /settlement-approvals/approver-preference
//
// 发起页带出该用户上次使用的审批人/抄送人(对齐官方审批"记住上次"体验)。
func (h *Handler) GetApproverPreference(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "settlement:view") {
		return
	}
	cl, _ := claims.FromContext(c.Request.Context())
	pref, err := h.svc.GetApproverPreference(c.Request.Context(), branchID, cl.Sub)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, pref)
}

// ---- 查询 ----

// List GET /settlement-approvals?status&from&to&limit&offset
func (h *Handler) List(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "settlement:view") {
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	items, total, err := h.svc.ListApprovals(c.Request.Context(), branchID,
		c.Query("status"), c.Query("from"), c.Query("to"), limit, offset)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": total})
}

// Get GET /settlement-approvals/:id
func (h *Handler) Get(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "settlement:view") {
		return
	}
	d, err := h.svc.GetApprovalDetail(c.Request.Context(), branchID, c.Param("id"))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, d)
}

// GetLogs GET /settlement-approvals/:id/logs
func (h *Handler) GetLogs(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "settlement:view") {
		return
	}
	logs, err := h.svc.GetLogs(c.Request.Context(), branchID, c.Param("id"))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": logs})
}

// Retry POST /settlement-approvals/:id/retry → 202
func (h *Handler) Retry(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "settlement:audit") {
		return
	}
	// 先按 branch 校验归属,再重跑
	if _, err := h.svc.GetApprovalDetail(c.Request.Context(), branchID, c.Param("id")); err != nil {
		mapErr(c, err)
		return
	}
	if err := h.svc.RetryApproval(c.Request.Context(), c.Param("id"),
		c.Request.Header.Get("Authorization")); err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"ok": true})
}

// ListCubeSettlements GET /settlement-approvals/cube/settlements?from&to&supplier_id
//
// H5 勾选数据源:透传 cube 已审核采购结款单(DP/CP)。
func (h *Handler) ListCubeSettlements(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "settlement:view") {
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "200"))
	rows, err := h.svc.ListCubeSettlements(c.Request.Context(), branchID,
		c.Query("from"), c.Query("to"), c.Query("supplier_id"), limit)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": rows})
}

// ---- 打印对账 ----

var exportTpl = template.Must(template.New("export").Parse(`<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>结算付款审批对账单</title>
<style>
body{font-family:"Microsoft YaHei",sans-serif;margin:24px;font-size:12px}
h1{font-size:18px}table{border-collapse:collapse;width:100%;margin-top:12px}
th,td{border:1px solid #333;padding:6px 8px;text-align:left}
th{background:#eee}.num{text-align:right}.sig{min-width:120px}
.total{font-weight:bold}.meta{color:#555;margin-top:4px}
@media print{.noprint{display:none}}
</style></head><body>
<h1>结算付款审批对账单</h1>
<div class="meta">门店:{{.BranchID}} &nbsp; 时间段:{{.From}} ~ {{.To}} &nbsp; 导出时间:{{.Now}}</div>
<table><thead><tr>
<th>提交时间</th><th>审批单号</th><th>企微单号</th><th>结算单(金额)</th>
<th class="num">手工付款金额</th><th>状态</th><th>申请人签名</th><th>财务确认签名</th>
</tr></thead><tbody>
{{range .Rows}}<tr>
<td>{{.SubmittedAt}}</td><td>{{.ID}}</td><td>{{.SpNo}}</td>
<td>{{range .Settlements}}{{.SettlementID}}({{.GoodsAmountYuan}})<br>{{end}}</td>
<td class="num">{{.ManualAmountYuan}}</td><td>{{.Status}}</td>
<td class="sig"></td><td class="sig"></td>
</tr>{{end}}
<tr class="total"><td colspan="4">合计({{.Count}} 笔)</td>
<td class="num">{{.TotalAmount}}</td><td colspan="3"></td></tr>
</tbody></table>
<p class="meta">说明:本单用于付款前纸质签名确认与留存,与总账号余额对账。</p>
</body></html>`))

type exportViewRow struct {
	SubmittedAt      string
	ID               string
	SpNo             string
	Settlements      []exportSettlement
	ManualAmountYuan string
	Status           string
}

type exportSettlement struct {
	SettlementID    string
	GoodsAmountYuan string
}

// Export GET /settlement-approvals/export?from&to → text/html(打印视图)
func (h *Handler) Export(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "settlement:view") {
		return
	}
	from, to := c.Query("from"), c.Query("to")
	rows, err := h.svc.ExportApprovals(c.Request.Context(), branchID, from, to)
	if err != nil {
		mapErr(c, err)
		return
	}
	total := decimal.Zero
	view := make([]exportViewRow, 0, len(rows))
	for _, r := range rows {
		vr := exportViewRow{
			SubmittedAt:      r.Approval.CreatedAt.Format("2006-01-02 15:04"),
			ID:               r.Approval.ID,
			SpNo:             r.Approval.SpNo,
			ManualAmountYuan: r.Approval.ManualAmountYuan.StringFixed(2),
			Status:           r.Approval.Status,
		}
		total = total.Add(r.Approval.ManualAmountYuan)
		for _, st := range r.Settlements {
			vr.Settlements = append(vr.Settlements, exportSettlement{
				SettlementID:    st.SettlementID,
				GoodsAmountYuan: st.GoodsAmountYuan.StringFixed(2),
			})
		}
		view = append(view, vr)
	}
	data := map[string]any{
		"BranchID": branchID, "From": from, "To": to,
		"Now":  time.Now().Format("2006-01-02 15:04:05"),
		"Rows": view, "Count": len(view),
		"TotalAmount": total.StringFixed(2),
	}
	c.Header("Content-Type", "text/html; charset=utf-8")
	if err := exportTpl.Execute(c.Writer, data); err != nil {
		writeError(c, http.StatusInternalServerError, "internal_error", fmt.Sprintf("render export: %v", err))
	}
}
