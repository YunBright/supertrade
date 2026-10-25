package service

// internal/fresh-meat/service/business_day.go —— **营业日状态机 + 跨日顺序校验**。
//
// # 门店口径(2026-10-24,别改回去)
//
// 「整个鲜肉板块都是围绕如何计算**每日真实毛利**展开的。倘若不知道前天不做日终盘点,
//   那就没办法从冷库搬上台继续售卖;倘若没录入今天的猪,那也无法售卖。
//   补录是为了避免错过日期无法正确填补空缺而导致**整条时间线错乱,毛利就错完了**。」
// 「**不补录昨天,就无法录今天。**」
//
// ⇒ 每日循环:`录入猪 + 搬上台 → 日终盘点 → 次日…`(中间穿插大宗结算)。
// ⇒ **每个营业日的两个动作 + 一次盘点都必须齐全。**
//
// # 三件事
//
// ① **状态机**:营业日表(business_days)记录每个营业日走到哪一步:
//	未进猪(no_pig) → 已进猪(pig_in) → 已搬台(counter_loaded)
//	→ 已盘点(stocktaken;带 售罄 sold_out / 休市 rest_day 标志)
//	⇒ 状态**由实际动作推进**(录猪 / 搬台 / 盘点时 upsert),前端**不传状态**。
//
// ② **顺序校验**:录猪 / 搬上台之前 ⇒ 前一个营业日必须已盘点(is_complete=true,
//	含补录)⇒ 否则 400 并说清要补哪天。
//
// ③ **补录规则**:允许补**任意过去的营业日**;但归属日必须 ≤ 今天;
//	同一归属日已有盘点单 ⇒ **覆盖**(同一张单,不是新增)。
//
// # 为什么校验放在 service 层
//
// handler 只做 JSON 解码与错误码映射;业务顺序是**业务规则** ⇒ 放 service。
// (前端可以绕过前端校验直接打接口 ⇒ 顺序校验必须在服务端。)

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 营业日状态阶梯(取"走到最远的那一步",单调不回退)。
const (
	// BizDayStatusNoPig 未进猪。
	BizDayStatusNoPig = "no_pig"
	// BizDayStatusPigIn 已进猪。
	BizDayStatusPigIn = "pig_in"
	// BizDayStatusCounterLoaded 已搬台。
	BizDayStatusCounterLoaded = "counter_loaded"
	// BizDayStatusStocktaken 已盘点(售罄 | 休市)。
	BizDayStatusStocktaken = "stocktaken"
)

// bizDayRank 状态阶梯的序号(越大 = 走得越远)。
func bizDayRank(status string) int {
	switch status {
	case BizDayStatusPigIn:
		return 1
	case BizDayStatusCounterLoaded:
		return 2
	case BizDayStatusStocktaken:
		return 3
	default:
		return 0
	}
}

// BusinessDayID 营业日表(business_days)的**确定性**代理主键。
//
// ⇒ 同一 (门店, 营业日) 永远只有一行 ⇒ 重复提交 = 覆盖。
func BusinessDayID(branchID, bizDate string) string {
	return surrogateID("BD", branchID, bizDate)
}

// CounterLoadID 搬台流水(counter_loads)的**确定性**代理主键。
//
// seq 是这次提交里的行序号(0 起)⇒ 同一归属日重复提交**同一份** = 覆盖同一批行。
func CounterLoadID(branchID, bizDate string, seq int) string {
	return surrogateID("CLD", branchID, bizDate, fmt.Sprintf("%03d", seq))
}

// SkuStockDailyID 按营业日的结余快照(cut_stock_daily)的**确定性**代理主键(按部位)。
func CutStockDailyID(branchID, bizDate, cutType string) string {
	return surrogateID("CSD", branchID, bizDate, cutType)
}

// ---- 状态推进 ----

