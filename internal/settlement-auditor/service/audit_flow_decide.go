// Package service / audit_flow_decide.go —— 机器审核的「流程定制」决策。
//
// # 用户口径（2026-10-28）
//
//	「审批结算」的机器审核先按**有无结算单**分类，这是第一个路径节点：
//	  - 有结算单 → 走**供应商**判断
//	      · 供应商配了「逻辑判断流程」→ 跑逻辑条件（当前只有"每日有且仅有一单"）
//	      · 供应商配了「直接通过」    → 直接通过
//	      · 供应商**没配置**         → 走现在实现的机器人（LLM）审核流程
//	  - 无结算单 → 走**付款项目**判断
//	      · 付款项配了「逻辑判断流程」→ 跑逻辑条件（当前只有"每月有且仅有一单"）
//	      · 付款项配了「直接通过」    → 直接通过
//	      · 付款项**没配置**         → 走现在实现的机器人（LLM）审核流程
//
// # 三条已定的语义（用户 2026-10-28 拍板）
//
//	① 「每日有且仅有一单」= 查**结算单结算到的采购订单**，
//	   这些采购订单按**制单日期**每日只有 1 张。
//	② 逻辑条件**不满足** ⇒ 直接机器拒绝（写明不满足的原因），**不回落到 LLM**。
//	③ 多个主体（多供应商 / 多付款项类型）⇒ **逐个评估，任一不满足就整单不通过**。
//
// # 决策发生在哪一步
//
//	流水线的 step 6（结论环节）。step 1~5（规则核对）照跑 ——
//	所以「数据正确性」类的阻断信号（结算单不存在、金额勾稽不符…）仍然硬拦，
//	见 flowHardStopSignals 的理由。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// ---- 决策路径 ----
const (
	// FlowPathSupplier 是「有结算单」那条路（按供应商查配置）。
	FlowPathSupplier = "supplier"
	// FlowPathPaymentItem 是「无结算单」那条路（按付款项目查配置）。
	FlowPathPaymentItem = "payment_item"
)

// flowHardStopSignals 是流程决策路径上**仍然硬拦**的信号类型。
//
// 判据：这些是**数据正确性**问题（单据不存在 / 未审核 / 金额勾稽不上），
// 不是"审核意见"。流程配置不该让系统在一张不存在的结算单、
// 或金额对不上的单子上放款 —— 那种单子连一份正确的企微审批单都构造不出来。
//
// ⚠️ 数量类信号（purchase_without_sales / qty_deviation）**不在**此列：
// 那是**业务判断**（进了货没卖可能是正常的），本来就归机器人研判；
// 命中流程配置时由流程结论裁决，不��规则代劳。
var flowHardStopSignals = map[string]bool{
	SigSettlementMissing:     true,
	SigSettlementNotApproved: true,
	SigSheetMissing:          true,
	SigSheetNotApproved:      true,
	SigSupplierMismatch:      true,
	SigLineAmountMismatch:    true,
	SigManualAmountMismatch:  true,
}

// FlowSubjectResult 是单个主体（供应商 / 付款项类型）的评估结果。
type FlowSubjectResult struct {
	// Kind 是 FlowPathSupplier / FlowPathPaymentItem。
	Kind string `json:"kind"`
	// ID 是供应商 id / 付款项类型文本。
	ID string `json:"id"`
	// Name 是展示名（供应商名 / 付款项类型）。
	Name string `json:"name"`
	// Mode / LogicKind 是命中的配置。
	Mode      string `json:"mode"`
	LogicKind string `json:"logic_kind"`
	// Satisfied 是该主体的评估结果。
	Satisfied bool `json:"satisfied"`
	// Reason 是可读的评估说明（满足与不满足都写）。
	Reason string `json:"reason"`
}

