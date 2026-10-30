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
)

// MinForecastSamples 基线可用的最小样本数。
//
// # ⚠️ 2026-10-30 owner 拍板：**有样本就算用（n ≥ 1）**
//
//	owner：「如果每天有两头猪，那么就有两个重量样本」
//	⇒ 样本库是一等公民，基线是派生结果 ⇒ 门槛的唯一作用是"有没有样本"，
//	  而不是"样本够不够稳"（样本稳不稳由**删样本**这个动作管）。
//
// ⇒ 之前是 5（样本不够就回退 LLM）；现在改成 1：
//
//	① 「每段两头猪 = n=2」这个模型**必须**有门槛 < 2 才成立
//	  （门槛 5 ⇒ 新店永远不够 ⇒ 永远回退 LLM ⇒ 死锁）；
//	② `manual_generated` 的**豁免机制随之退役**（见 [ForecastBaseline.Usable]）。
//
// ⚠️ 代价（有意）：只要有 1 条样本，该分层就用基线、不再回退 LLM
// ⇒ 样本质量的责任落到「删除不准确的样本」这个动作上。
const MinForecastSamples = 1

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
	// RecordedSampleCount / EstimatedSampleCount 是两个**样本池**各自的样本数。
	RecordedSampleCount  int
	EstimatedSampleCount int
	// SampleSource:recorded | estimated —— 这条基线**实际采用**的是哪个池子。
	SampleSource string
	// SourceBranchID 是样本来源门店(跨店借用时 = 被借方;自产 = 本店)。
	SourceBranchID string
	// ManualGenerated = true ⇒ 管理员**按某一天手动生成**的基线
	// ⇒ 绕过 MinForecastSamples 的可用性门槛(见 Usable)。
	ManualGenerated bool
	// GeneratedFromDate 是手动生成时用的那一天("YYYY-MM-DD");空 = 不是手动生成。
	GeneratedFromDate string
	MeanKg            decimal.Decimal
	P10Kg             decimal.Decimal
	P50Kg             decimal.Decimal
	P90Kg             decimal.Decimal
}

// forecastBucketKey 基线的分桶键:**体重段 × 部位**。
type forecastBucketKey struct {
	band string
	cut  model.CutType
}

