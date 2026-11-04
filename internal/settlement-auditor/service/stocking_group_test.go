package service

import (
	"context"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// 本文件覆盖「组级备货核对」（2026-11-01 用户口径）。
//
// ## 最容易假绿的三处
//
//  1. **组没真的归并**。父商品进货 1 件、子商品卖出 12 罐，若不换算到同一
//     单位，组里就成了"进货 1、销售 12"或"进货 12、销售 12"的巧合数字。
//     这里用 1 件 = 12 罐、销量刻意写成 12 的整数倍，任何漏换算都会露馅。
//  2. **判定只看有没有信号，不看级别**。warn 不阻断、block 才阻断，
//     二者混为一谈会让"只是提示"的单被拒（生产实踩）。
//  3. **文案带恶意揣测**。信号是给店长和 boss 看的，措辞必须中性。

var _ groupQuerier = (*fakeGroupCube)(nil)

// fakeGroupCube 让 fakeCube 额外实现 groupQuerier（组级备货核对用例专用）。
//
// ⚠️ 刻意**不**把这三个方法加到 fakeCube 上：既有的盘损降级 / CP 库存异常
// 用例锁的是「单 SKU 老口径」的判定与降级路径，给 fakeCube 加方法会让它们
// 静默改走组口径、测不到老口径。组口径由本文件的用例显式 opt-in。
type fakeGroupCube struct {
	*fakeCube
	combos       []ComboRelation
	productUnits map[string]ProductUnitInfo
	categories   map[string]CategoryInfo
	purchaseHist []PurchaseHistoryRow
}

func (f *fakeGroupCube) ListItemCombos(_ context.Context, _ string) ([]ComboRelation, error) {
	return f.combos, nil
}

func (f *fakeGroupCube) ListProductCategories(_ context.Context, _ string, categoryIDs []string) (map[string]CategoryInfo, error) {
	if len(f.categories) == 0 {
		return nil, nil
	}
	want := make(map[string]bool, len(categoryIDs))
	for _, id := range categoryIDs {
		want[id] = true
	}
	out := make(map[string]CategoryInfo, len(categoryIDs))
	for id, c := range f.categories {
		if want[id] {
			out[id] = c
		}
	}
	return out, nil
}

func (f *fakeGroupCube) ListProductUnits(_ context.Context, _ string, productIDs []string) (map[string]ProductUnitInfo, error) {
	if len(f.productUnits) == 0 {
		return nil, nil
	}
	want := make(map[string]bool, len(productIDs))
	for _, id := range productIDs {
		want[id] = true
	}
	out := make(map[string]ProductUnitInfo, len(productIDs))
	for id, u := range f.productUnits {
		if want[id] {
			out[id] = u
		}
	}
	return out, nil
}

func (f *fakeGroupCube) ListProductPurchaseHistory(_ context.Context, _ string, productIDs []string, _, _ string) ([]PurchaseHistoryRow, error) {
	if len(f.purchaseHist) == 0 {
		return nil, nil
	}
	want := make(map[string]bool, len(productIDs))
	for _, id := range productIDs {
		want[id] = true
	}
	var out []PurchaseHistoryRow
	for _, r := range f.purchaseHist {
		if want[r.ProductID] {
			out = append(out, r)
		}
	}
	return out, nil
}

// bannedWords 是结论/信号里**禁止出现**的揣测性措辞（2026-11-01 用户口径）。
var bannedWords = []string{
	"嫌疑", "疑似", "可能", "内盗", "回扣", "虚假入库", "违规", "异常行为", "态度",
}

func assertNeutralWording(t *testing.T, msgs []string) {
	t.Helper()
	for _, m := range msgs {
		for _, w := range bannedWords {
			if strings.Contains(m, w) {
				t.Errorf("信号文案含揣测性措辞 %q: %s", w, m)
			}
		}
	}
}

func signalTypesOf(drafts []signalDraft) map[string]string {
	out := make(map[string]string, len(drafts))
	for _, d := range drafts {
		out[d.SignalType] = d.Level
	}
	return out
}

// TestComputeGroupStockingFacts_ComboMerge 是组归并的口径锁：
// 1 件 = 12 罐，父商品历史进货、子商品销量/库存/本次进货都换算到「件」再相加。
func TestComputeGroupStockingFacts_ComboMerge(t *testing.T) {
	in := groupStockingInput{
		Products: []string{"P2"}, // 审批里只有子商品（整件进货、拆散零售）
		Combos:   []ComboRelation{{ParentID: "P1", ChildID: "P2", Quantity: dec("12")}},
		Units: map[string]ProductUnitInfo{
			"P1": {Unit: "件"},
			"P2": {Unit: "罐"},
		},
		Histories: map[string]*productHistory{
			"P1": {
				ProductID: "P1",
				Unit:      ProductUnitInfo{Unit: "件"},
				Receipts: []productHistoryReceipt{
					{SheetID: "H1", Date: "2026-05-01", Doc: "PI", Qty: dec("10"), Amount: dec("600"), UnitNo: "件", UnitFactor: dec("1")},
					{SheetID: "H2", Date: "2026-08-01", Doc: "PI", Qty: dec("12"), Amount: dec("720"), UnitNo: "件", UnitFactor: dec("1")},
				},
			},
			"P2": {
				ProductID: "P2",
				// 154 件 × 12 罐 = 1848 罐 ⇒ 组销量 154 件。
				SalesQty:       dec("1848"),
				RecentSalesQty: dec("360"),
				Unit:           ProductUnitInfo{Unit: "罐"},
			},
		},
		Stock: map[string]decimal.Decimal{"P2": dec("240")},
		PILines: []SheetLineRow{{
			SheetID: "PI-CUR", ProductID: "P2", ReceivedQuantity: dec("120"),
			TotalAmountYuan: dec("600"), UnitNo: "罐",
		}},
		Purchases: map[string]purchaseBasis{
			"P2": {NetQty: dec("120"), Days: 1, FirstDate: "2026-09-25", LastDate: "2026-09-25"},
		},
		Names: map[string]string{"P1": "可乐(整件)", "P2": "可乐"},
		Today: "2026-10-01",
	}
	res := computeGroupStockingFacts(in)
	if len(res.Facts) != 1 {
		t.Fatalf("应给出 1 个商品组, got %d", len(res.Facts))
	}
	g := res.Facts[0]
	if g.MemberCount != 2 || len(g.MemberIDs) != 2 || g.MemberIDs[0] != "P1" || g.MemberIDs[1] != "P2" {
		t.Fatalf("组成员应归并为 P1+P2, got %+v", g.MemberIDs)
	}
	if g.BaseUnit != "件" {
		t.Errorf("基准单位应为父商品的「件」, got %q", g.BaseUnit)
	}
	// 本次进货 120 罐 = 10 件；库存 240 罐 = 20 件；销量 1848 罐 = 154 件。
	if !g.ReceivedQty.Equal(dec("10")) || !g.StockQty.Equal(dec("20")) {
		t.Errorf("换算不对: 进货 %s 件、库存 %s 件 (want 10/20)", g.ReceivedQty, g.StockQty)
	}
	if !g.SalesQty.Equal(dec("154")) {
		t.Errorf("组销量 = %s 件, want 154", g.SalesQty)
	}
	// 有货天数 2026-05-01~2026-10-01 = 154 天 ⇒ 日均 1；
	// 补货后 30 件，扣掉正常陈列（历史单批中位数 11 件）⇒ 可售 19 件 = 19 天。
	if g.InStockDays != 154 {
		t.Errorf("有货天数 = %d, want 154", g.InStockDays)
	}
	if !g.AvgDailySales.Equal(dec("1")) || !g.CoverageDays.Equal(dec("19")) {
		t.Errorf("日均/扣除陈列后可售天数不对: %s / %s (want 1 / 19)", g.AvgDailySales, g.CoverageDays)
	}
	if !g.DisplayQty.Equal(dec("11")) {
		t.Errorf("陈列保有量应取历史单批中位数 11 件, got %s", g.DisplayQty)
	}
	if g.CapDays != 45 || !g.CapQty.Equal(dec("45")) {
		t.Errorf("饮料档位应为 45 天 / 45 件, got %d / %s", g.CapDays, g.CapQty)
	}
	// 历史两次进货 10/12 件 ⇒ 单批中位数 11 件（陈列保有量取它）。
	if !g.SingleMedian.Equal(dec("11")) || !g.DisplayQty.Equal(dec("11")) {
		t.Errorf("单批中位数/陈列保有量不对: %s / %s", g.SingleMedian, g.DisplayQty)
	}
	if g.HistoryReceiptCount != 2 {
		t.Errorf("历史进货次数 = %d, want 2(不含本次)", g.HistoryReceiptCount)
	}
	if g.LastReceipt != "2026-09-25" {
		t.Errorf("最后收货日应为本次收货日, got %q", g.LastReceipt)
	}
	if g.ZeroSalesCase != "" || g.Unconvertible || g.AllowanceBoost {
		t.Errorf("不该落零销量/不可换算/放宽分支: %+v", g)
	}
	if !strings.Contains(g.Label, "组合商品 2 个") {
		t.Errorf("组描述应注明组合商品: %q", g.Label)
	}
	// 陈列口径：可售量 = 库存 + 进货 − 陈列保有量，本次只算正常补货，不应出超量信号。
	msgs := checkGroupQuantity(res.Facts, 2.0, in.Today)
	for _, m := range msgs {
		if m.SignalType == SigQtyDeviation || m.SignalType == SigStockingExcess {
			t.Errorf("正常补货不该出超量信号: %s", m.Message)
		}
		if m.Level == model.SignalBlock {
			t.Errorf("不该有阻断: %s", m.Message)
		}
	}
}

// TestComputeGroupStockingFacts_AllowanceBoostCap 有陈列费/堆头费 ⇒ 陈列保有量 ×2。
func TestComputeGroupStockingFacts_AllowanceBoostCap(t *testing.T) {
	in := groupStockingInput{
		Products:       []string{"P1"},
		Histories:      map[string]*productHistory{"P1": {ProductID: "P1", SalesQty: dec("30"), RecentSalesQty: dec("10"), Unit: ProductUnitInfo{Unit: "瓶"}}},
		PILines:        []SheetLineRow{{SheetID: "PI-CUR", ProductID: "P1", ReceivedQuantity: dec("10"), TotalAmountYuan: dec("50")}},
		Stock:          map[string]decimal.Decimal{"P1": dec("5")},
		Names:          map[string]string{"P1": "矿泉水"},
		AllowanceBoost: true,
		Today:          "2026-10-01",
	}
	res := computeGroupStockingFacts(in)
	if len(res.Facts) != 1 {
		t.Fatalf("应给出 1 个商品组, got %d", len(res.Facts))
	}
	// ⚠️ 2026-11-02 口径：放宽的是**陈列量**，可售上限天数不再 ×1.5。
	if got := res.Facts[0].CapDays; got != 45 {
		t.Errorf("可售上限不应再放大, got %d want 45", got)
	}
	// 没有历史进货 ⇒ 陈列基数 1 瓶，有陈列费 ⇒ 生效陈列量 2 瓶。
	if !res.Facts[0].DisplayQty.Equal(dec("2")) {
		t.Errorf("有陈列费时陈列保有量应 ×2 = 2 瓶, got %s", res.Facts[0].DisplayQty)
	}
	if !res.Facts[0].AllowanceBoost {
		t.Errorf("应标记 AllowanceBoost")
	}
}

// TestComputeGroupStockingFacts_ManualProfileKept 人工档位优先，且不再生成自动行。
func TestComputeGroupStockingFacts_ManualProfileKept(t *testing.T) {
	in := groupStockingInput{
		Products:  []string{"P1"},
		Histories: map[string]*productHistory{"P1": {ProductID: "P1", SalesQty: dec("60"), RecentSalesQty: dec("20"), Unit: ProductUnitInfo{Unit: "瓶"}}},
		PILines:   []SheetLineRow{{SheetID: "PI-CUR", ProductID: "P1", ReceivedQuantity: dec("8"), TotalAmountYuan: dec("40")}},
		Stock:     map[string]decimal.Decimal{"P1": dec("10")},
		Names:     map[string]string{"P1": "抽纸"},
		Profiles: map[string]model.ProductProfile{
			"P1": {ProductID: "P1", Class: model.ProfileClassDaily, CapDays: 90, Source: model.ProfileSourceManual},
		},
		Today: "2026-10-01",
	}
	res := computeGroupStockingFacts(in)
	if len(res.Facts) != 1 {
		t.Fatalf("应给出 1 个商品组, got %d", len(res.Facts))
	}
	if len(res.Profiles) != 0 {
		t.Errorf("人工经营属性不该再生成建议: %+v", res.Profiles)
	}
	// 人工档位 90 天优先于品名（抽纸本身也是 90 天，这里再锁一次取值路径）。
	if res.Facts[0].CapDays != 90 {
		t.Errorf("可售上限应取人工值 90, got %d", res.Facts[0].CapDays)
	}
}

// TestComputeGroupStockingFacts_ZeroSalesCases 零销量四分支的分类口径。
func TestComputeGroupStockingFacts_ZeroSalesCases(t *testing.T) {
	today := "2026-10-01"
	base := func() groupStockingInput {
		return groupStockingInput{
			Products: []string{"P1"},
			Units:    map[string]ProductUnitInfo{"P1": {Unit: "罐"}},
			PILines: []SheetLineRow{{
				SheetID: "PI-CUR", ProductID: "P1", ReceivedQuantity: dec("10"), TotalAmountYuan: dec("50"), UnitNo: "罐",
			}},
			Names: map[string]string{"P1": ""},
			Today: today,
		}
	}
	historyWith := func(date string) map[string]*productHistory {
		return map[string]*productHistory{"P1": {
			ProductID: "P1",
			Unit:      ProductUnitInfo{Unit: "罐"},
			Receipts: []productHistoryReceipt{
				{SheetID: "H1", Date: date, Doc: "PI", Qty: dec("10"), Amount: dec("50"), UnitNo: "罐", UnitFactor: dec("1")},
			},
		}}
	}

	t.Run("recent_receipt", func(t *testing.T) {
		in := base()
		// 本次到货 2 天前 ⇒ 刚收货，不判定。
		in.Purchases = map[string]purchaseBasis{"P1": {NetQty: dec("10"), Days: 1, FirstDate: "2026-09-29", LastDate: "2026-09-29"}}
		res := computeGroupStockingFacts(in)
		if res.Facts[0].ZeroSalesCase != "recent_receipt" {
			t.Errorf("ZeroSalesCase = %q, want recent_receipt", res.Facts[0].ZeroSalesCase)
		}
	})
	t.Run("new_product", func(t *testing.T) {
		in := base() // 无历史进货、无本次收货日之外的记录
		res := computeGroupStockingFacts(in)
		if res.Facts[0].ZeroSalesCase != "new_product" {
			t.Errorf("ZeroSalesCase = %q, want new_product", res.Facts[0].ZeroSalesCase)
		}
	})
	t.Run("long_stock", func(t *testing.T) {
		in := base()
		in.Histories = historyWith("2026-03-01") // 214 天前
		in.Stock = map[string]decimal.Decimal{"P1": dec("20")}
		res := computeGroupStockingFacts(in)
		if res.Facts[0].ZeroSalesCase != "long_stock" {
			t.Errorf("ZeroSalesCase = %q, want long_stock", res.Facts[0].ZeroSalesCase)
		}
	})
	t.Run("ledger_gap", func(t *testing.T) {
		in := base()
		in.Histories = historyWith("2026-09-21") // 10 天前，有库存无销量但不算长期
		res := computeGroupStockingFacts(in)
		if res.Facts[0].ZeroSalesCase != "ledger_gap" {
			t.Errorf("ZeroSalesCase = %q, want ledger_gap", res.Facts[0].ZeroSalesCase)
		}
	})
}

// ---- 盘损并入（2026-11-01 用户口径：组消耗 = 销售 + 盘损） ----

// TestComputeGroupStockingFacts_LossCountsAsConsumption 锁「盘损也是货的去向」。
//
// 进货 60、销售 1、报损 29、库存 20、本次再进 10 ⇒ 结存 60+10−1−29−20 = 20；
// 日均消耗按 (1+29)/154 天算，而不是只看销售的 1/154。
func TestComputeGroupStockingFacts_LossCountsAsConsumption(t *testing.T) {
	in := groupStockingInput{
		Products: []string{"P1"},
		Units:    map[string]ProductUnitInfo{"P1": {Unit: "件"}},
		Histories: map[string]*productHistory{"P1": {
			ProductID:      "P1",
			SalesQty:       dec("1"),
			RecentSalesQty: dec("1"),
			LossQty:        dec("29"),
			Unit:           ProductUnitInfo{Unit: "件"},
			Receipts: []productHistoryReceipt{
				{SheetID: "H1", Date: "2026-05-01", Doc: "PI", Qty: dec("60"), Amount: dec("600"), UnitNo: "件", UnitFactor: dec("1")},
			},
		}},
		Stock: map[string]decimal.Decimal{"P1": dec("20")},
		PILines: []SheetLineRow{{
			SheetID: "PI-CUR", ProductID: "P1", ReceivedQuantity: dec("10"), TotalAmountYuan: dec("100"), UnitNo: "件",
		}},
		Purchases: map[string]purchaseBasis{
			"P1": {NetQty: dec("10"), Days: 1, FirstDate: "2026-09-25", LastDate: "2026-09-25"},
		},
		Names: map[string]string{"P1": "测试商品"},
		Today: "2026-10-01",
	}
	res := computeGroupStockingFacts(in)
	if len(res.Facts) != 1 {
		t.Fatalf("应给出 1 个商品组, got %d", len(res.Facts))
	}
	g := res.Facts[0]
	if !g.LossQty.Equal(dec("29")) {
		t.Errorf("组盘损 = %s, want 29", g.LossQty)
	}
	// 有货 2026-05-01~2026-10-01 = 154 天 ⇒ 日均消耗 = (1+29)/154 = 0.1948。
	if !g.AvgDailySales.Equal(dec("0.1948")) {
		t.Errorf("日均消耗 = %s, want 0.1948((销量+盘损)/有货天数)", g.AvgDailySales)
	}
	// 结存差异扣掉盘损：60+10−1−29−20 = 20（不扣盘损会算成 49）。
	if !g.LedgerGap.Equal(dec("20")) {
		t.Errorf("账面结存差异 = %s, want 20(已扣盘损)", g.LedgerGap)
	}
	if text := groupStockText(g); !strings.Contains(text, "组盘损 29 件") || !strings.Contains(text, "日均消耗") {
		t.Errorf("组消息应写出盘损与日均消耗口径: %s", text)
	}
	if ev := groupEvidence(g); ev["loss_qty"] != "29" {
		t.Errorf("证据里 loss_qty = %v, want 29", ev["loss_qty"])
	}
}

// TestCheckGroupQuantity_LossClosesLedgerGap 锁「盘损扣掉后结存对得上 ⇒ 不再提示缺口」。
func TestCheckGroupQuantity_LossClosesLedgerGap(t *testing.T) {
	ok := StockingGroupFact{
		GroupKey: "P1", ProductID: "P1", Label: "可乐-P1", BaseUnit: "件",
		ReceivedQty: dec("20"),
		SalesQty:    dec("5"), LossQty: dec("95"), StockQty: dec("20"),
		LedgerGap: decimal.Zero, SampleDays: 180, CapDays: 45,
	}
	if got := checkGroupQuantity([]StockingGroupFact{ok}, 2.0, "2026-10-01"); len(got) != 0 {
		t.Errorf("进货−销售−盘损−库存 = 0 ⇒ 不该出信号, got %+v", got)
	}
	// 盘损缺失（等同没读到盘损）：同样的数据会算出 95 的缺口 ⇒ warn。
	missing := ok
	missing.LossQty = decimal.Zero
	missing.LedgerGap = dec("95")
	got := checkGroupQuantity([]StockingGroupFact{missing}, 2.0, "2026-10-01")
	ty, found := signalTypesOf(got)[SigStockLedgerGap]
	if !found || ty != model.SignalWarn {
		t.Fatalf("缺盘损时应出库存差异提示, got %+v", got)
	}
	if !strings.Contains(got[0].Message, "进货−销售−盘损−库存") {
		t.Errorf("差额公式应写明扣了盘损: %s", got[0].Message)
	}
}

// TestCheckGroupQuantity_ZeroSalesMentionsLoss 零销量但有盘损 ⇒ 阻断消息带上盘损事实。
func TestCheckGroupQuantity_ZeroSalesMentionsLoss(t *testing.T) {
	g := StockingGroupFact{
		GroupKey: "P1", ProductID: "P1", Label: "滞销-P1", BaseUnit: "件",
		ReceivedQty: dec("10"), StockQty: dec("20"), LastReceipt: "2026-03-01",
		LossQty: dec("8"), ZeroSalesCase: "long_stock", CapDays: 45, SampleDays: 180,
	}
	got := checkGroupQuantity([]StockingGroupFact{g}, 2.0, "2026-10-01")
	if ty, ok := signalTypesOf(got)[SigPurchaseWithoutSales]; !ok || ty != model.SignalBlock {
		t.Fatalf("长期无销售应阻断, got %+v", got)
	}
	if !strings.Contains(got[0].Message, "期间盘损 8 件") {
		t.Errorf("阻断消息应带上盘损事实: %s", got[0].Message)
	}
}

// ---- checkGroupQuantity ----

func TestCheckGroupQuantity_WithinCap(t *testing.T) {
	g := StockingGroupFact{
		GroupKey: "P1", ProductID: "P1", Label: "可乐-P1", BaseUnit: "件",
		ReceivedQty: dec("20"),
		CapDays:     45, CoverageDays: dec("40"), ExcessYuan: decimal.Zero,
	}
	if got := checkGroupQuantity([]StockingGroupFact{g}, 2.0, "2026-10-01"); len(got) != 0 {
		t.Errorf("补货后可售不超上限 ⇒ 不该出信号, got %+v", got)
	}
}

func TestCheckGroupQuantity_OverCapWarnOnly(t *testing.T) {
	g := StockingGroupFact{
		GroupKey: "P1", ProductID: "P1", Label: "可乐-P1", BaseUnit: "件",
		ReceivedQty: dec("200"),
		StockQty:    dec("10"), AfterQty: dec("210"), SalesQty: dec("90"), CapQty: dec("180"),
		CapDays: 45, CoverageDays: dec("60"), ExcessYuan: dec("50"),
	}
	got := checkGroupQuantity([]StockingGroupFact{g}, 2.0, "2026-10-01")
	if ty, ok := signalTypesOf(got)[SigStockingExcess]; !ok || ty != model.SignalWarn {
		t.Fatalf("轻微超上限应只 warn, got %+v", got)
	}
	if hasBlock(got) {
		t.Errorf("轻微超上限不该阻断: %+v", got)
	}
	msgs := make([]string, 0, len(got))
	for _, d := range got {
		msgs = append(msgs, d.Message)
	}
	assertNeutralWording(t, msgs)
	if !strings.Contains(got[0].Message, "扣除正常陈列后可售 60.0 天") ||
		!strings.Contains(got[0].Message, "可售上限 45 天") {
		t.Errorf("消息应给出天数事实: %s", got[0].Message)
	}
}

func TestCheckGroupQuantity_SevereBlocks(t *testing.T) {
	g := StockingGroupFact{
		GroupKey: "P1", ProductID: "P1", Label: "可乐-P1", BaseUnit: "件",
		ReceivedQty: dec("500"),
		StockQty:    dec("10"), AfterQty: dec("510"), SalesQty: dec("90"), CapQty: dec("180"),
		CapDays: 45, CoverageDays: dec("100"), ExcessYuan: dec("100"),
	}
	got := checkGroupQuantity([]StockingGroupFact{g}, 2.0, "2026-10-01")
	ty, ok := signalTypesOf(got)[SigQtyDeviation]
	if !ok || ty != model.SignalBlock {
		t.Fatalf("超过上限×2 应阻断, got %+v", got)
	}
	assertNeutralWording(t, []string{got[0].Message})
}

func TestCheckGroupQuantity_ExcessAmountBlocks(t *testing.T) {
	g := StockingGroupFact{
		GroupKey: "P1", ProductID: "P1", Label: "可乐-P1", BaseUnit: "件",
		ReceivedQty: dec("100"),
		StockQty:    dec("10"), AfterQty: dec("110"), SalesQty: dec("90"), CapQty: dec("90"),
		CapDays: 45, CoverageDays: dec("50"), ExcessYuan: dec("400"), // > 300 元
	}
	got := checkGroupQuantity([]StockingGroupFact{g}, 2.0, "2026-10-01")
	if ty, ok := signalTypesOf(got)[SigQtyDeviation]; !ok || ty != model.SignalBlock {
		t.Fatalf("超量金额 >300 元应阻断, got %+v", got)
	}
}

func TestCheckGroupQuantity_UnconvertibleWarnOnly(t *testing.T) {
	g := StockingGroupFact{
		GroupKey: "P1", ProductID: "P1", Label: "可乐-P1", BaseUnit: "瓶",
		ReceivedQty: dec("100"),
		CapDays:     45, CoverageDays: dec("300"), ExcessYuan: dec("9000"),
		Unconvertible: true,
		UnitNote:      "采购单位「件」与商品档案单位「瓶」不一致",
	}
	got := checkGroupQuantity([]StockingGroupFact{g}, 2.0, "2026-10-01")
	if ty, ok := signalTypesOf(got)[SigUnitUnconvertible]; !ok || ty != model.SignalWarn {
		t.Fatalf("单位不可换算应只 warn, got %+v", got)
	}
	if hasBlock(got) {
		t.Errorf("单位不可换算不该阻断(无法比较不等于超量): %+v", got)
	}
	if !strings.Contains(got[0].Message, "未做数量比较") {
		t.Errorf("消息应说明未做数量比较: %s", got[0].Message)
	}
}

func TestCheckGroupQuantity_ZeroSalesSignalLevels(t *testing.T) {
	today := "2026-10-01"
	cases := []struct {
		name     string
		fact     StockingGroupFact
		wantType string
		wantLvl  string
	}{
		{
			name: "recent_receipt",
			fact: StockingGroupFact{ProductID: "P1", Label: "可乐-P1", BaseUnit: "件",
				ReceivedQty: dec("10"), LastDate: "2026-09-29", ZeroSalesCase: "recent_receipt", CapDays: 45},
			wantType: SigRecentReceiptNoSales, wantLvl: model.SignalWarn,
		},
		{
			name: "new_product",
			fact: StockingGroupFact{ProductID: "P1", Label: "新品-P1", BaseUnit: "件",
				ReceivedQty: dec("10"), ZeroSalesCase: "new_product", CapDays: 45},
			wantType: SigNewProductNoSales, wantLvl: model.SignalWarn,
		},
		{
			name: "long_stock",
			fact: StockingGroupFact{ProductID: "P1", Label: "滞销-P1", BaseUnit: "件",
				ReceivedQty: dec("10"), StockQty: dec("20"), LastReceipt: "2026-03-01",
				ZeroSalesCase: "long_stock", CapDays: 45},
			wantType: SigPurchaseWithoutSales, wantLvl: model.SignalBlock,
		},
		{
			name: "ledger_gap",
			fact: StockingGroupFact{ProductID: "P1", Label: "可乐-P1", BaseUnit: "件",
				ReceivedQty: dec("10"), StockQty: dec("5"), ZeroSalesCase: "ledger_gap", CapDays: 45},
			wantType: SigStockLedgerGap, wantLvl: model.SignalWarn,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := checkGroupQuantity([]StockingGroupFact{c.fact}, 2.0, today)
			lvl, ok := signalTypesOf(got)[c.wantType]
			if !ok {
				t.Fatalf("缺少信号 %s, got %+v", c.wantType, got)
			}
			if lvl != c.wantLvl {
				t.Errorf("%s 级别 = %s, want %s", c.wantType, lvl, c.wantLvl)
			}
			if c.wantLvl == model.SignalBlock && !strings.Contains(got[0].Message, "需盘点核对库存") {
				t.Errorf("阻断消息应注明盘点条件: %s", got[0].Message)
			}
			msgs := make([]string, 0, len(got))
			for _, d := range got {
				msgs = append(msgs, d.Message)
			}
			assertNeutralWording(t, msgs)
		})
	}
}

