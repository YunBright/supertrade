package salesdaily

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ErrRangeIncomplete 表示区间内**没有覆盖全部营业日**。
//
// ⚠️ 它与 ErrNotFound（一天都没有）是两件事，且**必须分开**：
//
//	ErrNotFound     —— 整个区间一行都没有（采集从没跑过 / 落库没配）
//	ErrRangeIncomplete —— 有行，但缺了其中几天
//
// 两者在调用方那里的处理**完全不同**：
//
//	ErrNotFound         → 该区间整体回退查 cube，并提示"PG 采集可能没跑"
//	ErrRangeIncomplete  → 也整体回退，但必须报出**缺了哪几天**
//
// 混为一谈的后果是"缺 3 天"被当成"没配 PG"，于是运营跑去检查
// 采集 job 配置，而真正的问题只是某三天采集失败了。
var ErrRangeIncomplete = errors.New("salesdaily: 区间内未覆盖全部营业日")

// RangeFacts 是一个区间的销售事实（把区间内每一天的行聚合起来）。
//
// # 为什么是"聚合"而不是"读一天"
//
// 月报要的是"这个月卖了多少"，而库里存的是**每天一行**。
// 跨日聚合放在存储层做（SQL 一条 GROUP BY），而不是拉回 Go 里再求和 ——
// 30 天 × 1500 个商品 = 4.5 万行，为了求和传 4.5 万行是不必要的往返。
type RangeFacts struct {
	BranchID string
	From     string
	To       string

	Totals     TotalsRow
	Categories []CategoryRow
	Products   []ProductRow
	Suppliers  []SupplierRow

	// CoveredDays 是区间内**已有快照**的天数，TotalDays 是自然日数。
	//
	// ⚠️ 这两个数必须一起给调用方：只给"覆盖了 27 天"没有用，
	// 它不知道该拿 27 天去和谁比。差额就是缺失天数。
	CoveredDays int
	TotalDays   int

	// CorrectedDays 是区间内**被次日校正过**的天数。
	//
	// ⚠️ 它单独存在是因为"被校正过"在数值上完全看不出来：
	//
	//	一个 22:45 首采的残缺数、被次日 01:05 补全后的数，
	//	在汇总结果里长得一模一样。而店长看到"这个月 12 天被补过"
	//	会知道思迅日结经常晚，需要人工留意；
	//	看不到就以为那就是当天真实的情况。
	//
	// ⚠️ 不设置期：不同天的来源可以不同，强行取一个会撒谎。
	CorrectedDays int
}

// MissingDays 是区间内没有快照的天数。
func (r RangeFacts) MissingDays() int {
	n := r.TotalDays - r.CoveredDays
	if n < 0 {
		return 0
	}
	return n
}

