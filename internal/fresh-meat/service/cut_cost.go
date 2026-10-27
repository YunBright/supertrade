// Package service / cut_cost.go —— **部位级**移动加权成本引擎(2026-10-27)。
//
// # 为什么从 SKU 级改成部位级
//
// 门店口径一直就是部位(见 model.go / CutStockDaily / SkuStockOpening 的注释):
//
//	「鲜肉只管部位数据,不管 SKU;SKU 映射只是为了把销售、数量等数据聚合到其部位……
//	  日终盘点也是记录部位。库存,大宗结算都是部位。」
//
// 而旧的成本链(`moving_average.go` 的 `computeSkuDailyCosts`)是 **SKU 主键** ⇒ 两套粒度打架:
//
//  1. `overlayStockOpenings` 把**部位**的期初结转 100% 记到该部位的「主 SKU」,
//     同部位其它 SKU 的期初被清零 ⇒ 那个货号账面恒为 0;
//  2. 账面 0 的货号卖出 ⇒ 移动加权单价 0 ⇒ **销售成本 0 ⇒ 毛利率 100%**(毛利虚高);
//  3. `expectedCloseKg` 负数裁 0 ⇒ 缺口被抹掉 ⇒ 部位"应有"反而变正
//     ⇒ 门店看到"进货 4.7、销售 16.9、应有 5.0"这种**自相矛盾**的行。
//
// ⇒ 本文件把**成本计算单元**换成部位:库存 / 结余 / 期初 / 成本链全是部位,
// SKU 只承载「卖了多少、卖了多少钱」。
//
// # 部位级引擎的计算式
//
//	期初(部位)   = 上一营业日的部位级成本快照结余
//	              → 回退: closingForDay(D−1) 部位结余 kg × 部位移动加权单价
//	              → 回退: sku_stock_openings 期初结转(**按部位直接用**,不再落 SKU)
//	入库(部位)   = 今日分割产出 kg + 其猪成本分摊额
//	可售金额     = 期初金额 + 入库金额
//	可售数量     = 期初 kg + 入库 kg
//	移动加权单价 = 可售金额 / 可售数量
//	期末(部位)   = 有盘点 → closingForDay(D)(柜台 + 冷库)
//	              无盘点 → 期初 + 入库 − 销售 − 统收 − 报损    ← **不裁 0**
//	期末金额     = 期末 kg × 移动加权单价
//	销售成本     = 可售金额 − 期末金额 − 统收成本 − 报损成本      ← **不裁 0**
//	毛利         = 收入 − 销售成本 − 统收成本 − 报损成本 + 预期毛利
//
// ⚠️ **两处"不裁 0"是本次改造的核心**:
//   - 期末为负 ⇒ 该部位**账面短卖**(卖超了库存)⇒ 短卖部分照摊成本 ⇒ 毛利不再虚高;
//   - 销售成本为负 ⇒ 只可能是"期末实盘 > 可售金额"(盘盈)⇒ 裁 0 并记日志。
//
// # 恒等式
//
//	Σ部位期初金额 + Σ部位入库金额 == Σ销售成本 + Σ统收成本 + Σ报损成本 + Σ期末存货金额
//
// ⚠️ 期末金额**可以为负**(账面欠货)⇒ 对账行里也会是负数 ⇒ 这是真相,不是 bug。
package service

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
)

// CutDailyCost 某店某营业日**某部位**的移动加权成本明细(成本链的计算单元)。
type CutDailyCost struct {
	BranchID string
	CutType  model.CutType

	OpeningKg     decimal.Decimal
	OpeningAmount decimal.Decimal
	ProducedKg    decimal.Decimal
	ProducedAmount decimal.Decimal
	SoldKg        decimal.Decimal
	SoldAmount    decimal.Decimal
	BulkSoldKg    decimal.Decimal
	BulkSoldAmount decimal.Decimal
	WasteKg       decimal.Decimal
	WasteAmount   decimal.Decimal

	// ExpectedClosingKg 是**账面应有**期末量(**不裁 0** ⇒ 负数 = 账面短卖)。
	ExpectedClosingKg decimal.Decimal
	// ClosingKg 是期末量(有盘点 = 实盘;无盘点 = 账面推演)。
	ClosingKg     decimal.Decimal
	ClosingAmount decimal.Decimal

	MovingAvgUnitCost decimal.Decimal
	SalesCostYuan     decimal.Decimal
	BulkCostYuan      decimal.Decimal
	WasteCostYuan     decimal.Decimal
	RevenueYuan       decimal.Decimal
	GrossMarginYuan   decimal.Decimal
	GrossMarginPct    float64

	PresaleMarginYuan          decimal.Decimal
	PresaleExpectedRevenueYuan decimal.Decimal
	// PresaleExpectedUnitYuan 是该部位的**预期销售单价**(元/kg)—— 加权出来的。
	PresaleExpectedUnitYuan decimal.Decimal

	// DataSource:actual | partial | estimated
	DataSource string
}