func TestCheckGroupQuantity_PriceRiseAndLedgerWarn(t *testing.T) {
	g := StockingGroupFact{
		GroupKey: "P1", ProductID: "P1", Label: "可乐-P1", BaseUnit: "件",
		ReceivedQty: dec("10"), // 数量判定跳过，只留价格/账实事实
		CapDays:     45, PriceThis: dec("11.5"), PriceHistory: dec("10"), PriceRisePct: dec("15"),
		SalesQty: dec("10"), StockQty: dec("10"), LedgerGap: dec("50"),
	}
	got := checkGroupQuantity([]StockingGroupFact{g}, 2.0, "2026-10-01")
	types := signalTypesOf(got)
	if lvl, ok := types[SigPurchasePriceAboveHistory]; !ok || lvl != model.SignalWarn {
		t.Errorf("进价高于历史均价应只 warn, got %+v", got)
	}
	if lvl, ok := types[SigStockLedgerGap]; !ok || lvl != model.SignalWarn {
		t.Errorf("账实差异应只 warn, got %+v", got)
	}
	if hasBlock(got) {
		t.Errorf("价格/账实差异不该阻断: %+v", got)
	}
	for _, d := range got {
		assertNeutralWording(t, []string{d.Message})
	}
}

// ---- 自动标记落库 ----