// LoadRange 聚合区间内的销售事实。
//
// # ⚠️ 覆盖不全时**仍然返回已聚合的部分**，同时返回 ErrRangeIncomplete
//
// 这不是"部分成功"，而是刻意的：调用方要靠它来回答
// 「这份月报覆盖了 28 天还是 30 天」——
// 只返回一个 error 就把这个信息弄丢了，
// 而"少了两天的营业额"这种数字必须在报告里显式表达，不能悄悄少掉。
//
// # ⚠️ 调用方必须按**整段**决定去留，不能逐天混
//
// 返回的部分数据看起来完全可用，于是很容易写出
// 「有 PG 的天读 PG、没有的天查 cube」的逐天混用 ——
// 那会让同一份报告里的"总额"与"品类合计"来自两套口径，
// 正是 2026-10-16「2024.6 vs 13003.8」那类分叉的复现。
//
// → 判据见 build 侧的 period 路由：**一个区间只选一条路**。
func (s *Store) LoadRange(ctx context.Context, branchID, from, to string) (RangeFacts, error) {
	if s == nil || s.db == nil {
		return RangeFacts{}, errors.New("salesdaily: store 未初始化")
	}
	if branchID == "" || from == "" || to == "" {
		return RangeFacts{}, fmt.Errorf("salesdaily: branch_id/from/to 必填（收到 %q/%q/%q）", branchID, from, to)
	}
	totalDays, err := daysBetween(from, to)
	if err != nil {
		return RangeFacts{}, err
	}

	db := s.db.WithContext(ctx).Where("branch_id = ? AND oper_date >= ? AND oper_date <= ?", branchID, from, to)

	var totals []DailyTotals
	if err := db.Order("oper_date asc").Find(&totals).Error; err != nil {
		return RangeFacts{}, fmt.Errorf("salesdaily: 聚合总额: %w", err)
	}
	out := RangeFacts{
		BranchID:    branchID,
		From:        from,
		To:          to,
		Totals:      sumTotals(totals),
		CoveredDays: len(totals),
		TotalDays:   totalDays,
	}
	for _, r := range totals {
		if r.Source == string(CollectSourceCorrected) {
			out.CorrectedDays++
		}
	}

	// ---- 品类 ----
	var cats []DailyCategory
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND oper_date >= ? AND oper_date <= ?", branchID, from, to).
		Group("name").
		Select("name, SUM(amount) AS amount, SUM(qty) AS qty, SUM(cost) AS cost," +
			" SUM(costed_row) AS costed_row, SUM(total_row) AS total_row").
		Order("amount desc").Find(&cats).Error; err != nil {
		return RangeFacts{}, fmt.Errorf("salesdaily: 聚合品类: %w", err)
	}
	for _, c := range cats {
		out.Categories = append(out.Categories, CategoryRow{
			Name: c.Name, Amount: c.Amount, Qty: c.Qty, Cost: c.Cost,
			CostedRow: c.CostedRow, TotalRow: c.TotalRow,
		})
	}

	// ---- 商品 ----
	//
	// ⚠️ 名称与类别**不**用 MAX(name) —— 那会按字典序取一个，
	//   一个月里改过名的商品会显示成某个历史名字，且没有任何标记。
	//   取区间内**最后一次采集**的那一份，与"这条商品最近叫什么"一致。
	prodNames, err := s.latestNames(ctx, branchID, from, to, productNameTable)
	if err != nil {
		return RangeFacts{}, err
	}
	var prods []productAgg
	if err := s.db.WithContext(ctx).
		// ⚠️ Table 必须显式给 —— 见 productAgg 的注释。
		Table("sales_daily_products").
		Where("branch_id = ? AND oper_date >= ? AND oper_date <= ?", branchID, from, to).
		Group("item_no").
		Select("item_no, SUM(amount) AS amount, SUM(qty) AS qty").
		Order("amount desc").Find(&prods).Error; err != nil {
		return RangeFacts{}, fmt.Errorf("salesdaily: 聚合商品: %w", err)
	}
	for _, p := range prods {
		meta := prodNames[p.ItemNo]
		out.Products = append(out.Products, ProductRow{
			ItemNo: p.ItemNo, Name: meta.Name, Category: meta.Category,
			Amount: p.Amount, Qty: p.Qty,
		})
	}

	// ---- 供应商 ----
	supNames, err := s.latestNames(ctx, branchID, from, to, supplierNameTable)
	if err != nil {
		return RangeFacts{}, err
	}
	var sups []supplierAgg
	if err := s.db.WithContext(ctx).
		// ⚠️ Table 必须显式给 —— 见 productAgg 的注释。
		Table("sales_daily_suppliers").
		Where("branch_id = ? AND oper_date >= ? AND oper_date <= ?", branchID, from, to).
		Group("code").
		Select("code, SUM(amount) AS amount, SUM(qty) AS qty").
		Order("amount desc").Find(&sups).Error; err != nil {
		return RangeFacts{}, fmt.Errorf("salesdaily: 聚合供应商: %w", err)
	}
	items, err := s.supplierTopItems(ctx, branchID, from, to)
	if err != nil {
		return RangeFacts{}, err
	}
	for _, u := range sups {
		out.Suppliers = append(out.Suppliers, SupplierRow{
			Code: u.Code, Name: supNames[u.Code].Name,
			Amount: u.Amount, Qty: u.Qty,
			// ⚠️ ItemCount 是**每日商品数的累加**，不是"区间内去重后的商品数"。
			//
			//	去重要把 item_no 拉回来再算，那是一次几千行的往返；
			//	而它的用途是"这家供应商供货多不多"，累加值同样回答得了。
			//	⚠️ 但它**不能**被当成"去重商品数"展示给店长 ——
			//	  一周里同一件货每天各记一次会显示成 7 个商品。
			ItemCount: items[u.Code].count,
			TopItems:  items[u.Code].top,
		})
	}

	if out.MissingDays() > 0 {
		return out, fmt.Errorf("%w: %s %s~%s 缺 %d 天（已覆盖 %d/%d）",
			ErrRangeIncomplete, branchID, from, to, out.MissingDays(), out.CoveredDays, out.TotalDays)
	}
	return out, nil
}

// productAgg 是商品跨日聚合的扫描目标。
//
// ⚠️ **必须显式 `.Table("sales_daily_products")`**：
//
//	没有它，GORM 会按结构体名推断表名（productAgg → product_aggs），
//	于是报「no such table」。而这个错只在**扫描目标不是 GORM 模型**时出现，
//	正常路径（Store.Save/Get）永远走不到 —— 所以它能通过所有既有测试。
type productAgg struct {
	ItemNo string  `gorm:"column:item_no"`
	Amount float64 `gorm:"column:amount"`
	Qty    float64 `gorm:"column:qty"`
}