// cutAmounts 部位的量与金额(内部累加用)。
type cutAmounts struct {
	kg     decimal.Decimal
	amount decimal.Decimal
}

// loadStocktakeFlags 取某营业日**有没有盘点单**与**盘点口径三态**。
//
// ⚠️ 只打索引列(branch_id + taken_at),不扫全表。
func (s *Service) loadStocktakeFlags(ctx context.Context, branchID string, startOfDay, endOfDay time.Time) (
	hasStocktake bool, dataSource string, err error,
) {
	var nAll, nComplete int64
	if err := s.db.WithContext(ctx).
		Model(&model.PorkCutsStocktake{}).
		Where("branch_id = ? AND taken_at >= ? AND taken_at < ?", branchID, startOfDay, endOfDay).
		Count(&nAll).Error; err != nil {
		return false, "", fmt.Errorf("count stocktake: %w", err)
	}
	if nAll == 0 {
		return false, dataSourceEstimated, nil
	}
	if err := s.db.WithContext(ctx).
		Model(&model.PorkCutsStocktake{}).
		Where("branch_id = ? AND taken_at >= ? AND taken_at < ? AND is_complete = ?",
			branchID, startOfDay, endOfDay, true).
		Count(&nComplete).Error; err != nil {
		return false, "", fmt.Errorf("count complete stocktake: %w", err)
	}
	if nComplete == 0 {
		return true, dataSourcePartial, nil
	}
	return true, dataSourceActual, nil
}

