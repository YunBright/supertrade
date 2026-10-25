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
// # 数据源
//
// `sku_cost_daily`(SKU 成本日快照)—— 它已经是移动加权口径
// (期初/入库/销售/期末/加权单价/销售成本都在),所以长期聚合就是**把日快照加起来**,
// 不需要重算任何东西。
//
// # 口径
//
//	期间收入 = Σ(sold_amount + bulk_sold_amount)   ← 零售 + 酒店统收
//	期间销售成本 = Σ sales_cost_yuan
//	期间毛利 = 收入 − 销售成本
//
// ⚠️ **期初存货的成本**在售出时按移动加权结转 ⇒ 自然落在期间的 sales_cost 里
// ⇒ 长期毛利不需要额外处理"期初存货"这一项。
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

	var rows []model.SkuCostDaily
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date >= ? AND biz_date <= ?", branchID, fromDate, toDate).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query sku_cost_daily: %w", err)
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
		case "sku":
			key = r.CubeProductID
			lbl = r.CubeProductID
		}
		row, ok := agg[key]
		if !ok {
			row = &PeriodMarginRow{Key: key}
			agg[key] = row
		}
		label[key] = lbl
		cutOf[key] = r.CutType
		row.RevenueYuan = row.RevenueYuan.Add(r.SoldAmount).Add(r.BulkSoldAmount)
		row.SalesCostYuan = row.SalesCostYuan.Add(r.SalesCostYuan)
		row.SoldKg = row.SoldKg.Add(r.SoldKg).Add(r.BulkSoldKg)
		row.ProducedKg = row.ProducedKg.Add(r.ProducedKg)
		row.OpeningKg = row.OpeningKg.Add(r.OpeningKg)
		row.ClosingKg = row.ClosingKg.Add(r.ClosingKg)
	}

	out := make([]PeriodMarginRow, 0, len(agg))
	var tot TotalGrossMargin
	for key, row := range agg {
		row.Label = label[key]
		if groupBy == "cut" {
			row.CutType = cutOf[key]
		}
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
		GroupBy:  groupBy,
		Rows:     out,
		Total:    tot,
		Days:     days,
		Note: "期间毛利 = 期间收入(零售+统收) − 期间销售成本(移动加权结转);" +
			"期初存货的成本在售出时结转,已含在期间成本内。",
	}, nil
}
