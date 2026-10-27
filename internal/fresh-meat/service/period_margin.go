// Package service / period_margin.go —— 长期毛利(周/月/季/自定义期间)。
//
// # 为什么要有"长期"这一层
//
// **单日毛利会被库存波动扭曲**:
// 今天卖得多 ≠ 毛利高(可能卖的是**便宜的陈货**);
// 今天毛利低   ≠ 经营差(可能只是**好货囤着**没卖)。
//
// ⇒ 单日毛利用于**当期经营诊断**;长期毛利才是**真实盈利能力**。
// 两者并列呈现,不要拿切换成本法前后的单日数字直接比(见 REQUIREMENTS §4.7 T1)。
//
// # 数据源(2026-10-27:成本链改部位级)
//
//	**逐日**取部位级成本数据,两种来源按优先级:
//
//	① `cut_cost_daily`(**部位级**成本日快照)—— 它已经是移动加权口径
//	  (期初/入库/销售/期末/加权单价/销售成本都在),所以长期聚合就是**把日快照加起来**。
//	② **没有快照的那天 ⇒ 实时算**(见下面的"为什么必须有 ②")。
//
// ⚠️ 旧的 `sku_cost_daily`(SKU 级)**已停写** ⇒ 这里**不能再查它**
//   (查了就是空/过期数据,而这个接口是对外的)。
//
// # ⚠️ 为什么必须有 ②(2026-10-27 生产实测踩到的坑)
//
//	日快照**只在日终盘点提交后落库** —— 而真实门店(00/01)**从 9 月至今没有盘点单**
//	⇒ `cut_cost_daily` 是空的
//	⇒ 只读快照 ⇒ `group_by=day|cut` 返回**全 0**、`group_by=sku` 成本恒 0
//	⇒ **接口看起来"正常"但没有数据**(最坏的一种坏法:没人发现)。
//
//	⇒ 缺快照的那天**实时算**(与每日毛利同一个引擎)⇒ 口径一致、数字不为 0。
//	⇒ 有快照的天用快照(快)⇒ 门店开始做日终盘点后,这个接口会自己变快。
//
// # 口径
//
//	期间收入 = Σ(sold_amount + bulk_sold_amount)   ← 零售 + 酒店统收
//	期间销售成本 = Σ(sales_cost_yuan + bulk_cost_yuan + waste_cost_yuan)
//	              (销售 + 统收 + 报损 —— 三块成本都在损益里)
//	期间毛利 = 收入 − 销售成本 + 预售结转的预期毛利
//	          (与每日毛利同口径 ⇒ 长期毛利 = 每日毛利之和)
//
// ⚠️ **期初存货的成本**在售出时按移动加权结转 ⇒ 自然落在期间的 sales_cost 里
// ⇒ 长期毛利不需要额外处理"期初存货"这一项。
//
// # group_by=sku 的口径(近似,见 Note)
//
// 部位数据里**没有 SKU 粒度** ⇒ SKU 粒度是**算出来**的:
//
//	营收 / 销量  = 按 SKU(销售流水本来就是 SKU 级)
//	销售成本     = 该 SKU 当日销量 × 当日该部位的移动加权单价(按部位分摊)
//
// ⇒ 多货号部位里,各货号按**同一部位的单价**分摊成本。
package service

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
)

// PeriodMarginRow 长期毛利的一行(按 group_by 聚合)。
type PeriodMarginRow struct {
	// Key 是聚合键:day=日期 / cut=部位 / sku=货号。
	Key   string `json:"key"`
	Label string `json:"label,omitempty"`
	// CutType 只有 group_by=cut 时有值。
	CutType model.CutType `json:"cut_type,omitempty"`

	RevenueYuan     decimal.Decimal `json:"revenue_yuan"`
	SalesCostYuan   decimal.Decimal `json:"sales_cost_yuan"`
	GrossMarginYuan decimal.Decimal `json:"gross_margin_yuan"`
	GrossMarginPct  float64         `json:"gross_margin_pct"`
	SoldKg          decimal.Decimal `json:"sold_kg"`
	ProducedKg      decimal.Decimal `json:"produced_kg"`
	OpeningKg       decimal.Decimal `json:"opening_kg"`
	ClosingKg       decimal.Decimal `json:"closing_kg"`
}

