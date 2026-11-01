// Package service / checks.go —— 确定性规则核对引擎(FR-2.2/FR-2.3)。
//
// 每个 check 是纯函数:输入 cube 行 + 参数,输出 []model.AuditSignal 草稿
// (ID/ApprovalID/CreatedAt 由 pipeline 落库前补),便于单测。
//
// 信号级别语义:
//   - info:提示(如非采购结算单),不阻断;
//   - warn:偏离超阈值,交给 LLM 综合研判;
//   - block:硬异常(单据缺失/未审核/金额勾稽不上),规则降级路径直接拒绝。
package service

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// 信号类型。
const (
	SigSettlementMissing     = "settlement_missing"      // 结算单不存在
	SigSettlementNotApproved = "settlement_not_approved" // 结算单未审核
	SigNoPurchaseLines       = "no_purchase_lines"       // 非采购结算单(无 PI/RO 行)
	SigSheetMissing          = "sheet_missing"           // 关联采购单不存在
	SigSheetNotApproved      = "sheet_not_approved"      // 采购单未审核
	SigSupplierMismatch      = "supplier_mismatch"       // 采购单与结算单供应商不一致
	SigLineAmountMismatch    = "line_amount_mismatch"    // Σ明细 vs 结算单金额勾稽不上
	SigManualAmountMismatch  = "manual_amount_mismatch"  // 结算总额 vs 手工累加金额不符
	SigPurchaseWithoutSales  = "purchase_without_sales"  // 窗口内无销售却有进货
	SigQtyDeviation          = "qty_deviation"           // 采购量/销售量偏离超阈值
	// SigRecentReceiptNoSales 是「刚收货、还没卖」的提示（warn，不阻断）。
	SigRecentReceiptNoSales = "recent_receipt_no_sales"
	// SigNewProductNoSales 是「新品首单、还没有销售」的提示（warn，不阻断）。
	SigNewProductNoSales = "new_product_no_sales"
	// SigUnitUnconvertible 是「采购单位与商品档案单位不一致、无法换算」的提示（warn）。
	SigUnitUnconvertible = "unit_unconvertible"
	// SigPurchasePriceAboveHistory 是「本单进价比近 180 天均价高」的提示（warn）。
	SigPurchasePriceAboveHistory = "purchase_price_above_history"
	// SigStockLedgerGap 是「进货 − 销售 − 库存」对不上账的提示（warn）。
	SigStockLedgerGap = "stock_ledger_gap"
)

// signalDraft 是落库前的信号草稿。
type signalDraft struct {
	SettlementID string
	ProductID    string
	SignalType   string
	Level        string
	Message      string
	Evidence     any
}

func newSignal(level, sigType, settlementID, productID, msg string, evidence any) signalDraft {
	return signalDraft{
		SettlementID: settlementID,
		ProductID:    productID,
		SignalType:   sigType,
		Level:        level,
		Message:      msg,
		Evidence:     evidence,
	}
}

// hasBlock 判断信号集里是否有 block。
func hasBlock(drafts []signalDraft) bool {
	for _, d := range drafts {
		if d.Level == model.SignalBlock {
			return true
		}
	}
	return false
}

// marshalEvidence 序列化证据(失败兜底为字符串)。
func marshalEvidence(ev any) []byte {
	if ev == nil {
		return nil
	}
	b, err := json.Marshal(ev)
	if err != nil {
		b, _ = json.Marshal(map[string]any{"raw": fmt.Sprint(ev)})
	}
	return b
}

// ---- check 1: 结算单存在性 + 审核状态 ----

// checkSettlements 核对期望的结算单是否全部存在且已审核。
func checkSettlements(expectedIDs []string, found []SettlementRow) []signalDraft {
	byID := make(map[string]SettlementRow, len(found))
	for _, r := range found {
		byID[r.ID] = r
	}
	var out []signalDraft
	for _, id := range expectedIDs {
		r, ok := byID[id]
		if !ok {
			out = append(out, newSignal(model.SignalBlock, SigSettlementMissing, id, "",
				fmt.Sprintf("结算单 %s 在 sixun 中不存在", id),
				map[string]any{"settlement_id": id}))
			continue
		}
		if r.ApproveStatus != "1" {
			out = append(out, newSignal(model.SignalBlock, SigSettlementNotApproved, id, "",
				fmt.Sprintf("结算单 %s 未审核(approve_status=%s)", id, r.ApproveStatus),
				map[string]any{"settlement_id": id, "approve_status": r.ApproveStatus}))
		}
	}
	return out
}

// ---- check 2: 采购单存在性/审核状态/供应商一致性 ----

// checkSheets 核对逐张拆出的采购单。settlementSupplier 提供结算单 → 供应商映射。
func checkSheets(lines []SettlementLineRow, sheets []PurchaseSheetRow, settlementSupplier map[string]string) []signalDraft {
	byID := make(map[string]PurchaseSheetRow, len(sheets))
	for _, r := range sheets {
		byID[r.ID] = r
	}
	var out []signalDraft
	for _, ln := range lines {
		sh, ok := byID[ln.VoucherID]
		if !ok {
			out = append(out, newSignal(model.SignalBlock, SigSheetMissing, ln.SettlementID, "",
				fmt.Sprintf("结算单 %s 关联的采购单 %s 不存在", ln.SettlementID, ln.VoucherID),
				map[string]any{"voucher_id": ln.VoucherID, "source_doc_type": ln.SourceDocType}))
			continue
		}
		if sh.ApproveStatus != "1" {
			out = append(out, newSignal(model.SignalBlock, SigSheetNotApproved, ln.SettlementID, "",
				fmt.Sprintf("采购单 %s 未审核(approve_status=%s)", ln.VoucherID, sh.ApproveStatus),
				map[string]any{"voucher_id": ln.VoucherID, "approve_status": sh.ApproveStatus}))
		}
		if want := settlementSupplier[ln.SettlementID]; want != "" && sh.SupplierID != want {
			out = append(out, newSignal(model.SignalBlock, SigSupplierMismatch, ln.SettlementID, "",
				fmt.Sprintf("采购单 %s 供应商 %s 与结算单供应商 %s 不一致", ln.VoucherID, sh.SupplierID, want),
				map[string]any{"voucher_id": ln.VoucherID, "sheet_supplier": sh.SupplierID, "settlement_supplier": want}))
		}
	}
	return out
}

