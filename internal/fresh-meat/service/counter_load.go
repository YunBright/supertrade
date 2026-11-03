package service

// internal/fresh-meat/service/counter_load.go —— **搬台流水**。
//
// # 为什么要有它(2026-10-24 门店口径)
//
// 「位置表只记余额、不留痕」⇒ 「当天搬上台了多少」查不到
// ⇒ 而三件事都要它:
//   ① **休市判定**:「当天既没有卖猪、也没有搬冷库商品上猪肉台」⇒ 鲜肉休市。
//      (历史:建这张表时只有"按批次整批搬"留痕,"按 SKU+kg 搬"完全不留痕
//       ⇒ 搬了台的休市日会被误判成休市。旧玩法已于 2026-11-02 下线。)
//   ② **按营业日推导的冷库余额**:「冷库余额 = 上期冷库 + 本期结转 − 本期搬上台
//      − 本期大宗结算出库」⇒ 本期搬上台必须有流水。
//   ③ **每日毛利的分摊重量**:搬上台的是**老货**,不能再摊一次猪成本。
//
// # 只有一种搬法(2026-11-02 owner:旧玩法下线)
//
//	source = sku  按 **SKU + kg** 搬。
//
//	(历史行里还可能有 source = bag ——「按批次整批搬」,那套玩法已下线。)
//
// # 归属日
//
// 搬台流水有**归属日**(biz_date,与盘点一致)⇒ 补录往期的搬台记在**那一天**。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 搬台流水的来源。
const (
	// CounterLoadSourceSKU 按 SKU + kg 搬。
	//
	// ⚠️ 2026-11-02 owner:旧玩法下线 ⇒ 「按批次整批搬」(source=bag)不再产生
	//   ⇒ 新行恒为 sku;历史行仍可能是 bag(只读展示)。
	CounterLoadSourceSKU = "sku"
)

// counterLoadLine 搬台集合里的一行(解析后:SKU 与重量都已确定)。
type counterLoadLine struct {
	// id 非空 = 覆盖那一行;空 = 新增一行。
	id string
	// seq 是这一行在当天的序号(新增时接续)。
	seq int
	sku string
	kg  decimal.Decimal
	// prevKg 是**覆盖前**那一行的重量(算位置差额用)。
	prevKg decimal.Decimal
	// cut 是这一行的**部位**(库存/结余按部位记)。
	cut model.CutType
}

// upsertCounterLoadLine 写一行搬台流水(**带 id = 覆盖,不带 id = 新增**)。
//
// # 幂等
//
// - 带 id ⇒ 覆盖那一行(重复提交同一份不会多搬一次);
// - 不带 id ⇒ 新增一行,序号接续(门店"中午再加一批")。
//
// ⇒ 位置差额由调用方按 (新重量 − 旧重量) 算(见 service/stock.go::CounterLoad)。
func (s *Service) upsertCounterLoadLine(ctx context.Context, tx *gorm.DB, branchID string, bizDate time.Time,
	nl counterLoadLine, operatorID string) error {
	if tx == nil {
		tx = s.db
	}
	date := datatypesDate(bizDate.In(s.bizTZOrUTC()))
	seq := nl.seq
	rowID := nl.id
	if rowID == "" {
		// 新增:序号接续(当天已有的最大序号 + 1)。
		start, end := s.businessDayBounds(bizDate)
		var maxSeq *int
		if err := tx.WithContext(ctx).Model(&model.CounterLoadRecord{}).
			Where("branch_id = ? AND biz_date >= ? AND biz_date < ?", branchID, start, end).
			Select("MAX(seq)").Scan(&maxSeq).Error; err != nil {
			return fmt.Errorf("query counter_loads(max seq): %w", err)
		}
		if maxSeq != nil {
			seq = *maxSeq + 1
		} else {
			seq = 0
		}
		rowID = CounterLoadID(branchID, dateKey(date), seq)
	}
	source := CounterLoadSourceSKU
	rec := model.CounterLoadRecord{
		ID:            rowID,
		BranchID:      branchID,
		BizDate:       date,
		Seq:           seq,
		CubeProductID: nl.sku,
		CutType:       nl.cut,
		QtyKg:         nl.kg,
		Source:        source,
		OperatorID:    operatorID,
		CreatedAt:     s.now(),
		UpdatedAt:     s.now(),
	}
	if err := tx.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"cube_product_id": rec.CubeProductID,
			"cut_type":        rec.CutType,
			"qty_kg":          rec.QtyKg,
			"source":          rec.Source,
			"operator_id":     rec.OperatorID,
			"updated_at":      rec.UpdatedAt,
		}),
	}).Create(&rec).Error; err != nil {
		return fmt.Errorf("upsert counter_load %s: %w", rec.ID, err)
	}
	return nil
}

