---
paths: ["internal/cubeclient/**/*.go", "internal/cubehttp/**/*.go", "internal/notification/**/*.go"]
---

# 跨服务调用（dapr service invocation）

跨 dapr app 调用**必须**经 dapr sidecar,**不要**直连目标进程。
**禁止**手拼 `/v1.0/invoke/<target-app-id>/method/<rest>` URL —— 一律走 `github.com/dapr/go-sdk/client`。

## 标准模式(dapr/go-sdk)

```go
import dapr "github.com/dapr/go-sdk/client"

// SDK 自动从 DAPR_GRPC_PORT (默认 :50001) 拿 sidecar,dapr run 自动注入
// 无需 DAPR_ENDPOINT / DAPR_HTTP_PORT 任何环境变量
daprCli, err := dapr.NewClient()
if err != nil {
    return fmt.Errorf("dapr.NewClient: %w (确认 dapr run 已起)", err)
}

// Service invocation —— 业务 HTTP 调用
resp, err := daprCli.InvokeMethodWithContent(ctx,
    "cube-router", "v1/load", "POST",
    &dapr.DataContent{ContentType: "application/json", Data: body},
)

// Pub/sub —— 业务事件发布
err = daprCli.PublishEvent(ctx, "pubsub", "auth.user.access_changed", data)
```

底层 sidecar 解析 app-id(`sixun-hbposv7` / `cube-gateway` / `userd` 等)→ 跨主机 DNS 解析→ HTTP 转给目标进程。

## JWT / Authorization 透传

走 SDK 时 `c.Request.Header.Get("Authorization")` 不能再直接 `req.Header.Set`,
要转 gRPC outgoing metadata,sidecar 自动转 outgoing HTTP `Authorization` 给目标:

```go
import "google.golang.org/grpc/metadata"

if bearer := c.Request.Header.Get("Authorization"); bearer != "" {
    ctx = metadata.AppendToOutgoingContext(ctx, "authorization", bearer)
}
```

(dapr sidecar 把 gRPC metadata keys 映射成 outgoing HTTP headers,
key 全部 lowercase,`authorization` → `Authorization`)

## 约束

- **不要**手拼 `http://localhost:3500/v1.0/invoke/<id>/method/<rest>` URL —— 一律 SDK
- **不要**硬编码目标 IP(`172.12.1.5:3000` 等)—— 跨主机部署 IP 不可知
- **不要**绕过 dapr 直连:哪怕本地测试也要 `dapr run` 起 sidecar
- **不要**读 `DAPR_ENDPOINT` 环境变量 —— SDK 自动拿 `DAPR_GRPC_PORT`(`dapr run` 注入)
- 调用方必须传 `dapr-app-id: <target>` header(SDK 已封装;nginx `proxy_set_header` 不需)
- 目标端鉴权由 dapr sidecar JWT middleware 完成;handler 只判 aud / RBAC

## 加新客户端封装

```text
internal/<x>/
    dapr_client.go        ← 暴露 XxxClient interface + NewDaprClient(dapr.Client, appID)
    dapr_client_test.go   ← fakeDaprClient(embed dapr.Client + 仅覆写 InvokeMethodWithContent)
```

测试 mock 模式:

```go
type fakeDaprClient struct {
    dapr.Client  // nil 实现 → 未覆写方法 panic
    Invoke func(ctx context.Context, appID, method, verb string,
                content *dapr.DataContent) ([]byte, error)
}
func (f *fakeDaprClient) InvokeMethodWithContent(...) ([]byte, error) {
    return f.Invoke(ctx, appID, method, verb, content)
}
```

> 不要再用 httptest.Server 模拟 dapr sidecar —— SDK 直连 sidecar,
> httptest 模拟的是底层 HTTP,而 SDK 走 gRPC over :50001。

## cube-router(多 cube 实例路由)

`cube-router` 是 cube 多源路由的收口点。stocktake / catalog / inventory / erp-connector /
bi-gateway 等所有需要 cube 的服务,**把 `POST /v1/load` 转发到 cube-router** 而不是直接到 cube-gateway。

```text
[业务服务] ─SDK invoke─▶ cube-router (按 X-Branch-ID 查 branch_cube_sources)
                          │
                          ├─ branch=S001 → sixun-hbposv7
                          └─ branch=S002 → sixun-ysx
                          │
                          ▼
                  cube-gateway /v1/load (实际 cube 实例)
```

**为什么不直接调 cube-gateway**:
- 同 store 可挂多个 cube app(sixun-hbposv7 / sixun-ysx 等)
- 不同门店要路由到不同 cube 实例(思迅不同分店可能在不同 cube 后端)
- 单 cube 实例切换 / 灰度期间不需要业务侧改动

**业务侧接入方式**(走 SDK):

```go
daprCli, _ := dapr.NewClient()
// cubehttp.NewClientFromEnv() 内部已经做 NewDaprCubeClient(daprCli, "cube-router")
cli, _ := cubehttp.NewClientFromEnv()
result, err := cli.LoadCubeQuery(ctx, "supplier.count", cubeclient.CubeQuery{
    Measures: []string{"supplier.count"},
})
```

参考:`internal/cube-router/handler/proxy.go`(handler 转发到 cube 实例),
`internal/cubeclient/dapr_client.go`(DaprCubeClient 实现),
`internal/cubehttp/handler.go::NewClientFromEnv`(caller 注入入口)。