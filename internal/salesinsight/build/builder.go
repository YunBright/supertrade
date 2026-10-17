// Package build 负责把散落在 cube 里的数据组装成一份**事实包**。
//
// # 组装顺序不可调换
//
//  1. 品类（本期 + 上期）→ 大盘、环比、可比性
//  2. 商品（本期 + 上期）→ 排行、环比
//  3. 库存（只查排行商品的）→ 缺货判定
//  4. 供应商（本期 + 上期 + 字典）→ 自用排除、单品分析
//  5. 归因（需要品类与库存都在场）
//  6. 规则摘要
//
// 归因必须最后做：它需要"这个商品所属品类跌了多少"和"当前库存多少"，
// 而后者又依赖第 3 步先确定"哪些商品值得查库存"（TopN，而不是全量）。
package build

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/salesinsight/calendar"
	"github.com/YunBright/supertrade/internal/salesinsight/fact"
	"github.com/YunBright/supertrade/internal/salesinsight/ignore"
	"github.com/YunBright/supertrade/internal/salesinsight/weather"
)

// Options 是组装参数。
type Options struct {
	// StoreBranchID 是门店（X-Branch-ID），必填。
	//
	// ⚠️ 它**不是**源库 branch_no（那是门店区域，门店的下一层）。
	// 它只做一件事：作为 X-Branch-ID header 路由到该门店专属 cube 实例。
	// 缺它会拿到全部分店的数据 —— 那不是本店日报。
	StoreBranchID string
	// BranchName 用于卡片抬头。
	BranchName string
	// Period 是周期与同期口径。
	Period calendar.Period
	// TopN 是排行条数（会被 fact.TopN 夹住）。
	TopN int
	// Location 是门店位置（天气用）。
	Location weather.Location
	// SelfUseKeywords 可选覆盖自用关键词（nil = 用默认）。
	SelfUseKeywords []string
}

// Builder 组装事实包。
type Builder struct {
	src DataSource
	wx  weather.Provider
	// ignores 是本门店的人工忽略名单存储（按门店分文件）。
	//
	// ⚠️ 它是**唯一**的供应商排除依据；为 nil 时所有自用供应商
	//   都会回到榜单，且上层会产生一条可见告警（不会静默）。
	ignores *ignore.Store
	now     func() time.Time
}

// NewBuilder 构造组装器。
func NewBuilder(src DataSource, wx weather.Provider) *Builder {
	if wx == nil {
		wx = weather.NewUnavailableProvider("")
	}
	return &Builder{src: src, wx: wx, now: time.Now}
}

// WithIgnores 注入人工忽略名单存储。
//
// ⚠️ 不注入也能跑，但那时供应商榜单**不排除任何自用渠道**，
//   且会在事实包里留下"尚未配置名单"的告警 ——
//   这是刻意的：改造后若没装配名单，宁可让人看见问题，也别静默出错。
func (b *Builder) WithIgnores(s *ignore.Store) *Builder {
	b.ignores = s
	return b
}

// WithNow 注入时钟（测试用）。
func (b *Builder) WithNow(f func() time.Time) *Builder {
	if f != nil {
		b.now = f
	}
	return b
}