// ---- check 3: 金额勾稽 ----

// AmountCheck 是金额勾稽的结论摘要。
//
// # 它为什么必须由引擎算好、再原样喂给 LLM
//
// 2026-10-22 生产实踩(审批 54bb82421a08480bbcfdacaf6c814b5e):
// 引擎算出的真实差额是 **0.0054 元**(结算单货款合计 8206.1946 vs 手工 8206.20),
// 在容差内 ⇒ 引擎产出 **0 条**金额信号(audit_logs 里 check_amounts 步写明
// "新增 0 条金额信号")。
//
// 但喂给 LLM 的 user prompt 里是 **StringFixed(2) 的显示值**
// (7765.19 + 441.00 = 8206.19),LLM 自己加出"手工 8206.20 与合计 8206.19 差 0.01",
// 当成 block 理由把整单拒了 —— **凭空造出一个引擎没发现的"账实不符"**。
//
// ⇒ 金额结论只有一个口径:引擎算完给 LLM,**不给它自己相加的机会**。
type AmountCheck struct {
	// ManualAmountYuan 是申请人手工累加的付款额。
	ManualAmountYuan decimal.Decimal
	// SettledTotalYuan 是勾选结算单的货款合计(引擎口径)。
	SettledTotalYuan decimal.Decimal
	// PaymentTotalYuan 是手工付款项合计（水电费/租金/…）。
	PaymentTotalYuan decimal.Decimal
	// ReceiptTotalYuan 是手工收款项合计（陈列费/堆头费/…）。
	ReceiptTotalYuan decimal.Decimal
	// NetPayableYuan 是净额 = 货款 + 付款项 − 收款项（**可能为负**）。
	NetPayableYuan decimal.Decimal
	// PayableYuan 是**真正要付的钱** = max(0, 净额)。
	//
	// ⚠️ 手工金额要跟这个比、不是跟货款合计比：收款项**冲减**付款金额
	//   （用户口径 2026-10-25：「收款项冲减付款金额」）⇒ 跟货款合计比会
	//   把"填了净额"的人判成不一致。
	PayableYuan decimal.Decimal
	// DiffYuan 是 |手工 - 应付|。
	DiffYuan decimal.Decimal
	// ToleranceYuan 是付款取整容差(元)。
	ToleranceYuan decimal.Decimal
	// Consistent 为 true 表示差异在取整容差内 —— 属正常取整,不构成阻断。
	Consistent bool
}

// yuanText 把金额打成"人看得懂"的样子:最多 4 位小数、去掉尾随 0。
//
// # 为什么不用 StringFixed(2)
//
// 生产实踩(2026-10-22):差额 0.014 元被 StringFixed(2) 打成 "0.01",
// 看的人(包括 LLM)以为只差 1 分,实际已经过了 0.01 的数值容差 ——
// **显示值比真实值小 ⇒ 人会低估问题的量级**,于是该拦的没拦、不该拦的拦了。
// ⇒ 差额一律按"能显示几位就显示几位"打印。
func yuanText(d decimal.Decimal) string {
	return d.Round(4).String()
}

// checkAmounts 两层勾稽:
//
//	a) 每张结算单:Σ(PI/RO 行 total_goods_yuan) vs settlement.total_goods_yuan
//	   —— **同一套源数据的自洽性**,容差用数值容差(默认 0.01 元),不放宽:
//	   单头与明细都来自思迅同一张单,差一分钱都要看;
//	b) 手工累加金额 vs **应付金额**(= 货款 + 付款项 − 收款项,不为负)
//	   —— **付款口径**,容差用付款取整容差(默认 1 元):
//	   实际付款按元付,分位一般不实际支付、角位也可能被舍去,
//	   分位/角位差异是正常取整,不是账实不符(2026-10-22 生产实踩,见 AmountCheck 注释)。
//
// 两层容差**刻意不同**:把付款的取整口径套到"单头 vs 明细"上,
// 会把源数据自身的不自洽也一并放过。
//
// ⚠️ b) 比的是**应付金额**而不是货款合计:收款项冲减付款金额
//
//	(用户口径 2026-10-25),填了净额的人不该被判成"与货款合计不符"。
//
// 返回信号 + 金额结论摘要(给 LLM 用,见 AmountCheck)。
func checkAmounts(settlements []SettlementRow, linesBySettlement map[string][]SettlementLineRow, manualAmount, tolerance, paymentTolerance, paymentTotal, receiptTotal decimal.Decimal) ([]signalDraft, AmountCheck) {
	var out []signalDraft
	grandTotal := decimal.Zero
	for _, st := range settlements {
		grandTotal = grandTotal.Add(st.TotalGoodsYuan)
		// 联营帐款单(LY)与代销帐款单(DX)都没有 PI/RO 明细行(明细按销售额/
		// 结算背书,见 joint.go / dx.go),它们的单头金额与明细自洽由 cube 的
		// settlement_ly / settlement_dx 口径保证 ⇒ 跳过本层校验,
		// 只计入 grandTotal(否则会凭"明细合计 0"误报 LineAmountMismatch)。
		if isJointOrDelivery(st.DocumentType) {
			continue
		}
		lineSum := decimal.Zero
		for _, ln := range linesBySettlement[st.ID] {
			lineSum = lineSum.Add(ln.TotalGoodsYuan)
		}
		diff := lineSum.Sub(st.TotalGoodsYuan).Abs()
		if diff.GreaterThan(tolerance) {
			out = append(out, newSignal(model.SignalBlock, SigLineAmountMismatch, st.ID, "",
				fmt.Sprintf("结算单 %s 金额勾稽不符:明细合计 %s ≠ 单头货款 %s(差 %s 元)",
					st.ID, yuanText(lineSum), yuanText(st.TotalGoodsYuan), yuanText(diff)),
				map[string]any{
					"settlement_id": st.ID,
					"line_sum":      lineSum.String(),
					"header_goods":  st.TotalGoodsYuan.String(),
					"diff":          diff.String(),
				}))
		}
	}
	totals := computeTotals(grandTotal, paymentTotal, receiptTotal)
	diff := totals.PayableYuan.Sub(manualAmount).Abs()
	// 付款取整口径:差额 **小于** 1 元 = 同一笔付款(分位不实际支付、角位可能被舍去)。
	summary := AmountCheck{
		ManualAmountYuan: manualAmount,
		SettledTotalYuan: grandTotal,
		PaymentTotalYuan: paymentTotal,
		ReceiptTotalYuan: receiptTotal,
		NetPayableYuan:   totals.NetYuan,
		PayableYuan:      totals.PayableYuan,
		DiffYuan:         diff,
		ToleranceYuan:    paymentTolerance,
		Consistent:       diff.LessThan(paymentTolerance),
	}
	if !summary.Consistent {
		out = append(out, newSignal(model.SignalBlock, SigManualAmountMismatch, "", "",
			fmt.Sprintf("手工累加付款金额 %s 元 与应付金额 %s 元(货款 %s + 付款项 %s - 收款项 %s)不符(差 %s 元,已达付款取整容差 %s 元)",
				yuanText(manualAmount), yuanText(totals.PayableYuan), yuanText(grandTotal),
				yuanText(paymentTotal), yuanText(receiptTotal),
				yuanText(diff), yuanText(paymentTolerance)),
			map[string]any{
				"manual_amount": manualAmount.String(),
				"settled_total": grandTotal.String(),
				"payment_total": paymentTotal.String(),
				"receipt_total": receiptTotal.String(),
				"payable":       totals.PayableYuan.String(),
				"diff":          diff.String(),
				"tolerance":     paymentTolerance.String(),
			}))
	}
	return out, summary
}

