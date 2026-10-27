// Package service / cut_margin.go —— 部位级毛利报表装配(2026-10-27)。
//
// # 与旧实现的差别
//
// 旧实现(`gross_margin.go` 的 `rollUpPerCut`)是**把 SKU 行汇总成部位行**;
// 成本链是 SKU 级的 ⇒ 汇总出来的部位行继承了两个坑:
//  1. 某 SKU 账面 0 却卖出 ⇒ 它的「应有」被裁成 0 ⇒ 部位"应有"被抬高;
//  2. 那些销量的成本是 0 ⇒ 部位毛利虚高。
//
// 新实现:**部位行就是引擎的输出**(不再汇总),SKU 行只承载销售/营收。
//
// # 特价分摊
//
// 特价(promo)是**部位级**的桶 ⇒ 分摊依据(各部位账面缺口)也用部位行算:
//	gap_c = 期初_c + 入库_c − 正常销量_c − 统收_c − 报损_c − 期末_c
package service

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
)

// promoRowsOf 把部位行转成特价分摊要的形状(字段名一致 ⇒ 复用同一套逻辑)。
//
// ⚠️ 只填分摊用到的字段;其余留 0。
//
// # ClosingKg 保持**原样**(不裁 0)——2026-10-27 核对过
//
//	特价分摊的"缺口" = 期初 + 入库 − 销售 − 统收 − 报损 − 期末。
//	- **盘点日**:期末 = 实盘 ⇒ 缺口 = 账面应有 − 实盘(盘亏/短卖的那块)
//	  ⇒ 特价销售被分摊到"账面说有、实际少了"的部位。
//	- **非盘点日**:期末 = 账面推演 ⇒ 缺口 = 0 ⇒ 不分摊(特价留在"未归属"桶)。
//
//	⚠️ 这与**旧引擎一致**:旧引擎的期末是**裁 0 的账面应有**,
//	  非盘点日缺口同样 ≤ 0(要么 0、要么负数被跳过)⇒ 旧引擎也只在盘点日分摊。
//	⇒ 这里**不要**再裁 0(那是多动):裁了会把"链上结余为负"的部位
//	  凭空造出一个正缺口,把特价分摊到不该去的部位。
func promoRowsOf(cutRows []CutDailyCost) []SkuDailyCost {
	out := make([]SkuDailyCost, 0, len(cutRows))
	for _, r := range cutRows {
		out = append(out, SkuDailyCost{
			CutType:      r.CutType,
			OpeningKg:    r.OpeningKg,
			ProducedKg:   r.ProducedKg,
			SoldKg:       r.SoldKg,
			SoldAmount:   r.SoldAmount,
			BulkSoldKg:   r.BulkSoldKg,
			WasteKg:      r.WasteKg,
			ClosingKg:    r.ClosingKg,
			RevenueYuan:  r.RevenueYuan,
			DataSource:   r.DataSource,
		})
	}
	return out
}

