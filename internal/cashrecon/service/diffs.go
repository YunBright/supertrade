// Package service / diffs.go —— 差异工作台：列表、详情、人工动作（FR-10/FR-11）。
//
// # 差异没有 DELETE
//
//	recon_diffs 不提供删除端点；状态只前进不后退，人工动作全部落
//	recon_audit_logs（append-only）。这是审计链的第一条红线。
//
// # 原因必填
//
//	mark / hang / escalate 都必须写原因：没有原因的"已处理"等于没处理，
//	半年后没人能解释这笔差异当时为什么被放过。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cashrecon/model"
)

// 差异人工动作。
const (
	DiffActionMatch    = "match"
	DiffActionReason   = "reason"
	DiffActionHang     = "hang"
	DiffActionEscalate = "escalate"
)

// diffActionStatus 把动作映射成目标状态与默认归因码。
var diffActionStatus = map[string]struct {
	Status string
	Code   string
}{
	DiffActionMatch:    {Status: model.DiffStatusHandled, Code: "manual_match"},
	DiffActionReason:   {Status: model.DiffStatusReasoned, Code: "reasonable_diff"},
	DiffActionHang:     {Status: model.DiffStatusHung, Code: "pending_investigation"},
	DiffActionEscalate: {Status: model.DiffStatusEscalated, Code: "escalated"},
}

// validDiffStatus 判断差异状态是否合法（查询参数校验用）。
func validDiffStatus(s string) bool {
	switch s {
	case model.DiffStatusOpen, model.DiffStatusHandled, model.DiffStatusReasoned,
		model.DiffStatusHung, model.DiffStatusEscalated:
		return true
	}
	return false
}

// validDiffKind 判断差异类型是否合法。
func validDiffKind(s string) bool {
	switch s {
	case model.DiffKindAmountDiff, model.DiffKindSingleCash, model.DiffKindSingleBank,
		model.DiffKindDuplicate, model.DiffKindFeeDiff, model.DiffKindUnmappedPayWay,
		model.DiffKindTimeLag:
		return true
	}
	return false
}

// DiffQuery 是差异列表的筛选条件。
type DiffQuery struct {
	BranchID string
	BatchID  string
	Status   string
	Kind     string
	Limit    int
	Offset   int
}

// DiffList 是差异列表结果。
type DiffList struct {
	Items []model.ReconDiff `json:"items"`
	Total int64             `json:"total"`
}

// ListDiffs 返回差异列表（门店内，可按批次/状态/类型过滤）。
func (s *Service) ListDiffs(ctx context.Context, q DiffQuery) (DiffList, error) {
	q.BranchID = strings.TrimSpace(q.BranchID)
	q.BatchID = strings.TrimSpace(q.BatchID)
	q.Status = strings.TrimSpace(q.Status)
	q.Kind = strings.TrimSpace(q.Kind)
	if q.BranchID == "" {
		return DiffList{}, model.ErrInvalidInput
	}
	if q.Status != "" && !validDiffStatus(q.Status) {
		return DiffList{}, fmt.Errorf("%w: 未知差异状态 %q", model.ErrInvalidInput, q.Status)
	}
	if q.Kind != "" && !validDiffKind(q.Kind) {
		return DiffList{}, fmt.Errorf("%w: 未知差异类型 %q", model.ErrInvalidInput, q.Kind)
	}
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if q.Limit > 200 {
		q.Limit = 200
	}
	if q.Offset < 0 {
		q.Offset = 0
	}

	db := s.db.WithContext(ctx).Model(&model.ReconDiff{}).Where("branch_id = ?", q.BranchID)
	if q.BatchID != "" {
		db = db.Where("batch_id = ?", q.BatchID)
	}
	if q.Status != "" {
		db = db.Where("status = ?", q.Status)
	}
	if q.Kind != "" {
		db = db.Where("kind = ?", q.Kind)
	}

	var total int64
	if err := db.Count(&total).Error; err != nil {
		return DiffList{}, err
	}
	items := make([]model.ReconDiff, 0, q.Limit)
	if err := db.Order("created_at DESC, id").Limit(q.Limit).Offset(q.Offset).
		Find(&items).Error; err != nil {
		return DiffList{}, err
	}
	return DiffList{Items: items, Total: total}, nil
}

// DiffDetail 是差异详情（含全部审计轨迹，支撑"按流水号反查操作历史"）。
type DiffDetail struct {
	Diff      model.ReconDiff       `json:"diff"`
	AuditLogs []model.ReconAuditLog `json:"audit_logs"`
}

