package handler

import (
	"errors"
	"fmt"
	"testing"

	"github.com/YunBright/supertrade/internal/salesagg/collect"
)

// 本文件钉死"auth 的 403 ⇒ 前端能分流"这条链路在 **handler 层的映射**。
//
// # 为什么这条映射必须单独测
//
// 编排层（collect.Control）的测试用的是 **fake** 账号客户端，
// 它不经过 HTTP 状态码解析。而"403 有没有被翻译成
// *collect.AuthDeniedError"这一步发生在**真实客户端**里，
// 于是 handler 这三个纯函数就成了这条链路上**唯一**能被单测覆盖的地方：
//
//	auth 403 → *collect.AuthDeniedError → controlStatus/Code/Message → 前端分流
//
// 三者任意一环写错，症状都是**同一个**：前端弹一句"操作失败，请重试"。
// 而用户缺的是权限，不是网络 —— 他重试一万次结果完全一样，
// 于是反复重试、反复失败，且没有任何日志指向"他其实没权限"。
//
// ⚠️ 期望值全部用**字面量**（403 / "forbidden" / "cube_source_not_configured"），
// 不用被测的常量推导 —— 否则把哨兵换掉，用例跟着变、照样全绿。

// authDenied 造一个被 auth 拒绝的 error（真实客户端会返回这个类型）。
func authDenied(code, msg string) error {
	return &collect.AuthDeniedError{Code: code, Message: msg}
}

func TestControlStatusMapsAuthForbiddenTo403(t *testing.T) {
	// ⭐ 本组最要紧的一条。
	//
	// 退化成 400 / 500 / 503 的症状：前端把"没权限"当成"参数错"或"上游挂了"，
	// 于是弹一个「重试」按钮 —— 而重试解决不了缺权限。
	got := controlStatus(authDenied("forbidden", "缺少：00:service-account:grant"))
	if got != 403 {
		t.Fatalf("auth 403 的状态码 = %d，期望 403", got)
	}
}

func TestControlStatusKeepsCubeSourceConflictsAt409(t *testing.T) {
	// ⚠️ 与上一条构成对照：数据源问题必须是 409，不能被 403 分支抢走。
	//
	// 抢走的话前端会把"去配置数据源"显示成"你没有权限"，
	// 而正确动作是去绑数据源 —— 用户去找管理员要权限，永远解决不了。
	for _, err := range []error{
		collect.ErrCubeSourceNotConfigured,
		collect.ErrCubeSourceDisabled,
	} {
		if got := controlStatus(err); got != 409 {
			t.Errorf("%v 的状态码 = %d，期望 409", err, got)
		}
	}
}

func TestControlStatusMapsUnexpectedFailureTo503(t *testing.T) {
	// ⚠️ 绝不能是 400。
	//
	// 400 ⇒ 前端当成"参数错"，弹"请检查后重试"，把排查引向请求体；
	// 而这类错误是 DB 不可达 / dapr 连不上，真因在服务端。
	if got := controlStatus(errors.New("boom")); got != 503 {
		t.Fatalf("未知失败的状态码 = %d，期望 503（可重试），实际 400 会把排查引向请求体", got)
	}
}

func TestControlCodeIsStableAndLiterallyAuthCode(t *testing.T) {
	// ⚠️ 分流逻辑**只依赖 code**（message 会随版本变文案）。
	// 自己编一个新码的话，前端与后端对同一语义会用两个码，分流永远落空。
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"数据源未配置", collect.ErrCubeSourceNotConfigured, "cube_source_not_configured"},
		{"数据源被禁用", collect.ErrCubeSourceDisabled, "cube_source_disabled"},
		{"服务账号已存在", collect.ErrServiceAccountExists, "service_account_exists"},
		{"auth 403", authDenied("forbidden", "缺少：00:cube:read"), "forbidden"},
		{"auth 403 无 code 时回退", authDenied("", "缺权限"), "forbidden"},
		{"未知失败", errors.New("boom"), "collect_control_failed"},
	}
	for _, tc := range cases {
		if got := controlCode(tc.err); got != tc.want {
			t.Errorf("%s 的 code = %q，期望 %q", tc.name, got, tc.want)
		}
	}
}

func TestControlMessagePassesAuthWordingThrough(t *testing.T) {
	// ⭐ 「缺哪几条权限」是用户唯一能照着去申请的东西。
	//
	// 哪怕只是加个 "auth 拒绝：" 前缀，前端按 code 分流后仍然显示一句
	// 用户看不懂的话 —— 分流对了，文案还是废的。
	const msg = "你没有开启门店采集的权限，缺少：00:service-account:grant、00:cube:read"
	got := controlMessage(authDenied("forbidden", msg))
	if got != msg {
		t.Fatalf("controlMessage = %q，期望逐字等于 auth 原话 %q", got, msg)
	}
}

func TestControlMessageFallsBackWhenAuthOmitsWording(t *testing.T) {
	// auth 若没带 message（老版本 / 网关截断），不能返回空串 ——
	// 空 message 在界面上是一片空白，用户连"失败了"都看不到。
	if got := controlMessage(authDenied("forbidden", "")); got == "" {
		t.Fatal("auth 没带 message 时返回了空串，界面上会是一片空白")
	}
}

func TestControlMessageKeepsNonAuthErrors(t *testing.T) {
	// 判别力：与上面两条只差错误类型。
	// 非 auth 错误仍走 err.Error() —— 数据源哨兵的文案本身就是要给用户看的。
	if got := controlMessage(collect.ErrCubeSourceNotConfigured); got == "" {
		t.Fatal("数据源哨兵被改成了空文案")
	}
}

// TestDeniedErrorSurvivesWrapping 钉一条容易被"顺手"破坏的性质：
// 编排层会用自己的上下文包一层错误。errors.As 必须还能捞到底下的
// AuthDeniedError —— 捞不到的话，403 会退化成 503，
// 而前端因此把"缺权限"显示成"上游暂不可用"。
func TestDeniedErrorSurvivesWrapping(t *testing.T) {
	wrapped := fmt.Errorf("代建服务账号：%w", authDenied("forbidden", "缺少：00:cube:read"))

	var denied *collect.AuthDeniedError
	if !errors.As(wrapped, &denied) {
		t.Fatal("包一层之后 errors.As 捞不到 AuthDeniedError，403 会退化成 503")
	}
	if denied.Message != "缺少：00:cube:read" {
		t.Errorf("捞到的 message = %q，期望原样", denied.Message)
	}
	if got := controlStatus(wrapped); got != 403 {
		t.Errorf("包一层后的状态码 = %d，期望 403", got)
	}
}

// TestDeniedErrorZeroValueHasReadableText 防止错误文案退化成空串。
//
// ⚠️ 断言的是**非 nil 的零值**（&AuthDeniedError{}），不是 nil 指针：
//
//	errors.As 只可能取到真实构造出来的值，nil receiver 不是这条链路上的契约；
//	而"构造了但 Message 为空"是真实可达的（auth 的 403 没带 message 时）。
//	那时若返回空串，用户界面上是一片空白，连"失败了"都看不到。
func TestDeniedErrorZeroValueHasReadableText(t *testing.T) {
	e := &collect.AuthDeniedError{}
	if got := e.Error(); got == "" {
		t.Fatal("零值 AuthDeniedError 的 Error() 是空串，界面上会是一片空白")
	}
	if got := controlMessage(e); got == "" {
		t.Fatal("零值 AuthDeniedError 经 controlMessage 后是空串")
	}
}
