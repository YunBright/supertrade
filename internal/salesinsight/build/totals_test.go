package build

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestTotals_大盘必须取自独立总额查询而非品类求和 是本次修复的判别力主用例。
//
// 线上实测（门店 00，2026-10-16）：品类口径 2024.6 元 vs 供应商口径 13003.8 元。
// 头条只统计到 15.6%，且零报错 —— LLM 只能把它写成一条
// "自营贡献高于当日总销售额"的软警告。
//
// 这里刻意让**总额 ≠ 品类之和**：若实现退回到 sumCategories，
// 断言立刻红。
func TestTotals_大盘必须取自独立总额查询而非品类求和(t *testing.T) {
	fs := newFake()
	// 品类（会被 topN 截断 / 丢弃空类目行）
	fs.catCur = []CategoryRow{
		{Name: "蛋类", Amount: 746.3, CostedRow: 100, TotalRow: 100},
		{Name: "干货/大米", Amount: 586.8, CostedRow: 100, TotalRow: 100},
		{Name: "调味类", Amount: 173, CostedRow: 100, TotalRow: 100},
	}
	// 真实大盘（供应商口径实测 13003.8）
	fs.hasTotals = true
	fs.totCur = TotalsRow{
		Amount:    13003.8,
		Qty:       1400,
		Cost:      10403.04,
		CostedRow: 1400,
		TotalRow:  1400,
	}
	fs.catPrev = []CategoryRow{{Name: "蛋类", Amount: 600}}
	fs.totPrev = TotalsRow{Amount: 11000, Qty: 1200, CostedRow: 1200, TotalRow: 1200}

	f, err := NewBuilder(fs, nil).Build(context.Background(), testOptions())
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}

	catSum := 746.3 + 586.8 + 173
	if f.Totals.NetAmount == catSum {
		t.Fatalf("大盘 = 品类之和 %.1f —— 说明仍在用 sumCategories，topN 截断与空类目丢行依然存在", catSum)
	}
	if d := f.Totals.NetAmount - 13003.8; d > 0.05 || d < -0.05 {
		t.Errorf("大盘 = %v, 期望 13003.8（独立总额查询的值）", f.Totals.NetAmount)
	}
	if f.Totals.NetQty != 1400 {
		t.Errorf("大盘销量 = %v, 期望 1400", f.Totals.NetQty)
	}
	// 毛利仍按同一套规则推算（覆盖率 100%）
	if !f.Totals.MarginAvailable {
		t.Error("覆盖率 100% 时毛利应可用")
	}
	if d := f.Totals.RefMarginPct - (13003.8-10403.04)/13003.8; d > 0.001 || d < -0.001 {
		t.Errorf("毛利率 = %v，期望约 %.4f", f.Totals.RefMarginPct, (13003.8-10403.04)/13003.8)
	}
}

// TestTotalsQueryHasNoDimensionNorLimit 是**结构断言**：
// 行为测试 fake 掉了 cube，因此"给总额查询加上 Dimensions"这种改动
// 不会被任何行为用例发现 —— 而它恰恰会把空类目丢行重新引进来。
//
// 本仓已有同类做法（cube 的 keysFromGoSource、deployer 的
// TestAuthkeyScopesCoverEveryDaprApp），这里沿用读源码断言的路子。
func TestTotalsQueryHasNoDimensionNorLimit(t *testing.T) {
	body := funcOf(t, "datasource.go", "func (s *DaprSource) TotalsSales(")
	q := queryLiteral(t, body, "LoadCubeQuery(")

	if strings.Contains(q, "Dimensions:") {
		t.Errorf("总额查询**不得**带 Dimensions：\n%s\n一旦按维度分组，空类目的行就会被丢掉，头条金额会再次少算。", q)
	}
	if strings.Contains(q, "Limit:") {
		t.Errorf("总额查询**不得**带 Limit：\n%s\n一旦限量，求和就不再是全量大盘。", q)
	}
	if !strings.Contains(q, "Measures:") {
		t.Errorf("总额查询必须带 Measures：\n%s", q)
	}
	if !strings.Contains(q, "dateFilters(from, to)") {
		t.Errorf("总额查询必须带日期过滤，否则会把全历史累加进来：\n%s", q)
	}
}

// funcOf 从源文件里截出以 sig 开头的那一个函数体。
func funcOf(t *testing.T, file, sig string) string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", file, err)
	}
	src := string(b)
	i := strings.Index(src, sig)
	if i < 0 {
		t.Fatalf("%s 里找不到 %q", file, sig)
	}
	rest := src[i:]
	depth, started := 0, false
	for j := 0; j < len(rest); j++ {
		switch rest[j] {
		case '{':
			depth++
			started = true
		case '}':
			depth--
			if started && depth == 0 {
				return rest[:j+1]
			}
		}
	}
	t.Fatalf("%s 里 %q 的函数体没有闭合", file, sig)
	return ""
}

// queryLiteral 截出 call(...) 的参数部分。
func queryLiteral(t *testing.T, body, call string) string {
	t.Helper()
	i := strings.Index(body, call)
	if i < 0 {
		t.Fatalf("函数体里找不到 %q", call)
	}
	rest := body[i+len(call):]
	depth := 1
	for j := 0; j < len(rest); j++ {
		switch rest[j] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return rest[:j]
			}
		}
	}
	t.Fatalf("%q 的参数没有闭合", call)
	return ""
}
