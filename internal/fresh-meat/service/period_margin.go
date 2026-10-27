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
// # 数据源(2026-10-27:成本链改部位级 + 链逐日接续)
//
//	**逐日**取部位级成本数据,**链式推进**:
//	今天的期初 = 昨天的期末 ⇒ 链不断 ⇒ 成本是真实的。
//
// ⚠️ **为什么必须链式**(生产实测踩到的坑):
//
//	旧实现每天各自从"门店期初结转"起算 ⇒ 链**每天断一次**
//	⇒ 成本塌成"期初那点残值" ⇒ 毛利虚高。
//	实测(店 00):09 月整月成本恒 0(毛利看起来 100%),
//	10-03 起成本 ≈ 100 元(毛利率 97%)—— 都不是真的。
//
// ⇒ 现在:逐日链式算(见 cut_chain.go);已物化(cut_cost_daily 有快照)时
//   直接用前一日快照当期初 ⇒ O(1) 快路径。
//
// ⚠️ 旧的 `sku_cost_daily`(SKU 级)**已停写** ⇒ 这里**不能再查它**。
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
// # ⚠️ "成本算不出来" ≠ "成本为 0"(2026-10-27 加)
//
//	链起算点之前(没有基线:既无期初结转、当天也没进货)⇒ 那天的成本**算不出来**。
//	报成 0 ⇒ 门店看到"毛利 100%" ⇒ 会以为赚疯了。
//	⇒ 显式标记(cost_incomplete / days_without_cost_baseline)+ Note 说明。
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

	// CostIncomplete = true ⇒ 本行的成本**不完整**:
	// 参与计算的天里,有部位的成本**算不出来**(既无期初库存也无进货)。
	// ⚠️ 这不是"成本为 0",是"不知道成本" ⇒ 成本被低估、毛利被高估。
	CostIncomplete bool `json:"cost_incomplete,omitempty"`
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
	// DaysWithoutCostBaseline 是"成本**算不出来**"的天数
	// (链起算点之前 / 那天既没有期初库存也没有进货)。
	DaysWithoutCostBaseline int `json:"days_without_cost_baseline,omitempty"`
	// Note 说明口径(尤其是"期初存货成本在售出时结转")。
	Note string `json:"note,omitempty"`
}

// periodCutRow 某营业日某部位的**期间毛利累加源**(归一后的同一形状)。
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
	// uncomputable = true ⇒ 该部位当天成本算不出来(不是成本为 0)。
	uncomputable bool
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

	// ⚠️ **链式推进**:基线早于区间起点时,会多算几天"只为把链接上" ⇒ 那几天不计入聚合。
	chainDays, err := s.computeCutChainDays(ctx, branchID, fromT, toT)
	if err != nil {
		return nil, err
	}

	agg := map[string]*PeriodMarginRow{}
	label := map[string]string{}
	cutOf := map[string]model.CutType{}
	days := 0
	daysWithoutBaseline := 0
	for _, cd := range chainDays {
		if cd.day.Before(fromT) {
			continue // 只为链接基线,不计入区间。
		}
		days++
		if cd.costIncomplete {
			daysWithoutBaseline++
		}
		// 部位级数据 → 聚合键。
		for _, r := range cd.cutRows {
			cut := string(r.CutType)
			aggKey := cd.dateKey
			lbl := cd.dateKey
			switch groupBy {
			case "cut":
				aggKey = cut
				lbl = model.CutLabel(r.CutType)
			case "sku":
				continue // SKU 粒度单独处理(见下)。
			}
			row, ok := agg[aggKey]
			if !ok {
				row = &PeriodMarginRow{Key: aggKey}
				agg[aggKey] = row
			}
			label[aggKey] = lbl
			cutOf[aggKey] = r.CutType
			// 营收 = 零售 + 统收。
			row.RevenueYuan = row.RevenueYuan.Add(r.RevenueYuan)
			// 成本三块:销售 + 统收 + 报损(都在损益里)。
			row.SalesCostYuan = row.SalesCostYuan.Add(r.SalesCostYuan).Add(r.BulkCostYuan).Add(r.WasteCostYuan)
			row.SoldKg = row.SoldKg.Add(r.SoldKg).Add(r.BulkSoldKg)
			row.ProducedKg = row.ProducedKg.Add(r.ProducedKg)
			row.OpeningKg = row.OpeningKg.Add(r.OpeningKg)
			row.ClosingKg = row.ClosingKg.Add(r.ClosingKg)
			// 预售结转的预期毛利(与每日毛利同口径 ⇒ 长期毛利 = 每日毛利之和)。
			row.GrossMarginYuan = row.GrossMarginYuan.Add(r.PresaleMarginYuan)
			if r.CostUncomputable {
				row.CostIncomplete = true
			}
		}
		if groupBy != "sku" {
			continue
		}
		// SKU 粒度:营收/销量按货号;成本按当日该部位的移动加权单价分摊。
		accumulateSkuPeriod(cd, agg, label)
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
	if daysWithoutBaseline > 0 {
		note += fmt.Sprintf(";其中 %d 天的成本**算不出来**(链起算点之前:既没有期初库存也没有进货)"+
			"⇒ 这部分成本被低估、毛利被高估(cost_incomplete=true 的行)", daysWithoutBaseline)
	}
	return &PeriodMarginReport{
		BranchID:                branchID,
		FromDate:                from,
		ToDate:                  to,
		GroupBy:                 groupBy,
		Rows:                    out,
		Total:                   tot,
		Days:                    days,
		DaysWithoutCostBaseline: daysWithoutBaseline,
		Note:                    note,
	}, nil
}

// accumulateSkuPeriod 把某营业日的**货号级**销售与分摊成本累加进期间聚合。
//
// ⚠️ 货号行的营收/销量来自**引擎产出的 SKU 销售行**(与部位行同一个引擎)
//   ⇒ 不会与部位口径打架。
func accumulateSkuPeriod(cd cutChainDay, agg map[string]*PeriodMarginRow, label map[string]string) {
	// 部位 → 当日移动加权单价(SKU 粒度按它分摊成本)。
	unitOf := map[string]decimal.Decimal{}
	for _, r := range cd.cutRows {
		unitOf[string(r.CutType)] = r.MovingAvgUnitCost
	}
	for _, r := range cd.skuRows {
		kg := r.SoldKg.Add(r.BulkSoldKg)
		amount := r.SoldAmount.Add(r.BulkSoldAmount)
		if kg.IsZero() && amount.IsZero() {
			continue
		}
		sku := r.CubeProductID
		if sku == "" {
			continue
		}
		row, ok := agg[sku]
		if !ok {
			row = &PeriodMarginRow{Key: sku}
			agg[sku] = row
		}
		if r.CubeProductName != "" {
			label[sku] = r.CubeProductName
		}
		row.RevenueYuan = row.RevenueYuan.Add(amount)
		row.SoldKg = row.SoldKg.Add(kg)
		// 成本 = 该 SKU 当日销量 × 当日该部位的移动加权单价(按部位分摊)。
		row.SalesCostYuan = row.SalesCostYuan.Add(kg.Mul(unitOf[string(r.CutType)]))
	}
}
