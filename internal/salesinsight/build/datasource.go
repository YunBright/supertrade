package build

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/salesinsight/fact"
	"github.com/YunBright/supertrade/internal/salesinsight/fact/supplier"
)

// DataSource 是事实包组装所需的**全部**数据依赖。
//
// # 为什么是五个方法而不是一个
//
// 每个方法对应一次独立的 cube 查询。收成一个方法会让"查了什么"
// 无法断言，而这里的每次查询都有明确的成本与口径
// （供应商名要跨 storage 单独查、库存要按 item_no 批量查）。
//
// 这五个方法就是**全部**的外部接触面 —— 事实包不可能
// 结构性地把全量商品塞进上下文，因为组装逻辑只能走这五个查询，
// 而它们的 Limit 都由 TopN 夹住。
type DataSource interface {
	// TotalsSales 取**大盘总额**：不分组、不限量、不做任何行过滤。
	//
	// ⚠️ 这条**不能**由"把所有品类加起来"代替（2026-10-17 修复）。
	//
	//	原来的 `f.Totals = sumCategories(catCur)` 有两个各自都会漏的口子：
	//
	//	  1. catCur 来自 CategorySales(…, topN)，cube 侧带 `Limit: topN`
	//	     ⇒ 排在 topN 之外的类目根本没回来，求和自然缺一块；
	//	  2. CategorySales 对 `item_cls_parent.name` 为空的行**直接 continue**
	//	     ⇒ 没有归类的商品整行消失，而供应商侧同一批行是**保留**的
	//	     （suppliers.go 的注释明说"丢弃会让销量凭空从大盘里消失"）。
	//
	//	线上实测（门店 00，2026-10-16）：品类口径 2024.6 元，
	//	供应商口径 13003.8 元 —— 头条只统计到 **15.6%**，
	//	而 LLM 只能把它写成一条"自营贡献高于总销售额"的软警告。
	//
	//	→ 所以大盘必须是**独立的一次无分组查询**：不分组就不会因空类目丢行，
	//	  不限量就不会被 topN 截断。
	//	  多一次查询的成本，换的是"头条数字是对的"。
	TotalsSales(ctx context.Context, storeBranchID, from, to string) (TotalsRow, error)
	// CategorySales 按父类别聚合，from/to 闭区间，返回 TopN。
	CategorySales(ctx context.Context, storeBranchID, from, to string, topN int) ([]CategoryRow, error)
	// ProductSales 按商品聚合，返回 TopN。
	ProductSales(ctx context.Context, storeBranchID, from, to string, topN int) ([]ProductRow, error)
	// SupplierSales 按供应商聚合，返回 TopN。
	SupplierSales(ctx context.Context, storeBranchID, from, to string, topN int) ([]SupplierSalesRow, error)
	// SupplierDict 按编码批量补供应商名称与类型。
	//
	// ⚠️ **必须**带 storeBranchID：supplier 是 storage: duck 的模型，
	//   与 sales_report(live) 一样依赖 X-Branch-ID 路由到**这家门店**的 DuckDB。
	//   少传的症状是 cube-router 返 400 branch_required，而本包会把它折叠成
	//   「字典获取失败，无法排除自用供应商」——
	//   **门店隔离的错误被伪装成数据质量问题**，排查会跑去查源库。
	//   （2026-10-17 生产实证：漏这个参数时日报照发，只是供应商榜单里
	//   混进了自用渠道。）
	SupplierDict(ctx context.Context, storeBranchID string, codes []string) ([]supplier.Record, error)
	// StockByItems 批量查库存。
	StockByItems(ctx context.Context, storeBranchID string, itemNos []string) (map[string]fact.StockRecord, error)
}

// CategoryRow 是品类聚合行（未格式化）。
type CategoryRow struct {
	Name      string
	Amount    float64
	Qty       float64
	Cost      float64
	CostedRow int64
	TotalRow  int64
}

// TotalsRow 是大盘总额（未格式化）。
//
// ⚠️ 它与 CategoryRow 的差别不是"少了个 Name"，而是**行集不同**：
// 品类是"按父类别分组后的子集"，总额是"全部行"。
type TotalsRow struct {
	Amount    float64
	Qty       float64
	Cost      float64
	CostedRow int64
	TotalRow  int64
}