// Build 组装一份事实包。
//
// # 部分失败的处理策略：**降级而不是整体失败**
//
// ⚠️ 这是本函数最重要的设计决策。
//
// 某一步失败（供应商字典查不到、天气接口超时）时，
// 报告**照出**，只是少了那一部分 —— 并在 DataSufficiency 里
// 标注缺了什么。
//
// 理由：日报的价值 60% 在数据（排行、环比、缺货），
// 而这些步骤里任何一个的短暂失败都不该让店长早上看不到任何东西。
// 相比之下，"取数失败 → 群里什么都没发生"是最难排查的失败形态
// （排查起点是"它到底跑没跑"，而 dapr 不提供 job 执行历史）。
//
// 只有**大盘**取数失败才整体失败：没有总额就没有任何结论。
func (b *Builder) Build(ctx context.Context, opt Options) (*fact.Fact, error) {
	if b.src == nil {
		return nil, fmt.Errorf("build: 数据源未配置")
	}
	if opt.StoreBranchID == "" {
		return nil, fmt.Errorf("build: 缺少门店（store = X-Branch-ID）")
	}

	topN := fact.TopN(opt.TopN)
	if opt.SelfUseKeywords != nil {
		fact.SetSelfUseKeywords(opt.SelfUseKeywords)
	}

	f := fact.Fact{
		Meta: fact.Meta{
			PeriodKind:          string(opt.Period.Kind),
			PeriodLabel:         opt.Period.Label(),
			From:                opt.Period.Cur.Start.Format(calendar.DateLayout),
			To:                  opt.Period.Cur.End.Format(calendar.DateLayout),
			PrevFrom:            opt.Period.Prev.Start.Format(calendar.DateLayout),
			PrevTo:              opt.Period.Prev.End.Format(calendar.DateLayout),
			BranchID:            opt.StoreBranchID,
			BranchName:          opt.BranchName,
			GeneratedAt:         b.now().Format(time.RFC3339),
			CalendarDataVersion: calendar.DataVersion,
		},
	}
	sufficiency := fact.Sufficiency{Sufficient: true}
	defer func() {
		f.DataSufficiency = sufficiency
	}()

	// ---- 1. 日历上下文（无外部依赖，必然成功）----
	curDay, err := calendar.Describe(f.Meta.From)
	if err != nil {
		return nil, fmt.Errorf("build: 解析本期起始日 %q: %w", f.Meta.From, err)
	}
	prevDay, err := calendar.Describe(f.Meta.PrevFrom)
	if err != nil {
		return nil, fmt.Errorf("build: 解析上期起始日 %q: %w", f.Meta.PrevFrom, err)
	}
	f.Context.From = curDay
	f.Context.PrevFrom = prevDay
	f.Context.Comparability = calendar.Compare(opt.Period.Cur, opt.Period.Prev)

	// ---- 2. 天气（失败降级，不影响主链路）----
	//
	// ⚠️ 这里对"日历与天气"取的是**本期首日**。
	//   周报/月报取首日而不是每天取，是因为后者意味着 30 次外部调用 ——
	//   而天气对周报结论的影响是"这周整体什么天气"，
	//   取首日是成本与价值的合理折中。
	if wx, err := b.wx.Fetch(ctx, f.Meta.From, opt.Location); err == nil {
		f.Context.Weather = wx
	} else {
		f.Context.Weather = weather.Info{
			Available:      false,
			DegradedReason: "天气查询失败：" + err.Error(),
		}
	}
	if !f.Context.Weather.Available && f.Context.Weather.DegradedReason != "" {
		// 天气缺失**不**标记为数据不足 ——
		// 它是增强项，不影响结论成立。记进 sufficiency 会让
		// "每天都有天气缺失"的门店误以为数据有问题。
		_ = f.Context.Weather.DegradedReason
	}

	// ---- 3. 品类（本期 + 上期）----
	catCur, err := b.src.CategorySales(ctx, opt.StoreBranchID, f.Meta.From, f.Meta.To, topN)
	if err != nil {
		return nil, fmt.Errorf("build: 取本期品类销售: %w", err)
	}
	catPrev, err := b.src.CategorySales(ctx, opt.StoreBranchID, f.Meta.PrevFrom, f.Meta.PrevTo, topN)
	if err != nil {
		// 上期拿不到不算致命：没有环比的报告仍然有用。
		sufficiency.Sufficient = false
		sufficiency.Issues = append(sufficiency.Issues, "上期数据获取失败，本次不做环比")
		// ⚠️ **仍然要用本期数据组装品类**，只是没有环比。
		//   最初写成 `if err != nil { ... } else { f.Categories = ... }`，
		//   于是上期查询一失败，本期排行也一起消失 ——
		//   而本期排行恰恰是日报里最不依赖上期、也最有价值的那部分。
		//   症状是"上期数据偶发失败 ⇒ 报告变成只有一句话的简报"，
		//   而这个耦合在正常路径上完全看不出来（测试全绿）。
		f.Categories = buildCategories(catCur, nil)
	} else {
		f.Categories = buildCategories(catCur, catPrev)
	}

	// 大盘由**品类**求和 —— 顶层分类覆盖了全部销售额。
	//
	// ⚠️ 这里刻意不用"单独查一次总额"：那会是第六次查询，
	//   而品类求和与总额一致（实测 salesagg 的类别口径已与既有报表对账）。
	//   多一次查询的成本换来的是"两个口径可能不一致"的风险。
	f.Totals = sumCategories(catCur)
	if len(catCur) == 0 {
		sufficiency.Sufficient = false
		sufficiency.Issues = append(sufficiency.Issues, "本期无销售数据")
	}

	// ---- 4. 商品（本期 + 上期）----
	prodCur, err := b.src.ProductSales(ctx, opt.StoreBranchID, f.Meta.From, f.Meta.To, topN)
	if err != nil {
		return nil, fmt.Errorf("build: 取本期商品销售: %w", err)
	}
	prodPrev, err := b.src.ProductSales(ctx, opt.StoreBranchID, f.Meta.PrevFrom, f.Meta.PrevTo, topN)
	if err != nil {
		sufficiency.Sufficient = false
		sufficiency.Issues = append(sufficiency.Issues, "上期商品数据获取失败，商品环比不可用")
	}

	// ---- 5. 库存（只查 TopN 商品，这是成本控制的关键）----
	//
	// ⚠️ 绝不能"查所有商品的库存"：那是 2.7 万行的量级，
	//   既慢又会把事实包撑爆。而缺货判断只需要看排行商品 ——
	//   排不进 TopN 的商品本来就不是店长关心的对象。
	itemNos := make([]string, 0, len(prodCur))
	for _, p := range prodCur {
		itemNos = append(itemNos, p.ItemNo)
	}
	// ⚠️ 必须在这里夹住 topN。
	//   DataSource.ProductSales 的 Limit 决定了 DaprSource 返回多少行，
	//   但**别的实现可能返回更多**（例如缓存命中、或将来改成全量后自己截断）。
	//   缺了这一夹，库存查询就会带上全部商品 ——
	//   实测这个 bug 的表现是：100 个商品的门店，库存查询也是 100 个，
	//   既慢又超出 maxStockQuery 的意图，而所有测试都绿。
	stocks := map[string]fact.StockRecord{}
	if len(prodCur) > topN {
		prodCur = prodCur[:topN]
		itemNos = itemNos[:topN]
	}
	if s, err := b.src.StockByItems(ctx, opt.StoreBranchID, itemNos); err != nil {
		sufficiency.Sufficient = false
		sufficiency.Issues = append(sufficiency.Issues, "库存数据获取失败，缺货判断不可用")
	} else {
		stocks = s
	}

	// ---- 6. 供应商 ----
	f.Suppliers, f.ExcludedSuppliers = b.buildSuppliers(ctx, opt, f, topN, &sufficiency)

	// ---- 7. 商品排行 + 归因（依赖品类与库存）----
	f.TopProducts = buildProducts(prodCur, prodPrev, f.Categories, stocks)
	f.Stockouts = fact.BuildStockouts(f.TopProducts, stocks)

	// ---- 8. 规则摘要 ----
	f.RuleInsights = buildRuleInsights(f, stocks)

	return &f, nil
}