// ---- check 4: 数量信号(采购量 vs 销售/库存) ----

// ProductLabel 给商品起一个"人能认出来"的名字:「<商品名称>-<商品货号>」。
//
// # 为什么信号里要带名字(2026-10-22 用户口径)
//
// 审批结论里涉及到商品时要写「<商品名称>-<商品货号>」:
// 店长看到 "02000116" 无从判断这是不是自家在卖的商品;
// 看到 "16.8升琅悦泉水桶装水-02000116" 立刻知道是桶装水。
//
// 查不到名字时**回退到纯货号** —— 不编造名字,也不因为查不到名字就不发信号。
func ProductLabel(names map[string]string, productID string) string {
	name := strings.TrimSpace(names[productID])
	if name == "" {
		return productID
	}
	return name + "-" + productID
}

// QuantityContext 是数量信号的上下文。
//
// # 为什么信号要带商品名和窗口起点
//
//   - **商品名**:审批结论给发起人看,只写货号没人认得(用户口径 2026-10-22:
//     写「<商品名称>-<商品货号>」)。
//   - **窗口起点**:信号的结论是"从哪天起没有销售记录" —— 把日期写进消息,
//     人才判断得了(而不是看一个"窗口内无销售"的空口径)。
type QuantityContext struct {
	// Names 是货号 → 商品名(可空 ⇒ 只写货号)。
	Names map[string]string
	// WindowFrom 是货号 → 销量窗口起点(该商品最后一次采购入库日,YYYY-MM-DD)。
	WindowFrom map[string]string
	// Stocking 是货号 → 备货系数事实(可空)。
	//
	// ⚠️ 2026-11-01 用户口径:有备货系数的 SKU **以「合理备货量」为准**判进货是否过量,
	// 不再用「自最后一次采购入库以来的销量」当分母 —— 刚收货一两天销量天然≈0,
	// 分母塌成 0 会把正常备货判成 ×13 的严重偏离(生产实踩:乌鸡蛋-00330 进货 13.5、
	// 合理备货量 9.25 ⇒ 实际只有 1.46 倍,却被 block 拒单)。
	Stocking map[string]StockingFact
	// Purchase 是货号 → 本次采购口径(净进货量 / 退货 / 有效收货日跨度)。
	//
	// ⚠️ 2026-11-01 用户口径:10 天合并付款的进货量是**多日累加值**,不能直接比
	// 单次的合理备货量 ⇒ 传进 checkQuantity 的 purchaseByProduct 已经是
	// 多日 SKU 的**日均采购量**(见 purchase_basis.go)。这里只负责把口径写进消息。
	Purchase map[string]purchaseBasis
	// Groups 是**组级**备货事实（2026-11-01 新口径，见 stocking_group.go）。
	//
	// 有组事实的商品不再走下面 checkQuantity 的单 SKU 老口径
	// （Skip 里的货号会被跳过）—— 组口径已经把它们归并到一起判过一次。
	Groups []StockingGroupFact
	// Skip 是**已由组口径处理**的货号集合（checkQuantity 跳过它们）。
	Skip map[string]struct{}
}

