package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/YunBright/supertrade/internal/salesdaily"
)

// fakeDaily 是 DailyRangeLister 的替身。
type fakeDaily struct {
	calls int
	facts salesdaily.RangeFacts
	err   error
}

func (f *fakeDaily) LoadRange(context.Context, string, string, string) (salesdaily.RangeFacts, error) {
	f.calls++
	return f.facts, f.err
}

// TestSummaryComesFromTotalsQuery ⭐ 顶部汇总必须来自**独立的无分组查询**。
//
// ⚠️ 明细是"按维度分组后的子集"，对它求和会偏低：
//
//	① cube 侧带 Limit（topN 截断）；
//	② 空类目名/空编码的行被丢弃，而未分组的总额保留同一批行。
//
//	线上实测门店 00 / 2026-10-16：品类口径 2024.6 元 vs 供应商口径
//	13003.8 元 —— 顶部只统计到 **15.6%**，且**零报错**。
//
// 本用例刻意让"行求和 = 302"与"独立总额 = 13003.8"差一个数量级：
// 若实现退回按明细求和，汇总会变成 302.00 而不是 13003.80。
func TestSummaryComesFromTotalsQuery(t *testing.T) {
	f := &fakeCube{
		rows: map[string][]RawReport{
			DimCategory: {
				{Code: "a", Name: "生鲜联营", NetAmount: 100, CostedRows: 1, TotalRows: 1},
				{Code: "b", Name: "百货联营", NetAmount: 200, CostedRows: 1, TotalRows: 1},
				{Code: "c", Name: "未归类", NetAmount: 2, CostedRows: 1, TotalRows: 1},
			},
		},
		totals: &RawReport{
			NetAmount: 13003.8, NetQty: 1400, RefCost: 10403.04,
			CostedRows: 100, TotalRows: 100,
		},
	}
	rep, err := New(f).Report(context.Background(), Query{
		StoreBranchID: "00", GroupBy: DimCategory,
		From: "2026-10-16", To: "2026-10-16",
	})
	if err != nil {
		t.Fatalf("Report：%v", err)
	}
	if rep.Summary.NetAmount != "13,003.80" {
		t.Errorf("summary net_amount = %q，期望 13,003.80（独立总额查询），"+
			"若得到 302.00 说明退回按明细求和了", rep.Summary.NetAmount)
	}
	if f.totalsCalls != 1 {
		t.Errorf("总额查询被调 %d 次，期望 1 次", f.totalsCalls)
	}
}

// TestSummaryFallsBackToRowSumButSaysSo 总额查询失败要降级，但**必须说**。
//
// ⚠️ 降级求和值非零却偏低（本例低 85%），
//
//	拿它当权威基去算环比会得到一个"看起来正常、实则全错"的百分比。
//	所以响应里必须有可读的降级原因。
func TestSummaryFallsBackToRowSumButSaysSo(t *testing.T) {
	f := &fakeCube{
		rows: map[string][]RawReport{
			DimCategory: {{Code: "a", Name: "A", NetAmount: 100, CostedRows: 1, TotalRows: 1}},
		},
		totalsErr: errors.New("源库超时"),
	}
	rep, err := New(f).Report(context.Background(), Query{
		StoreBranchID: "00", GroupBy: DimCategory,
		From: "2026-10-16", To: "2026-10-16",
	})
	if err != nil {
		t.Fatalf("总额查询失败不该让整份报表失败：%v", err)
	}
	if rep.Summary.NetAmount != "100.00" {
		t.Errorf("降级后汇总 = %q，期望 100.00（按明细求和）", rep.Summary.NetAmount)
	}
	if rep.FallbackReason == "" {
		t.Error("降级了却没写 fallback_reason —— 调用方无法判断这个数字可不可信")
	}
	if !strings.Contains(rep.FallbackReason, "偏低") {
		t.Errorf("降级文案要说明「可能偏低」，实际 %q", rep.FallbackReason)
	}
}

