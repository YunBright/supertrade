// Package service / dx.go —— 代销帐款单(毛利)核对(2026-11-01 用户口径)。
//
// # 用户口径
//
//	「wx-h5 和增加联营单勾选一样增加代销单勾选，配套修改后端，
//	  默认整体实现毛利同样大于 10%」
//
// # 金额口径(必须先读懂这一节)
//
// 代销是"门店不买货，供应商把货放店里卖，按实际销售结算":
//
//	零售额  = Σ明细 num1        (门店卖给顾客的钱)
//	结算额  = Σ明细 ac_amount   (门店应付供应商的钱)
//	毛利额  = 零售额 − 结算额
//	毛利率  = 毛利额 / 零售额   （**汇总后相除**，不是 AVG 逐行毛利率）
//
// 头表(sum_amount)只是结算额，零售额只在明细上 ⇒ 毛利只能从明细算
// (见 cube.go 的 ListDxSettlementFacts)。
//
// ⚠️ 与联营的区别:联营的扣点是**门店收**的款项(要随单申报收款项);
//   代销的毛利是**隐含在结算价差里**的,不涉及额外收付款项 ⇒ 本文件
//   只做"毛利率是否达标"的提示,不产生 block 信号。
package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// 代销毛利相关信号类型。
const (
	// SigDxMarginLow 代销整体实现毛利率低于最低要求。
	SigDxMarginLow = "dx_margin_low"
	// SigDxMarginUnknown 代销结算单取不到明细,毛利无法核算。
	SigDxMarginUnknown = "dx_margin_unknown"
)

// DefaultMinDxMarginRate 是代销最低毛利率的默认值（%）。
//
// 用户口径「默认整体实现毛利同样大于 10%」:未另行配置的按 10% 判。
const DefaultMinDxMarginRate = 10.0

// DxSupplierFact 是按**供应商**聚合的代销毛利事实(给 LLM 的明细)。
type DxSupplierFact struct {
	SupplierID string
	// SettlementIDs 是该供应商在本单里的代销帐款单号。
	SettlementIDs   []string
	RetailYuan      decimal.Decimal
	SettleYuan      decimal.Decimal
	MarginYuan      decimal.Decimal
	// MarginRatePercent 是毛利率(%) = MarginYuan / RetailYuan × 100。
	MarginRatePercent decimal.Decimal
	// MinRatePercent 是要求的最低毛利率(%)。
	MinRatePercent float64
}

// DxSummary 是代销毛利核对的**引擎口径汇总**(直接喂给 LLM)。
type DxSummary struct {
	// HasDx 表示本次审批是否包含代销( DX )结算单。
	HasDx bool
	// SettlementCount 是代销结算单张数。
	SettlementCount int
	// RetailYuan / SettleYuan / MarginYuan 是全部代销单的零售额/结算额/毛利合计。
	RetailYuan decimal.Decimal
	SettleYuan decimal.Decimal
	MarginYuan decimal.Decimal
	// MarginRatePercent 是合计毛利率(%)。
	MarginRatePercent decimal.Decimal
	// MinRatePercent 是涉及的**最高**最低毛利率(严格口径)。
	MinRatePercent float64
	// UnknownCount 是取不到明细(毛利无法核算)的代销单张数。
	UnknownCount int
	// Suppliers 是逐供应商明细(结论里要能说清是哪家不达标)。
	Suppliers []DxSupplierFact
}

