// Package handler / handler_s3.go —— S3 端点：长短款 / 其他收款 / 付款核销 / 应付。
//
// 金额一律以**字符串**返回（十进制精确值，避免 float 抖动）；
// 错误经 mapErr 统一映射（409 period_frozen / 409 already_exists / 400 …）。
package handler

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/cashrecon/model"
	"github.com/YunBright/supertrade/internal/cashrecon/service"
)

// registerRoutesS3 注册 S3 的业务路由（由 RegisterRoutes 调用）。
func (h *Handler) registerRoutesS3(r gin.IRouter, requireBranch gin.HandlerFunc, guard func(string) gin.HandlerFunc) {
	// ---- 现金长短款（FR-04） ----
	r.POST("/cash-counts", requireBranch, guard(ScopeOperate), h.RecordCashCount)
	r.GET("/cash-counts", requireBranch, guard(ScopeView), h.ListCashCounts)
	r.POST("/cash-counts/:id/confirm", requireBranch, guard(ScopeReview), h.ConfirmCashCount)
	r.GET("/cash-variances/summary", requireBranch, guard(ScopeView), h.CashVarianceSummary)

	// ---- 其他收款与凭证（FR-05） ----
	r.POST("/receipts", requireBranch, guard(ScopeOperate), h.CreateReceipt)
	r.GET("/receipts", requireBranch, guard(ScopeView), h.ListReceipts)
	r.GET("/receipts/:id", requireBranch, guard(ScopeView), h.GetReceipt)
	r.POST("/receipts/:id/attachments", requireBranch, guard(ScopeOperate), h.AddReceiptAttachment)
	r.GET("/receipts/:id/attachments", requireBranch, guard(ScopeView), h.ListReceiptAttachments)
	r.GET("/attachments/:id", requireBranch, guard(ScopeView), h.DownloadAttachment)

	// ---- 付款登记与核销（FR-07/§11） ----
	r.POST("/payments/sync", requireBranch, guard(ScopeOperate), h.SyncPayments)
	r.POST("/payments", requireBranch, guard(ScopeOperate), h.RegisterPayment)
	r.GET("/payments", requireBranch, guard(ScopeView), h.ListPayments)
	r.GET("/payments/:id", requireBranch, guard(ScopeView), h.GetPayment)
	r.POST("/payments/:id/verify", requireBranch, guard(ScopeReview), h.VerifyPayment)
	r.POST("/payments/:id/actions", requireBranch, guard(ScopeReview), h.ActOnPayment)

	// ---- 供应商应付（FR-08） ----
	r.GET("/payables/suppliers", requireBranch, guard(ScopeView), h.SupplierPayables)
}

// ---- 现金长短款 ----

