// Package cubesync —— dapr scheduler 回调路径的守卫测试。
//
// # 这组测试在防什么(2026-10-22 生产事故)
//
// 症状:定时搬运**从来没跑过**,而现场只有一行
//
//	unexpected status code returned from app while processing triggered job
//	cube-async-00-10. status code returned: 16
//
// 16 = gRPC Unauthenticated = HTTP 401。
//
// 真因(源码 + 生产日志双向核对):
//
//	pkg/channel/http/http_channel.go → constructJobRequest:
//	    uri := h.baseAddress + "/job/" + strings.TrimPrefix(name, "/")
//	pkg/runtime/scheduler/internal/cluster/streamer.go → invokeApp:
//	    appChannel.TriggerJob(ctx, job.GetName(), job.GetData())
//
// ⇒ dapr 恒打 **POST {app}/job/{job 名}**,
//   而且 **job spec 里的 address 字段被完全忽略**。
//
// 我们当时注册的是 "/v1/sync/job" ⇒ 请求落进 gin 的 NoRoute,
// 而 NoRoute **照跑全局中间件链** ⇒ RequireAudience 因无 claims 挡下 ⇒ 401。
//
// ⇒ 症状安静得可怕:服务正常、healthz 200、job 注册成功、
// 人工触发一切正常,**只有定时那一路没跑过**。
//
// # 测什么(对着症状写)
//
//	① 回调路径的字面形态 = /job/<job 名>(钉死 dapr 的契约)
//	② 路由表里**确实有**这条路径(不是"应该会注册")
//	③ **完全复刻 dapr 的请求**(路径 + body + 无任何 header)能被受理
//	④ 旧别名仍然可用(运维脚本不炸)
//	⑤ job 名只有一处真相源(注册名与回调路径不可能分叉)
package cubesync

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestJobCallbackPath_MatchesDaprContract 钉住回调路径的字面形态。
//
// ⚠️ 期望值是**硬编码字面量**:从实现里算出来的话,实现错了测试也跟着错。
func TestJobCallbackPath_MatchesDaprContract(t *testing.T) {
	cases := []struct {
		jobName string
		want    string
	}{
		{"cube-async-00-10", "/job/cube-async-00-10"},
		{"cube-sync-00-00", "/job/cube-sync-00-00"},
		// ⚠️ dapr 侧是 strings.TrimPrefix(name, "/") ⇒ 前导斜杠要去掉,
		//   否则会拼出 "/job//xxx"。
		{"/cube-async-00-10", "/job/cube-async-00-10"},
	}
	for _, tc := range cases {
		if got := JobCallbackPath(tc.jobName); got != tc.want {
			t.Errorf("JobCallbackPath(%q) = %q,期望 %q",
				tc.jobName, got, tc.want)
		}
	}
}

