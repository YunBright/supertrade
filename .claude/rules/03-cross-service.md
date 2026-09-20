---
paths: ["internal/cubeclient/**/*.go", "internal/cubehttp/**/*.go", "internal/notification/**/*.go"]
---

# 跨服务调用（dapr service invocation）

跨 dapr app 调用**必须**经本机 dapr sidecar，**不要**直连目标进程。

## 标准模式

```go
import "github.com/YunBright/supertrade/internal/cubehttp"  // 或 notification/http 等

cli, err := cubehttp.NewClientFromEnv()  // 内部从 DAPR_ENDPOINT 拿 :3500
if err != nil { return err }
result, err := cli.Load(ctx, "supplier.count", nil)
```

底层走 dapr `/v1.0/invoke/<target-app-id>/method/<rest>`，由 sidecar 解析 Consul 名（跨主机时返回 172.12.1.5 之类的真实 IP）。

## 约束

- **不要**硬编码 `http://localhost:8080` / `http://172.12.1.5:3000` — 跨主机部署 IP 不可知
- **不要**绕过 dapr 直连：哪怕本地测试也要 `dapr run` 起 sidecar
- 调用方必须传 `dapr-app-id: <target>` header（在 dapr invoke 客户端里设，或 nginx `proxy_set_header`）
- 目标端鉴权由 dapr sidecar JWT middleware 完成；handler 只判 aud / RBAC

## 加新客户端封装

```text
internal/<x>/http/
    client.go       ← 暴露 XxxClient interface + NewClientFromEnv()
    client_test.go  ← httptest.Server mock (用 <target-app-id> 路径)
```

参考：`internal/cubehttp/client.go`（cube-gateway 客户端）、`internal/cubehttp/client_test.go`（mock 模式）。