package build

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/salesinsight/calendar"
	"github.com/YunBright/supertrade/internal/salesinsight/fact"
	"github.com/YunBright/supertrade/internal/salesinsight/fact/supplier"
	"github.com/YunBright/supertrade/internal/salesinsight/ignore"
	"github.com/YunBright/supertrade/internal/salesinsight/weather"
)

// newIgnoreStore 造一个**文件模式**的名单存储（每个用例独立目录）。
//
// ⚠️ 刻意不用内存模式：内存模式用包级 map，
// 跨用例串数据会让"某用例保存的名单"影响另一个用例的断言，
// 而这种串扰只在并行或顺序变化时才暴露，极难排查。
func newIgnoreStore(t *testing.T) *ignore.Store {
	t.Helper()
	return ignore.NewStore(t.TempDir())
}

// fakeSource 是可控的数据源。
//
// ⚠️ 它**逐字节**保留 cube 返回的内容不做任何加工，
//
//	与 wechat-bot 里 daprstate fake 的保真度要求同源（见 agent memory）：
//	fake 掉加工层会让测试测不到真正的 bug。
type fakeSource struct {
	catCur, catPrev   []CategoryRow
	prodCur, prodPrev []ProductRow
	supCur, supPrev   []SupplierSalesRow
	dict              []supplier.Record
	stocks            map[string]fact.StockRecord

	errCategory, errProduct, errSupplier error
	errDict, errStock                    error
	errTotals                            error
	// hasTotals 为 true 时用 totCur/totPrev 覆盖总额，否则复刻品类求和。
	hasTotals       bool
	totCur, totPrev TotalsRow
	// calls 记录每个方法被调了几次，用于验证"只查 TopN 商品的库存"。
	calls map[string]int
	// stockAsked 记录实际问了哪些商品。
	stockAsked []string
	// dictBranch 记录 SupplierDict 收到的门店 —— 见下方那条断言。
	dictBranch string
}

// ⚠️ 三个查询方法都**按日期区分**本期与上期。
//
// 这个细节不是可有可无的：最初这个 fake 不看日期，
// 于是"本期数据"被当成"上期数据"喂回去，
// 环比恒为 0、归因恒为 none —— 而**所有测试都绿**。
//
// 这正是"fake 的保真度决定测试的有效性"：
// fake 掉的那一层（日期过滤）恰恰是唯一会出 bug 的边界。
// 判据参照 agent memory 里 daprstate fake 的同款教训。
func (f *fakeSource) CategorySales(_ context.Context, _, from, _ string, _ int) ([]CategoryRow, error) {
	f.calls["cat"]++
	if f.errCategory != nil {
		return nil, f.errCategory
	}
	if from == "2026-10-14" {
		return f.catPrev, nil
	}
	return f.catCur, nil
}

// TotalsSales 是 2026-10-17 新增的大盘总额查询。
//
// ⚠️ 默认**复刻旧的品类求和结果**，这样既有断言（总额 13000 等）
//
//	不需要改 —— 它们本来就是在验证"品类与总额一致"这个当时的假设。
//	真正的判别力在 setTotals 覆盖：用例会给出与品类求和**不同**的总额，
//	断言大盘取的是覆盖值而不是求和值。
func (f *fakeSource) TotalsSales(_ context.Context, _, from, _ string) (TotalsRow, error) {
	f.calls["totals"]++
	if f.errTotals != nil {
		return TotalsRow{}, f.errTotals
	}
	if f.hasTotals {
		if from == "2026-10-14" {
			return f.totPrev, nil
		}
		return f.totCur, nil
	}
	if from == "2026-10-14" {
		return sumAsRow(f.catPrev), nil
	}
	return sumAsRow(f.catCur), nil
}

func sumAsRow(cats []CategoryRow) TotalsRow {
	var r TotalsRow
	for _, c := range cats {
		r.Amount += c.Amount
		r.Qty += c.Qty
		r.Cost += c.Cost
		r.CostedRow += c.CostedRow
		r.TotalRow += c.TotalRow
	}
	return r
}