// cashCountView 是实盘响应（金额字符串）。
type cashCountView struct {
	ID           string     `json:"id"`
	BizDate      string     `json:"biz_date"`
	CashierID    string     `json:"cashier_id"`
	ExpectedCash string     `json:"expected_cash"`
	CountedCash  string     `json:"counted_cash"`
	Variance     string     `json:"variance"`
	Tolerance    string     `json:"tolerance"`
	Status       string     `json:"status"`
	ReasonCode   string     `json:"reason_code,omitempty"`
	ConfirmBy    string     `json:"confirm_by,omitempty"`
	ConfirmAt    *time.Time `json:"confirm_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

func cashCountViewOf(r model.ReconCashCount) cashCountView {
	return cashCountView{
		ID:           r.ID,
		BizDate:      r.BizDate.Format("2006-01-02"),
		CashierID:    r.CashierID,
		ExpectedCash: r.ExpectedCash.String(),
		CountedCash:  r.CountedCash.String(),
		Variance:     r.Variance.String(),
		Tolerance:    r.Tolerance.String(),
		Status:       r.Status,
		ReasonCode:   r.ReasonCode,
		ConfirmBy:    r.ConfirmBy,
		ConfirmAt:    r.ConfirmAt,
		CreatedAt:    r.CreatedAt,
	}
}

// varianceView 是长短款台账响应。
type varianceView struct {
	ID         string `json:"id"`
	BizDate    string `json:"biz_date"`
	CashierID  string `json:"cashier_id"`
	Kind       string `json:"kind"`
	Amount     string `json:"amount"`
	ReasonCode string `json:"reason_code,omitempty"`
	Status     string `json:"status"`
}

// cashVarianceViewOf 转长短款行。
func cashVarianceViewOf(r model.ReconCashVariance) varianceView {
	return varianceView{
		ID:         r.ID,
		BizDate:    r.BizDate.Format("2006-01-02"),
		CashierID:  r.CashierID,
		Kind:       r.Kind,
		Amount:     r.Amount.String(),
		ReasonCode: r.ReasonCode,
		Status:     r.Status,
	}
}

// cashCountReq 是实盘录入请求体。
type cashCountReq struct {
	BizDate      string           `json:"biz_date"`
	CashierID    string           `json:"cashier_id"`
	CountedCash  *decimal.Decimal `json:"counted_cash"`
	ExpectedCash *decimal.Decimal `json:"expected_cash"`
	Tolerance    *decimal.Decimal `json:"tolerance"`
	ReasonCode   string           `json:"reason_code"`
}

// RecordCashCount POST /cash-counts
func (h *Handler) RecordCashCount(c *gin.Context) {
	var req cashCountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体必须是 JSON: {biz_date,cashier_id,counted_cash,...}")
		return
	}
	day, err := time.Parse("2006-01-02", strings.TrimSpace(req.BizDate))
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "biz_date 必须是 YYYY-MM-DD")
		return
	}
	if req.CountedCash == nil {
		writeError(c, http.StatusBadRequest, "bad_request", "counted_cash 必填（0 也是合法实盘，不能省略）")
		return
	}
	actor := operatorOf(c)
	if actor == "" {
		writeError(c, http.StatusUnauthorized, "unauthenticated", "缺少调用者身份")
		return
	}
	res, err := h.svc.RecordCashCount(c.Request.Context(), service.CashCountInput{
		BranchID:     branchIDOf(c),
		BizDate:      day,
		CashierID:    req.CashierID,
		CountedCash:  req.CountedCash,
		ExpectedCash: req.ExpectedCash,
		Tolerance:    req.Tolerance,
		ReasonCode:   req.ReasonCode,
		Actor:        actor,
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	out := gin.H{"count": cashCountViewOf(res.Count)}
	if res.Variance != nil {
		out["variance"] = cashVarianceViewOf(*res.Variance)
	}
	c.JSON(http.StatusOK, out)
}

// ListCashCounts GET /cash-counts?date=|?from=&to=&cashier_id=&limit=&offset=
func (h *Handler) ListCashCounts(c *gin.Context) {
	from, to, ok := cashRange(c)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(strings.TrimSpace(c.Query("limit")))
	offset, _ := strconv.Atoi(strings.TrimSpace(c.Query("offset")))
	rows, total, err := h.svc.ListCashCounts(c.Request.Context(), branchIDOf(c), from, to,
		c.Query("cashier_id"), limit, offset)
	if err != nil {
		mapErr(c, err)
		return
	}
	items := make([]cashCountView, 0, len(rows))
	for _, r := range rows {
		items = append(items, cashCountViewOf(r))
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": total})
}

// confirmCashCountReq 是确认请求体。
type confirmCashCountReq struct {
	ReasonCode string `json:"reason_code"`
}

// ConfirmCashCount POST /cash-counts/:id/confirm
func (h *Handler) ConfirmCashCount(c *gin.Context) {
	var req confirmCashCountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体必须是 JSON: {reason_code}")
		return
	}
	row, err := h.svc.ConfirmCashCount(c.Request.Context(), branchIDOf(c), c.Param("id"),
		operatorOf(c), req.ReasonCode)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, cashCountViewOf(row))
}

// cashierVarianceView 是收银员汇总响应行（金额字符串）。
type cashierVarianceView struct {
	CashierID   string `json:"cashier_id"`
	OverAmount  string `json:"over_amount"`
	ShortAmount string `json:"short_amount"`
	NetAmount   string `json:"net_amount"`
	Count       int    `json:"count"`
	OpenCount   int    `json:"open_count"`
}

// CashVarianceSummary GET /cash-variances/summary?date=|?from=&to=&cashier_id=
func (h *Handler) CashVarianceSummary(c *gin.Context) {
	from, to, ok := cashRange(c)
	if !ok {
		return
	}
	rows, err := h.svc.CashVarianceSummary(c.Request.Context(), branchIDOf(c), from, to, c.Query("cashier_id"))
	if err != nil {
		mapErr(c, err)
		return
	}
	items := make([]cashierVarianceView, 0, len(rows))
	for _, r := range rows {
		items = append(items, cashierVarianceView{
			CashierID:   r.CashierID,
			OverAmount:  r.OverAmount.String(),
			ShortAmount: r.ShortAmount.String(),
			NetAmount:   r.NetAmount.String(),
			Count:       r.Count,
			OpenCount:   r.OpenCount,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"from":  from.Format("2006-01-02"),
		"to":    to.Format("2006-01-02"),
		"items": items,
		"total": len(items),
	})
}

// ---- 其他收款 ----

// receiptView 是其他收款响应（金额字符串）。
type receiptView struct {
	ID          string    `json:"id"`
	BizDate     string    `json:"biz_date"`
	Amount      string    `json:"amount"`
	Method      string    `json:"method"`
	Payer       string    `json:"payer"`
	Reason      string    `json:"reason"`
	Source      string    `json:"source"`
	Status      string    `json:"status"`
	ConfirmedBy string    `json:"confirmed_by,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

func receiptViewOf(r model.ReconOtherReceipt) receiptView {
	return receiptView{
		ID:          r.ID,
		BizDate:     r.BizDate.Format("2006-01-02"),
		Amount:      r.Amount.String(),
		Method:      r.Method,
		Payer:       r.Payer,
		Reason:      r.Reason,
		Source:      r.Source,
		Status:      r.Status,
		ConfirmedBy: r.ConfirmedBy,
		CreatedAt:   r.CreatedAt,
	}
}

// createReceiptReq 是其他收款录入请求体。
type createReceiptReq struct {
	BizDate string          `json:"biz_date"`
	Amount  decimal.Decimal `json:"amount"`
	Method  string          `json:"method"`
	Payer   string          `json:"payer"`
	Reason  string          `json:"reason"`
}

// CreateReceipt POST /receipts
func (h *Handler) CreateReceipt(c *gin.Context) {
	var req createReceiptReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体必须是 JSON: {biz_date,amount,method,...}")
		return
	}
	day, err := time.Parse("2006-01-02", strings.TrimSpace(req.BizDate))
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "biz_date 必须是 YYYY-MM-DD")
		return
	}
	actor := operatorOf(c)
	if actor == "" {
		writeError(c, http.StatusUnauthorized, "unauthenticated", "缺少调用者身份")
		return
	}
	row, err := h.svc.CreateReceipt(c.Request.Context(), service.CreateReceiptInput{
		BranchID: branchIDOf(c),
		BizDate:  day,
		Amount:   req.Amount,
		Method:   req.Method,
		Payer:    req.Payer,
		Reason:   req.Reason,
		Actor:    actor,
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, receiptViewOf(row))
}