// TestReportPrefersDailyStoreWhenRangeCovered 覆盖完整时**一次 cube 都不调**。
//
// ⚠️ 判据是"cube 调用数为 0"，不是"结果等于某个值"：
//
//	逐维度断言只能证明"这一维走了 PG"，而真正要防的是
//	「总额走 PG、明细走 cube」—— 每维单看都对，合起来才是那个 6 倍差。
func TestReportPrefersDailyStoreWhenRangeCovered(t *testing.T) {
	cube := &fakeCube{rows: map[string][]RawReport{
		DimCategory: {{Code: "cube", Name: "源库品类", NetAmount: 999}},
	}}
	daily := &fakeDaily{facts: salesdaily.RangeFacts{
		BranchID: "00", From: "2026-10-10", To: "2026-10-16",
		Totals:      salesdaily.TotalsRow{NetAmount: 500, TotalRows: 10, CostedRows: 10},
		Categories:  []salesdaily.CategoryRow{{Name: "PG品类", Amount: 420}},
		Products:    []salesdaily.ProductRow{{ItemNo: "P1", Name: "pg货", Category: "PG品类", Amount: 420}},
		Suppliers:   []salesdaily.SupplierRow{{Code: "S1", Name: "行", Amount: 500}},
		CoveredDays: 7, TotalDays: 7,
	}}
	rep, err := New(cube).WithDailyRange(daily).Report(context.Background(), Query{
		StoreBranchID: "00", GroupBy: DimCategory,
		From: "2026-10-10", To: "2026-10-16",
	})
	if err != nil {
		t.Fatalf("Report：%v", err)
	}
	if cube.calls != 0 {
		t.Fatalf("PG 覆盖完整时不该碰 cube，实际调用 %d 次", cube.calls)
	}
	if len(rep.Rows) != 1 || rep.Rows[0].Name != "PG品类" {
		t.Errorf("明细应来自 PG，实际 %+v", rep.Rows)
	}
	if rep.Summary.NetAmount != "500.00" {
		t.Errorf("汇总 = %q，期望 500.00", rep.Summary.NetAmount)
	}
	if rep.DataSource != string(SourceDailyStore) {
		t.Errorf("data_source = %q，期望 daily_store", rep.DataSource)
	}
	if rep.FallbackReason != "" {
		t.Errorf("走了 PG 却还有 fallback_reason：%q", rep.FallbackReason)
	}
}

// TestReportFallsBackAsWholeWhenRangeIncomplete 覆盖不全**整段**回退。
//
// ⚠️ 部分数据看着完全可用，逐天混用正是那个 6 倍差的复现 ——
//
//	症状是同一份报表里「总额」与「品类合计」来自两套口径，
//	而且这次看起来更"自然"（数据源不同源）。
func TestReportFallsBackAsWholeWhenRangeIncomplete(t *testing.T) {
	cube := &fakeCube{
		rows:   map[string][]RawReport{DimCategory: {{Code: "cube", Name: "源库品类", NetAmount: 999}}},
		totals: &RawReport{NetAmount: 999, TotalRows: 1, CostedRows: 1},
	}
	partial := salesdaily.RangeFacts{
		BranchID: "00", From: "2026-10-10", To: "2026-10-16",
		Totals:      salesdaily.TotalsRow{NetAmount: 555},
		Categories:  []salesdaily.CategoryRow{{Name: "PG品类", Amount: 555}},
		CoveredDays: 5, TotalDays: 7,
	}
	daily := &fakeDaily{facts: partial, err: errors.New("区间内未覆盖全部营业日")}

	rep, err := New(cube).WithDailyRange(daily).Report(context.Background(), Query{
		StoreBranchID: "00", GroupBy: DimCategory,
		From: "2026-10-10", To: "2026-10-16",
	})
	if err != nil {
		t.Fatalf("Report：%v", err)
	}
	if cube.calls != 1 {
		t.Fatalf("覆盖不全应整段查 cube，实际 cube 调用 %d 次", cube.calls)
	}
	if len(rep.Rows) != 1 || rep.Rows[0].Name != "源库品类" {
		t.Errorf("明细应全部来自 cube（PG 的部分品类不能混进来），实际 %+v", rep.Rows)
	}
	if rep.Summary.NetAmount != "999.00" {
		t.Errorf("汇总 = %q，期望 999.00 —— **不能**用 PG 的部分数据 555", rep.Summary.NetAmount)
	}
	if rep.DataSource != string(SourceCube) {
		t.Errorf("data_source = %q，期望 cube", rep.DataSource)
	}
	if !strings.Contains(rep.FallbackReason, "5/7") {
		t.Errorf("回退原因要报出覆盖天数 5/7，实际 %q", rep.FallbackReason)
	}
}