func TestStoreStockingSuggestions_PendingAutoManual(t *testing.T) {
	svc, db, _, _, _ := newTestService(t)
	ctx := context.Background()
	gk := "P1|P2"
	res := &groupStockingResult{
		Facts: []StockingGroupFact{{
			GroupKey: gk, ProductID: "P1", MemberIDs: []string{"P1", "P2"},
		}},
		Profiles: map[string]model.ProductProfile{
			"P1": {ProductID: "P1", ProductName: "可乐", Class: model.ProfileClassBeverage, CapDays: 45, ShelfLifeDays: 365},
			"P2": {ProductID: "P2", ProductName: "可乐罐", Class: model.ProfileClassBeverage, CapDays: 45, ShelfLifeDays: 365},
		},
	}
	// 人工改过的行（P1）自动标记永不覆盖。
	if err := db.Create(&model.ProductProfile{
		ID: "pp-manual", BranchID: "B1", ProductID: "P1", ProductName: "可乐",
		Class: model.ProfileClassBeverage, CapDays: 30, Source: model.ProfileSourceManual,
	}).Error; err != nil {
		t.Fatalf("seed manual: %v", err)
	}

	// 本次组被阻断 ⇒ 其余行落 pending，等人工确认。
	svc.storeStockingSuggestions(ctx, "B1", res, map[string]bool{gk: true}, fixedNow)

	var manual model.ProductProfile
	if err := db.First(&manual, "branch_id = ? AND product_id = ?", "B1", "P1").Error; err != nil {
		t.Fatalf("读 P1: %v", err)
	}
	if manual.Source != model.ProfileSourceManual || manual.CapDays != 30 {
		t.Errorf("人工行被覆盖了: %+v", manual)
	}
	var pending model.ProductProfile
	if err := db.First(&pending, "branch_id = ? AND product_id = ?", "B1", "P2").Error; err != nil {
		t.Fatalf("应落 P2 的自动标记: %v", err)
	}
	if pending.Source != model.ProfileSourcePending || pending.CapDays != 45 {
		t.Errorf("被阻断组的自动标记应为 pending, got %+v", pending)
	}

	// 下一次没有被阻断 ⇒ 自动行改回 auto。
	svc.storeStockingSuggestions(ctx, "B1", res, nil, fixedNow.AddDate(0, 0, 1))
	if err := db.First(&pending, "branch_id = ? AND product_id = ?", "B1", "P2").Error; err != nil {
		t.Fatalf("读 P2: %v", err)
	}
	if pending.Source != model.ProfileSourceAuto {
		t.Errorf("无阻断时应回到 auto, got %s", pending.Source)
	}
}

