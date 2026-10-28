package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// ---- 管理员人工放行（2026-10-28 用户口径）----
//
// 用户口径原文：
//
//	「在审批详情页针对被机器拒绝的增加『管理员』审批按钮，
//	 通过此按钮获批的审批需要在 AI 结论多文本控件**第一行**插入『已转人工审核并通过』。」
//
// 本文件锁三件事：
//  1. **状态守门**：只有 rejected 的单能被放行；
//  2. **首行前缀**：「AI审核结论」控件的**第一行**必须是「已转人工审核并通过」；
//  3. **来源可辨**：结论落库 source=admin_override（页面据此显示「人工放行」）。

// adminApproveAIControlID 是测试里配的「AI审核结论」控件 id。
const adminApproveAIControlID = "Textarea-AI-1"

// newAdminApproveSvc 起一个带「AI审核结论」控件的 service。
func newAdminApproveSvc(t *testing.T) (*Service, *gorm.DB, *fakeCube, *fakePublisher) {
	t.Helper()
	svc, db, cube, pub, _ := newTestService(t)
	svc.cfg.AIConclusionControlID = adminApproveAIControlID
	return svc, db, cube, pub
}

// seedRejectedApproval 造一条「被机器拒绝」的审批单。
//
// ⚠️ 带结算单快照 + 流程选人快照 ——
//	放行要**重新提交企微审批单**，没有节点成员 ⇒ buildProcess 报错 ⇒ 单子提交不出去。
func seedRejectedApproval(t *testing.T, db *gorm.DB) *model.ApprovalInstance {
	t.Helper()
	ap := &model.ApprovalInstance{
		ID:               "ap-admin-1",
		CorpID:           "wcorp-test",
		BranchID:         "B1",
		ApplicantUserID:  "u-1",
		ApplicantWecomID: "wecom-u1",
		ManualAmountYuan: dec("100"),
		Remark:           "9月货款",
		Status:           model.StatusRejected,
		RejectReason:     "机器判定金额勾稽不符",
		ApproverUserIDs:  datatypes.JSON([]byte(`["apr1"]`)),
		HandlerUserIDs:   datatypes.JSON([]byte(`["handler1"]`)),
		CcUserIDs:        datatypes.JSON([]byte(`["cc1"]`)),
		CreatedAt:        fixedNow,
	}
	if err := db.Create(ap).Error; err != nil {
		t.Fatalf("建审批: %v", err)
	}
	if err := db.Create(&model.ApprovalSettlement{
		ID:           "as-1",
		ApprovalID:   ap.ID,
		SettlementID: "S1",
		SupplierID:   "SUP1",
		DocumentType: "DP",
	}).Error; err != nil {
		t.Fatalf("建审批结算快照: %v", err)
	}
	// 机器当初的结论（放行后要被覆盖 ⇒ 先留着，测"原理由进结论"）。
	if err := db.Create(&model.LLMVerdict{
		ID:         "v-1",
		ApprovalID: ap.ID,
		Conclusion: ConclusionReject,
		Reason:     "机器判定金额勾稽不符",
		Source:     model.VerdictSourceLLM,
	}).Error; err != nil {
		t.Fatalf("建原结论: %v", err)
	}
	return ap
}

