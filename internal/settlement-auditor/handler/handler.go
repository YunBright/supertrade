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
// 初始化记账(财务批量把历史已付款结算单补录成已付款,PR 见 service/pay_state.go):
//
//	GET    /settlement-init/records              台账审计列表                   scope settlement:view
//	POST   /settlement-init/preview              回显影响面(不落库)             scope settlement:audit
//	POST   /settlement-init/apply                批量补录(设置方式=init)        scope settlement:audit
//	POST   /settlement-init/revert                撤销补录                       scope settlement:audit
//
// 错误映射:ErrApprovalNotFound→404,ErrInvalidInput/ErrInvalidStatus→400,
// ErrApprovalInFlight/ErrResultTruncated→409,ErrCubeUnavailable→503
// cube_unavailable,ErrUserInfoUnavailable→503 userd_unavailable。
package handler

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
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
	// ⚠️ 必须注册在 /:id 之前(见 GetApprovalFlow 的注释)。
	r.GET("/settlement-approvals/approval-flow", requireBranch, h.GetApprovalFlow)
	r.GET("/settlement-approvals/:id", requireBranch, h.Get)
	r.GET("/settlement-approvals/:id/logs", requireBranch, h.GetLogs)
	r.POST("/settlement-approvals/:id/retry", requireBranch, h.Retry)
	// 物理删一次审批及其 5 张从表（数据治理 / 测试清理）。
	// 流水线在途时会返 409 让调用方稍后重试 —— 详见 service.ErrApprovalInFlight。
	r.DELETE("/settlement-approvals/:id", requireBranch, h.Delete)

	// ---- 初始化记账(财务把历史已付款结算单批量补录成已付款) ----
	//
	// 独立前缀而不是挂在 /settlement-approvals 下：那一族已经有一个 `:id`
	// 参数段,再往里塞静态段只会让"路由注册顺序决定谁先命中"变成隐式契约。
	// 读(scope settlement:view)与写(scope settlement:audit)分开。
	r.GET("/settlement-init/records", requireBranch, h.ListPayStates)
	r.POST("/settlement-init/preview", requireBranch, h.PreviewInit)
	r.POST("/settlement-init/apply", requireBranch, h.ApplyInit)
	r.POST("/settlement-init/revert", requireBranch, h.RevertInit)

	// ---- 门店级「盘点计划推送群」配置 ----
	//
	// 运维在 Flutter 管理后台「仓库-设置」里选定门店后配置:
	// GET 读当前配置(未配置返 404,前端翻成空态),
	// PUT upsert,DELETE 清掉(回到"不推送到群")。
	r.GET("/stocktake-notify-group", requireBranch, h.GetStocktakeNotifyGroup)
	r.PUT("/stocktake-notify-group", requireBranch, h.SetStocktakeNotifyGroup)
	r.DELETE("/stocktake-notify-group", requireBranch, h.DeleteStocktakeNotifyGroup)
}

