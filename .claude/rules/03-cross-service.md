---
paths: ["internal/cubeclient/**/*.go", "internal/cubehttp/**/*.go", "internal/cubesync/**/*.go", "pkg/eventbus/**/*.go"]
---

# 跨服务调用(dapr service invocation)

跨 dapr app 调用**必须**经 dapr sidecar,**不要**直连目标进程。
**禁止**手拼 `/v1.0/invoke/<target-app-id>/method/<rest>` URL —— 一律走 `github.com/dapr/go-sdk/client`。

## 标准模式

```go
daprCli, err := dapr.NewClient()   // SDK 从 DAPR_GRPC_PORT(默认 :50001)拿 sidecar,dapr run 注入
if err != nil {
    return fmt.Errorf("dapr.NewClient: %w (确认 dapr run 已起)", err)
}

// service invocation
resp, err := daprCli.InvokeMethodWithContent(ctx,
    "supertrade-cube-router", "v1/load", "POST",
    &dapr.DataContent{ContentType: "application/json", Data: body},
)

// pub/sub
err = daprCli.PublishEvent(ctx, eventbus.Name, "auth.user.access_changed", data)
```

## JWT / Authorization 透传

走 SDK 时**不能**直接 `req.Header.Set("Authorization", ...)` —— 要转 gRPC outgoing metadata,
sidecar 自动转成 outgoing HTTP `Authorization` 给目标:

```go
if bearer := c.Request.Header.Get("Authorization"); bearer != "" {
    ctx = metadata.AppendToOutgoingContext(ctx, "authorization", bearer)
}
```

## 约束

- **不要**硬编码目标 IP —— 跨主机部署 IP 不可知。
- **不要**绕过 dapr 直连:哪怕本地测试也要 `dapr run` 起 sidecar。
- **不要**读 `DAPR_ENDPOINT` —— SDK 自动拿 `DAPR_GRPC_PORT`。
- **不要**混淆 `APP_PORT`(app 监听)与 `DAPR_HTTP_PORT`(sidecar 监听):app bind 3500 会跟 sidecar 抢端口。
- 目标端鉴权由 sidecar + 应用层完成;handler 只判 aud / RBAC。
- 测试 mock 用 **fake dapr.Client**(embed `dapr.Client` + 覆写要用的方法),
  **不要**用 `httptest.Server` 模拟 sidecar —— SDK 走 gRPC over `:50001`。

## cube 客户端:只走 cube-router

`internal/cubehttp.NewClient()` / `internal/cubeclient` 内部做 `NewDaprCubeClient(daprCli, appID, queryPath)`。

- 默认 `appID = supertrade-cube-router`、`queryPath = v1/load`(用 `CUBE_APP_ID` / `CUBE_QUERY_PATH` 覆盖,两者必须成对)。
- **只走 cube-router**:它按 `X-Branch-ID` 查 `branch_cube_sources` 做 per-branch 路由;
  直连某个 cube app 会绕过分门店隔离,拿到全部门店的数据。
- **没有任何 mock 分支**(没有内存假客户端):需要 mock 的测试用 `internal/cubeclient/cubeclientfake`。

参考:`internal/cube-router/handler/proxy.go`、`internal/cubeclient/dapr_client.go`、
`internal/cubehttp/handler.go::NewClient`。
