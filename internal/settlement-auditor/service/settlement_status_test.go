package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// 本文件覆盖「结算单状态诊断」与「列表关键字搜索」（2026-10-29 用户口径）。
//
// ## 这组功能最容易假绿的三处
//
//  1. **状态判据只看一处**。台账（已付款）与审批（待审批中）是**两套事实源**，
//     只查一套 ⇒ "已付款但审批还没落终态"这类单会被判成可付款。
//  2. **"查不到"被当成"不存在"**。源库里没这单 / 单没审核 / 不是 DP·CP 单
//     是三种不同的事，混成一种 ⇒ 店长以为自己输错了单号。
//  3. **搜索只是把前端过滤搬到后端**。如果后端仍然按 limit 截断后再匹配，
//     老单照样搜不到（生产实测：那 4 张单排名 256/210/205/200，全在 200 名外）。

// ---- 状态判定（纯函数）----

// 状态判定矩阵：每种事实组合 ⇒ 一个确定的状态。
func TestBuildSettlementStatus(t *testing.T) {
	approved := SettlementRow{ID: "S1", DocumentType: "CP", ApproveStatus: "1"}
	notApproved := SettlementRow{ID: "S2", DocumentType: "CP", ApproveStatus: "0"}
	otherType := SettlementRow{ID: "S3", DocumentType: "XX", ApproveStatus: "1"}

	cases := []struct {
		name  string
		src   SettlementRow
		state string
		has   bool
		paid  model.SettlementPayState
		appr  []settlementApprovalRow
		want  string
	}{
		{
			name: "已审核的采购结款单 ⇒ 可付款", src: approved,
			want: SettlementStatePayable,
		},
		{
			name: "源库没有这张单 ⇒ not_found", src: SettlementRow{},
			want: SettlementStateNotFound,
		},
		{
			name: "源库有但未审核 ⇒ not_audited", src: notApproved,
			want: SettlementStateNotAudited,
		},
		{
			name: "源库有但不是 DP/CP ⇒ not_payable", src: otherType,
			want: SettlementStateNotPayable,
		},
		{
			name: "台账里已付款(初始化) ⇒ paid_init", src: approved, has: true,
			paid: model.SettlementPayState{PaidStatus: model.PayStatusPaid, SetMethod: model.PayMethodInit},
			want: SettlementStatePaidInit,
		},
		{
			name: "台账里已付款(审批) ⇒ paid_approval", src: approved, has: true,
			paid: model.SettlementPayState{PaidStatus: model.PayStatusPaid, SetMethod: model.PayMethodApproval},
			want: SettlementStatePaidApproval,
		},
		{
			name: "有一笔在跑的审批 ⇒ pending", src: approved,
			appr: []settlementApprovalRow{{ApprovalID: "AP1", SpNo: "SP1", Status: model.StatusApproved}},
			want: SettlementStatePending,
		},
		{
			name: "审批被拒 ⇒ 恢复可付款", src: approved,
			appr: []settlementApprovalRow{{ApprovalID: "AP1", SpNo: "SP1", Status: model.StatusHumanRejected}},
			want: SettlementStatePayable,
		},
		{
			name: "已付款优先于待审批中(付掉了就是付掉了)", src: approved, has: true,
			paid: model.SettlementPayState{PaidStatus: model.PayStatusPaid, SetMethod: model.PayMethodApproval},
			appr: []settlementApprovalRow{{ApprovalID: "AP1", SpNo: "SP1", Status: model.StatusApproved}},
			want: SettlementStatePaidApproval,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildSettlementStatus(c.src.ID, c.src, c.paid, c.has, c.appr)
			if got.State != c.want {
				t.Fatalf("state = %q, want %q(label=%q)", got.State, c.want, got.StateLabel)
			}
			if got.StateLabel == "" {
				t.Fatal("必须有给店长看的中文说明")
			}
		})
	}
}

// 待审批中 ⇒ 必须给出**是哪一笔审批**（前端要拿它跳审批详情）。
func TestBuildSettlementStatus_PendingCarriesApproval(t *testing.T) {
	src := SettlementRow{ID: "S1", DocumentType: "CP", ApproveStatus: "1"}
	got := buildSettlementStatus("S1", src, model.SettlementPayState{}, false,
		[]settlementApprovalRow{{ApprovalID: "AP-42", SpNo: "202610290024", Status: model.StatusApproved}})
	if got.ApprovalID != "AP-42" {
		t.Fatalf("approval_id = %q, want AP-42", got.ApprovalID)
	}
	if got.ApprovalSpNo != "202610290024" {
		t.Fatalf("approval_sp_no = %q, want 202610290024", got.ApprovalSpNo)
	}
}

// ---- 服务层（带 DB + fake cube）----

// ListSettlementStatuses：查不到的单也要返回一条 not_found。
//
// ⚠️ 判别力：返回空列表 ⇒ 前端显示"无数据" ⇒ 店长以为是自己输错了单号。
func TestListSettlementStatuses_NotFoundStillReturnsRow(t *testing.T) {
	svc, _, cube, _, _ := newTestService(t)
	seedSettlementRows(t, cube, row("S1", "SUP1", "CP", "100.00"))

	items, err := svc.ListSettlementStatuses(context.Background(), "B1", []string{"S1", "NOPE"})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("条数 = %d, want 2(查不到的那张也要给一条)", len(items))
	}
	byID := map[string]SettlementStatusItem{}
	for _, it := range items {
		byID[it.SettlementID] = it
	}
	if byID["S1"].State != SettlementStatePayable {
		t.Fatalf("S1 state = %q, want payable", byID["S1"].State)
	}
	if byID["NOPE"].State != SettlementStateNotFound {
		t.Fatalf("NOPE state = %q, want not_found", byID["NOPE"].State)
	}
}

