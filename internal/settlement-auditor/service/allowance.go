// Package service / allowance.go —— 收款项给机器审核的**放宽额度**（2026-10-25 用户口径）。
//
// # 用户口径原文
//
//	「有堆头费，陈列费 - 进货可以多些，
//	   有补差价 - 可以允许毛利降低（小于补差价总额）」
//
// 举例：审批 af9905b6435b485998e38f99f38fa222 的
// 「伊利金典纯牛奶250ml-6907992507095 本次进货 216，为 2026-08-26
//   采购入库以来销量 7 的 30.9 倍」—— 含陈列费/堆头费时**应当通过**。
//
// # 设计原则：只放宽**既有**判断，不新增阻断
//
// 「维持原有的判断规则」——所以这里**不新造 block 信号**（新 block 会把
// 原本能过的单子挡掉，是更大的行为变更）。做法是：
//
//   - 陈列费/堆头费 ⇒ 把**数量偏离**（qty_deviation）由 block 降为 warn，
//     消息里写明放宽依据 ⇒ 规则降级路径不再因它拒绝，LLM 也看得见依据；
//   - 补差价 ⇒ 算出**窗口毛利缺口**，缺口 ≤ 补差价总额时记 info
//     （"允许毛利降低"），超出时记 warn（提醒人工终审）。
//     ⚠️ 只在**存在补差价**时才产生毛利信号 —— 没有补差价的单子
//     行为与改造前完全一致（不制造新噪音、不改变既有结论）。
package service