// stocktakeNotifyGroupResp 是推送群配置的响应体。
type stocktakeNotifyGroupResp struct {
	BranchID  string     `json:"branch_id"`
	ChatID    string     `json:"chat_id"`
	ChatName  string     `json:"chat_name"`
	AppID     string     `json:"app_id"`
	AppName   string     `json:"app_name"`
	UpdatedBy string     `json:"updated_by,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// GetStocktakeNotifyGroup GET /stocktake-notify-group
//
// 未配置 ⇒ 404(前端翻成"还没设"空态),与"读失败"区分开。
// ⚠️ 404 / 500 都**直接 c.JSON** 写 {code,message},不走 writeError ⇒ 不 abort 中间件链;
// body 形状与 writeError 一致(都是 code + message)。
func (h *Handler) GetStocktakeNotifyGroup(c *gin.Context) {
	branchID := h.branchIDOf(c)
	row, err := h.svc.GetStocktakeNotifyGroup(c.Request.Context(), branchID)
	if errors.Is(err, service.ErrNotifyGroupNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"code": "not_found", "message": "该门店尚未设置盘点计划推送群"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": "internal_error", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, stocktakeNotifyGroupResp{
		BranchID: row.BranchID, ChatID: row.ChatID, ChatName: row.ChatName,
		AppID: row.AppID, AppName: row.AppName, UpdatedBy: row.UpdatedBy, UpdatedAt: &row.UpdatedAt,
	})
}

// SetStocktakeNotifyGroup PUT /stocktake-notify-group
//
// ⚠️ **不做 per-branch scope 校验**(与本服务其它写端点不同):只按 X-Branch-ID
// 定位门店,不查 effective scopes。
func (h *Handler) SetStocktakeNotifyGroup(c *gin.Context) {
	var req struct {
		ChatID   string `json:"chat_id"`
		ChatName string `json:"chat_name"`
		AppID    string `json:"app_id"`
		AppName  string `json:"app_name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "bad_request", "message": "请求体非法: " + err.Error()})
		return
	}
	operator := ""
	if cl, ok := claims.FromContext(c.Request.Context()); ok && cl != nil {
		operator = cl.Sub
	}
	row, err := h.svc.SetStocktakeNotifyGroup(c.Request.Context(), h.branchIDOf(c), service.SetStocktakeNotifyGroupInput{
		ChatID: req.ChatID, ChatName: req.ChatName, AppID: req.AppID, AppName: req.AppName,
	}, operator)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "bad_request", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, stocktakeNotifyGroupResp{
		BranchID: row.BranchID, ChatID: row.ChatID, ChatName: row.ChatName,
		AppID: row.AppID, AppName: row.AppName, UpdatedBy: row.UpdatedBy, UpdatedAt: &row.UpdatedAt,
	})
}

// DeleteStocktakeNotifyGroup DELETE /stocktake-notify-group
//
// ⚠️ **不做 per-branch scope 校验**(与本服务其它写端点不同)。
// 幂等:未配置过也返 200 {"ok":true}(删 0 行不算错)。
func (h *Handler) DeleteStocktakeNotifyGroup(c *gin.Context) {
	if err := h.svc.DeleteStocktakeNotifyGroup(c.Request.Context(), h.branchIDOf(c)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": "internal_error", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// branchIDOf 取本次请求的门店 id(X-Branch-ID 由中间件注入 ctx)。
func (h *Handler) branchIDOf(c *gin.Context) string {
	if v := middleware.SingleBranchFromCtx(c); v != "" {
		return v
	}
	return strings.TrimSpace(c.GetHeader("X-Branch-ID"))
}

// settlementInitReq 是初始化记账三个写端点共用的请求体。
//
// 主体是**筛选条件**而不是 id 列表 —— 一次可能涉及上千张单,而客户端只看得见
// 自己加载过的那几页。集合由服务端解析(见 service.ResolveSettlementSet),
// preview 先回显"将影响 N 张、合计 ¥X",apply 才落库。
//
// 唯一的例外是 SettlementIDs(显式勾选):「勾这几张」与「全选搜索结果」是
// 两个不同意图,只支持后者的话界面上的复选框就成了装饰。
type settlementInitReq struct {
	From          string   `json:"from"` // 账期结束日 [from,to)
	To            string   `json:"to"`
	SupplierID    string   `json:"supplier_id"`    // 精确匹配
	Query         string   `json:"query"`          // 单号/供应商包含匹配
	DocumentType  string   `json:"document_type"`  // 空 = DP+CP
	PayStatus     string   `json:"pay_status"`     // unpaid / paid / all
	SettlementIDs []string `json:"settlement_ids"` // 显式勾选;非空时与筛选结果取交集
	Limit         int      `json:"limit"`          // 只影响回显样本,不影响落库集合
	Note          string   `json:"note"`           // 补录备注(apply 用)
}

// filterOf 收敛成 service 层的筛选条件。
func (r settlementInitReq) filterOf() service.SettlementFilter {
	return service.SettlementFilter{
		From:         r.From,
		To:           r.To,
		SupplierID:   r.SupplierID,
		Query:        r.Query,
		DocumentType: r.DocumentType,
		PayStatus:    r.PayStatus,
		Ids:          r.SettlementIDs,
		Limit:        r.Limit,
	}
}

// bindInitReq 绑定并校验请求体。
func bindInitReq(c *gin.Context) (settlementInitReq, bool) {
	var req settlementInitReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体非法: "+err.Error())
		return req, false
	}
	return req, true
}

// operatorOf 取"设置人"= 当前登录管理员。
//
// 取 claims.Sub(auth user id),显示名尽力解析 —— 服务端不信任客户端自称的
// operator,否则审计字段等于谁都能填。
func (h *Handler) operatorOf(c *gin.Context) service.PayStateOperator {
	cl, _ := claims.FromContext(c.Request.Context())
	if cl == nil || cl.Sub == "" {
		return service.PayStateOperator{}
	}
	return service.PayStateOperator{
		UserID: cl.Sub,
		Name:   h.svc.ResolveOperatorName(c.Request.Context(), cl.Sub),
	}
}

// PreviewInit POST /settlement-init/preview → 200 {count,total_goods_yuan,...}
//
// 只读:解析筛选条件并回显影响面,**不写任何数据**。前端必须先调它弹确认框。
func (h *Handler) PreviewInit(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "settlement:audit") {
		return
	}
	req, ok := bindInitReq(c)
	if !ok {
		return
	}
	pv, err := h.svc.PreviewSettlementInit(c.Request.Context(), branchID, req.filterOf())
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, pv)
}

