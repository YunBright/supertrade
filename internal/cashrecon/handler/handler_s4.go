// Package handler / handler_s4.go —— S4 端点：审核冻结 / 期末快照 / 日报 / 看板 / 导出。
//
// 金额一律**字符串**返回（十进制精确值）；缺期初 / 缺实盘 409 且 body 带 code
// （前端据此跳"期初建账"页），导出 CSV 带 UTF-8 BOM。
package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/cashrecon/model"
	"github.com/YunBright/supertrade/internal/cashrecon/service"
)

// registerRoutesS4 注册 S4 的业务路由（由 RegisterRoutes 调用）。
func (h *Handler) registerRoutesS4(r gin.IRouter, requireBranch gin.HandlerFunc, guard func(string) gin.HandlerFunc) {
	// ---- 审核结算（FR-12/§10）----
	r.POST("/batches/:id/freeze", requireBranch, guard(ScopeAudit), h.FreezeBatch)
	r.POST("/batches/:id/complete", requireBranch, guard(ScopeAudit), h.CompleteBatch)
	r.POST("/batches/:id/recompute", requireBranch, guard(ScopeAudit), h.RecomputeBatch)

	// ---- 日报 / 看板 / 导出（FR-14/FR-15/§12）----
	r.GET("/reports/daily", requireBranch, guard(ScopeView), h.DailyReport)
	r.GET("/reports/dashboard", requireBranch, guard(ScopeView), h.DashboardReport)
	r.GET("/exports/:kind", requireBranch, guard(ScopeExport), h.ExportCSV)
}

// writeOpeningMissing 把"缺期初"写成 409 + 缺失账户清单。
//
// 返回 true 表示已处理（调用方直接 return）。
func writeOpeningMissing(c *gin.Context, err error) bool {
	var om *service.OpeningMissingError
	if !errors.As(err, &om) {
		return false
	}
	c.AbortWithStatusJSON(http.StatusConflict, gin.H{
		"code":    "opening_balance_missing",
		"message": err.Error(),
		"missing": om.Missing,
	})
	return true
}

// freezeResp 是 freeze 响应体。
type freezeResp struct {
	Batch         batchView `json:"batch"`
	CashFlowRows  int       `json:"cash_flow_rows"`
	DataSource    string    `json:"data_source,omitempty"`
	AlreadyFrozen bool      `json:"already_frozen"`
}

// FreezeBatch POST /batches/:id/freeze
func (h *Handler) FreezeBatch(c *gin.Context) {
	actor := operatorOf(c)
	if actor == "" {
		writeError(c, http.StatusUnauthorized, "unauthenticated", "缺少调用者身份")
		return
	}
	res, err := h.svc.FreezeBatch(c.Request.Context(), branchIDOf(c), c.Param("id"), actor)
	if err != nil {
		if writeOpeningMissing(c, err) {
			return
		}
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, freezeResp{
		Batch:         batchViewOf(res.Batch),
		CashFlowRows:  res.CashFlowRows,
		DataSource:    res.DataSource,
		AlreadyFrozen: res.AlreadyFrozen,
	})
}

// snapshotView 是期末快照响应行（金额字符串）。
type snapshotView struct {
	ID               string `json:"id"`
	PeriodStart      string `json:"period_start"`
	PeriodEnd        string `json:"period_end"`
	AccountType      string `json:"account_type"`
	AccountKey       string `json:"account_key"`
	Opening          string `json:"opening"`
	OpeningSource    string `json:"opening_source"`
	AdjustmentAmount string `json:"adjustment_amount"`
	Inflow           string `json:"inflow"`
	Outflow          string `json:"outflow"`
	Fee              string `json:"fee"`
	Closing          string `json:"closing"`
	SourceBatchID    string `json:"source_batch_id"`
}

func snapshotViewOf(r model.ReconPeriodSnapshot) snapshotView {
	return snapshotView{
		ID:               r.ID,
		PeriodStart:      r.PeriodStart.Format("2006-01-02"),
		PeriodEnd:        r.PeriodEnd.Format("2006-01-02"),
		AccountType:      r.AccountType,
		AccountKey:       r.AccountKey,
		Opening:          r.Opening.String(),
		OpeningSource:    r.OpeningSource,
		AdjustmentAmount: r.AdjustmentAmount.String(),
		Inflow:           r.Inflow.String(),
		Outflow:          r.Outflow.String(),
		Fee:              r.Fee.String(),
		Closing:          r.Closing.String(),
		SourceBatchID:    r.SourceBatchID,
	}
}

// completeResp 是 complete 响应体。
type completeResp struct {
	Batch            batchView      `json:"batch"`
	Snapshots        []snapshotView `json:"snapshots"`
	CarriedOpenings  int            `json:"carried_openings"`
	CashClosing      string         `json:"cash_closing"`
	AlreadyCompleted bool           `json:"already_completed"`
}

