package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/YunBright/authkit/userinfo"
	"github.com/YunBright/supertrade/internal/cubeclient/cubeclientfake"
	"github.com/YunBright/supertrade/internal/stocktake/handler"
	"github.com/YunBright/supertrade/internal/stocktake/model"
	"github.com/YunBright/supertrade/internal/stocktake/service"
	"github.com/YunBright/supertrade/internal/stocktake/testdb"
	"github.com/YunBright/supertrade/pkg/middleware"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// 排除域(猪肉)在 **HTTP 层**的行为。
//
// 服务层已经保证"判出来就拒绝";这里锁的是**状态码与错误体**:
// 必须是 400(不是 500 —— 选错流程不是服务端故障),
// 且消息里要能看出"该去哪个流程盘"。

// buildScopeHandler 起一个带排除域配置的 handler。
//
// 与 buildTestHandler 的区别:这里要往库里写**配置行**,所以把 db 一起返回。
func buildScopeHandler(t *testing.T) (*gin.Engine, *gorm.DB) {
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
		&model.StocktakeBranchDefault{},
		&model.StocktakeExcludedScope{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cube := cubeclientfake.New()
	svc := service.New(db, cube)
	fixed := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return fixed })
	cube.SetClock(func() time.Time { return fixed })
	cube.UpsertStock(testBranchID, "P-1001", decimal.NewFromInt(100), decimal.NewFromFloat(2.5))
	// P-2001(五花肉)在 fake 里的分类是 CAT-02 —— 本用例把它当"猪肉"。
	cube.UpsertStock(testBranchID, "P-2001", decimal.NewFromFloat(15.5), decimal.NewFromFloat(38.0))

	// 排除域配置:本门店的 CAT-02 不许在通用盘点里盘。
	now := fixed
	if err := db.Create(&model.StocktakeExcludedScope{
		ID: "SES-TEST-CAT-02", BranchID: testBranchID,
		ScopeKind: model.ScopeKindCategory, ScopeValue: "CAT-02",
		Note:      "猪肉请用鲜肉盘点(生鲜肉盘点)",
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("写排除域配置: %v", err)
	}

	// fake userd,让 per-branch 守门走通(与 buildTestHandler 同款)。
	userd := fakeUserD(t)
	t.Cleanup(userd.Close)
	users, err := userinfo.New("userd", userinfo.WithMockClient(&fakeDaprForUserd{baseURL: userd.URL}))
	if err != nil {
		t.Fatalf("userinfo.New: %v", err)
	}
	svc.SetUserInfo(users)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(injectTestClaims())
	r.Use(middleware.XBranchID())
	handler.New(svc).RegisterRoutes(r)
	return r, db
}

// createScopeHeader 建一张盘点单,返回 header id。
func createScopeHeader(t *testing.T, r *gin.Engine) string {
	t.Helper()
	body := `{"branch_id":"` + testBranchID + `"}`
	req := httptest.NewRequest(http.MethodPost, "/stocktake-headers", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Claims", claimsHeader("u-1"))
	req.Header.Set("X-Branch-ID", testBranchID)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create header: status %d body %s", w.Code, w.Body.String())
	}
	var hdr model.StocktakeHeader
	if err := json.Unmarshal(w.Body.Bytes(), &hdr); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	return hdr.ID
}

// TestHandler_AddLine_ExcludedScope_400 —— 录猪肉 → 400,不是 500。
func TestHandler_AddLine_ExcludedScope_400(t *testing.T) {
	r, db := buildScopeHandler(t)
	headerID := createScopeHeader(t, r)

	body := `{"product_id":"P-2001","actual_qty":15}`
	req := httptest.NewRequest(http.MethodPost, "/stocktake-headers/"+headerID+"/lines",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Claims", claimsHeader("u-1"))
	req.Header.Set("X-Branch-ID", testBranchID)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode err body: %v", err)
	}
	if resp.Code != "excluded_scope" {
		t.Errorf("code = %q, want excluded_scope", resp.Code)
	}
	if !bytes.Contains([]byte(resp.Message), []byte("鲜肉盘点")) {
		t.Errorf("message 应点名正确流程, got %q", resp.Message)
	}
	// 一行都不该落库。
	var n int64
	if err := db.Model(&model.StocktakeLine{}).Count(&n).Error; err != nil {
		t.Fatalf("count lines: %v", err)
	}
	if n != 0 {
		t.Errorf("被拒绝的商品不应落库, lines=%d", n)
	}
}

// TestHandler_AddLine_NonExcludedScope_201 —— 非猪肉照常录(守卫不能误伤)。
func TestHandler_AddLine_NonExcludedScope_201(t *testing.T) {
	r, _ := buildScopeHandler(t)
	headerID := createScopeHeader(t, r)

	body := `{"product_id":"P-1001","actual_qty":95}`
	req := httptest.NewRequest(http.MethodPost, "/stocktake-headers/"+headerID+"/lines",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Claims", claimsHeader("u-1"))
	req.Header.Set("X-Branch-ID", testBranchID)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", w.Code, w.Body.String())
	}
}

// TestHandler_AddPlanItems_ExcludedScope_400 —— 计划清单圈了猪肉 → 400。
func TestHandler_AddPlanItems_ExcludedScope_400(t *testing.T) {
	r, db := buildScopeHandler(t)
	headerID := createScopeHeader(t, r)

	body := `{"items":[{"product_id":"P-1001","sort_order":1},{"product_id":"P-2001","sort_order":2}]}`
	req := httptest.NewRequest(http.MethodPost, "/stocktake-headers/"+headerID+"/plan-items",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Claims", claimsHeader("u-1"))
	req.Header.Set("X-Branch-ID", testBranchID)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode err body: %v", err)
	}
	if resp.Code != "excluded_scope" {
		t.Errorf("code = %q, want excluded_scope", resp.Code)
	}
	// 整批拒绝 ⇒ 计划清单里一行都不该有。
	var n int64
	if err := db.Model(&model.StocktakePlanItem{}).Count(&n).Error; err != nil {
		t.Fatalf("count plan items: %v", err)
	}
	if n != 0 {
		t.Errorf("整批拒绝 ⇒ 计划清单不该有行, got %d", n)
	}
}

// TestHandler_AddPlanItems_NonExcludedScope_201 —— 非猪肉计划照常建。
func TestHandler_AddPlanItems_NonExcludedScope_201(t *testing.T) {
	r, _ := buildScopeHandler(t)
	headerID := createScopeHeader(t, r)

	body := `{"items":[{"product_id":"P-1001","sort_order":1}]}`
	req := httptest.NewRequest(http.MethodPost, "/stocktake-headers/"+headerID+"/plan-items",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Claims", claimsHeader("u-1"))
	req.Header.Set("X-Branch-ID", testBranchID)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", w.Code, w.Body.String())
	}
}
