// Package service / cube.go —— cube 数据访问层(sixun 结算/采购/销售/库存)。
//
// 全部查询经 cubeclient.DaprCubeClient → cube-router(按 X-Branch-ID 路由到
// 门店专属 cube 实例,live 实时查询)。
//
// 关键领域事实(勿再推翻,见 cubeclient.GetStock 注释):
//   - 不按 branch_id 过滤 cube 行(思迅 branch_no 与本系统 branch_id 无关);
//     门店隔离靠 X-Branch-ID 路由链,内部调用传提交时的 branch 即可。
//   - settlement_line 必须 filter source_doc_type in (PI, RO),否则会拆出
//     扣补(KB)/费用(FG)/联营(LY)行,污染采购核对。
package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/cubeclient"
)

// CubeQuerier 是流水线对 cube 的依赖面(单测注入 fake)。
type CubeQuerier interface {
	// ListSettlements 按 id 批量查结算单头。
	ListSettlements(ctx context.Context, branchID string, ids []string) ([]SettlementRow, error)
	// ListAuditedSettlements 查时间段内已审核的采购结款单(H5 勾选数据源)。
	ListAuditedSettlements(ctx context.Context, branchID, from, to, supplierID string, limit int) ([]SettlementRow, error)
	// ListPurchaseLines 查一张结算单关联的采购进货/退货行(仅 PI/RO)。
	ListPurchaseLines(ctx context.Context, branchID, settlementID string) ([]SettlementLineRow, error)
	// ListPurchaseSheets 按 id 批量查采购单头。
	ListPurchaseSheets(ctx context.Context, branchID string, ids []string) ([]PurchaseSheetRow, error)
	// ListSheetLines 按 sheet id 批量查采购单行(商品粒度)。
	ListSheetLines(ctx context.Context, branchID string, sheetIDs []string) ([]SheetLineRow, error)
	// SumSalesByProduct 拉商品近 N 天销售汇总(net 销量/销额)。
	SumSalesByProduct(ctx context.Context, branchID string, productIDs []string, fromDate, toDate string) ([]SaleRow, error)
	// GetStocks 拉商品当前库存。
	GetStocks(ctx context.Context, branchID string, productIDs []string) ([]StockRow, error)
}

// ---- 行 DTO ----

// SettlementRow cube settlement 头行。
type SettlementRow struct {
	ID             string
	SupplierID     string
	DocumentType   string // DP/CP
	ApproveStatus  string // "1"=已审核
	PeriodStart    string
	PeriodEnd      string
	TotalGoodsYuan decimal.Decimal // 货款净额(勾稽用这个)
}

// SettlementLineRow cube settlement_line 行(仅 PI/RO)。
type SettlementLineRow struct {
	SettlementID    string
	VoucherID       string // 采购单号(PI…/RO…)
	SourceDocType   string // PI/RO
	TotalAmountYuan decimal.Decimal
	TotalGoodsYuan  decimal.Decimal
}

// PurchaseSheetRow cube purchase_sheet 头行。
type PurchaseSheetRow struct {
	ID                 string
	SupplierID         string
	DocumentType       string // PI/RO
	ApproveStatus      string // "1"=已审核
	TotalAmountYuan    decimal.Decimal
	TotalNetAmountYuan decimal.Decimal
}

// SheetLineRow cube purchase_sheet_line 行(商品粒度)。
type SheetLineRow struct {
	SheetID          string
	ProductID        string
	TotalAmountYuan  decimal.Decimal
	ReceivedQuantity decimal.Decimal
	OrderQuantity    decimal.Decimal
}

// SaleRow 商品销售窗口汇总。
type SaleRow struct {
	ProductID         string
	NetSaleQuantity   decimal.Decimal
	NetSaleAmountYuan decimal.Decimal
}

// StockRow 商品库存快照。
type StockRow struct {
	ProductID string
	Quantity  decimal.Decimal
	AvgCost   decimal.Decimal
}

// ---- DaprCubeQuerier ----

// DaprCubeQuerier CubeQuerier 的 cubeclient 实现。
type DaprCubeQuerier struct {
	c *cubeclient.DaprCubeClient
}

// NewDaprCubeQuerier 包装 cubeclient。
func NewDaprCubeQuerier(c *cubeclient.DaprCubeClient) *DaprCubeQuerier {
	return &DaprCubeQuerier{c: c}
}