// loadCutOpening 取某营业日的**部位级期初**(kg + 金额)。
//
// # kg 与金额**分开定价**(2026-10-27 修)
//
//	kg 来源:① 上一营业日的部位级成本日快照(cut_cost_daily)
//	        → ② 上一营业日的**实时余额链**日终结余(closingForDay)
//	        → ③ 门店期初结转(sku_stock_openings,按部位录)
//	单价:   ① 快照里的金额(直接用,可以是负数)
//	        → ② cut_cost_states 的部位移动加权单价
//	        → ③ 门店期初结转里**门店给的进价**
//	        → ④ 今日该部位的产出单价(产出金额 ÷ 产出 kg)
//
// ⚠️ **绝不能"有 kg 没金额"**:
//
//	移动加权单价 = 可售金额 ÷ 可售数量 ⇒ 期初 10kg、金额 0 ⇒ 单价被稀释
//	⇒ 成本低估 ⇒ **毛利虚高** —— 正是这次改造要消灭的那个坑,换个位置又冒出来。
//
//	⇒ **上线第一天最危险**:此时 cut_cost_states 还没有历史单价
//	   (生产:真实门店 00/01 只有一条期初结转 ⇒ trotter 9.23kg @8元)
//	   ⇒ 金额必须由 ③/④ 兜住,否则期初按 0 元入账 ⇒ 整条链的毛利虚高。
//
// ⚠️ 第 ③ 步是这次改造的关键:旧引擎把它落到"主 SKU"上 ⇒ 另一个货号账面恒为 0
//   ⇒ 卖出零成本 ⇒ 毛利率 100%。
func (s *Service) loadCutOpening(ctx context.Context, branchID string, bizDate time.Time,
	producedKg, producedAmount map[string]decimal.Decimal,
	histUnit map[string]decimal.Decimal,
) (kg map[string]decimal.Decimal, amount map[string]decimal.Decimal, err error) {
	kg = map[string]decimal.Decimal{}
	amount = map[string]decimal.Decimal{}

	// ---- ① 上一营业日的部位级成本日快照 ----
	prevStart, prevEnd := s.previousBizDayBounds(bizDate)
	var prev []model.CutCostDaily
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date >= ? AND biz_date < ?", branchID, prevStart, prevEnd).
		Find(&prev).Error; err != nil {
		return nil, nil, fmt.Errorf("query cut_cost_daily(prev): %w", err)
	}
	snapKg := map[string]decimal.Decimal{}
	snapAmt := map[string]decimal.Decimal{}
	hasSnap := map[string]bool{}
	for _, r := range prev {
		c := string(r.CutType)
		snapKg[c] = r.ClosingKg
		snapAmt[c] = r.ClosingAmount
		hasSnap[c] = true
	}

	// ---- ② 上一营业日的实时余额链日终结余(部位) ----
	chainKg := map[string]decimal.Decimal{}
	hasChain := map[string]bool{}
	prevCounter, prevCold, ok, cerr := s.closingForDay(ctx, branchID, prevStart)
	if cerr != nil {
		s.pubLogger.Warn("loadCutOpening: 读上一营业日的部位结余失败(只走快照/期初结转)",
			"branch_id", branchID, "biz_date", bizDate.In(s.bizTZOrUTC()).Format("2006-01-02"), "err", cerr)
	} else if ok {
		for cut, v := range prevCold {
			chainKg[cut] = v.Add(prevCounter[cut])
			hasChain[cut] = true
		}
	}

	// ---- ③ 门店期初结转(按部位) ----
	openings, err := s.loadOpenings(ctx, branchID)
	if err != nil {
		return nil, nil, err
	}
	key := bizDate.In(s.bizTZOrUTC()).Format("2006-01-02")

	// 今日产出单价(兜底价):该部位的产出金额 ÷ 产出 kg。
	producedUnit := map[string]decimal.Decimal{}
	for cut, k := range producedKg {
		if !k.IsPositive() {
			continue
		}
		if a := producedAmount[cut]; a.IsPositive() {
			producedUnit[cut] = a.Div(k)
		}
	}

	// ---- 合并:kg 定来源,金额**单独定价** ----
	cuts := map[string]struct{}{}
	for c := range snapKg {
		cuts[c] = struct{}{}
	}
	for c := range chainKg {
		cuts[c] = struct{}{}
	}
	for c := range openings {
		cuts[c] = struct{}{}
	}
	for c := range cuts {
		// 生效日在**今天之后**的期初结转还没开始 ⇒ 不参与(那是给未来那天准备的)。
		op, hasOp := openings[c]
		if hasOp && op.dateKey > key {
			hasOp = false
		}
		switch {
		case hasSnap[c]:
			// 快照是链的权威 ⇒ kg 与金额都直接用(金额可能是负数 = 账面短卖)。
			kg[c] = snapKg[c]
			amount[c] = snapAmt[c]
			continue
		case hasChain[c]:
			kg[c] = chainKg[c]
		case hasOp:
			kg[c] = op.kg
		default:
			continue
		}
		// 定价:历史部位单价 → 门店给的进价 → 今日产出单价。
		unit := decimal.Zero
		switch {
		case histUnit[c].IsPositive():
			unit = histUnit[c]
		case hasOp && op.unitPrice.IsPositive():
			unit = op.unitPrice
		case producedUnit[c].IsPositive():
			unit = producedUnit[c]
		}
		if unit.IsPositive() {
			amount[c] = kg[c].Mul(unit)
			continue
		}
		// ⚠️ 有量无价 ⇒ 成本会被稀释 ⇒ 必须让人知道(别让它悄悄变成 0 成本)。
		if !kg[c].IsZero() {
			s.pubLogger.Warn("loadCutOpening: 期初有量但无价(成本会被稀释),请补录期初进价或盘点",
				"branch_id", branchID, "biz_date", key, "cut_type", c, "opening_kg", kg[c].String())
		}
		amount[c] = decimal.Zero
	}
	return kg, amount, nil
}

