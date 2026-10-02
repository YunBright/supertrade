# YunBright SuperTrade — Backend Monorepo

> Dapr + Gin + PostgreSQL 后端服务网格,为前端 POS / 盘点 / BI 提供业务 API。
> 本仓负责业务实现;`auth`(userd + authkit)、`cube`(思迅 ERP 兼容层)、`deployer`(nginx 入口与 contract tests)由同级仓提供。

---

## 目录

- [项目总览](#项目总览)
- [技术栈](#技术栈)
- [生产架构:nginx + Dapr 服务网格](#生产架构nginx--dapr-服务网格)
- [仓库目录结构](#仓库目录结构)
- [服务清单 (App ↔ URL)](#服务清单-app--url)
- [本地启动与调试](#本地启动与调试)
- [前端联调 API 路由](#前端联调-api-路由)
- [鉴权模型 (Auth)](#鉴权模型-auth)
- [分店上下文 X-Branch-ID](#分店上下文-x-branch-id)
- [跨服务调用 (Dapr)](#跨服务调用-dapr)
- [事件总线 (Dapr Pub/Sub)](#事件总线-dapr-pubsub)
- [数据库](#数据库)
- [测试](#测试)
- [关联文档](#关联文档)
- [修订记录](#修订记录)

---

## 项目总览

**模块路径**:`github.com/YunBright/supertrade`(Go 1.26+)

**定位**:零售门店运营后端,基于 Dapr 服务网格的微服务集合。

**本期业务域**:POS / 采购 / 盘点 / 库存 / 蔬果 / 生肉 / 定价 / 通知 / BI。

**关键约定**(全文适用):

- **业务实现不写身份与权限**:身份与权限完全委托 `../auth` 仓的 `userd` + `authkit` 包。
- **跨服务调用全部经 Dapr sidecar**:禁止手拼 `/v1.0/invoke/...` URL,统一走 `dapr/go-sdk`。
- **不写 Docker / Makefile / 启动脚本**:本地用 `dapr run` + `go run` 直接跑。
- **DB 迁移走 GORM AutoMigrate**,服务启动时自带;不写独立 SQL migration 文件。
- **branch 上下文唯一来源 `X-Branch-ID` header**:`/api/v1/<app>/<rest>` 的 URL 不再带 `:branch_id`。

**当前实现的服务(4 个)**:stocktake / catalog / inventory / cube-router。
**已订阅 pub/sub 的服务(2 个)**:stocktake(订阅 `auth.user.access_changed`)+ notification-gateway(订阅 stocktake.* + auth.user.*)。

其余 10 个 `cmd` 目录(pos-gateway / pos / pricing / procurement / sales-agg / fresh-produce / fresh-meat / erp-connector / bi-gateway / master-data)保留为后续 Sprint 入口骨架。
`notification`(企微/钉钉占位)与 `llm-gw`(LLM 网关占位)已于 2026-09-29 删除 — 占位 cmd 不再保留,这两个能力由外部服务承担。

---

## 技术栈

| 类别 | 选型 |
|---|---|
| 语言 | Go 1.26+ |
| Web 框架 | [Gin](https://github.com/gin-gonic/gin) v1.10 |
| 微服务编排 | [Dapr](https://dapr.io/) 1.14+ (standalone / no docker) |
| 跨服务 SDK | [`dapr/go-sdk`](https://github.com/dapr/go-sdk) v1.15(gRPC over `:50001`) |
| ORM | [GORM](https://gorm.io/) v1.25 |
| 数据库 | PostgreSQL 13+(每 dapr app 一个 schema) |
| 缓存 / PubSub | Redis(本地 `dapr init` 自带) |
| 鉴权 | JWT,`authkit` v0.2(`claims` / `rbac` / `userinfo`) |
| WebSocket | [`gorilla/websocket`](https://github.com/gorilla/websocket) v1.5 |
| 金额 | [`shopspring/decimal`](https://github.com/shopspring/decimal) v1.4 |
| 测试 | `stretchr/testify` v1.11 |

---

## 生产架构:nginx + Dapr 服务网格

生产环境的请求路径固定为:

```
浏览器 / 移动端
   │
   │  HTTPS + Authorization: Bearer <jwt>
   │  + X-Branch-ID: <branch>           ← 分店上下文(可空 / 单值 / 逗号列表 / "*")
   ▼
nginx (`../deployer/nginx/yun-bright.conf`)
   │
   │  map $uri $dapr_appid             ← 把 <app> 段映成 dapr app-id
   │  rewrite ^/api/v1/[^/]+/(.*)$ /$1 break
   │  proxy_set_header dapr-app-id     $appid;
   │  proxy_pass http://dapr_sidecar;
   ▼
dapr sidecar (`:3500`)
   │  JWT 验签 → aud 校验 → per-branch scope 校验(走 userd /internal/users/{id}/permissions)
   │  业务 HTTP 转发到目标 backend
   ▼
Go backend (`cmd/<app>` 启动的进程)
```

**关键事实**:

- 浏览器**只跟 nginx 通信**,不知道 dapr sidecar 存在。
- dapr sidecar 在请求进入 Go 进程前已完成 JWT 验签;Go handler 不再验签,直接 `claims.FromContext(c)` 取 claims。
- 所有 Go backend 监听 `:8080`(可用 `APP_PORT` env 改)。
- `dapr run` 自动注入 `DAPR_GRPC_PORT`(默认 `:50001`)给 Go 进程;**不读 `DAPR_ENDPOINT`**。

**公网 URL 形式**(全仓唯一):

```
/api/v1/<app>/<rest>
```

- `<app>` 是 dapr app-id,见 [服务清单](#服务清单-app--url)。
- `<rest>` 是 handler 在 `RegisterRoutes` 里注册的路径,**不再带 `<app>` 前缀**(nginx 已剥)。
- 例:`POST /api/v1/stocktake/stocktake-headers` 对应 `internal/stocktake/handler/handler.go::RegisterRoutes` 里的 `r.POST("/stocktake-headers", ...)`。

---

## 仓库目录结构

```
supertrade/
├── cmd/                              # 每个 dapr app 一个 cmd 子目录
│   ├── stocktake/  main.go           # 盘点管理(本期重点)
│   ├── catalog/    main.go           # 商品 / 供应商(本地表 + cube 兜底)
│   ├── inventory/  main.go           # cube stock 转发
│   ├── cube-router/main.go           # 多 cube 实例 per-branch 路由
│   ├── notification-gateway/main.go  # WebSocket 推送网关
│   ├── pos-gateway/main.go           # BFF 骨架(占位)
│   └── ... (其它 9 个占位 cmd)
│
├── internal/                          # 各 cmd 对应的内部包(handler / service / model / db)
│   ├── stocktake/
│   │   ├── handler/handler.go        # 业务路由注册(RegisterRoutes)
│   │   ├── handler/subscribe.go      # Dapr pub/sub 订阅(/dapr/subscribe + /events/<topic>)
│   │   ├── service/service.go
│   │   ├── model/model.go
│   │   └── db.go
│   ├── catalog/handler/{handler,supplier,product}.go
│   ├── cube-router/handler/{handler,proxy}.go
│   ├── cubeclient/                   # cube 客户端(DaprCubeClient + InMemoryClient)
│   ├── cubehttp/handler.go           # inventory 用的 cube HTTP 转发器
│   ├── notification-gateway/         # WS Hub / Registry / TenantRouter
│   └── ...
│
├── pkg/                              # 跨服务共享的本仓包
│   ├── cmdbootstrap/bootstrap.go     # 启动骨架(15 个 cmd 共用)
│   ├── cmdbootstrap/port.go          # APP_PORT 解析
│   └── middleware/x_branch_id.go     # X-Branch-ID header 中间件
│
├── docs/                             # 设计 + 部署契约文档
│   ├── DESIGN.md                     # 微服务拆分 + 技术方案
│   ├── REQUIREMENTS.md               # 业务需求
│   ├── FIELD_SPEC.md                 # 字段契约
│   ├── EVENT-CATALOG.md              # Dapr pub/sub 事件契约
│   └── DEPLOY-FRONTEND-2026-09-24.md # 前端对接 + 部署步骤
│
├── dapr/components/                  # Dapr 组件定义(state / pubsub / bindings)
├── .claude/rules/                    # Claude 开发规约(00~04)
├── go.mod / go.sum
└── README.md                         # 本文件
```

---

## 服务清单 (App ↔ URL)

下表覆盖本仓 15 个 cmd(14 个 dapr app + 1 个 BFF)+ 2 个外部依赖。

| `cmd` 目录 | dapr app-id | 公网 URL 前缀 | 端口(本仓默认值) | 实现状态 |
|---|---|---|---|---|
| `cmd/stocktake` | `stocktake` | `/api/v1/stocktake/` | `:8106` | ✅ 完整 |
| `cmd/catalog` | `catalog` | `/api/v1/catalog/` | `:8103` | ✅ 完整 |
| `cmd/inventory` | `inventory` | `/api/v1/inventory/` | `:8105` | ✅ 完整(stock 转发) |
| `cmd/cube-router` | `cube-router` | `/api/v1/cube-router/` | `:8107` | ✅ 完整 |
| `cmd/notification-gateway` | `notification-gateway` | `/api/v1/notification-gateway/` | `:8101` | ✅ WS / events |
| `cmd/pos-gateway` | `pos-gateway` | `/api/v1/pos-gateway/` | `:8080` | 🟡 BFF 骨架 |
| `cmd/pos` | `pos` | `/api/v1/pos/` | `:8080` | 🟡 占位 |
| `cmd/pricing` | `pricing` | `/api/v1/pricing/` | `:8080` | 🟡 占位 |
| `cmd/procurement` | `procurement` | `/api/v1/procurement/` | `:8080` | 🟡 占位 |
| `cmd/sales-agg` | `sales-agg` | `/api/v1/sales-agg/` | `:8080` | 🟡 占位 |
| `cmd/fresh-produce` | `fresh-produce` | `/api/v1/fresh-produce/` | `:8080` | 🟡 占位 |
| `cmd/fresh-meat` | `fresh-meat` | `/api/v1/fresh-meat/` | `:8080` | 🟡 占位 |
| `cmd/erp-connector` | `erp-connector` | `/api/v1/erp-connector/` | `:8080` | 🟡 占位 |
| `cmd/bi-gateway` | `bi-gateway` | `/api/v1/bi-gateway/` | `:8080` | 🟡 占位 |
| `cmd/master-data` | `master-data` | `/api/v1/master-data/` | `:8080` | 🟡 占位 |

**外部依赖**(不在本仓,但业务会调):

| AppID | 来源仓 | 调用入口 |
|---|---|---|
| `userd` | `../auth` | `authkit/userinfo`(`userinfo.New("userd")`) |
| `login` | `../auth`(不走 dapr,nginx 旁路) | 前端直接走 nginx `/api/v1/login/auth/...` |
| `cube-gateway` | `../cube` | `internal/cubehttp` / `internal/cubeclient`;**实际经 `cube-router` 多源路由** |

---

## 本地启动与调试

### 前置条件

| 依赖 | 版本 / 说明 |
|---|---|
| Go | 1.26+ |
| PostgreSQL | 13+(各 dapr app 共用一库,各自一个 schema) |
| Redis | 任意(本地 `dapr init` 自带) |
| Dapr CLI | 1.14+(`dapr init` 一次性) |
| authkit | `replace github.com/YunBright/authkit => ../authkit`(已在 go.mod) |

### 环境变量

| 变量 | 必填 | 说明 |
|---|---|---|
| `POSTGRES_DSN` | ✅ | PostgreSQL 连接串;缺失则启动失败 |
| `CUBE_CLIENT_MODE` | ❌ | `memory`(默认,本地 mock)/ `dapr`(经 sidecar) |
| `CUBE_APP_ID` | ❌ | 默认 `cube-router`(多源路由) |
| `APP_PORT` | ❌ | 监听端口;默认 `:8080`;常用别名 `:8101 / :8103 / :8105 / :8106 / :8107` |
| `DAPR_GRPC_PORT` | ❌ | dapr run 自动注入;勿手设 |

### 启动顺序

```bash
# 1. dapr 一次性初始化
dapr init

# 2. 启外部依赖 userd(必须,stocktake / catalog 校验 per-branch scope 都要走它)
cd ../auth && dapr run --app-id userd --app-port 8081 \
  --components-path ./dapr/components -- go run ./cmd/userd &

# 3. 启本仓任一 dapr app(以 stocktake 为例)
POSTGRES_DSN="postgres://postgres@localhost/supertrade?sslmode=disable" \
dapr run --app-id stocktake --app-port 8106 \
  --components-path ~/.dapr/components -- \
  go run ./cmd/stocktake
```

### 健康检查

每个 dapr app 都暴露 `GET /healthz`(由 `pkg/cmdbootstrap` 自动注册,公开,不走鉴权):

```bash
curl http://localhost:8106/healthz
# {"app":"stocktake","at":"2026-09-25T10:00:00Z","status":"ok"}
```

### 跑测试

```bash
go test ./...                                       # 全量单元 + 集成测试
go test ./internal/stocktake/... -run TestSearch     # 单包 / 单 case
```

---

## 前端联调 API 路由

### 路由形态与生成约定

**生产路由**(浏览器实际请求):

```
/api/v1/<app>/<rest>
```

**handler 注册路径**:`/<rest>`(**无 `<app>` 前缀**,nginx 已剥)

- 例:`POST /api/v1/stocktake/stocktake-headers` → handler 注册 `r.POST("/stocktake-headers", ...)`

**添加新端点的 checklist**(详见 `.claude/rules/02-handler-routes.md`):

1. 在 `internal/<svc>/handler/<file>.go::RegisterRoutes` 加一行 + 实现 handler 方法。
2. nginx map **不动**(`<app>` 段没变)。
3. nginx `/deployer/tests/contract/<service>_test.go` **必须新增 contract test**——本仓与 deployer 仓**有契约测试**(end-to-end,通过公网 nginx 入口跑)。

### 路由速查(按需求分组)

> 列顺序按"业务入口/扫条码/盘点单/库存/供应商/admin/订阅/实时推送"分组。
> 完整字段定义见 `docs/FIELD_SPEC.md`;事件 payload 见 `docs/EVENT-CATALOG.md`。

#### A. 扫条码 / 商品查询

| 生产路由 | 方法 | 鉴权 | X-Branch-ID | 必填 scope | 路由注册位置 |
|---|---|---|---|---|---|
| `/api/v1/catalog/products/search` | `GET` | ✅ JWT | 必填 | `product:view` | [internal/catalog/handler/handler.go:106-108](../internal/catalog/handler/handler.go#L106-L108) |
| `/api/v1/catalog/products/:id` | `GET` | ✅ JWT | 必填 | `product:view` | [internal/catalog/handler/handler.go:109-111](../internal/catalog/handler/handler.go#L109-L111) |
| `/api/v1/stocktake/products/search` | `GET` | ✅ JWT | 必填 | `inventory:view` + `supplier:view` | [internal/stocktake/handler/handler.go:86](../internal/stocktake/handler/handler.go#L86) |

> ⚠ `/api/v1/stocktake/products/search` 与 `/api/v1/catalog/products/search` 字段对齐,**前端扫条码入口推荐用 catalog**。

#### B. 盘点单(stocktake)

| 生产路由 | 方法 | 鉴权 | X-Branch-ID | 必填 scope | 路由注册位置 |
|---|---|---|---|---|---|
| `/api/v1/stocktake/stocktake-headers` | `POST` | ✅ JWT | 必填(body) | `inventory:manage` | [internal/stocktake/handler/handler.go:69](../internal/stocktake/handler/handler.go#L69) |
| `/api/v1/stocktake/stocktake-headers` | `GET` | ✅ JWT | 必填(header) | `inventory:view` | [internal/stocktake/handler/handler.go:70](../internal/stocktake/handler/handler.go#L70) |
| `/api/v1/stocktake/stocktake-headers/search` | `GET` | ✅ JWT | 必填(header) | `inventory:view` | [internal/stocktake/handler/handler.go:72](../internal/stocktake/handler/handler.go#L72) |
| `/api/v1/stocktake/stocktake-headers/:id` | `GET` | ✅ JWT | 派生(header.branch) | `inventory:view` | [internal/stocktake/handler/handler.go:73](../internal/stocktake/handler/handler.go#L73) |
| `/api/v1/stocktake/stocktake-headers/:id/history` | `GET` | ✅ JWT | 派生 | `inventory:view` | [internal/stocktake/handler/handler.go:76](../internal/stocktake/handler/handler.go#L76) |
| `/api/v1/stocktake/stocktake-headers/:id/plan-items` | `GET` | ✅ JWT | 派生 | `inventory:view` | [internal/stocktake/handler/handler.go:77](../internal/stocktake/handler/handler.go#L77) |
| `/api/v1/stocktake/stocktake-headers/:id/plan-items` | `POST` | ✅ JWT | 派生 | `inventory:manage` | [internal/stocktake/handler/handler.go:78](../internal/stocktake/handler/handler.go#L78) |
| `/api/v1/stocktake/stocktake-headers/:id/lines` | `POST` | ✅ JWT | 派生 | `inventory:manage` | [internal/stocktake/handler/handler.go:74](../internal/stocktake/handler/handler.go#L74) |
| `/api/v1/stocktake/stocktake-lines/:id` | `PUT` | ✅ JWT | 派生 | `inventory:manage` | [internal/stocktake/handler/handler.go:82](../internal/stocktake/handler/handler.go#L82) |
| `/api/v1/stocktake/stocktake-lines/:id` | `DELETE` | ✅ JWT | 派生 | `inventory:manage` | [internal/stocktake/handler/handler.go:83](../internal/stocktake/handler/handler.go#L83) |
| `/api/v1/stocktake/stocktake-headers/:id/diff-report` | `GET` | ✅ JWT | 派生 | `inventory:view` | [internal/stocktake/handler/handler.go:75](../internal/stocktake/handler/handler.go#L75) |
| `/api/v1/stocktake/stocktake-headers/:id/submit` | `POST` | ✅ JWT | 派生 | `inventory:manage` | [internal/stocktake/handler/handler.go:79](../internal/stocktake/handler/handler.go#L79) |
| `/api/v1/stocktake/stocktake-headers/:id/approve` | `POST` | ✅ JWT | 派生 | `inventory:approve`(店长) | [internal/stocktake/handler/handler.go:80](../internal/stocktake/handler/handler.go#L80) |
| `/api/v1/stocktake/default-stocktake` | `GET` | ✅ JWT | 必填(header) | `inventory:view` | [internal/stocktake/handler/handler.go:89](../internal/stocktake/handler/handler.go#L89) |
| `/api/v1/stocktake/default-stocktake` | `PUT` | ✅ JWT | 必填(header) | `inventory:manage` | [internal/stocktake/handler/handler.go:90](../internal/stocktake/handler/handler.go#L90) |

#### C. 库存查询(inventory)

| 生产路由 | 方法 | 鉴权 | X-Branch-ID | 必填 scope | 路由注册位置 |
|---|---|---|---|---|---|
| `/api/v1/inventory/stock/:product_id` | `GET` | ✅ JWT | 必填(header) | `inventory:view`(走 `claims.AccessibleBranches` JWT snapshot) | [internal/cubehttp/handler.go:68](../internal/cubehttp/handler.go#L68) |

> 高频读端点用 JWT snapshot 守门(零 userd 调用);其它端点走 `rbac.RequireScopeWithBranch` 调 userd 实时校验。

#### D. 供应商(catalog)

| 生产路由 | 方法 | 鉴权 | X-Branch-ID | 必填 scope | 路由注册位置 |
|---|---|---|---|---|---|
| `/api/v1/catalog/suppliers` | `GET` | ✅ JWT | 必填(header) | `supplier:view` | [internal/catalog/handler/handler.go:89-91](../internal/catalog/handler/handler.go#L89-L91) |
| `/api/v1/catalog/suppliers/:id` | `GET` | ✅ JWT | 必填(header) | `supplier:view` | [internal/catalog/handler/handler.go:92-94](../internal/catalog/handler/handler.go#L92-L94) |
| `/api/v1/catalog/suppliers` | `POST` | ✅ JWT | 必填(header) | `supplier:manage` | [internal/catalog/handler/handler.go:95-97](../internal/catalog/handler/handler.go#L95-L97) |
| `/api/v1/catalog/suppliers/:id` | `PUT` | ✅ JWT | 必填(header) | `supplier:manage` | [internal/catalog/handler/handler.go:98-100](../internal/catalog/handler/handler.go#L98-L100) |
| `/api/v1/catalog/suppliers/:id` | `DELETE` | ✅ JWT | 必填(header) | `supplier:manage`(软删) | [internal/catalog/handler/handler.go:101-103](../internal/catalog/handler/handler.go#L101-L103) |

#### E. cube 多源路由转发(cube-router)

| 生产路由 | 方法 | 鉴权 | X-Branch-ID | 必填 scope | 路由注册位置 |
|---|---|---|---|---|---|
| `/api/v1/cube-router/v1/load` | `POST` | ✅ JWT | 必填(header) | `cube:read` | [internal/cube-router/handler/handler.go:94-97](../internal/cube-router/handler/handler.go#L94-L97),[internal/cube-router/handler/proxy.go](../internal/cube-router/handler/proxy.go) |

**业务侧必须经 cube-router**(不再直连 cube-gateway);按 `X-Branch-ID` 查 `branch_cube_sources` 表路由到正确的 cube 实例。

#### F. admin(cube-router)

| 生产路由 | 方法 | 鉴权 | X-Branch-ID | 必填 scope | 路由注册位置 |
|---|---|---|---|---|---|
| `/api/v1/cube-router/admin/branch-cube-sources` | `GET` | ✅ JWT | ❌ 不需要 | 角色 `admin`(由 `rbac.RequireRole("admin")` 守门) | [internal/cube-router/handler/handler.go:102](../internal/cube-router/handler/handler.go#L102) |
| `/api/v1/cube-router/admin/branch-cube-sources` | `POST` | ✅ JWT | ❌ 不需要 | 角色 `admin` | [internal/cube-router/handler/handler.go:103](../internal/cube-router/handler/handler.go#L103) |
| `/api/v1/cube-router/admin/branch-cube-sources/:branch_id` | `GET` | ✅ JWT | ❌ 不需要 | 角色 `admin` | [internal/cube-router/handler/handler.go:104](../internal/cube-router/handler/handler.go#L104) |
| `/api/v1/cube-router/admin/branch-cube-sources/:branch_id` | `PUT` | ✅ JWT | ❌ 不需要 | 角色 `admin` | [internal/cube-router/handler/handler.go:105](../internal/cube-router/handler/handler.go#L105) |
| `/api/v1/cube-router/admin/branch-cube-sources/:branch_id` | `DELETE` | ✅ JWT | ❌ 不需要 | 角色 `admin` | [internal/cube-router/handler/handler.go:106](../internal/cube-router/handler/handler.go#L106) |

#### G. BFF(占位骨架)

| 生产路由 | 方法 | 鉴权 | X-Branch-ID | 必填 scope | 路由注册位置 |
|---|---|---|---|---|---|
| `/api/v1/pos-gateway/v1/home` | `GET` | ✅ JWT(由 cmdbootstrap 强制) | ❌ 不需要 | — | [cmd/pos-gateway/main.go:18-25](../cmd/pos-gateway/main.go#L18-L25) |

#### H. 健康检查(公开)

| 生产路由 | 方法 | 鉴权 | X-Branch-ID | 必填 scope | 路由注册位置 |
|---|---|---|---|---|---|
| `/api/v1/<any-app>/healthz` | `GET` | ❌ 公开 | ❌ 不需要 | — | `pkg/cmdbootstrap/bootstrap.go:70`(每个 app 自动注册) |

#### I. 实时推送(WebSocket)

| 生产路由 | 协议 | 鉴权 | X-Branch-ID | 必填 scope | 路由注册位置 |
|---|---|---|---|---|---|
| `/api/v1/notification-gateway/ws` | WebSocket Upgrade | ✅ JWT(claims 中 `aud` 含 `notification-gateway`) | ❌ 不需要 | — | [cmd/notification-gateway/main.go:73](../cmd/notification-gateway/main.go#L73) |
| `/api/v1/notification-gateway/metrics` | `GET` | ✅ JWT(cmdbootstrap 默认) | ❌ 不需要 | — | [cmd/notification-gateway/main.go:76](../cmd/notification-gateway/main.go#L76) |
| `/api/v1/notification-gateway/dapr/subscribe` | `GET` | ❌ 公开(dapr sidecar 调) | ❌ | — | [internal/notification-gateway/events/dapr_subscription.go:73](../internal/notification-gateway/events/dapr_subscription.go#L73) |
| `/api/v1/notification-gateway/events/<topic>` | `POST` | ❌ 公开(dapr sidecar 推 CloudEvents) | ❌ | — | [internal/notification-gateway/events/dapr_subscription.go:74-77](../internal/notification-gateway/events/dapr_subscription.go#L74-L77) |

订阅 topic 清单:[`stocktake.line.added`, `stocktake.line.updated`, `stocktake.line.deleted`, `stocktake.header.submitted`, `stocktake.header.approved`, `stocktake.plan_item.added`, `auth.user.access_changed`](../internal/notification-gateway/events/dapr_subscription.go#L38-L46)

### 通用请求 / 响应头

| Header | 必填 | 说明 |
|---|---|---|
| `Authorization: Bearer <jwt>` | 业务端点必填,`/healthz` 公开 | JWT 由 userd 签发;sidecar 验签 |
| `X-Branch-ID: <branch>` | per-branch 端点必填 | 见下一节 |
| `Content-Type: application/json` | POST/PUT 必填 | — |

### 错误响应统一形如

```jsonc
{
  "code":    "forbidden",                 // 机器可读,前端 switch
  "message": "用户在该 branch 下无 ..."    // 给用户看的中文
}
```

| HTTP | code | 含义 |
|---|---|---|
| 400 | `bad_request` / `branch_required` / `branch_required` / `missing_param` / `bad_json` | 参数或 header 缺失 |
| 401 | `unauthenticated` | JWT 缺失或过期 |
| 403 | `forbidden` | 该用户在当前 branch 无 `xxx:view/manage` scope |
| 404 | `not_found` / `product_not_found` / `stocktake_header_not_found` / `cube_source_not_configured` / ... | 资源不存在 |
| 409 | `conflict` / `supplier_already_exists` | 资源冲突 |
| 502 | `cube_error` / `cube_unavailable` | cube 网关错误 |
| 503 | `userd_unavailable` / `cube_source_disabled` | 依赖服务不可用 |

### 契约测试

**本仓与 `../deployer` 仓之间有契约测试(contract tests / e2e)**。

- 文件位置:`../deployer/tests/contract/<service>_test.go`
- 形态:走公网 nginx 入口,assert 请求路径 / 方法 / 鉴权码 / 错误码符合本 README 表
- 添加新端点时,**必须**同步在 deployer 仓加对应 contract case;缺测试视同未完成

---

## 鉴权模型 (Auth)

### 三层链

```
nginx (HTTPS 终结)
  ↓  Authorization: Bearer <jwt>
dapr sidecar (验签 JWT + 解析 claims 到 ctx)
  ↓  aud 校验(由 cmdbootstrap/rbac.RequireAudience 强制)
  ↓  per-branch scope 校验(由 rbac.RequireScopeWithBranch / RequireBranch 强制)
Go handler (业务逻辑,不再验签)
```

### 鉴权中间件选型

| 中间件 | 来源 | 用途 |
|---|---|---|
| `claims.GinMiddleware()` | `authkit/claims` | 解析 JWT claims 到 ctx(Dapr sidecar 已验签,handler 不再验签) |
| `rbac.RequireAudience(<svc>)` | `authkit/rbac` | 全局,token 的 `aud` 必须含本服务名 |
| `rbac.RequireScopeWithBranch(scope, branchFn, resolver)` | `authkit/rbac` | 三元 `(user × branch × scope)` 校验,走 userd 实时权限矩阵 |
| `rbac.RequireBranch(branchFn, allowedFn)` | `authkit/rbac` | 高频只读,走 `claims.AccessibleBranches` JWT snapshot(零 userd 调用) |
| `rbac.RequireRole("admin")` | `authkit/rbac` | admin 端点守门 |
| `rbac.HasAnyScopeWithBranch(users)` | `authkit/rbac` | `RequireScopeWithBranch` 的 resolver 适配器 |

### Scope 命名

`<domain>:<action>`,全小写,如:

- `inventory:view` / `inventory:manage` / `inventory:approve`(店长)
- `supplier:view` / `supplier:manage`
- `product:view` / `product:manage`
- `cube:read`

### JWT 透传

跨 dapr app 调用时(SDK 模式),`Authorization` header 必须转 gRPC outgoing metadata:

```go
import "google.golang.org/grpc/metadata"
if bearer := c.Request.Header.Get("Authorization"); bearer != "" {
    ctx = metadata.AppendToOutgoingContext(ctx, "authorization", bearer)
}
```

dapr sidecar 把 gRPC metadata lowercase keys 映射到 outgoing HTTP headers。本仓 `cmd/<svc>/main.go` 已自带 `forwardBearerToOutgoing` 中间件。

---

## 分店上下文 X-Branch-ID

### Header 形态

| header 值 | 语义 | 调用方处理 |
|---|---|---|
| `<branch-id>`(单值,如 `S001` / `B001` / `01`) | 单店操作 | 直接用 |
| `B001,B002,03` | 多店并行(逗号分隔) | 迭代各 branch 跑业务 |
| `*` | 当前用户的全部 accessible branches | 调 `claims.AccessibleBranches` 展开 |

> **branch-id 是 VARCHAR(64) 自编码字符串**,不要求 UUID;只要与 DB `branches.id` 一致即可。

### 解析路径

```
浏览器 Header X-Branch-ID
  ↓
nginx (透传 header,不做处理)
  ↓
dapr sidecar (透传)
  ↓
Go handler
  ↓
middleware.XBranchID() (cmd/<svc>/main.go::registerRoutes 全局挂)
  ↓ 注入 ctx.Branches []string
  ↓ 不强制 header 存在,handler 自选 source
handler 调用 middleware.SingleBranchFromCtx(c) (单店) / BranchFromCtx(c) (多店)
```

**约束**(详见 `.claude/rules/02-handler-routes.md`):

- **禁止**把 `:branch_id` 放 path 里(已重构移除)。
- **禁止**从 `?branch_id=` query 兜底(已删除)。
- **禁止**从 `claims.DefaultBranchID` 兜底(已删除)。
- header 缺失时,由 handler 显式判 `400 branch_required`。

---

## 跨服务调用 (Dapr)

**唯一允许**:走 `dapr/go-sdk` 的 `InvokeMethodWithContent`(走 gRPC over `:50001`)。

```go
import dapr "github.com/dapr/go-sdk/client"

daprCli, err := dapr.NewClient()           // SDK 自动从 DAPR_GRPC_PORT 拿 sidecar
daprCli.InvokeMethodWithContent(ctx,
    "cube-router", "v1/load", "POST",
    &dapr.DataContent{ContentType: "application/json", Data: body})
```

**禁止**:

- ❌ 手拼 `http://localhost:3500/v1.0/invoke/<id>/method/<rest>` URL
- ❌ 硬编码目标 IP(`172.12.1.5:3000` 等)
- ❌ 绕过 dapr 直连(本地测试也必须 `dapr run` 起 sidecar)
- ❌ 读 `DAPR_ENDPOINT` env var

**cube-router 多源路由**:`catalog` / `inventory` / `stocktake.SearchProducts` 等所有需要 cube 的服务,把 `POST /v1/load` 经 SDK 转发到 `cube-router`,由其按 `X-Branch-ID` 查 `branch_cube_sources` 路由到对应 cube 实例(如 `sixun-hbposv7` / `sixun-ysx`)。

---

## 事件总线 (Dapr Pub/Sub)

完整事件契约见 [`docs/EVENT-CATALOG.md`](../docs/EVENT-CATALOG.md)。订阅清单必须严格对齐该文件 §1 topic 索引 + §2 payload schema。

### 本仓订阅

| 订阅服务 | topic | 用途 |
|---|---|---|
| `stocktake` | `auth.user.access_changed` | 失效 scope cache,下次校验重读 userd |
| `notification-gateway` | `stocktake.line.{added,updated,deleted}` | WS 推送盘点明细变动 |
| `notification-gateway` | `stocktake.header.{submitted,approved}` | WS 推送盘点单状态变更 |
| `notification-gateway` | `stocktake.plan_item.added` | WS 推送计划商品添加 |
| `notification-gateway` | `auth.user.access_changed` | WS 推送权限变更给前端,触发重新登录 |

### Dapr 订阅端点(每个订阅方)

| 端点 | 用途 | 注册位置 |
|---|---|---|
| `GET /dapr/subscribe` | 返回订阅清单 JSON(sidecar 启动时拉) | [internal/stocktake/handler/subscribe.go:54](../internal/stocktake/handler/subscribe.go#L54) / [internal/notification-gateway/events/dapr_subscription.go:73](../internal/notification-gateway/events/dapr_subscription.go#L73) |
| `POST /events/<topic>` | 接收 CloudEvents envelope | 同上 |

---

## 数据库

### 总体规则

- 每个 dapr app 一个 PostgreSQL schema;跨服务**禁止直连 DB**。
- 跨域查询走 `sales-agg` 视图 API 或同步落本地宽表。
- DB 迁移用 GORM AutoMigrate,服务启动自带,**不写独立 SQL migration 文件**。

### 本仓各服务 schema / 表

| 服务 | Schema | 关键表 |
|---|---|---|
| `stocktake` | `stocktake` | `stocktake_headers` / `stocktake_lines` / `stocktake_line_operations` / `stocktake_plan_items` / `stocktake_branch_defaults` |
| `catalog` | `catalog` | `suppliers`(复合主键 `(id, branch_id)`)/ `products`(复合主键) |
| `cube-router` | `cube_router` | `branch_cube_sources`(主键 `branch_id`) |

不维护(由 cube / auth 仓提供):products / stock / suppliers(原始 cube 数据)、users / roles / permissions(userd)。

---

## 测试

### 本仓测试

- **单元 + 集成测试**:Go `*_test.go`,运行 `go test ./...`
- **Mock 模式**:外部依赖(Dapr sidecar / cube / userd)用 fakeDaprClient / InMemoryClient mock;不写 httptest mock sidecar(SDK 走 gRPC over `:50001`)
- **不写** powershell / python / shell 测试脚本
- **不写** 启动脚本

### 契约测试(Contract / E2E)

本仓**与 deployer 仓之间有契约测试**,目的是端到端验证公网 nginx 入口的路由 + 鉴权 + 错误码契约。

- **位置**:`../deployer/tests/contract/<service>_test.go`
- **形态**:通过公网 nginx 入口跑(非直连 sidecar / backend)
- **覆盖**:每个 `internal/<svc>/handler/*.go` 注册的端点都必须在对应 `<service>_test.go` 至少一个 case(成功路径 + 关键错误路径)
- **加新端点的硬性要求**:同一 PR 必须同步加 contract case,缺测试 CI 拒收

---

## 关联文档

| 文档 | 内容 |
|---|---|
| [`docs/DESIGN.md`](docs/DESIGN.md) | 微服务拆分 + 技术方案 + 数据模型 + 风险对策 |
| [`docs/REQUIREMENTS.md`](docs/REQUIREMENTS.md) | 业务需求契约 |
| [`docs/FIELD_SPEC.md`](docs/FIELD_SPEC.md) | 字段契约(数据库字段 ↔ cube 字段) |
| [`docs/EVENT-CATALOG.md`](docs/EVENT-CATALOG.md) | Dapr pub/sub 事件 payload 契约 |
| [`docs/DEPLOY-FRONTEND-2026-09-24.md`](docs/DEPLOY-FRONTEND-2026-09-24.md) | 前端对接 + 部署步骤(branch 重构后) |
| [`.claude/rules/00-app-catalog.md`](.claude/rules/00-app-catalog.md) | App ↔ URL 快查表 |
| [`.claude/rules/01-cmd-bootstrap.md`](.claude/rules/01-cmd-bootstrap.md) | `cmd/<svc>/main.go` 入口规范 |
| [`.claude/rules/02-handler-routes.md`](.claude/rules/02-handler-routes.md) | Handler 路由注册 + X-Branch-ID 规范 |
| [`.claude/rules/03-cross-service.md`](.claude/rules/03-cross-service.md) | dapr/go-sdk 跨服务调用规范 |
| [`.claude/rules/04-nginx-map.md`](.claude/rules/04-nginx-map.md) | deployer nginx map 同步规范 |

---

## 修订记录

| 日期 | 修订人 | 内容 |
|---|---|---|
| 2026-09-25 | Tinkler | 初版;汇总 4 个完整服务(stocktake / catalog / inventory / cube-router)+ notification-gateway + 12 占位 cmd 的路由表;声明 deployer 仓契约测试 |
| 2026-09-29 | Tinkler | 删除 `cmd/notification` 与 `cmd/llm-gw` 占位 cmd(对应 .goreleaser.yaml / deploy-supertrade.ps1 / docs/EVENT-CATALOG / docs/DESIGN / .claude/rules);本仓现 14 个 dapr app + 1 BFF;LLM 网关 + 企微/钉钉通知改由外部服务承担 |