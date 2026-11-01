// Package service / cost_reference.go —— 参考口径的成本与毛利(2026-11-01 owner 口径切换)。
//
// # 口径(B 口径,owner 2026-11-01 拍板)
//
//	部位成本单价_c = **建议成本单价**(分割基线的部位单价 × 今天这头猪的进价折算)
//	参考毛利_c     = 营收_c − 成本单价_c×(销量_c + 搭赠_c + 报损_c)
//	全店参考毛利   = **Σ 部位参考毛利**(头与部位同源)
//	全店参考毛利率 = 全店参考毛利 ÷ 营收
//
// # 为什么换口径(owner 原话)
//
//	「在录入猪的时候不是应用了 auto 基线计算吗？不是会有 7.3 元/斤猪价的成本单价？
//	  然后根据分割基线，销售重量×建议成本单价不就有了销售成本，
//	  销售重量×销售单价−销售成本，不就有当前毛利吗？」
//
// ⇒ 旧口径(A)的部位成本单价 = 零售均价 ÷ (1 + m),而 m = (营收 − 采购 + 期末台面剩余×采购均价) ÷ 营收
//   **依赖日终实盘** ⇒ 门店当天没做日终盘点 ⇒ m / 成本单价 / 参考毛利 / 毛利率 / 售罄率
//   **连锁算不出来** ⇒ 界面一片「—」⇒ 明明有猪价、有分割基线,却拿不到当天的毛利。
// ⇒ 新口径的成本单价用**录猪时就算好的建议成本单价** ⇒ **不需要盘点**。
//
// # ⚠️ B 口径的取舍(owner 知情选定)
//
//	① **依赖统计基线**:建议成本单价是「分割基线 × 今天进价」折算出来的统计量,
//	   不是这批货的实际成本(今天卖掉的货可能还是前几天进的猪)。
//	② **偏差方向与旧口径相反**:旧口径把"今天进货但没卖掉"算进今天(单日毛利偏高);
//	   新口径只给"今天卖掉的货"计成本(猪价涨了当天毛利偏低)⇒ **两者只能选一个**。
//	③ **当天没进货 ⇒ 算不出来**:建议成本单价要靠当天的猪做缩放 ⇒ 「—」(不回落成 0)。
//
// # ⚠️ 单价基线(样本采集)仍走**旧口径**
//
//	单价基线 = 历史样本倒推(进价 = 零售均价 ÷ (1+m))⇒ 这里的 m 用的是**旧口径**
//	(依赖日终实盘,见下面的 referenceMarginRateOfDay)。
//	⇒ 这是**有意保留**:基线是历史统计量,换口径不会改写历史。
//	⇒ 要不要连"样本采集 + 基线"一起换口径,需要连同"重算样本 + 重算基线"另案决定。
//
// # ⚠️ 参照口径 ≠ 「账面口径」:本文件算的是**参考**值,
// 账面值(移动加权链)仍留在 cut_cost.go,只服务对账/审计,并在 P1 退休。
package service

import (
	"context"
	"fmt"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
)

// ReferenceDayTotals 参考口径的**当日总量输入**。
//
// ⚠️ 全部来自**事实**:猪(whole_pigs)、销售(cube)、盘点单(猪肉台实盘)。
// ⚠️ 没有任何一项来自分割建议 ⇒ 切割不准也不影响这里。
type ReferenceDayTotals struct {
	// PurchasedKg / PurchasedYuan 是当日进店的猪的合计。
	PurchasedKg   decimal.Decimal
	PurchasedYuan decimal.Decimal
	// PurchaseAvgYuan 是**采购均价**(元/kg)= 采购金额 ÷ 采购重量。
	//
	// ⚠️ 它是**剩余成本**的估值价(owner:「可以用采购均价计算」)——
	//   也是**部位成本单价取不到零售均价时的兜底价**。
	PurchaseAvgYuan decimal.Decimal
	// RemainKg 是**期末猪肉台实盘**(kg)—— **不含冷库**。
	RemainKg decimal.Decimal
	// HasRemain = false ⇒ 当天没有完整日终盘点 ⇒ 剩余算不出来。
	//
	// ⚠️ 这时**不能**给毛利(前端显示「—」),也不能给 0
	//   (0 会被门店误读成"当天一点货都没剩、还亏了本")。
	HasRemain bool
}

