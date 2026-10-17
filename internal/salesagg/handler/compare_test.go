package handler_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YunBright/authkit/claims"
	"github.com/gin-gonic/gin"

	// modernc 是纯 Go 的 SQLite（无需 cgo），必须空白导入注册驱动。
	_ "modernc.org/sqlite"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/salesagg/compare"
	"github.com/YunBright/supertrade/internal/salesagg/handler"
	"github.com/YunBright/supertrade/internal/salesagg/service"
)

// cmpCube 是 CompareHandler 用的替身：按**门店**存数据并记录调用序，
// 断言 forbidden 列零调用（与 handler_test.go 的 fakeCube 按 groupBy
// 存数据的观测面不同）。
type cmpCube struct {
	calls []string
	rows  map[string][]service.RawReport
	errs  map[string]error
}

func (f *cmpCube) LoadSalesReport(
	_ context.Context, storeBranchID, groupBy, _, _ string, _ int,
) (*service.RawSalesReport, error) {
	f.calls = append(f.calls, storeBranchID)
	_ = groupBy
	if err, ok := f.errs[storeBranchID]; ok {
		return nil, err
	}
	return &service.RawSalesReport{Rows: f.rows[storeBranchID]}, nil
}

// claimsStub 是验签中间件的替身：生产里 cmdbootstrap 的 authn verifier
// 在验签后用 claims.WithClaims 注入；测试里凡带 Authorization 头的请求
// 注入固定身份（S001/S002 有权限，ZZZZ 没有）。
func claimsStub() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetHeader("Authorization") != "" {
			cl := &claims.Claims{
				Sub:                "11111111-1111-1111-1111-111111111111",
				Aud:                []string{"sales-agg"},
				AccessibleBranches: []string{"S001", "S002"},
			}
			c.Request = c.Request.WithContext(claims.WithClaims(c.Request.Context(), cl))
		}
		c.Next()
	}
}

// newCompareRouter 起带 SQLite 模板库的对比路由（claims 由 stub 注入）。
func newCompareRouter(t *testing.T) (*gin.Engine, *cmpCube) {
	t.Helper()
	dialector := sqlite.Dialector{
		DriverName: "sqlite",
		DSN:        ":memory:?_pragma=foreign_keys(1)&_time_format=sqlite",
	}
	db, err := gorm.Open(dialector, &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&compare.Template{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)

	f := &cmpCube{rows: map[string][]service.RawReport{}, errs: map[string]error{}}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(claimsStub())
	handler.NewCompare(service.New(f), compare.NewStore(db)).RegisterRoutes(r)
	return r, f
}

// doJSON 发带 JSON body 的请求；token 头由 withAuth 决定。
func doJSON(t *testing.T, r *gin.Engine, method, path string, token bool, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(payload))
	if token {
		req.Header.Set("Authorization", "Bearer test-token")
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// comparePayload 是一份合法的模板保存体（2 店 1 行）。
// 第二家店故意用 ZZZZ（claims 里没有）——数据类测试需要一条 forbidden 列。
func comparePayload(name string) string {
	return fmt.Sprintf(`{
		"name": %q, "dimension": "category",
		"stores": ["S001", "ZZZZ"],
		"rows": [{"key": "r1", "label": "猪肉档", "items": {"S001": "01", "ZZZZ": "01"}}]
	}`, name)
}

// TestCompareRequiresAuth 无 token 一律 401 —— 与"模板全局共享"互补：
// 共享免的是**门店授权**，免不了**身份认证**。
func TestCompareRequiresAuth(t *testing.T) {
	r, _ := newCompareRouter(t)
	paths := []struct{ method, path string }{
		{http.MethodGet, "/compare-templates"},
		{http.MethodPost, "/compare-templates"},
		{http.MethodGet, "/compare-templates/xyz"},
		{http.MethodPut, "/compare-templates/xyz"},
		{http.MethodDelete, "/compare-templates/xyz"},
		{http.MethodGet, "/compare-templates/xyz/data"},
		{http.MethodPost, "/compare/preview"},
	}
	for _, p := range paths {
		w := doJSON(t, r, p.method, p.path, false, "{}")
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", p.method, p.path, w.Code)
		}
	}
}

// TestCompareTemplateCRUD 全生命周期：create(rev 1) → list 壳 → get →
// put 正确 revision(rev 2) → put 旧 revision 409 → delete → get 404。
func TestCompareTemplateCRUD(t *testing.T) {
	r, _ := newCompareRouter(t)

	// create
	w := doJSON(t, r, http.MethodPost, "/compare-templates", true, comparePayload("对比模板"))
	if w.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	created := body(t, w)
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatal("create 未返回 id")
	}
	if rev, _ := created["revision"].(float64); rev != 1 {
		t.Fatalf("revision = %v, want 1", created["revision"])
	}
	if by, _ := created["created_by"].(string); by != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("created_by = %v", created["created_by"])
	}

	// list：{templates:[...]} 壳
	w = doJSON(t, r, http.MethodGet, "/compare-templates", true, "")
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d", w.Code)
	}
	m := body(t, w)
	arr, ok := m["templates"].([]any)
	if !ok || len(arr) != 1 {
		t.Fatalf("list 缺 templates 数组: %s", w.Body.String())
	}

	// get 单个
	w = doJSON(t, r, http.MethodGet, "/compare-templates/"+id, true, "")
	if w.Code != http.StatusOK {
		t.Fatalf("get = %d", w.Code)
	}

	// put 正确 revision
	w = doJSON(t, r, http.MethodPut, "/compare-templates/"+id, true,
		fmt.Sprintf(`{"revision": 1, "name": "改名", "dimension": "category",
			"stores": ["S001", "S002"], "rows": [{"key": "k", "label": "行"}]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("put = %d: %s", w.Code, w.Body.String())
	}
	if rev, _ := body(t, w)["revision"].(float64); rev != 2 {
		t.Fatalf("put 后 revision = %v, want 2", rev)
	}

	// put 旧 revision → 409（校验通过，卡在乐观锁上）
	w = doJSON(t, r, http.MethodPut, "/compare-templates/"+id, true,
		fmt.Sprintf(`{"revision": 1, "name": "并发者", "dimension": "category",
			"stores": ["S001", "S002"], "rows": [{"key": "k", "label": "行"}]}`))
	if w.Code != http.StatusConflict {
		t.Fatalf("put 旧 revision = %d, want 409", w.Code)
	}
	if code, _ := body(t, w)["code"].(string); code != "compare_template_conflict" {
		t.Fatalf("conflict code = %v", body(t, w)["code"])
	}

	// put 缺 revision → 400（不能静默按最新版更新）
	w = doJSON(t, r, http.MethodPut, "/compare-templates/"+id, true,
		fmt.Sprintf(`{"name": "缺版本", "dimension": "category",
			"stores": ["S001", "S002"], "rows": [{"key": "k", "label": "行"}]}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("put 缺 revision = %d, want 400", w.Code)
	}

	// delete → 204；再 get → 404
	w = doJSON(t, r, http.MethodDelete, "/compare-templates/"+id, true, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", w.Code)
	}
	w = doJSON(t, r, http.MethodGet, "/compare-templates/"+id, true, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("get 已删模板 = %d, want 404", w.Code)
	}
	if code, _ := body(t, w)["code"].(string); code != "compare_template_not_found" {
		t.Fatalf("not-found code = %v", body(t, w)["code"])
	}
}

