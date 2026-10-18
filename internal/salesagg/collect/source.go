// Package collect 把某门店某个营业日的**完整销售事实**从 cube 采下来，落进 PG。
//
// # 为什么在 sales-agg 而不是 sales-insight
//
// 每日销售统计卡片（走 sales-agg）与 AI 销售日报（走 sales-insight）
// 是**同一份销售事实的两种呈现**。改造前两边各自现查 cube，
// 于是 2026-10-16 出现了同一个营业日的两个口径：
// 品类 2024.6 元 vs 供应商 13003.8 元 —— 差 6 倍，**零报错**。
//
// 两边各查一次，无论怎么改都只是把同一个坑换个地方重挖；
// 唯一能消掉它的办法是**只查一次、只落一次、两边读同一行**。
//
//	采集：sales-agg（本包，唯一写者）
//	读取：sales-insight（只读，见 internal/salesdaily）
//
// # 一次采集，四次查询
//
//	总额（无分组）→ 品类 → 商品 → 供应商 × 商品 → 供应商名字典
//
// 前四个是同一个 cube（`sales_report`，live 源），
// 字典是另一个（`supplier`，storage: duck）—— 两者不能 join，
// 原因见 Source.SupplierNames 的注释。
package collect

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/salesagg/cubeq"
	"github.com/YunBright/supertrade/internal/salesdaily"
)

// Source 是采集所需的**全部** cube 接触面。
//
// # 五个方法、**全部不限量**
//
// 这里每个方法都不接受 topN/limit，且实现里一律传 0（不限量）。
//
// 这不是"忘了加参数"，而是落库的前提：月报靠"把每天定稿的日行加起来"
// 出数，而"加起来 = 大盘"这个等式只在明细集是**全集**时成立。
// 任何一次带上限的查询都会让某天少一截，症状是月报偏小、
// 无报错、无降级标记 —— 与 2026-10-17 那个「大盘少了 85%」是同一类。
//
// ⚠️ 反过来说：**报表路径**（internal/salesagg/service）是允许带 limit 的，
//
//	它展示的是 TopN，而"TopN 之和 ≠ 大盘"在报表语境下是可接受的。
//	两条链路的 limit 语义不同，所以是两个方法而不是一个。
type Source interface {
	// TotalsSales 取大盘总额：**不带任何维度、不限量**。
	TotalsSales(ctx context.Context, storeBranchID, from, to string) (salesdaily.TotalsRow, error)
	// CategorySales 按父类别聚合，返回**全量**。
	CategorySales(ctx context.Context, storeBranchID, from, to string) ([]salesdaily.CategoryRow, error)
	// ProductSales 按商品聚合，返回**全量**（带所属父类别）。
	ProductSales(ctx context.Context, storeBranchID, from, to string) ([]salesdaily.ProductRow, error)
	// SupplierSales 按供应商聚合，返回**全量**（已按供应商归并）。
	//
	// 返回行的 Name **可能为空**：名字要另查字典（见 SupplierNames）。
	SupplierSales(ctx context.Context, storeBranchID, from, to string) ([]salesdaily.SupplierRow, error)
	// SupplierNames 按编码批量补供应商名称（编码 → 名称）。
	SupplierNames(ctx context.Context, storeBranchID string, codes []string) (map[string]string, error)
}

// DaprSource 是经 dapr service invocation 的生产实现。
type DaprSource struct {
	c *cubeclient.DaprCubeClient
}

// NewDaprSource 构造生产数据源。
func NewDaprSource(c *cubeclient.DaprCubeClient) *DaprSource {
	return &DaprSource{c: c}
}

var _ Source = (*DaprSource)(nil)

// withBranch 注入 X-Branch-ID。
//
// ⚠️ 这**不是**源库的 branch_no 过滤，只是给 cube-router 的路由头。
//
//	门店隔离由 branch → cube-router → 该门店专属 cube 实例完成。
//	思迅 branch_no 与本系统 branch_id 是两套无关编码，
//	按 branch_no 过滤会永远匹配不上（实测记录见 cubeclient.GetStock）。
func withBranch(ctx context.Context, storeBranchID string) context.Context {
	if strings.TrimSpace(storeBranchID) == "" {
		return ctx
	}
	return cubeclient.WithBranchID(ctx, storeBranchID)
}

