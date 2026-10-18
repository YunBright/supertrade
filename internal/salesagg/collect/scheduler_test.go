package collect

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---- fakes ----

type fakeBranchLister struct {
	branches []Branch
	err      error
	calls    int
}

func (f *fakeBranchLister) ListActiveBranches(context.Context) ([]Branch, error) {
	f.calls++
	return f.branches, f.err
}

type fakeJobClient struct {
	registered []string
	deleted    []string
	cronOf     map[string]string
	dataOf     map[string]map[string]any
	addrOf     map[string]string
	failOn     map[string]error
}

func newFakeJobClient() *fakeJobClient {
	return &fakeJobClient{
		cronOf: map[string]string{}, dataOf: map[string]map[string]any{},
		addrOf: map[string]string{}, failOn: map[string]error{},
	}
}

func (f *fakeJobClient) Register(_ context.Context, name, schedule, address string, data map[string]any) error {
	if err, ok := f.failOn[name]; ok {
		return err
	}
	f.registered = append(f.registered, name)
	f.cronOf[name] = schedule
	f.addrOf[name] = address
	f.dataOf[name] = data
	return nil
}

func (f *fakeJobClient) Delete(_ context.Context, name string) error {
	f.deleted = append(f.deleted, name)
	return nil
}

// ---- cron 换算 ----

// TestCronExprHasSixFields ⭐
//
// ✅ MEASURED 2026-10-14：dapr scheduler **只接受 6 段** cron
// （秒 分 时 日 月 周），传 5 段直接 500：
//
//	job failed validation: expected exactly 6 fields, found 5: [0 14 * * *]
//
// ⚠️ 这条判据必须留在测试里而不是注释里：
//
//	注释不会因为有人改格式而变红，而 5 段 cron 的后果是
//	**所有采集任务一个都注册不上**，而服务照常启动、报表照出。
func TestCronExprHasSixFields(t *testing.T) {
	for _, hm := range []string{InitialCollectTime, CorrectedCollectTime, "00:00", "23:59"} {
		expr, err := CronExpr(hm)
		if err != nil {
			t.Fatalf("CronExpr(%q)：%v", hm, err)
		}
		if n := len(strings.Fields(expr)); n != 6 {
			t.Errorf("CronExpr(%q) = %q，字段数 %d，期望 6", hm, expr, n)
		}
	}
}

// TestCronExprAppliesUTCOffset ⭐ 调度器按 UTC 解释 cron。
//
// ✅ MEASURED 2026-10-14：调度器容器 distroless（无 shell）读不到 TZ，
// 所以 cron 是 UTC。直接写本地时刻会**整整差一个时区偏移**，
// 而症状只是"采集时间不对"，**没有任何报错**。
func TestCronExprAppliesUTCOffset(t *testing.T) {
	_, offsetSec := time.Now().Zone()
	offsetMin := offsetSec / 60
	if offsetMin == 0 {
		t.Skip("本机就在 UTC，偏移为 0 时这条判据无意义")
	}
	for _, hm := range []string{"22:45", "01:05", "00:30"} {
		var h, m int
		fmt.Sscanf(hm, "%d:%d", &h, &m)
		wantUTC := ((h*60+m-offsetMin)%1440 + 1440) % 1440
		expr, err := CronExpr(hm)
		if err != nil {
			t.Fatalf("CronExpr(%q)：%v", hm, err)
		}
		var sec, umin, uhour int
		if _, err := fmt.Sscanf(expr, "%d %d %d", &sec, &umin, &uhour); err != nil {
			t.Fatalf("解析 %q：%v", expr, err)
		}
		if got := uhour*60 + umin; got != wantUTC {
			t.Errorf("CronExpr(%q) = %q（UTC %d:%02d），期望 UTC %d:%02d（本地偏移 %+d 分钟）",
				hm, expr, uhour, umin, wantUTC/60, wantUTC%60, offsetMin)
		}
	}
}

