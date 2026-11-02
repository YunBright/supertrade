// Package handler 实现 cash-recon 服务的 HTTP handler(Gin)。
//
// 端点(nginx 已剥过 /api/v1/cash-recon/):
//
//	GET  /batches                    批次列表        scope recon:view
//	GET  /batches/:id                批次详情        scope recon:view
//	POST /batches/preview            dry-run 匹配    scope recon:operate
//
// # 鉴权链路
//
//	rbac.RequireScopeWithBranch(scope, branchFromHeader, resolver)
//	  - branchFn 取 X-Branch-ID(缺 → 400 branch_required)
//	  - claims 缺失 → 401 unauthenticated
//	  - resolver 报错(userd 不可用) → 503 userd_unavailable
//	  - 该 branch 下无此 scope → 403 forbidden
//
// scope 清单(recon:view / operate / review / audit / export)由 auth migration 006 落库。
package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/YunBright/authkit/claims"
	"github.com/YunBright/authkit/rbac"
	"github.com/YunBright/authkit/userinfo"
	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/cashrecon/engine"
	"github.com/YunBright/supertrade/internal/cashrecon/model"
	"github.com/YunBright/supertrade/internal/cashrecon/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// 端点使用的 scope。
const (
	// ScopeView 是读端点(批次/差异/日报/台账)。
	ScopeView = "recon:view"
	// ScopeOperate 是日常操作(导入、生成批次、实盘、其他收款、付款登记)。
	ScopeOperate = "recon:operate"
	// ScopeReview 是差异人工动作(复核)。
	ScopeReview = "recon:review"
	// ScopeAudit 是审核结算/冻结/账户与期初维护。
	ScopeAudit = "recon:audit"
	// ScopeExport 是导出(含敏感信息的 CSV)。
	ScopeExport = "recon:export"
)

// Handler 是 cash-recon 的 HTTP handler 聚合。
type Handler struct {
	svc *service.Service
	// users 是 userd 客户端(per-branch effective scopes 校验)。
	// nil ⇒ 带 scope 守门的端点一律 503(fail-closed)。
	users *userinfo.Client
}