// ---- 查询构造（纯函数，可离线断言形状）----
//
// ⚠️ 刻意做成纯函数而不是内联在 DaprSource 的方法里：
//
//	"这次采集发出的 query 到底有没有带维度/带上限"是本包最要紧的断言
//	（带了就等于把「大盘少了 85%」那个 bug 搬进新链路），
//	而内联写法只能靠读源码文本去断言 —— 那既脆弱又测不到重构。
//	拆成纯函数后，测试能直接检查 CubeQuery 的字段。
//
// 注意：这些函数**只**服务于采集。报表链路（internal/salesagg/service）
// 有自己的维度成员与 limit 语义，两者不可互相调用。

// totalsQuery 是大盘总额查询。
//
// ⚠️ **不带 Dimensions、不带 Limit** —— 这两条各对应一个真实漏点：
//
//	带 Dimensions → 空类目行被丢，"总额"其实只是"有类目的那部分"
//	带 Limit      → 排在上限之外的供应商整批不见
//
//	两者都表现为数字偏小、零报错、无降级标记。
func totalsQuery(from, to string) cubeclient.CubeQuery {
	return cubeq.Query(nil, from, to, 0)
}

// categoryQuery 是品类查询（按父类别，全量）。
func categoryQuery(from, to string) cubeclient.CubeQuery {
	return cubeq.Query([]string{"item_cls_parent.name"}, from, to, 0)
}

// productQuery 是商品查询（全量，带所属父类别）。
//
// ⚠️ 比 internal/salesagg/service 的商品维度**多一个** item_cls_parent.name：
//
//	AI 报告的归因要"这个商品所属类别跌了多少"，
//	而报表的商品维度刻意不带类别（见其 dimSpecs 的注释）。
//	两者不能共用同一个查询形状。
func productQuery(from, to string) cubeclient.CubeQuery {
	return cubeq.Query([]string{
		"sales_report.item_no",
		"item.name",
		"item_cls_parent.name",
	}, from, to, 0)
}

// supplierQuery 是供应商查询（按 供应商 × 商品 分组，全量）。
//
// ⚠️ 同时按 **item_no** 分组，是为了拿到"这家供应商今天卖得最好的几个商品"。
//
//	代价是返回行数 = Σ(每家供应商的商品数)，比"只按供应商分组"大一个量级。
//	这是可接受的：单店单日的 (供应商 × 商品) 行数在几千量级，
//	而它换来的是 TopItems 能**真实**反映当天，而不是猜。
func supplierQuery(from, to string) cubeclient.CubeQuery {
	return cubeq.Query([]string{
		"sales_report.supcust_no",
		"sales_report.item_no",
		"item.name",
	}, from, to, 0)
}

// supplierDictQuery 是供应商字典查询。
//
// ⚠️ 跨 storage：supplier 是 **storage: duck**，sales_report 是 **live**。
//
//	cube 的 join checkStorage 只允许 live→live，live→duck 在**查询构造期**
//	就被拒绝 —— 所以名字必须另查一次（实测 2026-10-16）。
//	两次查询都只传编码列表，数据量很小。
func supplierDictQuery(vals []any) cubeclient.CubeQuery {
	return cubeclient.CubeQuery{
		Measures:   []string{"supplier.count"},
		Dimensions: []string{"supplier.id", "supplier.name"},
		Filters: []cubeclient.CubeFilter{
			{Member: "supplier.id", Operator: "in", Values: vals},
		},
		Limit: len(vals) + dictLimitSlack,
	}
}

// dictLimitSlack 是字典查询在编码数之外多取的条数。
//
// ⚠️ 存在的理由是"一条编码可能对应多行"（同一供应商在字典里有历史记录）。
//
//	取少了会让**排在后面的编码**查不到 —— 而症状是"部分供应商没名字"，
//	看起来像数据质量问题，实际是分页。
const dictLimitSlack = 10

