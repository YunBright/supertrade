---
paths: ["internal/**/handler/*.go", "internal/**/handler/**/*.go"]
---

# Handler 路由注册规范

`internal/<service>/handler/handler.go` 导出 `RegisterRoutes(r gin.IRouter)`。

## 核心约束:路径从 root 起

```go
func (h *Handler) RegisterRoutes(r gin.IRouter) {
    r.POST("/<resource>",            h.Create)   // ✅ nginx 已剥 /api/v1/<app>/
    r.GET("/<resource>/:id",         h.Get)
    r.POST("/<resource>/:id/<action>", h.DoAction)
}
```

nginx 用 `rewrite ^/api/v1/[^/]+/(.*)$ /$1 break` 把 `/api/v1/<app>/<rest>` 剥成 `/<rest>`
给 sidecar,sidecar 原样转给 backend ⇒ backend 注册的路径就是 `/<rest>`,**不能再带 `<app>`**。

## 路径命名

- 资源用复数 + kebab-case:`/stocktake-headers`、`/pos-orders`
- 子资源嵌套:`/stocktake-headers/:id/lines`
- 动作挂在资源下:`/stocktake-headers/:id/submit`(不是 `/submit-stocktake-header`)
- 不用 query 表达资源层级:`/lines?header_id=...` ❌

## 不要做的事

- **不要**注册 `/healthz` —— `cmdbootstrap` 已注册(公开,绕过鉴权)。
- **不要**用 `/api/v1/<app>/` 前缀。
- **不要**在 handler 层做 admin 判断 —— 用 `authkit/rbac.RequireRole("admin")` 或 service 层判。
- **不要**在 handler 里验签 JWT —— `claims.GinMiddleware` 已解析 claims 进 ctx。

## X-Branch-ID 是默认上下文

- branch 全部从 `X-Branch-ID` header 取;**path 里不许出现 `:branch_id`**。
- `middleware.XBranchID()` 全局挂(各 `cmd/<svc>/main.go::registerRoutes` 第一行),**不强制 header 存在**,
  由 handler 自己判要不要 400。
- 取值:单店 `middleware.SingleBranchFromCtx(c)`;多店 `middleware.BranchFromCtx(c)`。
- 需要 branch 的端点:handler 自挂 `middleware.RequireBranch()`,或自己 `if x == "" { 400 }`。
- 业务鉴权:`rbac.RequireScopeWithBranch(scope, branchFn, rbac.HasAnyScopeWithBranch(users))`。
- 高频只读:`rbac.RequireBranch(branchFromHeader, accessibleBranchesFromClaims)`(JWT 静态版,零 userd 调用)。

| header | 含义 | 调用方处理 |
|---|---|---|
| `<uuid>` / `01` | 单店 | 直接用 |
| `01,02` | 多店并行(逗号分隔) | 迭代各 branch |
| `*` | 全部 accessible branches | 调 `claims.AccessibleBranches` 展开 |

`middleware` 不解析 `*` / 逗号 —— 展开由下游 server 按业务语义做。

## 加端点的清单

1. 在 `RegisterRoutes` 加一行 + 实现 handler 方法。
2. nginx map **不动**(URL 段没变);**加新 app** 才动 map。
3. 补业务校验(scope / branch / 参数边界)。
4. 加新 app 时同步 `00-app-catalog.md` + deployer nginx map + authd `JWT_AUDIENCE`。
5. **本仓写分模块单元测试**(`internal/<svc>/handler/*_test.go`,httptest + fake 依赖)。
   **契约 / e2e 测试不进本仓** —— 那是 `../deployer` 仓的 `tests/contract/`,走公网 nginx 入口。
6. handler 注释只写**契约与边界**(保证什么、拒什么、依赖什么),不写调试经过与历史。

参考:`internal/stocktake/handler/handler.go`、`internal/catalog/handler/handler.go`、
`internal/cubehttp/handler.go`。