// referenceMarginRate 算当日**总毛利率** m(**旧口径 A**)。
//
// ⚠️ **只给「单价样本采集」用**(见 referenceMarginRateOfDay)——
//
//	看板上的参考毛利已改成 B 口径(成本单价 = 建议成本单价),不再用 m。
//	⇒ 这里保留 A 口径,是因为**单价基线**是历史统计量:
//	  它的样本是用「进价 = 零售均价 ÷ (1+m)」倒推的,m 用旧口径 ⇒ 基线不动。
//	⇒ 要不要连基线一起换口径,是另一个决定(要连同重算样本 + 重算基线)。
//
// # 口径
//
//	m = (营收 − 采购 + 期末剩余成本 − 期初剩余成本) / 营收
//
// 期初剩余成本 = 0(夜里柜台清空)⇒ 公式退化成
//
//	m = (营收 − 采购 + 期末台面剩余 × 采购均价) / 营收
//
// ⚠️ **剩余 → 0 时 m = (营收 − 采购)/营收** ⇒ 精确(owner 的收敛判据)。
//
// ⚠️ 返回 ok=false 的三种情况(都**不能**给 0):
//   - 当天**没有进货** ⇒ 参考口径整体失效(见下);
//   - 当天没有完整日终盘点 ⇒ 剩余算不出来;
//   - 当天没有营收 ⇒ 毛利率没有意义。
//
// # ⚠️ 为什么"没有进货"也算不出来(2026-11-01 补)
//
//	没有进货 ⇒ ① 没有采购均价 ⇒ 期末剩余的**成本估不出来**;
//	② 当天的成本**根本没发生**(卖的是前几天进冷库的货)⇒ 公式退化成
//
//		m = (营收 − 0 + 剩余×0) / 营收 = **100%**
//
//	⇒ 门店会看到"参考毛利 = 营收、毛利率 100%" ⇒ 以为赚疯了。
//	⇒ 而当天其实只是**还没录猪**(毛利看板本来就有「今天还没录猪」提示)。
//	⇒ 这种日子只能显示「—」(算不出来),不能给一个假的 100%。
func referenceMarginRate(tot ReferenceDayTotals, revenue decimal.Decimal) (decimal.Decimal, bool) {
	if !tot.PurchasedKg.IsPositive() {
		return decimal.Zero, false
	}
	if !tot.HasRemain || !revenue.IsPositive() {
		return decimal.Zero, false
	}
	remainYuan := tot.RemainKg.Mul(tot.PurchaseAvgYuan)
	gm := revenue.Sub(tot.PurchasedYuan).Add(remainYuan)
	return gm.Div(revenue), true
}

// referenceDayTotalsOf 采集当日的**总采**与**总剩余**。
//
// # 总采
//
//	当日营业日窗口内进店的 whole_pigs(重量 + 采购金额)。
//
// ⚠️ 与 computeCutDailyCostsChained 用**同一个窗口**(bizTZ 的营业日),
//
//	否则两处的"今日进货"会差一天。
//
// # 总剩余
//
//	**猪肉台日终实盘**(盘点单)—— 不含冷库。
//
// ⚠️ 判据用"当天有没有**完整**盘点单":半盘/草稿不算实盘
//
//	⇒ 没有完整盘点 ⇒ HasRemain=false ⇒ 毛利算不出来(前端「—」)。
func (s *Service) referenceDayTotalsOf(ctx context.Context, branchID string, day time.Time,
	rows []CutDailyCost) (ReferenceDayTotals, error) {
	var out ReferenceDayTotals
	startOfDay, endOfDay := s.businessDayBounds(day)

	var pigs []model.WholePig
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND arrived_at >= ? AND arrived_at < ?", branchID, startOfDay, endOfDay).
		Find(&pigs).Error; err != nil {
		return out, fmt.Errorf("query whole_pigs(reference): %w", err)
	}
	for _, p := range pigs {
		out.PurchasedKg = out.PurchasedKg.Add(p.GrossWeightKg)
		out.PurchasedYuan = out.PurchasedYuan.Add(p.PurchaseCostYuan)
	}
	if out.PurchasedKg.IsPositive() {
		out.PurchaseAvgYuan = out.PurchasedYuan.Div(out.PurchasedKg)
	}

	// 期末猪肉台实盘:只认**完整盘点单**的那个数据源标记。
	for _, r := range rows {
		if r.DataSource != dataSourceActual {
			continue
		}
		out.RemainKg = out.RemainKg.Add(r.CounterActualKg)
		out.HasRemain = true
	}
	return out, nil
}