import (
	"fmt"
	"sort"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// SigGrossMargin 毛利信号类型（warn/info，**永不 block**）。
const SigGrossMargin = "gross_margin"

// applyReceiptAllowance 按放宽额度调整信号级别。
//
// 放宽的是**两个数量类**信号：
//
//   - qty_deviation（进货量是销量若干倍）—— 用户口径「进货可以多些」；
//   - purchase_without_sales（自入库后无销售记录）—— 同理：供应商付了陈列费/
//     堆头费，门店就是**有意多备货/铺货**，短期没卖动是正常的。
//
// ⚠️ 为什么后者也放宽：系统**本来**就允许"申请人的备注"解释数量类信号
//   （system prompt:「若备注说明了信号背后的合理原因…可据此认为相应的数量信号
//   不需要拦截」）⇒ 有结构的「陈列费/堆头费收款」给出同样的 latitude 是
//   **既有口径的延续**，不是新开的后门。
//   而用户的举例单（af9905b6435b485998e38f99f38fa222）里 7 条 block 全是
//   purchase_without_sales ⇒ 只放宽数量偏离的话，那笔单仍然过不了。
//
// ⚠️ 放宽后信号**仍是 warn**（不是消失）⇒ 人工终审照旧看得见这些事实。
//
// 返回新切片:入参可能是共享底层数组的切片,原地改会污染调用方的快照。
func applyReceiptAllowance(drafts []signalDraft, al Allowance) []signalDraft {
	if !al.HasDisplayOrPromotion() || len(drafts) == 0 {
		return drafts
	}
	out := make([]signalDraft, len(drafts))
	copy(out, drafts)
	for i := range out {
		if out[i].Level != model.SignalBlock {
			continue
		}
		switch out[i].SignalType {
		case SigQtyDeviation, SigPurchaseWithoutSales:
			out[i].Level = model.SignalWarn
			out[i].Message += fmt.Sprintf(";本次含陈列费/堆头费收款 ¥%s,按口径「进货可以多些」不作为阻断项",
				yuanText(al.DisplayYuan.Add(al.PromotionYuan)))
		}
	}
	return out
}

// applyStocktakeVerification 按「关联盘点单已审核通过」放宽数量类信号。
//
// # 用户口径（2026-10-30）
//
//	「盘点审核后重新提交付款审批，可以通过机器人审核」。
//
// 盘点单就是对「进货量/有无销售」的**实地核对结果** —— 盘点人已经确认过
// 库存实际数量，机器再用"进货量是销量的 N 倍"拦这单，等于让同一件事审两遍。
//
// ⚠️ 放宽后信号**仍是 warn**（不是消失）⇒ 人工终审照旧看得见这些事实；
// 只降级数量类（SigQtyDeviation / SigPurchaseWithoutSales），
// 单据缺失/未审核/金额勾稽**不受影响**（那些盘点解决不了）。
//
// 返回新切片：入参可能是共享底层数组的切片，原地改会污染调用方的快照。
func applyStocktakeVerification(drafts []signalDraft, verified []StocktakeVerification) []signalDraft {
	if len(verified) == 0 || len(drafts) == 0 {
		return drafts
	}
	ids := make([]string, 0, len(verified))
	for _, v := range verified {
		if v.HeaderID != "" {
			ids = append(ids, v.HeaderID)
		}
	}
	if len(ids) == 0 {
		return drafts
	}
	if len(ids) > 3 {
		ids = ids[:3]
	}
	label := strings.Join(ids, ",")
	out := make([]signalDraft, len(drafts))
	copy(out, drafts)
	for i := range out {
		if out[i].Level != model.SignalBlock {
			continue
		}
		switch out[i].SignalType {
		case SigQtyDeviation, SigPurchaseWithoutSales:
			out[i].Level = model.SignalWarn
			out[i].Message += fmt.Sprintf(";关联盘点单 %s 已审核通过,库存已盘点核对,按口径不作为阻断项", label)
		}
	}
	return out
}

// GrossMargin 是本次审批涉及商品的**窗口毛利估算**。
//
// # 口径（为什么是"估算"）
//
// 毛利 = 销额 − 成本，成本用**库存加权平均成本价**估算
// （cube `stock.avg_cost`，即思迅「库存查询」里的**平均成本价**）。
//
// ⚠️ 2026-10-29 **换过口径**（生产实踩，审批单 202610290024 / CP002609144367）：
//
//	旧口径 = 本次进货均价（进货金额 ÷ 收货量）；
//	新口径 = stock.avg_cost（思迅库存的加权平均成本价）。
//
//	为什么必须换：蜜梨-01860 的旧口径算出 4.6349 元/kg，把一笔**正毛利**
//	算成 -280.57 元 ⇒ 机器审核报了个**假的负毛利**（warn 信号）。
//	核对下来：思迅「库存查询」里该商品的**平均成本价 = 2.82**、库存 70.84 kg，
//	cube `stock.avg_cost` 查出来 = 2.8192，与思迅**逐位一致**；
//	而 4.6349 是**这一笔进货批次**的价格 —— 店里的蜜梨是分批进的
//	（本次 15.75kg @4.63 + 之前 55.09kg @2.30 ⇒ 加权 2.82），
//	拿"最新一批的价格"当"这批货的成本"必然偏。
//
//	⇒ 成本口径必须与**财务核对的同一个数**（思迅库存的平均成本价），
//	  否则机器审核给出的毛利对不上店长在思迅看到的数，信号就是噪音。
//
//	税额口径（销额含税、成本含税）一致，故此处只做**相对判断**
// （毛利是否为负、缺口多大），不做绝对毛利考核。
type GrossMargin struct {
	// AmountYuan 是窗口毛利估算（可正可负）。
	AmountYuan decimal.Decimal
	// Calculated 为 true 表示算得出来（至少一个商品既有进货又有销量）。
	Calculated bool
}

// ProductMargin 是**单个 SKU（货号）**的窗口毛利估算。
//
// 为什么要有它（2026-10-29 用户口径）：
//
//	「检查是否实现"采购明细的 SKU 有负毛利现象时产生 warn 信号"」
//
// 整体毛利把**赚钱的商品和亏钱的商品混在一起**算 —— 整体为正时，
// 某个 SKU 在亏钱这件事会被完全抹掉（其他 SKU 的利润把它盖住了）。
// ⇒ 毛利必须**逐 SKU** 看，才能发现"这笔货卖一单亏一单"的事实。
type ProductMargin struct {
	// ProductID 是货号。
	ProductID string
	// AmountYuan 是该 SKU 的窗口毛利估算（可正可负）。
	AmountYuan decimal.Decimal
	// SalesAmountYuan 是窗口销额。
	SalesAmountYuan decimal.Decimal
	// SalesQuantity 是窗口净销量。
	SalesQuantity decimal.Decimal
	// UnitCostYuan 是该 SKU 的单位成本（元/件）。
	UnitCostYuan decimal.Decimal
	// CostSource 是单位成本的口径来源（见 CostSourceAvgCost / CostSourcePurchase）。
	CostSource string
}

// 成本口径来源（写进 signal evidence，让审核人知道这个数是按哪个口径算的）。
const (
	// CostSourceAvgCost = 库存加权平均成本价（思迅「库存查询」的平均成本价）。
	CostSourceAvgCost = "avg_cost"
	// CostSourcePurchase = 本批次进货均价（无库存成本记录时的回退口径）。
	CostSourcePurchase = "purchase"
)

// CostBasis 是单个 SKU 的成本口径。
type CostBasis struct {
	// UnitCostYuan 是单位成本（元/件）。
	UnitCostYuan decimal.Decimal
	// Source 是口径来源（CostSourceAvgCost / CostSourcePurchase）。
	Source string
}

// estimateGrossMargin 用成本口径估算窗口毛利。
//
// costByProduct:货号 → 成本口径（见 CostBasis）。
// salesByProduct:货号 → 窗口销售汇总。
//
// 只统计**同时**有成本口径与销量的商品：只有销量的算不出成本，
// 只有进货的没有收入 ⇒ 两者都不该进毛利口径。
func estimateGrossMargin(costByProduct map[string]CostBasis, salesByProduct map[string]SaleRow) GrossMargin {
	var gm GrossMargin
	for pid, sale := range salesByProduct {
		cost, ok := costByProduct[pid]
		if !ok || !cost.UnitCostYuan.GreaterThan(decimal.Zero) {
			continue
		}
		if !sale.NetSaleQuantity.GreaterThan(decimal.Zero) {
			continue
		}
		margin := sale.NetSaleAmountYuan.Sub(sale.NetSaleQuantity.Mul(cost.UnitCostYuan))
		gm.AmountYuan = gm.AmountYuan.Add(margin)
		gm.Calculated = true
	}
	return gm
}

// estimateProductMargins 逐 SKU 估算窗口毛利。
//
// ⚠️ 计入集合必须与 estimateGrossMargin **完全一致**（同样要求：既有成本口径、
// 又有正的净销量）—— 否则"逐条之和"与"整体"对不上，两套口径打架。
func estimateProductMargins(costByProduct map[string]CostBasis, salesByProduct map[string]SaleRow) map[string]ProductMargin {
	out := make(map[string]ProductMargin)
	for pid, sale := range salesByProduct {
		cost, ok := costByProduct[pid]
		if !ok || !cost.UnitCostYuan.GreaterThan(decimal.Zero) {
			continue
		}
		if !sale.NetSaleQuantity.GreaterThan(decimal.Zero) {
			continue
		}
		out[pid] = ProductMargin{
			ProductID:       pid,
			AmountYuan:      sale.NetSaleAmountYuan.Sub(sale.NetSaleQuantity.Mul(cost.UnitCostYuan)),
			SalesAmountYuan: sale.NetSaleAmountYuan,
			SalesQuantity:   sale.NetSaleQuantity,
			UnitCostYuan:    cost.UnitCostYuan,
			CostSource:      cost.Source,
		}
	}
	return out
}

// negativeProductMargins 取负毛利的 SKU，按缺口绝对值**从大到小**排序。
//
// 排序理由：信号消息里只列前几条（见 maxNegativeSKUInMessage），
// 必须保证列出来的是**亏得最多的**那几个，而不是随机的。
func negativeProductMargins(per map[string]ProductMargin) []ProductMargin {
	out := make([]ProductMargin, 0)
	for _, pm := range per {
		if pm.AmountYuan.IsNegative() {
			out = append(out, pm)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].AmountYuan.Abs().GreaterThan(out[j].AmountYuan.Abs())
	})
	return out
}