// checkQuantity 对每个进货商品比较窗口采购量与窗口**消耗量**:
//
//   - 有备货系数(见 QuantityContext.Stocking)的商品:**以「合理备货量」为准**
//     —— 进货 > factor 倍合理备货量才 block;2~factor 倍由 checkStockingExcess 给 warn;
//     其余不出信号(刚收货一两天「自入库以来销量≈0」是正常现象,不再据此 block);
//   - 无系数(算不出备货系数)才沿用老口径:
//   - 消耗量 = 窗口销量 + 窗口盘损(报损/库存调整减少,见 aggregateLoss);
//   - 消耗量 == 0 且进货 > 0 → block(purchase_without_sales);
//   - 进货 > 消耗量 × factor×2 → block(qty_deviation);
//   - 进货 > 消耗量 × factor   → warn(qty_deviation);
//   - 原本(不扣盘损)会 block、扣掉盘损后不再 block → 降级 warn
//     (用户 2026-10-31 口径:有盘损单解释去向,就不再阻断付款审批)。
//
// # 为什么盘损要并进"销量"
//
// 进货 10、窗口只卖 0.9,看起来"买了 10 倍卖不动";但门店同期报损 8.4,
// 货其实有去向(破损/过期/被偷)。只拿销量做分母会把这类正常损耗当成虚进。
// ⇒ 分母改成 销量 + 盘损。**证据必须真实存在且已审核**才计入(见 SumLossByProduct)。
//
// ⚠️ **消息只陈述事实**(哪个商品、进了多少、卖了多少、盘损多少),不做动机推断 ——
// 这些文字是给**发起人**看的,推断性措辞("虚进""套取付款""风险")会让人不适,
// 而且判断本来就不该由机器下(见 llm.go 的措辞约束)。
//
// received 只统计 PI(进货);RO(退货)按负量冲减由 caller 聚合时处理
// (退货量大是另一类信号,本期不做)。
//
// ⚠️ purchaseByProduct 是**用于比较的**采购量:多日合并付款的 SKU 传进来的是
// 日均采购量(见 purchase_basis.go 与 QuantityContext.Purchase),单日 SKU 是
// 净进货量。别在这里再除一次天数。
func checkQuantity(purchaseByProduct map[string]decimal.Decimal, salesByProduct map[string]SaleRow, lossByProduct map[string]decimal.Decimal, factor float64, qc QuantityContext) []signalDraft {
	var out []signalDraft
	f := decimal.NewFromFloat(factor)
	f2 := decimal.NewFromFloat(factor * 2)
	for pid, recv := range purchaseByProduct {
		if !recv.GreaterThan(decimal.Zero) {
			continue
		}
		if _, skip := qc.Skip[pid]; skip {
			// 已由组级口径判过（组销量/组库存已含该货号）。
			continue
		}
		label := ProductLabel(qc.Names, pid)
		from := qc.WindowFrom[pid]
		basis := qc.Purchase[pid]

		// ---- 备货系数口径（2026-11-01 用户口径）----
		//
		// 有系数 ⇒ 只按「合理备货量」判:超过 factor 倍才 block(严重过量),
		// 2~factor 倍由 checkStockingExcess 给 warn ⇒ 这里不再出信号。
		// 没有系数(算不出)才回退到「自最后一次采购入库以来销量」的老口径。
		if fact, ok := qc.Stocking[pid]; ok && fact.ReasonableQty.GreaterThan(decimal.Zero) {
			ratio := recv.Div(fact.ReasonableQty)
			if ratio.GreaterThan(f) {
				params := map[string]any{
					"product_id":       pid,
					"received":         recv.String(),
					"basis":            "stocking",
					"period_days":      fact.PeriodDays,
					"multiplier":       fact.Multiplier.String(),
					"period_sales_qty": fact.PeriodSalesQty.String(),
					"reasonable_qty":   fact.ReasonableQty.String(),
					"ratio":            ratio.String(),
					"sample_days":      fact.SampleDays,
					"in_stock_days":    fact.InStockDays,
					"stock_qty":        fact.StockQty.String(),
					"coverage_days":    fact.CoverageDays.String(),
					"window_from":      from,
				}
				mergeBasisParams(params, basis)
				out = append(out, newSignal(model.SignalBlock, SigQtyDeviation, "", pid,
					fmt.Sprintf("商品 %s %s,按备货系数(周期 %d 天、倍数 %s)周期销售数 %s、合理备货量 %s,%s约为其 %s 倍",
						label, purchaseText(basis, recv), fact.PeriodDays, fact.Multiplier.StringFixed(2),
						fact.PeriodSalesQty.String(), fact.ReasonableQty.String(),
						ratioLabel(basis), ratio.StringFixed(1)),
					params))
			}
			continue
		}

		sale := salesByProduct[pid]
		sold := sale.NetSaleQuantity
		loss := lossByProduct[pid]
		covered := sold.Add(loss)

		if !covered.GreaterThan(decimal.Zero) {
			msg := fmt.Sprintf("商品 %s 自最后一次采购入库日至今无销售记录,%s", label, purchaseText(basis, recv))
			if from != "" {
				msg = fmt.Sprintf("商品 %s 自 %s 采购入库后无销售记录,%s", label, from, purchaseText(basis, recv))
			}
			params := map[string]any{"product_id": pid, "received": recv.String(), "sold": "0", "window_from": from}
			mergeBasisParams(params, basis)
			out = append(out, newSignal(model.SignalBlock, SigPurchaseWithoutSales, "", pid, msg, params))
			continue
		}

		ratio := recv.Div(covered)
		params := map[string]any{
			"product_id": pid, "received": recv.String(), "sold": sold.String(),
			"loss": loss.String(), "covered": covered.String(), "ratio": ratio.String(),
			"window_from": from,
		}
		mergeBasisParams(params, basis)
		switch {
		case recv.GreaterThan(covered.Mul(f2)):
			out = append(out, newSignal(model.SignalBlock, SigQtyDeviation, "", pid,
				fmt.Sprintf("商品 %s %s,%s,约为其 %s 倍", label, purchaseText(basis, recv),
					consumptionText(from, sold, loss), ratio.StringFixed(1)),
				params))
		case recv.GreaterThan(covered.Mul(f)):
			out = append(out, newSignal(model.SignalWarn, SigQtyDeviation, "", pid,
				fmt.Sprintf("商品 %s %s,%s,约为其 %s 倍", label, purchaseText(basis, recv),
					consumptionText(from, sold, loss), ratio.StringFixed(1)),
				params))
		case loss.GreaterThan(decimal.Zero) && rawQuantityBlock(recv, sold, f2):
			// 盘损把原本的 block 消掉了 ⇒ 降级为 warn(不阻断,但留痕)。
			out = append(out, newSignal(model.SignalWarn, SigQtyDeviation, "", pid,
				fmt.Sprintf("商品 %s %s,%s,约为其 %s 倍;已核对盘损单,偏离降至可接受范围",
					label, purchaseText(basis, recv), consumptionText(from, sold, loss), ratio.StringFixed(1)),
				params))
		}
	}
	return out
}

