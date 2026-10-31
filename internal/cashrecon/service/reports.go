// Package service / reports.go —— 资金日报 / 看板 / CSV 导出（FR-14/FR-15，计划 §12）。
//
// # 汇总 = 明细
//
// 日报的每个数字都同时返回**下钻 id 列表**（drill），单测断言"汇总值等于明细之和"；
// 余额列一律取 `recon_period_snapshots`，不实时累算 —— 日报与快照逐项可勾稽。
//
// # 导出
//
// CSV 统一 `\ufeff` BOM + `text/csv; charset=utf-8`（Excel 打开不乱码）；
// kind ∈ daily / cash_variance / payables / diff_log；每次导出写审计日志。
package service

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/cashrecon/model"
)

// 导出类型。
const (
	ExportDaily        = "daily"
	ExportCashVariance = "cash_variance"
	ExportPayables     = "payables"
	ExportDiffLog      = "diff_log"
)

// ValidExportKind 判断导出类型是否合法。
func ValidExportKind(kind string) bool {
	switch kind {
	case ExportDaily, ExportCashVariance, ExportPayables, ExportDiffLog:
		return true
	}
	return false
}

// DailyReceiptRow 是某支付方式的当日收款额。
type DailyReceiptRow struct {
	Method   string          `json:"method"`
	Amount   decimal.Decimal `json:"amount"`
	TxnCount int             `json:"txn_count"`
}

// DailyBalanceRow 是某账户当期快照的余额。
type DailyBalanceRow struct {
	AccountType string          `json:"account_type"`
	AccountKey  string          `json:"account_key"`
	Name        string          `json:"name"`
	PeriodStart string          `json:"period_start"`
	PeriodEnd   string          `json:"period_end"`
	Opening     decimal.Decimal `json:"opening"`
	Inflow      decimal.Decimal `json:"inflow"`
	Outflow     decimal.Decimal `json:"outflow"`
	Fee         decimal.Decimal `json:"fee"`
	Closing     decimal.Decimal `json:"closing"`
}

// DailyDrill 是日报各数字的下钻 id（前端点数字跳明细）。
type DailyDrill struct {
	BatchIDs    []string `json:"batch_ids"`
	DiffIDs     []string `json:"diff_ids"`
	BankFlowIDs []string `json:"bank_flow_ids"`
	PaymentIDs  []string `json:"payment_ids"`
}

// DailyReport 是 `GET /reports/daily` 的响应。
type DailyReport struct {
	BranchID string `json:"branch_id"`
	Date     string `json:"date"`

	Receipts     []DailyReceiptRow `json:"receipts"`
	ReceiptTotal decimal.Decimal   `json:"receipt_total"`
	CashTotal    decimal.Decimal   `json:"cash_total"`
	BankTotal    decimal.Decimal   `json:"bank_total"`
	FeeTotal     decimal.Decimal   `json:"fee_total"`

	DiffCount     int             `json:"diff_count"`
	OpenDiffCount int             `json:"open_diff_count"`
	DiffAmount    decimal.Decimal `json:"diff_amount"`

	CashOverAmount  decimal.Decimal `json:"cash_over_amount"`
	CashShortAmount decimal.Decimal `json:"cash_short_amount"`

	PaymentTotal        decimal.Decimal `json:"payment_total"`
	PendingPaymentCount int             `json:"pending_payment_count"`

	Balances []DailyBalanceRow `json:"balances"`
	Drill    DailyDrill        `json:"drill"`
}