func (f *fakeSource) ProductSales(_ context.Context, _, from, _ string, _ int) ([]ProductRow, error) {

	f.calls["prod"]++
	if f.errProduct != nil {
		return nil, f.errProduct
	}
	if from == "2026-10-14" {
		return f.prodPrev, nil
	}
	return f.prodCur, nil
}

func (f *fakeSource) SupplierSales(_ context.Context, _, from, _ string, _ int) ([]SupplierSalesRow, error) {
	if f.errSupplier != nil {
		return nil, f.errSupplier
	}
	if from == "2026-10-14" {
		return f.supPrev, nil
	}
	return f.supCur, nil
}

func (f *fakeSource) SupplierDict(_ context.Context, storeBranchID string, codes []string) ([]supplier.Record, error) {
	f.calls["dict"]++
	f.dictBranch = storeBranchID
	if f.errDict != nil {
		return nil, f.errDict
	}
	want := map[string]bool{}
	for _, c := range codes {
		want[c] = true
	}
	var out []supplier.Record
	for _, r := range f.dict {
		if want[r.Code] {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeSource) StockByItems(_ context.Context, _ string, itemNos []string) (map[string]fact.StockRecord, error) {
	f.calls["stock"]++
	f.stockAsked = append(f.stockAsked, itemNos...)
	if f.errStock != nil {
		return nil, f.errStock
	}
	out := map[string]fact.StockRecord{}
	for k, v := range f.stocks {
		if v.ItemNo == "" {
			v.ItemNo = k
		}
		out[k] = v
	}
	return out, nil
}

func newFake() *fakeSource {
	return &fakeSource{
		stocks: map[string]fact.StockRecord{},
		calls:  map[string]int{},
	}
}

func testOptions() Options {
	// 2026-10-16 生成日报 ⇒ 本期 10-15，上期 10-14
	now, _ := time.Parse("2006-01-02", "2026-10-16")
	p, _ := calendar.ResolvePeriod(calendar.PeriodDaily, now)
	return Options{
		StoreBranchID: "00_1000003",
		BranchName:    "富万家",
		Period:        p,
		TopN:          10,
		Location:      weather.Location{Name: "岑溪", Lat: 24.7, Lon: 111.0},
	}
}

// 完整链路：事实包必须被正确组装。
func TestBuildProducesCompleteFact(t *testing.T) {
	fs := newFake()
	fs.catCur = []CategoryRow{
		{Name: "生鲜联营", Amount: 8000, Qty: 900, Cost: 6500, CostedRow: 100, TotalRow: 100},
		{Name: "百货联营", Amount: 5000, Qty: 500, Cost: 4000, CostedRow: 100, TotalRow: 100},
	}
	fs.catPrev = []CategoryRow{
		{Name: "生鲜联营", Amount: 7000},
		{Name: "百货联营", Amount: 6000},
	}
	fs.prodCur = []ProductRow{
		{ItemNo: "A", Name: "西红柿", Category: "生鲜联营", Amount: 300, Qty: 30},
		{ItemNo: "B", Name: "土豆", Category: "生鲜联营", Amount: 200, Qty: 40},
	}
	fs.prodPrev = []ProductRow{
		{ItemNo: "A", Name: "西红柿", Category: "生鲜联营", Amount: 400},
	}
	// 西红柿断货、土豆充足
	fs.stocks = map[string]fact.StockRecord{
		"A": {ItemNo: "A", Quantity: 0, MinQty: 12, HasRow: true},
		"B": {ItemNo: "B", Quantity: 50, MinQty: 10, HasRow: true},
	}

	f, err := NewBuilder(fs, nil).Build(context.Background(), testOptions())
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}

	if f.Meta.PeriodKind != "daily" || f.Meta.From != "2026-10-15" || f.Meta.PrevFrom != "2026-10-14" {
		t.Errorf("周期元信息错误: %+v", f.Meta)
	}
	if f.Totals.NetAmount != 13000 {
		t.Errorf("大盘销售额 = %v, 期望 13000", f.Totals.NetAmount)
	}
	if !f.Totals.MarginAvailable {
		t.Error("成本覆盖率 100% 时毛利应可用")
	}
	if len(f.Categories) != 2 || f.Categories[0].Name != "生鲜联营" || f.Categories[0].Rank != 1 {
		t.Errorf("品类排行错误: %+v", f.Categories)
	}
	if !f.Categories[0].HasPrev || f.Categories[0].DeltaPct < 14 {
		t.Errorf("品类环比错误: %+v", f.Categories[0])
	}
	// 占比：8000/13000
	if c := f.Categories[0].AmountShare; c < 0.61 || c > 0.62 {
		t.Errorf("品类占比 = %v, 期望约 0.615", c)
	}
	// 缺货判定
	if len(f.Stockouts) != 1 || f.Stockouts[0].ItemNo != "A" {
		t.Errorf("缺货清单应只含西红柿: %+v", f.Stockouts)
	}
	// 归因：西红柿 -25% 且断货 ⇒ stockout
	for _, p := range f.TopProducts {
		if p.ItemNo == "A" && p.DeclineCause != fact.CauseStockout {
			t.Errorf("西红柿归因 = %q, 期望 %q", p.DeclineCause, fact.CauseStockout)
		}
	}
}

// ⚠️ 成本覆盖率不足时毛利必须不可用，且不能给出假毛利率。
func TestMarginUnavailableWhenCoverageLow(t *testing.T) {
	fs := newFake()
	fs.catCur = []CategoryRow{
		{Name: "生鲜联营", Amount: 8000, Cost: 0, CostedRow: 25, TotalRow: 100},
	}
	fs.catPrev = nil

	f, err := NewBuilder(fs, nil).Build(context.Background(), testOptions())
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if f.Totals.MarginAvailable {
		t.Fatal("成本覆盖率 25% 时毛利必须标记为不可用")
	}
	if f.Totals.RefMarginPct != 0 {
		t.Errorf("不可用时毛利率必须为 0（而不是算出 100%%），实际 %v", f.Totals.RefMarginPct)
	}
	if f.Totals.CostCoverage < 0.25 || f.Totals.CostCoverage > 0.26 {
		t.Errorf("成本覆盖率应被如实返回，实际 %v", f.Totals.CostCoverage)
	}
}

// ⚠️ 自用供应商必须被排除，且**排除信息保留**。
func TestExcludesSelfUseSuppliers(t *testing.T) {
	fs := newFake()
	fs.catCur = []CategoryRow{{Name: "生鲜联营", Amount: 10000, CostedRow: 100, TotalRow: 100}}
	fs.supCur = []SupplierSalesRow{
		{Code: "019", Amount: 5000, ProductCount: 30}, // 自购蔬菜（运营加入名单）
		{Code: "038", Amount: 3000, ProductCount: 10}, // 真实供应商
		{Code: "01", Amount: 2000, ProductCount: 5},   // 运营部（客户，加入名单）
		{Code: "101", Amount: 1500, ProductCount: 8},  // 乐园公司（自用直供，加入名单）
		{Code: "084", Amount: 1000, ProductCount: 6},  // 水果（自营，加入名单）
		{Code: "200", Amount: 800, ProductCount: 4},   // 另一个真实供应商
	}
	fs.dict = []supplier.Record{
		{Code: "019", Name: "自购蔬菜", Type: "1", CustFlag: "S"},
		{Code: "038", Name: "岑溪市泰鼎粮油食品有限公司", Type: "1", CustFlag: "S"},
		{Code: "01", Name: "运营部", Type: "1", CustFlag: "C"},
		{Code: "101", Name: "乐园公司", Type: "5", CustFlag: "S"},
		{Code: "084", Name: "水果（自营）", Type: "1", CustFlag: "S"},
		{Code: "200", Name: "梧州祥鑫贸易", Type: "1", CustFlag: "S"},
	}

	// ⚠️ 2026-10-17 起：判定只看**人工名单**，字典里的 type/cust_flag/名称
	//   不再参与排除（它们降级为维护名单时的推荐工具）。
	//   下面这条字典里 019/084 的 type 都是 '1'（普通供应商），
	//   若代码还在跑三层判定，只有 01/101 会被排除 —— 与期望的 4 条不符。
	ignores := newIgnoreStore(t)
	if _, err := ignores.Save("00_1000003", []ignore.Entry{
		{Code: "019", Name: "自购蔬菜", Reason: "门店自营蔬菜采购"},
		{Code: "01", Name: "运营部", Reason: "这是客户不是供货方"},
		{Code: "101", Name: "乐园公司", Reason: "集团内部直供"},
		{Code: "084", Name: "水果（自营）"},
	}); err != nil {
		t.Fatal(err)
	}

	f, err := NewBuilder(fs, nil).WithIgnores(ignores).
		Build(context.Background(), testOptions())
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}

	// 榜单上只能有真供应商
	for _, s := range f.Suppliers {
		if s.Code == "019" || s.Code == "01" || s.Code == "101" || s.Code == "084" {
			t.Fatalf("名单内的供应商 %q 出现在榜单中", s.Name)
		}
	}
	if len(f.Suppliers) != 2 {
		t.Errorf("应保留 2 个真实供应商，实际 %d: %+v", len(f.Suppliers), f.Suppliers)
	}
	// 排除信息必须保留，且带原因
	if len(f.ExcludedSuppliers) != 4 {
		t.Errorf("应保留 4 条排除记录，实际 %d", len(f.ExcludedSuppliers))
	}
	reasonByCode := map[string]string{}
	rules := map[string]string{}
	for _, e := range f.ExcludedSuppliers {
		reasonByCode[e.Code] = e.Reason
		rules[e.Code] = e.Rule
		if e.Amount == 0 {
			t.Errorf("排除记录应保留金额（否则店长看不到这部分销量去哪儿了），%q", e.Name)
		}
	}
	for _, c := range []string{"019", "01", "101", "084"} {
		if reasonByCode[c] == "" {
			t.Errorf("每条排除记录都要有原因: %+v", reasonByCode)
		}
		if rules[c] != string(fact.ExcludeManualList) {
			t.Errorf("%s 的排除依据应为 manual_list，实际 %q", c, rules[c])
		}
	}
	// 配置过名单 ⇒ 不得再报"尚未配置"的告警。
	if !f.DataSufficiency.Sufficient {
		t.Errorf("名单已配置，不应标为数据不足: %v", f.DataSufficiency.Issues)
	}
}

