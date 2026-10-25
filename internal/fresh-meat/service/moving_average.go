// Package service / moving_average.go —— SKU 级移动加权平均成本核算。
//
// # 为什么换口径(2026-10-23)
//
// 旧口径(`gross_margin.go` 的历史实现)把**今天卖的钱**按**今天进场的猪**摊成本:
//
//	pigs := 当日进场的猪
//	costForCut[cut] = Σ pig(今天的猪)(pig_cost × 该猪该部位占比)
//	case purch.IsZero(): ratio = 1        ← 今天没产出该部位 ⇒ 比例按 100%
//	consumedCost := costForCut[cut].Mul(ratio)   ← 但 costForCut = 0
//
// ⇒ **今天卖昨天剩的五花肉(今天没进猪 / 今天的猪没出五花)= 成本 0、毛利率 100%。**
// 不是偶发 —— 只要店里在卖隔夜货就每天都发生。而且旧口径里**没有"昨天"的概念**。
//
// # 新口径
//
// 零售会计的**移动加权平均**,逐 SKU 独立算:
//
//	期初数量 Q0 = 昨日日终实盘(全店)          ← 自动取,零人工
//	期初金额 V0 = Q0 × 昨日该 SKU 的移动加权单价
//	本期入库 Q1 = 今日分割产出到该 SKU 的 kg
//	本期入库 V1 = Σ(今日产出该 SKU 的每头猪的成本分摊额)
//	可售总金额   = V0 + V1
//	可售总数量   = Q0 + Q1
//	本期移动加权单价 = 可售总金额 / 可售总数量
//	期末数量 Q2 = 今日日终实盘(全店)
//	期末金额 V2 = Q2 × 本期移动加权单价
//	本期销售成本 = 可售总金额 − V2 − 统收成本 − 报损成本
//	本期毛利     = 本期销售收入 − 本期销售成本
//
// # 全店口径
//
// 毛利用**全店**(猪肉台 + 冷库)。位置只影响"可售量"查询,不影响成本:
// 转库只是物理位置变化,**成本跟着货走**。
//
// # 不变量(对账行)
//
//	Σ 各 SKU 销售成本 + 统收成本 + 报损成本 + 期末存货金额
//	  == 期初存货金额 + 本期入库金额
//
// 差额只允许来自**显式化的损耗**(切分损耗 / 隔夜失重·盘亏),报表会单列。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// SkuDailyCost 单 SKU 的当日移动加权成本明细(对外 JSON)。
type SkuDailyCost struct {
	BranchID        string          `json:"branch_id"`
	CubeProductID   string          `json:"cube_product_id"`
	CubeProductName string          `json:"cube_product_name,omitempty"`
	CutType         model.CutType   `json:"cut_type"`

	OpeningKg     decimal.Decimal `json:"opening_kg"`
	OpeningAmount decimal.Decimal `json:"opening_amount"`
	ProducedKg    decimal.Decimal `json:"produced_kg"`
	ProducedAmount decimal.Decimal `json:"produced_amount"`
	SoldKg        decimal.Decimal `json:"sold_kg"`
	SoldAmount    decimal.Decimal `json:"sold_amount"`
	BulkSoldKg    decimal.Decimal `json:"bulk_sold_kg"`
	BulkSoldAmount decimal.Decimal `json:"bulk_sold_amount"`
	WasteKg       decimal.Decimal `json:"waste_kg"`
	WasteAmount   decimal.Decimal `json:"waste_amount"`
	// ExpectedClosingKg 是**账面应有**期末量 = 期初 + 入库 − 卖出 − 统收 − 报损。
	// 它不依赖盘点单 ⇒ 盘点提交前就能算(盘点单的 expected 就是它)。
	ExpectedClosingKg decimal.Decimal `json:"expected_closing_kg"`
	ClosingKg         decimal.Decimal `json:"closing_kg"`
	ClosingAmount     decimal.Decimal `json:"closing_amount"`

	MovingAvgUnitCost decimal.Decimal `json:"moving_avg_unit_cost"`
	SalesCostYuan     decimal.Decimal `json:"sales_cost_yuan"`
	// BulkCostYuan / WasteCostYuan 是**统收成本** / **报损成本**。
	//
	// ⚠️ 2026-10-23 增:它们原先没被单独带出来,导致部位行/全店合计在汇总时
	// 把这两块成本**漏掉** ⇒ "卖板油给酒店"和"报损"都成了零成本收入(毛利率虚高)。
	BulkCostYuan  decimal.Decimal `json:"bulk_cost_yuan"`
	WasteCostYuan decimal.Decimal `json:"waste_cost_yuan"`
	RevenueYuan   decimal.Decimal `json:"revenue_yuan"`
	GrossMarginYuan decimal.Decimal `json:"gross_margin_yuan"`
	GrossMarginPct  float64         `json:"gross_margin_pct"`

	// CounterKg / ColdStoreKg 是期末两位置分布(全店 = 两者之和)。
	CounterKg   decimal.Decimal `json:"counter_kg"`
	ColdStoreKg decimal.Decimal `json:"cold_store_kg"`

	// PresaleMarginYuan 是**预期毛利(预售结转)**金额:
	// 审计日把期末结余按**预期销售单价**重估,重估差额(预期收入 − 结余成本)
	// 计入本期毛利 ⇒ 这一列就是它。
	//
	// ⚠️ 只有做过预售结转的营业日才有值;其余日子恒为 0(行为不变)。
	// ⚠️ 它是**预期值**(没有现金进来),不是"多卖出来的钱"。
	PresaleMarginYuan decimal.Decimal `json:"presale_margin_yuan"`
	// PresaleExpectedRevenueYuan 是预售的**预期收入**(同样不是真实现金收入)。
	PresaleExpectedRevenueYuan decimal.Decimal `json:"presale_expected_revenue_yuan,omitempty"`
	// PresaleExpectedUnitYuan 是这批结余的**预期销售单价**(下一期期初单价就是它)。
	PresaleExpectedUnitYuan decimal.Decimal `json:"presale_expected_unit_yuan,omitempty"`

	// DataSource:actual | partial | estimated
	DataSource string `json:"data_source"`
}

