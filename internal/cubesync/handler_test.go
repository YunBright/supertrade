// Package cubesync —— 触发端点的单元测试。
//
// # 测什么(对着症状写)
//
//	① 缺 / 错密钥 ⇒ 401(免鉴权窗口的唯一收口)
//	② 密钥从三个来源任一都能带上(header / query / body)
//	③ 人工触发的 mode / scopes 覆盖:非法值必须**报错**而不是静默忽略
//	④ status 端点能看到上一次结果
//
// # 为什么这些断言重要
//
// 免鉴权窗口意味着"同网段任何主机都能 POST"。
// 密钥校验一旦被改坏(比如误删了比较那一行),
// 症状是"同步能被任何人触发"—— 而除此之外**没有任何症状**:
// 服务正常、job 正常、同步正常。
package cubesync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/archivedto"
)

const testToken = "s3cr3t-token"

// testJobPath 是 dapr scheduler **实际会打**的回调路径。
//
// ⚠️ 这里是**硬编码字面量**,不是从实现里算出来的 ——
//   算出来的话,实现错了测试也会跟着错(同义反复,测了个寂寞)。
//
// 依据(dapr v1.18 源码):
//
//	pkg/channel/http/http_channel.go → constructJobRequest:
//	    uri := h.baseAddress + "/job/" + strings.TrimPrefix(name, "/")
//
// ⇒ 回调恒为 POST {app}/job/{job 名},job 名 = "cube-async-00-10"。
const testJobPath = "/job/cube-async-00-10"

// newTestHandler 造一个带 fake 依赖的 Handler。
func newTestHandler() (*Handler, *fakeExporter, *fakeImporter) {
	ex := &fakeExporter{pages: map[string][]map[string]any{
		archivedto.ScopeProducts: {productRow("P1", "商品一")},
	}}
	im := &fakeImporter{}
	// ⚠️ JobPrefix 固定成 "cube-async"(与生产一致)⇒ job 名固定为
	//   "cube-async-00-10" ⇒ 回调路径固定为 testJobPath。
	cfg := Config{SourceBranchID: "00", TargetBranchID: "10", JobPrefix: "cube-async",
		Mode:  "overwrite",
		Scopes: []string{archivedto.ScopeProducts}, PageSize: 10}
	h := NewHandler(newTestSyncer(cfg, ex, im), testToken, nil)
	return h, ex, im
}

// newTestRouter 造一个挂了触发端点的 gin engine。
func newTestRouter(h *Handler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h.RegisterDaprRoutes(r)
	return r
}

// do 发一个请求并返回 recorder。
func do(r *gin.Engine, method, path, token, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set(TokenHeader, token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestTrigger_RequiresToken 锁"缺 / 错密钥一律 401"。
func TestTrigger_RequiresToken(t *testing.T) {
	h, _, im := newTestHandler()
	r := newTestRouter(h)

	cases := []struct {
		name  string
		token string
	}{
		{"完全不带凭据", ""},
		{"错误的密钥", "wrong-token"},
		{"前缀相同但不等", testToken[:len(testToken)-1]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(r, http.MethodPost, RunPath, tc.token, "")
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("应 401,实际 %d: %s", w.Code, w.Body.String())
			}
		})
	}
	if len(im.batches) != 0 {
		t.Errorf("未通过鉴权的请求不应触发导入,实际触发了 %d 次", len(im.batches))
	}
}

// TestTrigger_TokenFromAnySource 锁"三种携带方式都能用"。
//
// dapr scheduler 回调只能带 body(它不带 header),
// 而人工用 curl 最顺手的是 header ⇒ 三处都要认。
//
// ⚠️ **定时回调是异步的**:它立刻 ack(同步在后台跑),
// 所以断言"导入已发生"必须等后台跑完,不能只看响应。
func TestTrigger_TokenFromAnySource(t *testing.T) {
	cases := []struct {
		name  string
		token string
		body  string
		path  string
	}{
		{"请求头", testToken, "", RunPath},
		{"请求头 + job 路径", testToken, "", testJobPath},
		{"body 字段", "", `{"token":"` + testToken + `"}`, RunPath},
		{"body 字段 + job 路径", "", `{"token":"` + testToken + `"}`, testJobPath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, im := newTestHandler()
			r := newTestRouter(h)
			w := do(r, http.MethodPost, tc.path, tc.token, tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("应 200,实际 %d: %s", w.Code, w.Body.String())
			}
			waitForImport(t, im, 5*time.Second)
		})
	}
}

