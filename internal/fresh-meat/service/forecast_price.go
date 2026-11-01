// Package service / forecast_price.go —— 分割基线的**单价**通道(2026-10-31 owner)。
//
// # owner 的原话(需求原文)
//
//	「分割基线加入单价,样本以日为单位,从每日各部位的实际销售单价和整体毛利率
//	  来推算部位进货单价,被搭赠等无记录的则保持无分割单价基线(即无法生成建议单价,
//	  退取门店整店加权平均价,如仍无退取LLM缓存单价,兜底LLM重新给价)。
//	  分割基线有了单价后,应用此基线(注意应用过程中需要结合单猪的采购价,
//	  如基线是14.6/kg的,那16/kg的猪每个部位单价都要按比例上涨),
//	  则可同时获得建议成本单价。」
//
// # 两段式(与重量基线同构:样本库是一等公民,基线是派生结果)
//
//	① **日样本**(pig_cut_price_samples)= 店 × 日 × 部位 的一条观察:
//	   零售单价(营收÷销量) + 当日整体毛利率 ⇒ 倒推**部位进货单价**。
//	② **基线**(pig_cut_price_baselines)= 店 × 部位:多日样本按销量加权聚合。
//
// # ⚠️ 循环依赖怎么解(这是本文件存在的核心理由)
//
//	单价的倒推需要"整体毛利率"⇒ 毛利率来自**成本链**;
//	而"建议成本单价"又要喂给报表 ⇒ 如果基线把**当天自己**的样本也算进去,
//	就成了:今天的毛利率 → 今天的单价基线 → 今天的建议成本单价 → 今天的毛利率。
//
//	⇒ **结构性断环**:算某营业日的建议成本单价时,单价基线**只用该日之前**的样本
//	  (loadPriceBaselinesAsOf 的 cutoff)。于是"今天的建议"只依赖"昨天为止的事实",
//	  天然**滞后一天**,但**永远不自证**。
//
//	⚠️ 物化表(pig_cut_price_baselines)是给门店**看**的;算建议成本单价时
//	  **不读它**,而是现算 as-of 版本 ⇒ 物化表的刷新时机不会影响正确性。
//
// # 边界(owner 给定的两条硬边界)
//
//	① 重量:建议分割之和不得超过猪重(已由 cut_split_algo.go 的 scaleSplitToPigWeight 保证)。
//	② **各部位合计价值不得超过录入的猪的总进货金额**
//	   ⇒ 由 pigSuggestedUnitPrices 的**封顶**保证(超了就按比例把单价缩下去,
//	     与重量"只往下缩"同一条原则)。
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
	"gorm.io/gorm/clause"
)

// 单价来源标记(前端 tooltip 用 —— 门店要看得见这个价**是怎么来的**)。
const (
	// PriceSourceBaseline 单价基线 × 单猪进价缩放(最准)。
	PriceSourceBaseline = "baseline"
	// PriceSourceStoreAvg 退取链①:门店整店加权平均价。
	PriceSourceStoreAvg = "store_avg"
	// PriceSourceLLMCache 退取链②:LLM 缓存单价。
	PriceSourceLLMCache = "llm_cache"
	// PriceSourceNone 退取链③也没走通 ⇒ 该部位**没有建议成本单价**(前端显示「—」)。
	PriceSourceNone = "none"
)

// priceMarginValidLow / priceMarginValidHigh 是"整体毛利率"能用来倒推的区间。
//
// ⚠️ 为什么要有上下界(不是"有毛利率就行"):
//
//	≤ 0  : 亏本/成本算错 ⇒ 倒推出的进货单价是负数或离谱值 ⇒ 会把基线教歪。
//	≥ 0.95: 成本几乎为 0 ⇒ 多半是"**成本算不出来**"(没有期初也没有进货)
//	         ⇒ 倒推出的单价接近零售价 ⇒ 同样会把基线教歪。
//
// ⇒ 越界时**整天不产样本**(宁缺毋滥),而不是产一条垃圾样本。
const (
	priceMarginValidLow  = "0.0"
	priceMarginValidHigh = "0.95"
)

// CutPriceSampleResult 采集单价日样本的结果(给门店看的一句话在 Message 里)。
type CutPriceSampleResult struct {
	Date string `json:"date"`
	// Written 是写入/覆盖的样本条数(部位级)。
	Written int `json:"written"`
	// SkippedExcluded 是因已被删除(墓碑)而没写的条数。
	SkippedExcluded int `json:"skipped_excluded"`
	// NoSaleCutCount 是"当天没卖出过"的部位数(搭赠等)⇒ 它们**没有单价样本**。
	NoSaleCutCount int `json:"no_sale_cut_count"`
	// MarginRate 是当日整体毛利率(倒推用的那个);0 = 当日不产出任何样本。
	MarginRate decimal.Decimal `json:"margin_rate"`
	// PigPriceYuan 是当日猪的加权平均进价(元/kg)—— 单价缩放的上下文。
	PigPriceYuan decimal.Decimal `json:"pig_price_yuan"`
	// Message 是给门店看的一句话。
	Message string `json:"message"`
}

// priceBaselineValue 一条单价基线的数值(服务层用,与 model.PigCutPriceBaseline 同形)。
type priceBaselineValue struct {
	UnitCostYuan    decimal.Decimal
	RefPigPriceYuan decimal.Decimal
	SampleCount     int
}

// ---- 采集:日样本 ----

