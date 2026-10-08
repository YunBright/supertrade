package cubeclient

import (
	"errors"
	"fmt"
	"os"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 默认的 cube 调用目标与查询路径。
//
// 这两个值必须成对理解 —— 它们描述的是**同一个 dapr app** 的地址,不是两个可独立
// 猜测的默认值:
//
//   - DefaultCubeAppID = supertrade-cube-router
//   - DefaultCubeQueryPath = v1/load
//
// 历史上默认分别是 "cube-router" 和 "query",两个都不是任何服务的真实身份:
// dapr / Consul 里注册的名字是 supertrade-cube-router(见 deployer 的 systemd unit),
// 而 supertrade-cube-router 只注册 POST /v1/load,压根没有 /query 路由
// (query 是 cube 语义层 app 的路由)。两者一错,调用稳定 404。
//
// 选 cube-router 作为默认,是因为只有它读 branch_cube_sources 做 per-branch 路由;
// 直连某个 cube app 会绕过分门店隔离,把全部门店的数据混在一起返回。
const (
	// DefaultCubeAppID 是生产实际注册的 dapr app-id。
	DefaultCubeAppIDName = "supertrade-cube-router"
	// DefaultCubeQueryPath 是 supertrade-cube-router 上的查询方法路径。
	DefaultCubeQueryPathName = "v1/load"
)

// DefaultCubeAppID 返回本次进程实际使用的 dapr app-id(CUBE_APP_ID 覆盖)。
func DefaultCubeAppID() string {
	if v := os.Getenv("CUBE_APP_ID"); v != "" {
		return v
	}
	return DefaultCubeAppIDName
}

// DefaultCubeQueryPath 返回本次进程实际使用的查询路径(CUBE_QUERY_PATH 覆盖)。
func DefaultCubeQueryPath() string {
	if v := os.Getenv("CUBE_QUERY_PATH"); v != "" {
		return v
	}
	return DefaultCubeQueryPathName
}

// classifyCubeError 把 dapr/gRPC 错误映射成三档互斥语义。
//
// 这一层是整个链路上最关键的错误边界。上游(StocktakeHandler.SearchProducts)会
// 把 ErrCubeNotFound / ErrProductNotFound 翻译成"本店没有这个商品"(HTTP 200 + 空数组),
// 而其它错误一律往上抛成 5xx。因此**这里判错一次,故障就会被伪装成业务结论**,
// 而且伪装得毫无痕迹:没有堆栈、没有 5xx、没有日志,只有一句误导性的文案。
//
// 判据是 gRPC status code —— dapr SDK 把 sidecar 收到的 HTTP 状态码
// 折叠进了 status code,所以下游 cube 的 404 / 503 / 500 在这里都以 gRPC code 呈现。
func classifyCubeError(err error) error {
	if err == nil {
		return nil
	}

	st, ok := status.FromError(err)
	if !ok {
		// 不是 gRPC 错误 → SDK 层失败(Dapr 未就绪、序列化失败等),属于链路故障。
		return fmt.Errorf("%w: %v", ErrCubeUnavailable, err)
	}

	switch st.Code() {
	case codes.NotFound:
		// cube 明确说"没有这条数据"。业务结论,可以交给上层翻译成"没有此商品"。
		return fmt.Errorf("%w: %s", ErrCubeNotFound, st.Message())

	case codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange:
		// 查询本身不合法。调用方 bug,不该退化成"查不到"。
		return fmt.Errorf("%w: %s", ErrCubeBadRequest, st.Message())

	default:
		// Unimplemented(方法名错) / Unavailable(连不上) / DeadlineExceeded(超时)
		// / ResourceExhausted / Internal / 5xx —— 全部是基础设施故障。
		return fmt.Errorf("%w: %s: %s", ErrCubeUnavailable, st.Code(), st.Message())
	}
}

// IsCubeUnavailable 判断错误是否属于调用链路故障(而非"数据不存在")。
// 业务层需要在把错误暴露给终端用户之前先问这一句。
func IsCubeUnavailable(err error) bool {
	return errors.Is(err, ErrCubeUnavailable)
}

// IsCubeNotFound 判断错误是否代表"cube 明确说没有这条数据"。
func IsCubeNotFound(err error) bool {
	return errors.Is(err, ErrCubeNotFound)
}
