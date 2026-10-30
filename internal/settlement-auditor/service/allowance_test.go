// service/allowance_test.go —— 收款项放宽额度的单测（2026-10-25 用户口径）。
//
// 锁住两件事：
//  1. 有陈列费/堆头费 ⇒ 「进货可以多些」（数量偏离不再阻断）；
//  2. 有补差价 ⇒ 允许毛利降低，额度 = 补差价总额。
package service

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// 有陈列费 ⇒ 数量偏离的 block 降为 warn（用户举例:30.9 倍那单应当通过）。
func TestApplyReceiptAllowance_DowngradesQtyDeviation(t *testing.T) {
	drafts := []signalDraft{
		newSignal(model.SignalBlock, SigQtyDeviation, "", "P1",
			"商品 伊利金典纯牛奶250ml-6907992507095 本次进货 216,为 2026-08-26 采购入库以来销量 7 的 30.9 倍", nil),
	}
	al := Allowance{DisplayYuan: dec("300")}
	out := applyReceiptAllowance(drafts, al)
	if out[0].Level != model.SignalWarn {
		t.Fatalf("级别 = %s, want warn(有陈列费⇒进货可以多些)", out[0].Level)
	}
	if !strings.Contains(out[0].Message, "陈列费/堆头费") {
		t.Fatalf("消息里要写明放宽依据: %q", out[0].Message)
	}
	// 入参不被改(调用方可能还拿着它)。
	if drafts[0].Level != model.SignalBlock {
		t.Fatal("不该原地改入参切片")
	}
}

// 没有陈列费/堆头费 ⇒ 信号原样(维持原有判断规则)。
func TestApplyReceiptAllowance_NoAllowance(t *testing.T) {
	drafts := []signalDraft{
		newSignal(model.SignalBlock, SigQtyDeviation, "", "P1", "进货 216,销量 7", nil),
	}
	out := applyReceiptAllowance(drafts, Allowance{})
	if out[0].Level != model.SignalBlock {
		t.Fatalf("级别 = %s, want block(无放宽额度)", out[0].Level)
	}
}

// 「窗口内完全无销售」也放宽:供应商付了陈列费/堆头费,门店就是有意多备货/铺货。
// (系统本来就允许"申请人的备注"解释数量类信号 ⇒ 有结构的收款给出同样口径。)
func TestApplyReceiptAllowance_DowngradesNoSales(t *testing.T) {
	drafts := []signalDraft{
		newSignal(model.SignalBlock, SigPurchaseWithoutSales, "", "P1", "无销售记录", nil),
	}
	out := applyReceiptAllowance(drafts, Allowance{PromotionYuan: dec("100")})
	if out[0].Level != model.SignalWarn {
		t.Fatalf("级别 = %s, want warn(有堆头费⇒备货铺货是正常的)", out[0].Level)
	}
	if !strings.Contains(out[0].Message, "进货可以多些") {
		t.Fatalf("消息里要写明放宽依据: %q", out[0].Message)
	}
}

// 其它 block 信号(单据缺失等)不受影响。
func TestApplyReceiptAllowance_KeepsOtherBlocks(t *testing.T) {
	drafts := []signalDraft{
		newSignal(model.SignalBlock, SigSettlementMissing, "S1", "", "结算单不存在", nil),
	}
	out := applyReceiptAllowance(drafts, Allowance{DisplayYuan: dec("300")})
	if out[0].Level != model.SignalBlock {
		t.Fatalf("级别 = %s, want block", out[0].Level)
	}
}

// ---- 毛利额度 ----

func TestEstimateGrossMargin(t *testing.T) {
	cost := costBasisOf(map[string]string{"P1": "10"}) // 进价 10
	sales := map[string]SaleRow{
		// 卖 10 件、收入 150 ⇒ 毛利 50
		"P1": {ProductID: "P1", NetSaleQuantity: dec("10"), NetSaleAmountYuan: dec("150")},
		// 没有进货均价 ⇒ 算不出成本，不计入
		"P2": {ProductID: "P2", NetSaleQuantity: dec("5"), NetSaleAmountYuan: dec("100")},
	}
	gm := estimateGrossMargin(cost, sales)
	if !gm.Calculated {
		t.Fatal("应算得出毛利")
	}
	if gm.AmountYuan.String() != "50" {
		t.Fatalf("毛利 = %s, want 50", gm.AmountYuan.String())
	}
}

