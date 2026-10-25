// Package service / forecast.go —— 分割重量的**统计基线**。
//
// # 为什么不用 LLM 做这件事
//
// "300kg 的猪能出多少五花肉"是**数值回归**问题。
// LLM 给的是**语言先验**,不是从样本拟合出来的;而同一体重段的差异来自
// 品种/膘情/季节/师傅手法 —— 这些是**结构化特征**,该用统计/回归。
//
// 实测(2026-10-23):现行 `defaultPredictFn` = `history_avg`,而 history 为空时
// **直接返回 `cuts: []`** ⇒ 生产上 LLM 基本什么都没说 ⇒ 成本摊销基数是 0。
//
// ⇒ **统计基线优先,LLM 降级为"没有历史样本的冷启动兜底"。**
//
// # 口径
//
//	样本 = (whole_pig 的毛重, pig_cuts 的各部位 kg)
//	分层 = 体重段(kg,50 一段)× 部位
//	统计 = n / mean / p10 / p50 / p90
//	样本数不足(默认 5)⇒ 不用该基线 ⇒ 回退 LLM
package service

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MinForecastSamples 基线可用的最小样本数。
//
// 低于它就不信这个分层(样本太少,均值方差都不可靠)⇒ 回退 LLM。
const MinForecastSamples = 5

// weightBandSizeKg 体重段的段宽(kg)。
const weightBandSizeKg = 50

// WeightBandOf 把毛重分到体重段标签(如 "250-299")。
//
// ⚠️ 只按**体重**分层(没按品种):`whole_pigs` 表里**没有品种字段**。
// 品种(隔山 vs 普通)目前只能从 SKU 名上看,拿不到猪级别 ⇒ 暂不维度。
// 将来加了品种字段,这里扩一维即可。
func WeightBandOf(grossKg decimal.Decimal) string {
	kg := grossKg.IntPart()
	if kg < 0 {
		kg = 0
	}
	lo := (kg / weightBandSizeKg) * weightBandSizeKg
	return strconv.FormatInt(lo, 10) + "-" + strconv.FormatInt(lo+weightBandSizeKg-1, 10)
}

// ForecastBaseline 一条基线(服务层用,与 model.PigForecastBaseline 同形)。
type ForecastBaseline struct {
	WeightBand  string
	CutType     model.CutType
	SampleCount int
	MeanKg      decimal.Decimal
	P10Kg       decimal.Decimal
	P50Kg       decimal.Decimal
	P90Kg       decimal.Decimal
}

// RecomputeForecastBaselines 重算某店的分割重量统计基线。
//
// 幂等:按 (branch_id, weight_band, cut_type) upsert。
// 返回写入了多少行。
func (s *Service) RecomputeForecastBaselines(ctx context.Context, branchID string) (int, error) {
	if branchID == "" {
		return 0, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	// 样本:每头猪每个部位一条(先按 pig+cut 汇总)。
	type sample struct {
		band string
		cut  model.CutType
		kg   decimal.Decimal
	}
	var pigCuts []model.PigCut
	if err := s.db.WithContext(ctx).
		Joins("JOIN whole_pigs ON whole_pigs.id = pig_cuts.pig_id").
		Where("pig_cuts.branch_id = ?", branchID).
		Find(&pigCuts).Error; err != nil {
		return 0, fmt.Errorf("query pig_cuts: %w", err)
	}
	if len(pigCuts) == 0 {
		return 0, nil
	}
	var pigs []model.WholePig
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Find(&pigs).Error; err != nil {
		return 0, fmt.Errorf("query whole_pigs: %w", err)
	}
	bandOf := map[string]string{}
	for _, p := range pigs {
		bandOf[p.ID] = WeightBandOf(p.GrossWeightKg)
	}
	// pig+cut → kg
	perPigCut := map[string]map[model.CutType]decimal.Decimal{}
	for _, c := range pigCuts {
		if perPigCut[c.PigID] == nil {
			perPigCut[c.PigID] = map[model.CutType]decimal.Decimal{}
		}
		perPigCut[c.PigID][c.CutType] = perPigCut[c.PigID][c.CutType].Add(c.WeightKg)
	}
	type key struct {
		band string
		cut  model.CutType
	}
	buckets := map[key][]decimal.Decimal{}
	for pigID, perCut := range perPigCut {
		band, ok := bandOf[pigID]
		if !ok {
			continue
		}
		for cut, kg := range perCut {
			buckets[key{band, cut}] = append(buckets[key{band, cut}], kg)
		}
	}

	now := s.now()
	written := 0
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for k, list := range buckets {
			mean, p10, p50, p90 := statsOf(list)
			rec := model.PigForecastBaseline{
				ID:          fmt.Sprintf("PFB%s%s%s", k.band, strings.ReplaceAll(string(k.cut), "_", "-"), branchID),
				BranchID:    branchID,
				WeightBand:  k.band,
				CutType:     k.cut,
				SampleCount: len(list),
				MeanKg:      mean,
				P10Kg:       p10,
				P50Kg:       p50,
				P90Kg:       p90,
				ComputedAt:  now,
			}
			if err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "branch_id"}, {Name: "weight_band"}, {Name: "cut_type"}},
				DoUpdates: clause.Assignments(map[string]any{
					"sample_count": len(list),
					"mean_kg":      mean,
					"p10_kg":       p10,
					"p50_kg":       p50,
					"p90_kg":       p90,
					"computed_at":  now,
				}),
			}).Create(&rec).Error; err != nil {
				return err
			}
			written++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return written, nil
}

