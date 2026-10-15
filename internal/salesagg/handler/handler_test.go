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

	"github.com/YunBright/supertrade/internal/salesagg/handler"
	"github.com/YunBright/supertrade/internal/salesagg/service"
)

// fakeCube 是 service.CubeQuerier 的替身。
//
// 它**按 groupBy 返回不同的数据**：类别维度给「生鲜联营」，商品维度给「五花肉」。
// 若两个维度返回同一份数据，下面「branch 维度被拒」那条断言就会退化成
// "只断言了状态码"，无法观测维度是否真的分流 —— 这与本仓对
// 「按身份造不同数据」的既有要求同源。
type fakeCube struct {
	calls    int
	gotStore string
	gotGroup string
	gotFrom  string
	gotTo    string
	rows     map[string][]service.RawReport
	err      error
}

func (f *fakeCube) LoadSalesReport(
	_ context.Context, storeBranchID, groupBy, from, to string, _ int,
) (*service.RawSalesReport, error) {
	f.calls++
	f.gotStore, f.gotGroup = storeBranchID, groupBy
	f.gotFrom, f.gotTo = from, to
	if f.err != nil {
		return nil, f.err
	}
	return &service.RawSalesReport{Rows: f.rows[groupBy]}, nil
}

func newRouter(f *fakeCube) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	handler.New(service.New(f)).RegisterRoutes(r)
	return r
}

func do(t *testing.T, r *gin.Engine, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

func body(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("响应不是 JSON (%v): %s", err, w.Body.String())
	}
	return m
}

// TestReportRequiresStore store 必填。
//
// ⚠️ 这一条守的是**真实存在的坑**：handler 早先的注释写的是
// 「空 = 该 cube 实例内全部门店」，但空值会让本服务不带 X-Branch-ID header
// 发给 cube-router，而 `internal/cube-router/handler/proxy.go` 里
// `branchID == ""` 直接 400 `branch_required` —— 那条路径从来跑不通，
// 只是一度被文档化成了特性。
//
// 空白字符也必须被拒（handler 走的是 strings.TrimSpace）。
func TestReportRequiresStore(t *testing.T) {
	for _, path := range []string{
		"/reports/sales",
		"/reports/sales?store=",
		"/reports/sales?store=%20%20",
	} {
		f := &fakeCube{}
		w := do(t, newRouter(f), http.MethodGet, path)
		if w.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", path, w.Code)
			continue
		}
		if code, _ := body(t, w)["code"].(string); code != "store_required" {
			t.Errorf("GET %s code = %q, want store_required", path, code)
		}
		// 关键：没给门店时**不能**去查 cube。
		// 若它照查不误，用户会拿到一份路由不明的数据并以为报表坏了。
		if f.calls != 0 {
			t.Errorf("GET %s 仍然调了 cube %d 次，want 0", path, f.calls)
		}
	}
}

// TestReportRejectsBranchDimension ⭐ 数据源的门店维度在**端点**上必须被拒。
//
// 业务要求「忽略数据源的门店（branch_no = 门店区域，X-Branch-ID 的下一层）」。
// service 层的 ValidDimension 已锁住，但 ValidDimension 与 handler 的 400
// 映射是两条独立路径 —— 只测 service 时，"handler 把它当 503 透传出去"
// 这种缺陷不可观测，而调用方（Flutter 报表页）拿到的是 5xx，
// 表现为「报表页面一直转圈」，排查成本远高于一个 400。
func TestReportRejectsBranchDimension(t *testing.T) {
	for _, dim := range []string{"branch", "BRANCH"} {
		f := &fakeCube{}
		w := do(t, newRouter(f), http.MethodGet, "/reports/sales?store=00&dimension="+dim)
		if w.Code != http.StatusBadRequest {
			t.Errorf("dimension=%s → %d, want 400", dim, w.Code)
			continue
		}
		if code, _ := body(t, w)["code"].(string); code != "invalid_dimension" {
			t.Errorf("dimension=%s code = %q, want invalid_dimension", dim, code)
		}
		// 更关键：不能让请求走到 cube。dimSpecs 里没有 branch 条目，
		// 但如果哪天有人"顺手加回来"，这里就是唯一的外部证据。
		if f.calls != 0 {
			t.Errorf("dimension=%s 仍然把请求发给了 cube，want 0 次", dim)
		}
	}
}

