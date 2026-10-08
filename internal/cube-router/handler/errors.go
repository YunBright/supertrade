// Package handler / errors.go —— cube 调用的错误分类。
//
// 背景(一次真实故障的完整坍缩链):
//
//	cube app 收到错误方法名 → 404
//	  → dapr sidecar 转 gRPC status code = NotFound
//	    → cubeclient 把 NotFound 一律判成 ErrCubeNotFound
//	      → stocktake 把它当成"商品不存在" ErrProductNotFound
//	        → handler return [], nil
//	          → HTTP 200 {"products": []}
//	            → Flutter 显示「本门店没有条码 6957583900828 的商品」
//
// 整条链路上每一层都"正常"返回,没有一处报错,唯一可观测的信号就是那句
// 误导性的用户文案。而根因(方法名写错)其实是可以在最外层就识别出来的。
//
// 因此这里把错误分成两类,语义互斥:
//
//   - cube_not_found(404):目标 cube 明确表示"没有这条数据"。这是业务结论,
//     可以翻译成"本店没有这个商品"。
//   - cube_unavailable(502):链路/路由/协议层故障(方法不存在、source 未注册、
//     连接被拒、超时、5xx)。这是基础设施结论,**绝不能**翻译成"商品不存在"。
//
// 两者的区别对上层是可判定的:前者可以回一句"没扫到",后者必须提示重试,
// 否则店员会以为商品没入库,进而重复盘货或手工建档。
package handler

import (
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// cubeQueryMethod 是 cube app 实际注册的查询方法。
//
// cube app 侧(语义层 binary)只注册 POST /query + GET /healthz;
// cube-gateway 侧只注册 /register /unregister /v1/source/:source/load /v1/sources /healthz,
// 且 gateway 调上游同样是 /query。
// 历史上这里写的是 "v1/load",该方法在整条链路上无人提供,稳定 404。
const cubeQueryMethod = "query"

// 错误码常量。供上层(supertrade 各业务 handler)做分支判断,避免依赖文案。
const (
	// CodeCubeNotFound 表示目标 cube 明确返回"无此数据"。
	CodeCubeNotFound = "cube_not_found"
	// CodeCubeUnavailable 表示调用链路故障,与数据是否存在无关。
	CodeCubeUnavailable = "cube_unavailable"
	// CodeCubeBadRequest 表示 cube 拒绝了查询本身(参数/schema 不合法)。
	CodeCubeBadRequest = "cube_bad_request"
)

// classifyInvokeError 把 dapr/gRPC 错误映射为 (code, HTTP status, message)。
//
// gRPC status 是这里唯一可靠的信号:dapr SDK 把 sidecar 收到的 HTTP 状态码
// 折叠进 status code,因此下游 cube 的 404/503/500 都会以 gRPC code 的形式回来。
//
// 注意返回值命名:局部变量叫 httpStatus 而不是 status,否则会遮蔽 grpc/status 包。
func classifyInvokeError(err error) (code string, httpStatus int, message string) {
	if err == nil {
		// 不该发生,但不要把 nil 悄悄当成成功。
		return CodeCubeUnavailable, http.StatusBadGateway, "nil error"
	}

	st, ok := status.FromError(err)
	if !ok {
		// 不是 gRPC 错误 → SDK 层的失败(Dapr 未就绪、序列化失败等)。
		return CodeCubeUnavailable, http.StatusBadGateway, err.Error()
	}

	switch st.Code() {
	case codes.NotFound:
		// cube 说"没有这条数据"。业务结论,允许上层翻译成"本店没有此商品"。
		return CodeCubeNotFound, http.StatusNotFound, st.Message()

	case codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange:
		// 查询本身不合法(schema 字段不存在、filters 写错),是调用方的 bug,
		// 不该退化成"查不到"。
		return CodeCubeBadRequest, http.StatusBadRequest, st.Message()

	case codes.DeadlineExceeded, codes.Unavailable, codes.Unimplemented,
		codes.ResourceExhausted, codes.Internal, codes.Unknown:
		// 连接被拒 / 超时 / 方法未实现 / sidecar 挂掉 —— 全是基础设施故障。
		return CodeCubeUnavailable, http.StatusBadGateway, st.Message()

	default:
		return CodeCubeUnavailable, http.StatusBadGateway, st.Message()
	}
}

// IsCubeDataNotFound 判断错误是否代表"cube 明确说没有这条数据"。
//
// 与 IsCubeUnavailable 互斥。业务层(如扫码查商品)必须用这两个函数二选一,
// 不要自己判断 status code —— 一旦自己判错,就会退回到"故障冒充业务结论"的老问题。
func IsCubeDataNotFound(err error) bool {
	code, _, _ := classifyInvokeError(err)
	return code == CodeCubeNotFound
}

// IsCubeUnavailable 判断错误是否为调用链路故障。
func IsCubeUnavailable(err error) bool {
	code, _, _ := classifyInvokeError(err)
	return code == CodeCubeUnavailable
}
