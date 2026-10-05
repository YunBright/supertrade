// Package service / stocktake_calc.go —— 服务端计算 expected_remain_kg_by_cut。
//
//   expected_remain_kg_by_cut
//     = opening(昨夜库存结转) + 入库(whole_pig + pig_cuts) - 已销(line_sales_by_pig) - 报损(waste_logs)
//
// 入库口径优先级:
//   1. pig_cuts.weight_kg(仓管实际扫码录入,权威)
//   2. 兜底:whole_pig.llm_advice_json.cuts[].expected_kg(LLM 推演值;若 LLM 未返则 0)
//
// opening 口径(2026-10-01 优化):
//   - 查 is_complete=true PorkCutsStocktake WHERE taken_at ∈ [startOfDay-24h, startOfDay)
//   - 取其 cuts[] 数组的 ActualRemainKg per (cut_type, cube_product_id) 作今日 opening
//   - 无上一日盘 → opening=0
//
// 时区:day 在 bizTZ 下取年月日(默认 Asia/Shanghai,UTC+8);4:30 北京时间进场 → 当日窗口。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// expectedPerCut 是单 cut_type 的中间结果。
type expectedPerCut struct {
	CutType         model.CutType
	OpeningKg       decimal.Decimal
	InKgFromPigCuts decimal.Decimal
	InKgFromLLM     decimal.Decimal
	SoldKg          decimal.Decimal
	WasteKg         decimal.Decimal
	ExpectedRemain  decimal.Decimal
}

// computeExpectedRemainKgByCut 按 branch + bizDay 计算各 cut_type 的 expected_remain_kg。
//
// 数据源:
//   - opening:上一营业日 is_complete=true 盘点的 ActualRemainKg
//   - pig_cuts:仓管扫码补打
//   - llm_advice:全量兜底
//   - sold:line_sales_by_pig.qty_kg(全部,含 R 行的负数,自动抵销)
//   - waste:waste_logs.qty_kg
//
// 返回 map[CutType]decimal.Decimal;缺失 cut 类型返 0。
func (s *Service) computeExpectedRemainKgByCut(ctx context.Context, branchID string, day time.Time) ([]expectedPerCut, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	startOfDay, endOfDay := s.businessDayBounds(day)

	// 0) opening — 昨夜库存结转(上一营业日 is_complete=true)。
	openingPerCut, err := s.loadOpeningPerCut(ctx, branchID, day)
	if err != nil {
		return nil, fmt.Errorf("load opening: %w", err)
	}

	// 1) pig_cuts.weight_kg 按 cut_type 聚合
	pigCutsAgg := make(map[model.CutType]decimal.Decimal)
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
	}

	// 2) whole_pig.llm_advice_json.cuts[].expected_kg 兜底(当 pig_cuts 没录入某 cut 时)
	llmAgg := make(map[model.CutType]decimal.Decimal)
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
			s.pubLogger.Warn("computeExpectedRemainKgByCut: parse llm_advice_json",
				"pig_id", p.ID, "err", err)
			continue
		}
		for _, cut := range advice.Cuts {
			llmAgg[cut.CutType] = llmAgg[cut.CutType].Add(cut.ExpectedKg)
		}
	}

	// 3) line_sales_by_pig.qty_kg 按 cut_type 聚合(扣减;R 行 qty 为负自动抵销)
	soldAgg := make(map[model.CutType]decimal.Decimal)
	var sales []model.LineSalesByPig
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND sale_date >= ? AND sale_date < ?",
			branchID, startOfDay, endOfDay).
		Find(&sales).Error; err != nil {
		return nil, fmt.Errorf("query line_sales_by_pig: %w", err)
	}
	for _, s2 := range sales {
		soldAgg[s2.CutType] = soldAgg[s2.CutType].Add(s2.QtyKg)
	}

	// 4) waste_logs.qty_kg 按 cut_type 聚合(扣减;cut_type 空的不计)
	wasteAgg := make(map[model.CutType]decimal.Decimal)
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
		wasteAgg[w.CutType] = wasteAgg[w.CutType].Add(w.QtyKg)
	}

	// 5) 合并 cut_type 集合(全 12 类)
	cutSet := make(map[model.CutType]struct{})
	for c := range openingPerCut {
		cutSet[c] = struct{}{}
	}
	for c := range pigCutsAgg {
		cutSet[c] = struct{}{}
	}
	for c := range llmAgg {
		cutSet[c] = struct{}{}
	}
	for c := range soldAgg {
		cutSet[c] = struct{}{}
	}
	for c := range wasteAgg {
		cutSet[c] = struct{}{}
	}

	results := make([]expectedPerCut, 0, len(cutSet))
	for c := range cutSet {
		opening := openingPerCut[c]
		inKg := pigCutsAgg[c]
		llmKg := llmAgg[c].Sub(pigCutsAgg[c])
		if llmKg.IsNegative() {
			llmKg = decimal.Zero
		}
		sold := soldAgg[c]
		waste := wasteAgg[c]
		expected := opening.Add(inKg).Add(llmKg).Sub(sold).Sub(waste)
		if expected.IsNegative() {
			expected = decimal.Zero
		}
		results = append(results, expectedPerCut{
			CutType:         c,
			OpeningKg:       opening,
			InKgFromPigCuts: inKg,
			InKgFromLLM:     llmKg,
			SoldKg:          sold,
			WasteKg:         waste,
			ExpectedRemain:  expected,
		})
	}
	return results, nil
}