// categoryChangeByName 给出每个品类的环比百分比，供归因使用。
func categoryChangeByName(cats []fact.Category) map[string]float64 {
	m := make(map[string]float64, len(cats))
	for _, c := range cats {
		if c.HasPrev {
			m[c.Name] = c.DeltaPct
		}
	}
	return m
}

// buildCategories 组装品类排行与大盘。
func buildCategories(cur, prev []CategoryRow) []fact.Category {
	prevByName := map[string]CategoryRow{}
	for _, p := range prev {
		prevByName[p.Name] = p
	}

	total := 0.0
	for _, c := range cur {
		total += c.Amount
	}

	out := make([]fact.Category, 0, len(cur))
	for _, c := range cur {
		c2 := fact.Category{
			Name:   c.Name,
			Amount: c.Amount,
		}
		if total > 0 {
			c2.AmountShare = c.Amount / total
		}
		if p, ok := prevByName[c.Name]; ok {
			c2.PrevAmount = p.Amount
			c2.DeltaAbs = c.Amount - p.Amount
			c2.DeltaPct = pctChange(c.Amount, p.Amount)
			c2.HasPrev = true
		}
		out = append(out, c2)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Amount > out[j].Amount })
	for i := range out {
		out[i].Rank = i + 1
	}
	// 上期排名，用于识别"新晋/掉出"的品类
	prevRank := map[string]int{}
	for i, p := range prev {
		prevRank[p.Name] = i + 1
	}
	for i := range out {
		if r, ok := prevRank[out[i].Name]; ok {
			out[i].PrevRank = r
			out[i].RankChanged = r != out[i].Rank
		}
	}
	return out
}

