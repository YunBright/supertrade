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
	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// CubeQuerier 是流水线对 cube 的依赖面(单测注入 fake)。
type CubeQuerier interface {
	// ListSettlements 按 id 批量查结算单头。
	ListSettlements(ctx context.Context, branchID string, ids []string) ([]SettlementRow, error)
	// ListAuditedSettlements 查时间段内已审核的采购结款单(H5 勾选数据源)。
	ListAuditedSettlements(ctx context.Context, branchID, from, to, supplierID string, limit int) ([]SettlementRow, error)
	// ListSupplierNames 取供应商字典(id → 名称)。
	//
	// ⚠️ 这是**另一个 cube model** 的数据,不是 settlement 上的列:settlement 是
	// live model(实时打思迅源库),supplier 是 duck model(同步落 DuckDB)。
	// cube 的 JOIN 目前**只支持 live↔live**(跨 storage 明确报错,见 cube
	// pkg/cubequery/join.go),所以 supplier.name **拿不到** settlement 查询里 ——
	// 只能在 Go 侧 join,与 catalog 域取"商品→供应商名"同一套路。
	ListSupplierNames(ctx context.Context, branchID string) (map[string]string, error)
	// ListPurchaseLines 查一张结算单关联的采购进货/退货行(仅 PI/RO)。
	ListPurchaseLines(ctx context.Context, branchID, settlementID string) ([]SettlementLineRow, error)
	// ListPurchaseSheets 按 id 批量查采购单头(含制单日期,见 PurchaseSheetRow.CreatedAt)。
	ListPurchaseSheets(ctx context.Context, branchID string, ids []string) ([]PurchaseSheetRow, error)
	// ListSheetLines 按 sheet id 批量查采购单行(商品粒度)。
	ListSheetLines(ctx context.Context, branchID string, sheetIDs []string) ([]SheetLineRow, error)
	// SumSalesByProduct 拉商品 [fromDate, toDate) 销售汇总(net 销量/销额)。
	SumSalesByProduct(ctx context.Context, branchID string, productIDs []string, fromDate, toDate string) ([]SaleRow, error)
	// SumLossByProduct 拉商品 [fromDate, toDate) 的**库存单据损耗**分组行。
	//
	// ⚠️ 返回的是**未开口径的原始分组行**(按商品 + 单据类型 + 方向),
	//   口径由 aggregateLoss 收敛:JO(报损单)全部算损耗;OO(库存调整单)只算
	//   direction='-' 的那些('+' 是盘盈/补录,算成损耗会把偏离"解释"掉)。
	//
	// 读源按门店数据源分流,见 LossService(有源走本方法,无源查本域 PG)。
	SumLossByProduct(ctx context.Context, branchID string, productIDs []string, fromDate, toDate string) ([]LossRow, error)
	// ListProductNames 取商品名(货号 → 名称),给信号/拒绝理由用。
	ListProductNames(ctx context.Context, branchID string, productIDs []string) (map[string]string, error)
	// GetStocks 拉商品当前库存。
	GetStocks(ctx context.Context, branchID string, productIDs []string) ([]StockRow, error)
	// GetSettlementsByIDs 按**单号精确**查结算单头,**不加**审核/单据类型过滤。
	//
	// ⚠️ 与 ListAuditedSettlements 的区别就是"不过滤":
	//    诊断「这张单去哪了」时,必须能看到**未审核** / **非 DP·CP** 的单,
	//    否则只能回答"没找到",回答不了"为什么没找到"。
	GetSettlementsByIDs(ctx context.Context, branchID string, ids []string) ([]SettlementRow, error)
}

// ---- 行 DTO ----

