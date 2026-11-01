// Package service / forecast_samples.go —— **样本库**（2026-10-30 owner 拍板：样本库是一等公民，基线是派生结果）。
//
// # 模型
//
//	owner：「实际是我们需要一个**管理样本**的功能，按一天生成基线是错的，其实是**按一天加入样本**……
//	       基线列表的**重算**功能就是把样本库重新按照算法算一遍产生新的统计回归基线」
//
//	⇒ 三个动作：
//	  ① **加入样本**（按一天）：跑那天的引擎 ⇒ 落估计样本；
//	  ② **删除样本**（按天+体重段整组 / 按单头猪）：**打墓碑**，不物理删；
//	  ③ **重算**：从样本库全量拟合 ⇒ 覆盖所有分层（见 forecast.go）。
//
// # 为什么删除是**打墓碑**而不是物理删
//
//	引擎每次跑那天的成本链都会**重建**那天的估计样本（见 stock.go::persistCutCostDaily）
//	⇒ 物理删掉的样本会在下一次重建时**原样复活** ⇒ "删除"等于没删。
//	⇒ 墓碑（`excluded_at`）：不参与拟合、不被重建覆盖、可以恢复。
//
// # 样本库的边界
//
//	样本库 = **`pig_cut_estimates`**（引擎倒推的观察，带 biz_date + weight_band）。
//	`pig_cuts`（门店手工补录）**继续参与拟合**，但**不在样本库里删**
//	—— 删它会动 FIFO 摊销与「有补录锁死删猪」的逻辑 ⇒ 成本链会被污染。
//	⇒ 顶层返回 `recorded_sample_count` 让门店看得见「还有多少实录样本在参与拟合」，
//	  样本的 `sample_source` 会标成 `mixed`（这头猪同时有实录）。
package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// ---- 出参 ----

// ForecastSampleCut 样本库里的一条（猪 × 部位）观察。
type ForecastSampleCut struct {
	CutType  model.CutType   `json:"cut_type"`
	WeightKg decimal.Decimal `json:"weight_kg"`
	// SampleSource 是这条观察来自哪个池子（recorded | estimated）。
	SampleSource string `json:"sample_source"`
	// Excluded = true ⇒ 这条样本已被门店删掉（墓碑）。
	Excluded bool `json:"excluded"`
}

// ForecastSamplePig 样本库里的一头猪（= 一组样本）。
type ForecastSamplePig struct {
	PigID string `json:"pig_id"`
	// EarTag 是猪的耳标。
	//
	// ⚠️ **耳标空有两种原因**(2026-11-02 owner 截图指出,前端曾因此把活猪说成"猪已删"):
	//
	//	① **猪被删了** ⇒ 耳标空 **且 [GrossWeightKg] = 0**(读不到 whole_pigs 这行);
	//	② **猪还在、但门店没录耳标号** ⇒ 耳标**也空**,可毛重**有值**
	//	  (后端明确允许:"有些猪没有耳标/不扫码"⇒ ear_tag 只是普通索引)。
	//
	// ⇒ **判"猪被删"要看毛重,不能看耳标**。
	EarTag string `json:"ear_tag"`
	// GrossWeightKg 是这头猪的毛重（**猪被删了则 0** ⇒ 这是"猪还在不在"的可靠信号）。
	GrossWeightKg decimal.Decimal `json:"gross_weight_kg"`
	WeightBand    string          `json:"weight_band"`
	// Date 是这头猪的营业日（样本归属的那一天）。
	Date string `json:"date"`
	// SampleSource 是这头猪的样本来源（recorded | estimated | mixed）。
	//
	// ⚠️ `mixed` = 这头猪**同时**有门店手工补录的实录
	//   ⇒ 那部分**不在样本库里、也删不掉**（删它会动 FIFO 成本）。
	SampleSource string `json:"sample_source"`
	// Excluded = true ⇒ 这头猪的样本**全部**被删掉了。
	Excluded bool `json:"excluded"`
	// Cuts 是各部位的样本量。
	Cuts []ForecastSampleCut `json:"cuts"`
}

