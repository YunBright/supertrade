package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/salesagg/collect"
	"github.com/YunBright/supertrade/internal/salesagg/handler"
	"github.com/YunBright/supertrade/internal/salesdaily"
)

// errBoom 是采集失败用的哨兵错误。
var errBoom = errors.New("boom: cube 不可达")

// today 返回本机今天的 YYYY-MM-DD。
//
// ⚠️ 它只用于**否定式**断言（"校正采集不能采今天"），
// 绝不用于拼出期望值 —— 那样常量改错也全绿。
func today() string { return time.Now().Format("2006-01-02") }

// fakeCollector 记录采集器被调用时**实际拿到的**门店与营业日。
type fakeCollector struct {
	calls   int
	branch  string
	date    string
	source  salesdaily.CollectSource
	result  collect.Result
	failure error
}

func (f *fakeCollector) Run(_ context.Context, storeBranchID, operDate string, source salesdaily.CollectSource) (collect.Result, error) {
	f.calls++
	f.branch, f.date, f.source = storeBranchID, operDate, source
	if f.failure != nil {
		return collect.Result{}, f.failure
	}
	r := f.result
	// 回显实际入参：handler 的响应里 store/oper_date/source 必须与进来的一致，
	// 否则测试就只在断言"返回了 200"，而参数是否真的传对了没人看。
	if r.BranchID == "" {
		r.BranchID = storeBranchID
	}
	if r.OperDate == "" {
		r.OperDate = operDate
	}
	if r.Source == "" {
		r.Source = source
	}
	return r, nil
}

// fakeBearer 记录 bearer 解析器被调用时的门店，并可返回受控错误。
//
// ⚠️ 不去 ctx 里翻 token：cubeclient 的 bearer ctx key 是**未导出**的，
//
//	用"ctx.Value(某个私钥)"来断言等于把实现细节抄进测试，
//	重构即红。真正要守的性质是"Run 之前一定换过 token"，
//	而那从外部只观测得到一件事：**换失败时 Run 不会被调用**。
type fakeBearer struct {
	branch  string
	calls   int
	token   string
	failure error
}

func (f *fakeBearer) Bearer(_ context.Context, branchID string) (string, error) {
	f.calls++
	f.branch = branchID
	if f.failure != nil {
		return "", f.failure
	}
	if f.token == "" {
		return "test-service-token", nil
	}
	return f.token, nil
}

func newCollectRouter(c *fakeCollector) *gin.Engine {
	return newCollectRouterWith(c, &fakeBearer{})
}

func newCollectRouterWith(c *fakeCollector, b collect.BearerResolver) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	handler.NewDailyCollectHandler(c, b).RegisterRoutes(r)
	return r
}

func doReq(r *gin.Engine, method, target, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestCollectRouteAcceptsPost 钉住 **POST 路由存在**。
//
// ✅ MEASURED 2026-10-18：dapr scheduler 一律 POST 回调（生产日志
// `msg=http method=POST path=/job/... status=200`），而本端点此前**只注册了
// GET** ⇒ 定时采集每次都落不到 handler 上。
//
// 而症状安静得可怕：job 注册成功、服务正常、healthz 200、报表照出，
// 没有任何一行说"采集没跑过"。所以这条断言不能省。
func TestCollectRouteAcceptsPost(t *testing.T) {
	c := &fakeCollector{}
	r := newCollectRouter(c)

	w := doReq(r, http.MethodPost,
		"/sales/daily/collect?store=00&source=initial", "")

	if w.Code != http.StatusOK {
		t.Fatalf("POST 采集端点 status = %d（body=%s）；"+
			"dapr scheduler 只发 POST，只注册 GET 会让定时采集永远落不了地",
			w.Code, w.Body.String())
	}
	if c.calls != 1 {
		t.Fatalf("采集器被调用 %d 次，期望 1 次", c.calls)
	}
}

// TestCollectReadsBranchFromJobBody 是本组最要紧的一条。
//
// ⚠️ job 注册体里门店的键叫 **branch**，不是 store：
//
//	data := {"trigger":"dapr-job","job":..., "branch": b.ID, "source": spec.source}
//
// 只认 store 的话，scheduler 回调会得到 400「必须指定门店」，
// 而门店明明就在 body 里 —— 排查时会被引到"门店清单为空"那个错方向。
func TestCollectReadsBranchFromJobBody(t *testing.T) {
	c := &fakeCollector{}
	r := newCollectRouter(c)

	body := `{"trigger":"dapr-job","job":"sales-daily-collect-00-initial",` +
		`"branch":"00","source":"initial"}`
	w := doReq(r, http.MethodPost, "/sales/daily/collect", body)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）；job data 里的 branch 必须被认成门店",
			w.Code, w.Body.String())
	}
	// ⚠️ 用字面量 "00" 断言，不用 c.branch 之外的东西推导。
	if c.branch != "00" {
		t.Fatalf("采集器收到的门店 = %q，期望 %q", c.branch, "00")
	}
}

