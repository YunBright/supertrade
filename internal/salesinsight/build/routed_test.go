package build

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/YunBright/supertrade/internal/salesdaily"
	"github.com/YunBright/supertrade/internal/salesinsight/fact"
	"github.com/YunBright/supertrade/internal/salesinsight/fact/supplier"
)

// fakeLoader 模拟 PG 侧的区间读取。
type fakeLoader struct {
	calls []string
	facts map[string]salesdaily.RangeFacts
	errs  map[string]error
}

func (f *fakeLoader) LoadRange(_ context.Context, branchID, from, to string) (salesdaily.RangeFacts, error) {
	key := from + "~" + to
	f.calls = append(f.calls, key)
	if err, ok := f.errs[key]; ok {
		return f.facts[key], err
	}
	fc, ok := f.facts[key]
	if !ok {
		return salesdaily.RangeFacts{}, errors.New("PG 里没有这个区间")
	}
	return fc, nil
}

// countingCube 记录每个方法被调用了几次、带了什么区间。
type countingCube struct {
	calls []string
	// errByMethod 可按方法注入错误。
	errByMethod map[string]error
	// 名字维度：只有 SupplierDict 返回名字，模拟"cube 侧查不到名字"。
	dictNames map[string]string
}

func (c *countingCube) note(m string) error {
	c.calls = append(c.calls, m)
	return c.errByMethod[m]
}

func (c *countingCube) TotalsSales(_ context.Context, _, _, _ string) (TotalsRow, error) {
	if err := c.note("totals"); err != nil {
		return TotalsRow{}, err
	}
	return TotalsRow{Amount: 999}, nil
}

func (c *countingCube) CategorySales(_ context.Context, _, _, _ string, _ int) ([]CategoryRow, error) {
	if err := c.note("category"); err != nil {
		return nil, err
	}
	return []CategoryRow{{Name: "cube品类", Amount: 999}}, nil
}

func (c *countingCube) ProductSales(_ context.Context, _, _, _ string, _ int) ([]ProductRow, error) {
	if err := c.note("product"); err != nil {
		return nil, err
	}
	return []ProductRow{{ItemNo: "CUBE", Name: "cube商品", Amount: 999}}, nil
}

func (c *countingCube) SupplierSales(_ context.Context, _, _, _ string, _ int) ([]SupplierSalesRow, error) {
	if err := c.note("supplier"); err != nil {
		return nil, err
	}
	return []SupplierSalesRow{{Code: "CUBE", Name: "", Amount: 999}}, nil
}

func (c *countingCube) SupplierDict(_ context.Context, _ string, _ []string) ([]supplier.Record, error) {
	if err := c.note("dict"); err != nil {
		return nil, err
	}
	return nil, nil
}

func (c *countingCube) StockByItems(_ context.Context, _ string, _ []string) (map[string]fact.StockRecord, error) {
	if err := c.note("stock"); err != nil {
		return nil, err
	}
	return map[string]fact.StockRecord{}, nil
}

func countOf(calls []string, want string) int {
	n := 0
	for _, c := range calls {
		if c == want {
			n++
		}
	}
	return n
}

// pgFacts 造一份"PG 覆盖完整"的区间事实。
func pgFacts(from, to string) salesdaily.RangeFacts {
	return salesdaily.RangeFacts{
		BranchID: "00", From: from, To: to,
		Totals:      salesdaily.TotalsRow{NetAmount: 500, NetQty: 50},
		Categories:  []salesdaily.CategoryRow{{Name: "PG品类", Amount: 300}, {Name: "PG品类2", Amount: 200}},
		Products:    []salesdaily.ProductRow{{ItemNo: "PG1", Name: "pg商品", Category: "PG品类", Amount: 300}},
		Suppliers:   []salesdaily.SupplierRow{{Code: "S1", Name: "南宁蔬菜行", Amount: 400, ItemCount: 5}},
		CoveredDays: 7, TotalDays: 7,
	}
}

