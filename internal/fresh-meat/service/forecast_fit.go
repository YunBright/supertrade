// Package service / forecast_fit.go —— 分割基线的**拟合口径**（2026-10-30 owner 拍板：样本库是一等公民）。
//
// # 为什么改口径
//
//	owner：「基线列表的重算功能就是把**样本库**重新按照算法算一遍产生新的统计回归基线」
//	⇒ 基线是**派生结果**，样本库才是底座。
//	⇒ 拟合要回答两个问题：
//	  ① 「哪些观察算样本」⇒ **合并观察**（实录 ∪ 估计，按 (猪, 部位) 去重，实录优先）
//	  ② 「多少样本才算数」⇒ **有样本就算用**（n ≥ 1，见 [MinForecastSamples]）
//
// # 合并观察（为什么不能两个池子一起算）
//
//	同一头猪的同一部位，实录(`pig_cuts`)与估计(`pig_cut_estimates`)代表**同一件事**
//	⇒ 同时计入 = **重复计样本** ⇒ 均值被拉偏、n 被虚高。
//	⇒ 实录更准 ⇒ **实录优先**，估计只补实录没有的部位。
//
// # 一头猪一个样本（为什么 n 按猪计）
//
//	同一天同一体重段进了两头猪 ⇒ 该分层 **n = 2**。
//	⚠️ 若按「体重」合并，两条样本会互相覆盖 ⇒ n 永远卡在 1 ⇒ 基线永远「样本不足」⇒ 死锁。
package service

import (
	"context"
	"fmt"
	"sort"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
)

// SampleSourceMixed 基线采用的样本池：**实录 + 估计**（合并观察，两个池子都有贡献）。
const SampleSourceMixed = "mixed"

// forecastSampleRow 一条样本观察：**一头猪 × 一个部位** = 一条。
type forecastSampleRow struct {
	PigID string
	Cut   model.CutType
	Kg    decimal.Decimal
	// Band 是这头猪的体重段（分桶用）。
	Band string
	// Source 是这条观察来自哪个池子（recorded | estimated）。
	Source string
}

// forecastSampleKey 一条观察的身份：(猪, 部位)。
//
// ⚠️ 必须是 **(猪, 部位)** 而不是「体重」：
//
//	同段两头猪是**两条**样本（n=2），不是一条。
type forecastSampleKey struct {
	PigID string
	Cut   model.CutType
}

// loadRecordedForecastRows 读某店的**实录样本**（门店手工补录的分割）。
//
// ⚠️ JOIN 时**两边都按 branch_id 过滤**（`pig_cuts.branch_id` + `whole_pigs.branch_id`）
// ⇒ 就算数据脏了（某行的 branch_id 与猪不一致），也不会把别店的猪算进本店的基线。
func (s *Service) loadRecordedForecastRows(ctx context.Context, branchID string) ([]forecastSampleRow, error) {
	var pigCuts []model.PigCut
	if err := s.db.WithContext(ctx).
		Joins("JOIN whole_pigs ON whole_pigs.id = pig_cuts.pig_id").
		Where("pig_cuts.branch_id = ? AND whole_pigs.branch_id = ?", branchID, branchID).
		Find(&pigCuts).Error; err != nil {
		return nil, fmt.Errorf("query pig_cuts: %w", err)
	}
	if len(pigCuts) == 0 {
		return nil, nil
	}
	bandOf, err := s.weightBandOfPigs(ctx, branchID)
	if err != nil {
		return nil, err
	}
	// 同一头猪同一部位可能有多行 ⇒ 先求和 ⇒ **一头猪一个样本**。
	perPigCut := map[string]map[model.CutType]decimal.Decimal{}
	for _, c := range pigCuts {
		if perPigCut[c.PigID] == nil {
			perPigCut[c.PigID] = map[model.CutType]decimal.Decimal{}
		}
		perPigCut[c.PigID][c.CutType] = perPigCut[c.PigID][c.CutType].Add(c.WeightKg)
	}
	out := make([]forecastSampleRow, 0, len(pigCuts))
	for pigID, perCut := range perPigCut {
		band, ok := bandOf[pigID]
		if !ok {
			continue // 猪不在本店 ⇒ 不算本店样本
		}
		for cut, kg := range perCut {
			out = append(out, forecastSampleRow{
				PigID:  pigID,
				Cut:    cut,
				Kg:     kg,
				Band:   band,
				Source: SampleSourceRecorded,
			})
		}
	}
	return sortForecastSampleRows(out), nil
}

// loadEstimatedForecastRows 读某店的**估计样本**（引擎倒推摊出来的）。
//
// ⚠️ **墓碑行（excluded_at 非空）不参与拟合** —— 那是门店明确删掉的样本。
func (s *Service) loadEstimatedForecastRows(ctx context.Context, branchID string) ([]forecastSampleRow, error) {
	var rows []model.PigCutEstimate
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND excluded_at IS NULL", branchID).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query pig_cut_estimates: %w", err)
	}
	out := make([]forecastSampleRow, 0, len(rows))
	for _, r := range rows {
		if r.WeightBand == "" {
			continue // 没有体重段 ⇒ 分不了桶
		}
		out = append(out, forecastSampleRow{
			PigID:  r.PigID,
			Cut:    r.CutType,
			Kg:     r.WeightKg,
			Band:   r.WeightBand,
			Source: SampleSourceEstimated,
		})
	}
	return sortForecastSampleRows(out), nil
}