// ReconciliationRow 成本对账行 —— **整猪/整店毛利准不准,一眼可判**。
//
// 恒等式:期初存货金额 + 本期入库金额 == 销售成本 + 统收成本 + 报损成本 + 期末存货金额
//
// DiffYuan 是差额。正常应为 0;不为 0 说明有**未显式化**的损耗
// (切分损耗 / 隔夜失重 / 盘亏),报表会把它们单列,而不是让它们凭空消失。
type ReconciliationRow struct {
	OpeningInventoryCostYuan decimal.Decimal `json:"opening_inventory_cost_yuan"`
	ProducedCostYuan         decimal.Decimal `json:"produced_cost_yuan"`
	SalesCostYuan            decimal.Decimal `json:"sales_cost_yuan"`
	BulkCostYuan             decimal.Decimal `json:"bulk_cost_yuan"`
	WasteCostYuan            decimal.Decimal `json:"waste_cost_yuan"`
	ClosingInventoryCostYuan decimal.Decimal `json:"closing_inventory_cost_yuan"`
	// PigPurchaseCostYuan 是今日进场猪的采购成本合计。
	PigPurchaseCostYuan decimal.Decimal `json:"pig_purchase_cost_yuan"`
	// UnallocatedPigCostYuan 是"今日进场但没有任何产出数据、因而摊不出去"的猪成本。
	UnallocatedPigCostYuan decimal.Decimal `json:"unallocated_pig_cost_yuan"`
	// DiffYuan = (期初 + 入库) − (销售 + 统收 + 报损 + 期末);应为 0。
	DiffYuan decimal.Decimal `json:"diff_yuan"`
	// RevaluationYuan 是**期末结存重估**(2026-10-24 预售结转)。
	//
	// 审计日把结余按**预期销售单价**估值 ⇒ 期末存货金额比成本口径大出差额
	// ⇒ 对账恒等式要把它单列,否则差额行会凭空多出这一笔。
	//
	// ⚠️ 它是**预期值**(不是真实现金),所以单列而不是塞进"期末存货金额"。
	RevaluationYuan decimal.Decimal `json:"revaluation_yuan"`
	// CuttingLossKg 是切分损耗 = 进猪毛重 − 产出 kg − 副产 kg(不含成本,成本已摊进产出)。
	CuttingLossKg decimal.Decimal `json:"cutting_loss_kg"`
	// OvernightShrinkKg 是隔夜失重·盘亏 = 昨日全店 − 今日全店实盘 − 今日净变动。
	OvernightShrinkKg decimal.Decimal `json:"overnight_shrink_kg"`
}

// LossSummary 损耗汇总(以前这些 kg 完全不在任何表里,成本凭空消失)。
type LossSummary struct {
	CuttingLossKg        decimal.Decimal `json:"cutting_loss_kg"`
	OvernightShrinkKg    decimal.Decimal `json:"overnight_shrink_kg"`
	OvernightShrinkPct   float64         `json:"overnight_shrink_pct"`
	ShrinkNote           string          `json:"shrink_note,omitempty"`
}

