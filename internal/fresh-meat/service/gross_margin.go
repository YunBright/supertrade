// Package service / gross_margin.go —— 按门店 + 营业日汇总鲜肉毛利。
//
// 算法:
//   per cut:
//
//     purchased_kg   = pig_cuts(weight_kg) + llm_advice(expected_kg) - pig_cuts(同上)
//                     （= llm_advice 中"未录入 pig_cuts"的 cut 的兜底）
//     sold_kg        = Σ line_sales_by_pig.qty_kg WHERE OrderStatus='S' and qty_kg>0
//     waste_kg       = Σ waste_logs.qty_kg
//     opening_kg     = 上一营业日 is_complete=true stocktake 的 ActualRemainKg
//     expected_kg    = opening + purchased - sold - waste(裁到 >= 0)
//     actual_kg      = 当日 is_complete=true stocktake 的 ActualRemainKg(若无 → 空)
//
//     revenue_yuan   = Σ line_sales_by_pig.amount_yuan WHERE OrderStatus='S' and qty_kg>0
//     purchase_cost_yuan
//                    = (本 cut 来源 pig 的 purchase_cost_yuan 按 in_kg 比例摊销) ×
//                      min(1, sold_kg / max(purchased_kg, sold_kg))
//
//     gross_margin_yuan = revenue - purchase_cost_yuan
//     gross_margin_pct  = gross_margin / revenue × 100(无 revenue 时 0)
//
//  cost_allocation 细节:
//     - 同一 cut 可能来自多 pig;每 pig 的 purchase_cost 按 (pig_in_kg_for_cut / pig_total_in_kg) 比例分摊
//     - pig_total_in_kg = 该 pig 当日 LLM 给出所有 cut 的 expected_kg 之和
//     - 实际消耗比例 = min(1, sold_kg / purchased_kg);purchased=0 时 100%(LLM 兜底成本照样算)
//
//  data_source:
//     - is_complete=true stocktake 存在 → "actual"(含 actual_kg / variance_kg)
//     - 仅部分 stocktake → "partial"
//     - 无 stocktake → "estimated"(expected - actual 字段缺失)
//
// 退货行(OrderStatus='R' 或 qty<0):**不计入** revenue/sold_kg/cost。
//
// 全程不引入 tenant_id。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// GrossMarginReport 单店单日毛利总览。
type GrossMarginReport struct {
	BranchID            string                `json:"branch_id"`
	Date                string                `json:"date"`                  // YYYY-MM-DD(bizTZ)
	IsStocktakeComplete bool                  `json:"is_stocktake_complete"`
	DataSource          string                `json:"data_source"`           // actual | partial | estimated
	PerCut              []PerCutGrossMargin   `json:"per_cut"`
	Total               TotalGrossMargin      `json:"total"`
	GeneratedAt         time.Time             `json:"generated_at"`
}

// PerCutGrossMargin 单 cut 的毛利明细。
type PerCutGrossMargin struct {
	CutType          model.CutType   `json:"cut_type"`
	OpeningKg        decimal.Decimal `json:"opening_kg"`
	PurchasedKg      decimal.Decimal `json:"purchased_kg"`
	SoldKg           decimal.Decimal `json:"sold_kg"`
	WasteKg          decimal.Decimal `json:"waste_kg"`
	ActualRemainKg   decimal.Decimal `json:"actual_remain_kg,omitempty"`
	ExpectedRemainKg decimal.Decimal `json:"expected_remain_kg"`
	VarianceKg       decimal.Decimal `json:"variance_kg,omitempty"`
	RevenueYuan      decimal.Decimal `json:"revenue_yuan"`
	PurchaseCostYuan decimal.Decimal `json:"purchase_cost_yuan"`
	GrossMarginYuan  decimal.Decimal `json:"gross_margin_yuan"`
	GrossMarginPct   float64         `json:"gross_margin_pct"`
}

