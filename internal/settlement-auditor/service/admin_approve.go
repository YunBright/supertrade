// Package service / admin_approve.go —— 管理员人工放行（机器拒绝的单）。
//
// # 用户口径（2026-10-28）
//
//	「在审批详情页针对被机器拒绝的增加『管理员』审批按钮，
//	 通过此按钮获批的审批需要在 AI 结论多文本控件**第一行**插入『已转人工审核并通过』。」
//
// # 它做的事
//
//  1. 只允许对**机器拒绝**（status=rejected）的单放行（其它状态 400）；
//  2. 重新组装企微 apply_event 命令并下发 ⇒ 这单进人工终审（不是直接变成"已支付"）；
//  3. 「AI审核结论」控件的**第一行** = 「已转人工审核并通过」，
//     后面跟机器当初的拒绝理由 ⇒ 审批人既知道是人放行的，也知道机器为什么拒；
//  4. 落一条新的结论（source=admin_override）+ 一条审计日志（谁放行的）。
//
// # 为什么重新提交企微而不是直接置为"通过"
//
//	「AI审核结论」这个控件**只存在于企微审批单上** ——
//	不提交企微就没有这个控件 ⇒ 需求里"第一行插入"无处可插。
//	⇒ 放行 = 让这单**重新进入人工终审**，只是结论那一行被人改过了。
//
// # 权限
//
//	scope = settlement:audit（与提交/重跑同级）**且** 角色必须是 admin。
//	⚠️ merchant（店长）持有 settlement:audit ⇒ 只守 scope 不够，
//	⇒ 端点必须**双重**守门（scope + role），否则店长能自己放行自己的单。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// AdminApprovePrefix 是「AI审核结论」控件的**首行**（2026-10-28 用户口径原文）。
const AdminApprovePrefix = "已转人工审核并通过"

// maxAdminApproveNoteLen 是「放行说明」的字数上限（企微多行文本控件 10000 字符，
// 这里按可读性限住；前端同款校验）。
const maxAdminApproveNoteLen = 300

