// Package service / events.go —— 会员域事件(经 pkg/outbox 发布)。
//
// # 铁律:事件与业务行**同事务**
//
//	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
//	    if err := svc.Create(tx, ...); err != nil { return err }
//	    return outbox.Recorder{}.Enqueue(tx, outbox.Event{...})  // ← 必须传 tx
//	})
//
// ⇒ 进程在 commit 与 publish 之间被杀,事件**也不会丢**
//
//	(发件箱行已经随业务行一起提交了)。
//
// ⛔ **绝不在事务外 publish**(那是 internal/stocktake/service/events.go 的反面教材)。
//
// # topic 命名(EVENT-CATALOG §0.4:`<domain>.<verb>`)
//
//	member.opened         开卡
//	member.points.changed 积分变动
//	member.updated        字段变更
//
// # 幂等键
//
// 发件箱行的 id **就是** op_id(业务幂等键)—— 不是另生成的一个 id:
// 重发时订阅方看到的必须是**同一个**去重键。
//
// ⚠️ 推论:**一个 op_id 对应一个事件**。
//
//	同一个 op_id 用在两种不同操作上 ⇒ 第二个事件撞发件箱主键
//	(这是调用方的 bug:op_id 标识"一次操作",一次操作只产生一个事件)。
package service

import (
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/member/model"
	"github.com/YunBright/supertrade/pkg/outbox"
)

// 会员域发布的 topic。
const (
	// TopicMemberOpened 是开卡事件。
	TopicMemberOpened = "member.opened"
	// TopicMemberPointsChanged 是积分变动事件。
	TopicMemberPointsChanged = "member.points.changed"
	// TopicMemberUpdated 是会员字段变更事件。
	TopicMemberUpdated = "member.updated"
)

// MemberOpenedPayload 是 member.opened 的 payload(方案 §6.5)。
type MemberOpenedPayload struct {
	MemberID   string `json:"member_id"`
	BranchID   string `json:"branch_id"`
	CardTypeID string `json:"card_type_id"`
	Name       string `json:"name"`
	OpenedAt   string `json:"opened_at"`
}

// MemberPointsChangedPayload 是 member.points.changed 的 payload(方案 §6.5)。
type MemberPointsChangedPayload struct {
	MemberID    string `json:"member_id"`
	BranchID    string `json:"branch_id"`
	Direction   string `json:"direction"`
	Points      string `json:"points"`
	PointsAfter string `json:"points_after"`
	Reason      string `json:"reason"`
	OperatorID  string `json:"operator_id"`
	// AuthorizedBy 是越权授权人;空 = 操作人自己有权限。
	AuthorizedBy string `json:"authorized_by"`
}

// MemberUpdatedPayload 是 member.updated 的 payload(方案 §6.5)。
type MemberUpdatedPayload struct {
	MemberID string `json:"member_id"`
	BranchID string `json:"branch_id"`
	// ChangedFields 是**字段名**列表(⛔ 不含字段值 —— 值不该进事件流)。
	ChangedFields []string `json:"changed_fields"`
}

// enqueueMemberOpened 入队 member.opened。
func (s *Service) enqueueMemberOpened(tx *gorm.DB, m *model.Member, now time.Time) error {
	return s.recorder.Enqueue(tx, outbox.Event{
		ID:       m.OpID,
		BranchID: m.BranchID,
		Topic:    TopicMemberOpened,
		Subject:  "member/" + m.ID,
		Payload: MemberOpenedPayload{
			MemberID:   m.ID,
			BranchID:   m.BranchID,
			CardTypeID: m.CardTypeID,
			Name:       m.Name,
			OpenedAt:   now.Format(time.RFC3339),
		},
	})
}

// enqueuePointsChanged 入队 member.points.changed。
func (s *Service) enqueuePointsChanged(tx *gorm.DB, row *model.MemberPointLedger) error {
	return s.recorder.Enqueue(tx, outbox.Event{
		ID:       row.OpID,
		BranchID: row.BranchID,
		Topic:    TopicMemberPointsChanged,
		Subject:  "member/" + row.MemberID,
		Payload: MemberPointsChangedPayload{
			MemberID:     row.MemberID,
			BranchID:     row.BranchID,
			Direction:    row.Direction,
			Points:       row.Points.String(),
			PointsAfter:  row.PointsAfter.String(),
			Reason:       row.Reason,
			OperatorID:   row.OperatorID,
			AuthorizedBy: row.AuthorizedBy,
		},
	})
}

// enqueueMemberUpdated 入队 member.updated。
//
// ⚠️ 发件箱主键 = 业务 op_id(与 pkg/outbox 的约定一致),
//
//	于是"这个 op_id 已经生效过"可以直接用发件箱行判定(见 opApplied)。
func (s *Service) enqueueMemberUpdated(tx *gorm.DB, opID, memberID, branchID string, changed []string) error {
	return s.recorder.Enqueue(tx, outbox.Event{
		ID:       opID,
		BranchID: branchID,
		Topic:    TopicMemberUpdated,
		Subject:  "member/" + memberID,
		Payload: MemberUpdatedPayload{
			MemberID:      memberID,
			BranchID:      branchID,
			ChangedFields: changed,
		},
	})
}

// opApplied 判断某个 op_id 是否已经应用过(以发件箱行为准)。
//
// ⚠️ 发件箱行与业务行**同事务**写入 ⇒ 它天然就是"这个 op_id 已经生效过"的凭证。
//
//	用它做幂等判据,不需要额外加一张幂等表。
func (s *Service) opApplied(tx *gorm.DB, opID string) (bool, error) {
	var n int64
	if err := tx.Model(&outbox.Outbox{}).Where("id = ?", s.recorder.RowID(opID)).Count(&n).Error; err != nil {
		return false, fmt.Errorf("member: 检查 op_id 是否已应用失败: %w", err)
	}
	return n > 0, nil
}