// weightBandOfPigs 读某店所有猪的体重段（pig_id → 体重段）。
func (s *Service) weightBandOfPigs(ctx context.Context, branchID string) (map[string]string, error) {
	var pigs []model.WholePig
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Find(&pigs).Error; err != nil {
		return nil, fmt.Errorf("query whole_pigs: %w", err)
	}
	out := make(map[string]string, len(pigs))
	for _, p := range pigs {
		out[p.ID] = WeightBandOf(p.GrossWeightKg)
	}
	return out, nil
}

// mergeForecastSampleRows 合并两个样本池：**按 (猪, 部位) 去重，实录优先**。
//
// ⚠️ 顺序稳定（按 体重段 → 猪 → 部位 排）⇒ 写库顺序可复现（便于比对）。
func mergeForecastSampleRows(recorded, estimated []forecastSampleRow) []forecastSampleRow {
	byKey := map[forecastSampleKey]forecastSampleRow{}
	order := make([]forecastSampleKey, 0, len(recorded)+len(estimated))
	// 估计先放 ⇒ 后面实录直接覆盖（实录优先）。
	for _, r := range estimated {
		k := forecastSampleKey{PigID: r.PigID, Cut: r.Cut}
		if _, ok := byKey[k]; !ok {
			order = append(order, k)
		}
		byKey[k] = r
	}
	for _, r := range recorded {
		k := forecastSampleKey{PigID: r.PigID, Cut: r.Cut}
		if _, ok := byKey[k]; !ok {
			order = append(order, k)
		}
		byKey[k] = r
	}
	out := make([]forecastSampleRow, 0, len(order))
	for _, k := range order {
		out = append(out, byKey[k])
	}
	return sortForecastSampleRows(out)
}

// sortForecastSampleRows 按 (体重段, 猪, 部位) 排序（写库顺序稳定 ⇒ 结果可复现）。
func sortForecastSampleRows(rows []forecastSampleRow) []forecastSampleRow {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Band != rows[j].Band {
			return rows[i].Band < rows[j].Band
		}
		if rows[i].PigID != rows[j].PigID {
			return rows[i].PigID < rows[j].PigID
		}
		return string(rows[i].Cut) < string(rows[j].Cut)
	})
	return rows
}

// bucketForecastSampleRows 按 (体重段 × 部位) 分桶。
func bucketForecastSampleRows(rows []forecastSampleRow) map[forecastBucketKey][]forecastSampleRow {
	out := map[forecastBucketKey][]forecastSampleRow{}
	for _, r := range rows {
		k := forecastBucketKey{band: r.Band, cut: r.Cut}
		out[k] = append(out[k], r)
	}
	return out
}

// forecastBucketKeysOf 取一组分层键（排序稳定 ⇒ 写库顺序稳定）。
func forecastBucketKeysOf(buckets map[forecastBucketKey][]forecastSampleRow) []forecastBucketKey {
	out := make([]forecastBucketKey, 0, len(buckets))
	for k := range buckets {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].band != out[j].band {
			return out[i].band < out[j].band
		}
		return string(out[i].cut) < string(out[j].cut)
	})
	return out
}

// forecastFit 一个分层的拟合结果。
type forecastFit struct {
	// N 是**合并后**的样本数（几头猪）。
	N int
	// RecordedN / EstimatedN 是两个池子各自贡献了多少条（留痕用）。
	RecordedN  int
	EstimatedN int
	// Source 是这条基线实际采用的来源：recorded | estimated | mixed。
	Source string
	MeanKg decimal.Decimal
	P10Kg  decimal.Decimal
	P50Kg  decimal.Decimal
	P90Kg  decimal.Decimal
}

// fitForecastSamples 拟合一个分层的统计量。
//
// ⚠️ `sample_source` 会是 **mixed**：实录与估计都有贡献时。
func fitForecastSamples(rows []forecastSampleRow) forecastFit {
	f := forecastFit{N: len(rows)}
	list := make([]decimal.Decimal, 0, len(rows))
	for _, r := range rows {
		list = append(list, r.Kg)
		if r.Source == SampleSourceEstimated {
			f.EstimatedN++
		} else {
			f.RecordedN++
		}
	}
	switch {
	case f.RecordedN > 0 && f.EstimatedN > 0:
		f.Source = SampleSourceMixed
	case f.EstimatedN > 0:
		f.Source = SampleSourceEstimated
	default:
		f.Source = SampleSourceRecorded
	}
	f.MeanKg, f.P10Kg, f.P50Kg, f.P90Kg = statsOf(list)
	return f
}
