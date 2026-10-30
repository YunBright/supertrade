// Package service / forecast_estimates.go —— 分割基线的**估计样本**通道（2026-10-30 owner 选的 A 方案）。
//
// # 背景：为什么门店的"建议分割"会偏
//
//	建议分割 S_c = 统计基线 p50 优先、LLM 兜底（见 cut_split_algo.go::loadSuggestedSplitKg）。
//	而统计基线的样本来自 `pig_cuts` —— **门店手工补录的分割实录**。
//
//	⇒ 门店**从没补录过**（生产实测：店 00 的 `pig_cuts` = 0 行、`pig_forecast_baselines` = 0 行）
//	  ⇒ 基线永远不够样本 ⇒ 建议分割**全部**来自 LLM
//	  ⇒ 那是**语言先验**（通用出肉率），不是这家店的真实出肉率
//	  ⇒ 建议分割系统性偏 ⇒ W_c 偏 ⇒ 销售成本偏 ⇒ 毛利偏。
//
// # A 方案：用**倒推值**当估计样本
//
//	门店不补录,但每天都有**日终实盘** ⇒ 全店恒等式能解出"今日新分割"：
//
//		A_c = 全店期末结余 + 销售 + 统收 + 报损 − 全店期初
//
//	⇒ 这个 A_c 是**部位级**的当日总量 ⇒ 按当日各猪的**毛重比例**摊到每头猪
//	  ⇒ 每头猪每个部位一条**估计样本** ⇒ 喂基线 ⇒ 建议分割开始反映本店真实出肉率。
//
// # 三条硬规矩
//
//	① **只信"有完整日终盘点"的倒推**：没有完整盘点单时，倒推是"台面流量近似"
//	   （搬上台是老货 ⇒ 只能扣掉估计）⇒ 噪声太大 ⇒ **不当样本**。
//	② **只喂 derived（倒推）部位**：有 `pig_cuts` 实录的部位，实录本身就是更准的样本
//	   ⇒ 估计样本只补"没有实录"的部位 ⇒ 两个池子互补、不打架。
//	③ **独立通道**：估计样本**不写进 `pig_cuts`**（会污染 FIFO 摊销 + 锁死删猪）。
//
// # 门店隔离
//
//	`pig_cut_estimates` 的唯一键含 branch_id ⇒ **每店一份样本**
//	⇒ 每店生成**自己的**基线 ⇒ 互不影响（owner 2026-10-30 要求）。
package service