// TestCompareTemplateValidation 校验矩阵 + 未知 id 的 404。
func TestCompareTemplateValidation(t *testing.T) {
	r, _ := newCompareRouter(t)
	bad := []struct {
		name string
		body string
	}{
		{"1 家门店", `{"name":"x","dimension":"category","stores":["S001"],"rows":[{"key":"k","label":"行"}]}`},
		{"4 家门店", `{"name":"x","dimension":"category","stores":["A","B","C","D"],"rows":[{"key":"k","label":"行"}]}`},
		{"0 行", `{"name":"x","dimension":"category","stores":["S001","S002"],"rows":[]}`},
		{"未知维度", `{"name":"x","dimension":"brand","stores":["S001","S002"],"rows":[{"key":"k","label":"行"}]}`},
		{"空模板名", `{"name":"  ","dimension":"category","stores":["S001","S002"],"rows":[{"key":"k","label":"行"}]}`},
		{"31 字模板名", fmt.Sprintf(`{"name":%q,"dimension":"category","stores":["S001","S002"],"rows":[{"key":"k","label":"行"}]}`, strings.Repeat("名", 31))},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			w := doJSON(t, r, http.MethodPost, "/compare-templates", true, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("create = %d, want 400", w.Code)
			}
			if code, _ := body(t, w)["code"].(string); code != "compare_template_invalid" {
				t.Fatalf("code = %v, want compare_template_invalid", body(t, w)["code"])
			}
		})
	}

	// 未知 id：get / put / delete 均 404。
	//（PUT 的 body 必须合法且带 revision —— 先过校验，再卡在"行不存在"。）
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/compare-templates/no-such"},
		{http.MethodPut, "/compare-templates/no-such"},
		{http.MethodDelete, "/compare-templates/no-such"},
	} {
		w := doJSON(t, r, tc.method, tc.path, true,
			`{"revision":1,"name":"x","dimension":"category","stores":["S001","S002"],"rows":[{"key":"k","label":"行"}]}`)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s 未知 id = %d, want 404", tc.method, w.Code)
		}
	}
}