// New 构造 handler。
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
	guard := func(scope string) gin.HandlerFunc {
		return rbac.RequireScopeWithBranch(scope, branchFromHeader, resolver)
	}

	r.POST("/batches/preview", requireBranch, guard(ScopeOperate), h.PreviewBatch)
	r.POST("/batches", requireBranch, guard(ScopeOperate), h.ApplyBatch)
	r.GET("/batches", requireBranch, guard(ScopeView), h.ListBatches)
	r.GET("/batches/:id", requireBranch, guard(ScopeView), h.GetBatch)

	// ---- S2：差异工作台 ----
	r.GET("/diffs", requireBranch, guard(ScopeView), h.ListDiffs)
	r.GET("/diffs/:id", requireBranch, guard(ScopeView), h.GetDiff)
	r.POST("/diffs/:id/match", requireBranch, guard(ScopeReview), h.diffAction(service.DiffActionMatch))
	r.POST("/diffs/:id/reason", requireBranch, guard(ScopeReview), h.diffAction(service.DiffActionReason))
	r.POST("/diffs/:id/hang", requireBranch, guard(ScopeReview), h.diffAction(service.DiffActionHang))
	r.POST("/diffs/:id/escalate", requireBranch, guard(ScopeReview), h.diffAction(service.DiffActionEscalate))

	// ---- S1：取数 / 建账 / 导入 ----
	r.GET("/flows/cash", requireBranch, guard(ScopeView), h.GetCashFlows)
	r.GET("/flows/payment-methods", requireBranch, guard(ScopeView), h.GetPaymentMethods)
	r.GET("/flows/cashiers", requireBranch, guard(ScopeView), h.GetCashierFlows)

	r.POST("/imports/bank", requireBranch, guard(ScopeOperate), h.ImportBank)
	r.GET("/imports", requireBranch, guard(ScopeView), h.ListImports)
	r.GET("/imports/:id", requireBranch, guard(ScopeView), h.GetImport)

	r.GET("/rules", requireBranch, guard(ScopeView), h.GetRules)
	r.PUT("/rules", requireBranch, guard(ScopeReview), h.PutRule)

	r.GET("/accounts", requireBranch, guard(ScopeView), h.ListAccounts)
	r.POST("/accounts", requireBranch, guard(ScopeAudit), h.CreateAccount)
	r.PUT("/accounts/:id", requireBranch, guard(ScopeAudit), h.UpdateAccount)

	r.GET("/opening-balances", requireBranch, guard(ScopeView), h.GetOpeningBalances)
	r.GET("/opening-balances/check", requireBranch, guard(ScopeView), h.CheckOpenings)
	r.POST("/opening-balances", requireBranch, guard(ScopeAudit), h.CreateOpenings)
	r.POST("/opening-balances/:id/void", requireBranch, guard(ScopeAudit), h.VoidOpening)

	// ---- S3：长短款 / 其他收款 / 付款核销 / 供应商应付 ----
	h.registerRoutesS3(r, requireBranch, guard)

	// ---- S4：审核冻结 / 期末快照 / 日报 / 看板 / 导出 ----
	h.registerRoutesS4(r, requireBranch, guard)
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
//
// ⚠️ 期初缺失 / 实盘缺失一律 **409 且 body 带 code**:
// 前端靠 code 跳"期初建账"页 —— 500 会让出纳以为系统坏了,
// 而实际只是"还没维护期初"。
func mapErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, model.ErrRangeTooLarge):
		writeError(c, http.StatusBadRequest, "range_too_large", err.Error())
	case errors.Is(err, model.ErrOpeningItemsIncomplete):
		writeError(c, http.StatusBadRequest, "opening_items_incomplete", err.Error())
	case errors.Is(err, model.ErrInvalidInput):
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, model.ErrNotFound):
		writeError(c, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, model.ErrPeriodFrozen):
		writeError(c, http.StatusConflict, "period_frozen", err.Error())
	case errors.Is(err, model.ErrOpeningBalanceMissing):
		writeError(c, http.StatusConflict, "opening_balance_missing", err.Error())
	case errors.Is(err, model.ErrCashCountMissing):
		writeError(c, http.StatusConflict, "cash_count_missing", err.Error())
	case errors.Is(err, model.ErrCubeSourceUnavailable):
		writeError(c, http.StatusServiceUnavailable, "cube_source_unavailable", err.Error())
	case errors.Is(err, model.ErrCubeSourceDisabled):
		writeError(c, http.StatusServiceUnavailable, "cube_source_disabled", err.Error())
	case errors.Is(err, model.ErrCubeQueryFailed):
		writeError(c, http.StatusServiceUnavailable, "cube_query_failed", err.Error())
	case errors.Is(err, model.ErrAlreadyExists):
		writeError(c, http.StatusConflict, "already_exists", err.Error())
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
// ⚠️ 取 claims.Sub —— **不信任客户端自称的 operator**,否则审计字段等于谁都能填。
func operatorOf(c *gin.Context) string {
	cl, _ := claims.FromContext(c.Request.Context())
	if cl == nil {
		return ""
	}
	return cl.Sub
}

// previewReq 是 dry-run 请求体。
type previewReq struct {
	BizDateFrom string `json:"biz_date_from"`
	BizDateTo   string `json:"biz_date_to"`
}

// previewResp 是 dry-run 响应体。
//
// 金额一律**字符串**(十进制精确值);不用 float —— 0.01 的浮点误差
// 会让"汇总=明细"的断言偶发失败,而这类失败最容易被当成"数据错"。
type previewResp struct {
	DryRun  bool           `json:"dry_run"`
	Summary previewSummary `json:"summary"`
	Matches []matchItem    `json:"matches"`
	Diffs   []diffItem     `json:"diffs"`
}

// matchItem 是一条匹配（金额字符串，fee 永远单列）。
type matchItem struct {
	Channel        string `json:"channel"`
	BizDate        string `json:"biz_date"`
	BankFlowID     string `json:"bank_flow_id"`
	CashKey        string `json:"cash_key"`
	ExpectedAmount string `json:"expected_amount"`
	ActualAmount   string `json:"actual_amount"`
	Fee            string `json:"fee"`
	Kind           string `json:"kind"`
}

// diffItem 是一条差异。
type diffItem struct {
	ID             string     `json:"id,omitempty"`
	BatchID        string     `json:"batch_id,omitempty"`
	Kind           string     `json:"kind"`
	Channel        string     `json:"channel"`
	CashRef        string     `json:"cash_ref"`
	BankRef        string     `json:"bank_ref"`
	Expected       string     `json:"expected"`
	Actual         string     `json:"actual"`
	DiffAmount     string     `json:"diff_amount"`
	Status         string     `json:"status,omitempty"`
	ResolutionCode string     `json:"resolution_code,omitempty"`
	ResolutionNote string     `json:"resolution_note,omitempty"`
	HandledBy      string     `json:"handled_by,omitempty"`
	HandledAt      *time.Time `json:"handled_at,omitempty"`
	CreatedAt      *time.Time `json:"created_at,omitempty"`
}

