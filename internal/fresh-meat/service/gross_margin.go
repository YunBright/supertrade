// Package service / gross_margin.go —— 按门店 + 营业日汇总鲜肉毛利。
//
// # 口径(2026-10-23 改:移动加权平均)
//
// **逐 SKU** 用移动加权平均算本期销售成本,再按部位汇总:
//
//	期初 = 昨日日终实盘(全店)× 昨日移动加权单价
//	入库 = 今日分割产出 kg + 其对应的猪成本分摊额
//	移动加权单价 = (期初金额 + 入库金额) / (期初 kg + 入库 kg)
//	期末金额 = 今日日终实盘(全店)× 本期移动加权单价
//	本期销售成本 = 期初金额 + 入库金额 − 期末金额 − 统收成本 − 报损成本
//	本期毛利 = 本期销售收入 − 本期销售成本
//
// 逐 SKU 的计算在 service/moving_average.go(computeSkuDailyCosts)。
//
// # 为什么换口径
//
// 旧口径把"今天卖的钱"按"今天进场的猪"摊成本 ⇒ **今天卖昨天剩的货时成本为 0、
// 毛利率 100%**(今天没进猪 / 今天的猪没出该部位 ⇒ costForCut=0 而 ratio=1)。
// 旧口径里**没有"昨天"的概念**。详见 moving_average.go 的包注释。
//
// # 全店口径
//
// 毛利用**全店**(猪肉台 + 冷库)。位置只影响"可售量"查询,不影响成本:
// 转库只是物理位置变化,成本跟着货走。
//
// # 对账行
//
// 报告带一行 **Reconciliation**:期初 + 入库 == 销售 + 统收 + 报损 + 期末。
// 差额只允许来自**显式化的损耗**(切分损耗 / 隔夜失重·盘亏),两者都在 Losses 里单列。
// ⇒ 毛利准不准一眼可判,不再靠人肉对账。
//
// # data_source
//
//   - actual    当日有 is_complete=true 的盘点单
//   - partial   有盘点单但 is_complete=false
//   - estimated 当日无盘点单
//
// # 退货
//
// OrderStatus='R' 或 qty<0 的行**不计入** revenue / sold / 成本。
//
// 全程不引入 tenant_id。
package service

import (
	"context"
	"fmt"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
)

// GrossMarginReport 单店单日毛利总览。
type GrossMarginReport struct {
	BranchID            string              `json:"branch_id"`
	Date                string              `json:"date"` // YYYY-MM-DD(bizTZ)
	IsStocktakeComplete bool                `json:"is_stocktake_complete"`
	DataSource          string              `json:"data_source"` // actual | partial | estimated
	PerCut              []PerCutGrossMargin `json:"per_cut"`
	// PerSku 是 SKU 级明细 —— **这才是实际的计算单元**
	// (一个部位挂多个 SKU 时,部位行是它们的汇总)。
	PerSku []SkuDailyCost `json:"per_sku"`
	Total  TotalGrossMargin `json:"total"`
	// Reconciliation 成本对账行。
	Reconciliation ReconciliationRow `json:"reconciliation"`
	// Losses 显式化的损耗(以前这些 kg 完全不在任何表里,成本凭空消失)。
	Losses LossSummary `json:"losses"`
	// PromoAllocation 特价的**日终缺口分摊**明细。
	//
	// ⚠️ 归一后的**部位行已经含**分摊进来的特价收入/重量
	// (分摊成功时 promo 桶不再单列,避免重复计收入);
	// 分摊失败(IsEstimated=true)时特价整体留在 promo 桶里,金额重量仍然可见。
	PromoAllocation *PromoAllocation `json:"promo_allocation,omitempty"`
	// Alerts 是**红字告警**(门店口径:「修改值造成负库存时,需要在每日毛利的 alert 上
	// 红字显示」)⇒ 前端直接标红,不要吞掉。
	Alerts      []MarginAlert `json:"alerts,omitempty"`
	// Presale 是**预期毛利(预售结转)**明细(审计日才有;其余日子不返)。
	//
	// ⚠️ 它的金额**已经含在** PerCut / Total 的毛利里(期末结余按预期售价重估)
	//   ⇒ 这一节是"把预期毛利单独列出来给门店看",不是再加一次。
	Presale *PresaleView `json:"presale,omitempty"`
	// PresaleHint 是**悬浮提示**用的数据(前端直接显示,不用自己拼)。
	//
	// 门店口径:「显示全部毛利(含预期毛利),鼠标悬浮时显示含多少预期毛利」
	// ⇒ 页面上的毛利 = Total.GrossMarginYuan(**已含**预期毛利);
	//   悬浮提示 = 本 Hint 里的 Text(含金额)。
	PresaleHint *PresaleHint `json:"presale_hint,omitempty"`
	GeneratedAt time.Time     `json:"generated_at"`
}