// FlowDecision 是流程决策的产物。
type FlowDecision struct {
	// Applied 表示命中了配置。
	//
	// ⚠️ false ⇒ **一个都没命中** ⇒ 走机器人审核流程（现状不变）。
	Applied bool `json:"applied"`
	// Path 是命中的路径（supplier / payment_item）。
	Path string `json:"path"`
	// Reason 是整体结论说明（会写进 llm_verdicts.reason 与审计日志）。
	Reason string `json:"reason"`
	// Subjects 是逐个主体的评估明细。
	Subjects []FlowSubjectResult `json:"subjects"`
}

// decideAuditFlow 按「有无结算单」分路，查配置，逐个评估主体。
//
// ⚠️ 返回 Applied=false 时，调用方**必须**走原来的机器人审核流程 ——
// 那才是「没配置的供应商/付款项」的既定行为。
func (s *Service) decideAuditFlow(ctx context.Context, ap *model.ApprovalInstance,
	settlements []SettlementRow, linesBySettlement map[string][]SettlementLineRow,
	sheets []PurchaseSheetRow, items []model.ApprovalItem,
) (*FlowDecision, error) {
	dec := &FlowDecision{Subjects: []FlowSubjectResult{}}

	if len(settlements) > 0 {
		// ---- 分支一：有结算单 → 供应商判断 ----
		dec.Path = FlowPathSupplier
		for _, sid := range uniqueSupplierIDs(settlements) {
			cfg, err := s.findSupplierFlow(ctx, ap.BranchID, sid)
			if err != nil {
				return nil, err
			}
			if cfg == nil {
				continue // 未配置 ⇒ 不参与本流程
			}
			// ⚠️ **按主体隔离**：只拿**这个供应商**的结算单结算到的采购单去判。
			//
			//	否则一单里有两个供应商、各有 1 张同日采购单时，
			//	合并后变成"同日 2 张" ⇒ 两个主体都被判不满足 ⇒ 整单误拒。
			//	用户口径③说「**逐个评估**」⇒ 每个主体只看自己那份数据。
			sub, err := s.evaluateSupplierFlow(cfg, sheetsOfSupplier(settlements, linesBySettlement, sheets, sid))
			if err != nil {
				return nil, err
			}
			dec.Subjects = append(dec.Subjects, sub)
		}
	} else {
		// ---- 分支二：无结算单 → 付款项目判断 ----
		dec.Path = FlowPathPaymentItem
		for _, it := range uniquePaymentItemTypes(items) {
			cfg, err := s.findItemFlow(ctx, ap.BranchID, it)
			if err != nil {
				return nil, err
			}
			if cfg == nil {
				continue
			}
			sub, err := s.evaluateItemFlow(ctx, ap, cfg, items)
			if err != nil {
				return nil, err
			}
			dec.Subjects = append(dec.Subjects, sub)
		}
	}

	if len(dec.Subjects) == 0 {
		// 一个都没命中配置 ⇒ 走机器人审核流程（现状）。
		return dec, nil
	}
	dec.Applied = true

	// ③ 逐个评估，任一不满足 ⇒ 整单不通过。
	var failed []string
	for _, sub := range dec.Subjects {
		if !sub.Satisfied {
			failed = append(failed, fmt.Sprintf("[%s %s] %s", sub.Kind, sub.Name, sub.Reason))
		}
	}
	if len(failed) > 0 {
		dec.Reason = "流程定制判定不通过:" + strings.Join(failed, ";")
		return dec, nil
	}
	parts := make([]string, 0, len(dec.Subjects))
	for _, sub := range dec.Subjects {
		parts = append(parts, fmt.Sprintf("[%s %s] %s", sub.Kind, sub.Name, sub.Reason))
	}
	dec.Reason = "流程定制判定通过:" + strings.Join(parts, ";")
	return dec, nil
}