// Daily 生成某门店某日的资金日报。
func (s *Service) Daily(ctx context.Context, branchID string, date time.Time) (DailyReport, error) {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" || date.IsZero() {
		return DailyReport{}, model.ErrInvalidInput
	}
	day := dayOnly(date)
	out := DailyReport{
		BranchID: branchID,
		Date:     day.Format("2006-01-02"),
		Receipts: make([]DailyReceiptRow, 0),
		Balances: make([]DailyBalanceRow, 0),
		Drill:    DailyDrill{BatchIDs: []string{}, DiffIDs: []string{}, BankFlowIDs: []string{}, PaymentIDs: []string{}},
	}

	// ---- 收款：按支付方式聚合（cube / 本地同一口径）----
	rows, _, err := s.CashDayRows(ctx, branchID, day, day)
	if err != nil {
		return DailyReport{}, err
	}
	byMethod := map[string]*DailyReceiptRow{}
	for _, r := range rows {
		if r.Method == model.MethodChange {
			continue // 找零不是收款
		}
		item, ok := byMethod[r.Method]
		if !ok {
			item = &DailyReceiptRow{Method: r.Method}
			byMethod[r.Method] = item
		}
		item.Amount = item.Amount.Add(r.AmountNet)
		item.TxnCount += r.TxnCount
		out.ReceiptTotal = out.ReceiptTotal.Add(r.AmountNet)
		if r.Method == model.MethodCash {
			out.CashTotal = out.CashTotal.Add(r.AmountNet)
		}
	}
	methods := make([]string, 0, len(byMethod))
	for k := range byMethod {
		methods = append(methods, k)
	}
	sortStrings(methods)
	for _, m := range methods {
		out.Receipts = append(out.Receipts, *byMethod[m])
	}

	// ---- 银行到账 ----
	var bank []model.ReconBankFlow
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND value_date = ?", branchID, day).
		Order("channel, id").Find(&bank).Error; err != nil {
		return DailyReport{}, err
	}
	for _, b := range bank {
		out.BankTotal = out.BankTotal.Add(b.Amount)
		out.FeeTotal = out.FeeTotal.Add(b.Fee)
		out.Drill.BankFlowIDs = append(out.Drill.BankFlowIDs, b.ID)
	}

	// ---- 差异（批次区间覆盖当日）----
	batches, err := s.batchesOverlapping(ctx, branchID, day, day)
	if err != nil {
		return DailyReport{}, err
	}
	for _, b := range batches {
		out.Drill.BatchIDs = append(out.Drill.BatchIDs, b.ID)
	}
	diffs, err := s.diffsOfBatches(ctx, branchID, batchIDsOf(batches))
	if err != nil {
		return DailyReport{}, err
	}
	for _, d := range diffs {
		out.DiffCount++
		out.DiffAmount = out.DiffAmount.Add(d.DiffAmount)
		if d.Status == model.DiffStatusOpen {
			out.OpenDiffCount++
		}
		out.Drill.DiffIDs = append(out.Drill.DiffIDs, d.ID)
	}

	// ---- 长短款 ----
	var variances []model.ReconCashVariance
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date = ?", branchID, day).
		Find(&variances).Error; err != nil {
		return DailyReport{}, err
	}
	for _, v := range variances {
		switch v.Kind {
		case model.VarianceKindOver:
			out.CashOverAmount = out.CashOverAmount.Add(v.Amount)
		case model.VarianceKindShort:
			out.CashShortAmount = out.CashShortAmount.Add(v.Amount)
		}
	}

	// ---- 付款 ----
	var payments []model.ReconPaymentRecord
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND pay_date = ?", branchID, day).
		Find(&payments).Error; err != nil {
		return DailyReport{}, err
	}
	for _, p := range payments {
		out.PaymentTotal = out.PaymentTotal.Add(p.Amount)
		out.Drill.PaymentIDs = append(out.Drill.PaymentIDs, p.ID)
	}
	var pending int64
	if err := s.db.WithContext(ctx).Model(&model.ReconPaymentRecord{}).
		Where("branch_id = ? AND verified = ?", branchID, false).
		Count(&pending).Error; err != nil {
		return DailyReport{}, err
	}
	out.PendingPaymentCount = int(pending)

	// ---- 余额：只取快照（不实时累算）----
	var snaps []model.ReconPeriodSnapshot
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND period_start <= ? AND period_end >= ?", branchID, day, day).
		Order("account_type, account_key").Find(&snaps).Error; err != nil {
		return DailyReport{}, err
	}
	names := map[string]string{}
	if accounts, err := s.ListAccounts(ctx, branchID, false); err != nil {
		return DailyReport{}, err
	} else {
		for _, a := range accounts {
			names[a.AccountType+"|"+a.AccountKey] = a.Name
		}
	}
	for _, sn := range snaps {
		out.Balances = append(out.Balances, DailyBalanceRow{
			AccountType: sn.AccountType,
			AccountKey:  sn.AccountKey,
			Name:        names[sn.AccountType+"|"+sn.AccountKey],
			PeriodStart: sn.PeriodStart.Format("2006-01-02"),
			PeriodEnd:   sn.PeriodEnd.Format("2006-01-02"),
			Opening:     sn.Opening,
			Inflow:      sn.Inflow,
			Outflow:     sn.Outflow,
			Fee:         sn.Fee,
			Closing:     sn.Closing,
		})
	}
	return out, nil
}