// statsOf 算一组样本的 mean / p10 / p50 / p90。
func statsOf(list []decimal.Decimal) (mean, p10, p50, p90 decimal.Decimal) {
	if len(list) == 0 {
		return decimal.Zero, decimal.Zero, decimal.Zero, decimal.Zero
	}
	sorted := make([]decimal.Decimal, len(list))
	copy(sorted, list)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].LessThan(sorted[j]) })
	sum := decimal.Zero
	for _, v := range sorted {
		sum = sum.Add(v)
	}
	mean = sum.Div(decimal.NewFromInt(int64(len(sorted))))
	p10 = percentile(sorted, 0.10)
	p50 = percentile(sorted, 0.50)
	p90 = percentile(sorted, 0.90)
	return mean, p10, p50, p90
}

// percentile 取分位数(线性插值)。
func percentile(sorted []decimal.Decimal, q float64) decimal.Decimal {
	n := len(sorted)
	if n == 0 {
		return decimal.Zero
	}
	if n == 1 {
		return sorted[0]
	}
	pos := q * float64(n-1)
	lo := int(pos)
	hi := lo + 1
	if hi >= n {
		return sorted[n-1]
	}
	frac := decimal.NewFromFloat(pos - float64(lo))
	return sorted[lo].Add(sorted[hi].Sub(sorted[lo]).Mul(frac))
}

// loadForecastBaselines 读某店的全部基线,索引 = band + "|" + cut_type。
func (s *Service) loadForecastBaselines(ctx context.Context, branchID string) (map[string]ForecastBaseline, error) {
	out := map[string]ForecastBaseline{}
	var rows []model.PigForecastBaseline
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query pig_forecast_baselines: %w", err)
	}
	for _, r := range rows {
		out[baselineKey(r.WeightBand, r.CutType)] = ForecastBaseline{
			WeightBand:  r.WeightBand,
			CutType:     r.CutType,
			SampleCount: r.SampleCount,
			MeanKg:      r.MeanKg,
			P10Kg:       r.P10Kg,
			P50Kg:       r.P50Kg,
			P90Kg:       r.P90Kg,
		}
	}
	return out, nil
}

// baselineKey 基线索引键。
func baselineKey(band string, cut model.CutType) string { return band + "|" + string(cut) }

// ListForecastBaselines 列某店的全部基线(给后台查看用)。
func (s *Service) ListForecastBaselines(ctx context.Context, branchID string) ([]model.PigForecastBaseline, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	var rows []model.PigForecastBaseline
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("weight_band ASC, cut_type ASC").
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query pig_forecast_baselines: %w", err)
	}
	return rows, nil
}

// ForecastFor 查某头猪某部位的预测产出(优先 p50,样本不足返零 + false)。
func (b ForecastBaseline) Usable() bool { return b.SampleCount >= MinForecastSamples }

// ForecastForWeight 查某体重段某部位的基线预测。
func (s *Service) ForecastFor(ctx context.Context, branchID string, grossKg decimal.Decimal, cut model.CutType) (ForecastBaseline, bool, error) {
	all, err := s.loadForecastBaselines(ctx, branchID)
	if err != nil {
		return ForecastBaseline{}, false, err
	}
	b, ok := all[baselineKey(WeightBandOf(grossKg), cut)]
	if !ok || !b.Usable() {
		return ForecastBaseline{}, false, nil
	}
	return b, true, nil
}
