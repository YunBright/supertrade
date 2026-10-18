package collect

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/salesdaily"
)

// ---- fakes ----

// fakeSource 逐方法可注入错误。
//
// ⚠️ 用**每方法独立开关**而不是一个全局 fail：
//
//	采集策略（任一维失败就整次不落库）的判别力**全在"是哪一维失败"**。
//	一个全局开关只能测出"失败会不落库"，测不出
//	"品类失败但商品成功时会不会只落一半" —— 而那正是本包最贵的错。
type fakeSource struct {
	calls []string
	// ranges 记录每次调用的 (from, to)，用于断言"只采一天"。
	ranges [][2]string

	totalsErr   error
	categoryErr error
	productErr  error
	supplierErr error
	namesErr    error

	names map[string]string
	// namesAsked 记录字典查询带了哪些编码。
	namesAsked []string
}

func (f *fakeSource) note(method, from, to string) {
	f.calls = append(f.calls, method)
	f.ranges = append(f.ranges, [2]string{from, to})
}

func (f *fakeSource) TotalsSales(_ context.Context, _, from, to string) (salesdaily.TotalsRow, error) {
	f.note("totals", from, to)
	if f.totalsErr != nil {
		return salesdaily.TotalsRow{}, f.totalsErr
	}
	return salesdaily.TotalsRow{NetAmount: 1000, TotalRows: 10, CostedRows: 10, MarginAvailable: true}, nil
}

func (f *fakeSource) CategorySales(_ context.Context, _, from, to string) ([]salesdaily.CategoryRow, error) {
	f.note("category", from, to)
	if f.categoryErr != nil {
		return nil, f.categoryErr
	}
	return []salesdaily.CategoryRow{{Name: "生鲜联营", Amount: 700}}, nil
}

func (f *fakeSource) ProductSales(_ context.Context, _, from, to string) ([]salesdaily.ProductRow, error) {
	f.note("product", from, to)
	if f.productErr != nil {
		return nil, f.productErr
	}
	return []salesdaily.ProductRow{{ItemNo: "A1", Name: "土豆", Category: "生鲜联营", Amount: 50}}, nil
}

func (f *fakeSource) SupplierSales(_ context.Context, _, from, to string) ([]salesdaily.SupplierRow, error) {
	f.note("supplier", from, to)
	if f.supplierErr != nil {
		return nil, f.supplierErr
	}
	return []salesdaily.SupplierRow{{Code: "S1", Amount: 300, ItemCount: 2}}, nil
}

func (f *fakeSource) SupplierNames(_ context.Context, _ string, codes []string) (map[string]string, error) {
	f.note("names", "", "")
	f.namesAsked = append(f.namesAsked, codes...)
	if f.namesErr != nil {
		return nil, f.namesErr
	}
	if f.names != nil {
		return f.names, nil
	}
	return map[string]string{"S1": "南宁蔬菜行"}, nil
}

// fakeStore 记录被写入的快照。
type fakeStore struct {
	saves  int
	last   salesdaily.Snapshot
	source salesdaily.CollectSource
	err    error
}

func (s *fakeStore) Save(_ context.Context, snap salesdaily.Snapshot, src salesdaily.CollectSource) error {
	s.saves++
	s.last = snap
	s.source = src
	return s.err
}

var _ Source = (*fakeSource)(nil)

// ---- 落库策略 ----

// TestCollectorWritesNothingWhenAnySourceFails ⭐ 本包最重要的一条。
//
// 采集是每天跑两次的**唯一写者**，写进去的那一行会被月报、日报、
// 门店对比长期当作"当天的事实"。所以策略是：五个查询任一失败，
// 整次不落库 —— 消费侧读到 ErrNotFound 会回退查 cube，数字仍然正确；
// 而"半残事实"没有任何机制能事后发现。
//
// ⚠️ 必须**逐维**注入错误，而不是笼统地"某一步失败"：
//
//	只测总额失败的话，"品类失败但总额成功"这条最贵的路径完全没被覆盖 ——
//	而实现里那四处 if err != nil { return } 是四个**独立**的提前返回，
//	其中任何一处被改成"记个警告继续"，本用例都不会红。
func TestCollectorWritesNothingWhenAnySourceFails(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(*fakeSource)
	}{
		{"总额失败", func(f *fakeSource) { f.totalsErr = errors.New("总额挂了") }},
		{"品类失败", func(f *fakeSource) { f.categoryErr = errors.New("品类挂了") }},
		{"商品失败", func(f *fakeSource) { f.productErr = errors.New("商品挂了") }},
		{"供应商失败", func(f *fakeSource) { f.supplierErr = errors.New("供应商挂了") }},
		{"供应商字典失败", func(f *fakeSource) { f.namesErr = errors.New("字典挂了") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeSource{}
			tc.break_(src)
			store := &fakeStore{}
			c := NewCollector(src, store)

			if _, err := c.Run(context.Background(), "B1", "2026-10-16", salesdaily.CollectSourceInitial); err == nil {
				t.Fatalf("期望报错，实际成功 —— 那会把一份残缺事实写进库里")
			}
			if store.saves != 0 {
				t.Fatalf("Save 被调用了 %d 次，期望 0 次", store.saves)
			}
		})
	}
}