// UpdateCounterLoad 改一行的搬台重量(PUT /stock/counter-load/:id)。
//
// # 允许改**过往**某天的一行(「甚至过一天修改」)
//
// ⇒ 只影响那一天及其之后的余额链;位置表按差额调整。
//
// ⚠️ **改出来负库存不拦**(门店口径)⇒ 在每日毛利的告警里红字显示。
func (s *Service) UpdateCounterLoad(ctx context.Context, branchID, id string, in CounterLoadUpdateInput) (*StockSnapshot, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("%w: id 必填", ErrInvalidInput)
	}
	if !in.QtyKg.IsPositive() {
		return nil, fmt.Errorf("%w: qty_kg 必须 > 0", ErrInvalidInput)
	}
	var cur model.CounterLoadRecord
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND id = ?", branchID, id).
		First(&cur).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("%w: 搬台流水 %s 不存在", ErrInvalidInput, id)
	}
	if err != nil {
		return nil, fmt.Errorf("query counter_load %s: %w", id, err)
	}
	delta := in.QtyKg.Sub(cur.QtyKg)
	if delta.IsZero() {
		return s.ListStock(ctx, branchID, model.StockScopeCounter)
	}
	bizDate := time.Time(cur.BizDate).In(s.bizTZOrUTC())
	// ⚠️ 2026-11-01 owner：审计结转下线 ⇒ 冻结机制一并下线
	//   ⇒ 改搬台不再被「截止日已冻结」拦截。
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.WithContext(ctx).Model(&model.CounterLoadRecord{}).
			Where("branch_id = ? AND id = ?", branchID, id).
			Updates(map[string]any{"qty_kg": in.QtyKg, "updated_at": s.now()}).Error; err != nil {
			return fmt.Errorf("update counter_load %s: %w", id, err)
		}
		if err := s.addPosition(ctx, tx, branchID, cur.CubeProductID, model.StockLocationColdStore, delta.Neg()); err != nil {
			return err
		}
		if err := s.addPosition(ctx, tx, branchID, cur.CubeProductID, model.StockLocationCounter, delta); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// 余额链变了 ⇒ 重算那天的结余快照(过一天修改 ⇒ 后面的日期跟着变)。
	if err := s.recomputeCostSnapshots(ctx, branchID, bizDate); err != nil {
		s.pubLogger.Error("UpdateCounterLoad: 结余快照重算失败",
			"branch_id", branchID, "id", id, "err", err)
	}
	return s.ListStock(ctx, branchID, model.StockScopeCounter)
}

// DeleteCounterLoad 撤掉一行搬台(DELETE /stock/counter-load/:id)。
//
// ⇒ 货"搬回冷库"(冷库 +、柜台 −);允许撤**过往**某天的一行。
func (s *Service) DeleteCounterLoad(ctx context.Context, branchID, id string) (*StockSnapshot, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("%w: id 必填", ErrInvalidInput)
	}
	var cur model.CounterLoadRecord
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND id = ?", branchID, id).
		First(&cur).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("%w: 搬台流水 %s 不存在", ErrInvalidInput, id)
	}
	if err != nil {
		return nil, fmt.Errorf("query counter_load %s: %w", id, err)
	}
	bizDate := time.Time(cur.BizDate).In(s.bizTZOrUTC())
	// ⚠️ 2026-11-01 owner：审计结转下线 ⇒ 冻结机制一并下线
	//   ⇒ 撤搬台不再被「截止日已冻结」拦截。
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.WithContext(ctx).Where("branch_id = ? AND id = ?", branchID, id).
			Delete(&model.CounterLoadRecord{}).Error; err != nil {
			return fmt.Errorf("delete counter_load %s: %w", id, err)
		}
		// 撤掉 ⇒ 货搬回冷库(冷库 +、柜台 −)。
		if err := s.addPosition(ctx, tx, branchID, cur.CubeProductID, model.StockLocationColdStore, cur.QtyKg); err != nil {
			return err
		}
		if err := s.addPosition(ctx, tx, branchID, cur.CubeProductID, model.StockLocationCounter, cur.QtyKg.Neg()); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := s.recomputeCostSnapshots(ctx, branchID, bizDate); err != nil {
		s.pubLogger.Error("DeleteCounterLoad: 结余快照重算失败",
			"branch_id", branchID, "id", id, "err", err)
	}
	return s.ListStock(ctx, branchID, model.StockScopeCounter)
}

