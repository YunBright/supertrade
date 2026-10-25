package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// 审批流程节点声明(= 企微管理后台里「付款审批」模板的流程设置)驱动三件事：
//  1. UI 渲染「审批流程」几行(哪些节点、单选还是多选、哪些必填);
//  2. 提交时各节点的人怎么分桶;
//  3. 提交给企微的 process.node_list 怎么组装(节点类型/顺序/多人办理方式)。
//
// 本文件把这三件事都钉住 —— 判别力：任何一个跑偏，企微里的审批单就会
// **少一个节点**(办理人就是这样丢的)或**顺序/办理方式不对**,而页面看起来一切正常。
//
// 模板实际配置(2026-10-23 企微后台截图):
//
//	审批人  申请人自选 不限范围 依次审批
//	办理人  申请人自选 不限范围 单选
//	抄送人  申请人自选

// TestDefaultApprovalFlow_MatchesTemplate 默认流程必须与企微模板一致。
//
// 这条是"第二份真相"的护栏：app 侧这份声明与企微后台那份配置必须对得上。
// 对不上 ⇒ 少一个节点 ⇒ 企微里没人办。
func TestDefaultApprovalFlow_MatchesTemplate(t *testing.T) {
	nodes := DefaultApprovalFlow()
	if len(nodes) != 3 {
		t.Fatalf("默认流程应有 3 个节点(审批人/办理人/抄送人),got %d: %+v", len(nodes), nodes)
	}
	want := []struct {
		nodeType string
		label    string
		select_  string
		apvRel   int
		required bool
	}{
		{FlowNodeApprover, "审批人", FlowSelectMulti, ApvRelSerial, true},
		{FlowNodeHandler, "办理人", FlowSelectSingle, ApvRelAny, true},
		{FlowNodeCC, "抄送人", FlowSelectMulti, 0, false},
	}
	for i, w := range want {
		got := nodes[i]
		if got.Type != w.nodeType || got.Label != w.label || got.Select != w.select_ ||
			got.ApvRel != w.apvRel || got.Required != w.required {
			t.Errorf("第 %d 个节点 = %+v, want %+v", i+1, got, w)
		}
	}
	// 节点类型 → 企微 node type:1 审批人 / 3 办理人 / 2 抄送人。
	if got := (ApprovalFlowNode{Type: FlowNodeApprover}).NodeType(); got != 1 {
		t.Errorf("审批人 node type = %d, want 1", got)
	}
	if got := (ApprovalFlowNode{Type: FlowNodeHandler}).NodeType(); got != 3 {
		t.Errorf("办理人 node type = %d, want 3", got)
	}
	if got := (ApprovalFlowNode{Type: FlowNodeCC}).NodeType(); got != 2 {
		t.Errorf("抄送人 node type = %d, want 2", got)
	}
}