// mergeBusinessDay 读出当前行 → 应用 mutate → 按"状态只进不退"写回。
//
// ⚠️ **幂等**:同一 (门店, 营业日) 只有一行 ⇒ 重复提交 = 覆盖。
// ⚠️ **失败不回滚业务动作**:状态是**派生状态**(事实是动作本身),
// 下一轮动作会自愈(upsert 覆盖)。
func (s *Service) mergeBusinessDay(ctx context.Context, tx *gorm.DB, branchID string, bizDate time.Time,
	mutate func(*model.BusinessDay)) error {
	if tx == nil {
		tx = s.db
	}
	day := bizDate.In(s.bizTZOrUTC())
	key := day.Format("2006-01-02")
	start, end := s.businessDayBounds(day)
	var cur model.BusinessDay
	err := tx.WithContext(ctx).
		Where("branch_id = ? AND biz_date >= ? AND biz_date < ?", branchID, start, end).
		First(&cur).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return fmt.Errorf("query business_day %s: %w", key, err)
	}
	if err == gorm.ErrRecordNotFound {
		cur = model.BusinessDay{
			ID:        BusinessDayID(branchID, key),
			BranchID:  branchID,
			BizDate:   datatypesDate(day),
			Status:    BizDayStatusNoPig,
			CreatedAt: s.now(),
		}
	}
	oldStatus := cur.Status
	if mutate != nil {
		mutate(&cur)
	}
	// 状态只进不退:已经走到更远的阶段,别被后来的动作回退。
	if bizDayRank(cur.Status) < bizDayRank(oldStatus) {
		cur.Status = oldStatus
	}
	cur.UpdatedAt = s.now()
	if err := tx.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"status":           cur.Status,
			"has_pig":          cur.HasPig,
			"has_counter_load": cur.HasCounterLoad,
			"has_stocktake":    cur.HasStocktake,
			"stocktake_id":     cur.StocktakeID,
			"sold_out":         cur.SoldOut,
			"rest_day":         cur.RestDay,
			"pig_count":        cur.PigCount,
			"counter_load_kg":  cur.CounterLoadKg,
			"updated_at":       cur.UpdatedAt,
		}),
	}).Create(&cur).Error; err != nil {
		return fmt.Errorf("upsert business_day %s: %w", key, err)
	}
	return nil
}

// markBusinessDayPigIn 录猪后推进状态(已进猪)。
func (s *Service) markBusinessDayPigIn(ctx context.Context, tx *gorm.DB, branchID string, bizDate time.Time) error {
	return s.mergeBusinessDay(ctx, tx, branchID, bizDate, func(d *model.BusinessDay) {
		d.HasPig = true
		d.PigCount++
		if bizDayRank(d.Status) < bizDayRank(BizDayStatusPigIn) {
			d.Status = BizDayStatusPigIn
		}
	})
}

// markBusinessDayCounterLoaded 搬上台后推进状态(已搬台)。
//
// ⚠️ CounterLoadKg 是**按流水重算**的(不是累加)⇒ 重复提交不会翻倍。
func (s *Service) markBusinessDayCounterLoaded(ctx context.Context, tx *gorm.DB, branchID string, bizDate time.Time) error {
	total, err := s.counterLoadTotalKg(ctx, tx, branchID, bizDate)
	if err != nil {
		return err
	}
	return s.mergeBusinessDay(ctx, tx, branchID, bizDate, func(d *model.BusinessDay) {
		d.HasCounterLoad = true
		d.CounterLoadKg = total
		if bizDayRank(d.Status) < bizDayRank(BizDayStatusCounterLoaded) {
			d.Status = BizDayStatusCounterLoaded
		}
	})
}

// markBusinessDayStocktaken 盘点后推进状态(已盘点 + 售罄/休市标志)。
func (s *Service) markBusinessDayStocktaken(ctx context.Context, tx *gorm.DB, branchID string, bizDate time.Time,
	stocktakeID string, soldOut, restDay bool) error {
	return s.mergeBusinessDay(ctx, tx, branchID, bizDate, func(d *model.BusinessDay) {
		d.HasStocktake = true
		d.StocktakeID = stocktakeID
		d.SoldOut = soldOut
		d.RestDay = restDay
		d.Status = BizDayStatusStocktaken
	})
}

// ---- 跨日顺序校验 ----