// ctxWithBranch 透传 branch。bearer 不在此处处理:
//   - 请求路径(H5 勾选数据源)由 main.go 中间件把请求 JWT 注入 ctx;
//   - 流水线路径由 runAudit 从实例记录注入申请人 JWT。
// cube 实例 sidecar 的 middleware.http.bearer 强制 JWT,空 bearer 必被
// Unauthenticated 拒(2026-10-10 契约测试红过一次:此处曾 WithBearer(ctx,"")
// 把中间件注入的 JWT 覆盖掉)。
func ctxWithBranch(ctx context.Context, branchID string) context.Context {
	if branchID != "" {
		ctx = cubeclient.WithBranchID(ctx, branchID)
	}
	return ctx
}

func rowStr(row map[string]any, key string) string {
	if v, ok := row[key]; ok && v != nil {
		return fmt.Sprint(v)
	}
	return ""
}

func rowDec(row map[string]any, key string) decimal.Decimal {
	return cubeclient.ParseDecimal(row[key])
}

// ListSettlements 按 id in (...) 查 settlement。
func (q *DaprCubeQuerier) ListSettlements(ctx context.Context, branchID string, ids []string) ([]SettlementRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	vals := make([]any, 0, len(ids))
	for _, id := range ids {
		vals = append(vals, id)
	}
	data, err := q.c.LoadCubeQuery(ctxWithBranch(ctx, branchID), "settlement", cubeclient.CubeQuery{
		Measures: []string{"settlement.total_goods_yuan"},
		Dimensions: []string{
			"settlement.id", "settlement.supplier_id", "settlement.document_type",
			"settlement.approve_status", "settlement.period_start", "settlement.period_end",
		},
		Filters: []cubeclient.CubeFilter{
			{Member: "settlement.id", Operator: "in", Values: vals},
		},
		Limit: len(ids) * 4,
	})
	if err != nil {
		return nil, fmt.Errorf("cube settlement query: %w", err)
	}
	out := make([]SettlementRow, 0, len(data))
	for _, r := range data {
		out = append(out, SettlementRow{
			ID:             rowStr(r, "settlement.id"),
			SupplierID:     rowStr(r, "settlement.supplier_id"),
			DocumentType:   rowStr(r, "settlement.document_type"),
			ApproveStatus:  rowStr(r, "settlement.approve_status"),
			PeriodStart:    rowStr(r, "settlement.period_start"),
			PeriodEnd:      rowStr(r, "settlement.period_end"),
			TotalGoodsYuan: rowDec(r, "settlement.total_goods_yuan"),
		})
	}
	return out, nil
}

// cubeSettlementFetchCap 是单次从 cube 拉取已审核结算单的行数上限。
//
// cube /query 协议(measures/dimensions/filters/limit)没有 ORDER BY,返回源库
// 自然序(实测 ≈ 单号升序,即最老在前)。H5 要"最新的 limit 张",在 Go 侧只排
// limit 行是错的——cube TOP limit 给回来的是**最老的** limit 行,排序救不了
// 窗口截断(实测 4353 张已审核单,H5 永远只看到 2021 年的最老 200 张)。
// 所以固定拉满 5000 行,Go 侧排序后截断到 limit。超上限时窗口退化为
// "自然序前 5000 张内排序",对当前数据量(4353)无影响。
const cubeSettlementFetchCap = 5000

