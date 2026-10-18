package service

import (
	"context"
	"fmt"
	"sort"

	"github.com/YunBright/supertrade/internal/salesdaily"
)

// DailyRangeLister 读「门店 × 区间」的已定稿销售事实（PG 实现）。
type DailyRangeLister interface {
	LoadRange(ctx context.Context, branchID, from, to string) (salesdaily.RangeFacts, error)
}

// WithDailyRange 注入已定稿事实库。
//
// # 为什么注入后**现有端点**的行为就变了，而不是另开一个端点
//
// 每日销售统计卡片（wechat-bot）与 Flutter 报表页调的都是
// `GET /reports/sales` 与 `/reports/sales/today`。
//
// 若为"读 PG"另开一个端点，两个调用方都要改，而**只要有一个没改**，
// 就又出现"卡片读 PG、报表页读 cube"的口径分叉 ——
// 正是这次改造要消灭的东西。→ 换的是**数据来源**，不是接口。
//
// ⚠️ 未注入时行为与改造前**逐字相同**（全部现查源库）。
func (s *Service) WithDailyRange(l DailyRangeLister) *Service {
	s.daily = l
	return s
}

// dataSource 标明这一次报表的数字来自哪里。
type dataSource string

const (
	// SourceDailyStore 表示来自已定稿的每日事实库。
	SourceDailyStore dataSource = "daily_store"
	// SourceCube 表示现查源库。
	SourceCube dataSource = "cube"
)

// rawSource 是一次报表查询的原始结果 + 它是从哪条路来的。
type rawSource struct {
	rows []RawReport
	// totals 是**大盘总额**，无分组、不限量。
	//
	// ⚠️ 它必须独立于 rows：rows 是"按维度分组后的子集"，
	//	对 rows 求和会在两种情况下偏低 ——
	//	① cube 侧带 Limit（topN 截断）；② 空类目名/空编码被丢弃，
	//	而未分组的总额查询保留同一批行。
	//	线上实测门店 00 / 2026-10-16：品类口径 2024.6 元 vs
	//	供应商口径 13003.8 元（差 6 倍，**零报错**）。
	//	→ 顶部汇总必须用 totals，不能用 sum(rows)。
	totals *RawReport
	source dataSource
	// reason 非空表示**没有**走已定稿库时的可读原因（面向运维/店长）。
	reason string
	// totalsDegraded 表示 totals 取不到、汇总退回"按明细求和"。
	totalsDegraded bool
}

// loadRaw 取一次报表的原始行与大盘总额，按「整段一条路」选源。
//
// # ⚠️ 必须**整段**统一选源，不许逐天混
//
// 区间内 5 天在 PG、25 天只能现查源库时，若逐天拼接，
// 同一份报表里的"总额"与"品类合计"就来自两套口径 ——
// 那正是上面注释里那个 6 倍差的复现，而且这次更难发现
// （数据源不同源，看起来更"自然"）。
//
// → LoadRange 在覆盖不全时返回 ErrRangeIncomplete，
//
//	于是这里天然是二选一：有快照整段用 PG，没有整段查 cube。
func (s *Service) loadRaw(ctx context.Context, q Query) (rawSource, error) {
	if s.daily != nil {
		facts, err := s.daily.LoadRange(ctx, q.StoreBranchID, q.From, q.To)
		if err == nil {
			// ⚠️ 只在**完全覆盖**时走 PG；err != nil 的分支一律回落。
			//   理由见本函数注释。
			return rawFromDaily(facts, q.GroupBy), nil
		}
		return s.loadRawFromCube(ctx, q, rangeFallbackReason(err, facts))
	}
	return s.loadRawFromCube(ctx, q, "")
}

// rangeFallbackReason 把"为什么没用上已定稿库"翻译成人能读的一句话。
func rangeFallbackReason(err error, facts salesdaily.RangeFacts) string {
	if facts.BranchID == "" {
		return ""
	}
	if facts.CoveredDays == 0 {
		return fmt.Sprintf("销售数据存储里还没有 %s~%s 的采集结果，本次现查源库", facts.From, facts.To)
	}
	return fmt.Sprintf("销售数据存储里 %s~%s 只覆盖了 %d/%d 天，本次现查源库",
		facts.From, facts.To, facts.CoveredDays, facts.TotalDays)
}

