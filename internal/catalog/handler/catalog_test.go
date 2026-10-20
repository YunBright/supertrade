// 本文件锁住 catalog 商品选择控件端点的 HTTP 层行为。
//
// # 为什么门店校验要单独测
//
// X-Branch-ID 必须是一个**具体门店**。最容易踩的坑是用
// middleware.SingleBranchFromCtx:它对 "*" 返回 "*" 本身、对 "00,01" 返回
// **第一项** "00"。用它做严格单店校验,多店请求会**静默按 00 查** ——
// 放行了不该放行的请求,而调用方以为拿到的是两家店的并集。
//
// 所以这里的断言不是"handler 没崩",而是:
//   - 明确返 400 + 规定的 code
//   - **一个 cube 查询都没发出去**(证明没有"先查了再说")
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	_ "modernc.org/sqlite" // 注册驱动名 "sqlite"(纯 Go 驱动,无需 CGO)

	"github.com/YunBright/supertrade/internal/catalog/model"
	"github.com/YunBright/supertrade/internal/catalog/service"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubeclient/cubeclientfake"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// recordingCube 记录"有没有真的去查 cube"。
//
// 用它断言"被拒的请求一个 cube 查询都没发出去" —— 只断言 HTTP 状态码的话,
// 一个"先查了再拒绝"的实现也能通过。
type recordingCube struct {
	*cubeclientfake.Client
	calls int
}

func (c *recordingCube) ListProducts(ctx context.Context, q cubeclient.ProductQuery) (*cubeclient.ProductPageDTO, error) {
	c.calls++
	return c.Client.ListProducts(ctx, q)
}

func (c *recordingCube) SearchProducts(ctx context.Context, q cubeclient.ProductQuery) (*cubeclient.ProductPageDTO, error) {
	c.calls++
	return c.Client.SearchProducts(ctx, q)
}

func (c *recordingCube) ListCategories(ctx context.Context) (*cubeclient.CategoryListDTO, error) {
	c.calls++
	return c.Client.ListCategories(ctx)
}

func (c *recordingCube) ListSuppliers(ctx context.Context) (*cubeclient.SupplierListDTO, error) {
	c.calls++
	return c.Client.ListSuppliers(ctx)
}

func (c *recordingCube) CountProductsByCategory(ctx context.Context) (map[string]int, error) {
	c.calls++
	return c.Client.CountProductsByCategory(ctx)
}

func (c *recordingCube) CountProductsBySupplier(ctx context.Context) (map[string]int, error) {
	c.calls++
	return c.Client.CountProductsBySupplier(ctx)
}

// newTestRouter 造一个只挂商品选择控件 4 条路由的 gin engine。
//
// 守门链只保留"严格单店" —— rbac 那层要 userd,单测里拉不起来,而它跟
// 路由 / 门店校验是两件事。
func newTestRouter(t *testing.T, cube cubeclient.Client) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.XBranchID())
	h := New(nil, nil, cube, nil, nil)
	h.RegisterProductPickerRoutes(r, func(scope string) []gin.HandlerFunc {
		return []gin.HandlerFunc{h.requireSingleBranch()}
	})
	return r
}

// newTestDB 造一个内存 sqlite(供 /suppliers/:id 路由的本地表查询用)。
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dialector := sqlite.Dialector{DriverName: "sqlite", DSN: ":memory:?_pragma=foreign_keys(1)"}
	db, err := gorm.Open(dialector, &gorm.Config{})
	require.NoError(t, err, "open sqlite")
	require.NoError(t, db.AutoMigrate(&model.Supplier{}, &model.Product{}), "auto migrate")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return db
}

// do 打一个请求,返回 recorder。
func do(t *testing.T, r *gin.Engine, path, branch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if branch != "" {
		req.Header.Set("X-Branch-ID", branch)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// errorCode 取错误信封里的 code。
func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON:%s", w.Body.String())
	}
	return body.Code
}

// pickerPaths 是商品选择控件的 4 条端点。
var pickerPaths = []string{
	"/products",
	"/products/search",
	"/categories",
	"/suppliers/summary",
}

// ---- 门店口径 ----

// 缺 X-Branch-ID → 400 branch_required;通配 / 多店 → 400 invalid_branch_scope。
func TestRequireSingleBranch_RejectsMissingWildcardAndMultiBranch(t *testing.T) {
	cases := []struct {
		name   string
		branch string
		want   string
	}{
		{"缺失", "", "branch_required"},
		{"通配", "*", "invalid_branch_scope"},
		{"多店", "00,01", "invalid_branch_scope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cube := &recordingCube{Client: cubeclientfake.New()}
			r := newTestRouter(t, cube)
			for _, path := range pickerPaths {
				w := do(t, r, path, tc.branch)
				if w.Code != http.StatusBadRequest {
					t.Fatalf("%s(branch=%q) 应返 400,实际 %d:%s", path, tc.branch, w.Code, w.Body.String())
				}
				if got := errorCode(t, w); got != tc.want {
					t.Errorf("%s(branch=%q) 的 code = %q, want %q", path, tc.branch, got, tc.want)
				}
			}
			// 关键:被拒的请求**一个 cube 查询都不该发**。
			// "先查了再拒绝"会让多店请求真的落到某一家店的 cube 上。
			if cube.calls != 0 {
				t.Errorf("被拒的请求发了 %d 次 cube 查询,必须一次都不发", cube.calls)
			}
		})
	}
}