// aiConclusionTextOf 从已发布的 apply_event 命令里取出「AI审核结论」控件的文本。
//
// ⚠️ 走**真实链路**：apply_data.contents[].value.text ——
//	只测 aiConclusionText 这个纯函数会漏掉"前缀根本没进控件"这类问题。
func aiConclusionTextOf(t *testing.T, cmds []*WecomCommand) string {
	t.Helper()
	if len(cmds) == 0 {
		t.Fatal("没有发布 apply_event 命令")
	}
	var payload ApplyEventPayload
	if err := json.Unmarshal(cmds[0].Payload, &payload); err != nil {
		t.Fatalf("解析 apply_event 载荷: %v", err)
	}
	var doc struct {
		Contents []struct {
			Control string `json:"control"`
			ID      string `json:"id"`
			Value   struct {
				Text string `json:"text"`
			} `json:"value"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(payload.ApplyData, &doc); err != nil {
		t.Fatalf("解析 apply_data: %v", err)
	}
	for _, c := range doc.Contents {
		if c.ID == adminApproveAIControlID {
			return c.Value.Text
		}
	}
	t.Fatalf("apply_data 里没有「AI审核结论」控件(%s)", adminApproveAIControlID)
	return ""
}

// TestAdminApprove_PrefixMatchesUserWording 锁死前缀字面值。
//
// ⚠️ 判别力自检：把 AdminApprovePrefix 改成别的字 ⇒ 本用例立刻红。
func TestAdminApprove_PrefixMatchesUserWording(t *testing.T) {
	if AdminApprovePrefix != "已转人工审核并通过" {
		t.Fatalf("首行前缀必须逐字等于用户口径原文, got %q", AdminApprovePrefix)
	}
}

// TestAdminApprove_ConclusionFirstLineIsPrefix 是本模块**最重要**的一条。
//
// 判别力自检：
//   - 把 aiConclusionText 里的 prefix 逻辑去掉 ⇒ 本用例立刻红；
//   - 把 prefix 拼到**末尾**而不是开头 ⇒ 本用例立刻红。
func TestAdminApprove_ConclusionFirstLineIsPrefix(t *testing.T) {
	ctx := context.Background()
	svc, db, _, pub := newAdminApproveSvc(t)
	ap := seedRejectedApproval(t, db)

	if err := svc.AdminApprove(ctx, "B1", ap.ID, "admin-1"); err != nil {
		t.Fatalf("AdminApprove: %v", err)
	}
	text := aiConclusionTextOf(t, pub.commands(CmdApplyEvent))
	lines := strings.Split(text, "\n")
	if len(lines) == 0 {
		t.Fatal("AI 结论为空")
	}
	if lines[0] != AdminApprovePrefix {
		t.Fatalf("AI 结论**第一行**必须是 %q, got %q (全文=%q)", AdminApprovePrefix, lines[0], text)
	}
	// 第二行必须带上机器当初的拒绝理由 ⇒ 审批人既知道是人放行的,也知道机器为什么拒。
	if len(lines) < 2 || !strings.Contains(lines[1], "金额勾稽不符") {
		t.Errorf("第二行应保留机器原拒绝理由, got %q (全文=%q)", text, text)
	}
	// ⚠️ 不能出现"机器审核通过" —— 是人放行的,不是机器审过的。
	if strings.Contains(text, "机器审核通过") {
		t.Errorf("人工放行的结论不该出现「机器审核通过」: %q", text)
	}
}

// TestAdminApprove_VerdictSourceIsAdminOverride 锁结论来源。
//
// 页面据此显示「人工放行」⇒ source 错��页面就会显示成"机器审过了"。
func TestAdminApprove_VerdictSourceIsAdminOverride(t *testing.T) {
	ctx := context.Background()
	svc, db, _, _ := newAdminApproveSvc(t)
	ap := seedRejectedApproval(t, db)

	if err := svc.AdminApprove(ctx, "B1", ap.ID, "admin-1"); err != nil {
		t.Fatalf("AdminApprove: %v", err)
	}
	var v model.LLMVerdict
	if err := db.Where("approval_id = ?", ap.ID).First(&v).Error; err != nil {
		t.Fatalf("读结论: %v", err)
	}
	if v.Source != model.VerdictSourceAdminOverride {
		t.Errorf("结论来源应是 admin_override, got %q", v.Source)
	}
	if v.Conclusion != ConclusionPass {
		t.Errorf("人工放行后结论应是 pass, got %q", v.Conclusion)
	}
}

// TestAdminApprove_OnlyRejectedCanBeApproved 锁状态守门。
//
// ⚠️ 判别力自检：把 AdminApprove 里的 status 守门去掉 ⇒ 本用例立刻红。
func TestAdminApprove_OnlyRejectedCanBeApproved(t *testing.T) {
	ctx := context.Background()
	for _, status := range []string{
		model.StatusReceived,
		model.StatusAuditing,
		model.StatusApproved,
		model.StatusFailed,
		model.StatusHumanApproved,
		model.StatusHumanRejected,
		model.StatusCancelled,
	} {
		t.Run(status, func(t *testing.T) {
			svc, db, _, pub := newAdminApproveSvc(t)
			ap := seedRejectedApproval(t, db)
			if err := db.Model(&model.ApprovalInstance{}).Where("id = ?", ap.ID).
				Update("status", status).Error; err != nil {
				t.Fatalf("改状态: %v", err)
			}
			err := svc.AdminApprove(ctx, "B1", ap.ID, "admin-1")
			if err == nil {
				t.Fatalf("状态 %s 不该被放行", status)
			}
			if !errors.Is(err, ErrInvalidStatus) {
				t.Errorf("应报状态非法, got %v", err)
			}
			// ⚠️ 关键：没放行就**不能**提交企微审批单（否则等于绕过状态机）。
			if n := len(pub.commands(CmdApplyEvent)); n != 0 {
				t.Errorf("状态 %s 不该发布 apply_event, 实际 %d 条", status, n)
			}
		})
	}
}

// TestAdminApprove_ClearsRejectReason 放行后要把拒绝理由清掉。
//
// ⚠️ 状态已是 approved,页面再挂一条红色"机器拒了" ⇒ 人会以为这单还没放行。
func TestAdminApprove_ClearsRejectReason(t *testing.T) {
	ctx := context.Background()
	svc, db, _, _ := newAdminApproveSvc(t)
	ap := seedRejectedApproval(t, db)

	if err := svc.AdminApprove(ctx, "B1", ap.ID, "admin-1"); err != nil {
		t.Fatalf("AdminApprove: %v", err)
	}
	var got model.ApprovalInstance
	if err := db.First(&got, "id = ?", ap.ID).Error; err != nil {
		t.Fatalf("读审批: %v", err)
	}
	if got.Status != model.StatusApproved {
		t.Errorf("放行后状态应是 approved, got %q", got.Status)
	}
	if got.RejectReason != "" {
		t.Errorf("放行后应清掉 reject_reason, got %q", got.RejectReason)
	}
	if got.LastCommandID == "" {
		t.Error("放行后应回填 last_command_id")
	}
}

// TestAdminApprove_NotifiesApplicant 放行后要通知发起人。
//
// ⚠️ 不通知 ⇒ 发起人可能已经重新提交了一单 ⇒ 重复付款。
func TestAdminApprove_NotifiesApplicant(t *testing.T) {
	ctx := context.Background()
	svc, db, _, pub := newAdminApproveSvc(t)
	ap := seedRejectedApproval(t, db)

	if err := svc.AdminApprove(ctx, "B1", ap.ID, "admin-1"); err != nil {
		t.Fatalf("AdminApprove: %v", err)
	}
	notices := pub.commands(CmdNotifyText)
	if len(notices) == 0 {
		t.Fatal("放行后应通知发起人")
	}
	var payload NotifyTextPayload
	if err := json.Unmarshal(notices[0].Payload, &payload); err != nil {
		t.Fatalf("解析通知载荷: %v", err)
	}
	if len(payload.ToUser) == 0 || payload.ToUser[0] != "wecom-u1" {
		t.Errorf("通知应发给发起人 wecom-u1, got %v", payload.ToUser)
	}
	if !strings.Contains(payload.Content, "请勿重复提交") {
		t.Errorf("通知应提示勿重复提交, got %q", payload.Content)
	}
}

// TestAdminApprove_DoesNotReQueryCube 放行时**不重查 cube**。
//
// ⚠️ 判别力自检：把 settlementsFromSnapshot 改成查 cube ⇒ 本用例立刻红
//    （cube 被设成"一查就报错"）。
//
// 为什么重要：cube 里是**今天**的金额,快照里是**当初被拒那单**的金额
// ⇒ 企微单上的金额必须等于当初被拒的那单,否则审批人看到的金额变了。
func TestAdminApprove_DoesNotReQueryCube(t *testing.T) {
	ctx := context.Background()
	svc, db, cube, _ := newAdminApproveSvc(t)
	ap := seedRejectedApproval(t, db)
	cube.mu.Lock()
	cube.settlementsErr = errors.New("放行时不该查 cube")
	cube.mu.Unlock()

	if err := svc.AdminApprove(ctx, "B1", ap.ID, "admin-1"); err != nil {
		t.Fatalf("AdminApprove 不该碰 cube: %v", err)
	}
}

// TestAdminApprove_CrossBranchIsNotFound 跨门店放行当作不存在。
func TestAdminApprove_CrossBranchIsNotFound(t *testing.T) {
	ctx := context.Background()
	svc, db, _, pub := newAdminApproveSvc(t)
	ap := seedRejectedApproval(t, db)

	err := svc.AdminApprove(ctx, "B-OTHER", ap.ID, "admin-1")
	if !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("跨门店应报不存在, got %v", err)
	}
	if n := len(pub.commands(CmdApplyEvent)); n != 0 {
		t.Errorf("跨门店不该发布 apply_event, 实际 %d 条", n)
	}
}