// CollectCutPriceSamples 采集某营业日的**单价日样本**。
//
// # 入参 rows = 那天的**部位级成本行**(引擎已经算好的)
//
//	⚠️ 复用现成的行(而不是自己再跑一遍引擎):引擎一次要读 cube 的销售,很贵。
//
// # 口径
//
//	① 当日**整体毛利率** = (营收 − 销售成本 − 统收成本 − 报损成本 − 搭赠成本) ÷ 营收
//	   ⚠️ **不含**预期毛利(预售结转):那是"预期",不是当天真实赚到的
//	     ⇒ 混进来会让倒推的单价失真。
//	② 逐部位:零售单价 = 营收 ÷ 销量(零售+统收);
//	   **销量为 0 的部位不产样本**(owner:搭赠等无销售记录的部位保持无单价基线)。
//	③ 倒推:进货单价 = 零售单价 ÷ (1 + 整体毛利率)(owner 2026-10-31 亲口定的口径)。
//
// # 幂等
//
//	同一 (店, 日, 部位) 只有一行 ⇒ 重复采集 = 覆盖。
//	⚠️ **墓碑行不复活**(与重量样本同口径):门店删掉的样本,重跑时原样保留"已删除"。
func (s *Service) CollectCutPriceSamples(ctx context.Context, branchID string, day time.Time,
	rows []CutDailyCost) (*CutPriceSampleResult, error) {
	res := &CutPriceSampleResult{Date: day.In(s.bizTZOrUTC()).Format("2006-01-02")}
	if branchID == "" {
		return res, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	loc := s.bizTZOrUTC()
	dayKey := day.In(loc).Format("2006-01-02")

	// ---- ① 当日整体毛利率(**参考口径**)----
	//
	// ⚠️ 2026-11-01 owner 终稿:这里**不再用账面毛利率**。
	//   账面的销售成本走移动加权链 ⇒ 依赖分割重量 ⇒ 切割误差会被带进单价基线
	//   ⇒ 正是 owner 要断的那条污染链。
	//   参考口径 = (营收 − 采购 + 期末猪肉台剩余成本) / 营收 ⇒ **零分割假设**。
	margin, ok, err := s.referenceMarginRateOfDay(ctx, branchID, day, rows)
	if err != nil {
		return res, err
	}
	res.MarginRate = margin
	if !ok {
		res.Message = fmt.Sprintf(
			"%s 这一天没有产出单价样本:整体毛利率 %s 不在可用区间(%s ~ %s)—— "+
				"多半是当天缺日终盘点(剩余算不出来)或当天没有进货", dayKey, margin.String(),
			priceMarginValidLow, priceMarginValidHigh)
		return res, nil
	}

	// ---- ② 当日猪的加权平均进价(缩放上下文) ----
	pigPrice, err := s.dayPigPrice(ctx, branchID, day)
	if err != nil {
		return res, err
	}
	res.PigPriceYuan = pigPrice

	// ---- ③ 逐部位产样本 ----
	excluded, err := s.excludedCutPriceSampleKeys(ctx, branchID, dayKey)
	if err != nil {
		return res, err
	}
	keepIDs := map[string]bool{}
	now := s.now()
	samples := make([]model.PigCutPriceSample, 0, len(rows))
	for _, r := range rows {
		soldKg := r.SoldKg.Add(r.BulkSoldKg)
		if !soldKg.IsPositive() {
			// ⚠️ **当天没卖出过** ⇒ 不产样本(owner:搭赠等无记录的部位保持无单价基线)。
			res.NoSaleCutCount++
			continue
		}
		retail := r.RevenueYuan.Div(soldKg)
		// ⚠️ **倒推口径(owner 2026-10-31 拍板)**:进货单价 = 零售均价 ÷ (1 + 整体毛利率)。
		//
		//	owner 原话(统计栏「日终剩余」那一格):「用零售均价/(1+今日毛利率)」
		//	⇒ 本仓**统一**用这一条(单价样本的倒推、建议成本单价、日终剩余估值
		//	  全都走它)⇒ 三处数字自洽,不会出现"两个成本价口径"。
		costPrice := retail.Div(decimal.NewFromInt(1).Add(margin))
		if !costPrice.IsPositive() {
			// 倒推出非正数(营收为 0 之类)⇒ 不产样本。
			res.NoSaleCutCount++
			continue
		}
		id := cutPriceSampleID(branchID, dayKey, string(r.CutType))
		keepIDs[id] = true
		samples = append(samples, model.PigCutPriceSample{
			ID:              id,
			BranchID:        branchID,
			BizDate:         dayKey,
			CutType:         r.CutType,
			SoldKg:          soldKg,
			RevenueYuan:     r.RevenueYuan,
			RetailPriceYuan: retail,
			MarginRate:      margin,
			CostPriceYuan:   costPrice,
			PigPriceYuan:    pigPrice,
			CreatedAt:       now,
			UpdatedAt:       now,
		})
	}
	// 墓碑行:不复活,但**不能被 stale 清理删掉**(删了 ⇒ 下次重建会写回来 ⇒ 复活)。
	for k := range excluded {
		keepIDs[k] = true
		res.SkippedExcluded++
	}

	if len(samples) > 0 {
		err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			for i := range samples {
				rec := samples[i]
				// ⚠️ **先读后写**:id 是**确定性**算出来的(surrogateID(店,日,部位))
				//   ⇒ 同一键永远是同一个 id ⇒ 这里用 ON CONFLICT 是安全的
				//   (与"借来的行"那种 id 不一致的情况不同,见 forecast.go 的注释)。
				if err := tx.WithContext(ctx).Clauses(clause.OnConflict{
					Columns: []clause.Column{
						{Name: "branch_id"}, {Name: "biz_date"}, {Name: "cut_type"},
					},
					DoUpdates: clause.Assignments(map[string]any{
						"sold_kg":           rec.SoldKg,
						"revenue_yuan":      rec.RevenueYuan,
						"retail_price_yuan": rec.RetailPriceYuan,
						"margin_rate":       rec.MarginRate,
						"cost_price_yuan":   rec.CostPriceYuan,
						"pig_price_yuan":    rec.PigPriceYuan,
						"updated_at":        rec.UpdatedAt,
					}),
				}).Create(&rec).Error; err != nil {
					return fmt.Errorf("upsert pig_cut_price_sample: %w", err)
				}
				res.Written++
			}
			return nil
		})
		if err != nil {
			return res, err
		}
	}
	// ---- ④ 清掉那天**不再产出**的旧样本(幂等的关键) ----
	if err := s.deleteStaleCutPriceSamples(ctx, branchID, dayKey, keepIDs); err != nil {
		return res, err
	}

	switch {
	case res.Written > 0:
		res.Message = fmt.Sprintf("已采集 %d 条单价样本（整体毛利率 %s%%），点「重算」后生效",
			res.Written, margin.Mul(decimal.NewFromInt(100)).StringFixed(2))
	case res.NoSaleCutCount > 0:
		res.Message = "这一天没有产出单价样本：当天没有卖出过的部位不产生单价（搭赠等无销售记录的部位保持无基线）"
	default:
		res.Message = "这一天没有可采集的单价样本"
	}
	return res, nil
}

