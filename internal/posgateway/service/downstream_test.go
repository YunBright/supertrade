package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	dapr "github.com/dapr/go-sdk/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/YunBright/supertrade/internal/posgateway/service"
)

// fakeInvoker 是假 dapr client(只实现 DaprCaller 用到的两个方法)。
type fakeInvoker struct {
	getBody  []byte
	getErr   error
	postResp []byte
	postErr  error
	// postBody 是**捕获到的请求体**(不是响应体)。
	postBody []byte
	lastApp  string
	lastPath string
	lastVerb string
	lastCtx  context.Context
}

func (f *fakeInvoker) InvokeMethod(ctx context.Context, appID, methodName, verb string) ([]byte, error) {
	f.lastApp, f.lastPath, f.lastVerb, f.lastCtx = appID, methodName, verb, ctx
	return f.getBody, f.getErr
}

func (f *fakeInvoker) InvokeMethodWithContent(ctx context.Context, appID, methodName, verb string,
	content *dapr.DataContent) ([]byte, error) {
	f.lastApp, f.lastPath, f.lastVerb, f.lastCtx = appID, methodName, verb, ctx
	if content != nil {
		f.postBody = append([]byte(nil), content.Data...)
	}
	return f.postResp, f.postErr
}

func TestDaprCaller_Get_ForwardsBearerAndBranch(t *testing.T) {
	inv := &fakeInvoker{getBody: []byte(`{"ok":true}`)}
	caller := service.NewDaprCaller(inv)

	ctx := service.WithBearer(context.Background(), "Bearer tok-1")
	ctx = service.WithBranchID(ctx, "B10")
	out, err := caller.Get(ctx, service.AppMember, "/config")
	require.NoError(t, err)
	assert.JSONEq(t, `{"ok":true}`, string(out))

	assert.Equal(t, service.AppMember, inv.lastApp)
	assert.Equal(t, "/config", inv.lastPath)
	assert.Equal(t, "GET", inv.lastVerb)

	md, ok := metadata.FromOutgoingContext(inv.lastCtx)
	require.True(t, ok, "必须把令牌/门店放进 outgoing metadata")
	require.Len(t, md.Get("authorization"), 1)
	assert.Equal(t, "Bearer tok-1", md.Get("authorization")[0])
	require.Len(t, md.Get("x-branch-id"), 1)
	assert.Equal(t, "B10", md.Get("x-branch-id")[0])
}

func TestDaprCaller_Get_WithoutBearer_NoAuthMetadata(t *testing.T) {
	inv := &fakeInvoker{getBody: []byte(`{}`)}
	caller := service.NewDaprCaller(inv)

	_, err := caller.Get(context.Background(), service.AppMember, "/config")
	require.NoError(t, err)

	md, ok := metadata.FromOutgoingContext(inv.lastCtx)
	if ok {
		assert.Empty(t, md.Get("authorization"), "没有 caller 令牌时不该塞空头")
	}
}

func TestDaprCaller_Get_ClassifiesAuthError(t *testing.T) {
	inv := &fakeInvoker{getErr: status.Error(codes.Unauthenticated, "missing token")}
	caller := service.NewDaprCaller(inv)

	_, err := caller.Get(context.Background(), service.AppMember, "/config")
	require.Error(t, err)

	de := service.AsDownstreamError(err)
	require.NotNil(t, de)
	assert.Equal(t, http.StatusUnauthorized, de.Status)
	assert.True(t, service.IsAuthError(err), "401 必须被判成鉴权失败")
}

func TestDaprCaller_Get_ClassifiesForbidden(t *testing.T) {
	inv := &fakeInvoker{getErr: status.Error(codes.PermissionDenied, "no scope")}
	caller := service.NewDaprCaller(inv)

	_, err := caller.Get(context.Background(), service.AppMember, "/config")
	require.Error(t, err)

	de := service.AsDownstreamError(err)
	require.NotNil(t, de)
	assert.Equal(t, http.StatusForbidden, de.Status)
	assert.True(t, service.IsAuthError(err))
}

func TestDaprCaller_Get_ClassifiesNotFoundAsBusinessError(t *testing.T) {
	inv := &fakeInvoker{getErr: status.Error(codes.NotFound, "not found")}
	caller := service.NewDaprCaller(inv)

	_, err := caller.Get(context.Background(), service.AppMember, "/members/M1")
	require.Error(t, err)

	de := service.AsDownstreamError(err)
	require.NotNil(t, de)
	assert.Equal(t, http.StatusNotFound, de.Status)
	assert.False(t, service.IsAuthError(err), "404 不是鉴权失败 ⇒ 可以降级")
}

func TestDaprCaller_Get_ClassifiesUnavailableAsBusinessError(t *testing.T) {
	inv := &fakeInvoker{getErr: status.Error(codes.Unavailable, "no app id")}
	caller := service.NewDaprCaller(inv)

	_, err := caller.Get(context.Background(), service.AppMember, "/config")
	require.Error(t, err)

	de := service.AsDownstreamError(err)
	require.NotNil(t, de)
	assert.Equal(t, http.StatusServiceUnavailable, de.Status)
	assert.False(t, service.IsAuthError(err))
}

func TestDaprCaller_Post_SendsBodyVerbatim(t *testing.T) {
	inv := &fakeInvoker{postResp: []byte(`{"sale_id":"S1"}`)}
	caller := service.NewDaprCaller(inv)

	body := []byte(`{"op_id":"op-1","lines":[]}`)
	out, err := caller.Post(context.Background(), service.AppPOS, "/sales", body)
	require.NoError(t, err)
	assert.JSONEq(t, `{"sale_id":"S1"}`, string(out))
	assert.Equal(t, string(body), string(inv.postBody), "body 必须一字不改地转发")
	assert.Equal(t, "POST", inv.lastVerb)
}

func TestDaprCaller_NilClient_ReturnsError(t *testing.T) {
	caller := service.NewDaprCaller(nil)
	_, err := caller.Get(context.Background(), service.AppMember, "/config")
	require.Error(t, err)
	assert.False(t, errors.Is(err, service.ErrInvalidInput))
}

func TestAmountString_UnmarshalAcceptsStringAndNumber(t *testing.T) {
	type holder struct {
		A service.AmountString `json:"a"`
		B service.AmountString `json:"b"`
	}
	var h holder
	require.NoError(t, json.Unmarshal([]byte(`{"a":"22.20","b":22.2}`), &h))
	assert.Equal(t, "22.20", h.A.String())
	assert.Equal(t, "22.2", h.B.String())

	out, err := json.Marshal(h)
	require.NoError(t, err)
	assert.JSONEq(t, `{"a":"22.20","b":"22.2"}`, string(out), "写出来必须一律是字符串")
}