// findSupplierFlow 查某门店某供应商的配置（没配置 / 已停用 ⇒ nil）。
func (s *Service) findSupplierFlow(ctx context.Context, branchID, supplierID string) (*model.AuditSupplierFlow, error) {
	var row model.AuditSupplierFlow
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND supplier_id = ? AND enabled = ?", branchID, supplierID, true).
		First(&row).Error
	if err != nil {
		if errorsIsRecordNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("find supplier flow: %w", err)
	}
	return &row, nil
}

// findItemFlow 查某门店某付款项类型的配置（没配置 / 已停用 ⇒ nil）。
func (s *Service) findItemFlow(ctx context.Context, branchID, itemType string) (*model.AuditPaymentItemFlow, error) {
	var row model.AuditPaymentItemFlow
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND item_type = ? AND enabled = ?", branchID, itemType, true).
		First(&row).Error
	if err != nil {
		if errorsIsRecordNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("find item flow: %w", err)
	}
	return &row, nil
}

// sheetsOfSupplier 取某个供应商**自己的结算单结算到的采购单**。
//
// # 口径（用户 2026-10-28 拍板①）
//
//	「查结算单的具体结算的采购订单」——
//	链路 = 结算单 → 结算明细行的采购单号(voucher) → 采购单头。
//
// ⚠️ 为什么必须**按供应商**取，而不是把整单的采购单都丢进去：
//
//	一单里有两个供应商、各 1 张同日采购单时，合并后是"同日 2 张"
//	⇒ 两个主体都会被判不满足 ⇒ 整单误拒（正常付款被卡）。
//	用户口径③是「**逐个评估**」⇒ 每个主体只评估自己的那份数据。
//
// ⚠️ 单据归属用**结算单上的供应商**（审批人勾的那几张单），
//	而不是采购单自己的供应商字段 ——
//	两者不一致时 checkSheets 会出 supplier_mismatch 硬拦信号，整单已被拒，
//	这里怎么取都不影响结论。
func sheetsOfSupplier(settlements []SettlementRow, linesBySettlement map[string][]SettlementLineRow,
	sheets []PurchaseSheetRow, supplierID string,
) []PurchaseSheetRow {
	voucherSet := map[string]struct{}{}
	for _, st := range settlements {
		if st.SupplierID != supplierID {
			continue
		}
		for _, ln := range linesBySettlement[st.ID] {
			if ln.VoucherID != "" {
				voucherSet[ln.VoucherID] = struct{}{}
			}
		}
	}
	if len(voucherSet) == 0 {
		return nil
	}
	out := make([]PurchaseSheetRow, 0, len(voucherSet))
	for _, sh := range sheets {
		if _, ok := voucherSet[sh.ID]; ok {
			out = append(out, sh)
		}
	}
	return out
}

// evaluateSupplierFlow 评估一个供应商的流程配置。
func (s *Service) evaluateSupplierFlow(cfg *model.AuditSupplierFlow, sheets []PurchaseSheetRow) (FlowSubjectResult, error) {
	sub := FlowSubjectResult{
		Kind: FlowPathSupplier, ID: cfg.SupplierID, Name: cfg.SupplierName,
		Mode: cfg.Mode, LogicKind: cfg.LogicKind,
	}
	switch cfg.Mode {
	case model.FlowModeAutoPass:
		sub.Satisfied = true
		sub.Reason = "该供应商已配置为直接通过"
		return sub, nil
	case model.FlowModeLogic:
		switch cfg.LogicKind {
		case model.LogicDailySingle:
			ok, reason := evaluateDailySingle(sheets)
			sub.Satisfied = ok
			sub.Reason = reason
			return sub, nil
		default:
			sub.Satisfied = false
			sub.Reason = "不支持的逻辑条件:" + cfg.LogicKind
			return sub, nil
		}
	default:
		sub.Satisfied = false
		sub.Reason = "不支持的流程模式:" + cfg.Mode
		return sub, nil
	}
}