// TrendPoint 是看板某日的差异趋势点。
type TrendPoint struct {
	Date   string          `json:"date"`
	Count  int             `json:"count"`
	Amount decimal.Decimal `json:"amount"`
}

// DashboardDiff 是未匹配清单的一行。
type DashboardDiff struct {
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	Channel    string          `json:"channel"`
	Expected   decimal.Decimal `json:"expected"`
	Actual     decimal.Decimal `json:"actual"`
	DiffAmount decimal.Decimal `json:"diff_amount"`
	Status     string          `json:"status"`
}

// Dashboard 是 `GET /reports/dashboard` 的响应。
type Dashboard struct {
	BranchID      string          `json:"branch_id"`
	Days          int             `json:"days"`
	From          string          `json:"from"`
	To            string          `json:"to"`
	MatchRate     decimal.Decimal `json:"match_rate"`
	BatchCount    int             `json:"batch_count"`
	OpenDiffCount int             `json:"open_diff_count"`
	Trend         []TrendPoint    `json:"trend"`
	Unmatched     []DashboardDiff `json:"unmatched"`
}

// Dashboard 生成 7/30 日看板：匹配率、差异趋势、未匹配清单。
func (s *Service) Dashboard(ctx context.Context, branchID string, days int) (Dashboard, error) {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		return Dashboard{}, model.ErrInvalidInput
	}
	if days != 7 && days != 30 {
		return Dashboard{}, fmt.Errorf("%w: days 只能是 7 或 30", model.ErrInvalidInput)
	}
	to := dayOnly(s.now())
	from := to.AddDate(0, 0, -(days - 1))
	out := Dashboard{
		BranchID:  branchID,
		Days:      days,
		From:      from.Format("2006-01-02"),
		To:        to.Format("2006-01-02"),
		Trend:     make([]TrendPoint, 0, days),
		Unmatched: make([]DashboardDiff, 0),
	}

	batches, err := s.batchesOverlapping(ctx, branchID, from, to)
	if err != nil {
		return Dashboard{}, err
	}
	out.BatchCount = len(batches)
	rateSum := decimal.Zero
	for _, b := range batches {
		rateSum = rateSum.Add(b.MatchRate)
	}
	if len(batches) > 0 {
		out.MatchRate = rateSum.Div(decimal.NewFromInt(int64(len(batches)))).Round(4)
	}

	diffs, err := s.diffsOfBatches(ctx, branchID, batchIDsOf(batches))
	if err != nil {
		return Dashboard{}, err
	}
	// 差异无 biz_date ⇒ 按所属批次的**区间末日**归属到某一天（同一批次不重复计数）。
	byDay := map[string]*TrendPoint{}
	for i := from; !i.After(to); i = i.AddDate(0, 0, 1) {
		p := &TrendPoint{Date: i.Format("2006-01-02")}
		byDay[p.Date] = p
		out.Trend = append(out.Trend, *p)
	}
	batchEnd := map[string]time.Time{}
	for _, b := range batches {
		batchEnd[b.ID] = dayOnly(b.BizDateTo)
	}
	for _, d := range diffs {
		end, ok := batchEnd[d.BatchID]
		if !ok {
			continue
		}
		p, ok := byDay[end.Format("2006-01-02")]
		if !ok {
			continue
		}
		p.Count++
		p.Amount = p.Amount.Add(d.DiffAmount.Abs())
		if d.Status == model.DiffStatusOpen {
			out.OpenDiffCount++
		}
	}
	// map 里的指针改完后再回写切片。
	for i := range out.Trend {
		if p, ok := byDay[out.Trend[i].Date]; ok {
			out.Trend[i] = *p
		}
	}

	for _, d := range diffs {
		if d.Status != model.DiffStatusOpen {
			continue
		}
		if len(out.Unmatched) >= 20 {
			break
		}
		out.Unmatched = append(out.Unmatched, DashboardDiff{
			ID:         d.ID,
			Kind:       d.Kind,
			Channel:    d.Channel,
			Expected:   d.Expected,
			Actual:     d.Actual,
			DiffAmount: d.DiffAmount,
			Status:     d.Status,
		})
	}
	return out, nil
}