// 毛利缺口 ≤ 补差价 ⇒ info（按口径允许毛利降低）。
func TestCheckGrossMargin_CoveredByPriceDiff(t *testing.T) {
	gm := GrossMargin{AmountYuan: dec("-100"), Calculated: true}
	sigs := checkGrossMarginWithAllowance(gm, Allowance{PriceDiffYuan: dec("150")}, nil, nil)
	if len(sigs) != 1 {
		t.Fatalf("信号数 = %d, want 1", len(sigs))
	}
	if sigs[0].Level != model.SignalInfo {
		t.Fatalf("级别 = %s, want info(缺口被补差价覆盖)", sigs[0].Level)
	}
	if sigs[0].SignalType != SigGrossMargin {
		t.Fatalf("信号类型 = %s, want %s", sigs[0].SignalType, SigGrossMargin)
	}
}

// 毛利缺口 > 补差价 ⇒ warn（提醒人工终审，仍不阻断）。
func TestCheckGrossMargin_ExceedsPriceDiff(t *testing.T) {
	gm := GrossMargin{AmountYuan: dec("-300"), Calculated: true}
	sigs := checkGrossMarginWithAllowance(gm, Allowance{PriceDiffYuan: dec("150")}, nil, nil)
	if len(sigs) != 1 {
		t.Fatalf("信号数 = %d, want 1", len(sigs))
	}
	if sigs[0].Level != model.SignalWarn {
		t.Fatalf("级别 = %s, want warn", sigs[0].Level)
	}
	if !strings.Contains(sigs[0].Message, "超过补差价") {
		t.Fatalf("消息 = %q", sigs[0].Message)
	}
}

// 没有补差价 **且** 没有任何 SKU 毛利为负 ⇒ 不产生毛利信号。
//
// ⚠️ 这条断言的边界在 2026-10-29 被改过：原先是"没有补差价就一律不报毛利"，
// 现在"没有补差价"时**逐 SKU 负毛利仍要报 warn**（见下一个用例）。
// 这里保��"没有负毛利 SKU ⇒ 一条都不报"（不制造新噪音）。
func TestCheckGrossMargin_NoPriceDiff_NoNegativeSKU(t *testing.T) {
	gm := GrossMargin{AmountYuan: dec("50"), Calculated: true}
	if sigs := checkGrossMarginWithAllowance(gm, Allowance{}, nil, nil); len(sigs) != 0 {
		t.Fatalf("没有补差价却产生了 %d 条信号: %+v", len(sigs), sigs)
	}
}

// 毛利为正 ⇒ 不产生信号。
func TestCheckGrossMargin_Positive(t *testing.T) {
	gm := GrossMargin{AmountYuan: dec("50"), Calculated: true}
	if sigs := checkGrossMarginWithAllowance(gm, Allowance{PriceDiffYuan: dec("150")}, nil, nil); len(sigs) != 0 {
		t.Fatalf("毛利为正却产生了信号: %+v", sigs)
	}
}

// ---- 逐 SKU 负毛利（2026-10-29 用户口径）----

// 逐 SKU 毛利估算：与整体口径用**同一个**计入集合。
func TestEstimateProductMargins(t *testing.T) {
	cost := costBasisOf(map[string]string{
		"P1": "10", // 进价 10，卖 10 件收 150 ⇒ 毛利 +50
		"P2": "30", // 进价 30，卖 2 件收 40  ⇒ 毛利 -20
	})
	sales := map[string]SaleRow{
		"P1": {ProductID: "P1", NetSaleQuantity: dec("10"), NetSaleAmountYuan: dec("150")},
		"P2": {ProductID: "P2", NetSaleQuantity: dec("2"), NetSaleAmountYuan: dec("40")},
		"P3": {ProductID: "P3", NetSaleQuantity: dec("5"), NetSaleAmountYuan: dec("100")}, // 无进价 ⇒ 不计
	}
	per := estimateProductMargins(cost, sales)
	if len(per) != 2 {
		t.Fatalf("逐 SKU 条数 = %d, want 2(无进货均价的 P3 不计)", len(per))
	}
	if got := per["P1"].AmountYuan.String(); got != "50" {
		t.Fatalf("P1 毛利 = %s, want 50", got)
	}
	if got := per["P2"].AmountYuan.String(); got != "-20" {
		t.Fatalf("P2 毛利 = %s, want -20", got)
	}
}

