// Package service / joint.go —— 联营帐款单(扣点)核对(2026-10-31 用户口径)。
//
// # 用户口径
//
//	「LLM机器审核需要审扣点是否随联营帐款单一起发起『付款审批』申请,
//	  扣点率是否满足（默认大于10%，可在flutter管理后台结算-供应商流程管理页里
//	  配置供应商联营最低扣点率）」
//
// # 金额口径(必须先读懂这一节)
//
// 联营(联营联销)是"门店不买货,供应商在店里卖,门店按销售额抽成":
//
//	毛销售额  = Σ明细 sale_amount      (结算单上的"货款"就是这个数)
//	联营扣点  = Σ明细 sup_ly_amount    (门店应得的抽成)
//	实际应付  = 毛销售额 − 联营扣点
//
// 所以联营单在**现有**金额勾稽公式里天然成立:
//
//	应付 = 货款(毛销售额) + 付款项 − 收款项(「扣点」收款项)
//
// ⇒ 申请人在提交联营结算付款时,**必须同时填报一笔「扣点」收款项**;
//   没填报 ⇒ 应付被算成毛销售额 ⇒ 多付了门店的抽成。
//
// # 两个校验
//
//  1. **扣点是否随单申报**:应扣点 > 0 而「扣点」收款项为 0 ⇒ block;
//     申报额明显少于应扣点(超过付款取整容差)⇒ block(少付金额会失真)。
//  2. **扣点率是否达标**:扣点率 = Σ扣点 / Σ销售额,按**供应商**分别算,
//     与该供应商配置的最低扣点率比较(默认 10%)⇒ 低于阈值给 warn,
//     由 LLM 结合申请人备注研判(政策问题不一定等于这一单要拒)。
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

// 联营扣点相关信号类型。
const (
	// SigJointDeductMissing 联营单应扣点 > 0,但本次审批没有「扣点」收款项。
	SigJointDeductMissing = "joint_deduct_missing"
	// SigJointDeductShort 联营单申报的「扣点」收款项明显少于应扣点。
	SigJointDeductShort = "joint_deduct_short"
	// SigJointRateLow 联营扣点率低于该供应商配置的最低扣点率。
	SigJointRateLow = "joint_rate_low"
)

// DefaultMinJointDeductRate 是联营最低扣点率的默认值（%）。
//
// 用户口径「默认大于 10%」:未配置的供应商按 10% 判。
const DefaultMinJointDeductRate = 10.0

// JointSupplierFact 是按**供应商**聚合的联营扣点事实(给 LLM 的明细)。
type JointSupplierFact struct {
	SupplierID string
	// SettlementIDs 是该供应商在本单里的联营帐款单号。
	SettlementIDs []string
	SalesYuan     decimal.Decimal
	DeductYuan    decimal.Decimal
	// DeductRatePercent 是扣点率(%) = DeductYuan / SalesYuan × 100。
	DeductRatePercent decimal.Decimal
	// MinRatePercent 是该供应商配置的最低扣点率(%)。
	MinRatePercent float64
}

// JointSummary 是联营扣点核对的**引擎口径汇总**(直接喂给 LLM)。
//
type JointSummary struct {
	// HasJoint 表示本次审批是否包含联营( LY )结算单。
	HasJoint bool
	// SettlementCount 是联营结算单张数。
	SettlementCount int
	// SalesYuan / DeductYuan 是全部联营单的销售额 / 扣点合计。
	SalesYuan  decimal.Decimal
	DeductYuan decimal.Decimal
	// DeductRatePercent 是合计扣点率(%)。
	DeductRatePercent decimal.Decimal
	// MinRatePercent 是涉及供应商里**最高**的最低扣点率(严格口径)。
	MinRatePercent float64
	// DeductReceiptYuan 是本次审批里「扣点」收款项的申报合计。
	DeductReceiptYuan decimal.Decimal
	// Suppliers 是逐供应商明细(结论里要能说清是哪家不达标)。
	Suppliers []JointSupplierFact
}

