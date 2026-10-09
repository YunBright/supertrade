package cubeclient

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 回归锁:2026-10-08 之前的分类把所有 gRPC 错误都当成 "cube 404 not found",
// 于是下面这串坍缩把一次**基础设施故障**伪装成了业务结论:
//
//	cube 方法名写错 → 404 → ErrCubeNotFound → ErrProductNotFound
//	  → SearchProductsByBarcode return [], nil
//	    → HTTP 200 {"products":[]}
//	      → Flutter「本门店没有条码 6957583900828 的商品」
//
// 全程零报错、零 5xx,唯一信号是一句误导文案。
// 下面 4 个测试分别锁住这条坍缩链的每一环。
func TestClassifyCubeError_NotFoundIsDataAbsent(t *testing.T) {
	err := classifyCubeError(status.Error(codes.NotFound, "no rows"))
	if !errors.Is(err, ErrCubeNotFound) {
		t.Fatalf("NotFound 应归类为 ErrCubeNotFound, got %v", err)
	}
	if errors.Is(err, ErrCubeUnavailable) {
		t.Fatalf("NotFound 不应同时是 ErrCubeUnavailable")
	}
}

// 方法名写错(Unimplemented)是本次故障的直接成因,必须算链路故障。
func TestClassifyCubeError_UnimplementedIsUnavailable(t *testing.T) {
	err := classifyCubeError(status.Error(codes.Unimplemented, "no route"))
	if !errors.Is(err, ErrCubeUnavailable) {
		t.Fatalf("Unimplemented 应归类为 ErrCubeUnavailable, got %v", err)
	}
	// 这是最关键的一条:曾经它被误判成 not-found。
	if errors.Is(err, ErrCubeNotFound) {
		t.Fatalf("Unimplemented 绝不能被当成 '数据不存在',否则故障会被显示成 '本门店没有此商品'")
	}
}

func TestClassifyCubeError_Infrastructure(t *testing.T) {
	cases := []struct {
		name string
		code codes.Code
	}{
		{"Unavailable", codes.Unavailable},
		{"DeadlineExceeded", codes.DeadlineExceeded},
		{"Internal", codes.Internal},
		{"ResourceExhausted", codes.ResourceExhausted},
		{"Unknown", codes.Unknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyCubeError(status.Error(tc.code, "boom"))
			if !errors.Is(err, ErrCubeUnavailable) {
				t.Fatalf("%s 应归类为 ErrCubeUnavailable, got %v", tc.code, err)
			}
			if errors.Is(err, ErrCubeNotFound) {
				t.Fatalf("%s 不能被当成 '数据不存在'", tc.code)
			}
			// 2026-10-08 新增:基础设施故障不得被误判成权限/登录态问题,
			// 否则一次 cube 挂机会被显示成"你没权限"。
			if errors.Is(err, ErrCubeForbidden) || errors.Is(err, ErrCubeAuthRejected) {
				t.Fatalf("%s 是基础设施故障,不得归类为权限/登录态(403/401)", tc.code)
			}
		})
	}
}

// 查询本身不合法是调用方 bug,退化成"查不到"会掩盖 schema 漂移。
func TestClassifyCubeError_BadRequest(t *testing.T) {
	for _, c := range []codes.Code{codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange} {
		err := classifyCubeError(status.Error(c, "unknown member product.barcode"))
		if !errors.Is(err, ErrCubeBadRequest) {
			t.Fatalf("%s 应归类为 ErrCubeBadRequest, got %v", c, err)
		}
		if errors.Is(err, ErrCubeNotFound) {
			t.Fatalf("%s 不能被当成 '数据不存在'", c)
		}
	}
}

// ---------------------------------------------------------------------------
// 2026-10-08 第二次线上故障:cube-router 的 401/403 被 default 分支吞成 503。
//
// 真实链路:店员(merchant 角色,无 cube:read)盘点扫码 → stocktake 透传 caller JWT
// 调 cube-router /v1/load → rbac.RequireScopeWithBranch("cube:read") 返 **403**
// → 旧代码 default 分支 → ErrCubeUnavailable → handler 503 cube_unavailable
// → wx-h5 显示「查询失败: HTTP 503」。
//
// 危害:①用户以为是服务故障,反复重试/反复登录;②排查的人全去看 cube 健不健康,
// 而 cube 一直好好的,只是拒绝了这个人。
// ---------------------------------------------------------------------------

func TestClassifyCubeError_UnauthenticatedIsAuthNotUnavailable(t *testing.T) {
	err := classifyCubeError(status.Error(codes.Unauthenticated, "Unauthorized"))
	if !errors.Is(err, ErrCubeAuthRejected) {
		t.Fatalf("Unauthenticated 应归类为 ErrCubeAuthRejected, got %v", err)
	}
	// 反向锁:绝不能再被标成"链路不可用",否则又退回 503。
	if errors.Is(err, ErrCubeUnavailable) || IsCubeUnavailable(err) {
		t.Fatalf("401 是登录态问题,不得被标为 ErrCubeUnavailable(503)")
	}
	// 更不能被当成"商品不存在"——那会把权限问题翻译成业务结论。
	if errors.Is(err, ErrCubeNotFound) {
		t.Fatalf("401 不得被当成 not-found")
	}
}

func TestClassifyCubeError_PermissionDeniedIsForbiddenNotUnavailable(t *testing.T) {
	err := classifyCubeError(status.Error(codes.PermissionDenied, "需要 cube:read"))
	if !errors.Is(err, ErrCubeForbidden) {
		t.Fatalf("PermissionDenied 应归类为 ErrCubeForbidden, got %v", err)
	}
	if errors.Is(err, ErrCubeUnavailable) || IsCubeUnavailable(err) {
		t.Fatalf("403 是权限问题,不得被标为 ErrCubeUnavailable(503)")
	}
	if errors.Is(err, ErrCubeNotFound) {
		t.Fatalf("403 不得被当成 not-found")
	}
}

func TestClassifyCubeError_NonGRPCStaysUnavailable(t *testing.T) {
	// 防御:非 gRPC 错误不能被 status.FromError 误判成 Unauthenticated。
	// status.FromError 对普通 error 返回 ok=false,但如果哪天底层换了包装方式,
	// 这条锁能第一时间发现。
	err := classifyCubeError(errors.New("dial tcp 172.12.1.5:50001: connection refused"))
	if !errors.Is(err, ErrCubeUnavailable) {
		t.Fatalf("非 gRPC 错误应为 ErrCubeUnavailable, got %v", err)
	}
	if errors.Is(err, ErrCubeForbidden) || errors.Is(err, ErrCubeAuthRejected) {
		t.Fatalf("非 gRPC 错误不得归类为权限/登录态")
	}
}

func TestClassifyCubeError_NilPassthrough(t *testing.T) {
	if classifyCubeError(nil) != nil {
		t.Fatalf("nil 输入必须返回 nil")
	}
}

func TestClassifyCubeError_MessageKeepsDownstreamDetail(t *testing.T) {
	// 排查时要能看到 cube 说了什么(哪个 scope 缺),所以原 message 必须保留。
	err := classifyCubeError(status.Error(codes.PermissionDenied, "用户在该 branch 下无 cube:read scope"))
	if !strings.Contains(err.Error(), "cube:read") {
		t.Fatalf("错误信息必须保留下游原文, got %v", err)
	}
}