// ListReceipts GET /receipts?date=|?from=&to=&method=&limit=&offset=
func (h *Handler) ListReceipts(c *gin.Context) {
	from, to, ok := cashRange(c)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(strings.TrimSpace(c.Query("limit")))
	offset, _ := strconv.Atoi(strings.TrimSpace(c.Query("offset")))
	rows, total, err := h.svc.ListReceipts(c.Request.Context(), branchIDOf(c), from, to,
		c.Query("method"), limit, offset)
	if err != nil {
		mapErr(c, err)
		return
	}
	items := make([]receiptView, 0, len(rows))
	for _, r := range rows {
		items = append(items, receiptViewOf(r))
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": total})
}

// GetReceipt GET /receipts/:id
func (h *Handler) GetReceipt(c *gin.Context) {
	row, err := h.svc.GetReceipt(c.Request.Context(), branchIDOf(c), c.Param("id"))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, receiptViewOf(row))
}

// attachmentView 是附件元数据（不含二进制）。
type attachmentView struct {
	ID          string    `json:"id"`
	ReceiptID   string    `json:"receipt_id"`
	FileName    string    `json:"file_name"`
	ContentType string    `json:"content_type"`
	SizeBytes   int64     `json:"size_bytes"`
	CreatedAt   time.Time `json:"created_at"`
}

func attachmentViewOf(r model.ReconAttachment) attachmentView {
	return attachmentView{
		ID:          r.ID,
		ReceiptID:   r.ReceiptID,
		FileName:    r.FileName,
		ContentType: r.ContentType,
		SizeBytes:   r.SizeBytes,
		CreatedAt:   r.CreatedAt,
	}
}

// AddReceiptAttachment POST /receipts/:id/attachments（multipart，字段 file）
func (h *Handler) AddReceiptAttachment(c *gin.Context) {
	if !strings.HasPrefix(c.ContentType(), "multipart/form-data") {
		writeError(c, http.StatusBadRequest, "bad_request", "必须用 multipart/form-data 上传 file")
		return
	}
	fh, err := c.FormFile("file")
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "multipart 需要 file 字段")
		return
	}
	actor := operatorOf(c)
	if actor == "" {
		writeError(c, http.StatusUnauthorized, "unauthenticated", "缺少调用者身份")
		return
	}
	limit := h.svc.Config().AttachmentMaxBytes
	if fh.Size > limit {
		writeError(c, http.StatusBadRequest, "bad_request", fmt.Sprintf("附件超过 %d 字节", limit))
		return
	}
	f, err := fh.Open()
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "无法读取上传文件")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "无法读取上传文件")
		return
	}
	if int64(len(data)) > limit {
		writeError(c, http.StatusBadRequest, "bad_request", fmt.Sprintf("附件超过 %d 字节", limit))
		return
	}
	row, err := h.svc.AddAttachment(c.Request.Context(), service.AddAttachmentInput{
		BranchID:    branchIDOf(c),
		ReceiptID:   c.Param("id"),
		FileName:    fh.Filename,
		ContentType: fh.Header.Get("Content-Type"),
		Data:        data,
		Actor:       actor,
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, attachmentViewOf(row))
}

