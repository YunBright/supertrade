package cubehttp_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubeclient/cubeclientfake"
	"github.com/YunBright/supertrade/internal/cubehttp"
	"github.com/gin-gonic/gin"
)

// buildTestRouter 起一个带 cubehttp.Handler 的路由(仅 stock 选项)。
//
// PR 2 后:cubehttp 只保留 stock 转发;product / supplier / search 已搬到
// catalog 服务,不在本测试覆盖范围。
func buildTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	cube := cubeclientfake.New()
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
	// X-Branch-ID 缺失 → 400 branch_required(handler 不强制挂 RequireBranch 时)。
	r := buildTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stock/P-1001", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (no branch header): %s", w.Code, w.Body.String())
	}
}

// ---- NewClient ----

// 回归锁(2026-10-08):原先 NewClientFromEnv 在未设 CUBE_CLIENT_MODE 时默认走
// InMemoryClient(mock 假数据),而 catalog / inventory / fresh-meat 三个服务的
// systemd unit 根本没有这个变量 —— 它们因此在**生产**读 mock 数据且零报错。
//
// 现在 mock 已从生产包移出(internal/cubeclient/cubeclientfake),构造函数恒定返回
// dapr 实现。本测试锁死两件事:
//  1. 即使环境里还残留 CUBE_CLIENT_MODE=memory,也**不得**返回任何 mock;
//  2. 返回的具体类型必须是 *cubeclient.DaprCubeClient。
func TestNewClient_HasNoMockBranch(t *testing.T) {
	// 故意设成旧值:证明这个变量已经彻底失效,不会把假数据接回线上。
	t.Setenv("CUBE_CLIENT_MODE", "memory")

	c, err := cubehttp.NewClient()
	if err != nil {
		// 没有 dapr sidecar 时构造可能失败(grpc 懒连接,通常不会);
		// 失败也算通过 —— 关键是**不可能**返回 mock。
		t.Skipf("dapr 不可用(%v),跳过类型断言;仍可确认没有 mock 分支", err)
	}

	if _, isDapr := c.(*cubeclient.DaprCubeClient); !isDapr {
		t.Fatalf("NewClient 必须返回 *cubeclient.DaprCubeClient, got %T", c)
	}
	// 类型名里出现 fake / Memory 即说明 mock 又被接回生产路径。
	if tn := fmt.Sprintf("%T", c); strings.Contains(tn, "fake") || strings.Contains(tn, "Memory") {
		t.Errorf("NewClient 返回了 mock 实现: %s", tn)
	}
}