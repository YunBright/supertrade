// Package service / forecast_maintain.go —— **基线维护**(owner 2026-10-30 第 3 条)。
//
// # 要解决什么
//
//	基线是**统计拟合**出来的 ⇒ 数据脏了(补录错、搬台漏、盘点补录)基线就歪
//	⇒ 管理员得能 ① **删掉**不准确/脏的那一行;② **按某一天**重新生成一条干净的。
//
// # 两条维护动作
//
//	① `DeleteForecastBaseline` —— 删掉 (体重段 × 部位) 那一行(只删**本店**的)。
//	② `GenerateForecastBaselines` —— 选**数据完整的一天**生成新基线(新增或覆盖)。
//
// # ⚠️ 生成基线**不设 ≥5 门槛**(owner 明确要求)
//
//	「只要那天的录入、搬台(含 0 搬台)、销售、盘点都全就可以生成」
//	⇒ 生成的行 `manual_generated = true`
//	⇒ `ForecastBaseline.Usable()` 认它(见 forecast.go)。
//
// # ⚠️ 前置校验:那一天的数据必须**完整**
//
//	缺哪一项就 400 并**点名缺哪一项** —— 门店才知道要补什么。
//	五项:① 录入(进猪) ② 搬台(含 0 kg 的"今天不搬台"标记) ③ 销售(零售或统收)
//	     ④ 盘点(**完整**盘点单 is_complete = true)
//	     ⑤ 分割样本(实录或估计样本 —— 否则生成结果是空的,2026-10-30 门店实测)。
//
// # 手动跨店选用
//
//	`source_branch_id` 非空 ⇒ 用**别店那一天**的数据生成到**本店的基线表**
//	⇒ 行的 source_branch_id 记被借方(= 手动版的"跨店借用")。
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// ForecastBaselineGenerateInput 按某一天生成基线的入参。
type ForecastBaselineGenerateInput struct {
	// Date 是"用哪一天的数据"(YYYY-MM-DD);**只用那一天**,不是累计。
	Date string `json:"date"`
	// SourceBranchID 是**样本来源门店**;空 = 本店。
	SourceBranchID string `json:"source_branch_id"`
	// Overwrite = true ⇒ 新增或覆盖(upsert);false ⇒ 只新增(已有分层不动)。
	Overwrite bool `json:"overwrite"`
}

// ForecastBaselineGenerateResult 生成结果。
type ForecastBaselineGenerateResult struct {
	// Date 是用的那一天。
	Date string `json:"date"`
	// SourceBranchID 是**入参原样回显**(空 = 用本店自己的数据)。
	SourceBranchID string `json:"source_branch_id"`
	// Written 是写入了多少行;Skipped 是因为"已有分层"而没动的行数(overwrite=false 时)。
	Written int `json:"written"`
	Skipped int `json:"skipped"`
	// Baselines 是写入的那些行(门店要看"到底生成了什么")。
	Baselines []model.PigForecastBaseline `json:"baselines"`
}

