// Package service / settlement_status.go —— 结算单「在哪儿、什么状态」（2026-10-29 用户口径）。
//
// # 用户口径原文
//
//	「CP002608194277，CP002609024324 没有找到，初始化结算也无法查找…
//	  在此场景下，反思遇到这类结算单，我该如何准确找出该单在什么状态？
//	  被初始化结算了，还是待审核中，如果待审批，那现在它在哪个付款审批里，
//	  查到了，我从哪里可以点击进去查看其审批详情。」
//
// # 为什么「找不到」本身就是一个答案
//
// 「发起付款审批」的可勾选列表（cube/settlements）是**可付款集合**，
// 它按设计要剔除两类单：
//
//   - **已付款**（台账 settlement_pay_states 有记录）—— 付过就是付过；
//   - **待审批中**（被某笔未终态的审批占用）—— 不能再选一次，否则付两次钱。
//
// ⇒ 「在可勾选列表里找不到」**恰恰说明这张单已经有状态了**，
//   只是那个列表**不告诉你它是什么状态、在哪一笔审批里**。
//   于是店长只能靠"翻台账 / 翻审批列表 / 问财务"三处拼凑 ⇒ 这就是本文件。
//
// # 三个问题，三个答案
//
//  1. **源库有没有？**（not_audited / not_payable / not_found）
//     —— 用 GetSettlementsByIDs 这种**不过滤**的查询才能区分。
//  2. **台账里什么状态？**（paid_init / paid_approval / pending / payable）
//  3. **待审批中的话在哪一笔审批？**（approval_id + sp_no + 状态）
//     —— 前端拿 approval_id 就能跳进审批详情页。
//
// # 与「可勾选列表」的关系
//
// 本文件**不替代**可勾选列表，而是回答"为什么它不在那个列表里"。
// 两者口径必须一致：列表剔除的每一类，在这里都要能说出**为什么**。
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

// 结算单状态枚举（前端按 state 渲染，不要按文案匹配）。
//
// ⚠️ 状态集合是**判据**不是留痕：加一个值就多一种"这张单不能被选"的理由，
// 少一个值就会让本该被拦的单漏出去。
const (
	// SettlementStatePayable = 可发起付款审批（没付、也没有在跑的审批）。
	SettlementStatePayable = "payable"
	// SettlementStatePending = 待审批中（被某笔未终态的审批占用）。
	SettlementStatePending = "pending"
	// SettlementStatePaidInit = 已付款（初始化补录进台账的）。
	SettlementStatePaidInit = "paid_init"
	// SettlementStatePaidApproval = 已付款（正常审批流程付掉的）。
	SettlementStatePaidApproval = "paid_approval"
	// SettlementStateNotAudited = 源库里有这张单，但**未审核**。
	SettlementStateNotAudited = "not_audited"
	// SettlementStateNotPayable = 源库里有这张单，但不是 DP/CP 采购结款单。
	SettlementStateNotPayable = "not_payable"
	// SettlementStateNotFound = 源库里查不到这张单号。
	SettlementStateNotFound = "not_found"
)

// SettlementStatusItem 是一张结算单的状态诊断结果。
type SettlementStatusItem struct {
	SettlementID string `json:"settlement_id"`
	SupplierID   string `json:"supplier_id"`
	SupplierName string `json:"supplier_name"`
	// GoodsAmountYuan 是货款净额（2 位小数字符串）；查不到源单时为空。
	GoodsAmountYuan string `json:"goods_amount_yuan"`
	DocumentType    string `json:"document_type"`
	ApproveStatus   string `json:"approve_status"`
	PeriodStart     string `json:"period_start"`
	PeriodEnd       string `json:"period_end"`

	// State 是状态枚举（见上面的 SettlementState*）。
	State string `json:"state"`
	// StateLabel 是给店长看的中文说明。
	StateLabel string `json:"state_label"`

	// ApprovalID / ApprovalSpNo / ApprovalStatus 在「待审批中」或
	// 「已付款（正常审批流程）」时给出 —— 前端拿 ApprovalID 跳审批详情。
	ApprovalID         string `json:"approval_id"`
	ApprovalSpNo       string `json:"approval_sp_no"`
	ApprovalStatus     string `json:"approval_status"`
	ApprovalStatusLabel string `json:"approval_status_label"`

	// PaidSetMethod 是台账的设置方式（init / approval）；未付款时为空。
	PaidSetMethod string `json:"paid_set_method"`
	// PaidAt 是台账记录的付款时间（RFC3339）；未付款时为空。
	PaidAt string `json:"paid_at"`
}