// referenceCutUnitPriceFromBaseline 取某个部位当天的**参考成本单价**(元/kg)。
//
// # 口径(B 口径,owner 2026-11-01 拍板)
//
//	成本单价_c = **建议成本单价**(基线单价 × 今天这头猪的进价折算)
//
// ⇒ **不需要日终实盘** ⇒ 没做盘点的日子照样有毛利。
//
// # 兜底链
//
//	建议成本单价 → 全店采购均价 → 取不到(= none)
//
// ⚠️ 为什么要有兜底:某个部位可能**没有自己的基线价**
//
//	(部位单价基线覆盖不到、或退取链只走到了整店均价)⇒ 它也要计成本
//	(搭赠/报损这类"有量、无销售"的部位也要计成本)⇒ 单价不能是空。
//
// ⚠️ 返回 source 让调用方能说清"这个单价是基线折算的还是兜底的"。
func referenceCutUnitPriceFromBaseline(suggested *decimal.Decimal, purchaseAvg decimal.Decimal) (
	decimal.Decimal, string,
) {
	if suggested != nil && suggested.IsPositive() {
		return *suggested, referencePriceSourceBaseline
	}
	if purchaseAvg.IsPositive() {
		return purchaseAvg, referencePriceSourcePurchaseAvg
	}
	return decimal.Zero, referencePriceSourceNone
}

// 参考成本单价的**来源**标记(前端要能说清"这个数是怎么来的")。
const (
	// referencePriceSourceBaseline = **建议成本单价**(分割基线 × 今天进价折算)—— B 口径的正统来源。
	referencePriceSourceBaseline = "baseline"
	// referencePriceSourcePurchaseAvg = 全店采购均价兜底(这个部位没有基线价)。
	referencePriceSourcePurchaseAvg = "purchase_avg"
	// referencePriceSourceNone = 取不到(既没有基线价、当天也没进货)。
	referencePriceSourceNone = "none"
)

// referenceMarginRateOfDay 算当日**总毛利率** m —— 给**单价样本采集**用(**旧口径 A**)。
//
// ⚠️ 看板的参考毛利已改成 B 口径(不用 m,也不用日终实盘);
//
//	**这里刻意保留旧口径**:单价基线是**历史统计量**,
//	它的样本按「进价 = 零售均价 ÷ (1+m)」倒推 ⇒ m 用旧口径 ⇒ 基线不动。
//	⇒ 换口径不会改写历史;要不要连基线一起换,是另一个决定。
//
// # 为什么单价样本要用参考口径(2026-11-01 owner)
//
//	账面毛利率 = (营收 − 账面销售成本 − …) ÷ 营收
//	而账面销售成本走移动加权链 ⇒ **依赖分割重量** ⇒ 切割误差会被带进单价基线
//	⇒ 正是 owner 要断的那条污染链。
//	参考口径的 m = (营收 − 采购 + 期末剩余成本) ÷ 营收 ⇒ **零分割假设**。
//
// # 返回
//
//	(margin, 是否可用于倒推, error)
//
// ⚠️ 不可用的两种情况:
//   - 当天没有完整日终盘点 ⇒ 剩余算不出来 ⇒ m 算不出来;
//   - m 越界(≤0 或 ≥0.95)⇒ 倒推出的单价会把基线教歪 ⇒ 整天不产样本。
func (s *Service) referenceMarginRateOfDay(ctx context.Context, branchID string, day time.Time,
	rows []CutDailyCost) (decimal.Decimal, bool, error) {
	ref, err := s.referenceDayTotalsOf(ctx, branchID, day, rows)
	if err != nil {
		return decimal.Zero, false, err
	}
	var revenue decimal.Decimal
	for _, r := range rows {
		revenue = revenue.Add(r.RevenueYuan)
	}
	margin, ok := referenceMarginRate(ref, revenue)
	if !ok {
		return margin, false, nil
	}
	if !priceMarginUsable(margin) {
		return margin, false, nil
	}
	return margin, true, nil
}