// TestCompareIgnoresXBranchID 模板请求带 X-Branch-ID 仍成功（头被忽略）；
// 数据请求同样只认 token —— 头指向无权限门店也不影响授权店取数。
func TestCompareIgnoresXBranchID(t *testing.T) {
	r, _ := newCompareRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/compare-templates",
		bytes.NewBufferString(comparePayload("带头模板")))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Branch-ID", "ZZZZ")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("带头 create = %d: %s", w.Code, w.Body.String())
	}
	id, _ := body(t, w)["id"].(string)

	// 数据请求带头：授权仍只认 token（ZZZZ 列 forbidden，而非请求被拒）。
	req = httptest.NewRequest(http.MethodGet, "/compare-templates/"+id+"/data?preset=today", nil)
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("X-Branch-ID", "ZZZZ")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("带头 data = %d: %s", w.Code, w.Body.String())
	}
	cols, _ := body(t, w)["columns"].([]any)
	last := cols[len(cols)-1].(map[string]any)
	if st, _ := last["status"].(string); st != "forbidden" {
		t.Fatalf("ZZZZ 列 = %v, want forbidden（头不参与授权）", last["status"])
	}
}

// TestCompareDataShape 数据端点的契约形状：
// columns 顺序=模板顺序、status ok/forbidden、summary 仅授权店、
// cells 无 ZZZZ key、数值字段是字符串、from/to 回显。
func TestCompareDataShape(t *testing.T) {
	r, f := newCompareRouter(t)
	f.rows["S001"] = []service.RawReport{
		{Code: "01", Name: "生鲜", NetAmount: 100, NetQty: 10, RefCost: 40, CostedRows: 5, TotalRows: 5},
	}

	w := doJSON(t, r, http.MethodPost, "/compare-templates", true, comparePayload("形状"))
	if w.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	id, _ := body(t, w)["id"].(string)

	w = doJSON(t, r, http.MethodGet, "/compare-templates/"+id+"/data?preset=today", true, "")
	if w.Code != http.StatusOK {
		t.Fatalf("data = %d: %s", w.Code, w.Body.String())
	}
	m := body(t, w)
	if dim, _ := m["dimension"].(string); dim != "category" {
		t.Fatalf("dimension = %v", m["dimension"])
	}
	if from, _ := m["from"].(string); from == "" || from != m["to"] {
		t.Fatalf("from/to 未回显 today 区间: %v / %v", m["from"], m["to"])
	}

	cols, _ := m["columns"].([]any)
	if len(cols) != 2 {
		t.Fatalf("columns = %v, want 2 列", m["columns"])
	}
	c0 := cols[0].(map[string]any)
	c1 := cols[1].(map[string]any)
	if s, _ := c0["store"].(string); s != "S001" {
		t.Fatalf("columns[0].store = %v, want S001（顺序=请求顺序）", c0["store"])
	}
	if st, _ := c0["status"].(string); st != "ok" {
		t.Fatalf("columns[0].status = %v, want ok", c0["status"])
	}
	if st, _ := c1["status"].(string); st != "forbidden" {
		t.Fatalf("columns[1].status = %v, want forbidden", c1["status"])
	}
	if f.calls[0] != "S001" || len(f.calls) != 1 {
		t.Fatalf("cube 调用 = %v, want 仅 [S001]", f.calls)
	}

	summary, _ := m["summary"].(map[string]any)
	if _, ok := summary["ZZZZ"]; ok {
		t.Fatal("summary 不得含 ZZZZ")
	}
	s1, ok := summary["S001"].(map[string]any)
	if !ok {
		t.Fatalf("summary 缺 S001: %v", m["summary"])
	}
	// 数值字段必须是字符串（契约：服务端格式化）。
	for _, k := range []string{"net_amount", "net_qty", "ref_cost", "cost_coverage"} {
		if _, ok := s1[k].(string); !ok {
			t.Fatalf("summary.S001.%s 不是字符串: %v", k, s1[k])
		}
	}

	rows, _ := m["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want 1 行", m["rows"])
	}
	row := rows[0].(map[string]any)
	cells, _ := row["cells"].(map[string]any)
	if _, ok := cells["ZZZZ"]; ok {
		t.Fatal("cells 不得含 ZZZZ")
	}
	cell, ok := cells["S001"].(map[string]any)
	if !ok {
		t.Fatalf("cells.S001 缺失: %v", row["cells"])
	}
	for _, k := range []string{"code", "name", "net_amount", "net_qty", "ref_margin", "ref_margin_pct"} {
		if _, ok := cell[k].(string); !ok {
			t.Fatalf("cell.%s 不是字符串: %v", k, cell[k])
		}
	}
	if code, _ := cell["code"].(string); code != "01" {
		t.Fatalf("cell.code = %v, want 01", cell["code"])
	}
}