// checkGroupQuantity 按**组级**备货事实出数量信号（2026-11-01 用户口径）。
//
// 判定顺序（见 stocking_group.go 的口径说明）：
//
//  1. 单位不可换算 ⇒ 只发 warn（无法比较不等于超量），不做数量阻断；
//  2. 组内近 180 天无销量 ⇒ 按"刚收货 / 首单 / 长期无销售 / 账实缺口"分类，
//     只有**长期无销售**（有库存且最后一次收货超过 30 天）才 block，且注明需盘点；
//  3. 本次进货 ≤ 下限 ⇒ 不是压货（最小包装进货），不出信号；
//  4. 补货后可售天数 ≤ 可售上限 ⇒ 通过；
//  5. 超过「可售上限 × factor」**或**超量金额 > 300 元 ⇒ block；
//     其余超过上限的情况 ⇒ warn（只提示，不阻断）；
//  6. 价格高于历史均价 10% / 账面进货−销售−库存对不上 ⇒ 独立 warn 事实。
//
// ⚠️ 消息只陈述事实，不做动机推断（措辞纪律见 llm.go 的 prompt 约束）。
func checkGroupQuantity(groups []StockingGroupFact, factor float64, today string) []signalDraft {
	var out []signalDraft
	f := decimal.NewFromFloat(factor)
	for _, g := range groups {
		evidence := groupEvidence(g)
		// 价格与账面结存是与数量判定**无关**的独立事实，先出。
		if g.PriceRisePct.GreaterThan(decimal.NewFromFloat(stockingPriceRiseRatio * 100)) {
			out = append(out, newSignal(model.SignalWarn, SigPurchasePriceAboveHistory, "", g.ProductID,
				fmt.Sprintf("商品组 %s 本次进货均价 %s 元/%s,近 %d 天加权均价 %s 元/%s,高 %s%%",
					g.Label, factorText(g.PriceThis), g.BaseUnit, g.SampleDays,
					factorText(g.PriceHistory), g.BaseUnit, g.PriceRisePct.StringFixed(1)),
				evidence))
		}
		if g.ZeroSalesCase == "" {
			out = append(out, checkGroupLedger(g, evidence)...)
		}

		if g.Unconvertible {
			note := g.UnitNote
			if note == "" {
				note = "采购行单位与商品档案单位不一致"
			}
			out = append(out, newSignal(model.SignalWarn, SigUnitUnconvertible, "", g.ProductID,
				fmt.Sprintf("商品组 %s %s;单位说明:%s,该组本次未做数量比较",
					g.Label, groupPurchaseText(g), note),
				evidence))
			continue
		}

		switch g.ZeroSalesCase {
		case "recent_receipt":
			days := daysSince(g.LastDate, today)
			out = append(out, newSignal(model.SignalWarn, SigRecentReceiptNoSales, "", g.ProductID,
				fmt.Sprintf("商品组 %s %s;本次到货 %s(距今 %d 天),组内近 %d 天无销售记录,按口径刚收货不判定",
					g.Label, groupPurchaseText(g), g.LastDate, days, g.SampleDays),
				evidence))
			continue
		case "new_product":
			out = append(out, newSignal(model.SignalWarn, SigNewProductNoSales, "", g.ProductID,
				fmt.Sprintf("商品组 %s %s;近 %d 天没有进货记录,也没有销售记录,按口径首单不判定",
					g.Label, groupPurchaseText(g), g.SampleDays),
				evidence))
			continue
		case "long_stock":
			out = append(out, newSignal(model.SignalBlock, SigPurchaseWithoutSales, "", g.ProductID,
				fmt.Sprintf("商品组 %s %s;组内库存 %s,最后一次进货 %s(距今 %d 天),近 %d 天无销售记录%s;阻断条件:需盘点核对库存",
					g.Label, groupPurchaseText(g), qtyUnitText(g.StockQty, g.BaseUnit),
					g.LastReceipt, daysSince(g.LastReceipt, today), g.SampleDays, groupLossText(g)),
				evidence))
			continue
		case "ledger_gap":
			out = append(out, newSignal(model.SignalWarn, SigStockLedgerGap, "", g.ProductID,
				fmt.Sprintf("商品组 %s %s;组内库存 %s,近 %d 天与近 %d 天均无销售记录%s",
					g.Label, groupPurchaseText(g), qtyUnitText(g.StockQty, g.BaseUnit),
					g.SampleDays, stockingRecentSaleDays, groupLossText(g)),
				evidence))
			continue
		}

		// 本次进货不超过「通常一次进多少」⇒ 不是压货。
		if g.ReceivedQty.LessThanOrEqual(g.FloorQty) {
			continue
		}
		if !g.CoverageDays.GreaterThan(decimal.Zero) {
			continue
		}
		if g.CoverageDays.LessThanOrEqual(decimal.NewFromInt(int64(g.CapDays))) {
			continue
		}
		// 超过可售上限：看"超了多少倍"与"超了多少钱"两个硬阈值。
		over := g.CoverageDays.Div(decimal.NewFromInt(int64(g.CapDays))).Round(1)
		severe := g.CoverageDays.GreaterThan(
			decimal.NewFromInt(int64(g.CapDays)).Mul(f))
		amount := g.ExcessYuan.GreaterThan(decimal.NewFromInt(stockingExcessAmountYuan))
		msg := fmt.Sprintf("商品组 %s %s;%s,补货后可售 %s 天,超过可售上限 %d 天的 %s 倍",
			g.Label, groupPurchaseText(g), groupStockText(g),
			g.CoverageDays.StringFixed(1), g.CapDays, over.StringFixed(1))
		if g.ExcessYuan.GreaterThan(decimal.Zero) {
			msg += fmt.Sprintf(",超出约 %s", yuanText(g.ExcessYuan))
		}
		if severe || amount {
			out = append(out, newSignal(model.SignalBlock, SigQtyDeviation, "", g.ProductID, msg, evidence))
			continue
		}
		out = append(out, newSignal(model.SignalWarn, SigStockingExcess, "", g.ProductID, msg, evidence))
	}
	return out
}

