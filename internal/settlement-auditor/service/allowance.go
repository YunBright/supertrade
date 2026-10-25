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

// GrossMargin 是本次审批涉及商品的**窗口毛利估算**。
//
// # 口径（为什么是"估算"）
//
// 毛利 = 销额 − 成本，成本用**本次进货均价**估算（进货金额 ÷ 收货量）。
// 引擎手上没有"该商品的历史成本"，用本次进货均价是最接近的口径；
// 税额口径（销额含税、进货金额含税）一致，故此处只做**相对判断**
// （毛利是否为负、缺口多大），不做绝对毛利考核。
type GrossMargin struct {
	// AmountYuan 是窗口毛利估算（可正可负）。
	AmountYuan decimal.Decimal
	// Calculated 为 true 表示算得出来（至少一个商品既有进货又有销量）。
	Calculated bool
}

// estimateGrossMargin 用本次进货均价估算窗口毛利。
//
// unitCostByProduct:货号 → 本次进货均价（元/件）。
// salesByProduct:货号 → 窗口销售汇总。
//
// 只统计**同时**有进货均价与销量的商品：只有销量的算不出成本，
// 只有进货的没有收入 ⇒ 两者都不该进毛利口径。
func estimateGrossMargin(unitCostByProduct map[string]decimal.Decimal, salesByProduct map[string]SaleRow) GrossMargin {
	var gm GrossMargin
	for pid, sale := range salesByProduct {
		cost, ok := unitCostByProduct[pid]
		if !ok || !cost.GreaterThan(decimal.Zero) {
			continue
		}
		if !sale.NetSaleQuantity.GreaterThan(decimal.Zero) {
			continue
		}
		margin := sale.NetSaleAmountYuan.Sub(sale.NetSaleQuantity.Mul(cost))
		gm.AmountYuan = gm.AmountYuan.Add(margin)
		gm.Calculated = true
	}
	return gm
}

// checkGrossMarginWithAllowance 按「补差价」额度给毛利信号。
//
// # 触发条件：本次**存在补差价收款**
//
// 没有补差价 ⇒ 这条规则无从谈起（没有额度）⇒ 不产生任何信号，
// 审批行为与改造前完全一致。
//
// # 分档
//
//	缺口 ≤ 补差价总额 ⇒ info（按口径允许毛利降低，写明依据）
//	缺口 > 补差价总额 ⇒ warn（提示人工终审关注，仍不阻断）
func checkGrossMarginWithAllowance(gm GrossMargin, al Allowance) []signalDraft {
	if !al.PriceDiffYuan.GreaterThan(decimal.Zero) || !gm.Calculated {
		return nil
	}
	if gm.AmountYuan.GreaterThanOrEqual(decimal.Zero) {
		return nil
	}
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
	return []signalDraft{newSignal(level, SigGrossMargin, "", "", msg, map[string]any{
		"gross_margin_yuan": gm.AmountYuan.String(),
		"shortfall_yuan":    shortfall.String(),
		"price_diff_yuan":   al.PriceDiffYuan.String(),
	})}
}
