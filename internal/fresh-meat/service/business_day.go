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
// ⚠️ **例外**:该店**还没有任何营业日历史**(没进过猪、没搬过台、没盘过点)
//   ⇒ 目标日就是它的**第一个营业日** ⇒ 允许(没有"昨天"要补)。
//
// ⚠️ **为什么用"最近一次已盘点"而不是"精确查昨天"**:
//   补录往日时(目标日 < 最近一次盘点日)必须放行 —— 那是门店在**补时间线**。
func (s *Service) assertPrevBizDayStocktaken(ctx context.Context, branchID string, targetDate time.Time) error {
	target := targetDate.In(s.bizTZOrUTC())
	prevDay := target.AddDate(0, 0, -1)
	prevKey := prevDay.Format("2006-01-02")

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