// checkGroupLedger 出「进货 − 销售 − 盘损 − 库存」对不上账的提示（只 warn）。
//
// 阈值 = max(3 个基准单位, 180 天进货量的 20%)：小数量商品的绝对差没意义，
// 大数量商品的相对差才说明账实有缺口。方向不分正负（多了少了都要核对）。
func checkGroupLedger(g StockingGroupFact, evidence map[string]any) []signalDraft {
	if g.Unconvertible {
		return nil
	}
	purchases := g.LedgerGap.Add(g.SalesQty).Add(g.LossQty).Add(g.StockQty)
	floor := purchases.Mul(decimal.NewFromFloat(stockingLedgerGapRatio))
	if min := decimal.NewFromInt(stockingLedgerGapMinQty); floor.LessThan(min) {
		floor = min
	}
	if !g.LedgerGap.Abs().GreaterThan(floor) {
		return nil
	}
	lossText := ""
	if g.LossQty.GreaterThan(decimal.Zero) {
		lossText = "、盘损 " + qtyUnitText(g.LossQty, g.BaseUnit)
	}
	return []signalDraft{newSignal(model.SignalWarn, SigStockLedgerGap, "", g.ProductID,
		fmt.Sprintf("商品组 %s 近 %d 天进货 %s、销售 %s%s、当前库存 %s,进货−销售−盘损−库存 = %s",
			g.Label, g.SampleDays, qtyUnitText(purchases, g.BaseUnit),
			qtyUnitText(g.SalesQty, g.BaseUnit), lossText, qtyUnitText(g.StockQty, g.BaseUnit),
			qtyUnitText(g.LedgerGap, g.BaseUnit)),
		evidence)}
}

// groupLossText 渲染「期间盘损 X」片段（无盘损 ⇒ 空串）。
//
// 零销量分支也带上它：报损/盘亏是货的真实去向，写清楚才不会让审批人
// 把"没有销售"读成"货一直压在仓里"。
func groupLossText(g StockingGroupFact) string {
	if !g.LossQty.GreaterThan(decimal.Zero) {
		return ""
	}
	return fmt.Sprintf(",期间盘损 %s", qtyUnitText(g.LossQty, g.BaseUnit))
}

// groupPurchaseText 渲染组消息里的「本次进货」片段（多日写成累计+天数）。
func groupPurchaseText(g StockingGroupFact) string {
	s := "本次进货 " + qtyUnitText(g.ReceivedQty, g.BaseUnit)
	if g.PurchaseDays > 1 {
		if g.FirstDate != "" && g.LastDate != "" {
			s = fmt.Sprintf("本次进货累计 %s(%s~%s 共 %d 天)",
				qtyUnitText(g.ReceivedQty, g.BaseUnit), g.FirstDate, g.LastDate, g.PurchaseDays)
		} else {
			s = fmt.Sprintf("本次进货累计 %s(共 %d 天)",
				qtyUnitText(g.ReceivedQty, g.BaseUnit), g.PurchaseDays)
		}
	}
	if g.ReturnedQty.GreaterThan(decimal.Zero) {
		s += "(已扣退货 " + qtyUnitText(g.ReturnedQty, g.BaseUnit) + ")"
	}
	return s
}

// groupStockText 渲染组消息里的「库存 + 销量 + 上限」片段。
func groupStockText(g StockingGroupFact) string {
	s := fmt.Sprintf("组内库存 %s,补货后合计 %s,近 %d 天组销量 %s",
		qtyUnitText(g.StockQty, g.BaseUnit), qtyUnitText(g.AfterQty, g.BaseUnit),
		g.SampleDays, qtyUnitText(g.SalesQty, g.BaseUnit))
	// 盘损也是货的去向：写出来，审批人和机器才看得懂日均是怎么来的。
	if g.LossQty.GreaterThan(decimal.Zero) {
		s += "、组盘损 " + qtyUnitText(g.LossQty, g.BaseUnit)
	}
	s += fmt.Sprintf("(有货 %d 天,日均消耗 %s),可售上限 %d 天(%s)",
		g.InStockDays, qtyUnitText(g.AvgDailySales, g.BaseUnit), g.CapDays,
		qtyUnitText(g.CapQty, g.BaseUnit))
	if g.AllowanceBoost {
		s += ";本次含陈列费/堆头费,可售上限按 ×1.5 计"
	}
	// 进货量级事实：本单是历史常见单次进货量的几倍。
	if g.SingleMedian.GreaterThan(decimal.Zero) && g.ReceivedQty.GreaterThan(decimal.Zero) {
		ratio := g.ReceivedQty.Div(g.SingleMedian).Round(1)
		s += fmt.Sprintf(";近 %d 天单次进货中位数 %s,本单约为其 %s 倍",
			g.SampleDays, qtyUnitText(g.SingleMedian, g.BaseUnit), ratio.StringFixed(1))
	}
	return s
}

