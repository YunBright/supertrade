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

// 非 gRPC 错误(Dapr 未就绪 / 序列化失败)同样是链路故障。
func TestClassifyCubeError_NonGRPC(t *testing.T) {
	err := classifyCubeError(errors.New("dapr sidecar not ready"))
	if !errors.Is(err, ErrCubeUnavailable) {
		t.Fatalf("非 gRPC 错误应归类为 ErrCubeUnavailable, got %v", err)
	}
}

func TestClassifyCubeError_NilIsNil(t *testing.T) {
	if err := classifyCubeError(nil); err != nil {
		t.Fatalf("nil 应返回 nil, got %v", err)
	}
}

// 三类错误必须互斥且完备:每个错误恰好命中一个谓词。
// 任何两个 sentinel 同时命中(或全不命中),上层"二选一"的判断就会失效,
// 故障又会退回到"数据不存在"的歧义里。
func TestCubeErrorClassesAreMutuallyExclusive(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"notfound", classifyCubeError(status.Error(codes.NotFound, "x")), "notfound"},
		{"unavailable", classifyCubeError(status.Error(codes.Unavailable, "x")), "unavailable"},
		{"unimplemented", classifyCubeError(status.Error(codes.Unimplemented, "x")), "unavailable"},
		{"badrequest", classifyCubeError(status.Error(codes.InvalidArgument, "x")), "badrequest"},
	}
	predicates := map[string]func(error) bool{
		"notfound":    IsCubeNotFound,
		"unavailable": IsCubeUnavailable,
		"badrequest":  func(e error) bool { return errors.Is(e, ErrCubeBadRequest) },
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits []string
			for pname, pred := range predicates {
				if pred(tc.err) {
					hits = append(hits, pname)
				}
			}
			if len(hits) != 1 {
				t.Fatalf("命中 %v,应恰好命中 1 个(err=%v)", hits, tc.err)
			}
			if hits[0] != tc.want {
				t.Fatalf("归类为 %q, want %q (err=%v)", hits[0], tc.want, tc.err)
			}
		})
	}
}

// 回归锁(2026-10-08 生产实测):默认 app-id 与默认路径必须成对正确。
//
// 原先是 app-id="cube-router" + path="query"。但 dapr / Consul 里注册的
// 名字是 supertrade-cube-router(见 deployer systemd unit),而它只注册
// POST /v1/load,没有 /query —— 于是每次调用都 404,再被误分类成"商品不存在"。
//
// 这里同时锁 app-id 与 path,避免只修一半又错一次。
func TestDefaultTargetAndPathPair(t *testing.T) {
	t.Setenv("CUBE_APP_ID", "")
	t.Setenv("CUBE_QUERY_PATH", "")

	appID := DefaultCubeAppID()
	path := DefaultCubeQueryPath()

	if appID != "supertrade-cube-router" {
		t.Errorf("默认 dapr app-id = %q, want \"supertrade-cube-router\"(deployer unit 里注册的名字)", appID)
	}
	if path != "v1/load" {
		t.Errorf("默认查询路径 = %q, want \"v1/load\"(supertrade-cube-router 只注册 /v1/load)", path)
	}
	// 这两个不是同一个服务,不能用 cube app 的路由去调 cube-router。
	if strings.EqualFold(appID, "cube-router") {
		t.Errorf("\"cube-router\" 只是代码里的约定名,dapr 里没有这个 app-id")
	}
	if path == "query" {
		t.Errorf("\"query\" 是 cube 语义层 app 的路由,不是 cube-router 的")
	}
}

func TestDefaultTargetAndPathEnvOverride(t *testing.T) {
	t.Setenv("CUBE_APP_ID", "cube-sixun-ysx-fb")
	t.Setenv("CUBE_QUERY_PATH", "query")
	if got := DefaultCubeAppID(); got != "cube-sixun-ysx-fb" {
		t.Errorf("CUBE_APP_ID 未生效: %q", got)
	}
	if got := DefaultCubeQueryPath(); got != "query" {
		t.Errorf("CUBE_QUERY_PATH 未生效: %q", got)
	}
}