// CompleteBatch POST /batches/:id/complete
func (h *Handler) CompleteBatch(c *gin.Context) {
	actor := operatorOf(c)
	if actor == "" {
		writeError(c, http.StatusUnauthorized, "unauthenticated", "缺少调用者身份")
		return
	}
	res, err := h.svc.CompleteBatch(c.Request.Context(), branchIDOf(c), c.Param("id"), actor)
	if err != nil {
		if writeOpeningMissing(c, err) {
			return
		}
		mapErr(c, err)
		return
	}
	views := make([]snapshotView, 0, len(res.Snapshots))
	for _, r := range res.Snapshots {
		views = append(views, snapshotViewOf(r))
	}
	c.JSON(http.StatusOK, completeResp{
		Batch:            batchViewOf(res.Batch),
		Snapshots:        views,
		CarriedOpenings:  res.CarriedOpenings,
		CashClosing:      res.CashClosing.String(),
		AlreadyCompleted: res.AlreadyCompleted,
	})
}

// recomputeResp 是重做响应体。
type recomputeResp struct {
	Batch           batchView `json:"batch"`
	CashFlowRows    int       `json:"cash_flow_rows"`
	InsertedMatches int       `json:"inserted_matches"`
	InsertedDiffs   int       `json:"inserted_diffs"`
	RemovedMatches  int       `json:"removed_matches"`
	RemovedDiffs    int       `json:"removed_diffs"`
	CarriedOver     int       `json:"carried_over"`
}

// RecomputeBatch POST /batches/:id/recompute
//
// 重做：按当前收银流水重新跑匹配与差异（审核中批次也可以重做）。
func (h *Handler) RecomputeBatch(c *gin.Context) {
	actor := operatorOf(c)
	if actor == "" {
		writeError(c, http.StatusUnauthorized, "unauthenticated", "缺少调用者身份")
		return
	}
	res, err := h.svc.RecomputeBatch(c.Request.Context(), branchIDOf(c), c.Param("id"), actor)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, recomputeResp{
		Batch:           batchViewOf(res.Batch),
		CashFlowRows:    res.CashFlowRows,
		InsertedMatches: res.InsertedMatches,
		InsertedDiffs:   res.InsertedDiffs,
		RemovedMatches:  res.RemovedMatches,
		RemovedDiffs:    res.RemovedDiffs,
		CarriedOver:     res.CarriedOver,
	})
}

// ---- 日报 / 看板 ----

// receiptRowView 是日报收款行（金额字符串）。
type receiptRowView struct {
	Method   string `json:"method"`
	Amount   string `json:"amount"`
	TxnCount int    `json:"txn_count"`
}

// balanceRowView 是日报余额行（金额字符串）。
type balanceRowView struct {
	AccountType string `json:"account_type"`
	AccountKey  string `json:"account_key"`
	Name        string `json:"name"`
	PeriodStart string `json:"period_start"`
	PeriodEnd   string `json:"period_end"`
	Opening     string `json:"opening"`
	Inflow      string `json:"inflow"`
	Outflow     string `json:"outflow"`
	Fee         string `json:"fee"`
	Closing     string `json:"closing"`
}

// drillView 是下钻 id 列表。
type drillView struct {
	BatchIDs    []string `json:"batch_ids"`
	DiffIDs     []string `json:"diff_ids"`
	BankFlowIDs []string `json:"bank_flow_ids"`
	PaymentIDs  []string `json:"payment_ids"`
}

// dailyReportView 是日报响应（金额字符串）。
type dailyReportView struct {
	BranchID string `json:"branch_id"`
	Date     string `json:"date"`

	Receipts     []receiptRowView `json:"receipts"`
	ReceiptTotal string           `json:"receipt_total"`
	CashTotal    string           `json:"cash_total"`
	BankTotal    string           `json:"bank_total"`
	FeeTotal     string           `json:"fee_total"`

	DiffCount     int    `json:"diff_count"`
	OpenDiffCount int    `json:"open_diff_count"`
	DiffAmount    string `json:"diff_amount"`

	CashOverAmount  string `json:"cash_over_amount"`
	CashShortAmount string `json:"cash_short_amount"`

	PaymentTotal        string `json:"payment_total"`
	PendingPaymentCount int    `json:"pending_payment_count"`

	Balances []balanceRowView `json:"balances"`
	Drill    drillView        `json:"drill"`
}

