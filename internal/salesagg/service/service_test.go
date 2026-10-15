package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeCube 是 CubeQuerier 的测试替身。
//
// 它按 groupBy 返回**不同的数字**，而不是恒定值 —— 否则"维度选错了"
// 这类缺陷根本不可观测（三个维度返回同一份数据时，断言恒真）。
// 这与本仓对"按身份造不同数据"的既有要求同源。
type fakeCube struct {
	lastStoreBranchID string
	lastGroupBy       string
	lastFrom          string
	lastTo            string
	lastLimit         int
	calls             int
	rows              map[string][]RawReport
	err               error
}

func (f *fakeCube) LoadSalesReport(
	_ context.Context, storeBranchID, groupBy, from, to string, limit int,
) (*RawSalesReport, error) {
	f.calls++
	f.lastStoreBranchID, f.lastGroupBy = storeBranchID, groupBy
	f.lastFrom, f.lastTo, f.lastLimit = from, to, limit
	if f.err != nil {
		return nil, f.err
	}
	return &RawSalesReport{Rows: f.rows[groupBy]}, nil
}

func TestValidDimension(t *testing.T) {
	for _, d := range []string{DimCategory, DimProduct} {
		if !ValidDimension(d) {
			t.Errorf("ValidDimension(%q) = false, want true", d)
		}
	}
	// ⭐ 数据源的门店（branch_no = 门店区域，X-Branch-ID 的下一层）**已移除**。
	//
	// 业务明确要求「忽略数据源的门店」。它仍留在 cube plugin 里，
	// 但报表维度不暴露 —— 断言的作用是：将来有人"顺手加回来"时立刻变红。
	//
	// 这个断言之所以关键：`ValidDimension` 是 `BuildSalesReportQuery` 与
	// handler 400 映射的**共同**判据，只在某一层测就可能漏掉另一层。
	for _, d := range []string{DimBranchRemoved, "BRANCH", "Store", "store"} {
		if ValidDimension(d) {
			t.Errorf("ValidDimension(%q) = true, want false（数据源门店区域已按业务要求移除）", d)
		}
	}
	// 品牌维度已被业务砍掉（实测按品牌聚合当天只返回 1 组）。
	// 这个断言的作用是：将来有人"顺手加回来"时会立刻变红。
	for _, d := range []string{"brand", ""} {
		if ValidDimension(d) {
			t.Errorf("ValidDimension(%q) = true, want false", d)
		}
	}
}

// TestBuildSalesReportQueryRejectsBranchDimension 移除 branch 维度必须是
// 行为上的拒绝，而不只是"列表里没有"。
//
// 只断言 `ValidDimension` 是不够的：`dimSpecs` 与 `ValidDimension` 是两张
// 互不相干的表，理论上可以只删掉其中一张 —— 那样 `ValidDimension` 通过，
// 而 `BuildSalesReportQuery` 仍然会为 "branch" 造出一条 query 发给 cube，
// 用户就在报表里看到了本该忽略的门店区域维度。
func TestBuildSalesReportQueryRejectsBranchDimension(t *testing.T) {
	if _, err := BuildSalesReportQuery(DimBranchRemoved, "2026-10-01", "2026-10-14", 50); !errors.Is(err, ErrInvalidDimension) {
		t.Fatalf("BuildSalesReportQuery(%q) err = %v, want ErrInvalidDimension", DimBranchRemoved, err)
	}
	// 反向确认：支持的维度仍能造出 query（防止"全拒"式实现也能过上面那条）。
	if _, err := BuildSalesReportQuery(DimCategory, "2026-10-01", "2026-10-14", 50); err != nil {
		t.Fatalf("BuildSalesReportQuery(%q): %v", DimCategory, err)
	}
}