// ListAuditedSettlements H5 勾选数据源:approve_status=1 且 document_type in (DP,CP)。
//
// 返回按账期结束日倒序(新的在前),最多 limit 张。
func (q *DaprCubeQuerier) ListAuditedSettlements(ctx context.Context, branchID, from, to, supplierID string, limit int) ([]SettlementRow, error) {
	filters := []cubeclient.CubeFilter{
		{Member: "settlement.approve_status", Operator: "equals", Values: []any{"1"}},
		{Member: "settlement.document_type", Operator: "in", Values: []any{"DP", "CP"}},
	}
	if from != "" {
		filters = append(filters, cubeclient.CubeFilter{Member: "settlement.settled_at", Operator: "gte", Values: []any{from}})
	}
	if to != "" {
		filters = append(filters, cubeclient.CubeFilter{Member: "settlement.settled_at", Operator: "lt", Values: []any{to}})
	}
	if supplierID != "" {
		filters = append(filters, cubeclient.CubeFilter{Member: "settlement.supplier_id", Operator: "equals", Values: []any{supplierID}})
	}
	if limit <= 0 {
		limit = 200
	}
	// 拉满 cap 行再排序截断,而不是拉 limit 行 —— 见 cubeSettlementFetchCap 注释。
	fetch := limit
	if fetch < cubeSettlementFetchCap {
		fetch = cubeSettlementFetchCap
	}
	data, err := q.c.LoadCubeQuery(ctxWithBranch(ctx, branchID), "settlement", cubeclient.CubeQuery{
		Measures: []string{"settlement.total_goods_yuan"},
		Dimensions: []string{
			"settlement.id", "settlement.supplier_id", "settlement.document_type",
			"settlement.approve_status", "settlement.period_start", "settlement.period_end",
		},
		Filters: filters,
		Limit:   fetch,
	})
	if err != nil {
		return nil, fmt.Errorf("cube settlement list query: %w", err)
	}
	out := make([]SettlementRow, 0, len(data))
	for _, r := range data {
		out = append(out, SettlementRow{
			ID:             rowStr(r, "settlement.id"),
			SupplierID:     rowStr(r, "settlement.supplier_id"),
			DocumentType:   rowStr(r, "settlement.document_type"),
			ApproveStatus:  rowStr(r, "settlement.approve_status"),
			// MSSQL live 源的 datetime 是 "09  1 2021 12:00AM" 这种串,展示与
			// 排序都不友好,统一规范成 "2006-01-02"(解析失败保留原串)。
			PeriodStart:    normalizeCubeDate(rowStr(r, "settlement.period_start")),
			PeriodEnd:      normalizeCubeDate(rowStr(r, "settlement.period_end")),
			TotalGoodsYuan: rowDec(r, "settlement.total_goods_yuan"),
		})
	}
	sortSettlementRowsDesc(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// parseCubeTime 解析 cube 日期串,覆盖两种来源:
//   - MSSQL live 源(sixun 直查):"09  1 2021 12:00AM" —— go-mssqldb 把 datetime
//     RTRIM 成 "01 _2 2006 3:04PM" 形态(月/日空格补位),布局 "01 _2 2006 3:04PM"。
//   - 同步缓存源/ISO:"2021-09-01 [12:00:00]"。
//
// 解析失败返 (零值, false),调用方按"无日期"处理。
func parseCubeTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		"01 _2 2006 3:04PM",
		"1 _2 2006 3:04PM", // 月不补零兜底(实测源数据月恒为 2 位,防脏数据)
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// normalizeCubeDate 把 cube 日期串规范成 "2006-01-02"(解析失败保留原串)。
func normalizeCubeDate(s string) string {
	if t, ok := parseCubeTime(s); ok {
		return t.Format("2006-01-02")
	}
	return strings.TrimSpace(s)
}

// sortSettlementRowsDesc 按账期结束日倒序(新的在前);结束日相同看开始日;
// 再相同按单号倒序(sixun 单号流水递增,倒序即新单在前)。解析不出日期的行排最后。
func sortSettlementRowsDesc(rows []SettlementRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		aEnd, aok := parseCubeTime(a.PeriodEnd)
		bEnd, bok := parseCubeTime(b.PeriodEnd)
		if aok != bok {
			return aok // 有日期的在前
		}
		if aok && !aEnd.Equal(bEnd) {
			return aEnd.After(bEnd)
		}
		aStart, aok2 := parseCubeTime(a.PeriodStart)
		bStart, bok2 := parseCubeTime(b.PeriodStart)
		if aok2 != bok2 {
			return aok2
		}
		if aok2 && !aStart.Equal(bStart) {
			return aStart.After(bStart)
		}
		return a.ID > b.ID
	})
}

