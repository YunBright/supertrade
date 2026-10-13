package service

import (
	"context"
	"errors"
	"testing"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
	"gorm.io/gorm"
)

// 本文件覆盖 DeleteApproval（2026-10-13 新增）。
//
// ## 这个端点最容易被省掉的两个东西
//
//  1. **流水线在途时必须拒删。** POST /settlement-approvals 返 202 后，
//     后台流水线还在往 audit_signals / llm_verdicts 写。此时删主表，
//     流水线下一次写入会撞上"审批实例不存在" → 一次本该成功的审核变成 failed,
//     **清理反而制造了新的脏数据**。
//  2. **approved 不是终态。** 它是"机器一审通过、企微申请已提交/提交中",
//     后面还会转 human_approved / human_rejected / cancelled。
//     把它当终态放行删除，等于让在途的企微回调写进虚空。

// seedApprovalInStatus 造一条指定状态的审批实例 + 每张从表各一行。
func seedApprovalInStatus(t *testing.T, db *gorm.DB, id, status string) {
	t.Helper()
	now := fixedNow
	inst := &model.ApprovalInstance{
		ID: id, CorpID: "wcorp-test", BranchID: "B1",
		ApplicantUserID: "u1", Remark: "contract-test", Status: status,
		SpStatus: 0, ApproverUserIDs: []byte("[]"), CcUserIDs: []byte("[]"),
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(inst).Error; err != nil {
		t.Fatalf("seed instance: %v", err)
	}
	rows := []any{
		&model.ApprovalSettlement{
			ID: id + "-S1", ApprovalID: id, SettlementID: "DP1", SupplierID: "sup1",
			DocumentType: "DP", GoodsAmountYuan: dec("100.00"), Checked: true, CreatedAt: now,
		},
		&model.AuditSignal{
			ID: id + "-G1", ApprovalID: id, SettlementID: "DP1", ProductID: "P1",
			SignalType: "amount_mismatch", Level: "warn", Message: "m",
			Evidence: []byte("{}"), CreatedAt: now,
		},
		&model.AuditLog{
			ID: id + "-L1", ApprovalID: id, Step: "step1",
			InputSummary: "in", OutputSummary: "out", DurationMs: 12, CreatedAt: now,
		},
		&model.LLMVerdict{
			ID: id + "-V1", ApprovalID: id, Conclusion: "pass", Reason: "ok",
			Confidence: dec("0.900"), RawJSON: []byte("{}"),
			Source: model.VerdictSourceRuleFallback, CreatedAt: now,
		},
		&model.ApprovalStocktakeLink{
			ID: id + "-K1", ApprovalID: id, StocktakeHeaderID: "ST-1", CreatedAt: now,
		},
	}
	for _, r := range rows {
		if err := db.Create(r).Error; err != nil {
			t.Fatalf("seed child: %v", err)
		}
	}
}

func countChildren(t *testing.T, db *gorm.DB, approvalID string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for name, m := range map[string]any{
		"approval_settlements":     &model.ApprovalSettlement{},
		"audit_signals":            &model.AuditSignal{},
		"audit_logs":               &model.AuditLog{},
		"llm_verdicts":             &model.LLMVerdict{},
		"approval_stocktake_links": &model.ApprovalStocktakeLink{},
	} {
		var n int64
		if err := db.Model(m).Where("approval_id = ?", approvalID).Count(&n).Error; err != nil {
			t.Fatalf("count %s: %v", name, err)
		}
		out[name] = n
	}
	return out
}

// TestDeleteApproval_RemovesAllChildren —— 级联 5 张从表。
//
// 少删任何一张，这条用例红。approval_id 列没有外键约束，
// 少删不会报错，只会静默留下孤儿行。
func TestDeleteApproval_RemovesAllChildren(t *testing.T) {
	svc, db, _, _, _ := newTestService(t)
	const id = "AP-DEL-1"
	seedApprovalInStatus(t, db, id, model.StatusRejected)

	if err := svc.DeleteApproval(context.Background(), id); err != nil {
		t.Fatalf("DeleteApproval: %v", err)
	}
	for name, n := range countChildren(t, db, id) {
		if n != 0 {
			t.Errorf("%s 残留 %d 行 —— approval_id 无外键,必须显式删", name, n)
		}
	}
	var inst model.ApprovalInstance
	if err := db.First(&inst, "id = ?", id).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Errorf("主表行应被删除,实际仍可查到: %v", err)
	}
}

// TestDeleteApproval_RejectsInFlight —— 流水线在途必须拒删。
//
// 判据:返 409 让调用方稍后重试,而不是"等它跑完再删"。
// 让 HTTP 请求挂住等 LLM 调用既占连接又时长不可控;
// 而中途删主表会让流水线把一次成功的审核变成 failed ——
// 那等于**清理制造了新的脏数据**,比不清理更糟。
func TestDeleteApproval_RejectsInFlight(t *testing.T) {
	for _, status := range []string{model.StatusReceived, model.StatusAuditing} {
		t.Run(status, func(t *testing.T) {
			svc, db, _, _, _ := newTestService(t)
			id := "AP-INFlight-" + status
			seedApprovalInStatus(t, db, id, status)

			err := svc.DeleteApproval(context.Background(), id)
			if !errors.Is(err, ErrApprovalInFlight) {
				t.Fatalf("状态 %q 应返 ErrApprovalInFlight,实际 %v", status, err)
			}
			for name, n := range countChildren(t, db, id) {
				if n == 0 {
					t.Errorf("被拒绝的删除不该动 %s", name)
				}
			}
		})
	}
}

// TestDeleteApproval_RejectsApproved —— approved 不是终态。
//
// 它是"机器一审通过、企微申请已提交/提交中",后面还会转
// human_approved / human_rejected / cancelled。放行删除等于让
// 在途的企微回调写进虚空。
func TestDeleteApproval_RejectsApproved(t *testing.T) {
	svc, db, _, _, _ := newTestService(t)
	const id = "AP-APPROVED"
	seedApprovalInStatus(t, db, id, model.StatusApproved)

	if err := svc.DeleteApproval(context.Background(), id); !errors.Is(err, ErrApprovalInFlight) {
		t.Fatalf("approved 单应拒删,实际 %v", err)
	}
}

func TestDeleteApproval_NotFound(t *testing.T) {
	svc, _, _, _, _ := newTestService(t)
	if err := svc.DeleteApproval(context.Background(), "AP-NOPE"); !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("不存在的实例应返 ErrApprovalNotFound,实际 %v", err)
	}
}

func TestDeleteApproval_EmptyID(t *testing.T) {
	svc, _, _, _, _ := newTestService(t)
	if err := svc.DeleteApproval(context.Background(), ""); err == nil {
		t.Fatal("空 id 必须报错")
	}
}
