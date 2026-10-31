// Package service / cut_margin_dayend.go —— 日终剩余、售罄率、建议毛利(2026-10-31)。
//
// # 这三个数分别是什么
//
//	日终剩余(kg) = **猪肉台日终盘点**的剩余量(柜台实盘,**不含冷库**)
//	日终剩余(元) = 日终剩余(kg) × 折算进货单价
//	               折算进货单价 = **零售均价 ÷ (1 + 今日毛利率)**(owner 亲定)
//	售罄率       = 销售 ÷ (销售 + 日终剩余)
//	建议毛利     = 营收 − 建议成本单价×零售销量 − 统收成本 − 报损成本 − 搭赠成本 + 预期毛利
//
// # 为什么售罄率不再用 sold/purchased
//
// 生产实测(店 00 2026-10-30):旧口径 = **100.75%** ⇒ >100%。
// 根因:**销售**里含**冷库**卖出去的货,而**当天进货**里没有那部分
// ⇒ 分子含了分母之外的货 ⇒ 比率可以 >1。
// ⇒ 新口径把**日终盘点**放进分母 ⇒ 结果恒 ≤ 100%。
//
// ⚠️ 没有完整日终盘点 ⇒ 售罄率 = **nil**(前端显示「—」)
//
//	⇒ **绝不回落成 100%**(那等于告诉门店"全卖完了",而其实不知道)。
//
// # 为什么"折算进货单价"和单价基线用同一条公式
//
// 单价日样本的倒推(见 forecast_price.go)也是「零售均价 ÷ (1 + 毛利率)」
// ⇒ 同一套口径 ⇒ 「日终剩余估值」与「单价基线」不会各算各的。
package service

import (
	"github.com/shopspring/decimal"
)

// foldDownCostPrice 按 owner 口径倒推**进货单价**(元/kg)= 零售均价 ÷ (1 + 毛利率)。
//
// ⚠️ 毛利率 ≤ −95%(亏到毛利率比 −1 还狠)⇒ 1+毛利率 ≤ 0.05
//
//	⇒ 倒推出来的单价会翻十几倍 ⇒ **不用这个价**(宁可退回全店兜底价)。
//
// ⚠️ 均价必须 > 0 ⇒ 没卖过的部位没有自己的均价 ⇒ 由调用方走兜底。
func foldDownCostPrice(avgPrice, marginRate decimal.Decimal) (decimal.Decimal, bool) {
	if !avgPrice.IsPositive() {
		return decimal.Zero, false
	}
	divisor := decimal.NewFromInt(1).Add(marginRate)
	if divisor.LessThanOrEqual(decimal.NewFromFloat(0.05)) {
		return decimal.Zero, false
	}
	return avgPrice.Div(divisor), true
}

// storeFoldPrice 全店折算进货单价(兜底价)。
//
// ⚠️ 全店没有销量(整天没卖)⇒ 算不出来 ⇒ ok=false。
func storeFoldPrice(mids []cutRollupMid) (decimal.Decimal, bool) {
	var revenue, soldKg, gm, gmBase decimal.Decimal
	for _, m := range mids {
		revenue = revenue.Add(m.revenue)
		soldKg = soldKg.Add(m.soldKg).Add(m.bulkKg)
		gm = gm.Add(m.gm)
		gmBase = gmBase.Add(m.gmBase)
	}
	if !soldKg.IsPositive() || gmBase.IsZero() {
		return decimal.Zero, false
	}
	return foldDownCostPrice(revenue.Div(soldKg), gm.Div(gmBase))
}

// cutFoldPrice 该部位的折算进货单价(元/kg)。
//
// 口径:① 该部位自己的「零售均价 ÷ (1 + 该部位毛利率)」
//
//	② 兜底:全店折算进货单价
//
// ⚠️ **该部位没卖过** ⇒ 没有自己的均价 ⇒ 直接用全店兜底价。
func cutFoldPrice(m cutRollupMid, storePrice decimal.Decimal, storeOK bool) (decimal.Decimal, bool) {
	soldKg := m.soldKg.Add(m.bulkKg)
	if soldKg.IsPositive() && !m.gmBase.IsZero() {
		avg := m.revenue.Div(soldKg)
		margin := m.gm.Div(m.gmBase)
		if price, ok := foldDownCostPrice(avg, margin); ok {
			return price, true
		}
	}
	if storeOK {
		return storePrice, true
	}
	return decimal.Zero, false
}