// TestCollectorSavesWholeSnapshotOnSuccess 成功路径必须写，且四维齐全。
func TestCollectorSavesWholeSnapshotOnSuccess(t *testing.T) {
	src := &fakeSource{}
	store := &fakeStore{}
	c := NewCollector(src, store)

	res, err := c.Run(context.Background(), "B1", "2026-10-16", salesdaily.CollectSourceCorrected)
	if err != nil {
		t.Fatalf("采集失败：%v", err)
	}
	if store.saves != 1 {
		t.Fatalf("Save 调用 %d 次，期望 1", store.saves)
	}
	if store.source != salesdaily.CollectSourceCorrected {
		t.Errorf("落库来源 = %q，期望 corrected", store.source)
	}
	snap := store.last
	if snap.BranchID != "B1" || snap.OperDate != "2026-10-16" {
		t.Errorf("快照标识 = %s/%s，期望 B1/2026-10-16", snap.BranchID, snap.OperDate)
	}
	if snap.Totals.NetAmount != 1000 {
		t.Errorf("总额 = %v，期望 1000", snap.Totals.NetAmount)
	}
	if len(snap.Categories) != 1 || len(snap.Products) != 1 || len(snap.Suppliers) != 1 {
		t.Fatalf("四维不齐：品类 %d / 商品 %d / 供应商 %d",
			len(snap.Categories), len(snap.Products), len(snap.Suppliers))
	}
	if !res.Saved {
		t.Error("Result.Saved 应为 true")
	}
	if res.Categories != 1 || res.Suppliers != 1 || res.Products != 1 {
		t.Errorf("Result 摘要计数不对：%+v", res)
	}
}

// TestSaveFailurePropagates 落库失败必须上抛。
//
// ⚠️ 吞掉它的话，job 回调会返回"采集成功"，
//
//	而库里什么都没有 —— wechat-bot 以为不用回退，
//	推送侧却在推送时才发现自己没有数据。
func TestSaveFailurePropagates(t *testing.T) {
	store := &fakeStore{err: errors.New("PG 写不进去")}
	c := NewCollector(&fakeSource{}, store)
	if _, err := c.Run(context.Background(), "B1", "2026-10-16", salesdaily.CollectSourceInitial); err == nil {
		t.Fatal("落库失败必须上抛，期望报错")
	}
}

// ---- 采集区间 ----

// TestCollectQueriesExactlyOneOperDate 每次查询的 from 与 to 都必须等于那一天。
//
// ⚠️ 采集的单位是"日"，而月报聚合的是"若干个日行"。
//
//	若 from/to 传成区间（哪怕只是误传了 to=""），就会把整月存成一行，
//	而月报聚合再把它当一天加起来 —— 得到的数会是真值的几十倍。
//	判据是**每一次调用**的区间都恰好等于目标营业日。
func TestCollectQueriesExactlyOneOperDate(t *testing.T) {
	src := &fakeSource{}
	store := &fakeStore{}
	c := NewCollector(src, store)

	if _, err := c.Run(context.Background(), "B1", "2026-10-16", salesdaily.CollectSourceInitial); err != nil {
		t.Fatalf("采集失败：%v", err)
	}
	// 字典查询不带日期（它查的是 supplier 档案，不是销售流水）。
	for _, r := range src.ranges {
		if r[0] == "" && r[1] == "" {
			continue
		}
		if r[0] != "2026-10-16" || r[1] != "2026-10-16" {
			t.Fatalf("查询区间 = [%s, %s]，期望 [2026-10-16, 2026-10-16]", r[0], r[1])
		}
	}
	if len(src.ranges) != 5 {
		t.Fatalf("期望 5 次查询（总额/品类/商品/供应商/字典），实际 %d", len(src.ranges))
	}
}