// matchItemOf 把引擎匹配转成响应行。
func matchItemOf(m engine.Match) matchItem {
	return matchItem{
		Channel:        m.Channel,
		BizDate:        m.BizDate.Format("2006-01-02"),
		BankFlowID:     m.BankFlowID,
		CashKey:        m.CashKey,
		ExpectedAmount: m.ExpectedAmount.String(),
		ActualAmount:   m.ActualAmount.String(),
		Fee:            m.Fee.String(),
		Kind:           m.Kind,
	}
}

// diffItemOf 把引擎差异转成响应行。
func diffItemOf(d engine.Diff) diffItem {
	return diffItem{
		Kind:           d.Kind,
		Channel:        d.Channel,
		CashRef:        d.CashRef,
		BankRef:        d.BankRef,
		Expected:       d.Expected.String(),
		Actual:         d.Actual.String(),
		DiffAmount:     d.DiffAmount.String(),
		Status:         d.Status,
		ResolutionCode: d.ResolutionCode,
		ResolutionNote: d.ResolutionNote,
		HandledBy:      d.HandledBy,
		HandledAt:      d.HandledAt,
	}
}

// storedDiffItemOf 把已落库差异转成响应行。
func storedDiffItemOf(r model.ReconDiff) diffItem {
	created := r.CreatedAt
	return diffItem{
		ID:             r.ID,
		BatchID:        r.BatchID,
		Kind:           r.Kind,
		Channel:        r.Channel,
		CashRef:        r.CashRef,
		BankRef:        r.BankRef,
		Expected:       r.Expected.String(),
		Actual:         r.Actual.String(),
		DiffAmount:     r.DiffAmount.String(),
		Status:         r.Status,
		ResolutionCode: r.ResolutionCode,
		ResolutionNote: r.ResolutionNote,
		HandledBy:      r.HandledBy,
		HandledAt:      r.HandledAt,
		CreatedAt:      &created,
	}
}

// batchView 是批次响应（金额字符串）。
type batchView struct {
	ID          string     `json:"id"`
	BatchNo     string     `json:"batch_no"`
	Kind        string     `json:"kind"`
	Status      string     `json:"status"`
	BizDateFrom string     `json:"biz_date_from"`
	BizDateTo   string     `json:"biz_date_to"`
	MatchRate   string     `json:"match_rate"`
	DiffCount   int        `json:"diff_count"`
	CashTotal   string     `json:"cash_total"`
	BankTotal   string     `json:"bank_total"`
	FeeTotal    string     `json:"fee_total"`
	CreatedBy   string     `json:"created_by"`
	FrozenAt    *time.Time `json:"frozen_at"`
	CompletedAt *time.Time `json:"completed_at"`
	CreatedAt   time.Time  `json:"created_at"`
}

// batchViewOf 转批次视图。
func batchViewOf(b model.ReconBatch) batchView {
	return batchView{
		ID:          b.ID,
		BatchNo:     b.BatchNo,
		Kind:        b.Kind,
		Status:      b.Status,
		BizDateFrom: b.BizDateFrom.Format("2006-01-02"),
		BizDateTo:   b.BizDateTo.Format("2006-01-02"),
		MatchRate:   b.MatchRate.String(),
		DiffCount:   b.DiffCount,
		CashTotal:   b.CashTotal.String(),
		BankTotal:   b.BankTotal.String(),
		FeeTotal:    b.FeeTotal.String(),
		CreatedBy:   b.CreatedBy,
		FrozenAt:    b.FrozenAt,
		CompletedAt: b.CompletedAt,
		CreatedAt:   b.CreatedAt,
	}
}

type previewSummary struct {
	MatchRate     string `json:"match_rate"`
	DiffCount     int    `json:"diff_count"`
	CashTotal     string `json:"cash_total"`
	BankTotal     string `json:"bank_total"`
	FeeTotal      string `json:"fee_total"`
	MatchedCount  int    `json:"matched_count"`
	EligibleCount int    `json:"eligible_count"`
}