// DailyReport GET /reports/daily?date=
func (h *Handler) DailyReport(c *gin.Context) {
	var date time.Time
	if strings.TrimSpace(c.Query("date")) != "" {
		var ok bool
		date, ok = dayFromQuery(c, "date")
		if !ok {
			return
		}
	} else {
		date = time.Now()
	}
	res, err := h.svc.Daily(c.Request.Context(), branchIDOf(c), date)
	if err != nil {
		mapErr(c, err)
		return
	}
	view := dailyReportView{
		BranchID:            res.BranchID,
		Date:                res.Date,
		Receipts:            make([]receiptRowView, 0, len(res.Receipts)),
		ReceiptTotal:        res.ReceiptTotal.String(),
		CashTotal:           res.CashTotal.String(),
		BankTotal:           res.BankTotal.String(),
		FeeTotal:            res.FeeTotal.String(),
		DiffCount:           res.DiffCount,
		OpenDiffCount:       res.OpenDiffCount,
		DiffAmount:          res.DiffAmount.String(),
		CashOverAmount:      res.CashOverAmount.String(),
		CashShortAmount:     res.CashShortAmount.String(),
		PaymentTotal:        res.PaymentTotal.String(),
		PendingPaymentCount: res.PendingPaymentCount,
		Balances:            make([]balanceRowView, 0, len(res.Balances)),
		Drill: drillView{
			BatchIDs:    res.Drill.BatchIDs,
			DiffIDs:     res.Drill.DiffIDs,
			BankFlowIDs: res.Drill.BankFlowIDs,
			PaymentIDs:  res.Drill.PaymentIDs,
		},
	}
	for _, r := range res.Receipts {
		view.Receipts = append(view.Receipts, receiptRowView{Method: r.Method, Amount: r.Amount.String(), TxnCount: r.TxnCount})
	}
	for _, b := range res.Balances {
		view.Balances = append(view.Balances, balanceRowView{
			AccountType: b.AccountType, AccountKey: b.AccountKey, Name: b.Name,
			PeriodStart: b.PeriodStart, PeriodEnd: b.PeriodEnd,
			Opening: b.Opening.String(), Inflow: b.Inflow.String(), Outflow: b.Outflow.String(),
			Fee: b.Fee.String(), Closing: b.Closing.String(),
		})
	}
	c.JSON(http.StatusOK, view)
}

// trendPointView 是看板趋势点（金额字符串）。
type trendPointView struct {
	Date   string `json:"date"`
	Count  int    `json:"count"`
	Amount string `json:"amount"`
}

// dashboardDiffView 是未匹配清单行（金额字符串）。
type dashboardDiffView struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Channel    string `json:"channel"`
	Expected   string `json:"expected"`
	Actual     string `json:"actual"`
	DiffAmount string `json:"diff_amount"`
	Status     string `json:"status"`
}

// dashboardView 是看板响应（金额字符串）。
type dashboardView struct {
	BranchID      string              `json:"branch_id"`
	Days          int                 `json:"days"`
	From          string              `json:"from"`
	To            string              `json:"to"`
	MatchRate     string              `json:"match_rate"`
	BatchCount    int                 `json:"batch_count"`
	OpenDiffCount int                 `json:"open_diff_count"`
	Trend         []trendPointView    `json:"trend"`
	Unmatched     []dashboardDiffView `json:"unmatched"`
}

// DashboardReport GET /reports/dashboard?days=7|30
func (h *Handler) DashboardReport(c *gin.Context) {
	days := 7
	if raw := strings.TrimSpace(c.Query("days")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || (n != 7 && n != 30) {
			writeError(c, http.StatusBadRequest, "bad_request", "days 只能是 7 或 30")
			return
		}
		days = n
	}
	res, err := h.svc.Dashboard(c.Request.Context(), branchIDOf(c), days)
	if err != nil {
		mapErr(c, err)
		return
	}
	view := dashboardView{
		BranchID:      res.BranchID,
		Days:          res.Days,
		From:          res.From,
		To:            res.To,
		MatchRate:     res.MatchRate.String(),
		BatchCount:    res.BatchCount,
		OpenDiffCount: res.OpenDiffCount,
		Trend:         make([]trendPointView, 0, len(res.Trend)),
		Unmatched:     make([]dashboardDiffView, 0, len(res.Unmatched)),
	}
	for _, p := range res.Trend {
		view.Trend = append(view.Trend, trendPointView{Date: p.Date, Count: p.Count, Amount: p.Amount.String()})
	}
	for _, d := range res.Unmatched {
		view.Unmatched = append(view.Unmatched, dashboardDiffView{
			ID: d.ID, Kind: d.Kind, Channel: d.Channel,
			Expected: d.Expected.String(), Actual: d.Actual.String(),
			DiffAmount: d.DiffAmount.String(), Status: d.Status,
		})
	}
	c.JSON(http.StatusOK, view)
}

// ---- CSV 导出 ----

// ExportCSV GET /exports/:kind?from=&to=
func (h *Handler) ExportCSV(c *gin.Context) {
	kind := strings.TrimSpace(c.Param("kind"))
	if !service.ValidExportKind(kind) {
		writeError(c, http.StatusBadRequest, "bad_request", "kind 只能是 daily/cash_variance/payables/diff_log")
		return
	}
	actor := operatorOf(c)
	if actor == "" {
		writeError(c, http.StatusUnauthorized, "unauthenticated", "缺少调用者身份")
		return
	}

	var from, to time.Time
	if kind != service.ExportPayables {
		var ok bool
		from, to, ok = cashRange(c)
		if !ok {
			return
		}
	}
	res, err := h.svc.ExportCSV(c.Request.Context(), branchIDOf(c), kind, from, to, actor)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="`+res.FileName+`"`)
	c.Data(http.StatusOK, "text/csv; charset=utf-8", res.Data)
}
