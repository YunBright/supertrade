// Package cubesync / sidecar.go —— 与本机 dapr sidecar 通话的公共底座。
//
// # 为什么走 sidecar 的 **HTTP API** 而不是 gRPC SDK
//
// 本包要调的是**两个 supertrade 自己的 dapr app**(cube-router 的导出端点、
// catalog 的导入端点),而不是 cube 语义层。
//
// ① 导出端点是 **GET + query 参数**(`?scope=&offset=&limit=`)。
//
//	dapr 的 **gRPC** 调用面(`InvokeMethodWithContent`)没有 query 参数入口,
//	只能把 query 拼进 methodName —— 那依赖 sidecar 把 methodName
//	当 URL 路径解析的**未文档化行为**,且对 gRPC 目标端直接失效。
//	⇒ 走 sidecar 的 **HTTP API**:`GET /v1.0/invoke/<app-id>/method/<path>?<query>`,
//	这是 dapr 文档明确支持的形态。
//
// ② HTTP API 让我们能**显式控制**两个必需的头:
//   - `Authorization: Bearer <机器 token>`(被调方要验签 + 要三元权限)
//   - `X-Branch-ID: <门店>`(被调方要按门店路由 / 取 per-branch 权限)
//     gRPC 那一侧要靠 metadata → HTTP 头的隐式转换,多一层不确定性。
//
// ⚠️ 而 token 签发与 job 注册**本来就**走 sidecar 的 HTTP API
// (见 internal/salesagg/collect 的既有实现)⇒ 本包三种调用面统一。
package cubesync

import (
	"net/http"
	"os"
	"strings"
)

// daprAPITokenEnvVar 是 Dapr API token 的环境变量名。
//
// ⚠️ 2026-10-20 起全网 sidecar 都要求 dapr-api-token(生产同版本 Dapr 1.18.0):
// /v1.0/healthz 豁免,/v1.0/metadata、/v1.0/state/*、/v1.0/invoke/* 全部 401。
//
// 令牌由 `dapr run` 从父进程环境继承 ⇒ app 不用额外配置。
const daprAPITokenEnvVar = "DAPR_API_TOKEN"

// setAPIToken 给请求带上 dapr-api-token(若配置了)。
//
// 集中在一处 ⇒ 新增调用点不会漏。
// 空值时**不带**头:sidecar 会 401,而"401 + 没带令牌"比"401 + 带错令牌"更容易定位。
func setAPIToken(req *http.Request) {
	if req == nil {
		return
	}
	if tok := strings.TrimSpace(os.Getenv(daprAPITokenEnvVar)); tok != "" {
		req.Header.Set("dapr-api-token", tok)
	}
}

// sidecarBaseURL 是本机 sidecar 的 HTTP 根地址。
//
// 空串 = 没有 sidecar(裸 `go run` 调试)。调用方必须自己判空,
// 绝不能拿空串去拼 URL —— 那样会得到一个"相对路径 URL",
// 而 net/http 会把它报成 "unsupported protocol scheme" ——
// 一个与真因(没起 sidecar)毫无关系的错误。
func sidecarBaseURL() string {
	port := strings.TrimSpace(os.Getenv("DAPR_HTTP_PORT"))
	if port == "" {
		return ""
	}
	return "http://127.0.0.1:" + port
}