// PreviewBatch POST /batches/preview
//
// **dry-run,不落库**。S0 无收银/银行流水接入时返回全零 summary。
func (h *Handler) PreviewBatch(c *gin.Context) {
	var req previewReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体必须是 JSON: {biz_date_from,biz_date_to}")
		return
	}
	from, err := time.Parse("2006-01-02", req.BizDateFrom)
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "biz_date_from 必须是 YYYY-MM-DD")
		return
	}
	to, err := time.Parse("2006-01-02", req.BizDateTo)
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "biz_date_to 必须是 YYYY-MM-DD")
		return
	}
	out, err := h.svc.Preview(c.Request.Context(), service.PreviewInput{
		BranchID:    branchIDOf(c),
		BizDateFrom: from,
		BizDateTo:   to,
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, previewResp{
		DryRun: true,
		Summary: previewSummary{
			MatchRate:     out.Summary.MatchRate.String(),
			DiffCount:     out.Summary.DiffCount,
			CashTotal:     out.Summary.CashTotal.String(),
			BankTotal:     out.Summary.BankTotal.String(),
			FeeTotal:      out.Summary.FeeTotal.String(),
			MatchedCount:  out.Summary.MatchedCount,
			EligibleCount: out.Summary.EligibleCount,
		},
		Matches: matchItemsOf(out.Matches),
		Diffs:   diffItemsOf(out.Diffs),
	})
}

// matchItemsOf 批量转匹配行（nil ⇒ 空数组，便于前端稳定消费）。
func matchItemsOf(rows []engine.Match) []matchItem {
	out := make([]matchItem, 0, len(rows))
	for _, m := range rows {
		out = append(out, matchItemOf(m))
	}
	return out
}

// diffItemsOf 批量转引擎差异行。
func diffItemsOf(rows []engine.Diff) []diffItem {
	out := make([]diffItem, 0, len(rows))
	for _, d := range rows {
		out = append(out, diffItemOf(d))
	}
	return out
}

// ListBatches GET /batches
func (h *Handler) ListBatches(c *gin.Context) {
	rows, err := h.svc.ListBatches(c.Request.Context(), branchIDOf(c), 0)
	if err != nil {
		mapErr(c, err)
		return
	}
	if rows == nil {
		rows = []model.ReconBatch{}
	}
	c.JSON(http.StatusOK, gin.H{"items": rows, "total": len(rows)})
}

// GetBatch GET /batches/:id
func (h *Handler) GetBatch(c *gin.Context) {
	row, err := h.svc.GetBatch(c.Request.Context(), branchIDOf(c), c.Param("id"))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, row)
}

// ---- S2：批次落库 / 差异工作台 ----

// applyBatchResp 是 POST /batches 的响应体。
type applyBatchResp struct {
	Batch           batchView `json:"batch"`
	InsertedMatches int       `json:"inserted_matches"`
	InsertedDiffs   int       `json:"inserted_diffs"`
	CarriedOver     int       `json:"carried_over"`
}

// ApplyBatch POST /batches
//
// 生成并落库一个 draft 批次：同一段引擎逻辑、幂等 upsert、人工结果保留。
func (h *Handler) ApplyBatch(c *gin.Context) {
	var req previewReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体必须是 JSON: {biz_date_from,biz_date_to}")
		return
	}
	from, err := time.Parse("2006-01-02", req.BizDateFrom)
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "biz_date_from 必须是 YYYY-MM-DD")
		return
	}
	to, err := time.Parse("2006-01-02", req.BizDateTo)
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "biz_date_to 必须是 YYYY-MM-DD")
		return
	}
	actor := operatorOf(c)
	if actor == "" {
		writeError(c, http.StatusUnauthorized, "unauthenticated", "缺少调用者身份")
		return
	}
	res, err := h.svc.ApplyBatch(c.Request.Context(), service.ApplyBatchInput{
		BranchID:    branchIDOf(c),
		BizDateFrom: from,
		BizDateTo:   to,
		Actor:       actor,
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, applyBatchResp{
		Batch:           batchViewOf(res.Batch),
		InsertedMatches: res.InsertedMatches,
		InsertedDiffs:   res.InsertedDiffs,
		CarriedOver:     res.CarriedOver,
	})
}

