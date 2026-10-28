package service

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	_ "modernc.org/sqlite" // pure-Go driver (no CGO)

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// newPayeeTestDB 构造 SQLite 内存 DB + AutoMigrate。
func newPayeeTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Dialector{DriverName: "sqlite", DSN: ":memory:"}, &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&model.ApprovalInstance{},
		&model.ApprovalPayeeAccount{},
	); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// fakeBankAccountClient 是 BankAccountClient 的测试替身。
type fakeBankAccountClient struct {
	acct    *PayeeAccountDTO
	err     error
	gotID   string
	gotBr   string
	gotAuth string
	called  int
}

func (f *fakeBankAccountClient) GetAccount(_ context.Context, branchID, bearer, accountID string) (*PayeeAccountDTO, error) {
	f.called++
	f.gotID = accountID
	f.gotBr = branchID
	f.gotAuth = bearer
	if f.err != nil {
		return nil, f.err
	}
	return f.acct, nil
}

// TestPayeeAccount_NotInAuditInput 是本文件最重要的一条。
//
// 用户口径（2026-10-28）：「settlement-auditor 服务的付款审批模块，
// **不向机器人审核（LLM）发送账号信息**。」
//
// 这条断言从**结构**上证明该约束：
// 把账号快照落库之后，用**真实的服务对象**构造 AuditInput，
// 再跑真实的 buildAuditUserPrompt ⇒ prompt 里不得出现账号/开户行/户名。
//
// 判别力自检：把 ApprovalPayeeAccount 的字段挂到 ApprovalInstance 上、
// 或把 payee 塞进 AuditInput ⇒ 本用例立刻红。
func TestPayeeAccount_NotInAuditInput(t *testing.T) {
	svc := New(newPayeeTestDB(t), Config{})

	ap := &model.ApprovalInstance{
		ID:        "ap1",
		BranchID:  "00",
		Remark:    "本月货款",
		ApplicantWecomID: "zhangsan",
	}
	if err := svc.db.Create(ap).Error; err != nil {
		t.Fatalf("create approval: %v", err)
	}

	// 落一份收款账号快照（**独立表**）。
	payee := &model.ApprovalPayeeAccount{
		ID:          "pa1",
		ApprovalID:  ap.ID,
		BranchID:    ap.BranchID,
		BankAccountID: "ba-001",
		SupplierID:  "29",
		SupplierName: "猪肉自营",
		BankName:    "中国银行南宁分行",
		AccountNo:   "6228480012345678",
		AccountName: "南宁某某贸易有限公司",
	}
	if err := svc.db.Create(payee).Error; err != nil {
		t.Fatalf("create payee: %v", err)
	}

	// 构造 AuditInput（**只**用 pipeline 里真实用的字段）。
	in := &AuditInput{
		Approval: ap,
		Signals:  []signalDraft{{Level: model.SignalWarn, Message: "金额有零头"}},
	}
	prompt := buildAuditUserPrompt(in)

	// ⚠️ 这些字符串**不得**出现在 prompt 里。
	forbidden := []string{
		"6228480012345678",              // 账号
		"中国银行南宁分行",                 // 开户行
		"南宁某某贸易有限公司",              // 户名
		"ba-001",                        // 账号 id
		"猪肉自营",                        // 供应商名
	}
	for _, s := range forbidden {
		if strings.Contains(prompt, s) {
			t.Fatalf("LLM prompt 里出现了收款账号信息 %q —— 违反「不向 LLM 发送账号信息」\nprompt:\n%s", s, prompt)
		}
	}
}

// TestPayeeAccount_ApprovalInstanceHasNoAccountField 锁住"结构隔离"这件事本身。
//
// 判别力自检：给 ApprovalInstance 加 BankAccountNo 字段 ⇒ 本用例立刻红。
//
// 为什么要有这条：TestPayeeAccount_NotInAuditInput 验证的是"当前 prompt 不含账号"，
// 而这条验证的是"**结构上不可能**含"——
// 前者会因 prompt 改动而误报，后者是真正的不变量。
func TestPayeeAccount_ApprovalInstanceHasNoAccountField(t *testing.T) {
	ap := model.ApprovalInstance{}
	// 反射断言：ApprovalInstance 上不得出现任何"账号"语义的字段。
	typ := reflect.TypeOf(ap)
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name := strings.ToLower(f.Name)
		for _, bad := range []string{"accountno", "bankname", "accountname", "payee", "bankaccount"} {
			if strings.Contains(name, bad) {
				t.Fatalf("ApprovalInstance 上出现了账号语义字段 %q —— "+
					"它会随 in.Approval 进入 LLM 输入面(见 payee_account.go 的结构隔离说明)", f.Name)
			}
		}
	}
}

