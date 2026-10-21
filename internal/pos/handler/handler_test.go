package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YunBright/authkit/claims"
	"github.com/YunBright/authkit/rbac"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
	_ "modernc.org/sqlite"

	"github.com/YunBright/supertrade/internal/pos/handler"
	"github.com/YunBright/supertrade/internal/pos/model"
	"github.com/YunBright/supertrade/internal/pos/service"
	"github.com/YunBright/supertrade/pkg/middleware"
	"github.com/YunBright/supertrade/pkg/outbox"
)

// newTestDB 构造文件型 SQLite + AutoMigrate(同 service 包的做法)。
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "pos.db")
	dialector := sqlite.Dialector{DriverName: "sqlite", DSN: dsn}
	db, err := gorm.Open(dialector, &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.SalesOrder{},
		&model.SaleLine{},
		&model.Payment{},
		&outbox.Outbox{},
	))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// fakeResolver 是 per-branch scope 解析器的 fake。
//
// ⚠️ rbac.ScopeResolver 是**函数类型**(不是接口)⇒ fake 也得是函数。
func fakeResolver(allow map[string]bool) rbac.ScopeResolver {
	return func(c *gin.Context, userID, branchID, scope string) (bool, error) {
		return allow[scope], nil
	}
}

// newTestRouter 造一个挂了全部 pos 路由的 gin engine。
//
// ⚠️ claims 是**注入**的:cmdbootstrap 的验签中间件在单测里不装配,
//
//	但 scope 守门需要 claims.FromContext(ctx) ⇒ 这里手工塞一个。
func newTestRouter(t *testing.T, db *gorm.DB, resolver rbac.ScopeResolver) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.XBranchID())
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(
			claims.WithClaims(c.Request.Context(), &claims.Claims{Sub: "u-1"}))
		c.Next()
	})
	svc := service.NewSalesService(db)
	handler.NewWithResolver(svc, resolver, nil).RegisterRoutes(r)
	return r
}

// do 发一个 JSON 请求。
func do(t *testing.T, r *gin.Engine, method, path, branch, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if branch != "" {
		req.Header.Set("X-Branch-ID", branch)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// saleBody 是一份自洽的开单请求体。
const saleBody = `{
  "op_id": "op-1",
  "branch_id": "10",
  "member_id": "M0001",
  "operator_id": "u-1",
  "lines": [
    {"product_id":"00000297","product_name":"可口可乐","product_no":"P0001",
     "barcode":"6900000000001","unit":"瓶","qty":"2","original_price_yuan":"12.00",
     "unit_price_yuan":"11.10","discount_amount_yuan":"1.80","amount_yuan":"22.20"}
  ],
  "payments": [
    {"pay_way":"RMB","amount_yuan":"22.20","exchange_rate":"1","converted_amount_yuan":"22.20"}
  ],
  "total_amount_yuan": "22.20",
  "discount_amount_yuan": "1.80",
  "paid_amount_yuan": "50.00",
  "change_amount_yuan": "27.80"
}`

// errResp 是错误信封。
type errResp struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	RequiredScope string `json:"required_scope"`
}

// saleResp 是成功响应。
type saleResp struct {
	SaleID   string            `json:"sale_id"`
	Replayed bool              `json:"replayed"`
	Order    *model.SalesOrder `json:"order"`
	Lines    []model.SaleLine  `json:"lines"`
	Payments []model.Payment   `json:"payments"`
}

func decodeSale(t *testing.T, w *httptest.ResponseRecorder) saleResp {
	t.Helper()
	var out saleResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out), "body=%s", w.Body.String())
	return out
}

func decodeErr(t *testing.T, w *httptest.ResponseRecorder) errResp {
	t.Helper()
	var out errResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out), "body=%s", w.Body.String())
	return out
}

// ---- 开单 ----

