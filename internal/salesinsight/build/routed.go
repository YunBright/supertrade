package build

import (
	"context"
	"fmt"
	"sync"

	"github.com/YunBright/supertrade/internal/salesdaily"
	"github.com/YunBright/supertrade/internal/salesinsight/fact"
	"github.com/YunBright/supertrade/internal/salesinsight/fact/supplier"
)

// RangeLoader 是"按区间读一份**已定稿**的销售事实"的能力。
//
// 生产实现是 internal/salesdaily.Store（PG）。
// 它是可选的：未装配时本包全部走 cube（见 Builder.WithRangeLoader）。
type RangeLoader interface {
	LoadRange(ctx context.Context, branchID, from, to string) (salesdaily.RangeFacts, error)
}

// historyRanger 是 LoadRange 之外额外的诊断能力（可缺省）。
//
// ⚠️ 刻意做成**可选的第二个接口**而不是塞进 RangeLoader：
//
//	它是纯诊断信息，缺了不影响正确性（只是回退文案少一句区分），
//	而塞进主接口会让所有实现（包括测试替身）都必须实现它。
type historyRanger interface {
	FirstCollectedDate(ctx context.Context, branchID string) (string, bool, error)
}

// routedSource 按**区间**决定销售数字的来源：整段要么全 PG，要么全 cube。
//
// # ⚠️ 为什么必须是"按区间"而不是"按查询"
//
// 改造前每日统计卡片与 AI 日报各自现查 cube，于是 2026-10-16
// 出现了同一个营业日的两个口径（品类 2024.6 元 vs 供应商 13003.8 元）——
// 差 6 倍，**零报错**。那不是某次查询算错，
// 而是**同一个数字被算了两遍**，两遍各漏一半。
//
// 落 PG 之后，如果读侧写成"有 PG 的天读 PG、没有的天查 cube"，
// 同一份报告里的"总额"与"品类合计"就来自两套口径 ——
// **同一个 bug 原样复现**，而且这次更难发现（数据源不同源）。
//
// → 所以路由决策只做一次：同一个 (branch, from, to) 的四个维度
//
//	必须来自同一处。本结构用 periodDecision 缓存决策来保证这一点 ——
//	不是靠约定，是靠"取到数据之后就不可能再换源"。
//
// # 上期与本期各自独立选路
//
// 本期走 PG、上期走 cube 是**允许**的：两者各自内部自洽，
// 环比比较的是两个各自完整的口径。
// 危险的是"同一个区间内部混"，那才是分叉。
//
// # 库存与天气**永远**走 cube
//
// 快照里没有库存（它是实时量，不是"当天的事实"），
// 天气更是外部 API。本结构只路由**销售四维**。
type routedSource struct {
	loader RangeLoader
	cube   DataSource

	mu   sync.Mutex
	seen map[string]*periodDecision
}

// periodDecision 是某个区间的路由结果。
type periodDecision struct {
	fromPG bool
	// facts 仅在 fromPG 时非 nil。
	facts salesdaily.RangeFacts
	// reason 非空表示**没有**走 PG 时给出的可读原因。
	//
	// ⚠️ 它会进 DataSufficiency：不是给开发者看的日志，
	//	而是给店长/运维看的"这次为什么用了另一条路"。
	reason string
}

// newRoutedSource 构造一个**请求级**的路由数据源。
//
// ⚠️ 必须是请求级、不能挂在 Builder 上共享：
//
//	决策缓存是"这个区间用哪条路"，
//	挂在单例上会让第二次请求复用第一次的决策 ——
//	而采集 job 可能刚把缺失的那天补上，
//	于是这份报告永远看不到它（直到进程重启）。
func newRoutedSource(loader RangeLoader, cube DataSource) *routedSource {
	return &routedSource{loader: loader, cube: cube, seen: map[string]*periodDecision{}}
}

// decide 返回该区间的路由决策（每个区间只算一次）。
func (s *routedSource) decide(ctx context.Context, branchID, from, to string) *periodDecision {
	key := branchID + "\x00" + from + "\x00" + to

	s.mu.Lock()
	if d, ok := s.seen[key]; ok {
		s.mu.Unlock()
		return d
	}
	s.mu.Unlock()

	d := s.resolve(ctx, branchID, from, to)

	s.mu.Lock()
	if existing, ok := s.seen[key]; ok {
		// 并发下另一个 goroutine 已经算过了，用先到的那个 ——
		// 保证同一个区间在一次报告里**永远**是同一个决策。
		s.mu.Unlock()
		return existing
	}
	s.seen[key] = d
	s.mu.Unlock()
	return d
}