// TestCategoryDimensionUsesParent 类别维度必须按 **父类别** 聚合。
//
// ✅ 实测 hbposepro 2026-10-14 与既有报表系统逐分对账：
//
//	按 cls_parent → 生鲜联营 4994.70 / 百货联营 2340.70，与对方一致；
//	按末级 item_clsname → 「佳乐超市猪肉部」这类商品名，完全是另一套。
//
// 同时锁住「不 select 末级类别」这条：cube 的 dimensions 全部进 GROUP BY，
// 多选一个 item_clsname 会把一个父类别裂成十几行，汇总数直接失真。
func TestCategoryDimensionUsesParent(t *testing.T) {
	q, err := BuildSalesReportQuery(DimCategory, "2026-10-01", "2026-10-10", 100)
	if err != nil {
		t.Fatalf("BuildSalesReportQuery: %v", err)
	}
	// ⚠️ 类别不在 sales_report 上 —— 它挂在 join 链末端的 item_cls_parent
	//   model 上(sales_report → item → item_cls → item_cls_parent)。
	//   成员地址必须带 model 前缀,写错前缀的症状不是报错而是**列全空**。
	wantCode := "item_cls_parent.id"
	wantName := "item_cls_parent.name"
	if len(q.Dimensions) != 2 {
		t.Fatalf("category dimensions = %v, want 恰好 2 个(code + name)", q.Dimensions)
	}
	if q.Dimensions[0] != wantCode || q.Dimensions[1] != wantName {
		t.Errorf("category dimensions = %v, want [%s %s]", q.Dimensions, wantCode, wantName)
	}
	// 末级类别在 item_cls 这个 model 上(不是 sales_report 上)，
	// 所以守卫也得按**真实地址**写,否则这条断言守的是一个不存在的成员。
	for _, leaf := range []string{"item_cls.name", "item_cls.id", "item_cls.parent_no"} {
		for _, d := range q.Dimensions {
			if d == leaf {
				t.Errorf("类别维度选了末级类别 %q —— 它会进 GROUP BY 并把父类别裂开,"+
					"汇总数失真(实测「生鲜联营」会裂成十几个末级行)", d)
			}
		}
	}
	// 反向断言：末级类别的 parent_no 是**成员名**,不是可以直接拿来当类别用的地址。
	// 拼错前缀(cube 会照收)的后果是该列恒为空,而恒空的维度不会被
	// "查不到"暴露,只会让父类别聚合成一坨 unnamed 的行。
	for _, d := range q.Dimensions {
		if d == "sales_report.item_cls_parent" || d == "sales_report.item_cls_parent_name" {
			t.Errorf("类别维度用了 sales_report 前缀 %q —— 该成员属于 item_cls_parent,"+
				"用 sales_report. 前缀查询不会报错,只会让该列全空", d)
		}
	}
}

func TestReportRejectsUnknownDimension(t *testing.T) {
	svc := New(&fakeCube{})
	_, err := svc.Report(context.Background(), Query{GroupBy: "brand"})
	if !errors.Is(err, ErrInvalidDimension) {
		t.Fatalf("err = %v, want ErrInvalidDimension", err)
	}
}