// GetDiff 取差异详情 + 审计轨迹。
func (s *Service) GetDiff(ctx context.Context, branchID, id string) (DiffDetail, error) {
	branchID = strings.TrimSpace(branchID)
	id = strings.TrimSpace(id)
	if branchID == "" || id == "" {
		return DiffDetail{}, model.ErrInvalidInput
	}
	var row model.ReconDiff
	err := s.db.WithContext(ctx).Where("branch_id = ? AND id = ?", branchID, id).First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return DiffDetail{}, model.ErrNotFound
	}
	if err != nil {
		return DiffDetail{}, err
	}
	var logs []model.ReconAuditLog
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND object_type = ? AND object_id = ?", branchID, "recon_diff", id).
		Order("created_at, id").
		Find(&logs).Error; err != nil {
		return DiffDetail{}, err
	}
	if logs == nil {
		logs = []model.ReconAuditLog{}
	}
	return DiffDetail{Diff: row, AuditLogs: logs}, nil
}

// ActDiffInput 是差异人工动作的入参。
type ActDiffInput struct {
	BranchID       string
	DiffID         string
	Action         string
	Reason         string
	ResolutionCode string
	Actor          string
}

// ActDiff 执行差异人工动作（match/reason/hang/escalate），写审计。
//
// 所有动作都要求原因；已处理过的差异允许再处理（例如挂起后查清可以标合理），
// 但每一次动作都会追加一条审计，不覆盖历史。
func (s *Service) ActDiff(ctx context.Context, in ActDiffInput) (model.ReconDiff, error) {
	in.BranchID = strings.TrimSpace(in.BranchID)
	in.DiffID = strings.TrimSpace(in.DiffID)
	in.Action = strings.TrimSpace(in.Action)
	in.Reason = strings.TrimSpace(in.Reason)
	in.ResolutionCode = strings.TrimSpace(in.ResolutionCode)
	in.Actor = strings.TrimSpace(in.Actor)
	if in.BranchID == "" || in.DiffID == "" || in.Actor == "" || in.Reason == "" {
		return model.ReconDiff{}, fmt.Errorf("%w: 人工动作必须填写原因", model.ErrInvalidInput)
	}
	mapping, ok := diffActionStatus[in.Action]
	if !ok {
		return model.ReconDiff{}, fmt.Errorf("%w: 未知动作 %q", model.ErrInvalidInput, in.Action)
	}
	if len(in.Reason) > 512 {
		return model.ReconDiff{}, fmt.Errorf("%w: 原因过长", model.ErrInvalidInput)
	}
	if in.ResolutionCode == "" {
		in.ResolutionCode = mapping.Code
	}
	if len(in.ResolutionCode) > 32 {
		return model.ReconDiff{}, fmt.Errorf("%w: 归因码过长", model.ErrInvalidInput)
	}

	now := s.now()
	var out model.ReconDiff
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row model.ReconDiff
		err := tx.WithContext(ctx).Where("branch_id = ? AND id = ?", in.BranchID, in.DiffID).First(&row).Error
		if err == gorm.ErrRecordNotFound {
			return model.ErrNotFound
		}
		if err != nil {
			return err
		}
		err = tx.WithContext(ctx).Model(&model.ReconDiff{}).
			Where("branch_id = ? AND id = ?", in.BranchID, in.DiffID).
			Updates(map[string]any{
				"status":          mapping.Status,
				"resolution_code": in.ResolutionCode,
				"resolution_note": in.Reason,
				"handled_by":      in.Actor,
				"handled_at":      now,
				"updated_at":      now,
			}).Error
		if err != nil {
			return err
		}
		before, _ := json.Marshal(map[string]string{
			"status":          row.Status,
			"resolution_code": row.ResolutionCode,
			"resolution_note": row.ResolutionNote,
		})
		after, _ := json.Marshal(map[string]string{
			"status":          mapping.Status,
			"resolution_code": in.ResolutionCode,
			"resolution_note": in.Reason,
		})
		if err := tx.WithContext(ctx).Create(&model.ReconAuditLog{
			ID:         newID(),
			BranchID:   in.BranchID,
			ObjectType: "recon_diff",
			ObjectID:   row.ID,
			Action:     in.Action,
			BeforeJSON: string(before),
			AfterJSON:  string(after),
			Reason:     in.Reason,
			Actor:      in.Actor,
			CreatedAt:  now,
		}).Error; err != nil {
			return err
		}
		out = row
		out.Status = mapping.Status
		out.ResolutionCode = in.ResolutionCode
		out.ResolutionNote = in.Reason
		out.HandledBy = in.Actor
		out.HandledAt = &now
		out.UpdatedAt = now
		return nil
	})
	if err != nil {
		return model.ReconDiff{}, err
	}
	return out, nil
}