// ProductRow 是商品聚合行（未格式化）。
type ProductRow struct {
	ItemNo    string
	Name      string
	Category  string
	Amount    float64
	Qty       float64
	SupcustNo string
	CostedRow int64
	TotalRow  int64
}

// SupplierSalesRow 是供应商聚合行（未格式化）。
type SupplierSalesRow struct {
	Code string
	// Name 是供应商名称，**可能为空**。
	//
	// ⚠️ 两条取数路径的差别就在这里，而它必须显式建模：
	//
	//	PG 路径（已落库的每日快照）：采集时补过字典，Name 有值；
	//	cube 路径：supplier 是 storage: duck 模型，与 sales_report（live）
	//	           **不能 join**（cube 的 join checkStorage 在构造期就拒绝
	//	           live→duck），所以必须另查一次字典，Name 永远是空的。
	//
	// 调用方要两条都处理：有 Name 就用，没有就查字典。
	// ⚠️ 早期实现让 cube 侧返回空 Name 却不写注释，
	//   后来"PG 路径"接进来时很容易被当成 bug 去"修"成查字典，
	//   于是每份报告都多打一次无用的字典请求。
	Name         string
	Amount       float64
	Qty          float64
	ProductCount int
	// TopItems 是该供应商贡献最高的商品（编码→金额），用于下钻。
	TopItems []SupplierItem
}

// SupplierItem 是供应商维度下的一个商品。
type SupplierItem struct {
	ItemNo string
	Name   string
	Amount float64
	Qty    float64
	// PrevAmount 用于判断该单品是否稳定。
	PrevAmount float64
}

// DaprSource 是经 dapr service invocation 的生产实现。
type DaprSource struct {
	c *cubeclient.DaprCubeClient
}

// NewDaprSource 构造生产数据源。
func NewDaprSource(c *cubeclient.DaprCubeClient) *DaprSource {
	return &DaprSource{c: c}
}

var _ DataSource = (*DaprSource)(nil)

// measures 是所有销售聚合共用的度量集合。
//
// ⚠️ costed_rows/count 一起取不是为了好看：没有它们，
// 调用方无法区分"毛利率 100%"是真实的还是"成本全为 0"的假象
// （hbposv10 实测当日 64 个类别组里多组覆盖率 0%）。
// 详见 salesagg/service/service.go 的包注释。
var measures = []string{
	"sales_report.total_net_amount",
	"sales_report.total_net_qty",
	"sales_report.total_ref_cost",
	"sales_report.costed_rows",
	"sales_report.count",
}

// dateFilters 构造日期闭区间过滤。
//
// ⚠️ 走 gte/lte 的字面值比较而不 CAST：oper_date 在源库是
// char(10) 字符串，CAST 会让百万行全表扫描（实测 7 天区间 225ms）。
func dateFilters(from, to string) []cubeclient.CubeFilter {
	var f []cubeclient.CubeFilter
	if from != "" {
		f = append(f, cubeclient.CubeFilter{
			Member: "sales_report.oper_date", Operator: "gte", Values: []any{from},
		})
	}
	if to != "" {
		f = append(f, cubeclient.CubeFilter{
			Member: "sales_report.oper_date", Operator: "lte", Values: []any{to},
		})
	}
	return f
}

// withBranch 注入 X-Branch-ID。
//
// ⚠️ 这**不是**源库的 branch_no 过滤，只是给 cube-router 的路由头。
// 门店隔离由 branch → cube-router → 该门店专属 cube 实例完成。
func withBranch(ctx context.Context, storeBranchID string) context.Context {
	if strings.TrimSpace(storeBranchID) == "" {
		return ctx
	}
	return cubeclient.WithBranchID(ctx, storeBranchID)
}

// num 从 cube 返回值取 float64。
//
// ⚠️ MSSQL live 源的 decimal 经 go-mssqldb 以 []byte 返回，
// encoding/json 会把它序列化成 base64。必须走 ParseDecimal，
// 不要自己 parse 字符串 —— 那会在某些驱动版本上静默得到 0。
func num(row map[string]any, key string) float64 {
	return cubeclient.ParseDecimal(row[key]).InexactFloat64()
}