// counterLoadKgByDay 某营业日搬上台的总重量(按 SKU)。
//
// ⇒ 「按营业日推导的冷库余额」要用它。
//
// ⚠️ 日期用**区间**而不是等值:gorm.io/datatypes 的 Date 落库是"带时区的零点",
//
//	PG 里 `date = timestamp` 会按会话时区折算 ⇒ 等值不可靠;区间两边各留一天
//	的余量,两个时区下都只命中那一天。
func (s *Service) counterLoadKgByDay(ctx context.Context, tx *gorm.DB, branchID string, bizDate time.Time) (
	map[string]decimal.Decimal, error) {
	if tx == nil {
		tx = s.db
	}
	start, end := s.businessDayBounds(bizDate)
	type row struct {
		CubeProductID string
		QtyKg         decimal.Decimal
	}
	var rows []row
	if err := tx.WithContext(ctx).Model(&model.CounterLoadRecord{}).
		Select("cube_product_id AS cube_product_id, qty_kg AS qty_kg").
		Where("branch_id = ? AND biz_date >= ? AND biz_date < ?", branchID, start, end).
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("query counter_loads: %w", err)
	}
	out := map[string]decimal.Decimal{}
	for _, r := range rows {
		out[r.CubeProductID] = out[r.CubeProductID].Add(r.QtyKg)
	}
	return out, nil
}

// counterLoadTotalKg 某营业日搬上台的总重量(不分 SKU)。
func (s *Service) counterLoadTotalKg(ctx context.Context, tx *gorm.DB, branchID string, bizDate time.Time) (
	decimal.Decimal, error) {
	bySku, err := s.counterLoadKgByDay(ctx, tx, branchID, bizDate)
	if err != nil {
		return decimal.Zero, err
	}
	total := decimal.Zero
	for _, v := range bySku {
		total = total.Add(v)
	}
	return total, nil
}

// CounterLoadLineView 搬台流水的一行(对外 JSON)。
//
// ⚠️ biz_date 用 **"YYYY-MM-DD" 文本**(不是 datatypes.Date 的完整时间戳)
// —— 与仓管/前端约定一致(见 model.BulkSettlement.SettleDate 的注释)。
type CounterLoadLineView struct {
	ID            string          `json:"id"`
	BranchID      string          `json:"branch_id"`
	BizDate       string          `json:"biz_date"`
	Seq           int             `json:"seq"`
	CubeProductID string          `json:"cube_product_id"`
	QtyKg         decimal.Decimal `json:"qty_kg"`
	// Source:sku(按 SKU+kg 搬)。历史行里还可能有 bag(按批次整批搬,已下线)。
	Source     string    `json:"source"`
	OperatorID string    `json:"operator_id"`
	CreatedAt  time.Time `json:"created_at"`
	// CutType 是这一行的**部位**(2026-10-29 加)。
	//
	// ⚠️ 搬台流水表 `counter_loads` **本来就有** `cut_type` 列(落库时就记了)
	//   ⇒ 直接取,不用再查部位映射表。
	//
	// ⚠️ 用 `model.CutType`(底层就是 string)⇒ wire 上仍是 JSON 字符串,
	//   与 `service.StockLine.CutType` 同一口径(前端两边都能拿到部位名)。
	CutType model.CutType `json:"cut_type"`
	// CubeProductName 是**商品名**(2026-10-29 加)。
	//
	// ⚠️ 搬台流水表**没有**名字 ⇒ 用 `loadSkuCatalog` 的 `nameOf` 反查
	//   (部位↔货号映射表里有 `cube_product_name`)。
	//
	// ⚠️ 查不到(该店没配这个货号的映射)⇒ 留空 ⇒ `omitempty` ⇒ wire 上
	//   **直接不出这个 key**(前端显示货号即可,别让整个列表挂掉)。
	CubeProductName string `json:"cube_product_name,omitempty"`
}