// evaluateDailySingle 判定「每日有且仅有一单」。
//
// # 口径（用户 2026-10-28 拍板）
//
//	「查结算单的具体结算的采购订单，采购订单每日只有 1 张。」
//	⇒ 被检查的集合 = 本次结算单**结算到的采购订单**（cube purchase_sheet），
//	  分组维度 = 采购单的**制单日期**（PurchaseSheetRow.CreatedAt），
//	  条件 = 每个日期分组里恰好 1 张采购单。
//
// ⚠️ 入参 sheets 已经由调用方**按供应商主体隔离**（见 sheetsOfSupplier）——
//	本函数只负责"给的这组采购单里，同一天有没有超过 1 张"。
//
// # 边界
//
//   - 0 张采购单 ⇒ **满足**（条件无对象，不构成违反）。
//     费用单/返利单这类非采购结算单不该被这条规则卡住。
//   - 采购单**取不到日期** ⇒ 不参与分组（避免把"两张都没日期"误判成同日多单）。
func evaluateDailySingle(sheets []PurchaseSheetRow) (bool, string) {
	if len(sheets) == 0 {
		return true, "本单未结算任何采购单，「每日有且仅有一单」无判定对象"
	}
	byDate := map[string][]string{}
	unknown := 0
	for _, sh := range sheets {
		d := strings.TrimSpace(sh.CreatedAt)
		if d == "" {
			unknown++
			continue
		}
		byDate[d] = append(byDate[d], sh.ID)
	}
	dates := make([]string, 0, len(byDate))
	for d := range byDate {
		dates = append(dates, d)
	}
	sort.Strings(dates)

	var violations []string
	for _, d := range dates {
		if len(byDate[d]) > 1 {
			violations = append(violations,
				fmt.Sprintf("%s 有 %d 张采购单(%s)", d, len(byDate[d]), strings.Join(byDate[d], ",")))
		}
	}
	if len(violations) > 0 {
		return false, "不满足「每日有且仅有一单」:" + strings.Join(violations, ";")
	}
	reason := fmt.Sprintf("满足「每日有且仅有一单」:结算到的采购单分布在 %d 个日期,每日各 1 张", len(dates))
	if unknown > 0 {
		reason += fmt.Sprintf("(%d 张采购单取不到制单日期,未参与判定)", unknown)
	}
	return true, reason
}

// evaluateItemFlow 评估一个付款项类型的流程配置。
func (s *Service) evaluateItemFlow(ctx context.Context, ap *model.ApprovalInstance,
	cfg *model.AuditPaymentItemFlow, items []model.ApprovalItem,
) (FlowSubjectResult, error) {
	sub := FlowSubjectResult{
		Kind: FlowPathPaymentItem, ID: cfg.ItemType, Name: cfg.ItemType,
		Mode: cfg.Mode, LogicKind: cfg.LogicKind,
	}
	switch cfg.Mode {
	case model.FlowModeAutoPass:
		sub.Satisfied = true
		sub.Reason = "该付款项目已配置为直接通过"
		return sub, nil
	case model.FlowModeLogic:
		switch cfg.LogicKind {
		case model.LogicMonthlySingle:
			ok, reason, err := s.evaluateMonthlySingle(ctx, ap, cfg, items)
			if err != nil {
				return sub, err
			}
			sub.Satisfied = ok
			sub.Reason = reason
			return sub, nil
		default:
			sub.Satisfied = false
			sub.Reason = "不支持的逻辑条件:" + cfg.LogicKind
			return sub, nil
		}
	default:
		sub.Satisfied = false
		sub.Reason = "不支持的流程模式:" + cfg.Mode
		return sub, nil
	}
}

// yearMonth 是「所属年/月」的键（付款项的所属日期）。
type yearMonth struct {
	Year  int
	Month int
}