// ListDiffs GET /diffs?batch_id=&status=&kind=&limit=&offset=
func (h *Handler) ListDiffs(c *gin.Context) {
	limit, _ := strconv.Atoi(strings.TrimSpace(c.Query("limit")))
	offset, _ := strconv.Atoi(strings.TrimSpace(c.Query("offset")))
	res, err := h.svc.ListDiffs(c.Request.Context(), service.DiffQuery{
		BranchID: branchIDOf(c),
		BatchID:  c.Query("batch_id"),
		Status:   c.Query("status"),
		Kind:     c.Query("kind"),
		Limit:    limit,
		Offset:   offset,
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	items := make([]diffItem, 0, len(res.Items))
	for _, r := range res.Items {
		items = append(items, storedDiffItemOf(r))
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": res.Total})
}

// GetDiff GET /diffs/:id（含审计轨迹）
func (h *Handler) GetDiff(c *gin.Context) {
	detail, err := h.svc.GetDiff(c.Request.Context(), branchIDOf(c), c.Param("id"))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"diff":       storedDiffItemOf(detail.Diff),
		"audit_logs": detail.AuditLogs,
	})
}

// diffActionReq 是差异人工动作的请求体（reason 必填）。
type diffActionReq struct {
	Reason         string `json:"reason"`
	ResolutionCode string `json:"resolution_code"`
}

// diffAction 构造某个动作的 handler（match/reason/hang/escalate）。
func (h *Handler) diffAction(action string) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req diffActionReq
		if err := c.ShouldBindJSON(&req); err != nil {
			writeError(c, http.StatusBadRequest, "bad_request", "请求体必须是 JSON: {reason,resolution_code}")
			return
		}
		if strings.TrimSpace(req.Reason) == "" {
			writeError(c, http.StatusBadRequest, "bad_request", "reason 必填（人工动作必须留痕）")
			return
		}
		row, err := h.svc.ActDiff(c.Request.Context(), service.ActDiffInput{
			BranchID:       branchIDOf(c),
			DiffID:         c.Param("id"),
			Action:         action,
			Reason:         req.Reason,
			ResolutionCode: req.ResolutionCode,
			Actor:          operatorOf(c),
		})
		if err != nil {
			mapErr(c, err)
			return
		}
		c.JSON(http.StatusOK, storedDiffItemOf(row))
	}
}

// ---- S1：取数 ----

// cashFlowItem 是收银流水聚合的一行（金额字面量 = 4 位小数字符串）。
type cashFlowItem struct {
	BizDate   string `json:"biz_date"`
	PayWay    string `json:"pay_way"`
	Method    string `json:"method"`
	AmountNet string `json:"amount_net"`
	TxnCount  int    `json:"txn_count"`
}

// cashResp 是 /flows/* 的响应体。
//
// ⚠️ data_source 必须透出：cube 与本地 PG 的口径（是否含冲销 / 是否有快照延迟）
// 并不相同，出纳看到"这一天没有流水"时要能分清是"没有"还是"读的是另一条链路"。
type cashResp struct {
	DataSource string         `json:"data_source"`
	From       string         `json:"from"`
	To         string         `json:"to"`
	Items      []cashFlowItem `json:"items"`
}

// dayFromQuery 解析 YYYY-MM-DD 查询参数。
func dayFromQuery(c *gin.Context, key string) (time.Time, bool) {
	v := strings.TrimSpace(c.Query(key))
	if v == "" {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", key+" 必须是 YYYY-MM-DD")
		return time.Time{}, false
	}
	return t, true
}

// cashRange 取 from/to（也接受单个 date）。
//
// ⚠️ **每个分支最多写一次错误体**：`AbortWithStatusJSON` 会追加写，
// 两次 writeError 会把两个 JSON 对象拼在一个 body 里 ⇒ 客户端反序列化失败、
// 丢掉 error.code（表现为 400 但 code 变成 "Bad Request"）。
// 因此"参数存在但格式非法"后**不再**回落到下一组参数。
func cashRange(c *gin.Context) (from, to time.Time, ok bool) {
	if strings.TrimSpace(c.Query("date")) != "" {
		d, ok := dayFromQuery(c, "date")
		if !ok {
			return time.Time{}, time.Time{}, false
		}
		return d, d, true
	}
	f, okFrom := dayFromQuery(c, "from")
	if !okFrom {
		if strings.TrimSpace(c.Query("from")) == "" {
			writeError(c, http.StatusBadRequest, "bad_request", "需要 date 或 from+to")
		}
		return time.Time{}, time.Time{}, false
	}
	t, okTo := dayFromQuery(c, "to")
	if !okTo {
		if strings.TrimSpace(c.Query("to")) == "" {
			writeError(c, http.StatusBadRequest, "bad_request", "需要 date 或 from+to")
		}
		return time.Time{}, time.Time{}, false
	}
	return f, t, true
}