// PeriodMarginReport 长期毛利报告。
type PeriodMarginReport struct {
	BranchID string            `json:"branch_id"`
	FromDate string            `json:"from_date"`
	ToDate   string            `json:"to_date"`
	GroupBy  string            `json:"group_by"`
	Rows     []PeriodMarginRow `json:"rows"`
	Total    TotalGrossMargin  `json:"total"`
	Days     int               `json:"days"`
	// Note 说明口径(尤其是"期初存货成本在售出时结转")。
	Note string `json:"note,omitempty"`
}

// periodCutRow 某营业日某部位的**期间毛利累加源**(快照或实时算,归一后的同一形状)。
type periodCutRow struct {
	revenue       decimal.Decimal
	salesCost     decimal.Decimal
	bulkCost      decimal.Decimal
	wasteCost     decimal.Decimal
	soldKg        decimal.Decimal
	producedKg    decimal.Decimal
	openingKg     decimal.Decimal
	closingKg     decimal.Decimal
	presaleMargin decimal.Decimal
	// unit 是当日该部位的移动加权单价 —— SKU 粒度按它分摊成本。
	unit decimal.Decimal
}

// periodDayData 某营业日的期间毛利源数据。
type periodDayData struct {
	// cuts 是**按部位**归一后的数据(快照或实时算,同一形状)。
	cuts map[string]periodCutRow
	// skus 只有**实时算**路径才有(引擎产出的 SKU 销售行)。
	// 快照路径没有 SKU 粒度 ⇒ 由调用方用 loadSkuFlows 补。
	skus []SkuDailyCost
	// fromSnapshot = true ⇒ 这一天用的是落库的日快照。
	fromSnapshot bool
}

// periodDayCuts 取某营业日的部位级数据:**快照优先,没有快照就实时算**。
//
// ⚠️ 实时算用的是**与每日毛利完全同一个引擎** ⇒ 长期毛利 = 每日毛利之和。
func (s *Service) periodDayCuts(ctx context.Context, branchID string, day time.Time,
	snaps []model.CutCostDaily) (periodDayData, error) {
	out := periodDayData{cuts: map[string]periodCutRow{}}
	if len(snaps) > 0 {
		out.fromSnapshot = true
		for _, r := range snaps {
			out.cuts[string(r.CutType)] = periodCutRow{
				revenue:    r.SoldAmount.Add(r.BulkSoldAmount),
				salesCost:  r.SalesCostYuan,
				bulkCost:   r.BulkCostYuan,
				wasteCost:  r.WasteCostYuan,
				soldKg:     r.SoldKg.Add(r.BulkSoldKg),
				producedKg: r.ProducedKg,
				openingKg:  r.OpeningKg,
				closingKg:  r.ClosingKg,
				// 快照里的毛利已含预售重估 ⇒ 这里只取重估额(用于单列)。
				presaleMargin: r.PresaleMarginYuan,
				unit:          r.MovingAvgUnitCost,
			}
		}
		return out, nil
	}
	// 没有快照 ⇒ 实时算(门店还没做日终盘点,快照不存在)。
	cutRows, skuRows, _, _, err := s.computeCutDailyCosts(ctx, branchID, day)
	if err != nil {
		return out, err
	}
	out.skus = skuRows
	for _, r := range cutRows {
		out.cuts[string(r.CutType)] = periodCutRow{
			revenue:       r.RevenueYuan,
			salesCost:     r.SalesCostYuan,
			bulkCost:      r.BulkCostYuan,
			wasteCost:     r.WasteCostYuan,
			soldKg:        r.SoldKg.Add(r.BulkSoldKg),
			producedKg:    r.ProducedKg,
			openingKg:     r.OpeningKg,
			closingKg:     r.ClosingKg,
			presaleMargin: r.PresaleMarginYuan,
			unit:          r.MovingAvgUnitCost,
		}
	}
	return out, nil
}

