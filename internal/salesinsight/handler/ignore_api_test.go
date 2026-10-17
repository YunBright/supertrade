package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/salesinsight/fact/supplier"
	"github.com/YunBright/supertrade/internal/salesinsight/ignore"
)

func newIgnoreHandler(t *testing.T) (*gin.Engine, *ignore.Store) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	st := ignore.NewStore(t.TempDir())
	New(nil).WithIgnores(st).RegisterRoutes(r)
	return r, st
}

// doIgnore 发一个带 X-Branch-ID 的请求。
func doIgnore(t *testing.T, r *gin.Engine, method, path, branch, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if branch != "" {
		req.Header.Set("X-Branch-ID", branch)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out map[string]any
	// ⚠️ 所有状态码都要解析 —— 错误响应（400/503）里同样有 code/message，
	// 只解 200 会让"错误码断言"读到 nil 而恒失败（踩过一次）。
	if len(w.Body.Bytes()) > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("响应不是 JSON: %v\nbody=%s", err, w.Body.String())
		}
	}
	return w, out
}

// ⚠️ **门店只能来自 X-Branch-ID，请求体里的 store 必须被忽略**。
//
// 这条是越权防线：如果允许请求体指定门店，
// A 店的登录态就能改 B 店的名单 —— 而"谁能改哪家店的配置"
// 必须由中间件的鉴权决定，不能由参数决定。
//
// 判别力：把 handler 改成优先读 body.store，本用例立刻失败。
func TestIgnoreList_BranchComesFromHeaderOnly(t *testing.T) {
	r, st := newIgnoreHandler(t)

	// 先给 B 店存一份。
	if _, err := st.Save("branch-B", []ignore.Entry{{Code: "B1", Name: "B店自营"}}); err != nil {
		t.Fatal(err)
	}

	// A 店登录态，body 里谎称 B 店，试图覆盖 B 的名单。
	body := `{"store":"branch-B","entries":[{"code":"HACK","name":"越权写入"}]}`
	w, _ := doIgnore(t, r, http.MethodPut, "/insights/supplier-ignore", "branch-A", body)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT 应成功，实际 %d: %s", w.Code, w.Body.String())
	}

	// B 店的名单必须纹丝不动。
	lB, err := st.Load("branch-B")
	if err != nil {
		t.Fatal(err)
	}
	if len(lB.Entries) != 1 || lB.Entries[0].Code != "B1" {
		t.Fatalf("B 店名单被越权改写: %+v", lB.Entries)
	}
	// 写进去的应是 A 店自己的名单。
	lA, err := st.Load("branch-A")
	if err != nil {
		t.Fatal(err)
	}
	if len(lA.Entries) != 1 || lA.Entries[0].Code != "HACK" {
		t.Errorf("应写入 A 店名单，实际 %+v", lA.Entries)
	}
}

// 缺 X-Branch-ID 必须 400 + 可操作的提示，而不是静默按"全局"处理。
func TestIgnoreList_MissingBranchRejected(t *testing.T) {
	r, _ := newIgnoreHandler(t)
	for _, c := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/insights/supplier-ignore", ""},
		{http.MethodPut, "/insights/supplier-ignore", `{"entries":[]}`},
	} {
		w, out := doIgnore(t, r, c.method, c.path, "", c.body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s %s 缺门店应 400，实际 %d", c.method, c.path, w.Code)
		}
		if out["code"] != "store_required" {
			t.Errorf("错误码应为 store_required，实际 %v", out["code"])
		}
	}
}