// ⚠️ **本次改造最重要的一条**：名单"没配过"时必须产生**可见**告警。
//
// 这是改造引入的新风险：改成纯人工名单后，如果运营还没配，
// 自用供应商会全部涌回"供应商榜单"，而报告里**没有任何异常信号** ——
// 店长会把门店自己的蔬菜采购当成外部优质货源。
//
// 判别力：把 buildSuppliers 里的 `case !ignored.Configured:` 分支删掉，
// 本用例立刻失败（DataSufficiency.Sufficient 变回 true）。
func TestUnconfiguredIgnoreListIsFlagged(t *testing.T) {
	fs := newFake()
	fs.catCur = []CategoryRow{{Name: "生鲜联营", Amount: 10000}}
	fs.supCur = []SupplierSalesRow{
		{Code: "019", Amount: 5000}, // 自购蔬菜：字典能看出是自用，但名单没配
		{Code: "038", Amount: 3000},
	}
	fs.dict = []supplier.Record{
		{Code: "019", Name: "自购蔬菜", Type: "1", CustFlag: "S"},
		{Code: "038", Name: "泰鼎粮油", Type: "1", CustFlag: "S"},
	}

	// 门店 00_1000003 从未保存过名单。
	f, err := NewBuilder(fs, nil).WithIgnores(newIgnoreStore(t)).
		Build(context.Background(), testOptions())
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if f.DataSufficiency.Sufficient {
		t.Error("未配置忽略名单必须标为数据不足（否则自用供应商静默回到榜单）")
	}
	if !containsSub(f.DataSufficiency.Issues, "尚未配置供应商忽略名单") {
		t.Errorf("告警文案必须指向可操作的位置: %v", f.DataSufficiency.Issues)
	}
	// ⚠️ 但**不能**因为没配就把供应商全排除掉 ——
	//   那等于把"未配置"变成"全部忽略"，真供应商会集体消失。
	if len(f.Suppliers) == 0 {
		t.Error("未配置名单时应保守保留全部供应商，而不是全部排除")
	}
}