// resolve 真正做一次 PG 读取并判定成败。
func (s *routedSource) resolve(ctx context.Context, branchID, from, to string) *periodDecision {
	if s.loader == nil {
		return &periodDecision{reason: ""}
	}
	facts, err := s.loader.LoadRange(ctx, branchID, from, to)
	if err == nil {
		return &periodDecision{fromPG: true, facts: facts, reason: pgNote(facts)}
	}

	// PG 没覆盖全 → 整段回退，**不逐天混**。
	// 理由见本文件的注释。
	return &periodDecision{reason: s.fallbackNote(ctx, branchID, from, to, err, facts)}
}

// pgNote 在**成功**走 PG 时也给一句说明。
//
// ⚠️ 它只在"区间多于一天"且"有日子被次日校正过"时才说话：
//
//	"本月 12 天是被次日 01:05 补齐的"是店长用得上的一句话
//	（说明思迅日结经常晚，得人工留意）；
//	而每天一句"这天被校正过"是噪声。
func pgNote(facts salesdaily.RangeFacts) string {
	if facts.TotalDays <= 1 || facts.CorrectedDays == 0 {
		return ""
	}
	return fmt.Sprintf("销售数据存储中 %s~%s 共 %d 天，其中 %d 天是次日校正后定稿的",
		facts.From, facts.To, facts.CoveredDays, facts.CorrectedDays)
}

// fallbackNote 生成"为什么没用上已定稿库"的可读原因。
//
// ⚠️ 关键是把两种"缺数据"分开：
//
//	① 历史还没攒够（缺失的日子都在首次采集日之前）→ 该等
//	② 中间/末尾漏采（缺失的日子在首次采集日之后）→ 该查采集日志
//
//	两者的处置完全相反，不分开就会让运营跑去查采集配置，
//	而真正的问题只是"这个功能上周才上线"。
func (s *routedSource) fallbackNote(
	ctx context.Context, branchID, from, to string, err error, facts salesdaily.RangeFacts,
) string {
	// 一行都没有：采集压根没跑过，或这一家店还没配。
	if facts.CoveredDays == 0 {
		if first, ok := s.firstDate(ctx, branchID); ok {
			return fmt.Sprintf("销售数据存储里 %s~%s 没有采集结果"+
				"（该门店最早的采集日是 %s），本次现查源库", from, to, first)
		}
		return fmt.Sprintf("销售数据存储里 %s~%s 没有采集结果，本次现查源库", from, to)
	}

	if first, ok := s.firstDate(ctx, branchID); ok && first > from {
		// 首次采集日落在区间中间或之后 ⇒ 缺失的都是"改造前的历史"。
		return fmt.Sprintf("销售数据存储里 %s~%s 只覆盖了 %d/%d 天"+
			"（该门店从 %s 起才开始采集落库，此前的数据不在库里），本次现查源库",
			from, to, facts.CoveredDays, facts.TotalDays, first)
	}
	return fmt.Sprintf("销售数据存储里 %s~%s 只覆盖了 %d/%d 天，本次现查源库",
		from, to, facts.CoveredDays, facts.TotalDays)
}

// firstDate 读该门店最早的采集日；读不到就当没有。
func (s *routedSource) firstDate(ctx context.Context, branchID string) (string, bool) {
	h, ok := s.loader.(historyRanger)
	if !ok {
		return "", false
	}
	first, ok, err := h.FirstCollectedDate(ctx, branchID)
	if err != nil || !ok {
		return "", false
	}
	return first, true
}

// reportLines 返回本次路由的可读说明（进 DataSufficiency）。
func (s *routedSource) reportLines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, d := range s.seen {
		if d.reason != "" {
			out = append(out, d.reason)
		}
	}
	return out
}

// ---- DataSource 实现 ----

// TotalsSales 实现 DataSource。
func (s *routedSource) TotalsSales(ctx context.Context, branchID, from, to string) (TotalsRow, error) {
	d := s.decide(ctx, branchID, from, to)
	if !d.fromPG {
		return s.cube.TotalsSales(ctx, branchID, from, to)
	}
	return TotalsRow{
		Amount:    d.facts.Totals.NetAmount,
		Qty:       d.facts.Totals.NetQty,
		Cost:      d.facts.Totals.RefCost,
		CostedRow: d.facts.Totals.CostedRows,
		TotalRow:  d.facts.Totals.TotalRows,
	}, nil
}

// CategorySales 实现 DataSource。
func (s *routedSource) CategorySales(ctx context.Context, branchID, from, to string, topN int) ([]CategoryRow, error) {
	d := s.decide(ctx, branchID, from, to)
	if !d.fromPG {
		return s.cube.CategorySales(ctx, branchID, from, to, topN)
	}
	out := make([]CategoryRow, 0, len(d.facts.Categories))
	for _, c := range d.facts.Categories {
		// ⚠️ 空类目名无法展示，也不构成月报 GROUP BY 的有效键。
		//   落库时已经过滤过一次，这里是第二道 —— 便宜的防御。
		if c.Name == "" {
			continue
		}
		out = append(out, CategoryRow{
			Name: c.Name, Amount: c.Amount, Qty: c.Qty, Cost: c.Cost,
			CostedRow: c.CostedRow, TotalRow: c.TotalRow,
		})
	}
	return topNCategories(out, topN), nil
}