// TestRoutedUsesPGForWholeRangeWhenCovered ⭐ 本路由最重要的一条。
//
// PG 覆盖完整时，同一个区间的**四个维度**必须全部来自 PG，
// 且 **cube 一次都不能被调用**。
//
// ⚠️ 为什么断言"cube 调用次数为 0"而不是"结果来自 PG"：
//
//	逐维度断言只能证明"这一维走了 PG"，而本 bug 的形态是
//	"品类走了 PG、供应商走了 cube" —— 每维单看都对，
//	合起来才是那份 2024.6 vs 13003.8。
//	"cube 调用 0 次"是唯一能同时锁住四维的判据。
func TestRoutedUsesPGForWholeRangeWhenCovered(t *testing.T) {
	loader := &fakeLoader{facts: map[string]salesdaily.RangeFacts{
		"2026-10-10~2026-10-16": pgFacts("2026-10-10", "2026-10-16"),
	}}
	cube := &countingCube{}
	s := newRoutedSource(loader, cube)
	ctx := context.Background()

	tot, err := s.TotalsSales(ctx, "00", "2026-10-10", "2026-10-16")
	if err != nil {
		t.Fatalf("totals：%v", err)
	}
	if tot.Amount != 500 {
		t.Errorf("总额 = %v，期望 500（PG 的数）", tot.Amount)
	}
	cats, _ := s.CategorySales(ctx, "00", "2026-10-10", "2026-10-16", 10)
	if len(cats) != 2 || cats[0].Name != "PG品类" {
		t.Errorf("品类 = %+v，期望来自 PG", cats)
	}
	prods, _ := s.ProductSales(ctx, "00", "2026-10-10", "2026-10-16", 10)
	if len(prods) != 1 || prods[0].ItemNo != "PG1" {
		t.Errorf("商品 = %+v，期望来自 PG", prods)
	}
	sups, _ := s.SupplierSales(ctx, "00", "2026-10-10", "2026-10-16", 10)
	if len(sups) != 1 || sups[0].Code != "S1" {
		t.Errorf("供应商 = %+v，期望来自 PG", sups)
	}
	if len(cube.calls) != 0 {
		t.Fatalf("PG 覆盖完整时不该碰 cube，实际调用了 %v", cube.calls)
	}
}

// TestRoutedFallsBackForWholeRangeWhenIncomplete ⭐ 与上一条成对。
//
// PG 只覆盖了部分天时，必须**整段**回退：四个维度全部查 cube，
// 且已聚合的那部分 PG 数据**不能**被拿来用。
//
// ⚠️ "部分天用 PG、缺的查 cube"在数据量上看着最优，
//
//	但它让同一份报告里的"总额"（全段 cube）与"品类合计"（部分 PG）
//	来自两套口径 —— 正是 2026-10-16 那个 6 倍差的复现。
func TestRoutedFallsBackForWholeRangeWhenIncomplete(t *testing.T) {
	loader := &fakeLoader{
		facts: map[string]salesdaily.RangeFacts{
			"2026-10-10~2026-10-16": {
				BranchID: "00", From: "2026-10-10", To: "2026-10-16",
				Totals:      salesdaily.TotalsRow{NetAmount: 999}, // 诱人的"部分数据"
				CoveredDays: 5, TotalDays: 7,
			},
		},
		errs: map[string]error{
			"2026-10-10~2026-10-16": errors.New(
				"salesdaily: 区间内未覆盖全部营业日: 缺 2 天"),
		},
	}
	// 让 errors.Is 成立，模拟真实的 ErrRangeIncomplete。
	loader.errs["2026-10-10~2026-10-16"] = wrapIncomplete(loader.errs["2026-10-10~2026-10-16"])

	cube := &countingCube{}
	s := newRoutedSource(loader, cube)
	ctx := context.Background()

	tot, err := s.TotalsSales(ctx, "00", "2026-10-10", "2026-10-16")
	if err != nil {
		t.Fatalf("totals：%v", err)
	}
	if tot.Amount != 999 {
		t.Errorf("总额 = %v，期望 999 —— **必须**是 cube 的数，不能用 PG 的部分数据", tot.Amount)
	}
	cats, _ := s.CategorySales(ctx, "00", "2026-10-10", "2026-10-16", 10)
	if len(cats) != 1 || cats[0].Name != "cube品类" {
		t.Errorf("品类 = %+v，期望来自 cube（PG 的部分品类不能混进来）", cats)
	}
	prods, _ := s.ProductSales(ctx, "00", "2026-10-10", "2026-10-16", 10)
	if len(prods) != 1 || prods[0].ItemNo != "CUBE" {
		t.Errorf("商品 = %+v，期望来自 cube", prods)
	}
	sups, _ := s.SupplierSales(ctx, "00", "2026-10-10", "2026-10-16", 10)
	if len(sups) != 1 || sups[0].Code != "CUBE" {
		t.Errorf("供应商 = %+v，期望来自 cube", sups)
	}
	for _, m := range []string{"category", "product", "supplier"} {
		if countOf(cube.calls, m) != 1 {
			t.Errorf("%s 应查 cube 一次，实际调用记录 %v", m, cube.calls)
		}
	}
	lines := s.reportLines()
	if len(lines) != 1 || !strings.Contains(lines[0], "5/7") {
		t.Errorf("回退说明应报出覆盖天数 5/7，实际 %v", lines)
	}
}

