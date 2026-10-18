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
	// loader 是"按区间读已定稿销售事实"的通道（PG）。
	//
	// ⚠️ 为 nil 时本服务**只**查 cube —— 那是未装配 PG 的形态，
	//   不是"PG 里没数据"，两者在健康检查与告警上必须区分。
	loader RangeLoader
	now    func() time.Time
}

// NewBuilder 构造组装器。
func NewBuilder(src DataSource, wx weather.Provider) *Builder {
	if wx == nil {
		wx = weather.NewUnavailableProvider("")
	}
	return &Builder{src: src, wx: wx, now: time.Now}
}

// WithRangeLoader 注入"已定稿销售事实"的读通道（PG）。
//
// ⚠️ 注入后，同一个营业日的销售数字会**优先**读已落库的那一份，
//
//	而不是现查源库 —— 因为源库在营业时段内还在被 POS 补写，
//	同一个营业日两次查询会得到两个数，而用户会把两次看到的数
//	当成"系统算错了"。
//
// 未覆盖整个区间时**整段**回退查源库，并在事实包里显式说明缺了几天；
// 绝不逐天混用（那会让同一份报告里的总额与品类合计来自两套口径）。
func (b *Builder) WithRangeLoader(l RangeLoader) *Builder {
	b.loader = l
	return b
}

// route 返回本次报告用的数据源。
//
// ⚠️ 未装配 loader 时**原样返回 b.src**，不做任何包装 ——
//
//	这样"没接 PG"这条路径的行为与改造前逐字相同，
//	而它的测试（builder_test.go 里那套 fakeSource）也继续有效。
func (b *Builder) route() DataSource {
	if b.loader == nil {
		return b.src
	}
	return newRoutedSource(b.loader, b.src)
}