func TestCreateSale_OK(t *testing.T) {
	db := newTestDB(t)
	r := newTestRouter(t, db, fakeResolver(map[string]bool{handler.ScopeSale: true}))

	w := do(t, r, http.MethodPost, "/sales", "10", saleBody)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	got := decodeSale(t, w)
	assert.NotEmpty(t, got.SaleID)
	assert.False(t, got.Replayed)
	require.NotNil(t, got.Order)
	assert.Equal(t, "M0001", got.Order.MemberID)
	assert.Equal(t, "paid", got.Order.Status)
	require.Len(t, got.Lines, 1)
	require.Len(t, got.Payments, 1)
	assert.Equal(t, "RMB", got.Payments[0].PayWay)
}

func TestCreateSale_AmountsAreStringsInJSON(t *testing.T) {
	db := newTestDB(t)
	r := newTestRouter(t, db, fakeResolver(map[string]bool{handler.ScopeSale: true}))

	w := do(t, r, http.MethodPost, "/sales", "10", saleBody)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var generic map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &generic))
	order, ok := generic["order"].(map[string]any)
	require.True(t, ok)
	assert.IsType(t, "", order["total_amount_yuan"], "金额在 JSON 里必须是字符串")
	assert.IsType(t, "", order["change_amount_yuan"])
}

// TestCreateSale_SameOpID_ReplaysFirstSale 是幂等的 HTTP 层守卫。
func TestCreateSale_SameOpID_ReplaysFirstSale(t *testing.T) {
	db := newTestDB(t)
	r := newTestRouter(t, db, fakeResolver(map[string]bool{handler.ScopeSale: true}))

	first := decodeSale(t, do(t, r, http.MethodPost, "/sales", "10", saleBody))
	require.Equal(t, http.StatusOK, do(t, r, http.MethodPost, "/sales", "10", saleBody).Code)
	second := decodeSale(t, do(t, r, http.MethodPost, "/sales", "10", saleBody))

	assert.Equal(t, first.SaleID, second.SaleID, "同 op_id 必须返回同一个 sale_id")
	assert.True(t, second.Replayed, "重放要标记 replayed=true")

	var n int64
	require.NoError(t, db.Model(&model.SalesOrder{}).Count(&n).Error)
	assert.EqualValues(t, 1, n, "重放不得新建单")
}

func TestCreateSale_EmptyLines_400(t *testing.T) {
	db := newTestDB(t)
	r := newTestRouter(t, db, fakeResolver(map[string]bool{handler.ScopeSale: true}))

	body := `{"op_id":"op-1","operator_id":"u-1","lines":[],"payments":[
	  {"pay_way":"RMB","amount_yuan":"22.20"}],
	  "total_amount_yuan":"22.20","paid_amount_yuan":"22.20","change_amount_yuan":"0"}`
	w := do(t, r, http.MethodPost, "/sales", "10", body)
	assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, "bad_request", decodeErr(t, w).Code)
}

func TestCreateSale_AmountMismatch_409(t *testing.T) {
	db := newTestDB(t)
	r := newTestRouter(t, db, fakeResolver(map[string]bool{handler.ScopeSale: true}))

	// 找零 ≠ 实收 - 合计 ⇒ 409。
	body := strings.Replace(saleBody, `"change_amount_yuan": "27.80"`, `"change_amount_yuan": "0.00"`, 1)
	w := do(t, r, http.MethodPost, "/sales", "10", body)
	assert.Equal(t, http.StatusConflict, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, "amount_mismatch", decodeErr(t, w).Code)
}

func TestCreateSale_NoBranchHeader_400(t *testing.T) {
	db := newTestDB(t)
	r := newTestRouter(t, db, fakeResolver(map[string]bool{handler.ScopeSale: true}))

	w := do(t, r, http.MethodPost, "/sales", "", saleBody)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "branch_required", decodeErr(t, w).Code)
}

func TestCreateSale_BranchMismatch_400(t *testing.T) {
	db := newTestDB(t)
	r := newTestRouter(t, db, fakeResolver(map[string]bool{handler.ScopeSale: true}))

	w := do(t, r, http.MethodPost, "/sales", "01", saleBody)
	assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, "branch_mismatch", decodeErr(t, w).Code)
}

