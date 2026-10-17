package fact

import (
	"fmt"
	"sort"
	"strings"
)

// StockRecord 是一个商品的当前库存快照。
type StockRecord struct {
	ItemNo string
	// Quantity 是当前库存数量。
	//
	// ⚠️ 单位是**称重重量**(生鲜按 kg)或源库的计量单位，
	//   不是件数。所以它只能与**同期销量**比，不能与"卖了几件"比。
	Quantity float64
	// MinQty 是安全库存线（源库 min_qty）。为 0 表示门店没设。
	MinQty float64
	// HasRow 表示是否真的查到了一条库存记录。
	//
	// ⚠️ 它与 Quantity==0 必须区分：查不到记录 ≠ 库存为零。
	//   "没查到"通常意味着这个商品根本没进过库存系统
	//   （临时摊位的生鲜、临时促销品），
	//   把它当成缺货会让店长去补一批本来就不该管的货。
	HasRow bool
}

// ClassifyStock 把库存快照转成状态。
func ClassifyStock(r StockRecord) StockStatus {
	if !r.HasRow {
		return StockNone
	}
	if r.Quantity <= 0 {
		return StockOut
	}
	// 低于安全库存线即视为偏低。
	// ⚠️ MinQty==0（门店没设安全库存）时**不能**判为 low ——
	//   那会让几乎所有商品都变成"低库存"，报告立刻失去筛选价值。
	if r.MinQty > 0 && r.Quantity < r.MinQty {
		return StockLow
	}
	return StockOK
}

// AttributionInput 是归因所需的全部输入。
type AttributionInput struct {
	// Product 是商品本期表现。
	Product TopProduct
	// PrevAmount 是上期该商品销售额。
	PrevAmount float64
	// HasPrev 表示上期是否有数据（无 = 新商品，不下结论）。
	HasPrev bool
	// Stock 是当前库存。
	Stock StockRecord
	// CategoryChangePct 是**该商品所属品类**的环比变化（%）。
	//
	// ⚠️ 它是区分"单品问题"和"整体下滑"的唯一依据：
	//   没有它，所有下滑都会被归到单品头上，而店长会照着砍品。
	CategoryChangePct float64
	// HasCategoryChange 表示品类对比是否可用。
	HasCategoryChange bool
	// TotalChangePct 是全店大盘环比（%），作为最后一道兜底。
	TotalChangePct float64
}

// 判定阈值。
//
// 为什么是这些数字：它们都是**相对量**而不是绝对量，
// 因为不同品类的正常波动幅度差异极大（生鲜日波动 30% 属正常，
// 标品日波动 3% 就值得注意）。用绝对额判断会让生鲜永远"正常"、
// 标品永远"异常"。
const (
	// DeclineThresholdPct 是"算作下降"的百分比。
	DeclineThresholdPct = -20.0
	// MarketWideThresholdPct 是"品类也在跌这么多"就判为整体下滑。
	MarketWideThresholdPct = -10.0
	// StockLowRatio 是库存低于安全线多少比例算"低"（归因用）。
	StockLowRatio = 1.0
)

// Attribute 判定某个商品的销量下降原因。
//
// # 这是本产品最核心的判断
//
// 店长早上看到"某商品销量跌 60%"时，需要知道该做什么：
//
//	缺货     → 紧急补货（砍掉它 = 永久失去一个爆款）
//	整体下滑 → 检查天气/客流/竞品（砍单品 = 误伤）
//	需求下降 → 考虑换供应商或下架（这才是真的该砍）
//
// 三种原因的动作完全相反。判错的代价是**真实的经营损失**，
// 所以它必须由数据规则算出，**不能交给 LLM 自由发挥** ——
// LLM 会倾向于给出听起来合理的归因，而它拿不到库存数据。
//
// # 判定顺序（顺序即优先级，不可调换）
//
//	1. 库存为 0 且销量下滑 → 缺货          【最重要，优先级最高】
//	2. 库存低于安全线     → 低库存导致
//	3. 品类同步下滑       → 整体下滑
//	4. 其余               → 单纯需求下降
func Attribute(in AttributionInput) (DeclineCause, string) {
	// 没有上期数据 = 新商品。"没有对比"不能推导出任何下降结论。
	if !in.HasPrev {
		return CauseNewOrUnknown, "上期无销售数据，暂不做归因（可能是新品）"
	}

	pct := in.Product.DeltaPct
	// 用传入的 PrevAmount 重算，而不是信 Product.DeltaPct ——
	// 上层组装 TopProduct 时可能走了不同的环比口径，两处不一致会让
	// 归因与报表上的数字打架。
	if in.PrevAmount > 0 {
		pct = (in.Product.Amount - in.PrevAmount) / in.PrevAmount * 100
	}

	// 没跌就不需要归因。
	if pct > DeclineThresholdPct {
		return CauseNone, ""
	}

	status := ClassifyStock(in.Stock)

	// ---- 1. 缺货：最高优先级 ----
	//
	// ⚠️ 为什么缺货优先于"整体下滑"：
	//   即使全店今天都在跌，一个没货的商品仍然是**因缺货而没卖成**。
	//   若先判"整体下滑"，店长会认为"大盘不好没办法"，
	//   于是不补货 —— 而补货之后这个商品照样能卖。
	if status == StockOut {
		return CauseStockout, fmt.Sprintf(
			"当前库存为 0，但上期仍售出 %.2f 元 —— 这次没卖出去是**缺货**，不是卖不动，请优先补货",
			in.PrevAmount)
	}

	// ---- 2. 低库存 ----
	if status == StockLow {
		return CauseLowStock, fmt.Sprintf(
			"当前库存 %.2f，低于安全库存 %.2f —— 备货不足可能已经影响了销售，建议尽快补货",
			in.Stock.Quantity, in.Stock.MinQty)
	}

	// ---- 3. 整体下滑 ----
	//
	// 判定用**品类**而不是只看大盘：全店跌 5% 而这个品类跌 40%
	// 时，这个单品更可能是被品类拖累，而不是它自己出了问题。
	if in.HasCategoryChange && in.CategoryChangePct <= MarketWideThresholdPct {
		return CauseMarketWide, fmt.Sprintf(
			"所属品类同期下降 %.1f%%，整体环境导致，不是单个商品的问题 —— "+
				"建议先观察整体，不宜据此下架", in.CategoryChangePct)
	}

	// ---- 4. 单纯的品需求下降 ----
	//
	// 到这一步才有资格说"卖不动"。有货、品类没跌，只有它在跌。
	return CauseDemandDrop, fmt.Sprintf(
		"有货（库存 %.2f）且所属品类未大幅下滑，仅该商品下降 %.1f%% —— "+
			"可能是需求减弱或同类竞争，建议考虑换供应商或调整陈列",
		in.Stock.Quantity, pct)
}

