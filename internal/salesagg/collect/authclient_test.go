package collect

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// DaprServiceAccountClient 的用例（httptest 起真 HTTP server）。
//
// # 为什么必须单独测它
//
// 编排层的测试用的是 **fake** 账号客户端，它不经过 HTTP 解析。
// 于是真实客户端里两处最容易出错的逻辑**结构上测不到**：
//
//  1. 把 auth 的 409 识别成 ErrServiceAccountExists（自愈路径的触发信号）
//     —— 认错了，"建号成功但落库失败"就永远自愈不了，门店永久开不了采集；
//  2. 拒绝把**空明文**当成功返回 —— 放行的话会落一个空凭据，
//     采集每次 401，而真因（auth 没真返回明文）被完全掩盖。
func newTestAuthClient(t *testing.T, h http.HandlerFunc) (*DaprServiceAccountClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewDaprServiceAccountClient(srv.URL, "userd")
	if c == nil {
		t.Fatal("NewDaprServiceAccountClient 返回 nil")
	}
	return c, srv
}

// TestAuthClientRecognisesConflictAsAccountExists ⭐
//
// auth 对"client_id 已存在"返 409 + {"code":"conflict"}。
// 客户端必须把它翻译成**可判别**的哨兵 ErrServiceAccountExists ——
//
// 包装成普通错误的话，编排层的"命中已存在 ⇒ 轮换密钥"自愈分支永远不会触发，
// 症状是"开启采集永远失败：client_id 已存在"，而真实原因是上次落库失败。
func TestAuthClientRecognisesConflictAsAccountExists(t *testing.T) {
	c, _ := newTestAuthClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"conflict","message":"client_id 已存在"}`))
	})

	_, err := c.CreateScoped(context.Background(), "Bearer u", "sdc-00", "店 00", "00", []string{"cube:read"})
	if err == nil {
		t.Fatal("409 却没报错")
	}
	if !errors.Is(err, ErrServiceAccountExists) {
		t.Fatalf("错误 = %v，期望 errors.Is(ErrServiceAccountExists)", err)
	}
}

// TestAuthClientForwardsUserBearer ⭐
//
// 必须**原样**转发用户的 Authorization。
//
// 不转发 ⇒ auth 看不到真人身份，权限判定就落到"这台机器自称是谁"，
// 整个权限模型失效（任何能调到本服务的人都能建机器身份）。
func TestAuthClientForwardsUserBearer(t *testing.T) {
	var gotAuth string
	var gotPath string
	var gotBody string
	c, _ := newTestAuthClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"client_id":"sdc-00","client_secret":"plain-secret"}`))
	})

	secret, err := c.CreateScoped(context.Background(), "Bearer user-token", "sdc-00", "店 00", "00", []string{"cube:read"})
	if err != nil {
		t.Fatalf("CreateScoped: %v", err)
	}
	if gotAuth != "Bearer user-token" {
		t.Errorf("Authorization = %q，期望原样转发用户的 bearer", gotAuth)
	}
	if !strings.HasSuffix(gotPath, "/method/service-accounts") {
		t.Errorf("请求路径 = %q，期望以 /method/service-accounts 结尾", gotPath)
	}
	if !strings.Contains(gotBody, `"branch_id":"00"`) {
		t.Errorf("请求体 = %q，期望含 branch_id 00", gotBody)
	}
	if !strings.Contains(gotBody, `"cube:read"`) {
		t.Errorf("请求体 = %q，期望含 cube:read", gotBody)
	}
	if secret != "plain-secret" {
		t.Errorf("返回的明文 = %q，期望 plain-secret", secret)
	}
}

// TestAuthClientRejectsEmptyRotatedSecret ⭐
//
// 轮换后 auth 若返回空明文，**必须**当失败。
//
// 放行 ⇒ 调用方把空串当凭据落库 ⇒ 采集每次 401，
// 而真因（auth 没真返回明文）在整条链路上没有任何痕迹。
func TestAuthClientRejectsEmptyRotatedSecret(t *testing.T) {
	c, _ := newTestAuthClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"client_id":"sdc-00","client_secret":""}`))
	})

	secret, err := c.ResetSecret(context.Background(), "Bearer u", "sdc-00")
	if err == nil {
		t.Fatalf("空明文却当成功返回了（secret=%q）", secret)
	}
	if secret != "" {
		t.Errorf("出错时仍返回了明文 %q", secret)
	}
}

// TestAuthClientPassesThroughPermissionErrorMessage ⭐
//
// auth 的权限错误里**列全了缺哪几条权限**（assertMayGrant 的行为），
// 客户端必须把这句 message **原样**带回。
//
// 吞掉它 ⇒ 前端只能显示"操作失败"，而用户不知道自己缺 `cube:read`
// 还是缺 `service-account:grant` —— 于是反复重试，永不成功。
func TestAuthClientPassesThroughPermissionErrorMessage(t *testing.T) {
	c, _ := newTestAuthClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"forbidden","message":"你没有开启门店采集的权限，缺少：00:cube:read"}`))
	})

	_, err := c.CreateScoped(context.Background(), "Bearer u", "sdc-00", "店 00", "00", []string{"cube:read"})
	if err == nil {
		t.Fatal("403 却没报错")
	}
	if !strings.Contains(err.Error(), "00:cube:read") {
		t.Errorf("错误 = %v，期望原样带回缺哪条权限", err)
	}
}

// TestAuthClientNeverEchoesSecretInErrors
//
// 出错时**不得**回显请求体（里面有 client_secret）。
// 而这个错误会经 HTTP 响应与日志被长期翻阅。
func TestAuthClientNeverEchoesSecretInErrors(t *testing.T) {
	c, _ := newTestAuthClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"internal_error","message":"boom"}`))
	})

	_, err := c.CreateScoped(context.Background(), "Bearer u", "sdc-00", "店 00", "00", []string{"cube:read"})
	if err == nil {
		t.Fatal("500 却没报错")
	}
	if strings.Contains(err.Error(), "plain-secret") {
		t.Errorf("错误里回显了 secret: %v", err)
	}
}