// TestCollectReadsStoreFromBody store 键也要认（人工用 POST 触发时用）。
func TestCollectReadsStoreFromBody(t *testing.T) {
	c := &fakeCollector{}
	r := newCollectRouter(c)

	w := doReq(r, http.MethodPost, "/sales/daily/collect",
		`{"store":"B002","source":"corrected"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", w.Code, w.Body.String())
	}
	if c.branch != "B002" {
		t.Fatalf("采集器收到的门店 = %q，期望 %q", c.branch, "B002")
	}
	if c.source != salesdaily.CollectSourceCorrected {
		t.Errorf("source = %q，期望 corrected", c.source)
	}
}

// TestCollectQueryBeatsBody POST + query 同时给时以 query 为准。
//
// 为什么要在意：`?oper_date=2026-10-16` 是"重采某一天"唯一入口。
// 若 body 的值把 query 盖掉，运营点了"重跑 10-16"，
// 系统悄悄采了**别的某一天**并覆盖掉 —— 而界面上什么异常都没有。
//
// ⚠️ 必须让 query 与 body **都带 oper_date 且取值不同**：
//
//	只让 query 带（body 为空）的话，"query 优先"与"body 优先"
//	两种实现的输出逐字相同，测试永远绿 —— 那它测的是
//	"body 为空时不丢 query"，不是"query 优先"。
func TestCollectQueryBeatsBody(t *testing.T) {
	c := &fakeCollector{}
	r := newCollectRouter(c)

	body := `{"branch":"00","source":"initial","oper_date":"2026-10-10"}`
	w := doReq(r, http.MethodPost,
		"/sales/daily/collect?oper_date=2026-10-16&store=00&source=initial", body)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", w.Code, w.Body.String())
	}
	if c.date != "2026-10-16" {
		t.Fatalf("采集营业日 = %q，期望 2026-10-16（query 的 oper_date=2026-10-16 "+
			"必须压过 body 的 2026-10-10）", c.date)
	}
}

// TestCollectQueryStoreBeatsBodyBranch 同理，门店也必须以 query 为准。
//
// 场景：scheduler 回调的 body 里有 branch，而运维用
// `POST /sales/daily/collect?store=B002` 临时补采另一家店。
// 若 body 的 branch 压过 query，就会去采 body 里那家 —— 采错店却报成功。
func TestCollectQueryStoreBeatsBodyBranch(t *testing.T) {
	c := &fakeCollector{}
	r := newCollectRouter(c)

	body := `{"trigger":"dapr-job","branch":"00","source":"initial"}`
	w := doReq(r, http.MethodPost, "/sales/daily/collect?store=B002", body)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", w.Code, w.Body.String())
	}
	if c.branch != "B002" {
		t.Fatalf("采集门店 = %q，期望 B002（query 的 store 必须压过 body 的 branch=00）",
			c.branch)
	}
}

// TestCollectGetStillWorks 人工重跑不能因为加了 POST 就坏掉。
func TestCollectGetStillWorks(t *testing.T) {
	c := &fakeCollector{}
	r := newCollectRouter(c)

	w := doReq(r, http.MethodGet,
		"/sales/daily/collect?store=00&oper_date=2026-10-16&source=initial", "")

	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d（body=%s）；人工重跑路径不能被 POST 改造带坏",
			w.Code, w.Body.String())
	}
	if c.branch != "00" || c.date != "2026-10-16" {
		t.Fatalf("采集入参 = (%q, %q)，期望 (00, 2026-10-16)", c.branch, c.date)
	}
}

// TestCollectGetHasNoBodyStillWorks GET 没有 body，解析必须容忍空。
//
// ⚠️ 直接 ShouldBindJSON 在这里会 EOF —— 而 500 的症状是"人工重跑全挂"，
// 定时链路却是好的，很容易被误判成"只有手动那条坏了"。
func TestCollectGetHasNoBodyStillWorks(t *testing.T) {
	c := &fakeCollector{}
	r := newCollectRouter(c)

	w := doReq(r, http.MethodGet, "/sales/daily/collect?store=00", "")

	if w.Code != http.StatusOK {
		t.Fatalf("GET 无 body 时 status = %d（body=%s）", w.Code, w.Body.String())
	}
}

// TestCollectMissingStoreIsRejected 门店必填，且**不能**默认成全部门店。
func TestCollectMissingStoreIsRejected(t *testing.T) {
	c := &fakeCollector{}
	r := newCollectRouter(c)

	w := doReq(r, http.MethodGet, "/sales/daily/collect", "")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("缺门店 status = %d，期望 400（采到的是路由不明的数据）", w.Code)
	}
	if c.calls != 0 {
		t.Error("缺门店时不得调用采集器")
	}
	var got struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON：%v", err)
	}
	if got.Code != "store_required" {
		t.Errorf("code = %q，期望 store_required", got.Code)
	}
}

// TestCollectUnknownSourceRejected 未知 source 报错而不是当默认。
//
// ⚠️ 若静默当初值，一次校正采集会被记成首采，
// 而"没校正过"与"校正过"在数值上长得一模一样 —— 库里再没有痕迹。
func TestCollectUnknownSourceRejected(t *testing.T) {
	c := &fakeCollector{}
	r := newCollectRouter(c)

	w := doReq(r, http.MethodPost,
		"/sales/daily/collect",
		`{"branch":"00","source":"correct"}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("未知 source status = %d，期望 400", w.Code)
	}
	if c.calls != 0 {
		t.Error("未知 source 时不得调用采集器")
	}
}