import (
	"context"
	"fmt"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// EstimateSourceStocktakeDerived 估计样本的来源:由日终倒推值摊出来的。
const EstimateSourceStocktakeDerived = "stocktake_derived"

// SampleSourceRecorded 基线采用的样本池:**实录**(pig_cuts)。
const SampleSourceRecorded = "recorded"

// SampleSourceEstimated 基线采用的样本池:**估计**(pig_cut_estimates)。
const SampleSourceEstimated = "estimated"

// PigCutEstimateID 估计样本的确定性代理主键(同一 (店, 猪, 部位) 只有一行)。
func PigCutEstimateID(branchID, pigID string, cut model.CutType) string {
	return surrogateID("PCE", branchID, pigID, string(cut))
}

// rebuildEstimatedSplitSamples 用当天**已经算好**的成本行,重建那天的估计样本。
//
// # 为什么复用现成的 rows(而不是自己再跑一遍引擎)
//
//	`persistCutCostDaily` 刚跑完引擎(rows 里已有 opening/实盘/冷库/搬台/实际分割)
//	⇒ 复用它 ⇒ **不多跑一次销售读取**（引擎一次要读 cube 的销售,很贵）。
//
// # 幂等
//
//	同一天重复调用 ⇒ 同 (店, 猪, 部位) 覆盖 ⇒ 结果不变。
//	⚠️ 还要**删掉不再产出的旧行**（例如某部位今天补了实录 ⇒ 不再是 derived ⇒ 它的估计样本要消失）。
func (s *Service) rebuildEstimatedSplitSamples(ctx context.Context, branchID string, day time.Time,
	rows []CutDailyCost) (int, error) {
	if branchID == "" {
		return 0, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	start, end := s.businessDayBounds(day)

	// ① 当天进场的猪（估计样本要摊到猪上 ⇒ 没有猪就没有样本）。
	var pigs []model.WholePig
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND arrived_at >= ? AND arrived_at < ?", branchID, start, end).
		Find(&pigs).Error; err != nil {
		return 0, fmt.Errorf("query whole_pigs(estimates): %w", err)
	}

	// ② 从成本行里挑出"可信的倒推值"。
	//
	// ⚠️ 两个条件缺一不可:
	//   - DataSource == actual ⇒ 当天有**完整**日终盘点（否则倒推只是近似）;
	//   - ActualSplitSource == derived ⇒ 该部位是**倒推**的（有实录的部位不用估）。
	derived := map[model.CutType]decimal.Decimal{}
	for _, r := range rows {
		// ⚠️ **只信有完整日终盘点的倒推**(硬规矩①):
		//   没有完整盘点单时,倒推走的是"台面流量近似"(搬上台是老货 ⇒ 只能扣掉估计)
		//   ⇒ 噪声太大 ⇒ 不当样本(会把基线教歪)。
		if r.DataSource != dataSourceActual {
			continue
		}
		if r.ActualSplitSource != ActualSplitSourceDerived {
			continue
		}
		if r.ActualSplitKg == nil || !r.ActualSplitKg.IsPositive() {
			continue
		}
		derived[r.CutType] = *r.ActualSplitKg
	}

	// ③ 写样本 + 清掉不再产出的旧行。
	written := 0
	keepIDs := map[string]bool{}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if len(pigs) > 0 && len(derived) > 0 {
			for _, cut := range sortedCutTypes(derived) {
				shares := allocateByGrossWeight(derived[cut], pigs)
				for i, p := range pigs {
					if !shares[i].IsPositive() {
						continue
					}
					rec := model.PigCutEstimate{
						ID:         PigCutEstimateID(branchID, p.ID, cut),
						BranchID:   branchID,
						PigID:      p.ID,
						CutType:    cut,
						WeightKg:   shares[i],
						WeightBand: WeightBandOf(p.GrossWeightKg),
						BizDate:    datatypesDate(day.In(s.bizTZOrUTC())),
						Source:     EstimateSourceStocktakeDerived,
						CreatedAt:  s.now(),
						UpdatedAt:  s.now(),
					}
					if err := tx.Clauses(clause.OnConflict{
						Columns: []clause.Column{{Name: "id"}},
						DoUpdates: clause.Assignments(map[string]any{
							"weight_kg":   rec.WeightKg,
							"weight_band": rec.WeightBand,
							"biz_date":    rec.BizDate,
							"source":      rec.Source,
							"updated_at":  rec.UpdatedAt,
						}),
					}).Create(&rec).Error; err != nil {
						return fmt.Errorf("upsert pig_cut_estimate: %w", err)
					}
					keepIDs[rec.ID] = true
					written++
				}
			}
		}
		// ⚠️ 幂等的关键:那天的旧行(不再产出的部位 / 猪)要**清掉**。
		//   不清 ⇒ 补了实录的部位会同时留着一份旧估计 ⇒ 两个池子重复计样本。
		return s.deleteStaleEstimates(ctx, tx, branchID, day, keepIDs)
	})
	if err != nil {
		return 0, err
	}
	return written, nil
}

// deleteStaleEstimates 删掉那天**不在 keepIDs 里**的估计样本行。
func (s *Service) deleteStaleEstimates(ctx context.Context, tx *gorm.DB, branchID string, day time.Time,
	keepIDs map[string]bool) error {
	q := tx.WithContext(ctx).Where("branch_id = ? AND biz_date = ?", branchID, datatypesDate(day.In(s.bizTZOrUTC())))
	if len(keepIDs) > 0 {
		ids := make([]string, 0, len(keepIDs))
		for id := range keepIDs {
			ids = append(ids, id)
		}
		q = q.Where("id NOT IN ?", ids)
	}
	if err := q.Delete(&model.PigCutEstimate{}).Error; err != nil {
		return fmt.Errorf("delete stale pig_cut_estimates: %w", err)
	}
	return nil
}

