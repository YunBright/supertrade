// Package service / forecast_compare.go —— **多门店分级基线对比**(owner 2026-10-30 第 1 条)。
//
// # 要看什么
//
//	按 (体重段, 部位) 把多家店的基线**并排** ⇒ 一眼看出"哪家店的出肉率偏"。
//
//	例:同一段 200-249kg 的五花,店 00 的 p50 是 10kg、店 01 是 12kg
//	⇒ 差 20% ⇒ 要么是师傅手法不同,要么是其中一家的基线**不干净**(脏基线)。
//
// # 权限(⚠️ 硬要求)
//
//	请求里的**每一个**门店都必须有 `freshmeat:view`
//	⇒ 否则 403。**绝不能**让一个门店看到别家的基线。
//	(鉴权在 handler 层逐店判,见 handler.CompareForecastBaselines。)
//
// # 只读
//
//	这个端点不写任何东西 ⇒ 纯查询。
package service

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
)

// ForecastCompareRow 一家店在某个分层上的基线(对比用的一行)。
//
// ⚠️ 重量用**字符串**(固定 3 位小数)而不是数字:
//
//	JSON 数字会把 10.000 变成 10 ⇒ 前端显示"10"与"10.000"对不上,
//	跨店对比时这点差别会被误读成"数据不一致"。
type ForecastCompareRow struct {
	BranchID    string `json:"branch_id"`
	SampleCount int    `json:"sample_count"`
	P10Kg       string `json:"p10_kg"`
	P50Kg       string `json:"p50_kg"`
	P90Kg       string `json:"p90_kg"`
	MeanKg      string `json:"mean_kg"`
	// SampleSource:recorded | estimated —— 这条基线用的是哪个样本池。
	SampleSource string `json:"sample_source"`
	// SourceBranchID 是**样本来源门店**(跨店借用时 = 被借方)。
	SourceBranchID string `json:"source_branch_id"`
	// ManualGenerated = true ⇒ 管理员按某一天手动生成的。
	ManualGenerated bool `json:"manual_generated"`
	// Usable = 这条基线**能不能用**(样本够,或是手动生成)。
	Usable bool `json:"usable"`
}

// ForecastCompareBucket 一个 (体重段, 部位) 分层下,多家店的基线并排。
type ForecastCompareBucket struct {
	WeightBand string               `json:"weight_band"`
	CutType    model.CutType        `json:"cut_type"`
	Baselines  []ForecastCompareRow `json:"baselines"`
	// MinP50Kg / MaxP50Kg / AvgP50Kg 是这一层里各店 p50 的最小/最大/平均。
	MinP50Kg string `json:"min_p50_kg"`
	MaxP50Kg string `json:"max_p50_kg"`
	AvgP50Kg string `json:"avg_p50_kg"`
	// SpreadPct 是"最大与最小相差百分之几"((max−min)/min×100)。
	SpreadPct float64 `json:"spread_pct"`
}

// ForecastCompareResult 多门店对比的结果。
type ForecastCompareResult struct {
	Branches []string                `json:"branches"`
	Buckets  []ForecastCompareBucket `json:"buckets"`
}

// CompareForecastBaselines 把多家店的基线按 (体重段, 部位) 并排。
//
// ⚠️ 只返回**请求里**的门店 ⇒ 别的店的数据一点都看不到。
func (s *Service) CompareForecastBaselines(ctx context.Context, branchIDs []string) (*ForecastCompareResult, error) {
	if len(branchIDs) == 0 {
		return nil, fmt.Errorf("%w: branches 必填(逗号分隔的门店号)", ErrInvalidInput)
	}
	// 去重但**保持请求顺序**(前端是按请求顺序展示的)。
	seen := map[string]bool{}
	ordered := make([]string, 0, len(branchIDs))
	for _, b := range branchIDs {
		b = strings.TrimSpace(b)
		if b == "" || seen[b] {
			continue
		}
		seen[b] = true
		ordered = append(ordered, b)
	}
	if len(ordered) == 0 {
		return nil, fmt.Errorf("%w: branches 必填(逗号分隔的门店号)", ErrInvalidInput)
	}

	// 每家店的基线(索引 = 分层键)。
	perBranch := map[string]map[forecastBucketKey]model.PigForecastBaseline{}
	for _, b := range ordered {
		rows, err := s.loadBranchBaselineRows(ctx, b)
		if err != nil {
			return nil, err
		}
		perBranch[b] = rows
	}

	// 分组:(体重段, 部位) → 各店的行(按请求顺序)。
	type bucketKey struct {
		band string
		cut  model.CutType
	}
	grouped := map[bucketKey][]ForecastCompareRow{}
	for _, b := range ordered {
		for k, row := range perBranch[b] {
			bk := bucketKey{band: k.band, cut: k.cut}
			grouped[bk] = append(grouped[bk], ForecastCompareRow{
				BranchID:        b,
				SampleCount:     row.SampleCount,
				P10Kg:           row.P10Kg.StringFixed(3),
				P50Kg:           row.P50Kg.StringFixed(3),
				P90Kg:           row.P90Kg.StringFixed(3),
				MeanKg:          row.MeanKg.StringFixed(3),
				SampleSource:    row.SampleSource,
				SourceBranchID:  row.SourceBranchID,
				ManualGenerated: row.ManualGenerated,
				Usable:          baselineRowUsable(row),
			})
		}
	}

	keys := make([]bucketKey, 0, len(grouped))
	for k := range grouped {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].band != keys[j].band {
			return keys[i].band < keys[j].band
		}
		return string(keys[i].cut) < string(keys[j].cut)
	})

	out := &ForecastCompareResult{Branches: ordered, Buckets: []ForecastCompareBucket{}}
	for _, k := range keys {
		rows := grouped[k]
		// 桶内按请求顺序排(上面的循环就是按 ordered 追加的,这里再稳一次)。
		sort.SliceStable(rows, func(i, j int) bool {
			return branchOrder(ordered, rows[i].BranchID) < branchOrder(ordered, rows[j].BranchID)
		})
		minP50, maxP50, sum := decimal.Zero, decimal.Zero, decimal.Zero
		valid := 0
		for _, r := range rows {
			v, err := decimal.NewFromString(r.P50Kg)
			if err != nil {
				continue
			}
			if valid == 0 || v.LessThan(minP50) {
				minP50 = v
			}
			if valid == 0 || v.GreaterThan(maxP50) {
				maxP50 = v
			}
			sum = sum.Add(v)
			valid++
		}
		avg := decimal.Zero
		if valid > 0 {
			avg = sum.Div(decimal.NewFromInt(int64(valid)))
		}
		spread := decimal.Zero
		if minP50.IsPositive() {
			spread = maxP50.Sub(minP50).Div(minP50).Mul(decimal.NewFromInt(100))
		}
		out.Buckets = append(out.Buckets, ForecastCompareBucket{
			WeightBand: k.band,
			CutType:    k.cut,
			Baselines:  rows,
			MinP50Kg:   minP50.StringFixed(3),
			MaxP50Kg:   maxP50.StringFixed(3),
			AvgP50Kg:   avg.StringFixed(3),
			SpreadPct:  spread.Round(2).InexactFloat64(),
		})
	}
	return out, nil
}

// branchOrder 门店在请求顺序里的位置(排序用)。
func branchOrder(ordered []string, branchID string) int {
	for i, b := range ordered {
		if b == branchID {
			return i
		}
	}
	return len(ordered)
}