// supplierAgg 是供应商跨日聚合的扫描目标（同样需要显式表名）。
type supplierAgg struct {
	Code   string  `gorm:"column:code"`
	Amount float64 `gorm:"column:amount"`
	Qty    float64 `gorm:"column:qty"`
}

// sumTotals 把每日的 totals 汇成区间总额。
//
// ⚠️ 毛利按**聚合后**的金额算，不是"每天毛利率的平均" ——
//
//	后者在某天亏损时会被其它天的盈利掩盖，
//	而店长看到的"这个月赚了多少"恰恰要那个合计数。
func sumTotals(rows []DailyTotals) TotalsRow {
	var agg TotalsRow
	for _, r := range rows {
		agg.NetAmount += r.NetAmount
		agg.NetQty += r.NetQty
		agg.RefCost += r.RefCost
		agg.TotalRows += r.TotalRows
		agg.CostedRows += r.CostedRows
	}
	if agg.TotalRows > 0 {
		agg.CostCoverage = float64(agg.CostedRows) / float64(agg.TotalRows)
	}
	agg.MarginAvailable = agg.CostCoverage >= minCostCoverage
	if agg.MarginAvailable {
		agg.RefMargin = agg.NetAmount - agg.RefCost
		if agg.NetAmount != 0 {
			agg.RefMarginPct = agg.RefMargin / agg.NetAmount
		}
	}
	return agg
}

// nameMeta 是"最新一天"里的名称与类别。
type nameMeta struct {
	Name     string
	Category string
}

// nameTableSpec 描述一张"名称表"（商品表 / 供应商表）。
//
// ⚠️ 用它而不是两个独立函数：
//
//	两张表的差别只有**表名、主键列名、以及商品表多一个类别**。
//	写成两个函数就会有一处改了一处没改 ——
//	而漏改的后果是商品全部没有名称
//	（症状是报告里一排「商品000123」，看起来像源库坏了）。
type nameTableSpec struct {
	table string
	key   string
	// hasCategory 为 true 时多投影一列 category。
	hasCategory bool
}

var (
	productNameTable  = nameTableSpec{table: "sales_daily_products", key: "item_no", hasCategory: true}
	supplierNameTable = nameTableSpec{table: "sales_daily_suppliers", key: "code"}
)

// latestNames 取区间内**最后一次采集**那一天的名称/类别。
func (s *Store) latestNames(ctx context.Context, branchID, from, to string, spec nameTableSpec) (map[string]nameMeta, error) {
	out := map[string]nameMeta{}
	if s == nil || s.db == nil {
		return out, errors.New("salesdaily: store 未初始化")
	}

	// ⚠️ 必须扫进 sql.NullString：
	//
	//	区间内一行都没有时 MAX(oper_date) 是 NULL，
	//	直接扫进 string 会报「converting NULL to string is unsupported」
	//	—— 那会把"这几天没数据"变成一个看不懂的 SQL 错误。
	var last sql.NullString
	if err := s.db.WithContext(ctx).
		Table(spec.table).
		Where("branch_id = ? AND oper_date >= ? AND oper_date <= ?", branchID, from, to).
		Select("MAX(oper_date)").Scan(&last).Error; err != nil {
		return nil, fmt.Errorf("salesdaily: 定位 %s 最新采集日: %w", spec.table, err)
	}
	if !last.Valid || strings.TrimSpace(last.String) == "" {
		return out, nil
	}

	// ⚠️ 这里按 spec.key 拼 SQL。它只来自本文件内的两个字面量
	//   （item_no / code），**不接受任何外部输入**；
	//   一旦有人把用户输入接到这里，必须改成白名单映射。
	//
	//   投影成固定别名 key_col，是为了让下面的扫描结构体不依赖列名。
	sel := spec.key + " AS key_col, name"
	if spec.hasCategory {
		sel += ", category"
	}
	type row struct {
		Key      string `gorm:"column:key_col"`
		Name     string `gorm:"column:name"`
		Category string `gorm:"column:category"`
	}
	rows := make([]row, 0, 512)
	if err := s.db.WithContext(ctx).
		Table(spec.table).
		Where("branch_id = ? AND oper_date = ?", branchID, last.String).
		Select(sel).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("salesdaily: 读 %s 最新名称: %w", spec.table, err)
	}
	for _, r := range rows {
		out[r.Key] = nameMeta{Name: r.Name, Category: r.Category}
	}
	return out, nil
}