// computeSkuDailyCosts 算某店某营业日的 SKU 级移动加权成本。
//
// 返回按 cube_product_id 升序的明细(输出稳定,便于断言)。
func (s *Service) computeSkuDailyCosts(ctx context.Context, branchID string, day time.Time) ([]SkuDailyCost, *ReconciliationRow, *LossSummary, error) {
	if branchID == "" {
		return nil, nil, nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	startOfDay, endOfDay := s.businessDayBounds(day)
	bizDate := day.In(s.bizTZOrUTC())

	// ---- 1. 今日进场的猪 + 它们的成本 ----
	var pigs []model.WholePig
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND arrived_at >= ? AND arrived_at < ?", branchID, startOfDay, endOfDay).
		Find(&pigs).Error; err != nil {
		return nil, nil, nil, fmt.Errorf("query whole_pigs: %w", err)
	}
	pigCost := make(map[string]decimal.Decimal, len(pigs))
	for _, p := range pigs {
		pigCost[p.ID] = p.PurchaseCostYuan
	}

	// ---- 2. 今日产出: pig_cuts(实录)+ LLM 兜底,归到 SKU ----
	producedKg, producedByPigKg, unallocatedPigCost, cuttingLossKg, err :=
		s.computeProducedBySku(ctx, branchID, startOfDay, endOfDay, pigs, pigCost)
	if err != nil {
		return nil, nil, nil, err
	}
	// ---- 3. 卖出 / 统收 / 报损 ----
	soldKg, soldAmount, bulkKg, bulkAmount, wasteKg, cutFromSales, err :=
		s.loadSkuFlows(ctx, branchID, startOfDay, endOfDay)
	if err != nil {
		return nil, nil, nil, err
	}

	// ---- 3b. 入库量兜底:没有分割补录时,用"销售 + 盘点结转 − 搬上台"倒推 ----
	//
	// ⚠️ 这三个量里的"盘点结转"与"搬上台"来自**袋子表**(标签号),
	// 所以门店**贴标签**是这条兜底能生效的前提。
	if err := s.applyProducedFallback(ctx, branchID, bizDate,
		producedKg, producedByPigKg, pigs, soldKg); err != nil {
		return nil, nil, nil, err
	}

	// ---- 4. 期初(全店)与期末(全店) ----
	openingKg, openingAmount, err := s.loadSkuOpening(ctx, branchID, bizDate)
	if err != nil {
		return nil, nil, nil, err
	}
	// ---- 4b. 预期毛利(预售结转)的**期末重估** ----
	//
	// 审计日(D)做过预售结转 ⇒ 该日的结余按**预期销售单价**估值:
	//
	//	期末金额(重估后) = 结余 kg × 预期销售单价
	//	重估差额         = 结余 kg × (预期售价 − 成本口径单价) = 预售毛利
	//
	// ⇒ 本期毛利自动含进这笔预期毛利(销售成本被"减"掉了这一块);
	// ⇒ 下一期的期初金额从上一日的 closing_amount 读 ⇒ 自动按预期售价进来
	//    (链式一致,不需要额外旁路)。
	presaleUnits, err := s.loadPresaleUnits(ctx, branchID, bizDate)
	if err != nil {
		return nil, nil, nil, err
	}
	_, coldKg, hasStocktake, dataSource, err := s.loadSkuCounterAndCold(ctx, branchID, startOfDay, endOfDay)
	if err != nil {
		return nil, nil, nil, err
	}
	// ---- 5. 位置分布(期末)----
	//
	// ⚠️ 2026-10-24:有盘点 ⇒ 期末用**按营业日推导的结余快照**(柜台实盘 + 冷库余额)
	// (sku_stock_daily)⇒ 补录往日的盘点 ⇒ 那天的期末是对的,不是"今天的余额"。
	// 无盘点 ⇒ 用账面位置余额(与原来一致)。
	//
	// ⚠️ 与上面的 counterActualKg 不同:那是账面柜台余额,
	// 这里是**期末位置分布**(报表要展示的是期末结余,不是当前余额)。
	counterKg, coldKg, err := s.loadSkuPositions(ctx, branchID)
	if err != nil {
		return nil, nil, nil, err
	}
	if hasStocktake {
		// ⚠️ 结余是**按部位**算的(门店口径:「这个模块只管部位数据」)
		// ⇒ 这里拿到的是部位的日终冷库 ⇒ 内部再按 SKU 拆开(见 splitCutClosing)。
		cutCounter, cutCold, ok, derr := s.closingForDay(ctx, branchID, bizDate)
		if derr != nil {
			s.pubLogger.Warn("computeSkuDailyCosts: 读取按营业日的结余失败(回退账面余额)",
				"branch_id", branchID, "biz_date", bizDate.Format("2006-01-02"), "err", derr)
		} else if ok {
			counterKg, coldKg = s.splitCutClosing(ctx, branchID, cutCounter, cutCold)
		}
	}

	// ---- 6. 汇总 SKU 集合 ----
	skuSet := map[string]struct{}{}
	add := func(m map[string]decimal.Decimal) {
		for k, v := range m {
			if !v.IsZero() {
				skuSet[k] = struct{}{}
			}
		}
	}
	add(producedKg)
	add(soldKg)
	add(bulkKg)
	add(wasteKg)
	add(openingKg)
	add(counterKg)
	add(coldKg)

	cutOfSku, nameOfSku, err := s.loadSkuCatalog(ctx, branchID, skuSet)
	if err != nil {
		return nil, nil, nil, err
	}
	// 没有部位映射的 SKU(如特价档)⇒ 用销售行自带的 cut_type 兜底,
	// 否则这类销售会落进一个**空 cut_type 的桶**(报表上"无名桶")。
	for sku, cut := range cutFromSales {
		if cutOfSku[sku] == "" {
			cutOfSku[sku] = cut
		}
	}

	// ---- 7. 逐 SKU 算移动加权 ----
	out := make([]SkuDailyCost, 0, len(skuSet))
	recon := &ReconciliationRow{}
	for sku := range skuSet {
		openKg := openingKg[sku]
		openAmt := openingAmount[sku]
		prodKg := producedKg[sku]
		prodAmt := decimal.Zero
		// 本期入库金额 = 该 SKU 的产出 kg 占"今日总产出 kg"的比例 × 今日猪成本合计。
		//
		// ⚠️ 逐猪摊(而不是全店池化):一头猪的成本只摊到它自己产出的部位上,
		// 否则"这头猪的五花成本"会串到别的猪的产出上 ⇒ 整猪毛利失真。
		if totalProducedKg := s.totalProducedKg(producedByPigKg); totalProducedKg.IsPositive() {
			// 该 SKU 的成本 = Σ pig (pig_cost × sku_kg_in_pig / pig_total_produced_kg)
			for pigID, perPig := range producedByPigKg {
				kgInPig := perPig[sku]
				if kgInPig.IsZero() {
					continue
				}
				pigTotal := s.pigTotalProducedKg(perPig)
				if !pigTotal.IsPositive() {
					continue
				}
				prodAmt = prodAmt.Add(pigCost[pigID].Mul(kgInPig).Div(pigTotal))
			}
		}

		availAmt := openAmt.Add(prodAmt)
		availKg := openKg.Add(prodKg)
		movingAvg := decimal.Zero
		if availKg.IsPositive() {
			movingAvg = availAmt.Div(availKg)
		}
		expectedCloseKg := availKg.Sub(soldKg[sku]).Sub(bulkKg[sku]).Sub(wasteKg[sku])
		if expectedCloseKg.IsNegative() {
			expectedCloseKg = decimal.Zero
		}
		// 期末(全店):
		//   有盘点 → 柜台实盘 + 冷库余额
		//   无盘点 → 账面(期初 + 入库 − 卖出 − 统收 − 报损)
		//
		// ⚠️ 无盘点时**不能**用 0:那会把"没卖掉的货"的全额成本算成当期销售成本
		// ⇒ 毛利被严重低估。用账面值,成本就按 sold×加权单价 摊(与旧口径一致)。
		closeKg := decimal.Zero
		if hasStocktake {
			// 期末(全店)= 柜台实盘 + 冷库余额
			// (柜台实盘与冷库余额来自**按营业日推导的结余快照**;无快照时是账面余额)
			closeKg = counterKg[sku].Add(coldKg[sku])
		} else {
			closeKg = expectedCloseKg
		}
		// 期末金额(**成本口径**)—— 这是写进 sku_cost_daily 的**冻结快照**的值。
		closeAmtBasis := closeKg.Mul(movingAvg)

		// 预期毛利(预售结转):期末结余按**预期销售单价**重估。
		//
		// ⚠️ 重估**不混进销售成本、也不写进冻结快照**:
		//   它是"存货按预期售价重估"的 **accrual** ⇒ 单独进毛利(下面 gm 的
		//   +revaluation)、对账行里单列、下一期期初金额里加回来
		//   (见 loadSkuOpening)⇒ **冻结的快照始终是成本口径**
		//   ⇒ 门店审计人改预售价时不用动冻结数据。
		//
		// ⚠️ 为什么不能靠"重估后的期末金额"去压销售成本:
		//   预期售价 > 成本时,重估后的期末金额会**大于**账面可用金额
		//   ⇒ 销售成本变负 ⇒ 被"盘盈"保护裁成 0 ⇒ 这笔预期毛利就被吃掉了。
		revaluation := decimal.Zero
		presaleUnit := decimal.Zero
		// ⚠️ 判据是"**有没有预售结转行**",不是"单价是否大于 0":
		//   维护为 **0 元**的货号也要结转(负毛利照做)⇒ 单价 0 一样要重估。
		if unit, ok := presaleUnits[sku]; ok && closeKg.IsPositive() {
			presaleUnit = unit
			revaluation = closeKg.Mul(unit.Sub(movingAvg))
		}
		closeAmt := closeAmtBasis

		bulkCost := bulkKg[sku].Mul(movingAvg)
		wasteAmt := wasteKg[sku].Mul(movingAvg)
		wasteCost := wasteAmt
		// ⚠️ 销售成本用**成本口径**的期末金额(不含重估)——
		//   重估是 accrual,不是成本(见上面的注释)。
		salesCost := availAmt.Sub(closeAmtBasis).Sub(bulkCost).Sub(wasteCost)
		if salesCost.IsNegative() {
			// 期末实盘 > 账面可用(盘盈)⇒ 成本不能为负,裁到 0 并把差额记进对账行。
			salesCost = decimal.Zero
		}

		revenue := soldAmount[sku].Add(bulkAmount[sku])
		// ⚠️ 2026-10-23 修正:**统收成本与报损成本必须进损益**。
		//
		// 原来 gm = revenue − salesCost,而 salesCost 里**已经减掉了** bulkCost/wasteCost
		// (它们从"可售金额"里被扣除,不代表成本已入账)⇒
		//   · 统收收入(卖板油给酒店)被当成**零成本收入** ⇒ 毛利率 100%
		//   · 报损的 kg 成本**凭空消失** ⇒ 报损不体现为损失
		//
		// 正确式:本期损益 = (零售收入 + 统收收入) − (零售销售成本 + 统收成本 + 报损成本)
		//           + 预期毛利(预售结转的重估差额)。
		gm := revenue.Sub(salesCost).Sub(bulkCost).Sub(wasteCost).Add(revaluation)
		var gmPct float64
		// 毛利率的分母:有预售结转时,本期毛利里含**预期收入**那一块
		// ⇒ 分母也要含它,否则百分比会虚高(甚至 > 100%)。
		presaleRevenue := decimal.Zero
		if presaleUnit.IsPositive() {
			presaleRevenue = closeKg.Mul(presaleUnit)
		}
		gmBase := revenue.Add(presaleRevenue)
		if !gmBase.IsZero() {
			f, _ := gm.Mul(decimal.NewFromInt(100)).Div(gmBase).Float64()
			gmPct = f
		}

		out = append(out, SkuDailyCost{
			BranchID:          branchID,
			CubeProductID:     sku,
			CubeProductName:   nameOfSku[sku],
			CutType:           cutOfSku[sku],
			OpeningKg:         openKg,
			OpeningAmount:     openAmt,
			ProducedKg:        prodKg,
			ProducedAmount:    prodAmt,
			SoldKg:            soldKg[sku],
			SoldAmount:        soldAmount[sku],
			BulkSoldKg:        bulkKg[sku],
			BulkSoldAmount:    bulkAmount[sku],
			WasteKg:           wasteKg[sku],
			WasteAmount:       wasteAmt,
			ExpectedClosingKg: expectedCloseKg,
			ClosingKg:         closeKg,
			ClosingAmount:     closeAmt,
			MovingAvgUnitCost: movingAvg,
			SalesCostYuan:     salesCost,
			BulkCostYuan:      bulkCost,
			WasteCostYuan:     wasteCost,
			RevenueYuan:       revenue,
			GrossMarginYuan:   gm,
			GrossMarginPct:    gmPct,
			CounterKg:         counterKg[sku],
			ColdStoreKg:       coldKg[sku],
			PresaleMarginYuan: revaluation,
			PresaleExpectedRevenueYuan: presaleRevenue,
			PresaleExpectedUnitYuan:    presaleUnit,
			DataSource:        dataSource,
		})

		recon.OpeningInventoryCostYuan = recon.OpeningInventoryCostYuan.Add(openAmt)
		recon.ProducedCostYuan = recon.ProducedCostYuan.Add(prodAmt)
		recon.SalesCostYuan = recon.SalesCostYuan.Add(salesCost)
		recon.BulkCostYuan = recon.BulkCostYuan.Add(bulkCost)
		recon.WasteCostYuan = recon.WasteCostYuan.Add(wasteCost)
		// ⚠️ 期末存货金额记**成本口径**(不含重估)⇒ 成本流恒等式不变:
		//   「期初+入库 = 销售+统收+报损+期末(成本口径)」
		//   重估差额是 accrual(预期值),单列在 RevaluationYuan,不当"钱"进恒等式。
		recon.ClosingInventoryCostYuan = recon.ClosingInventoryCostYuan.Add(closeAmtBasis)
		recon.RevaluationYuan = recon.RevaluationYuan.Add(revaluation)
	}

	// 对账行:差额 = (期初 + 入库) − (销售 + 统收 + 报损 + 期末)
	//
	// ⚠️ **要 round**:移动加权单价是"金额 ÷ kg"除出来的,十进制除法会留
	// 1e-16 量级的尾数 ⇒ 差额显示成 "-0.0000000000000001"。
	// 那一行是"差额必须为 0"的判据 ⇒ 显示成 1e-16 会让人以为**对账不平**。
	// ⇒ round 到 4 位(0.0001 元 = 0.01 分):远低于任何有意义的金额,
	// 真实的不平(几元、几十元)照样原样显示。
	recon.PigPurchaseCostYuan = decimal.Zero
	for _, p := range pigs {
		recon.PigPurchaseCostYuan = recon.PigPurchaseCostYuan.Add(p.PurchaseCostYuan)
	}
	recon.UnallocatedPigCostYuan = unallocatedPigCost
	// ⚠️ 恒等式里**不减**重估:重估是 accrual(预期毛利),不是成本流的一环。
	//   (成本流恒等式用**成本口径**的期末金额 ⇒ 差额仍然是 0。)
	recon.DiffYuan = recon.OpeningInventoryCostYuan.Add(recon.ProducedCostYuan).
		Sub(recon.SalesCostYuan).Sub(recon.BulkCostYuan).
		Sub(recon.WasteCostYuan).Sub(recon.ClosingInventoryCostYuan).
		Round(4)
	recon.CuttingLossKg = cuttingLossKg

	// 隔夜失重·盘亏:昨日全店 − 今日全店 − (今日产出 − 今日卖出 − 今日报损)
	losses, err := s.computeOvernightShrink(ctx, branchID, bizDate, out)
	if err != nil {
		return nil, nil, nil, err
	}
	recon.OvernightShrinkKg = losses.OvernightShrinkKg

	sort.Slice(out, func(i, j int) bool { return out[i].CubeProductID < out[j].CubeProductID })
	return out, recon, losses, nil
}

// computeProducedBySku 算今日产出(入库)按 SKU 归集。
//
// 返回:
//   - producedKg: SKU → 今日产出 kg
//   - producedByPigKg: pigID → SKU → kg(逐猪摊成本要用)
//   - unallocatedPigCost: 今日进场但**没有任何产出数据**的猪的成本(摊不出去)
//   - cuttingLossKg: 进猪毛重 − 产出 kg(切分损耗)
func (s *Service) computeProducedBySku(
	ctx context.Context, branchID string, startOfDay, endOfDay time.Time,
	pigs []model.WholePig, pigCost map[string]decimal.Decimal,
) (map[string]decimal.Decimal, map[string]map[string]decimal.Decimal, decimal.Decimal, decimal.Decimal, error) {
	producedKg := map[string]decimal.Decimal{}
	byPig := map[string]map[string]decimal.Decimal{}

	pigIDs := make([]string, 0, len(pigs))
	for _, p := range pigs {
		pigIDs = append(pigIDs, p.ID)
		byPig[p.ID] = map[string]decimal.Decimal{}
	}

	// 1) pig_cuts 实录
	var pigCuts []model.PigCut
	if err := s.db.WithContext(ctx).
		Joins("JOIN whole_pigs ON whole_pigs.id = pig_cuts.pig_id").
		Where("pig_cuts.branch_id = ? AND whole_pigs.arrived_at >= ? AND whole_pigs.arrived_at < ?",
			branchID, startOfDay, endOfDay).
		Find(&pigCuts).Error; err != nil {
		return nil, nil, decimal.Zero, decimal.Zero, fmt.Errorf("query pig_cuts: %w", err)
	}
	// 没有 cube_product_id 的补录:先按 cut_type 记着,稍后归到该部位的主 SKU。
	pendingByCut := map[model.CutType]decimal.Decimal{}
	for _, c := range pigCuts {
		sku := strings.TrimSpace(c.CubeProductID)
		if sku == "" {
			pendingByCut[c.CutType] = pendingByCut[c.CutType].Add(c.WeightKg)
			continue
		}
		producedKg[sku] = producedKg[sku].Add(c.WeightKg)
		if byPig[c.PigID] != nil {
			byPig[c.PigID][sku] = byPig[c.PigID][sku].Add(c.WeightKg)
		}
	}

	// 2) **统计基线优先**、LLM 兜底。
	//
	// ⚠️ 顺序很重要(2026-10-23):统计基线是从**真实补录样本**拟合出来的,
	// LLM 是语言先验 ⇒ 同样缺数据时,**基线更准**。
	// 而生产上 LLM 实际经常什么都给不出(history_avg 空历史 → cuts: [])。
	//
	// 2a) 该猪体重段有基线、且该部位**没有** pig_cuts → 用基线 p50
	// 2b) 基线也没有的部位 → 才用 llm_advice
	llmByPig := map[string]map[model.CutType]decimal.Decimal{}
	var advised []struct {
		PigID string
		Cut   model.CutType
		Kg    decimal.Decimal
	}
	for _, p := range pigs {
		if len(p.LLMAdviceJSON) == 0 {
			continue
		}
		var advice struct {
			Cuts []PredictedCut `json:"cuts"`
		}
		if err := json.Unmarshal(p.LLMAdviceJSON, &advice); err != nil {
			continue
		}
		if llmByPig[p.ID] == nil {
			llmByPig[p.ID] = map[model.CutType]decimal.Decimal{}
		}
		for _, c := range advice.Cuts {
			llmByPig[p.ID][c.CutType] = llmByPig[p.ID][c.CutType].Add(c.ExpectedKg)
			advised = append(advised, struct {
				PigID string
				Cut   model.CutType
				Kg    decimal.Decimal
			}{p.ID, c.CutType, c.ExpectedKg})
		}
	}
	// 该部位是否已有 pig_cuts 实录 —— **按猪**判定,不是按部位!
	//
	// ⚠️ 2026-10-23 修正:原来用一个全局的 cutHasActual[cut] ⇒
	// 只要**任何一头猪**补录过该部位,统计基线就再也用不上了
	// (基线是给"没补录的猪"兜底的)⇒ 基线形同虚设。
	actualCutsByPig := map[string]map[model.CutType]bool{}
	for _, c := range pigCuts {
		if actualCutsByPig[c.PigID] == nil {
			actualCutsByPig[c.PigID] = map[model.CutType]bool{}
		}
		actualCutsByPig[c.PigID][c.CutType] = true
	}
	hasActual := func(pigID string, cut model.CutType) bool {
		return actualCutsByPig[pigID][cut]
	}
	cutPrimarySku, err := s.primarySkuPerCut(ctx, branchID)
	if err != nil {
		return nil, nil, decimal.Zero, decimal.Zero, err
	}

	// 2a) 统计基线
	baselines, err := s.loadForecastBaselines(ctx, branchID)
	if err != nil {
		return nil, nil, decimal.Zero, decimal.Zero, err
	}
	coveredByBaseline := map[string]map[model.CutType]bool{}
	for _, p := range pigs {
		band := WeightBandOf(p.GrossWeightKg)
		for k, b := range baselines {
			if b.WeightBand != band || !b.Usable() {
				continue
			}
			if hasActual(p.ID, b.CutType) {
				continue
			}
			sku, ok := cutPrimarySku[b.CutType]
			if !ok || b.P50Kg.IsZero() {
				continue
			}
			producedKg[sku] = producedKg[sku].Add(b.P50Kg)
			if byPig[p.ID] != nil {
				byPig[p.ID][sku] = byPig[p.ID][sku].Add(b.P50Kg)
			}
			if coveredByBaseline[p.ID] == nil {
				coveredByBaseline[p.ID] = map[model.CutType]bool{}
			}
			coveredByBaseline[p.ID][b.CutType] = true
			_ = k
		}
	}

	// 2b) LLM 兜底(基线没覆盖的部位)
	for _, a := range advised {
		if hasActual(a.PigID, a.Cut) {
			continue
		}
		if coveredByBaseline[a.PigID][a.Cut] {
			continue // 基线已经给过了
		}
		sku, ok := cutPrimarySku[a.Cut]
		if !ok {
			continue
		}
		producedKg[sku] = producedKg[sku].Add(a.Kg)
		if byPig[a.PigID] != nil {
			byPig[a.PigID][sku] = byPig[a.PigID][sku].Add(a.Kg)
		}
	}

	// 3) 没有 cube_product_id 的 pig_cuts → 归到该部位的主 SKU
	for cut, kg := range pendingByCut {
		sku, ok := cutPrimarySku[cut]
		if !ok || len(pigs) == 0 {
			continue
		}
		producedKg[sku] = producedKg[sku].Add(kg)
		// 这批 kg 的猪归属不明(补录没记 SKU)⇒ 归给当日第一头猪
		// (仓管补录顺序即分割顺序)。
		byPig[pigs[0].ID][sku] = byPig[pigs[0].ID][sku].Add(kg)
	}

	// 4) 切分损耗 = 进猪毛重 − 产出 kg;摊不出去的成本(没有任何产出数据的猪)
	totalProduced := decimal.Zero
	for _, m := range byPig {
		for _, kg := range m {
			totalProduced = totalProduced.Add(kg)
		}
	}
	totalGross := decimal.Zero
	for _, p := range pigs {
		totalGross = totalGross.Add(p.GrossWeightKg)
	}
	cuttingLoss := totalGross.Sub(totalProduced)
	if cuttingLoss.IsNegative() {
		cuttingLoss = decimal.Zero
	}
	unallocated := decimal.Zero
	for _, p := range pigs {
		if s.pigTotalProducedKg(byPig[p.ID]).IsZero() {
			unallocated = unallocated.Add(pigCost[p.ID])
		}
	}
	return producedKg, byPig, unallocated, cuttingLoss, nil
}

// pigTotalProducedKg 某头猪今日产出的总 kg。
func (s *Service) pigTotalProducedKg(perPig map[string]decimal.Decimal) decimal.Decimal {
	total := decimal.Zero
	for _, kg := range perPig {
		total = total.Add(kg)
	}
	return total
}

// totalProducedKg 全部猪今日产出的总 kg。
func (s *Service) totalProducedKg(byPig map[string]map[string]decimal.Decimal) decimal.Decimal {
	total := decimal.Zero
	for _, perPig := range byPig {
		for _, kg := range perPig {
			total = total.Add(kg)
		}
	}
	return total
}

// loadSkuFlows 拉当日卖出 / 统收 / 报损,按 SKU 归集。
//
// ⚠️ 报损**只回数量**,金额留到主循环里按当期移动加权单价算
// (那时单价才是正确的 —— 报损货的成本就是当期的加权成本)。
//
// cutFromSales 是"从销售行反推的部位":**没有部位映射的 SKU**(如特价档)
// 在 branch_cut_mappings 里查不到部位,但销售行自带 cut_type ⇒ 用它兜底,
// 否则这类销售会落进一个**空 cut_type 的桶**(报表上"无名桶")。
func (s *Service) loadSkuFlows(ctx context.Context, branchID string, startOfDay, endOfDay time.Time) (
	soldKg, soldAmount, bulkKg, bulkAmount, wasteKg map[string]decimal.Decimal,
	cutFromSales map[string]model.CutType, err error,
) {
	soldKg = map[string]decimal.Decimal{}
	soldAmount = map[string]decimal.Decimal{}
	bulkKg = map[string]decimal.Decimal{}
	bulkAmount = map[string]decimal.Decimal{}
	wasteKg = map[string]decimal.Decimal{}
	cutFromSales = map[string]model.CutType{}

	var sales []model.LineSalesByPig
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND sale_date >= ? AND sale_date < ?", branchID, startOfDay, endOfDay).
		Find(&sales).Error; err != nil {
		return nil, nil, nil, nil, nil, nil, fmt.Errorf("query line_sales_by_pig: %w", err)
	}
	for _, sa := range sales {
		if sa.OrderStatus != "S" || sa.QtyKg.IsZero() || sa.QtyKg.IsNegative() {
			continue
		}
		sku := strings.TrimSpace(sa.CubeProductID)
		if sku == "" {
			continue
		}
		if sa.CutType != "" {
			cutFromSales[sku] = sa.CutType
		}
		if sa.Source == "bulk" {
			bulkKg[sku] = bulkKg[sku].Add(sa.QtyKg)
			bulkAmount[sku] = bulkAmount[sku].Add(sa.AmountYuan)
			continue
		}
		soldKg[sku] = soldKg[sku].Add(sa.QtyKg)
		soldAmount[sku] = soldAmount[sku].Add(sa.AmountYuan)
	}

	var wasteRows []model.WasteLog
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND recorded_at >= ? AND recorded_at < ?", branchID, startOfDay, endOfDay).
		Find(&wasteRows).Error; err != nil {
		return nil, nil, nil, nil, nil, nil, fmt.Errorf("query waste_logs: %w", err)
	}
	for _, w := range wasteRows {
		sku := strings.TrimSpace(w.CubeProductID)
		if sku == "" {
			continue
		}
		wasteKg[sku] = wasteKg[sku].Add(w.QtyKg)
	}
	return soldKg, soldAmount, bulkKg, bulkAmount, wasteKg, cutFromSales, nil
}

// loadSkuOpening 取期初(全店)数量与金额。
//
// 优先取**上一营业日的日快照**(全店口径,链式一致);
// 没有快照时回退到"上一日盘点(柜台实盘)+ 当前冷库余额",并把 data_source 降级。
//
// ⚠️ 回退分支最后会叠加**期初结转**(见 overlayStockOpenings):
//
//	生效日 == 当前营业日 的期初行 ⇒ 链的起点就在今天
//	⇒ kg 与金额都取**门店给的**(重量、单价)
//	⇒ 否则那批货零成本 ⇒ 毛利虚高(2026-10-25 门店口径)。
func (s *Service) loadSkuOpening(ctx context.Context, branchID string, bizDate time.Time) (
	kg, amount map[string]decimal.Decimal, err error,
) {
	kg = map[string]decimal.Decimal{}
	amount = map[string]decimal.Decimal{}

	prevStart, prevEnd := s.previousBizDayBounds(bizDate)
	// 前一个营业日的日期键（"YYYY-MM-DD"）—— 纯基线日判定要用。
	prevKey := prevStart.In(s.bizTZOrUTC()).Format("2006-01-02")
	var prev []model.SkuCostDaily
	e := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date >= ? AND biz_date < ?", branchID, prevStart, prevEnd).
		Order("cube_product_id ASC").
		Find(&prev).Error
	if e != nil {
		return nil, nil, fmt.Errorf("query sku_cost_daily(prev): %w", e)
	}
	if len(prev) > 0 {
		// 上一日的**预售重估**(预期售价 − 成本口径单价)要加回期初金额:
		// ⇒ 「下一期期初单价 = 预期销售单价」这条口径在这里落地;
		// ⇒ 否则那批货会在本期**再结一次毛利**(两期总毛利多算)。
		//
		// ⚠️ 重估**不写进** sku_cost_daily.closing_amount(那是**冻结的成本口径快照**)
		//   ⇒ 门店审计人改预售价时,冻结的快照不用动,只改预售那一层。
		units, err := s.loadPresaleUnits(ctx, branchID, prevStart)
		if err != nil {
			return nil, nil, err
		}
		for _, r := range prev {
			kg[r.CubeProductID] = r.ClosingKg
			amount[r.CubeProductID] = r.ClosingAmount
			// ⚠️ 判据是"**有没有预售结转行**",不是"单价是否大于 0":
			//   维护为 0 元 / 取不到价的货号也是 0 ⇒ 也要重估(写减记)。
			if unit, ok := units[r.CubeProductID]; ok {
				amount[r.CubeProductID] = amount[r.CubeProductID].
					Add(r.ClosingKg.Mul(unit.Sub(r.MovingAvgUnitCost)))
			}
		}
		return kg, amount, nil
	}

	// 回退:上一营业日**盘点单的实盘**(盘点单存的是**全店**实盘 = 柜台实盘 + 冷库余额)
	prevClosing, err := s.loadStocktakeClosing(ctx, branchID, prevStart, prevEnd)
	if err != nil {
		return nil, nil, err
	}
	for sku, v := range prevClosing {
		kg[sku] = v
	}
	// 期初金额 = 期初量 × 上日的移动加权单价(来自 sku_cost_states)。
	var states []model.SkuCostState
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Find(&states).Error; err != nil {
		return nil, nil, fmt.Errorf("query sku_cost_states: %w", err)
	}
	for _, st := range states {
		if q, ok := kg[st.CubeProductID]; ok {
			amount[st.CubeProductID] = q.Mul(st.MovingAvgUnitCost)
		}
	}
	// ⚠️ **期初结转覆盖**(2026-10-25 门店口径)—— 必须放在最后。
	//
	// 生效日 == 当前营业日 的期初行 ⇒ 链的起点就在今天
	// ⇒ 这批货的 kg 与金额都用**门店给的**(重量、单价)
	// ⇒ 否则它**零成本**(金额 0)⇒ 卖出去全是利润(毛利虚高)。
	//
	// ⚠️ 覆盖"盘点实盘"与"移动加权单价"两套口径:
	//   上面的两套都是"链外"的兜底(链还没起 ⇒ 没有上一日的快照),
	//   而期初结转才是这条链**真正的起点** ⇒ 它说了算。
	if err := s.overlayStockOpenings(ctx, branchID, bizDate, kg, amount); err != nil {
		return nil, nil, err
	}
	// ⚠️ **纯基线日的次日**:期初那一行就是前一天的"日终"(那天什么都没做)
	//   ⇒ 次日的期初 = 期初的 kg / 金额。
	//
	//   不补这一条 ⇒ 上线第一天(10-02)的成本链里**没有**上线前那批货
	//   ⇒ 期初那 1800 元的成本消失 ⇒ **毛利虚高**。
	baseline, err := s.isBaselineDay(ctx, branchID, prevKey)
	if err != nil {
		return nil, nil, err
	}
	if baseline {
		baseKg, baseAmt, _, err := s.openingBalancesAt(
			ctx, branchID, mustParseDateInLoc(prevKey, s.bizTZOrUTC()))
		if err != nil {
			return nil, nil, err
		}
		for sku, v := range baseKg {
			kg[sku] = v
		}
		for sku, v := range baseAmt {
			amount[sku] = v
		}
	}
	// ⚠️ 这里**不补**预售重估(回退路径上期初量本身就是空的/过期的,
	//   硬补一次重估会让"两期总毛利"算错)⇒ 断链的那一天,成本链本来就不完整,
	//   报表的数据_source 会标成 estimated,门店能看出来。
	return kg, amount, nil
}

// loadStocktakeClosing 取某窗口的**盘点单实盘**(per-SKU,全店口径)。
//
// ⚠️ 2026-10-24(盘点口径拆分):盘点单里存的是**柜台实盘**(仓管只盘猪肉台),
// 冷库余额**单独一个字段**(ColdStoreKg)⇒ 全店实盘 = 柜台实盘 + 冷库余额。
//
// ⚠️ 老数据(拆分之前)存的是**全店实盘**(ColdStoreKg 为 0)⇒ 公式一样成立。
func (s *Service) loadStocktakeClosing(ctx context.Context, branchID string, start, end time.Time) (map[string]decimal.Decimal, error) {
	out := map[string]decimal.Decimal{}
	var st model.PorkCutsStocktake
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND taken_at >= ? AND taken_at < ? AND is_complete = ?", branchID, start, end, true).
		Order("taken_at DESC").
		First(&st).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query prev stocktake: %w", err)
	}
	var snaps []model.CutSnapshot
	if err := json.Unmarshal(st.Cuts, &snaps); err != nil {
		return nil, fmt.Errorf("unmarshal prev stocktake cuts: %w", err)
	}
	for _, sn := range snaps {
		sku := strings.TrimSpace(sn.CubeProductID)
		if sku == "" {
			continue
		}
		out[sku] = out[sku].Add(sn.ActualRemainKg).Add(sn.ColdStoreKg)
	}
	return out, nil
}

