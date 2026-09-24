package cubehttp_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubehttp"
	"github.com/gin-gonic/gin"
)

// buildTestRouter 起一个带 cubehttp.Handler 的路由(仅 stock 选项)。
//
// PR 2 后:cubehttp 只保留 stock 转发;product / supplier / search 已搬到
// catalog 服务,不在本测试覆盖范围。
func buildTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	cube := cubeclient.NewInMemoryClient()
	h := cubehttp.New(cube)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1")
	h.Register(api, cubehttp.RegisterOptions{Stock: true})
	return r
}

// ---- /stock/:branch_id/:product_id ----

func TestHandler_GetStock_OK(t *testing.T) {
	r := buildTestRouter(t)

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
	r := buildTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stock/S002/P-1003", nil) // 跨店阻断
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", w.Code)
	}
}

// ---- NewClientFromEnv ----

func TestNewClientFromEnv_ReturnsHTTPCubeClient(t *testing.T) {
	// NewClientFromEnv 现在固定返回 HTTPCubeClient(去掉 CUBE_CLIENT_MODE 切换):
	// PR 2 后 catalog / inventory 都走 HTTP(生产),测试场景用 cubeclient.NewInMemoryClient 直连。
	c, err := cubehttp.NewClientFromEnv()
	if err != nil {
		t.Fatalf("NewClientFromEnv: %v", err)
	}
	if _, ok := c.(*cubeclient.HTTPCubeClient); !ok {
		t.Errorf("应 HTTPCubeClient, got %T", c)
	}
}