// PresaleHint 毛利里的**预期毛利**占比(悬浮提示用)。
type PresaleHint struct {
	// GrossMarginYuan 是本期毛利(**已含**预期毛利)。
	GrossMarginYuan decimal.Decimal `json:"gross_margin_yuan"`
	// PresaleMarginYuan 是其中**预期毛利**的金额(负数 = 预估亏本)。
	PresaleMarginYuan decimal.Decimal `json:"presale_margin_yuan"`
	// ExpectedRevenueYuan 是预售的**预期收入**(不是现金收入)。
	ExpectedRevenueYuan decimal.Decimal `json:"expected_revenue_yuan"`
	// Text 是可直接显示的一句话。
	Text string `json:"text"`
}

// MarginAlert 每日毛利的**红字告警**。
type MarginAlert struct {
	// Level:error(红字)|warning(黄字)。
	Level string `json:"level"`
	// Code:negative_inventory(负库存)|balance_mismatch(余额与按日推导不一致)。
	Code    string `json:"code"`
	Message string `json:"message"`
	// CubeProductID / Kg 便于前端定位到具体 SKU。
	CubeProductID string          `json:"cube_product_id,omitempty"`
	Kg            decimal.Decimal `json:"kg,omitempty"`
}

// 告警级别。
const (
	// MarginAlertLevelError 红字(必须处理)。
	MarginAlertLevelError = "error"
	// MarginAlertLevelWarning 黄字(需要看一眼)。
	MarginAlertLevelWarning = "warning"
)

// 告警码。
const (
	// MarginAlertNegativeInventory 库存为负(负库存)。
	MarginAlertNegativeInventory = "negative_inventory"
	// MarginAlertBalanceMismatch 当前余额与"按营业日推导"的余额不一致。
	MarginAlertBalanceMismatch = "balance_mismatch"
)

// buildMarginAlerts 生成当天的红字/黄字告警。
//
// # ① 负库存(红字)
//
// 门店口径(2026-10-24):「修改值造成负库存时,需要在每日毛利的 alert 上红字显示」
// ⇒ 改/撤搬台流水**不拦**(允许改成负数)⇒ 负数必须在这里显式暴露。
//
// # ② 余额与按日推导不一致(黄字)
//
// 位置表是"当前余额"(增量维护),按营业日推导的快照是"那天的结余"
// ⇒ 补录往日之后两者会分叉 ⇒ 用黄字提示,让人去核对。
func (s *Service) buildMarginAlerts(ctx context.Context, branchID string, day time.Time, rows []SkuDailyCost) []MarginAlert {
	var out []MarginAlert
	counter, cold, err := s.loadSkuPositions(ctx, branchID)
	if err != nil {
		return out
	}
	for _, r := range rows {
		if r.CounterKg.IsNegative() {
			out = append(out, MarginAlert{
				Level:         MarginAlertLevelError,
				Code:          MarginAlertNegativeInventory,
				Message:       fmt.Sprintf("%s 柜台余额为负 %s kg", r.CubeProductID, r.CounterKg.String()),
				CubeProductID: r.CubeProductID,
				Kg:            r.CounterKg,
			})
		}
		if r.ColdStoreKg.IsNegative() {
			out = append(out, MarginAlert{
				Level:         MarginAlertLevelError,
				Code:          MarginAlertNegativeInventory,
				Message:       fmt.Sprintf("%s 冷库余额为负 %s kg", r.CubeProductID, r.ColdStoreKg.String()),
				CubeProductID: r.CubeProductID,
				Kg:            r.ColdStoreKg,
			})
		}
	}
	// ② 当前余额 vs 按营业日推导的结余(有当天快照时才可比)。
	snapCounter, snapCold, ok, err := s.closingForDay(ctx, branchID, day)
	if err != nil || !ok {
		return out
	}
	for sku, v := range snapCold {
		live := cold[sku].Add(counter[sku])
		derived := v.Add(snapCounter[sku])
		diff := derived.Sub(live)
		if diff.IsZero() {
			continue
		}
		out = append(out, MarginAlert{
			Level:         MarginAlertLevelWarning,
			Code:          MarginAlertBalanceMismatch,
			Message: fmt.Sprintf("%s 当前余额 %s kg 与按营业日推导的结余 %s kg 不一致(差 %s kg)",
				sku, live.String(), derived.String(), diff.String()),
			CubeProductID: sku,
			Kg:            diff,
		})
	}
	return out
}