// loadSkuCounterAndCold 取**两位置**余额(账面)+ 今日是否有盘点。
//
// ⚠️ **counter 就是账面余额,不用盘点值覆盖**。
// 原因:日终盘点后会把柜台的货"搬进冷库过夜"(counter := 0,cold += 柜台实盘),
// 此时冷库里**已经包含**了柜台那份 ⇒ 若再拿盘点值加一次就是**重复计算**
// (曾导致全店期末翻倍:柜台 10 + 冷库 10 = 20,而真实是 10)。
//
// hasStocktake / dataSource 只用于**标注可信度**,不参与数量计算。
func (s *Service) loadSkuCounterAndCold(ctx context.Context, branchID string, startOfDay, endOfDay time.Time) (
	counter, cold map[string]decimal.Decimal, hasStocktake bool, dataSource string, err error,
) {
	counter, cold, err = s.loadSkuPositions(ctx, branchID)
	if err != nil {
		return nil, nil, false, "", err
	}
	var n int64
	if err := s.db.WithContext(ctx).
		Model(&model.PorkCutsStocktake{}).
		Where("branch_id = ? AND taken_at >= ? AND taken_at < ?", branchID, startOfDay, endOfDay).
		Count(&n).Error; err != nil {
		return nil, nil, false, "", fmt.Errorf("count stocktake: %w", err)
	}
	if n == 0 {
		return counter, cold, false, "estimated", nil
	}
	return counter, cold, true, "actual", nil
}