// DeleteForecastBaseline 删掉一条基线(不准确/脏的那一行)。
//
// ⚠️ **只删本店那一行**:别家店的基线不该被本店删。
func (s *Service) DeleteForecastBaseline(ctx context.Context, branchID, weightBand, cutType string) (int, error) {
	if branchID == "" {
		return 0, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	band := strings.TrimSpace(weightBand)
	cut := model.CutType(strings.TrimSpace(cutType))
	if band == "" {
		return 0, fmt.Errorf("%w: weight_band 必填(要删哪一段的基线)", ErrInvalidInput)
	}
	if cut == "" {
		return 0, fmt.Errorf("%w: cut_type 必填(要删哪个部位的基线)", ErrInvalidInput)
	}
	res := s.db.WithContext(ctx).
		Where("branch_id = ? AND weight_band = ? AND cut_type = ?", branchID, band, cut).
		Delete(&model.PigForecastBaseline{})
	if res.Error != nil {
		return 0, fmt.Errorf("delete pig_forecast_baseline: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}

// GenerateForecastBaselines 按某一天生成基线(新增或覆盖到**本店**的基线表)。
//
// # 流程
//
//	① 校验那一天的数据**完整**(缺哪一项点哪一项);
//	② 取那一天的**实录池**(当天进场的猪的 pig_cuts)与**估计池**(pig_cut_estimates 那天的行);
//	③ 按**既有的选池口径**算 stats(实录 ≥5 用实录,否则估计 ≥5 用估计,否则取多的留痕);
//	④ 写进 pig_forecast_baselines,带 manual_generated = true 与 generated_from_date。
func (s *Service) GenerateForecastBaselines(ctx context.Context, branchID string,
	in ForecastBaselineGenerateInput) (*ForecastBaselineGenerateResult, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	dayKey := strings.TrimSpace(in.Date)
	if dayKey == "" {
		return nil, fmt.Errorf("%w: date 必填(用哪一天的数据生成基线)", ErrInvalidInput)
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

	// ---- ① 样本(实录池 + 估计池) ----
	//
	// ⚠️ **先取样本,再校验**:完整性校验的第 ⑤ 项(分割样本)必须用
	//   **实际会用来拟合的那一批样本**判定
	//   ⇒ 否则会出现"校验说有样本、生成却一条都没写出来"的空成功。
	recorded, err := s.loadRecordedSamplesForDay(ctx, source, day)
	if err != nil {
		return nil, err
	}
	estimated, err := s.loadEstimatedSamplesForDay(ctx, source, day)
	if err != nil {
		return nil, err
	}

	// ---- ② 完整性校验 ----
	gaps, err := s.dayDataGaps(ctx, source, day, recorded, estimated)
	if err != nil {
		return nil, err
	}
	if len(gaps) > 0 {
		return nil, fmt.Errorf("%w: %s 这一天数据不完整,缺:%s —— 补齐后才能生成基线",
			ErrInvalidInput, dayKey, strings.Join(gaps, "、"))
	}

	// ---- ③ 选池 + 写库 ----
	keys := forecastBucketKeys(recorded, estimated)
	if len(keys) == 0 {
		return &ForecastBaselineGenerateResult{
			Date:           dayKey,
			SourceBranchID: in.SourceBranchID,
			Written:        0,
			Skipped:        0,
			Baselines:      []model.PigForecastBaseline{},
		}, nil
	}

	now := s.now()
	out := &ForecastBaselineGenerateResult{
		Date:           dayKey,
		SourceBranchID: in.SourceBranchID,
		Baselines:      []model.PigForecastBaseline{},
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		existing, err := loadBaselineRows(ctx, tx, branchID)
		if err != nil {
			return err
		}
		for _, k := range keys {
			recList := recorded[k]
			estList := estimated[k]
			recN, estN := len(recList), len(estList)

			// 选池口径与自动重算**完全一致**(同一套口径,不然手动生成的数会跟自动的对不上)。
			var useList []decimal.Decimal
			source2 := SampleSourceRecorded
			switch {
			case recN >= MinForecastSamples:
				useList, source2 = recList, SampleSourceRecorded
			case estN >= MinForecastSamples:
				useList, source2 = estList, SampleSourceEstimated
			default:
				if estN > recN {
					useList, source2 = estList, SampleSourceEstimated
				} else {
					useList, source2 = recList, SampleSourceRecorded
				}
			}
			if len(useList) == 0 {
				continue
			}
			// overwrite=false ⇒ 已有分层**不动**(报 skipped)。
			if !in.Overwrite {
				if _, ok := existing[k]; ok {
					out.Skipped++
					continue
				}
			}
			mean, p10, p50, p90 := statsOf(useList)
			row := model.PigForecastBaseline{
				ID:                   forecastBaselineID(branchID, k.band, k.cut),
				BranchID:             branchID,
				WeightBand:           k.band,
				CutType:              k.cut,
				SampleCount:          len(useList),
				RecordedSampleCount:  recN,
				EstimatedSampleCount: estN,
				SampleSource:         source2,
				// 手动跨店选用时 = 被借方;用本店数据时 = 本店。
				SourceBranchID:    source,
				ManualGenerated:   true,
				GeneratedFromDate: dayKey,
				MeanKg:            mean,
				P10Kg:             p10,
				P50Kg:             p50,
				P90Kg:             p90,
				ComputedAt:        now,
			}
			if err := s.upsertForecastBaseline(ctx, tx, row, existing); err != nil {
				return err
			}
			out.Written++
			out.Baselines = append(out.Baselines, row)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// dayDataGaps 返回那一天**缺**的项目(空 = 数据完整)。
//
// ⚠️ 报给门店的文案要**点名缺哪一项** —— "数据不完整"门店没法行动。
//
// recorded / estimated 是**实际会用来拟合的那一批样本**
// ⇒ 第 ⑤ 项的判据与生成口径**完全一致**(不会"校验说有、生成却 0 行")。
func (s *Service) dayDataGaps(ctx context.Context, branchID string, day time.Time,
	recorded, estimated map[forecastBucketKey][]decimal.Decimal) ([]string, error) {
	var gaps []string
	start, end := s.businessDayBounds(day)
	dayKey := day.In(s.bizTZOrUTC()).Format("2006-01-02")

	// ① 录入:当天进了猪。
	var pigCnt int64
	if err := s.db.WithContext(ctx).Model(&model.WholePig{}).
		Where("branch_id = ? AND arrived_at >= ? AND arrived_at < ?", branchID, start, end).
		Count(&pigCnt).Error; err != nil {
		return nil, fmt.Errorf("count whole_pigs: %w", err)
	}
	if pigCnt == 0 {
		gaps = append(gaps, "录入(当天没有进猪)")
	}

	// ② 搬台:**含 0 kg 的「今天不搬台」标记**也算(门店口径:有时整日不搬台)。
	var loadCnt int64
	if err := s.db.WithContext(ctx).Model(&model.CounterLoadRecord{}).
		Where("branch_id = ? AND "+s.fmDateColEqExpr("biz_date"), branchID, dayKey).
		Count(&loadCnt).Error; err != nil {
		return nil, fmt.Errorf("count counter_loads: %w", err)
	}
	if loadCnt == 0 {
		gaps = append(gaps, "搬台(当天没有搬台流水,含「今天不搬台」的标记)")
	}

	// ③ 销售:零售或统收,至少一条流水。
	//
	// ⚠️ **必须跟毛利看板走同一条销售读路径**(2026-10-30 门店实测反馈):
	//
	//	有源门店(店 00/01)的销售在**它自己的 cube 源**里,
	//	本地 `line_sales_by_pigs` 是**空的**(那是无源门店/自营店才有的本地 POS 事件)
	//	⇒ 只查本地表 ⇒ 明明卖了一整天,却报「缺:销售(当天没有销售流水)」
	//	⇒ 门店点「按一天生成基线」直接被挡(实测:富百家 2026-10-29 就是这样被挡的)。
	//	⇒ 这里交给 hasSalesOnDay(cube / local 由 salesReadPath 判定)—— 与看板完全一致。
	hasSales, err := s.hasSalesOnDay(ctx, branchID, day)
	if err != nil {
		return nil, err
	}
	if !hasSales {
		gaps = append(gaps, "销售(当天没有销售流水)")
	}

	// ④ 盘点:**完整**盘点单(is_complete = true);半盘不算。
	var stCnt int64
	if err := s.db.WithContext(ctx).Model(&model.PorkCutsStocktake{}).
		Where("branch_id = ? AND is_complete = ? AND taken_at >= ? AND taken_at < ?",
			branchID, true, start, end).
		Count(&stCnt).Error; err != nil {
		return nil, fmt.Errorf("count pork_cuts_stocktakes: %w", err)
	}
	if stCnt == 0 {
		gaps = append(gaps, "盘点(当天没有完整盘点单)")
	}

	// ⑤ 分割补录:那天得有**可拟合的分割重量**(实录 或 估计)。
	//
	// ⚠️ 为什么这一项也算"完整性"(2026-10-30 门店实测):
	//
	//	没有分割重量 ⇒ 生成结果是 **written=0**
	//	⇒ 门店点「生成」只看到"已生成 0 条基线" ⇒ 以为按钮坏了,却不知道要做什么。
	//	⇒ 宁可 400 点名"缺:分割补录",也不要给一个**空的成功**。
	//
	//	判据:实录 ∪ 估计 **都**空才算缺
	//	⇒ 有估计样本(A 方案:盘点后自动倒推出的分割重量)的日子照样能生成
	//	  —— 那正是 A 方案存在的意义。
	//
	//	⚠️ **没进猪的那天不报这一项**:没猪 ⇒ 天然没有分割样本
	//	⇒ 那是"缺录入"的后果,不是另一个独立缺口(别让门店补两遍)。
	if pigCnt > 0 && len(recorded) == 0 && len(estimated) == 0 {
		gaps = append(gaps, "分割补录(那天没有可用于拟合的分割重量,请补录那天的分割)")
	}
	return gaps, nil
}

// hasSalesOnDay 判断某营业日**有没有销售流水**(零售或统收)。
//
// # ⚠️ 必须跟毛利看板**同一条读路径**(2026-10-30 门店实测反馈)
//
//	有源门店(店 00/01 富百家/真美门店):销售在**它自己的 cube 源**里
//	                                    ⇒ 本地 `line_sales_by_pigs` 是空的;
//	无源门店(店 10 自营店)            :销售在本地 PG(本地 POS 的 sale.paid 事件)。
//
//	⇒ 只查本地表 ⇒ 有源门店**永远**报「缺:销售」
//	⇒ 「按一天生成基线」按钮对有源门店直接不可用(富百家 2026-10-29 实测)。
//
// ⇒ 这里用 [Service.salesReadPath] 判定(cube / local),与看板**完全一致**
//
//	(cube 侧也按**映射表登记过的货号**过滤,非猪肉不混进来)。
//
// # cube 读失败 ⇒ **上抛**(不回落成"没卖")
//
//	回落 = 把一次链路故障显示成「这家店没卖东西」
//	⇒ 那正是 2026-10-26 门店看到"毛利看板没有销售"的形状 ⇒ 不能再来一次。
//
// # 统收(bulk_settlements)是**本地表** ⇒ 所有店都查。
func (s *Service) hasSalesOnDay(ctx context.Context, branchID string, day time.Time) (bool, error) {
	loc := s.bizTZOrUTC()
	dayKey := day.In(loc).Format("2006-01-02")

	// ---- ① 零售:走看板那条路 ----
	useCube, err := s.salesReadPath(ctx, branchID)
	if err != nil {
		return false, err
	}
	if useCube {
		skus, _, err := s.mappedSkus(ctx, branchID)
		if err != nil {
			return false, err
		}
		// ⚠️ 一个货号都没登记 ⇒ 谈不上"有零售销售"(与 loadCubeSalesInto 同口径,不报错)。
		if len(skus) > 0 {
			rows, err := s.loadCubeSales(ctx, branchID, dayKey, skus)
			if err != nil {
				// ⚠️ 权限类错误(403/401)**原样上抛** —— 别把"没权限"说成"数据源挂了";
				//   其余(cube 查询失败/超时)⇒ 503「数据源暂不可用」,
				//   **不能**落成 400「缺:销售」—— 那等于告诉门店"你那天没卖"。
				if errors.Is(err, cubeclient.ErrCubeForbidden) || errors.Is(err, cubeclient.ErrCubeAuthRejected) {
					return false, err
				}
				return false, fmt.Errorf("%w: 读门店销售失败: %v", ErrCubeUnavailable, err)
			}
			for _, r := range rows {
				// ⚠️ 判据与引擎一致(见 loadCubeSalesInto):数量或金额任一非零就算有销售。
				if !r.QtyKg.IsZero() || !r.AmountYuan.IsZero() {
					return true, nil
				}
			}
		}
	} else {
		var saleCnt int64
		if err := s.db.WithContext(ctx).Model(&model.LineSalesByPig{}).
			Where("branch_id = ? AND "+s.fmSaleDateExpr(), branchID, dayKey).
			Count(&saleCnt).Error; err != nil {
			return false, fmt.Errorf("count line_sales_by_pigs: %w", err)
		}
		if saleCnt > 0 {
			return true, nil
		}
	}

	// ---- ② 统收(本地表,所有店都有) ----
	var settleCnt int64
	if err := s.db.WithContext(ctx).Model(&model.BulkSettlement{}).
		Where("branch_id = ? AND "+s.fmDateColEqExpr("settle_date"), branchID, dayKey).
		Count(&settleCnt).Error; err != nil {
		return false, fmt.Errorf("count bulk_settlements: %w", err)
	}
	return settleCnt > 0, nil
}

// loadRecordedSamplesForDay 取某店**那一天**的实录样本(当天进场的猪的分割补录)。
//
// ⚠️ JOIN 两边都按 branch_id 过滤 ⇒ 就算数据脏了也不会把别店的猪算进来。
func (s *Service) loadRecordedSamplesForDay(ctx context.Context, branchID string, day time.Time) (
	map[forecastBucketKey][]decimal.Decimal, error) {
	out := map[forecastBucketKey][]decimal.Decimal{}
	start, end := s.businessDayBounds(day)

	var pigCuts []model.PigCut
	if err := s.db.WithContext(ctx).
		Joins("JOIN whole_pigs ON whole_pigs.id = pig_cuts.pig_id").
		Where("pig_cuts.branch_id = ? AND whole_pigs.branch_id = ?"+
			" AND whole_pigs.arrived_at >= ? AND whole_pigs.arrived_at < ?",
			branchID, branchID, start, end).
		Find(&pigCuts).Error; err != nil {
		return nil, fmt.Errorf("query pig_cuts(day): %w", err)
	}
	if len(pigCuts) == 0 {
		return out, nil
	}
	var pigs []model.WholePig
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND arrived_at >= ? AND arrived_at < ?", branchID, start, end).
		Find(&pigs).Error; err != nil {
		return nil, fmt.Errorf("query whole_pigs(day): %w", err)
	}
	bandOf := map[string]string{}
	for _, p := range pigs {
		bandOf[p.ID] = WeightBandOf(p.GrossWeightKg)
	}
	// 同一头猪同一部位可能有多行 ⇒ 先求和,**一头猪一个样本**。
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

// loadEstimatedSamplesForDay 取某店**那一天**的估计样本(pig_cut_estimates 里那天的行)。
func (s *Service) loadEstimatedSamplesForDay(ctx context.Context, branchID string, day time.Time) (
	map[forecastBucketKey][]decimal.Decimal, error) {
	out := map[forecastBucketKey][]decimal.Decimal{}
	dayKey := day.In(s.bizTZOrUTC()).Format("2006-01-02")
	var rows []model.PigCutEstimate
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND "+s.fmDateColEqExpr("biz_date"), branchID, dayKey).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query pig_cut_estimates(day): %w", err)
	}
	for _, r := range rows {
		band := r.WeightBand
		if band == "" {
			continue
		}
		k := forecastBucketKey{band: band, cut: r.CutType}
		out[k] = append(out[k], r.WeightKg)
	}
	return out, nil
}

// fmDateColEqExpr 返回"某 DATE 列 == 'YYYY-MM-DD'"的 SQL 表达式(跨驱动)。
//
// # ⚠️ 为什么不能直接传 datatypes.Date 当参数
//
//	datatypes.Date.Value() 返回带**自身时区**的 time.Time
//	⇒ PG 把它当 timestamptz 转成**会话时区**(UTC)的日期
//	⇒ 业务日"上海 10-02 00:00"落成 **10-01** ⇒ 查错一天(生产实测确认)。
//
//	⇒ 一律传**字符串日期** + 归一后的列表达式(同 service.fmSaleDateExpr 的做法)。
func (s *Service) fmDateColEqExpr(col string) string {
	if s.db != nil && s.db.Dialector.Name() == "postgres" {
		return fmt.Sprintf("substr(%s::text, 1, 10) = ?", col)
	}
	return fmt.Sprintf("substr(%s, 1, 10) = ?", col)
}
