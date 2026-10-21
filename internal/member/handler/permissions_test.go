package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/YunBright/authkit/claims"
	"github.com/YunBright/authkit/userinfo"
	dapr "github.com/dapr/go-sdk/client"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/YunBright/supertrade/internal/member/handler"
	"github.com/YunBright/supertrade/internal/member/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// permOperatorSub 是测试里的当前操作人(auth user id)。
const permOperatorSub = "u-1"

// permUserdStub 模拟 userd 的 per-branch permissions 端点。
//
// ⚠️ rows 固定返回(不随请求的 branch_id 变)⇒ 用来测"别的门店的权限不串台"。
type permUserdStub struct {
	mu sync.Mutex
	// rows 是返回的 per-branch 矩阵(nil ⇒ 按请求的 branch_id 回一行)。
	rows []userinfo.BranchPermissionsRow
	// failFromCall 是"第 N 次调用起失败"(0 = 永远不失败)。
	failFromCall int
	// calls 是被调用次数(守门 1 次 + handler 1 次 = 2 次)。
	calls int
	// lastBranch 是最近一次请求带的 branch_id。
	lastBranch string
}

// ServeHTTP 实现 http.Handler。
func (s *permUserdStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.calls++
	n := s.calls
	s.mu.Unlock()

	if s.failFromCall > 0 && n >= s.failFromCall {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	bid := r.URL.Query().Get("branch_id")
	s.mu.Lock()
	s.lastBranch = bid
	rows := s.rows
	s.mu.Unlock()
	if rows == nil {
		rows = []userinfo.BranchPermissionsRow{{BranchID: bid, Scopes: []string{"member:view"}}}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(userinfo.BranchPermissions{Branches: rows})
}

// callCount 返回被调次数。
func (s *permUserdStub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// permFakeDapr 把 dapr.Client.InvokeMethodWithContent 转成对 userd httptest server 的 HTTP 调用。
//
// 嵌入 dapr.Client(nil impl):其它方法被调用会 panic(提示测试漏了覆写)。
type permFakeDapr struct {
	dapr.Client
	baseURL string
}

// InvokeMethodWithContent 走 httptest server,模拟 dapr sidecar 转发语义。
func (f *permFakeDapr) InvokeMethodWithContent(
	ctx context.Context, appID, method, verb string, content *dapr.DataContent,
) ([]byte, error) {
	// method 形如 "internal/users/{id}/permissions?branch_id=…"
	path := method
	q := ""
	if i := strings.Index(method, "?"); i >= 0 {
		path = method[:i]
		q = method[i+1:]
	}
	target := f.baseURL + "/v1.0/invoke/" + appID + "/method/" + path
	if q != "" {
		target += "?" + q
	}
	var body io.Reader
	if content != nil && len(content.Data) > 0 {
		body = bytes.NewReader(content.Data)
	}
	req, err := http.NewRequestWithContext(ctx, verb, target, body)
	if err != nil {
		return nil, err
	}
	if content != nil {
		req.Header.Set("Content-Type", content.ContentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		// 转成 gRPC status,跟真实 SDK 行为对齐。
		if resp.StatusCode == 404 {
			return nil, status.Error(codes.NotFound, string(data))
		}
		return nil, status.Error(codes.Internal, string(data))
	}
	return data, nil
}

// newPermEngine 装配带 fake userd 的 member handler。
func newPermEngine(t *testing.T, stub *permUserdStub, withClaims bool) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	users, err := userinfo.New("userd", userinfo.WithMockClient(&permFakeDapr{baseURL: srv.URL}))
	require.NoError(t, err)

	svc := service.New(nil)
	r := gin.New()
	r.Use(middleware.XBranchID())
	if withClaims {
		r.Use(func(c *gin.Context) {
			ctx := claims.WithClaims(c.Request.Context(), &claims.Claims{Sub: permOperatorSub})
			c.Request = c.Request.WithContext(ctx)
			c.Next()
		})
	}
	handler.New(svc, users, nil).RegisterRoutes(r)
	return r
}

// doPerm 发起一次 GET /permissions 请求。
func doPerm(t *testing.T, r *gin.Engine, branchID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/permissions", nil)
	if branchID != "" {
		req.Header.Set("X-Branch-ID", branchID)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// permBody 是 GET /permissions 的响应体(测试侧解构)。
type permBody struct {
	BranchID string   `json:"branch_id"`
	Scopes   []string `json:"scopes"`
	Warnings []string `json:"warnings"`
}

// decodePerm 解响应体。
func decodePerm(t *testing.T, w *httptest.ResponseRecorder) permBody {
	t.Helper()
	var body permBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), "body=%s", w.Body.String())
	return body
}

// TestPermissions_HappyPath 锁"返当前工号在当前门店的 scope 集合"。
func TestPermissions_HappyPath(t *testing.T) {
	stub := &permUserdStub{rows: []userinfo.BranchPermissionsRow{
		{BranchID: "10", Scopes: []string{"member:view", "pos:sale", "member:point-deduct"}},
	}}
	r := newPermEngine(t, stub, true)

	w := doPerm(t, r, "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	body := decodePerm(t, w)
	assert.Equal(t, "10", body.BranchID)
	assert.Equal(t, []string{"member:point-deduct", "member:view", "pos:sale"}, body.Scopes)
	assert.Empty(t, body.Warnings, "正常路径不该有 warnings")
	assert.Equal(t, 2, stub.callCount(), "守门 1 次 + handler 1 次")
}

// TestPermissions_ScopesAreDedupedAndSorted 锁"去重 + 排序(响应稳定)"。
func TestPermissions_ScopesAreDedupedAndSorted(t *testing.T) {
	stub := &permUserdStub{rows: []userinfo.BranchPermissionsRow{
		{BranchID: "10", Scopes: []string{"pos:sale", "member:view", "pos:sale"}},
	}}
	r := newPermEngine(t, stub, true)

	w := doPerm(t, r, "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	body := decodePerm(t, w)
	assert.Equal(t, []string{"member:view", "pos:sale"}, body.Scopes)
}

// TestPermissions_OtherBranchNotLeaked 锁"别的门店的权限不串台"。
//
// ⚠️ 矩阵里有 "10" 与 "20" 两行,查 "10" ⇒ 只返 "10" 行的 scope。
//
//	(若过滤写坏,"20" 行的 scope 会混进来 ⇒ 客户端会以为自己有别家店的权限。)
func TestPermissions_OtherBranchNotLeaked(t *testing.T) {
	stub := &permUserdStub{rows: []userinfo.BranchPermissionsRow{
		{BranchID: "10", Scopes: []string{"member:view", "member:point-deduct"}},
		{BranchID: "20", Scopes: []string{"member:view", "pos:approve"}},
	}}
	r := newPermEngine(t, stub, true)

	w := doPerm(t, r, "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	body := decodePerm(t, w)
	assert.Equal(t, "10", body.BranchID)
	assert.Equal(t, []string{"member:point-deduct", "member:view"}, body.Scopes,
		"别的门店的 scope 不能串台")
	assert.NotContains(t, body.Scopes, "pos:approve")
}

// TestPermissions_NoScopeInBranch_403 锁"该店一个 scope 都没有 ⇒ 403 forbidden"。
//
// ⚠️ **这是"没有权限"的真实表现形态** —— 不是 200 + 空数组:
//
//	守门(rbac.RequireScopeWithBranch)要求该店有 member:view,
//	没有就直接 403,根本进不到 handler。
//	⇒ 客户端拿到 403 要按"没有权限"处理(fail-closed)。
func TestPermissions_NoScopeInBranch_403(t *testing.T) {
	stub := &permUserdStub{rows: []userinfo.BranchPermissionsRow{
		{BranchID: "10", Scopes: []string{"member:view", "member:point-deduct"}},
	}}
	r := newPermEngine(t, stub, true)

	// 查 "20":矩阵里没有这一行 ⇒ 该店没绑定 ⇒ 守门直接 403。
	w := doPerm(t, r, "20")
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), `"code":"forbidden"`)
}

// TestPermissions_NoBranch_400 锁"没带 X-Branch-ID ⇒ 400 branch_required"。
func TestPermissions_NoBranch_400(t *testing.T) {
	stub := &permUserdStub{}
	r := newPermEngine(t, stub, true)

	w := doPerm(t, r, "")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), `"code":"branch_required"`)
}

// TestPermissions_NoClaims_401 锁"没带已签 token ⇒ 401"。
func TestPermissions_NoClaims_401(t *testing.T) {
	stub := &permUserdStub{}
	r := newPermEngine(t, stub, false)

	w := doPerm(t, r, "10")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), `"code":"unauthenticated"`)
}