// batchesOverlapping 取与 [from,to] 有交集的批次。
func (s *Service) batchesOverlapping(ctx context.Context, branchID string, from, to time.Time) ([]model.ReconBatch, error) {
	var rows []model.ReconBatch
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date_from <= ? AND biz_date_to >= ?",
			branchID, dayOnly(to), dayOnly(from)).
		Order("biz_date_from, biz_date_to, created_at").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// batchIDsOf 提取批次 id。
func batchIDsOf(rows []model.ReconBatch) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

// diffsOfBatches 取批次集合里的差异（无批次 ⇒ 空）。
func (s *Service) diffsOfBatches(ctx context.Context, branchID string, batchIDs []string) ([]model.ReconDiff, error) {
	if len(batchIDs) == 0 {
		return nil, nil
	}
	var rows []model.ReconDiff
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND batch_id IN ?", branchID, batchIDs).
		Order("created_at, id").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// ExportResult 是导出的产物。
type ExportResult struct {
	FileName string `json:"file_name"`
	Rows     int    `json:"rows"`
	Data     []byte `json:"-"`
}

// ExportCSV 生成 CSV（UTF-8 BOM），并写导出审计。
//
// from / to 对 payables 可为零值（应付是当前时点台账）；其余类型必须给区间。
func (s *Service) ExportCSV(
	ctx context.Context, branchID, kind string, from, to time.Time, actor string,
) (ExportResult, error) {
	branchID = strings.TrimSpace(branchID)
	kind = strings.TrimSpace(kind)
	actor = strings.TrimSpace(actor)
	if branchID == "" || actor == "" || !ValidExportKind(kind) {
		return ExportResult{}, model.ErrInvalidInput
	}
	if kind != ExportPayables {
		if err := s.validateRange(branchID, from, to); err != nil {
			return ExportResult{}, err
		}
	}

	var header []string
	var records [][]string
	var err error
	switch kind {
	case ExportDaily:
		header, records, err = s.exportDaily(ctx, branchID, from, to)
	case ExportCashVariance:
		header, records, err = s.exportCashVariance(ctx, branchID, from, to)
	case ExportPayables:
		header, records, err = s.exportPayables(ctx, branchID)
	case ExportDiffLog:
		header, records, err = s.exportDiffLog(ctx, branchID, from, to)
	}
	if err != nil {
		return ExportResult{}, err
	}

	var buf bytes.Buffer
	buf.WriteString("\ufeff") // UTF-8 BOM：Excel 直接打开不乱码
	w := csv.NewWriter(&buf)
	if err := w.Write(header); err != nil {
		return ExportResult{}, err
	}
	if err := w.WriteAll(records); err != nil {
		return ExportResult{}, err
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return ExportResult{}, err
	}

	now := s.now()
	res := ExportResult{
		FileName: fmt.Sprintf("cash-recon-%s-%s.csv", kind, now.Format("20060102-150405")),
		Rows:     len(records),
		Data:     buf.Bytes(),
	}
	_ = s.writeAudit(ctx, model.ReconAuditLog{
		BranchID:   branchID,
		ObjectType: "export",
		ObjectID:   kind,
		Action:     "export",
		AfterJSON: fmt.Sprintf(`{"kind":%q,"from":%q,"to":%q,"rows":%d,"file_name":%q}`,
			kind, dayOnly(from).Format("2006-01-02"), dayOnly(to).Format("2006-01-02"), len(records), res.FileName),
		Reason:    "CSV 导出",
		Actor:     actor,
		CreatedAt: now,
	})
	return res, nil
}

// exportDaily 导出资金日报（来自期末快照，逐账户一行）。
func (s *Service) exportDaily(ctx context.Context, branchID string, from, to time.Time) ([]string, [][]string, error) {
	var rows []model.ReconPeriodSnapshot
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND period_start >= ? AND period_start <= ?", branchID, dayOnly(from), dayOnly(to)).
		Order("period_start, account_type, account_key").Find(&rows).Error; err != nil {
		return nil, nil, err
	}
	header := []string{"period_start", "period_end", "account_type", "account_key",
		"opening", "opening_source", "adjustment_amount", "inflow", "outflow", "fee", "closing", "source_batch_id"}
	records := make([][]string, 0, len(rows))
	for _, r := range rows {
		records = append(records, []string{
			r.PeriodStart.Format("2006-01-02"), r.PeriodEnd.Format("2006-01-02"),
			r.AccountType, r.AccountKey,
			r.Opening.String(), r.OpeningSource, r.AdjustmentAmount.String(),
			r.Inflow.String(), r.Outflow.String(), r.Fee.String(), r.Closing.String(),
			r.SourceBatchID,
		})
	}
	return header, records, nil
}

// exportCashVariance 导出长短款台账。
func (s *Service) exportCashVariance(ctx context.Context, branchID string, from, to time.Time) ([]string, [][]string, error) {
	var rows []model.ReconCashVariance
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date >= ? AND biz_date <= ?", branchID, dayOnly(from), dayOnly(to)).
		Order("biz_date, cashier_id, kind").Find(&rows).Error; err != nil {
		return nil, nil, err
	}
	header := []string{"biz_date", "cashier_id", "kind", "amount", "reason_code", "status"}
	records := make([][]string, 0, len(rows))
	for _, r := range rows {
		records = append(records, []string{
			r.BizDate.Format("2006-01-02"), r.CashierID, r.Kind, r.Amount.String(),
			r.ReasonCode, r.Status,
		})
	}
	return header, records, nil
}

// exportPayables 导出供应商应付台账。
func (s *Service) exportPayables(ctx context.Context, branchID string) ([]string, [][]string, error) {
	res, err := s.SupplierPayables(ctx, branchID)
	if err != nil {
		return nil, nil, err
	}
	header := []string{"supplier", "unpaid_amount", "unpaid_count", "paid_amount", "paid_count"}
	records := make([][]string, 0, len(res.Items))
	for _, r := range res.Items {
		records = append(records, []string{
			r.Supplier, r.UnpaidAmount.String(), fmt.Sprintf("%d", r.UnpaidCount),
			r.PaidAmount.String(), fmt.Sprintf("%d", r.PaidCount),
		})
	}
	return header, records, nil
}

// exportDiffLog 导出差异处理日志。
func (s *Service) exportDiffLog(ctx context.Context, branchID string, from, to time.Time) ([]string, [][]string, error) {
	batches, err := s.batchesOverlapping(ctx, branchID, from, to)
	if err != nil {
		return nil, nil, err
	}
	diffs, err := s.diffsOfBatches(ctx, branchID, batchIDsOf(batches))
	if err != nil {
		return nil, nil, err
	}
	header := []string{"batch_id", "kind", "channel", "expected", "actual", "diff_amount",
		"status", "resolution_code", "resolution_note", "handled_by", "handled_at"}
	records := make([][]string, 0, len(diffs))
	for _, d := range diffs {
		handledAt := ""
		if d.HandledAt != nil {
			handledAt = d.HandledAt.Format("2006-01-02 15:04:05")
		}
		records = append(records, []string{
			d.BatchID, d.Kind, d.Channel, d.Expected.String(), d.Actual.String(), d.DiffAmount.String(),
			d.Status, d.ResolutionCode, d.ResolutionNote, d.HandledBy, handledAt,
		})
	}
	return header, records, nil
}
