package service

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
	"github.com/YunBright/supertrade/internal/stocktake/testdb"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

var fixedNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// ---- fakes ----

// fakeCube 实现 CubeQuerier(字段加锁,pipeline 在 goroutine 里读)。
type fakeCube struct {
	mu             sync.Mutex
	settlements    []SettlementRow
	settlementsErr error
	lines          map[string][]SettlementLineRow
	sheets         []PurchaseSheetRow
	sheetLines     []SheetLineRow
	sales          []SaleRow
}

func (f *fakeCube) ListSettlements(_ context.Context, _ string, ids []string) ([]SettlementRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.settlementsErr != nil {
		return nil, f.settlementsErr
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	var out []SettlementRow
	for _, r := range f.settlements {
		if want[r.ID] {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeCube) ListAuditedSettlements(_ context.Context, _, _, _, _ string, _ int) ([]SettlementRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SettlementRow(nil), f.settlements...), nil
}

func (f *fakeCube) ListPurchaseLines(_ context.Context, _, settlementID string) ([]SettlementLineRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lines[settlementID], nil
}

func (f *fakeCube) ListPurchaseSheets(_ context.Context, _ string, ids []string) ([]PurchaseSheetRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	var out []PurchaseSheetRow
	for _, r := range f.sheets {
		if want[r.ID] {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeCube) ListSheetLines(_ context.Context, _ string, sheetIDs []string) ([]SheetLineRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	want := make(map[string]bool, len(sheetIDs))
	for _, id := range sheetIDs {
		want[id] = true
	}
	var out []SheetLineRow
	for _, r := range f.sheetLines {
		if want[r.SheetID] {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeCube) SumSalesByProduct(_ context.Context, _ string, productIDs []string, _, _ string) ([]SaleRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	want := make(map[string]bool, len(productIDs))
	for _, id := range productIDs {
		want[id] = true
	}
	var out []SaleRow
	for _, r := range f.sales {
		if want[r.ProductID] {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeCube) GetStocks(_ context.Context, _ string, _ []string) ([]StockRow, error) {
	return nil, nil
}

// seedPassData 灌一套全通过的 cube 数据:S1(货款 100)→ P1(PI)→ PR1 收 10 销 10。
func seedPassData(c *fakeCube) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.settlementsErr = nil
	c.settlements = []SettlementRow{
		{ID: "S1", SupplierID: "SUP1", DocumentType: "DP", ApproveStatus: "1", TotalGoodsYuan: dec("100")},
	}
	c.lines = map[string][]SettlementLineRow{
		"S1": {{SettlementID: "S1", VoucherID: "P1", SourceDocType: "PI", TotalGoodsYuan: dec("100")}},
	}
	c.sheets = []PurchaseSheetRow{
		{ID: "P1", SupplierID: "SUP1", DocumentType: "PI", ApproveStatus: "1"},
	}
	c.sheetLines = []SheetLineRow{
		{SheetID: "P1", ProductID: "PR1", ReceivedQuantity: dec("10")},
	}
	c.sales = []SaleRow{
		{ProductID: "PR1", NetSaleQuantity: dec("10")},
	}
}

type publishedEvent struct {
	topic string
	data  any
}

// fakePublisher 记录全部发布(加锁;终态后仍有 status/log 事件在飞)。
type fakePublisher struct {
	mu     sync.Mutex
	events []publishedEvent
}

func (f *fakePublisher) Publish(_ context.Context, topic string, data any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, publishedEvent{topic, data})
	return nil
}

// commands 取某类 wecom 命令(按发布序)。
func (f *fakePublisher) commands(cmdType string) []*WecomCommand {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*WecomCommand
	for _, e := range f.events {
		if e.topic != TopicWecomApprovalCommand {
			continue
		}
		if cmd, ok := e.data.(*WecomCommand); ok && cmd.Type == cmdType {
			out = append(out, cmd)
		}
	}
	return out
}

func (f *fakePublisher) countTopic(topic string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.events {
		if e.topic == topic {
			n++
		}
	}
	return n
}

// fakeStocktake 记录建单入参并返回固定 header。
type fakeStocktake struct {
	mu     sync.Mutex
	header *StocktakeHeaderDTO
	err    error
	got    *StocktakeCreatePlanInput
	calls  int
}

func (f *fakeStocktake) CreatePlanHeader(_ context.Context, in StocktakeCreatePlanInput) (*StocktakeHeaderDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	cp := in
	f.got = &cp
	if f.err != nil {
		return nil, f.err
	}
	return f.header, nil
}

// ---- 测试基建 ----

func newTestService(t *testing.T) (*Service, *gorm.DB, *fakeCube, *fakePublisher, *fakeStocktake) {
	t.Helper()
	db, err := testdb.OpenSQLite(":memory:?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&model.ApprovalInstance{}, &model.ApprovalSettlement{}, &model.AuditSignal{},
		&model.AuditLog{}, &model.LLMVerdict{}, &model.ApprovalStocktakeLink{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cube := &fakeCube{}
	pub := &fakePublisher{}
	st := &fakeStocktake{header: &StocktakeHeaderDTO{ID: "ST-PLAN-1", BranchID: "B1", Type: "plan", Status: "counting"}}
	svc := New(db, Config{
		CorpID:               "wcorp-test",
		TemplateID:           "tpl-1",
		StocktakeNotifyUsers: []string{"keeper-1"},
		WxH5BaseURL:          "https://h5.example.com",
	})
	svc.SetCube(cube)
	svc.SetPublisher(pub)
	svc.SetStocktake(st)
	svc.SetClock(func() time.Time { return fixedNow })
	return svc, db, cube, pub, st
}

// submitOK 提交一笔合法审批(S1,金额 amount)。
func submitOK(t *testing.T, svc *Service, amount string) *model.ApprovalInstance {
	t.Helper()
	ap, err := svc.SubmitApproval(context.Background(), "B1", SubmitInput{
		ApplicantUserID:  "u-1",
		ApplicantWecomID: "wecom-u1",
		ApplicantBearer:  "Bearer test-token",
		SettlementIDs:    []string{"S1"},
		ManualAmountYuan: dec(amount),
		Remark:           "9月货款",
		ApproverUserIDs:  []string{"apr1"},
		CcUserIDs:        []string{"cc1"},
	})
	if err != nil {
		t.Fatalf("SubmitApproval: %v", err)
	}
	return ap
}

// waitTerminal 轮询到终态(approved/rejected/failed)或超时。
func waitTerminal(t *testing.T, db *gorm.DB, id string) model.ApprovalInstance {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var ap model.ApprovalInstance
	for {
		if err := db.First(&ap, "id = ?", id).Error; err == nil {
			switch ap.Status {
			case model.StatusApproved, model.StatusRejected, model.StatusFailed:
				return ap
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待终态超时:id=%s 当前 status=%s", id, ap.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---- 提交校验 ----

func TestSubmitApproval_Validation(t *testing.T) {
	svc, _, _, _, _ := newTestService(t)
	valid := SubmitInput{
		ApplicantUserID:  "u-1",
		ApplicantWecomID: "wecom-u1",
		ApplicantBearer:  "Bearer test-token",
		SettlementIDs:    []string{"S1"},
		ManualAmountYuan: dec("100"),
		ApproverUserIDs:  []string{"apr1"},
	}
	tooMany := make([]string, 0, 51)
	for i := 0; i < 51; i++ {
		tooMany = append(tooMany, "S"+string(rune('A'+i%26))+string(rune('0'+i/26)))
	}
	tooManyApprovers := make([]string, 0, 101)
	for i := 0; i < 101; i++ {
		tooManyApprovers = append(tooManyApprovers, "A"+strconv.Itoa(i))
	}
	cases := []struct {
		name     string
		branchID string
		mutate   func(in *SubmitInput)
	}{
		{"缺 branch", "", nil},
		{"缺 applicant_user_id", "B1", func(in *SubmitInput) { in.ApplicantUserID = "" }},
		{"缺 applicant_wecom_id", "B1", func(in *SubmitInput) { in.ApplicantWecomID = "" }},
		{"缺 bearer(流水线无法调 cube)", "B1", func(in *SubmitInput) { in.ApplicantBearer = "" }},
		{"金额为 0", "B1", func(in *SubmitInput) { in.ManualAmountYuan = decimal.Zero }},
		{"金额为负", "B1", func(in *SubmitInput) { in.ManualAmountYuan = dec("-1") }},
		{"结算单为空", "B1", func(in *SubmitInput) { in.SettlementIDs = nil }},
		{"结算单超上限", "B1", func(in *SubmitInput) { in.SettlementIDs = tooMany }},
		{"缺审批人", "B1", func(in *SubmitInput) { in.ApproverUserIDs = nil }},
		{"审批人超上限", "B1", func(in *SubmitInput) { in.ApproverUserIDs = tooManyApprovers }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := valid
			if c.mutate != nil {
				c.mutate(&in)
			}
			_, err := svc.SubmitApproval(context.Background(), c.branchID, in)
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("err = %v, want ErrInvalidInput", err)
			}
		})
	}
}

// ---- 流水线:通过 ----

func TestRunAudit_PassLLM(t *testing.T) {
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

	ap := submitOK(t, svc, "100")
	ap = waitTerminalPtr(t, db, ap.ID)

	if ap.Status != model.StatusApproved {
		t.Fatalf("status = %s, want approved(err=%s)", ap.Status, ap.Error)
	}
	if ap.LastCommandID == "" {
		t.Error("last_command_id 应回填 apply_event 命令 id")
	}
	// apply_event 命令载荷
	cmds := pub.commands(CmdApplyEvent)
	if len(cmds) != 1 {
		t.Fatalf("apply_event 命令数 = %d, want 1", len(cmds))
	}
	var p ApplyEventPayload
	if err := json.Unmarshal(cmds[0].Payload, &p); err != nil {
		t.Fatalf("解析 apply_event payload: %v", err)
	}
	if p.CreatorUserID != "wecom-u1" || p.TemplateID != "tpl-1" || p.UseTemplateApprover != 0 {
		t.Errorf("payload = %+v", p)
	}
	if p.Process == nil || len(p.Process.NodeList) != 2 {
		t.Fatalf("process 应有审批+抄送 2 节点: %+v", p.Process)
	}
	n0 := p.Process.NodeList[0]
	if n0.Type != 1 || n0.ApvRel != 2 || len(n0.UserID) != 1 || n0.UserID[0] != "apr1" {
		t.Errorf("审批节点 = %+v, want type=1 apv_rel=2 [apr1]", n0)
	}
	n1 := p.Process.NodeList[1]
	if n1.Type != 2 || n1.UserID[0] != "cc1" {
		t.Errorf("抄送节点 = %+v, want type=2 [cc1]", n1)
	}
	if !strings.Contains(string(p.ApplyData), "S1") || !strings.Contains(string(p.ApplyData), "100.00") {
		t.Errorf("apply_data 应含结算单号与金额: %s", p.ApplyData)
	}
	// 结论落库(source=llm)
	var v model.LLMVerdict
	if err := db.First(&v, "approval_id = ?", ap.ID).Error; err != nil {
		t.Fatalf("查 verdict: %v", err)
	}
	if v.Source != model.VerdictSourceLLM || v.Conclusion != ConclusionPass {
		t.Errorf("verdict source=%s conclusion=%s, want llm/pass", v.Source, v.Conclusion)
	}
	// 结算单快照补全且 Checked
	var snaps []model.ApprovalSettlement
	db.Find(&snaps, "approval_id = ?", ap.ID)
	if len(snaps) != 1 || !snaps[0].Checked || !snaps[0].GoodsAmountYuan.Equal(dec("100")) {
		t.Errorf("快照 = %+v, want 1 条 checked 且金额 100", snaps)
	}
	// 无信号 + 分步日志齐全
	var sigCount int64
	db.Model(&model.AuditSignal{}).Where("approval_id = ?", ap.ID).Count(&sigCount)
	if sigCount != 0 {
		t.Errorf("通过场景应无信号, got %d", sigCount)
	}
	var steps []string
	db.Model(&model.AuditLog{}).Where("approval_id = ?", ap.ID).Order("created_at ASC").Pluck("step", &steps)
	for _, want := range []string{StepFetchSettlements, StepLLMVerdict, StepFinish} {
		found := false
		for _, s := range steps {
			if s == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("audit_logs 缺步骤 %s(现有 %v)", want, steps)
		}
	}
	// 状态事件至少 received + auditing + approved
	if n := pub.countTopic(TopicAuditStatus); n < 3 {
		t.Errorf("status 事件数 = %d, want >= 3", n)
	}
}

// waitTerminalPtr 同 waitTerminal,返回指针(语义对齐 submitOK)。
func waitTerminalPtr(t *testing.T, db *gorm.DB, id string) *model.ApprovalInstance {
	t.Helper()
	ap := waitTerminal(t, db, id)
	return &ap
}

// ---- 流水线:规则拒绝 ----

func TestRunAudit_RejectManualMismatch(t *testing.T) {
	svc, db, cube, pub, _ := newTestService(t)
	seedPassData(cube)

	ap := submitOK(t, svc, "200") // 手工 200 vs 结算 100 → block
	ap = waitTerminalPtr(t, db, ap.ID)

	if ap.Status != model.StatusRejected {
		t.Fatalf("status = %s, want rejected", ap.Status)
	}
	if !strings.Contains(ap.RejectReason, "不符") {
		t.Errorf("reject_reason = %q, 应含勾稽不符描述", ap.RejectReason)
	}
	// block 信号落库
	var sig model.AuditSignal
	if err := db.First(&sig, "approval_id = ? AND signal_type = ?", ap.ID, SigManualAmountMismatch).Error; err != nil {
		t.Fatalf("缺 manual_amount_mismatch 信号: %v", err)
	}
	if sig.Level != model.SignalBlock {
		t.Errorf("信号级别 = %s, want block", sig.Level)
	}
	// 通知申请人,且不发 apply_event
	cmds := pub.commands(CmdNotifyText)
	if len(cmds) != 1 {
		t.Fatalf("notify_text 命令数 = %d, want 1", len(cmds))
	}
	var p NotifyTextPayload
	if err := json.Unmarshal(cmds[0].Payload, &p); err != nil {
		t.Fatalf("解析 notify_text payload: %v", err)
	}
	if len(p.ToUser) != 1 || p.ToUser[0] != "wecom-u1" {
		t.Errorf("to_user = %v, want [wecom-u1]", p.ToUser)
	}
	if !strings.Contains(p.Content, ap.ID) {
		t.Errorf("通知内容应含审批单号: %q", p.Content)
	}
	if n := len(pub.commands(CmdApplyEvent)); n != 0 {
		t.Errorf("拒绝场景不应发 apply_event, got %d", n)
	}
	// 结论来源规则降级
	var v model.LLMVerdict
	if err := db.First(&v, "approval_id = ?", ap.ID).Error; err != nil {
		t.Fatalf("查 verdict: %v", err)
	}
	if v.Source != model.VerdictSourceRuleFallback || v.Conclusion != ConclusionReject {
		t.Errorf("verdict source=%s conclusion=%s, want rule_fallback/reject", v.Source, v.Conclusion)
	}
}

// ---- 流水线:LLM 失败降级规则 ----

func TestRunAudit_LLMErrorFallback(t *testing.T) {
	svc, db, cube, _, _ := newTestService(t)
	seedPassData(cube)
	svc.SetVerdictFn(func(_ context.Context, _ *AuditInput) (*VerdictResult, error) {
		return nil, errors.New("llm down")
	})

	ap := submitOK(t, svc, "100")
	ap = waitTerminalPtr(t, db, ap.ID)

	if ap.Status != model.StatusApproved {
		t.Fatalf("status = %s, want approved(降级规则应通过)", ap.Status)
	}
	var v model.LLMVerdict
	if err := db.First(&v, "approval_id = ?", ap.ID).Error; err != nil {
		t.Fatalf("查 verdict: %v", err)
	}
	if v.Source != model.VerdictSourceRuleFallback {
		t.Errorf("source = %s, want rule_fallback", v.Source)
	}
	var logRow model.AuditLog
	if err := db.First(&logRow, "approval_id = ? AND step = ?", ap.ID, StepLLMVerdict).Error; err != nil {
		t.Fatalf("缺 llm_verdict 日志: %v", err)
	}
	if !strings.Contains(logRow.LLMTrace, "降级") {
		t.Errorf("llm_trace 应记录降级原因, got %q", logRow.LLMTrace)
	}
}

// ---- 流水线:建议盘点 ----

func TestRunAudit_SuggestStocktake(t *testing.T) {
	svc, db, cube, pub, st := newTestService(t)
	seedPassData(cube)
	svc.SetVerdictFn(func(_ context.Context, _ *AuditInput) (*VerdictResult, error) {
		return &VerdictResult{
			Conclusion:       ConclusionPass,
			Reason:           "建议盘点 PR1",
			Confidence:       dec("0.9"),
			SuggestStocktake: &StocktakeSuggestion{Scope: "product", IDs: []string{"PR1"}},
			RawJSON:          []byte(`{"conclusion":"pass"}`),
			Source:           model.VerdictSourceLLM,
		}, nil
	})

	ap := submitOK(t, svc, "100")
	ap = waitTerminalPtr(t, db, ap.ID)
	if ap.Status != model.StatusApproved {
		t.Fatalf("status = %s, want approved", ap.Status)
	}

	// stocktake 建单入参
	st.mu.Lock()
	got := st.got
	st.mu.Unlock()
	if got == nil {
		t.Fatal("应调 stocktake 建单")
	}
	if got.BranchID != "B1" || got.OperatorID != "u-1" {
		t.Errorf("建单入参 branch/operator = %s/%s", got.BranchID, got.OperatorID)
	}
	if len(got.ProductIDs) != 1 || got.ProductIDs[0] != "PR1" {
		t.Errorf("product scope 应透传计划商品, got %v", got.ProductIDs)
	}
	// 绑定表
	var link model.ApprovalStocktakeLink
	if err := db.First(&link, "approval_id = ?", ap.ID).Error; err != nil {
		t.Fatalf("缺审批↔盘点绑定: %v", err)
	}
	if link.StocktakeHeaderID != "ST-PLAN-1" {
		t.Errorf("绑定 header = %s, want ST-PLAN-1", link.StocktakeHeaderID)
	}
	// taskcard 待办
	cmds := pub.commands(CmdTaskCard)
	if len(cmds) != 1 {
		t.Fatalf("taskcard 命令数 = %d, want 1", len(cmds))
	}
	var p TaskCardPayload
	if err := json.Unmarshal(cmds[0].Payload, &p); err != nil {
		t.Fatalf("解析 taskcard payload: %v", err)
	}
	if p.URL != "https://h5.example.com/inventory.html?header_id=ST-PLAN-1" {
		t.Errorf("taskcard url = %s", p.URL)
	}
	if p.Title != "结算异常盘点计划" {
		t.Errorf("taskcard title = %s", p.Title)
	}
	if len(p.ToUser) != 1 || p.ToUser[0] != "keeper-1" {
		t.Errorf("to_user = %v", p.ToUser)
	}
	if p.TaskID != "stocktake-plan-ST-PLAN-1" {
		t.Errorf("task_id = %s", p.TaskID)
	}
}

func TestRunAudit_SuggestStocktake_NoClient(t *testing.T) {
	svc, db, cube, _, _ := newTestService(t)
	svc.stocktake = nil // 清掉 newTestService 的注入,验证"未配置 client"路径
	seedPassData(cube)
	svc.SetVerdictFn(func(_ context.Context, _ *AuditInput) (*VerdictResult, error) {
		return &VerdictResult{
			Conclusion:       ConclusionPass,
			Reason:           "建议盘点但无 client",
			Confidence:       dec("0.9"),
			SuggestStocktake: &StocktakeSuggestion{Scope: "supplier", IDs: []string{"SUP1"}},
			RawJSON:          []byte(`{}`),
			Source:           model.VerdictSourceLLM,
		}, nil
	})

	ap := submitOK(t, svc, "100")
	ap = waitTerminalPtr(t, db, ap.ID)
	if ap.Status != model.StatusApproved {
		t.Fatalf("status = %s, want approved(盘点失败不影响主结论)", ap.Status)
	}
	var linkCount int64
	db.Model(&model.ApprovalStocktakeLink{}).Where("approval_id = ?", ap.ID).Count(&linkCount)
	if linkCount != 0 {
		t.Errorf("无 client 不应有绑定, got %d", linkCount)
	}
}

// TestStepStocktake_Idempotent 同一审批重跑时,已有绑定必须跳过:
// 不重复建单、不重复推 taskcard(2026-10-11:RetryApproval/RescanStuck 场景)。
func TestStepStocktake_Idempotent(t *testing.T) {
	svc, db, _, pub, st := newTestService(t)
	ap := &model.ApprovalInstance{
		ID: "ap-idem", CorpID: "wcorp-test", BranchID: "B1",
		ApplicantUserID: "u-1", ApplicantWecomID: "wecom-u1", Status: model.StatusAuditing,
	}
	if err := db.Create(ap).Error; err != nil {
		t.Fatalf("create ap: %v", err)
	}
	db.Create(&model.ApprovalSettlement{ID: "x1", ApprovalID: ap.ID, SettlementID: "CP002610104463"})
	verdict := &VerdictResult{SuggestStocktake: &StocktakeSuggestion{Scope: "product", IDs: []string{"PR1"}}}

	svc.stepStocktake(context.Background(), ap, verdict)
	svc.stepStocktake(context.Background(), ap, verdict) // 模拟重跑

	if st.calls != 1 {
		t.Fatalf("CreatePlanHeader 应只调 1 次(幂等), got %d", st.calls)
	}
	if cmds := pub.commands(CmdTaskCard); len(cmds) != 1 {
		t.Fatalf("taskcard 应只推 1 次, got %d", len(cmds))
	}
	var links []model.ApprovalStocktakeLink
	db.Where("approval_id = ?", ap.ID).Find(&links)
	if len(links) != 1 || links[0].StocktakeHeaderID != "ST-PLAN-1" {
		t.Errorf("绑定表异常: %+v", links)
	}
	// 第二次应写"幂等跳过"step 日志。
	var skipLogs int64
	db.Model(&model.AuditLog{}).Where("approval_id = ? AND output_summary LIKE ?", ap.ID, "%幂等%").Count(&skipLogs)
	if skipLogs != 1 {
		t.Errorf("应有 1 条幂等跳过日志, got %d", skipLogs)
	}
}

// TestRunAudit_CPInventoryAnomaly_FullChain CP 结算单库存异常的端到端规则路径:
// 进货商品窗口内无销售 → block(purchase_without_sales) → 规则降级 verdict
// reject 并派生 product 盘点建议 → 建计划单(带异常商品)+ 绑定 + taskcard
// + notify_text 通知申请人。模拟 CP002610104463 的用户故事(不注入自定义
// verdictFn,走生产默认 defaultVerdictFn)。
func TestRunAudit_CPInventoryAnomaly_FullChain(t *testing.T) {
	svc, db, cube, pub, st := newTestService(t)
	// CP 结算单:货款 100,勾稽一致,但 PR1 进了 10 件、窗口销量为 0(库存异常)。
	cube.mu.Lock()
	cube.settlements = []SettlementRow{
		{ID: "CP002610104463", SupplierID: "SUP9", DocumentType: "CP", ApproveStatus: "1", TotalGoodsYuan: dec("100")},
	}
	cube.lines = map[string][]SettlementLineRow{
		"CP002610104463": {{SettlementID: "CP002610104463", VoucherID: "PI9", SourceDocType: "PI", TotalGoodsYuan: dec("100")}},
	}
	cube.sheets = []PurchaseSheetRow{
		{ID: "PI9", SupplierID: "SUP9", DocumentType: "PI", ApproveStatus: "1"},
	}
	cube.sheetLines = []SheetLineRow{
		{SheetID: "PI9", ProductID: "PR-ANOMALY", ReceivedQuantity: dec("10")},
	}
	cube.sales = nil // 窗口内零销售
	cube.mu.Unlock()

	ap, err := svc.SubmitApproval(context.Background(), "B1", SubmitInput{
		ApplicantUserID:  "u-1",
		ApplicantWecomID: "wecom-u1",
		ApplicantBearer:  "Bearer test-token",
		SettlementIDs:    []string{"CP002610104463"},
		ManualAmountYuan: dec("100"),
		Remark:           "CP 异常模拟",
		ApproverUserIDs:  []string{"apr1"},
	})
	if err != nil {
		t.Fatalf("SubmitApproval: %v", err)
	}
	ap = waitTerminalPtr(t, db, ap.ID)
	if ap.Status != model.StatusRejected {
		t.Fatalf("库存异常 block 应 rejected, got %s (err=%s)", ap.Status, ap.Error)
	}

	// 规则 verdict 应落 reject + product 建议(PR-ANOMALY)。
	var v model.LLMVerdict
	if err := db.First(&v, "approval_id = ?", ap.ID).Error; err != nil {
		t.Fatalf("缺 verdict: %v", err)
	}
	if v.Conclusion != ConclusionReject || v.Source != model.VerdictSourceRuleFallback {
		t.Errorf("verdict = %s/%s, want reject/rule_fallback", v.Conclusion, v.Source)
	}
	if !strings.Contains(string(v.SuggestStocktake), "PR-ANOMALY") ||
		!strings.Contains(string(v.SuggestStocktake), `"scope":"product"`) {
		t.Errorf("verdict 应含 product 建议 PR-ANOMALY: %s", string(v.SuggestStocktake))
	}

	// 计划单应圈定异常商品。
	st.mu.Lock()
	got := st.got
	st.mu.Unlock()
	if got == nil {
		t.Fatal("应调 stocktake 建计划单")
	}
	if got.BranchID != "B1" || len(got.ProductIDs) != 1 || got.ProductIDs[0] != "PR-ANOMALY" {
		t.Errorf("计划单入参异常: %+v", got)
	}
	if !strings.Contains(got.Remark, "CP002610104463") {
		t.Errorf("计划单备注应带来源结算单号: %s", got.Remark)
	}

	// 绑定 + taskcard + 申请人拒绝通知。
	var link model.ApprovalStocktakeLink
	if err := db.First(&link, "approval_id = ?", ap.ID).Error; err != nil {
		t.Fatalf("缺盘点绑定: %v", err)
	}
	if cmds := pub.commands(CmdTaskCard); len(cmds) != 1 {
		t.Fatalf("taskcard 应推 1 次, got %d", len(cmds))
	} else {
		var p TaskCardPayload
		_ = json.Unmarshal(cmds[0].Payload, &p)
		if !strings.Contains(p.Description, "CP002610104463") ||
			!strings.Contains(p.URL, "/inventory.html?header_id=ST-PLAN-1") {
			t.Errorf("taskcard 文案/链接异常: %+v", p)
		}
	}
	if cmds := pub.commands(CmdNotifyText); len(cmds) != 1 {
		t.Fatalf("reject 应通知申请人 1 次, got %d", len(cmds))
	}
}

// ---- 失败与重跑 ----

func TestRunAudit_CubeErrorThenRetry(t *testing.T) {
	svc, db, cube, _, _ := newTestService(t)
	cube.mu.Lock()
	cube.settlementsErr = errors.New("cube down")
	cube.mu.Unlock()

	ap := submitOK(t, svc, "100")
	ap = waitTerminalPtr(t, db, ap.ID)
	if ap.Status != model.StatusFailed {
		t.Fatalf("status = %s, want failed", ap.Status)
	}
	if !strings.Contains(ap.Error, "fetch settlements") {
		t.Errorf("error = %q, 应含 fetch settlements", ap.Error)
	}

	// cube 恢复后人工重跑(重跑请求侧带新 JWT)
	seedPassData(cube)
	if err := svc.RetryApproval(context.Background(), ap.ID, "Bearer retry-token"); err != nil {
		t.Fatalf("RetryApproval: %v", err)
	}
	ap = waitTerminalPtr(t, db, ap.ID)
	if ap.Status != model.StatusApproved {
		t.Fatalf("重跑后 status = %s, want approved(err=%s)", ap.Status, ap.Error)
	}
	if ap.Error != "" {
		t.Errorf("重跑成功后 error 应清空, got %q", ap.Error)
	}
}

func TestRetryApproval_NotFailed(t *testing.T) {
	svc, db, _, _, _ := newTestService(t)
	db.Create(&model.ApprovalInstance{
		ID: "ap-ok", BranchID: "B1", Status: model.StatusApproved, ManualAmountYuan: dec("1"),
	})
	if err := svc.RetryApproval(context.Background(), "ap-ok", "Bearer t"); !errors.Is(err, ErrInvalidStatus) {
		t.Errorf("err = %v, want ErrInvalidStatus", err)
	}
	if err := svc.RetryApproval(context.Background(), "no-such", "Bearer t"); !errors.Is(err, ErrApprovalNotFound) {
		t.Errorf("err = %v, want ErrApprovalNotFound", err)
	}
}

func TestRescanStuck(t *testing.T) {
	svc, db, cube, _, _ := newTestService(t)
	seedPassData(cube)
	old := fixedNow.Add(-20 * time.Minute)
	db.Create(&model.ApprovalInstance{
		ID: "ap-stuck", BranchID: "B1", Status: model.StatusReceived,
		ApplicantUserID: "u-1", ApplicantWecomID: "wecom-u1",
		ApproverUserIDs: datatypes.JSON([]byte(`["apr1"]`)),
		ManualAmountYuan: dec("1"), CreatedAt: old, UpdatedAt: old,
	})
	db.Create(&model.ApprovalInstance{
		ID: "ap-done", BranchID: "B1", Status: model.StatusApproved,
		ManualAmountYuan: dec("1"), CreatedAt: old, UpdatedAt: old,
	})
	n, err := svc.RescanStuck(context.Background())
	if err != nil {
		t.Fatalf("RescanStuck: %v", err)
	}
	if n != 1 {
		t.Errorf("rescanned = %d, want 1(仅 received 且超时)", n)
	}
}

// ---- 企微回调 ----

func TestOnStatusChanged(t *testing.T) {
	svc, db, _, _, _ := newTestService(t)
	mk := func(id, spNo string) {
		db.Create(&model.ApprovalInstance{
			ID: id, BranchID: "B1", ApplicantUserID: "u-1", Status: model.StatusApproved,
			SpNo: spNo, ManualAmountYuan: dec("1"),
		})
	}
	cases := []struct {
		spStatus  int
		want      string
		unchanged bool
	}{
		{SpStatusApproved, model.StatusHumanApproved, false},
		{SpStatusRejected, model.StatusHumanRejected, false},
		{SpStatusCancelled, model.StatusCancelled, false},
		{SpStatusCancelAfterOK, model.StatusCancelled, false},
		{SpStatusApproving, model.StatusApproved, true}, // 审批中:状态不变,仅更新 sp_status
	}
	for i, c := range cases {
		id := "ap-sc-" + string(rune('a'+i))
		spNo := "SP-" + id
		mk(id, spNo)
		if err := svc.OnStatusChanged(context.Background(), &WecomStatusChanged{SpNo: spNo, SpStatus: c.spStatus}); err != nil {
			t.Fatalf("OnStatusChanged(%d): %v", c.spStatus, err)
		}
		var ap model.ApprovalInstance
		db.First(&ap, "id = ?", id)
		if ap.Status != c.want {
			t.Errorf("sp_status=%d → status=%s, want %s", c.spStatus, ap.Status, c.want)
		}
		if ap.SpStatus != c.spStatus {
			t.Errorf("sp_status 未回写: %d, want %d", ap.SpStatus, c.spStatus)
		}
	}
	// 未知 sp_no → 忽略不报错
	if err := svc.OnStatusChanged(context.Background(), &WecomStatusChanged{SpNo: "NOPE", SpStatus: 2}); err != nil {
		t.Errorf("未知 sp_no 应忽略, got %v", err)
	}
	// 空事件 → 报错
	if err := svc.OnStatusChanged(context.Background(), nil); err == nil {
		t.Error("nil 事件应报错")
	}
}

func TestOnCommandResult(t *testing.T) {
	svc, db, _, _, _ := newTestService(t)
	mk := func(id, cmdID, status string) {
		db.Create(&model.ApprovalInstance{
			ID: id, BranchID: "B1", ApplicantUserID: "u-1", Status: status,
			LastCommandID: cmdID, ManualAmountYuan: dec("1"),
		})
	}

	// apply_event ok → 回填 sp_no + sp_status=审批中
	mk("ap-cr-1", "CMD-1", model.StatusApproved)
	if err := svc.OnCommandResult(context.Background(), &WecomCommandResult{
		CommandID: "CMD-1", Type: CmdApplyEvent, OK: true, SpNo: "SP123",
	}); err != nil {
		t.Fatalf("OnCommandResult ok: %v", err)
	}
	var ap1 model.ApprovalInstance
	db.First(&ap1, "id = ?", "ap-cr-1")
	if ap1.SpNo != "SP123" || ap1.SpStatus != SpStatusApproving {
		t.Errorf("回填 sp_no=%s sp_status=%d, want SP123/1", ap1.SpNo, ap1.SpStatus)
	}
	if ap1.Status != model.StatusApproved {
		t.Errorf("ok 不应改 status, got %s", ap1.Status)
	}

	// apply_event 失败 → failed(可重跑)
	mk("ap-cr-2", "CMD-2", model.StatusApproved)
	if err := svc.OnCommandResult(context.Background(), &WecomCommandResult{
		CommandID: "CMD-2", Type: CmdApplyEvent, OK: false, Error: "企微限流",
	}); err != nil {
		t.Fatalf("OnCommandResult fail: %v", err)
	}
	var ap2 model.ApprovalInstance
	db.First(&ap2, "id = ?", "ap-cr-2")
	if ap2.Status != model.StatusFailed || !strings.Contains(ap2.Error, "企微限流") {
		t.Errorf("失败后 status=%s error=%q, want failed/含原因", ap2.Status, ap2.Error)
	}

	// 通知类命令失败 → 状态不变
	mk("ap-cr-3", "CMD-3", model.StatusRejected)
	if err := svc.OnCommandResult(context.Background(), &WecomCommandResult{
		CommandID: "CMD-3", Type: CmdNotifyText, OK: false, Error: "用户未关注",
	}); err != nil {
		t.Fatalf("OnCommandResult notify: %v", err)
	}
	var ap3 model.ApprovalInstance
	db.First(&ap3, "id = ?", "ap-cr-3")
	if ap3.Status != model.StatusRejected {
		t.Errorf("通知类失败不应改状态, got %s", ap3.Status)
	}

	// 未知 command_id → 忽略
	if err := svc.OnCommandResult(context.Background(), &WecomCommandResult{
		CommandID: "NOPE", Type: CmdApplyEvent, OK: true,
	}); err != nil {
		t.Errorf("未知 command_id 应忽略, got %v", err)
	}
}

// ---- 查询 ----

func TestGetApprovalDetail_BranchIsolation(t *testing.T) {
	svc, db, _, _, _ := newTestService(t)
	db.Create(&model.ApprovalInstance{
		ID: "ap-1", BranchID: "B1", Status: model.StatusApproved, ManualAmountYuan: dec("100"),
	})
	db.Create(&model.ApprovalSettlement{
		ID: "snap-1", ApprovalID: "ap-1", SettlementID: "S1", GoodsAmountYuan: dec("100"), Checked: true,
	})
	db.Create(&model.AuditSignal{
		ID: "sig-1", ApprovalID: "ap-1", SignalType: SigQtyDeviation, Level: model.SignalWarn, Message: "m",
	})

	// 跨 branch 视为不存在
	if _, err := svc.GetApprovalDetail(context.Background(), "B2", "ap-1"); !errors.Is(err, ErrApprovalNotFound) {
		t.Errorf("跨 branch err = %v, want ErrApprovalNotFound", err)
	}
	// 本 branch 正常读,聚合齐全
	d, err := svc.GetApprovalDetail(context.Background(), "B1", "ap-1")
	if err != nil {
		t.Fatalf("GetApprovalDetail: %v", err)
	}
	if len(d.Settlements) != 1 || len(d.Signals) != 1 {
		t.Errorf("聚合 settlements=%d signals=%d, want 1/1", len(d.Settlements), len(d.Signals))
	}
	// 不存在
	if _, err := svc.GetApprovalDetail(context.Background(), "B1", "nope"); !errors.Is(err, ErrApprovalNotFound) {
		t.Errorf("err = %v, want ErrApprovalNotFound", err)
	}
}

func TestListApprovals_Filters(t *testing.T) {
	svc, db, _, _, _ := newTestService(t)
	mk := func(id, branch, status string) {
		db.Create(&model.ApprovalInstance{
			ID: id, BranchID: branch, Status: status, ManualAmountYuan: dec("1"),
		})
	}
	mk("a1", "B1", model.StatusApproved)
	mk("a2", "B1", model.StatusRejected)
	mk("a3", "B1", model.StatusFailed)
	mk("a4", "B2", model.StatusApproved)

	items, total, err := svc.ListApprovals(context.Background(), "B1", "", "", "", 50, 0)
	if err != nil {
		t.Fatalf("ListApprovals: %v", err)
	}
	if total != 3 || len(items) != 3 {
		t.Errorf("B1 total=%d len=%d, want 3/3", total, len(items))
	}
	items, total, err = svc.ListApprovals(context.Background(), "B1", model.StatusApproved, "", "", 50, 0)
	if err != nil {
		t.Fatalf("ListApprovals filter: %v", err)
	}
	if total != 1 || items[0].ID != "a1" {
		t.Errorf("status 过滤 total=%d, want 1(a1)", total)
	}
	// 缺 branch
	if _, _, err := svc.ListApprovals(context.Background(), "", "", "", "", 50, 0); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("err = %v, want ErrInvalidInput", err)
	}
}

func TestExportApprovals(t *testing.T) {
	svc, db, _, _, _ := newTestService(t)
	db.Create(&model.ApprovalInstance{
		ID: "ap-1", BranchID: "B1", Status: model.StatusHumanApproved, SpNo: "SP1",
		ApplicantUserID: "u-1", ManualAmountYuan: dec("100"),
	})
	db.Create(&model.ApprovalSettlement{ID: "s1", ApprovalID: "ap-1", SettlementID: "S1", GoodsAmountYuan: dec("60")})
	db.Create(&model.ApprovalSettlement{ID: "s2", ApprovalID: "ap-1", SettlementID: "S2", GoodsAmountYuan: dec("40")})

	rows, err := svc.ExportApprovals(context.Background(), "B1", "", "")
	if err != nil {
		t.Fatalf("ExportApprovals: %v", err)
	}
	if len(rows) != 1 || len(rows[0].Settlements) != 2 {
		t.Fatalf("导出 rows=%d settlements=%d, want 1/2", len(rows), len(rows[0].Settlements))
	}
	if rows[0].Approval.SpNo != "SP1" {
		t.Errorf("sp_no = %s", rows[0].Approval.SpNo)
	}
	if _, err := svc.ExportApprovals(context.Background(), "", "", ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("err = %v, want ErrInvalidInput", err)
	}
}

// ---- 提交参数组装 ----

func TestBuildApplyData(t *testing.T) {
	svc, db, _, _, _ := newTestService(t)
	db.Create(&model.ApprovalInstance{
		ID: "ap-x", BranchID: "B1", Status: model.StatusApproved,
		ManualAmountYuan: dec("123.45"), Remark: "9月货款",
	})
	db.Create(&model.ApprovalSettlement{ID: "s1", ApprovalID: "ap-x", SettlementID: "S1"})
	db.Create(&model.ApprovalSettlement{ID: "s2", ApprovalID: "ap-x", SettlementID: "S2"})
	var ap model.ApprovalInstance
	db.First(&ap, "id = ?", "ap-x")

	verdict := &VerdictResult{
		Conclusion: ConclusionPass,
		Reason:     "金额勾稽一致,采购单均已审核",
		Confidence: dec("0.95"),
		Source:     model.VerdictSourceLLM,
	}
	applyData, summary := svc.buildApplyData(context.Background(), &ap, verdict, []signalDraft{{Level: model.SignalWarn}})
	ad := string(applyData)
	if !strings.Contains(ad, "S1") || !strings.Contains(ad, "S2") {
		t.Errorf("apply_data 应含全部结算单号: %s", ad)
	}
	if !strings.Contains(ad, "123.45") {
		t.Errorf("apply_data 应含付款金额: %s", ad)
	}
	if !strings.Contains(ad, "机器一审通过") || !strings.Contains(ad, "1 条警告") {
		t.Errorf("有 warn 信号时备注应附加提示: %s", ad)
	}
	if !strings.Contains(string(summary), "2 张") {
		t.Errorf("summary 应含结算单张数: %s", summary)
	}
	// 未配置 AIConclusionControlID → 不出现第 4 个控件(向后兼容)。
	if strings.Contains(ad, "机器审核通过:") {
		t.Errorf("未配置 AI 结论控件时不应写入结论: %s", ad)
	}
}

// TestBuildApplyData_AIConclusion 验证「AI审核结论」控件接线:
// 配置非空 + pass 结论 → apply_data 追加 Textarea 控件写入精简结论;
// verdict 缺失 → 跳过控件不阻塞提交。
func TestBuildApplyData_AIConclusion(t *testing.T) {
	svc, db, _, _, _ := newTestService(t)
	svc.cfg.AIConclusionControlID = "Textarea-1791692081606"
	db.Create(&model.ApprovalInstance{
		ID: "ap-ai", BranchID: "B1", Status: model.StatusApproved,
		ManualAmountYuan: dec("100"), Remark: "9月货款",
	})
	db.Create(&model.ApprovalSettlement{ID: "s1", ApprovalID: "ap-ai", SettlementID: "S1"})
	var ap model.ApprovalInstance
	db.First(&ap, "id = ?", "ap-ai")

	verdict := &VerdictResult{
		Conclusion: ConclusionPass,
		Reason:     "金额勾稽一致,采购单均已审核",
		Confidence: dec("0.95"),
		Source:     model.VerdictSourceLLM,
	}
	applyData, _ := svc.buildApplyData(context.Background(), &ap, verdict, nil)
	ad := string(applyData)
	if !strings.Contains(ad, `"Textarea-1791692081606"`) {
		t.Errorf("apply_data 应含 AI 结论控件 id: %s", ad)
	}
	if !strings.Contains(ad, "机器审核通过:金额勾稽一致") {
		t.Errorf("AI 结论控件应含精简结论: %s", ad)
	}

	// verdict 缺失 → 跳过控件(不写 AI 结论,也不报错)。
	applyData, _ = svc.buildApplyData(context.Background(), &ap, nil, nil)
	if strings.Contains(string(applyData), `"Textarea-1791692081606"`) {
		t.Errorf("verdict 缺失时应跳过 AI 结论控件: %s", applyData)
	}
}

// TestBuildApplyData_AIConclusion_RejectBlock 验证 reject 防御路径:
// 取首条 block 信号且 ≤40 字(实际流程 reject 不发 applyevent,此为兜底)。
func TestBuildApplyData_AIConclusion_RejectBlock(t *testing.T) {
	svc, db, _, _, _ := newTestService(t)
	svc.cfg.AIConclusionControlID = "Textarea-1791692081606"
	db.Create(&model.ApprovalInstance{
		ID: "ap-rj", BranchID: "B1", Status: model.StatusApproved,
		ManualAmountYuan: dec("100"), Remark: "",
	})
	db.Create(&model.ApprovalSettlement{ID: "s1", ApprovalID: "ap-rj", SettlementID: "S1"})
	var ap model.ApprovalInstance
	db.First(&ap, "id = ?", "ap-rj")

	longBlock := "结算单 S1 金额勾稽不上:手工 100.00,明细合计 99999999.00 元,差额巨大需要人工复核确认"
	verdict := &VerdictResult{Conclusion: ConclusionReject, Reason: "规则审核拒绝:" + longBlock}
	signals := []signalDraft{
		{Level: model.SignalWarn, Message: "警告信号"},
		{Level: model.SignalBlock, Message: longBlock},
	}
	applyData, _ := svc.buildApplyData(context.Background(), &ap, verdict, signals)
	ad := string(applyData)
	got := truncateRunes(longBlock, 40)
	if !strings.Contains(ad, `"text":"`+got+`"`) {
		t.Errorf("reject 时应写入首条 block 信号(≤40 字): got=%s", ad)
	}
	if strings.Contains(ad, longBlock) {
		t.Errorf("block 信号应被截断到 40 字: %s", ad)
	}
}

// TestTruncateRunes 验证按 rune 数截断:中文不产生半个字符,长度可控。
func TestTruncateRunes(t *testing.T) {
	in := "金额勾稽一致,采购单均已审核无异常" // 16 个 rune
	if got := truncateRunes(in, 10); len([]rune(got)) != 10 || got != string([]rune(in)[:10]) {
		t.Errorf("truncateRunes(中文,10) = %q", got)
	}
	if got := truncateRunes(in, 100); got != in {
		t.Errorf("未超上限应原样返回: %q", got)
	}
	if got := truncateRunes("abc", 2); got != "ab" {
		t.Errorf("truncateRunes(ascii,2) = %q", got)
	}
}

func TestSaleWindowDates(t *testing.T) {
	svc, _, _, _, _ := newTestService(t)
	from, to := svc.saleWindowDates()
	if to != "2026-10-01" || from != "2026-09-01" {
		t.Errorf("窗口 = %s~%s, want 2026-09-01~2026-10-01(30 天)", from, to)
	}
}

func TestSubmitApproval_DedupeIDs(t *testing.T) {
	svc, db, cube, _, _ := newTestService(t)
	seedPassData(cube)
	// 追加 S2(货款 50,同样全通过)
	cube.mu.Lock()
	cube.settlements = append(cube.settlements, SettlementRow{
		ID: "S2", SupplierID: "SUP1", DocumentType: "DP", ApproveStatus: "1", TotalGoodsYuan: dec("50"),
	})
	cube.lines["S2"] = []SettlementLineRow{{SettlementID: "S2", VoucherID: "P2", SourceDocType: "PI", TotalGoodsYuan: dec("50")}}
	cube.sheets = append(cube.sheets, PurchaseSheetRow{ID: "P2", SupplierID: "SUP1", DocumentType: "PI", ApproveStatus: "1"})
	cube.sheetLines = append(cube.sheetLines, SheetLineRow{SheetID: "P2", ProductID: "PR1", ReceivedQuantity: dec("5")})
	cube.mu.Unlock()

	ap, err := svc.SubmitApproval(context.Background(), "B1", SubmitInput{
		ApplicantUserID:  "u-1",
		ApplicantWecomID: "wecom-u1",
		ApplicantBearer:  "Bearer test-token",
		SettlementIDs:    []string{"S1", "S1", " S2 "},
		ManualAmountYuan: dec("150"),
		ApproverUserIDs:  []string{"apr1"},
	})
	if err != nil {
		t.Fatalf("SubmitApproval: %v", err)
	}
	waitTerminal(t, db, ap.ID)
	var count int64
	db.Model(&model.ApprovalSettlement{}).Where("approval_id = ?", ap.ID).Count(&count)
	if count != 2 {
		t.Errorf("快照条数 = %d, want 2(重复 id 去重)", count)
	}
}