// TestQuantityBlockedProducts_OnlyBlockLevel 只有 block 级数量信号才让组落 pending。
func TestQuantityBlockedProducts_OnlyBlockLevel(t *testing.T) {
	drafts := []signalDraft{
		{Level: model.SignalWarn, SignalType: SigQtyDeviation, ProductID: "P1"},
		{Level: model.SignalBlock, SignalType: SigQtyDeviation, ProductID: "P2"},
		{Level: model.SignalBlock, SignalType: SigStockLedgerGap, ProductID: "P3"},
	}
	blocked := quantityBlockedProducts(drafts)
	if _, ok := blocked["P1"]; ok {
		t.Errorf("warn 不该算阻断")
	}
	if _, ok := blocked["P2"]; !ok {
		t.Errorf("block 级数量信号应算阻断")
	}
	if _, ok := blocked["P3"]; ok {
		t.Errorf("账实差异不是数量类阻断")
	}
	facts := []StockingGroupFact{
		{GroupKey: "P1|P9", ProductID: "P1"},
		{GroupKey: "P2", ProductID: "P2"},
	}
	got := blockedGroupKeys(facts, blocked)
	if !got["P2"] || got["P1|P9"] {
		t.Errorf("阻断组键映射不对: %+v", got)
	}
}

// ---- 盘点放行 ----

