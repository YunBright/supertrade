---
paths: ["internal/**/handler/*.go", "internal/**/handler/**/*.go"]
---

# Handler 路由注册规范

`internal/<service>/handler/handler.go` 必须导出 `RegisterRoutes(r gin.IRouter)`。

## 核心约束：路径必须从 root 起

```go
func (h *Handler) RegisterRoutes(r gin.IRouter) {
    // ✅ 正确：从 root 注册 (nginx 已经剥过 /api/v1/<app>/ 前缀)
    r.POST("/<resource>",          h.Create)
    r.GET("/<resource>/:id",       h.Get)
    r.POST("/<resource>/:id/<action>", h.DoAction)

    // ❌ 错误：带了 <app> 前缀 (nginx 不会重复剥)
    // r.POST("/<app>/<resource>", h.Create)
}
```

为什么：nginx 用 `rewrite ^/api/v1/[^/]+/(.*)$ /$1 break` 把 `/api/v1/<app>/<rest>` 剥成 `/<rest>` 给 dapr-sidecar，sidecar 再原样转给 backend。backend 注册的路径就是 `/<rest>`，不能再带 `<app>`。

## 路径命名约定

- 资源用复数 + kebab-case：`/stocktake-headers`、`/stocktake-lines`、`/pos-orders`
- 子资源嵌套：`/stocktake-headers/:id/lines`
- 不用 query 表达资源层级：`/lines?header_id=...` ❌
- 动作挂在资源下：`/stocktake-headers/:id/submit`、`/stocktake-headers/:id/approve`（而不是 `/submit-stocktake-header`）

## 不要做的事

- **不要**注册 `/healthz` — `cmdbootstrap` 已注册（公开，绕过 auth 中间件）
- **不要**用 `/api/v1/<app>/` 前缀（nginx 已剥）
- **不要**把 admin 校验写在 handler 层 — 用 `authkit/rbac.RequireRole("admin")` 中间件或在 service 层判
- **不要**在 handler 里验签 JWT — `authkit/claims.GinMiddleware` 已解析 claims 进 ctx

## 加端点的 checklist

1. 在 `RegisterRoutes` 加一行 + 实现 handler 方法
2. nginx map **不动**（`<app>` 段没变）
3. 加 `cmdbootstrap` 自动接的鉴权（`aud` 校验 + claims 解析）之外的业务校验
4. 在 deployer 仓 `tests/contract/<service>_test.go` 加 e2e 测试

参考：`internal/stocktake/handler/handler.go:55-72`。