// TotalsSales 实现 Source。
func (s *DaprSource) TotalsSales(ctx context.Context, storeBranchID, from, to string) (salesdaily.TotalsRow, error) {
	data, err := s.c.LoadCubeQuery(withBranch(ctx, storeBranchID), "sales_report", totalsQuery(from, to))
	if err != nil {
		return salesdaily.TotalsRow{}, fmt.Errorf("collect: cube 总额查询: %w", err)
	}
	if len(data) == 0 {
		// 空结果与"有数据但全为 0"是两回事：前者不该报成功，
		// 否则会写下一行 0 元的快照，把"没采到"说成"当天没生意"。
		return salesdaily.TotalsRow{}, fmt.Errorf("collect: cube 总额查询返回空结果（%s~%s）", from, to)
	}
	return totalsRow(data[0]), nil
}

// totalsRow 把一行 cube 返回转成大盘汇总。
func totalsRow(row map[string]any) salesdaily.TotalsRow {
	return salesdaily.TotalsRow{
		NetAmount:       cubeq.Num(row, "sales_report.total_net_amount"),
		NetQty:          cubeq.Num(row, "sales_report.total_net_qty"),
		RefCost:         cubeq.Num(row, "sales_report.total_ref_cost"),
		TotalRows:       int64(cubeq.Num(row, "sales_report.count")),
		CostedRows:      int64(cubeq.Num(row, "sales_report.costed_rows")),
		CostCoverage:    costCoverage(row),
		MarginAvailable: marginAvailable(row),
		RefMargin:       marginOf(row),
		RefMarginPct:    marginPctOf(row),
	}
}

func costCoverage(row map[string]any) float64 {
	total := cubeq.Num(row, "sales_report.count")
	if total <= 0 {
		return 0
	}
	return cubeq.Num(row, "sales_report.costed_rows") / total
}

// marginAvailable 判断能否展示毛利。
//
// ⚠️ 阈值 0.5 与 internal/salesagg/service.MinCostCoverage 一致，
//
//	但**独立定义**：存储层不该依赖报表层。
//	而阈值一旦不一致，"同一份数据能不能显示毛利"就会随读取路径变化 ——
//	 hbposv10 实测当日 64 个类别组里多组覆盖率 0%，
//	按 cost=0 算会显示"毛利率 100%"，那比不显示更坏。
const minCostCoverage = 0.5

func marginAvailable(row map[string]any) bool {
	return costCoverage(row) >= minCostCoverage
}

// marginOf 算毛利额。覆盖率不足时返回 0 而不是负数。
func marginOf(row map[string]any) float64 {
	if !marginAvailable(row) {
		return 0
	}
	return cubeq.Num(row, "sales_report.total_net_amount") - cubeq.Num(row, "sales_report.total_ref_cost")
}

// marginPctOf 算毛利率。覆盖率不足或分母为 0 时返回 0。
func marginPctOf(row map[string]any) float64 {
	if !marginAvailable(row) {
		return 0
	}
	amt := cubeq.Num(row, "sales_report.total_net_amount")
	if amt == 0 {
		return 0
	}
	return marginOf(row) / amt
}

// CategorySales 实现 Source：按**父类别**聚合的全量品类。
//
// ⚠️ 成员地址必须带 model 前缀（item_cls_parent.*）：
//
//	类别不在 sales_report 上，它在 join 链 sales_report → item →
//	item_cls → item_cls_parent 上。写成 sales_report.xxx 会查得到、
//	但那一列全空 —— 而全空的维度不会被"查不到"暴露，
//	只会让金额凭空少一截。
func (s *DaprSource) CategorySales(ctx context.Context, storeBranchID, from, to string) ([]salesdaily.CategoryRow, error) {
	data, err := s.c.LoadCubeQuery(withBranch(ctx, storeBranchID), "sales_report", categoryQuery(from, to))
	if err != nil {
		return nil, fmt.Errorf("collect: cube 品类查询: %w", err)
	}
	out := make([]salesdaily.CategoryRow, 0, len(data))
	for _, row := range data {
		name := cubeq.Str(row, "item_cls_parent.name")
		if name == "" {
			// 空类目名既无法构成主键也无法展示。
			// ⚠️ 丢弃它**不会**让总额少账 —— 总额走上面那次无分组查询。
			//   但这里必须写明，否则下一个人会"顺手改成保留空名行"，
			//   而那会让月报 GROUP BY 多出一行无名类别。
			continue
		}
		out = append(out, salesdaily.CategoryRow{
			Name:      name,
			Amount:    cubeq.Num(row, "sales_report.total_net_amount"),
			Qty:       cubeq.Num(row, "sales_report.total_net_qty"),
			Cost:      cubeq.Num(row, "sales_report.total_ref_cost"),
			TotalRow:  int64(cubeq.Num(row, "sales_report.count")),
			CostedRow: int64(cubeq.Num(row, "sales_report.costed_rows")),
		})
	}
	return out, nil
}