// 别的门店的审批不该让本门店的单变成"待审批中"。
func TestListSettlementStatuses_BranchIsolation(t *testing.T) {
	svc, db, cube, _, _ := newTestService(t)
	seedSettlementRows(t, cube, row("S1", "SUP1", "CP", "100.00"))
	seedApprovalWithSettlements(t, db, "B2", "AP-OTHER", model.StatusApproved, "S1")

	items, err := svc.ListSettlementStatuses(context.Background(), "B1", []string{"S1"})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(items) != 1 || items[0].State != SettlementStatePayable {
		t.Fatalf("B2 的审批不该影响 B1 的 S1: %+v", items)
	}
}

// ---- 列表关键字搜索 ----

// 搜索能翻出**被 limit 截断**的老单。
//
// ⚠️ 判别力：把搜索模式改回"先截断再匹配" ⇒ 本用例立刻红。
// 生产实测（2026-10-29）：该店已审核结款单 4422 张，列表 limit=200；
// CP002608194277 按账期倒序排名 256 ⇒ 常规列表永远搜不到。
func TestListCubeSettlements_SearchFindsOldSettlement(t *testing.T) {
	svc, _, cube, _, _ := newTestService(t)
	rows := make([]SettlementRow, 0, 251)
	for i := 0; i < 250; i++ {
		r := row(fmt.Sprintf("CP%03d", i), "S1", "CP", "100.00")
		r.PeriodEnd = "2026-09-30"
		rows = append(rows, r)
	}
	old := row("CP-OLD", "S1", "CP", "100.00")
	old.PeriodEnd = "2026-08-19" // 最旧的账期 ⇒ 倒序排在最后
	rows = append(rows, old)
	seedSettlementRows(t, cube, rows...)

	// 常规列表：limit=200 ⇒ 最老的那张被截断。
	list, err := svc.ListCubeSettlements(context.Background(), "B1", "", "", "", "", 200, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Rows) != 200 {
		t.Fatalf("常规列表条数 = %d, want 200", len(list.Rows))
	}
	for _, r := range list.Rows {
		if r.ID == "CP-OLD" {
			t.Fatal("CP-OLD 不该出现在 limit=200 的常规列表里")
		}
	}

	// 搜索：拉全量再匹配 ⇒ 能搜到。
	found, err := svc.ListCubeSettlements(context.Background(), "B1", "", "", "", "CP-OLD", 200, false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(found.Rows) != 1 || found.Rows[0].ID != "CP-OLD" {
		t.Fatalf("搜索结果 = %+v, want CP-OLD", found.Rows)
	}
	if found.MatchTotal != 1 {
		t.Fatalf("match_total = %d, want 1", found.MatchTotal)
	}
}

// 搜索命中但**不可勾**的单 ⇒ 连同原因一起回传（前端要能说清"它怎么了"）。
func TestListCubeSettlements_SearchReportsUnavailable(t *testing.T) {
	svc, db, cube, _, _ := newTestService(t)
	seedSettlementRows(t, cube, row("CP1", "S1", "CP", "100.00"))
	seedApprovalWithSettlements(t, db, "B1", "AP-1", model.StatusApproved, "CP1")

	found, err := svc.ListCubeSettlements(context.Background(), "B1", "", "", "", "CP1", 200, false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(found.Rows) != 0 {
		t.Fatalf("待审批中的单不该可勾,实际 %+v", found.Rows)
	}
	if found.MatchTotal != 1 {
		t.Fatalf("match_total = %d, want 1", found.MatchTotal)
	}
	if len(found.Unavailable) != 1 {
		t.Fatalf("unavailable 条数 = %d, want 1", len(found.Unavailable))
	}
	u := found.Unavailable[0]
	if u.State != SettlementStatePending {
		t.Fatalf("state = %q, want pending", u.State)
	}
	if u.ApprovalID != "AP-1" {
		t.Fatalf("approval_id = %q, want AP-1(前端要拿它跳审批详情)", u.ApprovalID)
	}
	if u.StateLabel == "" {
		t.Fatal("必须有给店长看的中文说明")
	}
}

// 常规列表（没传关键字）**不返回** unavailable/match_total ——
// 那两个字段只在搜索模式下有意义。
func TestListCubeSettlements_NoQueryHasNoUnavailable(t *testing.T) {
	svc, db, cube, _, _ := newTestService(t)
	seedSettlementRows(t, cube, row("CP1", "S1", "CP", "100.00"))
	seedApprovalWithSettlements(t, db, "B1", "AP-1", model.StatusApproved, "CP1")

	list, err := svc.ListCubeSettlements(context.Background(), "B1", "", "", "", "", 200, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if list.MatchTotal != 0 || len(list.Unavailable) != 0 {
		t.Fatalf("常规列表不该带 match_total/unavailable: %+v", list)
	}
}

// SettlementStatusIDsFromText：把用户贴的一整段话拆成单号候选。
func TestSettlementStatusIDsFromText(t *testing.T) {
	got := SettlementStatusIDsFromText("CP002608194277, CP002609024324；CP002609064328、 CP002608194277")
	want := []string{"CP002608194277", "CP002609024324", "CP002609064328"}
	if len(got) != len(want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got = %v, want %v", got, want)
		}
	}
}
