---
paths: ["cmd/**/*.go"]
---

# cmd 入口文件规范

`cmd/<service>/main.go` 是每个 dapr app 的入口,统一走 `cmdbootstrap.Run`。

## 模板

```go
func main() {
    cmdbootstrap.Run(cmdbootstrap.Options{
        AppID:    "<aud-short-name>",   // ← JWT  aud 短名,不是 dapr app-id(见 00-app-catalog.md)
        Port:     cmdbootstrap.AppPort(":8080"), // 可选,默认 :8080
        Audience: "<aud-short-name>",   // 可选,缺省 = AppID
        Register: registerRoutes,       // 业务路由
        RegisterDapr: registerDaprRoutes, // 可选,Dapr 内部路由(在鉴权之前注册)
    })
}
```

## Options 字段语义

| 字段 | 含义 | 缺省 |
|---|---|---|
| `AppID` | **JWT aud 目标**(也是 /healthz 与日志里的 app 名) | 必填 |
| `Audience` | `rbac.RequireAudience` 的目标 | `= AppID` |
| `Port` | 监听地址 | `:8080` |
| `Register` | 业务路由(在鉴权中间件**之后**) | nil |
| `RegisterDapr` | Dapr 内部路由(`/dapr/subscribe`、`/events/<topic>`),在鉴权**之前** | nil |
| `OnStart` / `OnStop` | 起停钩子 | nil |
| `Logger` | 自定义 logger | slog JSON stdout |
| `AllowUnverified` | 跳过验签(只给单测) | false |

⚠️ **`AppID` 不是 dapr app-id。** 路由用的 dapr app-id(`supertrade-*`)是 nginx map 的值,
两者混用 ⇒ 401 `invalid_audience`。见 `00-app-catalog.md`。

## 约束

- **不要**注册 `/healthz` —— `cmdbootstrap` 已注入(公开,绕过鉴权)。
- **不要**手写 `http.Server.ListenAndServe` —— `cmdbootstrap.Run` 自带 5s 优雅退出。
- **不要**在 `Register` 里读 JWT —— `claims.GinMiddleware` 已塞进 ctx;用 `claims.FromContext(c)`。
- **不要**把 Dapr 内部路由放进 `Register` —— sidecar 调 `/dapr/subscribe` 不带 token,
  放进 `Register` 会被 `RequireAudience` 挡成 401,整条 pub/sub 链路静默失效。
- **不要**把 `AppID` 改成 dapr app-id —— 改 aud 要同步 authd `JWT_AUDIENCE`(短名);
  只有**新增 app** 才需要动 deployer nginx map。

## 启动

```bash
dapr run --app-id <dapr-app-id> --app-port 8080 \
  --components-path ./dapr/components \
  -- go run ./cmd/<service>
```

`--app-id` 传 dapr app-id(带 `supertrade-` 前缀),与 deployer nginx map 一致;
`AppID` 传 aud 短名。两者不是一个东西。

参考:`pkg/cmdbootstrap/bootstrap.go`、`cmd/stocktake/main.go`、`cmd/pos-gateway/main.go`。
