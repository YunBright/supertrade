// Package service / promo_alloc.go —— 特价销售的**日终缺口分摊**。
//
// # 问题
//
// 特价档是"价格槽位",装的是当天滞销的那个部位 ⇒ POS 数据里**没有**"这斤是五花还是里脊"。
// 所以特价销售落进 `promo` 桶:金额与重量都进来了,但**不摊到具体部位**。
//
// # 分摊依据:账面缺口
//
// 对每个部位 c:
//
//	gap_c = 期初_c + 入库_c − 正常销量_c − 统收_c − 报损_c − 期末实盘_c
//
// `gap_c > 0` = 这个部位**"少了这么多 kg 没被正常卖掉"** ⇒ 它就是特价(或有未记损耗)的来源。
// 多个部位同时有缺口时,**按缺口大小分配**(缺口就是直接证据)。
//
// 例:当日两头猪应分割出后腿肉 30 斤,正常卖出 20 斤,日终盘点剩 2 斤
// ⇒ gap_后腿 = 30 − 20 − 2 = 8 ⇒ 这 8 斤就是后腿被特价处理掉的。
//
// # 总量用实测、比例用缺口
//
// 分摊**总量**取实测值(特价桶实际卖了多少 kg / 多少钱),
// 缺口只用来定**比例**。
// ⇒ 缺口里混着未记损耗/切分损耗/称重误差 ⇒ 拿缺口当总量会把损耗算成特价收入。
//
// # 不落库(纯读路径)
//
// 分摊结果**不写回** `line_sales_by_pigs` —— 写回去下一轮分摊就会把它当输入,
// 形成自反馈。分摊只在毛利报告里现算。
package service

import (
	"sort"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
)

// PromoWeight 一个部位在特价分摊里的权重与证据。
type PromoWeight struct {
	CutType model.CutType   `json:"cut_type"`
	GapKg   decimal.Decimal `json:"gap_kg"`
	RemainKg decimal.Decimal `json:"remain_kg"`
	Weight  float64         `json:"weight"`
}

// PromoAllocatedCut 一个部位分到的特价 kg 与金额。
type PromoAllocatedCut struct {
	CutType     model.CutType   `json:"cut_type"`
	PromoKg     decimal.Decimal `json:"promo_kg"`
	PromoAmount decimal.Decimal `json:"promo_amount"`
}

// PromoAllocation 特价分摊结果(进毛利报告的 `promo_allocation`)。
type PromoAllocation struct {
	// PromoKgTotal / PromoAmountTotal 是**实测**的特价总量。
	PromoKgTotal     decimal.Decimal `json:"promo_kg_total"`
	PromoAmountTotal decimal.Decimal `json:"promo_amount_total"`
	// GapKgTotal 是各部位缺口之和(证据总量)。
	GapKgTotal decimal.Decimal `json:"gap_kg_total"`
	// UnexplainedKg = 实测特价 kg − 缺口之和。
	//   > 0 ⇒ 有一块特价 kg 没有对应的缺口证据(或有未记损耗抵消了缺口)
	//   < 0 ⇒ 缺口比特价多(有未记损耗)
	UnexplainedKg decimal.Decimal `json:"unexplained_kg"`
	// Weights 是各部位的权重与证据(按权重降序)。
	Weights []PromoWeight `json:"weights"`
	// Allocated 是分摊结果(按部位名升序)。
	Allocated []PromoAllocatedCut `json:"allocated"`
	// Basis 分摊依据标签。
	Basis string `json:"basis"`
	// IsEstimated = true ⇒ **没有分摊**(证据不足),特价整体留在 promo 桶。
	IsEstimated bool `json:"is_estimated"`
	Note        string `json:"note,omitempty"`
}