func TestApplyStocktakeVerification_OnlyQuantityBlockDegraded(t *testing.T) {
	drafts := []signalDraft{
		{Level: model.SignalBlock, SignalType: SigQtyDeviation, ProductID: "P1", Message: "超量"},
		{Level: model.SignalBlock, SignalType: SigPurchaseWithoutSales, ProductID: "P2", Message: "无销售"},
		{Level: model.SignalBlock, SignalType: SigSheetMissing, ProductID: "P3", Message: "单据缺失"},
		{Level: model.SignalWarn, SignalType: SigQtyDeviation, ProductID: "P4", Message: "轻微超量"},
	}
	got := applyStocktakeVerification(drafts, []StocktakeVerification{{HeaderID: "ST-1", Status: "approved"}})
	if len(got) != len(drafts) {
		t.Fatalf("信号数量不变, got %d", len(got))
	}
	if got[0].Level != model.SignalWarn || got[1].Level != model.SignalWarn {
		t.Errorf("数量类阻断应降为 warn: %+v", got[:2])
	}
	if got[2].Level != model.SignalBlock {
		t.Errorf("单据类阻断不受盘点影响: %+v", got[2])
	}
	if !strings.Contains(got[0].Message, "ST-1") {
		t.Errorf("降级消息应注明关联盘点单: %s", got[0].Message)
	}
	// 入参不得被原地改写（调用方还持有快照）。
	if drafts[0].Level != model.SignalBlock {
		t.Errorf("入参被原地修改: %+v", drafts[0])
	}
}

// ---- service 级：组事实取数 ----