// TestReportHidesMarginWhenCoverageLow 是本包最重要的一条用例。
//
// ✅ 实测事实：base_price1 覆盖率极低（当日 25.3%），多个类别的
// coverage 为 0.0%。若直接用 cost=0 算 margin，会显示「毛利率 100%」
// —— 一个看起来精确、实际毫无意义的假数，比不显示更坏。
func TestReportHidesMarginWhenCoverageLow(t *testing.T) {
	f := &fakeCube{rows: map[string][]RawReport{
		DimCategory: {
			// 成本全覆盖 → 毛利可展示
			{Code: "010301", Name: "牛肉", NetAmount: 453.80, NetQty: 5.04,
				RefCost: 423.56, CostedRows: 1, TotalRows: 1},
			// 零覆盖 → 毛利必须留空（否则会显示 100%）
			{Code: "020801", Name: "大米", NetAmount: 396.00, NetQty: 22.50,
				RefCost: 0, CostedRows: 0, TotalRows: 10},
		},
	}}
	svc := New(f)
	rep, err := svc.Report(context.Background(), Query{GroupBy: DimCategory})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(rep.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rep.Rows))
	}
	byName := map[string]ReportRow{}
	for _, r := range rep.Rows {
		byName[r.Name] = r
	}

	beef := byName["牛肉"]
	if beef.RefMargin == "" {
		t.Error("牛肉 coverage=100% 但 ref_margin 为空，应展示")
	}
	if beef.RefMarginPct == "" {
		t.Error("牛肉 coverage=100% 但 ref_margin_pct 为空，应展示")
	}
	// 453.80 - 423.56 = 30.24
	if beef.RefMargin != "30.24" {
		t.Errorf("牛肉 ref_margin = %q, want 30.24", beef.RefMargin)
	}

	rice := byName["大米"]
	if rice.RefMargin != "" {
		t.Errorf("大米 coverage=0 但 ref_margin = %q, want 空（否则显示 100%%）", rice.RefMargin)
	}
	if rice.RefMarginPct != "" {
		t.Errorf("大米 coverage=0 但 ref_margin_pct = %q, want 空", rice.RefMarginPct)
	}
	// 覆盖率字段必须仍如实返回，供调用方自己决定
	if rice.CostCoverage != "0.0%" {
		t.Errorf("大米 cost_coverage = %q, want 0.0%%", rice.CostCoverage)
	}
	if rice.TotalRows != 10 || rice.CostedRows != 0 {
		t.Errorf("大米 costed/total = %d/%d, want 0/10", rice.CostedRows, rice.TotalRows)
	}
	// 金额列无论如何都要有 —— 卡片与报表的主指标是它
	if rice.NetAmount != "396.00" {
		t.Errorf("大米 net_amount = %q, want 396.00", rice.NetAmount)
	}
}

// TestReportSummaryMarginUnavailable 当整体覆盖率不足时，汇总毛利也必须留空。
func TestReportSummaryMarginUnavailable(t *testing.T) {
	f := &fakeCube{rows: map[string][]RawReport{
		DimCategory: {
			{Code: "a", Name: "A", NetAmount: 100, RefCost: 0, CostedRows: 0, TotalRows: 10},
			{Code: "b", Name: "B", NetAmount: 200, RefCost: 0, CostedRows: 0, TotalRows: 10},
		},
	}}
	rep, err := New(f).Report(context.Background(), Query{GroupBy: DimCategory})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if rep.Summary.MarginAvailable {
		t.Error("summary margin_available = true，但 coverage = 0，应为 false")
	}
	if rep.Summary.RefMargin != "" || rep.Summary.RefMarginPct != "" {
		t.Errorf("summary margin = %q / %q, want 空", rep.Summary.RefMargin, rep.Summary.RefMarginPct)
	}
	// 但金额与数量必须照常返回
	if rep.Summary.NetAmount != "300.00" {
		t.Errorf("summary net_amount = %q, want 300.00", rep.Summary.NetAmount)
	}
	// 汇总覆盖率 = 0/20 = 0.0%
	if rep.Summary.CostCoverage != "0.0%" {
		t.Errorf("summary cost_coverage = %q, want 0.0%%", rep.Summary.CostCoverage)
	}
}

// TestReportSortsByAmountDesc 报表打开时用户先看销售额最高的行。
func TestReportSortsByAmountDesc(t *testing.T) {
	f := &fakeCube{rows: map[string][]RawReport{
		DimCategory: {
			{Code: "a", Name: "A", NetAmount: 10, CostedRows: 1, TotalRows: 1},
			{Code: "c", Name: "C", NetAmount: 3000, CostedRows: 1, TotalRows: 1},
			{Code: "b", Name: "B", NetAmount: 200, CostedRows: 1, TotalRows: 1},
		},
	}}
	rep, err := New(f).Report(context.Background(), Query{GroupBy: DimCategory})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	want := []string{"c", "b", "a"}
	for i, w := range want {
		if rep.Rows[i].Code != w {
			t.Fatalf("rows[%d] = %q, want %q (full order %v)", i, rep.Rows[i].Code, w, codes(rep.Rows))
		}
	}
}