// TestCollectCorrectedUsesYesterday 校正采集修**昨天**。
//
// 期望值用字面量推导不了（相对今天），所以这里断言"早于今天且就是昨天"，
// 而不是拿 collect.PrevOperDate 反推 —— 后者会让常量改错也全绿。
func TestCollectCorrectedUsesYesterday(t *testing.T) {
	c := &fakeCollector{}
	r := newCollectRouter(c)

	w := doReq(r, http.MethodPost, "/sales/daily/collect", `{"branch":"00","source":"corrected"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", w.Code, w.Body.String())
	}
	if c.date == "" {
		t.Fatal("校正采集必须定出具体营业日")
	}
	if c.date >= today() {
		t.Fatalf("校正采集的营业日 = %s，期望**昨天**（今天=%s）；"+
			"按今天校正会覆盖一个刚开门、几乎没销售的今天，"+
			"而昨天那份残缺的首采永远留在库里", c.date, today())
	}
}

// TestCollectBadBodyRejected 坏 JSON 必须 400，不能静默当成"没传参数"。
func TestCollectBadBodyRejected(t *testing.T) {
	c := &fakeCollector{}
	r := newCollectRouter(c)

	w := doReq(r, http.MethodPost, "/sales/daily/collect?store=00", `{"branch":`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("坏 body status = %d，期望 400", w.Code)
	}
	if c.calls != 0 {
		t.Error("坏 body 时不得调用采集器")
	}
}

// TestCollectFailureIs503 采集失败一律 503（不是 400：参数已在入口挡掉）。
func TestCollectFailureIs503(t *testing.T) {
	c := &fakeCollector{failure: errBoom}
	r := newCollectRouter(c)

	w := doReq(r, http.MethodGet, "/sales/daily/collect?store=00", "")

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("采集失败 status = %d，期望 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "boom") {
		t.Errorf("响应应包含底层原因，got=%s", w.Body.String())
	}
}

// TestCollectResponseEchoesOperands 响应回显入参。
//
// 为什么单独一条：handler 里 oper_date 可以由 source 推导，
// 若响应不给出实际采的是哪天，运维看日志就只能靠猜。
func TestCollectResponseEchoesOperands(t *testing.T) {
	c := &fakeCollector{result: collect.Result{
		BranchID: "00", OperDate: "2026-10-16",
		Source: salesdaily.CollectSourceInitial, Saved: true,
		Categories: 3, Suppliers: 2, Products: 11,
	}}
	r := newCollectRouter(c)

	w := doReq(r, http.MethodGet,
		"/sales/daily/collect?store=00&oper_date=2026-10-16", "")

	var got struct {
		Store      string `json:"store"`
		OperDate   string `json:"oper_date"`
		Source     string `json:"source"`
		Saved      bool   `json:"saved"`
		Categories int    `json:"categories"`
		Suppliers  int    `json:"suppliers"`
		Products   int    `json:"products"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON：%v（%s）", err, w.Body.String())
	}
	if got.OperDate != "2026-10-16" {
		t.Errorf("oper_date = %q，期望 2026-10-16", got.OperDate)
	}
	if got.Store != "00" {
		t.Errorf("store = %q，期望 00", got.Store)
	}
	if !got.Saved {
		t.Error("saved 应为 true")
	}
	// 各维度行数必须原样回传：任一为 0 而没报错，正是"落库了但不完整"的样子。
	if got.Categories != 3 || got.Suppliers != 2 || got.Products != 11 {
		t.Errorf("维度行数回显不对：%+v", got)
	}
}


// ---- 机器身份：换 token 必须发生在 Run 之前，且失败要分流 ----

// TestCollectDoesNotRunWhenBearerFails ⭐
//
// 换不到机器身份时**绝不能**继续跑采集。
//
// ⚠️ 为什么这条最重要：若先 Run 再换 token，采集会真的去查一次 cube，
//
//	而 cube-gateway 的 auth-validator 必然 401 —— 于是错误变成
//	"cube 拒绝调用者凭证"，把人一路引向 cube / 网关 / token 有效期，
//	真因（这台门店的机器身份不可用）完全不出现在链路上。
func TestCollectDoesNotRunWhenBearerFails(t *testing.T) {
	c := &fakeCollector{}
	b := &fakeBearer{failure: collect.ErrCollectDisabled}
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/sales/daily/collect?store=00", nil)

	newCollectRouterWith(c, b).ServeHTTP(w, req)

	if c.calls != 0 {
		t.Fatalf("换 token 失败时仍调用了采集器 %d 次 —— 顺序错了", c.calls)
	}
	if b.calls != 1 {
		t.Fatalf("bearer 解析器调用 %d 次，期望 1", b.calls)
	}
	if b.branch != "00" {
		t.Errorf("解析器拿到的门店 = %q，期望 00", b.branch)
	}
	// 已关停 = 用户主动关的，**不该**告诉他"重试"，
	// 而应引导去重新开启 ⇒ 409 而不是 503。
	if w.Code != http.StatusConflict {
		t.Errorf("已关停时状态码 = %d，期望 409（503 会让前端弹「重试」，把人引向错误的动作）", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body["code"] != "collect_disabled" {
		t.Errorf("code = %v，期望 collect_disabled（前端按 code 分流）", body["code"])
	}
}

// TestCollectNotEnabledIsDistinctFromDisabled
//
// "没开过" 与 "被关停" 必须是**两个不同的 code**。
//
// 合并成一句"采集未开启"的话，前端只有一种引导；
// 而这两种场景下用户的下一步是一样的（去开启），文案可以复用，
// 但 code 必须能让人区分"第一次开"与"重新开"。
func TestCollectNotEnabledIsDistinctFromDisabled(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"没开过", collect.ErrCollectNotEnabled, "collect_not_enabled"},
		{"被关停", collect.ErrCollectDisabled, "collect_disabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &fakeCollector{}
			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodPost, "/sales/daily/collect?store=00", nil)

			newCollectRouterWith(c, &fakeBearer{failure: tc.err}).ServeHTTP(w, req)

			var body map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if body["code"] != tc.want {
				t.Errorf("code = %v，期望 %v", body["code"], tc.want)
			}
			if c.calls != 0 {
				t.Errorf("采集器被调用了 %d 次，期望 0", c.calls)
			}
		})
	}
}