// TestOperDateMustBeExplicit 营业日不默认取今天。
//
// ⚠️ 次日 01:00 的校正采集要修的是**昨天**。
//
//	若默认成今天，01:00 会去覆盖一个刚开门、几乎没销售的今天，
//	而昨天那份残缺的首采**永远留在库里** ——
//	症状是月报某天明显偏低，且没有任何标记。
func TestOperDateMustBeExplicit(t *testing.T) {
	for _, v := range []string{"", "   "} {
		src := &fakeSource{}
		store := &fakeStore{}
		c := NewCollector(src, store)
		if _, err := c.Run(context.Background(), "B1", v, salesdaily.CollectSourceInitial); err == nil {
			t.Fatalf("oper_date=%q 必须报错而不是默认成今天", v)
		}
		if len(src.calls) != 0 {
			t.Fatalf("oper_date 非法时不该发起任何查询，实际调用了 %v", src.calls)
		}
	}
}

// TestOperDateFormatIsValidated 非法日期必须报错。
func TestOperDateFormatIsValidated(t *testing.T) {
	for _, v := range []string{"2026/10/16", "20261016", "2026-10-16 00:00", "昨天"} {
		if _, err := NormalizeOperDate(v); err == nil {
			t.Errorf("NormalizeOperDate(%q) 应报错", v)
		}
	}
	if got, err := NormalizeOperDate("  2026-10-16  "); err != nil || got != "2026-10-16" {
		t.Errorf("NormalizeOperDate 应修剪空白后返回 2026-10-16，实际 %q/%v", got, err)
	}
}

// TestBranchIsRequired 门店必填 —— 空门店会拿到"路由不明的数据"。
func TestBranchIsRequired(t *testing.T) {
	src := &fakeSource{}
	store := &fakeStore{}
	c := NewCollector(src, store)
	if _, err := c.Run(context.Background(), "  ", "2026-10-16", salesdaily.CollectSourceInitial); err == nil {
		t.Fatal("缺少门店必须报错")
	}
	if len(src.calls) != 0 {
		t.Fatalf("缺门店时不该发起查询，实际 %v", src.calls)
	}
}

// TestPrevOperDate 校正采集指向**昨天**。
//
// ⚠️ 覆盖月初/年末：01:00 的校正跨月时若写成"本月 1 号"或简单减 30 天，
//
//	就会去覆盖一个完全无关的日子，而真正需要校正的那天留在残缺状态。
func TestPrevOperDate(t *testing.T) {
	cases := []struct{ now, want string }{
		{"2026-10-17T01:00:00+08:00", "2026-10-16"},
		{"2026-11-01T01:00:00+08:00", "2026-10-31"},
		{"2026-03-01T01:00:00+08:00", "2026-02-28"},
		{"2024-03-01T01:00:00+08:00", "2024-02-29"}, // 闰年
		{"2026-01-01T01:00:00+08:00", "2025-12-31"},
	}
	for _, tc := range cases {
		now, err := time.Parse(time.RFC3339, tc.now)
		if err != nil {
			t.Fatalf("解析 %s 失败：%v", tc.now, err)
		}
		if got := PrevOperDate(now); got != tc.want {
			t.Errorf("PrevOperDate(%s) = %s，期望 %s", tc.now, got, tc.want)
		}
	}
}

// ---- 供应商名称 ----

// TestSupplierNamesAreFilled 名称要从字典补上。
func TestSupplierNamesAreFilled(t *testing.T) {
	src := &fakeSource{names: map[string]string{"S1": "南宁蔬菜行"}}
	store := &fakeStore{}
	c := NewCollector(src, store)
	if _, err := c.Run(context.Background(), "B1", "2026-10-16", salesdaily.CollectSourceInitial); err != nil {
		t.Fatalf("采集失败：%v", err)
	}
	if got := store.last.Suppliers[0].Name; got != "南宁蔬菜行" {
		t.Errorf("供应商名称 = %q，期望 南宁蔬菜行", got)
	}
	if len(src.namesAsked) != 1 || src.namesAsked[0] != "S1" {
		t.Errorf("字典查询编码 = %v，期望 [S1]", src.namesAsked)
	}
}