// TestParseHMRejectsLooseFormat 时刻必须是补零的两位数。
//
// ⚠️ `time.Parse("15:04", "9:45")` 是**成功**的（Go 对该布局容忍单数字小时），
//
//	于是配置里会同时存在 "9:45" 与 "09:45" 两种写法，
//	之后每次比较都要考虑两种。判据：**在入口收紧到唯一一种**。
func TestParseHMRejectsLooseFormat(t *testing.T) {
	for _, bad := range []string{"9:45", "09:5", "24:00", "09:60", "22", "", "22:4x", "22:45:00"} {
		if _, err := CronExpr(bad); err == nil {
			t.Errorf("CronExpr(%q) 应报错", bad)
		}
	}
	if _, err := CronExpr("22:45"); err != nil {
		t.Errorf("CronExpr(22:45) 不该报错：%v", err)
	}
	// ⚠️ 首尾空白**允许**：配置值来自 env / DB，多余空格是常态。
	//	而它必须在测试里被钉住 —— 否则下一个人为了"更严格"把它改成报错，
	//	症状是运维在 env 里多敲了一个空格，采集 job 全没了。
	padded, err := CronExpr("  22:45  ")
	if err != nil {
		t.Fatalf("CronExpr(\"  22:45  \") 应容忍首尾空白：%v", err)
	}
	plain, _ := CronExpr("22:45")
	if padded != plain {
		t.Errorf("带空白的结果 = %q，应与不带空白一致 %q", padded, plain)
	}
}

// ---- job 名 ----

// TestJobNameSanitizesBranchID ⭐
//
// 门店 id 会被写进 job 名，而 job 名出现在 dapr 的 **URL 路径**里。
// 只挡 `/` 不够 —— `\`、`?`、`#` 在 URL 与文件系统里同样改变解析。
// 而门店 id 来自 auth（运营可录入），不是可证的 uuid。
//
// ⚠️ 后果是两个门店撞成同一个 job 名时**注册全部成功**，
// 于是只有一个门店在采，而日志里没有任何异常。
func TestJobNameSanitizesBranchID(t *testing.T) {
	cases := []struct {
		branch string
		src    string
		want   string
	}{
		{"00", "initial", "sales-daily-collect-00-initial"},
		{"branch-A1", "corrected", "sales-daily-collect-branch-A1-corrected"},
		{"../../etc", "initial", "sales-daily-collect-______etc-initial"}, // `.` 与 `/` 都不在白名单内
		{`a\b`, "initial", "sales-daily-collect-a_b-initial"},
		{"a b?c#d", "initial", "sales-daily-collect-a_b_c_d-initial"},
	}
	for _, tc := range cases {
		if got := JobName(tc.branch, tc.src); got != tc.want {
			t.Errorf("JobName(%q,%q) = %q，期望 %q", tc.branch, tc.src, got, tc.want)
		}
	}
	// ⚠️ 不同门店绝不能撞名。
	a := JobName("../x", "initial")
	b := JobName("../y", "initial")
	if a == b {
		t.Errorf("不同门店撞了同一个 job 名 %q —— 只有一个门店会被采", a)
	}
}

// TestJobNameEmptyBranchIsStillDistinct 空门店 id 也要有确定的名字。
func TestJobNameEmptyBranchIsStillDistinct(t *testing.T) {
	got := JobName("", "initial")
	if got != "sales-daily-collect-_-initial" {
		t.Errorf("JobName(\"\",initial) = %q", got)
	}
}

// fakeEnabledLister 模拟凭据存储的 ListEnabled（已开启采集的门店）。
type fakeEnabledLister struct {
	ids  []string
	err  error
	calls int
}

func (f *fakeEnabledLister) ListEnabled(context.Context) ([]string, error) {
	f.calls++
	return f.ids, f.err
}