// ListPurchaseLines 拆结算单:仅 PI(进货)/RO(退货)行。
//
// ⚠️ source_doc_type in (PI,RO) 是必须条件 —— 不带会把扣补/费用/联营行
// 混进采购核对,金额勾稽永远对不上。
func (q *DaprCubeQuerier) ListPurchaseLines(ctx context.Context, branchID, settlementID string) ([]SettlementLineRow, error) {
	data, err := q.c.LoadCubeQuery(ctxWithBranch(ctx, branchID), "settlement_line", cubeclient.CubeQuery{
		Measures: []string{"settlement_line.total_amount_yuan", "settlement_line.total_goods_yuan"},
		Dimensions: []string{
			"settlement_line.settlement_id", "settlement_line.voucher_id", "settlement_line.source_doc_type",
		},
		Filters: []cubeclient.CubeFilter{
			{Member: "settlement_line.settlement_id", Operator: "equals", Values: []any{settlementID}},
			{Member: "settlement_line.source_doc_type", Operator: "in", Values: []any{"PI", "RO"}},
		},
		Limit: 1000,
	})
	if err != nil {
		return nil, fmt.Errorf("cube settlement_line query: %w", err)
	}
	out := make([]SettlementLineRow, 0, len(data))
	for _, r := range data {
		out = append(out, SettlementLineRow{
			SettlementID:    rowStr(r, "settlement_line.settlement_id"),
			VoucherID:       strings.TrimSpace(rowStr(r, "settlement_line.voucher_id")),
			SourceDocType:   rowStr(r, "settlement_line.source_doc_type"),
			TotalAmountYuan: rowDec(r, "settlement_line.total_amount_yuan"),
			TotalGoodsYuan:  rowDec(r, "settlement_line.total_goods_yuan"),
		})
	}
	return out, nil
}

// ListPurchaseSheets 按 id in (...) 查 purchase_sheet。
func (q *DaprCubeQuerier) ListPurchaseSheets(ctx context.Context, branchID string, ids []string) ([]PurchaseSheetRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	vals := make([]any, 0, len(ids))
	for _, id := range ids {
		vals = append(vals, id)
	}
	data, err := q.c.LoadCubeQuery(ctxWithBranch(ctx, branchID), "purchase_sheet", cubeclient.CubeQuery{
		Measures: []string{"purchase_sheet.total_amount_yuan", "purchase_sheet.total_net_amount_yuan"},
		Dimensions: []string{
			"purchase_sheet.id", "purchase_sheet.supplier_id",
			"purchase_sheet.document_type", "purchase_sheet.approve_status",
		},
		Filters: []cubeclient.CubeFilter{
			{Member: "purchase_sheet.id", Operator: "in", Values: vals},
		},
		Limit: len(ids) * 4,
	})
	if err != nil {
		return nil, fmt.Errorf("cube purchase_sheet query: %w", err)
	}
	out := make([]PurchaseSheetRow, 0, len(data))
	for _, r := range data {
		out = append(out, PurchaseSheetRow{
			ID:                 rowStr(r, "purchase_sheet.id"),
			SupplierID:         rowStr(r, "purchase_sheet.supplier_id"),
			DocumentType:       rowStr(r, "purchase_sheet.document_type"),
			ApproveStatus:      rowStr(r, "purchase_sheet.approve_status"),
			TotalAmountYuan:    rowDec(r, "purchase_sheet.total_amount_yuan"),
			TotalNetAmountYuan: rowDec(r, "purchase_sheet.total_net_amount_yuan"),
		})
	}
	return out, nil
}

// ListSheetLines 按 sheet_id in (...) 查 purchase_sheet_line(商品粒度)。
func (q *DaprCubeQuerier) ListSheetLines(ctx context.Context, branchID string, sheetIDs []string) ([]SheetLineRow, error) {
	if len(sheetIDs) == 0 {
		return nil, nil
	}
	vals := make([]any, 0, len(sheetIDs))
	for _, id := range sheetIDs {
		vals = append(vals, id)
	}
	data, err := q.c.LoadCubeQuery(ctxWithBranch(ctx, branchID), "purchase_sheet_line", cubeclient.CubeQuery{
		Measures: []string{
			"purchase_sheet_line.total_amount_yuan",
			"purchase_sheet_line.total_received_quantity",
			"purchase_sheet_line.total_order_quantity",
		},
		Dimensions: []string{"purchase_sheet_line.sheet_id", "purchase_sheet_line.product_id"},
		Filters: []cubeclient.CubeFilter{
			{Member: "purchase_sheet_line.sheet_id", Operator: "in", Values: vals},
		},
		Limit: 5000,
	})
	if err != nil {
		return nil, fmt.Errorf("cube purchase_sheet_line query: %w", err)
	}
	out := make([]SheetLineRow, 0, len(data))
	for _, r := range data {
		out = append(out, SheetLineRow{
			SheetID:          rowStr(r, "purchase_sheet_line.sheet_id"),
			ProductID:        rowStr(r, "purchase_sheet_line.product_id"),
			TotalAmountYuan:  rowDec(r, "purchase_sheet_line.total_amount_yuan"),
			ReceivedQuantity: rowDec(r, "purchase_sheet_line.total_received_quantity"),
			OrderQuantity:    rowDec(r, "purchase_sheet_line.total_order_quantity"),
		})
	}
	return out, nil
}

