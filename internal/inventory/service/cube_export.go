// Package service / cube_export.go —— 把归档导出请求**转发**给 cube-router。
//
// # 为什么是转发而不是自己查 cube
//
// 门店 → cube 实例的映射只有 cube-router 知道（branch_cube_sources），
// 而"某个 scope 怎么拼 cube 查询、怎么把 cube 行整形成契约行"也只有它有一份实现
// ⇒ 本服务再写一份的后果是"同一个 scope 在两条路上导出不同的列"。
//
// # 调用链
//
//	inventory → dapr(service invocation) → supertrade-cube-router
//	                                        → GET /v1/export?scope=stock
//	                                        → branch_cube_sources 路由
//	                                        → 该门店专属 cube 实例
//
// # ⚠️ JWT 与门店头必须一起透传
//
// cube-router 的 /v1/export 挂 `cube:read` scope（**per-branch 实时解析**），
// 门店从 X-Branch-ID header 取 ⇒ 两个头缺一个就 401 / 400，
// 而 dapr 会把 400 包成 gRPC 错误 ⇒ 现象是"导出失败"，看不出是少了头。
package service

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// cubeRouterExportPath 是 cube-router 上的导出端点路径。
const cubeRouterExportPath = "v1/export"

// exportInvokeTimeout 是导出单次调用的超时。
//
// ⚠️ 比普通业务查询长：导出页最大 10000 行，比业务查询重得多；
// 但也不能无限长（sidecar 那边也有默认超时）。
const exportInvokeTimeout = 120 * time.Second

// daprInvoker 是转发导出请求所需的**全部** dapr 依赖面。
//
// ⚠️ 收窄的理由同 internal/cubeclient：dapr.Client 有几十个方法，
// 单测想断言"到底往 cube-router 发了什么 query"就得手写整个接口 ——
// 成本高到没人愿意写，于是拼错的 scope / 分页参数只能靠肉眼 review 保护。
type daprInvoker interface {
	InvokeMethod(ctx context.Context, appID, methodName, verb string) ([]byte, error)
}

// CubeRouterExporter 把导出请求转发给 cube-router 的 /v1/export。
type CubeRouterExporter struct {
	cli   daprInvoker
	appID string
}

// NewCubeRouterExporter 构造转发器。
//
// appID 是 cube-router 的 dapr app-id（默认 supertrade-cube-router）。
func NewCubeRouterExporter(cli daprInvoker, appID string) *CubeRouterExporter {
	if appID == "" {
		appID = "supertrade-cube-router"
	}
	return &CubeRouterExporter{cli: cli, appID: appID}
}

// ExportRaw 转发一次导出请求，**原样**返回被调方的响应体。
func (e *CubeRouterExporter) ExportRaw(ctx context.Context, in ExportInput) ([]byte, error) {
	if e == nil || e.cli == nil {
		return nil, fmt.Errorf("inventory: 归档导出转发器未装配")
	}
	q := url.Values{}
	q.Set("scope", in.Scope)
	q.Set("offset", strconv.Itoa(in.Offset))
	q.Set("limit", strconv.Itoa(in.Limit))
	if asOf := in.AsOf; asOf != "" {
		q.Set("as_of", asOf)
	}
	// ⚠️ since = 增量水位 ⇒ 透传给 cube-router ⇒ 它给 cube 查询加
	//   `stock.updated_at gt <since>` ⇒ 只导"变过"的行。
	if since := in.Since; since != "" {
		q.Set("since", since)
	}
	// ⚠️ dapr 的 gRPC 调用把 method 里的 "?query" 拆出来放进 HTTP 扩展的 querystring
	//   （go-sdk 的 extractMethodAndQuery）⇒ 目标 app 收到的是带 query 的 GET。
	method := cubeRouterExportPath + "?" + q.Encode()

	// ⚠️ **JWT 与门店头一起透传**：cube-router 的 scope 守门要靠前者，
	//   门店路由要靠后者。
	if bearer := in.Bearer; bearer != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", bearer)
	}
	if branchID := in.BranchID; branchID != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-branch-id", branchID)
	}
	ctx, cancel := context.WithTimeout(ctx, exportInvokeTimeout)
	defer cancel()

	raw, err := e.cli.InvokeMethod(ctx, e.appID, method, "GET")
	if err != nil {
		return nil, classifyExportError(err)
	}
	return raw, nil
}

// ExportError 是导出链路的错误（带 HTTP 语义 + code）。
//
// ⚠️ 自己带 code/status（而不是包一个通用 error）：
//
//	被调方的 403（没 cube:read）与 404（没配数据源）必须原样透出，
//	否则调用方只会看到一句"导出失败"，分不清是权限问题还是数据问题。
type ExportError struct {
	// Status 是 HTTP 状态码。
	Status int
	// Code 是错误码（如 cube_source_not_configured）。
	Code string
	// Message 是错误文案。
	Message string
}

// Error 实现 error。
func (e *ExportError) Error() string { return e.Message }

// classifyExportError 把 dapr / gRPC 错误映射成 HTTP 语义。
//
// ⚠️ dapr 会把目标 app 的 HTTP 状态码翻成 gRPC status code
//
//	（404 → NotFound、403 → PermissionDenied、400 → InvalidArgument …），
//	而**链路故障**是 Unavailable / DeadlineExceeded ⇒ 503。
func classifyExportError(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return &ExportError{Status: 503, Code: "cube_unavailable", Message: "数据源暂不可用，请稍后重试"}
	}
	switch st.Code() {
	case codes.PermissionDenied:
		return &ExportError{Status: 403, Code: "forbidden", Message: "没有该门店的数据读权限"}
	case codes.Unauthenticated:
		return &ExportError{Status: 401, Code: "unauthorized", Message: "登录已失效，请重新登录"}
	case codes.InvalidArgument:
		return &ExportError{Status: 400, Code: "bad_request", Message: st.Message()}
	case codes.NotFound:
		return &ExportError{Status: 404, Code: "cube_source_not_configured", Message: st.Message()}
	default:
		return &ExportError{Status: 503, Code: "cube_unavailable", Message: "数据源暂不可用，请稍后重试"}
	}
}