// str 从 cube 返回值取字符串。
func str(row map[string]any, key string) string {
	v, ok := row[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

// TotalsSales 实现 DataSource。
//
// ⚠️ 这里**刻意不带 Dimensions，也不带 Limit** ——
//
//	带了任何一项，这次查询就退化成"某一维度的子集求和"，
//	于是 DataSource.TotalsSales 注释里那两个漏点会原样复现。
//	判据见 builder_test.go 的 TestTotalsQueryHasNoDimensionNorLimit。
func (s *DaprSource) TotalsSales(ctx context.Context, storeBranchID, from, to string) (TotalsRow, error) {
	data, err := s.c.LoadCubeQuery(withBranch(ctx, storeBranchID), "sales_report", cubeclient.CubeQuery{
		Measures: measures,
		Filters:  dateFilters(from, to),
	})
	if err != nil {
		return TotalsRow{}, fmt.Errorf("build: cube totals query: %w", err)
	}
	if len(data) == 0 {
		// 无数据与"有数据但全为 0"是两回事：前者不该报成功。
		return TotalsRow{}, fmt.Errorf("build: cube totals query 返回空结果（%s~%s）", from, to)
	}
	row := data[0]
	return TotalsRow{
		Amount:    num(row, "sales_report.total_net_amount"),
		Qty:       num(row, "sales_report.total_net_qty"),
		Cost:      num(row, "sales_report.total_ref_cost"),
		CostedRow: int64(num(row, "sales_report.costed_rows")),
		TotalRow:  int64(num(row, "sales_report.count")),
	}, nil
}

// CategorySales 实现 DataSource。
func (s *DaprSource) CategorySales(ctx context.Context, storeBranchID, from, to string, topN int) ([]CategoryRow, error) {
	data, err := s.c.LoadCubeQuery(withBranch(ctx, storeBranchID), "sales_report", cubeclient.CubeQuery{
		Measures:   measures,
		Dimensions: []string{"item_cls_parent.name"},
		Filters:    dateFilters(from, to),
		Limit:      topN,
	})
	if err != nil {
		return nil, fmt.Errorf("build: cube category query: %w", err)
	}
	out := make([]CategoryRow, 0, len(data))
	for _, row := range data {
		name := str(row, "item_cls_parent.name")
		if name == "" {
			continue
		}
		out = append(out, CategoryRow{
			Name:      name,
			Amount:    num(row, "sales_report.total_net_amount"),
			Qty:       num(row, "sales_report.total_net_qty"),
			Cost:      num(row, "sales_report.total_ref_cost"),
			CostedRow: int64(num(row, "sales_report.costed_rows")),
			TotalRow:  int64(num(row, "sales_report.count")),
		})
	}
	return out, nil
}

// ProductSales 实现 DataSource。
func (s *DaprSource) ProductSales(ctx context.Context, storeBranchID, from, to string, topN int) ([]ProductRow, error) {
	data, err := s.c.LoadCubeQuery(withBranch(ctx, storeBranchID), "sales_report", cubeclient.CubeQuery{
		Measures: measures,
		Dimensions: []string{
			"sales_report.item_no",
			"item.name",
			"item_cls_parent.name",
			"sales_report.supcust_no",
		},
		Filters: dateFilters(from, to),
		Limit:   topN,
	})
	if err != nil {
		return nil, fmt.Errorf("build: cube product query: %w", err)
	}
	out := make([]ProductRow, 0, len(data))
	for _, row := range data {
		itemNo := str(row, "sales_report.item_no")
		if itemNo == "" {
			// 编码与名称都空 = 脏数据，丢弃而不是留一行无名项。
			// 报表里出现空白行比少一行更让人困惑。
			continue
		}
		out = append(out, ProductRow{
			ItemNo:    itemNo,
			Name:      displayName(str(row, "item.name"), itemNo),
			Category:  str(row, "item_cls_parent.name"),
			Amount:    num(row, "sales_report.total_net_amount"),
			Qty:       num(row, "sales_report.total_net_qty"),
			SupcustNo: str(row, "sales_report.supcust_no"),
			CostedRow: int64(num(row, "sales_report.costed_rows")),
			TotalRow:  int64(num(row, "sales_report.count")),
		})
	}
	return out, nil
}

// SupplierSales 实现 DataSource。
func (s *DaprSource) SupplierSales(ctx context.Context, storeBranchID, from, to string, topN int) ([]SupplierSalesRow, error) {
	data, err := s.c.LoadCubeQuery(withBranch(ctx, storeBranchID), "sales_report", cubeclient.CubeQuery{
		Measures:   []string{"sales_report.total_net_amount", "sales_report.total_net_qty", "sales_report.count"},
		Dimensions: []string{"sales_report.supcust_no", "sales_report.item_no", "item.name"},
		Filters:    dateFilters(from, to),
		// ⚠️ 这里**故意**不放 topN 当 Limit：
		//   按 (供应商,商品) 分组时,TopN 会截掉整个供应商的部分商品,
		//   让我们算出偏低的供应商销售额 —— 那会直接影响"哪个供应商更好"的结论。
		//   改为取一个较大的上限,再在 Go 里按供应商归并取 TopN。
		Limit: supplierSalesScanLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("build: cube supplier query: %w", err)
	}
	return collapseSuppliers(data), nil
}

// supplierSalesScanLimit 是供应商扫描的行数上限。
//
// 取 2000 而不是 TopN：按 (供应商 × 商品) 分组时，一个供应商下有几十个商品很正常。
// 2000 行大约覆盖 30~60 个供应商 × 各 30~60 个商品，足够找出排名靠前的供应商。
//
// ⚠️ 它同时是**成本上限**：行数越大 cube 传输越慢。
//
//	超出时返回的行会被截断，此时供应商金额会偏低 ——
//	所以 Fact.DataSufficiency 会据此标记"供应商数据可能被截断"。
const supplierSalesScanLimit = 2000

// collapseSuppliers 把 (供应商 × 商品) 明细行归并成按供应商聚合的行。
//
// 这是"必须多扫一些再归并"的原因：cube 的 SQL 层做不了
// "按 A 分组取每组内 B 的 TopN"（源库是 2008 R2，没有窗口函数的可用写法），
// 所以归并只能在 Go 里做。
func collapseSuppliers(data []map[string]any) []SupplierSalesRow {
	type acc struct {
		amount float64
		qty    float64
		items  []SupplierItem
		seen   map[string]bool
	}
	order := make([]string, 0, 32)
	by := make(map[string]*acc, 32)

	for _, row := range data {
		code := str(row, "sales_report.supcust_no")
		if code == "" {
			continue
		}
		a, ok := by[code]
		if !ok {
			a = &acc{seen: map[string]bool{}}
			by[code] = a
			order = append(order, code)
		}
		itemNo := str(row, "sales_report.item_no")
		amt := num(row, "sales_report.total_net_amount")
		qty := num(row, "sales_report.total_net_qty")
		a.amount += amt
		a.qty += qty
		// 同一商品可能因多次成交分成多行，按 item_no 去重后再加
		if itemNo != "" && !a.seen[itemNo] {
			a.seen[itemNo] = true
			a.items = append(a.items, SupplierItem{
				ItemNo: itemNo,
				Name:   displayName(str(row, "item.name"), itemNo),
				Amount: amt,
				Qty:    qty,
			})
		}
	}

	out := make([]SupplierSalesRow, 0, len(order))
	for _, code := range order {
		a := by[code]
		// 每个供应商只保留金额最高的 SupplierTopN 个商品
		top := append([]SupplierItem(nil), a.items...)
		sortSupplierItems(top)
		if len(top) > fact.SupplierTopN {
			top = top[:fact.SupplierTopN]
		}
		out = append(out, SupplierSalesRow{
			Code:         code,
			Amount:       a.amount,
			Qty:          a.qty,
			ProductCount: len(a.items),
			TopItems:     top,
		})
	}
	sortSuppliersByAmount(out)
	return out
}

// displayName 名称为空时退回编码。
//
// ⚠️ 缺档案的商品在流水上是存在的（sales_report.item_no 非空），
//
//	只是 item.name 为空。若丢掉这些行，"今天卖了多少"会因为
//	一条商品档案缺失而凭空少一截。
func displayName(name, itemNo string) string {
	if n := strings.TrimSpace(name); n != "" {
		return n
	}
	if itemNo != "" {
		return "商品" + itemNo
	}
	return "未知商品"
}

// sortSupplierItems 按金额降序排商品明细。
func sortSupplierItems(items []SupplierItem) {
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].Amount > items[j].Amount
	})
}

// sortSuppliersByAmount 按金额降序排供应商。
func sortSuppliersByAmount(rows []SupplierSalesRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].Amount > rows[j].Amount
	})
}