// rawFromDaily 把已定稿事实转成报表用的原始行。
func rawFromDaily(facts salesdaily.RangeFacts, groupBy string) rawSource {
	out := rawSource{source: SourceDailyStore}
	totals := RawReport{
		NetAmount:  facts.Totals.NetAmount,
		NetQty:     facts.Totals.NetQty,
		RefCost:    facts.Totals.RefCost,
		CostedRows: int64(facts.Totals.CostedRows),
		TotalRows:  int64(facts.Totals.TotalRows),
	}
	out.totals = &totals

	switch groupBy {
	case DimProduct:
		for _, p := range facts.Products {
			if p.ItemNo == "" {
				continue
			}
			out.rows = append(out.rows, RawReport{
				Code: p.ItemNo, Name: p.Name, Extra: p.Category,
				NetAmount: p.Amount, NetQty: p.Qty,
			})
		}
	default: // DimCategory
		for _, c := range facts.Categories {
			if c.Name == "" {
				continue
			}
			out.rows = append(out.rows, RawReport{
				Code: c.Name, Name: c.Name,
				NetAmount: c.Amount, NetQty: c.Qty, RefCost: c.Cost,
				CostedRows: c.CostedRow, TotalRows: c.TotalRow,
			})
		}
	}
	sortByAmount(out.rows)
	return out
}

// loadRawFromCube 是"现查源库"那条路。
func (s *Service) loadRawFromCube(ctx context.Context, q Query, reason string) (rawSource, error) {
	raw, err := s.cube.LoadSalesReport(ctx, q.StoreBranchID, q.GroupBy, q.From, q.To, q.Limit)
	if err != nil {
		return rawSource{}, err
	}
	out := rawSource{source: SourceCube, reason: reason}
	if raw != nil {
		out.rows = raw.Rows
	}
	// 总额走**独立的无分组查询**，不是对 rows 求和。
	// ⚠️ 查不到时退回按明细求和，但**必须标记降级**（见 totalsViaCube）。
	t, ok, terr := s.totalsViaCube(ctx, q.StoreBranchID, q.From, q.To, out.rows)
	if ok {
		out.totals = &t
	} else {
		out.totals = &t
		out.totalsDegraded = true
		if out.reason == "" && terr != nil {
			out.reason = "大盘总额查询失败，本次销售额按明细求和（可能偏低）：" + terr.Error()
		}
	}
	return out, nil
}

// buildRawSummary 从明细行求和出一个总额（仅降级路径用）。
//
// ⚠️ 它天生偏低（被 Limit 截断、被丢弃空名行），见 rawSource.totals 的注释。
func buildRawSummary(rows []RawReport) RawReport {
	var sum RawReport
	for _, r := range rows {
		sum.NetAmount += r.NetAmount
		sum.NetQty += r.NetQty
		sum.RefCost += r.RefCost
		sum.CostedRows += r.CostedRows
		sum.TotalRows += r.TotalRows
	}
	return sum
}

// totalsViaCube 取大盘总额（独立的无分组查询），失败时退回按明细求和。
//
// # 返回值第二个是 authoritative
//
// ⚠️ 降级求和值**非零但偏低**（本例低 85%），
//
//	拿它当权威基去算环比会得到一个"看起来正常、实则全错"的百分比。
//	所以调用点必须同时读这个标志，不能只看数字。
//
// ⚠️ 它被 loadRaw 与 CompareData **共用**：
//
//	这两条路径都产出顶部汇总，而"单店报表"与"对比列"必须同口径 ——
//	不一致时用户看到的是同一家的两个数字，且没人说得清哪份对。
//	（TestCompareData_SummaryMatchesReport 就是这条不变量。）
func (s *Service) totalsViaCube(
	ctx context.Context, storeBranchID, from, to string, rows []RawReport,
) (RawReport, bool, error) {
	if s.cube != nil {
		t, err := s.cube.LoadSalesTotals(ctx, storeBranchID, from, to)
		if err == nil {
			return t, true, nil
		}
		return buildRawSummary(rows), false, err
	}
	return buildRawSummary(rows), false, fmt.Errorf("salesagg: cube 未配置，无法取大盘总额")
}

func sortByAmount(rows []RawReport) {
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].NetAmount > rows[j].NetAmount })
}