// SumSalesByProduct 拉商品 [fromDate, toDate) 销售汇总(YYYY-MM-DD)。
func (q *DaprCubeQuerier) SumSalesByProduct(ctx context.Context, branchID string, productIDs []string, fromDate, toDate string) ([]SaleRow, error) {
	if len(productIDs) == 0 {
		return nil, nil
	}
	vals := make([]any, 0, len(productIDs))
	for _, id := range productIDs {
		vals = append(vals, id)
	}
	data, err := q.c.LoadCubeQuery(ctxWithBranch(ctx, branchID), "sale_day", cubeclient.CubeQuery{
		Measures:   []string{"sale_day.net_sale_quantity", "sale_day.net_sale_amount_yuan"},
		Dimensions: []string{"sale_day.product_id"},
		Filters: []cubeclient.CubeFilter{
			{Member: "sale_day.product_id", Operator: "in", Values: vals},
			{Member: "sale_day.business_date", Operator: "gte", Values: []any{fromDate}},
			{Member: "sale_day.business_date", Operator: "lt", Values: []any{toDate}},
		},
		Limit: 5000,
	})
	if err != nil {
		return nil, fmt.Errorf("cube sale_day query: %w", err)
	}
	out := make([]SaleRow, 0, len(data))
	for _, r := range data {
		out = append(out, SaleRow{
			ProductID:         rowStr(r, "sale_day.product_id"),
			NetSaleQuantity:   rowDec(r, "sale_day.net_sale_quantity"),
			NetSaleAmountYuan: rowDec(r, "sale_day.net_sale_amount_yuan"),
		})
	}
	return out, nil
}

// GetStocks 拉商品库存快照。
func (q *DaprCubeQuerier) GetStocks(ctx context.Context, branchID string, productIDs []string) ([]StockRow, error) {
	if len(productIDs) == 0 {
		return nil, nil
	}
	vals := make([]any, 0, len(productIDs))
	for _, id := range productIDs {
		vals = append(vals, id)
	}
	data, err := q.c.LoadCubeQuery(ctxWithBranch(ctx, branchID), "stock", cubeclient.CubeQuery{
		Measures:   []string{"stock.total_quantity", "stock.avg_cost"},
		Dimensions: []string{"stock.product_id"},
		Filters: []cubeclient.CubeFilter{
			{Member: "stock.product_id", Operator: "in", Values: vals},
		},
		Limit: 5000,
	})
	if err != nil {
		return nil, fmt.Errorf("cube stock query: %w", err)
	}
	out := make([]StockRow, 0, len(data))
	for _, r := range data {
		out = append(out, StockRow{
			ProductID: rowStr(r, "stock.product_id"),
			Quantity:  rowDec(r, "stock.total_quantity"),
			AvgCost:   rowDec(r, "stock.avg_cost"),
		})
	}
	return out, nil
}

// ListCubeSettlements H5 勾选数据源:透传 cube 已审核采购结款单(cube 未配置 → 503)。
func (s *Service) ListCubeSettlements(ctx context.Context, branchID, from, to, supplierID string, limit int) ([]SettlementRow, error) {
	if s.cube == nil {
		return nil, ErrCubeUnavailable
	}
	return s.cube.ListAuditedSettlements(ctx, branchID, from, to, supplierID, limit)
}

// saleWindowDates 计算销售回看窗口 [from, to)(to = 今天,YYYY-MM-DD)。
func (s *Service) saleWindowDates() (from, to string) {
	now := s.now()
	to = now.Format("2006-01-02")
	from = now.AddDate(0, 0, -s.cfg.SaleWindowDays).Format("2006-01-02")
	return
}

var _ = time.Now // 静默引用(窗口计算预留)