// ⚠️ 未装配名单存储时返回 503，**而不是**空名单。
//
// "服务没装配"与"这家店没配过"对运营是完全不同的两件事：
// 前者要运维修，后者要运营去配。返回空名单会让运营去反复点保存，
// 而真正的问题在服务端。
//
// 判别力：把 WithIgnores(nil) 时的分支去掉，本用例失败。
func TestIgnoreList_StoreNotReadyReturns503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	New(nil).RegisterRoutes(r) // ⚠️ 不注入名单存储

	w, out := doIgnore(t, r, http.MethodGet, "/insights/supplier-ignore", "branch-A", "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("未装配时应 503，实际 %d: %s", w.Code, w.Body.String())
	}
	if out["code"] != "ignore_store_not_ready" {
		t.Errorf("错误码 = %v", out["code"])
	}
}

// 读回的名单必须带 configured 标志 —— App 靠它区分
// "这家店确实全是外采" 与 "还没配"。
func TestIgnoreList_GetReturnsConfiguredFlag(t *testing.T) {
	r, st := newIgnoreHandler(t)

	w, out := doIgnore(t, r, http.MethodGet, "/insights/supplier-ignore", "branch-A", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET 应 200，实际 %d", w.Code)
	}
	l := out["list"].(map[string]any)
	if l["configured"] != false {
		t.Errorf("未保存过应 configured=false，实际 %v", l["configured"])
	}

	if _, err := st.Save("branch-A", nil); err != nil {
		t.Fatal(err)
	}
	_, out = doIgnore(t, r, http.MethodGet, "/insights/supplier-ignore", "branch-A", "")
	l = out["list"].(map[string]any)
	if l["configured"] != true {
		t.Errorf("显式保存空名单后应 configured=true，实际 %v", l["configured"])
	}
}