// ForecastSampleGroup 样本库的一组 = **（营业日 × 体重段）**。
//
// ⚠️ 这正是 owner 要的删除粒度：「删除某天某个重量的样本」。
type ForecastSampleGroup struct {
	Date       string `json:"date"`
	WeightBand string `json:"weight_band"`
	// PigCount 是这一组里有几头猪。
	PigCount int `json:"pig_count"`
	// SampleCount 是**未删除**的样本条数（猪 × 部位）。
	SampleCount int `json:"sample_count"`
	// ExcludedSampleCount 是**已删除**（墓碑）的样本条数。
	ExcludedSampleCount int                 `json:"excluded_sample_count"`
	Pigs                []ForecastSamplePig `json:"pigs"`
}

// ForecastSampleLibrary 样本库全貌。
type ForecastSampleLibrary struct {
	BranchID string `json:"branch_id"`
	// SampleCount / ExcludedSampleCount 是库里的样本条数（猪 × 部位）。
	SampleCount         int `json:"sample_count"`
	ExcludedSampleCount int `json:"excluded_sample_count"`
	// PigCount 是库里涉及几头猪。
	PigCount int `json:"pig_count"`
	// RecordedSampleCount 是本店的**实录**样本条数。
	//
	// ⚠️ 实录样本(`pig_cuts`)**不在样本库里**（删它会动 FIFO 成本）
	//   但**照样参与拟合** ⇒ 这个数字让门店看得见"拟合里还有多少不是库里的东西"。
	RecordedSampleCount int                   `json:"recorded_sample_count"`
	Groups              []ForecastSampleGroup `json:"groups"`
}

// ---- 入参 ----

// ForecastSampleQuery 样本库查询条件。
type ForecastSampleQuery struct {
	// Date 过滤某一天（YYYY-MM-DD）；空 = 全部。
	Date string
	// WeightBand 过滤某体重段；空 = 全部。
	WeightBand string
	// IncludeExcluded = true ⇒ 连**已删除**（墓碑）的样本一起返回。
	IncludeExcluded bool
}

// ForecastSampleCollectInput 「按一天加入样本」的入参。
type ForecastSampleCollectInput struct {
	// Date 是要加入样本的那一天（YYYY-MM-DD）。
	Date string `json:"date"`
	// SourceBranchID 是**样本来源门店**；空 = 本店。
	//
	// ⚠️ 给了 = **替别店**跑那一天 ⇒ 样本落在**别店自己的**样本库
	//   （样本归猪的门店 ⇒ 不能把别店的猪写进本店样本库）。
	//   本店要用别店的数字 ⇒ 走「跨店选用」（借用配置），不是这里。
	SourceBranchID string `json:"source_branch_id"`
}

// ForecastSampleCollectResult 「加入样本」的结果。
type ForecastSampleCollectResult struct {
	Date           string `json:"date"`
	SourceBranchID string `json:"source_branch_id"`
	// Written 是落库的样本条数（猪 × 部位）。
	Written int `json:"written"`
	// SkippedExcluded 是**因已被删除（墓碑）而没写**的条数。
	SkippedExcluded int `json:"skipped_excluded"`
	// PigCount 是那天进场的猪数。
	PigCount int `json:"pig_count"`
	// GroupCount 是落库后形成了几组（天 × 体重段）。
	GroupCount int `json:"group_count"`
	// Message 是给门店看的一句话（前端直接显示）。
	Message string `json:"message"`
}

// ForecastSampleTarget 定位要删/要恢复的样本（二选一）。
type ForecastSampleTarget struct {
	// Date + WeightBand ⇒ 整组（那天该体重段的所有猪）。
	Date       string `json:"date"`
	WeightBand string `json:"weight_band"`
	// PigID ⇒ 单头猪（该猪的所有部位）。
	PigID string `json:"pig_id"`
}

// ---- 读：样本库 ----