// ComputePeriodMargin 算某店某期间的毛利。
//
// groupBy:day(默认) | cut | sku
func (s *Service) ComputePeriodMargin(ctx context.Context, branchID, from, to, groupBy string) (*PeriodMarginReport, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	switch groupBy {
	case "":
		groupBy = "day"
	case "day", "cut", "sku":
	default:
		return nil, fmt.Errorf("%w: group_by 只支持 day | cut | sku", ErrInvalidInput)
	}
	loc := s.bizTZOrUTC()
	fromT, err := time.ParseInLocation("2006-01-02", from, loc)
	if err != nil {
		return nil, fmt.Errorf("%w: from 格式应为 YYYY-MM-DD", ErrInvalidInput)
	}
	toT, err := time.ParseInLocation("2006-01-02", to, loc)
	if err != nil {
		return nil, fmt.Errorf("%w: to 格式应为 YYYY-MM-DD", ErrInvalidInput)
	}
	if toT.Before(fromT) {
		return nil, fmt.Errorf("%w: to 不能早于 from", ErrInvalidInput)
	}
	fromDate := datatypesDate(fromT)
	toDate := datatypesDate(toT)

	// 区间内的部位级日快照(有快照的天用它;没有的那天实时算)。
	var snaps []model.CutCostDaily
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date >= ? AND biz_date <= ?", branchID, fromDate, toDate).
		Find(&snaps).Error; err != nil {
		return nil, fmt.Errorf("query cut_cost_daily: %w", err)
	}
	snapsByDay := map[string][]model.CutCostDaily{}
	for _, r := range snaps {
		d := dateKey(r.BizDate)
		snapsByDay[d] = append(snapsByDay[d], r)
	}

	agg := map[string]*PeriodMarginRow{}
	label := map[string]string{}
	cutOf := map[string]model.CutType{}
	// 逐日推进。
	days := 0
	realTimeDays := 0
	for d := fromT; !d.After(toT); d = d.AddDate(0, 0, 1) {
		days++
		key := d.Format("2006-01-02")
		data, err := s.periodDayCuts(ctx, branchID, d, snapsByDay[key])
		if err != nil {
			return nil, err
		}
		if !data.fromSnapshot {
			realTimeDays++
		}
		// 部位级数据 → 聚合键。
		for cut, c := range data.cuts {
			aggKey := key
			lbl := key
			switch groupBy {
			case "cut":
				aggKey = cut
				lbl = model.CutLabel(model.CutType(cut))
			case "sku":
				continue // SKU 粒度单独处理(见下)。
			}
			row, ok := agg[aggKey]
			if !ok {
				row = &PeriodMarginRow{Key: aggKey}
				agg[aggKey] = row
			}
			label[aggKey] = lbl
			cutOf[aggKey] = model.CutType(cut)
			// 营收 = 零售 + 统收。
			row.RevenueYuan = row.RevenueYuan.Add(c.revenue)
			// 成本三块:销售 + 统收 + 报损(都在损益里)。
			row.SalesCostYuan = row.SalesCostYuan.Add(c.salesCost).Add(c.bulkCost).Add(c.wasteCost)
			row.SoldKg = row.SoldKg.Add(c.soldKg)
			row.ProducedKg = row.ProducedKg.Add(c.producedKg)
			row.OpeningKg = row.OpeningKg.Add(c.openingKg)
			row.ClosingKg = row.ClosingKg.Add(c.closingKg)
			// 预售结转的预期毛利(与每日毛利同口径 ⇒ 长期毛利 = 每日毛利之和)。
			row.GrossMarginYuan = row.GrossMarginYuan.Add(c.presaleMargin)
		}
		if groupBy != "sku" {
			continue
		}
		// SKU 粒度:营收/销量按货号;成本按当日该部位的移动加权单价分摊。
		if err := s.accumulateSkuPeriod(ctx, branchID, d, data, agg, label); err != nil {
			return nil, err
		}
	}

	out := make([]PeriodMarginRow, 0, len(agg))
	var tot TotalGrossMargin
	for key, row := range agg {
		row.Label = label[key]
		if groupBy == "cut" {
			row.CutType = cutOf[key]
		}
		row.GrossMarginYuan = row.RevenueYuan.Sub(row.SalesCostYuan).Add(row.GrossMarginYuan)
		if !row.RevenueYuan.IsZero() {
			f, _ := row.GrossMarginYuan.Mul(decimal.NewFromInt(100)).Div(row.RevenueYuan).Float64()
			row.GrossMarginPct = f
		}
		out = append(out, *row)
		tot.RevenueYuan = tot.RevenueYuan.Add(row.RevenueYuan)
		tot.SalesCostYuan = tot.SalesCostYuan.Add(row.SalesCostYuan)
		tot.GrossMarginYuan = tot.GrossMarginYuan.Add(row.GrossMarginYuan)
	}
	if !tot.RevenueYuan.IsZero() {
		f, _ := tot.GrossMarginYuan.Mul(decimal.NewFromInt(100)).Div(tot.RevenueYuan).Float64()
		tot.GrossMarginPct = f
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })

	note := "期间毛利 = 期间收入(零售+统收) − 期间成本(销售+统收+报损,移动加权结转);" +
		"期初存货的成本在售出时结转,已含在期间成本内;预售结转的预期毛利也计在内。"
	if groupBy == "sku" {
		note = "SKU 粒度:营收/销量按货号(销售流水口径);" +
			"成本按**当日该部位**的移动加权单价分摊(库存是部位的 ⇒ 货号级成本只能近似)。"
	}
	if realTimeDays > 0 {
		note += fmt.Sprintf(";其中 %d 天没有日终盘点快照(门店未录日终盘点)⇒ 按当日实时计算补齐。", realTimeDays)
	}
	return &PeriodMarginReport{
		BranchID: branchID,
		FromDate: from,
		ToDate:   to,
		GroupBy:  groupBy,
		Rows:     out,
		Total:    tot,
		Days:     days,
		Note:     note,
	}, nil
}

