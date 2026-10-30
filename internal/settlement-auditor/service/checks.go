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
//   (用户口径 2026-10-25),填了净额的人不该被判成"与货款合计不符"。
//
// 返回信号 + 金额结论摘要(给 LLM 用,见 AmountCheck)。
func checkAmounts(settlements []SettlementRow, linesBySettlement map[string][]SettlementLineRow, manualAmount, tolerance, paymentTolerance, paymentTotal, receiptTotal decimal.Decimal) ([]signalDraft, AmountCheck) {
	var out []signalDraft
	grandTotal := decimal.Zero
	for _, st := range settlements {
		lineSum := decimal.Zero
		for _, ln := range linesBySettlement[st.ID] {
			lineSum = lineSum.Add(ln.TotalGoodsYuan)
		}
		grandTotal = grandTotal.Add(st.TotalGoodsYuan)
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
}

// checkQuantity 对每个进货商品比较窗口采购量与窗口销量:
//
//   - 窗口销量 == 0 且进货 > 0 → block(purchase_without_sales);
//   - 进货 > 销量 × factor×2   → block(qty_deviation);
//   - 进货 > 销量 × factor     → warn(qty_deviation)。
//
// received 只统计 PI(进货);RO(退货)按负量冲减由 caller 聚合时处理
// (退货量大是另一类信号,本期不做)。
//
// ⚠️ **消息只陈述事实**(哪个商品、进了多少、从哪天起没卖),不做动机推断 ——
// 这些文字是给**发起人**看的,推断性措辞("虚进""套取付款""风险")会让人不适,
// 而且判断本来就不该由机器下(见 llm.go 的措辞约束)。
func checkQuantity(receivedByProduct map[string]decimal.Decimal, salesByProduct map[string]SaleRow, factor float64, qc QuantityContext) []signalDraft {
	var out []signalDraft
	f := decimal.NewFromFloat(factor)
	f2 := decimal.NewFromFloat(factor * 2)
	for pid, recv := range receivedByProduct {
		if !recv.GreaterThan(decimal.Zero) {
			continue
		}
		label := ProductLabel(qc.Names, pid)
		from := qc.WindowFrom[pid]
		sale := salesByProduct[pid]
		sold := sale.NetSaleQuantity
		if !sold.GreaterThan(decimal.Zero) {
			msg := fmt.Sprintf("商品 %s 自最后一次采购入库日至今无销售记录,本次进货 %s", label, recv.String())
			if from != "" {
				msg = fmt.Sprintf("商品 %s 自 %s 采购入库后无销售记录,本次进货 %s", label, from, recv.String())
			}
			out = append(out, newSignal(model.SignalBlock, SigPurchaseWithoutSales, "", pid,
				msg,
				map[string]any{"product_id": pid, "received": recv.String(), "sold": "0", "window_from": from}))
			continue
		}
		ratio := recv.Div(sold)
		switch {
		case recv.GreaterThan(sold.Mul(f2)):
			out = append(out, newSignal(model.SignalBlock, SigQtyDeviation, "", pid,
				fmt.Sprintf("商品 %s 本次进货 %s,为 %s 以来销量 %s 的 %s 倍", label, recv.String(), windowText(from), sold.String(), ratio.StringFixed(1)),
				map[string]any{"product_id": pid, "received": recv.String(), "sold": sold.String(), "ratio": ratio.String(), "window_from": from}))
		case recv.GreaterThan(sold.Mul(f)):
			out = append(out, newSignal(model.SignalWarn, SigQtyDeviation, "", pid,
				fmt.Sprintf("商品 %s 本次进货 %s,为 %s 以来销量 %s 的 %s 倍", label, recv.String(), windowText(from), sold.String(), ratio.StringFixed(1)),
				map[string]any{"product_id": pid, "received": recv.String(), "sold": sold.String(), "ratio": ratio.String(), "window_from": from}))
		}
	}
	return out
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
//    不是同一个信号的两种写法。
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
//   两种口径下"放宽"都只会让结论更接近"整体水平",不会凭空造出亏损。
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
