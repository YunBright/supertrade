// 本文件覆盖 2026-10-08 线上故障的根因:userd 拒绝调用者 token 时,stocktake
// 必须返回 401 而不是 503「userd 不可用」。
//
// 背景:本服务 sidecar **没有** middleware.http.bearer,而
// authkit/claims.GinMiddleware 明确"不验签、只解析 payload" —— 所以 userd 是
// 这条链路上唯一的验签点。access_token 过期时 stocktake 照常放行,把过期 token
// 转发给 userd,userd 返 401。旧代码把任何 userd 错误一律包成
// ErrUserInfoUnavailable(503),于是一个"重新登录就能解决"的问题被伪装成
// "依赖服务故障",排查时所有人都会去看 userd 健不健康。
//
// 下面每条用例锁的是**具体 sentinel error**,不是"返回了某个 error"。
// 将来有人把 ErrCallerTokenRejected 换成别的包装,测试必须立刻红。
package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/YunBright/authkit/userinfo"
	"github.com/YunBright/supertrade/internal/cubeclient/cubeclientfake"
	"github.com/YunBright/supertrade/internal/stocktake/model"
	"github.com/YunBright/supertrade/internal/stocktake/service"
	"github.com/YunBright/supertrade/internal/stocktake/testdb"
	dapr "github.com/dapr/go-sdk/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// errDaprClient 让所有 dapr service invocation 都返回固定错误的假 client。
//
// 嵌 dapr.Client(nil) + 只覆写 InvokeMethodWithContent —— 与 handler_test.go 的
// fakeDaprForUserd 同一套路:userinfo 只用到这一个方法。
type errDaprClient struct {
	dapr.Client
	err error
}

func (c *errDaprClient) InvokeMethodWithContent(
	_ context.Context, _, _ string, _ string, _ *dapr.DataContent,
) ([]byte, error) {
	return nil, c.err
}

// setupWithErrUserd 起一个注入了"永远返回 err"的 userinfo client 的 service。
func setupWithErrUserd(t *testing.T, invokeErr error) *service.Service {
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
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// 包一层,模拟 userinfo.Get* 真实的 fmt.Errorf("%w") 包装
	// (userinfo 内部就是这么包的,service 必须能穿透它识别 gRPC code)。
	ui, err := userinfo.New("userd", userinfo.WithMockClient(&errDaprClient{err: invokeErr}))
	if err != nil {
		t.Fatalf("userinfo.New: %v", err)
	}
	svc := service.New(db, cubeclientfake.New())
	svc.SetUserInfo(ui)
	return svc
}

const (
	testUserID  = "8dd9b3ce-e94a-4828-9cdb-2f2d10c8acce"
	testBranchB = "00"
)

func TestGetEffectiveScopes_TokenRejectedByUserd_IsNotUserdUnavailable(t *testing.T) {
	// userd 端 middleware.http.bearer 拒收 → dapr 映射成 gRPC Unauthenticated。
	// 这正是"access_token 过期"在服务调用链上的真实表现。
	svc := setupWithErrUserd(t, status.Error(codes.Unauthenticated, "Unauthorized"))

	_, err := svc.GetEffectiveScopes(context.Background(), testUserID, testBranchB)

	if !errors.Is(err, service.ErrCallerTokenRejected) {
		t.Fatalf("应返回 ErrCallerTokenRejected,实际: %v", err)
	}
	// 反向锁:绝不能再被误报成"userd 不可用",否则前端又会去查服务健康度。
	if errors.Is(err, service.ErrUserInfoUnavailable) {
		t.Fatalf("401 不得被包装成 ErrUserInfoUnavailable(会让 401 伪装成 503)")
	}
}

func TestGetEffectiveScopes_TransportFailure_StaysUnavailable(t *testing.T) {
	// 真·调不通:超时 / sidecar 挂了 / Consul 解析不到 —— 这些仍然该是 503。
	for _, c := range []codes.Code{codes.Unavailable, codes.DeadlineExceeded, codes.PermissionDenied} {
		svc := setupWithErrUserd(t, status.Error(c, "boom"))

		_, err := svc.GetEffectiveScopes(context.Background(), testUserID, testBranchB)

		if !errors.Is(err, service.ErrUserInfoUnavailable) {
			t.Fatalf("code=%s 应返回 ErrUserInfoUnavailable,实际: %v", c, err)
		}
		if errors.Is(err, service.ErrCallerTokenRejected) {
			t.Fatalf("code=%s 是链路故障,不得误判为登录态失效(401)", c)
		}
	}
}

func TestGetEffectiveScopesMulti_TokenRejected_IsNotUserdUnavailable(t *testing.T) {
	// 多店 union 版本走的是另一段错误处理,必须同样分开 —— 历史上这类"复制一份
	// 改一份"的分叉最容易漏。
	svc := setupWithErrUserd(t, status.Error(codes.Unauthenticated, "Unauthorized"))

	_, err := svc.GetEffectiveScopesMulti(context.Background(), testUserID, []string{"00", "01"})

	if !errors.Is(err, service.ErrCallerTokenRejected) {
		t.Fatalf("应返回 ErrCallerTokenRejected,实际: %v", err)
	}
	if errors.Is(err, service.ErrUserInfoUnavailable) {
		t.Fatalf("401 不得被包装成 ErrUserInfoUnavailable")
	}
}

func TestHasEffectiveScope_TokenRejected_PropagatesErrCallerTokenRejected(t *testing.T) {
	// handler 的 requireScope 靠 HasEffectiveScope 的 error 决定状态码,
	// 所以便捷判定层不能把错误吞掉变成 (false, nil) —— 那会变成 403 forbidden,
	// 前端会提示"没有权限",而真实原因是"登录过期了,重新登录就有权限"。
	svc := setupWithErrUserd(t, status.Error(codes.Unauthenticated, "Unauthorized"))

	ok, err := svc.HasEffectiveScope(context.Background(), testUserID, testBranchB, "inventory:view")

	if ok {
		t.Fatalf("token 被拒时不得判定为有权限")
	}
	if !errors.Is(err, service.ErrCallerTokenRejected) {
		t.Fatalf("应上抛 ErrCallerTokenRejected,实际: %v", err)
	}
}

func TestGetEffectiveScopes_NonGRPCError_StaysUnavailable(t *testing.T) {
	// 防御:非 gRPC 错误(比如 client 根本没建起来)不能被 status.FromError
	// 误判成 Unauthenticated —— status.FromError 对普通 error 会返回 ok=false,
	// 但如果哪天底层换了包装方式,这条锁能第一时间发现。
	svc := setupWithErrUserd(t, errors.New("dial tcp 172.12.1.1:50001: connection refused"))

	_, err := svc.GetEffectiveScopes(context.Background(), testUserID, testBranchB)

	if !errors.Is(err, service.ErrUserInfoUnavailable) {
		t.Fatalf("非 gRPC 错误应返回 ErrUserInfoUnavailable,实际: %v", err)
	}
}

var _ grpc.ClientConnInterface = (grpc.ClientConnInterface)(nil)