// allEnabled 让「在营门店」全部视为已开启。
//
// ⚠️ 这是**显式**表达原有用例的前提，不是随手写的便利函数：
//
//	注册范围从「在营」收窄成「在营 ∩ 已开启」之后，
//	"这家店会拿到 job" 就成了两个条件的交集。
//	把交集的第二项藏进默认行为里，等于让用例名不副实 ——
//	而「关停之后 job 真的被删了」正是本轮最要紧的断言之一，
//	它的反面（开启之后 job 真的被注册）同样不能含糊。
func allEnabled(bl *fakeBranchLister) *fakeEnabledLister {
	ids := make([]string, 0, len(bl.branches))
	for _, b := range bl.branches {
		ids = append(ids, b.ID)
	}
	return &fakeEnabledLister{ids: ids}
}

// ---- 对账 ----

// TestReconcileRegistersTwoJobsPerBranch ⭐
//
// 每家在营门店要有**两个** job：22:45 首采 + 次日 01:05 校正。
//
// ⚠️ 少一个的后果不会报错：
//
//	只有首采 ⇒ 思迅日结晚于 22:45 时，那份残缺事实**永久留在库里**，
//	而月报正好拿它求和 —— 数字偏低、无标记。
func TestReconcileRegistersTwoJobsPerBranch(t *testing.T) {
	bl := &fakeBranchLister{branches: []Branch{{ID: "00"}, {ID: "01"}}}
	jc := newFakeJobClient()
	r := NewReconciler(bl, allEnabled(bl), jc, "http://127.0.0.1:8110/sales/daily/collect")

	res, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile：%v", err)
	}
	if res.Branches != 2 {
		t.Errorf("门店数 = %d，期望 2", res.Branches)
	}
	if res.Jobs != 4 {
		t.Fatalf("注册 job 数 = %d，期望 4（2 门店 × 2 个时刻）", len(jc.registered))
	}
	for _, want := range []string{
		"sales-daily-collect-00-initial",
		"sales-daily-collect-00-corrected",
		"sales-daily-collect-01-initial",
		"sales-daily-collect-01-corrected",
	} {
		if !contains(jc.registered, want) {
			t.Errorf("缺少 job %s，实际注册 %v", want, jc.registered)
		}
	}
}

// TestReconcileUsesCorrectTimes 首采 22:45、校正 01:05，顺序不能反。
//
// ⚠️ 反过来的后果很隐蔽：01:05 采"今天"、22:45 采"昨天"，
//
//	于是每次采集都在采一个刚开门或还没收档的日子，
//	而"当天的事实"永远停留在前一天 —— 月报整体**晚一天**，
//	且每一天的总额都偏低。
//
// ⚠️⚠️ 期望值必须来自**字面量**，不能由 InitialCollectTime 推导：
//
//	写成 `want := CronExpr(InitialCollectTime)` 会让期望值**跟着被测常量一起变**
//	—— 变异把 22:45 改成 01:05 时，期望值也变成 01:05，测试照样全绿。
//	这类"前提写错的断言"测的是"注册值 == 换算(常量)"，
//	而不是"这个业务时刻对不对"。业务时刻必须在这里被钉死。
func TestReconcileUsesCorrectTimes(t *testing.T) {
	// 业务时刻在这里被**字面量**钉死，不用被测常量。
	if InitialCollectTime != "22:45" {
		t.Errorf("首采时刻 = %q，期望 22:45（用户 6:00-22:45 营业，早于收档取到的是半个营业日）", InitialCollectTime)
	}
	if CorrectedCollectTime != "01:05" {
		t.Errorf("校正时刻 = %q，期望 01:05（思迅日结常在午夜后才完成，01:00 太早）", CorrectedCollectTime)
	}

	bl := &fakeBranchLister{branches: []Branch{{ID: "00"}}}
	jc := newFakeJobClient()
	r := NewReconciler(bl, allEnabled(bl), jc, "http://addr")

	if _, err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile：%v", err)
	}

	// 期望值来自**字面量**，与被测常量无关。
	wantInitial, err := CronExpr("22:45")
	if err != nil {
		t.Fatal(err)
	}
	wantCorrected, err := CronExpr("01:05")
	if err != nil {
		t.Fatal(err)
	}
	if got := jc.cronOf["sales-daily-collect-00-initial"]; got != wantInitial {
		t.Errorf("首采 cron = %q，期望 %q", got, wantInitial)
	}
	if got := jc.cronOf["sales-daily-collect-00-corrected"]; got != wantCorrected {
		t.Errorf("校正 cron = %q，期望 %q", got, wantCorrected)
	}
	if jc.cronOf["sales-daily-collect-00-initial"] == jc.cronOf["sales-daily-collect-00-corrected"] {
		t.Error("首采与校正不能是同一个时刻 —— 那等于只有一次采集")
	}
}