// WithIgnores 注入人工忽略名单存储。
//
// ⚠️ 不注入也能跑，但那时供应商榜单**不排除任何自用渠道**，
//
//	且会在事实包里留下"尚未配置名单"的告警 ——
//	这是刻意的：改造后若没装配名单，宁可让人看见问题，也别静默出错。
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

	// ⚠️ 请求级数据源：装配了 PG 读通道时，销售四维按**区间**统一选路
	//   （见 routedSource 的注释）。未装配时它就是 b.src 本身。
	src := b.route()

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
	catCur, err := src.CategorySales(ctx, opt.StoreBranchID, f.Meta.From, f.Meta.To, topN)
	if err != nil {
		return nil, fmt.Errorf("build: 取本期品类销售: %w", err)
	}
	catPrev, err := src.CategorySales(ctx, opt.StoreBranchID, f.Meta.PrevFrom, f.Meta.PrevTo, topN)
	if err != nil {
		// 上期拿不到不算致命：没有环比的报告仍然有用。
		//
		// ⚠️ 文案必须精确到"**品类**环比"，不能说"本次不做环比"。
		//
		//	2026-10-17 之前这句话是对的（大盘由品类求和，品类没了环比就没了）。
		//	大盘改成独立查询之后，两条查询有了各自的成败 ——
		//	本分支失败而总额查询成功时，报告**仍然有大盘环比**。
		//	若沿用旧文案，就会出现同一份报告里
		//	"销售额 +30%（上期 10000 元）"与"本次不做环比"并存。
		sufficiency.Sufficient = false
		sufficiency.Issues = append(sufficiency.Issues,
			"上期品类数据获取失败，品类环比不可用（大盘环比不受影响）")
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

	// 大盘总额：**独立一次无分组查询**（2026-10-17 从"品类求和"改过来）。
	//
	// ⚠️ 原来这里写的是"大盘由品类求和 —— 顶层分类覆盖了全部销售额，
	//   刻意不用单独查一次总额，省一次查询"。那条推理**是错的**：
	//   "顶层分类覆盖全部销售额"这个前提只在**不截断、不丢行**时成立，
	//   而 CategorySales 恰恰带 Limit:topN 且丢弃空类目行。
	//   线上实测门店 00 / 2026-10-16：品类口径 2024.6 元 vs 供应商口径
	//   13003.8 元 —— 头条只统计到 15.6%，且**零报错**。
	//
	// → 省一次查询的成本，换头条数字错，不划算。
	curTotals, _ := b.loadTotals(ctx, opt.StoreBranchID, f.Meta.From, f.Meta.To, catCur, &sufficiency, src)
	f.Totals = curTotals

	// ---- 环比：与上期的对比 ----
	//
	// ⚠️ 这一段以前**整段不存在**（2026-10-17 修复）。
	//
	//	Deltas 全仓只有 engine_test.go 里被赋过值，生产代码从不赋值，
	//	于是它永远是零值：prev_net_amount=0、has_prev=false。
	//	后果不是"数字难看"，而是**报告自相矛盾**：
	//
	//	  prompt.go 走 `else if f.Deltas.HasPrev` 的 else 分支 →
	//	  头条写「销售额 X 元（无上期数据，不做环比）」，
	//	  而同一次生成的品类却带着真实环比（蛋类 +36.1%）——
	//	  因为品类环比来自 catPrev，与 Deltas 是两条独立通路。
	//	  LLM 于是把矛盾当成一条 warning 写进报告
	//	  （"本期无上期销售额数据，环比仅有百分比，缺基数参考"）。
	//
	// → 所以 has_prev 必须与上期**是否真的取到数据**一致，
	//   而不是恒为 false；缺它时环比无法在总览层解释，
	//   用户会以为"这家店第一天开张"。
	//
	// ⚠️ 上期查询失败（catPrev 为 nil）时保持 has_prev=false，
	//   与上面 sufficiency 里那条「上期数据获取失败」呼应 ——
	//   这两处必须同时成立，否则一条说有数据、一条说没数据。
	prevTotals, prevAuthentic := b.loadTotals(ctx, opt.StoreBranchID, f.Meta.PrevFrom, f.Meta.PrevTo, catPrev, &sufficiency, src)
	f.Deltas.PrevNetAmount = prevTotals.NetAmount
	f.Deltas.PrevNetQty = prevTotals.NetQty
	// ⚠️ 必须要求 prevAuthentic：降级求和值非零但偏低 85%，
	//   拿它当基会算出一个看起来正常、实则全错的环比。
	f.Deltas.HasPrev = prevAuthentic && prevTotals.NetAmount != 0
	if f.Deltas.HasPrev {
		f.Deltas.AmountAbs = f.Totals.NetAmount - prevTotals.NetAmount
		// pctChange 在 prev==0 时返回 0 而不是 +Inf，
		// 与 fact.Deltas.AmountPct 的注释一致。
		f.Deltas.AmountPct = pctChange(f.Totals.NetAmount, prevTotals.NetAmount)
	}

	if len(catCur) == 0 && f.Totals.NetAmount == 0 {
		sufficiency.Sufficient = false
		sufficiency.Issues = append(sufficiency.Issues, "本期无销售数据")
	}

	// ---- 4. 商品（本期 + 上期）----
	prodCur, err := src.ProductSales(ctx, opt.StoreBranchID, f.Meta.From, f.Meta.To, topN)
	if err != nil {
		return nil, fmt.Errorf("build: 取本期商品销售: %w", err)
	}
	prodPrev, err := src.ProductSales(ctx, opt.StoreBranchID, f.Meta.PrevFrom, f.Meta.PrevTo, topN)
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
	if s, err := src.StockByItems(ctx, opt.StoreBranchID, itemNos); err != nil {
		sufficiency.Sufficient = false
		sufficiency.Issues = append(sufficiency.Issues, "库存数据获取失败，缺货判断不可用")
	} else {
		stocks = s
	}

	// ---- 6. 供应商 ----
	f.Suppliers, f.ExcludedSuppliers = b.buildSuppliers(ctx, opt, f, topN, &sufficiency, src)

	// ---- 7. 商品排行 + 归因（依赖品类与库存）----
	f.TopProducts = buildProducts(prodCur, prodPrev, f.Categories, stocks)
	f.Stockouts = fact.BuildStockouts(f.TopProducts, stocks)

	// ---- 8. 规则摘要 ----
	f.RuleInsights = buildRuleInsights(f, stocks)

	// ---- 9. 取数来源说明 ----
	//
	// ⚠️ 装配了 PG 读通道时，这里会写清"哪一段没走已定稿数据、为什么"。
	//
	//	它是**面向使用者**的（店长/运维在 App 与审计里都能看到），
	//	不是给开发者的日志 —— 所以措辞要能回答一个运营问题：
	//	"这份报告的数字是哪儿来的、少了哪几天"。
	//
	// ⚠️ 但它**不**把 sufficient 置为 false：
	//
	//	"PG 没覆盖全、已回退查源库"并不代表这份报告不可信 ——
	//	回退路径取到的数字是完整的。把它算成"数据不足"会让
	//	"落库没跑起来"变成一条长期挂着的红色告警，
	//	反而把真正需要立刻处理的情况淹没掉。
	if rs, ok := src.(*routedSource); ok {
		for _, line := range rs.reportLines() {
			sufficiency.Issues = append(sufficiency.Issues, line)
		}
	}

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
// loadTotals 取大盘总额，查询失败时退回"品类求和"并**显式降级**。
//
// ⚠️ 第二个返回值 authoritative 必须在调用点参与判定，不能只读第一个：
//
//	降级求和值是**非零的**（线上实测偏低 85%），
//	若把它当权威基去算环比，会得到一个看起来正常、实则全错的百分比。
//	——这正是本函数要显式回报成败的原因。
//
// ⚠️ 退回而不是直接失败：总额拿不到时，一份带明确告警的报告
//
//	仍然比"没有报告"有用 —— 店长至少知道今天该去查源库。
//	但**必须**降级标记，否则数字偏低会被当成真实经营结果。
func (b *Builder) loadTotals(
	ctx context.Context,
	storeBranchID, from, to string,
	fallback []CategoryRow,
	suff *fact.Sufficiency,
	src DataSource,
) (fact.Totals, bool) {
	row, err := src.TotalsSales(ctx, storeBranchID, from, to)
	if err == nil {
		return totalsOf(row), true
	}
	suff.Sufficient = false
	suff.Issues = append(suff.Issues,
		"大盘总额查询失败，本次销售额可能偏低（已退回按品类求和）："+err.Error())
	return sumCategories(fallback), false
}

// sumCategories 把品类行汇成大盘 —— **仅作为总额查询失败时的降级路径**。
//
// ⚠️ 它天生带两个漏点（被 Limit 截断、被丢弃空类目行），
//
//	详见 DataSource.TotalsSales 的注释。正常路径**不要**用它当大盘。
func sumCategories(cats []CategoryRow) fact.Totals {
	var r TotalsRow
	for _, c := range cats {
		r.Amount += c.Amount
		r.Qty += c.Qty
		r.Cost += c.Cost
		r.CostedRow += c.CostedRow
		r.TotalRow += c.TotalRow
	}
	return totalsOf(r)
}

// totalsOf 把原始汇总行补上成本覆盖率与毛利。
//
// ⚠️ 毛利/覆盖率的推算**只允许在这里发生** ——
//
//	大盘走独立查询、降级路径走品类求和，两者必须共用这一段，
//	否则"查询成功时"与"降级时"会给出两套毛利口径。
func totalsOf(r TotalsRow) fact.Totals {
	t := fact.Totals{
		NetAmount: r.Amount,
		NetQty:    r.Qty,
		RefCost:   r.Cost,
	}
	costed, totalRows := float64(r.CostedRow), float64(r.TotalRow)
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
