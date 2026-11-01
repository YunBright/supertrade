// Package service / stocktake_plan_test.go —— 手动生成计划盘点单。
//
// 锁 2026-11-01 用户口径：warn 级盘点类信号不再自动建单，改由审批详情页
// 由人触发；block 信号仍走自动路径（见 pipeline_test.go）。
package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// seedWarnApproval 建一笔已拒绝的审批 + 指定信号（以及可选的已落库建议）。
func seedWarnApproval(t *testing.T, svc *Service, signals []model.AuditSignal, suggestion string) *model.ApprovalInstance {
	t.Helper()
	db := svc.db
	ap := &model.ApprovalInstance{
		ID: "ap-manual", CorpID: "wcorp-test", BranchID: "B1",
		ApplicantUserID: "u-1", ApplicantWecomID: "wecom-u1",
		Status: model.StatusRejected,
	}
	if err := db.Create(ap).Error; err != nil {
		t.Fatalf("create ap: %v", err)
	}
	db.Create(&model.ApprovalSettlement{ID: "x1", ApprovalID: ap.ID, SettlementID: "CP002610104463"})
	for i := range signals {
		signals[i].ID = "sig-" + string(rune('a'+i))
		signals[i].ApprovalID = ap.ID
		if err := db.Create(&signals[i]).Error; err != nil {
			t.Fatalf("create signal: %v", err)
		}
	}
	if suggestion != "" {
		v := &model.LLMVerdict{
			ID: "v-manual", ApprovalID: ap.ID, Conclusion: ConclusionReject,
			SuggestStocktake: []byte(suggestion),
			RawJSON:          []byte(`{}`), Source: model.VerdictSourceRuleFallback,
		}
		if err := db.Create(v).Error; err != nil {
			t.Fatalf("create verdict: %v", err)
		}
	}
	return ap
}

// TestGenerateStocktakePlan_WarnSignalCreatesPlan 是**最关键**的一条：
// 仅 warn 数量信号 ⇒ 手动端点应建出计划单，商品来自 warn 信号。
func TestGenerateStocktakePlan_WarnSignalCreatesPlan(t *testing.T) {
	svc, _, _, pub, st := newTestService(t)
	ap := seedWarnApproval(t, svc, []model.AuditSignal{
		{SignalType: SigQtyDeviation, Level: model.SignalWarn, ProductID: "PR9", Message: "采购 35 销 10"},
	}, "")

	headerID, created, err := svc.GenerateStocktakePlan(context.Background(), "B1", ap.ID, "op-1", "Bearer manual-token")
	if err != nil {
		t.Fatalf("GenerateStocktakePlan: %v", err)
	}
	if !created || headerID != "ST-PLAN-1" {
		t.Fatalf("header/created = %s/%v, want ST-PLAN-1/true", headerID, created)
	}
	st.mu.Lock()
	got := st.got
	st.mu.Unlock()
	if got == nil {
		t.Fatal("应调 stocktake 建单")
	}
	if got.OperatorID != "op-1" {
		t.Errorf("operator = %s, want 操作人 op-1", got.OperatorID)
	}
	if len(got.ProductIDs) != 1 || got.ProductIDs[0] != "PR9" {
		t.Errorf("计划商品应来自 warn 信号, got %v", got.ProductIDs)
	}
	if !strings.Contains(got.Remark, "用户手动生成") {
		t.Errorf("计划单备注应标明手动生成: %s", got.Remark)
	}
	var link model.ApprovalStocktakeLink
	if err := svc.db.First(&link, "approval_id = ?", ap.ID).Error; err != nil {
		t.Fatalf("缺审批↔盘点绑定: %v", err)
	}
	if cmds := pub.commands(CmdTaskCard); len(cmds) != 1 {
		t.Errorf("应推 1 次盘点卡片, got %d", len(cmds))
	}
}

// TestGenerateStocktakePlan_Idempotent 已有绑定 ⇒ 不重复建单。
func TestGenerateStocktakePlan_Idempotent(t *testing.T) {
	svc, db, _, pub, st := newTestService(t)
	ap := &model.ApprovalInstance{ID: "ap-idem2", CorpID: "wcorp-test", BranchID: "B1",
		ApplicantUserID: "u-1", Status: model.StatusRejected}
	if err := db.Create(ap).Error; err != nil {
		t.Fatalf("create ap: %v", err)
	}
	db.Create(&model.ApprovalStocktakeLink{ID: "lk-1", ApprovalID: ap.ID, StocktakeHeaderID: "ST-EXIST"})

	headerID, created, err := svc.GenerateStocktakePlan(context.Background(), "B1", ap.ID, "op-1", "")
	if err != nil {
		t.Fatalf("GenerateStocktakePlan: %v", err)
	}
	if created || headerID != "ST-EXIST" {
		t.Errorf("header/created = %s/%v, want ST-EXIST/false", headerID, created)
	}
	if st.calls != 0 {
		t.Errorf("已有绑定不该再建单, calls=%d", st.calls)
	}
	if cmds := pub.commands(CmdTaskCard); len(cmds) != 0 {
		t.Errorf("已有绑定不该再推卡片, got %d", len(cmds))
	}
}

// TestGenerateStocktakePlan_RejectsNonStocktakeWarn 非盘点类 warn 不允许建单。
func TestGenerateStocktakePlan_RejectsNonStocktakeWarn(t *testing.T) {
	svc, _, _, _, _ := newTestService(t)
	ap := seedWarnApproval(t, svc, []model.AuditSignal{
		{SignalType: SigJointRateLow, Level: model.SignalWarn, Message: "扣点率 0%"},
	}, "")

	_, _, err := svc.GenerateStocktakePlan(context.Background(), "B1", ap.ID, "op-1", "")
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput, got %v", err)
	}
}

// TestGenerateStocktakePlan_CrossBranchIsNotFound 跨门店当作不存在。
func TestGenerateStocktakePlan_CrossBranchIsNotFound(t *testing.T) {
	svc, _, _, _, _ := newTestService(t)
	ap := seedWarnApproval(t, svc, []model.AuditSignal{
		{SignalType: SigQtyDeviation, Level: model.SignalWarn, ProductID: "PR9"},
	}, "")

	_, _, err := svc.GenerateStocktakePlan(context.Background(), "B-OTHER", ap.ID, "op-1", "")
	if !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("want ErrApprovalNotFound, got %v", err)
	}
}

// TestGenerateStocktakePlan_PrefersStoredSuggestion 有 LLM 建议时沿用其范围。
func TestGenerateStocktakePlan_PrefersStoredSuggestion(t *testing.T) {
	svc, _, _, _, st := newTestService(t)
	ap := seedWarnApproval(t, svc, []model.AuditSignal{
		{SignalType: SigQtyDeviation, Level: model.SignalWarn, ProductID: "PR9"},
	}, `{"scope":"product","ids":["PR-LLM"]}`)

	_, created, err := svc.GenerateStocktakePlan(context.Background(), "B1", ap.ID, "op-1", "")
	if err != nil || !created {
		t.Fatalf("GenerateStocktakePlan: created=%v err=%v", created, err)
	}
	st.mu.Lock()
	got := st.got
	st.mu.Unlock()
	if got == nil || len(got.ProductIDs) != 1 || got.ProductIDs[0] != "PR-LLM" {
		t.Errorf("应沿用已落库的 LLM 建议商品, got %+v", got)
	}
}