func TestCreateSale_WithoutScope_403(t *testing.T) {
	db := newTestDB(t)
	r := newTestRouter(t, db, fakeResolver(map[string]bool{}))

	w := do(t, r, http.MethodPost, "/sales", "10", saleBody)
	assert.Equal(t, http.StatusForbidden, w.Code, "body=%s", w.Body.String())
}

func TestCreateSale_InvalidAmountString_400(t *testing.T) {
	db := newTestDB(t)
	r := newTestRouter(t, db, fakeResolver(map[string]bool{handler.ScopeSale: true}))

	body := strings.Replace(saleBody, `"total_amount_yuan": "22.20"`, `"total_amount_yuan": "abc"`, 1)
	w := do(t, r, http.MethodPost, "/sales", "10", body)
	assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, "bad_request", decodeErr(t, w).Code)
}

// ---- 查单 / 列表 ----

func TestGetSale_OK(t *testing.T) {
	db := newTestDB(t)
	r := newTestRouter(t, db, fakeResolver(map[string]bool{handler.ScopeSale: true}))

	created := decodeSale(t, do(t, r, http.MethodPost, "/sales", "10", saleBody))
	w := do(t, r, http.MethodGet, "/sales/"+created.SaleID, "10", "")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	got := decodeSale(t, w)
	assert.Equal(t, created.SaleID, got.SaleID)
	require.Len(t, got.Lines, 1)
	require.Len(t, got.Payments, 1)
}

func TestGetSale_NotFound_404(t *testing.T) {
	db := newTestDB(t)
	r := newTestRouter(t, db, fakeResolver(map[string]bool{handler.ScopeSale: true}))

	w := do(t, r, http.MethodGet, "/sales/SO20261021009999", "10", "")
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, "sale_not_found", decodeErr(t, w).Code)
}

