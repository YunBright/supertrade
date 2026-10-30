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
// 幂等:按 (branch_id, weight_band, cut_type) 落一行。
// 返回写入了多少行。
//
// # 两个样本池(2026-10-30 owner 选的 A 方案)
//
//	① **实录池** = `pig_cuts`(门店手工补录的分割)—— 最准;
//	② **估计池** = `pig_cut_estimates`(由日终倒推值按毛重比例摊到每头猪)—— 近似。
//
// ⇒ 口径:**实录样本够(≥ MinForecastSamples)就用实录**,不够才用估计。
//
//	两个池子都不够 ⇒ 这条分层不可用 ⇒ 回退 LLM。
//
// ⚠️ **每个店只用自己的样本**(owner 2026-10-30:「每个门店样本要单独」)
//
//	⇒ 两个池子的查询**都按 branch_id 过滤** ⇒ 互不影响。
//
// # 跨店借用(owner 2026-10-30 第 2 条)
//
//	本店某个分层**自己不可用**(!Usable())且有**启用中**的借用配置
//	⇒ 采用**被借店同一 (体重段, 部位)**那一行的数字,并把 source_branch_id 记成被借方。
//
//	被借店那一行自己也不可用 ⇒ **不借用**(回退 LLM)。
//
// ⚠️ **管理员手动生成的行(manual_generated=true)整层跳过**
//
//	那是管理员**显式指定**的数字 ⇒ 既不被自动重算覆盖,也不被借用覆盖。
func (s *Service) RecomputeForecastBaselines(ctx context.Context, branchID string) (int, error) {
	if branchID == "" {
		return 0, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	// ---- ① 实录样本(每头猪每个部位一条) ----
	recorded, err := s.loadRecordedSplitSamples(ctx, branchID)
	if err != nil {
		return 0, err
	}
	// ---- ② 估计样本(每头猪每个部位一条) ----
	estimated, err := s.loadEstimatedForecastSamples(ctx, branchID)
	if err != nil {
		return 0, err
	}
	// ---- ③ 跨店借用解析(在事务外读,避免 SQLite 单连接死锁) ----
	borrowed, err := s.resolveBorrowedBaselines(ctx, branchID)
	if err != nil {
		return 0, err
	}
	ownKeys := forecastBucketKeys(recorded, estimated)
	// 本店没样本、但被借店有的分层 ⇒ 也要建行(否则整店借用没意义)。
	keys := mergeForecastBucketKeys(ownKeys, borrowedBucketKeys(borrowed))

	// ---- ④ 逐桶选池 + 借用解析 ----
	now := s.now()
	written := 0
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 已被管理员手动生成的分层 ⇒ 整层跳过。
		manual, err := loadManualBaselineKeys(ctx, tx, branchID)
		if err != nil {
			return err
		}
		// 已有行的主键(先读后写,见 upsertForecastBaseline 的原因)。
		existing, err := loadBaselineRows(ctx, tx, branchID)
		if err != nil {
			return err
		}
		writtenKeys := map[forecastBucketKey]bool{}
		for _, k := range keys {
			if manual[k] {
				continue
			}
			recList := recorded[k]
			estList := estimated[k]
			recN, estN := len(recList), len(estList)

			// 选池:实录优先,不够才用估计;两个都不够 ⇒ 取样本多的那个留痕(但**不可用**)。
			var useList []decimal.Decimal
			source := SampleSourceRecorded
			switch {
			case recN >= MinForecastSamples:
				useList, source = recList, SampleSourceRecorded
			case estN >= MinForecastSamples:
				useList, source = estList, SampleSourceEstimated
			default:
				if estN > recN {
					useList, source = estList, SampleSourceEstimated
				} else {
					useList, source = recList, SampleSourceRecorded
				}
			}

			var rec model.PigForecastBaseline
			switch {
			case recN >= MinForecastSamples || estN >= MinForecastSamples:
				// 本店样本够 ⇒ 用自己的。
				mean, p10, p50, p90 := statsOf(useList)
				rec = model.PigForecastBaseline{
					ID:                   forecastBaselineID(branchID, k.band, k.cut),
					BranchID:             branchID,
					WeightBand:           k.band,
					CutType:              k.cut,
					SampleCount:          len(useList),
					RecordedSampleCount:  recN,
					EstimatedSampleCount: estN,
					SampleSource:         source,
					SourceBranchID:       branchID,
					MeanKg:               mean,
					P10Kg:                p10,
					P50Kg:                p50,
					P90Kg:                p90,
					ComputedAt:           now,
				}
			default:
				// 本店自己**不可用** ⇒ 看能不能借用被借店的数字。
				if b, ok := borrowed[k]; ok {
					rec = b
					rec.BranchID = branchID
					// ⚠️ **主键必须重算**:被借店那一行带的是**它自己的** id
					//   ⇒ 直接写会撞主键(两行 id 相同)⇒ 借用永远写不进去。
					rec.ID = forecastBaselineID(branchID, k.band, k.cut)
					rec.ComputedAt = now
					// ★ 标出这是**借来的**行(见 model.PigForecastBaseline.Borrowed):
					//   借用配置一关/一改,它必须能被刷新、也能被清掉;
					//   而管理员手动指定的行(borrowed=false)永远受保护。
					rec.Borrowed = true
					break
				}
				if len(useList) == 0 {
					continue
				}
				// 借不到 ⇒ 留痕(样本多的那个池子),但**不可用** ⇒ 回退 LLM。
				mean, p10, p50, p90 := statsOf(useList)
				rec = model.PigForecastBaseline{
					ID:                   forecastBaselineID(branchID, k.band, k.cut),
					BranchID:             branchID,
					WeightBand:           k.band,
					CutType:              k.cut,
					SampleCount:          len(useList),
					RecordedSampleCount:  recN,
					EstimatedSampleCount: estN,
					SampleSource:         source,
					SourceBranchID:       branchID,
					MeanKg:               mean,
					P10Kg:                p10,
					P50Kg:                p50,
					P90Kg:                p90,
					ComputedAt:           now,
				}
			}
			if err := s.upsertForecastBaseline(ctx, tx, rec, existing); err != nil {
				return err
			}
			written++
			writtenKeys[k] = true
		}
		// ---- 清理:**借来的**旧行 ----
		//
		// 借用被关掉(或被借方换了)⇒ 那一行必须消失,否则本店会一直用别家的旧数字
		// ⇒ 「关掉就回到只用本店样本」这条语义就废了。
		//
		// ⚠️ **手动生成的行不动**(管理员显式指定的)。
		//
		// ⚠️ 判据的顺序很重要(2026-10-30 修):
		//
		//	借来的行**继承**了来源行的 manual_generated(来源可能是管理员手动生成的)
		//	⇒ 若先判 manual_generated ⇒ 借来的行永远删不掉 ⇒ 关掉借用也没用。
		//	⇒ 先判 `borrowed`(见 model.PigForecastBaseline.Borrowed)⇒ 借来的先删。
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
			if prev.ManualGenerated {
				continue // 管理员手动指定的,永不删
			}
			if prev.SourceBranchID == "" || prev.SourceBranchID == branchID {
				continue // 本店自产的行(哪怕过时)也不在这里删
			}
			if err := tx.WithContext(ctx).Where("id = ?", prev.ID).
				Delete(&model.PigForecastBaseline{}).Error; err != nil {
				return fmt.Errorf("delete borrowed pig_forecast_baseline %s: %w", prev.ID, err)
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

// loadManualBaselineKeys 读本店**手动生成**的分层键(manual_generated = true)。
//
// ⚠️ **借来的行要排除**(borrowed = false):
//
//	借来的行会继承来源行的 manual_generated(来源可能是管理员手动生成的)
//	⇒ 若把它也算成"管理员指定的"⇒ 重算时整层跳过 ⇒ 借来的数字**永远不刷新**
//	⇒ 借用一旦配上就"冻"在那儿了(见 model.PigForecastBaseline.Borrowed 的说明)。
func loadManualBaselineKeys(ctx context.Context, tx *gorm.DB, branchID string) (map[forecastBucketKey]bool, error) {
	out := map[forecastBucketKey]bool{}
	var rows []model.PigForecastBaseline
	if err := tx.WithContext(ctx).
		Where("branch_id = ? AND manual_generated = ? AND borrowed = ?", branchID, true, false).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query pig_forecast_baselines(manual): %w", err)
	}
	for _, r := range rows {
		out[forecastBucketKey{band: r.WeightBand, cut: r.CutType}] = true
	}
	return out, nil
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

// loadRecordedSplitSamples 读某店的**实录样本**,按 (体重段 × 部位) 分桶。
//
// # 门店隔离
//
//	⚠️ JOIN 时**两边都按 branch_id 过滤**(`pig_cuts.branch_id` + `whole_pigs.branch_id`)
//	  ⇒ 就算数据脏了(某行的 branch_id 与猪不一致),也不会把别店的猪算进本店的基线。
func (s *Service) loadRecordedSplitSamples(ctx context.Context, branchID string) (
	map[forecastBucketKey][]decimal.Decimal, error) {
	out := map[forecastBucketKey][]decimal.Decimal{}
	var pigCuts []model.PigCut
	if err := s.db.WithContext(ctx).
		Joins("JOIN whole_pigs ON whole_pigs.id = pig_cuts.pig_id").
		Where("pig_cuts.branch_id = ? AND whole_pigs.branch_id = ?", branchID, branchID).
		Find(&pigCuts).Error; err != nil {
		return nil, fmt.Errorf("query pig_cuts: %w", err)
	}
	if len(pigCuts) == 0 {
		return out, nil
	}
	var pigs []model.WholePig
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Find(&pigs).Error; err != nil {
		return nil, fmt.Errorf("query whole_pigs: %w", err)
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
	for pigID, perCut := range perPigCut {
		band, ok := bandOf[pigID]
		if !ok {
			continue
		}
		for cut, kg := range perCut {
			k := forecastBucketKey{band: band, cut: cut}
			out[k] = append(out[k], kg)
		}
	}
	return out, nil
}

// loadEstimatedForecastSamples 读某店的**估计样本**,按 (体重段 × 部位) 分桶。
func (s *Service) loadEstimatedForecastSamples(ctx context.Context, branchID string) (
	map[forecastBucketKey][]decimal.Decimal, error) {
	out := map[forecastBucketKey][]decimal.Decimal{}
	perPigCut, bandOf, err := s.loadEstimatedSplitSamples(ctx, branchID)
	if err != nil {
		return nil, err
	}
	for pigID, perCut := range perPigCut {
		band, ok := bandOf[pigID]
		if !ok {
			continue
		}
		for cut, kg := range perCut {
			k := forecastBucketKey{band: band, cut: cut}
			out[k] = append(out[k], kg)
		}
	}
	return out, nil
}

// forecastBucketKeys 两个池子的**并集**分桶键(排序稳定 ⇒ 写库顺序稳定)。
func forecastBucketKeys(recorded, estimated map[forecastBucketKey][]decimal.Decimal) []forecastBucketKey {
	seen := map[forecastBucketKey]bool{}
	out := make([]forecastBucketKey, 0, len(recorded)+len(estimated))
	for k := range recorded {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for k := range estimated {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for i := 1; i < len(out); i++ {
		j := i
		for j > 0 && (out[j-1].band > out[j].band ||
			(out[j-1].band == out[j].band && string(out[j-1].cut) > string(out[j].cut))) {
			out[j-1], out[j] = out[j], out[j-1]
			j--
		}
	}
	return out
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

// ForecastFor 查某头猪某部位的预测产出(优先 p50,样本不足返零 + false)。
//
// # 可用性判据
//
//	`sample_count >= MinForecastSamples` **或** `manual_generated = true`。
//
//	后者是 owner 2026-10-30 的补充:「生成基线**不设 ≥5 门槛**,
//	只要那天的录入/搬台/销售/盘点都全就可以生成」⇒ 手动生成的行**必须**能用,
//	否则管理员辛苦选的那一天等于白选。
func (b ForecastBaseline) Usable() bool {
	return b.SampleCount >= MinForecastSamples || b.ManualGenerated
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