// maxNegativeSKUInMessage 消息里最多列几个负毛利 SKU。
//
// 理由同 service/checks.go 的"只陈述事实"：一条审批可能涉及几十个 SKU，
// 全列出来会把 LLM prompt 撑爆、也没人看得下去。列最亏的几个 + 给出总数即可。
const maxNegativeSKUInMessage = 5

// negativeSKUText 渲染负毛利 SKU 明细（给信号消息用）。
func negativeSKUText(neg []ProductMargin, names map[string]string) string {
	if len(neg) == 0 {
		return ""
	}
	parts := make([]string, 0, len(neg))
	for i, pm := range neg {
		if i >= maxNegativeSKUInMessage {
			break
		}
		parts = append(parts, fmt.Sprintf("%s(%s 元)",
			ProductLabel(names, pm.ProductID), yuanText(pm.AmountYuan)))
	}
	txt := strings.Join(parts, "、")
	if len(neg) > maxNegativeSKUInMessage {
		txt += fmt.Sprintf(" 等 %d 个商品", len(neg))
	}
	return txt
}

// checkGrossMarginWithAllowance 出毛利信号（整体额度口径 + 逐 SKU 负毛利）。
//
// # 两条口径的关系（为什么不是两条信号各报各的）
//
//	口径 A（既有）：**整体**毛利缺口 vs 补差价额度
//	                缺口 ≤ 额度 ⇒ info；缺口 > 额度 ⇒ warn。
//	口径 B（2026-10-29 新增）：**逐 SKU** 毛利为负 ⇒ warn。
//
// 合并规则：
//
//   - 整体毛利为负 **且** 有补差价额度 ⇒ 出**口径 A** 的那一条信号，
//     并把负毛利 SKU 明细**挂进它的消息**（不再单独报口径 B）。
//     理由：整体缺口已被额度解释过了，再单独报一遍等于把同一件事说两次，
//     而且会把"已按口径允许毛利降低"这件事冲淡。
//   - 其余情况（无额度 / 整体不为负）⇒ 出**口径 B** 的逐 SKU warn。
//     这正是原先**完全没有信号**的缺口：没有补差价时，毛利为负一个字都不报；
//     整体为正时，亏钱的 SKU 被利润 SKU 盖住。
//   - 兜底：整体为负但拿不到逐 SKU 明细 ⇒ 报一条**不带明细**的 warn。
//     宁可粗一点，也不能把"整体毛利为负"咽下去。
//
// ⚠️ 逐 SKU 信号**永不 block**（与既有毛利信号一致）—— 毛利是估算口径，
//    让它拦付款会把"估算不准"变成"付不了款"。
//
// names 是货号 → 名称的字典，用于把消息里的货号渲染成「<名称>-<货号>」。
func checkGrossMarginWithAllowance(gm GrossMargin, al Allowance, per map[string]ProductMargin, names map[string]string) []signalDraft {
	neg := negativeProductMargins(per)
	// 口径 A 生效（整体为负 + 有额度）⇒ 出整体信号，附带 SKU 明细。
	if gm.Calculated && al.PriceDiffYuan.GreaterThan(decimal.Zero) && gm.AmountYuan.IsNegative() {
		shortfall := gm.AmountYuan.Abs()
		msg := fmt.Sprintf("本单涉及商品窗口毛利估算为 %s 元(缺口 %s 元)",
			yuanText(gm.AmountYuan), yuanText(shortfall))
		level := model.SignalWarn
		if shortfall.LessThanOrEqual(al.PriceDiffYuan) {
			level = model.SignalInfo
			msg += fmt.Sprintf(";本次含补差价收款 %s 元,已覆盖该缺口,按口径允许毛利降低",
				yuanText(al.PriceDiffYuan))
		} else {
			msg += fmt.Sprintf(";超过补差价收款 %s 元,请人工终审关注",
				yuanText(al.PriceDiffYuan))
		}
		evidence := map[string]any{
			"gross_margin_yuan": gm.AmountYuan.String(),
			"shortfall_yuan":    shortfall.String(),
			"price_diff_yuan":   al.PriceDiffYuan.String(),
		}
		if skuTxt := negativeSKUText(neg, names); skuTxt != "" {
			msg += fmt.Sprintf(";其中毛利为负的商品:%s", skuTxt)
			evidence["negative_skus"] = negativeSKUEvidence(neg)
		}
		return []signalDraft{newSignal(level, SigGrossMargin, "", "", msg, evidence)}
	}
	// 口径 B：逐 SKU 负毛利 ⇒ warn。
	if len(neg) > 0 {
		msg := fmt.Sprintf("本单有 %d 个商品窗口毛利估算为负:%s,请人工终审关注",
			len(neg), negativeSKUText(neg, names))
		return []signalDraft{newSignal(model.SignalWarn, SigGrossMargin, "", "", msg, map[string]any{
			"gross_margin_yuan": gm.AmountYuan.String(),
			"negative_skus":     negativeSKUEvidence(neg),
		})}
	}
	// 兜底：整体毛利为负、但拿不到逐 SKU 明细（per 没传或两个口径集合不一致）。
	// 宁可报一条没有明细的 warn，也不能把"整体毛利为负"这件事咽下去。
	if gm.Calculated && gm.AmountYuan.IsNegative() {
		msg := fmt.Sprintf("本单涉及商品窗口毛利估算为 %s 元(缺口 %s 元),请人工终审关注",
			yuanText(gm.AmountYuan), yuanText(gm.AmountYuan.Abs()))
		return []signalDraft{newSignal(model.SignalWarn, SigGrossMargin, "", "", msg, map[string]any{
			"gross_margin_yuan": gm.AmountYuan.String(),
			"shortfall_yuan":    gm.AmountYuan.Abs().String(),
		})}
	}
	return nil
}