// ⚠️ 超限必须 400，**不能**静默截断。
//
// 截断会让运营以为保存成功了，而末尾几条消失 ——
// "配了却不生效"是最难排查的一类问题。
func TestIgnoreList_TooManyEntriesRejected(t *testing.T) {
	r, st := newIgnoreHandler(t)
	var sb strings.Builder
	sb.WriteString(`{"entries":[`)
	for i := 0; i < MaxIgnoreEntries+1; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"code":"C` + itoaTest(i) + `"}`)
	}
	sb.WriteString(`]}`)

	w, out := doIgnore(t, r, http.MethodPut, "/insights/supplier-ignore", "branch-A", sb.String())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("超限应 400，实际 %d", w.Code)
	}
	if out["code"] != "too_many_entries" {
		t.Errorf("错误码 = %v", out["code"])
	}
	// ⚠️ 确认**什么都没写进去**。
	l, _ := st.Load("branch-A")
	if len(l.Entries) != 0 {
		t.Errorf("被拒的请求不应写入任何条目，实际 %d 条", len(l.Entries))
	}
	if l.Configured {
		t.Error("被拒的请求不应把门店标记成已配置")
	}
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// 低于上限的正常保存必须成功且能读回。
func TestIgnoreList_PutGetRoundTrip(t *testing.T) {
	r, _ := newIgnoreHandler(t)
	body := `{"entries":[{"code":"019","name":"自购蔬菜","reason":"门店自营"},{"code":"084","name":"水果（自营）"}]}`

	w, _ := doIgnore(t, r, http.MethodPut, "/insights/supplier-ignore", "branch-A", body)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT 应 200，实际 %d: %s", w.Code, w.Body.String())
	}

	_, out := doIgnore(t, r, http.MethodGet, "/insights/supplier-ignore", "branch-A", "")
	entries := out["list"].(map[string]any)["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("应存下 2 条，实际 %d", len(entries))
	}
	first := entries[0].(map[string]any)
	if first["code"] != "019" || first["name"] != "自购蔬菜" {
		t.Errorf("首条内容不对: %+v", first)
	}
	if first["reason"] != "门店自营" {
		t.Errorf("reason 未落库: %+v", first)
	}
}

// ⚠️ 推荐端点必须走 supplier.Record 的 json tag。
//
// encoding/json 匹配**不区分大小写**，所以 tag 写成 custFlag
// 也不会报错 —— 只会永远取不到值（结果一律是空 cust_flag，
// 于是"客户"这条推荐永远不触发，而且没有任何报错）。
//
// 这条测试用"必须有 cust_flag='C' 的记录被判为客户"来锁住契约。
func TestIgnoreSuggest_UsesSupplierRecordJSONTags(t *testing.T) {
	r, _ := newIgnoreHandler(t)
	body := `{"records":[
		{"code":"01","name":"运营部","type":"1","cust_flag":"C"},
		{"code":"038","name":"泰鼎粮油","type":"1","cust_flag":"S"}
	]}`
	w, out := doIgnore(t, r, http.MethodPost, "/insights/supplier-ignore/suggest", "branch-A", body)
	if w.Code != http.StatusOK {
		t.Fatalf("suggest 应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	sug := out["suggestions"].([]any)
	if len(sug) != 1 {
		t.Fatalf("应只推荐「运营部」1 条（客户），实际 %d 条: %+v", len(sug), sug)
	}
	s0 := sug[0].(map[string]any)
	if s0["code"] != "01" {
		t.Errorf("推荐对象不对: %+v", s0)
	}
	// ⚠️ rule 必须是 cust_flag —— 证明 tag 真的生效了。
	if s0["rule"] != "cust_flag" {
		t.Errorf("rule 应为 cust_flag（说明 cust_flag 字段解析成功），实际 %v", s0["rule"])
	}
}

// fakeDict 是一个可控的字典源。
//
// ⚠️ 它返回**真实的 supplier.Record**（而不是自造类型），
//   否则测到的是 fake 的形状而不是接口的形状 ——
//   json tag 漂移这类问题恰好在两者不一致时才暴露。
type fakeDict struct {
	recs      []supplier.Record
	err       error
	gotBranch string
	gotQuery  string
	gotLimit  int
	gotOffset int
	calls     int
}

func (f *fakeDict) SupplierDictPage(_ context.Context, branch string,
	offset, limit int, query string) ([]supplier.Record, error) {
	f.calls++
	f.gotBranch, f.gotOffset, f.gotLimit, f.gotQuery = branch, offset, limit, query
	if f.err != nil {
		return nil, f.err
	}
	return f.recs, nil
}

// ⚠️ **正常用法必须由服务端自查字典**，不能要求 App 传 records。
//
// 判别力：把 postIgnoreSuggest 里 `if len(recs) == 0` 那个分支删掉，
// 本用例立刻 400。
//
// 为什么这条重要：App 侧 catalog 的 `/suppliers` **不返回**
// `type` / `cust_flag`（实测 2026-10-17），所以 App 传上来的字典
// 判定会退化成只看名称 —— 置信度最低、最容易误判的那一层。
// 更糟的是两边数据源可能不同，运营会看到"配好了但报告里还是自用"。
func TestIgnoreSuggest_ServerQueriesDictWhenRecordsEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	d := &fakeDict{recs: []supplier.Record{
		{Code: "019", Name: "自购蔬菜", Type: "1", CustFlag: "S"},
		{Code: "101", Name: "乐园公司", Type: "5", CustFlag: "S"},
		{Code: "038", Name: "泰鼎粮油", Type: "1", CustFlag: "S"},
	}}
	New(nil).WithIgnores(ignore.NewStore(t.TempDir())).WithDict(d).RegisterRoutes(r)

	w, out := doIgnore(t, r, http.MethodPost, "/insights/supplier-ignore/suggest", "branch-A", `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("空 records 应由服务端自查字典，实际 %d: %s", w.Code, w.Body.String())
	}
	if d.calls != 1 {
		t.Fatalf("应调用字典 1 次，实际 %d 次", d.calls)
	}
	// ⚠️ 门店必须来自头。
	if d.gotBranch != "branch-A" {
		t.Errorf("字典查询的门店 = %q，期望 branch-A", d.gotBranch)
	}
	// 应推荐 019（名称层）与 101（类型层），不推荐 038。
	sug := out["suggestions"].([]any)
	if len(sug) != 2 {
		t.Fatalf("应推荐 2 条，实际 %d: %+v", len(sug), sug)
	}
}