// computeCutDailyCosts 算某店某营业日的**部位级**移动加权成本。
//
// 返回 (部位明细, SKU 明细, 成本对账行, 损耗汇总)。
//
// ⚠️ **SKU 明细只承载"卖了多少、卖了多少钱"**:
//   销售 kg / 营收 / 分割产出是 SKU 级的事实;
//   **期初 / 期末 / 成本 / 毛利都是部位口径** ⇒ SKU 行里不返(留 0)。
//   ⇒ 这正是门店要的口径(2026-10-27):
//   「以部位为主(分割仍然是部位的重量),销售、营收等和 SKU 有关的数据分列」。
func (s *Service) computeCutDailyCosts(ctx context.Context, branchID string, day time.Time) (
	[]CutDailyCost, []SkuDailyCost, *ReconciliationRow, *LossSummary, error,
) {
	startOfDay, endOfDay := s.businessDayBounds(day)
	bizDate := day.In(s.bizTZOrUTC())

	// ---- 1. 今日进场的猪 + 成本 ----
	var pigs []model.WholePig
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND arrived_at >= ? AND arrived_at < ?", branchID, startOfDay, endOfDay).
		Find(&pigs).Error; err != nil {
		return nil, nil, nil, nil, fmt.Errorf("query whole_pigs: %w", err)
	}
	pigCost := make(map[string]decimal.Decimal, len(pigs))
	for _, p := range pigs {
		pigCost[p.ID] = p.PurchaseCostYuan
	}

	// ---- 2. 今日产出(分割)→ 部位口径 ----
	producedKg, producedByPigKg, unallocatedPigCost, cuttingLossKg, err :=
		s.computeProducedBySku(ctx, branchID, startOfDay, endOfDay, pigs, pigCost)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	cutOfSku, nameOfSku, err := s.loadSkuCatalog(ctx, branchID, nil)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	// ---- 3. 卖出 / 统收 / 报损(SKU 级读,后面汇总到部位) ----
	soldKg, soldAmount, bulkKg, bulkAmount, wasteKg, cutFromSales, err :=
		s.loadSkuFlows(ctx, branchID, startOfDay, endOfDay)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	// ---- 2b. 入库量兜底(门店没贴标签 ⇒ 没有袋子 ⇒ 不兜底) ----
	//
	// ⚠️ **必须先兜底再汇总到部位**:兜底改的是 SKU 级的 producedKg / producedByPigKg,
	//   汇总之后就没法把兜底量算进部位了。
	//
	// ⚠️ 兜底本身仍是 **SKU 级**(袋子表按货号记)⇒ 兜底完再按部位汇总。
	if err := s.applyProducedFallback(ctx, branchID, bizDate,
		producedKg, producedByPigKg, pigs, soldKg); err != nil {
		return nil, nil, nil, nil, err
	}
	partProducedKg := map[string]decimal.Decimal{}
	for sku, v := range producedKg {
		cut := string(cutOfSku[sku])
		if cut == "" {
			continue
		}
		partProducedKg[cut] = partProducedKg[cut].Add(v)
	}
	// 猪成本按**部位**分摊(逐猪摊:该猪该部位的 kg 占该猪总产出的比例)。
	partProducedAmount := map[string]decimal.Decimal{}
	for pigID, perPig := range producedByPigKg {
		pigTotal := s.pigTotalProducedKg(perPig)
		if !pigTotal.IsPositive() {
			continue
		}
		partKgInPig := map[string]decimal.Decimal{}
		for sku, kg := range perPig {
			cut := string(cutOfSku[sku])
			if cut == "" {
				continue
			}
			partKgInPig[cut] = partKgInPig[cut].Add(kg)
		}
		for cut, kg := range partKgInPig {
			partProducedAmount[cut] = partProducedAmount[cut].
				Add(pigCost[pigID].Mul(kg).Div(pigTotal))
		}
	}

	// ---- 3b. 汇总到部位 ----
	partSoldKg := map[string]decimal.Decimal{}
	partSoldAmount := map[string]decimal.Decimal{}
	partBulkKg := map[string]decimal.Decimal{}
	partBulkAmount := map[string]decimal.Decimal{}
	partWasteKg := map[string]decimal.Decimal{}
	cutOf := func(sku string) string {
		if c := string(cutOfSku[sku]); c != "" {
			return c
		}
		return string(cutFromSales[sku])
	}
	for sku, v := range soldKg {
		partSoldKg[cutOf(sku)] = partSoldKg[cutOf(sku)].Add(v)
	}
	for sku, v := range soldAmount {
		partSoldAmount[cutOf(sku)] = partSoldAmount[cutOf(sku)].Add(v)
	}
	for sku, v := range bulkKg {
		partBulkKg[cutOf(sku)] = partBulkKg[cutOf(sku)].Add(v)
	}
	for sku, v := range bulkAmount {
		partBulkAmount[cutOf(sku)] = partBulkAmount[cutOf(sku)].Add(v)
	}
	for sku, v := range wasteKg {
		partWasteKg[cutOf(sku)] = partWasteKg[cutOf(sku)].Add(v)
	}

	// ---- 4. 期初(部位) ----
	//
	// ⚠️ 部位的历史移动加权单价先取出来:**期初定价**要用它(见 loadCutOpening),
	//   "可售数量为 0 时给短卖补提成本"也要用它。
	var states []model.CutCostState
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Find(&states).Error; err != nil {
		return nil, nil, nil, nil, fmt.Errorf("query cut_cost_states: %w", err)
	}
	histUnit := map[string]decimal.Decimal{}
	todayKey := bizDate.In(s.bizTZOrUTC()).Format("2006-01-02")
	for _, st := range states {
		// ⚠️ **只用"更早的营业日"的状态**(2026-10-27 修:自引用漂移)。
		//
		//	`cut_cost_states` 是"当前成本状态"——今天的快照落库后,它记的就是**今天**的
		//	移动加权单价。若拿它给**今天**的期初定价 ⇒ 今天刚算出来的成本
		//	反过来给今天的期初重新定价 ⇒ 同一份数据被算两遍(且随调用顺序漂移)。
		//
		//	⇒ 只有 `last_biz_date` **早于今天**的状态才能当"上期单价"用。
		if time.Time(st.LastBizDate).In(s.bizTZOrUTC()).Format("2006-01-02") >= todayKey {
			continue
		}
		histUnit[string(st.CutType)] = st.MovingAvgUnitCost
	}
	openKg, openAmount, err := s.loadCutOpening(ctx, branchID, bizDate,
		partProducedKg, partProducedAmount, histUnit)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	// ---- 6. 期末(部位)与盘点口径 ----
	hasStocktake, dataSource, err := s.loadStocktakeFlags(ctx, branchID, startOfDay, endOfDay)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	closeKg := map[string]decimal.Decimal{}
	if hasStocktake {
		counter, cold, ok, derr := s.closingForDay(ctx, branchID, bizDate)
		if derr != nil {
			s.pubLogger.Warn("computeCutDailyCosts: 读取按营业日的部位结余失败(回退账面推演)",
				"branch_id", branchID, "biz_date", bizDate.Format("2006-01-02"), "err", derr)
		} else if ok {
			for cut, v := range cold {
				closeKg[cut] = v.Add(counter[cut])
			}
		}
	}

	// ---- 7. 预售(预期售价):SKU 价 → 部位加权价 ----
	presaleUnits, err := s.loadPresaleUnits(ctx, branchID, bizDate)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	partPresaleUnit := s.cutPresaleUnits(presaleUnits, cutOfSku, soldAmount, soldKg)

	// ---- 8. 逐部位算移动加权 ----
	recon := &ReconciliationRow{}
	out := make([]CutDailyCost, 0, len(partProducedKg))
	cuts := map[string]struct{}{}
	for c := range partProducedKg {
		cuts[c] = struct{}{}
	}
	for c := range partSoldKg {
		cuts[c] = struct{}{}
	}
	for c := range partBulkKg {
		cuts[c] = struct{}{}
	}
	for c := range partWasteKg {
		cuts[c] = struct{}{}
	}
	for c := range openKg {
		cuts[c] = struct{}{}
	}
	for c := range closeKg {
		cuts[c] = struct{}{}
	}
	for c := range presaleUnits {
		// 预售按 SKU 记 ⇒ 它的部位也必须有行(哪怕当期没有销售)。
		if cut := string(cutOfSku[c]); cut != "" {
			cuts[cut] = struct{}{}
		}
	}
	cutList := make([]string, 0, len(cuts))
	for c := range cuts {
		cutList = append(cutList, c)
	}
	sort.Strings(cutList)

	// 部位的历史移动加权单价(上面已查)。
	//
	// ⚠️ 兜底价:该部位今日的产出单价(产出金额 ÷ 产出 kg)——
	//   期初没有历史单价、又没有门店给的进价时,用今天的进价给那批货估值。
	fallbackUnit := func(cut string) decimal.Decimal {
		if histUnit[cut].IsPositive() {
			return histUnit[cut]
		}
		k := partProducedKg[cut]
		if !k.IsPositive() {
			return decimal.Zero
		}
		if a := partProducedAmount[cut]; a.IsPositive() {
			return a.Div(k)
		}
		return decimal.Zero
	}

	for _, c := range cutList {
		cut := model.CutType(c)
		openK := openKg[c]
		openA := openAmount[c]
		prodK := partProducedKg[c]
		prodA := partProducedAmount[c]
		availAmt := openA.Add(prodA)
		availKg := openK.Add(prodK)
		movingAvg := decimal.Zero
		switch {
		case availKg.IsPositive() && availAmt.IsPositive():
			movingAvg = availAmt.Div(availKg)
		case availKg.IsPositive():
			// ⚠️ **有量无价**:可售金额为 0(期初没定价)⇒ 加权单价会算成 0
			//   ⇒ 那批货零成本 ⇒ **毛利虚高**(我们正在消灭的那个坑)。
			//   ⇒ 用兜底价(历史部位单价 / 今日产出单价)计成本。
			movingAvg = fallbackUnit(c)
			if movingAvg.IsPositive() {
				s.pubLogger.Warn("computeCutDailyCosts: 可售金额为 0 但有货,已用兜底单价计成本",
					"branch_id", branchID, "biz_date", bizDate.In(s.bizTZOrUTC()).Format("2006-01-02"),
					"cut_type", c, "avail_kg", availKg.String(), "fallback_unit", movingAvg.String())
			}
		case fallbackUnit(c).IsPositive():
			// ⚠️ 本期既没有期初也没有产出 ⇒ 可售数量为 0 ⇒ 加权单价算不出来。
			//   但"短卖"的那部分货**仍要有成本**(否则虚高毛利)⇒ 用兜底单价。
			movingAvg = fallbackUnit(c)
		}
		expectedCloseKg := availKg.
			Sub(partSoldKg[c]).Sub(partBulkKg[c]).Sub(partWasteKg[c])
		// ⚠️ **不裁 0**:负数 = 账面短卖 ⇒ 必须看得见。
		closeK := expectedCloseKg
		if hasStocktake {
			if v, ok := closeKg[c]; ok {
				closeK = v
			}
		}
		closeAmtBasis := closeK.Mul(movingAvg)
		bulkCost := partBulkKg[c].Mul(movingAvg)
		wasteCost := partWasteKg[c].Mul(movingAvg)
		salesCost := availAmt.Sub(closeAmtBasis).Sub(bulkCost).Sub(wasteCost)
		if salesCost.IsNegative() {
			// 期末实盘 > 可售金额(盘盈)⇒ 成本不能为负,裁到 0 并记日志。
			s.pubLogger.Warn("computeCutDailyCosts: 销售成本为负(盘盈),已裁 0",
				"branch_id", branchID, "biz_date", bizDate.Format("2006-01-02"),
				"cut_type", c, "sales_cost", salesCost.String())
			salesCost = decimal.Zero
		}
		revenue := partSoldAmount[c].Add(partBulkAmount[c])
		// 预期毛利(预售结转):期末结余按**部位预期售价**重估。
		//
		// ⚠️ 判据是"有没有预售结转行",不是"单价是否大于 0":
		//   维护为 **0 元**的货号也要结转(负毛利照做)⇒ 单价 0 一样要重估。
		revaluation := decimal.Zero
		presaleRevenue := decimal.Zero
		presaleUnit := decimal.Zero
		if unit, ok := partPresaleUnit[c]; ok && closeK.IsPositive() {
			presaleUnit = unit
			revaluation = closeK.Mul(unit.Sub(movingAvg))
			presaleRevenue = closeK.Mul(unit)
		}
		gm := revenue.Sub(salesCost).Sub(bulkCost).Sub(wasteCost).Add(revaluation)
		var gmPct float64
		gmBase := revenue.Add(presaleRevenue)
		if !gmBase.IsZero() {
			f, _ := gm.Mul(decimal.NewFromInt(100)).Div(gmBase).Float64()
			gmPct = f
		}
		out = append(out, CutDailyCost{
			BranchID:          branchID,
			CutType:           cut,
			OpeningKg:         openK,
			OpeningAmount:     openA,
			ProducedKg:        prodK,
			ProducedAmount:    prodA,
			SoldKg:            partSoldKg[c],
			SoldAmount:        partSoldAmount[c],
			BulkSoldKg:        partBulkKg[c],
			BulkSoldAmount:    partBulkAmount[c],
			WasteKg:           partWasteKg[c],
			WasteAmount:       partWasteKg[c].Mul(movingAvg),
			ExpectedClosingKg: expectedCloseKg,
			ClosingKg:         closeK,
			ClosingAmount:     closeAmtBasis,
			MovingAvgUnitCost: movingAvg,
			SalesCostYuan:     salesCost,
			BulkCostYuan:      bulkCost,
			WasteCostYuan:     wasteCost,
			RevenueYuan:       revenue,
			GrossMarginYuan:   gm,
			GrossMarginPct:    gmPct,
			PresaleMarginYuan:          revaluation,
			PresaleExpectedRevenueYuan: presaleRevenue,
			PresaleExpectedUnitYuan:    presaleUnit,
			DataSource:        dataSource,
		})
		recon.OpeningInventoryCostYuan = recon.OpeningInventoryCostYuan.Add(openA)
		recon.ProducedCostYuan = recon.ProducedCostYuan.Add(prodA)
		recon.SalesCostYuan = recon.SalesCostYuan.Add(salesCost)
		recon.BulkCostYuan = recon.BulkCostYuan.Add(bulkCost)
		recon.WasteCostYuan = recon.WasteCostYuan.Add(wasteCost)
		recon.ClosingInventoryCostYuan = recon.ClosingInventoryCostYuan.Add(closeAmtBasis)
		recon.RevaluationYuan = recon.RevaluationYuan.Add(revaluation)
	}

	// 对账行:差额 = (期初 + 入库) − (销售 + 统收 + 报损 + 期末)
	//
	// ⚠️ **要 round**:移动加权单价是"金额 ÷ kg"除出来的 ⇒ 尾数 1e-16
	//   ⇒ 显示成 "-0.0000000000000001" 会让人以为**对账不平**。
	recon.PigPurchaseCostYuan = decimal.Zero
	for _, p := range pigs {
		recon.PigPurchaseCostYuan = recon.PigPurchaseCostYuan.Add(p.PurchaseCostYuan)
	}
	recon.UnallocatedPigCostYuan = unallocatedPigCost
	recon.DiffYuan = recon.OpeningInventoryCostYuan.Add(recon.ProducedCostYuan).
		Sub(recon.SalesCostYuan).Sub(recon.BulkCostYuan).
		Sub(recon.WasteCostYuan).Sub(recon.ClosingInventoryCostYuan).
		Round(4)
	recon.CuttingLossKg = cuttingLossKg

	losses, err := s.computeCutOvernightShrink(ctx, branchID, bizDate, out)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	recon.OvernightShrinkKg = losses.OvernightShrinkKg

	// ---- 9. SKU 级明细行(只有销售 / 营收 / 分割产出) ----
	skuRows := buildSkuSalesRows(branchID, cutOfSku, nameOfSku, cutOf, out,
		producedKg, soldKg, soldAmount, bulkKg, bulkAmount, wasteKg)

	return out, skuRows, recon, losses, nil
}