// checkJoint 联营扣点核对(纯函数,便于单测)。
//
// minRateOf 返回某供应商的最低扣点率(%) —— 由 caller 注入(读配置表)。
// tolerance 用**付款取整容差**:扣点金额按元申报,分/角差异属正常取整。
func checkJoint(settlements []SettlementRow, items []model.ApprovalItem, minRateOf func(supplierID string) float64, tolerance decimal.Decimal) ([]signalDraft, JointSummary) {
	var summary JointSummary
	type agg struct {
		sales, deduct decimal.Decimal
		ids           []string
	}
	bySupplier := make(map[string]*agg)
	var allIDs []string
	for _, st := range settlements {
		if st.DocumentType != DocumentTypeJoint {
			continue
		}
		summary.HasJoint = true
		allIDs = append(allIDs, st.ID)
		sales := st.TotalGoodsYuan
		deduct := decimal.Zero
		if st.Joint != nil {
			if st.Joint.SalesYuan.GreaterThan(decimal.Zero) {
				sales = st.Joint.SalesYuan
			}
			deduct = st.Joint.DeductYuan
		}
		summary.SalesYuan = summary.SalesYuan.Add(sales)
		summary.DeductYuan = summary.DeductYuan.Add(deduct)
		key := strings.TrimSpace(st.SupplierID)
		a := bySupplier[key]
		if a == nil {
			a = &agg{}
			bySupplier[key] = a
		}
		a.sales = a.sales.Add(sales)
		a.deduct = a.deduct.Add(deduct)
		a.ids = append(a.ids, st.ID)
	}
	if !summary.HasJoint {
		return nil, summary
	}
	summary.SettlementCount = len(allIDs)

	// 本次审批申报的「扣点」收款项(方向必须是 receipt —— 付款项里写"扣点"不算)。
	var declared decimal.Decimal
	for _, it := range items {
		if strings.TrimSpace(it.Direction) != ItemDirectionReceipt {
			continue
		}
		if strings.TrimSpace(it.ItemType) != ReceiptTypeDeduction {
			continue
		}
		declared = declared.Add(it.AmountYuan)
	}
	summary.DeductReceiptYuan = declared

	var out []signalDraft
	idsText := strings.Join(allIDs, ",")

	// ① 扣点是否随单申报。
	switch {
	case summary.DeductYuan.GreaterThan(tolerance) && declared.IsZero():
		out = append(out, newSignal(model.SignalBlock, SigJointDeductMissing, "", "",
			fmt.Sprintf("联营账款单 %s 应扣点 %s 元,但本次付款审批没有填报「扣点」收款项(联营扣点须随账款单一起申请)",
				idsText, yuanText(summary.DeductYuan)),
			map[string]any{
				"settlement_ids":  allIDs,
				"expected_deduct": summary.DeductYuan.String(),
				"declared_deduct": declared.String(),
			}))
	case declared.LessThan(summary.DeductYuan.Sub(tolerance)):
		out = append(out, newSignal(model.SignalBlock, SigJointDeductShort, "", "",
			fmt.Sprintf("联营账款单 %s 应扣点 %s 元,本次「扣点」收款项只申报 %s 元(少 %s 元)",
				idsText, yuanText(summary.DeductYuan), yuanText(declared),
				yuanText(summary.DeductYuan.Sub(declared))),
			map[string]any{
				"settlement_ids":  allIDs,
				"expected_deduct": summary.DeductYuan.String(),
				"declared_deduct": declared.String(),
				"short":           summary.DeductYuan.Sub(declared).String(),
			}))
	}

	// ② 扣点率(逐供应商;分母是销售额,合计口径一并给出)。
	if summary.SalesYuan.GreaterThan(decimal.Zero) {
		summary.DeductRatePercent = percentOf(summary.DeductYuan, summary.SalesYuan)
	}
	supplierIDs := make([]string, 0, len(bySupplier))
	for id := range bySupplier {
		supplierIDs = append(supplierIDs, id)
	}
	// 供应商 id 排序保证输出稳定(单测可断言)。
	sort.Strings(supplierIDs)
	for _, sid := range supplierIDs {
		a := bySupplier[sid]
		rate := decimal.Zero
		if a.sales.GreaterThan(decimal.Zero) {
			rate = percentOf(a.deduct, a.sales)
		}
		minRate := minRateOf(sid)
		if minRate <= 0 {
			minRate = DefaultMinJointDeductRate
		}
		summary.Suppliers = append(summary.Suppliers, JointSupplierFact{
			SupplierID:        sid,
			SettlementIDs:     a.ids,
			SalesYuan:         a.sales,
			DeductYuan:        a.deduct,
			DeductRatePercent: rate,
			MinRatePercent:    minRate,
		})
		if minRate > summary.MinRatePercent {
			summary.MinRatePercent = minRate
		}
		if a.sales.GreaterThan(decimal.Zero) && rate.LessThan(decimal.NewFromFloat(minRate)) {
			out = append(out, newSignal(model.SignalWarn, SigJointRateLow, "", "",
				fmt.Sprintf("联营账款单 %s(供应商 %s)扣点率 %s%%,低于该供应商最低扣点率 %s%%",
					strings.Join(a.ids, ","), sid, rate.StringFixed(2), decimal.NewFromFloat(minRate).StringFixed(2)),
				map[string]any{
					"settlement_ids": a.ids,
					"supplier_id":    sid,
					"deduct_rate":    rate.StringFixed(4),
					"min_rate":       minRate,
					"sales":          a.sales.String(),
					"deduct":         a.deduct.String(),
				}))
		}
	}
	return out, summary
}