// 多店请求**不得**静默按第一项门店查 —— 这条单独再钉一次。
func TestRequireSingleBranch_MultiBranchDoesNotFallBackToFirstBranch(t *testing.T) {
	cube := &recordingCube{Client: cubeclientfake.New()}
	r := newTestRouter(t, cube)

	w := do(t, r, "/products", "00,01")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("多店请求应返 400,实际 %d:%s", w.Code, w.Body.String())
	}
	if got := errorCode(t, w); got != "invalid_branch_scope" {
		t.Errorf("code = %q, want invalid_branch_scope", got)
	}
	if cube.calls != 0 {
		t.Errorf("多店请求发起了 %d 次 cube 查询:它被静默按第一家店查了", cube.calls)
	}
	if got := w.Body.String(); contains(got, "\"00\"") {
		t.Errorf("响应里不应出现门店 id %q:%s", "00", got)
	}
}

// 单店合法 → 放行。
func TestRequireSingleBranch_AllowsSingleConcreteBranch(t *testing.T) {
	cube := &recordingCube{Client: cubeclientfake.New()}
	r := newTestRouter(t, cube)

	for _, path := range pickerPaths {
		w := do(t, r, path, "00")
		if w.Code != http.StatusOK {
			t.Fatalf("%s(branch=00) 应返 200,实际 %d:%s", path, w.Code, w.Body.String())
		}
	}
	if cube.calls == 0 {
		t.Errorf("合法请求应真的去查 cube")
	}
}

// ---- 分页校验 ----

// page<1 / page_size 越界 → 400 bad_request。
func TestParsePageQuery_RejectsInvalidPagination(t *testing.T) {
	cases := []string{
		"page=0&page_size=20",
		"page=-1&page_size=20",
		"page=1&page_size=201",
		"page=1&page_size=10000",
	}
	cube := &recordingCube{Client: cubeclientfake.New()}
	r := newTestRouter(t, cube)
	for _, q := range cases {
		w := do(t, r, "/products?"+q, "00")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("/products?%s 应返 400,实际 %d:%s", q, w.Code, w.Body.String())
		}
		if got := errorCode(t, w); got != "bad_request" {
			t.Errorf("/products?%s 的 code = %q, want bad_request", q, got)
		}
	}
	if cube.calls != 0 {
		t.Errorf("越界的分页参数应被**在查 cube 之前**拒掉,实际发了 %d 次查询", cube.calls)
	}
}

// 合法分页 → 200,且回显 page / page_size。
func TestListProducts_EchoesPagination(t *testing.T) {
	cube := &recordingCube{Client: cubeclientfake.New()}
	r := newTestRouter(t, cube)

	w := do(t, r, "/products?page=2&page_size=5", "00")
	if w.Code != http.StatusOK {
		t.Fatalf("应返 200,实际 %d:%s", w.Code, w.Body.String())
	}
	var body struct {
		Products []map[string]any `json:"products"`
		Count    int              `json:"count"`
		Page     int              `json:"page"`
		PageSize int              `json:"page_size"`
		Total    int              `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON:%s", w.Body.String())
	}
	if body.Page != 2 || body.PageSize != 5 {
		t.Errorf("page/page_size 未回显:%+v", body)
	}
	if body.Count != len(body.Products) {
		t.Errorf("count(%d) 必须等于实际条数(%d)", body.Count, len(body.Products))
	}
}

// ---- 路由冲突 ----

// /suppliers/summary 必须命中 summary,**不能**被 /suppliers/:id 吃掉。
//
// 只注册 :id 时,`/suppliers/summary` 会返 404 supplier_not_found(实测)。
// gin 的路由树里静态段优先于参数段 —— 但这条依赖 gin 版本,必须实测。
func TestSuppliersSummaryRouteWinsOverParam(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.XBranchID())
	cube := &recordingCube{Client: cubeclientfake.New()}
	db := newTestDB(t)
	h := New(service.NewSupplierService(db), nil, cube, nil, nil)
	// 与生产同款的路径集合(:id 先注册,summary 后注册,顺序反过来也要成立)。
	r.GET("/suppliers/:id", h.getSupplier)
	h.RegisterProductPickerRoutes(r, func(scope string) []gin.HandlerFunc {
		return []gin.HandlerFunc{h.requireSingleBranch()}
	})

	w := do(t, r, "/suppliers/summary", "00")
	if w.Code != http.StatusOK {
		t.Fatalf("/suppliers/summary 应命中 summary 端点(200),实际 %d:%s", w.Code, w.Body.String())
	}
	var body struct {
		Suppliers []map[string]any `json:"suppliers"`
		Total     int              `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON:%s", w.Body.String())
	}
	if len(body.Suppliers) == 0 {
		t.Errorf("/suppliers/summary 返回了空列表,说明命中了错误的 handler:%s", w.Body.String())
	}

	// 另一个 id 仍然走 :id(找不到 → 404 supplier_not_found)。
	w2 := do(t, r, "/suppliers/SUP-999", "00")
	if w2.Code != http.StatusNotFound {
		t.Errorf("/suppliers/SUP-999 应走 :id 路由(404),实际 %d:%s", w2.Code, w2.Body.String())
	}
	if got := errorCode(t, w2); got != "supplier_not_found" {
		t.Errorf("code = %q, want supplier_not_found", got)
	}
}

