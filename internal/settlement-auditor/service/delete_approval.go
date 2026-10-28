package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
	"gorm.io/gorm"
)

// ErrApprovalInFlight —— 审批流水线仍在运行时拒绝删除（映射 409）。
//
// 为什么不"等它跑完再删"，而是直接拒绝：
//
//	调用方（契约测试清理）拿到的 409 带着"稍后重试"的明确指引，
//	比让 HTTP 请求挂住等流水线更诚实 —— 后者会占着连接直到超时，
//	而流水线时长取决于 LLM 调用，谁也说不准要多久。
//
//	拒删还有个更实际的好处：流水线跑一半时把主表删掉，
//	它后面写 audit_signals / llm_verdicts 时会撞上"审批实例不存在"，
//	于是一次本该成功的审核变成 failed，还会留下孤儿行 ——
//	即"清理反而制造了新的脏数据"。
var ErrApprovalInFlight = errors.New("service: 审批流水线仍在运行,稍后重试")

// terminalStatuses 是可安全删除的终态集合。
//
// 注意 approved **不在**这里：它是"机器一审通过、企微申请已提交/提交中"，
// 后面还可能转 human_approved / human_rejected / cancelled ——
// 企微回调仍可能回来并写入。对它删表，等于让一次在途的外部回调写进虚空。
//
// 只有 received / auditing 是"本服务正在写"，那才是真正需要拒绝的。
var terminalStatuses = map[string]bool{
	model.StatusRejected:      true,
	model.StatusFailed:        true,
	model.StatusHumanApproved: true,
	model.StatusHumanRejected: true,
	model.StatusCancelled:     true,
}

// DeleteApproval 物理删除一次审批实例及其 5 张从表（数据治理 / 测试清理用）。
//
// ## 为什么需要它
//
// 契约测试 `deployer/tests/contract/settlement_auditor_test.go` 会真的提交一次
// 付款审批（POST 返 202，随后跑异步流水线），一次写入 6 张表。而本服务此前
// **一个 DELETE 路由都没有**，于是每次全量测试都往生产留一整套审批残留。
//
// ## 级联范围：主表 + 5 张从表
//
//	approval_instances          主表
//	approval_settlements        审批 ↔ 结算单快照
//	audit_signals               核对信号
//	audit_logs                  推理分步日志
//	llm_verdicts                LLM 结论（一审批一条）
//	approval_stocktake_links    审批 ↔ 盘点计划绑定
//
// **approval_approver_preferences 不在范围内**：它的主键是
// `<branchID>:<applicantUserID>`，记的是"这个人上次选了谁当审批人"，
// 与某一次审批无关。它是按人的偏好缓存，删审批不该动它。
//
// 这些表的 approval_id 列都没有外键约束（AutoMigrate 建表，字段只是
// 带 index 的 string），所以级联必须显式写。
//
// ## 幂等性
//
// 实例不存在 → ErrApprovalNotFound。清理侧必须把"已经不在了"和"删掉了"
// 视作同一件事 —— 重复调用会拿到 404，这是刻意的：让"参数写错 id"
// 不会返回成功。
func (s *Service) DeleteApproval(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("%w: id 不能为空", ErrInvalidInput)
	}

	var inst model.ApprovalInstance
	if err := s.db.WithContext(ctx).First(&inst, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrApprovalNotFound
		}
		return err
	}

	// 流水线在途 → 拒删（见 ErrApprovalInFlight 的长注释）。
	if !terminalStatuses[inst.Status] {
		return fmt.Errorf("%w: 当前状态 %q 仍在处理中", ErrApprovalInFlight, inst.Status)
	}

	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, child := range []any{
			&model.ApprovalSettlement{},
			&model.AuditSignal{},
			&model.AuditLog{},
			&model.LLMVerdict{},
			&model.ApprovalStocktakeLink{},
			// 收款账号快照(2026-10-28):独立表 ⇒ 删审批时要一起删,
			// 否则留下一行指向已不存在的审批的孤儿快照。
			&model.ApprovalPayeeAccount{},
		} {
			if err := tx.Delete(child, "approval_id = ?", id).Error; err != nil {
				return fmt.Errorf("delete children of approval %s: %w", id, err)
			}
		}
		if err := tx.Delete(&inst).Error; err != nil {
			return fmt.Errorf("delete approval %s: %w", id, err)
		}
		return nil
	})
}