// TestParseApprovalFlow_EmptyUsesDefault 没配就用默认流程。
func TestParseApprovalFlow_EmptyUsesDefault(t *testing.T) {
	nodes, err := ParseApprovalFlow("")
	if err != nil {
		t.Fatalf("空配置应回退默认流程,got err = %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("默认流程应有 3 个节点,got %d", len(nodes))
	}
}

// TestParseApprovalFlow_RejectsBadConfig 配置错了要**启动期**就炸。
//
// 判别力：解析失败时返回错误 ⇒ main.go 直接退出。配置错了却静默起进程,
// 症状是"店长发起时才发现这单没人办" —— 那时已经是在生产上了。
func TestParseApprovalFlow_RejectsBadConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"不是 JSON", "not-json"},
		{"type 非法", `[{"type":"boss","label":"x","select":"single"}]`},
		{"select 非法", `[{"type":"approver","label":"x","select":"maybe"}]`},
		{"apv_rel 非法", `[{"type":"approver","label":"x","select":"single","apv_rel":9}]`},
		{"type 重复", `[{"type":"approver","label":"a","select":"single"},{"type":"approver","label":"b","select":"single"}]`},
		{"缺 label", `[{"type":"approver","select":"single"}]`},
		{"没有审批人节点", `[{"type":"cc","label":"抄送人","select":"multi"}]`},
		{"空数组", `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseApprovalFlow(tc.raw); err == nil {
				t.Errorf("非法配置应报错: %s", tc.raw)
			}
		})
	}
}

// TestResolveFlowSelections_RequiredNodesMustHavePeople 必填节点缺人 ⇒ 报错。
//
// 错误文案要带节点标签("请选择办理人")—— 报错说"approver"没人看得懂。
func TestResolveFlowSelections_RequiredNodesMustHavePeople(t *testing.T) {
	svc, _, _, _, _ := newTestService(t)
	_, err := svc.resolveFlowSelections(FlowSelections{
		FlowNodeApprover: []string{"boss"},
		// 办理人缺失
	})
	if err == nil {
		t.Fatal("办理人缺失时应报错")
	}
	if !strings.Contains(err.Error(), "办理人") {
		t.Errorf("报错应带节点标签,got %q", err.Error())
	}
}

// TestResolveFlowSelections_SingleSelectRejectsMultiple 单选节点不能选多人。
//
// 模板里办理人配的是「单选」⇒ 选 2 个人会让企微那边按多人方式处理,
// 与模板声明不一致。
func TestResolveFlowSelections_SingleSelectRejectsMultiple(t *testing.T) {
	svc, _, _, _, _ := newTestService(t)
	_, err := svc.resolveFlowSelections(FlowSelections{
		FlowNodeApprover: []string{"boss"},
		FlowNodeHandler:  []string{"u1", "u2"},
	})
	if err == nil {
		t.Fatal("单选节点选了 2 人应报错")
	}
	if !strings.Contains(err.Error(), "单选") {
		t.Errorf("报错应说明是单选限制,got %q", err.Error())
	}
}

// TestBuildProcess_NodeOrderAndTypes 核心判据：node_list 的**顺序与类型**。
//
// 这就是"办理人丢在哪"的答案：早先只给了 审批人(type=1) + 抄送人(type=2),
// 模板里的办理人(type=3)节点**根本不在 node_list 里** ⇒ 企微按 app 给的走,
// 没有人办理。
func TestBuildProcess_NodeOrderAndTypes(t *testing.T) {
	svc, _, _, _, _ := newTestService(t)
	process, err := svc.buildProcess(FlowSelections{
		FlowNodeApprover: []string{"boss", "boss2"},
		FlowNodeHandler:  []string{"cashier"},
		FlowNodeCC:       []string{"cc1"},
	})
	if err != nil {
		t.Fatalf("buildProcess: %v", err)
	}
	if len(process.NodeList) != 3 {
		t.Fatalf("node_list 应有 3 节点,got %d: %+v", len(process.NodeList), process.NodeList)
	}
	// 顺序必须与模板流程图一致:申请人 → 审批人 → 办理人 → 抄送人。
	n0 := process.NodeList[0]
	if n0.Type != 1 || n0.ApvRel != ApvRelSerial || len(n0.UserID) != 2 {
		t.Errorf("第 1 个节点 = %+v, want type=1 apv_rel=3 [boss boss2]", n0)
	}
	n1 := process.NodeList[1]
	if n1.Type != 3 || n1.ApvRel != ApvRelAny || len(n1.UserID) != 1 || n1.UserID[0] != "cashier" {
		t.Errorf("第 2 个节点 = %+v, want type=3 apv_rel=2 [cashier]", n1)
	}
	n2 := process.NodeList[2]
	if n2.Type != 2 || len(n2.UserID) != 1 || n2.UserID[0] != "cc1" {
		t.Errorf("第 3 个节点 = %+v, want type=2 [cc1]", n2)
	}
}

// TestBuildProcess_EmptyOptionalNodeIsOmitted 可选节点没人 ⇒ 干脆不给企微一个空节点。
func TestBuildProcess_EmptyOptionalNodeIsOmitted(t *testing.T) {
	svc, _, _, _, _ := newTestService(t)
	process, err := svc.buildProcess(FlowSelections{
		FlowNodeApprover: []string{"boss"},
		FlowNodeHandler:  []string{"cashier"},
	})
	if err != nil {
		t.Fatalf("buildProcess: %v", err)
	}
	if len(process.NodeList) != 2 {
		t.Fatalf("抄送人没人时不应给空节点,got %d 个: %+v", len(process.NodeList), process.NodeList)
	}
}

// TestSubmitApproval_FlowSelectionsEndToEnd 端到端：提交 → 快照 → applyevent。
func TestSubmitApproval_FlowSelectionsEndToEnd(t *testing.T) {
	svc, db, cube, pub, _ := newTestService(t)
	seedPassData(cube)
	svc.SetVerdictFn(func(_ context.Context, in *AuditInput) (*VerdictResult, error) {
		return &VerdictResult{
			Conclusion: ConclusionPass,
			Reason:     "LLM 审核通过",
			Confidence: dec("0.95"),
			RawJSON:    []byte(`{"conclusion":"pass"}`),
			Source:     model.VerdictSourceLLM,
		}, nil
	})

	ap, err := svc.SubmitApproval(context.Background(), "B1", SubmitInput{
		ApplicantUserID:  "u-1",
		ApplicantWecomID: "wecom-u1",
		ApplicantBearer:  "Bearer test-token",
		SettlementIDs:    []string{"S1"},
		ManualAmountYuan: dec("100"),
		Remark:           "9月货款",
		FlowSelections: FlowSelections{
			FlowNodeApprover: []string{"boss"},
			FlowNodeHandler:  []string{"cashier"},
			FlowNodeCC:       []string{"cc1"},
		},
	})
	if err != nil {
		t.Fatalf("SubmitApproval: %v", err)
	}
	ap = waitTerminalPtr(t, db, ap.ID)
	if ap.Status != model.StatusApproved {
		t.Fatalf("status = %s, want approved(err=%s)", ap.Status, ap.Error)
	}

	// 快照里各节点的人都在(详情页/审计要看)。
	var got model.ApprovalInstance
	db.First(&got, "id = ?", ap.ID)
	var handlers []string
	_ = json.Unmarshal(got.HandlerUserIDs, &handlers)
	if len(handlers) != 1 || handlers[0] != "cashier" {
		t.Errorf("快照里的办理人 = %v, want [cashier]", handlers)
	}

	// applyevent 的 node_list 含办理人节点(type=3)。
	cmds := pub.commands(CmdApplyEvent)
	if len(cmds) != 1 {
		t.Fatalf("apply_event 命令数 = %d, want 1", len(cmds))
	}
	var p ApplyEventPayload
	if err := json.Unmarshal(cmds[0].Payload, &p); err != nil {
		t.Fatalf("解析 apply_event payload: %v", err)
	}
	if p.UseTemplateApprover != 0 {
		t.Errorf("use_template_approver = %d, want 0(自带 node_list:模板节点全是「申请人自选」)", p.UseTemplateApprover)
	}
	if p.Process == nil || len(p.Process.NodeList) != 3 {
		t.Fatalf("node_list 应有 3 节点: %+v", p.Process)
	}
	found := false
	for _, n := range p.Process.NodeList {
		if n.Type == 3 && len(n.UserID) == 1 && n.UserID[0] == "cashier" {
			found = true
		}
	}
	if !found {
		t.Errorf("node_list 里没有办理人节点(type=3): %+v", p.Process.NodeList)
	}
}

// TestSubmitApproval_LegacyFieldsStillWork 旧客户端(只发 approver/cc)仍能提交。
//
// 旧客户端不知道"办理人"这回事 ⇒ 合并后办理人缺失 ⇒ 按 required 判定报错。
// 这条锁的是"**报错文案**对不对、且报的是办理人" —— 而不是让它静默通过
// (静默通过 = 提交成功但没人办 = 更糟)。
func TestSubmitApproval_LegacyFieldsStillWork(t *testing.T) {
	svc, _, _, _, _ := newTestService(t)
	_, err := svc.SubmitApproval(context.Background(), "B1", SubmitInput{
		ApplicantUserID:  "u-1",
		ApplicantWecomID: "wecom-u1",
		ApplicantBearer:  "Bearer test-token",
		SettlementIDs:    []string{"S1"},
		ManualAmountYuan: dec("100"),
		ApproverUserIDs:  []string{"boss"},
		CcUserIDs:        []string{"cc1"},
	})
	if err == nil {
		t.Fatal("缺办理人时应报错(否则提交成功但没人办)")
	}
	if !strings.Contains(err.Error(), "办理人") {
		t.Errorf("报错应指向办理人,got %q", err.Error())
	}
}

// TestFlowSelectionsMerge 合并 legacy 字段与分桶(不丢)。
//
// ⚠️ Merge 只做**拼接**,不去重 —— 去重在 resolveFlowSelections 里统一做。
// 判别力：这里若顺手去重,resolve 里的去重就变成两处真相。
func TestFlowSelectionsMerge(t *testing.T) {
	got := FlowSelections{FlowNodeHandler: []string{"cashier"}}.Merge(
		[]string{"boss", "boss"}, []string{"cashier2"}, []string{"cc1"},
	)
	if len(got[FlowNodeApprover]) != 2 {
		t.Errorf("审批人合并 = %v, want 2 个(Merge 不去重)", got[FlowNodeApprover])
	}
	if len(got[FlowNodeHandler]) != 2 {
		t.Errorf("办理人合并 = %v, want 2 个", got[FlowNodeHandler])
	}
	if len(got[FlowNodeCC]) != 1 {
		t.Errorf("抄送人合并 = %v", got[FlowNodeCC])
	}
}