// TestReportFromDailyStoreByProductDimension 商品维度的编码取 item_no。
func TestReportFromDailyStoreByProductDimension(t *testing.T) {
	cube := &fakeCube{}
	daily := &fakeDaily{facts: salesdaily.RangeFacts{
		BranchID: "00", From: "2026-10-16", To: "2026-10-16",
		Totals:      salesdaily.TotalsRow{NetAmount: 300, TotalRows: 2, CostedRows: 2},
		Products:    []salesdaily.ProductRow{{ItemNo: "000123", Name: "土豆", Category: "生鲜联营", Amount: 300}},
		CoveredDays: 1, TotalDays: 1,
	}}
	rep, err := New(cube).WithDailyRange(daily).Report(context.Background(), Query{
		StoreBranchID: "00", GroupBy: DimProduct,
		From: "2026-10-16", To: "2026-10-16",
	})
	if err != nil {
		t.Fatalf("Report：%v", err)
	}
	if len(rep.Rows) != 1 {
		t.Fatalf("明细行数 = %d，期望 1", len(rep.Rows))
	}
	// ⚠️ 编码必须是 item_no 而不是名称 ——
	//   Flutter 报表页的下钻是拿 code 发起第二次查询的。
	if rep.Rows[0].Code != "000123" {
		t.Errorf("行编码 = %q，期望 000123（item_no）", rep.Rows[0].Code)
	}
	if rep.Rows[0].Extra != "生鲜联营" {
		t.Errorf("行 Extra = %q，期望 生鲜联营（类别）", rep.Rows[0].Extra)
	}
}

// TestWithoutDailyStoreCubeIsOnlySource 未装配事实库时行为与改造前一致。
func TestWithoutDailyStoreCubeIsOnlySource(t *testing.T) {
	cube := &fakeCube{
		rows:   map[string][]RawReport{DimCategory: {{Code: "a", Name: "A", NetAmount: 50, CostedRows: 1, TotalRows: 1}}},
		totals: &RawReport{NetAmount: 50, TotalRows: 1, CostedRows: 1},
	}
	rep, err := New(cube).Report(context.Background(), Query{
		StoreBranchID: "00", GroupBy: DimCategory,
		From: "2026-10-16", To: "2026-10-16",
	})
	if err != nil {
		t.Fatalf("Report：%v", err)
	}
	if cube.calls != 1 {
		t.Errorf("未装配事实库时应查 cube 一次，实际 %d", cube.calls)
	}
	if rep.DataSource != string(SourceCube) {
		t.Errorf("data_source = %q，期望 cube", rep.DataSource)
	}
	// ⚠️ 正常路径不该有多余字段：
	//   wechat-bot 的卡片按固定字段解析，多出来的内容会进 diff。
	if rep.FallbackReason != "" {
		t.Errorf("没装配事实库不是回退，不该写 fallback_reason：%q", rep.FallbackReason)
	}
}

// TestCompareSummaryUsesAuthoritativeTotals 门店对比的汇总与单店报表同源。
//
// ⚠️ CompareData 原来用"按明细行求和"，于是同一家的
//
//	「对比列」与「单店报表」对不上数，而两边各自看起来都正常。
//	TestCompareData_SummaryMatchesReport 守这条不变量。
func TestCompareSummaryUsesAuthoritativeTotals(t *testing.T) {
	cube := &fakeCube{
		rows: map[string][]RawReport{DimCategory: {
			{Code: "a", Name: "A", NetAmount: 100, CostedRows: 1, TotalRows: 1},
		}},
		// 总额查询返回 13003.8，与明细行求和 100 差两个数量级。
		totals: &RawReport{NetAmount: 13003.8, TotalRows: 1, CostedRows: 1},
	}
	s := New(cube)
	out, err := s.CompareData(context.Background(), CompareInput{
		Dimension: DimCategory,
		Stores:    []string{"S001"},
		Rows:      []CompareRow{{Key: "r1", Label: "A", Items: map[string]string{"S001": "a"}}},
		Allowed:   map[string]bool{"S001": true},
		From:      "2026-10-16", To: "2026-10-16",
	})
	if err != nil {
		t.Fatalf("CompareData：%v", err)
	}
	got := out.Summary["S001"]
	if got.NetAmount != "13,003.80" {
		t.Errorf("对比列汇总 = %q，期望 13,003.80（独立总额查询），"+
			"若得到 100.00 说明退回按明细求和了", got.NetAmount)
	}
	// 同一口径下，单店报表必须是同一个数。
	rep, err := s.Report(context.Background(), Query{
		StoreBranchID: "S001", GroupBy: DimCategory,
		From: "2026-10-16", To: "2026-10-16",
	})
	if err != nil {
		t.Fatalf("Report：%v", err)
	}
	if rep.Summary.NetAmount != got.NetAmount {
		t.Errorf("同口径下对比列 %q 与单店报表 %q 不一致", got.NetAmount, rep.Summary.NetAmount)
	}
}