// ProductSales 实现 Source：全量商品（带所属父类别）。
//
// ⚠️ 编码取 sales_report.item_no 而不是 item.id：
//
//	item 是 LEFT JOIN 过来的档案表，商品缺档案时 item.id 为 NULL，
//	而行解析会把"编码和名称都空"的行整行丢掉 ——
//	那样卖出去的商品会凭空从月报里消失。item_no 在流水上实测非空。
func (s *DaprSource) ProductSales(ctx context.Context, storeBranchID, from, to string) ([]salesdaily.ProductRow, error) {
	data, err := s.c.LoadCubeQuery(withBranch(ctx, storeBranchID), "sales_report", productQuery(from, to))
	if err != nil {
		return nil, fmt.Errorf("collect: cube 商品查询: %w", err)
	}
	out := make([]salesdaily.ProductRow, 0, len(data))
	for _, row := range data {
		itemNo := cubeq.Str(row, "sales_report.item_no")
		if itemNo == "" {
			continue
		}
		out = append(out, salesdaily.ProductRow{
			ItemNo:   itemNo,
			Name:     displayName(cubeq.Str(row, "item.name"), itemNo),
			Category: cubeq.Str(row, "item_cls_parent.name"),
			Amount:   cubeq.Num(row, "sales_report.total_net_amount"),
			Qty:      cubeq.Num(row, "sales_report.total_net_qty"),
		})
	}
	return out, nil
}

// TopItemsPerSupplier 是每个供应商保留的商品明细条数。
//
// ⚠️ 与 internal/salesinsight/fact.SupplierTopN 同为 3，但**独立定义**：
//
//	这里是"落库时存几条"，那里是"读出来展示几条"，
//	前者是存储成本，后者是上下文预算 —— 两者会各自调整。
const TopItemsPerSupplier = 3

// SupplierSales 实现 Source：全量供应商。
func (s *DaprSource) SupplierSales(ctx context.Context, storeBranchID, from, to string) ([]salesdaily.SupplierRow, error) {
	data, err := s.c.LoadCubeQuery(withBranch(ctx, storeBranchID), "sales_report", supplierQuery(from, to))
	if err != nil {
		return nil, fmt.Errorf("collect: cube 供应商查询: %w", err)
	}
	return collapseSuppliers(data), nil
}