func cashItems(rows []service.CashDay) []cashFlowItem {
	out := make([]cashFlowItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, cashFlowItem{
			BizDate:   r.BizDate.Format("2006-01-02"),
			PayWay:    r.PayWay,
			Method:    r.Method,
			AmountNet: r.AmountNet.StringFixed(4),
			TxnCount:  r.TxnCount,
		})
	}
	return out
}

// GetCashFlows GET /flows/cash?date= | ?from=&to=
func (h *Handler) GetCashFlows(c *gin.Context) {
	from, to, ok := cashRange(c)
	if !ok {
		return
	}
	rows, src, err := h.svc.CashDayRows(c.Request.Context(), branchIDOf(c), from, to)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, cashResp{
		DataSource: src,
		From:       from.Format("2006-01-02"),
		To:         to.Format("2006-01-02"),
		Items:      cashItems(rows),
	})
}

// GetPaymentMethods GET /flows/payment-methods?date=
//
// 支付方式分析（FR-01）：单日有界聚合 + 映射结果，**未映射项显式列出**（不猜）。
func (h *Handler) GetPaymentMethods(c *gin.Context) {
	d, ok := dayFromQuery(c, "date")
	if !ok {
		if strings.TrimSpace(c.Query("date")) == "" {
			writeError(c, http.StatusBadRequest, "bad_request", "date 必填（单日有界查询）")
		}
		return
	}
	rows, src, err := h.svc.CashDayRows(c.Request.Context(), branchIDOf(c), d, d)
	if err != nil {
		mapErr(c, err)
		return
	}
	unmapped := make([]cashFlowItem, 0)
	for _, it := range cashItems(rows) {
		if it.Method == model.MethodUnmapped {
			unmapped = append(unmapped, it)
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"data_source": src,
		"date":        d.Format("2006-01-02"),
		"items":       cashItems(rows),
		"unmapped":    unmapped,
	})
}

// cashierPayWayView 是一张收银员卡片里的一个支付方式。
type cashierPayWayView struct {
	PayWay    string `json:"pay_way"`
	Method    string `json:"method"`
	AmountNet string `json:"amount_net"`
	TxnCount  int    `json:"txn_count"`
}

// cashierFlowItem 是一个收银员当日的收银聚合（前端渲染成一张卡片）。
//
// Shift 恒为 ALL：数据源 payflow.shift_no 实测全空（sale_detail 也为空），
// 没有班次口径时**不猜班次**，由前端把"班次 ALL"作为显式提示。
type cashierFlowItem struct {
	CashierID   string              `json:"cashier_id"`
	CashierName string              `json:"cashier_name"`
	Shift       string              `json:"shift"`
	TxnCount    int                 `json:"txn_count"`
	PayWays     []cashierPayWayView `json:"pay_ways"`
}

// cashierFlowsResp 是 /flows/cashiers 的响应体。
type cashierFlowsResp struct {
	DataSource string            `json:"data_source"`
	Date       string            `json:"date"`
	Items      []cashierFlowItem `json:"items"`
}

// GetCashierFlows GET /flows/cashiers?date=
//
// 单日按收银员拆分（收银流水页卡片）。`date` 必填：跨日会把收银员混在一起。
func (h *Handler) GetCashierFlows(c *gin.Context) {
	d, ok := dayFromQuery(c, "date")
	if !ok {
		if strings.TrimSpace(c.Query("date")) == "" {
			writeError(c, http.StatusBadRequest, "bad_request", "date 必填（单日有界查询）")
		}
		return
	}
	rows, src, err := h.svc.CashierDayRows(c.Request.Context(), branchIDOf(c), d)
	if err != nil {
		mapErr(c, err)
		return
	}

	items := make([]cashierFlowItem, 0)
	byCashier := map[string]int{}
	for _, r := range rows {
		pw := cashierPayWayView{
			PayWay:    r.PayWay,
			Method:    r.Method,
			AmountNet: r.AmountNet.StringFixed(4),
			TxnCount:  r.TxnCount,
		}
		if idx, ok := byCashier[r.CashierID]; ok {
			items[idx].TxnCount += r.TxnCount
			items[idx].PayWays = append(items[idx].PayWays, pw)
			continue
		}
		byCashier[r.CashierID] = len(items)
		items = append(items, cashierFlowItem{
			CashierID:   r.CashierID,
			CashierName: r.CashierName,
			Shift:       "ALL",
			TxnCount:    r.TxnCount,
			PayWays:     []cashierPayWayView{pw},
		})
	}
	c.JSON(http.StatusOK, cashierFlowsResp{
		DataSource: src,
		Date:       d.Format("2006-01-02"),
		Items:      items,
	})
}