// 无补差价时，某 SKU 负毛利 ⇒ 产生 warn（2026-10-29 用户口径）。
//
// ⚠️ 判别力：改造前这里**一条信号都没有**（毛利只在有补差价时才看，且只看整体）。
// 去掉本分支 ⇒ 用例立刻红。
func TestCheckGrossMargin_NegativeSKU_NoPriceDiff_Warns(t *testing.T) {
	cost := costBasisOf(map[string]string{"P2": "30"})
	sales := map[string]SaleRow{
		"P2": {ProductID: "P2", NetSaleQuantity: dec("2"), NetSaleAmountYuan: dec("40")},
	}
	per := estimateProductMargins(cost, sales)
	gm := estimateGrossMargin(cost, sales) // 整体也是 -20
	sigs := checkGrossMarginWithAllowance(gm, Allowance{}, per, map[string]string{"P2": "某商品"})
	if len(sigs) != 1 {
		t.Fatalf("信号数 = %d, want 1(逐 SKU 负毛利应报 warn)", len(sigs))
	}
	if sigs[0].Level != model.SignalWarn {
		t.Fatalf("级别 = %s, want warn", sigs[0].Level)
	}
	if sigs[0].SignalType != SigGrossMargin {
		t.Fatalf("信号类型 = %s, want %s", sigs[0].SignalType, SigGrossMargin)
	}
	// 消息里要能看出是哪个商品、亏了多少、用的什么口径。
	if !strings.Contains(sigs[0].Message, "某商品-P2") {
		t.Fatalf("消息应带「<名称>-<货号>」: %q", sigs[0].Message)
	}
	if !strings.Contains(sigs[0].Message, "-20") {
		t.Fatalf("消息应带毛利金额: %q", sigs[0].Message)
	}
}

// 整体毛利为正、但某个 SKU 负毛利 ⇒ 仍要报 warn
// （利润 SKU 把亏钱 SKU 盖住了 —— 这正是"只看整体"会漏掉的情况）。
func TestCheckGrossMargin_NegativeSKU_HiddenByPositiveOverall_Warns(t *testing.T) {
	cost := costBasisOf(map[string]string{"P1": "10", "P2": "30"})
	sales := map[string]SaleRow{
		"P1": {ProductID: "P1", NetSaleQuantity: dec("10"), NetSaleAmountYuan: dec("150")}, // +50
		"P2": {ProductID: "P2", NetSaleQuantity: dec("2"), NetSaleAmountYuan: dec("40")},  // -20
	}
	per := estimateProductMargins(cost, sales)
	gm := estimateGrossMargin(cost, sales) // 整体 +30
	if !gm.AmountYuan.Equal(dec("30")) {
		t.Fatalf("整体毛利 = %s, want 30", gm.AmountYuan.String())
	}
	sigs := checkGrossMarginWithAllowance(gm, Allowance{PriceDiffYuan: dec("150")}, per, nil)
	if len(sigs) != 1 {
		t.Fatalf("信号数 = %d, want 1", len(sigs))
	}
	if sigs[0].Level != model.SignalWarn {
		t.Fatalf("级别 = %s, want warn", sigs[0].Level)
	}
	if !strings.Contains(sigs[0].Message, "P2") {
		t.Fatalf("消息应指出亏钱的 SKU: %q", sigs[0].Message)
	}
}

// 有补差价且整体为负 ⇒ 出**整体**信号(口径 A)，并把负毛利 SKU 挂进消息，
// 不再单独报一条逐 SKU 信号（同一件事不说两遍）。
func TestCheckGrossMargin_PriceDiff_MergesNegativeSKUs(t *testing.T) {
	cost := costBasisOf(map[string]string{"P2": "30"})
	sales := map[string]SaleRow{
		"P2": {ProductID: "P2", NetSaleQuantity: dec("2"), NetSaleAmountYuan: dec("40")},
	}
	per := estimateProductMargins(cost, sales)
	gm := estimateGrossMargin(cost, sales)
	sigs := checkGrossMarginWithAllowance(gm, Allowance{PriceDiffYuan: dec("150")}, per,
		map[string]string{"P2": "某商品"})
	if len(sigs) != 1 {
		t.Fatalf("信号数 = %d, want 1(整体与逐 SKU 合并成一条)", len(sigs))
	}
	if sigs[0].Level != model.SignalInfo {
		t.Fatalf("级别 = %s, want info(缺口被补差价覆盖)", sigs[0].Level)
	}
	if !strings.Contains(sigs[0].Message, "毛利为负的商品") {
		t.Fatalf("整体信号应附带负毛利 SKU 明细: %q", sigs[0].Message)
	}
}