// evaluateMonthlySingle 判定「每月有且仅有一单」。
//
// # 口径
//
//	该门店该付款项类型，在**所属年/月**上的笔数（含本单）必须恰好为 1。
//
//	「所属年/月」从**备注**里提取（用户口径：备注写"9月水电费"⇒ 9 月），
//	提取用的正则是该付款项配置里指定的那条（audit_regex_patterns）。
//
// # 为什么用「所属月份」而不是「审批单月份」
//
//	9 月的水电费一般在 10 月付 —— 按审批单月份统计会把跨月补缴算成两笔。
//
// # 年份从哪来
//
//	备注里没写年 ⇒ 取**该笔所属审批单的年份**（本单用本单的年份）。
//	历史笔数同理（各自审批单的年份）—— 否则跨年补缴会落到错误的月份窗口。
func (s *Service) evaluateMonthlySingle(ctx context.Context, ap *model.ApprovalInstance,
	cfg *model.AuditPaymentItemFlow, items []model.ApprovalItem,
) (bool, string, error) {
	re, groupIndex, err := s.loadRegex(ctx, ap.BranchID, cfg.RegexID)
	if err != nil {
		return false, "", err
	}
	if re == nil {
		return false, "无法判定「每月有且仅有一单」:该付款项目没有可用的所属日期提取正则", nil
	}
	cur := paymentItemsOfType(items, cfg.ItemType)
	if len(cur) == 0 {
		return false, "本单没有该类型的付款项", nil
	}
	counts := map[yearMonth]int{}
	defaultYear := s.approvalYear(ap)
	for _, it := range cur {
		text, ok := matchRegexGroup(re, groupIndex, it.Remark)
		if !ok {
			return false, fmt.Sprintf("无法从备注 %q 提取款项所属日期(正则未匹配)", it.Remark), nil
		}
		y, m, ok := parsePeriodText(text, defaultYear)
		if !ok {
			return false, fmt.Sprintf("无法从备注 %q 的提取结果 %q 解析出所属年/月", it.Remark, text), nil
		}
		counts[yearMonth{y, m}]++
	}
	hist, err := s.countHistoricalMonthlyItems(ctx, ap, cfg, re, groupIndex)
	if err != nil {
		return false, "", err
	}
	for k, v := range hist {
		counts[k] += v
	}
	keys := make([]yearMonth, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Year != keys[j].Year {
			return keys[i].Year < keys[j].Year
		}
		return keys[i].Month < keys[j].Month
	})
	var violations []string
	for _, k := range keys {
		if counts[k] != 1 {
			violations = append(violations,
				fmt.Sprintf("%d 年 %d 月有 %d 笔「%s」", k.Year, k.Month, counts[k], cfg.ItemType))
		}
	}
	if len(violations) > 0 {
		return false, "不满足「每月有且仅有一单」:" + strings.Join(violations, ";"), nil
	}
	return true, fmt.Sprintf("满足「每月有且仅有一单」:%s 所属月份各 1 笔", cfg.ItemType), nil
}

// histItemRow 是历史付款项的一行（带所属审批单的年份）。
type histItemRow struct {
	ApprovalID        string
	Remark            string
	ApprovalCreatedAt time.Time
}