// settlementApprovalRow 是查"这张单出现在哪些审批里"用的行。
type settlementApprovalRow struct {
	SettlementID string `gorm:"column:settlement_id"`
	ApprovalID   string `gorm:"column:approval_id"`
	SpNo         string `gorm:"column:sp_no"`
	Status       string `gorm:"column:status"`
	CreatedAt    time.Time `gorm:"column:created_at"`
}

// ListSettlementStatuses 查一批结算单的状态诊断结果。
//
// ⚠️ **不存在的单号也会返回一条**（state=not_found）——
//    "你问的这张单压根不存在"是一个**答案**，不是"查不到"。
//    返回空列表会让前端显示"无数据"，店长以为是自己输错了单号。
func (s *Service) ListSettlementStatuses(ctx context.Context, branchID string, ids []string) ([]SettlementStatusItem, error) {
	clean := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		v := strings.TrimSpace(id)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		clean = append(clean, v)
	}
	if len(clean) == 0 {
		return []SettlementStatusItem{}, nil
	}

	// ① 源库原始行（不过滤审核/单据类型）—— 用来区分"不存在"与"存在但不可付"。
	sourceRows := map[string]SettlementRow{}
	if s.cube != nil {
		rows, err := s.cube.GetSettlementsByIDs(ctx, branchID, clean)
		if err != nil {
			return nil, fmt.Errorf("settlement status cube query: %w", err)
		}
		for _, r := range rows {
			sourceRows[strings.TrimSpace(r.ID)] = r
		}
		s.fillSupplierNames(ctx, branchID, rows)
		for i := range rows {
			if r, ok := sourceRows[strings.TrimSpace(rows[i].ID)]; ok {
				r.SupplierName = rows[i].SupplierName
				sourceRows[strings.TrimSpace(rows[i].ID)] = r
			}
		}
	}

	// ② 台账（已付款 / 初始化补录）。
	states, err := s.payStatesOf(ctx, branchID, clean)
	if err != nil {
		return nil, err
	}

	// ③ 出现在哪些审批里（含终态与进行中）。
	approvals, err := s.settlementApprovalRows(ctx, branchID, clean)
	if err != nil {
		return nil, err
	}

	out := make([]SettlementStatusItem, 0, len(clean))
	for _, id := range clean {
		st, hasState := states[id]
		out = append(out, buildSettlementStatus(id, sourceRows[id], st, hasState, approvals[id]))
	}
	return out, nil
}

// settlementApprovalRows 查这些结算单出现在哪些审批里。
//
// ⚠️ 必须 JOIN approval_instances 拿 branch_id（approval_settlements 没有门店列）。
// ⚠️ 按 created_at 倒序：同一张单可能先被拒又重提，要能挑到"最新那一笔"。
func (s *Service) settlementApprovalRows(ctx context.Context, branchID string, ids []string) (map[string][]settlementApprovalRow, error) {
	out := make(map[string][]settlementApprovalRow)
	if len(ids) == 0 {
		return out, nil
	}
	for start := 0; start < len(ids); start += settlementInClauseChunk {
		end := start + settlementInClauseChunk
		if end > len(ids) {
			end = len(ids)
		}
		var rows []settlementApprovalRow
		if err := s.db.WithContext(ctx).
			Model(&model.ApprovalSettlement{}).
			Select("approval_settlements.settlement_id, approval_instances.id AS approval_id, approval_instances.sp_no, approval_instances.status, approval_instances.created_at").
			Joins("JOIN approval_instances ON approval_instances.id = approval_settlements.approval_id").
			Where("approval_instances.branch_id = ?", branchID).
			Where("approval_settlements.settlement_id IN ?", ids[start:end]).
			Order("approval_instances.created_at DESC").
			Scan(&rows).Error; err != nil {
			return nil, fmt.Errorf("read settlement approvals: %w", err)
		}
		for _, r := range rows {
			key := strings.TrimSpace(r.SettlementID)
			out[key] = append(out[key], r)
		}
	}
	return out, nil
}

