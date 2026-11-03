// Package service / cut_margin_dayend.go —— 日终剩余、售罄率、**参考口径毛利**(2026-11-01 owner 终稿)。
//
// # 这几个数分别是什么
//
//	日终剩余(kg) = **猪肉台日终盘点**的剩余量(柜台实盘,**不含冷库**)
//	日终剩余(元) = 日终剩余(kg) × **采购均价**
//	售罄率       = 销售 ÷ (销售 + 日终剩余)
//	参考成本单价_c = **建议成本单价**(分割基线的部位单价 × 今天这头猪的进价折算)
//	参考毛利_c   = 营收_c − 成本单价_c×销量_c − 搭赠成本_c − 报损成本_c
//	全店参考毛利 = **Σ 部位参考毛利**(头与部位同源)
//	全店参考毛利率 = 全店参考毛利 ÷ 营收
//
// ⚠️ **参考口径(B,2026-11-01 owner 拍板)不再依赖日终实盘**
//
//	旧口径(A)的 m = (营收 − 采购 + 期末剩余成本) / 营收 要**日终盘点**,
//	而部位成本单价 = 零售均价_c ÷ (1 + m) ⇒ 没盘点就连锁算不出来。
//	⇒ 现在部位单价走**建议成本单价** ⇒ 没做日终盘点也有数。
//	⚠️ 下面「为什么日终剩余的金额用采购均价」那段说的是**日终剩余估值**,
//	  仍是采购均价(无环)—— 与参考毛利口径无关。
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
//	它的定义就是「用建议成本单价再倒推一遍毛利」——而单价列已经给了。
//	⚠️ 口径切 B 之后,「建议毛利」与「参考毛利」**其实是同一个数**
//	(参考毛利用的就是建议成本单价)⇒ 加上去就是**同一列显示两遍**
//	⇒ 保持只有**参考毛利**一个口径(见 applyReferenceMargin)。
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
// # 口径(B 口径,owner 2026-11-01 拍板 + 2026-11-03 补外采)
//
//	成本单价_c = **建议成本单价**(分割基线 × 今天这头猪的进价折算)
//	            **与外采价按量加权**(owner 2026-11-03:「求均值」)
//	参考毛利_c = 营收_c − 成本单价_c×(销量_c + 搭赠_c + 报损_c)
//	全店参考毛利 = **Σ 部位参考毛利** ⇒ 头与部位同源。
//
// ⚠️ **不再需要日终实盘**(旧口径要它)⇒ 门店没做日终盘点也有数。
//
// ⚠️ 兜底:某部位没有基线价 ⇒ 用**进猪采购均价**;
//	连采购均价都没有(当天没进货)⇒ 该部位算不出来(「—」),
//	此时全店也给不出来(Σ 里缺一块 ⇒ 会把毛利算高)⇒ 全店给 nil。
//
// ⚠️ **绝不回落成 0**(0 会被门店误读成"当天不赚钱")。
func applyReferenceMargin(mids []cutRollupMid, tot *TotalGrossMargin, ref ReferenceDayTotals) {
	// ---- 全店 ----
	tot.ReferenceMarginRate = nil
	tot.ReferenceGrossMarginYuan = nil
	tot.ReferenceGrossMarginPct = nil
	tot.ReferenceCostPartial = false
	tot.PurchasedKg = ref.PurchasedKg
	tot.PurchasedYuan = ref.PurchasedYuan
	tot.PurchaseAvgYuan = ref.PurchaseAvgYuan
	// 外采(2026-11-03 owner):单列一栏,不动进猪口径。
	tot.ExternalPurchaseKg = ref.ExternalKg
	tot.ExternalPurchaseYuan = ref.ExternalYuan
	tot.ReferenceRemainKg = ref.RemainKg
	// ⚠️ 期末剩余的估值价用**含外采**的采购均价(owner 2026-11-03):
	//   剩下的货里有外采那部分 ⇒ 它的成本就该按含外采的均价估。
	tot.ReferenceRemainYuan = ref.RemainKg.Mul(ref.TotalPurchaseAvgYuan())

	// ---- 部位 ----
	partial := false
	var sumMargin decimal.Decimal
	for i := range mids {
		mid := &mids[i]
		soldAll := mid.soldKg.Add(mid.bulkKg)
		ext := ref.ExternalByCut[string(mid.src.CutType)]
		unit, source := referenceCutUnitPrice(mid.src.SuggestedCostUnitPriceYuan,
			ref.PurchaseAvgYuan, ref.TotalPurchaseAvgYuan(), pigSplitKgOf(&mid.src), ext)
		if source == referencePriceSourceNone {
			partial = true
			mid.row.ReferenceCostUnitPriceYuan = nil
			mid.row.ReferenceCostPriceSource = referencePriceSourceNone
			mid.row.ReferenceGrossMarginYuan = nil
			mid.row.ReferenceGrossMarginPct = nil
			continue
		}
		rc := referencePartCostOf(unit, source, mid.revenue, soldAll,
			mid.src.GiftKg, mid.src.WasteKg)
		price := rc.UnitPriceYuan
		gmC := rc.GrossMarginYuan
		pctC := rc.GrossMarginPct
		mid.row.ReferenceCostUnitPriceYuan = &price
		mid.row.ReferenceCostPriceSource = rc.UnitPriceSource
		mid.row.ReferenceGrossMarginYuan = &gmC
		mid.row.ReferenceGrossMarginPct = &pctC
		sumMargin = sumMargin.Add(gmC)
	}
	tot.ReferenceCostPartial = partial

	// ---- 全店 = 部位之和 ----
	//
	// ⚠️ **有部位算不出来 ⇒ 全店也给不出来**:
	//	Σ 缺了那块成本 ⇒ 毛利会偏高 ⇒ 与其给一个偏高的数,不如给「—」。
	if partial || !tot.RevenueYuan.IsPositive() {
		return
	}
	amount := sumMargin
	pct, _ := sumMargin.Mul(decimal.NewFromInt(100)).Div(tot.RevenueYuan).Float64()
	rate := sumMargin.Div(tot.RevenueYuan)
	tot.ReferenceGrossMarginYuan = &amount
	tot.ReferenceGrossMarginPct = &pct
	tot.ReferenceMarginRate = &rate
}
