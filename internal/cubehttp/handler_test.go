package cubehttp_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/YunBright/authkit/claims"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubehttp"
	"github.com/gin-gonic/gin"
)

// buildTestRouter 起一个带 cubehttp.Handler 的路由,withClaims=true 时注入测试 claims。
func buildTestRouter(t *testing.T, withClaims bool) *gin.Engine {
	t.Helper()
	cube := cubeclient.NewInMemoryClient()
	h := cubehttp.New(cube)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	if withClaims {
		r.Use(func(c *gin.Context) {
			cl := &claims.Claims{
				Sub:      "u-1",
				BranchID: "S001",
			}
			ctx := claims.WithClaims(c.Request.Context(), cl)
			c.Request = c.Request.WithContext(ctx)
			c.Next()
		})
	}
	api := r.Group("/api/v1")
	h.Register(api, cubehttp.RegisterOptions{
		Products:  true,
		Stock:     true,
		Suppliers: true,
	})
	return r
}

// ---- /products/:id ----

func TestHandler_GetProduct_OK(t *testing.T) {
	r := buildTestRouter(t, false)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/products/P-1001", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var p cubeclient.ProductDTO
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.ID != "P-1001" {
		t.Errorf("id = %q", p.ID)
	}
}

func TestHandler_GetProduct_NotFound(t *testing.T) {
	r := buildTestRouter(t, false)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/products/P-9999", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", w.Code)
	}
}

// ---- /stock/:branch_id/:product_id ----

func TestHandler_GetStock_OK(t *testing.T) {
	r := buildTestRouter(t, false)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stock/S001/P-1001", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var s cubeclient.StockSnapshotDTO
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s.ProductID != "P-1001" || s.BranchID != "S001" {
		t.Errorf("ids wrong: %+v", s)
	}
}

func TestHandler_GetStock_NotFound(t *testing.T) {
	r := buildTestRouter(t, false)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stock/S002/P-1003", nil) // 跨店阻断
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", w.Code)
	}
}

// ---- /suppliers ----

func TestHandler_ListSuppliers_All(t *testing.T) {
	r := buildTestRouter(t, false)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/suppliers", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var resp struct {
		Suppliers []cubeclient.SupplierDTO `json:"suppliers"`
		Count     int                      `json:"count"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count < 3 {
		t.Errorf("至少 3 个 mock supplier, got %d", resp.Count)
	}
}

func TestHandler_ListSuppliers_ByQuery(t *testing.T) {
	r := buildTestRouter(t, false)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/suppliers?q=可口可乐", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var resp struct {
		Count int `json:"count"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Count != 1 {
		t.Errorf("应 1 条, got %d", resp.Count)
	}
}

// ---- /products/search ----

func TestHandler_SearchProducts_OK(t *testing.T) {
	// 需要 claims 才能 default branch_id
	r := buildTestRouter(t, true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/products/search?barcode=6901234567890", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Count int `json:"count"`
		Meta  struct {
			InvViewable bool `json:"inv_viewable"`
		} `json:"meta"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Count != 1 {
		t.Errorf("count = %d, want 1", resp.Count)
	}
}

func TestHandler_SearchProducts_MissingBarcode(t *testing.T) {
	r := buildTestRouter(t, true)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/products/search", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400", w.Code)
	}
}

func TestHandler_SearchProducts_BranchIDFromQuery(t *testing.T) {
	r := buildTestRouter(t, false) // 无 claims,必须 ?branch_id=
	req := httptest.NewRequest(http.MethodGet, "/api/v1/products/search?barcode=6901234567890&branch_id=S001", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status %d, want 200, body=%s", w.Code, w.Body.String())
	}
}

func TestHandler_SearchProducts_BranchIDRequired_NoClaims(t *testing.T) {
	r := buildTestRouter(t, false)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/products/search?barcode=6901234567890", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400 (缺 branch_id)", w.Code)
	}
}

func TestHandler_SearchProducts_TooShort(t *testing.T) {
	r := buildTestRouter(t, true)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/products/search?barcode=123", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var resp struct {
		Count int `json:"count"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Count != 0 {
		t.Errorf("<5 位应 0, got %d", resp.Count)
	}
}

// ---- /products 列表 ----

func TestHandler_ListProducts_Limit(t *testing.T) {
	r := buildTestRouter(t, false)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/products?limit=2", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var resp struct {
		Products []cubeclient.ProductDTO `json:"products"`
		Count    int                     `json:"count"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	// mock 5 个商品,limit=2 → 至多 2 条
	if resp.Count > 2 {
		t.Errorf("limit=2 应不超过 2 条, got %d", resp.Count)
	}
}

// ---- NewClientFromEnv ----

func TestNewClientFromEnv_DefaultMemory(t *testing.T) {
	// 假设 env 没设 CUBE_CLIENT_MODE, 默认 memory
	c, err := cubehttp.NewClientFromEnv()
	if err != nil {
		t.Fatalf("NewClientFromEnv: %v", err)
	}
	if _, ok := c.(*cubeclient.InMemoryClient); !ok {
		t.Errorf("默认模式应 InMemoryClient, got %T", c)
	}
}