// PerCutGrossMargin 单部位的毛利明细(由其下各 SKU 汇总)。
type PerCutGrossMargin struct {
	CutType           model.CutType   `json:"cut_type"`
	OpeningKg         decimal.Decimal `json:"opening_kg"`
	PurchasedKg       decimal.Decimal `json:"purchased_kg"` // 本期入库 kg(语义同旧:进货)
	SoldKg            decimal.Decimal `json:"sold_kg"`
	BulkSoldKg        decimal.Decimal `json:"bulk_sold_kg"`
	WasteKg           decimal.Decimal `json:"waste_kg"`
	ActualRemainKg    decimal.Decimal `json:"actual_remain_kg,omitempty"`
	ExpectedRemainKg  decimal.Decimal `json:"expected_remain_kg"`
	VarianceKg        decimal.Decimal `json:"variance_kg,omitempty"`
	RevenueYuan       decimal.Decimal `json:"revenue_yuan"`
	// ⚠️ 改名(2026-10-23):旧字段 `purchase_cost_yuan` 是"按消耗比例摊销的猪采购成本";
	// 新字段是**本期销售成本**(移动加权结转)—— 两者语义不同,不是同一个数。
	// 旧的"摊销采购成本"在"今天没进猪的部位今天在卖"时会算成 0 ⇒ 毛利虚高。
	SalesCostYuan     decimal.Decimal `json:"sales_cost_yuan"`
	// BulkCostYuan 是**统收成本**(卖给酒店的大宗品项的结转成本)。
	//
	// ⚠️ 2026-10-23 增:原来它没进毛利,导致"卖板油给酒店"被算成**零成本收入**
	// (毛利率 100%)—— 实测:卖 10kg 收 60 元、成本 1000 元,毛利显示 +60,
	// 真实应是 **−940**。现在它显式成列,毛利 = 收入 − 销售成本 − 统收成本 − 报损成本。
	BulkCostYuan   decimal.Decimal `json:"bulk_cost_yuan"`
	// WasteCostYuan 是**报损成本**(扔掉的货的成本)。
	//
	// ⚠️ 同上:原来报损的成本**凭空消失**(不进损益)⇒ 报损不体现为损失。
	WasteCostYuan    decimal.Decimal `json:"waste_cost_yuan"`
	GrossMarginYuan   decimal.Decimal `json:"gross_margin_yuan"`
	GrossMarginPct    float64         `json:"gross_margin_pct"`
	// PresaleMarginYuan 是该部位的**预期毛利(预售结转)**金额。
	//
	// ⚠️ 它**已经含在 GrossMarginYuan 里**(审计日的结余按预期售价重估),
	//   这一列只是把它**单独列出来**给门店看。
	// ⚠️ 它是**预期值**(没有现金进来),不是"多卖出来的钱"。
	PresaleMarginYuan decimal.Decimal `json:"presale_margin_yuan,omitempty"`
	// PresaleExpectedRevenueYuan 是预售的**预期收入**(同样不是真实现金收入)。
	PresaleExpectedRevenueYuan decimal.Decimal `json:"presale_expected_revenue_yuan,omitempty"`
	// MovingAvgUnitCost 是该部位的加权平均成本(元/kg)。
	MovingAvgUnitCost decimal.Decimal `json:"moving_avg_unit_cost"`
}

