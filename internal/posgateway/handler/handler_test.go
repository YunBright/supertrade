package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YunBright/authkit/claims"
	"github.com/YunBright/authkit/rbac"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/posgateway/handler"
	"github.com/YunBright/supertrade/internal/posgateway/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// fakeCaller 是假下游(按 "appID path" 匹配;带 query 的再去掉 query 匹配一次)。
type fakeCaller struct {
	responses map[string]fakeResp
	lastBody  []byte
}

type fakeResp struct {
	body json.RawMessage
	err  error
}

func newFakeCaller() *fakeCaller {
	return &fakeCaller{responses: map[string]fakeResp{}}
}

func (f *fakeCaller) stub(key, body string) *fakeCaller {
	f.responses[key] = fakeResp{body: json.RawMessage(body)}
	return f
}

func (f *fakeCaller) stubErr(key string, err error) *fakeCaller {
	f.responses[key] = fakeResp{err: err}
	return f
}

func (f *fakeCaller) lookup(appID, path string) (fakeResp, bool) {
	if r, ok := f.responses[appID+" "+path]; ok {
		return r, true
	}
	base := appID + " " + path
	if i := strings.IndexByte(path, '?'); i >= 0 {
		base = appID + " " + path[:i]
	}
	r, ok := f.responses[base]
	return r, ok
}

func (f *fakeCaller) Get(_ context.Context, appID, path string) (json.RawMessage, error) {
	r, ok := f.lookup(appID, path)
	if !ok {
		return nil, &service.DownstreamError{
			AppID: appID, Path: path, Status: http.StatusNotFound,
			Code: "not_found", Message: "fake: 没注册这个桩", Err: errors.New("no stub"),
		}
	}
	return r.body, r.err
}

func (f *fakeCaller) Post(_ context.Context, appID, path string, body []byte) (json.RawMessage, error) {
	f.lastBody = append([]byte(nil), body...)
	r, ok := f.lookup(appID, path)
	if !ok {
		return nil, &service.DownstreamError{
			AppID: appID, Path: path, Status: http.StatusNotFound,
			Code: "not_found", Message: "fake: 没注册这个桩", Err: errors.New("no stub"),
		}
	}
	return r.body, r.err
}

// happyCaller 是一组全部正常的桩。
func happyCaller() *fakeCaller {
	c := newFakeCaller()
	c.stub(service.AppMasterData+" /stores",
		`{"stores":[{"branch_id":"B10","code":"S10","name":"云商超市","pos_enabled":true,
		  "member_enabled":true,"counter_nos":"01"}],"count":1}`)
	c.stub(service.AppMasterData+" /shifts",
		`{"shifts":[{"id":"SH1","branch_id":"B10","employee_id":"u-1","counter_no":"01",
		  "opened_at":"2026-10-21T09:30:00+08:00","status":"open"}],"count":1}`)
	c.stub(service.AppMasterData+" /employees",
		`{"employees":[{"id":"u-1","employee_code":"1002","name":"临时","role":"cashier",
		  "status":"active","counter_no":"01","discount_rate":"0.9500"}],"count":1}`)
	c.stub(service.AppMember+" /config",
		`{"branch_id":"B10","membership_enabled":true,"points_rate":"1.0000"}`)
	c.stub(service.AppMember+" /payment-ways",
		`{"payment_ways":[{"code":"RMB","name":"现金","rate":"1.0000","sort_order":1}],"count":1}`)
	c.stub(service.AppMember+" /types", `{"member_types":[],"count":0}`)
	c.stub(service.AppMember+" /members/M1", `{"id":"M1","branch_id":"B10","name":"张三"}`)
	c.stub(service.AppMember+" /members/M1/last-order",
		`{"member_id":"M1","last_order":null,"sale_lines":[],"payments":[]}`)
	c.stub(service.AppCatalog+" /products/local",
		`{"products":[{"id":"P1","name":"可乐","barcode":"690","sub_code":"KL"}],"count":1,"total":1}`)
	c.stub(service.AppPricing+" /prices/local",
		`{"branch_id":"B10","as_of":"2026-10-21","prices":[{"product_id":"P1","price_yuan":"3.5000"}],"count":1}`)
	c.stub(service.AppPricing+" /promotions/local",
		`{"branch_id":"B10","as_of":"2026-10-21","promotions":[],"count":0}`)
	c.stub(service.AppPOS+" /sales", `{"sale_id":"S1","replayed":false}`)
	return c
}