// ---- S1：银行流水导入 ----

// importBankReq 是 JSON 导入的请求体。
type importBankReq struct {
	Kind     string                  `json:"kind"`
	FileName string                  `json:"file_name"`
	CSV      string                  `json:"csv"`
	Rows     []service.ImportBankRow `json:"rows"`
}

// ImportBank POST /imports/bank
//
// 支持两种 body：
//   - JSON：{kind,file_name,csv|rows}
//   - multipart/form-data：file=CSV 文件，kind=bank|channel
func (h *Handler) ImportBank(c *gin.Context) {
	actor := operatorOf(c)
	if actor == "" {
		writeError(c, http.StatusUnauthorized, "unauthenticated", "缺少调用者身份")
		return
	}
	in := service.ImportBankInput{
		BranchID: branchIDOf(c),
		Actor:    actor,
	}
	if strings.HasPrefix(c.ContentType(), "multipart/form-data") {
		fh, err := c.FormFile("file")
		if err != nil {
			writeError(c, http.StatusBadRequest, "bad_request", "multipart 需要 file 字段")
			return
		}
		if fh.Size > 8<<20 {
			writeError(c, http.StatusBadRequest, "bad_request", "文件超过 8MB")
			return
		}
		f, err := fh.Open()
		if err != nil {
			writeError(c, http.StatusBadRequest, "bad_request", "无法读取上传文件")
			return
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, 8<<20))
		if err != nil {
			writeError(c, http.StatusBadRequest, "bad_request", "无法读取上传文件")
			return
		}
		in.CSV = string(b)
		in.FileName = fh.Filename
		in.Kind = c.PostForm("kind")
	} else {
		var req importBankReq
		if err := c.ShouldBindJSON(&req); err != nil {
			writeError(c, http.StatusBadRequest, "bad_request", "请求体必须是 JSON 或 multipart")
			return
		}
		in.Kind = req.Kind
		in.FileName = req.FileName
		in.CSV = req.CSV
		in.Rows = req.Rows
	}

	res, err := h.svc.ImportBankFlows(c.Request.Context(), in)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// ListImports GET /imports
func (h *Handler) ListImports(c *gin.Context) {
	rows, err := h.svc.ListImports(c.Request.Context(), branchIDOf(c), 0)
	if err != nil {
		mapErr(c, err)
		return
	}
	if rows == nil {
		rows = []model.ReconImport{}
	}
	c.JSON(http.StatusOK, gin.H{"items": rows, "total": len(rows)})
}

// GetImport GET /imports/:id
func (h *Handler) GetImport(c *gin.Context) {
	row, err := h.svc.GetImport(c.Request.Context(), branchIDOf(c), c.Param("id"))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, row)
}

// ---- S1：规则 ----

// GetRules GET /rules（返回生效值：默认值 + 全局行 + 门店行合并）
func (h *Handler) GetRules(c *gin.Context) {
	rules, err := h.svc.EffectiveRules(c.Request.Context(), branchIDOf(c))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"branch_id": branchIDOf(c), "rules": rules})
}

// putRuleReq 是 PUT /rules 的请求体。
type putRuleReq struct {
	RuleKey string          `json:"rule_key"`
	Value   json.RawMessage `json:"value"`
}

// PutRule PUT /rules（门店级 upsert）
func (h *Handler) PutRule(c *gin.Context) {
	var req putRuleReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体必须是 JSON: {rule_key,value}")
		return
	}
	if len(req.Value) == 0 {
		writeError(c, http.StatusBadRequest, "bad_request", "value 必填")
		return
	}
	if err := h.svc.SaveRule(c.Request.Context(), branchIDOf(c), operatorOf(c), req.RuleKey, string(req.Value)); err != nil {
		mapErr(c, err)
		return
	}
	rules, err := h.svc.EffectiveRules(c.Request.Context(), branchIDOf(c))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"branch_id": branchIDOf(c), "rules": rules})
}

// ---- S1：账户台账 ----

// ListAccounts GET /accounts
func (h *Handler) ListAccounts(c *gin.Context) {
	includeInactive := strings.EqualFold(c.Query("include_inactive"), "true")
	rows, err := h.svc.ListAccounts(c.Request.Context(), branchIDOf(c), includeInactive)
	if err != nil {
		mapErr(c, err)
		return
	}
	if rows == nil {
		rows = []model.ReconAccount{}
	}
	c.JSON(http.StatusOK, gin.H{"items": rows, "total": len(rows)})
}