// ApplyInit POST /settlement-init/apply → 200 {matched,changed,already_paid,...}
//
// 设置方式恒为 init(常量在 service 层,请求体没有这个字段);
// 设置人恒为当前登录管理员。集合触到拉取上限时 409,拒绝执行。
func (h *Handler) ApplyInit(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "settlement:audit") {
		return
	}
	req, ok := bindInitReq(c)
	if !ok {
		return
	}
	res, err := h.svc.ApplySettlementInit(c.Request.Context(), branchID,
		req.filterOf(), h.operatorOf(c), req.Note)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// RevertInit POST /settlement-init/revert → 200
//
// 撤销初始化:只退回 set_method=init 且仍为 paid 的单,保留原设置人并记录撤销人。
func (h *Handler) RevertInit(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "settlement:audit") {
		return
	}
	req, ok := bindInitReq(c)
	if !ok {
		return
	}
	res, err := h.svc.RevertSettlementInit(c.Request.Context(), branchID,
		req.filterOf(), h.operatorOf(c))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// ListPayStates GET /settlement-init/records?method&pay_status&supplier_id&limit&offset
func (h *Handler) ListPayStates(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "settlement:view") {
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	rows, total, err := h.svc.ListPayStateRecords(c.Request.Context(), branchID,
		c.Query("method"), c.Query("pay_status"), c.Query("supplier_id"), limit, offset)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": rows, "total": total})
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
	case errors.Is(err, service.ErrApprovalInFlight):
		// 409 + 明确的重试指引。
		// 不用 400:这不是请求写错了,客户端改任何参数都没用 ——
		// 只能等流水线自己跑完。400 会让清理脚本把它当成参数错误而放弃,
		// 或者 endless 重试。
		writeError(c, http.StatusConflict, "approval_in_flight", err.Error())
	case errors.Is(err, service.ErrResultTruncated):
		// 同样是 409 而不是 400:请求本身合法,但**照原样重试永远不会成功**,
		// 必须收窄筛选条件。批量写入下尤其不能放行 —— 预览说"影响 800 张"、
		// 实际只处理 500 张,剩下 300 张既没标记也没跳过,事后对不上账。
		writeError(c, http.StatusConflict, "result_truncated", err.Error())
	case errors.Is(err, service.ErrCubeUnavailable):
		writeError(c, http.StatusServiceUnavailable, "cube_unavailable", err.Error())
	case errors.Is(err, service.ErrUserInfoUnavailable):
		// ⚠️ 不透出 err.Error():错误含 gRPC 码与内部 app-id,对门店无意义;细节留服务端日志。
		writeError(c, http.StatusServiceUnavailable, "userd_unavailable",
			"权限服务暂时不可用，请稍后重试")
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
	// FlowSelections 是按**节点类型**分桶的人员选择
	// (键 approver/handler/cc,值 企微 userid 列表)。
	//
	// 节点集合由后端的审批流程配置决定(= 企微管理后台里那个模板的流程设置),
	// 所以模板加一个节点(比如办理人)协议不用改。
	//
	// ⚠️ 不能加 binding:"required" —— 键是动态的,required 只对固定字段有意义。
	FlowSelections map[string][]string `json:"flow_selections"`
	// ApproverUserIDs / HandlerUserIDs / CcUserIDs 是按固定字段给的兼容入参
	// (旧客户端只发 approver/cc;服务端会并进 flow_selections)。
	ApproverUserIDs []string `json:"approver_userids"`
	HandlerUserIDs  []string `json:"handler_userids"`
	CcUserIDs       []string `json:"cc_userids"`
	// Attachments 是**发起人自己上传**的附件(拍照/相册选的图片、文件)。
	//
	// 随审批一路活到企微审批单的「附件」控件上 ⇒ 审批人直接看得到发票照片。
	// ⚠️ base64 会让体积涨约 1/3;上限由 service 层校验(最多 3 个,单个 ≤2MB)。
	Attachments []attachmentReq `json:"attachments"`
	// ReceiptItems / PaymentItems 是**手工收付款明细行**。
	//
	// 供应商结算单不在这里 —— 它是「付款类型-货款」,由 settlement_ids 派生。
	// 收款项冲减付款金额 ⇒ 应付金额 = 货款 + 付款项 − 收款项(不为负)。
	ReceiptItems []itemReq `json:"receipt_items"`
	PaymentItems []itemReq `json:"payment_items"`
}