// supItems 是某家供应商在区间内的商品明细汇总。
type supItems struct {
	count int
	top   []TopItemRow
}

// supplierTopItems 合并区间内每家供应商的 TopItems。
//
// # ⚠️ 这里有个**固有的精度损失**，必须说清楚
//
// 落库时每天只保留该供应商金额最高的 TopItemsPerSupplier 个商品，
// 所以跨日合并后得到的是「各天 top-N 的并集里金额最高的 N 个」，
// **不等于**区间内真正的 top-N ——
// 一件"每天都排第 5"的商品可能一件都留不下。
//
// 这是为控制存储体积做的取舍。用途（让店长看到"这家供货卖得动的是哪几件"）
// 在这个精度下依然成立；但**不要**把这里的 TopItems 当成精确排行榜，
// 也不要拿它做"某商品在这家供应商的销量排名"这类推论。
//
// 若将来要精确，办法是落库时不截断（体积换精度），
// 而不是让读侧假装自己是精确的。
func (s *Store) supplierTopItems(ctx context.Context, branchID, from, to string) (map[string]supItems, error) {
	type raw struct {
		Code     string `gorm:"column:code"`
		TopItems string `gorm:"column:top_items"`
	}
	var rows []raw
	if err := s.db.WithContext(ctx).
		// ⚠️ Table 必须显式给 —— 见 productAgg 的注释（raw → raws）。
		Table("sales_daily_suppliers").
		Where("branch_id = ? AND oper_date >= ? AND oper_date <= ?", branchID, from, to).
		Select("code, top_items").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("salesdaily: 读供应商商品明细: %w", err)
	}

	agg := map[string]map[string]*TopItemRow{}
	dailyCount := map[string]int{}
	for _, r := range rows {
		if strings.TrimSpace(r.TopItems) == "" {
			continue
		}
		var items []TopItemRow
		if err := json.Unmarshal([]byte(r.TopItems), &items); err != nil {
			return nil, fmt.Errorf("salesdaily: 解码供应商 %s 的商品: %w", r.Code, err)
		}
		dailyCount[r.Code] += len(items)
		m := agg[r.Code]
		if m == nil {
			m = map[string]*TopItemRow{}
			agg[r.Code] = m
		}
		for i := range items {
			cur, ok := m[items[i].ItemNo]
			if !ok {
				cp := items[i]
				m[items[i].ItemNo] = &cp
				continue
			}
			cur.Amount += items[i].Amount
			cur.Qty += items[i].Qty
		}
	}

	out := make(map[string]supItems, len(agg))
	for code, m := range agg {
		list := make([]TopItemRow, 0, len(m))
		for _, v := range m {
			list = append(list, *v)
		}
		sort.SliceStable(list, func(i, j int) bool { return list[i].Amount > list[j].Amount })
		if len(list) > AggregatedTopItems {
			list = list[:AggregatedTopItems]
		}
		out[code] = supItems{count: dailyCount[code], top: list}
	}
	// 没有任何商品明细的供应商也要有条目（count=0），
	// 否则上层遍历时会把它们当成"不存在"。
	for code, n := range dailyCount {
		if _, ok := out[code]; !ok {
			out[code] = supItems{count: n}
		}
	}
	return out, nil
}

// AggregatedTopItems 是区间聚合后每家供应商保留的商品条数。
//
// ⚠️ 与 collect.TopItemsPerSupplier（落库每天保留几条）**不是同一个量**：
//
//	这里是"合并多天之后保留几条"，取值应当 ≥ 落库时的每天条数，
//	否则合并只会让更少的天被并进来。
const AggregatedTopItems = 3

// MissingDates 返回区间内**没有快照**的日期清单。
//
// 供报告显式表达缺口用（"本月缺 10-03、10-11 两天的数据"）。
func (s *Store) MissingDates(ctx context.Context, branchID, from, to string) ([]string, error) {
	metas, err := s.ListMetas(ctx, branchID, from, to)
	if err != nil {
		return nil, err
	}
	have := make(map[string]bool, len(metas))
	for _, m := range metas {
		have[m.OperDate] = true
	}
	start, err := time.Parse("2006-01-02", from)
	if err != nil {
		return nil, fmt.Errorf("salesdaily: 解析 from %q: %w", from, err)
	}
	end, err := time.Parse("2006-01-02", to)
	if err != nil {
		return nil, fmt.Errorf("salesdaily: 解析 to %q: %w", to, err)
	}
	var missing []string
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		if !have[d.Format("2006-01-02")] {
			missing = append(missing, d.Format("2006-01-02"))
		}
	}
	return missing, nil
}