func TestListSales_OK(t *testing.T) {
	db := newTestDB(t)
	r := newTestRouter(t, db, fakeResolver(map[string]bool{handler.ScopeSale: true}))

	require.Equal(t, http.StatusOK, do(t, r, http.MethodPost, "/sales", "10", saleBody).Code)
	w := do(t, r, http.MethodGet, "/sales?page=1&page_size=10", "10", "")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var out struct {
		Sales    []model.SalesOrder `json:"sales"`
		Count    int                `json:"count"`
		Total    int64              `json:"total"`
		Page     int                `json:"page"`
		PageSize int                `json:"page_size"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Len(t, out.Sales, 1)
	assert.EqualValues(t, 1, out.Total)
	assert.Equal(t, 1, out.Page)
	assert.Equal(t, 10, out.PageSize)
}

// ---- 改价 ----

// TestPriceOverride_WithoutScope_403 是"可引导的 403"的守卫测试。
func TestPriceOverride_WithoutScope_403(t *testing.T) {
	db := newTestDB(t)
	r := newTestRouter(t, db, fakeResolver(map[string]bool{handler.ScopeSale: true}))

	created := decodeSale(t, do(t, r, http.MethodPost, "/sales", "10", saleBody))
	w := do(t, r, http.MethodPost,
		"/sales/"+created.SaleID+"/lines/"+created.SaleID+"-L1/price-override",
		"10", `{"unit_price_yuan":"9.90"}`)

	assert.Equal(t, http.StatusForbidden, w.Code, "body=%s", w.Body.String())
	e := decodeErr(t, w)
	assert.Equal(t, "pos_permission_required", e.Code)
	assert.Equal(t, "当前收银员没有改价权限，请输入授权", e.Message)
	assert.Equal(t, "pos:price-override", e.RequiredScope)
}

// TestPriceOverride_WithAuthorizedBy_Allowed 是越权授权放行 + 审计落库。
func TestPriceOverride_WithAuthorizedBy_Allowed(t *testing.T) {
	db := newTestDB(t)
	r := newTestRouter(t, db, fakeResolver(map[string]bool{handler.ScopeSale: true}))

	created := decodeSale(t, do(t, r, http.MethodPost, "/sales", "10", saleBody))
	w := do(t, r, http.MethodPost,
		"/sales/"+created.SaleID+"/lines/"+created.SaleID+"-L1/price-override",
		"10", `{"unit_price_yuan":"9.90","authorized_by":"boss-1"}`)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	got := decodeSale(t, w)
	require.NotNil(t, got.Order)
	assert.Equal(t, "boss-1", got.Order.AuthorizedBy, "越权授权人必须落库")
	assert.Equal(t, "19.80", got.Order.TotalAmountYuan.StringFixed(2))
	require.Len(t, got.Lines, 1)
	assert.Equal(t, "9.90", got.Lines[0].UnitPriceYuan.StringFixed(2))
}

func TestPriceOverride_WithScope_OK(t *testing.T) {
	db := newTestDB(t)
	r := newTestRouter(t, db, fakeResolver(map[string]bool{
		handler.ScopeSale:          true,
		handler.ScopePriceOverride: true,
	}))

	created := decodeSale(t, do(t, r, http.MethodPost, "/sales", "10", saleBody))
	w := do(t, r, http.MethodPost,
		"/sales/"+created.SaleID+"/lines/"+created.SaleID+"-L1/price-override",
		"10", `{"unit_price_yuan":"9.90"}`)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	got := decodeSale(t, w)
	require.NotNil(t, got.Order)
	assert.Empty(t, got.Order.AuthorizedBy, "有 scope 时不需要授权人")
	assert.Equal(t, "19.80", got.Order.TotalAmountYuan.StringFixed(2))
}

func TestPriceOverride_UnknownLine_404(t *testing.T) {
	db := newTestDB(t)
	r := newTestRouter(t, db, fakeResolver(map[string]bool{
		handler.ScopeSale:          true,
		handler.ScopePriceOverride: true,
	}))

	created := decodeSale(t, do(t, r, http.MethodPost, "/sales", "10", saleBody))
	w := do(t, r, http.MethodPost,
		"/sales/"+created.SaleID+"/lines/NOPE/price-override",
		"10", `{"unit_price_yuan":"9.90"}`)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, "sale_line_not_found", decodeErr(t, w).Code)
}

// ---- 退货 ----

func TestRefundSale_OK(t *testing.T) {
	db := newTestDB(t)
	r := newTestRouter(t, db, fakeResolver(map[string]bool{
		handler.ScopeSale:   true,
		handler.ScopeRefund: true,
	}))

	created := decodeSale(t, do(t, r, http.MethodPost, "/sales", "10", saleBody))
	w := do(t, r, http.MethodPost, "/sales/"+created.SaleID+"/refund", "10",
		`{"op_id":"op-refund-1","operator_id":"u-2"}`)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	got := decodeSale(t, w)
	require.NotNil(t, got.Order)
	assert.Equal(t, "returned", got.Order.Status)

	// 发件箱里应有两条事件(销售 + 退货),且都是 sale.paid。
	var rows []outbox.Outbox
	require.NoError(t, db.Order("id asc").Find(&rows).Error)
	require.Len(t, rows, 2)
	for _, row := range rows {
		assert.Equal(t, "sale.paid", row.Topic)
	}
}

func TestRefundSale_WithoutScope_403(t *testing.T) {
	db := newTestDB(t)
	r := newTestRouter(t, db, fakeResolver(map[string]bool{handler.ScopeSale: true}))

	created := decodeSale(t, do(t, r, http.MethodPost, "/sales", "10", saleBody))
	w := do(t, r, http.MethodPost, "/sales/"+created.SaleID+"/refund", "10", `{"op_id":"op-r"}`)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// ---- 路由表 ----

func TestRegisterRoutes(t *testing.T) {
	db := newTestDB(t)
	r := newTestRouter(t, db, fakeResolver(map[string]bool{}))

	got := map[string]bool{}
	for _, ri := range r.Routes() {
		got[ri.Method+" "+ri.Path] = true
	}
	for _, want := range []string{
		"POST /sales",
		"GET /sales",
		"GET /sales/:id",
		"POST /sales/:id/refund",
		"POST /sales/:id/lines/:line_id/price-override",
	} {
		assert.True(t, got[want], "路由表里没有 %s(实际:%v)", want, got)
	}
}