// TotalGrossMargin 全店汇总。
type TotalGrossMargin struct {
	RevenueYuan       decimal.Decimal `json:"revenue_yuan"`
	PurchaseCostYuan  decimal.Decimal `json:"purchase_cost_yuan"`
	GrossMarginYuan decimal.Decimal `json:"gross_margin_yuan"`
	GrossMarginPct    float64        `json:"gross_margin_pct"`
}

// ComputeGrossMargin 按 (branchID, day) 算毛利。
//
// day 是任一时刻;取 day 在 bizTZ 下的日期作为营业日。
func (s *Service) ComputeGrossMargin(ctx context.Context, branchID string, day time.Time) (*GrossMarginReport, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}

	startOfDay, endOfDay := s.businessDayBounds(day)
	openingPerCut, err := s.loadOpeningPerCut(ctx, branchID, day)
	if err != nil {
		return nil, fmt.Errorf("load opening: %w", err)
	}

	// 1) pig_cuts 入库 + 全集 cuts
	pigCutsAgg := make(map[model.CutType]decimal.Decimal)
	cutByPig := make(map[string]map[model.CutType]decimal.Decimal) // pigID → cut → kg
	var pigCuts []model.PigCut
	if err := s.db.WithContext(ctx).
		Joins("JOIN whole_pigs ON whole_pigs.id = pig_cuts.pig_id").
		Where("pig_cuts.branch_id = ? AND whole_pigs.arrived_at >= ? AND whole_pigs.arrived_at < ?",
			branchID, startOfDay, endOfDay).
		Find(&pigCuts).Error; err != nil {
		return nil, fmt.Errorf("query pig_cuts: %w", err)
	}
	for _, c := range pigCuts {
		pigCutsAgg[c.CutType] = pigCutsAgg[c.CutType].Add(c.WeightKg)
		if cutByPig[c.PigID] == nil {
			cutByPig[c.PigID] = make(map[model.CutType]decimal.Decimal)
		}
		cutByPig[c.PigID][c.CutType] = cutByPig[c.PigID][c.CutType].Add(c.WeightKg)
	}

	// 2) LLM advice 兜底
	llmAgg := make(map[model.CutType]decimal.Decimal)
	llmByPig := make(map[string]map[model.CutType]decimal.Decimal) // pigID → cut → kg(LLM 期望)
	var pigs []model.WholePig
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND arrived_at >= ? AND arrived_at < ?", branchID, startOfDay, endOfDay).
		Find(&pigs).Error; err != nil {
		return nil, fmt.Errorf("query whole_pigs: %w", err)
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
			llmByPig[p.ID] = make(map[model.CutType]decimal.Decimal)
		}
		for _, cut := range advice.Cuts {
			llmAgg[cut.CutType] = llmAgg[cut.CutType].Add(cut.ExpectedKg)
			llmByPig[p.ID][cut.CutType] = llmByPig[p.ID][cut.CutType].Add(cut.ExpectedKg)
		}
	}

	// purchased_kg = pig_cuts(优先) + llm 兜底(扣除 pig_cuts 已覆盖)
	purchased := make(map[model.CutType]decimal.Decimal)
	for c := range pigCutsAgg {
		purchased[c] = pigCutsAgg[c]
	}
	for c, llmKg := range llmAgg {
		extra := llmKg.Sub(pigCutsAgg[c])
		if extra.IsNegative() {
			extra = decimal.Zero
		}
		purchased[c] = purchased[c].Add(extra)
	}

	// 3) sold_kg / revenue(仅 OrderStatus='S' 且 qty>0)
	soldPerCut := make(map[model.CutType]decimal.Decimal)
	revenuePerCut := make(map[model.CutType]decimal.Decimal)
	var sales []model.LineSalesByPig
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND sale_date >= ? AND sale_date < ?",
			branchID, startOfDay, endOfDay).
		Find(&sales).Error; err != nil {
		return nil, fmt.Errorf("query line_sales_by_pig: %w", err)
	}
	for _, sa := range sales {
		if sa.OrderStatus != "S" || sa.QtyKg.IsZero() || sa.QtyKg.IsNegative() {
			continue
		}
		soldPerCut[sa.CutType] = soldPerCut[sa.CutType].Add(sa.QtyKg)
		revenuePerCut[sa.CutType] = revenuePerCut[sa.CutType].Add(sa.AmountYuan)
	}

	// 4) waste_kg
	wastePerCut := make(map[model.CutType]decimal.Decimal)
	var wasteRows []model.WasteLog
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND recorded_at >= ? AND recorded_at < ?",
			branchID, startOfDay, endOfDay).
		Find(&wasteRows).Error; err != nil {
		return nil, fmt.Errorf("query waste_logs: %w", err)
	}
	for _, w := range wasteRows {
		if w.CutType == "" {
			continue
		}
		wastePerCut[w.CutType] = wastePerCut[w.CutType].Add(w.QtyKg)
	}

	// 5) 当日 stocktake(取最新一条)
	actualPerCut := make(map[model.CutType]decimal.Decimal)
	dataSource := "estimated"
	isComplete := false
	var st model.PorkCutsStocktake
	err = s.db.WithContext(ctx).
		Where("branch_id = ? AND taken_at >= ? AND taken_at < ?",
			branchID, startOfDay, endOfDay).
		Order("taken_at DESC").
		First(&st).Error
	if err == nil {
		isComplete = st.IsComplete
		var snaps []model.CutSnapshot
		if uerr := json.Unmarshal(st.Cuts, &snaps); uerr == nil {
			if st.IsComplete {
				for _, sn := range snaps {
					actualPerCut[sn.CutType] = sn.ActualRemainKg
				}
				dataSource = "actual"
			} else {
				dataSource = "partial"
			}
		}
	} else if !isNotFound(err) {
		return nil, fmt.Errorf("query stocktake: %w", err)
	}

	// 6) cost allocation per cut。
	//
	// 对每 cut:
	//   cost_for_cut = Σ pig (pig_total_cost × (in_cut_for_pig / pig_total_in_kg))
	// 实际消耗比例 = min(1, sold_kg / max(purchased_kg, sold_kg))
	// cost_consumed = cost_for_cut × consumed_ratio
	pigCostByID := make(map[string]decimal.Decimal, len(pigs))
	pigTotalInByID := make(map[string]decimal.Decimal, len(pigs))
	for _, p := range pigs {
		pigCostByID[p.ID] = p.PurchaseCostYuan
		if llm, ok := llmByPig[p.ID]; ok {
			total := decimal.Zero
			for _, kg := range llm {
				total = total.Add(kg)
			}
			pigTotalInByID[p.ID] = total
		}
	}

	costForCut := make(map[model.CutType]decimal.Decimal)
	for cut := range purchased {
		var cutCost decimal.Decimal
		// pig_cuts 来源
		for pigID, perCut := range cutByPig {
			kgInCut, ok := perCut[cut]
			if !ok || kgInCut.IsZero() {
				continue
			}
			pigTotal := pigTotalInByID[pigID]
			if pigTotal.IsZero() {
				continue
			}
			cutCost = cutCost.Add(pigCostByID[pigID].Mul(kgInCut).Div(pigTotal))
		}
		// LLM 兜底来源:LLM 给了该 cut 但无 pig_cuts 的部分
		for pigID, perCut := range llmByPig {
			llmKg, ok := perCut[cut]
			if !ok || llmKg.IsZero() {
				continue
			}
			already := cutByPig[pigID][cut] // 0 if nil
			extra := llmKg.Sub(already)
			if extra.IsNegative() {
				extra = decimal.Zero
			}
			if extra.IsZero() {
				continue
			}
			pigTotal := pigTotalInByID[pigID]
			if pigTotal.IsZero() {
				continue
			}
			cutCost = cutCost.Add(pigCostByID[pigID].Mul(extra).Div(pigTotal))
		}
		costForCut[cut] = cutCost
	}

	// 7) 汇总 per cut
	cutSet := make(map[model.CutType]struct{})
	for c := range purchased {
		cutSet[c] = struct{}{}
	}
	for c := range soldPerCut {
		cutSet[c] = struct{}{}
	}
	for c := range wastePerCut {
		cutSet[c] = struct{}{}
	}
	for c := range actualPerCut {
		cutSet[c] = struct{}{}
	}
	for c := range openingPerCut {
		cutSet[c] = struct{}{}
	}

	perCut := make([]PerCutGrossMargin, 0, len(cutSet))
	totRev := decimal.Zero
	totCost := decimal.Zero

	for cut := range cutSet {
		opening := openingPerCut[cut]
		purch := purchased[cut]
		sold := soldPerCut[cut]
		waste := wastePerCut[cut]
		expected := opening.Add(purch).Sub(sold).Sub(waste)
		if expected.IsNegative() {
			expected = decimal.Zero
		}

		// 消耗比例
		var ratio decimal.Decimal
		switch {
		case purch.IsZero() && sold.IsZero():
			ratio = decimal.Zero
		case purch.IsZero():
			ratio = decimal.NewFromInt(1)
		default:
			r := sold.Div(purch)
			if r.GreaterThan(decimal.NewFromInt(1)) {
				r = decimal.NewFromInt(1)
			}
			ratio = r
		}
		consumedCost := costForCut[cut].Mul(ratio)

		revenue := revenuePerCut[cut]
		gm := revenue.Sub(consumedCost)
		var gmPct float64
		if !revenue.IsZero() {
			gmPctF, _ := gm.Mul(decimal.NewFromInt(100)).Div(revenue).Float64()
			gmPct = gmPctF
		}

		row := PerCutGrossMargin{
			CutType:          cut,
			OpeningKg:        opening,
			PurchasedKg:      purch,
			SoldKg:           sold,
			WasteKg:          waste,
			ExpectedRemainKg: expected,
			RevenueYuan:      revenue,
			PurchaseCostYuan: consumedCost,
			GrossMarginYuan:  gm,
			GrossMarginPct:   gmPct,
		}
		if actual, ok := actualPerCut[cut]; ok {
			row.ActualRemainKg = actual
			row.VarianceKg = actual.Sub(expected)
		}
		perCut = append(perCut, row)
		totRev = totRev.Add(revenue)
		totCost = totCost.Add(consumedCost)
	}

	totGM := totRev.Sub(totCost)
	var totGMPct float64
	if !totRev.IsZero() {
		f, _ := totGM.Mul(decimal.NewFromInt(100)).Div(totRev).Float64()
		totGMPct = f
	}

	// 排序 by cut_type 字符串(便于输出稳定)
	sortPerCut(perCut)

	return &GrossMarginReport{
		BranchID:            branchID,
		Date:                day.In(s.bizTZOrUTC()).Format("2006-01-02"),
		IsStocktakeComplete: isComplete,
		DataSource:          dataSource,
		PerCut:              perCut,
		Total: TotalGrossMargin{
			RevenueYuan:       totRev,
			PurchaseCostYuan:  totCost,
			GrossMarginYuan: totGM,
			GrossMarginPct:    totGMPct,
		},
		GeneratedAt: s.now(),
	}, nil
}

func (s *Service) bizTZOrUTC() *time.Location {
	if s.bizTZ != nil {
		return s.bizTZ
	}
	return time.UTC
}

// sortPerCut 按 cut_type 字符串升序(输出稳定,便于 e2e 断言)。
func sortPerCut(rows []PerCutGrossMargin) {
	// 插入排序(通常 < 12 行)
	for i := 1; i < len(rows); i++ {
		j := i
		for j > 0 && string(rows[j-1].CutType) > string(rows[j].CutType) {
			rows[j-1], rows[j] = rows[j], rows[j-1]
			j--
		}
	}
}

// 静默引用(避免编译警告)
var (
	_ = gorm.ErrRecordNotFound
)