// RecomputeForecastBaselines 重算某店的分割重量统计基线。
//
// # 幂等:按 (branch_id, weight_band, cut_type) 落一行。
// 返回写入了多少行。
//
// # 口径（2026-10-30 owner 拍板：样本库是一等公民,基线是派生结果）
//
//	① **样本** = 合并观察（实录 ∪ 估计，按 (猪, 部位) 去重、实录优先）
//	② **分层** = 体重段 × 部位
//	③ **可用门槛** = n ≥ 1（有样本就算用,见 [MinForecastSamples]）
//
//	⇒ **覆盖所有分层**：不再跳过 `manual_generated` 的行
//	  （基线是**派生物** ⇒ 重算就该把它算回去）。
//	  `manual_generated` 列保留为"曾手工触发"的可追溯标记（重算时不清零）。
//
// # 跨店借用(owner 2026-10-30 第 2 条)
//
//	本店某个分层**自己一个样本都没有** ⇒ 采用**被借店**同一 (体重段, 部位)那一行的数字,
//	并把 source_branch_id 记成被借方。
//
//	被借店那一行自己也不可用 ⇒ **不借用**(回退 LLM)。
//
// # ⚠️ 样本被删光 ⇒ 该层基线行随之删除
//
//	否则脏数字会一直给出错建议（owner:「如果觉得它不准,分割不合理」）。
//	判据:本次没写出来、但原来 sample_count > 0 的**自产**行 ⇒ 删。
//	（sample_count = 0 的历史行不动 —— 那些本来不可用,留着无害。）
//
// # ⚠️ **每个店只用自己的样本**(owner 2026-10-30:「每个门店样本要单独」)
//
//	⇒ 样本查询**都按 branch_id 过滤** ⇒ 互不影响。
func (s *Service) RecomputeForecastBaselines(ctx context.Context, branchID string) (int, error) {
	if branchID == "" {
		return 0, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	// ---- ① 样本(实录 ∪ 估计 → 合并观察) ----
	//
	// ⚠️ **事务外读**:SQLite 单连接库在事务里读会死锁(见 db.go 的 MaxOpenConns(1))。
	recorded, err := s.loadRecordedForecastRows(ctx, branchID)
	if err != nil {
		return 0, err
	}
	estimated, err := s.loadEstimatedForecastRows(ctx, branchID)
	if err != nil {
		return 0, err
	}
	merged := mergeForecastSampleRows(recorded, estimated)
	buckets := bucketForecastSampleRows(merged)

	// ---- ② 跨店借用解析(在事务外读,避免 SQLite 单连接死锁) ----
	borrowed, err := s.resolveBorrowedBaselines(ctx, branchID)
	if err != nil {
		return 0, err
	}
	// 本店没样本、但被借店有的分层 ⇒ 也要建行(否则整店借用没意义)。
	keys := mergeForecastBucketKeys(forecastBucketKeysOf(buckets), borrowedBucketKeys(borrowed))

	// ---- ③ 逐桶拟合 + 写库 ----
	now := s.now()
	written := 0
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 已有行的主键(先读后写,见 upsertForecastBaseline 的原因)。
		existing, err := loadBaselineRows(ctx, tx, branchID)
		if err != nil {
			return err
		}
		writtenKeys := map[forecastBucketKey]bool{}
		for _, k := range keys {
			samples := buckets[k]
			var rec model.PigForecastBaseline
			switch {
			case len(samples) > 0:
				// 本店有样本 ⇒ 用自己的(门槛 n ≥ 1 ⇒ 有样本就算用)。
				fit := fitForecastSamples(samples)
				rec = model.PigForecastBaseline{
					ID:                   forecastBaselineID(branchID, k.band, k.cut),
					BranchID:             branchID,
					WeightBand:           k.band,
					CutType:              k.cut,
					SampleCount:          fit.N,
					RecordedSampleCount:  fit.RecordedN,
					EstimatedSampleCount: fit.EstimatedN,
					SampleSource:         fit.Source,
					SourceBranchID:       branchID,
					MeanKg:               fit.MeanKg,
					P10Kg:                fit.P10Kg,
					P50Kg:                fit.P50Kg,
					P90Kg:                fit.P90Kg,
					ComputedAt:           now,
				}
				// ⚠️ 保留"曾手工触发"标记(基线是派生物,但这行**曾经**被人工触发过 ⇒ 留痕)。
				if prev, ok := existing[k]; ok {
					rec.ManualGenerated = prev.ManualGenerated
					rec.GeneratedFromDate = prev.GeneratedFromDate
				}
			default:
				// 本店这个分层**一个样本都没有** ⇒ 看能不能借用被借店的数字。
				b, ok := borrowed[k]
				if !ok {
					continue
				}
				rec = b
				rec.BranchID = branchID
				// ⚠️ **主键必须重算**:被借店那一行带的是**它自己的** id
				//   ⇒ 直接写会撞主键(两行 id 相同)⇒ 借用永远写不进去。
				rec.ID = forecastBaselineID(branchID, k.band, k.cut)
				rec.ComputedAt = now
				// ★ 标出这是**借来的**行(见 model.PigForecastBaseline.Borrowed):
				//   借用配置一关/一改,它必须能被刷新、也能被清掉。
				rec.Borrowed = true
				// ⚠️ "曾手工触发"是**本店**的留痕 ⇒ 借来的行也要保留它
				//   (它记录的是"本店这一层曾经被人工设过",与数字来自谁无关)。
				//   ⚠️ 但 `generated_from_date` 同理保留(可追溯)。
				if prev, ok := existing[k]; ok {
					rec.ManualGenerated = prev.ManualGenerated
					rec.GeneratedFromDate = prev.GeneratedFromDate
				}
			}
			if err := s.upsertForecastBaseline(ctx, tx, rec, existing); err != nil {
				return err
			}
			written++
			writtenKeys[k] = true
		}
		// ---- 清理 ----
		//
		// ① **借来的**旧行:借用被关掉(或被借方换了)⇒ 那一行必须消失,
		//    否则本店会一直用别家的旧数字 ⇒「关掉就回到只用本店样本」这条语义就废了。
		// ② **样本被删光**的自产行:原来有样本、这次没写出来 ⇒ 该层失效 ⇒ 删。
		//
		// ⚠️ sample_count = 0 的历史行**不动**(本来不可用,留着无害)。
		for k, prev := range existing {
			if writtenKeys[k] {
				continue
			}
			if prev.Borrowed {
				if err := tx.WithContext(ctx).Where("id = ?", prev.ID).
					Delete(&model.PigForecastBaseline{}).Error; err != nil {
					return fmt.Errorf("delete borrowed pig_forecast_baseline %s: %w", prev.ID, err)
				}
				continue
			}
			if prev.SampleCount > 0 {
				if err := tx.WithContext(ctx).Where("id = ?", prev.ID).
					Delete(&model.PigForecastBaseline{}).Error; err != nil {
					return fmt.Errorf("delete stale pig_forecast_baseline %s: %w", prev.ID, err)
				}
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return written, nil
}

// forecastBaselineID 基线行的**确定性**代理主键。
//
// ⚠️ 必须是**确定性**的:purge 按"单列主键"删行(见 handler/purge.go)
// ⇒ 同一 (店, 体重段, 部位) 永远算出同一个 id ⇒ 重算/覆盖都不会让 purge 删不到行。
func forecastBaselineID(branchID, weightBand string, cut model.CutType) string {
	return fmt.Sprintf("PFB%s%s%s", weightBand, strings.ReplaceAll(string(cut), "_", "-"), branchID)
}

// loadBaselineRows 读本店的已有基线行(索引 = 分层键)。
//
// ⚠️ 用它做"先读后写":命中已有行就 UPDATE(**它自己的主键**),否则 INSERT。
func loadBaselineRows(ctx context.Context, tx *gorm.DB, branchID string) (map[forecastBucketKey]model.PigForecastBaseline, error) {
	out := map[forecastBucketKey]model.PigForecastBaseline{}
	var rows []model.PigForecastBaseline
	if err := tx.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query pig_forecast_baselines(ids): %w", err)
	}
	for _, r := range rows {
		out[forecastBucketKey{band: r.WeightBand, cut: r.CutType}] = r
	}
	return out, nil
}

// upsertForecastBaseline 写一行基线(按 (门店, 体重段, 部位) 定位)。
//
// # ⚠️ 为什么**先读后写**而不是 ON CONFLICT DO UPDATE
//
//	ON CONFLICT (业务唯一键) DO UPDATE 在 PG 上**不会**报主键冲突(2026-10-30 本地 PG 实测:
//	主键与业务唯一键同时撞时照样 INSERT 0 1)⇒ **但它保留的是旧行的主键**。
//
//	⇒ 如果本次算出来的 id 与库里那一行的 id **不一致**(例如借来的行/历史行),
//	  DO UPDATE 之后库里仍是**旧 id**,而溯源记账记的是"本次 INSERT 里的 id"
//	  ⇒ purge 拿着一个**不存在的 id** 去删 ⇒ 删不到任何行 ⇒ 残留照旧、零报错。
//	  (这正是本仓已记录的坑:复合/随机主键 + ON CONFLICT ⇒ 残留。)
//
//	⇒ 改成:命中已有行就按**它自己的主键** UPDATE,否则 INSERT(确定性 id)
//	  ⇒ 记账记的 id 一定是库里真实存在的那个。
func (s *Service) upsertForecastBaseline(ctx context.Context, tx *gorm.DB, row model.PigForecastBaseline,
	existing map[forecastBucketKey]model.PigForecastBaseline) error {
	key := forecastBucketKey{band: row.WeightBand, cut: row.CutType}
	fields := map[string]any{
		"sample_count":           row.SampleCount,
		"recorded_sample_count":  row.RecordedSampleCount,
		"estimated_sample_count": row.EstimatedSampleCount,
		"sample_source":          row.SampleSource,
		"source_branch_id":       row.SourceBranchID,
		"manual_generated":       row.ManualGenerated,
		"generated_from_date":    row.GeneratedFromDate,
		"borrowed":               row.Borrowed,
		"mean_kg":                row.MeanKg,
		"p10_kg":                 row.P10Kg,
		"p50_kg":                 row.P50Kg,
		"p90_kg":                 row.P90Kg,
		"computed_at":            row.ComputedAt,
	}
	if prev, ok := existing[key]; ok && prev.ID != "" {
		if err := tx.WithContext(ctx).Model(&model.PigForecastBaseline{}).
			Where("id = ?", prev.ID).Updates(fields).Error; err != nil {
			return fmt.Errorf("update pig_forecast_baseline %s: %w", prev.ID, err)
		}
		return nil
	}
	if err := tx.WithContext(ctx).Create(&row).Error; err != nil {
		return fmt.Errorf("create pig_forecast_baseline: %w", err)
	}
	existing[key] = row
	return nil
}

// mergeForecastBucketKeys 合并多组分层键并**排序**。
//
// ⚠️ 排序是必须的:借用会把"本店没样本、但被借店有"的分层带进来
// ⇒ 不排序 ⇒ 写库顺序随 map 迭代随机 ⇒ 重算结果不可复现(难查)。
func mergeForecastBucketKeys(lists ...[]forecastBucketKey) []forecastBucketKey {
	seen := map[forecastBucketKey]bool{}
	out := []forecastBucketKey{}
	for _, l := range lists {
		for _, k := range l {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].band != out[j].band {
			return out[i].band < out[j].band
		}
		return string(out[i].cut) < string(out[j].cut)
	})
	return out
}

// borrowedBucketKeys 取"借来的"那些分层键(用于并进本店的分层集合)。
func borrowedBucketKeys(borrowed map[forecastBucketKey]model.PigForecastBaseline) []forecastBucketKey {
	out := make([]forecastBucketKey, 0, len(borrowed))
	for k := range borrowed {
		out = append(out, k)
	}
	return out
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
			WeightBand:           r.WeightBand,
			CutType:              r.CutType,
			SampleCount:          r.SampleCount,
			RecordedSampleCount:  r.RecordedSampleCount,
			EstimatedSampleCount: r.EstimatedSampleCount,
			SampleSource:         r.SampleSource,
			SourceBranchID:       r.SourceBranchID,
			ManualGenerated:      r.ManualGenerated,
			GeneratedFromDate:    r.GeneratedFromDate,
			MeanKg:               r.MeanKg,
			P10Kg:                r.P10Kg,
			P50Kg:                r.P50Kg,
			P90Kg:                r.P90Kg,
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

// Usable 判断这条基线**能不能用**。
//
// # 判据：`sample_count >= MinForecastSamples`（= 1）
//
// ⚠️ **原来的 `manual_generated` 豁免已退役**（2026-10-30 owner 拍板）：
//
//	基线是**派生结果** ⇒ 有样本就算用 ⇒ "手动生成"不再是"绕过门槛"的理由，
//	它只是"这一层曾经被人工触发过"的**留痕**（见 model.PigForecastBaseline.ManualGenerated）。
//
//	⇒ n = 0 的行（迁移默认值）**不可用** ⇒ 回退 LLM。
func (b ForecastBaseline) Usable() bool {
	return b.SampleCount >= MinForecastSamples
}

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
