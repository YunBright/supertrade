package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/YunBright/authkit/claims"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/stocktake/handler"
	"github.com/YunBright/supertrade/internal/stocktake/model"
	"github.com/YunBright/supertrade/internal/stocktake/service"
	"github.com/YunBright/supertrade/internal/stocktake/testdb"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

// buildTestHandler 起一个完整 handler(配 SQLite + InMemoryCube + 注入 claims)。
//
// DB 走 internal/stocktake/testdb(测试专用,生产代码不引用)。
// 生产代码只支持 PostgreSQL(见 stocktake.OpenPostgres)。
func buildTestHandler(t *testing.T) *gin.Engine {
	t.Helper()
	db, err := testdb.OpenSQLite(":memory:?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&model.StocktakeHeader{},
		&model.StocktakeLine{},
		&model.StocktakeLineOperation{},
		&model.StocktakePlanItem{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cube := cubeclient.NewInMemoryClient()
	svc := service.New(db, cube)
	fixed := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return fixed })
	cube.SetClock(func() time.Time { return fixed })

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(injectTestClaims())
	handler.New(svc).RegisterRoutes(r)
	return r
}

// injectTestClaims 模拟 dapr sidecar 已验签后由 authkit claims.GinMiddleware 解析的 claims。
func injectTestClaims() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := c.GetHeader("X-Test-Claims")
		if raw == "" {
			c.Next()
			return
		}
		var cl claims.Claims
		if err := json.Unmarshal([]byte(raw), &cl); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"code": "bad_claims"})
			return
		}
		ctx := claims.WithClaims(c.Request.Context(), &cl)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

func claimsHeader(userID string) string {
	cl := claims.Claims{
		Sub:      userID,
		BranchID: "S001",
	}
	b, _ := json.Marshal(cl)
	return string(b)
}

func TestHandler_CreateHeader_201(t *testing.T) {
	r := buildTestHandler(t)
	body := `{"branch_id":"S001","remark":"demo"}`
	req := httptest.NewRequest(http.MethodPost, "/stocktake-headers", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Claims", claimsHeader("u-1"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", w.Code, w.Body.String())
	}
	var h model.StocktakeHeader
	if err := json.Unmarshal(w.Body.Bytes(), &h); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if h.Status != model.StatusCounting {
		t.Errorf("status 应 counting")
	}
	if h.OperatorID != "u-1" {
		t.Errorf("operator_id 应取自 claims")
	}
}