// negativeSKUEvidence 把负毛利 SKU 列表落成可核对的证据（进 signal 的 evidence JSON）。
func negativeSKUEvidence(neg []ProductMargin) []map[string]string {
	out := make([]map[string]string, 0, len(neg))
	for _, pm := range neg {
		out = append(out, map[string]string{
			"product_id":        pm.ProductID,
			"margin_yuan":       pm.AmountYuan.String(),
			"sales_amount_yuan": pm.SalesAmountYuan.String(),
			"sales_quantity":    pm.SalesQuantity.String(),
			"unit_cost_yuan":    pm.UnitCostYuan.String(),
			"unit_cost_source":  pm.CostSource,
		})
	}
	return out
}

// ---- 实现毛利（2026-10-29 用户口径）----

// SigRealizedMargin 是「实现毛利」信号类型（warn，**永不 block**）。
//
// ⚠️ 与 [SigGrossMargin]（窗口毛利）是**两个不同的信号** —— 两个口径会同时
// 出现、且结论可能相反，那正是店长要知道的：老库存成本低（avg_cost 低）
// 会**掩盖**"这次进货进贵了"这件事。
const SigRealizedMargin = "realized_margin"

// BatchPurchase 是某货号在**本批次**（本单采购单）里的进货汇总。
type BatchPurchase struct {
	// AmountYuan 是该货号在本批次里的进货金额合计（元）。
	AmountYuan decimal.Decimal
	// Qty 是该货号在本批次里的收货量合计。
	Qty decimal.Decimal
}