// TestTodayByCategoryRequiresStore 群卡片走的那个端点同样必须要求门店。
//
// 少这一条的话，"Report 校验了、today 没校验" 会让每日推送
// 悄悄走到 503 —— 而失败信息在企业微信群里，用户只会看到今天没卡片。
func TestTodayByCategoryRequiresStore(t *testing.T) {
	f := &fakeCube{}
	w := do(t, newRouter(f), http.MethodGet, "/reports/sales/today")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("GET /reports/sales/today = %d, want 400", w.Code)
	}
	if code, _ := body(t, w)["code"].(string); code != "store_required" {
		t.Errorf("code = %q, want store_required", code)
	}
	if f.calls != 0 {
		t.Errorf("仍然调了 cube %d 次，want 0", f.calls)
	}
}

// TestReportPassesStoreThrough store 必须原样透传给 cube 作为 X-Branch-ID。
//
// 若被改写或丢弃，报表会展示**别的门店**的数字 —— 而每一步都"成功"，
// 没有任何错误可查。
func TestReportPassesStoreThrough(t *testing.T) {
	f := &fakeCube{rows: map[string][]service.RawReport{
		service.DimCategory: {
			{Code: "01", Name: "生鲜联营", NetAmount: 4994.70, NetQty: 874.44,
				RefCost: 4880.00, CostedRows: 120, TotalRows: 120},
		},
	}}
	w := do(t, newRouter(f), http.MethodGet,
		"/reports/sales?store=1000003&dimension=category&from=2026-10-10&to=2026-10-10")
	if w.Code != http.StatusOK {
		t.Fatalf("= %d, want 200: %s", w.Code, w.Body.String())
	}
	if f.gotStore != "1000003" {
		t.Errorf("store 透传 = %q, want 1000003", f.gotStore)
	}
	if f.gotFrom != "2026-10-10" || f.gotTo != "2026-10-10" {
		t.Errorf("range = %q..%q, want 2026-10-10..2026-10-10", f.gotFrom, f.gotTo)
	}
	m := body(t, w)
	if m["group_by"] != service.DimCategory {
		t.Errorf("group_by = %v, want %q", m["group_by"], service.DimCategory)
	}
	summary, _ := m["summary"].(map[string]any)
	if summary["net_amount"] != "4,994.70" {
		t.Errorf("summary.net_amount = %v, want 4,994.70", summary["net_amount"])
	}
}

// TestReportCustomRangeWinsOverPreset 给了 from/to 时不得被 preset 覆盖。
//
// 这是常见的手滑：把「预设值先算出来再无条件赋值」写成 from/to 优先，
// 就会让自定义区间静默失效 —— 用户选了 10 月 1..10 日，看到的还是今天。
func TestReportCustomRangeWinsOverPreset(t *testing.T) {
	f := &fakeCube{}
	w := do(t, newRouter(f), http.MethodGet,
		"/reports/sales?store=00&preset=today&from=2026-10-01&to=2026-10-10")
	if w.Code != http.StatusOK {
		t.Fatalf("= %d, want 200: %s", w.Code, w.Body.String())
	}
	if f.gotFrom != "2026-10-01" || f.gotTo != "2026-10-10" {
		t.Errorf("range = %q..%q, want 2026-10-01..2026-10-10", f.gotFrom, f.gotTo)
	}
}