// 端到端:创建 → 录入 → 差异表
func TestHandler_E2E_RealTimeStocktake(t *testing.T) {
	r := buildTestHandler(t)

	// 1. create header
	createBody := `{"branch_id":"S001","remark":"营业中 demo"}`
	req := httptest.NewRequest(http.MethodPost, "/stocktake-headers", bytes.NewBufferString(createBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Claims", claimsHeader("u-1"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status %d body %s", w.Code, w.Body.String())
	}
	var hdr model.StocktakeHeader
	json.Unmarshal(w.Body.Bytes(), &hdr)
	headerID := hdr.ID

	// 2. add line 1: P-1001 实盘 95,cube=100 → diff -5 / amount -12.5
	addBody := `{"product_id":"P-1001","actual_qty":95,"diff_reason":"loss"}`
	req = httptest.NewRequest(http.MethodPost, "/stocktake-headers/"+headerID+"/lines", bytes.NewBufferString(addBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Claims", claimsHeader("u-1"))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("add line 1: status %d body %s", w.Code, w.Body.String())
	}
	var line1 model.StocktakeLine
	json.Unmarshal(w.Body.Bytes(), &line1)
	if !line1.DiffAmountYuan.Equal(decimal.NewFromFloat(-12.5)) {
		t.Errorf("line1.diff_amount = %s, want -12.5", line1.DiffAmountYuan)
	}

	// 3. add line 2: P-1002 实盘 52,cube=50 → diff +2 / amount +5
	addBody = `{"product_id":"P-1002","actual_qty":52,"diff_reason":"overage"}`
	req = httptest.NewRequest(http.MethodPost, "/stocktake-headers/"+headerID+"/lines", bytes.NewBufferString(addBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Claims", claimsHeader("u-1"))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("add line 2: status %d body %s", w.Code, w.Body.String())
	}

	// 4. 拉差异表
	req = httptest.NewRequest(http.MethodGet, "/stocktake-headers/"+headerID+"/diff-report", nil)
	req.Header.Set("X-Test-Claims", claimsHeader("u-1"))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("diff report: status %d body %s", w.Code, w.Body.String())
	}
	var rep model.DiffReport
	json.Unmarshal(w.Body.Bytes(), &rep)
	if rep.Summary.TotalLines != 2 {
		t.Errorf("total_lines = %d, want 2", rep.Summary.TotalLines)
	}
	if rep.Summary.LossLines != 1 || rep.Summary.OverageLines != 1 {
		t.Errorf("loss/overage 行数错: %+v", rep.Summary)
	}
	if !rep.Summary.TotalDiffQty.Equal(decimal.NewFromInt(-3)) {
		t.Errorf("total_diff_qty = %s, want -3", rep.Summary.TotalDiffQty)
	}
	if !rep.Summary.TotalDiffAmountYuan.Equal(decimal.NewFromFloat(-7.5)) {
		t.Errorf("total_diff_amount = %s, want -7.5", rep.Summary.TotalDiffAmountYuan)
	}
	if len(rep.ByReason) != 2 {
		t.Errorf("by_reason 应 2 条, got %d", len(rep.ByReason))
	}
}

// 跨店阻断:S001 盘点,但录 P-1003(S001 有 200)改录 P-9999 → 400
func TestHandler_AddLine_ProductNotFound_400(t *testing.T) {
	r := buildTestHandler(t)

	// 先建 header
	createBody := `{"branch_id":"S001"}`
	req := httptest.NewRequest(http.MethodPost, "/stocktake-headers", bytes.NewBufferString(createBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Claims", claimsHeader("u-1"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var hdr model.StocktakeHeader
	json.Unmarshal(w.Body.Bytes(), &hdr)

	// 录不存在的商品
	addBody := `{"product_id":"P-9999","actual_qty":10}`
	req = httptest.NewRequest(http.MethodPost, "/stocktake-headers/"+hdr.ID+"/lines", bytes.NewBufferString(addBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Claims", claimsHeader("u-1"))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", w.Code, w.Body.String())
	}
	var body struct{ Code string }
	json.Unmarshal(w.Body.Bytes(), &body)
	if body.Code != "bad_request" {
		t.Errorf("code = %q, want bad_request", body.Code)
	}
}

func TestHandler_Submit_Approve_Lifecycle(t *testing.T) {
	r := buildTestHandler(t)

	// create
	req := httptest.NewRequest(http.MethodPost, "/stocktake-headers", bytes.NewBufferString(`{"branch_id":"S001"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Claims", claimsHeader("u-1"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var hdr model.StocktakeHeader
	json.Unmarshal(w.Body.Bytes(), &hdr)

	// add line
	addBody := `{"product_id":"P-1001","actual_qty":95,"diff_reason":"loss"}`
	req = httptest.NewRequest(http.MethodPost, "/stocktake-headers/"+hdr.ID+"/lines", bytes.NewBufferString(addBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Claims", claimsHeader("u-1"))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// submit
	req = httptest.NewRequest(http.MethodPost, "/stocktake-headers/"+hdr.ID+"/submit", nil)
	req.Header.Set("X-Test-Claims", claimsHeader("u-1"))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("submit status %d body %s", w.Code, w.Body.String())
	}
	var submitted model.StocktakeHeader
	json.Unmarshal(w.Body.Bytes(), &submitted)
	if submitted.Status != model.StatusAdjusted {
		t.Errorf("status 应 adjusted, got %q", submitted.Status)
	}
	if !submitted.TotalDiffAmountYuan.Equal(decimal.NewFromFloat(-12.5)) {
		t.Errorf("submit 后 total_diff_amount = %s, want -12.5", submitted.TotalDiffAmountYuan)
	}

	// approve
	req = httptest.NewRequest(http.MethodPost, "/stocktake-headers/"+hdr.ID+"/approve",
		bytes.NewBufferString(`{"auditor_id":"u-admin"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Claims", claimsHeader("u-1"))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("approve status %d body %s", w.Code, w.Body.String())
	}
	var approved model.StocktakeHeader
	json.Unmarshal(w.Body.Bytes(), &approved)
	if approved.Status != model.StatusApproved {
		t.Errorf("status 应 approved, got %q", approved.Status)
	}
	if approved.AuditorID != "u-admin" {
		t.Errorf("auditor_id = %q", approved.AuditorID)
	}
}

// ---- SearchProducts (REQUIREMENTS §2.1.4.1) ----

// 模拟带 inventory:view + supplier:view 的登录用户
func claimsHeaderFullPerm(userID string) string {
	cl := claims.Claims{
		Sub:      userID,
		BranchID: "S001",
		Scopes:   []string{"inventory:view", "supplier:view"},
	}
	b, _ := json.Marshal(cl)
	return string(b)
}

// 模拟只读 + 无 supplier/inventory 权限
func claimsHeaderNoPerm(userID string) string {
	cl := claims.Claims{
		Sub:      userID,
		BranchID: "S001",
	}
	b, _ := json.Marshal(cl)
	return string(b)
}

func TestHandler_SearchProducts_FullPerm(t *testing.T) {
	r := buildTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/products/search?barcode=6901234567890", nil)
	req.Header.Set("X-Test-Claims", claimsHeaderFullPerm("u-1"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Products []map[string]any `json:"products"`
		Count    int              `json:"count"`
		Meta     struct {
			InvViewable      bool   `json:"inv_viewable"`
			SupplierViewable bool   `json:"supplier_viewable"`
			BarcodeQuery     string `json:"barcode_query"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 1 || len(resp.Products) != 1 {
		t.Fatalf("count = %d, want 1", resp.Count)
	}
	row := resp.Products[0]
	if row["product_id"] != "P-1001" {
		t.Errorf("product_id = %v", row["product_id"])
	}
	// decimal.Decimal 序列化为 string,断言比较
	if v, ok := row["stock_qty"].(string); !ok || v != "100" {
		t.Errorf("stock_qty = %v(%T), want '100'", row["stock_qty"], row["stock_qty"])
	}
	if row["supplier_id"] != "SUP-001" || row["supplier_name"] != "可口可乐华南" {
		t.Errorf("supplier = %v/%v", row["supplier_id"], row["supplier_name"])
	}
	if !resp.Meta.InvViewable || !resp.Meta.SupplierViewable {
		t.Errorf("meta 权限标志应都 true")
	}
	if resp.Meta.BarcodeQuery != "6901234567890" {
		t.Errorf("barcode_query = %q", resp.Meta.BarcodeQuery)
	}
}

func TestHandler_SearchProducts_NoPermFiltersSensitive(t *testing.T) {
	r := buildTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/products/search?barcode=6901234567890", nil)
	req.Header.Set("X-Test-Claims", claimsHeaderNoPerm("u-1"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp struct {
		Products []map[string]any `json:"products"`
		Meta     struct {
			InvViewable      bool `json:"inv_viewable"`
			SupplierViewable bool `json:"supplier_viewable"`
		} `json:"meta"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	row := resp.Products[0]
	if _, ok := row["stock_qty"]; ok {
		t.Errorf("无 inventory:view 时 stock_qty 应缺省, got %v", row["stock_qty"])
	}
	if _, ok := row["supplier_id"]; ok {
		t.Errorf("无 supplier:view 时 supplier_id 应缺省, got %v", row["supplier_id"])
	}
	if _, ok := row["supplier_name"]; ok {
		t.Errorf("无 supplier:view 时 supplier_name 应缺省, got %v", row["supplier_name"])
	}
	if resp.Meta.InvViewable || resp.Meta.SupplierViewable {
		t.Errorf("meta 权限标志应都 false, got %+v", resp.Meta)
	}
}

func TestHandler_SearchProducts_BranchFromClaims(t *testing.T) {
	r := buildTestHandler(t)

	// 不传 branch_id,应取自 claims.BranchID = S001
	req := httptest.NewRequest(http.MethodGet, "/api/v1/products/search?barcode=6901234567890", nil)
	req.Header.Set("X-Test-Claims", claimsHeaderNoPerm("u-1"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
}

func TestHandler_SearchProducts_MissingBarcode_400(t *testing.T) {
	r := buildTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/products/search", nil)
	req.Header.Set("X-Test-Claims", claimsHeaderNoPerm("u-1"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", w.Code, w.Body.String())
	}
}

func TestHandler_SearchProducts_TooShortReturnsEmpty(t *testing.T) {
	r := buildTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/products/search?barcode=123", nil)
	req.Header.Set("X-Test-Claims", claimsHeaderFullPerm("u-1"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp struct {
		Count int `json:"count"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Count != 0 {
		t.Errorf("<5 位应 0 条, got %d", resp.Count)
	}
}

// ---- 2026-09-19 盘点单 H5 新端点 ----

func TestHandler_RecheckHeader_RequiresParent_400(t *testing.T) {
	r := buildTestHandler(t)

	// type=recheck 但没传 parent_header_id → 400 bad_request
	body := `{"branch_id":"S001","type":"recheck"}`
	req := httptest.NewRequest(http.MethodPost, "/stocktake-headers", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Claims", claimsHeader("u-1"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json: %v", err)
	}
	if resp.Code != "bad_request" {
		t.Errorf("code = %q, want bad_request", resp.Code)
	}

	// 即使传了 parent_header_id 但指向不存在的 header → 400
	body = `{"branch_id":"S001","type":"recheck","parent_header_id":"ST999999999999"}`
	req = httptest.NewRequest(http.MethodPost, "/stocktake-headers", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Claims", claimsHeader("u-1"))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("parent 不存在也应 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandler_PlanItems_PostAndGet(t *testing.T) {
	r := buildTestHandler(t)

	// 先建一个 plan 盘点单(用不同 count_date 避开 id 冲突)
	createBody := `{"branch_id":"S001","type":"plan","count_date":"2026-09-18"}`
	req := httptest.NewRequest(http.MethodPost, "/stocktake-headers", bytes.NewBufferString(createBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Claims", claimsHeader("u-1"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status %d body %s", w.Code, w.Body.String())
	}
	var hdr model.StocktakeHeader
	json.Unmarshal(w.Body.Bytes(), &hdr)

	// POST 加 2 条
	addBody := `{"items":[{"product_id":"P-1001","sort_order":2},{"product_id":"P-1002","sort_order":1}]}`
	req = httptest.NewRequest(http.MethodPost, "/stocktake-headers/"+hdr.ID+"/plan-items",
		bytes.NewBufferString(addBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Claims", claimsHeader("u-1"))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("add plan items: status %d body %s", w.Code, w.Body.String())
	}
	var addResp struct {
		Items []model.StocktakePlanItem `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &addResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(addResp.Items) != 2 {
		t.Errorf("应 2 条, got %d", len(addResp.Items))
	}

	// GET 应返 2 条,按 sort_order ASC 排序 → P-1002 (sort=1) 在前
	req = httptest.NewRequest(http.MethodGet, "/stocktake-headers/"+hdr.ID+"/plan-items", nil)
	req.Header.Set("X-Test-Claims", claimsHeader("u-1"))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get plan items: status %d body %s", w.Code, w.Body.String())
	}
	var getResp struct {
		Items []model.StocktakePlanItem `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &getResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(getResp.Items) != 2 {
		t.Fatalf("get 应 2 条, got %d", len(getResp.Items))
	}
	if getResp.Items[0].ProductID != "P-1002" {
		t.Errorf("sort_order ASC 应 P-1002 在前, got %s", getResp.Items[0].ProductID)
	}

	// 重复加 → 400 plan_item_duplicated
	dupBody := `{"items":[{"product_id":"P-1001","sort_order":3}]}`
	req = httptest.NewRequest(http.MethodPost, "/stocktake-headers/"+hdr.ID+"/plan-items",
		bytes.NewBufferString(dupBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Claims", claimsHeader("u-1"))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("重复加应 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandler_ListHeaders_FilteredByType(t *testing.T) {
	r := buildTestHandler(t)

	// 3 个盘点单:2 general + 1 plan。用不同的 count_date 错开 unique id。
	bodies := []string{
		`{"branch_id":"S001","type":"general","count_date":"2026-09-19"}`,
		`{"branch_id":"S001","type":"general","count_date":"2026-09-20"}`,
		`{"branch_id":"S001","type":"plan","count_date":"2026-09-21"}`,
	}
	for i, body := range bodies {
		_ = i
		req := httptest.NewRequest(http.MethodPost, "/stocktake-headers", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Test-Claims", claimsHeader("u-1"))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("create #%d: status %d body %s", i, w.Code, w.Body.String())
		}
	}

	// GET ?type=plan → 1 条
	req := httptest.NewRequest(http.MethodGet, "/stocktake-headers?type=plan&page_size=10", nil)
	req.Header.Set("X-Test-Claims", claimsHeader("u-1"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list: status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Total    int                     `json:"total"`
		Page     int                     `json:"page"`
		PageSize int                     `json:"page_size"`
		Headers  []model.StocktakeHeader `json:"headers"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Total != 1 {
		t.Errorf("type=plan 应 1 条, got total=%d", resp.Total)
	}
	if len(resp.Headers) > 0 && resp.Headers[0].Type != model.TypePlan {
		t.Errorf("首条 type=%q, want plan", resp.Headers[0].Type)
	}
}

func TestHandler_History_AfterAdd(t *testing.T) {
	r := buildTestHandler(t)

	// create header
	req := httptest.NewRequest(http.MethodPost, "/stocktake-headers",
		bytes.NewBufferString(`{"branch_id":"S001","count_date":"2026-09-22"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Claims", claimsHeader("u-1"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var hdr model.StocktakeHeader
	json.Unmarshal(w.Body.Bytes(), &hdr)

	// add line
	req = httptest.NewRequest(http.MethodPost, "/stocktake-headers/"+hdr.ID+"/lines",
		bytes.NewBufferString(`{"product_id":"P-1001","actual_qty":95,"actor_name":"张三","method":"scan"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Claims", claimsHeader("u-1"))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("add line: status %d body %s", w.Code, w.Body.String())
	}

	// GET history
	req = httptest.NewRequest(http.MethodGet, "/stocktake-headers/"+hdr.ID+"/history", nil)
	req.Header.Set("X-Test-Claims", claimsHeader("u-1"))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("history: status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Operations []model.StocktakeLineOperation `json:"operations"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Operations) != 1 {
		t.Fatalf("应 1 条历史, got %d", len(resp.Operations))
	}
	if resp.Operations[0].OpType != model.OpCreate {
		t.Errorf("op_type=%q, want create", resp.Operations[0].OpType)
	}
	if resp.Operations[0].ActorName != "张三" {
		t.Errorf("actor_name=%q, want 张三", resp.Operations[0].ActorName)
	}
	if resp.Operations[0].Method != model.MethodScan {
		t.Errorf("method=%q, want scan", resp.Operations[0].Method)
	}
}