// rollUpPerCutCuts 把**部位行**装配成对外的部位明细 + 全店合计。
//
// promoAlloc 是特价分摊结果:
//   - **分摊成功** ⇒ 部位行加上分摊进来的特价 kg/金额/成本,
//     且**不再单列 promo 桶**(否则同一笔收入被算两次);
//   - **分摊失败** ⇒ promo 桶原样保留(金额重量仍然可见,只是没分到部位)。
func rollUpPerCutCuts(cutRows []CutDailyCost, promoAlloc *PromoAllocation) ([]PerCutGrossMargin, TotalGrossMargin) {
	allocByCut := promoAllocByCut(promoAlloc)
	allocated := promoAlloc != nil && !promoAlloc.IsEstimated && len(promoAlloc.Allocated) > 0

	perCut := make([]PerCutGrossMargin, 0, len(cutRows))
	var tot TotalGrossMargin
	for _, r := range cutRows {
		if allocated && r.CutType == model.CutPromo {
			// 已分摊到各部位 ⇒ 这里不再计(否则重复)。
			continue
		}
		revenue := r.RevenueYuan
		soldKg := r.SoldKg
		salesCost := r.SalesCostYuan
		bulkCost := r.BulkCostYuan
		wasteCost := r.WasteCostYuan
		// 分摊进来的特价:重量 + 收入 + 成本(成本 = kg × 该部位移动加权单价)
		if pa, ok := allocByCut[r.CutType]; ok {
			soldKg = soldKg.Add(pa.PromoKg)
			revenue = revenue.Add(pa.PromoAmount)
			salesCost = salesCost.Add(pa.PromoKg.Mul(r.MovingAvgUnitCost))
		}
		// 毛利 = 收入 − 销售成本 − 统收成本 − 报损成本 + 预期毛利(预售结转的重估)
		gm := revenue.Sub(salesCost).Sub(bulkCost).Sub(wasteCost).Add(r.PresaleMarginYuan)
		var gmPct float64
		gmBase := revenue.Add(r.PresaleExpectedRevenueYuan)
		if !gmBase.IsZero() {
			f, _ := gm.Mul(decimal.NewFromInt(100)).Div(gmBase).Float64()
			gmPct = f
		}
		row := PerCutGrossMargin{
			CutType:           r.CutType,
			OpeningKg:         r.OpeningKg,
			PurchasedKg:       r.ProducedKg,
			SoldKg:            soldKg,
			BulkSoldKg:        r.BulkSoldKg,
			WasteKg:           r.WasteKg,
			// ⚠️ 应有 = **账面推演**(不裁 0)⇒ 负数 = **账面短卖**。
			//   旧实现是"逐 SKU 裁剪后再求和"⇒ 缺口被抹掉 ⇒ 门店看到的行自相矛盾。
			ExpectedRemainKg: r.ExpectedClosingKg,
			RevenueYuan:       revenue,
			SalesCostYuan:     salesCost,
			BulkCostYuan:      bulkCost,
			WasteCostYuan:     wasteCost,
			GrossMarginYuan:   gm,
			GrossMarginPct:    gmPct,
			PresaleMarginYuan: r.PresaleMarginYuan,
			PresaleExpectedRevenueYuan: r.PresaleExpectedRevenueYuan,
			MovingAvgUnitCost: r.MovingAvgUnitCost,
			// ⚠️ 该部位当天成本**算不出来**(不是成本为 0)⇒ 行级标注。
			CostIncomplete: r.CostUncomputable,
		}
		// ⚠️ 「实盘剩余」**只在有真实盘点时才有值**。
		// 无盘点 ⇒ 留空(指针 nil ⇒ JSON 里**没有这个键**)⇒ 前端显示「—」,
		// 而不是把账面推演值冒充成实盘(2026-10-26 门店反馈)。
		if r.DataSource == dataSourceActual {
			actual := r.ClosingKg
			variance := r.ClosingKg.Sub(r.ExpectedClosingKg)
			row.ActualRemainKg = &actual
			row.VarianceKg = &variance
		}
		perCut = append(perCut, row)

		tot.RevenueYuan = tot.RevenueYuan.Add(revenue)
		tot.SalesCostYuan = tot.SalesCostYuan.Add(salesCost)
		tot.BulkCostYuan = tot.BulkCostYuan.Add(bulkCost)
		tot.WasteCostYuan = tot.WasteCostYuan.Add(wasteCost)
		tot.GrossMarginYuan = tot.GrossMarginYuan.Add(gm)
		tot.PresaleMarginYuan = tot.PresaleMarginYuan.Add(r.PresaleMarginYuan)
		tot.PresaleExpectedRevenueYuan = tot.PresaleExpectedRevenueYuan.Add(r.PresaleExpectedRevenueYuan)
	}
	if !tot.RevenueYuan.Add(tot.PresaleExpectedRevenueYuan).IsZero() {
		f, _ := tot.GrossMarginYuan.Mul(decimal.NewFromInt(100)).
			Div(tot.RevenueYuan.Add(tot.PresaleExpectedRevenueYuan)).Float64()
		tot.GrossMarginPct = f
	}
	sortPerCut(perCut)
	return perCut, tot
}