// RebuildEstimatedSplitSamples 重建某店某营业日的估计样本（自己跑引擎）。
//
// ⚠️ 代价:跑一次引擎要读 cube 的销售 ⇒ 不便宜 ⇒ 只在**重算基线**时按需调用。
func (s *Service) RebuildEstimatedSplitSamples(ctx context.Context, branchID string, day time.Time) (int, error) {
	rows, _, _, _, err := s.computeCutDailyCosts(ctx, branchID, day)
	if err != nil {
		return 0, err
	}
	return s.rebuildEstimatedSplitSamples(ctx, branchID, day, rows)
}

// loadEstimatedSplitSamples 读某店的**估计样本**（按 pig + 部位汇总）。
//
// # 返回
//
//	perPigCut: pig_id → 部位 → kg（同一头猪同一部位可能有多行 ⇒ 求和）
//	bandOf:    pig_id → 体重段
//
// ⚠️ **按 branch_id 过滤** ⇒ 门店隔离（owner 2026-10-30 的硬要求）。
func (s *Service) loadEstimatedSplitSamples(ctx context.Context, branchID string) (
	map[string]map[model.CutType]decimal.Decimal, map[string]string, error) {
	perPigCut := map[string]map[model.CutType]decimal.Decimal{}
	bandOf := map[string]string{}
	var rows []model.PigCutEstimate
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Find(&rows).Error; err != nil {
		return nil, nil, fmt.Errorf("query pig_cut_estimates: %w", err)
	}
	for _, r := range rows {
		if perPigCut[r.PigID] == nil {
			perPigCut[r.PigID] = map[model.CutType]decimal.Decimal{}
		}
		perPigCut[r.PigID][r.CutType] = perPigCut[r.PigID][r.CutType].Add(r.WeightKg)
		bandOf[r.PigID] = r.WeightBand
	}
	return perPigCut, bandOf, nil
}

// allocateByGrossWeight 把 totalKg 按各猪**毛重比例**摊到每头猪。
//
// # 为什么按毛重
//
//	⚠️ 不能按"建议分割"摊（那是循环论证：建议会复制自己）。
//	⚠️ 也不能按"上次实录"摊（实录本来就是另一个池子）。
//	⇒ 毛重是**唯一独立于本算法的体量信号** ⇒ 大猪出得多,小猪出得少。
//
// # 尾差
//
//	除法有精度(16 位)⇒ 各份直接相加可能差 1e-16
//	⇒ **最大那头猪的份额 = 总量 − 其余各份之和**(而不是"加个差额")
//	⇒ 十进制加减是**精确**的 ⇒ Σ 恰好等于 totalKg(基线样本不能凭空多/少)。
func allocateByGrossWeight(totalKg decimal.Decimal, pigs []model.WholePig) []decimal.Decimal {
	out := make([]decimal.Decimal, len(pigs))
	if len(pigs) == 0 || !totalKg.IsPositive() {
		return out
	}
	var sumGross decimal.Decimal
	for _, p := range pigs {
		if p.GrossWeightKg.IsPositive() {
			sumGross = sumGross.Add(p.GrossWeightKg)
		}
	}
	if !sumGross.IsPositive() {
		return out // 毛重全 0 ⇒ 摊不了（不猜）
	}
	biggest := 0
	for i, p := range pigs {
		if p.GrossWeightKg.GreaterThan(pigs[biggest].GrossWeightKg) {
			biggest = i
		}
	}
	var others decimal.Decimal
	for i, p := range pigs {
		if i == biggest {
			continue
		}
		if !p.GrossWeightKg.IsPositive() {
			continue
		}
		out[i] = totalKg.Mul(p.GrossWeightKg).Div(sumGross)
		others = others.Add(out[i])
	}
	// 最大那头拿"剩下的"(精确减法 ⇒ Σ 恰好 = totalKg)。
	out[biggest] = totalKg.Sub(others)
	return out
}

// sortedCutTypes 把部位集合按 wire 值排好序（写库顺序稳定 ⇒ 好查、好比对）。
func sortedCutTypes(m map[model.CutType]decimal.Decimal) []model.CutType {
	out := make([]model.CutType, 0, len(m))
	for c := range m {
		out = append(out, c)
	}
	for i := 1; i < len(out); i++ {
		j := i
		for j > 0 && string(out[j-1]) > string(out[j]) {
			out[j-1], out[j] = out[j], out[j-1]
			j--
		}
	}
	return out
}