// assertPrevBizDayStocktaken 校验"前一个营业日必须已盘点"。
//
// # 规则(2026-10-24 门店口径)
//
// 录猪 / 搬上台之前 ⇒ **前一个营业日必须已盘点**(is_complete=true 的盘点单,
// 含补录)⇒ 否则 400,并说清要补哪天。
//
// # 实现
//
// 取该店**最近一次已盘点**的日期(lastComplete):
//   - lastComplete ≥ 目标归属日 − 1 天 ⇒ 通过(昨天盘过了,或目标日是补录);
//   - lastComplete < 目标归属日 − 1 天 ⇒ **中间缺了** ⇒ 拒绝,
//     并指出**要补的那一天**(目标归属日的前一天)。
//
// ⚠️ **例外①:该店还没有任何营业日历史**(没进过猪、没搬过台、没盘过点)
//   ⇒ 目标日就是它的**第一个营业日** ⇒ 允许(没有"昨天"要补)。
//
// ⚠️ **例外②:前一个营业日正好是**期初结转的生效日(2026-10-25 门店反馈)
//
//	期初结转那一行的冷库余额 = **链的起点** = 那天的基线
//	⇒ 那天**不需要盘点单**。
//
//	门店的真实场景：**上线前冷库里的货**用期初结转记一笔(生效日 10-01),
//	**10-02 才开始第一期营业** ⇒ 10-01 根本没营业、哪来的盘点单?
//	⇒ 之前这里直接报"10-01 未盘点"⇒ 门店连第一期猪都录不进去。
//
//	⚠️ **只豁免"纯基线日"**(见 isBaselineDay)：期初生效日**且那天没有任何营业日动作**。
//	  若门店**在那天录了猪 / 搬了台** ⇒ 那天就是**营业日**
//	  ⇒ 那天的日终 = 期初 + 当天的进出 ⇒ **必须盘点**(链要逐日推)
//	  ⇒ 次日才放行
//	  ⇒ 否则那天没有日终快照 ⇒ 期初那批货的成本在次日**消失** ⇒ 毛利虚高。
//
// ⚠️ **为什么用"最近一次已盘点"而不是"精确查昨天"**:
//   补录往日时(目标日 < 最近一次盘点日)必须放行 —— 那是门店在**补时间线**。
func (s *Service) assertPrevBizDayStocktaken(ctx context.Context, branchID string, targetDate time.Time) error {
	target := targetDate.In(s.bizTZOrUTC())
	prevDay := target.AddDate(0, 0, -1)
	prevKey := prevDay.Format("2006-01-02")

	// ★ **纯基线日**（期初生效日 + 那天什么都没做）⇒ 不需要盘点单（见上面的例外②）。
	baseline, err := s.isBaselineDay(ctx, branchID, prevKey)
	if err != nil {
		return err
	}
	if baseline {
		return nil
	}

	lastComplete, err := s.lastCompleteStocktakeDate(ctx, branchID)
	if err != nil {
		return err
	}
	if lastComplete != "" {
		if lastComplete >= prevKey {
			return nil
		}
		return fmt.Errorf("%w: %s 未盘点，请先补录 %s 的盘点（不补录昨天，就无法录今天）",
			ErrInvalidInput, prevKey, prevKey)
	}
	// 一次都没盘过 ⇒ 只有"第一个营业日"才放行。
	hasHistory, err := s.hasBizDayHistory(ctx, branchID, target)
	if err != nil {
		return err
	}
	if !hasHistory {
		return nil
	}
	return fmt.Errorf("%w: %s 未盘点，请先补录 %s 的盘点（不补录昨天，就无法录今天）",
		ErrInvalidInput, prevKey, prevKey)
}