// TestReportFormatsThousands 金额必须带千分位且两位小数。
func TestReportFormatsThousands(t *testing.T) {
	f := &fakeCube{rows: map[string][]RawReport{
		DimCategory: {{Code: "a", Name: "A", NetAmount: 18896.379, NetQty: 3573.851}},
	}}
	rep, err := New(f).Report(context.Background(), Query{GroupBy: DimCategory})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if got := rep.Rows[0].NetAmount; got != "18,896.38" {
		t.Errorf("net_amount = %q, want 18,896.38", got)
	}
	if got := rep.Rows[0].NetQty; got != "3,573.85" {
		t.Errorf("net_qty = %q, want 3,573.85", got)
	}
}

// TestReportPassesStoreToCube 门店必须透传给 cube，否则会拿到别的门店数据。
//
// ⚠️ 透传的**唯一**用途是 X-Branch-ID header（由 cubeclient.WithBranchID
// 放进 outgoing gRPC metadata），它**不是**源库 branch_no 过滤条件。
// 本测试只锁住"传了"，"没有变成 WHERE"由 query_test 侧的 filters 断言负责
// （BuildSalesReportQuery 的签名里根本没有 branch 参数，结构上不可能加进去）。
func TestReportPassesStoreToCube(t *testing.T) {
	f := &fakeCube{}
	_, err := New(f).Report(context.Background(), Query{
		StoreBranchID: "00", GroupBy: DimCategory, From: "2026-10-01", To: "2026-10-14", Limit: 50,
	})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if f.lastStoreBranchID != "00" {
		t.Errorf("storeBranchID = %q, want 00", f.lastStoreBranchID)
	}
	if f.lastFrom != "2026-10-01" || f.lastTo != "2026-10-14" {
		t.Errorf("range = %q..%q, want 2026-10-01..2026-10-14", f.lastFrom, f.lastTo)
	}
	if f.lastLimit != 50 {
		t.Errorf("limit = %d, want 50", f.lastLimit)
	}
}