// checkDx 代销毛利核对(纯函数,便于单测)。
//
// 只有 warn 级信号:毛利率低是**政策/议价问题**,未必等于这一单要拒,
// 由 LLM 结合申请人备注研判(与联营扣点率同一定位)。
func checkDx(settlements []SettlementRow) ([]signalDraft, DxSummary) {
	var summary DxSummary
	type agg struct {
		retail, settle decimal.Decimal
		ids            []string
	}
	bySupplier := make(map[string]*agg)
	var allIDs []string
	var out []signalDraft
	for _, st := range settlements {
		if st.DocumentType != DocumentTypeDelivery {
			continue
		}
		summary.HasDx = true
		allIDs = append(allIDs, st.ID)
		if st.Dx == nil {
			summary.UnknownCount++
			out = append(out, newSignal(model.SignalWarn, SigDxMarginUnknown, st.ID, "",
				fmt.Sprintf("代销账款单 %s 取不到销售明细，无法核算毛利率", st.ID),
				map[string]any{"settlement_id": st.ID}))
			continue
		}
		retail := st.Dx.RetailYuan
		settle := st.Dx.SettleYuan
		summary.RetailYuan = summary.RetailYuan.Add(retail)
		summary.SettleYuan = summary.SettleYuan.Add(settle)
		key := strings.TrimSpace(st.SupplierID)
		a := bySupplier[key]
		if a == nil {
			a = &agg{}
			bySupplier[key] = a
		}
		a.retail = a.retail.Add(retail)
		a.settle = a.settle.Add(settle)
		a.ids = append(a.ids, st.ID)
	}
	if !summary.HasDx {
		return nil, summary
	}
	summary.SettlementCount = len(allIDs)
	summary.MarginYuan = summary.RetailYuan.Sub(summary.SettleYuan)
	if summary.RetailYuan.GreaterThan(decimal.Zero) {
		summary.MarginRatePercent = percentOf(summary.MarginYuan, summary.RetailYuan)
	}
	summary.MinRatePercent = DefaultMinDxMarginRate

	// 逐供应商:毛利率 = (零售额 − 结算额) / 零售额。
	supplierIDs := make([]string, 0, len(bySupplier))
	for id := range bySupplier {
		supplierIDs = append(supplierIDs, id)
	}
	// 供应商 id 排序保证输出稳定(单测可断言)。
	sort.Strings(supplierIDs)
	for _, sid := range supplierIDs {
		a := bySupplier[sid]
		margin := a.retail.Sub(a.settle)
		rate := decimal.Zero
		if a.retail.GreaterThan(decimal.Zero) {
			rate = percentOf(margin, a.retail)
		}
		summary.Suppliers = append(summary.Suppliers, DxSupplierFact{
			SupplierID:        sid,
			SettlementIDs:     a.ids,
			RetailYuan:        a.retail,
			SettleYuan:        a.settle,
			MarginYuan:        margin,
			MarginRatePercent: rate,
			MinRatePercent:    DefaultMinDxMarginRate,
		})
		if rate.LessThan(decimal.NewFromFloat(DefaultMinDxMarginRate)) {
			out = append(out, newSignal(model.SignalWarn, SigDxMarginLow, "", "",
				fmt.Sprintf("代销账款单 %s(供应商 %s)整体实现毛利率 %s%%,低于最低要求 %s%%(零售额 %s 元,结算额 %s 元,毛利 %s 元)",
					strings.Join(a.ids, ","), sid, rate.StringFixed(2),
					decimal.NewFromFloat(DefaultMinDxMarginRate).StringFixed(2),
					yuanText(a.retail), yuanText(a.settle), yuanText(margin)),
				map[string]any{
					"settlement_ids":  a.ids,
					"supplier_id":     sid,
					"margin_rate":     rate.StringFixed(4),
					"min_rate":        DefaultMinDxMarginRate,
					"retail_yuan":     a.retail.String(),
					"settle_yuan":     a.settle.String(),
					"margin_yuan":     margin.String(),
				}))
		}
	}
	return out, summary
}

// writeDxFacts 把代销毛利事实写进 LLM user prompt（2026-11-01 用户口径）。
//
// ⚠️ 只写**汇总 + 逐供应商**，不逐明细行：明细行会把真正需要关注的信号冲淡。
func writeDxFacts(sb *strings.Builder, d DxSummary) {
	if !d.HasDx {
		return
	}
	fmt.Fprintf(sb, "代销毛利(引擎口径):代销帐款单 %d 张,零售额合计 %s 元,结算额合计 %s 元,毛利 %s 元,整体毛利率 %s%%(最低要求 %s%%)",
		d.SettlementCount, yuanText(d.RetailYuan), yuanText(d.SettleYuan),
		yuanText(d.MarginYuan), d.MarginRatePercent.StringFixed(2),
		decimal.NewFromFloat(DefaultMinDxMarginRate).StringFixed(0))
	if d.UnknownCount > 0 {
		fmt.Fprintf(sb, ";其中 %d 张取不到明细、无法核算毛利", d.UnknownCount)
	}
	sb.WriteString("。\n")
	for i, s := range d.Suppliers {
		if i >= 10 {
			fmt.Fprintf(sb, "  ... 其余 %d 个供应商略\n", len(d.Suppliers)-10)
			break
		}
		flag := ""
		if s.MarginRatePercent.LessThan(decimal.NewFromFloat(s.MinRatePercent)) {
			flag = ",低于最低率"
		}
		fmt.Fprintf(sb, "  - 供应商 %s 代销单 %s:零售额 %s 元,结算额 %s 元,毛利 %s 元,毛利率 %s%%,最低要求 %s%%%s\n",
			s.SupplierID, strings.Join(s.SettlementIDs, ","), yuanText(s.RetailYuan),
			yuanText(s.SettleYuan), yuanText(s.MarginYuan), s.MarginRatePercent.StringFixed(2),
			decimal.NewFromFloat(s.MinRatePercent).StringFixed(2), flag)
	}
}

// stepCheckDx 代销毛利核对 + 汇总(写 audit_logs)。
//
// ⚠️ 没有代销单时**不产生任何信号**,只记一条"跳过"日志 —— 让推理时间线
//    能看出流水线确实走过这一步,而不是"疑似漏掉了"。
func (s *Service) stepCheckDx(ctx context.Context, ap *model.ApprovalInstance, settlements []SettlementRow, signals *[]signalDraft) DxSummary {
	start := time.Now()
	before := len(*signals)
	sigs, summary := checkDx(settlements)
	*signals = append(*signals, sigs...)
	if !summary.HasDx {
		s.logStep(ctx, ap, StepCheckDx, "无代销结算单", "跳过代销毛利核对", "", time.Since(start).Milliseconds())
		return summary
	}
	s.logStep(ctx, ap, StepCheckDx,
		fmt.Sprintf("代销结算单 %d 张,零售额合计 %s 元", summary.SettlementCount, summary.RetailYuan.StringFixed(2)),
		fmt.Sprintf("新增 %d 条代销毛利信号;整体毛利率 %s%%(毛利 %s 元)",
			len(*signals)-before, summary.MarginRatePercent.StringFixed(2), summary.MarginYuan.StringFixed(2)),
		"", time.Since(start).Milliseconds())
	return summary
}