func TestGroupStockingFactsForAudit_ComboMerge(t *testing.T) {
	svc, db, cube, _, _ := newTestService(t)
	ctx := context.Background()
	gc := &fakeGroupCube{fakeCube: cube}
	svc.SetCube(gc)
	gc.combos = []ComboRelation{{ParentID: "P1", ChildID: "P2", Quantity: dec("12")}}
	gc.productUnits = map[string]ProductUnitInfo{
		"P1": {Unit: "件"},
		"P2": {Unit: "罐"},
	}
	gc.purchaseHist = []PurchaseHistoryRow{{
		SheetID: "H1", ProductID: "P1", DocumentType: "PI", SheetDate: "2026-08-01",
		Quantity: dec("10"), AmountYuan: dec("600"), UnitNo: "件", UnitFactor: dec("1"),
	}}
	cube.stocks = []StockRow{{ProductID: "P2", Quantity: dec("120")}}
	cube.sales = []SaleRow{{ProductID: "P2", NetSaleQuantity: dec("120")}}
	cube.productNames = map[string]string{"P1": "可乐(整件)", "P2": "可乐"}

	piLines := []SheetLineRow{{
		SheetID: "PI-CUR", ProductID: "P2", ReceivedQuantity: dec("120"),
		TotalAmountYuan: dec("600"), UnitNo: "罐",
	}}
	got := svc.groupStockingFactsForAudit(ctx, "B1", []string{"P2"}, piLines, nil,
		map[string]purchaseBasis{"P2": {NetQty: dec("120"), Days: 1, FirstDate: "2026-09-25", LastDate: "2026-09-25"}},
		nil, map[string]string{"P2": "可乐"}, Allowance{}, fixedNow)

	if got == nil || len(got.Facts) != 1 {
		t.Fatalf("应给出 1 个商品组, got %+v", got)
	}
	f := got.Facts[0]
	if len(f.MemberIDs) != 2 || f.MemberIDs[0] != "P1" || f.MemberIDs[1] != "P2" {
		t.Errorf("组合关系没生效: %+v", f.MemberIDs)
	}
	if f.BaseUnit != "件" || !f.ReceivedQty.Equal(dec("10")) || !f.StockQty.Equal(dec("10")) {
		t.Errorf("组口径换算不对: unit=%q received=%s stock=%s", f.BaseUnit, f.ReceivedQty, f.StockQty)
	}
	if !f.SalesQty.Equal(dec("10")) {
		t.Errorf("组销量 = %s 件, want 10", f.SalesQty)
	}
	if f.ZeroSalesCase != "" || f.Unconvertible {
		t.Errorf("有销量且单位一致, got %+v", f)
	}
	// 缓存应落库（同一批 SKU 一天内重复审批不再打源库）。
	var cached int64
	if err := db.Model(&model.StockingFactCache{}).Where("branch_id = ?", "B1").Count(&cached).Error; err != nil {
		t.Fatalf("读缓存: %v", err)
	}
	if cached != 2 {
		t.Errorf("缓存行数 = %d, want 2(成员两个商品)", cached)
	}
}

// ---- 流水线：组口径 end-to-end ----

// submitOneSettlement 提交一张单结算单的审批（固定流程人员）。
func submitOneSettlement(t *testing.T, svc *Service, settlementID, amount string) *model.ApprovalInstance {
	t.Helper()
	ap, err := svc.SubmitApproval(context.Background(), "B1", SubmitInput{
		ApplicantUserID:  "u-1",
		ApplicantWecomID: "wecom-u1",
		ApplicantBearer:  "Bearer test-token",
		SettlementIDs:    []string{settlementID},
		ManualAmountYuan: dec(amount),
		Remark:           "9月货款",
		FlowSelections: FlowSelections{
			FlowNodeApprover: []string{"apr1"},
			FlowNodeHandler:  []string{"handler1"},
			FlowNodeCC:       []string{"cc1"},
		},
	})
	if err != nil {
		t.Fatalf("SubmitApproval: %v", err)
	}
	return ap
}

// TestRunAudit_GroupMode_LongStockBlocks 组口径的阻断路径：
// 有库存、最后一次进货 45 天前、近 180 天无销售 ⇒ 长期无销售 ⇒ block ⇒ 拒绝 + 盘点建议。
func TestRunAudit_GroupMode_LongStockBlocks(t *testing.T) {
	svc, db, cube, _, _ := newTestService(t)
	gc := &fakeGroupCube{fakeCube: cube}
	svc.SetCube(gc)

	cube.mu.Lock()
	cube.settlements = []SettlementRow{
		{ID: "CP-G1", SupplierID: "SUP9", DocumentType: "CP", ApproveStatus: "1", TotalGoodsYuan: dec("100")},
	}
	cube.lines = map[string][]SettlementLineRow{
		"CP-G1": {{SettlementID: "CP-G1", VoucherID: "PI-G1", SourceDocType: "PI", TotalGoodsYuan: dec("100")}},
	}
	cube.sheets = []PurchaseSheetRow{
		{ID: "PI-G1", SupplierID: "SUP9", DocumentType: "PI", ApproveStatus: "1", CreatedAt: "2026-09-20"},
	}
	cube.sheetLines = []SheetLineRow{
		{SheetID: "PI-G1", ProductID: "PR-LS", ReceivedQuantity: dec("10"), TotalAmountYuan: dec("100")},
	}
	cube.sales = nil
	cube.stocks = []StockRow{{ProductID: "PR-LS", Quantity: dec("20")}}
	cube.mu.Unlock()
	gc.purchaseHist = []PurchaseHistoryRow{{
		SheetID: "H-OLD", ProductID: "PR-LS", DocumentType: "PI", SheetDate: "2026-08-17",
		Quantity: dec("15"), AmountYuan: dec("150"),
	}}

	ap := submitOneSettlement(t, svc, "CP-G1", "100")
	ap = waitTerminalPtr(t, db, ap.ID)
	if ap.Status != model.StatusRejected {
		t.Fatalf("长期无销售应拒绝, got %s (err=%s)", ap.Status, ap.Error)
	}
	var sig model.AuditSignal
	if err := db.First(&sig, "approval_id = ? AND signal_type = ?", ap.ID, SigPurchaseWithoutSales).Error; err != nil {
		t.Fatalf("缺 purchase_without_sales 信号: %v", err)
	}
	if sig.Level != model.SignalBlock {
		t.Errorf("信号级别 = %s, want block", sig.Level)
	}
	if sig.ProductID != "PR-LS" {
		t.Errorf("product_id = %s, want PR-LS", sig.ProductID)
	}
	if !strings.Contains(sig.Message, "需盘点核对库存") {
		t.Errorf("阻断消息应注明盘点条件: %s", sig.Message)
	}
	assertNeutralWording(t, []string{sig.Message})

	var v model.LLMVerdict
	if err := db.First(&v, "approval_id = ?", ap.ID).Error; err != nil {
		t.Fatalf("缺 verdict: %v", err)
	}
	if v.Conclusion != ConclusionReject {
		t.Errorf("结论 = %s, want reject", v.Conclusion)
	}
	if !strings.Contains(string(v.SuggestStocktake), "PR-LS") {
		t.Errorf("应派生盘点建议 PR-LS: %s", string(v.SuggestStocktake))
	}
}