// TestReportPropagatesCubeError cube 挂了必须上抛，不能变成"空报表"。
func TestReportPropagatesCubeError(t *testing.T) {
	sentinel := errors.New("boom")
	f := &fakeCube{err: sentinel}
	_, err := New(f).Report(context.Background(), Query{GroupBy: DimCategory})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

// TestTodayByCategory 每日推送的口径：只到今天、只按类别、不限行数。
func TestTodayByCategory(t *testing.T) {
	f := &fakeCube{rows: map[string][]RawReport{
		DimCategory: {{Code: "01", Name: "生鲜", NetAmount: 2977.40, CostedRows: 16, TotalRows: 26}},
	}}
	svc := New(f)
	// 通过 now 注入固定时钟，避免测试在跨天边界随机失败。
	fixed := time.Date(2026, 10, 14, 10, 45, 0, 0, time.Local)
	svc.now = func() time.Time { return fixed }

	rep, err := svc.TodayByCategory(context.Background(), "00", "")
	if err != nil {
		t.Fatalf("TodayByCategory: %v", err)
	}
	if f.lastGroupBy != DimCategory {
		t.Errorf("groupBy = %q, want %q", f.lastGroupBy, DimCategory)
	}
	if f.lastFrom != "2026-10-14" || f.lastTo != "2026-10-14" {
		t.Errorf("range = %q..%q, want 2026-10-14..2026-10-14", f.lastFrom, f.lastTo)
	}
	if f.lastLimit != 0 {
		t.Errorf("limit = %d, want 0（不限）", f.lastLimit)
	}
	if f.lastStoreBranchID != "00" {
		t.Errorf("storeBranchID = %q, want 00", f.lastStoreBranchID)
	}
	if len(rep.Rows) != 1 || rep.Rows[0].NetAmount != "2,977.40" {
		t.Errorf("rows = %+v", rep.Rows)
	}
}

func TestResolveRange(t *testing.T) {
	// 2026-10-14 是星期三
	now := time.Date(2026, 10, 14, 10, 45, 0, 0, time.Local)
	cases := []struct {
		preset   Preset
		from, to string
	}{
		{PresetToday, "2026-10-14", "2026-10-14"},
		{PresetYesterday, "2026-10-13", "2026-10-13"},
		// 周三往前推 2 天 = 周一 2026-10-12
		{PresetWeek, "2026-10-12", "2026-10-14"},
		{PresetMonth, "2026-10-01", "2026-10-14"},
	}
	for _, c := range cases {
		from, to := ResolveRange(c.preset, now)
		if from != c.from || to != c.to {
			t.Errorf("ResolveRange(%q) = %q..%q, want %q..%q", c.preset, from, to, c.from, c.to)
		}
	}
}

// TestResolveRangeUnknownPresetFallsBackToToday 未知预设不能返回空区间
// ——那会让查询变成"全历史"，而报表要的是今天。
func TestResolveRangeUnknownPresetFallsBackToToday(t *testing.T) {
	now := time.Date(2026, 10, 14, 23, 59, 0, 0, time.Local)
	from, to := ResolveRange(Preset("garbage"), now)
	if from != "2026-10-14" || to != "2026-10-14" {
		t.Errorf("range = %q..%q, want 2026-10-14..2026-10-14", from, to)
	}
}

// TestResolveRangeWeekStartsMonday 周一是一周之始（Go 里 Sunday=0，要换算）。
func TestResolveRangeWeekStartsMonday(t *testing.T) {
	monday := time.Date(2026, 10, 12, 8, 0, 0, 0, time.Local) // 周一
	from, _ := ResolveRange(PresetWeek, monday)
	if from != "2026-10-12" {
		t.Errorf("周一 from = %q, want 2026-10-12", from)
	}
	sunday := time.Date(2026, 10, 18, 8, 0, 0, 0, time.Local) // 周日
	from2, _ := ResolveRange(PresetWeek, sunday)
	if from2 != "2026-10-12" {
		t.Errorf("周日 from = %q, want 2026-10-12（周日应归属上一个周一）", from2)
	}
}

func TestGroupThousands(t *testing.T) {
	cases := []struct{ in, want string }{
		{"0.00", "0.00"},
		{"999.99", "999.99"},
		{"1000.00", "1,000.00"},
		{"18896.38", "18,896.38"},
		{"3573.85", "3,573.85"},
		{"1234567.89", "1,234,567.89"},
		{"-18896.38", "-18,896.38"},
	}
	for _, c := range cases {
		if got := groupThousands(c.in); got != c.want {
			t.Errorf("groupThousands(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestNumOfRoundTrip fmtAmount 的输出必须能被 numOf 解析回来（排序依赖它）。
func TestNumOfRoundTrip(t *testing.T) {
	if got := numOf("18,896.38"); got < 18896.37 || got > 18896.39 {
		t.Errorf("numOf(18,896.38) = %v, want ~18896.38", got)
	}
	if got := numOf("garbage"); got != 0 {
		t.Errorf("numOf(garbage) = %v, want 0（解析失败不该让查询失败）", got)
	}
}

func TestCoverageZeroTotal(t *testing.T) {
	if got := coverage(0, 0); got != 0 {
		t.Errorf("coverage(0,0) = %v, want 0", got)
	}
	if got := coverage(1, 4); got != 0.25 {
		t.Errorf("coverage(1,4) = %v, want 0.25", got)
	}
}

func codes(rows []ReportRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Code)
	}
	return out
}