// buildSkuSalesRows 由部位级引擎产出 **SKU 级**明细行。
//
// ⚠️ **只填 SKU 粒度真实存在的量**:销售 kg / 销售金额 / 统收 / 分割产出 / 营收。
//
// ⚠️ **期初 / 期末 / 销售成本 / 毛利一律留 0** —— 它们是**部位口径**:
//   库存是部位的(门店口径)⇒ SKU 粒度的库存是**假的**
//   ⇒ 填一个数进去就是继续制造同一个坑(门店会拿它跟盘点对账)。
//
//   ⇒ 门店要看 SKU 的销售/营收分列(2026-10-27),成本/毛利看部位行。
func buildSkuSalesRows(
	branchID string,
	cutOfSku map[string]model.CutType,
	nameOfSku map[string]string,
	cutOf func(sku string) string,
	cutRows []CutDailyCost,
	producedKg, soldKg, soldAmount, bulkKg, bulkAmount, wasteKg map[string]decimal.Decimal,
) []SkuDailyCost {
	// 部位 → 它的行(取 dataSource / 单价)。
	cutRow := map[string]CutDailyCost{}
	for _, r := range cutRows {
		cutRow[string(r.CutType)] = r
	}
	// SKU 集合:有流水的都算。
	skuSet := map[string]struct{}{}
	for sku := range producedKg {
		skuSet[sku] = struct{}{}
	}
	for sku := range soldKg {
		skuSet[sku] = struct{}{}
	}
	for sku := range soldAmount {
		skuSet[sku] = struct{}{}
	}
	for sku := range bulkKg {
		skuSet[sku] = struct{}{}
	}
	for sku := range wasteKg {
		skuSet[sku] = struct{}{}
	}
	list := make([]string, 0, len(skuSet))
	for sku := range skuSet {
		list = append(list, sku)
	}
	sort.Strings(list)

	out := make([]SkuDailyCost, 0, len(list))
	for _, sku := range list {
		cut := cutOf(sku)
		if cut == "" {
			continue
		}
		row := cutRow[cut]
		revenue := soldAmount[sku].Add(bulkAmount[sku])
		out = append(out, SkuDailyCost{
			BranchID:        branchID,
			CubeProductID:   sku,
			CubeProductName: nameOfSku[sku],
			CutType:         model.CutType(cut),
			ProducedKg:      producedKg[sku],
			SoldKg:          soldKg[sku],
			SoldAmount:      soldAmount[sku],
			BulkSoldKg:      bulkKg[sku],
			BulkSoldAmount:  bulkAmount[sku],
			WasteKg:         wasteKg[sku],
			RevenueYuan:     revenue,
			// ⚠️ 期初/期末/成本/毛利是**部位口径** ⇒ 这里不返(留 0)。
			DataSource: row.DataSource,
		})
	}
	return out
}