// TestRunAudit_GroupMode_WithinCapPasses 是口径切换的回归锁：
// 「进货 8 / 窗口销量 0.91」在旧单 SKU 口径是 8.8 倍 ⇒ block；
// 组口径看补货后可售天数（约 35 天 ≤ 45 天上限）⇒ 只留事实，不阻断。
func TestRunAudit_GroupMode_WithinCapPasses(t *testing.T) {
	svc, db, cube, _, _ := newTestService(t)
	gc := &fakeGroupCube{fakeCube: cube}
	svc.SetCube(gc)

	cube.mu.Lock()
	cube.settlements = []SettlementRow{
		{ID: "CP-G2", SupplierID: "SUP9", DocumentType: "CP", ApproveStatus: "1", TotalGoodsYuan: dec("100")},
	}
	cube.lines = map[string][]SettlementLineRow{
		"CP-G2": {{SettlementID: "CP-G2", VoucherID: "PI-G2", SourceDocType: "PI", TotalGoodsYuan: dec("100")}},
	}
	cube.sheets = []PurchaseSheetRow{
		{ID: "PI-G2", SupplierID: "SUP9", DocumentType: "PI", ApproveStatus: "1", CreatedAt: "2026-09-28"},
	}
	cube.sheetLines = []SheetLineRow{
		{SheetID: "PI-G2", ProductID: "PR-OK", ReceivedQuantity: dec("8"), TotalAmountYuan: dec("100")},
	}
	cube.sales = []SaleRow{{ProductID: "PR-OK", NetSaleQuantity: dec("0.91")}}
	cube.mu.Unlock()

	ap := submitOneSettlement(t, svc, "CP-G2", "100")
	ap = waitTerminalPtr(t, db, ap.ID)
	if ap.Status != model.StatusApproved {
		t.Fatalf("补货后可售天数在可售上限内应通过, got %s (reason=%s)", ap.Status, ap.RejectReason)
	}
	var blocks int64
	db.Model(&model.AuditSignal{}).Where("approval_id = ? AND level = ?", ap.ID, model.SignalBlock).Count(&blocks)
	if blocks != 0 {
		t.Errorf("block 信号数 = %d, want 0", blocks)
	}
	var qty int64
	db.Model(&model.AuditSignal{}).Where("approval_id = ? AND signal_type = ?", ap.ID, SigQtyDeviation).Count(&qty)
	if qty != 0 {
		t.Errorf("不该出 qty_deviation(旧口径会按 8.8 倍判超量): %d 条", qty)
	}
	// 账面结存差异仍作为事实提示（warn 不阻断）。
	var ledger int64
	db.Model(&model.AuditSignal{}).Where("approval_id = ? AND signal_type = ?", ap.ID, SigStockLedgerGap).Count(&ledger)
	if ledger == 0 {
		t.Errorf("进货 8、销售 0.91、库存 0 ⇒ 应有账面结存差异提示")
	}
}

// TestGroupStockingFactsForAudit_UnitMismatchNotBlocked 采购单位与档案单位不一致 ⇒
// 只给警告，不做数量阻断（生产实证：用品类商品按「瓶」进货、档案是「罐」）。
func TestGroupStockingFactsForAudit_UnitMismatchNotBlocked(t *testing.T) {
	svc, _, cube, _, _ := newTestService(t)
	ctx := context.Background()
	gc := &fakeGroupCube{fakeCube: cube}
	svc.SetCube(gc)
	gc.productUnits = map[string]ProductUnitInfo{"P1": {Unit: "罐"}}
	cube.sales = []SaleRow{{ProductID: "P1", NetSaleQuantity: dec("2")}}

	piLines := []SheetLineRow{{
		SheetID: "PI-CUR", ProductID: "P1", ReceivedQuantity: dec("100"),
		TotalAmountYuan: dec("500"), UnitNo: "瓶",
	}}
	got := svc.groupStockingFactsForAudit(ctx, "B1", []string{"P1"}, piLines, nil,
		map[string]purchaseBasis{"P1": {NetQty: dec("100"), Days: 1, FirstDate: "2026-09-25", LastDate: "2026-09-25"}},
		nil, map[string]string{"P1": "可乐"}, Allowance{}, fixedNow)
	if got == nil || len(got.Facts) != 1 {
		t.Fatalf("应给出 1 个商品组, got %+v", got)
	}
	if !got.Facts[0].Unconvertible {
		t.Fatalf("单位不一致应标记 Unconvertible: %+v", got.Facts[0])
	}
	if !strings.Contains(got.Facts[0].UnitNote, "瓶") || !strings.Contains(got.Facts[0].UnitNote, "罐") {
		t.Errorf("单位说明应写清两个单位: %s", got.Facts[0].UnitNote)
	}
	msgs := checkGroupQuantity(got.Facts, 2.0, "2026-10-01")
	for _, d := range msgs {
		if d.Level == model.SignalBlock {
			t.Errorf("单位不一致时不该阻断: %s", d.Message)
		}
	}
}

// TestComputeGroupStockingFacts_BookedVsPending 已入账的收货不再叠加到库存，
// 只有收货日晚于库存行最后变动日的才算"未入账补货"（2026-11-02 用户口径）。
func TestComputeGroupStockingFacts_BookedVsPending(t *testing.T) {
	in := groupStockingInput{
		Products:       []string{"P1"},
		Units:          map[string]ProductUnitInfo{"P1": {Unit: "瓶"}},
		Stock:          map[string]decimal.Decimal{"P1": dec("5")},
		StockUpdatedAt: map[string]string{"P1": "2026-09-18"},
		SheetDates: map[string]string{
			"PI-OLD": "2026-09-18", // 与库存最后变动日同一天 ⇒ 已在库存里
			"PI-NEW": "2026-10-25", // 晚于库存最后变动日 ⇒ 未入账
		},
		PILines: []SheetLineRow{
			{SheetID: "PI-OLD", ProductID: "P1", ReceivedQuantity: dec("5"), TotalAmountYuan: dec("30"), UnitNo: "瓶"},
			{SheetID: "PI-NEW", ProductID: "P1", ReceivedQuantity: dec("7"), TotalAmountYuan: dec("42"), UnitNo: "瓶"},
		},
		Purchases: map[string]purchaseBasis{
			"P1": {NetQty: dec("12"), Days: 2, FirstDate: "2026-09-18", LastDate: "2026-10-25"},
		},
		Names: map[string]string{"P1": "矿泉水"},
		Today: "2026-11-02",
	}
	res := computeGroupStockingFacts(in)
	if len(res.Facts) != 1 {
		t.Fatalf("应给出 1 个商品组, got %+v", res)
	}
	g := res.Facts[0]
	if !g.BookedQty.Equal(dec("5")) || !g.PendingQty.Equal(dec("7")) {
		t.Errorf("已入账/未入账 = %s/%s, want 5/7", g.BookedQty, g.PendingQty)
	}
	// 库存 5（已含 9/18 那批）+ 未入账 7 = 12；已入账的 5 不能再加一次。
	if !g.AfterQty.Equal(dec("12")) {
		t.Errorf("补货后合计 = %s, want 12(库存5+未入账7)", g.AfterQty)
	}
	if g.BatchCount != 2 || !g.MaxBatchQty.Equal(dec("7")) || !g.BatchMedian.Equal(dec("6")) {
		t.Errorf("单批量级 = %d 批 / 最大 %s / 中位 %s, want 2/7/6",
			g.BatchCount, g.MaxBatchQty, g.BatchMedian)
	}
}