// loadSkuPositions 读当前两位置余额。
func (s *Service) loadSkuPositions(ctx context.Context, branchID string) (counter, cold map[string]decimal.Decimal, err error) {
	counter = map[string]decimal.Decimal{}
	cold = map[string]decimal.Decimal{}
	var rows []model.SkuStockPosition
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Find(&rows).Error; err != nil {
		return nil, nil, fmt.Errorf("query sku_stock_positions: %w", err)
	}
	for _, r := range rows {
		switch r.Location {
		case model.StockLocationCounter:
			counter[r.CubeProductID] = counter[r.CubeProductID].Add(r.QtyKg)
		case model.StockLocationColdStore:
			cold[r.CubeProductID] = cold[r.CubeProductID].Add(r.QtyKg)
		}
	}
	return counter, cold, nil
}

// loadSkuCatalog 取 SKU → 部位 / 名称(部位来自映射表)。
//
// ⚠️ 2026-10-23:大宗品项**不再有独立的位置 key**。
// 原来大宗记在伪 SKU "bulk:<code>" 上(它们没有思迅货号),
// 但伪 SKU 不在部位映射里 ⇒ 没有入库金额 ⇒ 移动加权单价恒为 0
// ⇒ **统收成本算成 0 ⇒ 卖板油的毛利率 100%**(实测:卖 10kg 收 60 元,
// 成本 0、毛利 +60;真实成本 1000 元、毛利应是 −940)。
// ⇒ 大宗库存与收入现在记在**部位主 SKU** 上 ⇒ 与零售/盘点同一口径,成本自动匹配。
func (s *Service) loadSkuCatalog(ctx context.Context, branchID string, skuSet map[string]struct{}) (cutOf map[string]model.CutType, nameOf map[string]string, err error) {
	cutOf = map[string]model.CutType{}
	nameOf = map[string]string{}
	var mappings []model.BranchCutMapping
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Find(&mappings).Error; err != nil {
		return nil, nil, fmt.Errorf("query branch_cut_mappings: %w", err)
	}
	for _, m := range mappings {
		cutOf[m.CubeProductID] = m.CutType
		nameOf[m.CubeProductID] = m.CubeProductName
	}
	return cutOf, nameOf, nil
}