// TestReconcilePassesBranchAndSourceToJob job 数据必须带上门店与来源。
//
// ⚠️ 少了 source，回调端点就只能猜"这次采的是哪一天"，
//
//	而 01:00 猜"今天"就会覆盖一个刚开门的日子。
func TestReconcilePassesBranchAndSourceToJob(t *testing.T) {
	bl := &fakeBranchLister{branches: []Branch{{ID: "branch-7"}}}
	jc := newFakeJobClient()
	r := NewReconciler(bl, allEnabled(bl), jc, "http://addr")
	if _, err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile：%v", err)
	}
	d := jc.dataOf["sales-daily-collect-branch-7-corrected"]
	if d == nil {
		t.Fatal("没找到校正 job 的 data")
	}
	if d["branch"] != "branch-7" {
		t.Errorf("data.branch = %v，期望 branch-7", d["branch"])
	}
	if d["source"] != "corrected" {
		t.Errorf("data.source = %v，期望 corrected", d["source"])
	}
	if jc.addrOf["sales-daily-collect-branch-7-corrected"] != "http://addr" {
		t.Errorf("回调地址没透传")
	}
}

// TestReconcileReportsFailuresWithoutGivingUp 单个 job 失败不影响其它。
func TestReconcileReportsFailuresWithoutGivingUp(t *testing.T) {
	bl := &fakeBranchLister{branches: []Branch{{ID: "00"}, {ID: "01"}}}
	jc := newFakeJobClient()
	jc.failOn["sales-daily-collect-00-corrected"] = errors.New("sidecar 502")
	r := NewReconciler(bl, allEnabled(bl), jc, "http://addr")

	res, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("个别 job 失败不该让整次对账失败：%v", err)
	}
	if len(res.Failed) != 1 || res.Failed[0] != "sales-daily-collect-00-corrected" {
		t.Errorf("失败清单 = %v，期望只含那一个", res.Failed)
	}
	if res.Jobs != 3 {
		t.Errorf("成功数 = %d，期望 3（4 个里坏 1 个）", res.Jobs)
	}
}

// TestReconcileFailsLoudlyWhenListerBroken 门店列表读不到必须报错。
//
// ⚠️ 不能"读不到就当零门店"：那会让所有 job 都不更新，
//
//	而函数返回成功 —— 运维看到的是"对账完成"，实际是"什么都没做"。
func TestReconcileFailsLoudlyWhenListerBroken(t *testing.T) {
	bl := &fakeBranchLister{err: errors.New("auth 不可达")}
	jc := newFakeJobClient()
	r := NewReconciler(bl, allEnabled(bl), jc, "http://addr")
	if _, err := r.Reconcile(context.Background()); err == nil {
		t.Fatal("读不到门店列表必须报错")
	}
	if len(jc.registered) != 0 {
		t.Errorf("读不到清单时不该注册任何 job，实际 %v", jc.registered)
	}
}