// cutPresaleUnits 把**SKU 级**的预期售价加权成**部位级**预期售价。
//
// # 口径(2026-10-27 决议 R4)
//
// 预期售价是**货号级**的(门店按货号填),而期末结余是**部位级**的
// ⇒ 部位预期售价 = Σ(SKU 预期售价 × SKU 权重) / Σ(SKU 权重)。
//
// ⚠️ 权重优先用**当日营收**(SKU 粒度里最可靠的量:销售是实打实的);
//   某货号当日没营收 ⇒ 退回**当日销量**;部位当期完全没动销 ⇒ 用该部位
//   预售货号的**简单平均**(权���无意义时至少给个可解释的数)。
func (s *Service) cutPresaleUnits(
	presaleUnits map[string]decimal.Decimal,
	cutOfSku map[string]model.CutType,
	skuSoldAmount, skuSoldKg map[string]decimal.Decimal,
) map[string]decimal.Decimal {
	if len(presaleUnits) == 0 {
		return map[string]decimal.Decimal{}
	}
	weightOf := func(sku string) decimal.Decimal {
		if v := skuSoldAmount[sku]; v.IsPositive() {
			return v
		}
		return skuSoldKg[sku]
	}
	num := map[string]decimal.Decimal{}
	den := map[string]decimal.Decimal{}
	simpleSum := map[string]decimal.Decimal{}
	simpleCnt := map[string]int{}
	for sku, unit := range presaleUnits {
		cut := string(cutOfSku[sku])
		if cut == "" {
			continue
		}
		w := weightOf(sku)
		num[cut] = num[cut].Add(unit.Mul(w))
		den[cut] = den[cut].Add(w)
		simpleSum[cut] = simpleSum[cut].Add(unit)
		simpleCnt[cut]++
	}
	out := map[string]decimal.Decimal{}
	for cut, d := range den {
		if d.IsPositive() {
			out[cut] = num[cut].Div(d)
			continue
		}
		if simpleCnt[cut] > 0 {
			out[cut] = simpleSum[cut].Div(decimal.NewFromInt(int64(simpleCnt[cut])))
		}
	}
	return out
}