// primarySkuPerCut 取每个部位在映射表里的**主 SKU**(用于 LLM 兜底与无 SKU 补录的归集)。
//
// 取"该部位下 cube_product_id 最小"的那条 ⇒ 确定性,且与前端下拉顺序一致。
func (s *Service) primarySkuPerCut(ctx context.Context, branchID string) (map[model.CutType]string, error) {
	var mappings []model.BranchCutMapping
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("cut_type ASC, cube_product_id ASC").
		Find(&mappings).Error; err != nil {
		return nil, fmt.Errorf("query branch_cut_mappings: %w", err)
	}
	out := map[model.CutType]string{}
	for _, m := range mappings {
		if _, ok := out[m.CutType]; !ok {
			out[m.CutType] = m.CubeProductID
		}
	}
	return out, nil
}

// expectedPerCutByEngine 用**移动加权引擎**算各部位的账面应有期末量(全店口径)。
//
// ⚠️ 2026-10-23:盘点单的 expected 改用它,而不是旧的 computeExpectedRemainKgByCut。
// 理由:旧算法是**部位级**、且不含"昨天的货";新算法是 SKU 级全店口径,
// 与毛利报告**同一个引擎** ⇒ 不会出现"盘点单的应有"与"报表的应有"两套口径。
//
// 引擎里的 expected(ExpectedClosingKg)不依赖盘点单 ⇒ 盘点提交前就能算。
func (s *Service) expectedPerCutByEngine(ctx context.Context, branchID string, day time.Time) (
	expected, opening, waste map[model.CutType]decimal.Decimal, err error,
) {
	expected = map[model.CutType]decimal.Decimal{}
	opening = map[model.CutType]decimal.Decimal{}
	waste = map[model.CutType]decimal.Decimal{}
	rows, _, _, err := s.computeSkuDailyCosts(ctx, branchID, day)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, r := range rows {
		expected[r.CutType] = expected[r.CutType].Add(r.ExpectedClosingKg)
		opening[r.CutType] = opening[r.CutType].Add(r.OpeningKg)
		waste[r.CutType] = waste[r.CutType].Add(r.WasteKg)
	}
	return expected, opening, waste, nil
}