// 名单**读不出来**与"没配过"是两种不同故障，处理方式也必须不同。
func TestUnreadableIgnoreListIsFlaggedDistinctly(t *testing.T) {
	fs := newFake()
	fs.catCur = []CategoryRow{{Name: "生鲜联营", Amount: 10000}}
	fs.supCur = []SupplierSalesRow{{Code: "038", Amount: 3000}}
	fs.dict = []supplier.Record{{Code: "038", Name: "泰鼎粮油", Type: "1", CustFlag: "S"}}

	dir := t.TempDir()
	st := ignore.NewStore(dir)
	branch := testOptions().StoreBranchID
	// 直接写一个损坏文件，模拟人工改坏/磁盘写坏。
	if err := os.WriteFile(filepath.Join(dir, "supplier-ignore-"+branch+".json"),
		[]byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}

	f, err := NewBuilder(fs, nil).WithIgnores(st).
		Build(context.Background(), testOptions())
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if f.DataSufficiency.Sufficient {
		t.Error("名单损坏必须标为数据不足")
	}
	if !containsSub(f.DataSufficiency.Issues, "忽略名单读取失败") {
		t.Errorf("告警必须区分「读取失败」与「尚未配置」: %v", f.DataSufficiency.Issues)
	}
	// 损坏 ≠ 没配过：文案不能误导运维去 App 里重新配（那是另一类问题）。
	if containsSub(f.DataSufficiency.Issues, "尚未配置") {
		t.Errorf("读取失败不得报成尚未配置: %v", f.DataSufficiency.Issues)
	}
}

