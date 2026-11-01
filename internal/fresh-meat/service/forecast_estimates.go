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
//
// ⚠️ **已停用**(2026-11-01 owner D8:「基线只从销售数据采集样本来计算」)。
// 保留常量是为了让**存量行**仍能被识别(它们还在库里)。
const EstimateSourceStocktakeDerived = "stocktake_derived"

// EstimateSourceSalesDerived 估计样本的来源:由**当日逐部位销量**摊出来的。
//
// # 为什么换掉倒推值(2026-11-01 owner D8)
//
//	旧口径的样本 = 日终倒推值 = 台面实盘 + 销量 + 统收 + 报损 + 搭赠 − 搬上台
//	⇒ **依赖盘点单与搬台登记** ⇒ 事后改搬台 / 改盘点 ⇒ 样本会变
//	⇒ 与 owner 的「**采集即固化**」直接冲突。
//
//	新口径的样本 = 当日逐部位**销量**(cube 的销售流水)
//	⇒ 销量是**事实** ⇒ 改搬台 / 改盘点 / 改猪重都不影响它
//	⇒ 配合「已存在的样本不覆盖」⇒ **采集即固化**成立。
//
// ⚠️ **代价(必须写明,别让它悄悄变成惊喜)**:
// 销量 ≠ 切分产出 —— 柜台上没卖掉的货不在销量里。
// 生产实测(店 00 三天):销量比「卖出 + 柜台剩余」合计少 47.5kg(约占真实产出 7.3%);
// 慢销部位单日可差 40%(leg_rear 10-29:15.88kg vs 27.03kg)
// ⇒ 用销量做样本 ⇒ **基线系统性偏低**(且偏低量随当天剩货量波动)。
const EstimateSourceSalesDerived = "sales_derived"

// SampleSourceRecorded 基线采用的样本池:**实录**(pig_cuts)。
const SampleSourceRecorded = "recorded"

// SampleSourceEstimated 基线采用的样本池:**估计**(pig_cut_estimates)。
const SampleSourceEstimated = "estimated"

// PigCutEstimateID 估计样本的确定性代理主键(同一 (店, 猪, 部位) 只有一行)。
func PigCutEstimateID(branchID, pigID string, cut model.CutType) string {
	return surrogateID("PCE", branchID, pigID, string(cut))
}

// forecastRebuildStats 重建估计样本的结果。
type forecastRebuildStats struct {
	// Written 是**写入/覆盖**的样本条数。
	Written int
	// SkippedExcluded 是**因已被删除（墓碑）而跳过**的条数。
	//
	// ⚠️ 墓碑行**不复活**：门店明确删掉的样本，重建时必须原样保留"已删除"状态
	//   （否则"删除"这个动作会被引擎悄悄撤销 ⇒ 用户以为删了，其实没删）。
	SkippedExcluded int
	// SkippedExisting 是**因"已存在"而跳过**的条数（固化模式）。
	//
	// ⚠️ 这是 owner「采集即固化」的实现点：**已存在的样本一律不动**
	//   ⇒ 事后改搬台 / 改猪重 ⇒ 重物化 ⇒ 样本库原样不动。
	SkippedExisting int
	// PigCount 是当天进场的猪数。
	PigCount int
}