// ProductSales 实现 DataSource。
func (s *routedSource) ProductSales(ctx context.Context, branchID, from, to string, topN int) ([]ProductRow, error) {
	d := s.decide(ctx, branchID, from, to)
	if !d.fromPG {
		return s.cube.ProductSales(ctx, branchID, from, to, topN)
	}
	out := make([]ProductRow, 0, len(d.facts.Products))
	for _, p := range d.facts.Products {
		if p.ItemNo == "" {
			continue
		}
		out = append(out, ProductRow{
			ItemNo: p.ItemNo, Name: p.Name, Category: p.Category,
			Amount: p.Amount, Qty: p.Qty,
		})
	}
	return topNProducts(out, topN), nil
}

// SupplierSales 实现 DataSource。
//
// ⚠️ PG 路径**自带供应商名称**（采集时补过一次），
//
//	而 cube 路径没有 —— supplier 是 storage: duck 模型，
//	与 sales_report（live）不能 join（见 collect.SupplierNames 的注释）。
//	所以两条路返回的 Name 一空一满，调用方必须两者都处理。
func (s *routedSource) SupplierSales(ctx context.Context, branchID, from, to string, topN int) ([]SupplierSalesRow, error) {
	d := s.decide(ctx, branchID, from, to)
	if !d.fromPG {
		return s.cube.SupplierSales(ctx, branchID, from, to, topN)
	}
	out := make([]SupplierSalesRow, 0, len(d.facts.Suppliers))
	for _, u := range d.facts.Suppliers {
		if u.Code == "" {
			continue
		}
		out = append(out, SupplierSalesRow{
			Code:         u.Code,
			Name:         u.Name,
			Amount:       u.Amount,
			Qty:          u.Qty,
			ProductCount: u.ItemCount,
			TopItems:     topItemsOf(u.TopItems),
		})
	}
	return topNSuppliers(out, topN), nil
}

// SupplierDict 实现 DataSource：**永远**走 cube。
//
// ⚠️ PG 路径其实用不上它（快照自带采集时补好的名称），但这里**仍然**照查：
//
//	buildSuppliers 用字典做"名字兜底"，让两条取数路径共用同一段逻辑。
//	若在 PG 路径上跳过，就多出一条"只在某些数据状态下才走到"的分支 ——
//	而那种分支正是"PG 路径下某家供应商没名字"这类缺陷的藏身处。
//	代价是一次 `in` 几十个编码的小查询，可以忽略。
//
// ⚠️ 它**不是**排除自用供应商的依据：
//
//	排除只认人工名单（ignore.IndexForSales，按流水编码对齐），
//	与字典无关。所以字典查不到**不会**让自用供应商混进榜单 ——
//	buildSuppliers 里那条告警的措辞偏保守，但结论仍然成立。
func (s *routedSource) SupplierDict(ctx context.Context, storeBranchID string, codes []string) ([]supplier.Record, error) {
	return s.cube.SupplierDict(ctx, storeBranchID, codes)
}

// StockByItems 实现 DataSource：**永远**走 cube（库存是实时量）。
func (s *routedSource) StockByItems(ctx context.Context, storeBranchID string, itemNos []string) (map[string]fact.StockRecord, error) {
	return s.cube.StockByItems(ctx, storeBranchID, itemNos)
}

// topNCategories 按金额截断（快照侧本来就按金额降序）。
func topNCategories(rows []CategoryRow, topN int) []CategoryRow {
	if topN > 0 && len(rows) > topN {
		return rows[:topN]
	}
	return rows
}

// topNProducts 按金额截断。
func topNProducts(rows []ProductRow, topN int) []ProductRow {
	if topN > 0 && len(rows) > topN {
		return rows[:topN]
	}
	return rows
}

// topNSuppliers 按金额截断。
func topNSuppliers(rows []SupplierSalesRow, topN int) []SupplierSalesRow {
	if topN > 0 && len(rows) > topN {
		return rows[:topN]
	}
	return rows
}

// topItemsOf 把存储层的商品明细转成 build 层的形状。
func topItemsOf(rows []salesdaily.TopItemRow) []SupplierItem {
	if len(rows) == 0 {
		return nil
	}
	out := make([]SupplierItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, SupplierItem{
			ItemNo: r.ItemNo, Name: r.Name, Amount: r.Amount, Qty: r.Qty,
		})
	}
	return out
}
