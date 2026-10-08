package handler

import (
	"errors"
	"net/http"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 回归锁:cube-router 曾把请求转发到 v1/load,而 cube app 只注册 POST /query。
// 该 404 再经 cubeclient 一律判成 "商品不存在",最终在盘点 App 上显示为
// 「本门店没有条码 6957583900828 的商品」—— 现场完全看不出是链路故障。
//
// 注意方向:cube-router → cube app 用 /query;而 supertrade → cube-router 用 /v1/load。
// 两段路径不同,不可互相套用。参见 internal/cubeclient/errors.go 的对照表。
func TestCubeQueryMethodIsQuery(t *testing.T) {
	if cubeQueryMethod != "query" {
		t.Fatalf("cubeQueryMethod = %q, want \"query\"(cube app 未注册 v1/load)", cubeQueryMethod)
	}
}

// 路由层必须把"cube 说没有"与"路由/链路坏了"分成两种状态码。
// 前者是业务结论(404),后者是基础设施故障(502),二者语义互斥。
func TestClassifyInvokeError_NotFoundIs404(t *testing.T) {
	code, httpStatus, _ := classifyInvokeError(status.Error(codes.NotFound, "no rows"))
	if code != CodeCubeNotFound {
		t.Errorf("code = %q, want %q", code, CodeCubeNotFound)
	}
	if httpStatus != http.StatusNotFound {
		t.Errorf("status = %d, want 404", httpStatus)
	}
}

func TestClassifyInvokeError_UnimplementedIs502(t *testing.T) {
	// 这是本次故障的直接成因:方法名写错 → Unimplemented。
	// 若被当成 404,上层会继续翻译成"商品不存在"。
	for _, c := range []codes.Code{
		codes.Unimplemented,
		codes.Unavailable,
		codes.DeadlineExceeded,
		codes.Internal,
		codes.Unknown,
	} {
		code, httpStatus, _ := classifyInvokeError(status.Error(c, "boom"))
		if code != CodeCubeUnavailable {
			t.Errorf("%s → code = %q, want %q", c, code, CodeCubeUnavailable)
		}
		if httpStatus != http.StatusBadGateway {
			t.Errorf("%s → status = %d, want 502", c, httpStatus)
		}
		if code == CodeCubeNotFound {
			t.Errorf("%s 绝不能被分类成 '数据不存在'", c)
		}
	}
}

func TestClassifyInvokeError_BadRequestIs400(t *testing.T) {
	for _, c := range []codes.Code{codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange} {
		code, httpStatus, _ := classifyInvokeError(status.Error(c, "bad member"))
		if code != CodeCubeBadRequest || httpStatus != http.StatusBadRequest {
			t.Errorf("%s → (%q, %d), want (%q, 400)", c, code, httpStatus, CodeCubeBadRequest)
		}
	}
}

// 非 gRPC 错误(Dapr 未就绪)也要算链路故障。
func TestClassifyInvokeError_NonGRPC(t *testing.T) {
	code, httpStatus, _ := classifyInvokeError(errors.New("sidecar down"))
	if code != CodeCubeUnavailable || httpStatus != http.StatusBadGateway {
		t.Errorf("非 gRPC 错误 → (%q, %d), want (%q, 502)", code, httpStatus, CodeCubeUnavailable)
	}
}

func TestClassifyInvokeError_Nil(t *testing.T) {
	code, httpStatus, _ := classifyInvokeError(nil)
	if code != CodeCubeUnavailable || httpStatus != http.StatusBadGateway {
		t.Errorf("nil → (%q, %d), want (%q, 502) —— nil 不能被当成成功", code, httpStatus, CodeCubeUnavailable)
	}
}

// 两个谓词必须互斥:上层要靠它们二选一决定"提示未扫到"还是"提示查询失败"。
func TestErrorPredicatesAreMutuallyExclusive(t *testing.T) {
	nfErr := status.Error(codes.NotFound, "x")
	unErr := status.Error(codes.Unavailable, "x")

	if !IsCubeDataNotFound(nfErr) || IsCubeUnavailable(nfErr) {
		t.Errorf("not-found 谓词判定错误: nf=%v un=%v", IsCubeDataNotFound(nfErr), IsCubeUnavailable(nfErr))
	}
	if !IsCubeUnavailable(unErr) || IsCubeDataNotFound(unErr) {
		t.Errorf("unavailable 谓词判定错误: nf=%v un=%v", IsCubeDataNotFound(unErr), IsCubeUnavailable(unErr))
	}
}