// ListForecastSamples 列某店的样本库。
func (s *Service) ListForecastSamples(ctx context.Context, branchID string,
	in ForecastSampleQuery) (*ForecastSampleLibrary, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	rows, err := s.queryForecastSampleRows(ctx, branchID, in)
	if err != nil {
		return nil, err
	}
	pigInfo, err := s.loadForecastSamplePigInfo(ctx, branchID)
	if err != nil {
		return nil, err
	}
	recCount, recPigs, err := s.countRecordedForecastSamples(ctx, branchID)
	if err != nil {
		return nil, err
	}

	lib := &ForecastSampleLibrary{
		BranchID:            branchID,
		RecordedSampleCount: recCount,
		Groups:              []ForecastSampleGroup{},
	}
	// ---- 聚合：(天, 体重段) → 组 → 猪 → 部位 ----
	type groupKey struct {
		date string
		band string
	}
	groups := map[groupKey]*ForecastSampleGroup{}
	pigsOf := map[groupKey]map[string]*ForecastSamplePig{}
	for _, r := range rows {
		gk := groupKey{date: r.bizDate, band: r.row.WeightBand}
		g, ok := groups[gk]
		if !ok {
			g = &ForecastSampleGroup{
				Date:       r.bizDate,
				WeightBand: r.row.WeightBand,
				Pigs:       []ForecastSamplePig{},
			}
			groups[gk] = g
			pigsOf[gk] = map[string]*ForecastSamplePig{}
		}
		p, ok := pigsOf[gk][r.row.PigID]
		if !ok {
			info := pigInfo[r.row.PigID]
			p = &ForecastSamplePig{
				PigID:         r.row.PigID,
				EarTag:        info.earTag,
				GrossWeightKg: info.grossKg,
				WeightBand:    r.row.WeightBand,
				Date:          r.bizDate,
				Cuts:          []ForecastSampleCut{},
			}
			pigsOf[gk][r.row.PigID] = p
		}
		p.Cuts = append(p.Cuts, ForecastSampleCut{
			CutType:      r.row.CutType,
			WeightKg:     r.row.WeightKg,
			SampleSource: SampleSourceEstimated,
			Excluded:     r.excluded,
		})
		if r.excluded {
			g.ExcludedSampleCount++
			lib.ExcludedSampleCount++
		} else {
			g.SampleCount++
			lib.SampleCount++
		}
	}

	// ---- 汇总 ----
	gks := make([]groupKey, 0, len(groups))
	for k := range groups {
		gks = append(gks, k)
	}
	sort.Slice(gks, func(i, j int) bool {
		if gks[i].date != gks[j].date {
			return gks[i].date > gks[j].date // 新的一天在前
		}
		return gks[i].band < gks[j].band
	})
	seenPigs := map[string]bool{}
	for _, gk := range gks {
		g := groups[gk]
		pm := pigsOf[gk]
		ids := make([]string, 0, len(pm))
		for id := range pm {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			p := pm[id]
			sort.SliceStable(p.Cuts, func(a, b int) bool {
				return string(p.Cuts[a].CutType) < string(p.Cuts[b].CutType)
			})
			live := 0
			for _, c := range p.Cuts {
				if !c.Excluded {
					live++
				}
			}
			p.Excluded = live == 0
			p.SampleSource = SampleSourceEstimated
			if recPigs[id] {
				// 这头猪**同时**有门店手工补录的实录 ⇒ 混合来源（实录删不掉）。
				p.SampleSource = SampleSourceMixed
			}
			if !seenPigs[id] {
				seenPigs[id] = true
				lib.PigCount++
			}
			g.Pigs = append(g.Pigs, *p)
		}
		g.PigCount = len(g.Pigs)
		lib.Groups = append(lib.Groups, *g)
	}
	return lib, nil
}

// forecastSamplePigInfo 猪的展示信息（耳标 / 毛重）。
type forecastSamplePigInfo struct {
	earTag  string
	grossKg decimal.Decimal
}