// TestRoutedDecisionIsCachedPerRange 同一区间只读一次 PG。
//
// ⚠️ 不缓存的话每份报告要多打一次 PG 全区间聚合
//
//	（7 条 GROUP BY），而日报周报月报一天要出好几份。
func TestRoutedDecisionIsCachedPerRange(t *testing.T) {
	loader := &fakeLoader{facts: map[string]salesdaily.RangeFacts{
		"2026-10-10~2026-10-16": pgFacts("2026-10-10", "2026-10-16"),
	}}
	cube := &countingCube{}
	s := newRoutedSource(loader, cube)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		_, _ = s.CategorySales(ctx, "00", "2026-10-10", "2026-10-16", 10)
	}
	if len(loader.calls) != 1 {
		t.Fatalf("PG 被读了 %d 次，期望 1 次（决策必须缓存）", len(loader.calls))
	}
}

// TestRoutedDecidesPeriodsIndependently 上期与本期各自选路。
//
// 本期走 PG、上期走 cube 是**允许**的：两者各自内部自洽，
// 环比比较的是两个完整口径。危险的是"同一区间内部混"。
func TestRoutedDecidesPeriodsIndependently(t *testing.T) {
	loader := &fakeLoader{facts: map[string]salesdaily.RangeFacts{
		"2026-10-10~2026-10-16": pgFacts("2026-10-10", "2026-10-16"),
		// 上期不在 facts 里 ⇒ LoadRange 报错 ⇒ 回退
	}}
	cube := &countingCube{}
	s := newRoutedSource(loader, cube)
	ctx := context.Background()

	cur, _ := s.TotalsSales(ctx, "00", "2026-10-10", "2026-10-16")
	prev, _ := s.TotalsSales(ctx, "00", "2026-10-03", "2026-10-09")
	if cur.Amount != 500 {
		t.Errorf("本期 = %v，期望 500（PG）", cur.Amount)
	}
	if prev.Amount != 999 {
		t.Errorf("上期 = %v，期望 999（cube）", prev.Amount)
	}
	if countOf(cube.calls, "totals") != 1 {
		t.Errorf("只有上期该走 cube，实际 %v", cube.calls)
	}
}

// TestRoutedWithoutLoaderGoesStraightToCube 未装配 PG 时行为与改造前一致。
func TestRoutedWithoutLoaderGoesStraightToCube(t *testing.T) {
	cube := &countingCube{}
	s := newRoutedSource(nil, cube)
	ctx := context.Background()

	if _, err := s.TotalsSales(ctx, "00", "2026-10-10", "2026-10-16"); err != nil {
		t.Fatalf("totals：%v", err)
	}
	if len(cube.calls) != 1 || cube.calls[0] != "totals" {
		t.Errorf("未装配时该直接查 cube，实际 %v", cube.calls)
	}
	if len(s.reportLines()) != 0 {
		t.Errorf("未装配 PG 不该产生回退说明，实际 %v", s.reportLines())
	}
}

