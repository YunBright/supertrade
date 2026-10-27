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
//	`cut_cost_daily`(**部位级**成本日快照)—— 它已经是移动加权口径
//	(期初/入库/销售/期末/加权单价/销售成本都在),所以长期聚合就是**把日快照加起来**,
//	不需要重算任何东西。
//
// ⚠️ 旧的 `sku_cost_daily`(SKU 级)**已停写** ⇒ 这里**不能再查它**
//   (查了就是空/过期数据,而这个接口是对外的)。
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
// 部位快照里**没有 SKU 粒度** ⇒ SKU 粒度是**算出来**的:
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

	// ⚠️ **部位级**成本日快照(旧的 sku_cost_daily 已停写)。
	var rows []model.CutCostDaily
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date >= ? AND biz_date <= ?", branchID, fromDate, toDate).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query cut_cost_daily: %w", err)
	}

	if groupBy == "sku" {
		return s.periodMarginBySku(ctx, branchID, fromT, toT, from, to, rows)
	}

	agg := map[string]*PeriodMarginRow{}
	label := map[string]string{}
	cutOf := map[string]model.CutType{}
	for _, r := range rows {
		key := dateKey(r.BizDate)
		lbl := dateKey(r.BizDate)
		switch groupBy {
		case "cut":
			key = string(r.CutType)
			lbl = model.CutLabel(r.CutType)
		}
		row, ok := agg[key]
		if !ok {
			row = &PeriodMarginRow{Key: key}
			agg[key] = row
		}
		label[key] = lbl
		cutOf[key] = r.CutType
		// 营收 = 零售 + 统收。
		row.RevenueYuan = row.RevenueYuan.Add(r.SoldAmount).Add(r.BulkSoldAmount)
		// 成本三块:销售 + 统收 + 报损(都在损益里)。
		row.SalesCostYuan = row.SalesCostYuan.Add(r.SalesCostYuan).Add(r.BulkCostYuan).Add(r.WasteCostYuan)
		row.SoldKg = row.SoldKg.Add(r.SoldKg).Add(r.BulkSoldKg)
		row.ProducedKg = row.ProducedKg.Add(r.ProducedKg)
		row.OpeningKg = row.OpeningKg.Add(r.OpeningKg)
		row.ClosingKg = row.ClosingKg.Add(r.ClosingKg)
		// 预售结转的预期毛利(与每日毛利同口径 ⇒ 长期毛利 = 每日毛利之和)。
		row.GrossMarginYuan = row.GrossMarginYuan.Add(r.PresaleMarginYuan)
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

	days := int(toT.Sub(fromT).Hours()/24) + 1
	return &PeriodMarginReport{
		BranchID: branchID,
		FromDate: from,
		ToDate:   to,
		GroupBy:  groupBy,
		Rows:     out,
		Total:    tot,
		Days:     days,
		Note: "期间毛利 = 期间收入(零售+统收) − 期间成本(销售+统收+报损,移动加权结转);" +
			"期初存货的成本在售出时结转,已含在期间成本内;预售结转的预期毛利也计在内。",
	}, nil
}

// periodMarginBySku 算 **SKU 粒度**的长期毛利(近似口径)。
//
// # 为什么是近似的
//
// 部位级成本日快照里**没有 SKU 粒度**(唯一键是 branch_id + biz_date + cut_type)
// ⇒ SKU 的成本只能**按部位单价分摊**:
//
//	营收 / 销量  = 按 SKU(销售流水本来就是 SKU 级)
//	销售成本     = 该 SKU 当日销量 × 当日该部位的移动加权单价
//
// ⚠️ 当日**没有盘点** ⇒ 没有日快照 ⇒ 用**最近一次已知的部位单价**兜底。
//
// # 为什么不按货号拆成本
//
// 多货号部位里各货号的成本本来就不可分(库存是部位的)
// ⇒ 与每日毛利的口径一致:成本看部位行,这里只是给个 SKU 视角的下钻。
func (s *Service) periodMarginBySku(ctx context.Context, branchID string,
	fromT, toT time.Time, from, to string, snaps []model.CutCostDaily) (*PeriodMarginReport, error) {
	// 每日每部位的单价(来自部位级日快照)。
	unitOfDay := map[string]map[string]decimal.Decimal{}
	for _, r := range snaps {
		d := dateKey(r.BizDate)
		if unitOfDay[d] == nil {
			unitOfDay[d] = map[string]decimal.Decimal{}
		}
		unitOfDay[d][string(r.CutType)] = r.MovingAvgUnitCost
	}
	cutOf, nameOf, err := s.loadSkuCatalog(ctx, branchID, nil)
	if err != nil {
		return nil, err
	}
	agg := map[string]*PeriodMarginRow{}
	// 最近一次已知的部位单价(逐日往后带)。
	lastUnit := map[string]decimal.Decimal{}
	for d := fromT; !d.After(toT); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		for cut, unit := range unitOfDay[key] {
			lastUnit[cut] = unit
		}
		start, end := s.businessDayBounds(d)
		soldKg, soldAmount, bulkKg, bulkAmount, _, cutFromSales, err :=
			s.loadSkuFlows(ctx, branchID, start, end)
		if err != nil {
			return nil, err
		}
		for sku, kg := range soldKg {
			cut := string(cutOf[sku])
			if cut == "" {
				cut = string(cutFromSales[sku])
			}
			if cut == "" {
				continue
			}
			row, ok := agg[sku]
			if !ok {
				row = &PeriodMarginRow{Key: sku, Label: nameOf[sku]}
				agg[sku] = row
			}
			row.RevenueYuan = row.RevenueYuan.Add(soldAmount[sku]).Add(bulkAmount[sku])
			row.SoldKg = row.SoldKg.Add(kg).Add(bulkKg[sku])
			// 成本 = 该 SKU 当日销量 × 当日该部位的移动加权单价(按部位分摊)。
			row.SalesCostYuan = row.SalesCostYuan.Add(kg.Mul(lastUnit[cut]))
		}
	}
	out := make([]PeriodMarginRow, 0, len(agg))
	var tot TotalGrossMargin
	for _, row := range agg {
		row.GrossMarginYuan = row.RevenueYuan.Sub(row.SalesCostYuan)
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

	days := int(toT.Sub(fromT).Hours()/24) + 1
	return &PeriodMarginReport{
		BranchID: branchID,
		FromDate: from,
		ToDate:   to,
		GroupBy:  "sku",
		Rows:     out,
		Total:    tot,
		Days:     days,
		Note: "SKU 粒度:营收/销量按货号(销售流水口径);" +
			"成本按**当日该部位**的移动加权单价分摊(库存是部位的 ⇒ 货号级成本只能近似);" +
			"当日没有盘点时用最近一次已知的部位单价。",
	}, nil
}