// 进货均价：Σ金额 ÷ Σ收货量。
func TestUnitCostByProduct(t *testing.T) {
	cost := unitCostByProduct([]SheetLineRow{
		{ProductID: "P1", TotalAmountYuan: dec("100"), ReceivedQuantity: dec("10")},
		{ProductID: "P1", TotalAmountYuan: dec("50"), ReceivedQuantity: dec("5")},
		{ProductID: "P2", TotalAmountYuan: dec("80"), ReceivedQuantity: dec("0")},
	})
	if got := cost["P1"].String(); got != "10" {
		t.Fatalf("P1 均价 = %s, want 10", got)
	}
	if _, ok := cost["P2"]; ok {
		t.Fatal("收货量为 0 的商品不该有均价")
	}
}

// costBasisOf 造一份成本口径表（测试用）。
//
// 默认按「本批次进货均价」口径 —— 与老用例的语义一致。
func costBasisOf(m map[string]string) map[string]CostBasis {
	out := make(map[string]CostBasis, len(m))
	for k, v := range m {
		out[k] = CostBasis{UnitCostYuan: dec(v), Source: CostSourcePurchase}
	}
	return out
}

// ---- 成本口径（2026-10-29 生产实踩）----

// mergeCostBasis：库存加权平均成本价**优先**于本批次进货均价。
//
// ⚠️ 判别力：这就是蜜梨-01860 那次**假负毛利**的根因 ——
//    旧口径拿「本批次进货价 4.6349」当成本，把一笔正毛利算成 -280.57 元；
//    思迅库存里的加权平均成本价是 2.8192 ⇒ 毛利为正。
//    去掉这个优先级 ⇒ 本用例立刻红。
func TestMergeCostBasis_PrefersAvgCost(t *testing.T) {
	batch := map[string]decimal.Decimal{"01860": dec("4.6349015873015873")}
	avg := map[string]decimal.Decimal{"01860": dec("2.8192")}
	got := mergeCostBasis(batch, avg)
	c, ok := got["01860"]
	if !ok {
		t.Fatal("应有 01860 的成本口径")
	}
	if c.Source != CostSourceAvgCost {
		t.Fatalf("口径来源 = %s, want %s", c.Source, CostSourceAvgCost)
	}
	if c.UnitCostYuan.String() != "2.8192" {
		t.Fatalf("单位成本 = %s, want 2.8192", c.UnitCostYuan.String())
	}
}

// 没有库存成本 ⇒ 回退本批次进货均价（不能因为「查不到成本」就让 SKU 整个消失）。
func TestMergeCostBasis_FallsBackToPurchase(t *testing.T) {
	got := mergeCostBasis(
		map[string]decimal.Decimal{"P1": dec("4.5")},
		map[string]decimal.Decimal{}, // 没有库存成本记录
	)
	if got["P1"].Source != CostSourcePurchase {
		t.Fatalf("口径来源 = %s, want %s", got["P1"].Source, CostSourcePurchase)
	}
	// avg_cost = 0（库存为 0 / 成本没维护）同样要回退。
	got2 := mergeCostBasis(
		map[string]decimal.Decimal{"P1": dec("4.5")},
		map[string]decimal.Decimal{"P1": dec("0")},
	)
	if got2["P1"].Source != CostSourcePurchase {
		t.Fatalf("avg_cost 为 0 时口径来源 = %s, want %s", got2["P1"].Source, CostSourcePurchase)
	}
}