// accumulateSkuPeriod 把某营业日的**货号级**销售与分摊成本累加进期间聚合。
//
// ⚠️ 两条路径:
//   - **实时算**的路径:引擎已经产出了 SKU 销售行 ⇒ 直接用(不重复查销售流水)。
//   - **快照**的路径:快照没有 SKU 粒度 ⇒ 读一次销售流水。
func (s *Service) accumulateSkuPeriod(ctx context.Context, branchID string, day time.Time,
	data periodDayData, agg map[string]*PeriodMarginRow, label map[string]string) error {
	cutOf, nameOf, err := s.loadSkuCatalog(ctx, branchID, nil)
	if err != nil {
		return err
	}
	type skuLine struct {
		cut    model.CutType
		kg     decimal.Decimal
		amount decimal.Decimal
	}
	lines := map[string]skuLine{}
	if len(data.skus) > 0 {
		for _, r := range data.skus {
			kg := r.SoldKg.Add(r.BulkSoldKg)
			if kg.IsZero() && r.SoldAmount.IsZero() && r.BulkSoldAmount.IsZero() {
				continue
			}
			lines[r.CubeProductID] = skuLine{
				cut:    r.CutType,
				kg:     kg,
				amount: r.SoldAmount.Add(r.BulkSoldAmount),
			}
		}
	} else {
		start, end := s.businessDayBounds(day)
		soldKg, soldAmount, bulkKg, bulkAmount, _, cutFromSales, err :=
			s.loadSkuFlows(ctx, branchID, start, end)
		if err != nil {
			return err
		}
		for sku, kg := range soldKg {
			cut := cutOf[sku]
			if cut == "" {
				cut = cutFromSales[sku]
			}
			if cut == "" {
				continue
			}
			lines[sku] = skuLine{
				cut:    cut,
				kg:     kg.Add(bulkKg[sku]),
				amount: soldAmount[sku].Add(bulkAmount[sku]),
			}
		}
	}
	for sku, l := range lines {
		if l.cut == "" {
			continue
		}
		row, ok := agg[sku]
		if !ok {
			row = &PeriodMarginRow{Key: sku, Label: nameOf[sku]}
			agg[sku] = row
		}
		label[sku] = nameOf[sku]
		row.RevenueYuan = row.RevenueYuan.Add(l.amount)
		row.SoldKg = row.SoldKg.Add(l.kg)
		// 成本 = 该 SKU 当日销量 × 当日该部位的移动加权单价(按部位分摊)。
		unit := data.cuts[string(l.cut)].unit
		row.SalesCostYuan = row.SalesCostYuan.Add(l.kg.Mul(unit))
	}
	return nil
}
