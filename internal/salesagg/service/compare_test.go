package service

import (
	"context"
	"errors"
	"testing"
)

// perStoreCube 是 CompareData 专用的替身：按**门店**（而不是 groupBy）
// 存数据，并记录每次调用的门店 —— 用于断言 forbidden 列零 cube 调用。
// 与 handler 包的 cmpCube、service_test.go 的 fakeCube 各自独立：
// 三者的观测面不同（这里要 per-store 数据 + 调用序）。
type perStoreCube struct {
	calls     []string // 按序记录每次 LoadSalesReport 的门店
	lastGroup string
	rows      map[string][]RawReport
	errs      map[string]error
}

func (f *perStoreCube) LoadSalesReport(
	_ context.Context, storeBranchID, groupBy, _, _ string, _ int,
) (*RawSalesReport, error) {
	f.calls = append(f.calls, storeBranchID)
	f.lastGroup = groupBy
	if err, ok := f.errs[storeBranchID]; ok {
		return nil, err
	}
	return &RawSalesReport{Rows: f.rows[storeBranchID]}, nil
}

// LoadSalesTotals 满足 CubeQuerier。
//
// ⚠️ 这里**故意让总额等于按行求和**：对比功能的既有口径就是
// "从明细行汇总"，而门店对比读的是各店**同一次**结果，
// 不存在"分组行求和偏低"的问题（见 service.go 的 CubeQuerier 注释）。
// 改成独立查询会让本组既有断言全部失效且掩盖真正的问题。
func (f *perStoreCube) LoadSalesTotals(
	_ context.Context, storeBranchID, _, _ string,
) (RawReport, error) {
	if err, ok := f.errs[storeBranchID]; ok {
		return RawReport{}, err
	}
	var sum RawReport
	for _, r := range f.rows[storeBranchID] {
		sum.NetAmount += r.NetAmount
		sum.NetQty += r.NetQty
		sum.RefCost += r.RefCost
		sum.CostedRows += r.CostedRows
		sum.TotalRows += r.TotalRows
	}
	return sum, nil
}

// TestCompareData_ForbiddenNoCubeCall 无权限门店：整列 forbidden、
// summary / cells 无该店 key，且**一次 cube 调用都不发生**。
//
// 零调用是语义要求而不是性能优化：模板人人可读，取数逐店鉴权，
// 若无权限门店仍触发真实查询，等于"借别人的 token 查了别人的账"。
func TestCompareData_ForbiddenNoCubeCall(t *testing.T) {
	f := &perStoreCube{rows: map[string][]RawReport{
		"S001": {{Code: "01", Name: "生鲜", NetAmount: 100}},
	}}
	s := New(f)
	out, err := s.CompareData(context.Background(), CompareInput{
		Dimension: DimCategory,
		Stores:    []string{"S001", "ZZZZ"},
		Rows:      []CompareRow{{Key: "r1", Label: "猪肉", Items: map[string]string{"S001": "01"}}},
		Allowed:   map[string]bool{"S001": true, "S002": true},
		From:      "2026-10-17", To: "2026-10-17",
	})
	if err != nil {
		t.Fatalf("CompareData: %v", err)
	}
	if len(out.Columns) != 2 || out.Columns[0].Status != ColOK || out.Columns[1].Status != ColForbidden {
		t.Fatalf("columns 状态错误: %+v", out.Columns)
	}
	if len(f.calls) != 1 || f.calls[0] != "S001" {
		t.Fatalf("cube 调用 = %v, want 仅 [S001]", f.calls)
	}
	if _, ok := out.Summary["ZZZZ"]; ok {
		t.Fatal("forbidden 店不得出现在 summary")
	}
	for _, r := range out.Rows {
		if _, ok := r.Cells["ZZZZ"]; ok {
			t.Fatal("forbidden 店不得出现在 cells")
		}
	}
}

// TestCompareData_UnavailableColumn 有权限但数据源故障：只标记该列为
// unavailable + 机器码，其余列照常 —— 对比的意义就是"有多少看多少"。
func TestCompareData_UnavailableColumn(t *testing.T) {
	f := &perStoreCube{
		rows: map[string][]RawReport{"S002": {{Code: "01", Name: "生鲜", NetAmount: 50}}},
		errs: map[string]error{"S001": errors.New("dapr: connection refused")},
	}
	s := New(f)
	out, err := s.CompareData(context.Background(), CompareInput{
		Dimension: DimCategory,
		Stores:    []string{"S001", "S002"},
		Allowed:   map[string]bool{"S001": true, "S002": true},
		From:      "2026-10-17", To: "2026-10-17",
	})
	if err != nil {
		t.Fatalf("CompareData: %v", err)
	}
	if out.Columns[0].Status != ColUnavailable ||
		out.Columns[0].ErrorCode != ErrSalesSourceUnavailable {
		t.Fatalf("S001 列 = %+v, want unavailable/%s", out.Columns[0], ErrSalesSourceUnavailable)
	}
	if out.Columns[1].Status != ColOK {
		t.Fatalf("S002 列 = %+v, want ok", out.Columns[1])
	}
	if _, ok := out.Summary["S001"]; ok {
		t.Fatal("unavailable 店不得出现在 summary")
	}
}