// CollectCutPriceSamplesForDay 按一天采集单价样本(**自己跑引擎**)。
//
// ⚠️ 跑一次引擎要读 cube 的销售 ⇒ 不便宜 ⇒ 只在**明确按天采集**时调。
//
// ⚠️ **不自动重算基线** —— 重算是另一个明确动作(与重量样本同口径)。
func (s *Service) CollectCutPriceSamplesForDay(ctx context.Context, branchID, dayKey string) (
	*CutPriceSampleResult, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	loc := s.bizTZOrUTC()
	day, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(dayKey), loc)
	if err != nil {
		return nil, fmt.Errorf("%w: date 必须是 YYYY-MM-DD(收到 %q)", ErrInvalidInput, dayKey)
	}
	rows, _, _, _, err := s.computeCutDailyCosts(ctx, branchID, day)
	if err != nil {
		// ⚠️ 引擎跑不了(例如 cube 读失败)⇒ **上抛**,别回落成"那天没样本"
		//   (回落 = 把一次链路故障说成"没数据")。
		return nil, fmt.Errorf("跑那天的成本链: %w", err)
	}
	return s.CollectCutPriceSamples(ctx, branchID, day, rows)
}

// cutPriceSampleID 单价日样本的**确定性**代理主键。
//
// ⚠️ 必须确定性:purge 按"单列主键"删行 ⇒ 随机 id ⇒ purge 拿着不存在的 id 去删
//   ⇒ 行永远清不掉(见 surrogate_id.go 的说明)。
func cutPriceSampleID(branchID, bizDate, cutType string) string {
	return surrogateID("PCP", branchID, bizDate, cutType)
}

// cutPriceBaselineID 单价基线的**确定性**代理主键。
func cutPriceBaselineID(branchID string, cut model.CutType) string {
	return surrogateID("PCB", branchID, string(cut))
}

// priceMarginRateOfRows 从部位级成本行汇总出当日**整体毛利率**(**账面口径**)。
//
// 口径 = (营收 − 销售成本 − 统收成本 − 报损成本 − 搭赠成本) ÷ 营收
//
// ⚠️ **不含**预期毛利(预售结转):那是"预期",不是当天真实赚到的。
//
// ⚠️ 2026-11-01:这个**账面**口径已经**不再喂单价样本**了
//   (账面销售成本依赖分割重量 ⇒ 会把切割误差带进单价基线)。
//   单价样本现在走 **参考口径**(见 referenceMarginRateOfDay)。
//   本函数保留:它是"账面口径"的对照口径,测试要能对比两者。
// 返回 (毛利率, 是否可用于倒推)。
func priceMarginRateOfRows(rows []CutDailyCost) (decimal.Decimal, bool) {
	var revenue, salesCost, bulkCost, wasteCost, giftCost decimal.Decimal
	for _, r := range rows {
		revenue = revenue.Add(r.RevenueYuan)
		salesCost = salesCost.Add(r.SalesCostYuan)
		bulkCost = bulkCost.Add(r.BulkCostYuan)
		wasteCost = wasteCost.Add(r.WasteCostYuan)
		giftCost = giftCost.Add(r.GiftCostYuan)
	}
	if !revenue.IsPositive() {
		return decimal.Zero, false
	}
	margin := revenue.Sub(salesCost).Sub(bulkCost).Sub(wasteCost).Sub(giftCost).Div(revenue)
	return margin, priceMarginUsable(margin)
}

// priceMarginUsable 判断毛利率能不能用来倒推单价。
//
// ⚠️ 越界 ⇒ **整天不产样本**(宁缺毋滥),而不是产一条垃圾样本。
//   ≤ 0  : 亏本/口径错 ⇒ 倒推出的进货单价是负数或离谱值 ⇒ 会把基线教歪。
//   ≥ 0.95: 成本几乎为 0 ⇒ 多半是"成本算不出来"⇒ 倒推出的单价接近零售价 ⇒ 同样教歪。
func priceMarginUsable(margin decimal.Decimal) bool {
	low, _ := decimal.NewFromString(priceMarginValidLow)
	high, _ := decimal.NewFromString(priceMarginValidHigh)
	return margin.GreaterThan(low) && margin.LessThan(high)
}

// dayPigPrice 当日猪的**加权平均进价**(元/kg)= Σ猪成本 ÷ Σ猪毛重。
//
// ⚠️ 没有猪(当天没进货)⇒ 返回 0 ⇒ 单价基线**没有缩放基准**
//   ⇒ 应用时按基线原价用(不缩放)。
func (s *Service) dayPigPrice(ctx context.Context, branchID string, day time.Time) (decimal.Decimal, error) {
	start, end := s.businessDayBounds(day)
	var pigs []model.WholePig
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND arrived_at >= ? AND arrived_at < ?", branchID, start, end).
		Find(&pigs).Error; err != nil {
		return decimal.Zero, fmt.Errorf("query whole_pigs(price): %w", err)
	}
	var cost, weight decimal.Decimal
	for _, p := range pigs {
		cost = cost.Add(p.PurchaseCostYuan)
		weight = weight.Add(p.GrossWeightKg)
	}
	if !weight.IsPositive() {
		return decimal.Zero, nil
	}
	return cost.Div(weight), nil
}

// excludedCutPriceSampleKeys 读某店那一天**已删除(墓碑)**的单价样本键。
func (s *Service) excludedCutPriceSampleKeys(ctx context.Context, branchID, dayKey string) (
	map[string]bool, error) {
	out := map[string]bool{}
	var rows []model.PigCutPriceSample
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date = ? AND excluded_at IS NOT NULL", branchID, dayKey).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query pig_cut_price_samples(excluded): %w", err)
	}
	for _, r := range rows {
		out[r.ID] = true
	}
	return out, nil
}