// writeJointFacts 把联营扣点事实写进 LLM user prompt（2026-10-31 用户口径）。
//
// ⚠️ 只写**汇总 + 逐供应商**，不逐明细行：明细行会把真正需要关注的信号冲淡。
func writeJointFacts(sb *strings.Builder, j JointSummary) {
	if !j.HasJoint {
		return
	}
	fmt.Fprintf(sb, "联营扣点(引擎口径):联营帐款单 %d 张,销售额合计 %s 元,应扣点合计 %s 元,扣点率 %s%%;本次申报「扣点」收款项 %s 元(最低扣点率按供应商配置,默认 %s%%)。\n",
		j.SettlementCount, yuanText(j.SalesYuan), yuanText(j.DeductYuan),
		j.DeductRatePercent.StringFixed(2), yuanText(j.DeductReceiptYuan),
		decimal.NewFromFloat(DefaultMinJointDeductRate).StringFixed(0))
	for i, s := range j.Suppliers {
		if i >= 10 {
			fmt.Fprintf(sb, "  ... 其余 %d 个供应商略\n", len(j.Suppliers)-10)
			break
		}
		flag := ""
		if s.SalesYuan.GreaterThan(decimal.Zero) &&
			s.DeductRatePercent.LessThan(decimal.NewFromFloat(s.MinRatePercent)) {
			flag = ",低于最低率"
		}
		fmt.Fprintf(sb, "  - 供应商 %s 联营单 %s:销售额 %s 元,扣点 %s 元,扣点率 %s%%,最低要求 %s%%%s\n",
			s.SupplierID, strings.Join(s.SettlementIDs, ","), yuanText(s.SalesYuan),
			yuanText(s.DeductYuan), s.DeductRatePercent.StringFixed(2),
			decimal.NewFromFloat(s.MinRatePercent).StringFixed(2), flag)
	}
}

// percentOf 返回 part / total × 100(总为 0 时返回 0)。
func percentOf(part, total decimal.Decimal) decimal.Decimal {
	if !total.GreaterThan(decimal.Zero) {
		return decimal.Zero
	}
	return part.Div(total).Mul(decimal.NewFromInt(100)).Round(4)
}

// minJointDeductRate 读某供应商配置的最低联营扣点率(%)。
//
// 配置来自 audit_supplier_flows(branch_id + supplier_id) —— 同一张表上的
// 「流程定制」与「最低扣点率」是两个独立设置(没配流程也可以单独配扣点率)。
//
// ⚠️ 没配置 / 读到 0 ⇒ 用 [DefaultMinJointDeductRate];读库失败也只回退默认,
//   不能因为配置读不到就让联营审核挂掉(与供应商字典同一定位)。
func (s *Service) minJointDeductRate(ctx context.Context, branchID, supplierID string) float64 {
	supplierID = strings.TrimSpace(supplierID)
	if supplierID == "" {
		return DefaultMinJointDeductRate
	}
	var row model.AuditSupplierFlow
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND supplier_id = ?", branchID, supplierID).
		First(&row).Error; err != nil {
		return DefaultMinJointDeductRate
	}
	if row.MinJointDeductRate <= 0 {
		return DefaultMinJointDeductRate
	}
	return row.MinJointDeductRate
}

// stepCheckJoint 联营扣点核对 + 汇总(写 audit_logs)。
//
// ⚠️ 没有联营单时**不产生任何信号**,只记一条"跳过"日志 —— 让推理时间线
//    能看出流水线确实走过这一步,而不是"疑似漏掉了"。
func (s *Service) stepCheckJoint(ctx context.Context, ap *model.ApprovalInstance, settlements []SettlementRow, signals *[]signalDraft, items []model.ApprovalItem) JointSummary {
	start := time.Now()
	before := len(*signals)
	tol := decimal.NewFromFloat(s.cfg.PaymentToleranceYuan)
	cache := make(map[string]float64)
	minRateOf := func(supplierID string) float64 {
		if v, ok := cache[supplierID]; ok {
			return v
		}
		v := s.minJointDeductRate(ctx, ap.BranchID, supplierID)
		cache[supplierID] = v
		return v
	}
	sigs, summary := checkJoint(settlements, items, minRateOf, tol)
	*signals = append(*signals, sigs...)
	if !summary.HasJoint {
		s.logStep(ctx, ap, StepCheckJoint, "无联营结算单", "跳过联营扣点核对", "", time.Since(start).Milliseconds())
		return summary
	}
	verdict := fmt.Sprintf("应扣点 %s 元,申报「扣点」收款 %s 元,合计扣点率 %s", summary.DeductYuan.StringFixed(2),
		summary.DeductReceiptYuan.StringFixed(2), summary.DeductRatePercent.StringFixed(2)+"%")
	s.logStep(ctx, ap, StepCheckJoint,
		fmt.Sprintf("联营结算单 %d 张,销售额合计 %s 元", summary.SettlementCount, summary.SalesYuan.StringFixed(2)),
		fmt.Sprintf("新增 %d 条联营扣点信号;%s", len(*signals)-before, verdict),
		"", time.Since(start).Milliseconds())
	return summary
}