// TestTrigger_RejectsBadModeAndScopes 锁"非法覆盖值必须报错"。
//
// 静默忽略的后果:运维以为自己跑的是 incremental(安全),
// 实际跑的是 overwrite(会清条码)—— 而报告显示"成功"。
func TestTrigger_RejectsBadModeAndScopes(t *testing.T) {
	h, _, im := newTestHandler()
	r := newTestRouter(h)

	w := do(r, http.MethodPost, RunPath, testToken, `{"mode":"destroy"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("非法 mode 应 400,实际 %d: %s", w.Code, w.Body.String())
	}
	w = do(r, http.MethodPost, RunPath, testToken, `{"scopes":["products","nonexistent"]}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("未知 scope 应 400,实际 %d: %s", w.Code, w.Body.String())
	}
	if len(im.snapshot()) != 0 {
		t.Errorf("参数非法的请求不应触发导入,实际 %d 次", len(im.snapshot()))
	}
}

// TestTrigger_ScopeOverrideIsApplied 锁"合法覆盖值生效"。
func TestTrigger_ScopeOverrideIsApplied(t *testing.T) {
	h, _, im := newTestHandler()
	r := newTestRouter(h)

	w := do(r, http.MethodPost, RunPath, testToken, `{"mode":"incremental","scopes":["products"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d: %s", w.Code, w.Body.String())
	}
	batches := im.snapshot()
	if len(batches) != 1 {
		t.Fatalf("应提交 1 次,实际 %d 次", len(batches))
	}
	if batches[0].Mode != "incremental" {
		t.Errorf("mode 覆盖未生效,实际 %q", batches[0].Mode)
	}
	if len(batches[0].Scopes) != 1 || batches[0].Scopes[0] != archivedto.ScopeProducts {
		t.Errorf("scopes 覆盖未生效,实际 %v", batches[0].Scopes)
	}
}

// TestJobCallback_IgnoresModeOverride 锁"定时任务不吃 body 里的 mode"。
//
// 定时任务的参数由**环境变量**决定。
// 若 body 能改它,任何人(拿着密钥)都能把每晚的 overwrite 改成 incremental,
// 或者反过来 —— 症状是"同步结果每天不一样,没人知道为什么"。
func TestJobCallback_IgnoresModeOverride(t *testing.T) {
	h, _, im := newTestHandler()
	r := newTestRouter(h)

	w := do(r, http.MethodPost, testJobPath, testToken, `{"mode":"incremental"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d: %s", w.Code, w.Body.String())
	}
	// ⚠️ 定时回调是**异步**的 ⇒ 断言要等后台跑完。
	batches := waitForImport(t, im, 5*time.Second)
	if len(batches) != 1 {
		t.Fatalf("应提交 1 次,实际 %d 次", len(batches))
	}
	if batches[0].Mode != "overwrite" {
		t.Errorf("定时回调不应被 body 的 mode 改变,实际 %q", batches[0].Mode)
	}
}

// TestJobCallback_AcksImmediately 锁"定时回调立刻 ack"。
//
// ⚠️ 一次全量同步(门店 00 实测 27565 个商品)约 **100 秒**。
//
//	dapr scheduler 的投递有超时 ⇒ 回调里同步跑完会被判失败 ⇒ **反复重试**
//	⇒ 同一批数据被一遍遍导。
//
// ⇒ 回调必须只做"鉴权 + 受理",同步放后台。
func TestJobCallback_AcksImmediately(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	w := do(r, http.MethodPost, testJobPath, testToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Code != "accepted" {
		t.Errorf("定时回调应回 accepted(已受理,后台跑),实际 %q", body.Code)
	}
}

// TestStatus_RequiresTokenAndReportsLastRun 锁 status 端点。
func TestStatus_RequiresTokenAndReportsLastRun(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	if w := do(r, http.MethodGet, StatusPath, "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("无密钥访问 status 应 401,实际 %d", w.Code)
	}

	if w := do(r, http.MethodPost, RunPath, testToken, ""); w.Code != http.StatusOK {
		t.Fatalf("触发同步应 200,实际 %d", w.Code)
	}

	w := do(r, http.MethodGet, StatusPath, testToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Report *SyncReport `json:"report"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Report == nil {
		t.Fatal("status 应带上一次的报告")
	}
	if body.Report.TargetBranchID != "10" {
		t.Errorf("报告里的目标门店应为 10,实际 %q", body.Report.TargetBranchID)
	}
}

// TestTrigger_ReportsFailureWithPartialProgress 锁"失败时也要带进度"。
//
// 症状:一次中途失败若只返回错误,运维会以为"什么都没导" ——
// 而实际已经有几千行落库(下次重跑是幂等的,不会写坏)。
func TestTrigger_ReportsFailureWithPartialProgress(t *testing.T) {
	h, ex, _ := newTestHandler()
	ex.err = context.DeadlineExceeded
	r := newTestRouter(h)

	w := do(r, http.MethodPost, RunPath, testToken, "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("导出失败应 503,实际 %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Code   string      `json:"code"`
		Report *SyncReport `json:"report"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Code != "sync_failed" {
		t.Errorf("错误码应为 sync_failed,实际 %q", body.Code)
	}
	if body.Report == nil {
		t.Fatal("失败时也应带报告(哪怕是空的),否则运维看不出跑到哪一步")
	}
}