// rebuildEstimatedSplitSamples 用当天**已经算好**的成本行,重建那天的估计样本。
//
// # 为什么复用现成的 rows(而不是自己再跑一遍引擎)
//
//	`persistCutCostDaily` 刚跑完引擎(rows 里已有 opening/实盘/冷库/搬台/实际分割)
//	⇒ 复用它 ⇒ **不多跑一次销售读取**（引擎一次要读 cube 的销售,很贵）。
//
// # overwrite = **固化 vs 重采**（2026-11-01 owner D8「采集即固化」）
//
//	| 模式 | 调用方 | 已有样本 | 墓碑行 | 不再产出的旧行 |
//	|---|---|---|---|---|
//	| overwrite=false | 引擎自动路径(盘点提交 / 重物化) | **不动** | 不复活 | **不删** |
//	| overwrite=true  | 门店显式「按一天加入样本」/「重采」 | 覆盖 | **也不复活**(要先「恢复」) | 删掉 |
//
//	⇒ owner 的原话：「就算修改那天的搬台，猪重都不会影响他的样本库吧，
//	  如果不影响，那就对了。如果样本不对，我们手动删就行。」
//	⇒ 两条都要满足：**自动路径不覆盖** + **样本源不依赖搬台/盘点**（见 [EstimateSourceSalesDerived]）。
//
// # ⚠️ 墓碑行既不复活、也不被删掉（2026-10-30 owner:删除 = 打墓碑）
func (s *Service) rebuildEstimatedSplitSamples(ctx context.Context, branchID string, day time.Time,
	rows []CutDailyCost, overwrite bool) (forecastRebuildStats, error) {
	stats := forecastRebuildStats{}
	if branchID == "" {
		return stats, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	start, end := s.businessDayBounds(day)

	// ① 当天进场的猪（估计样本要摊到猪上 ⇒ 没有猪就没有样本）。
	var pigs []model.WholePig
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND arrived_at >= ? AND arrived_at < ?", branchID, start, end).
		Find(&pigs).Error; err != nil {
		return stats, fmt.Errorf("query whole_pigs(estimates): %w", err)
	}
	stats.PigCount = len(pigs)

	// ② 样本值 = 当日**逐部位销量**(owner 2026-11-01 D8)。
	//
	// ⚠️ 为什么不再用"日终倒推值":
	//   倒推值 = 台面实盘 + 销量 + 统收 + 报损 + 搭赠 − 搬上台
	//   ⇒ **依赖盘点单与搬台登记** ⇒ 事后改搬台 / 改盘点 ⇒ 样本会变
	//   ⇒ 与 owner 的「采集即固化」直接冲突。
	//   销量来自 cube 的销售流水 ⇒ 是**事实** ⇒ 改搬台 / 改盘点都不影响它。
	//
	// ⚠️ 代价见 [EstimateSourceSalesDerived] 的注释(基线会系统性偏低约 7%)。
	//
	// ⚠️ 零售销量 + 统收销量(两者都是"卖掉了")。
	//
	// ②b **有 pig_cuts 实录的部位不产生估计样本**(2026-10-30 硬规矩②:实录更准,
	// 两个池子互补、不打架)。
	//
	// ⚠️ 这里**直接查 pig_cuts**而不是读引擎的 ActualSplitSource
	//   ⇒ 不依赖盘点/倒推 ⇒ 样本的产出口径里没有"盘点"这一环。
	recordedCuts := map[model.CutType]bool{}
	if len(pigs) > 0 {
		pigIDs := make([]string, 0, len(pigs))
		for _, p := range pigs {
			pigIDs = append(pigIDs, p.ID)
		}
		var cuts []model.PigCut
		if err := s.db.WithContext(ctx).
			Where("branch_id = ? AND pig_id IN ?", branchID, pigIDs).
			Find(&cuts).Error; err != nil {
			return stats, fmt.Errorf("query pig_cuts(estimates): %w", err)
		}
		for _, c := range cuts {
			recordedCuts[c.CutType] = true
		}
	}
	sold := map[model.CutType]decimal.Decimal{}
	for _, r := range rows {
		if recordedCuts[r.CutType] {
			continue // 有实录 ⇒ 不估
		}
		kg := r.SoldKg.Add(r.BulkSoldKg)
		if !kg.IsPositive() {
			continue
		}
		sold[r.CutType] = sold[r.CutType].Add(kg)
	}

	// ②' 已删除（墓碑）的样本 ⇒ 固化模式下**跳过**（不复活）。
	excluded, err := s.excludedForecastSampleKeys(ctx, branchID, day)
	if err != nil {
		return stats, err
	}

	// ②'' **已存在**的样本 ID（固化模式要跳过它们）。
	existingIDs := map[string]bool{}
	if !overwrite {
		candidateIDs := make([]string, 0, len(pigs)*len(sold))
		for _, cut := range sortedCutTypes(sold) {
			for _, p := range pigs {
				candidateIDs = append(candidateIDs, PigCutEstimateID(branchID, p.ID, cut))
			}
		}
		if len(candidateIDs) > 0 {
			var existing []model.PigCutEstimate
			if err := s.db.WithContext(ctx).
				Where("branch_id = ? AND id IN ?", branchID, candidateIDs).
				Find(&existing).Error; err != nil {
				return stats, fmt.Errorf("query pig_cut_estimates(existing): %w", err)
			}
			for _, r := range existing {
				existingIDs[r.ID] = true
			}
		}
	}

	// ③ 写样本 + 清掉不再产出的旧行。
	keepIDs := map[string]bool{}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if len(pigs) > 0 && len(sold) > 0 {
			for _, cut := range sortedCutTypes(sold) {
				shares := allocateByGrossWeight(sold[cut], pigs)
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
						Source:     EstimateSourceSalesDerived,
						CreatedAt:  s.now(),
						UpdatedAt:  s.now(),
					}
					key := forecastSampleKey{PigID: p.ID, Cut: cut}
					// ⚠️ **固化**(owner D8「采集即固化」):
					//   引擎自动路径(overwrite=false)⇒ **已存在的样本一律不动**。
					//   ⇒ 事后改搬台 / 改猪重 ⇒ 重物化 ⇒ 样本库**原样不动**。
					if !overwrite && existingIDs[rec.ID] {
						stats.SkippedExisting++
						continue
					}
					// ⚠️ 墓碑行**一律不复活**(2026-10-30 owner:删除 = 打墓碑;
					//   对门店的承诺是「没有自动恢复,点『恢复』可找回」)
					//   ⇒ **显式「加入样本」也不复活** ⇒ 要找回必须先「恢复」
					//   (owner 2026-11-01:「不对就手动删掉重采」⇒ 删除 + 恢复 + 重采)。
					if excluded[key] {
						stats.SkippedExcluded++
						keepIDs[rec.ID] = true // 墓碑行**不能被 stale 清理删掉**
						continue
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
					stats.Written++
				}
			}
		}
		// ⚠️ **固化模式不删任何旧行**:样本是冻结的 ⇒ 删了就等于"改历史 ⇒ 样本库变了"。
		if !overwrite {
			return nil
		}
		// ⚠️ 幂等的关键:那天的旧行(不再产出的部位 / 猪)要**清掉**。
		//   不清 ⇒ 补了实录的部位会同时留着一份旧估计 ⇒ 两个池子重复计样本。
		//
		//   ⚠️ 但**墓碑行不能删**：删了墓碑 ⇒ 下次重建会把它写回来（复活）。
		return s.deleteStaleEstimates(ctx, tx, branchID, day, keepIDs)
	})
	if err != nil {
		return stats, err
	}
	return stats, nil
}