// TestPermissions_NoScope_403 锁"没有 member:view ⇒ 403 forbidden"。
func TestPermissions_NoScope_403(t *testing.T) {
	stub := &permUserdStub{rows: []userinfo.BranchPermissionsRow{
		{BranchID: "10", Scopes: []string{"pos:sale"}},
	}}
	r := newPermEngine(t, stub, true)

	w := doPerm(t, r, "10")
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), `"code":"forbidden"`)
}

// TestPermissions_ScopesUnavailable_EmptyWithWarning 锁"查不到 ⇒ 空数组 + warnings(**不 500**)"。
//
// ⚠️ 守门那一次调用成功(第 1 次)、handler 那一次失败(第 2 次)
//
//	⇒ 才会走到"查不到"的分支。
func TestPermissions_ScopesUnavailable_EmptyWithWarning(t *testing.T) {
	stub := &permUserdStub{
		rows: []userinfo.BranchPermissionsRow{
			{BranchID: "10", Scopes: []string{"member:view"}},
		},
		failFromCall: 2,
	}
	r := newPermEngine(t, stub, true)

	w := doPerm(t, r, "10")
	require.Equal(t, http.StatusOK, w.Code, "查不到权限**不是 500**,body=%s", w.Body.String())

	body := decodePerm(t, w)
	assert.Empty(t, body.Scopes, "查不到 ⇒ 空数组")
	assert.Equal(t, []string{"scopes_unavailable"}, body.Warnings,
		"客户端要能区分'没权限'与'查不到'")
}

// TestPermissions_UserdDown_503 锁"userd 挂 ⇒ 503(不是 200 空数组)"。
//
// ⚠️ 守门那一次调用就失败 ⇒ rbac 中间件返 503 userd_unavailable。
//
//	⇒ 客户端要把 503 当"**查不到**"(不是"没有权限")。
func TestPermissions_UserdDown_503(t *testing.T) {
	stub := &permUserdStub{failFromCall: 1}
	r := newPermEngine(t, stub, true)

	w := doPerm(t, r, "10")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), `"code":"userd_unavailable"`)
}
