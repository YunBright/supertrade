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
4. 加新 app 时同步检查 `00-app-catalog.md` 表 + deployer nginx map
5. 在 deployer 仓 `tests/contract/<service>_test.go` 加 e2e 测试

参考：`internal/stocktake/handler/handler.go:55-72`。

## X-Branch-ID header 是默认上下文（2026-09 重构后）

**所有 path 不带 `:branch_id` —— branch 全部从 `X-Branch-ID` header 取**。
原 `/stock/:branch_id/:product_id` 已并为 `/stock/:product_id`,
原 `/branches/:branch_id/default-stocktake` 已并为 `/default-stocktake`。

- **全局挂 `middleware.XBranchID()`** —— 由各 `cmd/<service>/main.go::registerRoutes` 第一行 `r.Use(middleware.XBranchID())` 注入
- **middleware 不强制 header 存在** —— 透传 + 解析,handler 决定要不要 400
- **handler 取 branch** —— 单店 `middleware.SingleBranchFromCtx(c)`;多店迭代 `middleware.BranchFromCtx(c)`(`*` / `01,02`)
- **强制要求的端点** —— handler 自挂 `middleware.RequireBranch()`,或自己 `if x == "" { 400 }`
- **业务端点鉴权** —— 走 `rbac.RequireScopeWithBranch(scope, branchFn, rbac.HasAnyScopeWithBranch(users))`
- **库存只读端点** —— 高频读走 `rbac.RequireBranch(branchFromHeader, accessibleBranchesFromClaims)`(JWT 静态版,零 userd 调用)

**多店 header 语义**(字面值透传):

| header | 含义 | 调用方处理 |
|---|---|---|
| `<uuid>` | 单店操作 | 单 UUID,直接用 |
| `01,02` | 多店并行操作(逗号分隔) | 迭代各 branch 跑 |
| `*` | 全部 accessible branches | 调 `claims.AccessibleBranches` 展开 |

`middleware` 不解析 `*` / 逗号 — 由下游 server(userd / catalog / cube-router)按业务语义展开。

**新增端点示例**(catalog):

```go
// cmd/catalog/main.go
func registerRoutes(r *gin.Engine) {
    users := userinfo.New("userd")
    r.Use(middleware.XBranchID())  // 全局:把 X-Branch-ID header 注入 ctx
    handler.New(appSup, appProd, appCube, users, nil).RegisterRoutes(r)
}

// internal/catalog/handler/handler.go
func (h *Handler) RegisterRoutes(r gin.IRouter) {
    resolver := rbac.HasAnyScopeWithBranch(h.users)
    r.GET("/suppliers",
        rbac.RequireScopeWithBranch("supplier:view", branchFromHeader, resolver),
        h.listSuppliers)
}

// internal/cubehttp/handler.go —— 高频只读走 JWT 静态校验
r.GET("/stock/:product_id",
    rbac.RequireBranch(branchFromHeader, accessibleBranchesFromClaims),
    h.getStock,
)
```

参考:`cmd/catalog/main.go`、`internal/catalog/handler/handler.go:82-109`、
`internal/cube-router/handler/handler.go:83-101`、`internal/cubehttp/handler.go:55-66`。