// loadForecastSamplePigInfo 读某店所有猪的耳标与毛重。
func (s *Service) loadForecastSamplePigInfo(ctx context.Context, branchID string) (
	map[string]forecastSamplePigInfo, error) {
	var pigs []model.WholePig
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Find(&pigs).Error; err != nil {
		return nil, fmt.Errorf("query whole_pigs(samples): %w", err)
	}
	out := make(map[string]forecastSamplePigInfo, len(pigs))
	for _, p := range pigs {
		out[p.ID] = forecastSamplePigInfo{earTag: p.EarTag, grossKg: p.GrossWeightKg}
	}
	return out, nil
}

// countRecordedForecastSamples 数本店的**实录**样本条数（pig_cuts）与涉及的猪。
//
// ⚠️ 实录样本**不在样本库里**（删它会动 FIFO 成本），但**照样参与拟合**
// ⇒ 这个数字让门店看得见"拟合里还有多少不是库里的东西"。
func (s *Service) countRecordedForecastSamples(ctx context.Context, branchID string) (int, map[string]bool, error) {
	var cuts []model.PigCut
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Find(&cuts).Error; err != nil {
		return 0, nil, fmt.Errorf("query pig_cuts(samples): %w", err)
	}
	pigs := map[string]bool{}
	for _, c := range cuts {
		pigs[c.PigID] = true
	}
	return len(cuts), pigs, nil
}

// forecastSampleRowWithMeta 样本行 + 展示用元信息。
type forecastSampleRowWithMeta struct {
	row      model.PigCutEstimate
	bizDate  string
	excluded bool
}

// queryForecastSampleRows 查样本库的行（含/不含墓碑）。
func (s *Service) queryForecastSampleRows(ctx context.Context, branchID string,
	in ForecastSampleQuery) ([]forecastSampleRowWithMeta, error) {
	q := s.db.WithContext(ctx).Where("branch_id = ?", branchID)
	if day := strings.TrimSpace(in.Date); day != "" {
		start, end, err := forecastSampleDateRange(day)
		if err != nil {
			return nil, err
		}
		// ⚠️ 用**纯日期字符串区间**而不是 datatypes.Date 当参数：
		//   PG 会按会话时区折算 ⇒ 差一天；SQLite 端又落成另一种串 ⇒ 等值比较必然落空。
		q = q.Where("biz_date >= ? AND biz_date < ?", start, end)
	}
	if band := strings.TrimSpace(in.WeightBand); band != "" {
		q = q.Where("weight_band = ?", band)
	}
	if !in.IncludeExcluded {
		q = q.Where("excluded_at IS NULL")
	}
	var rows []model.PigCutEstimate
	if err := q.Order("biz_date ASC, weight_band ASC, pig_id ASC, cut_type ASC").
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query pig_cut_estimates(library): %w", err)
	}
	loc := s.bizTZOrUTC()
	out := make([]forecastSampleRowWithMeta, 0, len(rows))
	for _, r := range rows {
		out = append(out, forecastSampleRowWithMeta{
			row: r,
			// ⚠️ `datatypes.Date` 是 `type Date time.Time`（不是 struct）⇒ 要显式转换。
			//   归一到**业务时区**再取日期 ⇒ 两种时区形态（+08 / UTC）都落到同一个营业日。
			bizDate:  time.Time(r.BizDate).In(loc).Format("2006-01-02"),
			excluded: r.ExcludedAt != nil,
		})
	}
	return out, nil
}

// forecastSampleDateRange 把 "YYYY-MM-DD" 转成 [start, end) 的**纯日期字符串**区间。
func forecastSampleDateRange(day string) (string, string, error) {
	d, err := time.Parse("2006-01-02", strings.TrimSpace(day))
	if err != nil {
		return "", "", fmt.Errorf("%w: date 必须是 YYYY-MM-DD(收到 %q)", ErrInvalidInput, day)
	}
	return d.Format("2006-01-02"), d.AddDate(0, 0, 1).Format("2006-01-02"), nil
}

// ---- 写：加入 / 删除 / 恢复 ----