// AdminApprove 把"机器拒绝"的审批重新推进企微人工终审。
//
// operator 是放行人（auth user id）—— 写进审计日志，不进企微文案。
//
// note 是可选的「放行说明」（2026-10-30 用户口径：管理员放行时填写，
// 回填到 AI 审核结论里，让审批人知道**为什么**被放行）。
func (s *Service) AdminApprove(ctx context.Context, branchID, approvalID, operator string, notes ...string) error {
	note := ""
	if len(notes) > 0 {
		note = strings.TrimSpace(notes[0])
	}
	if len([]rune(note)) > maxAdminApproveNoteLen {
		note = string([]rune(note)[:maxAdminApproveNoteLen])
	}
	ap, err := s.loadApproval(ctx, approvalID)
	if err != nil {
		return err
	}
	if branchID != "" && ap.BranchID != branchID {
		return ErrApprovalNotFound // 跨 branch 视为不存在(不泄露)
	}
	// ⚠️ 只允许放行**机器拒绝**的单:
	//	- received/auditing:还在跑,放行等于与流水线抢
	//	- approved/human_*:已经过了,再放行是重复提交企微审批单
	//	- failed:该走"重新审核"(Retry),不是人工放行
	if ap.Status != model.StatusRejected {
		return fmt.Errorf("%w: 仅机器拒绝(rejected)的审批可以人工放行(当前 %s)", ErrInvalidStatus, ap.Status)
	}
	// 机器当初的结论(放行后要被覆盖 ⇒ 先读出来)。
	var orig model.LLMVerdict
	origReason := ""
	if err := s.db.WithContext(ctx).First(&orig, "approval_id = ?", approvalID).Error; err == nil {
		origReason = strings.TrimSpace(orig.Reason)
	}
	if origReason == "" {
		origReason = strings.TrimSpace(ap.RejectReason)
	}
	if origReason == "" {
		origReason = "（机器未留下拒绝理由）"
	}

	// 结算单：用**快照**重建（放行时不该再查 cube —— 快照才是"当时审的那几张"）。
	settlements, err := s.settlementsFromSnapshot(ctx, approvalID)
	if err != nil {
		return err
	}
	items := s.loadApprovalItems(ctx, approvalID)
	var signals []model.AuditSignal
	if err := s.db.WithContext(ctx).Where("approval_id = ?", approvalID).
		Order("created_at ASC").Find(&signals).Error; err != nil {
		return fmt.Errorf("load signals: %w", err)
	}
	drafts := make([]signalDraft, 0, len(signals))
	for _, sg := range signals {
		drafts = append(drafts, signalDraft{
			SettlementID: sg.SettlementID,
			ProductID:    sg.ProductID,
			SignalType:   sg.SignalType,
			Level:        sg.Level,
			Message:      sg.Message,
		})
	}

	reason := fmt.Sprintf("管理员人工放行（原机器结论：%s）", truncate(origReason, 800))
	if note != "" {
		reason = fmt.Sprintf("管理员人工放行（放行说明：%s；原机器结论：%s）", note, truncate(origReason, 800))
	}
	raw, _ := json.Marshal(map[string]any{
		"conclusion": ConclusionPass,
		"reason":     reason,
		"confidence": 1,
		"admin_override": map[string]any{
			"operator":        operator,
			"original_reason": origReason,
			"prefix":          AdminApprovePrefix,
			"note":            note,
		},
	})
	verdict := &VerdictResult{
		Conclusion:         ConclusionPass,
		Reason:             reason,
		Confidence:         decimal.NewFromInt(1),
		RawJSON:            raw,
		Source:             model.VerdictSourceAdminOverride,
		AIConclusionPrefix: AdminApprovePrefix,
	}
	if err := s.saveVerdict(ctx, ap, verdict, string(raw), StepAdminApprove, len(drafts), 0); err != nil {
		return err
	}

	cmdID, err := s.publishApplyEvent(ctx, ap, verdict, drafts, settlements, items)
	if err != nil {
		return err
	}
	if err := s.db.WithContext(ctx).Model(&model.ApprovalInstance{}).
		Where("id = ?", approvalID).Updates(map[string]any{
		"status": model.StatusApproved, "last_command_id": cmdID,
		// ⚠️ 清空 reject_reason:状态已是 approved，页面再挂一条红色拒绝理由
		//    会让人以为这单还是被拒的。原理由已进结论与审计日志。
		"reject_reason":    "",
		"applicant_bearer": "",
	}).Error; err != nil {
		return fmt.Errorf("update approved failed: %w", err)
	}
	ap.Status = model.StatusApproved
	ap.LastCommandID = cmdID
	s.publishStatusEvent(ctx, &AuditStatusEvent{
		ApprovalID: ap.ID, BranchID: ap.BranchID, UserID: ap.ApplicantUserID,
		Status: ap.Status, At: s.now(),
	})
	s.logStep(ctx, ap, StepFinish, "",
		fmt.Sprintf("管理员人工放行,已提交企微审批(command_id=%s),等待人工终审", cmdID),
		"", 0)
	s.logger.Info("admin override approved",
		"approval_id", ap.ID, "branch_id", ap.BranchID, "operator", operator, "command_id", cmdID)

	// 通知发起人：机器拒单时已通知过"请核对后重新提交" ——
	// 不补这一条,发起人可能已经重新提交了一单 ⇒ 重复付款。
	// ⚠️ 失败只 warn 不阻断（放行已经生效,通知失败不该把它变成"没放行"）。
	if ap.ApplicantWecomID != "" {
		payload := NotifyTextPayload{
			ToUser:  []string{ap.ApplicantWecomID},
			Content: fmt.Sprintf("【结算付款审批】你提交的单已被管理员人工放行\n单号:%s\n请勿重复提交。", ap.ID),
		}
		if _, err := s.publishCommand(ctx, CmdNotifyText, payload); err != nil {
			s.logger.Warn("notify applicant failed", "approval_id", ap.ID, "err", err)
		}
	}
	return nil
}

// settlementsFromSnapshot 从审批快照重建 cube 结算单行。
//
// ⚠️ 放行时**不再查 cube**：快照才是"机器当时审的那几张"，
// 现在去 cube 查会拿到**今天的**金额 ⇒ 企微单上的金额与当初被拒的那单对不上。
func (s *Service) settlementsFromSnapshot(ctx context.Context, approvalID string) ([]SettlementRow, error) {
	var snaps []model.ApprovalSettlement
	if err := s.db.WithContext(ctx).Where("approval_id = ?", approvalID).
		Order("created_at ASC").Find(&snaps).Error; err != nil {
		return nil, fmt.Errorf("load approval settlements: %w", err)
	}
	out := make([]SettlementRow, 0, len(snaps))
	for _, sn := range snaps {
		out = append(out, SettlementRow{
			ID:             sn.SettlementID,
			SupplierID:     sn.SupplierID,
			SupplierName:   sn.SupplierName,
			DocumentType:   sn.DocumentType,
			TotalGoodsYuan: sn.GoodsAmountYuan,
		})
	}
	return out, nil
}