// applyDayEndRemain 回填**日终剩余**(kg/金额)与**售罄率**。
//
// ⚠️ 日终剩余 = **猪肉台实盘**:只有当天有**完整盘点**时才有值
//
//	⇒ 没有 ⇒ nil(前端显示「—」),**不是 0**。
//
// ⚠️ 金额 = 剩余 kg × 折算进货单价;取不到折算价 ⇒ 只给 kg,不给金额。
func applyDayEndRemain(mids []cutRollupMid, tot *TotalGrossMargin) {
	storePrice, storeOK := storeFoldPrice(mids)
	var totalRemainKg, totalRemainYuan, totalSoldKg decimal.Decimal
	hasStocktake := false
	for i := range mids {
		m := &mids[i]
		soldAll := m.soldKg.Add(m.bulkKg)
		totalSoldKg = totalSoldKg.Add(soldAll)
		if m.src.DataSource != dataSourceActual {
			continue
		}
		hasStocktake = true
		remainKg := m.src.CounterActualKg
		m.row.DayEndRemainKg = &remainKg
		totalRemainKg = totalRemainKg.Add(remainKg)
		if price, ok := cutFoldPrice(*m, storePrice, storeOK); ok {
			remainYuan := remainKg.Mul(price)
			m.row.DayEndRemainYuan = &remainYuan
			totalRemainYuan = totalRemainYuan.Add(remainYuan)
		}
		// 售罄率 = 销售 ÷ (销售 + 日终剩余)⇒ 恒 ≤ 100%。
		denom := soldAll.Add(remainKg)
		if denom.IsPositive() {
			f, _ := soldAll.Div(denom).Float64()
			m.row.SellThroughRate = &f
		}
	}
	tot.DayEndRemainKg = nil
	tot.DayEndRemainYuan = nil
	// 全店售罄率:分母里的日终剩余必须是**实盘**才有意义
	// ⇒ 没有完整盘点 ⇒ 不给(前端「—」),而不是给 100%。
	if hasStocktake {
		remainKg := totalRemainKg
		remainYuan := totalRemainYuan
		tot.DayEndRemainKg = &remainKg
		tot.DayEndRemainYuan = &remainYuan
		denom := totalSoldKg.Add(totalRemainKg)
		if denom.IsPositive() {
			f, _ := totalSoldKg.Div(denom).Float64()
			tot.SellThroughRate = &f
		}
	}
}

// applySuggestedMargin 回填**建议毛利 / 建议毛利率**。
//
// ⚠️ 建议毛利与实际毛利**逐项平行**,只差「零售货的成本口径」:
//
//	建议毛利 = 营收 − 建议成本单价×零售销量 − 统收成本 − 报损成本 − 搭赠成本 + 预期毛利
//
// ⚠️ 取不到建议成本单价 ⇒ 该部位的建议毛利**算不出来** ⇒ nil(前端显示「—」)
//
//	⇒ **不用实际销售成本冒充**(那会让「建议」和「实际」长得一样 ⇒ 这一列白做)。
//
// ⚠️ **没卖过**的部位不需要单价(零售成本恒 0)⇒ 照样算得出来。
func applySuggestedMargin(mids []cutRollupMid, tot *TotalGrossMargin) {
	var sum decimal.Decimal
	counted := 0
	partial := false
	for i := range mids {
		m := &mids[i]
		var suggestedSalesCost decimal.Decimal
		switch {
		case m.src.SuggestedCostUnitPriceYuan != nil:
			suggestedSalesCost = m.src.SuggestedCostUnitPriceYuan.Mul(m.soldKg)
		case m.soldKg.IsZero():
			suggestedSalesCost = decimal.Zero
		default:
			// 卖过、但这个部位**取不到价** ⇒ 建议毛利算不出来。
			partial = true
			continue
		}
		gm := m.revenue.Sub(suggestedSalesCost).Sub(m.bulkCost).Sub(m.wasteCost).
			Sub(m.giftCost).Add(m.presaleMargin)
		m.row.SuggestedGrossMarginYuan = &gm
		if !m.gmBase.IsZero() {
			f, _ := gm.Mul(decimal.NewFromInt(100)).Div(m.gmBase).Float64()
			m.row.SuggestedGrossMarginPct = &f
		}
		sum = sum.Add(gm)
		counted++
	}
	// ⚠️ **先**记 partial:一个部位都算不出来时也要让前端知道"缺价",
	//   否则前端会以为"没有建议毛利"就是"这天毛利是 0"。
	tot.SuggestedCostPartial = partial
	if counted == 0 {
		// 一个部位都算不出来 ⇒ 全店也给不出 ⇒ 前端显示「—」。
		return
	}
	tot.SuggestedGrossMarginYuan = &sum
	base := sumMid(mids, func(m cutRollupMid) decimal.Decimal { return m.gmBase })
	if !base.IsZero() {
		f, _ := sum.Mul(decimal.NewFromInt(100)).Div(base).Float64()
		tot.SuggestedGrossMarginPct = &f
	}
}