// createAccountReq 是 POST /accounts 的请求体。
type createAccountReq struct {
	AccountType string `json:"account_type"`
	AccountKey  string `json:"account_key"`
	Name        string `json:"name"`
	Currency    string `json:"currency"`
	SortOrder   int    `json:"sort_order"`
	Note        string `json:"note"`
}

// CreateAccount POST /accounts
func (h *Handler) CreateAccount(c *gin.Context) {
	var req createAccountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体必须是 JSON")
		return
	}
	row, err := h.svc.CreateAccount(c.Request.Context(), service.CreateAccountInput{
		BranchID:    branchIDOf(c),
		AccountType: req.AccountType,
		AccountKey:  req.AccountKey,
		Name:        req.Name,
		Currency:    req.Currency,
		SortOrder:   req.SortOrder,
		Note:        req.Note,
		Actor:       operatorOf(c),
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, row)
}

// updateAccountReq 是 PUT /accounts/:id 的请求体（指针 = 未提供）。
type updateAccountReq struct {
	Name      *string `json:"name"`
	IsActive  *bool   `json:"is_active"`
	SortOrder *int    `json:"sort_order"`
	Note      *string `json:"note"`
}

// UpdateAccount PUT /accounts/:id
func (h *Handler) UpdateAccount(c *gin.Context) {
	var req updateAccountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体必须是 JSON")
		return
	}
	row, err := h.svc.UpdateAccount(c.Request.Context(), service.UpdateAccountInput{
		BranchID:  branchIDOf(c),
		ID:        c.Param("id"),
		Name:      req.Name,
		IsActive:  req.IsActive,
		SortOrder: req.SortOrder,
		Note:      req.Note,
		Actor:     operatorOf(c),
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, row)
}

// ---- S1：期初余额 ----

// GetOpeningBalances GET /opening-balances?date=
func (h *Handler) GetOpeningBalances(c *gin.Context) {
	d, ok := dayFromQuery(c, "date")
	if !ok {
		if strings.TrimSpace(c.Query("date")) == "" {
			writeError(c, http.StatusBadRequest, "bad_request", "date 必填")
		}
		return
	}
	view, err := h.svc.OpeningView(c.Request.Context(), branchIDOf(c), d)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

// CheckOpenings GET /opening-balances/check?from=
//
// 前置校验：列出该期首日缺期初的启用账户（前端跳"期初建账"页）。
func (h *Handler) CheckOpenings(c *gin.Context) {
	from, ok := dayFromQuery(c, "from")
	if !ok {
		if strings.TrimSpace(c.Query("from")) == "" {
			writeError(c, http.StatusBadRequest, "bad_request", "from 必填")
		}
		return
	}
	missing, err := h.svc.MissingOpenings(c.Request.Context(), branchIDOf(c), from)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"from":     from.Format("2006-01-02"),
		"missing":  missing,
		"total":    len(missing),
		"complete": len(missing) == 0,
	})
}

// createOpeningsReq 是 POST /opening-balances 的请求体。
type createOpeningsReq struct {
	EffectiveDate string                     `json:"effective_date"`
	Note          string                     `json:"note"`
	Items         []service.OpeningItemInput `json:"items"`
}

// CreateOpenings POST /opening-balances
func (h *Handler) CreateOpenings(c *gin.Context) {
	var req createOpeningsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体必须是 JSON: {effective_date,items}")
		return
	}
	eff, err := time.Parse("2006-01-02", strings.TrimSpace(req.EffectiveDate))
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "effective_date 必须是 YYYY-MM-DD")
		return
	}
	rows, err := h.svc.CreateOpenings(c.Request.Context(), service.CreateOpeningsInput{
		BranchID:      branchIDOf(c),
		EffectiveDate: eff,
		Items:         req.Items,
		Note:          req.Note,
		Actor:         operatorOf(c),
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": rows, "total": len(rows)})
}

// voidOpeningReq 是 POST /opening-balances/:id/void 的请求体。
type voidOpeningReq struct {
	Reason string `json:"reason"`
}

// VoidOpening POST /opening-balances/:id/void
func (h *Handler) VoidOpening(c *gin.Context) {
	var req voidOpeningReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体必须是 JSON: {reason}")
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeError(c, http.StatusBadRequest, "bad_request", "reason 必填（作废必须留痕）")
		return
	}
	row, err := h.svc.VoidOpening(c.Request.Context(), branchIDOf(c), c.Param("id"), operatorOf(c), req.Reason)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, row)
}
