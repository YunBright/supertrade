package service

import (
	"context"
	"errors"
	"testing"

	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// 本文件覆盖「结算单待审批中不可再选」（2026-10-29 用户口径）。
//
// ## 这个功能最容易假绿的三处
//
//  1. **只测"新的那笔被挡住"**。提交校验挡住重复提交很容易；
//     难的是**审批被拒后那张单要重新可选** —— 只测前者的话，
//     把"待审批"写成"这张单这辈子都不能选"也是绿的。
//  2. **状态集合写错**。多写一个终态（如 human_approved）⇒ 被拒的单永远回不来；
//     少写一个（如漏掉 approved）⇒ 企微审批中的单能被重复提交 ⇒ 付两次钱。
//  3. **跨门店串号**。approval_settlements 表**没有 branch_id**，
//     不 JOIN approval_instances 就挡不住别的门店的审批占用同一张单。

// seedApprovalWithSettlements 写一笔审批 + 它勾的结算单快照。
func seedApprovalWithSettlements(t *testing.T, db *gorm.DB, branchID, approvalID, status string, sids ...string) {
	t.Helper()
	ap := &model.ApprovalInstance{
		ID:               approvalID,
		CorpID:           "wcorp-test",
		BranchID:         branchID,
		ApplicantUserID:  "u-1",
		ApplicantWecomID: "wecom-u1",
		Status:           status,
	}
	if err := db.Create(ap).Error; err != nil {
		t.Fatalf("seed approval %s: %v", approvalID, err)
	}
	for _, sid := range sids {
		row := &model.ApprovalSettlement{
			ID:           approvalID + "-" + sid,
			ApprovalID:   approvalID,
			SettlementID: sid,
		}
		if err := db.Create(row).Error; err != nil {
			t.Fatalf("seed approval settlement %s: %v", sid, err)
		}
	}
}

// 状态集合是**判据**：只有"已提交、尚未有终态"的三个状态算待审批中。
func TestListPendingSettlements_StatusSet(t *testing.T) {
	svc, db, _, _, _ := newTestService(t)
	cases := []struct {
		status  string
		pending bool
	}{
		{model.StatusReceived, true},       // 刚提交，流水线还没起来
		{model.StatusAuditing, true},       // 机器审核在跑
		{model.StatusApproved, true},       // 机器通过，企微待终审
		{model.StatusRejected, false},      // 机器拒绝 ⇒ 恢复可选
		{model.StatusFailed, false},        // 流水线异常 ⇒ 恢复可选
		{model.StatusHumanApproved, false}, // 企微通过 ⇒ 已付款，由台账接管
		{model.StatusHumanRejected, false}, // 企微驳回 ⇒ 恢复可选
		{model.StatusCancelled, false},     // 撤销 ⇒ 恢复可选
	}
	for _, c := range cases {
		sid := "S-" + c.status
		seedApprovalWithSettlements(t, db, "B1", "AP-"+c.status, c.status, sid)
		list, err := svc.ListPendingSettlements(context.Background(), "B1", []string{sid})
		if err != nil {
			t.Fatalf("list %s: %v", c.status, err)
		}
		got := len(list) == 1
		if got != c.pending {
			t.Errorf("状态 %q 待审批中 = %v, want %v", c.status, got, c.pending)
		}
	}
}

// 别的门店的审批不能占用本门店的结算单。
func TestListPendingSettlements_BranchIsolation(t *testing.T) {
	svc, db, _, _, _ := newTestService(t)
	seedApprovalWithSettlements(t, db, "B2", "AP-OTHER", model.StatusApproved, "S1")

	list, err := svc.ListPendingSettlements(context.Background(), "B1", []string{"S1"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("B2 的审批不该让 B1 的 S1 变成待审批中,实际 %+v", list)
	}
}

// 候选列表默认剔除「待审批中」的单,并回传剔除张数。
func TestListCubeSettlements_HidesPendingByDefault(t *testing.T) {
	svc, db, cube, _, _ := newTestService(t)
	seedSettlementRows(t, cube,
		row("DP1", "S1", "DP", "100.00"),
		row("DP2", "S2", "DP", "200.00"),
	)
	seedApprovalWithSettlements(t, db, "B1", "AP-1", model.StatusApproved, "DP2")

	list, err := svc.ListCubeSettlements(context.Background(), "B1", "", "", "", "", 200, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Rows) != 1 || list.Rows[0].ID != "DP1" {
		t.Fatalf("待审批中的 DP2 不该出现,实际 %+v", list.Rows)
	}
	if list.HiddenPending != 1 {
		t.Fatalf("hidden_pending 应为 1(前端要据此说明「列表少了 N 张」),实际 %d", list.HiddenPending)
	}
	if list.HiddenPaid != 0 {
		t.Fatalf("hidden_paid 应为 0,实际 %d", list.HiddenPaid)
	}
}

// 审批被拒 ⇒ 那张单**恢复可选**（2026-10-29 用户口径原文）。
func TestListCubeSettlements_RejectedBecomesSelectableAgain(t *testing.T) {
	svc, db, cube, _, _ := newTestService(t)
	seedSettlementRows(t, cube, row("DP1", "S1", "DP", "100.00"))
	seedApprovalWithSettlements(t, db, "B1", "AP-1", model.StatusRejected, "DP1")

	list, err := svc.ListCubeSettlements(context.Background(), "B1", "", "", "", "", 200, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Rows) != 1 {
		t.Fatalf("被拒的 DP1 应恢复可勾选,实际 %+v", list.Rows)
	}
	if list.HiddenPending != 0 {
		t.Fatalf("被拒的单不该计入待审批隐藏数,实际 %d", list.HiddenPending)
	}
}

// 提交时后端必须校验（不能只靠前端过滤）。
func TestSubmitApproval_RejectsPendingSettlement(t *testing.T) {
	svc, db, _, _, _ := newTestService(t)
	seedApprovalWithSettlements(t, db, "B1", "AP-1", model.StatusApproved, "DP1")

	_, err := svc.SubmitApproval(context.Background(), "B1", validSubmitInput("DP1"))
	if !errors.Is(err, ErrSettlementPending) {
		t.Fatalf("err = %v, want ErrSettlementPending", err)
	}
}

// 审批被拒后，同样的单可以再次提交。
func TestSubmitApproval_AllowsAfterRejection(t *testing.T) {
	svc, db, _, _, _ := newTestService(t)
	seedApprovalWithSettlements(t, db, "B1", "AP-1", model.StatusRejected, "DP1")

	if _, err := svc.SubmitApproval(context.Background(), "B1", validSubmitInput("DP1")); err != nil {
		t.Fatalf("被拒的单应能重新提交,实际 %v", err)
	}
}

// 别的门店的审批不挡本门店提交。
func TestSubmitApproval_OtherBranchDoesNotBlock(t *testing.T) {
	svc, db, _, _, _ := newTestService(t)
	seedApprovalWithSettlements(t, db, "B2", "AP-1", model.StatusApproved, "DP1")

	if _, err := svc.SubmitApproval(context.Background(), "B1", validSubmitInput("DP1")); err != nil {
		t.Fatalf("别的门店的审批不该挡住本门店提交,实际 %v", err)
	}
}

// validSubmitInput 造一份合法的提交入参。
func validSubmitInput(sids ...string) SubmitInput {
	return SubmitInput{
		ApplicantUserID:  "u-1",
		ApplicantWecomID: "wecom-u1",
		ApplicantBearer:  "Bearer test-token",
		SettlementIDs:    sids,
		ManualAmountYuan: dec("100"),
		Remark:           "9月货款",
		FlowSelections: FlowSelections{
			FlowNodeApprover: []string{"apr1"},
			FlowNodeHandler:  []string{"handler1"},
			FlowNodeCC:       []string{"cc1"},
		},
	}
}

// 纯函数：剔除待审批中的单。
func TestFilterOutPendingSettlements(t *testing.T) {
	rows := []SettlementRow{row("DP1", "S1", "DP", "1"), row("DP2", "S1", "DP", "2"), row("DP3", "S1", "DP", "3")}
	out, hidden := FilterOutPendingSettlements(rows, map[string]bool{"DP2": true})
	if len(out) != 2 || hidden != 1 {
		t.Fatalf("应剔除 1 张留 2 张,实际 %d/%d", len(out), hidden)
	}
	out, hidden = FilterOutPendingSettlements(rows, nil)
	if len(out) != 3 || hidden != 0 {
		t.Fatalf("没有待审批时应原样返回,实际 %d/%d", len(out), hidden)
	}
}