// TestJobCallbackPath_RegisteredInRouteTable 钉住"这条路径真的注册了"。
//
// ⚠️ 为什么不能只看"回调能通":那需要真的发一次带密钥的请求,
//   而这条断言直接把"路由表里有没有它"摆到台面上 ——
//   将来有人把 RegisterDaprRoutes 里的路径改回别的,这里立刻红。
func TestJobCallbackPath_RegisteredInRouteTable(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	found := false
	for _, rt := range r.Routes() {
		if rt.Method == http.MethodPost && rt.Path == testJobPath {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("路由表里没有 POST %s —— dapr 只会打这一条;"+
			"落空的请求会落进 NoRoute 并被 RequireAudience 挡成 401"+
			"(dapr 折成 gRPC code 16,现场只看到 status code returned: 16)。实际路由: %v",
			testJobPath, routePaths(r))
	}
}

// TestDaprJobCallback_AcceptsExactDaprRequest 是本组最要紧的一条:
// **完全复刻 dapr 的请求**,不许带任何额外 header。
//
// ⚠️ 为什么必须复刻:dapr 回调
//
//	① 路径 = /job/<job 名>(不是我们自己挑的)
//	② body = job data 的序列化值(内层 map ⇒ token 在**顶层**)
//	③ **不带任何 header**(没有 Authorization、没有 X-Sync-Token)
//
// 只测"带 X-Sync-Token 头能通"是**假绿**:dapr 根本不会带那个头。
//
// ⚠️ 期望值同样是硬编码:路径与 body 都不从实现里推导。
func TestDaprJobCallback_AcceptsExactDaprRequest(t *testing.T) {
	h, _, im := newTestHandler()
	r := newTestRouter(h)

	// 这就是 dapr 会送过来的 body(registerJob 里写进 job data 的那个 map)。
	// ⚠️ 顶层就有 token —— dapr 的 constructJobRequest 取的是
	//    google.protobuf.Value 的 MarshalJSON,也就是**内层 map**,
	//    所以 token 不在被剥掉的那层壳里。
	body := `{"trigger":"dapr-job","job":"cube-async-00-10",` +
		`"source_branch":"00","target_branch":"10","token":"` + testToken + `"}`

	w := do(r, http.MethodPost, testJobPath, "", body)
	if w.Code != http.StatusOK {
		t.Fatalf("dapr 的真实回调形态应被受理,实际 %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if resp.Code != "accepted" {
		t.Errorf("定时回调应回 accepted(已受理,后台跑),实际 %q", resp.Code)
	}
	// ⚠️ 回调是异步的 ⇒ 断言要等后台跑完,不能只看响应。
	waitForImport(t, im, 5*time.Second)
}

// TestDaprJobCallback_RejectsWrongToken 锁"路径对了也不能免鉴权"。
//
// 回调端点在免鉴权窗口 ⇒ 唯一的收口是共享密钥。
// 路径改成对之后,这条断言才开始有意义(改之前请求根本到不了 handler)。
func TestDaprJobCallback_RejectsWrongToken(t *testing.T) {
	h, _, im := newTestHandler()
	r := newTestRouter(h)

	body := `{"trigger":"dapr-job","job":"cube-async-00-10","token":"wrong-token"}`
	w := do(r, http.MethodPost, testJobPath, "", body)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("错的密钥应 401,实际 %d: %s", w.Code, w.Body.String())
	}
	if len(im.snapshot()) != 0 {
		t.Errorf("未通过鉴权的请求不应触发导入,实际触发了 %d 次", len(im.snapshot()))
	}
}

// TestLegacyJobPath_StillWorks 锁"旧别名仍可用"。
//
// 运维脚本 / 习惯用法可能还指着 /v1/sync/job。
// dapr 不会打它,但它在同一个免鉴权窗口、由同一个密钥收口 ——
// 留着它不影响安全,删了它会让老脚本 404。
func TestLegacyJobPath_StillWorks(t *testing.T) {
	h, _, im := newTestHandler()
	r := newTestRouter(h)

	w := do(r, http.MethodPost, LegacyJobPath, testToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("旧别名应仍可用,实际 %d: %s", w.Code, w.Body.String())
	}
	waitForImport(t, im, 5*time.Second)
}

// TestJobName_SingleSourceOfTruth 锁"注册名与回调路径不可能分叉"。
//
// ⚠️ 这条是**结构性**防线:两边一旦各写各的字符串,
//   症状就是"定时同步静默消失"—— 服务正常、job 注册成功、只有定时那路没跑。
//   所以 job 名必须从同一份 Config 派生。
func TestJobName_SingleSourceOfTruth(t *testing.T) {
	h, _, _ := newTestHandler()

	if h.JobName() != h.syncer.cfg.JobName() {
		t.Errorf("handler 的 job 名(%q)应与 cfg 派生的一致(%q)",
			h.JobName(), h.syncer.cfg.JobName())
	}
	if h.JobCallbackPath() != JobCallbackPath(h.JobName()) {
		t.Errorf("回调路径(%q)应由 job 名(%q)算出",
			h.JobCallbackPath(), h.JobName())
	}
	// ⚠️ 生产实测的 job 名/路径(cube-async 搬运 00 → 10)。
	if h.JobName() != "cube-async-00-10" {
		t.Errorf("job 名应为 cube-async-00-10,实际 %q", h.JobName())
	}
	if h.JobCallbackPath() != testJobPath {
		t.Errorf("回调路径应为 %s,实际 %q", testJobPath, h.JobCallbackPath())
	}
}

// TestConfig_JobCallbackURL_ReflectsRealPath 锁"登记在 job spec 里的地址是真的"。
//
// ⚠️ dapr 不读 address 字段 —— 但**填真值**仍有意义:
//   让"读 job spec 的人"看到真相,而不是让它继续误导下一个人。
func TestConfig_JobCallbackURL_ReflectsRealPath(t *testing.T) {
	c := Config{
		SourceBranchID: "00", TargetBranchID: "10", JobPrefix: "cube-async",
		CallbackHost: "127.0.0.1", CallbackPort: "8118",
	}
	got := c.JobCallbackURL()
	want := "http://127.0.0.1:8118/job/cube-async-00-10"
	if got != want {
		t.Errorf("JobCallbackURL() = %q,期望 %q", got, want)
	}
	if !strings.Contains(got, "/job/cube-async-00-10") {
		t.Errorf("回调地址里应是 dapr 真正会打的路径,实际 %q", got)
	}
}

// routePaths 列出路由表(给断言信息用)。
func routePaths(r *gin.Engine) []string {
	out := make([]string, 0)
	for _, rt := range r.Routes() {
		out = append(out, rt.Method+" "+rt.Path)
	}
	return out
}
