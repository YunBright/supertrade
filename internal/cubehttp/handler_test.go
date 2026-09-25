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

// ---- /stock/:product_id (X-Branch-ID header 取 branch) ----

func TestHandler_GetStock_OK(t *testing.T) {
	r := buildTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stock/P-1001", nil)
	req.Header.Set("X-Branch-ID", "S001")
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
	// 跨店阻断:S002 的 P-1003 没有库存。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stock/P-1003", nil)
	req.Header.Set("X-Branch-ID", "S002")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", w.Code)
	}
}

func TestHandler_GetStock_MissingBranchHeader(t *testing.T) {
	// X-Branch-ID 缺失 → 400 missing_branch_id(handler 不强制挂 RequireBranch 时)。
	r := buildTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stock/P-1001", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (no branch header): %s", w.Code, w.Body.String())
	}
}

// ---- NewClientFromEnv ----

func TestNewClientFromEnv_DefaultIsMemory(t *testing.T) {
	// 默认 CUBE_CLIENT_MODE=memory → NewInMemoryClient
	// (生产用 CUBE_CLIENT_MODE=dapr 切到 SDK 模式;此处验证默认行为)
	c, err := cubehttp.NewClientFromEnv()
	if err != nil {
		t.Fatalf("NewClientFromEnv: %v", err)
	}
	if _, ok := c.(*cubeclient.InMemoryClient); !ok {
		t.Errorf("默认应 InMemoryClient, got %T", c)
	}
}