// computeCutOvernightShrink 估"隔夜失重·盘亏"(部位口径)。
//
// 口径:昨日全店 − 今日全店 − (今日产出 − 今日卖出 − 今日报损)
//
// ⚠️ **负数不计入**(2026-10-27):部位期末为负 = **账面短卖**(卖超了库存),
//   那不是"货凭空消失",而是**卖多了** ⇒ 计进"隔夜失重"会把销售问题
//   误报成损耗问题 ⇒ 门店会去查损耗,而不是查销售。
//   ⇒ 负余额(无论是今天的还是昨天的)一律按 0 参与合计,
//     短卖本身由「账面短卖」红字告警暴露。
func (s *Service) computeCutOvernightShrink(ctx context.Context, branchID string, bizDate time.Time,
	rows []CutDailyCost) (*LossSummary, error) {
	out := &LossSummary{ShrinkNote: "期初沿用昨日日终重量,未做隔夜失重修正;实测销量可能被高估。"}
	prevStart, _ := s.previousBizDayBounds(bizDate)
	prevCounter, prevCold, ok, err := s.closingForDay(ctx, branchID, prevStart)
	if err != nil || !ok {
		return out, nil
	}
	prevTotal := decimal.Zero
	for _, v := range prevCold {
		if v.IsPositive() {
			prevTotal = prevTotal.Add(v)
		}
	}
	for _, v := range prevCounter {
		if v.IsPositive() {
			prevTotal = prevTotal.Add(v)
		}
	}
	if prevTotal.IsZero() {
		return out, nil
	}
	curTotal := decimal.Zero
	produced := decimal.Zero
	sold := decimal.Zero
	waste := decimal.Zero
	for _, r := range rows {
		if r.ClosingKg.IsPositive() {
			curTotal = curTotal.Add(r.ClosingKg)
		}
		produced = produced.Add(r.ProducedKg)
		sold = sold.Add(r.SoldKg).Add(r.BulkSoldKg)
		waste = waste.Add(r.WasteKg)
	}
	diff := prevTotal.Sub(curTotal).Sub(produced.Sub(sold.Sub(waste)))
	if diff.IsPositive() {
		out.OvernightShrinkKg = diff
		out.OvernightShrinkPct = 0
		if prevTotal.IsPositive() {
			f, _ := diff.Mul(decimal.NewFromInt(100)).Div(prevTotal).Float64()
			out.OvernightShrinkPct = f
		}
	}
	return out, nil
}