// TestReportMapsCubeErrorTo503 cube 挂了必须是 503 且指名是数据源不可用。
//
// 400 与 503 对调用方是完全不同的处置：前者改参数，后者等上游恢复。
// 混成 500 会让 Flutter 侧的重试逻辑无从判断。
func TestReportMapsCubeErrorTo503(t *testing.T) {
	f := &fakeCube{err: errors.New("dapr invoke failed")}
	w := do(t, newRouter(f), http.MethodGet, "/reports/sales?store=00")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("= %d, want 503", w.Code)
	}
	if code, _ := body(t, w)["code"].(string); code != "sales_source_unavailable" {
		t.Errorf("code = %q, want sales_source_unavailable", code)
	}
}

// TestReportDefaultsToCategoryAndToday 默认维度必须是类别。
//
// 群卡片与报表页的主视图都是类别；默认成 product 的话，报表打开就是
// 两千多行商品，且用户没有任何提示去切回来。
func TestReportDefaultsToCategoryAndToday(t *testing.T) {
	f := &fakeCube{}
	w := do(t, newRouter(f), http.MethodGet, "/reports/sales?store=00")
	if w.Code != http.StatusOK {
		t.Fatalf("= %d, want 200: %s", w.Code, w.Body.String())
	}
	if f.gotGroup != service.DimCategory {
		t.Errorf("默认 dimension = %q, want %q", f.gotGroup, service.DimCategory)
	}
	// today 预设：起止必须是同一天且非空。
	if f.gotFrom == "" || f.gotFrom != f.gotTo {
		t.Errorf("today range = %q..%q, want 同一非空日期", f.gotFrom, f.gotTo)
	}
	if _, err := time.Parse("2006-01-02", f.gotFrom); err != nil {
		t.Errorf("from = %q, 不是 YYYY-MM-DD", f.gotFrom)
	}
}

// TestTodayByCategoryUsesGivenDate 群卡片端点必须把 date 透传下去，
// 且门店同样按身份隔离。
func TestTodayByCategoryUsesGivenDate(t *testing.T) {
	f := &fakeCube{rows: map[string][]service.RawReport{
		service.DimCategory: {
			{Code: "01", Name: "生鲜联营", NetAmount: 2977.40, CostedRows: 16, TotalRows: 26},
		},
	}}
	w := do(t, newRouter(f), http.MethodGet,
		"/reports/sales/today?store=1000002&date=2026-10-10")
	if w.Code != http.StatusOK {
		t.Fatalf("= %d, want 200: %s", w.Code, w.Body.String())
	}
	if f.gotStore != "1000002" {
		t.Errorf("store 透传 = %q, want 1000002", f.gotStore)
	}
	if f.gotFrom != "2026-10-10" || f.gotTo != "2026-10-10" {
		t.Errorf("range = %q..%q, want 2026-10-10..2026-10-10", f.gotFrom, f.gotTo)
	}
	if f.gotGroup != service.DimCategory {
		t.Errorf("groupBy = %q, want %q", f.gotGroup, service.DimCategory)
	}
}

// TestReportRowsAreSortedByAmount 明细表在端点上仍然是按金额倒序 ——
// service 排的是结构体，序列化后顺序必须原样保留。
func TestReportRowsAreSortedByAmount(t *testing.T) {
	f := &fakeCube{rows: map[string][]service.RawReport{
		service.DimCategory: {
			{Code: "a", Name: "A", NetAmount: 10, CostedRows: 1, TotalRows: 1},
			{Code: "c", Name: "C", NetAmount: 3000, CostedRows: 1, TotalRows: 1},
			{Code: "b", Name: "B", NetAmount: 200, CostedRows: 1, TotalRows: 1},
		},
	}}
	w := do(t, newRouter(f), http.MethodGet, "/reports/sales?store=00&dimension=category")
	if w.Code != http.StatusOK {
		t.Fatalf("= %d, want 200: %s", w.Code, w.Body.String())
	}
	raw := w.Body.String()
	pos := func(code string) int { return strings.Index(raw, `"code":"`+code+`"`) }
	if !(pos("c") < pos("b") && pos("b") < pos("a")) || pos("c") < 0 {
		t.Errorf("rows 顺序不是金额倒序: %s", raw)
	}
}
