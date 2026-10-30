package service

import (
	"context"
	"strings"
	"testing"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// 本文件覆盖「列表卡片摘要」（2026-10-29 用户口径）与
// 「LLM 结论里的供应商显示为 <名称>-<编号>」。

// 列表行要带上供应商名与付款项名。
func TestListApprovals_CarriesSupplierAndPaymentItemNames(t *testing.T) {
	svc, db, cube, _, _ := newTestService(t)
	cube.suppliers = map[string]string{
		"01": "岑溪市泰鼎粮油食品有限公司",
		"02": "广西梧州XX商贸",
	}
	ap := &model.ApprovalInstance{
		ID: "AP-1", CorpID: "wcorp-test", BranchID: "B1",
		ApplicantUserID: "u-1", ApplicantWecomID: "wecom-u1",
		Status: model.StatusApproved,
	}
	if err := db.Create(ap).Error; err != nil {
		t.Fatalf("seed approval: %v", err)
	}
	for _, sn := range []model.ApprovalSettlement{
		{ID: "as-1", ApprovalID: "AP-1", SettlementID: "DP1", SupplierID: "01"},
		{ID: "as-2", ApprovalID: "AP-1", SettlementID: "DP2", SupplierID: "02"},
		// 同一供应商的另��张单 ⇒ 名称要去重
		{ID: "as-3", ApprovalID: "AP-1", SettlementID: "DP3", SupplierID: "01"},
	} {
		if err := db.Create(&sn).Error; err != nil {
			t.Fatalf("seed approval settlement: %v", err)
		}
	}
	for _, it := range []model.ApprovalItem{
		{ID: "ai-1", ApprovalID: "AP-1", BranchID: "B1", Direction: ItemDirectionPayment, ItemType: "水电费", AmountYuan: dec("100")},
		{ID: "ai-2", ApprovalID: "AP-1", BranchID: "B1", Direction: ItemDirectionPayment, ItemType: "租金", AmountYuan: dec("200")},
		// 收款项**不是**付款项 ⇒ 不该出现在卡片上
		{ID: "ai-3", ApprovalID: "AP-1", BranchID: "B1", Direction: ItemDirectionReceipt, ItemType: "陈列费", AmountYuan: dec("50")},
	} {
		if err := db.Create(&it).Error; err != nil {
			t.Fatalf("seed approval item: %v", err)
		}
	}

	items, total, err := svc.ListApprovals(context.Background(), "B1", "", "", "", 50, 0)
	if err != nil {
		t.Fatalf("ListApprovals: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("total=%d len=%d, want 1/1", total, len(items))
	}
	sup := items[0].SupplierNames
	if len(sup) != 2 || sup[0] != "岑溪市泰鼎粮油食品有限公司" || sup[1] != "广西梧州XX商贸" {
		t.Fatalf("供应商名 = %v, want [泰鼎 梧州](去重后)", sup)
	}
	pay := items[0].PaymentItemNames
	if len(pay) != 2 || pay[0] != "水电费" || pay[1] != "租金" {
		t.Fatalf("付款项名 = %v, want [水电费 租金](不含收款项)", pay)
	}
}

// 字典查不到供应商名时退回编号（不能是空串）。
func TestListApprovals_SupplierFallsBackToID(t *testing.T) {
	svc, db, cube, _, _ := newTestService(t)
	cube.suppliers = map[string]string{} // 字典空的
	ap := &model.ApprovalInstance{
		ID: "AP-1", CorpID: "wcorp-test", BranchID: "B1",
		ApplicantUserID: "u-1", ApplicantWecomID: "wecom-u1", Status: model.StatusApproved,
	}
	if err := db.Create(ap).Error; err != nil {
		t.Fatalf("seed approval: %v", err)
	}
	if err := db.Create(&model.ApprovalSettlement{
		ID: "as-1", ApprovalID: "AP-1", SettlementID: "DP1", SupplierID: "29",
	}).Error; err != nil {
		t.Fatalf("seed approval settlement: %v", err)
	}

	items, _, err := svc.ListApprovals(context.Background(), "B1", "", "", "", 50, 0)
	if err != nil {
		t.Fatalf("ListApprovals: %v", err)
	}
	if len(items[0].SupplierNames) != 1 || items[0].SupplierNames[0] != "29" {
		t.Fatalf("供应商名 = %v, want [29](字典查不到时退回编号)", items[0].SupplierNames)
	}
}

// 供应商展示串：「<名称>-<编号>」。
func TestSupplierLabelForPrompt(t *testing.T) {
	cases := []struct {
		name, id, want string
	}{
		{"岑溪市泰鼎粮油食品有限公司", "29", "岑溪市泰鼎粮油食品有限公司-29"},
		{"", "29", "29"},
		{"某某公司", "", "某某公司"},
		{"", "", "-"},
		{"  某某公司  ", " 29 ", "某某公司-29"},
	}
	for _, c := range cases {
		if got := supplierLabelForPrompt(c.name, c.id); got != c.want {
			t.Errorf("supplierLabelForPrompt(%q,%q) = %q, want %q", c.name, c.id, got, c.want)
		}
	}
}

// 提示词里的结算单行要带「<名称>-<编号>」，不能只有编号。
//
// ⚠️ 判别力：把 supplierLabelForPrompt 换回 st.SupplierID ⇒ 本用例立刻红。
func TestBuildAuditUserPrompt_SupplierShowsNameAndID(t *testing.T) {
	in := &AuditInput{
		Approval: &model.ApprovalInstance{ID: "AP-1", BranchID: "B1"},
		Settlements: []SettlementRow{{
			ID: "DP1", SupplierID: "29", SupplierName: "岑溪市泰鼎粮油食品有限公司",
			DocumentType: "DP", PeriodStart: "2026-09-01", PeriodEnd: "2026-09-30",
			TotalGoodsYuan: dec("100"),
		}},
	}
	prompt := buildAuditUserPrompt(in)
	if !strings.Contains(prompt, "岑溪市泰鼎粮油食品有限公司-29") {
		t.Fatalf("提示词应带「<名称>-<编号>」: %s", prompt)
	}
}