// newEngine 装一个带鉴权上下文与门店上下文的 gin engine。
func newEngine(svc *service.Service, resolver rbac.ScopeResolver) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.XBranchID())
	r.Use(func(c *gin.Context) {
		ctx := claims.WithClaims(c.Request.Context(), &claims.Claims{Sub: "u-1"})
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	handler.NewWithResolver(svc, resolver, nil).RegisterRoutes(r)
	return r
}

// allowAll 是一个"什么 scope 都放行"的解析器。
func allowAll(_ *gin.Context, _, _, _ string) (bool, error) { return true, nil }

// denyAll 是一个"什么 scope 都拒绝"的解析器。
func denyAll(_ *gin.Context, _, _, _ string) (bool, error) { return false, nil }

// do 发一个带门店头的请求。
func do(t *testing.T, r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("X-Branch-ID", "B10")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestHandler_RouteTable_AllEndpointsRegistered 守住"6 个端点都在路由表里"。
//
// ⚠️ 判据是"不是 404"(路由缺失)且"不是 403"(scope 解析器放行)——
//
//	两者任一出现都说明路由表缺了一条。
func TestHandler_RouteTable_AllEndpointsRegistered(t *testing.T) {
	svc := service.New(happyCaller(), nil)
	r := newEngine(svc, allowAll)

	cases := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/home", ""},
		{http.MethodGet, "/config", ""},
		{http.MethodGet, "/members/M1/last-order", ""},
		{http.MethodGet, "/products/search?q=abc", ""},
		{http.MethodGet, "/prices?product_ids=P1", ""},
		{http.MethodPost, "/sales", `{"op_id":"op-1"}`},
	}
	for _, tc := range cases {
		w := do(t, r, tc.method, tc.path, tc.body)
		assert.NotEqual(t, http.StatusNotFound, w.Code, "%s %s 不该 404(路由没注册)", tc.method, tc.path)
		assert.NotEqual(t, http.StatusForbidden, w.Code, "%s %s 不该 403(解析器放行)", tc.method, tc.path)
		assert.Equal(t, http.StatusOK, w.Code, "%s %s 应该 200", tc.method, tc.path)
	}
}

func TestHandler_ScopeMissing_Returns403(t *testing.T) {
	svc := service.New(happyCaller(), nil)
	r := newEngine(svc, denyAll)

	w := do(t, r, http.MethodGet, "/home", "")
	require.Equal(t, http.StatusForbidden, w.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "forbidden", body["code"])
	assert.NotEmpty(t, body["message"])
}

func TestHandler_MissingBranchHeader_Returns400(t *testing.T) {
	svc := service.New(happyCaller(), nil)
	r := newEngine(svc, allowAll)

	req := httptest.NewRequest(http.MethodGet, "/home", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "branch_required", body["code"])
}

func TestHandler_ErrorEnvelope_Shape(t *testing.T) {
	// 门店档案取不到 ⇒ 404 store_not_found。
	caller := happyCaller()
	caller.stub(service.AppMasterData+" /stores", `{"stores":[],"count":0}`)
	svc := service.New(caller, nil)
	r := newEngine(svc, allowAll)

	w := do(t, r, http.MethodGet, "/home", "")
	require.Equal(t, http.StatusNotFound, w.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Len(t, body, 2, "错误信封只有 code / message 两个字段")
	assert.Contains(t, body, "code")
	assert.Contains(t, body, "message")
}

func TestHandler_DownstreamAuthError_Returns403(t *testing.T) {
	caller := happyCaller()
	caller.stubErr(service.AppMember+" /config", &service.DownstreamError{
		AppID: service.AppMember, Path: "/config", Status: http.StatusForbidden,
		Code: "forbidden", Message: "下游拒绝:该门店下没有所需权限", Err: errors.New("forbidden"),
	})
	svc := service.New(caller, nil)
	r := newEngine(svc, allowAll)

	w := do(t, r, http.MethodGet, "/home", "")
	require.Equal(t, http.StatusForbidden, w.Code, "下游 403 必须整体失败,不能降级成 200")

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "forbidden", body["code"])
}

func TestHandler_Home_ResponseShape(t *testing.T) {
	svc := service.New(happyCaller(), nil)
	r := newEngine(svc, allowAll)

	w := do(t, r, http.MethodGet, "/home", "")
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	for _, key := range []string{"store", "shift", "operator", "member_enabled", "payment_ways", "warnings"} {
		assert.Contains(t, body, key)
	}
	store, ok := body["store"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "B10", store["id"])
	assert.Equal(t, "云商超市", store["display_name"])
}

func TestHandler_PostSales_ForwardsOpIDVerbatim(t *testing.T) {
	caller := happyCaller()
	svc := service.New(caller, nil)
	r := newEngine(svc, allowAll)

	body := `{"op_id":"op-uuid-1","lines":[],"payments":[]}`
	w := do(t, r, http.MethodPost, "/sales", body)
	require.Equal(t, http.StatusOK, w.Code)

	var forwarded map[string]any
	require.NoError(t, json.Unmarshal(caller.lastBody, &forwarded))
	assert.Equal(t, "op-uuid-1", forwarded["op_id"], "op_id 必须原样透传(离线重传靠它幂等)")
}
