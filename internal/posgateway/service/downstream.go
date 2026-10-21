// Package service 实现 pos-gateway(POS BFF)的聚合逻辑。
//
// # 边界:BFF **不拥有任何数据**
//
// 它只做两件事:
//  1. 把散在 member / pos / master-data / catalog / pricing 的数据**一次**返给 POS 客户端;
//  2. 在某一个下游失败时**降级**(那一项返 null + 在 warnings 里点名),而不是让整个请求失败。
//
// ⚠️ **鉴权失败不降级**:下游 401 / 403 一律向上抛(整个请求失败)。
// 把 401 吞成"部分成功"= 客户端以为拿到了数据,实际是一条越权响应被静默掉了。
// 判据见 [IsAuthError]。
//
// ⚠️ **金额绝不用 double**:本包用 `AmountString` 承载金额 ——
// 读时接受 JSON 字符串或数字,写时**一律字符串**(JSON number 到 Dart 侧会变 double)。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	dapr "github.com/dapr/go-sdk/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// 下游 dapr app-id。
//
// ⚠️ 这些值必须与 deployer/systemd/*.service 里 `dapr run --app-id` 的值**逐字相同**
// —— sidecar 按 app-id 做服务发现,写错一个字母 = "failed to find app id"。
const (
	// AppMember 是会员域(app 端口 8113)。
	AppMember = "supertrade-member"
	// AppPOS 是交易域(app 端口 8114)。
	AppPOS = "supertrade-pos"
	// AppMasterData 是门店 / 收银员 / 班次(app 端口 8102)。
	AppMasterData = "supertrade-master-data"
	// AppCatalog 是商品档案(app 端口 8103)。
	AppCatalog = "supertrade-catalog"
	// AppPricing 是售价 / 促销(app 端口 8111)。
	AppPricing = "supertrade-pricing"
)

// DefaultDownstreamTimeout 是单次下游调用的超时。
//
// 10s 与仓内其它跨服务调用(userinfo / cubeclient)同档:
// 跨主机经 Consul 解析 + daprd 中转实测 200~500ms,冷启动可到 1~3s。
const DefaultDownstreamTimeout = 10 * time.Second

// Caller 是"调下游 dapr app"的抽象。
//
// 抽成接口是为了**可测**:假 dapr 太重(要起 sidecar),
// 而 BFF 的核心逻辑(聚合 / 降级 / 鉴权失败上抛)不该被 sidecar 绑住。
type Caller interface {
	// Get 发 GET 到 appID 的 path(path 可带 "?query")。
	Get(ctx context.Context, appID, path string) (json.RawMessage, error)
	// Post 发 POST 到 appID 的 path,body **原样**作为请求体。
	Post(ctx context.Context, appID, path string, body []byte) (json.RawMessage, error)
}

// DownstreamError 是"下游没给出想要的结果"。
//
// 它同时承载两种语义,靠 [DownstreamError.Status] 区分:
//   - 401 / 403:**鉴权失败** ⇒ 整个请求失败(不可降级);
//   - 其它:下游的业务错误 / 服务不可用 ⇒ 该项降级。
type DownstreamError struct {
	// Status 是下游的 HTTP 状态码(由 dapr 的 gRPC code 反推;调用根本没发生时是 0)。
	Status int
	// Code 是错误码(能判定时给出;否则 "upstream_error")。
	Code string
	// Message 是给人看的说明。
	Message string
	// AppID / Path 是出错的调用点(排查用)。
	AppID string
	Path  string
	// Err 是原始错误。
	Err error
}

// Error 实现 error。
func (e *DownstreamError) Error() string {
	return fmt.Sprintf("posgateway: 下游 %s %s 失败(status=%d code=%s): %v",
		e.AppID, e.Path, e.Status, e.Code, e.Err)
}

// Unwrap 让 errors.Is / errors.As 能穿透到原始错误。
func (e *DownstreamError) Unwrap() error { return e.Err }

// AsDownstreamError 取出下游错误(不是下游错误则返 nil)。
func AsDownstreamError(err error) *DownstreamError {
	var de *DownstreamError
	if errors.As(err, &de) {
		return de
	}
	return nil
}

// IsAuthError 判断错误是不是**下游鉴权失败**(401 / 403)。
//
// ⚠️ 这两种错误**不能降级** —— 见包注释。
func IsAuthError(err error) bool {
	de := AsDownstreamError(err)
	if de == nil {
		return false
	}
	return de.Status == http.StatusUnauthorized || de.Status == http.StatusForbidden
}

// daprInvoker 是 DaprCaller 对 dapr SDK 的**全部**依赖面。
//
// 收窄的理由与 internal/cubeclient 一致:go-sdk 没有 client/mock,
// 手写整个 dapr.Client(几十个方法)成本高到没人愿意写,
// 于是"到底往下游发了什么"只能靠肉眼 review 保护。
//
// 收窄后 dapr.Client 自动满足本接口(结构化子集),调用方零改动。
type daprInvoker interface {
	InvokeMethod(ctx context.Context, appID, methodName, verb string) ([]byte, error)
	InvokeMethodWithContent(ctx context.Context, appID, methodName, verb string,
		content *dapr.DataContent) ([]byte, error)
}