// CounterLoadView 某营业日的搬台流水(对外 JSON)。
type CounterLoadView struct {
	BranchID string `json:"branch_id"`
	// BizDate 是归属日("YYYY-MM-DD")。
	BizDate string `json:"biz_date"`
	// TotalKg 是当天搬上台的总重量。
	TotalKg decimal.Decimal `json:"total_kg"`
	// Lines 是流水明细(按提交顺序)。
	Lines []CounterLoadLineView `json:"lines"`
	// Status 是当天营业日状态(前端第②步用:已搬台 / 未进猪 …)。
	Status     string `json:"status"`
	StatusText string `json:"status_text"`
	// HasCounterLoad 是"当天有没有搬过台"。
	HasCounterLoad bool `json:"has_counter_load"`
	// CounterLoadSkipped 是"门店**已确认今天不搬台**"(2026-10-30 owner)。
	//
	// ⚠️ 门店口径：「有时候整日都不需要搬台」⇒ 得有个办法把这一步标成"已完成"，
	//   否则每日作业页第②步永远显示"未搬台"。
	// ⇒ 落一条 qty_kg = 0 的搬台流水当标记 ⇒ **数值上等于没搬**。
	CounterLoadSkipped bool `json:"counter_load_skipped"`
	// RestDay 是"当天是不是鲜肉休市"(既没进猪也没搬台)。
	RestDay bool `json:"rest_day"`
}

// ListCounterLoads 列某店某营业日的搬台流水(读端点)。
//
// day 为空 ⇒ 今天(营业日界时区)。
func (s *Service) ListCounterLoads(ctx context.Context, branchID string, day time.Time) (*CounterLoadView, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	local := day.In(s.bizTZOrUTC())
	start, end := s.businessDayBounds(local)
	key := local.Format("2006-01-02")

	var rows []model.CounterLoadRecord
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date >= ? AND biz_date < ?", branchID, start, end).
		Order("seq ASC").
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query counter_loads: %w", err)
	}
	// 商品名反查(2026-10-29):搬台流水表没有名字 ⇒ 从部位↔货号映射表反查。
	//
	// ⚠️ 名字只是"锦上添花"(前端拿它显示"货号(商品名)")⇒ 查不到**降级**,
	//   不要让整个搬台列表 500。
	_, nameOf, err := s.loadSkuCatalog(ctx, branchID, nil)
	if err != nil {
		s.pubLogger.Warn("ListCounterLoads: 读 SKU 名称失败(降级:列表不显示商品名)",
			"branch_id", branchID, "err", err)
		nameOf = map[string]string{}
	}
	total := decimal.Zero
	lines := make([]CounterLoadLineView, 0, len(rows))
	skipped := false
	for _, r := range rows {
		total = total.Add(r.QtyKg)
		// ⚠️ qty_kg = 0 的行 = 「门店确认今天不搬台」的标记(2026-10-30 owner)。
		if r.QtyKg.IsZero() {
			skipped = true
		}
		lines = append(lines, CounterLoadLineView{
			ID:            r.ID,
			BranchID:      r.BranchID,
			BizDate:       dateKey(r.BizDate),
			Seq:           r.Seq,
			CubeProductID: r.CubeProductID,
			QtyKg:         r.QtyKg,
			Source:        r.Source,
			OperatorID:    r.OperatorID,
			CreatedAt:     r.CreatedAt,
			CutType:       r.CutType,
			// ⚠️ 查不到就留空(`omitempty` ⇒ wire 上不出这个 key)。
			CubeProductName: nameOf[r.CubeProductID],
		})
	}

	view := &CounterLoadView{
		BranchID: branchID,
		BizDate:  key,
		TotalKg:  total,
		Lines:    lines,
	}
	// 营业日状态(顺带把"已搬台"与"休市"给前端)。
	bd, err := s.GetBusinessDay(ctx, branchID, local)
	if err != nil {
		return nil, err
	}
	view.Status = bd.Status
	view.StatusText = bd.StatusText
	view.HasCounterLoad = bd.HasCounterLoad
	// ⚠️ 「已确认今天不搬台」= 有一条 0 kg 的标记行、且当天**确实没搬过货**。
	//   (又搬了货又有标记行 ⇒ 以"搬过"为准。)
	view.CounterLoadSkipped = skipped && !total.IsPositive()
	view.RestDay = bd.RestDay
	return view, nil
}
