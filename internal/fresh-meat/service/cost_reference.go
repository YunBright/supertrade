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
//
//	**依赖日终实盘** ⇒ 门店当天没做日终盘点 ⇒ m / 成本单价 / 参考毛利 / 毛利率 / 售罄率
//	**连锁算不出来** ⇒ 界面一片「—」⇒ 明明有猪价、有分割基线,却拿不到当天的毛利。
//
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
// ⚠️ 全部来自**事实**:猪(whole_pigs)、外采(external_purchases)、销售(cube)、盘点单(猪肉台实盘)。
// ⚠️ 没有任何一项来自分割建议 ⇒ 切割不准也不影响这里。
type ReferenceDayTotals struct {
	// PurchasedKg / PurchasedYuan 是当日进店的**猪**的合计。
	//
	// ⚠️ 这是**进猪口径**(owner 2026-11-03 拍板:看板的进货栏一个字不动)。
	PurchasedKg   decimal.Decimal
	PurchasedYuan decimal.Decimal
	// PurchaseAvgYuan 是**进猪**的采购均价(元/kg)= 进猪金额 ÷ 进猪重量。
	//
	// ⚠️ 它是**剩余成本**的估值价(owner:「可以用采购均价计算」)——
	//   也是**部位成本单价取不到零售均价时的兜底价**。
	PurchaseAvgYuan decimal.Decimal
	// ExternalKg / ExternalYuan 是当日**外采**的合计(kg / 元)。
	//
	// ⚠️ **与进猪口径分开**(owner 2026-11-03 拍板):
	//   进猪(whole_pigs)是"一整头猪",外采是"已经切好的某个部位的额外进货"
	//   ⇒ 两者不能合并成一个"总进货"数(合并了门店就看不出外采花了多少钱)。
	ExternalKg   decimal.Decimal
	ExternalYuan decimal.Decimal
	// ExternalByCut 是**按部位**的外采合计(owner 2026-11-03:参考毛利的成本单价
	// 要按量加权,必须知道每个部位各外采了多少、什么价)。
	//
	// ⚠️ key = cut_type(部位);一个部位多笔不同价 ⇒ 单价按**量加权**(金额 ÷ 重量)。
	ExternalByCut map[string]ExternalCutTotals
	// RemainKg 是**期末猪肉台实盘**(kg)—— **不含冷库**。
	RemainKg decimal.Decimal
	// HasRemain = false ⇒ 当天没有完整日终盘点 ⇒ 剩余算不出来。
	//
	// ⚠️ 这时**不能**给毛利(前端显示「—」),也不能给 0
	//   (0 会被门店误读成"当天一点货都没剩、还亏了本")。
	HasRemain bool
}

// ExternalCutTotals 某个部位当天的**外采**合计。
type ExternalCutTotals struct {
	// Kg 是该部位当天外采的重量(kg)。
	Kg decimal.Decimal
	// AmountYuan 是该部位当天外采的金额(元)。
	AmountYuan decimal.Decimal
	// UnitPriceYuan 是该部位外采的**加权单价**(元/kg)= 金额 ÷ 重量。
	UnitPriceYuan decimal.Decimal
}

// TotalPurchasedKg 当日**含外采**的进货量(kg)= 进猪 + 外采。
//
// ⚠️ 与 [ReferenceDayTotals.PurchasedKg](进猪口径)**刻意分开**:
//   看板的进货栏要保持"进猪口径"(owner 2026-11-03 拍板),
//   而长期毛利的「期间采购」与采样用的采购均价**要含外采**(owner 同日拍板)。
func (t ReferenceDayTotals) TotalPurchasedKg() decimal.Decimal {
	return t.PurchasedKg.Add(t.ExternalKg)
}

// TotalPurchasedYuan 当日**含外采**的进货金额(元)= 进猪 + 外采。
func (t ReferenceDayTotals) TotalPurchasedYuan() decimal.Decimal {
	return t.PurchasedYuan.Add(t.ExternalYuan)
}

// TotalPurchaseAvgYuan 当日**含外采**的采购均价(元/kg)= 总金额 ÷ 总重量。
//
// ⚠️ 这就是 owner 说的「**求均值**」:不是把成本单价直接换成外采价,
//   而是按**重量**把外采并进均价里。
func (t ReferenceDayTotals) TotalPurchaseAvgYuan() decimal.Decimal {
	kg := t.TotalPurchasedKg()
	if !kg.IsPositive() {
		return decimal.Zero
	}
	return t.TotalPurchasedYuan().Div(kg)
}

