package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件锁死一条**只有源码结构能断言**的不变量：
// 每日销售事实的采集端点必须注册在 auth 中间件**之前**的窗口里。
//
// # 为什么不能靠 HTTP 测试断这条
//
// registerDaprRoutes / registerRoutes 是 package main 的私有函数，
// 而鉴权中间件挂在 pkg/cmdbootstrap.NewRouter 内部 ——
// 想从 HTTP 层复现"挂错窗口"，得把整个 sales-agg 装配起来
// （含 dapr state、PG、cube sidecar），那属于集成测试，不该由单元测试承担。
//
// 而这条不变量的**失效症状极其安静**（2026-10-18 上线前才发现）：
//
//	dapr scheduler 回调不带任何凭据（job 注册体只有 schedule/data/address，
//	dapr scheduler 的 job spec 也没有 headers 字段），
//	挂进鉴权链 ⇒ 每晚 22:45 与次日 01:05 的采集**每一次都 401**，
//	而 job 注册照样成功（"采集定时任务已对齐 branches=7 jobs=14"）、
//	healthz 照样 200、报表照出（sales-insight 回退 cube）。
//	**没有任何一行说"采集没跑过"**，要到"报表一直是旧口径"时才被发现。
//
// 判据：读自己的源码，断言注册调用出现在哪个函数体内。

func readSelf(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读 main.go：%v", err)
	}
	// Go 文件在 Windows 检出时可能是 CRLF，跨平台断言要一并容忍。
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// fnBody 截出 `func <name>(...) {` 那一整个函数体。
//
// ⚠️ 用花括号配对而不是"到下一个 \nfunc 为止"：
//
//	这两个函数之间夹着大段注释，若按行截断，会把注释里
//	「注册在鉴权链之内」之类的字样算进 registerDaprRoutes 的正文，
//	于是断言恒为真 —— 判据自己把自己判死了。
func fnBody(t *testing.T, src, name string) string {
	t.Helper()
	marker := "func " + name + "("
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatalf("main.go 里找不到 %s", marker)
	}
	rest := src[i:]
	depth := 0
	for idx, r := range rest {
		switch r {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return rest[:idx+1]
			}
		}
	}
	t.Fatalf("%s 的花括号没配平，源码可能被截断了", marker)
	return ""
}

// TestCollectHandlerRegisteredInAuthFreeWindow 是本组最要紧的一条。
func TestCollectHandlerRegisteredInAuthFreeWindow(t *testing.T) {
	src := readSelf(t)

	free := fnBody(t, src, "registerDaprRoutes")
	guarded := fnBody(t, src, "registerRoutes")

	const ctor = "NewDailyCollectHandler"

	if !strings.Contains(free, ctor) {
		t.Errorf("%s 必须出现在 registerDaprRoutes（auth 之前的免鉴权窗口）里，实际没有。\n"+
			"dapr scheduler 回调不带凭据，挂进鉴权链会让每晚采集全部 401，"+
			"而症状只有「报表一直是旧口径」——没有任何一行会报「采集没跑过」",
			ctor)
	}
	if strings.Contains(guarded, ctor) {
		t.Errorf("%s 不该出现在 registerRoutes 里（那是鉴权链之内）；"+
			"两边都注册会让 gin 报 duplicate route 而服务起不来", ctor)
	}
}

// TestRegisterDaprRunsBeforeAuthInCmdbootstrap 钉住跨包的顺序假设。
//
// ⚠️ "在 registerDaprRoutes 里"只有在 cmdbootstrap **先**调 RegisterDapr
//
//	再挂鉴权中间件时才等于"免鉴权"。哪天 cmdbootstrap 调换了这两个回调的顺序，
//	采集端点就会重新变成每晚 401，而本文件的第一条仍会绿 ——
//	所以这个假设要在这里单独钉一遍。
func TestRegisterDaprRunsBeforeAuthInCmdbootstrap(t *testing.T) {
	p := filepath.Join("..", "..", "pkg", "cmdbootstrap", "bootstrap.go")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Skipf("找不到 %s（%v）；跨包顺序假设未验证", p, err)
	}
	src := strings.ReplaceAll(string(b), "\r\n", "\n")

	iDapr := strings.Index(src, "opts.RegisterDapr(r)")
	if iDapr < 0 {
		t.Skip("cmdbootstrap.NewRouter 里没有 opts.RegisterDapr(r) 调用（改名后请同步更新本断言）")
	}
	// 鉴权锚点：Gin 中间件是 r.Use(...) 链，取紧跟其后的第一个 Use。
	iAuth := strings.Index(src[iDapr:], "r.Use(")
	if iAuth < 0 {
		t.Skip("没找到 RegisterDapr 之后的鉴权中间件锚点，改结构后请同步更新本断言")
	}
	iAuth += iDapr

	if iDapr > iAuth {
		t.Errorf("cmdbootstrap.NewRouter 里 RegisterDapr(偏移 %d) 排在鉴权中间件(偏移 %d) 之后；"+
			"那样 registerDaprRoutes 里的端点就不再免鉴权，采集端点会重新变成每晚 401",
			iDapr, iAuth)
	}
}