// 生产实况回归：蜜梨-01860 必须是**正毛利**。
//
// 数据全部来自 2026-10-29 生产实测（审批单 202610290024 / CP002609144367）：
//   - 本次进货：72.9997 元 / 15.75 kg ⇒ 本批次均价 4.6349015873015873
//   - 思迅库存：70.84003 kg、平均成本价 2.8192（cube stock.avg_cost）
//   - 窗口销量 254.4101 kg、销额 898.6 元 ⇒ 均价 3.532
// 旧口径（本批次进货价）算出 -280.5658 元的**假负毛利**；
// 新口径（库存加权平均成本价）算出约 +181.3 元。
//
// ⚠️ 判别力：把 mergeCostBasis 的优先级去掉 ⇒ 本用例立刻红。
func TestEstimateProductMargins_PeaCaseIsPositiveMargin(t *testing.T) {
	cost := mergeCostBasis(
		map[string]decimal.Decimal{"01860": dec("4.6349015873015873")},
		map[string]decimal.Decimal{"01860": dec("2.8192")},
	)
	sales := map[string]SaleRow{
		"01860": {ProductID: "01860", NetSaleQuantity: dec("254.4101"), NetSaleAmountYuan: dec("898.6")},
	}
	per := estimateProductMargins(cost, sales)
	m, ok := per["01860"]
	if !ok {
		t.Fatal("01860 应计入毛利口径")
	}
	if m.AmountYuan.IsNegative() {
		t.Fatalf("蜜梨-01860 毛利 = %s, 应为正(思迅平均成本价 2.8192 < 售价 3.532)", m.AmountYuan.String())
	}
	if m.CostSource != CostSourceAvgCost {
		t.Fatalf("成本口径 = %s, want %s", m.CostSource, CostSourceAvgCost)
	}
	// 反证：旧口径（本批次进货价）确实是负的 —— 钉住"这次修的是真问题"。
	old := estimateProductMargins(
		costBasisOf(map[string]string{"01860": "4.6349015873015873"}),
		sales,
	)
	if !old["01860"].AmountYuan.IsNegative() {
		t.Fatalf("旧口径算出来 %s, 不是负数 —— 复现前提变了,请重新核对生产数据",
			old["01860"].AmountYuan.String())
	}
}
// ---- 实现毛利（2026-10-29 用户口径）----

// 生产实况回归：蜜梨-01860 的**实现毛利**为负（这批进货按自己的进价卖是亏的）。
//
// 数据全部来自 2026-10-29 生产实测（审批单 202610290024 / CP002609144367）：
//   - 本批次进货：72.9997 元 / 15.75 kg ⇒ 进货均价 4.6349015873015873
//   - 窗口销售：898.6 元 / 254.4101 kg ⇒ 销售均价 3.5319…
//   - 实现量 = min(15.75, 254.4101) = 15.75
//   ⇒ 实现毛利 = (3.5319 − 4.6349) × 15.75 ≈ **−17.37 元**
//
// ⚠️ 这与「窗口毛利」是**两个口径、两个结论**：
//   窗口毛利用库存加权平均成本价 2.8192 ⇒ **+181 元**（正）；
//   实现毛利用本批次进价 4.6349 ⇒ **负**。
//   两个都对 —— 后者才是"这批货进贵了"的真相。
func TestEstimateRealizedMargins_PeaCaseIsNegative(t *testing.T) {
	batch := map[string]BatchPurchase{
		"01860": {AmountYuan: dec("72.9997"), Qty: dec("15.75")},
	}
	sales := map[string]SaleRow{
		"01860": {ProductID: "01860", NetSaleQuantity: dec("254.4101"), NetSaleAmountYuan: dec("898.6")},
	}
	per := estimateRealizedMargins(batch, sales, map[string]string{"01860": "2026-09-09"})
	rm, ok := per["01860"]
	if !ok {
		t.Fatal("01860 应计入实现毛利口径")
	}
	if !rm.AmountYuan.IsNegative() {
		t.Fatalf("实现毛利 = %s, 应为负(进货均价 4.63 > 销售均价 3.53)", rm.AmountYuan.String())
	}
	// 量取 min(进货总量, 销售总量) ⇒ 这里被进货总量卡住。
	if rm.CountedQty.String() != "15.75" {
		t.Fatalf("实现量 = %s, want 15.75", rm.CountedQty.String())
	}
	if rm.WindowFrom != "2026-09-09" {
		t.Fatalf("窗口起点 = %s, want 2026-09-09", rm.WindowFrom)
	}
}