// collapseSuppliers 把 (供应商 × 商品) 明细行归并成按供应商聚合的行。
//
// # 为什么要归并而不是让 cube 直接按供应商分组
//
// cube 的 SQL 层做不了"按 A 分组、每组内取 B 的 TopN"
// （源库是 2008 R2，没有窗口函数的可用写法），
// 所以"金额按供应商汇总 + 商品明细各留 TopN"只能在 Go 里做。
//
// ⚠️ 归并**不能**丢掉任何一个供应商：
//
//	只保留 TopN 家会让其它供应商的金额从月报里消失，
//	而"供应商金额之和"正是店长判断货源结构的依据。
func collapseSuppliers(data []map[string]any) []salesdaily.SupplierRow {
	type acc struct {
		amount float64
		qty    float64
		items  []salesdaily.TopItemRow
		seen   map[string]bool
	}
	order := make([]string, 0, 32)
	by := make(map[string]*acc, 32)

	for _, row := range data {
		code := cubeq.Str(row, "sales_report.supcust_no")
		if code == "" {
			continue
		}
		a, ok := by[code]
		if !ok {
			a = &acc{seen: map[string]bool{}}
			by[code] = a
			order = append(order, code)
		}
		itemNo := cubeq.Str(row, "sales_report.item_no")
		amt := cubeq.Num(row, "sales_report.total_net_amount")
		qty := cubeq.Num(row, "sales_report.total_net_qty")
		a.amount += amt
		a.qty += qty
		// 同一商品可能因多次成交分成多行，按 item_no 去重后再加。
		if itemNo != "" && !a.seen[itemNo] {
			a.seen[itemNo] = true
			a.items = append(a.items, salesdaily.TopItemRow{
				ItemNo: itemNo,
				Name:   displayName(cubeq.Str(row, "item.name"), itemNo),
				Amount: amt,
				Qty:    qty,
			})
		}
	}

	out := make([]salesdaily.SupplierRow, 0, len(order))
	for _, code := range order {
		a := by[code]
		top := append([]salesdaily.TopItemRow(nil), a.items...)
		sort.SliceStable(top, func(i, j int) bool { return top[i].Amount > top[j].Amount })
		if len(top) > TopItemsPerSupplier {
			top = top[:TopItemsPerSupplier]
		}
		out = append(out, salesdaily.SupplierRow{
			Code:      code,
			Amount:    a.amount,
			Qty:       a.qty,
			ItemCount: len(a.items),
			TopItems:  top,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Amount > out[j].Amount })
	return out
}

// maxDictCodes 是字典查询一次最多带的编码数。
//
// ⚠️ 单店供应商字典实测约 219 条，理论上不该触发；
//
//	留这道夹是为了"将来某个调用方传入全量编码"时不至于把
//	 duck 全表拉回来。
const maxDictCodes = 1000

// SupplierNames 实现 Source：按编码批量补名称。
//
// # 为什么必须单独查一次（而不是 join）
//
// ✅ 实测（2026-10-16）：supplier 是 **storage: duck** 模型，
//
//	sales_report 是 **live**。cube 的 join checkStorage 只允许
//	live→live，live→duck 在**查询构造期**就被拒绝。
//
// ⚠️ 跨 storage 的时间不一致：supplier 是 DuckDB 快照（按 refresh 周期刷新），
//
//	sales_report 是实时打源库。名称可能比流水晚几分钟 ——
//	供应商名称不会在一天之内变，这个延迟可以接受。
//
// ⚠️ withBranch 必须传：supplier 是 duck 模型，同样按 X-Branch-ID
//
//	路由到这家门店的 DuckDB。漏了就是 400 branch_required，
//	而症状会被误读成"这家店没有供应商"。
func (s *DaprSource) SupplierNames(ctx context.Context, storeBranchID string, codes []string) (map[string]string, error) {
	if len(codes) == 0 {
		return map[string]string{}, nil
	}
	if len(codes) > maxDictCodes {
		codes = codes[:maxDictCodes]
	}
	vals := make([]any, 0, len(codes))
	for _, c := range codes {
		if c = strings.TrimSpace(c); c != "" {
			vals = append(vals, c)
		}
	}
	if len(vals) == 0 {
		return map[string]string{}, nil
	}

	data, err := s.c.LoadCubeQuery(withBranch(ctx, storeBranchID), "supplier", supplierDictQuery(vals))
	if err != nil {
		return nil, fmt.Errorf("collect: cube 供应商字典: %w", err)
	}
	out := make(map[string]string, len(data))
	for _, row := range data {
		if id := cubeq.Str(row, "supplier.id"); id != "" {
			out[id] = cubeq.Str(row, "supplier.name")
		}
	}
	return out, nil
}

// displayName 名称为空时退回编码。
//
// ⚠️ 缺档案的商品在流水上是存在的（item_no 非空），只是名字空。
//
//	丢掉这些行会让"今天卖了多少"因为一条商品档案缺失而凭空少一截。
func displayName(name, itemNo string) string {
	if n := strings.TrimSpace(name); n != "" {
		return n
	}
	if itemNo != "" {
		return "商品" + itemNo
	}
	return "未知商品"
}