// CollectForecastSamples 「按一天加入样本」。
//
// # 流程
//
//	① 完整性校验（录入 / 搬台 / 销售 / 盘点，**跟毛利看板同一条销售读路径**）
//	② 跑那天的引擎 ⇒ 落估计样本（**墓碑行不复活**）
//	③ 两个池都空 ⇒ 400 点名（避免"空成功"）
//
// ⚠️ **不自动重算**：重算是另一个明确动作（前端提示"点重算生效"）。
func (s *Service) CollectForecastSamples(ctx context.Context, branchID string,
	in ForecastSampleCollectInput) (*ForecastSampleCollectResult, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	dayKey := strings.TrimSpace(in.Date)
	if dayKey == "" {
		return nil, fmt.Errorf("%w: date 必填(要把哪一天的样本加进来)", ErrInvalidInput)
	}
	loc := s.bizTZOrUTC()
	day, err := time.ParseInLocation("2006-01-02", dayKey, loc)
	if err != nil {
		return nil, fmt.Errorf("%w: date 必须是 YYYY-MM-DD(收到 %q)", ErrInvalidInput, in.Date)
	}
	source := strings.TrimSpace(in.SourceBranchID)
	if source == "" {
		source = branchID
	}

	// ---- ① 完整性校验 ----
	//
	// ⚠️ **先校验,再跑引擎**：缺项的日子不该白跑一次引擎（要读 cube 的销售）。
	gaps, err := s.dayDataGaps(ctx, source, day)
	if err != nil {
		return nil, err
	}
	if len(gaps) > 0 {
		return nil, fmt.Errorf("%w: %s 这一天数据不完整,缺:%s —— 补齐后才能加入样本",
			ErrInvalidInput, dayKey, strings.Join(gaps, "、"))
	}

	// ---- ② 跑引擎 ⇒ 落估计样本 ----
	//
	// ⚠️ overwrite=true：这是门店**显式**「按一天加入样本」/「重采」
	//   ⇒ 覆盖该日的旧样本 + 复活墓碑行（owner:「不对就手动删掉重采」）。
	stats, err := s.rebuildForecastDaySamples(ctx, source, day, true)
	if err != nil {
		// ⚠️ 引擎跑不了(例如 cube 读失败)⇒ **上抛**,别回落成"那天没样本"
		//   (回落 = 把一次链路故障说成"数据不完整",门店会去补一堆不存在的数据)。
		return nil, fmt.Errorf("重建那天的分割样本: %w", err)
	}

	// ---- ③ 两个池都空 ⇒ 400 点名(避免"空成功") ----
	//
	// 实录池(门店手工补录)与估计池(自动倒推摊出来的)任一有样本 ⇒ 都能拟合 ⇒ 不缺。
	recorded, err := s.loadRecordedSamplesForDay(ctx, source, day)
	if err != nil {
		return nil, err
	}
	estimated, err := s.loadEstimatedSamplesForDay(ctx, source, day)
	if err != nil {
		return nil, err
	}
	if gap := forecastSplitSampleGap(
		countForecastSamples(recorded), countForecastSamples(estimated)); gap != "" {
		return nil, fmt.Errorf("%w: %s 这一天数据不完整,缺:%s —— 补齐后才能加入样本",
			ErrInvalidInput, dayKey, gap)
	}

	groups, err := s.countForecastSampleGroups(ctx, source)
	if err != nil {
		return nil, err
	}
	return &ForecastSampleCollectResult{
		Date:            dayKey,
		SourceBranchID:  in.SourceBranchID,
		Written:         stats.Written,
		SkippedExcluded: stats.SkippedExcluded,
		PigCount:        stats.PigCount,
		GroupCount:      groups,
		Message:         forecastCollectMessage(stats, countForecastSamples(recorded)),
	}, nil
}