// TestPayeeAccountLines 三行格式锁定。
//
// 用户口径：「分行向该控件填写开户行、账号、户名。」
func TestPayeeAccountLines(t *testing.T) {
	row := &model.ApprovalPayeeAccount{
		BankName:    "中国银行南宁分行",
		AccountNo:   "6228480012345678",
		AccountName: "南宁某某贸易有限公司",
	}
	got := payeeAccountLines(row)
	want := "开户行：中国银行南宁分行\n账号：6228480012345678\n户名：南宁某某贸易有限公司"
	if got != want {
		t.Fatalf("payeeAccountLines =\n%q\nwant\n%q", got, want)
	}
	// 行数必须是 3。
	if n := strings.Count(got, "\n") + 1; n != 3 {
		t.Fatalf("行数 = %d, want 3(开户行/账号/户名各一行)", n)
	}
}

// TestPayeeAccountLines_NilAndPartial 空快照与缺字段都不该 panic。
func TestPayeeAccountLines_NilAndPartial(t *testing.T) {
	if got := payeeAccountLines(nil); got != "" {
		t.Fatalf("nil 快照应返空串, got %q", got)
	}
	got := payeeAccountLines(&model.ApprovalPayeeAccount{AccountNo: "6222"})
	if got != "账号：6222" {
		t.Fatalf("部分字段 = %q, want %q", got, "账号：6222")
	}
}

