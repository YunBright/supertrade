package service

import (
	"context"
	"encoding/json"
	"testing"

	dapr "github.com/dapr/go-sdk/client"
	"google.golang.org/grpc/metadata"
)

// fakeDaprClient 只覆写 InvokeMethodWithContent。
//
// 其余方法靠嵌入的 nil 接口 —— 调用它们会 panic，
// 但本文件只测那一个方法，所以安全（标准做法）。
type fakeDaprClient struct {
	dapr.Client
	gotAppID   string
	gotMethod  string
	gotVerb    string
	gotContent *dapr.DataContent
	gotBranch  string
	gotAuth    string
	resp       []byte
	err        error
}

func (f *fakeDaprClient) InvokeMethodWithContent(ctx context.Context, appID, method, verb string, content *dapr.DataContent) ([]byte, error) {
	f.gotAppID = appID
	f.gotMethod = method
	f.gotVerb = verb
	f.gotContent = content
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		if v := md.Get("x-branch-id"); len(v) > 0 {
			f.gotBranch = v[0]
		}
		if v := md.Get("authorization"); len(v) > 0 {
			f.gotAuth = v[0]
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// TestDaprBankAccountClient_GetAccount 锁住 dapr 调用形状。
//
// # 为什么这条必须有（2026-10-28 生产实踩）
//
// 第一版写的是 `InvokeMethodWithContent(ctx, appID, method, "GET", nil)` ——
// **GET 也必须传非 nil 的 content**，go-sdk 在**发出请求之前**就返回
// "content required"。
//
// # 为什么这个 bug 极难靠"看功能有没有生效"发现
//
// 取号失败走的是 `logPayeeFetchError` 分支：**只 warn，不阻断提交**。
// ⇒ 现象是：审批单**照常提交、照常走完审核**，
//   只有企微的「付款信息」控件**永远是空的** ——
//   而这在页面上几乎看不出来（那个控件本来就看不见）。
//
// ⇒ 只能靠**单测钉住调用形状**。
//
// ⚠️ 判别力自检：把 `&dapr.DataContent{}` 改回 `nil` ⇒ 本用例立刻红。
func TestDaprBankAccountClient_GetAccount(t *testing.T) {
	raw, err := json.Marshal(PayeeAccountDTO{
		ID:           "ba-1",
		BranchID:     "00",
		SupplierID:   "S-77",
		SupplierName: "某贸易",
		BankName:     "中国银行",
		AccountNo:    "6222",
		AccountName:  "某贸易有限公司",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	fake := &fakeDaprClient{resp: raw}
	cli := NewDaprBankAccountClient(fake, "supertrade-bank-account")

	const bearer = "Bearer test-applicant-jwt"
	got, err := cli.GetAccount(context.Background(), "00", bearer, "ba-1")
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}

	// ---- ① content 必须非 nil（本次生产事故的直接原因）----
	if fake.gotContent == nil {
		t.Error("content 传了 nil —— go-sdk 的 InvokeMethodWithContent 会在发请求之前" +
			"返回 \"content required\"，取号必然失败（且失败被 warn 吞掉，症状极隐蔽）。" +
			"GET 也要传 &dapr.DataContent{}")
	}

	// ---- ② 目标 app-id / 方法路径 / 动词 ----
	if fake.gotAppID != "supertrade-bank-account" {
		t.Errorf("目标 app-id 应为 supertrade-bank-account，实际 %q", fake.gotAppID)
	}
	if fake.gotMethod != "supplier-bank-accounts/ba-1" {
		t.Errorf("方法路径应为 supplier-bank-accounts/ba-1，实际 %q", fake.gotMethod)
	}
	if fake.gotVerb != "GET" {
		t.Errorf("动词应为 GET，实际 %q", fake.gotVerb)
	}

	// ---- ③ 门店必须透传（被调方按 X-Branch-ID 做 per-branch scope 守门）----
	if fake.gotBranch != "00" {
		t.Errorf("x-branch-id 应透传为 00，实际 %q", fake.gotBranch)
	}

	// ---- ④ **JWT 必须透传**（第二次生产事故的直接原因）----
	//
	// 2026-10-28 生产实踩:第一版只透传了 x-branch-id,漏了 authorization。
	// ⇒ 请求**到达了** bank-account(它的访问日志里有这条,branch 有值),
	//   但 claims 没注入 ⇒ 401 ⇒ dapr 包成
	//   `rpc error: code = Unauthenticated desc = Unauthorized`,
	//   而取号失败只 warn 不阻断提交 ⇒ 企微「付款信息」控件永远是空的。
	//
	// ⇒ 判据必须是 outgoing metadata 里**真的有** authorization。
	//
	// ⚠️ 判别力自检:把 GetAccount 里的 authorization append 删掉 ⇒ 本用例立刻红。
	if fake.gotAuth != bearer {
		t.Errorf("authorization 未透传（实际 %q，应为 %q）—— bank-account 在应用层验签，"+
			"拿不到 Authorization 就是 401，症状是企微「付款信息」控件恒为空",
			fake.gotAuth, bearer)
	}

	// ---- ⑤ 响应能解析出账号字段 ----
	if got == nil || got.AccountNo != "6222" || got.BankName != "中国银行" {
		t.Fatalf("解析结果不对: %+v", got)
	}
}

// TestDaprBankAccountClient_Guards 锁住两个防御性分支。
func TestDaprBankAccountClient_Guards(t *testing.T) {
	t.Run("空 id 直接拒绝(不发请求)", func(t *testing.T) {
		fake := &fakeDaprClient{}
		cli := NewDaprBankAccountClient(fake, "supertrade-bank-account")
		if _, err := cli.GetAccount(context.Background(), "00", "Bearer x", "  "); err == nil {
			t.Fatal("空 account id 应报错")
		}
		if fake.gotMethod != "" {
			t.Errorf("空 id 时不该发出请求,实际调了 %q", fake.gotMethod)
		}
	})

	t.Run("跨门店的账号被拒", func(t *testing.T) {
		raw, _ := json.Marshal(PayeeAccountDTO{ID: "ba-9", BranchID: "10", AccountNo: "6222"})
		fake := &fakeDaprClient{resp: raw}
		cli := NewDaprBankAccountClient(fake, "supertrade-bank-account")
		// 客户端拿别的门店的 account_id 来提交 ⇒ 必须挡住
		// (否则 A 店的付款单上会出现 B 店的收款账号)。
		if _, err := cli.GetAccount(context.Background(), "00", "Bearer x", "ba-9"); err == nil {
			t.Fatal("跨门店的收款账号应被拒")
		}
	})

	t.Run("空账号响应被拒", func(t *testing.T) {
		fake := &fakeDaprClient{resp: []byte(`{}`)}
		cli := NewDaprBankAccountClient(fake, "supertrade-bank-account")
		if _, err := cli.GetAccount(context.Background(), "00", "Bearer x", "ba-x"); err == nil {
			t.Fatal("bank-account 返回空账号应报错(而不是静默落一条空快照)")
		}
	})
}