// forecastCollectMessage 给门店看的一句话。
//
// # ⚠️ 为什么不能只有一句"已加入 N 条"
//
//	三种情况差别很大，混成一句 = 错引导：
//	  - 写进去了 ⇒ 「点重算生效」
//	  - **全是墓碑** ⇒ 用户自己删过 ⇒ 「没有自动恢复，点『恢复』可找回」
//	    （说成"没加进来" ⇒ 门店会以为系统坏了）
//	  - 只有实录 ⇒ 「这一天已有手工补录的样本」
func forecastCollectMessage(stats forecastRebuildStats, recordedN int) string {
	switch {
	case stats.Written > 0:
		return fmt.Sprintf("已加入 %d 条样本（%d 头猪），点「重算」后生效",
			stats.Written, stats.PigCount)
	case stats.SkippedExcluded > 0:
		return "这一天的样本已被删除，没有自动恢复（点「恢复」可找回）"
	case recordedN > 0:
		return fmt.Sprintf("这一天已有 %d 条手工补录的样本，无需加入；点「重算」后生效", recordedN)
	default:
		return "这一天没有可加入的样本"
	}
}

// countForecastSampleGroups 数样本库里形成了几组（天 × 体重段，只算未删除的）。
func (s *Service) countForecastSampleGroups(ctx context.Context, branchID string) (int, error) {
	var rows []model.PigCutEstimate
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND excluded_at IS NULL", branchID).
		Find(&rows).Error; err != nil {
		return 0, fmt.Errorf("query pig_cut_estimates(groups): %w", err)
	}
	seen := map[string]bool{}
	loc := s.bizTZOrUTC()
	for _, r := range rows {
		seen[time.Time(r.BizDate).In(loc).Format("2006-01-02")+"|"+r.WeightBand] = true
	}
	return len(seen), nil
}

// DeleteForecastSamples 删除样本（**打墓碑**，不物理删）。
//
// ⚠️ 物理删会被引擎重跑复活 ⇒ 删除必须是墓碑。
//
// # 两种粒度（owner：两种都要）
//
//	① `date` + `weight_band` ⇒ **整组**删（那天该体重段的所有猪）
//	② `pig_id` ⇒ **单头猪**删（该猪的所有部位）
func (s *Service) DeleteForecastSamples(ctx context.Context, branchID string,
	in ForecastSampleTarget) (int, error) {
	if branchID == "" {
		return 0, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	q, err := s.forecastSampleTargetQuery(ctx, branchID, in)
	if err != nil {
		return 0, err
	}
	now := s.now()
	res := q.Model(&model.PigCutEstimate{}).
		Where("excluded_at IS NULL").
		Updates(map[string]any{
			"excluded_at":     now,
			"excluded_reason": "manual",
			"updated_at":      now,
		})
	if res.Error != nil {
		return 0, fmt.Errorf("exclude pig_cut_estimates: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}

// RestoreForecastSamples 恢复被删（墓碑）的样本。
func (s *Service) RestoreForecastSamples(ctx context.Context, branchID string,
	in ForecastSampleTarget) (int, error) {
	if branchID == "" {
		return 0, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	q, err := s.forecastSampleTargetQuery(ctx, branchID, in)
	if err != nil {
		return 0, err
	}
	res := q.Model(&model.PigCutEstimate{}).
		Where("excluded_at IS NOT NULL").
		Updates(map[string]any{
			"excluded_at":     gorm.Expr("NULL"),
			"excluded_reason": "",
			"updated_at":      s.now(),
		})
	if res.Error != nil {
		return 0, fmt.Errorf("restore pig_cut_estimates: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}

// forecastSampleTargetQuery 按目标（整组 / 单头猪）构造查询。
func (s *Service) forecastSampleTargetQuery(ctx context.Context, branchID string,
	in ForecastSampleTarget) (*gorm.DB, error) {
	q := s.db.WithContext(ctx).Where("branch_id = ?", branchID)
	pigID := strings.TrimSpace(in.PigID)
	day := strings.TrimSpace(in.Date)
	band := strings.TrimSpace(in.WeightBand)
	switch {
	case pigID != "":
		q = q.Where("pig_id = ?", pigID)
	case day != "" && band != "":
		start, end, err := forecastSampleDateRange(day)
		if err != nil {
			return nil, err
		}
		q = q.Where("biz_date >= ? AND biz_date < ? AND weight_band = ?", start, end, band)
	default:
		return nil, fmt.Errorf("%w: 要删哪一组样本?给 pig_id(单头猪),或给 date + weight_band(整组)",
			ErrInvalidInput)
	}
	return q, nil
}
