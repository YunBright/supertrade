// Package cubesync —— 与 sidecar / 下游通话的单元测试。
//
// # 测什么(对着症状写)
//
//	① 导出调用的 **URL 与 query** 对不对(错了 ⇒ 拿不到数据 / 拿到错门店)
//	② 两个必需的头带没带(Authorization / X-Branch-ID)
//	③ 下游返回错误时,错误里带上了它的 code(不带 ⇒ 排查只有一句 HTTP 500)
//	④ job 注册是 **DELETE → POST**(只 POST ⇒ 第二次 409,每次启动都失败)
package cubesync

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/YunBright/supertrade/internal/archivedto"
)

// TestSidecarExporter_CallsExportEndpoint 锁导出调用的形状。
func TestSidecarExporter_CallsExportEndpoint(t *testing.T) {
	var gotPath, gotQuery, gotAuth, gotBranch string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		gotBranch = r.Header.Get("X-Branch-ID")
		_ = json.NewEncoder(w).Encode(archivedto.ExportPage{
			Scope: "products", BranchID: "00", CubeSource: "cube-sixun-ysx-fb",
			Rows: []map[string]any{{"id": "P1"}}, Count: 1, Total: 1,
		})
	}))
	defer srv.Close()

	e := NewSidecarExporter(srv.URL, "supertrade-cube-router", "00")
	page, err := e.Export(context.Background(), "TOKEN", "products", 100, 500, "")
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	if !strings.HasSuffix(gotPath, "/v1.0/invoke/supertrade-cube-router/method/v1/export") {
		t.Errorf("调用路径不对: %s", gotPath)
	}
	for _, want := range []string{"scope=products", "offset=100", "limit=500"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query 应含 %q,实际 %q", want, gotQuery)
		}
	}
	if gotAuth != "Bearer TOKEN" {
		t.Errorf("Authorization 头不对: %q", gotAuth)
	}
	if gotBranch != "00" {
		t.Errorf("X-Branch-ID 头不对: %q", gotBranch)
	}
	if page.Total != 1 || len(page.Rows) != 1 {
		t.Errorf("解析结果不对: %+v", page)
	}
}

// TestSidecarImporter_PostsToImportEndpoint 锁导入调用的形状。
func TestSidecarImporter_PostsToImportEndpoint(t *testing.T) {
	var gotPath, gotAuth, gotBranch, gotMethod string
	var body archivedto.ImportRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotBranch = r.Header.Get("X-Branch-ID")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_ = json.NewEncoder(w).Encode(archivedto.ImportResult{Mode: body.Mode})
	}))
	defer srv.Close()

	im := NewSidecarImporter(srv.URL, "supertrade-catalog", "10")
	res, err := im.Import(context.Background(), "TOKEN", archivedto.ImportRequest{
		SourceBranchID: "00", Mode: "overwrite", Scopes: []string{"products"},
		Data: archivedto.ImportData{Products: []archivedto.ImportProductRow{{ID: "P1", Name: "商品"}}},
	})
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("方法应为 POST,实际 %q", gotMethod)
	}
	if !strings.HasSuffix(gotPath, "/v1.0/invoke/supertrade-catalog/method/archive/import") {
		t.Errorf("调用路径不对: %s", gotPath)
	}
	if gotAuth != "Bearer TOKEN" {
		t.Errorf("Authorization 头不对: %q", gotAuth)
	}
	// ⚠️ 目标门店与源门店**不同**:导出来自 00,写进 10。
	// 混用同一个 header 的症状是"导出了 00 的数据、写进了 00 的门店",
	// 而报告里两个门店号都写着 00,不看配置发现不了。
	if gotBranch != "10" {
		t.Errorf("X-Branch-ID 应为目标门店 10,实际 %q", gotBranch)
	}
	if body.SourceBranchID != "00" || body.Mode != "overwrite" {
		t.Errorf("请求体不对: %+v", body)
	}
	if len(body.Data.Products) != 1 || body.Data.Products[0].ID != "P1" {
		t.Errorf("商品行没对上: %+v", body.Data.Products)
	}
	if res.Mode != "overwrite" {
		t.Errorf("解析结果不对: %+v", res)
	}
}

