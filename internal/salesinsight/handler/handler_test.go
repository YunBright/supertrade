package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/salesinsight/calendar"
	"github.com/YunBright/supertrade/internal/salesinsight/insight"
)

// fakeSvc 是一个**记录入参**的假 Service。
//
// ⚠️ 它只替换"生成"这一个动作，handler 自己的解析/映射全部是真的
//
//	（真的 gin、真的绑定、真的 JSON 编码）。
//	如果连 handler 一起 fake 掉，这些测试就只是在测自己写的 fake。
type fakeSvc struct {
	lastIn GenerateInput

	insight insight.Insight
	audit   insight.Audit
	err     error
}

func (f *fakeSvc) Generate(_ context.Context, in GenerateInput) (insight.Insight, insight.Audit, error) {
	f.lastIn = in
	return f.insight, f.audit, f.err
}

// defaultAudit 返回一条字段齐全的审计。
//
// ⚠️ 它的重字段**故意填成可辨识的内容**（"CTX-"/"PROMPT-"/"RAW-"）而不是空串：
//
//	空串会让"full 模式带了字段"这条断言恒真 ——
//	空字符串本来就会被 omitempty 丢掉，测了等于没测。
func defaultAudit() insight.Audit {
	return insight.Audit{
		RunID:      "run-001",
		PeriodKind: "daily",
		From:       "2026-10-15",
		To:         "2026-10-15",
		BranchID:   "store-9",
		Degraded:   false,
		LatencyMS:  1234,
		Context:    "CTX-事实包全文",
		Prompt:     "PROMPT-最终提示词",
		RawOutput:  "RAW-模型原始返回",
		CreatedAt:  time.Date(2026, 10, 16, 8, 50, 0, 0, time.Local),
		Insight:    insight.Insight{GeneratedBy: "llm", Summary: "今天整体持平"},
	}
}

func doPost(t *testing.T, svc Service, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	New(svc).RegisterRoutes(r)

	req := httptest.NewRequest(http.MethodPost, "/insights/generate",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var out map[string]any
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("响应不是合法 JSON: %v\nbody=%s", err, w.Body.String())
		}
	}
	return w, out
}

// ---------------------------------------------------------------------------
// 入口校验
// ---------------------------------------------------------------------------

func TestGenerate_入口校验(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
		code string
	}{
		{"缺门店", `{"kind":"daily"}`, http.StatusBadRequest, "store_required"},
		{"门店是空串", `{"store":"   "}`, http.StatusBadRequest, "store_required"},
		{"未知周期", `{"store":"s9","kind":"quarterly"}`, http.StatusBadRequest, "bad_kind"},
		// ⚠️ 未知审计粒度必须 400。静默当默认值的后果是
		// "报告发出去了但后台查不到推理上下文"，排查会跑偏。
		{"审计粒度大小写错", `{"store":"s9","include_audit":"FULL"}`,
			http.StatusBadRequest, "bad_include_audit"},
		{"审计粒度写错词", `{"store":"s9","include_audit":"verbose"}`,
			http.StatusBadRequest, "bad_include_audit"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeSvc{audit: defaultAudit()}
			w, _ := doPost(t, svc, tc.body)
			if w.Code != tc.want {
				t.Fatalf("status = %d, 期望 %d；body=%s", w.Code, tc.want, w.Body.String())
			}
			var env struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &env)
			if env.Code != tc.code {
				t.Fatalf("error code = %q, 期望 %q；body=%s", env.Code, tc.code, w.Body.String())
			}
		})
	}
}