// TotalGrossMargin 全店汇总。
type TotalGrossMargin struct {
	RevenueYuan   decimal.Decimal `json:"revenue_yuan"`
	SalesCostYuan decimal.Decimal `json:"sales_cost_yuan"`
	// BulkCostYuan / WasteCostYuan 与部位行同义(统收成本 / 报损成本)。
	BulkCostYuan     decimal.Decimal `json:"bulk_cost_yuan"`
	WasteCostYuan    decimal.Decimal `json:"waste_cost_yuan"`
	GrossMarginYuan  decimal.Decimal `json:"gross_margin_yuan"`
	GrossMarginPct   float64         `json:"gross_margin_pct"`
	// PresaleMarginYuan / PresaleExpectedRevenueYuan 与部位行同义(预期毛利结转)。
	PresaleMarginYuan          decimal.Decimal `json:"presale_margin_yuan,omitempty"`
	PresaleExpectedRevenueYuan decimal.Decimal `json:"presale_expected_revenue_yuan,omitempty"`
}

// ComputeGrossMargin 按 (branchID, day) 算毛利。
//
// day 是任一时刻;取 day 在 bizTZ 下的日期作为营业日。
func (s *Service) ComputeGrossMargin(ctx context.Context, branchID string, day time.Time) (*GrossMarginReport, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	rows, recon, losses, err := s.computeSkuDailyCosts(ctx, branchID, day)
	if err != nil {
		return nil, err
	}
	promoAlloc := allocatePromo(rows)

	perCut, total := rollUpPerCut(rows, promoAlloc)

	dataSource := "estimated"
	isComplete := false
	for _, r := range rows {
		if r.DataSource == "actual" {
			dataSource = "actual"
			isComplete = true
			break
		}
		if r.DataSource == "partial" && dataSource != "actual" {
			dataSource = "partial"
		}
	}

	// 预期毛利(预售结转):只有做过结转的营业日才有 ⇒ 没有就不挂这一节。
	var presale *PresaleView
	if pv, err := s.ListPresale(ctx, branchID, day); err == nil && len(pv.Rows) > 0 {
		presale = pv
	}
	// 悬浮提示:毛利里的预期毛利占比。
	var hint *PresaleHint
	if presale != nil && !total.PresaleMarginYuan.IsZero() {
		hint = &PresaleHint{
			GrossMarginYuan:       total.GrossMarginYuan,
			PresaleMarginYuan:     total.PresaleMarginYuan,
			ExpectedRevenueYuan:   total.PresaleExpectedRevenueYuan,
			Text: fmt.Sprintf("毛利 %s 元中含预期毛利 %s 元(结余按当前售价结转;实际售价与预期售价的差额落在下一期)",
				total.GrossMarginYuan.String(), total.PresaleMarginYuan.String()),
		}
	}

	return &GrossMarginReport{
		BranchID:            branchID,
		Date:                day.In(s.bizTZOrUTC()).Format("2006-01-02"),
		IsStocktakeComplete: isComplete,
		DataSource:          dataSource,
		PerCut:              perCut,
		PerSku:              rows,
		Total:               total,
		Reconciliation:      *recon,
		Losses:              *losses,
		PromoAllocation:     promoAlloc,
		Alerts:              s.buildMarginAlerts(ctx, branchID, day, rows),
		Presale:             presale,
		PresaleHint:         hint,
		GeneratedAt:         s.now(),
	}, nil
}

// splitCutClosing 把**按部位**的日终结余拆成**按 SKU**的内部视图。
//
// # 为什么
//
// 门店口径(2026-10-24):「这个模块只管部位数据,不管 SKU;SKU 映射只是为了聚合
//   销售、数量等数据到其部位」⇒ **库存/结余是部位的**。
// 但移动加权引擎的成本计算单元仍是 SKU(每个 SKU 的加权单价不同)
// ⇒ 引擎内部需要一个 SKU 级的期末量。
//
// # 口径保证
//
// ⇒ **部位的总额精确**(各 SKU 之和 = 部位值)⇒ 每日毛利(按部位)完全不受影响;
// ⇒ SKU 级只是**内部拆分**:按该部位各 SKU 的**当前冷库余额**占比分摊,
//   余额不足的份额给该部位的**主 SKU**(确定性)。
//
// ⚠️ 门店只看点位的毛利 ⇒ SKU 级的这层拆分只是过渡,不影响对外口径。
func (s *Service) splitCutClosing(ctx context.Context, branchID string,
	cutCounter, cutCold map[string]decimal.Decimal) (counter, cold map[string]decimal.Decimal) {
	counter = map[string]decimal.Decimal{}
	cold = map[string]decimal.Decimal{}
	cutToSku, err := s.primarySkuPerCut(ctx, branchID)
	if err != nil {
		return counter, cold
	}
	_, liveCold, err := s.loadSkuPositions(ctx, branchID)
	if err != nil {
		return counter, cold
	}
	for cut, v := range cutCold {
		primary := cutToSku[model.CutType(cut)]
		if primary == "" {
			continue
		}
		// 该部位各 SKU 的当前冷库余额之和(占比的分母)。
		var total decimal.Decimal
		for c, sku := range cutToSku {
			if c == model.CutType(cut) {
				total = total.Add(liveCold[sku])
			}
		}
		if !total.IsPositive() {
			cold[primary] = cold[primary].Add(v)
			continue
		}
		for c, sku := range cutToSku {
			if c != model.CutType(cut) {
				continue
			}
			cold[sku] = cold[sku].Add(v.Mul(liveCold[sku]).Div(total))
		}
	}
	for cut, v := range cutCounter {
		primary := cutToSku[model.CutType(cut)]
		if primary == "" {
			continue
		}
		counter[primary] = counter[primary].Add(v)
	}
	return counter, cold
}