// TestCompareDataNegative data 的负向：未知模板 404；坏日期 / 缺一侧 400。
func TestCompareDataNegative(t *testing.T) {
	r, _ := newCompareRouter(t)

	if w := doJSON(t, r, http.MethodGet, "/compare-templates/no-such/data?preset=today", true, ""); w.Code != http.StatusNotFound {
		t.Fatalf("未知模板 data = %d, want 404", w.Code)
	}
	for _, q := range []string{
		"?from=2026-13-40&to=2026-10-17", // 非法日期
		"?from=2026-10-17",               // 缺 to
		"?to=2026-10-17",                 // 缺 from
		"?from=2026-10-20&to=2026-10-17", // from > to
	} {
		// 先建一个真实模板再带坏日期查。
		w := doJSON(t, r, http.MethodPost, "/compare-templates", true, comparePayload("坏日期"))
		if w.Code != http.StatusOK {
			t.Fatalf("前置 create = %d", w.Code)
		}
		id, _ := body(t, w)["id"].(string)
		w = doJSON(t, r, http.MethodGet, "/compare-templates/"+id+"/data"+q, true, "")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("data %s = %d, want 400", q, w.Code)
		}
		if code, _ := body(t, w)["code"].(string); code != "compare_template_invalid" {
			t.Fatalf("data %s code = %v, want compare_template_invalid", q, body(t, w)["code"])
		}
	}
}

// TestPreview 草稿取数：inline 配置 + from/to（无 preset）→ 200 同形状；
// preset → 200；rows:[] → 200 rows:[]（空草稿也放行）。
func TestPreview(t *testing.T) {
	r, f := newCompareRouter(t)
	f.rows["S002"] = []service.RawReport{
		{Code: "01", Name: "生鲜", NetAmount: 50, NetQty: 5, RefCost: 20, CostedRows: 5, TotalRows: 5},
	}

	// from/to 自定义区间。
	w := doJSON(t, r, http.MethodPost, "/compare/preview", true, `{
		"dimension": "category", "stores": ["S001", "S002"],
		"rows": [{"key": "r1", "label": "猪肉", "items": {"S002": "01"}}],
		"from": "2026-10-01", "to": "2026-10-17"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("preview(from/to) = %d: %s", w.Code, w.Body.String())
	}
	m := body(t, w)
	if from, _ := m["from"].(string); from != "2026-10-01" || m["to"] != "2026-10-17" {
		t.Fatalf("from/to 未回显: %v / %v", m["from"], m["to"])
	}
	// S001 无数据 → ok 列但 summary 为零值；S002 命中。
	summary, _ := m["summary"].(map[string]any)
	if _, ok := summary["S002"]; !ok {
		t.Fatalf("summary 缺 S002: %v", m["summary"])
	}

	// preset。
	w = doJSON(t, r, http.MethodPost, "/compare/preview", true, `{
		"dimension": "category", "stores": ["S001", "S002"],
		"rows": [], "preset": "yesterday"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("preview(preset) = %d: %s", w.Code, w.Body.String())
	}
	m = body(t, w)
	rows, _ := m["rows"].([]any)
	if len(rows) != 0 {
		t.Fatalf("preview rows:[] 应返回 [], got %v", m["rows"])
	}

	// 3 家门店（上限内）+ 未知 preset 退回 today。
	w = doJSON(t, r, http.MethodPost, "/compare/preview", true, `{
		"dimension": "product", "stores": ["S001", "S002", "ZZZZ"],
		"rows": [], "preset": "whatever"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("preview(未知 preset) = %d: %s", w.Code, w.Body.String())
	}
	cols, _ := body(t, w)["columns"].([]any)
	if len(cols) != 3 {
		t.Fatalf("columns = %d, want 3", len(cols))
	}
	last := cols[2].(map[string]any)
	if st, _ := last["status"].(string); st != "forbidden" {
		t.Fatalf("ZZZZ 列 = %v, want forbidden", last["status"])
	}
}

// TestPreviewUnavailable 门店有权限但数据源故障：preview 也走列状态，
// HTTP 恒 200 —— 与 data 端点一致（错误码语义不因入口不同而不同）。
func TestPreviewUnavailable(t *testing.T) {
	r, f := newCompareRouter(t)
	f.errs["S001"] = errors.New("boom")

	w := doJSON(t, r, http.MethodPost, "/compare/preview", true, `{
		"dimension": "category", "stores": ["S001", "S002"], "rows": []
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("preview = %d, want 200（列内联失败）", w.Code)
	}
	cols, _ := body(t, w)["columns"].([]any)
	first := cols[0].(map[string]any)
	if st, _ := first["status"].(string); st != "unavailable" {
		t.Fatalf("S001 列 = %v, want unavailable", first["status"])
	}
	if ec, _ := first["error_code"].(string); ec != "sales_source_unavailable" {
		t.Fatalf("error_code = %v", first["error_code"])
	}
}