// StockEvidence 构造库存证据白话（用于卡片/审计）。
func StockEvidence(r StockRecord) string {
	if !r.HasRow {
		return "未查到库存记录（该商品可能未纳入库存系统）"
	}
	return fmt.Sprintf("当前库存 %.2f，安全库存 %.2f", r.Quantity, r.MinQty)
}

// BuildStockouts 从排行商品里挑出需要紧急补货的。
//
// ⚠️ 判据是"**卖得好**但没货"，不是"没货"。
//   一个从来没卖过的商品缺货不值得补 —— 补了也是库存积压。
//   只有"曾经卖得动、现在断货"的才值得立刻补。
//
// 这是本产品 P0 价值之一：让店长早上就知道该订什么货。
func BuildStockouts(products []TopProduct, stocks map[string]StockRecord) []TopProduct {
	var out []TopProduct
	for _, p := range products {
		st, ok := stocks[p.ItemNo]
		if !ok {
			continue
		}
		status := ClassifyStock(st)
		if status != StockOut && status != StockLow {
			continue
		}
		// 必须"卖得好"。本期卖得多，或上期卖得多（现在断了），都要。
		soldWell := p.Amount > 0 || p.PrevAmount > 0
		if !soldWell {
			continue
		}
		p.StockStatus = status
		p.CurrentStock = st.Quantity
		p.HasStock = true
		p.MinStock = st.MinQty
		p.DeclineCause = DeclineCause("")
		if status == StockOut {
			p.DeclineEvidence = "已断货：当前库存 0" + stockSalesHint(p)
		} else {
			p.DeclineEvidence = fmt.Sprintf("库存偏低：当前 %.2f，低于安全库存 %.2f%s",
				st.Quantity, st.MinQty, stockSalesHint(p))
		}
		out = append(out, p)
	}
	// 断货的排前面，其次按金额降序 —— 店长最想先看到最该补的。
	sort.SliceStable(out, func(i, j int) bool {
		oi, oj := out[i].StockStatus == StockOut, out[j].StockStatus == StockOut
		if oi != oj {
			return oi
		}
		return out[i].Amount+out[i].PrevAmount > out[j].Amount+out[j].PrevAmount
	})
	return out
}

// stockSalesHint 附加"卖得好"的证据。
func stockSalesHint(p TopProduct) string {
	switch {
	case p.PrevAmount > 0 && p.Amount > 0:
		return fmt.Sprintf("；本期售出 %.2f 元、上期 %.2f 元，属于在售商品", p.Amount, p.PrevAmount)
	case p.PrevAmount > 0:
		return fmt.Sprintf("；上期售出 %.2f 元，属于在售商品", p.PrevAmount)
	case p.Amount > 0:
		return fmt.Sprintf("；本期售出 %.2f 元", p.Amount)
	}
	return ""
}

// NamesOf 从带名字的切片里取前 n 个去重名字。
func NamesOf(n int, get func(i int) string, length int) []string {
	seen := map[string]bool{}
	out := make([]string, 0, n)
	for i := 0; i < length && len(out) < n; i++ {
		s := strings.TrimSpace(get(i))
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