// externalByCutOf 某营业日的**外采**:按部位汇总(kg / 金额 / 加权单价)+ 全店合计。
//
// ⚠️ 日期用**区间**而不是等值:datatypes.Date 落库是"带时区的零点",
//   PG 里 `date = timestamp` 会按会话时区折算 ⇒ 等值不可靠。
//
// ⚠️ 同一部位多笔不同价 ⇒ 单价按**量加权**(金额 ÷ 重量)。
func (s *Service) externalByCutOf(ctx context.Context, branchID string, day time.Time) (
	map[string]ExternalCutTotals, decimal.Decimal, decimal.Decimal, error,
) {
	byCut := map[string]ExternalCutTotals{}
	startOfDay, endOfDay := s.businessDayBounds(day)
	var rows []model.ExternalPurchase
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date >= ? AND biz_date < ?", branchID, startOfDay, endOfDay).
		Find(&rows).Error; err != nil {
		return nil, decimal.Zero, decimal.Zero, fmt.Errorf("query external_purchases(reference): %w", err)
	}
	var totalKg, totalYuan decimal.Decimal
	for _, r := range rows {
		totalKg = totalKg.Add(r.QtyKg)
		totalYuan = totalYuan.Add(r.TotalAmountYuan)
		cut := string(r.CutType)
		if cut == "" {
			continue
		}
		v := byCut[cut]
		v.Kg = v.Kg.Add(r.QtyKg)
		v.AmountYuan = v.AmountYuan.Add(r.TotalAmountYuan)
		byCut[cut] = v
	}
	for cut, v := range byCut {
		if v.Kg.IsPositive() {
			v.UnitPriceYuan = v.AmountYuan.Div(v.Kg)
		}
		byCut[cut] = v
	}
	return byCut, totalKg, totalYuan, nil
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
// ⚠️ **采购与采购均价都含外采**(owner 2026-11-03 拍板:「采样算当日毛利率时
// 用含外采的采购均价」)⇒ 外采的成本进 m ⇒ 倒推出的进货单价含外采
// ⇒ **单价基线也就含了外采**。
//
// ⚠️ 返回 ok=false 的三种情况(都**不能**给 0):
//   - 当天**没有进货**(进猪 + 外采都没有)⇒ 参考口径整体失效(见下);
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
	if !tot.TotalPurchasedKg().IsPositive() {
		return decimal.Zero, false
	}
	if !tot.HasRemain || !revenue.IsPositive() {
		return decimal.Zero, false
	}
	remainYuan := tot.RemainKg.Mul(tot.TotalPurchaseAvgYuan())
	gm := revenue.Sub(tot.TotalPurchasedYuan()).Add(remainYuan)
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

	// 外采(2026-11-03 owner):单独一栏,**不并进 PurchasedKg/PurchasedYuan**。
	//
	// ⚠️ 为什么分开:外采是"已经切好的某个部位",进猪是"一整头猪"
	//   ⇒ 合并后门店看不出"今天额外买了多少某部位的货、花了多少钱"。
	extByCut, extKg, extYuan, err := s.externalByCutOf(ctx, branchID, day)
	if err != nil {
		return out, err
	}
	out.ExternalByCut = extByCut
	out.ExternalKg = extKg
	out.ExternalYuan = extYuan

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

// referenceCutUnitPrice 取某个部位当天的**参考成本单价**(元/kg)—— **含外采加权**。
//
// # 口径(owner 2026-11-03 拍板:「求均值」)
//
//	unit_c = (base_c × pigKg_c + extPrice_c × extKg_c) ÷ (pigKg_c + extKg_c)
//
//	base_c     = 建议成本单价(基线折算);取不到 ⇒ 用**进猪**采购均价
//	             (⚠️ **进猪口径、不含外采** ⇒ 免得外采被算两遍)
//	pigKg_c    = 该部位今天的**猪分割产出**(kg,不含外采)
//	extPrice_c = 该部位当天外采的加权单价;extKg_c = 外采量
//
// ⇒ **不是**把成本单价直接换成外采价(owner 明确否掉)
//   ⇒ 而是按**重量**把两批货的成本合到一起 ⇒ 这才是该部位当天的真实成本。
//
// # 兜底链(没有外采时与从前一致)
//
//	建议成本单价 → 全店采购均价(含外采) → 取不到(= none)
//
// ⚠️ 为什么要有兜底:某个部位可能**没有自己的基线价**
//
//	(部位单价基线覆盖不到、或退取链只走到了整店均价)⇒ 它也要计成本
//	(搭赠/报损这类"有量、无销售"的部位也要计成本)⇒ 单价不能是空。
//
// ⚠️ 返回 source 让调用方能说清"这个单价是怎么来的"。
func referenceCutUnitPrice(suggested *decimal.Decimal, pigAvg decimal.Decimal, totalAvg decimal.Decimal,
	pigKg decimal.Decimal, ext ExternalCutTotals) (decimal.Decimal, string) {
	// ---- base:建议成本单价优先;取不到 ⇒ 进猪采购均价(不含外采)----
	base := decimal.Zero
	baseSource := referencePriceSourceNone
	switch {
	case suggested != nil && suggested.IsPositive():
		base = *suggested
		baseSource = referencePriceSourceBaseline
	case pigAvg.IsPositive():
		base = pigAvg
		baseSource = referencePriceSourcePurchaseAvg
	}
	// ---- 有外采 ⇒ 按量加权 ----
	if ext.Kg.IsPositive() && ext.UnitPriceYuan.IsPositive() && base.IsPositive() {
		if total := pigKg.Add(ext.Kg); total.IsPositive() {
			unit := base.Mul(pigKg).Add(ext.UnitPriceYuan.Mul(ext.Kg)).Div(total)
			if baseSource == referencePriceSourceBaseline {
				return unit, referencePriceSourceBaselineExternal
			}
			return unit, referencePriceSourcePurchaseAvgExternal
		}
	}
	// ---- 没有外采 ⇒ 原兜底链 ----
	if base.IsPositive() {
		return base, baseSource
	}
	if totalAvg.IsPositive() {
		return totalAvg, referencePriceSourcePurchaseAvg
	}
	return decimal.Zero, referencePriceSourceNone
}

// pigSplitKgOf 该部位今天的**猪分割产出**(kg)—— 加权的另一半(不含外采)。
//
// ⚠️ **不能用 AdjustedSplitKg(= prodK)**:prodK 已经**叠加了外采**(cut_cost.go 的 8c-2)
//   ⇒ 拿它加权 = 把外采算两遍(外采既进权重、又进单价)。
//
// ⇒ 优先**实际分割 A_c**(实测),没有则退**建议分割 S_c**(基线)。
func pigSplitKgOf(src *CutDailyCost) decimal.Decimal {
	if src.ActualSplitKg != nil && src.ActualSplitKg.IsPositive() {
		return *src.ActualSplitKg
	}
	return src.SuggestedSplitKg
}

// 参考成本单价的**来源**标记(前端要能说清"这个数是怎么来的")。
const (
	// referencePriceSourceBaseline = **建议成本单价**(分割基线 × 今天进价折算)—— B 口径的正统来源。
	referencePriceSourceBaseline = "baseline"
	// referencePriceSourcePurchaseAvg = 全店采购均价兜底(这个部位没有基线价)。
	referencePriceSourcePurchaseAvg = "purchase_avg"
	// referencePriceSourceBaselineExternal = 建议成本单价与**外采价按量加权**(owner 2026-11-03)。
	referencePriceSourceBaselineExternal = "baseline_external"
	// referencePriceSourcePurchaseAvgExternal = 进猪采购均价与**外采价按量加权**。
	referencePriceSourcePurchaseAvgExternal = "purchase_avg_external"
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
//	产出 = 台面日终实盘 + 零售销量 + 统收量 + 报损量 + 搭赠量 − 搬上台量 − **外采量**
//
// # 为什么是台面口径就够
//
//	台面恒等式:期初台面(夜里清空 ⇒ 0)+ 今日切分 + 搬上台 − 销售 − 搭赠 − 统收 − 报损 = 期末台面
//	⇒ 今日切分 = 期末台面 + 销售 + 搭赠 + 统收 + 报损 − 搬上台
//	⚠️ **搬上台要减掉**:那是**前几天**切好的老货(冷库→台面),不是今天的产出。
//	  (生产实测 店 00 10-30:减掉它才能对上 241.755kg)
//
// # ⚠️ **外采也要减掉**(owner 2026-11-03)
//
//	外采是**买来的成品**(某个部位),不是今天从猪身上切出来的
//	⇒ 混进「今天的切分产出」里 ⇒ 产出被抬高、分母没抬高 ⇒ 出成率虚高。
//	生产实测 店 00 10-30:外采 12.25kg ⇒ 106.45% → **101.06%**。
//
// # 分子里**搭赠/报损要算进来**(owner 2026-11-03 拍板:按物理恒等式)
//
//	搭赠/报损的货**确实离开了台面** ⇒ 它属于「今天的产出」的一部分
//	⇒ 不算进去 ⇒ 产出被低估 ⇒ 出成率偏低。
//
// # 为什么这个指标重要(owner 2026-11-01)
//
//	**> 100% ⇒ 有问题,要去查**。
//	物理上切分产出不可能超过猪重 ⇒ >100% 说明:
//	  - **搬台没登记**(搬上来的老货被算成今天的产出)——生产实测 店 00 10-30 = 106.4%;
//	  - 或**盘点漏了部位**(真实剩余比盘出来的多)。
//	⇒ 它同时是"多店产出比评估"的基础指标。
//
// # ⚠️ 分母是**进猪毛重**(不含外采)
//
//	owner 2026-11-03 的公式:分子减外采 ⇒ 分母也只用进猪毛重
//	⇒ 外采那部分**不进这个比率**(它是"买来的成品",不是猪的产出)。
func referenceYieldRate(rows []CutDailyCost, purchasedKg, externalKg decimal.Decimal) (decimal.Decimal, bool) {
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
	produced := remain.Add(sold).Add(out).Sub(load).Sub(externalKg)
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