// batchPurchaseByProduct 按货号汇总本批次的进货金额与收货量。
//
// ⚠️ 与 unitCostByProduct 的区别：那个只留"均价"，这个把**量**也留下来 ——
//    实现毛利要用"本批次进货总量"当上限。
func batchPurchaseByProduct(lines []SheetLineRow) map[string]BatchPurchase {
	out := make(map[string]BatchPurchase)
	for _, ln := range lines {
		if ln.ProductID == "" {
			continue
		}
		b := out[ln.ProductID]
		b.AmountYuan = b.AmountYuan.Add(ln.TotalAmountYuan)
		b.Qty = b.Qty.Add(ln.ReceivedQuantity)
		out[ln.ProductID] = b
	}
	return out
}

// RealizedMargin 是**单个 SKU** 的实现毛利估算。
//
// # 口径（2026-10-29 用户口径原文）
//
//	「用本批次进货均价、结算单的采购订单里该sku最早采购日期起至
//	  发起'付款审批'止日期段的销售均价、本批次进货总量
//	  （>销售总量则取销售总量）来计算实现毛利」
//
//	实现毛利 = (销售均价 − 本批次进货均价) × min(本批次进货总量, 窗口销售总量)
//
// # 与「窗口毛利」的区别（两个都要有，缺一个店长就看不到真相）
//
//	窗口毛利（[GrossMargin]）：成本用**库存加权平均成本价**（avg_cost）
//	⇒ 回答"这批货整体上赚不赚钱"（含老库存的成本）；
//	实现毛利（本类型）：成本用**本批次进货均价**
//	⇒ 回答"这批新进的货，按它自己的进价卖出去赚不赚钱"。
//
//	蜜梨-01860 就是典型：avg_cost 2.82 ⇒ 窗口毛利 **+181 元**（正）；
//	而本批次进货均价 4.6349 > 销售均价 3.532 ⇒ 实现毛利 **为负**（亏）。
//	两个结论都对，回答的是两个问题 —— 后者才是"要不要付这笔货款"的依据。
type RealizedMargin struct {
	// ProductID 是货号。
	ProductID string
	// AmountYuan 是实现毛利估算（可正可负）。
	AmountYuan decimal.Decimal
	// SalesAvgPriceYuan 是窗口销售均价（销额 ÷ 销量）。
	SalesAvgPriceYuan decimal.Decimal
	// BatchUnitCostYuan 是本批次进货均价（进货金额 ÷ 进货量）。
	BatchUnitCostYuan decimal.Decimal
	// BatchQty 是本批次进货总量。
	BatchQty decimal.Decimal
	// SalesQty 是窗口销售总量。
	SalesQty decimal.Decimal
	// CountedQty 是 min(进货总量, 销售总量) —— 实际能按本批次价格卖掉的量。
	CountedQty decimal.Decimal
	// WindowFrom 是窗口起点（该货号在本单采购单里的最早采购日期）。
	WindowFrom string
}