// TestCompareData_CellMapping 单元格映射四分支：
// 命中（毛利按覆盖率口径填充）、编码未命中、未设置编码、编码带空白（trim）。
func TestCompareData_CellMapping(t *testing.T) {
	f := &perStoreCube{rows: map[string][]RawReport{
		"S001": {
			{Code: "01", Name: "生鲜", NetAmount: 100, NetQty: 10, RefCost: 40, CostedRows: 5, TotalRows: 5},
			// 覆盖率 0/10 = 0 < MinCostCoverage → margin 不可算。
			{Code: "02", Name: "日配", NetAmount: 30, NetQty: 3, RefCost: 10, CostedRows: 0, TotalRows: 10},
		},
	}}
	s := New(f)
	out, err := s.CompareData(context.Background(), CompareInput{
		Dimension: DimCategory,
		Stores:    []string{"S001", "S002"},
		Rows: []CompareRow{
			{Key: "r1", Label: "生鲜", Items: map[string]string{"S001": "01"}},
			{Key: "r2", Label: "查不到", Items: map[string]string{"S001": "ZZ"}},
			{Key: "r3", Label: "未设码"},
			{Key: "r4", Label: "覆盖不足", Items: map[string]string{"S001": "02"}},
			{Key: "r5", Label: "带空白", Items: map[string]string{"S001": " 01 "}},
		},
		Allowed: map[string]bool{"S001": true, "S002": true},
		From:    "2026-10-17", To: "2026-10-17",
	})
	if err != nil {
		t.Fatalf("CompareData: %v", err)
	}
	if len(out.Rows) != 5 {
		t.Fatalf("rows = %d, want 5", len(out.Rows))
	}
	// 命中：数字为服务端格式化字符串，毛利 100-40=60，60/100=60%。
	c := out.Rows[0].Cells["S001"]
	if c == nil || c.Code != "01" || c.Name != "生鲜" ||
		c.NetAmount != "100.00" || c.NetQty != "10.00" ||
		c.RefMargin != "60.00" || c.RefMarginPct != "60.0%" {
		t.Fatalf("r1 cell = %+v", out.Rows[0].Cells["S001"])
	}
	// 未命中 / 未设码 → nil（前端显示 --）。
	if out.Rows[1].Cells["S001"] != nil || out.Rows[2].Cells["S001"] != nil {
		t.Fatalf("r2/r3 cell 应为 nil: %+v / %+v", out.Rows[1].Cells["S001"], out.Rows[2].Cells["S001"])
	}
	// 覆盖率不足：金额照常，毛利为空串。
	low := out.Rows[3].Cells["S001"]
	if low == nil || low.NetAmount != "30.00" || low.RefMargin != "" || low.RefMarginPct != "" {
		t.Fatalf("r4 cell = %+v", low)
	}
	// 编码带空白：trim 后命中（与 r1 同一行数据）。
	if c5 := out.Rows[4].Cells["S001"]; c5 == nil || c5.Code != "01" {
		t.Fatalf("r5 cell = %+v, want 命中 01", c5)
	}
}

// TestCompareData_SummaryMatchesReport 对比列的 summary 必须与
// /reports/sales 的顶部汇总**同口径** —— 两个入口看到同一个门店的
// 同一天数据，数字对不上没人说得清哪份对。
func TestCompareData_SummaryMatchesReport(t *testing.T) {
	rows := []RawReport{
		{Code: "01", Name: "生鲜", NetAmount: 2977.40, NetQty: 315, RefCost: 1200, CostedRows: 16, TotalRows: 26},
		{Code: "02", Name: "日配", NetAmount: 18896.379, NetQty: 3573.851, RefCost: 8000, CostedRows: 100, TotalRows: 120},
	}

	// 报表路径：groupBy 维度的 fake。
	reportCube := &fakeCube{rows: map[string][]RawReport{DimCategory: rows}}
	rep, err := New(reportCube).Report(context.Background(), Query{
		StoreBranchID: "S001", GroupBy: DimCategory, From: "2026-10-17", To: "2026-10-17", Limit: 50,
	})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}

	// 对比路径：同一份数据。
	cmpCube := &perStoreCube{rows: map[string][]RawReport{"S001": rows}}
	out, err := New(cmpCube).CompareData(context.Background(), CompareInput{
		Dimension: DimCategory,
		Stores:    []string{"S001"},
		Allowed:   map[string]bool{"S001": true},
		From:      "2026-10-17", To: "2026-10-17",
	})
	if err != nil {
		t.Fatalf("CompareData: %v", err)
	}

	got := out.Summary["S001"]
	want := rep.Summary
	if got.NetAmount != want.NetAmount || got.NetQty != want.NetQty ||
		got.RefCost != want.RefCost || got.CostCoverage != want.CostCoverage ||
		got.MarginAvailable != want.MarginAvailable ||
		got.RefMargin != want.RefMargin || got.RefMarginPct != want.RefMarginPct {
		t.Fatalf("summary 口径漂移:\ncompare  = %+v\nreport   = %+v", got, want)
	}
}

// TestCompareData_InvalidDimension 维度在 service 层再拒一次
// （存储层白名单之外的第二道闸）。
func TestCompareData_InvalidDimension(t *testing.T) {
	s := New(&perStoreCube{})
	_, err := s.CompareData(context.Background(), CompareInput{
		Dimension: "brand",
		Stores:    []string{"A", "B"},
		Allowed:   map[string]bool{"A": true, "B": true},
	})
	if !errors.Is(err, ErrInvalidDimension) {
		t.Fatalf("err = %v, want ErrInvalidDimension", err)
	}
}