// deleteStaleCutPriceSamples 删掉那天**不在 keepIDs 里**的样本行。
//
// ⚠️ **墓碑行不删**:墓碑是"已删除"的留痕 ⇒ 删掉墓碑 ⇒ 下次采集会把它写回来(复活)
//   ⇒ 门店的"删除"被引擎悄悄撤销。
func (s *Service) deleteStaleCutPriceSamples(ctx context.Context, branchID, dayKey string,
	keepIDs map[string]bool) error {
	q := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date = ? AND excluded_at IS NULL", branchID, dayKey)
	if len(keepIDs) > 0 {
		ids := make([]string, 0, len(keepIDs))
		for id := range keepIDs {
			ids = append(ids, id)
		}
		q = q.Where("id NOT IN ?", ids)
	}
	if err := q.Delete(&model.PigCutPriceSample{}).Error; err != nil {
		return fmt.Errorf("delete stale pig_cut_price_samples: %w", err)
	}
	return nil
}

// ---- 基线:从日样本聚合 ----

// RecomputeCutPriceBaselines 从**单价日样本**重算某店的单价基线。
//
// # 口径
//
//	样本 = 该店全部**未删除**的单价日样本
//	聚合 = 按**销量加权平均**(销量大的那天更有代表性)
//	可用门槛 = n ≥ MinForecastSamples(= 1,与重量基线同口径)
//
// # 跨店借用
//
//	本店某个部位自己**没有样本** ⇒ 看能不能借用被借店同部位的**单价基线**
//	(借用配置见 forecast_borrow.go;价格只看**部位**,不看体重段)。
//
// # ⚠️ 样本被删光 ⇒ 该部位基线随之删除
//
//	否则脏数字会一直给出错建议(与重量基线同口径)。
func (s *Service) RecomputeCutPriceBaselines(ctx context.Context, branchID string) (int, error) {
	if branchID == "" {
		return 0, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	samples, err := s.loadCutPriceSamples(ctx, branchID, "")
	if err != nil {
		return 0, err
	}
	live := make([]model.PigCutPriceSample, 0, len(samples))
	for _, r := range samples {
		if r.ExcludedAt != nil {
			continue
		}
		live = append(live, r)
	}
	values := aggregatePriceBaselines(live)

	// ---- 借用:本店没有样本的部位 ⇒ 借被借店同部位的单价基线 ----
	borrowed, err := s.resolveBorrowedPriceBaselines(ctx, branchID)
	if err != nil {
		return 0, err
	}

	now := s.now()
	written := 0
	writtenCuts := map[model.CutType]bool{}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing []model.PigCutPriceBaseline
		if err := tx.WithContext(ctx).Where("branch_id = ?", branchID).Find(&existing).Error; err != nil {
			return fmt.Errorf("query pig_cut_price_baselines: %w", err)
		}
		existingByCut := map[model.CutType]model.PigCutPriceBaseline{}
		for _, r := range existing {
			existingByCut[r.CutType] = r
		}
		cuts := make([]model.CutType, 0, len(values))
		for c := range values {
			cuts = append(cuts, c)
		}
		for c := range borrowed {
			if _, ok := values[c]; !ok {
				cuts = append(cuts, c)
			}
		}
		sort.Slice(cuts, func(i, j int) bool { return cuts[i] < cuts[j] })
		for _, c := range cuts {
			rec := model.PigCutPriceBaseline{
				ID:             cutPriceBaselineID(branchID, c),
				BranchID:       branchID,
				CutType:        c,
				SourceBranchID: branchID,
				ComputedAt:     now,
			}
			if v, ok := values[c]; ok {
				rec.UnitCostYuan = v.UnitCostYuan
				rec.RefPigPriceYuan = v.RefPigPriceYuan
				rec.SampleCount = v.SampleCount
			} else {
				b, ok := borrowed[c]
				if !ok {
					continue
				}
				rec.UnitCostYuan = b.UnitCostYuan
				rec.RefPigPriceYuan = b.RefPigPriceYuan
				rec.SampleCount = b.SampleCount
				rec.SourceBranchID = b.SourceBranchID
				rec.Borrowed = true
				// ⚠️ **主键必须重算**:被借店那一行带的是**它自己的** id
				//   ⇒ 直接写会撞主键(两行 id 相同)⇒ 借用永远写不进去。
				rec.ID = cutPriceBaselineID(branchID, c)
			}
			if err := s.upsertCutPriceBaseline(ctx, tx, rec, existingByCut); err != nil {
				return err
			}
			existingByCut[c] = rec
			written++
			writtenCuts[c] = true
		}
		// ---- 清理 ----
		//
		// ① **借来的**旧行:借用被关掉 ⇒ 那一行必须消失(否则一直用别家的旧数字)。
		// ② **样本被删光**的自产行:原来有样本、这次没写出来 ⇒ 该部位失效 ⇒ 删。
		for c, prev := range existingByCut {
			if writtenCuts[c] {
				continue
			}
			if prev.Borrowed || prev.SampleCount > 0 {
				if err := tx.WithContext(ctx).Where("id = ?", prev.ID).
					Delete(&model.PigCutPriceBaseline{}).Error; err != nil {
					return fmt.Errorf("delete stale pig_cut_price_baseline %s: %w", prev.ID, err)
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

// aggregatePriceBaselines 把日样本聚合成单价基线(**按销量加权**)。
func aggregatePriceBaselines(samples []model.PigCutPriceSample) map[model.CutType]priceBaselineValue {
	numCost := map[model.CutType]decimal.Decimal{}
	numPig := map[model.CutType]decimal.Decimal{}
	den := map[model.CutType]decimal.Decimal{}
	cnt := map[model.CutType]int{}
	for _, r := range samples {
		if !r.SoldKg.IsPositive() || !r.CostPriceYuan.IsPositive() {
			continue
		}
		w := r.SoldKg
		numCost[r.CutType] = numCost[r.CutType].Add(r.CostPriceYuan.Mul(w))
		numPig[r.CutType] = numPig[r.CutType].Add(r.PigPriceYuan.Mul(w))
		den[r.CutType] = den[r.CutType].Add(w)
		cnt[r.CutType]++
	}
	out := map[model.CutType]priceBaselineValue{}
	for c, d := range den {
		if !d.IsPositive() {
			continue
		}
		out[c] = priceBaselineValue{
			UnitCostYuan:    numCost[c].Div(d),
			RefPigPriceYuan: numPig[c].Div(d),
			SampleCount:     cnt[c],
		}
	}
	return out
}

// loadCutPriceSamples 读某店的单价日样本(dateKey 空 = 全部)。
func (s *Service) loadCutPriceSamples(ctx context.Context, branchID, dateKey string) (
	[]model.PigCutPriceSample, error) {
	q := s.db.WithContext(ctx).Where("branch_id = ?", branchID)
	if d := strings.TrimSpace(dateKey); d != "" {
		q = q.Where("biz_date = ?", d)
	}
	var rows []model.PigCutPriceSample
	if err := q.Order("biz_date ASC, cut_type ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query pig_cut_price_samples: %w", err)
	}
	return rows, nil
}

// ListForecastPriceBaselines 列某店的单价基线(给后台查看用)。
func (s *Service) ListForecastPriceBaselines(ctx context.Context, branchID string) (
	[]model.PigCutPriceBaseline, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	var rows []model.PigCutPriceBaseline
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("cut_type ASC").
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query pig_cut_price_baselines: %w", err)
	}
	return rows, nil
}

// ListForecastPriceSamples 列某店的单价日样本(dateKey 空 = 全部)。
func (s *Service) ListForecastPriceSamples(ctx context.Context, branchID, dateKey string) (
	[]model.PigCutPriceSample, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	return s.loadCutPriceSamples(ctx, branchID, dateKey)
}

// DeleteCutPriceSample 给某店某天某部位的单价样本**打墓碑**(不物理删)。
//
// ⚠️ 物理删会被引擎重跑复活 ⇒ 删除必须是墓碑(与重量样本同口径)。
func (s *Service) DeleteCutPriceSample(ctx context.Context, branchID, dateKey string,
	cut model.CutType) (int, error) {
	if branchID == "" {
		return 0, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	day := strings.TrimSpace(dateKey)
	if day == "" {
		return 0, fmt.Errorf("%w: date 必填", ErrInvalidInput)
	}
	now := s.now()
	res := s.db.WithContext(ctx).Model(&model.PigCutPriceSample{}).
		Where("branch_id = ? AND biz_date = ? AND cut_type = ? AND excluded_at IS NULL",
			branchID, day, cut).
		Updates(map[string]any{
			"excluded_at":     now,
			"excluded_reason": "manual",
			"updated_at":      now,
		})
	if res.Error != nil {
		return 0, fmt.Errorf("exclude pig_cut_price_samples: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}

// upsertCutPriceBaseline 写一行单价基线(先读后写,见 forecast.go 的同款说明)。
func (s *Service) upsertCutPriceBaseline(ctx context.Context, tx *gorm.DB, row model.PigCutPriceBaseline,
	existing map[model.CutType]model.PigCutPriceBaseline) error {
	fields := map[string]any{
		"unit_cost_yuan":     row.UnitCostYuan,
		"ref_pig_price_yuan": row.RefPigPriceYuan,
		"sample_count":       row.SampleCount,
		"source_branch_id":   row.SourceBranchID,
		"borrowed":           row.Borrowed,
		"computed_at":        row.ComputedAt,
	}
	if prev, ok := existing[row.CutType]; ok && prev.ID != "" {
		if err := tx.WithContext(ctx).Model(&model.PigCutPriceBaseline{}).
			Where("id = ?", prev.ID).Updates(fields).Error; err != nil {
			return fmt.Errorf("update pig_cut_price_baseline %s: %w", prev.ID, err)
		}
		return nil
	}
	if err := tx.WithContext(ctx).Create(&row).Error; err != nil {
		return fmt.Errorf("create pig_cut_price_baseline: %w", err)
	}
	return nil
}

// resolveBorrowedPriceBaselines 解析本店的借用配置 ⇒ 单价基线该向谁借。
//
// ⚠️ 价格只看**部位**(不看体重段):单价基线本来就是 (店, 部位) 一条。
func (s *Service) resolveBorrowedPriceBaselines(ctx context.Context, branchID string) (
	map[model.CutType]model.PigCutPriceBaseline, error) {
	out := map[model.CutType]model.PigCutPriceBaseline{}
	cfgs, err := s.listEnabledBorrows(ctx, branchID)
	if err != nil {
		return nil, err
	}
	if len(cfgs) == 0 {
		return out, nil
	}
	srcRows := map[string]map[model.CutType]model.PigCutPriceBaseline{}
	for _, cfg := range cfgs {
		if _, ok := srcRows[cfg.SourceBranchID]; ok {
			continue
		}
		rows, err := s.loadBranchCutPriceBaselineRows(ctx, cfg.SourceBranchID)
		if err != nil {
			return nil, err
		}
		srcRows[cfg.SourceBranchID] = rows
	}
	candidates := map[model.CutType]bool{}
	for _, cfg := range cfgs {
		for c := range srcRows[cfg.SourceBranchID] {
			if cfg.CutType != "" && cfg.CutType != c {
				continue
			}
			candidates[c] = true
		}
	}
	for c := range candidates {
		best, found := pickPriceBorrowWinner(cfgs, srcRows, c)
		if !found {
			continue
		}
		row, ok := srcRows[best.SourceBranchID][c]
		if !ok {
			continue
		}
		// ⚠️ 被借店自己也不可用 ⇒ **不借用**(走退取链)。
		if row.SampleCount < MinForecastSamples || !row.UnitCostYuan.IsPositive() {
			continue
		}
		row.BranchID = branchID
		row.SourceBranchID = best.SourceBranchID
		row.Borrowed = true
		row.ID = cutPriceBaselineID(branchID, c)
		out[c] = row
	}
	return out, nil
}

// loadBranchCutPriceBaselineRows 读某店(被借方)的单价基线行,索引 = 部位。
func (s *Service) loadBranchCutPriceBaselineRows(ctx context.Context, branchID string) (
	map[model.CutType]model.PigCutPriceBaseline, error) {
	out := map[model.CutType]model.PigCutPriceBaseline{}
	var rows []model.PigCutPriceBaseline
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query pig_cut_price_baselines(%s): %w", branchID, err)
	}
	for _, r := range rows {
		out[r.CutType] = r
	}
	return out, nil
}

// pickPriceBorrowWinner 选出某个部位该向谁借(确定性)。
//
// 判据:① 精确部位 > 体重段 > 整店;② 同级取**样本数多**的;③ 再并列取门店号小的。
func pickPriceBorrowWinner(cfgs []model.PigForecastBaselineBorrow,
	srcRows map[string]map[model.CutType]model.PigCutPriceBaseline,
	c model.CutType) (model.PigForecastBaselineBorrow, bool) {
	var best model.PigForecastBaselineBorrow
	bestScope := 0
	bestSamples := -1
	found := false
	for _, cfg := range cfgs {
		if cfg.CutType != "" && cfg.CutType != c {
			continue
		}
		scope := borrowScopeOf(cfg)
		samples := 0
		if row, ok := srcRows[cfg.SourceBranchID][c]; ok {
			samples = row.SampleCount
		}
		better := !found ||
			scope < bestScope ||
			(scope == bestScope && samples > bestSamples) ||
			(scope == bestScope && samples == bestSamples && cfg.SourceBranchID < best.SourceBranchID)
		if better {
			best, bestScope, bestSamples, found = cfg, scope, samples, true
		}
	}
	return best, found
}

// ---- 应用:建议成本单价 ----

// suggestedCutUnitPrices 算各部位的**建议成本单价**(元/kg)。
//
// # 口径(owner 的原话)
//
//	「应用此基线(注意应用过程中需要结合单猪的采购价,如基线是 14.6/kg 的,
//	  那 16/kg 的猪每个部位单价都要按比例上涨)」
//
//	⇒ 逐猪逐部位:单价 = 基线单价 × (这头猪的进价 ÷ 基线的参考猪进价)
//	⇒ 再按**建议分割 kg** 加权汇总成部位口径。
//
// # ⚠️ 断环
//
//	基线只用**该营业日之前**的样本(见 loadPriceBaselinesAsOf)
//	⇒ 今天的建议只依赖昨天为止的事实 ⇒ 不自证。
//
// # ⚠️ 边界②:各部位合计价值 ≤ 猪的总进货金额
//
//	由 pigSuggestedUnitPrices 的封顶保证(超了就按比例缩,只往下缩)。
//
// 返回:部位 → 建议成本单价;部位 → 单价来源标记;是否有猪被**封顶**。
//
// ⚠️ capped 的意义:owner 给的边界②(各部位合计价值 ≤ 猪的总进货金额)是**静默**生效的
//   ⇒ 门店看不到"基线给的价比猪还贵,系统帮我砍了"⇒ 必须让他看见。
func (s *Service) suggestedCutUnitPrices(ctx context.Context, branchID string,
	pigs []model.WholePig, splitPerPig map[string]map[string]decimal.Decimal,
	bizDate time.Time) (map[string]decimal.Decimal, map[string]string, bool, error) {
	out := map[string]decimal.Decimal{}
	sources := map[string]string{}
	if len(pigs) == 0 {
		return out, sources, false, nil
	}
	baselines, err := s.loadPriceBaselinesAsOf(ctx, branchID, bizDate)
	if err != nil {
		return nil, nil, false, err
	}
	storeAvg, err := s.storeWeightedAvgCostPrice(ctx, branchID)
	if err != nil {
		return nil, nil, false, err
	}
	num := map[string]decimal.Decimal{}
	den := map[string]decimal.Decimal{}
	srcWeight := map[string]map[string]decimal.Decimal{}
	anyPriced := map[string]bool{}
	seenCuts := map[string]bool{}
	anyCapped := false
	for _, p := range pigs {
		split := splitPerPig[p.ID]
		if len(split) == 0 {
			continue
		}
		for cut := range split {
			seenCuts[cut] = true
		}
		perCut, perCutSource, capped, err := s.pigSuggestedUnitPrices(ctx, branchID, p, split, baselines, storeAvg)
		if err != nil {
			return nil, nil, false, err
		}
		if capped {
			anyCapped = true
		}
		for cut, v := range perCut {
			kg := split[cut]
			if !kg.IsPositive() {
				continue
			}
			num[cut] = num[cut].Add(v.Mul(kg))
			den[cut] = den[cut].Add(kg)
			anyPriced[cut] = true
			// 来源标记:按**贡献的 kg** 投票,票数最多的那个胜出(并列取字典序小的 ⇒ 确定性)。
			src := perCutSource[cut]
			if src == "" {
				src = PriceSourceNone
			}
			if srcWeight[cut] == nil {
				srcWeight[cut] = map[string]decimal.Decimal{}
			}
			srcWeight[cut][src] = srcWeight[cut][src].Add(kg)
		}
	}
	for cut, d := range den {
		if !d.IsPositive() {
			continue
		}
		out[cut] = num[cut].Div(d)
		sources[cut] = pickPriceSource(srcWeight[cut])
	}
	// ⚠️ **取不到价的部位也要给来源**(= none)⇒ 前端能区分
	//   「这个部位没有基线/没有价」与「压根没这个部位的数据」。
	for cut := range seenCuts {
		if !anyPriced[cut] {
			sources[cut] = PriceSourceNone
		}
	}
	return out, sources, anyCapped, nil
}

// pickPriceSource 从来源投票里选出胜者(票数最多;并列取字典序小的 ⇒ 确定性)。
func pickPriceSource(weights map[string]decimal.Decimal) string {
	best := ""
	bestW := decimal.Zero
	for _, src := range sortedSourceKeys(weights) {
		w := weights[src]
		if w.GreaterThan(bestW) {
			best, bestW = src, w
		}
	}
	if best == "" {
		return PriceSourceNone
	}
	return best
}

// sortedSourceKeys 来源标记的排序键(确定性)。
func sortedSourceKeys(m map[string]decimal.Decimal) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// pigSuggestedUnitPrices 算**一头猪**各部位的建议成本单价。
//
// # 退取链(owner 指定的顺序)
//
//	① **单价基线** × (这头猪的进价 ÷ 基线参考猪进价)
//	② **门店整店加权平均价**(退取链①)
//	③ **LLM 缓存单价**(退取链②)
//	④ **LLM 重新给价**(退取链③)—— 由 RefreshCutUnitPriceFromLLM 显式触发,
//	   **不在报表路径里同步调 LLM**(会把看板拖到 30s 超时)。
//
// # ⚠️ 边界②(owner 给定):各部位合计价值 ≤ 猪的总进货金额
//
//	超了 ⇒ 按比例把**单价**缩下去(只往下缩,与重量同一条原则)
//	⇒ 绝不允许"建议出来的货比猪还贵"。
// 返回的 capped = 本次是否真的触发了封顶(缩放)。
func (s *Service) pigSuggestedUnitPrices(ctx context.Context, branchID string, p model.WholePig,
	split map[string]decimal.Decimal, baselines map[model.CutType]priceBaselineValue,
	storeAvg decimal.Decimal) (map[string]decimal.Decimal, map[string]string, bool, error) {
	out := map[string]decimal.Decimal{}
	sources := map[string]string{}
	pigPrice := pigUnitPriceYuan(p)
	for cut, kg := range split {
		if !kg.IsPositive() {
			continue
		}
		v, src, err := s.resolveCutUnitPrice(ctx, branchID, model.CutType(cut), pigPrice, baselines, storeAvg)
		if err != nil {
			return nil, nil, false, err
		}
		if src == PriceSourceNone || !v.IsPositive() {
			// ⚠️ 取不到价 ⇒ 该部位**没有**建议成本单价(前端显示「—」,不是 0)。
			sources[cut] = PriceSourceNone
			continue
		}
		out[cut] = v
		sources[cut] = src
	}
	// ---- 封顶:各部位合计价值 ≤ 猪的总进货金额 ----
	pigCost := p.PurchaseCostYuan
	var total decimal.Decimal
	for cut, v := range out {
		total = total.Add(v.Mul(split[cut]))
	}
	capped := false
	if pigCost.IsPositive() && total.GreaterThan(pigCost) {
		factor := pigCost.Div(total)
		for cut := range out {
			out[cut] = out[cut].Mul(factor)
		}
		capped = true
		s.pubLogger.Warn("建议成本单价合计超过猪的总进货金额,已按比例缩到总进货金额",
			"branch_id", branchID, "pig_id", p.ID,
			"pig_cost_yuan", pigCost.String(), "suggested_value_yuan", total.String())
	}
	return out, sources, capped, nil
}

// resolveCutUnitPrice 走**退取链**取某个部位的成本单价。
func (s *Service) resolveCutUnitPrice(ctx context.Context, branchID string, cut model.CutType,
	pigPrice decimal.Decimal, baselines map[model.CutType]priceBaselineValue,
	storeAvg decimal.Decimal) (decimal.Decimal, string, error) {
	// ① 单价基线(按单猪进价缩放)。
	if b, ok := baselines[cut]; ok && b.UnitCostYuan.IsPositive() && b.SampleCount >= MinForecastSamples {
		v := b.UnitCostYuan
		if b.RefPigPriceYuan.IsPositive() && pigPrice.IsPositive() {
			v = b.UnitCostYuan.Mul(pigPrice).Div(b.RefPigPriceYuan)
		}
		return v, PriceSourceBaseline, nil
	}
	// ② 门店整店加权平均价。
	if storeAvg.IsPositive() {
		return storeAvg, PriceSourceStoreAvg, nil
	}
	// ③ LLM 缓存单价。
	v, ok, err := s.cachedCutUnitPrice(ctx, branchID, cut)
	if err != nil {
		return decimal.Zero, PriceSourceNone, err
	}
	if ok {
		return v, PriceSourceLLMCache, nil
	}
	return decimal.Zero, PriceSourceNone, nil
}

// loadPriceBaselinesAsOf 读"**截至某营业日之前**"的单价基线(**现算**,不读物化表)。
//
// # ⚠️ 为什么现算而不读物化表
//
//	物化表(pig_cut_price_baselines)是给门店**看**的,它的刷新时机由人工点"重算"决定
//	⇒ 若算建议成本单价时读它,今天的样本可能已经把今天的基线改了
//	⇒ 于是"今天的建议成本单价"依赖"今天的样本" ⇒ **同日循环依赖**。
//
//	⇒ 现算 as-of 版本 ⇒ 结构上不可能自证。
//
// ⚠️ cutoff 是**不含**的:算 D 日的建议时只用 < D 的样本。
func (s *Service) loadPriceBaselinesAsOf(ctx context.Context, branchID string, before time.Time) (
	map[model.CutType]priceBaselineValue, error) {
	rows, err := s.loadCutPriceSamples(ctx, branchID, "")
	if err != nil {
		return nil, err
	}
	cutoff := before.In(s.bizTZOrUTC()).Format("2006-01-02")
	live := make([]model.PigCutPriceSample, 0, len(rows))
	for _, r := range rows {
		if r.ExcludedAt != nil {
			continue
		}
		// ⚠️ 纯日期字符串比较 = 时间序(两边都是零填充 ISO)⇒ 跨驱动安全。
		if r.BizDate >= cutoff {
			continue
		}
		live = append(live, r)
	}
	return aggregatePriceBaselines(live), nil
}

// storeWeightedAvgCostPrice 退取链①:**门店整店加权平均价**(元/kg)。
//
// 口径 = 该店**全部有效单价样本**的倒推进货单价,按**销量**加权平均
// ⇒ 即"这家店平均每个部位的成本单价是多少"。
//
// ⚠️ 没有任何样本 ⇒ 返回 0(调用方继续往下走退取链)。
func (s *Service) storeWeightedAvgCostPrice(ctx context.Context, branchID string) (decimal.Decimal, error) {
	rows, err := s.loadCutPriceSamples(ctx, branchID, "")
	if err != nil {
		return decimal.Zero, err
	}
	var num, den decimal.Decimal
	for _, r := range rows {
		if r.ExcludedAt != nil || !r.SoldKg.IsPositive() || !r.CostPriceYuan.IsPositive() {
			continue
		}
		num = num.Add(r.CostPriceYuan.Mul(r.SoldKg))
		den = den.Add(r.SoldKg)
	}
	if !den.IsPositive() {
		return decimal.Zero, nil
	}
	return num.Div(den), nil
}

// cachedCutUnitPrice 退取链②:读 **LLM 缓存单价**。
func (s *Service) cachedCutUnitPrice(ctx context.Context, branchID string, cut model.CutType) (
	decimal.Decimal, bool, error) {
	var row model.CutUnitPriceCache
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND cut_type = ?", branchID, cut).
		First(&row).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return decimal.Zero, false, nil
		}
		return decimal.Zero, false, fmt.Errorf("query cut_unit_price_caches: %w", err)
	}
	if !row.UnitPriceYuan.IsPositive() {
		return decimal.Zero, false, nil
	}
	return row.UnitPriceYuan, true, nil
}

// saveCutUnitPriceCache 写 LLM 单价缓存(先读后写)。
func (s *Service) saveCutUnitPriceCache(ctx context.Context, branchID string, cut model.CutType,
	price decimal.Decimal, source string) error {
	if !price.IsPositive() {
		return nil
	}
	now := s.now()
	row := model.CutUnitPriceCache{
		ID:            surrogateID("CUP", branchID, string(cut)),
		BranchID:      branchID,
		CutType:       cut,
		UnitPriceYuan: price,
		Source:        source,
		ComputedAt:    now,
	}
	var cur model.CutUnitPriceCache
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND cut_type = ?", branchID, cut).
		First(&cur).Error
	switch {
	case err == nil:
		if err := s.db.WithContext(ctx).Model(&model.CutUnitPriceCache{}).
			Where("id = ?", cur.ID).Updates(map[string]any{
			"unit_price_yuan": price,
			"source":          source,
			"computed_at":     now,
		}).Error; err != nil {
			return fmt.Errorf("update cut_unit_price_caches: %w", err)
		}
		return nil
	case err == gorm.ErrRecordNotFound:
		if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
			return fmt.Errorf("create cut_unit_price_caches: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("query cut_unit_price_caches: %w", err)
	}
}

// captureLLMCutPrices 把 LLM 给的部位单价**写进缓存**(退取链②的来源)。
//
// ⚠️ 只接受**合理区间**内的价(0 < price < maxLLMUnitPriceYuan):
//	LLM 可能给出离谱数字 ⇒ 写进缓存会污染退取链。
func (s *Service) captureLLMCutPrices(ctx context.Context, branchID string, cuts []PredictedCut) error {
	for _, c := range cuts {
		if c.UnitPriceYuan == nil || !c.UnitPriceYuan.IsPositive() {
			continue
		}
		if c.UnitPriceYuan.GreaterThan(decimal.NewFromInt(maxLLMUnitPriceYuan)) {
			continue
		}
		if err := s.saveCutUnitPriceCache(ctx, branchID, c.CutType, *c.UnitPriceYuan, "llm"); err != nil {
			return err
		}
	}
	return nil
}

// maxLLMUnitPriceYuan LLM 单价的**合理性上限**(元/kg)。
//
// 猪肉部位单价超过这个数基本是 LLM 幻觉 ⇒ 直接丢弃,不入缓存。
const maxLLMUnitPriceYuan = 1000

// RefreshCutUnitPriceFromLLM 退取链③:**兜底 LLM 重新给价**。
//
// # 为什么不放在报表路径里同步调
//
//	LLM 调用要 10~30s ⇒ 放报表里会把毛利看板拖到超时(2026-10-26 踩过的坑)。
//	⇒ 退取链的前两级(整店均价 / LLM 缓存)覆盖绝大多数情况;
//	③ 只在**门店主动触发**时跑一次,跑完写进缓存 ⇒ 之后②就能命中。
//
// # 怎么调
//
//	复用注入的 predictFn(生产 = Dapr Conversation),拿它返回的 cuts[].unit_price_yuan。
//	⇒ 单测里注入 fake predictFn 就能覆盖整条链。
func (s *Service) RefreshCutUnitPriceFromLLM(ctx context.Context, branchID string,
	cut model.CutType) (decimal.Decimal, error) {
	if branchID == "" {
		return decimal.Zero, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if !cut.Valid() {
		return decimal.Zero, fmt.Errorf("%w: cut_type %q 不在枚举内", ErrInvalidInput, cut)
	}
	pig, err := s.recentPigForPrice(ctx, branchID)
	if err != nil {
		return decimal.Zero, err
	}
	// ⚠️ 焦点部位 = **只要这一个部位**的价(owner D8:LLM 只补缺的部分)。
	//   这里问全集也只会取一个 cut 的价 ⇒ 但提示词问全集 = 产整猪方案 ⇒ 浪费。
	result, err := s.predictFn(ctx, pig, nil, []model.CutType{cut})
	if err != nil {
		return decimal.Zero, fmt.Errorf("LLM 重新给价失败: %w", err)
	}
	for _, c := range result.Cuts {
		if c.CutType != cut || c.UnitPriceYuan == nil {
			continue
		}
		if !c.UnitPriceYuan.IsPositive() ||
			c.UnitPriceYuan.GreaterThan(decimal.NewFromInt(maxLLMUnitPriceYuan)) {
			return decimal.Zero, fmt.Errorf(
				"%w: LLM 给的 %s 单价不合理(%s 元/kg)—— 已拒绝采用", ErrInvalidInput, cut, c.UnitPriceYuan)
		}
		if err := s.saveCutUnitPriceCache(ctx, branchID, cut, *c.UnitPriceYuan, "llm"); err != nil {
			return decimal.Zero, err
		}
		return *c.UnitPriceYuan, nil
	}
	return decimal.Zero, fmt.Errorf("%w: LLM 没有给出 %s 的单价", ErrInvalidInput, cut)
}

// recentPigForPrice 取店里**最近一头猪**当作 LLM 给价的上下文。
//
// ⚠️ 没有猪 ⇒ 给价没有上下文(不知道猪多重)⇒ 直接报错,不猜。
func (s *Service) recentPigForPrice(ctx context.Context, branchID string) (*model.WholePig, error) {
	var pig model.WholePig
	err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("arrived_at DESC").
		First(&pig).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, fmt.Errorf("%w: 店里还没有录过猪,没法让 AI 给部位单价", ErrInvalidInput)
		}
		return nil, fmt.Errorf("query whole_pigs(price): %w", err)
	}
	return &pig, nil
}

// pigUnitPriceYuan 这头猪的**进价**(元/kg)。
//
// ⚠️ 优先用门店录的单价;没录就用总成本 ÷ 毛重(兜底)。
func pigUnitPriceYuan(p model.WholePig) decimal.Decimal {
	if p.PurchaseUnitPriceYuan.IsPositive() {
		return p.PurchaseUnitPriceYuan
	}
	if p.GrossWeightKg.IsPositive() && p.PurchaseCostYuan.IsPositive() {
		return p.PurchaseCostYuan.Div(p.GrossWeightKg)
	}
	return decimal.Zero
}