// 字典查询失败必须 5xx + 可读原因，而不是静默返回空推荐。
//
// 静默空推荐的危害：用户看到"没找到任何自营供应商"，
// 于是把空名单保存成"这家店确实没有自用" —— 一个错误的结论被固化。
func TestIgnoreSuggest_DictFailureIsVisible(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	d := &fakeDict{err: errors.New("cube 502")}
	New(nil).WithIgnores(ignore.NewStore(t.TempDir())).WithDict(d).RegisterRoutes(r)

	w, out := doIgnore(t, r, http.MethodPost, "/insights/supplier-ignore/suggest", "branch-A", `{}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("字典失败应 5xx，实际 %d", w.Code)
	}
	if out["suggestions"] != nil {
		t.Errorf("失败时不得返回空推荐（会被误当成「没有自用供应商」）: %+v", out)
	}
}

// dict 端点：空结果必须是 `[]` 而不是 null。
//
// ⚠️ App 侧 `raw is! List` 会把 null 判成"响应格式异常"而报错，
// 而"这家店没有供应商"是**正常**结果 —— 不该报错。
//
// 判别力：把 `if recs == nil { recs = [] }` 去掉，本用例立刻红。
func TestIgnoreDict_EmptyResultIsEmptyArrayNotNull(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	d := &fakeDict{} // 返回 nil
	New(nil).WithIgnores(ignore.NewStore(t.TempDir())).WithDict(d).RegisterRoutes(r)

	w, out := doIgnore(t, r, http.MethodGet, "/insights/supplier-ignore/dict", "branch-A", "")
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	raw, ok := out["records"]
	if !ok {
		t.Fatal("响应缺 records")
	}
	arr, isList := raw.([]any)
	if !isList {
		t.Fatalf("records 必须是数组（null 会让 App 报格式异常），实际 %T", raw)
	}
	if len(arr) != 0 {
		t.Errorf("应为空数组，实际 %d 条", len(arr))
	}
	// 顺便锁住响应体里就是 `[]` 而不是 null。
	if !strings.Contains(w.Body.String(), `"records":[]`) {
		t.Errorf("响应体应为 \"records\":[]，实际 %s", w.Body.String())
	}
}

// dict 端点必须透传搜索词与分页参数。
func TestIgnoreDict_PassesQueryAndPaging(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	d := &fakeDict{recs: []supplier.Record{{Code: "019", Name: "自购蔬菜"}}}
	New(nil).WithIgnores(ignore.NewStore(t.TempDir())).WithDict(d).RegisterRoutes(r)

	// ⚠️ 搜索词用 ASCII，避免"百分号编码 + 中文"在脚本/工具链里被改写
	//   （本次实测 %E8%87%87 被吃掉一个字节，解出 "臇菜"）。
	//   真实的中文搜索由 App 侧发，Go 的 url.Query() 会正确解码。
	w, _ := doIgnore(t, r, http.MethodGet,
		"/insights/supplier-ignore/dict?q=veg&offset=20&limit=50", "branch-A", "")
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	// ⚠️ 参数名是 `q`（与 catalog 的 /suppliers?q= 一致），
	//   不是 `query` —— 两处不一致会让 App 传了搜索词却拿到全量，
	//   而表现是"搜索框输了字，列表没变化"。
	if d.gotQuery != "veg" {
		t.Errorf("搜索词未透传：%q（检查 handler 用的是 c.Query(\"q\")）", d.gotQuery)
	}
	if d.gotOffset != 20 || d.gotLimit != 50 {
		t.Errorf("分页未透传：offset=%d limit=%d", d.gotOffset, d.gotLimit)
	}
}

// ⚠️ limit 超过上限必须被**夹住**，不能照单全收。
//
// cube 的 supplier 是 duck 快照，一次拉 5000 条会拖慢接口
// （且它是 per-branch 的，会影响别的请求）。
func TestIgnoreDict_ClampsLimitAndFixesBadOffset(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	d := &fakeDict{}
	New(nil).WithIgnores(ignore.NewStore(t.TempDir())).WithDict(d).RegisterRoutes(r)

	doIgnore(t, r, http.MethodGet,
		"/insights/supplier-ignore/dict?limit=99999&offset=-5", "branch-A", "")
	if d.gotLimit != MaxDictPageSize {
		t.Errorf("limit 应夹到 %d，实际 %d", MaxDictPageSize, d.gotLimit)
	}
	if d.gotOffset != 0 {
		t.Errorf("负 offset 应归零，实际 %d", d.gotOffset)
	}

	// 非法 limit 回落默认值而不是报错。
	doIgnore(t, r, http.MethodGet, "/insights/supplier-ignore/dict?limit=abc", "branch-A", "")
	if d.gotLimit != defaultDictPageSize {
		t.Errorf("非法 limit 应回落 %d，实际 %d", defaultDictPageSize, d.gotLimit)
	}
}

// 未装配字典源时 dict/suggest 必须 503，而不是"空字典"。
func TestIgnoreDict_NotReadyReturns503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	New(nil).WithIgnores(ignore.NewStore(t.TempDir())).RegisterRoutes(r)

	w, out := doIgnore(t, r, http.MethodGet, "/insights/supplier-ignore/dict", "branch-A", "")
	if w.Code != http.StatusServiceUnavailable || out["code"] != "dict_not_ready" {
		t.Errorf("dict 未装配应 503/dict_not_ready，实际 %d/%v", w.Code, out["code"])
	}
	w2, out2 := doIgnore(t, r, http.MethodPost, "/insights/supplier-ignore/suggest", "branch-A", `{}`)
	if w2.Code != http.StatusServiceUnavailable || out2["code"] != "dict_not_ready" {
		t.Errorf("suggest 未装配应 503/dict_not_ready，实际 %d/%v", w2.Code, out2["code"])
	}
}

// 缺门店必须 400：字典是 per-branch 的，缺门店等于拿不到该店数据。
func TestIgnoreDict_MissingBranchRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	d := &fakeDict{}
	New(nil).WithIgnores(ignore.NewStore(t.TempDir())).WithDict(d).RegisterRoutes(r)

	w, out := doIgnore(t, r, http.MethodGet, "/insights/supplier-ignore/dict", "", "")
	if w.Code != http.StatusBadRequest || out["code"] != "store_required" {
		t.Errorf("缺门店应 400/store_required，实际 %d/%v", w.Code, out["code"])
	}
	if d.calls != 0 {
		t.Errorf("缺门店时不应查询字典（会拿到路由不明的跨店数据）")
	}
}

// 非法 JSON 必须 400。
func TestIgnoreSuggest_BadJSON(t *testing.T) {
	r, _ := newIgnoreHandler(t)
	w, out := doIgnore(t, r, http.MethodPost, "/insights/supplier-ignore/suggest", "branch-A", "{broken")
	if w.Code != http.StatusBadRequest {
		t.Errorf("非法 JSON 应 400，实际 %d", w.Code)
	}
	if out["code"] != "bad_request" {
		t.Errorf("错误码 = %v", out["code"])
	}
}

// generate 端点必须仍然正常 —— 装配改动不能影响既有主链路。
func TestGenerate_仍可用(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	New(&fakeSvc{insight: defaultAudit().Insight, audit: defaultAudit()}).
		WithIgnores(ignore.NewStore(t.TempDir())).
		RegisterRoutes(r)

	req := httptest.NewRequest(http.MethodPost, "/insights/generate",
		strings.NewReader(`{"store":"00","kind":"daily"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("generate 应仍可用，实际 %d: %s", w.Code, w.Body.String())
	}
}