// sumCategories 求和出大盘与毛利可用性。
func sumCategories(cats []CategoryRow) fact.Totals {
	var t fact.Totals
	var costed, totalRows float64
	for _, c := range cats {
		t.NetAmount += c.Amount
		t.NetQty += c.Qty
		t.RefCost += c.Cost
		costed += float64(c.CostedRow)
		totalRows += float64(c.TotalRow)
	}
	if totalRows > 0 {
		t.CostCoverage = costed / totalRows
	}
	// ⚠️ 与 salesagg.MinCostCoverage 同为 0.5，但**独立定义**。
	//   覆盖率不足时不显示毛利：按 cost=0 算会显示"毛利率 100%"，
	//   一个"看起来完全正常、实际毫无意义"的数比不显示更坏。
	t.MarginAvailable = t.CostCoverage >= fact.MinCostCoverage
	if t.MarginAvailable {
		t.RefMargin = t.NetAmount - t.RefCost
		if t.NetAmount != 0 {
			t.RefMarginPct = t.RefMargin / t.NetAmount
		}
	}
	return t
}

// buildProducts 组装商品排行（含环比与归因）。
func buildProducts(cur, prev []ProductRow, cats []fact.Category, stocks map[string]fact.StockRecord) []fact.TopProduct {
	prevByNo := map[string]ProductRow{}
	for _, p := range prev {
		prevByNo[p.ItemNo] = p
	}
	catChange := categoryChangeByName(cats)

	out := make([]fact.TopProduct, 0, len(cur))
	for _, c := range cur {
		tp := fact.TopProduct{
			ItemNo:   c.ItemNo,
			Name:     c.Name,
			Amount:   c.Amount,
			Qty:      c.Qty,
			Category: c.Category,
		}
		if p, ok := prevByNo[c.ItemNo]; ok {
			tp.PrevAmount = p.Amount
			tp.DeltaPct = pctChange(c.Amount, p.Amount)
			tp.HasPrev = true
		}
		if st, ok := stocks[c.ItemNo]; ok {
			tp.CurrentStock = st.Quantity
			tp.MinStock = st.MinQty
			tp.HasStock = true
			tp.StockStatus = fact.ClassifyStock(st)
		} else {
			tp.StockStatus = fact.StockNone
		}
		// 归因：有库存且有上期数据时才可能得出"下降原因"
		cause, evidence := fact.Attribute(fact.AttributionInput{
			Product:           tp,
			PrevAmount:        tp.PrevAmount,
			HasPrev:           tp.HasPrev,
			Stock:             stockOf(stocks, c.ItemNo),
			CategoryChangePct: catChange[c.Category],
			HasCategoryChange: func() bool { _, ok := catChange[c.Category]; return ok }(),
		})
		tp.DeclineCause = cause
		tp.DeclineEvidence = evidence
		out = append(out, tp)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Amount > out[j].Amount })
	for i := range out {
		out[i].Rank = i + 1
	}
	return out
}