// groupEvidence 组装组级信号的证据参数（审批详情页 / LLM 核对用）。
func groupEvidence(g StockingGroupFact) map[string]any {
	return map[string]any{
		"group_key":        g.GroupKey,
		"product_id":       g.ProductID,
		"members":          g.MemberIDs,
		"base_unit":        g.BaseUnit,
		"received":         g.ReceivedQty.String(),
		"received_gross":   g.ReceivedGross.String(),
		"returned":         g.ReturnedQty.String(),
		"received_amount":  g.ReceivedAmountYuan.String(),
		"purchase_days":    g.PurchaseDays,
		"first_date":       g.FirstDate,
		"last_date":        g.LastDate,
		"stock_qty":        g.StockQty.String(),
		"after_qty":        g.AfterQty.String(),
		"sales_qty":        g.SalesQty.String(),
		"loss_qty":         g.LossQty.String(),
		"recent_sales_qty": g.RecentSalesQty.String(),
		"in_stock_days":    g.InStockDays,
		"avg_daily_sales":  g.AvgDailySales.String(),
		"cap_days":         g.CapDays,
		"cap_qty":          g.CapQty.String(),
		"coverage_days":    g.CoverageDays.String(),
		"excess_qty":       g.ExcessQty.String(),
		"excess_yuan":      g.ExcessYuan.String(),
		"floor_qty":        g.FloorQty.String(),
		"single_median":    g.SingleMedian.String(),
		"price_this":       g.PriceThis.String(),
		"price_history":    g.PriceHistory.String(),
		"price_rise_pct":   g.PriceRisePct.String(),
		"ledger_gap":       g.LedgerGap.String(),
		"zero_sales_case":  g.ZeroSalesCase,
		"unconvertible":    g.Unconvertible,
		"unit_note":        g.UnitNote,
		"allowance_boost":  g.AllowanceBoost,
	}
}

// qtyUnitText 渲染「数量 + 单位」（去掉无意义的尾零）。
func qtyUnitText(d decimal.Decimal, unit string) string {
	if strings.TrimSpace(unit) == "" {
		unit = "单位"
	}
	return factorText(d) + " " + unit
}

// purchaseText 渲染消息里的「本次进货」片段。
//
//	单日:      "本次进货 100"
//	多日:      "本次进货累计 220(2026-09-18~2026-09-27 共 10 天,日均 22)"
//	有退货:    末尾补 "(已扣退货 30)"
//
// ⚠️ 多日必须把「累计 / 天数 / 日均」写全 —— 审批人看到"进货 220"会以为
// 是一次进了 220,实际是十天合并付款(2026-11-01 用户口径)。
//
// fallback 是**比较用的量**(多日时是日均),basis 缺失(老调用方/单测)时用它。
func purchaseText(b purchaseBasis, fallback decimal.Decimal) string {
	if b.Days <= 0 && b.NetQty.IsZero() && b.ReturnedQty.IsZero() {
		return "本次进货 " + fallback.String()
	}
	var s string
	if b.Days > 1 {
		s = fmt.Sprintf("本次进货累计 %s(%s~%s 共 %d 天,日均 %s)",
			b.NetQty.String(), b.FirstDate, b.LastDate, b.Days, b.CompareQty().String())
	} else {
		s = "本次进货 " + b.NetQty.String()
	}
	if b.ReturnedQty.GreaterThan(decimal.Zero) {
		s = fmt.Sprintf("%s(已扣退货 %s)", s, b.ReturnedQty.String())
	}
	return s
}

// ratioLabel 渲染倍数的口径词:多日比的是日均,单日比的是进货量本身。
func ratioLabel(b purchaseBasis) string {
	if b.Days > 1 {
		return "日均进货"
	}
	return "进货"
}

// mergeBasisParams 把采购口径写进信号参数(审批详情页/LLM 核对要用)。
//
// 单日且无退货 ⇒ 不写(与历史信号保持一致,不制造噪音)。
func mergeBasisParams(params map[string]any, b purchaseBasis) {
	if b.Days <= 1 && b.ReturnedQty.IsZero() {
		return
	}
	params["received_total"] = b.NetQty.String()
	params["returned"] = b.ReturnedQty.String()
	params["purchase_days"] = b.Days
	params["daily_avg"] = b.CompareQty().String()
}

// consumptionText 把"窗口消耗量"渲染成一句话里的片段(带盘损时写出合计)。
//
// 不带盘损: "<from> 以来销量 10"
// 带盘损:   "<from> 以来销量 0.91 加盘损 8.43(合计 9.34)"
func consumptionText(from string, sold, loss decimal.Decimal) string {
	if loss.GreaterThan(decimal.Zero) {
		return fmt.Sprintf("%s 以来销量 %s 加盘损 %s(合计 %s)",
			windowText(from), sold.String(), loss.String(), sold.Add(loss).String())
	}
	return fmt.Sprintf("%s 以来销量 %s", windowText(from), sold.String())
}

// rawQuantityBlock 判断"只看销量(不扣盘损)时是否已经构成 block"。
//
// 无销量 → 纯"进货无销售"本身就是 block;否则按 ×factor×2 的阈值判。
func rawQuantityBlock(recv, sold, f2 decimal.Decimal) bool {
	if !sold.GreaterThan(decimal.Zero) {
		return true
	}
	return recv.GreaterThan(sold.Mul(f2))
}

// windowText 把窗口起点渲染成一句话里的片段。
func windowText(from string) string {
	if from == "" {
		return "上次采购入库"
	}
	return from + " 采购入库"
}

// ---- 销量窗口(按商品):从最后一次采购入库日到今天 ----

// saleWindowGroup 是一组"窗口起点相同"的商品 —— 它们共用一次 sale_day 查询。
type saleWindowGroup struct {
	// From 是窗口起点(YYYY-MM-DD,含)。
	From string
	// Products 是该组的商品货号。
	Products []string
}

// maxSaleWindowGroups 是窗口分组的**查询次数上限**。
//
// 每次分组 = 一次 cube sale_day 查询(源表 t_rm_daysum 120 万行,每次约 1.2s),
// 而"分组数 = 不同采购日期数"。一次审批可能勾选几十张结算单、对应上百张采购单
// ⇒ 日期数不可控 ⇒ 查询次数不可控(一次审批跑一分钟)。
//
// ⇒ 超过上限时**把起点最近的两组合并**(取更早的那个起点)。
// 合并方向是**放宽**:窗口更早 ⇒ 统计到的销量更多 ⇒ 只会少拦、不会多拦。
const maxSaleWindowGroups = 8