// TestSupplierMissingFromDictIsKept 字典查不到**这一条**时，行要留着。
//
// ⚠️ 丢弃它会让这家供应商的销量凭空从大盘里消失
// （与 internal/salesinsight/build/suppliers.go 同一取舍）。
//
//	注意与"字典整体查询失败"区分：后者是 error，会让整次采集不落库；
//	前者只是一个空名字，是**正常数据状态**。
func TestSupplierMissingFromDictIsKept(t *testing.T) {
	src := &fakeSource{names: map[string]string{}}
	store := &fakeStore{}
	c := NewCollector(src, store)
	if _, err := c.Run(context.Background(), "B1", "2026-10-16", salesdaily.CollectSourceInitial); err != nil {
		t.Fatalf("字典里没有这一条不该让采集失败：%v", err)
	}
	if len(store.last.Suppliers) != 1 {
		t.Fatalf("供应商行数 = %d，期望 1（缺名字的行必须保留）", len(store.last.Suppliers))
	}
	if store.last.Suppliers[0].Amount != 300 {
		t.Errorf("金额 = %v，期望 300", store.last.Suppliers[0].Amount)
	}
	if store.last.Suppliers[0].Name != "" {
		t.Errorf("名称 = %q，期望空（没有就是没有，不能编）", store.last.Suppliers[0].Name)
	}
}

// ---- 查询形状 ----

// hasLimit 判断查询是否带了上限。
func hasLimit(q cubeclient.CubeQuery) bool { return q.Limit > 0 }

// TestEveryCollectQueryHasNoLimit ⭐
//
// 采集的**每一条**查询都必须不限量。
//
// ⚠️ 为什么这条比"总额查询不能带维度"还重要：
//
//	总额带维度 → 少一个维度对应的行（这已被独立断言）；
//	任一带 limit → 排在上限之外的数据**整批不回来**，
//	而月报靠"把每天定稿的日行加起来"出数，
//	"加起来 = 大盘"这个等式只在明细集是全集时成立。
//
//	失败表现：月报某天偏小、无报错、无降级标记 ——
//	与 2026-10-17 那个「大盘少了 85%」完全同类。
func TestEveryCollectQueryHasNoLimit(t *testing.T) {
	qs := map[string]cubeclient.CubeQuery{
		"总额":     totalsQuery("2026-10-16", "2026-10-16"),
		"品类":     categoryQuery("2026-10-16", "2026-10-16"),
		"商品":     productQuery("2026-10-16", "2026-10-16"),
		"供应商":    supplierQuery("2026-10-16", "2026-10-16"),
		"字典(豁免)": supplierDictQuery([]any{"S1"}),
	}
	for name, q := range qs {
		if strings.HasPrefix(name, "字典") {
			// 字典按编码 in 查询，条数已知，limit 是安全上界；
			// 它不属于"销售明细"，不受本规则约束。
			continue
		}
		if hasLimit(q) {
			t.Errorf("%s 查询带了 Limit=%d —— 采集必须全量", name, q.Limit)
		}
	}
}

// TestTotalsQueryHasNoDimension 大盘总额查询不带任何维度。
//
// ⚠️ 带了维度 → 空类目行被 continue 丢弃，
//
//	于是"总额"其实只是"有类目的那部分"。
//	线上实测门店 00 / 2026-10-16：品类口径 2024.6 元 vs
//	供应商口径 13003.8 元，差 6 倍、零报错。
func TestTotalsQueryHasNoDimension(t *testing.T) {
	q := totalsQuery("2026-10-16", "2026-10-16")
	if len(q.Dimensions) != 0 {
		t.Fatalf("总额查询带了维度 %v —— 带了就不是大盘", q.Dimensions)
	}
}

// TestCollectQueriesCarryMeasuresAndDateFilter 查询必须带度量与日期过滤。
//
// ⚠️ 漏掉日期过滤 ⇒ cube 返回**全历史**，几十万行进内存；
//
//	而它的症状不是报错，是"采集特别慢然后被网关掐断"。
func TestCollectQueriesCarryMeasuresAndDateFilter(t *testing.T) {
	qs := map[string]cubeclient.CubeQuery{
		"总额":  totalsQuery("2026-10-16", "2026-10-16"),
		"品类":  categoryQuery("2026-10-16", "2026-10-16"),
		"商品":  productQuery("2026-10-16", "2026-10-16"),
		"供应商": supplierQuery("2026-10-16", "2026-10-16"),
	}
	for name, q := range qs {
		if len(q.Measures) == 0 {
			t.Errorf("%s 查询没有度量", name)
		}
		if len(q.Filters) != 2 {
			t.Errorf("%s 查询的日期过滤数 = %d，期望 2（gte + lte）", name, len(q.Filters))
			continue
		}
		if q.Filters[0].Operator != "gte" || q.Filters[1].Operator != "lte" {
			t.Errorf("%s 查询的日期算子 = %s/%s，期望 gte/lte",
				name, q.Filters[0].Operator, q.Filters[1].Operator)
		}
	}
}