// estimateRealizedMargins 逐 SKU 估算实现毛利。
//
// batch:货号 → 本批次进货汇总；salesByProduct:货号 → 窗口销售汇总；
// windowFromByProduct:货号 → 窗口起点（最早采购日）。
//
// ⚠️ 只统计**三者齐全**的商品：没进本批次的、窗口里没卖过的、
//    量算不出来的，都不进这个口径 —— 否则"没卖"会被当成"不亏"。
func estimateRealizedMargins(
	batch map[string]BatchPurchase,
	salesByProduct map[string]SaleRow,
	windowFromByProduct map[string]string,
) map[string]RealizedMargin {
	out := make(map[string]RealizedMargin)
	for pid, b := range batch {
		if !b.Qty.GreaterThan(decimal.Zero) {
			continue
		}
		sale, ok := salesByProduct[pid]
		if !ok || !sale.NetSaleQuantity.GreaterThan(decimal.Zero) {
			continue
		}
		salesAvg := sale.NetSaleAmountYuan.Div(sale.NetSaleQuantity)
		unitCost := b.AmountYuan.Div(b.Qty)
		// 本批次进货总量 > 窗口销售总量 ⇒ 只能卖掉销售量那么多（多的还压在库里）。
		counted := b.Qty
		if sale.NetSaleQuantity.LessThan(counted) {
			counted = sale.NetSaleQuantity
		}
		out[pid] = RealizedMargin{
			ProductID:         pid,
			AmountYuan:        salesAvg.Sub(unitCost).Mul(counted),
			SalesAvgPriceYuan: salesAvg,
			BatchUnitCostYuan: unitCost,
			BatchQty:          b.Qty,
			SalesQty:          sale.NetSaleQuantity,
			CountedQty:        counted,
			WindowFrom:        windowFromByProduct[pid],
		}
	}
	return out
}