// TestBuilderRouteIsIdentityWithoutLoader Builder 未装配时不包装。
//
// ⚠️ 这条守的是"改造没改变既有行为"：装配/未装配两条路
//
//	必须在**没有任何 loader** 时逐字等价，
//	否则每次改路由都在赌线上那套没回归。
func TestBuilderRouteIsIdentityWithoutLoader(t *testing.T) {
	cube := &countingCube{}
	b := NewBuilder(cube, nil)
	if got := b.route(); got != DataSource(cube) {
		t.Error("未装配 loader 时 route() 应原样返回数据源")
	}
}

// TestSupplierNamePrefersSourceAndFallsBackToDict ⭐ 供应商名称的两条来源。
//
//	PG 路径自带名称；cube 路径没有（live↔duck 不能 join），
//	必须靠字典兜底。两条都要有判别力 ——
//	只测一条的话，"PG 路径把名称清空了"这种缺陷会安静地活着。
func TestSupplierNamePrefersSourceAndFallsBackToDict(t *testing.T) {
	// 1) PG 路径：名称直接来自快照。
	loader := &fakeLoader{facts: map[string]salesdaily.RangeFacts{
		"2026-10-10~2026-10-16": pgFacts("2026-10-10", "2026-10-16"),
	}}
	s := newRoutedSource(loader, &countingCube{})
	sups, _ := s.SupplierSales(context.Background(), "00", "2026-10-10", "2026-10-16", 10)
	if sups[0].Name != "南宁蔬菜行" {
		t.Errorf("PG 路径的供应商名称 = %q，期望 南宁蔬菜行", sups[0].Name)
	}

	// 2) cube 路径：名称为空，由 buildSuppliers 用字典/编码兜底。
	c2 := &countingCube{}
	s2 := newRoutedSource(nil, c2)
	sups2, _ := s2.SupplierSales(context.Background(), "00", "2026-10-10", "2026-10-16", 10)
	if sups2[0].Name != "" {
		t.Errorf("cube 路径不该凭空造出名称，实际 %q", sups2[0].Name)
	}
}

// TestRoutedAppliesTopNOnlyAfterRouting topN 必须在选路**之后**才生效。
//
// ⚠️ 反过来（先截断再选路）会让回退路径拿到的是"被 PG 截过的数据"。
//
//	更隐蔽的一种是"PG 路径把 topN 传给 LoadRange" ——
//	那会让月报的品类聚合只包含 topN 个类别。
func TestRoutedAppliesTopNOnlyAfterRouting(t *testing.T) {
	facts := pgFacts("2026-10-10", "2026-10-16")
	facts.Categories = []salesdaily.CategoryRow{
		{Name: "C1", Amount: 500}, {Name: "C2", Amount: 400},
		{Name: "C3", Amount: 300}, {Name: "C4", Amount: 200},
	}
	loader := &fakeLoader{facts: map[string]salesdaily.RangeFacts{
		"2026-10-10~2026-10-16": facts,
	}}
	s := newRoutedSource(loader, &countingCube{})
	cats, _ := s.CategorySales(context.Background(), "00", "2026-10-10", "2026-10-16", 2)
	if len(cats) != 2 {
		t.Fatalf("topN=2 却返回 %d 条", len(cats))
	}
	if cats[0].Name != "C1" {
		t.Errorf("第一条 = %s，期望 C1（按金额降序取前 2）", cats[0].Name)
	}
}

// wrapIncomplete 让一个普通 error 能被 errors.Is 认成 ErrRangeIncomplete。
func wrapIncomplete(err error) error {
	return &incompleteErr{err}
}

type incompleteErr struct{ inner error }

func (e *incompleteErr) Error() string { return e.inner.Error() }
func (e *incompleteErr) Unwrap() error { return e.inner }
func (e *incompleteErr) Is(target error) bool {
	return target == salesdaily.ErrRangeIncomplete
}