// TestComputeGroupStockingFacts_ManualDisplayKept 人工陈列量优先，且不再生成自动行。
func TestComputeGroupStockingFacts_ManualDisplayKept(t *testing.T) {
	gk := groupKeyOf([]string{"P1"})
	in := groupStockingInput{
		Products: []string{"P1"},
		Units:    map[string]ProductUnitInfo{"P1": {Unit: "瓶"}},
		Histories: map[string]*productHistory{"P1": {
			ProductID: "P1",
			Unit:      ProductUnitInfo{Unit: "瓶"},
			Receipts: []productHistoryReceipt{
				{SheetID: "H1", Date: "2026-06-01", Doc: "PI", Qty: dec("5"), Amount: dec("30"), UnitNo: "瓶", UnitFactor: dec("1")},
				{SheetID: "H2", Date: "2026-08-01", Doc: "PI", Qty: dec("5"), Amount: dec("30"), UnitNo: "瓶", UnitFactor: dec("1")},
			},
		}},
		Stock: map[string]decimal.Decimal{"P1": dec("20")},
		PILines: []SheetLineRow{{
			SheetID: "PI-CUR", ProductID: "P1", ReceivedQuantity: dec("5"), TotalAmountYuan: dec("30"), UnitNo: "瓶",
		}},
		Names: map[string]string{"P1": "矿泉水"},
		Displays: map[string]model.StockingDisplay{
			gk: {GroupKey: gk, ProductID: "P1", DisplayQty: dec("8"), Source: model.ProfileSourceManual},
		},
		Today: "2026-11-02",
	}
	res := computeGroupStockingFacts(in)
	if len(res.Facts) != 1 {
		t.Fatalf("应给出 1 个商品组, got %+v", res)
	}
	g := res.Facts[0]
	if !g.DisplayQty.Equal(dec("8")) || g.DisplaySource != model.ProfileSourceManual {
		t.Errorf("陈列保有量应取人工值 8, got %s(%s)", g.DisplayQty, g.DisplaySource)
	}
	if len(res.Displays) != 0 {
		t.Errorf("人工陈列量不该再生成自动行: %+v", res.Displays)
	}
	if !g.SellableQty.Equal(dec("17")) {
		t.Errorf("可售量 = %s, want 17(库存20+进货5−陈列8)", g.SellableQty)
	}
}

// TestCategoryCapDays_UsesXunsCategory 品类档位优先按思迅类别判，品名关键词只兜底。
func TestCategoryCapDays_UsesXunsCategory(t *testing.T) {
	cases := []struct {
		name    string
		cat     *CategoryInfo
		wantCap int
	}{
		{"劲仔鱼仔12g（烧烤）", &CategoryInfo{Name: "休闲食品", ParentName: "食品类"}, 45},
		{"侬一点250ml高钙核桃奶", &CategoryInfo{Name: "常温", ParentName: "牛奶"}, 45},
		{"伊利八连杯味浓原味特惠装酸奶", &CategoryInfo{Name: "低温", ParentName: "牛奶"}, 20},
		{"家佳多幼嫩紫菜60g", &CategoryInfo{Name: "干货系列", ParentName: "干货/大米"}, 45},
		{"家佳多带嘴土鸡精100g", &CategoryInfo{Name: "调味粉", ParentName: "调味类"}, 45},
		{"每日生机苏打饼干-奶盐味228G", &CategoryInfo{Name: "食品类"}, 45},
		{"毛笔", &CategoryInfo{Name: "办公用品", ParentName: "文体用品"}, 90},
	}
	for _, c := range cases {
		_, capDays, _ := categoryCapDaysFor(c.name, c.cat)
		if capDays != c.wantCap {
			t.Errorf("%s: cap_days = %d, want %d", c.name, capDays, c.wantCap)
		}
	}
	// 类别缺失 ⇒ 仍回退品名关键词（紫菜含"菜" ⇒ 生鲜 7 天）。
	_, capDays, _ := categoryCapDaysFor("家佳多幼嫩紫菜60g", nil)
	if capDays != 7 {
		t.Errorf("无类别时应回退品名关键词(fresh 7), got %d", capDays)
	}
}

// TestStockingDisplay_ManualUpdateAndDelete 人工改陈列量后 Source=manual，删除后回到自动。
func TestStockingDisplay_ManualUpdateAndDelete(t *testing.T) {
	svc, db, _, _, _ := newTestService(t)
	ctx := context.Background()
	row := model.StockingDisplay{
		ID: "D1", BranchID: "B1", GroupKey: "P1", ProductID: "P1",
		DisplayQty: dec("5"), Source: model.ProfileSourceAuto,
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("seed display: %v", err)
	}
	upd, err := svc.UpdateStockingDisplay(ctx, "B1", "P1", StockingDisplayUpdateInput{DisplayQty: dec("8"), Note: "端架"})
	if err != nil {
		t.Fatalf("UpdateStockingDisplay: %v", err)
	}
	if !upd.DisplayQty.Equal(dec("8")) || upd.Source != model.ProfileSourceManual {
		t.Errorf("人工修改后 = %s(%s), want 8(manual)", upd.DisplayQty, upd.Source)
	}
	if err := svc.DeleteStockingDisplay(ctx, "B1", "P1"); err != nil {
		t.Fatalf("DeleteStockingDisplay: %v", err)
	}
	var cnt int64
	db.Model(&model.StockingDisplay{}).Where("branch_id = ? AND group_key = ?", "B1", "P1").Count(&cnt)
	if cnt != 0 {
		t.Errorf("删除后应无行, got %d", cnt)
	}
}

// TestStockingProfile_ManualUpdateAndDelete 档位人工修改后 manual，
// 删除后回到自动；列表支持按来源过滤。
func TestStockingProfile_ManualUpdateAndDelete(t *testing.T) {
	svc, db, _, _, _ := newTestService(t)
	ctx := context.Background()
	if err := db.Create(&model.ProductProfile{
		ID: "PR1", BranchID: "B1", ProductID: "P1", ProductName: "可乐",
		Class: model.ProfileClassBeverage, CapDays: 45, Source: model.ProfileSourcePending,
	}).Error; err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	p, err := svc.UpdateProductProfile(ctx, "B1", "P1", ProductProfileUpdateInput{
		CapDays: 90, ShelfLifeDays: 365, Note: "常温饮料",
	})
	if err != nil {
		t.Fatalf("UpdateProductProfile: %v", err)
	}
	if p.CapDays != 90 || p.Source != model.ProfileSourceManual {
		t.Errorf("档位 = %d(%s), want 90(manual)", p.CapDays, p.Source)
	}

	// 按 source=manual 过滤应只剩一行。
	profs, total, err := svc.ListProductProfiles(ctx, "B1", "", model.ProfileSourceManual, 10, 0)
	if err != nil || total != 1 || len(profs) != 1 {
		t.Fatalf("ListProductProfiles(manual) = %d/%d err=%v", len(profs), total, err)
	}

	if err := svc.DeleteProductProfile(ctx, "B1", "P1"); err != nil {
		t.Fatalf("DeleteProductProfile: %v", err)
	}
	var pc int64
	db.Model(&model.ProductProfile{}).Where("branch_id = ?", "B1").Count(&pc)
	if pc != 0 {
		t.Errorf("删除后应无行, profile=%d", pc)
	}
}