// ListReceiptAttachments GET /receipts/:id/attachments
func (h *Handler) ListReceiptAttachments(c *gin.Context) {
	rows, err := h.svc.ListAttachments(c.Request.Context(), branchIDOf(c), c.Param("id"))
	if err != nil {
		mapErr(c, err)
		return
	}
	items := make([]attachmentView, 0, len(rows))
	for _, r := range rows {
		items = append(items, attachmentViewOf(r))
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": len(items)})
}

// DownloadAttachment GET /attachments/:id
func (h *Handler) DownloadAttachment(c *gin.Context) {
	row, err := h.svc.GetAttachment(c.Request.Context(), branchIDOf(c), c.Param("id"))
	if err != nil {
		mapErr(c, err)
		return
	}
	name := strings.ReplaceAll(strings.ReplaceAll(row.FileName, "\"", ""), "\r", "")
	contentType := row.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	c.Header("Content-Disposition", fmt.Sprintf(`inline; filename="%s"`, name))
	c.Data(http.StatusOK, contentType, row.Data)
}

// ---- 付款核销 ----

// paymentView 是付款台账响应（金额字符串）。
type paymentView struct {
	ID           string     `json:"id"`
	Source       string     `json:"source"`
	SourceID     string     `json:"source_id"`
	SupplierName string     `json:"supplier_name"`
	Amount       string     `json:"amount"`
	RequestDate  *time.Time `json:"request_date,omitempty"`
	PayDate      *time.Time `json:"pay_date,omitempty"`
	Method       string     `json:"method,omitempty"`
	BankFlowID   string     `json:"bank_flow_id,omitempty"`
	Verified     bool       `json:"verified"`
	VerifiedBy   string     `json:"verified_by,omitempty"`
	MatchStatus  string     `json:"match_status"`
	MatchReason  string     `json:"match_reason,omitempty"`
	Note         string     `json:"note,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

func paymentViewOf(r model.ReconPaymentRecord) paymentView {
	return paymentView{
		ID:           r.ID,
		Source:       r.Source,
		SourceID:     r.SourceID,
		SupplierName: r.SupplierName,
		Amount:       r.Amount.String(),
		RequestDate:  r.RequestDate,
		PayDate:      r.PayDate,
		Method:       r.Method,
		BankFlowID:   r.BankFlowID,
		Verified:     r.Verified,
		VerifiedBy:   r.VerifiedBy,
		MatchStatus:  r.MatchStatus,
		MatchReason:  r.MatchReason,
		Note:         r.Note,
		CreatedAt:    r.CreatedAt,
	}
}

// SyncPayments POST /payments/sync
func (h *Handler) SyncPayments(c *gin.Context) {
	res, err := h.svc.SyncPayments(c.Request.Context(), branchIDOf(c))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// registerPaymentReq 是手工付款登记请求体。
type registerPaymentReq struct {
	SupplierName string          `json:"supplier_name"`
	Amount       decimal.Decimal `json:"amount"`
	RequestDate  string          `json:"request_date"`
	PayDate      string          `json:"pay_date"`
	Method       string          `json:"method"`
	Note         string          `json:"note"`
}

// RegisterPayment POST /payments
func (h *Handler) RegisterPayment(c *gin.Context) {
	var req registerPaymentReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体必须是 JSON: {supplier_name,amount,...}")
		return
	}
	actor := operatorOf(c)
	if actor == "" {
		writeError(c, http.StatusUnauthorized, "unauthenticated", "缺少调用者身份")
		return
	}
	reqDate, ok := optionalDay(c, req.RequestDate, "request_date")
	if !ok {
		return
	}
	payDate, ok := optionalDay(c, req.PayDate, "pay_date")
	if !ok {
		return
	}
	row, err := h.svc.RegisterPayment(c.Request.Context(), service.RegisterPaymentInput{
		BranchID:     branchIDOf(c),
		SupplierName: req.SupplierName,
		Amount:       req.Amount,
		RequestDate:  reqDate,
		PayDate:      payDate,
		Method:       req.Method,
		Note:         req.Note,
		Actor:        actor,
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, paymentViewOf(row))
}

// optionalDay 解析可选的 YYYY-MM-DD（空 ⇒ nil）。
func optionalDay(c *gin.Context, v, key string) (*time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, true
	}
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", key+" 必须是 YYYY-MM-DD")
		return nil, false
	}
	return &t, true
}

// ListPayments GET /payments?source=&verified=&limit=&offset=
func (h *Handler) ListPayments(c *gin.Context) {
	limit, _ := strconv.Atoi(strings.TrimSpace(c.Query("limit")))
	offset, _ := strconv.Atoi(strings.TrimSpace(c.Query("offset")))
	var verified *bool
	switch strings.ToLower(strings.TrimSpace(c.Query("verified"))) {
	case "true":
		v := true
		verified = &v
	case "false":
		v := false
		verified = &v
	}
	rows, total, err := h.svc.ListPayments(c.Request.Context(), service.PaymentQuery{
		BranchID: branchIDOf(c),
		Source:   c.Query("source"),
		Verified: verified,
		Limit:    limit,
		Offset:   offset,
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	items := make([]paymentView, 0, len(rows))
	for _, r := range rows {
		items = append(items, paymentViewOf(r))
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": total})
}

// GetPayment GET /payments/:id
func (h *Handler) GetPayment(c *gin.Context) {
	row, err := h.svc.GetPayment(c.Request.Context(), branchIDOf(c), c.Param("id"))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, paymentViewOf(row))
}

// verifyPaymentReq 是核销请求体。
type verifyPaymentReq struct {
	BankFlowID string `json:"bank_flow_id"`
	Note       string `json:"note"`
}

// VerifyPayment POST /payments/:id/verify
func (h *Handler) VerifyPayment(c *gin.Context) {
	var req verifyPaymentReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体必须是 JSON: {bank_flow_id}")
		return
	}
	actor := operatorOf(c)
	if actor == "" {
		writeError(c, http.StatusUnauthorized, "unauthenticated", "缺少调用者身份")
		return
	}
	row, err := h.svc.VerifyPayment(c.Request.Context(), service.VerifyPaymentInput{
		BranchID:   branchIDOf(c),
		PaymentID:  c.Param("id"),
		BankFlowID: req.BankFlowID,
		Note:       req.Note,
		Actor:      actor,
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, paymentViewOf(row))
}

// paymentActionReq 是付款匹配特殊处理请求体。
type paymentActionReq struct {
	Action string `json:"action"`
	Reason string `json:"reason"`
}

// ActOnPayment POST /payments/:id/actions
//
// action ∈ reason / hang / escalate；reason 必填。
func (h *Handler) ActOnPayment(c *gin.Context) {
	var req paymentActionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体必须是 JSON: {action,reason}")
		return
	}
	actor := operatorOf(c)
	if actor == "" {
		writeError(c, http.StatusUnauthorized, "unauthenticated", "缺少调用者身份")
		return
	}
	row, err := h.svc.ActOnPayment(c.Request.Context(), service.ActOnPaymentInput{
		BranchID:  branchIDOf(c),
		PaymentID: c.Param("id"),
		Action:    req.Action,
		Reason:    req.Reason,
		Actor:     actor,
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, paymentViewOf(row))
}

// ---- 供应商应付 ----

// payableView 是应付行（金额字符串）。
type payableView struct {
	Supplier     string `json:"supplier"`
	UnpaidAmount string `json:"unpaid_amount"`
	UnpaidCount  int    `json:"unpaid_count"`
	PaidAmount   string `json:"paid_amount"`
	PaidCount    int    `json:"paid_count"`
}

// SupplierPayables GET /payables/suppliers
func (h *Handler) SupplierPayables(c *gin.Context) {
	res, err := h.svc.SupplierPayables(c.Request.Context(), branchIDOf(c))
	if err != nil {
		mapErr(c, err)
		return
	}
	items := make([]payableView, 0, len(res.Items))
	for _, r := range res.Items {
		items = append(items, payableView{
			Supplier:     r.Supplier,
			UnpaidAmount: r.UnpaidAmount.String(),
			UnpaidCount:  r.UnpaidCount,
			PaidAmount:   r.PaidAmount.String(),
			PaidCount:    r.PaidCount,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"branch_id":    res.BranchID,
		"items":        items,
		"total_unpaid": res.TotalUnpaid.String(),
		"total_paid":   res.TotalPaid.String(),
	})
}
