// Package service / pending_approval.go —— 结算单「待审批中」状态（2026-10-29 用户口径）。
//
// # 用户口径原文
//
//	「已过机器审核提交"付款审批"时，该结算单未待审批中不能被再次选择，
//	  审批拒绝后恢复为可选择」
//
// # 语义：「待审批中」= 该结算单被**某一笔尚未出终态**的付款审批占用
//
// 判定事实源 = approval_settlements ⋈ approval_instances：
// 只要有一笔审批仍处在「已提交、尚未有终态」的状态，这张结算单就是待审批中。
//
// # 为什么与「已付款」是两回事
//
// 「已付款」（settlement_pay_states 台账）= 这张单**付掉了**，永远不能再选；
// 「待审批中」= 这张单**正在走流程**，还没付，但也不能再选一次 ——
// 否则同一张结算单会被提交两遍、付两次钱。
//
// 两者的**生命周期不同** ⇒ 恢复可选项也不同：
//
//   - 已付款：永不恢复（财务视角：付过就是付过）；
//   - 待审批中：**审批被拒 / 流水线失败 / 被撤销后自动恢复可选**
//     （用户口径「审批拒绝后恢复为可选择」）。
//
// # 为什么状态集合是这三个
//
//	received  —— 刚提交，流水线还没起来（防"秒提交两次"）；
//	auditing  —— 机器审核在跑；
//	approved  —— 机器一审通过、企微审批单已提交，等人工终审
//	             （用户口径说的就是这一档："已过机器审核提交付款审批时"）。
//
//	排除的终态：rejected（机器拒绝）、failed（流水线异常）、
//	            human_rejected（企微驳回）、cancelled（撤销）
//	            ⇒ 这四种一律**恢复可选**；
//	            human_approved（企微通过）⇒ 那时已记为已付款，由台账接管。
package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// pendingApprovalStatuses 是「待审批中」的审批状态集合。
//
// ⚠️ 这是**判据**不是留痕：改这里等于改"哪些单不能被再选一次"。
var pendingApprovalStatuses = []string{
	model.StatusReceived,
	model.StatusAuditing,
	model.StatusApproved,
}

// ErrSettlementPending —— 结算单已在另一笔审批中待审批，不能重复提交（映射 409）。
//
// 为什么是 409 而不是 400：这不是请求写错了，客户端改任何参数都没用 ——
// 只能等那一笔审批出终态（或被拒绝后重新选）。400 会让清理脚本/重试逻辑
// 把它当成参数错误而放弃，或者 endless 重试。
var ErrSettlementPending = errors.New("service: 结算单已在审批中,不能重复提交")

// PendingSettlement 描述一张「待审批中」的结算单。
type PendingSettlement struct {
	SettlementID string `gorm:"column:settlement_id" json:"settlement_id"`
	ApprovalID   string `gorm:"column:approval_id" json:"approval_id"`
	Status       string `gorm:"column:status" json:"status"`
}

// pendingSettlementRow 是查库用的行结构（列名显式标注，避免依赖命名策略推断）。
type pendingSettlementRow struct {
	SettlementID string `gorm:"column:settlement_id"`
	ApprovalID   string `gorm:"column:approval_id"`
	Status       string `gorm:"column:status"`
}

// ListPendingSettlements 读给定 id 中哪些处于「待审批中」。
//
// 返回按 settlement_id 排序的列表（去重：一个单可能同时被多笔审批引用 ——
// 例如上一批被拒后重提，**旧审批还没落终态**时）。
//
// ⚠️ branch 必须带：approval_settlements 表**没有** branch_id 列，
//   只能 JOIN approval_instances 拿 ⇒ 这也是必须 JOIN 而不能单表查的原因。
func (s *Service) ListPendingSettlements(ctx context.Context, branchID string, ids []string) ([]PendingSettlement, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	out := make([]PendingSettlement, 0)
	seen := make(map[string]bool)
	for start := 0; start < len(ids); start += settlementInClauseChunk {
		end := start + settlementInClauseChunk
		if end > len(ids) {
			end = len(ids)
		}
		var rows []pendingSettlementRow
		if err := s.db.WithContext(ctx).
			Model(&model.ApprovalSettlement{}).
			Select("approval_settlements.settlement_id, approval_settlements.approval_id, approval_instances.status").
			Joins("JOIN approval_instances ON approval_instances.id = approval_settlements.approval_id").
			Where("approval_instances.branch_id = ?", branchID).
			Where("approval_instances.status IN ?", pendingApprovalStatuses).
			Where("approval_settlements.settlement_id IN ?", ids[start:end]).
			Scan(&rows).Error; err != nil {
			return nil, fmt.Errorf("read pending settlements: %w", err)
		}
		for _, r := range rows {
			if seen[r.SettlementID] {
				continue
			}
			seen[r.SettlementID] = true
			out = append(out, PendingSettlement{
				SettlementID: r.SettlementID,
				ApprovalID:   r.ApprovalID,
				Status:       r.Status,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SettlementID < out[j].SettlementID })
	return out, nil
}

// pendingSettlementIDSet 把待审批列表压成 id 集合。
func pendingSettlementIDSet(list []PendingSettlement) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, p := range list {
		out[strings.TrimSpace(p.SettlementID)] = true
	}
	return out
}

// FilterOutPendingSettlements 从候选列表里剔除「待审批中」的结算单。
//
// 与 FilterOutPaidSettlements 同一形状：返回(剩下的行, 被剔除的张数)。
// 被剔除的张数要回给前端 —— 店长看到列表少了一堆单,会以为是系统出错,
// 必须能告诉他"这里隐藏了 N 张待审批中的"。
func FilterOutPendingSettlements(rows []SettlementRow, pending map[string]bool) ([]SettlementRow, int) {
	if len(pending) == 0 {
		return rows, 0
	}
	out := make([]SettlementRow, 0, len(rows))
	hidden := 0
	for _, r := range rows {
		if pending[strings.TrimSpace(r.ID)] {
			hidden++
			continue
		}
		out = append(out, r)
	}
	return out, hidden
}

// pendingStatusLabel 把审批状态翻成人话(错误提示用)。
func pendingStatusLabel(status string) string {
	switch status {
	case model.StatusReceived:
		return "已提交待审核"
	case model.StatusAuditing:
		return "机器审核中"
	case model.StatusApproved:
		return "已提交企微待终审"
	default:
		return status
	}
}

// describePendingSettlements 把待审批的结算单拼成一句可读的提示。
//
// 只列前若干张（后面跟总数)—— 一张审批可能勾了几十张单,
// 把 id 全列出来对店长没有意义。
func describePendingSettlements(list []PendingSettlement) string {
	const maxShow = 5
	if len(list) == 0 {
		return ""
	}
	ids := make([]string, 0, len(list))
	for i, p := range list {
		if i >= maxShow {
			break
		}
		ids = append(ids, p.SettlementID)
	}
	msg := "结算单 " + strings.Join(ids, "、")
	if len(list) > maxShow {
		msg += fmt.Sprintf(" 等 %d 张", len(list))
	}
	apID := list[0].ApprovalID
	msg += fmt.Sprintf(" 已在审批单 %s(%s)中待审批,不能重复提交", apID, pendingStatusLabel(list[0].Status))
	return msg
}