// TestCollectIdentityFailureIs503Not409
//
// "凭据解不开 / 换不到 token" 是**真的服务侧故障** ⇒ 503（可重试），
// 不能和"没开启"挤在同一个 409 里 —— 否则运维看告警时
// 分不清"用户没点开启"和"密钥坏了"，而后者要立刻处理。
func TestCollectIdentityFailureIs503Not409(t *testing.T) {
	c := &fakeCollector{}
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/sales/daily/collect?store=00", nil)

	newCollectRouterWith(c, &fakeBearer{failure: errors.New("采集凭据：解密失败（密文被篡改）")}).
		ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("凭据损坏时状态码 = %d，期望 503", w.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["code"] != "collect_identity_unavailable" {
		t.Errorf("code = %v，期望 collect_identity_unavailable", body["code"])
	}
}

// TestCollectWithoutBearerResolverFailsLoudly
//
// 没装配解析器（主密钥没配）时必须**明确失败**，
// 而不是无声地不带 Authorization 去打 cube ——
// 后者的症状是 "cube 401"，离真因（本服务没装机器身份）极远。
func TestCollectWithoutBearerResolverFailsLoudly(t *testing.T) {
	c := &fakeCollector{}
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/sales/daily/collect?store=00", nil)

	newCollectRouterWith(c, nil).ServeHTTP(w, req)

	if c.calls != 0 {
		t.Fatalf("没装配解析器时仍调用了采集器 %d 次", c.calls)
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("状态码 = %d，期望 503", w.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	msg, _ := body["message"].(string)
	if !strings.Contains(msg, "bearer 解析器未装配") {
		t.Errorf("message = %q，期望点名缺的是 bearer 解析器", msg)
	}
}