// referenceYieldRate 算**出成率** = 真实切分产出 ÷ 当日进货毛重。
//
// # 真实切分产出(台面恒等式解出来的,**纯事实**)
//
//	产出 = 台面日终实盘 + 零售销量 + 统收量 + 报损量 + 搭赠量 − 搬上台量
//
// # 为什么是台面口径就够
//
//	台面恒等式:期初台面(夜里清空 ⇒ 0)+ 今日切分 + 搬上台 − 销售 − 搭赠 − 统收 − 报损 = 期末台面
//	⇒ 今日切分 = 期末台面 + 销售 + 搭赠 + 统收 + 报损 − 搬上台
//	⚠️ **搬上台要减掉**:那是**前几天**切好的老货(冷库→台面),不是今天的产出。
//	  (生产实测 店 00 10-30:减掉它才能对上 241.755kg)
//
// # 为什么这个指标重要(owner 2026-11-01)
//
//	**> 100% ⇒ 有问题,要去查**。
//	物理上切分产出不可能超过猪重 ⇒ >100% 说明:
//	  - **搬台没登记**(搬上来的老货被算成今天的产出)——生产实测 店 00 10-30 = 106.4%;
//	  - 或**盘点漏了部位**(真实剩余比盘出来的多)。
//	⇒ 它同时是"多店产出比评估"的基础指标。
func referenceYieldRate(rows []CutDailyCost, purchasedKg decimal.Decimal) (decimal.Decimal, bool) {
	if !purchasedKg.IsPositive() {
		return decimal.Zero, false
	}
	var remain, sold, out, load decimal.Decimal
	hasStocktake := false
	for _, r := range rows {
		if r.DataSource == dataSourceActual {
			hasStocktake = true
			remain = remain.Add(r.CounterActualKg)
		}
		sold = sold.Add(r.SoldKg).Add(r.BulkSoldKg)
		out = out.Add(r.WasteKg).Add(r.GiftKg)
		load = load.Add(r.CounterLoadKg)
	}
	if !hasStocktake {
		// ⚠️ 没有完整日终盘点 ⇒ 期末台面算不出来 ⇒ 真实产出也算不出来。
		return decimal.Zero, false
	}
	produced := remain.Add(sold).Add(out).Sub(load)
	if produced.IsNegative() {
		produced = decimal.Zero
	}
	return produced.Div(purchasedKg), true
}

type referencePartCost struct {
	// UnitPriceYuan 是部位成本单价(元/kg)。
	UnitPriceYuan decimal.Decimal
	// UnitPriceSource 是 UnitPriceYuan 的来源标记。
	UnitPriceSource string
	// SalesCostYuan 是**参考销售成本** = 成本单价 × 销量(零售 + 统收)。
	SalesCostYuan decimal.Decimal
	// GiftCostYuan / WasteCostYuan 是搭赠 / 报损的成本。
	GiftCostYuan  decimal.Decimal
	WasteCostYuan decimal.Decimal
	// GrossMarginYuan 是**参考毛利** = 营收 − 销售成本 − 搭赠成本 − 报损成本。
	GrossMarginYuan decimal.Decimal
	// GrossMarginPct 是参考毛利率(%)。
	GrossMarginPct float64
	// Priceable = false ⇒ 这个部位的参考毛利**算不出来**(取不到成本单价)。
	Priceable bool
}

// referencePartCostOf 算某个部位当天的参考成本与毛利。
//
// # 输入
//
//	unit       该部位的**参考成本单价**(元/kg,由 referenceCutUnitPriceFromBaseline 给)
//	source     单价来源(见 referencePriceSource*)
//	revenue    当日该部位营收(零售 + 统收 + 分摊进来的特价)
//	soldKg     当日该部位销量(零售 + 统收)
//	giftKg     搭赠量
//	wasteKg    报损量
//
// # 输出
//
//	参考销售成本 = 成本单价 × 销量
//	参考毛利     = 营收 − 参考销售成本 − 搭赠成本 − 报损成本
//
// ⚠️ **没有"建议毛利"这一层**:建议成本单价 → 建议毛利 是把同一个信息换个形式说一遍,
//
//	而 m 进去、m′ 出来 ⇒ 数学上永远对不上 header ⇒ owner 2026-11-01 明确要求删掉。
func referencePartCostOf(unit decimal.Decimal, source string,
	revenue, soldKg, giftKg, wasteKg decimal.Decimal,
) referencePartCost {
	out := referencePartCost{}
	out.UnitPriceYuan = unit
	out.UnitPriceSource = source
	if source == referencePriceSourceNone {
		return out // Priceable=false ⇒ 毛利算不出来
	}
	out.Priceable = true
	out.SalesCostYuan = unit.Mul(soldKg)
	out.GiftCostYuan = unit.Mul(giftKg)
	out.WasteCostYuan = unit.Mul(wasteKg)
	out.GrossMarginYuan = revenue.Sub(out.SalesCostYuan).Sub(out.GiftCostYuan).Sub(out.WasteCostYuan)
	base := revenue
	if !base.IsZero() {
		f, _ := out.GrossMarginYuan.Mul(decimal.NewFromInt(100)).Div(base).Float64()
		out.GrossMarginPct = f
	}
	return out
}
