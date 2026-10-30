// Package service / forecast_maintain.go —— **基线维护**(owner 2026-10-30 第 3 条)。
//
// # ⚠️ 2026-10-30 owner 重新定义模型：**样本库是一等公民，基线是派生结果**
//
//	原来的「按一天生成基线」把**样本采集**和**基线拟合**混成了一个动作
//	⇒ 已拆成两个动作（见 forecast_samples.go）：
//	  ① 「**按一天加入样本**」= 跑引擎落样本（不重算）；
//	  ② 「**重算**」= 从样本库全量拟合 ⇒ 覆盖所有分层。
//
//	⇒ 本文件里原来的 `GenerateForecastBaselines` **已移除**。
//
// # 现在这里剩什么
//
//	① `DeleteForecastBaseline` —— 删掉 (体重段 × 部位) 那一行(只删**本店**的)。
//	   ⚠️ 基线是派生结果 ⇒ 重算会算回来 ⇒ 只适合"临时停用一层"；
//	     真正"剔除某层数据"要去**样本库**删样本。
//	② `dayDataGaps` —— 那一天的数据**完整性校验**(加入样本前用)。
//
// # ⚠️ 前置校验:那一天的数据必须**完整**
//
//	缺哪一项就 400 并**点名缺哪一项** —— 门店才知道要补什么。
//	四项:① 录入(进猪) ② 搬台(含 0 kg 的"今天不搬台"标记) ③ 销售(零售或统收)
//	     ④ 盘点(**完整**盘点单 is_complete = true)
//	第 ⑤ 项(分割样本)在加入样本时判 —— 见 forecast_samples.go::CollectForecastSamples。
//
// # ⚠️ 销售那一项必须跟**毛利看板**走同一条读路径
//
//	有源门店的销售在**它自己的 cube 源**里 ⇒ 只查本地表 ⇒ 明明卖了一整天却报「缺:销售」
//	⇒ 交给 [Service.hasSalesOnDay](由 `salesReadPath` 判定 cube / local)。
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
)

// DeleteForecastBaseline 删掉一条基线(不准确/脏的那一行)。
//
// ⚠️ **只删本店那一行**:别家店的基线不该被本店删。
//
// ⚠️ 2026-10-30:基线是**派生结果** ⇒ 重算会把它算回来 ⇒ 这个动作只适合"临时停用一层",
//
//	**不删样本**的话重算即恢复。真正"剔除某层数据"要去**样本库**删样本(见 forecast_samples.go)。
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

// forecastSplitSampleGap 返回"分割样本"缺口的文案(空串 = 不缺)。
//
// # 判据:**两个池都空**才算缺
//
//	实录池(门店手工补录)与估计池(自动倒推摊出来的)任一有样本 ⇒ 都能拟合 ⇒ 不缺。
//	⚠️ 写成"任一为空"⇒ 有实录的那天会被误报成缺 ⇒ 门店白忙一场。
//
// # 为什么单独抽成函数
//
//	这条判定在端到端场景里**几乎构造不出来**(引擎很会把销售归到"无名桶"里,
//	倒推值仍为正 ⇒ 总是有样本)⇒ 端到端用例锁不住它,
//	而判据本身有个容易写错的地方 ⇒ 抽出来直接锁。
func forecastSplitSampleGap(recordedN, estimatedN int) string {
	if recordedN == 0 && estimatedN == 0 {
		return "分割样本(那天的分割重量算不出来 —— 可手工补录那天的分割)"
	}
	return ""
}

// countForecastSamples 数一个样本池里**样本条数**(不是分桶数)。
func countForecastSamples(m map[forecastBucketKey][]decimal.Decimal) int {
	n := 0
	for _, list := range m {
		n += len(list)
	}
	return n
}

// dayDataGaps 返回那一天**缺**的项目(空 = 数据完整)。
//
// ⚠️ 报给门店的文案要**点名缺哪一项** —— "数据不完整"门店没法行动。
//
// ⚠️ 这里只管**四项业务动作**(录入/搬台/销售/盘点);
//
//	第 ⑤ 项(分割样本)在 CollectForecastSamples 里判 ——
//	因为它要先**自动补齐**估计样本(分割本来就是自动倒推的),补完才知道到底有没有。
func (s *Service) dayDataGaps(ctx context.Context, branchID string, day time.Time) ([]string, error) {
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
//
// # ⚠️ 这里**包含墓碑行**(已删除的样本)
//
//	这个判据回答的是「那天的分割重量**算不出来**吗」
//	⇒ 墓碑行的重量**确实算过、也确实存着**(只是门店把它从样本库里剔除了)
//	⇒ 算得出来 ⇒ 不该报「缺:分割样本」。
//
//	⚠️ 反过来(把墓碑行排除掉)会踩一个**错引导**：
//	门店刚删掉那天的样本 ⇒ 再点「加入样本」⇒ 报「分割重量算不出来」
//	⇒ 门店会以为要去手工补录分割 ⇒ 而真相是"你自己删的呀"。
//
//	拟合路径另走 [Service.loadEstimatedForecastRows] ⇒ 那里**排除**墓碑行。
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
