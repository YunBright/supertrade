---
paths: ["cmd/**/*.go"]
---

# cmd 入口文件规范

`cmd/<service>/main.go` 是每个 dapr app 的入口，必须遵循以下模式。

## 模板

```go
func main() {
    cmdbootstrap.Run(cmdbootstrap.Options{
        AppID:    "<service>",          // ← 必须跟 nginx map 一致 (见 00-app-catalog.md)
        Port:     ":8080",              // 可选, 默认 :8080
        Audience: "<service>",          // 可选, 默认 = AppID (rbac.RequireAudience target)
        OnStart:  func() error {        // 初始化 DB / Redis / 客户端
            return initApp()
        },
        Register: registerRoutes,       // 调 handler.New(svc).RegisterRoutes(r)
    })
}

func registerRoutes(r *gin.Engine) {
    handler.New(appSvc).RegisterRoutes(r)
}
```

## 约束

- **不要**重新注册 `/healthz` — `cmdbootstrap` 已注入。
- **不要**手写 `http.Server.ListenAndServe` — 用 `cmdbootstrap.Run`，自带 5s 优雅退出。
- **不要**在 `Register` 里读 JWT — `authkit/claims.GinMiddleware` 已塞到 ctx；用 `claims.FromContext(c)` 取。
- `AppID` 改动**必须同步** deployer/nginx/yun-bright.conf 的 map 规则（加新条目）。

## 启动

```bash
dapr run --app-id <service> --app-port 8080 \
  --components-path ./dapr/components \
  --config ./dapr/components/<service>-config.yaml \
  -- go run ./cmd/<service>
```

参考：`pkg/cmdbootstrap/bootstrap.go`、`cmd/stocktake/main.go`、`cmd/pos-gateway/main.go`。