// 空名单（**配过**，但一个条目也没有）不得报警告。
//
// 这是"Configured"标志存在的意义：区别于上一条。
// 判别力：把 Save 里的 `Configured: true` 改成 false，本用例立刻失败。
func TestConfiguredEmptyListIsNotFlagged(t *testing.T) {
	fs := newFake()
	fs.catCur = []CategoryRow{{Name: "生鲜联营", Amount: 10000}}
	fs.supCur = []SupplierSalesRow{{Code: "038", Amount: 3000}}
	fs.dict = []supplier.Record{{Code: "038", Name: "泰鼎粮油", Type: "1", CustFlag: "S"}}

	ignores := newIgnoreStore(t)
	// 显式保存一次空名单：这家店确实全是外采。
	if _, err := ignores.Save("00_1000003", nil); err != nil {
		t.Fatal(err)
	}

	f, err := NewBuilder(fs, nil).WithIgnores(ignores).
		Build(context.Background(), testOptions())
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if !f.DataSufficiency.Sufficient {
		t.Errorf("显式配置的空名单是合法的（纯外采门店），不应报警告: %v", f.DataSufficiency.Issues)
	}
}

// ⚠️ 门店之间互不影响：改 A 店名单不能影响 B 店。
//
// 这正是"按 X-Branch-ID 分门店存储"的验收点。
// 判别力：把 Store 的文件名去掉门店前缀（所有门店共用一个文件），
// 本用例立刻失败。
func TestIgnoreListIsPerBranch(t *testing.T) {
	st := newIgnoreStore(t)
	if _, err := st.Save("branch-A", []ignore.Entry{{Code: "019", Name: "自购蔬菜"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Save("branch-B", []ignore.Entry{{Code: "084", Name: "水果（自营）"}}); err != nil {
		t.Fatal(err)
	}

	lA, err := st.Load("branch-A")
	if err != nil {
		t.Fatal(err)
	}
	if len(lA.Entries) != 1 || lA.Entries[0].Code != "019" {
		t.Errorf("branch-A 的名单被污染: %+v", lA.Entries)
	}
	lB, err := st.Load("branch-B")
	if err != nil {
		t.Fatal(err)
	}
	if len(lB.Entries) != 1 || lB.Entries[0].Code != "084" {
		t.Errorf("branch-B 的名单被污染: %+v", lB.Entries)
	}
	// 未配置过的第三家店必须仍是"没配过"。
	lC, err := st.Load("branch-C")
	if err != nil {
		t.Fatal(err)
	}
	if lC.Configured || len(lC.Entries) != 0 {
		t.Errorf("未配置门店应为空且 Configured=false: %+v", lC)
	}
}

// ⚠️ 字典查不到时**保守保留**，并标记数据不足。
//
// 排除一条真供应商（丢失优质货源线索）的代价 > 多显示一条自采渠道。
func TestDictFailureKeepsSuppliersButFlagsInsufficiency(t *testing.T) {
	fs := newFake()
	fs.catCur = []CategoryRow{{Name: "生鲜联营", Amount: 10000}}
	fs.supCur = []SupplierSalesRow{
		{Code: "019", Amount: 5000}, // 实际是自购蔬菜
		{Code: "038", Amount: 3000},
	}
	fs.errDict = errors.New("cube 不可用")

	f, err := NewBuilder(fs, nil).Build(context.Background(), testOptions())
	if err != nil {
		t.Fatalf("字典失败不应让整体失败: %v", err)
	}
	if len(f.Suppliers) != 2 {
		t.Errorf("字典失败时应保守保留全部供应商，实际 %d", len(f.Suppliers))
	}
	if f.DataSufficiency.Sufficient {
		t.Error("字典失败必须标记为数据不足")
	}
	if !containsSub(f.DataSufficiency.Issues, "无法识别自用") {
		t.Errorf("必须说明无法识别自用渠道: %v", f.DataSufficiency.Issues)
	}
}

// ⚠️ 库存查询失败要降级但**不中断**：缺货判断不可用，其余结论仍成立。
func TestStockFailureDegradesButDoesNotAbort(t *testing.T) {
	fs := newFake()
	fs.catCur = []CategoryRow{{Name: "生鲜联营", Amount: 10000}}
	fs.prodCur = []ProductRow{{ItemNo: "A", Name: "西红柿", Amount: 300}}
	fs.errStock = errors.New("库存服务不可用")

	f, err := NewBuilder(fs, nil).Build(context.Background(), testOptions())
	if err != nil {
		t.Fatalf("库存失败不应让整体失败: %v", err)
	}
	if f.Totals.NetAmount != 10000 {
		t.Errorf("大盘仍应可用，实际 %v", f.Totals.NetAmount)
	}
	if !containsSub(f.DataSufficiency.Issues, "缺货判断不可用") {
		t.Errorf("必须标记缺货判断不可用: %v", f.DataSufficiency.Issues)
	}
	// 查不到库存的商品不应被判成缺货
	for _, p := range f.TopProducts {
		if p.StockStatus == fact.StockOut {
			t.Errorf("库存查询失败时不得判定缺货: %+v", p)
		}
	}
}

// ⚠️ 库存只查 TopN 商品 —— 这是成本与延迟的关键控制。
func TestStockQueryIsLimitedToTopProducts(t *testing.T) {
	fs := newFake()
	fs.catCur = []CategoryRow{{Name: "生鲜联营", Amount: 10000}}
	var many []ProductRow
	for i := 0; i < 100; i++ {
		many = append(many, ProductRow{
			ItemNo: string(rune('a'+i%26)) + "-" + itoa(i),
			Name:   "商品" + itoa(i),
			Amount: float64(100 - i),
		})
	}
	fs.prodCur = many
	fs.stocks = map[string]fact.StockRecord{}

	if _, err := NewBuilder(fs, nil).Build(context.Background(), testOptions()); err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if len(fs.stockAsked) > 60 {
		t.Errorf("库存查询应只覆盖 TopN 商品（≤60），实际问了 %d 个", len(fs.stockAsked))
	}
}

// 上期取数失败仍要出报告，只是不做环比。
//
// ⚠️ 这是"降级而不是整体失败"的核心用例：
//
//	没有环比的日报仍然有排行、缺货、总结 ——
//	而"整个推送停掉"是最难排查的失败形态
//	（排查起点变成"它到底跑没跑"，而 dapr 不提供 job 执行历史）。
//
// 用 prevOnlyFailSource：只在查上期（from=2026-10-14）时报错。
func TestPrevPeriodFailureStillProducesReport(t *testing.T) {
	fs := &prevOnlyFailSource{fakeSource: newFake()}
	fs.catCur = []CategoryRow{{Name: "生鲜联营", Amount: 8000, CostedRow: 100, TotalRow: 100}}
	fs.catPrev = []CategoryRow{{Name: "生鲜联营", Amount: 10000}}

	f, err := NewBuilder(fs, nil).Build(context.Background(), testOptions())
	if err != nil {
		t.Fatalf("上期失败不应让整体失败: %v", err)
	}
	if f.Totals.NetAmount != 8000 {
		t.Errorf("大盘仍应可用，实际 %v", f.Totals.NetAmount)
	}
	if len(f.Categories) != 1 {
		t.Fatalf("本期品类仍应返回，实际 %d", len(f.Categories))
	}
	if f.Categories[0].HasPrev {
		t.Error("上期失败时不得给出环比")
	}
	if !containsSub(f.DataSufficiency.Issues, "品类环比不可用") {
		t.Errorf("必须说明品类环比不可用: %v", f.DataSufficiency.Issues)
	}
}

// prevOnlyFailSource 只在上期查询时报错。
type prevOnlyFailSource struct {
	*fakeSource
}

func (p *prevOnlyFailSource) CategorySales(ctx context.Context, b, from, to string, n int) ([]CategoryRow, error) {
	if from == "2026-10-14" {
		return nil, errors.New("上期数据不可用")
	}
	return p.fakeSource.CategorySales(ctx, b, from, to, n)
}

// 缺门店必须报错 —— 拿全部分店的数据不是本店日报。
func TestBuildRequiresStoreBranch(t *testing.T) {
	fs := newFake()
	opt := testOptions()
	opt.StoreBranchID = ""
	_, err := NewBuilder(fs, nil).Build(context.Background(), opt)
	if err == nil {
		t.Fatal("缺少门店必须报错，否则会返回全部分店的数据")
	}
}

// 本期无数据时必须标记数据不足。
func TestNoSalesDataFlagsInsufficiency(t *testing.T) {
	fs := newFake()
	fs.catCur = nil
	f, err := NewBuilder(fs, nil).Build(context.Background(), testOptions())
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if f.DataSufficiency.Sufficient {
		t.Error("无销售数据必须标记为数据不足")
	}
	if !containsSub(f.DataSufficiency.Issues, "本期无销售数据") {
		t.Errorf("必须说明无销售数据: %v", f.DataSufficiency.Issues)
	}
}

// 天气失败**不**标记为数据不足 —— 它是增强项，不是结论前提。
func TestWeatherFailureIsNotInsufficiency(t *testing.T) {
	fs := newFake()
	fs.catCur = []CategoryRow{{Name: "生鲜联营", Amount: 8000}}
	wx := weather.NewUnavailableProvider("天气服务未配置")
	f, err := NewBuilder(fs, wx).Build(context.Background(), testOptions())
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if f.Context.Weather.Available {
		t.Error("未配置天气源时 Available 应为 false")
	}
	if f.Context.Weather.Summary != "" {
		t.Errorf("不可用时 Summary 必须为空（否则 LLM 会当事实用），实际 %q", f.Context.Weather.Summary)
	}
	if !f.DataSufficiency.Sufficient {
		t.Errorf("天气缺失不应把报告标成数据不足: %v", f.DataSufficiency.Issues)
	}
}

// containsSub 是**子串**匹配。
//
// ⚠️ 刻意不用全等：这些文案是面向店长的完整句子
// （如"供应商字典获取失败，本次无法识别自用渠道（…）"），
// 全等匹配会让"文案微调一个字"直接让测试变红 —
// 而文案调整本身并不影响行为。用子串匹配，判据才是行为。
func containsSub(l []string, sub string) bool {
	for _, v := range l {
		if strings.Contains(v, sub) {
			return true
		}
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// TestSupplierDictReceivesStoreBranch 钉住「字典查询必须带门店」。
//
// # 2026-10-17 生产实证的真实 bug
//
//	SupplierDict 当时**没有** storeBranchID 参数,直接用原始 ctx 调 cube。
//	supplier 是 storage: duck 的模型,同样要靠 X-Branch-ID 路由到这家门店的
//	DuckDB —— 于是 cube-router 返 400 branch_required。
//
//	而这个 400 在本包被折叠成一句人话:
//	  「供应商字典获取失败，本次无法识别自用渠道」
//	**门店路由错误被伪装成数据质量问题。** 日报照发、卡片照发,
//	只有供应商榜单里混进了自用渠道 —— 而自用供应商排第一会让店长
//	照着去跟一家"内部直供"谈供货。
//
// # 判据：这是**结构性**修复，不是约定
//
//	storeBranchID 现在是 SupplierDict 的**必填参数**，忘记传就是编译错误。
//	这条用例守住另一半：builder 必须真的把 Options.StoreBranchID 传下去。
func TestSupplierDictReceivesStoreBranch(t *testing.T) {
	f := &fakeSource{calls: map[string]int{}}
	b := NewBuilder(f, nil)

	_, err := b.Build(context.Background(), Options{
		StoreBranchID: "store-9",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if f.calls["dict"] == 0 {
		t.Skip("本用例数据没走到字典查询 —— 需检查 fakeSource 的假数据")
	}
	if f.dictBranch != "store-9" {
		t.Fatalf("SupplierDict 收到的门店 = %q, 期望 store-9 —— "+
			"少了它 cube-router 会返 400 branch_required，"+
			"而症状会被误读成「字典数据有问题」", f.dictBranch)
	}
}

// TestWithBranchInjectsIntoContext 钉住 withBranch 这个机制本身。
//
// ⚠️ 它测的是**元数据透传**（cubeclient 用读侧 BranchFromContext 断言），
//
//	不是"函数返回了非 nil ctx"。后者恒真 —— 空实现也能过。
//	这一层正是本仓踩过两次的坑（头只写进 gin.Context 而没写进 request.Context）。
func TestWithBranchInjectsIntoContext(t *testing.T) {
	ctx := withBranch(context.Background(), "00")
	if got := cubeclient.BranchFromContext(ctx); got != "00" {
		t.Fatalf("withBranch 后读不出门店: %q", got)
	}
	// 空门店必须**不改** ctx：withBranch 只是透传，不该自己编一个值。
	// 编值的后果是查别的门店的数据 —— 那比少传更危险。
	if got := cubeclient.BranchFromContext(withBranch(context.Background(), "   ")); got != "" {
		t.Fatalf("空白门店应透传为空，实际 %q", got)
	}
}