// stockOf 取库存快照；不存在时返回 HasRow=false 的零值。
func stockOf(stocks map[string]fact.StockRecord, itemNo string) fact.StockRecord {
	if st, ok := stocks[itemNo]; ok {
		return st
	}
	return fact.StockRecord{ItemNo: itemNo, HasRow: false}
}

// buildRuleInsights 生成规则摘要（确定性，不依赖 LLM）。
func buildRuleInsights(f fact.Fact, stocks map[string]fact.StockRecord) fact.RuleInsights {
	var ri fact.RuleInsights

	ri.StockoutProducts = productNames(f.Stockouts, fact.SummaryItemN)

	// 增长品类：有上期且涨幅明显的
	var growing, declining []fact.Category
	for _, c := range f.Categories {
		if !c.HasPrev {
			continue
		}
		if c.DeltaPct > 10 {
			growing = append(growing, c)
		} else if c.DeltaPct < -10 {
			declining = append(declining, c)
		}
	}
	ri.GrowingCategories = categoryNames(growing, fact.SummaryItemN)
	ri.DecliningCategories = categoryNames(declining, fact.SummaryItemN)

	// 增长最快的商品
	var fast []fact.TopProduct
	for _, p := range f.TopProducts {
		if p.HasPrev && p.DeltaPct > 20 && p.Amount > 0 {
			fast = append(fast, p)
		}
	}
	sort.SliceStable(fast, func(i, j int) bool { return fast[i].DeltaPct > fast[j].DeltaPct })
	ri.FastMovers = productNames(fast, fact.SummaryItemN)

	// 最佳供应商与最佳单品
	for i, s := range f.Suppliers {
		if i >= fact.SummaryItemN {
			break
		}
		ri.BestSuppliers = append(ri.BestSuppliers, s.Name)
		for _, tp := range s.TopProducts {
			if len(ri.BestSupplierProducts) >= fact.SummaryItemN {
				break
			}
			ri.BestSupplierProducts = append(ri.BestSupplierProducts, fact.SupplierProduct{
				Supplier: s.Name,
				ItemNo:   tp.ItemNo,
				Name:     tp.Name,
				Amount:   tp.Amount,
				Qty:      tp.Qty,
			})
		}
	}
	return ri
}

// productNames 取前 n 个商品名字（去重、跳过空名）。
//
// 去重是必要的：同一名字可能对应多个编码（不同规格同名），
// 不去重会让摘要变成"西红柿、西红柿"，店长会以为有两个商品。
func productNames(items []fact.TopProduct, n int) []string {
	if n <= 0 {
		return nil
	}
	seen := make(map[string]bool, len(items))
	out := make([]string, 0, n)
	for _, it := range items {
		name := strings.TrimSpace(it.Name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
		if len(out) >= n {
			break
		}
	}
	return out
}

// categoryNames 取前 n 个品类名（去重）。
func categoryNames(items []fact.Category, n int) []string {
	if n <= 0 {
		return nil
	}
	seen := make(map[string]bool, len(items))
	out := make([]string, 0, n)
	for _, it := range items {
		name := strings.TrimSpace(it.Name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
		if len(out) >= n {
			break
		}
	}
	return out
}

// pctChange 计算环比百分比。
//
// ⚠️ 上期为 0 时返回 0 而不是 +Inf：
// 一个无意义的巨大百分比会让卡片显示 "+∞%"，
// 而它在 JSON 里会让下游解析失败或算出垃圾数字。
// 此时应看 DeltaAbs。
func pctChange(cur, prev float64) float64 {
	if prev == 0 {
		return 0
	}
	return (cur - prev) / prev * 100
}