// allocatePromo 按账面缺口把特价分摊到各部位。
//
// rows 是当日的 SKU 级明细(已含 promo 桶的行)。
func allocatePromo(rows []SkuDailyCost) *PromoAllocation {
	res := &PromoAllocation{Basis: "gap_weighted"}

	// 1) 特价桶的实测总量(按 promo SKU 分行,保留各档不同价)
	type promoLine struct{ kg, amount decimal.Decimal }
	var lines []promoLine
	for _, r := range rows {
		if r.CutType != model.CutPromo {
			continue
		}
		if !r.SoldKg.IsPositive() && !r.SoldAmount.IsPositive() {
			continue
		}
		lines = append(lines, promoLine{kg: r.SoldKg, amount: r.SoldAmount})
		res.PromoKgTotal = res.PromoKgTotal.Add(r.SoldKg)
		res.PromoAmountTotal = res.PromoAmountTotal.Add(r.SoldAmount)
	}
	if len(lines) == 0 {
		res.Note = "当日无特价销售。"
		return res
	}

	// 2) 各部位缺口
	type gapItem struct {
		cut    model.CutType
		gap    decimal.Decimal
		remain decimal.Decimal
	}
	var items []gapItem
	sumGap := decimal.Zero
	for _, r := range rows {
		if r.CutType == model.CutPromo {
			continue
		}
		gap := r.OpeningKg.Add(r.ProducedKg).
			Sub(r.SoldKg).Sub(r.BulkSoldKg).Sub(r.WasteKg).Sub(r.ClosingKg)
		if !gap.IsPositive() {
			continue
		}
		items = append(items, gapItem{cut: r.CutType, gap: gap, remain: r.ClosingKg})
		sumGap = sumGap.Add(gap)
	}
	res.GapKgTotal = sumGap
	res.UnexplainedKg = res.PromoKgTotal.Sub(sumGap)

	if len(items) == 0 || !sumGap.IsPositive() {
		res.IsEstimated = true
		res.Note = "各部位账面都平(无缺口证据),特价未分摊到部位 —— 整体留在 promo 桶。"
		return res
	}

	// 3) 按权重分摊(逐行,保留各档不同价 ⇒ 不会按均价摊平)
	allocKg := map[model.CutType]decimal.Decimal{}
	allocAmt := map[model.CutType]decimal.Decimal{}
	for _, l := range lines {
		for _, it := range items {
			w := it.gap.Div(sumGap)
			allocKg[it.cut] = allocKg[it.cut].Add(l.kg.Mul(w))
			allocAmt[it.cut] = allocAmt[it.cut].Add(l.amount.Mul(w))
		}
	}

	// 4) 输出(权重降序,分配结果按部位名升序)
	sort.Slice(items, func(i, j int) bool {
		if !items[i].gap.Equal(items[j].gap) {
			return items[i].gap.GreaterThan(items[j].gap)
		}
		return items[i].cut < items[j].cut
	})
	for _, it := range items {
		w, _ := it.gap.Div(sumGap).Float64()
		res.Weights = append(res.Weights, PromoWeight{
			CutType: it.cut, GapKg: it.gap, RemainKg: it.remain, Weight: w,
		})
	}
	allocCuts := make([]PromoAllocatedCut, 0, len(items))
	for _, it := range items {
		allocCuts = append(allocCuts, PromoAllocatedCut{
			CutType: it.cut, PromoKg: allocKg[it.cut], PromoAmount: allocAmt[it.cut],
		})
	}
	sort.Slice(allocCuts, func(i, j int) bool { return allocCuts[i].CutType < allocCuts[j].CutType })
	res.Allocated = allocCuts
	res.Note = "按当日「期初+入库−正常销量−统收−报损−期末实盘」的账面缺口加权分摊;" +
		"总量取实测特价销量,缺口只用于定比例(缺口里混着未记损耗,不能当总量)。"
	return res
}

// promoAllocByCut 把分摊结果索引成 部位 → 分配。
func promoAllocByCut(p *PromoAllocation) map[model.CutType]PromoAllocatedCut {
	out := map[model.CutType]PromoAllocatedCut{}
	if p == nil {
		return out
	}
	for _, a := range p.Allocated {
		out[a.CutType] = a
	}
	return out
}
