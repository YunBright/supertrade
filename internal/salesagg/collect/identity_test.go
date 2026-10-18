package collect

import (
	"context"
	"errors"
	"testing"

	dapr "github.com/dapr/go-sdk/client"
	"google.golang.org/grpc/metadata"

	"github.com/YunBright/supertrade/internal/cubeclient"
)

// 本文件钉死「采集用 bearer 打 cube」这条链路的**最后一跳**。
//
// # 为什么断言点在 metadata 而不是返回值
//
// [WithBranchBearer] 返回的 ctx 本身看不出来对不对 ——
// 真正决定成败的是 cubeclient.LoadCubeQuery 从 ctx 里取出来、
// 塞进 outgoing gRPC metadata 的那一串。
// 只测「解析器返回了什么」的话，前缀加错/漏加都能全绿。
//
// 判据因此是：**实际会发给 cube 的 authorization metadata**。
//
// ⚠️ 这个洞是 2026-10-19 上线端到端实测抓出来的：
//   BearerResolver 按契约返回**裸 token**，而 cubeclient.WithBearer
//   要的是 Authorization 头的**完整内容**（本仓其余九个调用方都传
//   `Header.Get("Authorization")`，都带前缀）。直接透传 ⇒ cube 收到
//   `Authorization: eyJ...` ⇒ 401 ⇒ 报成「cube 拒绝调用者凭证」。
//   编译、单测、vet 全绿，只有真跑一次才看得见。

// mdCapturer 截获真正发往 cube 的 gRPC metadata。
type mdCapturer struct {
	md metadata.MD
}

func (c *mdCapturer) InvokeMethodWithContent(ctx context.Context, appID, methodName, verb string,
	content *dapr.DataContent) ([]byte, error) {
	c.md, _ = metadata.FromOutgoingContext(ctx)
	return []byte(`{"data":[]}`), nil
}

// fakeResolver 只实现 Bearer（BearerResolver 的全貌就这一个方法）。
type fakeResolver struct {
	token string
	err   error
}

func (f *fakeResolver) Bearer(ctx context.Context, branchID string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.token, nil
}

// sentAuth 跑一次真实调用，返回 cube 实际会收到的 authorization 头值。
func sentAuth(t *testing.T, r BearerResolver) string {
	t.Helper()
	ctx, err := WithBranchBearer(context.Background(), r, "01")
	if err != nil {
		t.Fatalf("WithBranchBearer: %v", err)
	}
	inv := &mdCapturer{}
	c := cubeclient.NewDaprCubeClient(inv, "supertrade-cube-router", "v1/load")
	if _, err := c.LoadCubeQuery(ctx, "product", cubeclient.CubeQuery{Limit: 1}); err != nil {
		t.Fatalf("LoadCubeQuery: %v", err)
	}
	got := inv.md.Get("authorization")
	if len(got) == 0 {
		return ""
	}
	return got[0]
}

func TestBearerSentToCubeCarriesBearerScheme(t *testing.T) {
	// ⭐ 本组最要紧的一条，也是那个真缺陷本身。
	//
	// 少了 "Bearer " ⇒ cube 的验签中间件判「缺少已签 token」⇒ 401
	// ⇒ dapr 转成 gRPC Unauthenticated ⇒ 报「cube 拒绝调用者凭证」，
	// 把排查一路引向 cube / 网关 / token 有效期 / 权限 —— 真因不在这条链路上。
	const tok = "eyJhbGciOiJSUzI1NiIsImtpZCI6IjIwMjYiLCJ0eXAiOiJKV1QifQ.payload.sig"
	got := sentAuth(t, &fakeResolver{token: tok})
	want := "Bearer " + tok
	if got != want {
		t.Fatalf("发往 cube 的 authorization = %q，期望 %q\n"+
			"少一个 \"Bearer \" 前缀 ⇒ cube 401 ⇒ 报「cube 拒绝调用者凭证」，"+
			"而人会被引去查 cube / 网关 / token 有效期，真因（少前缀）不在任何日志里",
			got, want)
	}
}

func TestBearerAlreadyPrefixedIsNotDoubled(t *testing.T) {
	// ⚠️ 幂等：解析器将来若改成带前缀返回，不能变成 "Bearer Bearer x"。
	//
	// 双重前缀同样 401，而症状与"没前缀"**完全一样** ——
	// 所以这条不是洁癖，它挡的是同一个故障的另一种形态。
	for _, in := range []string{
		"Bearer abc",
		"bearer abc", // 大小写不敏感：HTTP 的 auth-scheme 本就大小写不敏感
		"BEARER abc",
	} {
		got := sentAuth(t, &fakeResolver{token: in})
		if got != in {
			t.Errorf("输入 %q ⇒ 发出 %q，期望原样（不能重复加前缀）", in, got)
		}
	}
}

func TestBearerResolvingErrorIsPassedThrough(t *testing.T) {
	// ⚠️ 错误必须原样冒泡：handler 靠 errors.Is 判
	// 「这家店没开采集 / 被关停 / 凭据坏了」，
	// 包一层通用错误的话，前端会把三种都指向「去开启采集」，
	// 而第三种开了也还是坏 —— 死循环。
	want := ErrCollectDisabled
	_, err := WithBranchBearer(context.Background(), &fakeResolver{err: want}, "01")
	if err == nil {
		t.Fatal("解析失败却没报错")
	}
	if !errors.Is(err, want) {
		t.Fatalf("错误 = %v，期望 errors.Is 到 %v", err, want)
	}
}

func TestBearerEmptyTokenIsRejected(t *testing.T) {
	// 空 token 不能当成功：那等于让 cube 收到**完全没有** Authorization，
	// 症状同样是 Unauthenticated，而真因（解析器给了空串）被吞掉。
	// 拦住它，报错就会指向"解析器"，而不是指向 cube。
	if _, err := WithBranchBearer(context.Background(),
		&fakeResolver{token: "   "}, "01"); err == nil {
		t.Fatal("空 token 却当成功返回了，cube 会收到无 Authorization 的请求")
	}
}

func TestBearerNilResolverIsRejected(t *testing.T) {
	if _, err := WithBranchBearer(context.Background(), nil, "01"); err == nil {
		t.Fatal("解析器为 nil 却没报错")
	}
}