// windowStartPick 决定每个商品的窗口起点取「最早」还是「最晚」的采购日期。
//
// 两个口径**都真实存在**，别混用：
//
//   - [windowStartLatest] —— **窗口毛利**（预期毛利）用：起点取**最晚**的
//     采购入库日，回答"被审的这次进货进到今天为止，卖了多少"。
//   - [windowStartEarliest] —— **实现毛利**用：起点取该商品在**本单采购单**里
//     **最早**的采购日，回答"这批货从进店那天起一共卖了多少钱/多少量"
//     （2026-10-29 用户口径）。
//
// ⚠️ 起点不同 ⇒ 销量不同 ⇒ 毛利结论可能相反，这是**两个问题的两个答案**，
//
//	不是同一个信号的两种写法。
type windowStartPick int

const (
	// windowStartEarliest 取最早采购日。
	windowStartEarliest windowStartPick = iota
	// windowStartLatest 取最晚采购日。
	windowStartLatest
)

// buildSaleWindowGroups 按"最后一次采购的采购入库单日期"给商品分组。
//
// # 口径(2026-10-22 用户口径)
//
// 销量窗口 = **从最后一次采购的采购入库单日期到当前日期**。
//
// 为什么不用"今天往前 N 天"的固定窗口:
// 进货发生在 2026-08-25,却拿 2026-09-22~10-22 的销量去比 ——
// **被审的那次进货根本不在窗口里**,判的是"这批货卖不卖得动"却用了一个
// 与进货无关的窗口。滞销口径应该是"自我们进货以来,它卖了多少"。
//
// # 为什么起点取"本次审核里的最后一次进货",不是"该商品历史上的最后一次进货"
//
// 商品可能在本次审核之后又进过货(那批货不在本次结算里)⇒
// 用历史上的最后一次进货当起点,窗口反而不含**被审的这次进货** ——
// 正是上面要修的那个毛病。
//
// 参数:
//
//   - sheetDates:采购单 id → 制单日期(YYYY-MM-DD)
//   - productSheets:商品 → 它出现过的采购单 id 列表
//   - today:今天
//   - fallback:取不到日期时的回退起点(今天 - SaleWindowDays)
//   - maxGroups:分组数上限(≤0 ⇒ 用 maxSaleWindowGroups)
func buildSaleWindowGroups(sheetDates map[string]string, productSheets map[string][]string, today, fallback string, maxGroups int) []saleWindowGroup {
	return buildSaleWindowGroupsByDate(
		sheetDates, productSheets, today, fallback, maxGroups, windowStartLatest)
}

// buildSaleWindowGroupsByDate 是 buildSaleWindowGroups 的通用版：
// 窗口起点按 [pick] 取「最早」或「最晚」的采购日期。
//
// ⚠️ 合并超限分组时的方向：**取更早的起点**（放宽方向）。
//   - 对窗口毛利（最晚起点）⇒ 放宽 = 窗口更长 = 统计到更多销量；
//   - 对实现毛利（最早起点）⇒ 放宽 = 窗口更长 = 销售均价更平滑。
//     两种口径下"放宽"都只会让结论更接近"整体水平",不会凭空造出亏损。
func buildSaleWindowGroupsByDate(sheetDates map[string]string, productSheets map[string][]string, today, fallback string, maxGroups int, pick windowStartPick) []saleWindowGroup {
	if maxGroups <= 0 {
		maxGroups = maxSaleWindowGroups
	}
	// 每个商品 → 窗口起点(它出现过的采购单里最晚/最早的制单日期)。
	fromByProduct := make(map[string]string, len(productSheets))
	for pid, sheetIDs := range productSheets {
		from := ""
		for _, sid := range sheetIDs {
			d, ok := sheetDates[sid]
			if !ok || d == "" {
				continue
			}
			if from == "" {
				from = d
				continue
			}
			if (pick == windowStartLatest && d > from) ||
				(pick == windowStartEarliest && d < from) {
				from = d
			}
		}
		if from == "" || from > today {
			// 取不到日期 / 日期在未来(脏数据)⇒ 回退固定窗口。
			from = fallback
		}
		fromByProduct[pid] = from
	}
	// 按起点归组(起点升序,商品按货号排序 ⇒ 输出稳定可测)。
	byFrom := make(map[string][]string, len(fromByProduct))
	for pid, from := range fromByProduct {
		byFrom[from] = append(byFrom[from], pid)
	}
	groups := make([]saleWindowGroup, 0, len(byFrom))
	for from, pids := range byFrom {
		sort.Strings(pids)
		groups = append(groups, saleWindowGroup{From: from, Products: pids})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].From < groups[j].From })

	// 分组数超上限 ⇒ 反复合并"起点最近"的两组(取更早的起点)。
	for len(groups) > maxGroups {
		best, bestGap := -1, 0
		for i := 0; i+1 < len(groups); i++ {
			gap := dateGapDays(groups[i].From, groups[i+1].From)
			if best == -1 || gap < bestGap {
				best, bestGap = i, gap
			}
		}
		if best < 0 {
			break
		}
		merged := saleWindowGroup{
			From:     groups[best].From, // 取更早的起点(放宽方向)
			Products: append(append([]string{}, groups[best].Products...), groups[best+1].Products...),
		}
		sort.Strings(merged.Products)
		groups = append(groups[:best], append([]saleWindowGroup{merged}, groups[best+2:]...)...)
	}
	return groups
}

// windowFromByProduct 从分组反推每个商品的窗口起点。
//
// 用**分组后**的起点(分组可能被合并,见 buildSaleWindowGroups)——
// 信号里写的日期必须与实际用的窗口一致,否则文字和口径对不上。
func windowFromByProduct(groups []saleWindowGroup) map[string]string {
	out := make(map[string]string)
	for _, g := range groups {
		for _, pid := range g.Products {
			out[pid] = g.From
		}
	}
	return out
}

// dateGapDays 两个 YYYY-MM-DD 之间的天数差(后 - 前);解析失败按 0。
func dateGapDays(a, b string) int {
	ta, err1 := time.Parse("2006-01-02", a)
	tb, err2 := time.Parse("2006-01-02", b)
	if err1 != nil || err2 != nil {
		return 0
	}
	d := int(tb.Sub(ta).Hours() / 24)
	if d < 0 {
		d = 0
	}
	return d
}