// TestTotalsMarginNeedsCostCoverage 成本覆盖率不足时不得报毛利。
//
// ⚠️ hbposv10 实测当日 64 个类别组里多组覆盖率 0%，
//
//	按 cost=0 算会显示"毛利率 100%" —— 那是个完全正常、毫无意义的数，
//	比不显示更坏。
func TestTotalsMarginNeedsCostCoverage(t *testing.T) {
	lowCov := map[string]any{
		"sales_report.total_net_amount": "1000",
		"sales_report.total_net_qty":    "10",
		"sales_report.total_ref_cost":   "0",
		"sales_report.count":            "100",
		"sales_report.costed_rows":      "1",
	}
	got := totalsRow(lowCov)
	if got.MarginAvailable {
		t.Error("覆盖率 1% 时不应可用毛利")
	}
	if got.RefMargin != 0 || got.RefMarginPct != 0 {
		t.Errorf("覆盖率不足时毛利必须是 0，实际 %v / %v", got.RefMargin, got.RefMarginPct)
	}

	okCov := map[string]any{
		"sales_report.total_net_amount": "1000",
		"sales_report.total_net_qty":    "10",
		"sales_report.total_ref_cost":   "600",
		"sales_report.count":            "100",
		"sales_report.costed_rows":      "100",
	}
	got2 := totalsRow(okCov)
	if !got2.MarginAvailable {
		t.Fatal("覆盖率 100% 时应可用毛利")
	}
	if got2.RefMargin != 400 || got2.RefMarginPct != 0.4 {
		t.Errorf("毛利 = %v / %v，期望 400 / 0.4", got2.RefMargin, got2.RefMarginPct)
	}
}

// TestTotalsRowZeroCountMeansNoMargin count=0 时不得算覆盖率。
//
// "0/0" 在这里是"没有行"，不是"覆盖完美"。
func TestTotalsRowZeroCountMeansNoMargin(t *testing.T) {
	got := totalsRow(map[string]any{
		"sales_report.total_net_amount": "0",
		"sales_report.count":            "0",
		"sales_report.costed_rows":      "0",
	})
	if got.CostCoverage != 0 {
		t.Errorf("count=0 时覆盖率应是 0，实际 %v", got.CostCoverage)
	}
	if got.MarginAvailable {
		t.Error("count=0 时不应可用毛利")
	}
}

// ---- 供应商归并 ----

// TestCollapseSuppliersKeepsEveryone 归并不能丢掉任何一家供应商。
//
// ⚠️ 只保留 TopN 家 → 其它供应商的金额从月报里消失，
//
//	而"供应商金额之和"正是店长判断货源结构的依据。
//	症状是月报看起来正常，只是某家小供应商再也没出现过。
func TestCollapseSuppliersKeepsEveryone(t *testing.T) {
	var data []map[string]any
	for _, c := range []string{"S1", "S2", "S3", "S4", "S5"} {
		data = append(data, map[string]any{
			"sales_report.supcust_no":       c,
			"sales_report.item_no":          c + "-1",
			"item.name":                     "货",
			"sales_report.total_net_amount": "100",
			"sales_report.total_net_qty":    "1",
		})
	}
	got := collapseSuppliers(data)
	if len(got) != 5 {
		t.Fatalf("归并后 %d 家，期望 5 家 —— 少一家就是月报少一笔账", len(got))
	}
}

// TestCollapseSuppliersDeduplicatesItems 同一商品的多次成交只计一次明细。
//
// ⚠️ 金额要**累加**（多次成交是真实的），
//
//	但商品明细**只记一次**（否则 ItemCount 与 TopItems 都在虚高）。
func TestCollapseSuppliersDeduplicatesItems(t *testing.T) {
	data := []map[string]any{
		{"sales_report.supcust_no": "S1", "sales_report.item_no": "A", "item.name": "土豆",
			"sales_report.total_net_amount": "10", "sales_report.total_net_qty": "1"},
		{"sales_report.supcust_no": "S1", "sales_report.item_no": "A", "item.name": "土豆",
			"sales_report.total_net_amount": "15", "sales_report.total_net_qty": "2"},
		{"sales_report.supcust_no": "S1", "sales_report.item_no": "B", "item.name": "洋葱",
			"sales_report.total_net_amount": "20", "sales_report.total_net_qty": "1"},
	}
	got := collapseSuppliers(data)
	if len(got) != 1 {
		t.Fatalf("供应商数 = %d，期望 1", len(got))
	}
	if got[0].Amount != 45 {
		t.Errorf("金额 = %v，期望 45（两次成交都要计入）", got[0].Amount)
	}
	if got[0].Qty != 4 {
		t.Errorf("数量 = %v，期望 4", got[0].Qty)
	}
	if got[0].ItemCount != 2 {
		t.Errorf("商品数 = %d，期望 2（同商品去重后按 item_no 算）", got[0].ItemCount)
	}
}