// loadOpeningPerCut 拉上一营业日 is_complete=true 盘点的 ActualRemainKg,聚合到 cut_type 维度。
//
// 返回 map[CutType]decimal.Decimal;无上一日盘 → 空 map(opening 全部为 0)。
//
// 多 SKU 在同一 cut_type 下求和(per-SKU 在 Pcc 直传入;后续若需 per-SKU 维度,这里改返 map[(cut,sku)]kg)。
func (s *Service) loadOpeningPerCut(ctx context.Context, branchID string, today time.Time) (map[model.CutType]decimal.Decimal, error) {
	prevStart, prevEnd := s.previousBizDayBounds(today)
	var prev model.PorkCutsStocktake
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND taken_at >= ? AND taken_at < ? AND is_complete = ?",
			branchID, prevStart, prevEnd, true).
		Order("taken_at DESC").
		First(&prev).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return map[model.CutType]decimal.Decimal{}, nil
	}
	if err != nil {
		return nil, err
	}
	var snaps []model.CutSnapshot
	if err := json.Unmarshal(prev.Cuts, &snaps); err != nil {
		return nil, fmt.Errorf("unmarshal prev stocktake cuts: %w", err)
	}
	out := make(map[model.CutType]decimal.Decimal, len(snaps))
	for _, s := range snaps {
		out[s.CutType] = out[s.CutType].Add(s.ActualRemainKg)
	}
	return out, nil
}

// snapshotFromCalc 把 expectedPerCut 列表转 CutSnapshot 并填充 caller 传入的 actual_remain_kg。
//
// callerActual 按 cut_type 索引;缺失时 ActualRemainKg = 0(意味着"实际盘 0kg")。
// variance = actual - expected。
func snapshotFromCalc(calc []expectedPerCut, callerActual map[model.CutType]decimal.Decimal) []model.CutSnapshot {
	byCut := make(map[model.CutType]expectedPerCut, len(calc))
	for _, c := range calc {
		byCut[c.CutType] = c
	}

	// 全 cut 集合
	allCuts := make(map[model.CutType]struct{})
	for _, e := range calc {
		allCuts[e.CutType] = struct{}{}
	}
	for c := range callerActual {
		allCuts[c] = struct{}{}
	}

	out := make([]model.CutSnapshot, 0, len(allCuts))
	for c := range allCuts {
		exp := byCut[c]
		actual := callerActual[c]
		variance := actual.Sub(exp.ExpectedRemain)
		out = append(out, model.CutSnapshot{
			CutType:          c,
			ActualRemainKg:   actual,
			ExpectedRemainKg: exp.ExpectedRemain,
			VarianceKg:       variance,
		})
	}
	return out
}

// IsNotFound 工具:检查 err 是不是 record-not-found。
func isNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}

// 静默引用 model(若剥离部分代码)。
var _ = model.WholePig{}