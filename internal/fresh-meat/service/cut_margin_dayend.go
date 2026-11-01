// Package service / cut_margin_dayend.go —— 日终剩余、售罄率、**参考口径毛利**(2026-11-01 owner 终稿)。
//
// # 这几个数分别是什么
//
//	日终剩余(kg) = **猪肉台日终盘点**的剩余量(柜台实盘,**不含冷库**)
//	日终剩余(元) = 日终剩余(kg) × **采购均价**
//	售罄率       = 销售 ÷ (销售 + 日终剩余)
//	参考毛利率 m = (营收 − 采购 + 期末剩余成本) / 营收
//	参考毛利_c   = 营收_c − 成本单价_c×销量_c − 搭赠成本_c − 报损成本_c
//	              成本单价_c = 零售均价_c ÷ (1 + m)
//
// # 为什么日终剩余的金额改用**采购均价**(2026-11-01 owner)
//
//	旧口径是「零售均价 ÷ (1 + 今日毛利率)」倒推 ⇒ 那个价里**含 m**
//	⇒ 拿它去估值期末库存 ⇒ m 的输入里又出现了 m ⇒ **成环**。
//	而采购均价**不含 m** ⇒ 天然无环。
//	⚠️ 也正因为它无环,它才能当 m 的输入(owner:「可以用采购均价计算」)。
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
// # ⚠️「建议毛利 / 建议毛利率」已删除(2026-11-01 owner)
//
//	它的定义就是「用建议成本单价再倒推一遍毛利」——而单价列已经给了,
//	且数学上 m 进去、m′ 出来 ⇒ **永远对不上 header** ⇒ 纯属重复且误导。
//	⇒ 现在只有**参考毛利**一个口径(见 applyReferenceMargin)。
package service

import (
	"github.com/shopspring/decimal"
)

// applyDayEndRemain 回填**日终剩余**(kg/金额)与**售罄率**。
//
// ⚠️ 日终剩余 = **猪肉台实盘**:只有当天有**完整盘点**时才有值
//
//	⇒ 没有 ⇒ nil(前端显示「—」),**不是 0**。
//
// ⚠️ 金额 = 剩余 kg × **采购均价**(不含 m ⇒ 无环;见包注释)。
//
//	取不到采购均价(当天没进猪)⇒ 只给 kg,不给金额。
func applyDayEndRemain(mids []cutRollupMid, tot *TotalGrossMargin, purchaseAvg decimal.Decimal) {
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
		if purchaseAvg.IsPositive() {
			remainYuan := remainKg.Mul(purchaseAvg)
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

// applyReferenceMargin 回填**参考口径**的成本单价与毛利(部位 + 全店)。
//
// # 全店(头)的参考毛利 —— **不是**各部位之和
//
//	毛利 = 营收 − 当日采购总额 + 期末猪肉台剩余成本
//
// ⚠️ owner 2026-11-01:「header不需要与部位同口径」
//
//	⇒ 部位是"参考值"(产出靠基线估算),头是"总量口径"(总采/总售/总剩余)
//	⇒ **两者不相等是正常的**,不要为了对得上而把头的口径改成部位之和。
//
// # 部位
//
//	成本单价_c = 零售均价_c ÷ (1 + m)      (owner 亲定)
//	参考毛利_c = 营收_c − 成本单价_c×销量_c − 搭赠成本_c − 报损成本_c
//
// ⚠️ m 算不出来(没有完整日终盘点 / 当天没营收)⇒ 全部给 nil(前端「—」),
//
//	**绝不回落成 0**(0 会被门店误读成"当天不赚钱")。
func applyReferenceMargin(mids []cutRollupMid, tot *TotalGrossMargin, m decimal.Decimal,
	ref ReferenceDayTotals, marginOK bool) {
	// ---- 全店 ----
	tot.ReferenceMarginRate = nil
	tot.ReferenceGrossMarginYuan = nil
	tot.ReferenceGrossMarginPct = nil
	tot.ReferenceCostPartial = false
	tot.PurchasedKg = ref.PurchasedKg
	tot.PurchasedYuan = ref.PurchasedYuan
	tot.PurchaseAvgYuan = ref.PurchaseAvgYuan
	tot.ReferenceRemainKg = ref.RemainKg
	tot.ReferenceRemainYuan = ref.RemainKg.Mul(ref.PurchaseAvgYuan)

	if !marginOK {
		// ⚠️ m 算不出来 ⇒ 部位也给不出(成本单价依赖 m)。
		for i := range mids {
			mids[i].row.ReferenceCostUnitPriceYuan = nil
			mids[i].row.ReferenceCostPriceSource = referencePriceSourceNone
			mids[i].row.ReferenceGrossMarginYuan = nil
			mids[i].row.ReferenceGrossMarginPct = nil
		}
		return
	}

	rate := m
	tot.ReferenceMarginRate = &rate
	// 全店毛利 = 营收 − 采购 + 期末剩余成本。
	gm := tot.RevenueYuan.Sub(ref.PurchasedYuan).Add(tot.ReferenceRemainYuan)
	amount := gm
	pct, _ := gm.Mul(decimal.NewFromInt(100)).Div(tot.RevenueYuan).Float64()
	tot.ReferenceGrossMarginYuan = &amount
	tot.ReferenceGrossMarginPct = &pct

	// ---- 部位 ----
	partial := false
	for i := range mids {
		mid := &mids[i]
		soldAll := mid.soldKg.Add(mid.bulkKg)
		rc := referencePartCostOf(m, ref.PurchaseAvgYuan, mid.revenue, soldAll,
			mid.src.GiftKg, mid.src.WasteKg)
		if !rc.Priceable {
			partial = true
			mid.row.ReferenceCostUnitPriceYuan = nil
			mid.row.ReferenceCostPriceSource = referencePriceSourceNone
			mid.row.ReferenceGrossMarginYuan = nil
			mid.row.ReferenceGrossMarginPct = nil
			continue
		}
		unit := rc.UnitPriceYuan
		gmC := rc.GrossMarginYuan
		pctC := rc.GrossMarginPct
		mid.row.ReferenceCostUnitPriceYuan = &unit
		mid.row.ReferenceCostPriceSource = rc.UnitPriceSource
		mid.row.ReferenceGrossMarginYuan = &gmC
		mid.row.ReferenceGrossMarginPct = &pctC
	}
	tot.ReferenceCostPartial = partial
}