// TestCollapseSuppliersCapsTopItems 但要封住 TopItems 条数。
//
// ⚠️ 封顶的**不是金额**（那会丢供应商），只是下钻明细的条数 ——
//
//	不封的话一家供应商 200 个商品会让那一行膨胀到几 KB，
//	而每家都这样会让 suppliers 表一天涨几十 MB。
func TestCollapseSuppliersCapsTopItems(t *testing.T) {
	var data []map[string]any
	for i := 0; i < 10; i++ {
		data = append(data, map[string]any{
			"sales_report.supcust_no":       "S1",
			"sales_report.item_no":          string(rune('A'+i)) + "-item",
			"item.name":                     "货",
			"sales_report.total_net_amount": itoa(100 - i), // 让顺序可预期
			"sales_report.total_net_qty":    "1",
		})
	}
	got := collapseSuppliers(data)
	if len(got[0].TopItems) != TopItemsPerSupplier {
		t.Fatalf("TopItems %d 条，期望 %d", len(got[0].TopItems), TopItemsPerSupplier)
	}
	if got[0].TopItems[0].Amount <= got[0].TopItems[1].Amount {
		t.Error("TopItems 必须按金额降序")
	}
	if got[0].ItemCount != 10 {
		t.Errorf("ItemCount = %d，期望 10（封顶的只是明细条数，不是商品总数）", got[0].ItemCount)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// TestCollapseSuppliersSortsByAmount 归并结果按金额降序。
//
// ⚠️ 不是为了展示好看 —— 落库时顺序决定 PG 里 `ORDER BY amount desc`
//
//	之外的默认顺序，测试全绿时读出来会是一堆乱序行，
//	而"首行是什么"会被下一个人当成"第一大供应商"写进逻辑。
func TestCollapseSuppliersSortsByAmount(t *testing.T) {
	data := []map[string]any{
		{"sales_report.supcust_no": "S1", "sales_report.item_no": "A",
			"sales_report.total_net_amount": "10"},
		{"sales_report.supcust_no": "S2", "sales_report.item_no": "B",
			"sales_report.total_net_amount": "90"},
		{"sales_report.supcust_no": "S3", "sales_report.item_no": "C",
			"sales_report.total_net_amount": "50"},
	}
	got := collapseSuppliers(data)
	want := []string{"S2", "S3", "S1"}
	for i, w := range want {
		if got[i].Code != w {
			t.Fatalf("第 %d 家 = %s，期望 %s（实际顺序 %v）", i, got[i].Code, w, got[i].Code)
		}
	}
}

// TestSupplierNamesEmptyInputShortCircuits 没有编码时不发请求。
//
// ⚠️ 否则会发出一个 `in ()` 的空过滤查询 ——
//
//	它在 cube 侧要么报错，要么（更糟）被当成"不过滤"从而返回**全量**字典。
func TestSupplierNamesEmptyInputShortCircuits(t *testing.T) {
	s := &DaprSource{}
	got, err := s.SupplierNames(context.Background(), "B1", nil)
	if err != nil {
		t.Fatalf("空编码不该报错：%v", err)
	}
	if len(got) != 0 {
		t.Errorf("空编码应返回空 map，实际 %v", got)
	}
}

// TestStoreNilStillCollects 未装配 store 时只采集不落库（便于排查取数）。
func TestStoreNilStillCollects(t *testing.T) {
	src := &fakeSource{}
	c := NewCollector(src, nil)
	res, err := c.Run(context.Background(), "B1", "2026-10-16", salesdaily.CollectSourceInitial)
	if err != nil {
		t.Fatalf("采集失败：%v", err)
	}
	if res.Saved {
		t.Error("没有 store 时不该报 Saved")
	}
	if res.Categories != 1 {
		t.Errorf("摘要应仍反映采集结果，实际 %+v", res)
	}
}