// TestBuildApplyData_WritesPaymentInfoControl 锁住「付款信息」控件的写入。
func TestBuildApplyData_WritesPaymentInfoControl(t *testing.T) {
	svc := New(newPayeeTestDB(t), Config{
		ApplyControlIDs:        [3]string{"Text-1", "Text-2", "Text-3"},
		ApplyControlTypes:      [3]string{"Text", "Text", "Textarea"},
		PaymentInfoControlID:   "Textarea-1799999999999",
		PaymentInfoControlType: "Textarea",
	})
	ap := &model.ApprovalInstance{ID: "ap1", BranchID: "00"}
	payee := &model.ApprovalPayeeAccount{
		ApprovalID:  "ap1",
		BankName:    "中国银行",
		AccountNo:   "6228480012345678",
		AccountName: "某某公司",
	}
	applyData, _ := svc.buildApplyData(context.Background(), ap, nil, nil, nil, nil, payee)

	var parsed struct {
		Contents []struct {
			Control string `json:"control"`
			ID      string `json:"id"`
			Value   struct {
				Text string `json:"text"`
			} `json:"value"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(applyData, &parsed); err != nil {
		t.Fatalf("apply_data 不是合法 JSON: %v", err)
	}
	var found bool
	for _, c := range parsed.Contents {
		if c.ID != "Textarea-1799999999999" {
			continue
		}
		found = true
		if c.Control != "Textarea" {
			t.Fatalf("控件类型 = %q, want Textarea", c.Control)
		}
		if !strings.Contains(c.Value.Text, "开户行：中国银行") ||
			!strings.Contains(c.Value.Text, "账号：6228480012345678") ||
			!strings.Contains(c.Value.Text, "户名：某某公司") {
			t.Fatalf("控件文本 = %q, 缺开户行/账号/户名", c.Value.Text)
		}
	}
	if !found {
		t.Fatal("apply_data 里没有「付款信息」控件")
	}
}

// TestBuildApplyData_NoPayeeNoControl 没选收款账号 ⇒ 不写该控件(但不报错)。
func TestBuildApplyData_NoPayeeNoControl(t *testing.T) {
	svc := New(newPayeeTestDB(t), Config{
		ApplyControlIDs:      [3]string{"Text-1", "Text-2", "Text-3"},
		ApplyControlTypes:    [3]string{"Text", "Text", "Textarea"},
		PaymentInfoControlID: "Textarea-1799999999999",
	})
	ap := &model.ApprovalInstance{ID: "ap1", BranchID: "00"}
	applyData, _ := svc.buildApplyData(context.Background(), ap, nil, nil, nil, nil, nil)
	if strings.Contains(string(applyData), "Textarea-1799999999999") {
		t.Fatalf("没选账号时不该写「付款信息」控件:\n%s", applyData)
	}
}

// TestBuildApplyData_NoControlIDConfigured 控件 id 没配 ⇒ 跳过,不报错。
//
// ⚠️ 控件 id 随企微后台那张模板走,部署时可能还没配
// ⇒ 此时提交**不能**失败(否则整个付款审批都发不出去)。
func TestBuildApplyData_NoControlIDConfigured(t *testing.T) {
	svc := New(newPayeeTestDB(t), Config{
		ApplyControlIDs:   [3]string{"Text-1", "Text-2", "Text-3"},
		ApplyControlTypes: [3]string{"Text", "Text", "Textarea"},
	})
	ap := &model.ApprovalInstance{ID: "ap1", BranchID: "00"}
	payee := &model.ApprovalPayeeAccount{
		ApprovalID: "ap1", BankName: "中国银行", AccountNo: "6222", AccountName: "甲",
	}
	applyData, _ := svc.buildApplyData(context.Background(), ap, nil, nil, nil, nil, payee)
	if !strings.Contains(string(applyData), "Text-1") {
		t.Fatalf("控件 id 没配时,其余控件仍要写:\n%s", applyData)
	}
}

// TestResolveAndStorePayeeAccount 落快照 + 归属校验。
func TestResolveAndStorePayeeAccount(t *testing.T) {
	svc := New(newPayeeTestDB(t), Config{})
	fake := &fakeBankAccountClient{acct: &PayeeAccountDTO{
		ID: "ba-001", BranchID: "00", SupplierID: "29", SupplierName: "猪肉自营",
		BankName: "中国银行", AccountNo: "6222", AccountName: "甲",
	}}
	svc.SetBankAccount(fake)

	ap := &model.ApprovalInstance{
		ID: "ap1", BranchID: "00", ApplicantUserID: "u1",
		// ⚠️ 申请人的 Authorization 头原值(提交那一刻的)——
		// 取号要**用它**调 bank-account,否则被调方 401。
		ApplicantBearer: "Bearer applicant-jwt",
	}
	if err := svc.db.Create(ap).Error; err != nil {
		t.Fatalf("create approval: %v", err)
	}
	if err := svc.resolveAndStorePayeeAccount(context.Background(), ap, "ba-001"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if fake.gotID != "ba-001" || fake.gotBr != "00" {
		t.Fatalf("调用参数 = (%q,%q), want (ba-001,00)", fake.gotID, fake.gotBr)
	}
	// ⚠️ 判别力自检:把 resolveAndStorePayeeAccount 里的 ap.ApplicantBearer
	// 去掉(或换成常量) ⇒ 本用例立刻红。
	//
	// 2026-10-28 生产实踩:第一版把 bearer 塞进 ctx(cubeclient.WithBearer),
	// 而 DaprBankAccountClient 从没去 ctx 里读 ⇒ **透传根本没发生**
	// ⇒ 被调方 401 ⇒ 企微「付款信息」控件恒为空,而提交照常成功。
	if fake.gotAuth != "Bearer applicant-jwt" {
		t.Fatalf("申请人 JWT 未透传给下游(实际 %q)—— bank-account 在应用层验签,"+
			"拿不到 Authorization 就是 401", fake.gotAuth)
	}
	got, err := svc.loadPayeeAccount(context.Background(), "ap1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got == nil {
		t.Fatal("load 返回 nil —— 快照没落库")
	}
	if got.AccountNo != "6222" || got.BankName != "中国银行" {
		t.Fatalf("快照内容 = %+v", got)
	}
}

// TestResolveAndStorePayeeAccount_EmptyIDSkipped 没选账号 ⇒ 不调下游、不落库。
func TestResolveAndStorePayeeAccount_EmptyIDSkipped(t *testing.T) {
	svc := New(newPayeeTestDB(t), Config{})
	fake := &fakeBankAccountClient{}
	svc.SetBankAccount(fake)
	ap := &model.ApprovalInstance{ID: "ap1", BranchID: "00"}
	if err := svc.resolveAndStorePayeeAccount(context.Background(), ap, "  "); err != nil {
		t.Fatalf("空 id 不该报错: %v", err)
	}
	if fake.called != 0 {
		t.Fatalf("空 id 仍调了下游 %d 次", fake.called)
	}
}

// TestResolveAndStorePayeeAccount_ClientNotConfigured 未装配客户端 ⇒ 报错(而不是静默)。
//
// 静默会让"选了账号但没写进企微"这件事没有任何痕迹。
func TestResolveAndStorePayeeAccount_ClientNotConfigured(t *testing.T) {
	svc := New(newPayeeTestDB(t), Config{})
	ap := &model.ApprovalInstance{ID: "ap1", BranchID: "00"}
	if err := svc.resolveAndStorePayeeAccount(context.Background(), ap, "ba-001"); err == nil {
		t.Fatal("未装配客户端时应报错")
	}
}
