// Package service / subscribe.go —— 订阅事件的业务处理(wechat-bot 回调 + auth 失效)。
//
//   - wecom.approval.status_changed:企微审批状态回调(按 sp_no 找实例,映射终态);
//   - wecom.approval.command_result:命令执行结果(按 last_command_id 关联,
//     apply_event ok → 回填 sp_no;error → 实例 failed 可重跑);
//   - auth.user.access_changed:scope 缓存失效(handler 直调 InvalidateScopeCache)。
package service

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// OnStatusChanged 处理企微审批状态回调。
//
// 按 sp_no 找实例(找不到 → 返回 nil 并 log,非本服务提交的审批单忽略);
// sp_status 映射:2→human_approved,3→human_rejected,4/6→cancelled,其余仅更新 sp_status。
func (s *Service) OnStatusChanged(ctx context.Context, ev *WecomStatusChanged) error {
	if ev == nil || ev.SpNo == "" {
		return fmt.Errorf("%w: sp_no 必填", ErrInvalidInput)
	}
	var ap model.ApprovalInstance
	if err := s.db.WithContext(ctx).First(&ap, "sp_no = ?", ev.SpNo).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			s.logger.Info("status_changed: sp_no 无实例(非本服务提交,忽略)", "sp_no", ev.SpNo)
			return nil
		}
		return err
	}

	newStatus := ap.Status
	switch ev.SpStatus {
	case SpStatusApproved:
		newStatus = model.StatusHumanApproved
	case SpStatusRejected:
		newStatus = model.StatusHumanRejected
	case SpStatusCancelled, SpStatusCancelAfterOK:
		newStatus = model.StatusCancelled
	}
	updates := map[string]any{"sp_status": ev.SpStatus}
	if newStatus != ap.Status {
		updates["status"] = newStatus
	}
	if err := s.db.WithContext(ctx).Model(&model.ApprovalInstance{}).
		Where("id = ?", ap.ID).Updates(updates).Error; err != nil {
		return err
	}
	ap.SpStatus = ev.SpStatus
	ap.Status = newStatus
	s.publishStatusEvent(ctx, &AuditStatusEvent{
		ApprovalID: ap.ID, BranchID: ap.BranchID, UserID: ap.ApplicantUserID,
		Status: ap.Status, SpNo: ap.SpNo, SpStatus: ap.SpStatus, At: s.now(),
	})
	s.logger.Info("status_changed applied",
		"approval_id", ap.ID, "sp_no", ev.SpNo, "sp_status", ev.SpStatus, "status", ap.Status)
	return nil
}

// OnCommandResult 处理 wechat-bot 命令执行结果。
//
// 按 command_id = last_command_id 关联实例(找不到 → 忽略);
// apply_event: ok → 回填 sp_no + sp_status=审批中;error → 实例 failed(可 RetryApproval)。
// notify_text / stocktake_taskcard 失败仅 log(通知类命令不改审批状态)。
func (s *Service) OnCommandResult(ctx context.Context, res *WecomCommandResult) error {
	if res == nil || res.CommandID == "" {
		return fmt.Errorf("%w: command_id 必填", ErrInvalidInput)
	}
	var ap model.ApprovalInstance
	if err := s.db.WithContext(ctx).First(&ap, "last_command_id = ?", res.CommandID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			s.logger.Info("command_result: command_id 无实例(忽略)",
				"command_id", res.CommandID, "type", res.Type)
			return nil
		}
		return err
	}

	if res.Type != CmdApplyEvent {
		if !res.OK {
			s.logger.Warn("command_result: 通知类命令失败",
				"approval_id", ap.ID, "command_id", res.CommandID, "type", res.Type, "err", res.Error)
		}
		return nil
	}

	if !res.OK {
		msg := fmt.Sprintf("企微审批提交失败: %s", res.Error)
		if err := s.db.WithContext(ctx).Model(&model.ApprovalInstance{}).
			Where("id = ?", ap.ID).Updates(map[string]any{
			"status": model.StatusFailed, "error": truncate(msg, 1024),
		}).Error; err != nil {
			return err
		}
		ap.Status = model.StatusFailed
		s.publishStatusEvent(ctx, &AuditStatusEvent{
			ApprovalID: ap.ID, BranchID: ap.BranchID, UserID: ap.ApplicantUserID,
			Status: ap.Status, At: s.now(),
		})
		s.logger.Error("command_result: apply_event failed",
			"approval_id", ap.ID, "command_id", res.CommandID, "err", res.Error)
		return nil
	}

	// ok → 回填 sp_no,企微侧进入审批中
	if err := s.db.WithContext(ctx).Model(&model.ApprovalInstance{}).
		Where("id = ?", ap.ID).Updates(map[string]any{
		"sp_no": res.SpNo, "sp_status": SpStatusApproving,
	}).Error; err != nil {
		return err
	}
	ap.SpNo = res.SpNo
	ap.SpStatus = SpStatusApproving
	s.publishStatusEvent(ctx, &AuditStatusEvent{
		ApprovalID: ap.ID, BranchID: ap.BranchID, UserID: ap.ApplicantUserID,
		Status: ap.Status, SpNo: ap.SpNo, SpStatus: ap.SpStatus, At: s.now(),
	})
	s.logger.Info("command_result: apply_event ok",
		"approval_id", ap.ID, "command_id", res.CommandID, "sp_no", res.SpNo)
	return nil
}