// loadSkuClosingAll 取某窗口的**全店**期末量(柜台 + 冷库)。
//
// 有盘点 → 柜台实盘 + 冷库余额;无盘点 → 账面(柜台账面 + 冷库余额)。
func (s *Service) loadSkuClosingAll(ctx context.Context, branchID string, start, end time.Time) (map[string]decimal.Decimal, error) {
	counter, cold, hasStocktake, _, err := s.loadSkuCounterAndCold(ctx, branchID, start, end)
	if err != nil {
		return nil, err
	}
	out := map[string]decimal.Decimal{}
	for sku, v := range counter {
		out[sku] = v
	}
	for sku, v := range cold {
		out[sku] = out[sku].Add(v)
	}
	if !hasStocktake {
		// 无盘点 ⇒ 账面里没有"卖掉了多少"的信息,退回按位置余额(全店口径仍成立)。
		return out, nil
	}
	return out, nil
}

// bizTZOrUTC 返回营业日时区(未注入时用 UTC)。
func (s *Service) bizTZOrUTC() *time.Location {
	if s.bizTZ != nil {
		return s.bizTZ
	}
	return time.UTC
}

// computeOvernightShrink 估"隔夜失重·盘亏"。
//
// 口径:昨日全店实盘 − 今日全店实盘 − (今日产出 − 今日卖出 − 今日报损)
// ⇒ 差额只能是隔夜失重 + 称重误差 + 盘亏(因为进货/销售/报损都已在里面扣掉)。
//
// ⚠️ **不是纯隔夜失重**:门店按"昨晚称好的重量"上��(2026-10-23 口径),
// 今天的货实际可能轻了,但系统里没有今天的实称值 ⇒ 只能估,不能测。
func (s *Service) computeOvernightShrink(ctx context.Context, branchID string, bizDate time.Time, rows []SkuDailyCost) (*LossSummary, error) {
	out := &LossSummary{ShrinkNote: "期初沿用昨日日终重量,未做隔夜失重修正;实测销量可能被高估。"}

	prevStart, prevEnd := s.previousBizDayBounds(bizDate)
	prevClosing, err := s.loadSkuClosingAll(ctx, branchID, prevStart, prevEnd)
	if err != nil {
		return nil, err
	}
	if len(prevClosing) == 0 {
		return out, nil
	}
	prevTotal := decimal.Zero
	for _, v := range prevClosing {
		prevTotal = prevTotal.Add(v)
	}
	if prevTotal.IsZero() {
		return out, nil
	}
	curTotal := decimal.Zero
	for _, r := range rows {
		curTotal = curTotal.Add(r.ClosingKg)
	}
	// 今日净变动 = 产出 − 卖出 − 报损
	netMove := decimal.Zero
	for _, r := range rows {
		netMove = netMove.Add(r.ProducedKg).Sub(r.SoldKg).Sub(r.WasteKg)
	}
	shrink := prevTotal.Add(netMove).Sub(curTotal)
	if shrink.IsNegative() {
		shrink = decimal.Zero
	}
	out.OvernightShrinkKg = shrink
	f, _ := shrink.Mul(decimal.NewFromInt(100)).Div(prevTotal).Float64()
	out.OvernightShrinkPct = f
	return out, nil
}