// 生产用的 RegisterRoutes 必须真的注册了这 4 条路由(且不 panic)。
func TestRegisterRoutes_RegistersPickerRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := New(nil, nil, cubeclientfake.New(), nil, nil)
	h.RegisterRoutes(r)

	got := map[string]bool{}
	for _, ri := range r.Routes() {
		got[ri.Path] = true
	}
	for _, want := range append([]string{}, pickerPaths...) {
		if !got[want] {
			t.Errorf("路由表里没有 %s(实际注册:%v)", want, got)
		}
	}
}

// ---- 老路径兼容 ----

// ?barcode= 必须继续返回老字段(product_id / product_name / unit / price_yuan),
// 否则 Flutter 的 POP 价签流会静默拿到 null。
func TestSearchProducts_LegacyBarcodePathKeepsLegacyFields(t *testing.T) {
	cube := &recordingCube{Client: cubeclientfake.New()}
	r := newTestRouter(t, cube)

	w := do(t, r, "/products/search?barcode=6901234567890&limit=10", "00")
	if w.Code != http.StatusOK {
		t.Fatalf("老路径应返 200,实际 %d:%s", w.Code, w.Body.String())
	}
	var body struct {
		Products []map[string]any `json:"products"`
		Count    int              `json:"count"`
		Meta     map[string]any   `json:"meta"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON:%s", w.Body.String())
	}
	if len(body.Products) != 1 {
		t.Fatalf("应命中 1 条,实际 %d:%s", len(body.Products), w.Body.String())
	}
	p := body.Products[0]
	for _, key := range []string{"product_id", "product_name", "unit", "id", "name", "barcode"} {
		if _, ok := p[key]; !ok {
			t.Errorf("老路径的响应项缺字段 %q:%v", key, p)
		}
	}
	if p["product_id"] != p["id"] {
		t.Errorf("product_id(%v) 必须等于 id(%v)", p["product_id"], p["id"])
	}
	if body.Meta == nil {
		t.Errorf("老路径必须保留 meta 段")
	}
}

// 显式传空 barcode → 保持老行为(400 missing_barcode)。
func TestSearchProducts_EmptyBarcodeKeepsLegacyError(t *testing.T) {
	cube := &recordingCube{Client: cubeclientfake.New()}
	r := newTestRouter(t, cube)

	w := do(t, r, "/products/search?barcode=", "00")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("空 barcode 应返 400,实际 %d:%s", w.Code, w.Body.String())
	}
	if got := errorCode(t, w); got != "missing_barcode" {
		t.Errorf("code = %q, want missing_barcode", got)
	}
}

// 新路径(?q=)返回契约里的信封。
func TestSearchProducts_KeywordPathReturnsContractEnvelope(t *testing.T) {
	cube := &recordingCube{Client: cubeclientfake.New()}
	r := newTestRouter(t, cube)

	w := do(t, r, "/products/search?q=可口&page=1&page_size=20", "00")
	if w.Code != http.StatusOK {
		t.Fatalf("应返 200,实际 %d:%s", w.Code, w.Body.String())
	}
	var body struct {
		Products []map[string]any `json:"products"`
		Count    int              `json:"count"`
		Page     int              `json:"page"`
		PageSize int              `json:"page_size"`
		Total    int              `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON:%s", w.Body.String())
	}
	if len(body.Products) == 0 {
		t.Fatalf("按名称搜不到商品")
	}
	if body.Page != 1 || body.PageSize != 20 {
		t.Errorf("page/page_size 未回显:%+v", body)
	}
	if body.Count != len(body.Products) {
		t.Errorf("count(%d) 必须等于实际条数(%d)", body.Count, len(body.Products))
	}
	if body.Total <= 0 {
		t.Errorf("total = %d,应 > 0", body.Total)
	}
	for i, p := range body.Products {
		for _, key := range []string{"id", "name", "barcode", "category_id", "category_name",
			"supplier_id", "supplier_name", "unit", "price_yuan", "status"} {
			if _, ok := p[key]; !ok {
				t.Errorf("products[%d] 缺字段 %q:%v", i, key, p)
			}
		}
	}
}

// contains 是 strings.Contains 的本地小工具(避免为一个断言多引一个包)。
func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