// buildSettlementStatus 把三处事实拼成一条状态诊断。
//
// 判定顺序（**先终态后进行中**）是关键：
//
//	已付款 > 待审批中 > 可付款。
//
// 为什么「已付款」优先于「待审批中」：一张单可能**先提交审批（pending）、
// 后又被人工放行付掉（human_approved）** —— 此时它同时命中两个判据，
// 而店长真正关心的是"付掉了没有" ⇒ 报 paid_approval。
func buildSettlementStatus(id string, src SettlementRow, state model.SettlementPayState, hasState bool, approvals []settlementApprovalRow) SettlementStatusItem {
	item := SettlementStatusItem{
		SettlementID: id,
		SupplierID:   src.SupplierID,
		SupplierName: src.SupplierName,
		DocumentType: src.DocumentType,
		ApproveStatus: src.ApproveStatus,
		PeriodStart:  src.PeriodStart,
		PeriodEnd:    src.PeriodEnd,
	}
	if src.TotalGoodsYuan.GreaterThan(decimal.Zero) || !src.TotalGoodsYuan.IsZero() {
		item.GoodsAmountYuan = src.TotalGoodsYuan.StringFixed(2)
	}

	// ① 源库压根没有这张单 ⇒ 先说这个（后面都是"台账/审批"层面的事实）。
	if src.ID == "" {
		item.State = SettlementStateNotFound
		item.StateLabel = "思迅里查不到这张单号（可能单号写错，或该单不是采购结款单）"
		// 仍然把台账/审批里的事实带上：万一它已经被本系统处理过，
		// 店长要知道的是"它付掉了/在跑"，而不是"源库没有"。
		if hasState && state.IsPaid() {
			applyPayStateToStatus(&item, state)
			return item
		}
		if ap := pickApproval(approvals, pendingApprovalStatuses); ap != nil {
			applyApprovalToStatus(&item, *ap)
			item.State = SettlementStatePending
			item.StateLabel = "待审批中：" + item.ApprovalStatusLabel
			return item
		}
		return item
	}

	// ② 已付款（台账有记录）。
	if hasState && state.IsPaid() {
		applyPayStateToStatus(&item, state)
		if ap := pickApproval(approvals, []string{model.StatusHumanApproved}); ap != nil {
			applyApprovalToStatus(&item, *ap)
		}
		return item
	}

	// ③ 待审批中。
	if ap := pickApproval(approvals, pendingApprovalStatuses); ap != nil {
		applyApprovalToStatus(&item, *ap)
		item.State = SettlementStatePending
		item.StateLabel = "待审批中（" + item.ApprovalStatusLabel + "）"
		return item
	}

	// ④ 源库有，但不可付。
	if strings.TrimSpace(src.ApproveStatus) != "1" {
		item.State = SettlementStateNotAudited
		item.StateLabel = "思迅里这张单还没审核，审核后才能发起付款审批"
		return item
	}
	if src.DocumentType != "DP" && src.DocumentType != "CP" {
		item.State = SettlementStateNotPayable
		item.StateLabel = "这张单不是采购结款单（单据类型 " + src.DocumentType + "），不能发起付款审批"
		return item
	}

	// ⑤ 可付款。
	item.State = SettlementStatePayable
	item.StateLabel = "可发起付款审批（未付款，也没有在跑的审批）"
	return item
}

// applyPayStateToStatus 把台账事实写进诊断结果。
func applyPayStateToStatus(item *SettlementStatusItem, state model.SettlementPayState) {
	item.PaidSetMethod = state.SetMethod
	if state.PaidAt != nil {
		item.PaidAt = state.PaidAt.Format(time.RFC3339)
	}
	if state.SetMethod == model.PayMethodInit {
		item.State = SettlementStatePaidInit
		item.StateLabel = "已付款（初始化补录进台账）"
		return
	}
	item.State = SettlementStatePaidApproval
	item.StateLabel = "已付款（由正常付款审批流程付掉）"
}

// applyApprovalToStatus 把审批事实写进诊断结果。
func applyApprovalToStatus(item *SettlementStatusItem, ap settlementApprovalRow) {
	item.ApprovalID = ap.ApprovalID
	item.ApprovalSpNo = ap.SpNo
	item.ApprovalStatus = ap.Status
	item.ApprovalStatusLabel = pendingStatusLabel(ap.Status)
}

// pickApproval 从审批列表里挑**最新的一笔**、且状态在 given 里的。
//
// ⚠️ 列表已按 created_at 倒序 ⇒ 取第一个命中的就是最新的。
// 为什么挑最新而不是最早：同一张单被拒后重提时，
// 旧的审批可能还没落终态（那它也算 pending），但店长关心的是**当前那一笔**。
func pickApproval(approvals []settlementApprovalRow, statuses []string) *settlementApprovalRow {
	for i := range approvals {
		for _, st := range statuses {
			if approvals[i].Status == st {
				return &approvals[i]
			}
		}
	}
	return nil
}

// SettlementStatusIDsFromText 从一段自由文本里抽出结算单号候选。
//
// 用户在「这张单去哪了」的场景里会直接把单号贴进来（可能带空格、逗号、
// 中文标点）⇒ 先按分隔符切开再逐个校验，省得前端做正则。
func SettlementStatusIDsFromText(text string) []string {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', ',', '，', ';', '；', '、', '|', '/':
			return true
		}
		return false
	})
	out := make([]string, 0, len(fields))
	seen := make(map[string]bool)
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}