// negativeRealizedMargins 取实现毛利为负的 SKU，按缺口绝对值**从大到小**排序。
//
// 排序理由同 [negativeProductMargins]：消息里只列最亏的几个。
func negativeRealizedMargins(per map[string]RealizedMargin) []RealizedMargin {
	out := make([]RealizedMargin, 0, len(per))
	for _, rm := range per {
		if rm.AmountYuan.IsNegative() {
			out = append(out, rm)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].AmountYuan.Abs().GreaterThan(out[j].AmountYuan.Abs())
	})
	return out
}

// maxRealizedSKUInMessage 实现毛利信号里最多列几个 SKU。
const maxRealizedSKUInMessage = 5

// checkRealizedMargin 出「实现毛利」warn 信号。
//
// # 判据：**实现毛利为负** ⇒ warn（永不 block）
//
// 为什么只看负数：实现毛利为正 = 这批货按它自己的进价卖是赚的，没有异常。
// 为负 = "这批进货按今天的价格卖出去要亏钱" ⇒ 店长该知道，交给人工终审判断
// （可能是时令性定价、可能是清库存、也可能是进价报错了 —— 系统不该替人决定）。
//
// ⚠️ 与窗口毛利信号**分开报**：两者口径不同、结论可能相反，
//    合成一条会把"整体赚、这批亏"这件事说糊。
func checkRealizedMargin(per map[string]RealizedMargin, names map[string]string) []signalDraft {
	neg := negativeRealizedMargins(per)
	if len(neg) == 0 {
		return nil
	}
	parts := make([]string, 0, len(neg))
	for i, rm := range neg {
		if i >= maxRealizedSKUInMessage {
			break
		}
		parts = append(parts, fmt.Sprintf("%s(%s 元,进货均价 %s vs 销售均价 %s)",
			ProductLabel(names, rm.ProductID),
			yuanText(rm.AmountYuan),
			yuanText(rm.BatchUnitCostYuan),
			yuanText(rm.SalesAvgPriceYuan)))
	}
	txt := strings.Join(parts, "、")
	if len(neg) > maxRealizedSKUInMessage {
		txt += fmt.Sprintf(" 等 %d 个商品", len(neg))
	}
	msg := fmt.Sprintf("本单有 %d 个商品按本批次进货价算实现毛利为负:%s,请人工终审关注",
		len(neg), txt)
	return []signalDraft{newSignal(model.SignalWarn, SigRealizedMargin, "", "", msg, map[string]any{
		"negative_skus": realizedMarginEvidence(neg),
	})}
}

// realizedMarginEvidence 把实现毛利明细落成可核对的证据。
func realizedMarginEvidence(neg []RealizedMargin) []map[string]string {
	out := make([]map[string]string, 0, len(neg))
	for _, rm := range neg {
		out = append(out, map[string]string{
			"product_id":          rm.ProductID,
			"realized_margin_yuan": rm.AmountYuan.String(),
			"batch_unit_cost_yuan": rm.BatchUnitCostYuan.String(),
			"sales_avg_price_yuan": rm.SalesAvgPriceYuan.String(),
			"batch_qty":           rm.BatchQty.String(),
			"sales_qty":           rm.SalesQty.String(),
			"counted_qty":         rm.CountedQty.String(),
			"window_from":         rm.WindowFrom,
		})
	}
	return out
}