// sortedCutKeys 把部位集合按字典序排出来(告警顺序稳定 ⇒ 门店每次看到一样的顺序)。
func sortedCutKeys(m map[string]decimal.Decimal) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// buildCutMarginAlerts 生成当天的红字/黄字告警(**部位口径**)。
//
// # ① 负库存(红字)
//
// ⚠️ 旧实现按 SKU 判(`r.CounterKg` / `r.ColdStoreKg`),而位置表是 **SKU 级**、
//   日终结余快照是 **部位级** ⇒ 旧实现拿**两种粒度**比较 ⇒ 告警本身就是错的。
//   ⇒ 现在两边都聚到**部位**再比。
//
// # ② 余额与按营业日推导不一致(黄字)
func (s *Service) buildCutMarginAlerts(ctx context.Context, branchID string, day time.Time,
	cutRows []CutDailyCost) []MarginAlert {
	var out []MarginAlert
	counter, cold, err := s.loadSkuPositions(ctx, branchID)
	if err != nil {
		return out
	}
	cutOfSku, _, err := s.loadSkuCatalog(ctx, branchID, nil)
	if err != nil {
		return out
	}
	// 位置表(SKU 级)→ 部位级。
	//
	// ⚠️ **柜台与冷库分开聚合**:门店要求③「修改值造成负库存时红字显示」
	//   ⇒ 冷库被搬成负数(柜台 +10 / 冷库 −5)也必须红字 ⇒ 不能只看两者的合计。
	counterCut := map[string]decimal.Decimal{}
	coldCut := map[string]decimal.Decimal{}
	liveCut := map[string]decimal.Decimal{}
	for sku, v := range counter {
		cut := string(cutOfSku[sku])
		if cut == "" {
			continue
		}
		counterCut[cut] = counterCut[cut].Add(v)
		liveCut[cut] = liveCut[cut].Add(v)
	}
	for sku, v := range cold {
		cut := string(cutOfSku[sku])
		if cut == "" {
			continue
		}
		coldCut[cut] = coldCut[cut].Add(v)
		liveCut[cut] = liveCut[cut].Add(v)
	}
	// ① 账面短卖(负库存)= 账面推演的期末为负。
	for _, r := range cutRows {
		if r.ExpectedClosingKg.IsNegative() {
			out = append(out, MarginAlert{
				Level:   MarginAlertLevelError,
				Code:    MarginAlertNegativeInventory,
				// ⚠️ 文案必须说清"**已经计了成本**"(2026-10-27 改):
				//   旧口径把负数裁 0 ⇒ 卖超的部分**零成本** ⇒ 毛利虚高。
				//   新口径负数不裁 ⇒ 短卖那部分**照摊当期移动加权成本** ⇒ 毛利不再虚高。
				//   ⇒ 再说"未计成本"就是**错的**(worker A 报告的遗留问题 2)。
				Message: fmt.Sprintf("%s 账面短卖 %s kg(卖超账面库存;这部分销售已按当期移动加权成本计入成本,毛利相应下降)",
					r.CutType, r.ExpectedClosingKg.Abs().String()),
				CubeProductID: string(r.CutType),
				Kg:            r.ExpectedClosingKg.Abs(),
			})
		}
	}
	// ①b 位置表负余额(红字)—— 保住门店要求③(2026-10-24)。
	//
	// ⚠️ **为什么必须单独看位置表**:
	//   成本链的"应有"是**账面推演**(期初 + 入库 − 销售 − 统收 − 报损),
	//   它**不建模搬台/过夜结转**(那些是内部转移)⇒ 往日的搬台量改多了,
	//   冷库余额会变成负数,但账面推演仍然是正数
	//   ⇒ 只看账面推演就**看不到**这个负库存 ⇒ 门店要求就丢了。
	//
	// ⚠️ 位置表是 **SKU 级** ⇒ 聚到**部位**再判负(两边同一粒度)。
	for _, cut := range sortedCutKeys(counterCut) {
		if v := counterCut[cut]; v.IsNegative() {
			out = append(out, MarginAlert{
				Level:         MarginAlertLevelError,
				Code:          MarginAlertNegativeInventory,
				Message:       fmt.Sprintf("%s 柜台余额为负 %s kg", cut, v.String()),
				CubeProductID: cut,
				Kg:            v,
			})
		}
	}
	for _, cut := range sortedCutKeys(coldCut) {
		if v := coldCut[cut]; v.IsNegative() {
			out = append(out, MarginAlert{
				Level:         MarginAlertLevelError,
				Code:          MarginAlertNegativeInventory,
				Message:       fmt.Sprintf("%s 冷库余额为负 %s kg", cut, v.String()),
				CubeProductID: cut,
				Kg:            v,
			})
		}
	}
	// ② 当前余额 vs 按营业日推导的结余(有当天快照时才可比)。
	snapCounter, snapCold, ok, err := s.closingForDay(ctx, branchID, day)
	if err != nil || !ok {
		return out
	}
	cuts := map[string]struct{}{}
	for cut := range snapCold {
		cuts[cut] = struct{}{}
	}
	for cut := range snapCounter {
		cuts[cut] = struct{}{}
	}
	ordered := make([]string, 0, len(cuts))
	for cut := range cuts {
		ordered = append(ordered, cut)
	}
	sort.Strings(ordered)
	for _, cut := range ordered {
		derived := snapCold[cut].Add(snapCounter[cut])
		live := liveCut[cut]
		diff := derived.Sub(live)
		if diff.IsZero() {
			continue
		}
		out = append(out, MarginAlert{
			Level:   MarginAlertLevelWarning,
			Code:    MarginAlertBalanceMismatch,
			Message: fmt.Sprintf("%s 当前余额 %s kg 与按营业日推导的结余 %s kg 不一致(差 %s kg)",
				cut, live.String(), derived.String(), diff.String()),
			CubeProductID: cut,
			Kg:            diff,
		})
	}
	return out
}
