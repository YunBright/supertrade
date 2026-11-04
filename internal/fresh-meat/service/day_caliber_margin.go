package service

// internal/fresh-meat/service/day_caliber_margin.go —— **当日口径**的全店毛利
// (毛利看板**表头**与**长期毛利页的每日行**共用同一个公式)。
//
// # 口径(owner 2026-11-03 拍板)
//
//	成本 = 当日总支出(进猪 + 外采) + **搬出估算成本** − **盘点估算成本**
//	毛利 = 当日总收入 − 成本
//	毛利率 = 毛利 ÷ 当日总收入
//
// # 为什么**两个页面共用这一个函数**(2026-11-04 owner:「长期毛利页的也切」)
//
//	长期毛利页的**每日行**原来用的是「**猪肉台口径**」
//	(营收 − 当日采购 + 期末台面剩余×采购均价)
//	⇒ 与看板表头**只差「搬出估算成本」一项**
//	  (生产实测 店 00:10-29 差 1.58 元、11-01 差 114.84 元)
//	⇒ owner 拍板:长期毛利页的每日行**也切当日口径**
//	⇒ 两个页面的每日数**必须永远是同一个数**
//	⇒ 所以公式**只写一份**,两个页面都调它 ⇒ **不可能漂移**。
//
// ⚠️ **部位行不在这个口径里**:看板的部位参考毛利是
//   「销量 × 部位成本单价」估的 ⇒ 与本口径(今天的现金账)**不是同一个数**
//   ⇒ 门店**不要**拿部位行加总去核表头(owner 明确要求)。
//
// # 三个守卫(算不出来就是算不出来 ⇒ ok=false ⇒ 调用方给「—」)
//
//	① 当天没进货(进猪 + 外采)⇒ 成本算不出来(**不是 0 成本**)
//	② 没有完整日终盘点 ⇒ 「盘点估算成本」算不出来
//	③ 当天没营收 ⇒ 比率没有意义
//
// ⚠️ **绝不回落成 0**(0 会被门店误读成"当天不赚钱")。

import "github.com/shopspring/decimal"

// dayCaliberMargin 当日口径的全店毛利(元)与毛利率(%)。
//
// # loadCost = **搬出估算成本**
//
//	= Σ(搬上台 kg × 该部位**冷库移动加权成本**)
//
//	⚠️ 为什么用冷库移动加权成本:搬上台的是**前几天切好的老货**
//	  ⇒ 它的成本是当初进货的成本,不是今天的部位建议单价。
//
// # 返回
//
//	(gm, pct, ok) —— ok=false ⇒ 算不出来(调用方给「—」,**不是 0**)。
func dayCaliberMargin(ref ReferenceDayTotals, revenue, loadCost decimal.Decimal) (
	gm decimal.Decimal, pct float64, ok bool) {
	// 守卫①:当天没进货 ⇒ 没有采购均价 ⇒ 盘点估算成本算不出来。
	if !ref.TotalPurchasedKg().IsPositive() {
		return decimal.Zero, 0, false
	}
	// 守卫②:没有完整日终盘点 ⇒ 台面实盘拿不到 ⇒ 盘点估算成本算不出来。
	if !ref.HasRemain {
		return decimal.Zero, 0, false
	}
	// 守卫③:当天没营收 ⇒ 毛利率没有意义(毛利率没有分母)。
	if !revenue.IsPositive() {
		return decimal.Zero, 0, false
	}
	// 盘点估算成本 = **期末猪肉台实盘** kg × 含外采采购均价(= 参考剩余估值)。
	remainYuan := ref.RemainKg.Mul(ref.TotalPurchaseAvgYuan())
	cost := ref.TotalPurchasedYuan().Add(loadCost).Sub(remainYuan)
	amount := revenue.Sub(cost)
	p, _ := amount.Mul(decimal.NewFromInt(100)).Div(revenue).Float64()
	return amount, p, true
}