// countHistoricalMonthlyItems 统计**其它审批单**里该付款项类型的「所属年/月」笔数。
//
// ⚠️ **只算还在生效的审批单**（rejected / failed / cancelled 不算）：
//
//	机器拒绝过的单子**没有付过款** ⇒ 不该占掉该月的额度。
//	否则「拒了 → 改好再提交」会永远卡在"该月已有 1 笔"上（无限循环）。
//
// ⚠️ 本单被排除（excludeApprovalID）⇒ 重跑/重审不会把自己算两遍。
func (s *Service) countHistoricalMonthlyItems(ctx context.Context, ap *model.ApprovalInstance,
	cfg *model.AuditPaymentItemFlow, re *regexp.Regexp, groupIndex int,
) (map[yearMonth]int, error) {
	var rows []histItemRow
	err := s.db.WithContext(ctx).Model(&model.ApprovalItem{}).
		Select(`approval_items.approval_id AS approval_id,
			approval_items.remark AS remark,
			approval_instances.created_at AS approval_created_at`).
		Joins("JOIN approval_instances ON approval_instances.id = approval_items.approval_id").
		Where(`approval_items.branch_id = ? AND approval_items.direction = ?
			AND approval_items.item_type = ? AND approval_items.approval_id <> ?
			AND approval_instances.status NOT IN ?`,
			ap.BranchID, ItemDirectionPayment, cfg.ItemType, ap.ID,
			[]string{model.StatusRejected, model.StatusFailed, model.StatusCancelled}).
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("count historical monthly items: %w", err)
	}
	out := map[yearMonth]int{}
	for _, r := range rows {
		text, ok := matchRegexGroup(re, groupIndex, r.Remark)
		if !ok {
			// 历史行提不出月份 ⇒ 归不到任何月份 ⇒ 不计数。
			// ⚠️ 不当成"该月多了一笔"：那是误拒。
			continue
		}
		y, m, ok := parsePeriodText(text, r.ApprovalCreatedAt.Year())
		if !ok {
			continue
		}
		out[yearMonth{y, m}]++
	}
	return out, nil
}

// loadRegex 取该门店的正则（没配 / 不存在 / 已停用 / 非法 ⇒ nil）。
func (s *Service) loadRegex(ctx context.Context, branchID, regexID string) (*regexp.Regexp, int, error) {
	regexID = strings.TrimSpace(regexID)
	if regexID == "" {
		return nil, 0, nil
	}
	var row model.AuditRegexPattern
	if err := s.db.WithContext(ctx).
		Where("id = ? AND branch_id = ? AND enabled = ?", regexID, branchID, true).
		First(&row).Error; err != nil {
		if errorsIsRecordNotFound(err) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("load regex pattern: %w", err)
	}
	re, err := regexp.Compile(row.Pattern)
	if err != nil {
		return nil, 0, fmt.Errorf("编译正则失败(id=%s): %w", regexID, err)
	}
	return re, row.GroupIndex, nil
}

// matchRegexGroup 用正则跑一段文本并取指定捕获组（不匹配 / 组号越界 ⇒ false）。
func matchRegexGroup(re *regexp.Regexp, groupIndex int, sample string) (string, bool) {
	if re == nil {
		return "", false
	}
	m := re.FindStringSubmatch(sample)
	if m == nil || groupIndex >= len(m) {
		return "", false
	}
	return m[groupIndex], true
}

// approvalYear 取审批单年份（所属月份的缺省年）。
func (s *Service) approvalYear(ap *model.ApprovalInstance) int {
	if ap != nil && !ap.CreatedAt.IsZero() {
		return ap.CreatedAt.Year()
	}
	return time.Now().Year()
}

// buildFlowVerdict 把流程决策转成 VerdictResult。
//
// ⚠️ **数据正确性硬拦优先于流程结论**：
// 流程配置说"直接通过"，但这一单的结算单在 ERP 里不存在 / 金额勾稽不上 ⇒
// 仍然拒绝（见 flowHardStopSignals 的理由）。
//
// ⚠️ **不产盘点建议 ⇒ 不触发「盘点联动」**（2026-10-28 用户口径）：
//
//	「按流程逻辑通过的不需要触发盘点联动」。
//
//	「盘点联动」是**机器人研判的副产物**（stepStocktake 按 verdict 的建议建
//	针对性盘点计划）。命中流程定制 ⇒ 这一单**根本没问机器人** ⇒ 没有研判
//	⇒ 不该凭空造一个盘点计划出来（那等于让人去盘一台机器没怀疑过的货）。
//
//	⚠️ 顺带把 raw 里的 "suggest_stocktake" 也去掉 ——
//		留着它等于在 wire 上宣称"建议盘点"，详情页会照着显示。
func (s *Service) buildFlowVerdict(dec *FlowDecision, signals []signalDraft) *VerdictResult {
	var hard []string
	for _, d := range signals {
		if d.Level == model.SignalBlock && flowHardStopSignals[d.SignalType] {
			hard = append(hard, d.Message)
		}
	}
	reason := dec.Reason
	conclusion := ConclusionPass
	if len(hard) > 0 {
		conclusion = ConclusionReject
		reason = "流程定制判定被数据校验阻断:" + strings.Join(hard, ";")
	}
	raw, _ := json.Marshal(map[string]any{
		"conclusion": conclusion,
		"reason":     reason,
		"confidence": 1,
		"flow": map[string]any{
			"applied":  dec.Applied,
			"path":     dec.Path,
			"subjects": dec.Subjects,
		},
	})
	return &VerdictResult{
		Conclusion: conclusion,
		Reason:     reason,
		Confidence: decimal.NewFromInt(1),
		// ⚠️ SuggestStocktake 故意留 nil ⇒ stepStocktake 直接返回 ⇒
		//    时间线上不会出现「盘点联动」这一条。
		RawJSON: raw,
		Source:  model.VerdictSourceFlowConfig,
	}
}