// 实现量 = min(本批次进货总量, 窗口销售总量) —— 卖不掉的那些还压在库里。
func TestEstimateRealizedMargins_CountedQtyIsMin(t *testing.T) {
	batch := map[string]BatchPurchase{
		"P1": {AmountYuan: dec("100"), Qty: dec("10")}, // 进货 10
		"P2": {AmountYuan: dec("100"), Qty: dec("100")}, // 进货 100
	}
	sales := map[string]SaleRow{
		"P1": {ProductID: "P1", NetSaleQuantity: dec("5"), NetSaleAmountYuan: dec("50")},
		"P2": {ProductID: "P2", NetSaleQuantity: dec("5"), NetSaleAmountYuan: dec("50")},
	}
	per := estimateRealizedMargins(batch, sales, nil)
	if per["P1"].CountedQty.String() != "5" {
		t.Fatalf("P1 实现量 = %s, want 5(被销售量卡住)", per["P1"].CountedQty.String())
	}
	if per["P2"].CountedQty.String() != "5" {
		t.Fatalf("P2 实现量 = %s, want 5(被销售量卡住)", per["P2"].CountedQty.String())
	}
}

// 没有销量 / 没有本批次进货 ⇒ 不进这个口径（不能把"没卖"当成"不亏"）。
func TestEstimateRealizedMargins_SkipsIncomplete(t *testing.T) {
	per := estimateRealizedMargins(
		map[string]BatchPurchase{
			"P1": {AmountYuan: dec("100"), Qty: dec("10")}, // 有进货,没卖
			"P2": {AmountYuan: dec("100"), Qty: dec("0")},  // 量为 0
		},
		map[string]SaleRow{
			"P1": {ProductID: "P1", NetSaleQuantity: dec("0"), NetSaleAmountYuan: dec("0")},
		},
		nil,
	)
	if len(per) != 0 {
		t.Fatalf("口径不齐的商品不该进入实现毛利,实际 %+v", per)
	}
}

// 实现毛利为负 ⇒ warn（永不 block）；为正 ⇒ 一条都不报。
func TestCheckRealizedMargin(t *testing.T) {
	per := map[string]RealizedMargin{
		"P1": {
			ProductID:         "P1",
			AmountYuan:        dec("-17.37"),
			BatchUnitCostYuan: dec("4.63"),
			SalesAvgPriceYuan: dec("3.53"),
			BatchQty:          dec("15.75"),
			SalesQty:          dec("254.41"),
			CountedQty:        dec("15.75"),
		},
		"P2": {
			ProductID:         "P2",
			AmountYuan:        dec("30"),
			BatchUnitCostYuan: dec("2"),
			SalesAvgPriceYuan: dec("3"),
		},
	}
	sigs := checkRealizedMargin(per, map[string]string{"P1": "蜜梨"})
	if len(sigs) != 1 {
		t.Fatalf("信号数 = %d, want 1(只报为负的那个)", len(sigs))
	}
	if sigs[0].Level != model.SignalWarn {
		t.Fatalf("级别 = %s, want warn", sigs[0].Level)
	}
	if sigs[0].SignalType != SigRealizedMargin {
		t.Fatalf("信号类型 = %s, want %s", sigs[0].SignalType, SigRealizedMargin)
	}
	if !strings.Contains(sigs[0].Message, "实现毛利") {
		t.Fatalf("消息要说明是「实现毛利」: %q", sigs[0].Message)
	}
	if !strings.Contains(sigs[0].Message, "蜜梨-P1") {
		t.Fatalf("消息应带「<名称>-<货号>」: %q", sigs[0].Message)
	}
	// 证据里要能核对口径。
	ev, _ := sigs[0].Evidence.(map[string]any)
	skus, _ := ev["negative_skus"].([]map[string]string)
	if len(skus) != 1 {
		t.Fatalf("证据条数 = %d, want 1", len(skus))
	}
	if skus[0]["batch_unit_cost_yuan"] != "4.63" ||
		skus[0]["sales_avg_price_yuan"] != "3.53" ||
		skus[0]["counted_qty"] != "15.75" {
		t.Fatalf("证据不完整: %+v", skus[0])
	}
	// 全部为正 ⇒ 一条都不报（不制造噪音）。
	if s := checkRealizedMargin(map[string]RealizedMargin{
		"P2": {ProductID: "P2", AmountYuan: dec("30")},
	}, nil); len(s) != 0 {
		t.Fatalf("实现毛利为正却报了 %d 条信号", len(s))
	}
}