// SettlementRow cube settlement 头行。
type SettlementRow struct {
	ID             string
	SupplierID     string
	SupplierName   string // 供应商名称(Go 侧 join supplier 字典补上;查不到为空)
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
	ID            string
	SupplierID    string
	DocumentType  string // PI/RO
	ApproveStatus string // "1"=已审核
	// CreatedAt 是采购入库单的制单日期(YYYY-MM-DD)。
	//
	// ⚠️ 它是数量信号的**销售窗口起点**:规则改成"自最后一次采购入库单日期到今天"
	// (2026-10-22 用户口径)—— 进货发生在哪天,就该从哪天开始看卖不卖得动,
	// 不能拿一个与进货无关的固定窗口(今天-30 天)去比。
	//
	// 源列 oper_date(思迅 t_pm_sheet_master)是**真 datetime**,
	// cube 返回形如 "08 23 2026  4:19PM" ⇒ 用 normalizeCubeDate 规范化。
	CreatedAt          string
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

// LossRow 是库存单据损耗的**分组行**(商品 × 单据类型 × 方向)。
//
// Quantity 恒为正(cube 侧取 ABS);方向由 Direction 表达('+' 增加库存 / '-' 减少库存)。
// 口径收敛见 aggregateLoss。
type LossRow struct {
	ProductID    string
	DocumentType string // JO 报损单 / OO 库存调整单
	Direction    string // '+' / '-'
	Quantity     decimal.Decimal
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
//
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

// settlementPeriodFilters 构造账期区间过滤。
//
// 提成纯函数只为让"用哪一列"能被单测钉住 —— 这是本页最容易被人顺手改回去的
// 地方,而改错了**不报错、不降级**,只是日期筛选静默失效(见
// TestSettlementPeriodFilters_UsesPeriodEndNotSettledAt)。
//
// 语义:[from, to),打 `settlement.period_end`(账期结束日)。
// 空串 = 不限,该端不产生 filter。
func settlementPeriodFilters(from, to string) []cubeclient.CubeFilter {
	var out []cubeclient.CubeFilter
	if from != "" {
		out = append(out, cubeclient.CubeFilter{Member: "settlement.period_end", Operator: "gte", Values: []any{from}})
	}
	if to != "" {
		out = append(out, cubeclient.CubeFilter{Member: "settlement.period_end", Operator: "lt", Values: []any{to}})
	}
	return out
}

// ListAuditedSettlements H5 勾选数据源:approve_status=1 且 document_type in (DP,CP)。
//
// 返回按账期结束日倒序(新的在前),最多 limit 张。
//
// # [from, to) 筛的是 **period_end(账期结束日)**,不是 settled_at
//
// 2026-10-19 改。之前筛的是 `settled_at`(结算日),两处都是错的:
//
//  1. **列选错了**。`settled_at` 在 ysx 源库 4379 张已审核结款单里**只有 1 张**
//     有值(其余为 NULL)—— 思迅的 CP 购销返利单根本不走结算日。
//     实测同一账期窗口:settled_at 命中 0 张,period_end 命中 113 张。
//     UI 上写的是「账期范围」,运营说的"9 月这些单我付过了"指的也是账期。
//
//  2. **算子选错了**(已在上游 cube 修掉,但这里必须记着为什么不能改回去):
//     `gte`/`lt` 打在真 datetime 列上,历史版本会生成 `RTRIM(end_date) >= ?`
//     —— 拿 RTRIM 之后的**文本**跟 ISO 字面量比大小。MSSQL 隐式把 datetime
//     转成 "09  1 2026 12:00AM" 再比,结果取决于 SET LANGUAGE,实测恒返 0 张。
//     cube 现在对 `type: time` 且非 char_date 的列自动 CAST 成 DATETIME 再比;
//     `settlement.period_end` 正属于这一类(char_date 才是 char 存 ISO,
//     那种列保持字符串比较才对,见 cube pkg/cubequery 的 cmp 用例)。
//
// ⚠️ 改这一行之前先确认列名:period_end(账期结束日)与 settled_at(结算日)
// 在思迅源表里是两个不同的列(end_date / settle_date),语义不可互换。
func (q *DaprCubeQuerier) ListAuditedSettlements(ctx context.Context, branchID, from, to, supplierID string, limit int) ([]SettlementRow, error) {
	filters := []cubeclient.CubeFilter{
		{Member: "settlement.approve_status", Operator: "equals", Values: []any{"1"}},
		{Member: "settlement.document_type", Operator: "in", Values: []any{"DP", "CP"}},
	}
	filters = append(filters, settlementPeriodFilters(from, to)...)
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
			ID:            rowStr(r, "settlement.id"),
			SupplierID:    rowStr(r, "settlement.supplier_id"),
			DocumentType:  rowStr(r, "settlement.document_type"),
			ApproveStatus: rowStr(r, "settlement.approve_status"),
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

// ListSupplierNames 取供应商字典(id → 名称)。
//
// 复用 cubeclient.ListSuppliers(同一个 cube `supplier` model 的字典查询,
// catalog 域也在用它)—— **不另写一份查询**:字典查询一旦有两份定义,
// 迟早有一份漏掉 limit/维度,表现为"部分供应商查不到名字"这种静默缺陷。
//
// ⚠️ supplier 是 **duck model**(同步落 DuckDB),不是 live:这条查询**不碰**
//
//	思迅源库,代价可以忽略(对比 settlement 的 live 查询要拉 5000 行)。
//
// ⚠️ 键要 TrimSpace:思迅的 sup_no / supcust_no 是 char 定长,源库带尾随
//
//	空格。cube 查询层虽然已统一 RTRIM,但两边都再 trim 一次才敢当 map 键
//	—— 键对不上是"静默查不到名字",不报错。
func (q *DaprCubeQuerier) ListSupplierNames(ctx context.Context, branchID string) (map[string]string, error) {
	list, err := q.c.ListSuppliers(ctxWithBranch(ctx, branchID))
	if err != nil {
		return nil, fmt.Errorf("cube supplier query: %w", err)
	}
	out := make(map[string]string, len(list.Rows))
	for _, s := range list.Rows {
		id := strings.TrimSpace(s.ID)
		if id == "" {
			continue
		}
		out[id] = strings.TrimSpace(s.Name)
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
//
// ⚠️ created_at(制单日期)进了维度:它是数量信号的**销售窗口起点**
// (见 PurchaseSheetRow.CreatedAt)。实测思迅采购单头**一个 sheet_no 一行**
// (同一次查询 5 张单返回 5 行),所以加维度不会把一张单拆成多行。
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
			"purchase_sheet.created_at",
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
			CreatedAt:          normalizeCubeDate(rowStr(r, "purchase_sheet.created_at")),
			TotalAmountYuan:    rowDec(r, "purchase_sheet.total_amount_yuan"),
			TotalNetAmountYuan: rowDec(r, "purchase_sheet.total_net_amount_yuan"),
		})
	}
	return out, nil
}

// ListProductNames 取商品名(货号 → 名称),给信号/拒绝理由用。
//
// # 为什么信号里要带名字
//
// 用户口径(2026-10-22):审批结论里涉及到商品时要写「<商品名称>-<商品货号>」,
// 不能只给一串货号 —— 店长看到 "02000116" 无从判断这是不是自家在卖的商品,
// 看到 "16.8升琅悦泉水桶装水-02000116" 立刻知道是桶装水。
//
// product 是 **duck model**(同步落 DuckDB,不是实时打思迅源库)⇒ 查询很便宜。
// 查不到名字时调用方回退到纯货号(见 productLabel)。
func (q *DaprCubeQuerier) ListProductNames(ctx context.Context, branchID string, productIDs []string) (map[string]string, error) {
	if len(productIDs) == 0 {
		return nil, nil
	}
	vals := make([]any, 0, len(productIDs))
	for _, id := range productIDs {
		vals = append(vals, id)
	}
	data, err := q.c.LoadCubeQuery(ctxWithBranch(ctx, branchID), "product", cubeclient.CubeQuery{
		Measures:   []string{"product.count"},
		Dimensions: []string{"product.id", "product.name"},
		Filters: []cubeclient.CubeFilter{
			{Member: "product.id", Operator: "in", Values: vals},
		},
		Limit: len(productIDs) * 4,
	})
	if err != nil {
		return nil, fmt.Errorf("cube product name query: %w", err)
	}
	out := make(map[string]string, len(data))
	for _, r := range data {
		id := strings.TrimSpace(rowStr(r, "product.id"))
		if id == "" {
			continue
		}
		out[id] = strings.TrimSpace(rowStr(r, "product.name"))
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

// SumLossByProduct 拉商品 [fromDate, toDate) 的库存单据损耗(JO 报损 / OO 库存调整)。
//
// # 查询为什么这样写(性能与正确性都压在这三条上)
//
//  1. **从明细侧驱动**:商品货号在明细表 t_im_sheet_detail 上有单列索引
//     (i_im_sheet_detail_1 item_no) ⇒ `product_id IN (...)` 走索引,
//     不会全表扫 1 万行明细。
//  2. **按单据类型/方向/审核状态/日期筛头表**:明细表**没有** trans_no,
//     这些列全在头表 t_im_sheet_master ⇒ 必须 join stock_loss(回表走主键 sheet_no)。
//     日期打 created_at(源列 oper_date 有单列索引)。
//  3. **只要已审核**(approve_status='1'):未审核的报损单还没有生效,
//     拿它解释偏离等于用一张废单放行。
//
// ⚠️ 不按 branch_id 过滤(理由同 settlement:思迅 branch_no 与本系统 branch_id 无关,
//   门店隔离靠 X-Branch-ID 路由链)。
//
// ⚠️ 返回**未开口径**的分组行:JO 全部 / OO 仅 direction='-' 的收敛在 aggregateLoss。
func (q *DaprCubeQuerier) SumLossByProduct(ctx context.Context, branchID string, productIDs []string, fromDate, toDate string) ([]LossRow, error) {
	if len(productIDs) == 0 {
		return nil, nil
	}
	vals := make([]any, 0, len(productIDs))
	for _, id := range productIDs {
		vals = append(vals, id)
	}
	data, err := q.c.LoadCubeQuery(ctxWithBranch(ctx, branchID), "stock_loss_line", cubeclient.CubeQuery{
		Measures: []string{"stock_loss_line.total_loss_quantity"},
		Dimensions: []string{
			"stock_loss_line.product_id",
			"stock_loss.document_type",
			"stock_loss.direction",
		},
		Filters: []cubeclient.CubeFilter{
			{Member: "stock_loss_line.product_id", Operator: "in", Values: vals},
			{Member: "stock_loss.document_type", Operator: "in", Values: []any{"JO", "OO"}},
			{Member: "stock_loss.approve_status", Operator: "equals", Values: []any{"1"}},
			{Member: "stock_loss.created_at", Operator: "gte", Values: []any{fromDate}},
			{Member: "stock_loss.created_at", Operator: "lt", Values: []any{toDate}},
		},
		Limit: 5000,
	})
	if err != nil {
		return nil, fmt.Errorf("cube stock_loss query: %w", err)
	}
	out := make([]LossRow, 0, len(data))
	for _, r := range data {
		out = append(out, LossRow{
			ProductID:    rowStr(r, "stock_loss_line.product_id"),
			DocumentType: rowStr(r, "stock_loss.document_type"),
			Direction:    rowStr(r, "stock_loss.direction"),
			Quantity:     rowDec(r, "stock_loss_line.total_loss_quantity"),
		})
	}
	return out, nil
}

// aggregateLoss 把 cube 的分组行按**盘损口径**收敛成 商品 → 损耗数量。
//
// 口径(2026-10-31 用户拍板,生产库实测支撑):
//   - JO 报损单:全部计入(实测 ysx 427 张 / hbposv7 315 张,db_no **恒为 '-'**);
//   - OO 库存调整单:只计 direction='-'(实测 ysx 698 '-' + 471 '+',
//     hbposv7 178 '-' + 271 '+');'+' 是盘盈/补录,算进损耗反而会把偏离"解释"掉;
//   - 其它单据类型(KB 扣补 / KO 领用)不计入。
func aggregateLoss(rows []LossRow) map[string]decimal.Decimal {
	out := make(map[string]decimal.Decimal)
	for _, r := range rows {
		pid := strings.TrimSpace(r.ProductID)
		if pid == "" || !r.Quantity.GreaterThan(decimal.Zero) {
			continue
		}
		switch strings.TrimSpace(r.DocumentType) {
		case "JO":
			// 报损单全部计入(实测方向恒为 '-')。
		case "OO":
			if strings.TrimSpace(r.Direction) != "-" {
				continue
			}
		default:
			continue
		}
		out[pid] = out[pid].Add(r.Quantity.Abs())
	}
	return out
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

// GetSettlementsByIDs 按单号精确查结算单头(**不过滤**审核状态/单据类型)。
//
// # 为什么要一个"不过滤"的查询
//
// 诊断「这张结算单去哪了」时,答案可能落在被过滤掉的那一边:
// 未审核(not_audited)、不是 DP/CP 结款单(not_payable)。
// 只有不筛选的查询才能区分这两者与"压根不存在"(not_found)。
//
// ⚠️ id 列表分批:一个 IN 子句塞几千个值在 MSSQL 上很慢,
// 而且 cube 的参数绑定也有限制。
func (q *DaprCubeQuerier) GetSettlementsByIDs(ctx context.Context, branchID string, ids []string) ([]SettlementRow, error) {
	const chunk = 200
	clean := make([]string, 0, len(ids))
	for _, id := range ids {
		if v := strings.TrimSpace(id); v != "" {
			clean = append(clean, v)
		}
	}
	if len(clean) == 0 {
		return nil, nil
	}
	out := make([]SettlementRow, 0, len(clean))
	for start := 0; start < len(clean); start += chunk {
		end := start + chunk
		if end > len(clean) {
			end = len(clean)
		}
		vals := make([]any, 0, end-start)
		for _, id := range clean[start:end] {
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
			Limit: end - start,
		})
		if err != nil {
			return nil, fmt.Errorf("cube settlement by ids query: %w", err)
		}
		for _, r := range data {
			out = append(out, SettlementRow{
				ID:             rowStr(r, "settlement.id"),
				SupplierID:     rowStr(r, "settlement.supplier_id"),
				DocumentType:   rowStr(r, "settlement.document_type"),
				ApproveStatus:  rowStr(r, "settlement.approve_status"),
				PeriodStart:    normalizeCubeDate(rowStr(r, "settlement.period_start")),
				PeriodEnd:      normalizeCubeDate(rowStr(r, "settlement.period_end")),
				TotalGoodsYuan: rowDec(r, "settlement.total_goods_yuan"),
			})
		}
	}
	return out, nil
}

// CubeSettlementList 是「可勾选结算单」列表 + 两类隐藏统计。
//
// ⚠️ 两类隐藏必须**分开计数**：成因不同、给用户的话也不同
// （已付款 = 付过了；待审批中 = 正在走流程）—— 合成一个数字就没法解释了。
type CubeSettlementList struct {
	// Rows 是过滤后仍可勾选的结算单。
	Rows []SettlementRow
	// HiddenPaid 是被剔除的「已付款」张数。
	HiddenPaid int
	// HiddenPending 是被剔除的「待审批中」张数（2026-10-29 用户口径）。
	HiddenPending int
	// MatchTotal 是**服务端命中的总张数**（含被剔除的那些）。
	//
	// ⚠️ 只在**搜索模式**（传了 query）下有值；常规列表模式为 0。
	// 有了它，前端才能说"服务端命中 3 张，其中 1 张已付款、1 张待审批中"，
	// 而不是只显示剩下的那 1 张 ⇒ 用户以为"系统里没有那两张"。
	MatchTotal int
	// Unavailable 是**命中了但不能勾**的单 + 原因（同样只在搜索模式下有值）。
	//
	// 为什么要单独回：用户在搜索框里找一张老单，找不到 ⇒ 他需要知道
	// "它到底怎么了"。只回"没有匹配"等于把诊断责任推给店长。
	Unavailable []SettlementUnavailable
}

// SettlementUnavailable 是一张「命中了但不能勾选」的结算单 + 原因。
//
// ⚠️ State 取值与 settlement_status.go 的 SettlementState* 一致
// （前端按 state 渲染，不要按文案匹配）。
type SettlementUnavailable struct {
	SettlementID    string `json:"settlement_id"`
	SupplierID      string `json:"supplier_id"`
	GoodsAmountYuan string `json:"goods_amount_yuan"`
	// State 是状态枚举。
	State string `json:"state"`
	// StateLabel 是给店长看的中文说明。
	StateLabel string `json:"state_label"`
	// ApprovalID / ApprovalSpNo 在「待审批中」时给出 ⇒ 前端可跳审批详情。
	ApprovalID   string `json:"approval_id"`
	ApprovalSpNo string `json:"approval_sp_no"`
}

// ListCubeSettlements H5 勾选数据源:透传 cube 已审核采购结款单(cube 未配置 → 503)。
//
// includePaid=false(**默认**)时剔除两类单:
//   - 台账里已付款的 —— "过滤已结算的结算单":付款审批上线之前门店已经付掉的单,
//     不能再出现在发起勾选列表里;
//   - **待审批中**的(2026-10-29 用户口径)—— 已被某笔审批占用、还没出终态,
//     不能再被勾一次(否则同一张单付两次钱)。被拒/失败/撤销后自动恢复可选,
//     判定见 service/pending_approval.go。
//
// includePaid=true 给初始化结算页用(它要看的就是已付款的那些)。
// ⚠️ 该页**不过滤待审批中**:它是"把历史已付款补录进台账"的运维页面,
//    显式 include_paid=1 就意味着操作人明确要看全量;把它按待审批筛掉
//    会让"这张单正被人审批"这件事在补录页上彻底看不见。
//
// 两个隐藏张数都原样回给前端 —— 列表突然少了一堆单,店长会以为是系统出错,
// 必须能告诉他"这里隐藏了 N 张已付款的 / N 张待审批中的"。
//
// # query(关键字搜索)—— 2026-10-29 用户口径
//
//	「wx-h5 前端『提交付款审批』页的选择结算单也无法筛选出
//	  CP002608194277、CP002609024324 等」
//
// 根因(2026-10-29 生产实测):常规列表走
// ListAuditedSettlements(limit) —— 它按账期结束日**倒序截断到 limit 张**
// (默认 200)。实测该店已审核结款单共 4422 张,上面那几张单按账期倒序的排名
// 分别是 256 / 210 / 205 / 200 ⇒ **全部落在 200 名之外** ⇒ 永远搜不到。
// 而同一张单在「初始化结算」页搜得到(那条路径拉全量 5000 张再在 Go 侧匹配)
// ⇒ 两个页面对"这张单在不在"给出**相反的答案**。
//
// ⇒ query 非空时改成:拉**全量**(cubeSettlementFetchCap)再在 Go 侧做包含匹配,
//   **先筛后截** ⇒ 任意历史单都能搜到;并把命中的不可选单连同原因一起回传。
func (s *Service) ListCubeSettlements(ctx context.Context, branchID, from, to, supplierID, query string, limit int, includePaid bool) (*CubeSettlementList, error) {
	if s.cube == nil {
		return nil, ErrCubeUnavailable
	}
	query = strings.TrimSpace(query)
	var rows []SettlementRow
	if query == "" {
		var err error
		rows, err = s.cube.ListAuditedSettlements(ctx, branchID, from, to, supplierID, limit)
		if err != nil {
			return nil, err
		}
		// 补供应商名(cube 侧 JOIN 不了 live↔duck,只能在 Go 侧补)。
		s.fillSupplierNames(ctx, branchID, rows)
	} else {
		// 搜索模式:拉**全量**再在 Go 侧匹配 —— 截断必须发生在**匹配之后**,
		// 否则老单永远进不来(这正是上面那 4 张单搜不到的原因)。
		all, err := s.cube.ListAuditedSettlements(ctx, branchID, from, to, supplierID, cubeSettlementFetchCap)
		if err != nil {
			return nil, err
		}
		// ⚠️ 供应商名必须**先补再匹配**:按供应商**名称**搜是常用操作,
		//    而名称只有 join 过字典才有(见 settlementRowMatches 的注释)。
		s.fillSupplierNames(ctx, branchID, all)
		matched := make([]SettlementRow, 0, 16)
		for _, r := range all {
			if settlementRowMatches(r, query) {
				matched = append(matched, r)
			}
		}
		// 匹配后再截断(limit<=0 = 不限)。
		if limit > 0 && len(matched) > limit {
			matched = matched[:limit]
		}
		rows = matched
	}
	if includePaid {
		return &CubeSettlementList{Rows: rows}, nil
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, strings.TrimSpace(r.ID))
	}
	paid, err := s.ListPaidSettlementIDsOf(ctx, branchID, ids)
	if err != nil {
		return nil, err
	}
	kept, hiddenPaid := FilterOutPaidSettlements(rows, paid)
	// 待审批中:已被某笔未终态的审批占用的单不能再选一次。
	pending, err := s.ListPendingSettlements(ctx, branchID, ids)
	if err != nil {
		return nil, err
	}
	kept, hiddenPending := FilterOutPendingSettlements(kept, pendingSettlementIDSet(pending))
	list := &CubeSettlementList{
		Rows:          kept,
		HiddenPaid:    hiddenPaid,
		HiddenPending: hiddenPending,
	}
	if query == "" {
		return list, nil
	}
	// 搜索模式:把**命中但不可勾**的单连同原因一起回传。
	//
	// ⚠️ 为什么要单独查一遍状态:剔除逻辑只告诉我们"被剔了",
	//    告诉不了店长"它到底怎么了"(已付款？待审批中？在哪一笔审批？)。
	//    而这正是用户 2026-10-29 的原话诉求。
	list.MatchTotal = len(rows)
	list.Unavailable = s.unavailableSettlements(ctx, branchID, rows, kept, query)
	return list, nil
}

// unavailableSettlements 找出"命中了但被剔除"的单,并给每张单补上状态说明。
//
// rows 是命中集,kept 是剔除后仍可勾的集合 ⇒ 两者之差就是被剔除的。
func (s *Service) unavailableSettlements(ctx context.Context, branchID string, rows, kept []SettlementRow, query string) []SettlementUnavailable {
	keptIDs := make(map[string]bool, len(kept))
	for _, r := range kept {
		keptIDs[strings.TrimSpace(r.ID)] = true
	}
	excluded := make([]SettlementRow, 0, 4)
	for _, r := range rows {
		if !keptIDs[strings.TrimSpace(r.ID)] {
			excluded = append(excluded, r)
		}
	}
	if len(excluded) == 0 {
		return []SettlementUnavailable{}
	}
	ids := make([]string, 0, len(excluded))
	for _, r := range excluded {
		ids = append(ids, strings.TrimSpace(r.ID))
	}
	items, err := s.ListSettlementStatuses(ctx, branchID, ids)
	if err != nil {
		// 状态查不到**不阻断**列表:这只是一段"说明文字",
		// 查不到就退化成"该单当前不可选择"。
		s.logger.Warn("settlement-auditor: 取结算单状态失败,不可选原因留空",
			"branch_id", branchID, "query", query, "err", err)
		out := make([]SettlementUnavailable, 0, len(excluded))
		for _, r := range excluded {
			out = append(out, SettlementUnavailable{
				SettlementID:    strings.TrimSpace(r.ID),
				SupplierID:      strings.TrimSpace(r.SupplierID),
				GoodsAmountYuan: r.TotalGoodsYuan.StringFixed(2),
				State:           SettlementStatePayable,
				StateLabel:      "该单当前不可选择",
			})
		}
		return out
	}
	out := make([]SettlementUnavailable, 0, len(items))
	for _, it := range items {
		out = append(out, SettlementUnavailable{
			SettlementID:    it.SettlementID,
			SupplierID:      it.SupplierID,
			GoodsAmountYuan: it.GoodsAmountYuan,
			State:           it.State,
			StateLabel:      it.StateLabel,
			ApprovalID:      it.ApprovalID,
			ApprovalSpNo:    it.ApprovalSpNo,
		})
	}
	return out
}

// fillSupplierNames 用供应商字典补 rows[].SupplierName。
//
// # 为什么字典拉不到只告警、不让整个请求失败
//
// 结算单本身是主数据,供应商名是**显示层**。让"名字查不到"升级成"整个列表
// 503",等于为了让一行小字把付款审批的入口堵死 —— 店长看到的是"数据源不可用",
// 完全猜不到真正发生的是供应商字典这个**旁路**挂了。
// 与 catalog 域取商品供应商名同一取舍(catalog/service/catalog.go:
// "取供应商字典失败,供应商名留空")。
//
// 降级后的表现:列表照常出,供应商列只有编号(前端渲染成"编号"而不是
// "编号-"),按名称搜索自然也搜不到 —— 这与"字典里真没这个供应商"无法区分,
// 但至少不挡付款。
func (s *Service) fillSupplierNames(ctx context.Context, branchID string, rows []SettlementRow) {
	if len(rows) == 0 || s.cube == nil {
		return
	}
	names, err := s.cube.ListSupplierNames(ctx, branchID)
	if err != nil {
		s.logger.Warn("settlement-auditor: 取供应商字典失败,供应商名留空", "err", err)
		return
	}
	for i := range rows {
		if n, ok := names[strings.TrimSpace(rows[i].SupplierID)]; ok {
			rows[i].SupplierName = n
		}
	}
}

// fillApprovalSettlementNames 给审批快照的结算单行补供应商名称(读时 join)。
//
// 与 [fillSupplierNames] 同一个字典、同一套键处理,只是目标结构不同
// (approval_settlements 的行不是 cube 行,是 PG 快照行)。
//
// ⚠️ **读时**补而不是落库:理由见 model.ApprovalSettlement.SupplierName 的注释
// (历史行只能拿当前字典名回填,加列并不能得到"历史真值",却要动生产表 +
// 把审核流水线拖进来)。
//
// 降级:字典拿不到 ⇒ 名称留空,详情页其余内容照常(与列表页同一取舍)。
func (s *Service) fillApprovalSettlementNames(ctx context.Context, branchID string, rows []model.ApprovalSettlement) {
	if len(rows) == 0 || s.cube == nil {
		return
	}
	names, err := s.cube.ListSupplierNames(ctx, branchID)
	if err != nil {
		s.logger.Warn("settlement-auditor: 取供应商字典失败,审批详情里的供应商名留空", "err", err)
		return
	}
	for i := range rows {
		if n, ok := names[strings.TrimSpace(rows[i].SupplierID)]; ok {
			rows[i].SupplierName = n
		}
	}
}

// saleWindowDates 计算销售回看窗口 [from, to)(to = 今天,YYYY-MM-DD)。
func (s *Service) saleWindowDates() (from, to string) {
	now := s.now()
	to = now.Format("2006-01-02")
	from = now.AddDate(0, 0, -s.cfg.SaleWindowDays).Format("2006-01-02")
	return
}

var _ = time.Now // 静默引用(窗口计算预留)