// decideVerdict 出结论：**命中流程定制配置 ⇒ 用流程结论；没命中 ⇒ 机器人(LLM)研判**。
//
// ⚠️ 「没命中」是**绝大多数单子**的路径 ⇒ 行为与改造前**完全一致**
//
//	（规则核对照跑、LLM 照问、结论照落 llm_verdicts）。
//
// ⚠️ 命中时**不再调 LLM**（这是"流程定制"的意义）；
//
//	但 step 1~5 的规则核对照跑，数据正确性类阻断信号仍然硬拦。
func (s *Service) decideVerdict(ctx context.Context, ap *model.ApprovalInstance,
	settlements []SettlementRow, linesBySettlement map[string][]SettlementLineRow,
	sheets []PurchaseSheetRow, signals []signalDraft, amount *AmountCheck,
	items []model.ApprovalItem, allowance Allowance, extras ...AuditExtras,
) (*VerdictResult, error) {
	dec, err := s.decideAuditFlow(ctx, ap, settlements, linesBySettlement, sheets, items)
	if err != nil {
		return nil, fmt.Errorf("decide audit flow: %w", err)
	}
	if !dec.Applied {
		// 没命中任何配置 ⇒ 走机器人审核流程（现状）。
		return s.stepLLMVerdict(ctx, ap, settlements, linesBySettlement, sheets, signals, amount, items, allowance, extras...)
	}
	start := time.Now()
	verdict := s.buildFlowVerdict(dec, signals)
	trace, _ := json.Marshal(dec)
	if err := s.saveVerdict(ctx, ap, verdict, string(trace), StepFlowDecision, len(signals),
		time.Since(start).Milliseconds()); err != nil {
		return nil, err
	}
	s.logger.Info("audit flow decision applied",
		"approval_id", ap.ID, "branch_id", ap.BranchID, "path", dec.Path,
		"conclusion", verdict.Conclusion)
	return verdict, nil
}

// uniqueSupplierIDs 取结算单上的供应商 id（去重、保序）。
func uniqueSupplierIDs(settlements []SettlementRow) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(settlements))
	for _, st := range settlements {
		id := strings.TrimSpace(st.SupplierID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// uniquePaymentItemTypes 取本单付款项的类型文本（去重、保序）。
func uniquePaymentItemTypes(items []model.ApprovalItem) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if it.Direction != ItemDirectionPayment {
			continue
		}
		t := strings.TrimSpace(it.ItemType)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// paymentItemsOfType 取本单里某类型的付款项。
func paymentItemsOfType(items []model.ApprovalItem, itemType string) []model.ApprovalItem {
	out := make([]model.ApprovalItem, 0, 2)
	for _, it := range items {
		if it.Direction == ItemDirectionPayment && strings.TrimSpace(it.ItemType) == itemType {
			out = append(out, it)
		}
	}
	return out
}