// chainStartDate 该店**链的起点**（期初结转里最早的那个生效日）；空 = 没有期初。
//
// ⚠️ 链的起点 = 冷库余额链的**第一天**（见 deriveColdChain：在生效日那天，
//   余额被重置为期初的 kg）⇒ 更早的日子**链还没开始**
//   ⇒ 那几天的余额/毛利算不出来（账算在链外 ⇒ 毛利对不上）。
//
// ⚠️ 没有期初 ⇒ 空串 ⇒ **不限制**（链从门店的第一期营业日起算）。
func (s *Service) chainStartDate(ctx context.Context, branchID string) (string, error) {
	var rows []model.SkuStockOpening
	if err := s.db.WithContext(ctx).Model(&model.SkuStockOpening{}).
		Where("branch_id = ?", branchID).
		Find(&rows).Error; err != nil {
		return "", fmt.Errorf("query sku_stock_openings(chain start): %w", err)
	}
	best := ""
	for _, r := range rows {
		key := time.Time(r.BizDate).In(s.bizTZOrUTC()).Format("2006-01-02")
		if best == "" || key < best {
			best = key
		}
	}
	return best, nil
}

// assertOnOrAfterChainStart 校验归属日**不早于链的起点**。
//
// # 为什么
//
// 链的起点（期初结转的生效日）之前**链还没开始** ⇒ 那几天的冷库余额与毛利
// **算不出来** ⇒ 在那里录猪/搬台 = 录了链外的孤儿数据（报表对不上，门店更看不懂）。
//
// ⚠️ **起点当天是可以营业的**：期初的余额就是**那天的期初**（基线），
//   当天的产出/搬台照样往上累 ⇒ 与链完全自洽。
//   （"第一天不营业"是门店的**用法**，不是系统的**硬规则**——
//   想从次日开始，把期初生效日设成前一天即可。）
//
// ⚠️ 没有期初 ⇒ 不限制（链从门店的第一期营业日起算）。
func (s *Service) assertOnOrAfterChainStart(ctx context.Context, branchID string, day time.Time, what string) error {
	start, err := s.chainStartDate(ctx, branchID)
	if err != nil {
		return err
	}
	if start == "" {
		return nil
	}
	key := day.In(s.bizTZOrUTC()).Format("2006-01-02")
	if key < start {
		return fmt.Errorf("%w: 不能录%s：%s 在期初结转的生效日(%s)之前 —— 冷库余额链从 %s 才开始，那天的账算不出来",
			ErrInvalidInput, what, key, start, start)
	}
	return nil
}

// isBaselineDay 判断某天是不是一个**纯基线日**。
//
// # 纯基线日 = 期初结转的生效日 **且** 那天**没有任何营业日动作**
//
//	（没录猪、没搬台、没盘点）⇒ 那天的"日终"就等于**期初本身**
//	⇒ 次日的期初 = 期初的 kg / 金额 ⇒ **链从那里开始** ⇒ 不需要盘点单。
//
// # 为什么必须区分"纯基线日"和"期初生效日但那天有动作"
//
//	门店在 10-01 录了猪 ⇒ 10-01 就是**营业日**
//	⇒ 那天的日终 = 期初 + 当天的进出 ⇒ **必须盘点**（链要逐日推）
//	⇒ 否则那天没有日终快照 ⇒ 期初那批货的成本在次日**消失** ⇒ **毛利虚高**。
//
// ⚠️ 只认**最早**那个生效日（链的起点）⇒ 起点之前的日子链还不存在。
func (s *Service) isBaselineDay(ctx context.Context, branchID, dayKey string) (bool, error) {
	start, err := s.chainStartDate(ctx, branchID)
	if err != nil {
		return false, err
	}
	if start == "" || dayKey != start {
		return false, nil
	}
	hasActions, err := s.hasBizDayActions(ctx, branchID, dayKey)
	if err != nil {
		return false, err
	}
	return !hasActions, nil
}