// itemReq 是提交请求里的一行收付款明细。
//
// ⚠️ 金额用**十进制字符串**(与 manual_amount_yuan 同款):JSON number 走
// float64 会有精度损失,金额一律用字符串传。
type itemReq struct {
	// Type 是类型文本("陈列费"/"水电费"/…;「其它」可被改成自定义文字)。
	Type string `json:"type"`
	// Remark 是备注(所属日期等补充信息)。
	Remark string `json:"remark"`
	// AmountYuan 是金额(元)。
	AmountYuan string `json:"amount_yuan"`
}

// attachmentReq 是提交请求里"发起人上传的一个附件"。
type attachmentReq struct {
	FileName      string `json:"file_name"`
	ContentType   string `json:"content_type"`
	ContentBase64 string `json:"content_base64"`
	Size          int    `json:"size"`
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
	attachments := make([]service.AttachmentInput, 0, len(req.Attachments))
	for _, a := range req.Attachments {
		attachments = append(attachments, service.AttachmentInput{
			FileName:      a.FileName,
			ContentType:   a.ContentType,
			ContentBase64: a.ContentBase64,
			Size:          a.Size,
		})
	}
	// 收付款明细:金额是十进制字符串,解析失败当场 400(别留到流水线里炸)。
	parseItems := func(raw []itemReq, label string) ([]service.ItemInput, error) {
		out := make([]service.ItemInput, 0, len(raw))
		for i, it := range raw {
			amt, err := decimal.NewFromString(strings.TrimSpace(it.AmountYuan))
			if err != nil {
				return nil, fmt.Errorf("%s 第 %d 行金额不是十进制数字(%q)", label, i+1, it.AmountYuan)
			}
			out = append(out, service.ItemInput{
				Type:       it.Type,
				Remark:     it.Remark,
				AmountYuan: amt,
			})
		}
		return out, nil
	}
	receiptItems, err := parseItems(req.ReceiptItems, "收款明细")
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	paymentItems, err := parseItems(req.PaymentItems, "付款明细")
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	ap, err := h.svc.SubmitApproval(c.Request.Context(), branchID, service.SubmitInput{
		ApplicantUserID:  cl.Sub,
		ApplicantWecomID: req.ApplicantWecomID,
		ApplicantBearer:  c.Request.Header.Get("Authorization"),
		SettlementIDs:    req.SettlementIDs,
		ManualAmountYuan: amount,
		Remark:           req.Remark,
		FlowSelections:   service.FlowSelections(req.FlowSelections),
		ApproverUserIDs:  req.ApproverUserIDs,
		HandlerUserIDs:   req.HandlerUserIDs,
		CcUserIDs:        req.CcUserIDs,
		Attachments:      attachments,
		ReceiptItems:     receiptItems,
		PaymentItems:     paymentItems,
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"id": ap.ID, "status": ap.Status})
}