// rollUpPerCut 把 SKU 级明细汇总到部位级 + 全店。
//
// promoAlloc 是特价分摊结果:
//   - **分摊成功** ⇒ 部位行加上分摊进来的特价 kg/金额/成本,
//     且**不再单列 promo 桶**(否则同一笔收入被算两次);
//   - **分摊失败** ⇒ promo 桶原样保留(金额重量仍然可见,只是没分到部位)。
func rollUpPerCut(rows []SkuDailyCost, promoAlloc *PromoAllocation) ([]PerCutGrossMargin, TotalGrossMargin) {
	allocByCut := promoAllocByCut(promoAlloc)
	allocated := promoAlloc != nil && !promoAlloc.IsEstimated && len(promoAlloc.Allocated) > 0

	type agg struct {
		openingKg, producedKg, soldKg, bulkKg, wasteKg decimal.Decimal
		expectedKg, actualKg                           decimal.Decimal
		revenue, salesCost                             decimal.Decimal
		bulkCost, wasteCost                            decimal.Decimal
		availAmount, availKg                           decimal.Decimal
		presaleMargin, presaleRevenue                  decimal.Decimal
	}
	byCut := map[model.CutType]*agg{}
	get := func(c model.CutType) *agg {
		if byCut[c] == nil {
			byCut[c] = &agg{}
		}
		return byCut[c]
	}

	for _, r := range rows {
		if allocated && r.CutType == model.CutPromo {
			// 已分摊到各部位 ⇒ 这里不再计(否则重复)。
			continue
		}
		a := get(r.CutType)
		a.openingKg = a.openingKg.Add(r.OpeningKg)
		a.producedKg = a.producedKg.Add(r.ProducedKg)
		a.soldKg = a.soldKg.Add(r.SoldKg)
		a.bulkKg = a.bulkKg.Add(r.BulkSoldKg)
		a.wasteKg = a.wasteKg.Add(r.WasteKg)
		a.expectedKg = a.expectedKg.Add(r.ExpectedClosingKg)
		a.actualKg = a.actualKg.Add(r.ClosingKg)
		a.revenue = a.revenue.Add(r.RevenueYuan)
		a.salesCost = a.salesCost.Add(r.SalesCostYuan)
		// ⚠️ 统收成本 / 报损成本必须一起汇总 —— 少了它们,
		// 部位行与全店合计的毛利会把"卖板油"和"报损"算成零成本收入。
		a.bulkCost = a.bulkCost.Add(r.BulkCostYuan)
		a.wasteCost = a.wasteCost.Add(r.WasteCostYuan)
		a.availAmount = a.availAmount.Add(r.OpeningAmount).Add(r.ProducedAmount)
		a.availKg = a.availKg.Add(r.OpeningKg).Add(r.ProducedKg)
		// 预期毛利(预售结转):已经含在 r.GrossMarginYuan 里,这里只做**单列**。
		a.presaleMargin = a.presaleMargin.Add(r.PresaleMarginYuan)
		a.presaleRevenue = a.presaleRevenue.Add(r.PresaleExpectedRevenueYuan)
	}

	perCut := make([]PerCutGrossMargin, 0, len(byCut))
	var tot TotalGrossMargin
	for cut, a := range byCut {
		movingAvg := decimal.Zero
		if a.availKg.IsPositive() {
			movingAvg = a.availAmount.Div(a.availKg)
		}
		revenue := a.revenue
		soldKg := a.soldKg
		salesCost := a.salesCost
		bulkCost := a.bulkCost
		wasteCost := a.wasteCost
		// 分摊进来的特价:重量 + 收入 + 成本(成本 = kg × 该部位移动加权单价)
		if pa, ok := allocByCut[cut]; ok {
			soldKg = soldKg.Add(pa.PromoKg)
			revenue = revenue.Add(pa.PromoAmount)
			salesCost = salesCost.Add(pa.PromoKg.Mul(movingAvg))
		}
		// 毛利 = 收入 − 销售成本 − 统收成本 − 报损成本 + 预期毛利(预售结转的重估)
		// (⚠️ 少减后两项 ⇒ 大宗销售/报损被算成零成本收入 ⇒ 毛利虚高)
		//
		// ⚠️ 预售毛利**必须加上**:它已经计进了 SKU 行的毛利,
		//   但销售成本里**没有**它(重估是 accrual,不是成本)⇒ 不加就丢了。
		gm := revenue.Sub(salesCost).Sub(bulkCost).Sub(wasteCost).Add(a.presaleMargin)
		var gmPct float64
		// ⚠️ 有预售结转时,毛利里含**预期收入**那一块 ⇒ 分母也要含它
		//   (否则百分比虚高,甚至 > 100%)。
		gmBase := revenue.Add(a.presaleRevenue)
		if !gmBase.IsZero() {
			f, _ := gm.Mul(decimal.NewFromInt(100)).Div(gmBase).Float64()
			gmPct = f
		}
		row := PerCutGrossMargin{
			CutType:           cut,
			OpeningKg:         a.openingKg,
			PurchasedKg:       a.producedKg,
			SoldKg:            soldKg,
			BulkSoldKg:        a.bulkKg,
			WasteKg:           a.wasteKg,
			ExpectedRemainKg:  a.expectedKg,
			RevenueYuan:       revenue,
			SalesCostYuan:     salesCost,
			BulkCostYuan:      bulkCost,
			WasteCostYuan:     wasteCost,
			GrossMarginYuan:   gm,
			GrossMarginPct:    gmPct,
			PresaleMarginYuan: a.presaleMargin,
			PresaleExpectedRevenueYuan: a.presaleRevenue,
			MovingAvgUnitCost: movingAvg,
		}
		if !a.actualKg.IsZero() {
			row.ActualRemainKg = a.actualKg
			row.VarianceKg = a.actualKg.Sub(a.expectedKg)
		}
		perCut = append(perCut, row)

		tot.RevenueYuan = tot.RevenueYuan.Add(revenue)
		tot.SalesCostYuan = tot.SalesCostYuan.Add(salesCost)
		tot.BulkCostYuan = tot.BulkCostYuan.Add(bulkCost)
		tot.WasteCostYuan = tot.WasteCostYuan.Add(wasteCost)
		tot.GrossMarginYuan = tot.GrossMarginYuan.Add(gm)
		tot.PresaleMarginYuan = tot.PresaleMarginYuan.Add(a.presaleMargin)
		tot.PresaleExpectedRevenueYuan = tot.PresaleExpectedRevenueYuan.Add(a.presaleRevenue)
	}
	if !tot.RevenueYuan.Add(tot.PresaleExpectedRevenueYuan).IsZero() {
		f, _ := tot.GrossMarginYuan.Mul(decimal.NewFromInt(100)).
			Div(tot.RevenueYuan.Add(tot.PresaleExpectedRevenueYuan)).Float64()
		tot.GrossMarginPct = f
	}

	// 排序 by cut_type 字符串(输出稳定,便于 e2e 断言)。
	sortPerCut(perCut)
	return perCut, tot
}

// sortPerCut 按 cut_type 字符串升序(输出稳定,便于 e2e 断言)。
func sortPerCut(rows []PerCutGrossMargin) {
	for i := 1; i < len(rows); i++ {
		j := i
		for j > 0 && string(rows[j-1].CutType) > string(rows[j].CutType) {
			rows[j-1], rows[j] = rows[j], rows[j-1]
			j--
		}
	}
}