// hasBizDayActions 该店在 dayKey 那天有没有**营业日动作**（录猪 / 搬台 / 盘点）。
//
// ⚠️ 用来判断"那天是不是纯基线日"：期初生效日 + **没有任何动作**
//   ⇒ 那天只是把链的起点摆好 ⇒ 之后的第一期不用补它的盘点单。
func (s *Service) hasBizDayActions(ctx context.Context, branchID, dayKey string) (bool, error) {
	start, end := s.businessDayBounds(mustParseDateInLoc(dayKey, s.bizTZOrUTC()))
	var n int64
	// ① 录猪（按到店时间落在那天）。
	if err := s.db.WithContext(ctx).Model(&model.WholePig{}).
		Where("branch_id = ? AND arrived_at >= ? AND arrived_at < ?", branchID, start, end).
		Count(&n).Error; err != nil {
		return false, fmt.Errorf("count whole_pigs(actions): %w", err)
	}
	if n > 0 {
		return true, nil
	}
	// ② 搬台（按归属营业日）。
	if err := s.db.WithContext(ctx).Model(&model.CounterLoadRecord{}).
		Where("branch_id = ? AND biz_date = ?", branchID, datatypesDate(start)).
		Count(&n).Error; err != nil {
		return false, fmt.Errorf("count counter_loads(actions): %w", err)
	}
	if n > 0 {
		return true, nil
	}
	// ③ 盘点（按盘点时刻落在那天）。
	if err := s.db.WithContext(ctx).Model(&model.PorkCutsStocktake{}).
		Where("branch_id = ? AND taken_at >= ? AND taken_at < ?", branchID, start, end).
		Count(&n).Error; err != nil {
		return false, fmt.Errorf("count stocktakes(actions): %w", err)
	}
	return n > 0, nil
}

// hasStockOpeningOn 该店在 dateKey 那天有没有**期初结转**。
//
// ⚠️ 期初结转的生效日 = **链的起点**（那一行的冷库余额就是那天的基线）
//   ⇒ 那天**不需要盘点单**（门店可能 10-01 只录期初、10-02 才开始营业）。
//
// ⚠️ 查询的日期必须用**营业日界时区**解析（datatypes.Date 落库保留时区表示,
//   写读必须逐字一致,否则等值查询查不到 —— 见 mustParseDateInLoc 的注释）。
func (s *Service) hasStockOpeningOn(ctx context.Context, branchID, dateKey string) (bool, error) {
	var n int64
	if err := s.db.WithContext(ctx).Model(&model.SkuStockOpening{}).
		Where("branch_id = ? AND biz_date = ?", branchID,
			datatypesDate(mustParseDateInLoc(dateKey, s.bizTZOrUTC()))).
		Count(&n).Error; err != nil {
		return false, fmt.Errorf("count sku_stock_openings: %w", err)
	}
	return n > 0, nil
}

// lastCompleteStocktakeDate 该店**最近一次已盘点**(is_complete=true)的归属日。
//
// ⚠️ 归属日由 taken_at 按营业日界时区折算(盘点单没有独立的 biz_date 列)。
func (s *Service) lastCompleteStocktakeDate(ctx context.Context, branchID string) (string, error) {
	var st model.PorkCutsStocktake
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND is_complete = ?", branchID, true).
		Order("taken_at DESC").
		First(&st).Error
	if err == gorm.ErrRecordNotFound {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("query last complete stocktake: %w", err)
	}
	return st.TakenAt.In(s.bizTZOrUTC()).Format("2006-01-02"), nil
}

// hasBizDayHistory 该店在 targetDate(不含)之前有没有过任何营业日痕迹。
func (s *Service) hasBizDayHistory(ctx context.Context, branchID string, targetDate time.Time) (bool, error) {
	dayStart, _ := s.businessDayBounds(targetDate)
	var n int64
	if err := s.db.WithContext(ctx).Model(&model.WholePig{}).
		Where("branch_id = ? AND arrived_at < ?", branchID, dayStart).
		Count(&n).Error; err != nil {
		return false, fmt.Errorf("count whole_pigs(history): %w", err)
	}
	if n > 0 {
		return true, nil
	}
	if err := s.db.WithContext(ctx).Model(&model.PorkCutsStocktake{}).
		Where("branch_id = ? AND taken_at < ?", branchID, dayStart).
		Count(&n).Error; err != nil {
		return false, fmt.Errorf("count stocktakes(history): %w", err)
	}
	if n > 0 {
		return true, nil
	}
	if err := s.db.WithContext(ctx).Model(&model.CounterLoadRecord{}).
		Where("branch_id = ? AND biz_date < ?", branchID, datatypesDate(dayStart)).
		Count(&n).Error; err != nil {
		return false, fmt.Errorf("count counter_loads(history): %w", err)
	}
	return n > 0, nil
}

