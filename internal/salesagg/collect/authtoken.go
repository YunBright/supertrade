package collect

import (
	"net/http"
	"os"
)

// daprAPITokenEnvVar 是 Dapr API token 的环境变量名。
//
// 2026-10-20 起全网 sidecar 都要求 dapr-api-token
// (实测,生产同版本 Dapr 1.18.0):/v1.0/healthz 豁免,
// /v1.0/metadata、/v1.0/state/*、/v1.0/invoke/* 全部 401;
// 且**跨 sidecar 的服务调用**也要 ⇒ 令牌全网一致。
//
// 令牌由 `dapr run` 从父进程环境继承 ⇒ app 不用额外配置。
const daprAPITokenEnvVar = "DAPR_API_TOKEN"

// setAPIToken 给请求带上 dapr-api-token(若配置了)。
//
// 集中在一处 ⇒ 新增调用点不会漏。
// 空值时**不带**头:sidecar 会 401,而"401 + 没带令牌"比"401 + 带错令牌"
// 更容易定位。
func setAPIToken(req *http.Request) {
	if req == nil {
		return
	}
	if tok := os.Getenv(daprAPITokenEnvVar); tok != "" {
		req.Header.Set("dapr-api-token", tok)
	}
}