// excludedForecastSampleKeys 读某店那一天**已删除（墓碑）**的样本键。
func (s *Service) excludedForecastSampleKeys(ctx context.Context, branchID string, day time.Time) (
	map[forecastSampleKey]bool, error) {
	out := map[forecastSampleKey]bool{}
	start, end, err := forecastSampleDateRange(day.In(s.bizTZOrUTC()).Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	var rows []model.PigCutEstimate
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND excluded_at IS NOT NULL AND biz_date >= ? AND biz_date < ?",
			branchID, start, end).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query pig_cut_estimates(excluded): %w", err)
	}
	for _, r := range rows {
		out[forecastSampleKey{PigID: r.PigID, Cut: r.CutType}] = true
	}
	return out, nil
}

// deleteStaleEstimates 删掉那天**不在 keepIDs 里**的估计样本行。
//
// ⚠️ **墓碑行不删**（excluded_at IS NULL）：墓碑是"已删除"的留痕
//
//	删掉墓碑 ⇒ 下次重建会把这条样本**写回来** ⇒ 门店的"删除"被引擎悄悄撤销。
func (s *Service) deleteStaleEstimates(ctx context.Context, tx *gorm.DB, branchID string, day time.Time,
	keepIDs map[string]bool) error {
	q := tx.WithContext(ctx).Where("branch_id = ? AND excluded_at IS NULL AND "+s.fmDateColEqExpr("biz_date"),
		branchID, day.In(s.bizTZOrUTC()).Format("2006-01-02"))
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
//
// ⚠️ 这是**显式重采** ⇒ overwrite=true（覆盖已有样本 + 复活墓碑行）。
func (s *Service) RebuildEstimatedSplitSamples(ctx context.Context, branchID string, day time.Time) (int, error) {
	stats, err := s.rebuildForecastDaySamples(ctx, branchID, day, true)
	if err != nil {
		return 0, err
	}
	return stats.Written, nil
}

// rebuildForecastDaySamples 跑某天的引擎 ⇒ 重建那天的估计样本（带明细）。
//
// # 为什么单独一层
//
//	「按一天加入样本」需要知道**写了几条 / 跳过了几条（墓碑）**，
//	而 [RebuildEstimatedSplitSamples] 只回一个条数 ⇒ 这里多跑一层拿明细。
//
// # overwrite 的含义见 [rebuildEstimatedSplitSamples]。
func (s *Service) rebuildForecastDaySamples(ctx context.Context, branchID string,
	day time.Time, overwrite bool) (forecastRebuildStats, error) {
	rows, _, _, _, err := s.computeCutDailyCosts(ctx, branchID, day)
	if err != nil {
		return forecastRebuildStats{}, err
	}
	return s.rebuildEstimatedSplitSamples(ctx, branchID, day, rows, overwrite)
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