// ---- 读端 ----

// BusinessDayView 营业日状态(对外 JSON)。
type BusinessDayView struct {
	BranchID string `json:"branch_id"`
	// BizDate 是归属日("YYYY-MM-DD")。
	BizDate string `json:"biz_date"`
	// Status:no_pig | pig_in | counter_loaded | stocktaken。
	Status string `json:"status"`
	// StatusText 是中文说明(给门店看)。
	StatusText string `json:"status_text"`
	HasPig          bool             `json:"has_pig"`
	HasCounterLoad  bool             `json:"has_counter_load"`
	HasStocktake    bool             `json:"has_stocktake"`
	SoldOut         bool             `json:"sold_out"`
	RestDay         bool             `json:"rest_day"`
	StocktakeID     string           `json:"stocktake_id,omitempty"`
	PigCount        int              `json:"pig_count"`
	CounterLoadKg   decimal.Decimal  `json:"counter_load_kg"`
	// PrevBizDate / PrevStocktaken 是"前一个营业日"的判定结果(前端可提示要补哪天)。
	PrevBizDate     string `json:"prev_biz_date,omitempty"`
	PrevStocktaken  bool   `json:"prev_stocktaken"`
	// CanRecordPig / CanCounterLoad 是"现在能不能录猪 / 搬台"(含顺序校验结果)。
	CanRecordPig   bool   `json:"can_record_pig"`
	CanCounterLoad bool   `json:"can_counter_load"`
	// BlockReason 是不能做时的原因(直接可展示给门店)。
	BlockReason string `json:"block_reason,omitempty"`
}

// GetBusinessDay 查某店某营业日的状态(读端点)。
//
// date 为空 ⇒ 今天(营业日界时区)。
func (s *Service) GetBusinessDay(ctx context.Context, branchID string, day time.Time) (*BusinessDayView, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	loc := s.bizTZOrUTC()
	local := day.In(loc)
	key := local.Format("2006-01-02")

	var rec model.BusinessDay
	start, end := s.businessDayBounds(local)
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date >= ? AND biz_date < ?", branchID, start, end).
		First(&rec).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return nil, fmt.Errorf("query business_day: %w", err)
	}
	if err == gorm.ErrRecordNotFound {
		rec = model.BusinessDay{
			BranchID: branchID,
			BizDate:  datatypesDate(local),
			Status:   BizDayStatusNoPig,
		}
	}

	prevDay := local.AddDate(0, 0, -1)
	prevKey := prevDay.Format("2006-01-02")
	lastComplete, err := s.lastCompleteStocktakeDate(ctx, branchID)
	if err != nil {
		return nil, err
	}
	prevStocktaken := lastComplete != "" && lastComplete >= prevKey

	view := &BusinessDayView{
		BranchID:       branchID,
		BizDate:        key,
		Status:         rec.Status,
		StatusText:     businessDayStatusText(rec.Status),
		HasPig:         rec.HasPig,
		HasCounterLoad: rec.HasCounterLoad,
		HasStocktake:   rec.HasStocktake,
		SoldOut:        rec.SoldOut,
		RestDay:        rec.RestDay,
		StocktakeID:    rec.StocktakeID,
		PigCount:       rec.PigCount,
		CounterLoadKg:  rec.CounterLoadKg,
		PrevBizDate:    prevKey,
		PrevStocktaken: prevStocktaken,
	}
	if err := s.assertPrevBizDayStocktaken(ctx, branchID, local); err != nil {
		view.CanRecordPig = false
		view.CanCounterLoad = false
		view.BlockReason = strings.TrimPrefix(err.Error(), "service: 输入校验失败: ")
		return view, nil
	}
	view.CanRecordPig = true
	view.CanCounterLoad = true
	return view, nil
}

// businessDayStatusText 状态的中文说明。
func businessDayStatusText(status string) string {
	switch status {
	case BizDayStatusPigIn:
		return "已进猪"
	case BizDayStatusCounterLoaded:
		return "已搬台"
	case BizDayStatusStocktaken:
		return "已盘点"
	default:
		return "未进猪"
	}
}