// TestReconcileEmptyBranchListIsNotAnError auth 说"没有在营门店"是合法答案。
//
// ⚠️ 与"读失败"必须分开：后者报错，前者注册 0 个。
//
//	把两者混起来，会在 auth 临时返回空时把整个对账打成失败。
func TestReconcileEmptyBranchListIsNotAnError(t *testing.T) {
	bl := &fakeBranchLister{branches: nil}
	jc := newFakeJobClient()
	r := NewReconciler(bl, allEnabled(bl), jc, "http://addr")
	res, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("空门店清单不该报错：%v", err)
	}
	if res.Jobs != 0 {
		t.Errorf("应注册 0 个 job，实际 %d", res.Jobs)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// ---- auth 门店列表 ----

// TestAuthBranchListerParsesAndSorts 解析 + 排序 + 丢空 id。
func TestAuthBranchListerParsesAndSorts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ⚠️ 必须走 dapr service invocation：
		//	打错 app-id 会 404，而 404 在这里极易被误读成"auth 里没有门店"。
		//
		// ⚠️ 这里用**字面量 "userd"** 而不是 AuthAppID：
		//	期望值一旦由被测常量推导，把 AuthAppID 改回 "login"
		//	测试照样全绿 —— 而那正是 2026-10-18 上线失败的真实原因。
		want := "/v1.0/invoke/userd/method/auth/branches"
		if r.URL.Path != want {
			t.Errorf("请求路径 = %s，期望 %s", r.URL.Path, want)
		}
		_, _ = w.Write([]byte(`[
			{"id":"01","display_name":"二店"},
			{"id":"","display_name":"没有 id 的脏行"},
			{"id":"00","display_name":"一店"}
		]`))
	}))
	defer srv.Close()

	l := NewAuthBranchLister(srv.URL, "userd")
	got, err := l.ListActiveBranches(context.Background())
	if err != nil {
		t.Fatalf("ListActiveBranches：%v", err)
	}
	if len(got) != 2 {
		t.Fatalf("门店数 = %d，期望 2（空 id 那行必须丢掉）", len(got))
	}
	if got[0].ID != "00" || got[1].ID != "01" {
		t.Errorf("顺序 = %s,%s，期望 00,01（排序让注册顺序稳定）", got[0].ID, got[1].ID)
	}
	if got[0].DisplayName != "一店" {
		t.Errorf("DisplayName = %q", got[0].DisplayName)
	}
}

// TestAuthBranchListerErrors 状态码不对必须报错。
//
// ⚠️ 不报错的话读到的会是空列表 → 注册 0 个 job →
//
//	"采集悄悄停了"，而对账函数返回成功。
func TestAuthBranchListerErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := NewAuthBranchLister(srv.URL, "userd").ListActiveBranches(context.Background()); err == nil {
		t.Fatal("500 必须报错，不能当成空列表")
	}
}

// TestAuthBranchListerNilWhenUnconfigured 没配地址时返回 nil（而不是空实现）。
func TestAuthBranchListerNilWhenUnconfigured(t *testing.T) {
	if l := NewAuthBranchLister("   ", "userd"); l != nil {
		t.Error("未配置地址时应返回 nil，让调用方明确知道没接")
	}
}

// TestAuthAppIDIsUserd 把 app-id 用字面量钉死。
//
// ⚠️ 单靠上面那条路径断言**不够**：
//
//	路径断言写的是 "/v1.0/invoke/userd/..."（字面量），
//	但 NewAuthBranchLister(srv.URL, "userd") 是**显式传参**，
//	完全绕过了默认值这条路径。
//	而生产装配传的正是一个可能为空的值：
//	envStr("AUTH_USERD_APP_ID", collect.AuthAppID) ——
//	env 没配时**落回 AuthAppID**。所以 AuthAppID 错了没人拦。
//
//	症状：注册 0 个 job、服务照常启动、报表照出，
//	只有"明天起没有新数据"会被发现。
func TestAuthAppIDIsUserd(t *testing.T) {
	if AuthAppID != "userd" {
		t.Fatalf("AuthAppID = %q，期望 %q", AuthAppID, "userd")
	}
}

// TestNewAuthBranchListerDefaultsToUserd 空 app-id 必须落回 userd。
func TestNewAuthBranchListerDefaultsToUserd(t *testing.T) {
	l := NewAuthBranchLister("http://x", "  ")
	if l == nil {
		t.Fatal("配了地址就不该返回 nil")
	}
	if l.appID != "userd" {
		t.Errorf("默认 app-id = %q，期望 %q", l.appID, "userd")
	}
}
