package cmdbootstrap

import (
	"os"
	"strings"
)

// AppPort 返回应用 HTTP 监听地址。
//
// dapr run --app-port <p> 启动时会往子进程注入 APP_PORT=<p>(见
// https://docs.dapr.io/reference/environment/_print)。读它而不是硬编码,
// 让 app 跟着 operator 在 dapr run 时指定的端口走 — 多个实例共存 / 端口
// 冲突时不用动业务代码。
//
// fallback 是 APP_PORT 未注入时的回退端口(如本地直跑 go run,无 sidecar)。
// 接受带或不带 ":" 前缀;返回结果保证有 ":" 前缀,可以直接给 http.Server.Addr
// / gin.Run 用。
//
// 重要:不要跟 DAPR_HTTP_PORT 混淆 — 那是 dapr *sidecar* 的 HTTP 端口(默认 3500),
// 不是 app 的监听端口。app bind 3500 会跟 sidecar 抢端口,直接起不来。
func AppPort(fallback string) string {
	port := os.Getenv("APP_PORT")
	if port == "" {
		port = fallback
	}
	if port == "" {
		return ""
	}
	if !strings.HasPrefix(port, ":") {
		port = ":" + port
	}
	return port
}