// DaprCaller 是走 dapr service invocation 的 Caller 实现。
type DaprCaller struct {
	cli     daprInvoker
	timeout time.Duration
}

// NewDaprCaller 构造 DaprCaller。
//
// ⚠️ cli 必须来自 `github.com/YunBright/authkit/daprclient.New()`,
//
//	**不能**用 `dapr.NewClient()`:它的 api-token 拦截器用
//	`metadata.NewOutgoingContext` **整块替换** outgoing metadata,
//	把转发的 Authorization 抹掉 ⇒ 下游全 401 且**零报错**
//	(2026-10-20 全网加 DAPR_API_TOKEN 之后立刻变成事故)。
func NewDaprCaller(cli daprInvoker) *DaprCaller {
	return &DaprCaller{cli: cli, timeout: DefaultDownstreamTimeout}
}

// Get 实现 Caller。
func (c *DaprCaller) Get(ctx context.Context, appID, path string) (json.RawMessage, error) {
	if c == nil || c.cli == nil {
		return nil, fmt.Errorf("posgateway: dapr client 未装配")
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	// ⚠️ query 拼在 methodName 里:dapr go-sdk 的 extractMethodAndQuery 会
	//   按第一个 "?" 切成 method + querystring,sidecar 再把 querystring
	//   拼回 URL(dapr pkg/channel/http/http_channel.go::constructRequest)。
	out, err := c.cli.InvokeMethod(withOutgoing(ctx), appID, path, "GET")
	if err != nil {
		return nil, classify(appID, path, err)
	}
	return json.RawMessage(out), nil
}

// Post 实现 Caller。
func (c *DaprCaller) Post(ctx context.Context, appID, path string, body []byte) (json.RawMessage, error) {
	if c == nil || c.cli == nil {
		return nil, fmt.Errorf("posgateway: dapr client 未装配")
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	out, err := c.cli.InvokeMethodWithContent(withOutgoing(ctx), appID, path, "POST",
		&dapr.DataContent{ContentType: "application/json", Data: body})
	if err != nil {
		return nil, classify(appID, path, err)
	}
	return json.RawMessage(out), nil
}

// withOutgoing 把 caller 的 Authorization / X-Branch-ID 塞进 outgoing gRPC metadata。
//
// dapr sidecar 会把收到的 metadata 转成 outgoing HTTP 头给目标 app
// (dapr pkg/api/grpc/grpc.go::InvokeService 里的 req.WithMetadata(incomingMD))。
//
// ⚠️ **必须原样转发 caller's 令牌**:下游的权限是 per-branch **实时解析**的,
//
//	换成机器身份就丢了"这个人在这家店到底有没有权限"这条语义。
func withOutgoing(ctx context.Context) context.Context {
	if bearer := BearerFromContext(ctx); bearer != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", bearer)
	}
	if branchID := BranchFromContext(ctx); branchID != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-branch-id", branchID)
	}
	return ctx
}

// classify 把 dapr 调用错误映射成 DownstreamError。
//
// dapr 的 HTTP 通道对下游的非 2xx **不返回 error**,而是把状态码带回来;
// gRPC 门面再按 HTTP→gRPC 的标准映射转成 error
// (dapr pkg/messaging/v1/util.go::CodeFromHTTPStatus):
//
//	401 → Unauthenticated     403 → PermissionDenied
//	404 → NotFound            409 → AlreadyExists
//	400 → Internal            500 → Unknown
//	503 → Unavailable         504 → DeadlineExceeded
//
// ⇒ 由 gRPC code **反推** HTTP 状态码,才能把"下游 401"和"下游 503"分开 ——
// 这正是"鉴权失败必须整体失败"这条规则的实现基础。
func classify(appID, path string, err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return &DownstreamError{AppID: appID, Path: path, Err: err,
			Code: "upstream_error", Message: err.Error()}
	}
	de := &DownstreamError{
		AppID:   appID,
		Path:    path,
		Err:     err,
		Status:  httpStatusFromGRPCCode(st.Code()),
		Code:    "upstream_error",
		Message: st.Message(),
	}
	switch st.Code() {
	case codes.Unauthenticated:
		de.Code = "unauthenticated"
		de.Message = "下游拒绝:缺少或无效的登录态"
	case codes.PermissionDenied:
		de.Code = "forbidden"
		de.Message = "下游拒绝:该门店下没有所需权限"
	}
	return de
}

// httpStatusFromGRPCCode 把 dapr 的 gRPC code 映射回 HTTP 状态码。
func httpStatusFromGRPCCode(c codes.Code) int {
	switch c {
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.NotFound:
		return http.StatusNotFound
	case codes.AlreadyExists:
		return http.StatusConflict
	case codes.InvalidArgument, codes.Internal:
		return http.StatusBadRequest
	case codes.ResourceExhausted:
		return http.StatusTooManyRequests
	case codes.Unavailable:
		return http.StatusServiceUnavailable
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout
	case codes.Unimplemented:
		return http.StatusNotImplemented
	default:
		return http.StatusInternalServerError
	}
}