func TestGenerate_serviceNil返回503(t *testing.T) {
	// svc 为 nil 时不能 panic —— main.go 的 registerRoutes 会在
	// OnStart 失败时 panic，但单测直接构造 Handler，走的是另一条路。
	w, _ := doPost(t, nil, `{"store":"s9"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, 期望 503", w.Code)
	}
}

func TestGenerate_默认周期与入参透传(t *testing.T) {
	svc := &fakeSvc{audit: defaultAudit()}
	doPost(t, svc, `{"store":"  s9  "}`)
	if svc.lastIn.Kind != calendar.PeriodDaily {
		t.Fatalf("默认周期 = %q, 期望 daily", svc.lastIn.Kind)
	}
	// ⚠️ 门店必须 TrimSpace：调用方用环境变量拼 X-Branch-ID，
	// 带一个空格就会让本服务查不到数据，而现象是"报告全是 0"。
	if svc.lastIn.StoreBranchID != "s9" {
		t.Fatalf("门店 = %q, 期望 s9", svc.lastIn.StoreBranchID)
	}
}

// ---------------------------------------------------------------------------
// 审计粒度 —— 这一对用例必须**互相可判别**
// ---------------------------------------------------------------------------

func TestGenerate_审计粒度_summary不含重字段(t *testing.T) {
	svc := &fakeSvc{audit: defaultAudit()}
	_, out := doPost(t, svc, `{"store":"s9"}`)

	audit := out["audit"].(map[string]any)
	for _, k := range []string{"context", "prompt", "raw_output"} {
		if _, ok := audit[k]; ok {
			t.Fatalf("默认（summary）模式下不应出现 %q，实际出现且值=%v", k, audit[k])
		}
	}
	// 元信息必须在，否则调用方拼不出存储 key。
	if audit["run_id"] != "run-001" {
		t.Fatalf("run_id = %v, 期望 run-001", audit["run_id"])
	}
	if audit["token_estimated"] != true {
		t.Fatalf("token_estimated = %v, 期望 true（dapr 不返回 Usage）", audit["token_estimated"])
	}
}

func TestGenerate_审计粒度_full带回全文(t *testing.T) {
	svc := &fakeSvc{audit: defaultAudit()}
	_, out := doPost(t, svc, `{"store":"s9","include_audit":"full"}`)

	audit := out["audit"].(map[string]any)
	want := map[string]string{
		"context":    "CTX-事实包全文",
		"prompt":     "PROMPT-最终提示词",
		"raw_output": "RAW-模型原始返回",
	}
	for k, v := range want {
		if audit[k] != v {
			t.Fatalf("full 模式 %s = %v, 期望 %q", k, audit[k], v)
		}
	}
	if _, ok := audit["audit_truncated"]; ok {
		t.Fatal("未超限时不应出现 audit_truncated")
	}
}

// 反证：把 full 判别力去掉（两种模式都返回全文）时，上面那条 summary 用例必须变红。
// 这里只做"确认字段非空"的判别力自检 —— 若 summary 用例因某天把
// omitempty 去掉而恒真，本用例会提醒我们补判别力。
func TestAuditViewOf_full与summary字段集合相同(t *testing.T) {
	a := defaultAudit()
	sum := auditViewOf(a, false)
	full := auditViewOf(a, true)

	if sum.RunID != full.RunID {
		t.Fatalf("两种粒度的 run_id 应一致: %q vs %q", sum.RunID, full.RunID)
	}
	if full.Context == "" || full.Prompt == "" || full.RawOutput == "" {
		t.Fatal("full 模式下重字段不应为空")
	}
	if sum.Context != "" {
		t.Fatalf("summary 模式下 Context 应为空，实际 %q", sum.Context)
	}
}

// 审计必须携带调用方拼存储 key 所需的四个字段。
//
// ⚠️ 这条单测直接对着 wechat-bot 的 aireport.ReportKey 契约写：
//
//	少任何一个，症状都是"报告存进去了但历史列表里查不到"。
func TestGenerate_审计携带期次与门店(t *testing.T) {
	svc := &fakeSvc{audit: defaultAudit()}
	_, out := doPost(t, svc, `{"store":"s9"}`)

	audit := out["audit"].(map[string]any)
	for k, want := range map[string]string{
		"period_kind": "daily",
		"from":        "2026-10-15",
		"to":          "2026-10-15",
		"branch_id":   "store-9",
	} {
		if audit[k] != want {
			t.Fatalf("audit.%s = %v, 期望 %q", k, audit[k], want)
		}
	}
}

// ---------------------------------------------------------------------------
// 审计截断
// ---------------------------------------------------------------------------

func TestGenerate_full模式超限被截断(t *testing.T) {
	// 用 rune 构造，保证按字符而不是按字节截断。
	huge := strings.Repeat("测", MaxAuditFieldChars+500)
	audit := defaultAudit()
	audit.Context = huge

	svc := &fakeSvc{audit: audit}
	_, out := doPost(t, svc, `{"store":"s9","include_audit":"full"}`)

	got := out["audit"].(map[string]any)
	if got["audit_truncated"] != true {
		t.Fatal("超限时 audit_truncated 必须为 true，否则后台会以为审计是完整的")
	}
	ctx, _ := got["context"].(string)
	if len([]rune(ctx)) > MaxAuditFieldChars+16 {
		t.Fatalf("截断后仍过长: %d rune", len([]rune(ctx)))
	}
	rr := []rune(ctx)
	if !strings.HasSuffix(ctx, "…[已截断]") {
		t.Fatalf("截断后应有显式标记，实际尾部 %q", rr[len(rr)-8:])
	}
}

func TestGenerate_full模式未超限不标记截断(t *testing.T) {
	audit := defaultAudit()
	audit.Prompt = strings.Repeat("x", MaxAuditFieldChars) // 正好等于上限，不算超限
	v := auditViewOf(audit, true)
	if v.AuditTruncated {
		t.Fatal("正好等于上限不应标记截断（截断条件是 > 上限）")
	}
	if len(v.Prompt) != MaxAuditFieldChars {
		t.Fatalf("未超限时不应改动内容，长度 %d", len(v.Prompt))
	}
}

// ---------------------------------------------------------------------------
// 上期摘要（调用方传入）
// ---------------------------------------------------------------------------

func TestGenerate_上期摘要透传(t *testing.T) {
	svc := &fakeSvc{audit: defaultAudit()}
	body := `{"store":"s9","kind":"weekly","prev_summary":{
		"from":"2026-10-06","to":"2026-10-12",
		"summary":"上周生鲜走弱","highlights":["叶菜跌 12%"]}}`
	if w, _ := doPost(t, svc, body); w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	ps := svc.lastIn.PrevSummary
	if ps == nil {
		t.Fatal("上期摘要未被传入 GenerateInput")
	}
	// ⚠️ period_kind 必须由**本服务**按请求覆盖，不能由调用方指定 ——
	//   否则会出现"上一期是月报、本期是日报"的错配。
	if ps.PeriodKind != "weekly" {
		t.Fatalf("PrevSummary.PeriodKind = %q, 期望 weekly（应取本次请求的周期）", ps.PeriodKind)
	}
	if ps.From != "2026-10-06" || ps.To != "2026-10-12" {
		t.Fatalf("期次区间 = %q~%q, 期望 2026-10-06~2026-10-12", ps.From, ps.To)
	}
	if ps.Summary != "上周生鲜走弱" || len(ps.Highlights) != 1 {
		t.Fatalf("摘要内容未透传: %+v", ps)
	}
}

func TestGenerate_空上期摘要视为没有(t *testing.T) {
	// ⚠️ 空摘要是**正常分支**（首个周期），不是错误。
	//   但它必须被丢弃成 nil —— 留着会让 prompt 里出现一段空的
	//   "上期结论"，模型会对着空白编一句"上期增长平稳"。
	cases := map[string]string{
		"不传":      `{"store":"s9"}`,
		"空对象":     `{"store":"s9","prev_summary":{}}`,
		"全空白摘要":   `{"store":"s9","prev_summary":{"summary":"   "}}`,
		"摘要空但有要点": `{"store":"s9","prev_summary":{"summary":"","highlights":[]}}`,
		"只有高亮非空":  `{"store":"s9","prev_summary":{"summary":"","highlights":["a"]}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			svc := &fakeSvc{audit: defaultAudit()}
			doPost(t, svc, body)
			ps := svc.lastIn.PrevSummary
			if name == "只有高亮非空" {
				if ps == nil {
					t.Fatal("有非空 highlights 时应保留上期摘要")
				}
				return
			}
			if ps != nil {
				t.Fatalf("%s 时 PrevSummary 应为 nil，实际 %+v", name, ps)
			}
		})
	}
}
