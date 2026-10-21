package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/YunBright/supertrade/internal/archivedto"
	"github.com/YunBright/supertrade/internal/inventory/service"
)

// fakeInvoker 替身 dapr service invocation(记录到底往 cube-router 发了什么)。
type fakeInvoker struct {
	appID  string
	method string
	verb   string
	// md 记录 outgoing metadata(JWT / 门店头)。
	md metadata.MD
	// body 是被调方的响应体。
	body []byte
	// err 是调用错误。
	err error
}

func (f *fakeInvoker) InvokeMethod(ctx context.Context, appID, methodName, verb string) ([]byte, error) {
	f.appID = appID
	f.method = methodName
	f.verb = verb
	f.md, _ = metadata.FromOutgoingContext(ctx)
	if f.err != nil {
		return nil, f.err
	}
	return f.body, nil
}

// TestCubeRouterExporter_QueryShape 锁"转发出去的 query 与头"。
//
// ⚠️ 两个头缺一不可:
//   - Authorization ⇒ cube-router 的 per-branch scope 解析(cube:read)
//   - X-Branch-ID   ⇒ cube-router 决定路由到哪个门店的 cube 实例
func TestCubeRouterExporter_QueryShape(t *testing.T) {
	inv := &fakeInvoker{body: []byte(`{"scope":"stock"}`)}
	exp := service.NewCubeRouterExporter(inv, "supertrade-cube-router")

	raw, err := exp.ExportRaw(context.Background(), service.ExportInput{
		BranchID: "00",
		Scope:    archivedto.ScopeStock,
		Offset:   10,
		Limit:    50,
		AsOf:     "2026-10-21",
		Bearer:   "Bearer token-abc",
	})
	require.NoError(t, err)
	assert.Equal(t, `{"scope":"stock"}`, string(raw))

	assert.Equal(t, "supertrade-cube-router", inv.appID)
	assert.Equal(t, "GET", inv.verb)
	// ⚠️ dapr 会把 method 里的 "?query" 拆出来放进 HTTP 扩展的 querystring
	//   ⇒ 目标 app 收到的是带 query 的 GET。
	assert.Equal(t, "v1/export?as_of=2026-10-21&limit=50&offset=10&scope=stock", inv.method)
	assert.Equal(t, []string{"Bearer token-abc"}, inv.md.Get("authorization"))
	assert.Equal(t, []string{"00"}, inv.md.Get("x-branch-id"))
}

// TestCubeRouterExporter_ErrorClassification 锁"dapr 错误 → HTTP 语义"。
func TestCubeRouterExporter_ErrorClassification(t *testing.T) {
	cases := []struct {
		name   string
		code   codes.Code
		status int
	}{
		{"权限不足", codes.PermissionDenied, 403},
		{"登录失效", codes.Unauthenticated, 401},
		{"参数非法", codes.InvalidArgument, 400},
		{"没配数据源", codes.NotFound, 404},
		{"链路故障", codes.Unavailable, 503},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := &fakeInvoker{err: status.Error(tc.code, "boom")}
			exp := service.NewCubeRouterExporter(inv, "supertrade-cube-router")

			_, err := exp.ExportRaw(context.Background(), service.ExportInput{
				BranchID: "00", Scope: archivedto.ScopeStock, Limit: 50,
			})
			require.Error(t, err)
			var ee *service.ExportError
			require.True(t, errors.As(err, &ee), "错误必须带 HTTP 语义: %v", err)
			assert.Equal(t, tc.status, ee.Status)
		})
	}
}