// GetApproverPreference GET /settlement-approvals/approver-preference
//
// 发起页带出该用户上次使用的审批人/办理人/抄送人(对齐官方审批"记住上次"体验)。
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

// GetApprovalFlow GET /settlement-approvals/approval-flow
//
// 返回本次审批的**流程节点声明**(类型/标题/选人方式/是否必填),发起页按它渲染。
// ⚠️ 企微公开 API 读不到流程配置 ⇒ 这份声明由 app 侧持有(Config.ApprovalFlow,
// deploy 用 SETTLEMENT_AUDITOR_APPROVAL_FLOW 配置),必须与企微后台模板一致。
// ⚠️ 路由必须注册在 `/:id` 之前,否则会被当成 id="approval-flow" 走 Get → 404。
func (h *Handler) GetApprovalFlow(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "settlement:view") {
		return
	}
	c.JSON(http.StatusOK, gin.H{"nodes": h.svc.ApprovalFlowNodes()})
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

// Delete DELETE /settlement-approvals/:id
//
// 物理删除一次审批实例及其 5 张从表。scope 用 settlement:audit（与 Submit/Retry 同级）——
// 能发起审核就能删掉它产生的记录。
//
// 先按 branch 校验归属再删：跨门店删别人的审批不该被"有 audit scope"就放行，
// 顺序与 Retry 一致（先 GetApprovalDetail 验归属，再动数据）。
//
// 流水线在途时 service 返 ErrApprovalInFlight → 409，客户端应稍后重试。
func (h *Handler) Delete(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "settlement:audit") {
		return
	}
	if _, err := h.svc.GetApprovalDetail(c.Request.Context(), branchID, c.Param("id")); err != nil {
		mapErr(c, err)
		return
	}
	if err := h.svc.DeleteApproval(c.Request.Context(), c.Param("id")); err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true, "id": c.Param("id")})
}

// ListCubeSettlements GET /settlement-approvals/cube/settlements?from&to&supplier_id&include_paid
//
// H5 勾选数据源:透传 cube 已审核采购结款单(DP/CP)。
//
// include_paid 缺省 false —— 已付款的单**不再出现**在发起勾选列表里。
// hidden_paid_count 把被剔除的张数回给前端,让页面能说明"为什么少了"。
func (h *Handler) ListCubeSettlements(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "settlement:view") {
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "200"))
	includePaid := c.Query("include_paid") == "1" || c.Query("include_paid") == "true"
	rows, hidden, err := h.svc.ListCubeSettlements(c.Request.Context(), branchID,
		c.Query("from"), c.Query("to"), c.Query("supplier_id"), limit, includePaid)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"items":             rows,
		"hidden_paid_count": hidden,
	})
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
