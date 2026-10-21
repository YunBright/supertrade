// Package service / export_client.go —— 上游**导出端点**的客户端。
//
// # 它调的是谁
//
// cube-router 的 `GET /v1/export?scope=&offset=&limit=` ——
// 它按 X-Branch-ID 路由到该门店的 cube 实例,输出 archivedto 契约。
//
// ⚠️ 走 dapr 服务调用(不是手拼 sidecar URL):本仓的纪律是
// 「跨 dapr app 调用一律走 github.com/dapr/go-sdk/client」。
//
// # 为什么 catalog 需要它
//
// catalog 现在**自己**也是一个导出方(没配数据源的门店从本域 PG 导出),
// 而配了数据源的门店要把上游(cube-router)的导出**原样**转发出去 ——
// 字段名必须与上游**逐字一致**(见 internal/archivedto)。
//
// ⚠️ JWT / X-Branch-ID 必须透传:上游有 per-branch scope 守门(cube:read),
// 少了 Authorization 就是 403 —— 而症状看起来像"这个门店没权限导档案"。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/YunBright/supertrade/internal/cubeclient"
)

// exportInvokeTimeout 是导出单次调用的超时。
//
// ⚠️ 比业务查询长:一页 5000 行,比 /v1/load 重得多。
const exportInvokeTimeout = 120 * time.Second

// daprInvoker 是 dapr 客户端的**最小能力面**(只取服务调用)。
//
// ⚠️ 收窄依赖面:单测要能断言"到底往上游发了什么"就得手写整个 dapr.Client
// (几十个方法)⇒ 没人愿意写 ⇒ 只能靠肉眼 review 保护。
type daprInvoker interface {
	InvokeMethod(ctx context.Context, appID, methodName, verb string) ([]byte, error)
}

// DaprArchiveExporter 经 dapr 服务调用调上游的导出端点。
type DaprArchiveExporter struct {
	dapr    daprInvoker
	appID   string
	timeout time.Duration
}

// NewDaprArchiveExporter 构造。
//
// appID 默认取 cubeclient.DefaultCubeAppID()(生产是 supertrade-cube-router);
// ⚠️ 它与 /v1/load 是**同一个 app** ⇒ 装配时用同一个对象,不要建两个。
func NewDaprArchiveExporter(d daprInvoker, appID string) *DaprArchiveExporter {
	if appID == "" {
		appID = "supertrade-cube-router"
	}
	return &DaprArchiveExporter{dapr: d, appID: appID, timeout: exportInvokeTimeout}
}

var _ ArchiveExporter = (*DaprArchiveExporter)(nil)

// Export 取某个 scope 的一页。
func (e *DaprArchiveExporter) Export(ctx context.Context, bearer, branchID, scope string, offset, limit int) (*ExportPage, error) {
	if e == nil || e.dapr == nil {
		return nil, ErrCubeUnavailable
	}
	q := url.Values{}
	q.Set("scope", scope)
	q.Set("offset", strconv.Itoa(offset))
	q.Set("limit", strconv.Itoa(limit))
	// ⚠️ query 拼在 method 里:dapr SDK 会把它拆成 method + querystring
	// (见 go-sdk 的 extractMethodAndQuery)。
	method := "v1/export?" + q.Encode()

	if bearer != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", bearer)
	}
	if branchID != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-branch-id", branchID)
	}
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	raw, err := e.dapr.InvokeMethod(ctx, e.appID, method, "GET")
	if err != nil {
		return nil, classifyExportError(err)
	}
	var page ExportPage
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, fmt.Errorf("catalog: 解析上游导出响应失败(scope=%s): %w", scope, err)
	}
	return &page, nil
}

// classifyExportError 把 dapr/gRPC 错误映射成与 cube 查询**同一套**语义。
//
// ⚠️ 判据是 gRPC status code —— dapr SDK 把 sidecar 收到的 HTTP 状态码
// 折叠进了 status code,所以上游的 403 / 404 / 5xx 在这里都以 gRPC code 呈现。
//
// ⚠️ 403 与 503 必须分开:403 = "这个调用者没有该门店的读权限",
// 503 = "上游挂了"。混在一起 ⇒ 用户会去查上游健康度,而真正的原因是权限。
func classifyExportError(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("%w: %v", ErrCubeUnavailable, err)
	}
	switch st.Code() {
	case codes.Unauthenticated:
		return fmt.Errorf("%w: %s: %s", cubeclient.ErrCubeAuthRejected, st.Code(), st.Message())
	case codes.PermissionDenied:
		return fmt.Errorf("%w: %s: %s", cubeclient.ErrCubeForbidden, st.Code(), st.Message())
	case codes.NotFound:
		return fmt.Errorf("%w: %s: %s", cubeclient.ErrCubeNotFound, st.Code(), st.Message())
	default:
		return fmt.Errorf("%w: %s: %s", ErrCubeUnavailable, st.Code(), st.Message())
	}
}