// TestDownstreamError_CarriesCode 锁"错误里带被调方的 code"。
//
// 不带的话,排查只得到一句 HTTP 500 —— 无从判断是
// "门店没配 cube 源"还是"权限不够"还是"真的崩了"。
func TestDownstreamError_CarriesCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"code": "cube_source_not_configured", "message": "branch 未配置 cube 源",
		})
	}))
	defer srv.Close()

	e := NewSidecarExporter(srv.URL, "supertrade-cube-router", "00")
	_, err := e.Export(context.Background(), "TOKEN", "products", 0, 100, "")
	if err == nil {
		t.Fatal("404 应报错")
	}
	de, ok := err.(*DownstreamError)
	if !ok {
		t.Fatalf("错误类型不对: %T", err)
	}
	if de.Code != "cube_source_not_configured" {
		t.Errorf("错误码不对: %q", de.Code)
	}
	if !strings.Contains(err.Error(), "cube_source_not_configured") {
		t.Errorf("错误文案应含错误码,实际: %v", err)
	}
}

// TestDaprJobClient_DeletesBeforePost 锁"先 DELETE 再 POST"。
//
// ⚠️ 实测(wechat-bot 生产):同名 POST 返 **409**,不是 upsert。
// 只 POST ⇒ 第二次调用(含进程重启)必然失败。
func TestDaprJobClient_DeletesBeforePost(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewDaprJobClient(srv.URL)
	err := c.Register(context.Background(), "cube-sync-00-10", "0 30 3 * * *",
		"http://127.0.0.1:8116/v1/sync/job", map[string]any{"token": "t"})
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 2 {
		t.Fatalf("应发两次请求(DELETE + POST),实际 %d 次: %v", len(methods), methods)
	}
	if !strings.HasPrefix(methods[0], "DELETE") {
		t.Errorf("第一次应是 DELETE,实际 %q", methods[0])
	}
	if !strings.HasPrefix(methods[1], "POST") {
		t.Errorf("第二次应是 POST,实际 %q", methods[1])
	}
	if !strings.Contains(methods[1], "/v1.0-alpha1/jobs/cube-sync-00-10") {
		t.Errorf("job 路径不对: %q", methods[1])
	}
}

// TestDaprJobClient_ReportsPostFailure 锁"注册失败要报错"。
func TestDaprJobClient_ReportsPostFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()

	c := NewDaprJobClient(srv.URL)
	if err := c.Register(context.Background(), "j", "0 30 3 * * *", "http://x/y", nil); err == nil {
		t.Fatal("409 必须报错(否则每次启动都会静默失败)")
	}
}

// TestDaprJobClient_Delete404IsNoop 锁"删不存在的 job 不算错"。
func TestDaprJobClient_Delete404IsNoop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewDaprJobClient(srv.URL)
	if err := c.Delete(context.Background(), "nope"); err != nil {
		t.Errorf("404 应视为 no-op,实际: %v", err)
	}
}

// TestDaprTokenIssuer_ExchangesToken 锁机器 token 换取。
func TestDaprTokenIssuer_ExchangesToken(t *testing.T) {
	var gotPath, gotAuth string
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "AT", "token_type": "Bearer", "expires_in": 3600,
		})
	}))
	defer srv.Close()

	iss := NewDaprTokenIssuer(srv.URL, "userd", "cid", "csecret")
	tok, err := iss.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("换 token 失败: %v", err)
	}
	if tok != "AT" {
		t.Errorf("token 应为 AT,实际 %q", tok)
	}
	if !strings.HasSuffix(gotPath, "/v1.0/invoke/userd/method/auth/token") {
		t.Errorf("路径不对: %s", gotPath)
	}
	if gotAuth != "" {
		t.Errorf("换 token 不该带 Authorization,实际 %q", gotAuth)
	}
	if body["client_id"] != "cid" || body["client_secret"] != "csecret" {
		t.Errorf("请求体不对: %+v", body)
	}
}

// TestDaprTokenIssuer_RejectsEmptyToken 锁"空 token 不算成功"。
//
// 空 token 会让后续调用不带 Authorization ⇒ 症状是"下游 401",
// 排查跑到下游去,真因(没换到 token)却在 auth 侧。
func TestDaprTokenIssuer_RejectsEmptyToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": ""})
	}))
	defer srv.Close()

	iss := NewDaprTokenIssuer(srv.URL, "userd", "cid", "csecret")
	if _, err := iss.AccessToken(context.Background()); err == nil {
		t.Fatal("空 access_token 必须报错")
	}
}

// TestDaprTokenIssuer_ErrorDoesNotLeakSecret 锁"错误文案不回显密钥"。
//
// 错误会经同步端点的 503 回到日志被长期翻阅。
func TestDaprTokenIssuer_ErrorDoesNotLeakSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	iss := NewDaprTokenIssuer(srv.URL, "userd", "cid", "super-secret-value")
	_, err := iss.AccessToken(context.Background())
	if err == nil {
		t.Fatal("401 必须报错")
	}
	if strings.Contains(err.Error(), "super-secret-value") {
		t.Errorf("错误文案泄露了 client_secret: %v", err)
	}
}
