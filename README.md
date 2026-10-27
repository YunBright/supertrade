# YunBright SuperTrade — 后端微服务仓

> Dapr + Gin + GORM 的零售门店后端:商品 / 库存 / 盘点 / 生鲜 / 定价 / POS / 结算 / BI。
> 本仓只写业务;身份与权限在 `../auth`,ERP 语义层在 `../cube`,nginx 与**契约测试**在 `../deployer`。

---

## 1. 30 秒定位

- 一个 `cmd/<name>` = 一个 dapr app;对应 `internal/<name>`(handler / service / model)。
- 公网入口固定 `/api/v1/<app>/<rest>`;nginx 剥掉 `<app>` 前缀后转给 sidecar。
- **路由真源 = 各 `internal/<svc>/handler/handler.go::RegisterRoutes`**。
  本 README 不抄路由表(抄了就会腐化),要查路由直接看那个文件。

## 2. 目录结构

```
supertrade/
├── cmd/                 # 每个 dapr app 一个入口(22 个目录,含 2 个 seed CLI 与 3 个占位)
├── internal/            # 各 cmd 的内部包:handler / service / model / db
├── pkg/                 # 跨服务共享的本仓包
│   ├── cmdbootstrap/    # 启动骨架:装配顺序 / 验签 / 鉴权 / 访问日志 / 优雅退出
│   ├── eventbus/        # pub/sub 组件名(全仓唯一真相)
│   ├── middleware/      # X-Branch-ID 中间件
│   └── outbox/          # 发件箱(事件与业务行同事务落库)
├── docs/                # 规格与契约(见 §6)
├── dapr/components/     # Dapr 组件定义(pubsub / conversation)
└── .claude/rules/       # 开发规约(00~05)
```

## 3. 跑起来

```bash
dapr init                                   # 一次性

POSTGRES_DSN="postgres://postgres@localhost/supertrade?sslmode=disable" \
dapr run --app-id supertrade-stocktake --app-port 8106 \
  --components-path ./dapr/components \
  -- go run ./cmd/stocktake
```

- 外部依赖 `../auth` 的 userd 必须先起(per-branch scope 校验要走它)。
- 端口:`dapr run` 注入的 `APP_PORT` 优先;没有时用各 `cmd/<svc>/main.go` 里 `AppPort(...)` 的回退值。
- 健康检查:`GET /healthz`(公开,不走鉴权)。
- **不写** Docker / Makefile / 启动脚本 / shell 测试脚本。

### 环境变量

| 变量 | 必填 | 说明 |
|---|---|---|
| `POSTGRES_DSN` | ✅ | PostgreSQL 连接串;缺失则启动失败 |
| `APP_PORT` | ❌ | 监听端口;`dapr run` 自动注入,勿手设 |
| `CUBE_APP_ID` / `CUBE_QUERY_PATH` | ❌ | cube 客户端目标;两者必须成对,默认走 cube-router |
| `DAPR_PUBSUB` | ❌ | pub/sub 组件名;缺省用 `eventbus.Name` |

各服务私有变量(LLM 超时、时区、企微模板 id 等)在对应 `cmd/<svc>/main.go` 的 `OnStart` 里读。

## 4. 硬约束

> 改代码前先读这一节。每条都是**边界**或**安全红线**,不是风格偏好。

### 4.1 三个名字,三个命名空间(最容易混)

| 名字 | 出现在哪 | 例 |
|---|---|---|
| **aud 短名** | `cmdbootstrap.Options.AppID`;authd `JWT_AUDIENCE` 收录它 | `pricing` |
| **dapr app-id** | `dapr run --app-id`;deployer nginx map 的**值** | `supertrade-pricing` |
| **公网前缀** | nginx map 的 **key**(URL 段) | `/api/v1/pricing/` |

混用 aud 与 dapr app-id ⇒ **401 `invalid_audience`**(不是 403,极易误判成"登录过期")。
完整 app 表见 [`.claude/rules/00-app-catalog.md`](.claude/rules/00-app-catalog.md)。

### 4.2 鉴权链(分层,每层只做一件事)

```
nginx(HTTPS 终结) → dapr sidecar(验签 + 解析 claims) → cmdbootstrap(aud + 访问日志) → handler(业务 scope)
```

- `handler` **不验签、不解 token** —— 用 `claims.FromContext(c)`。
- per-branch scope 走 `rbac.RequireScopeWithBranch(...)`(userd 实时权限矩阵);JWT 里的 scopes 恒空,别用。
- 高频只读端点走 `rbac.RequireBranch(...)`(JWT `AccessibleBranches` 静态快照,零 userd 调用)。
- Dapr 内部路由(`/dapr/subscribe`、`/events/<topic>`)**必须在鉴权之前注册**(用 `Options.RegisterDapr`):
  sidecar 调它们不带 token,挂晚了整条 pub/sub 链路静默失效。

### 4.3 门店上下文

- branch 唯一来源 = `X-Branch-ID` header;**path 里不许出现 `:branch_id`**,query 里的 branch_id 只做一致性校验。
- `middleware.XBranchID()` 全局挂,不强制 header 存在;由 handler 决定 400。
- 多值语义:`01` 单店 / `01,02` 多店 / `*` 全部 accessible。

### 4.4 跨服务调用

- 一律走 `dapr/go-sdk`;**禁止**手拼 `/v1.0/invoke/<app-id>/method/<rest>`、禁止硬编码 IP、禁止直连目标进程。
- SDK 从 `DAPR_GRPC_PORT` 拿 sidecar;**不要**读 `DAPR_ENDPOINT`。
- 转发 JWT 要转 gRPC outgoing metadata(`metadata.AppendToOutgoingContext`)。

### 4.5 事件总线

- pub/sub 组件名只有 `eventbus.Name` 一个真相,各服务 components 的 `metadata.name` 必须逐字相同。
- 名字不一致 = 两条互不相通的 bus,事件静默丢失、全流程零报错。

### 4.6 安全红线

- **日志不落**:Authorization / Cookie / 请求体 / query string。path 只取路由模板。
- **panic 兜底不落请求头**;堆栈可打(不含请求数据)。
- **错误不透出 `err.Error()`**:含 gRPC 码与内部 app-id,对门店无意义;细节留服务端日志。
- **错误码是线上契约**:前端按 code 分支,不能改成通用 `not_found` / `conflict`。
- **权限点没 seed ⇒ fail-closed 403**(不是放行)。
- `/healthz` 公开;其余业务端点一律要登录态。

### 4.7 数据源路由

- 门店在 `branch_cube_sources` 配了数据源 ⇒ 走该门店的 cube 实例;没配 ⇒ 走本域 PG。
- **问不到路由 ⇒ 503,不回落本地**;cube 的 404 不等于"没有数据"(更常见的原因是"该店没配源")。
- 有源门店(档案在外部系统)的写操作一律拒 409。

### 4.8 金额与比率

- 金额 / 比率 / 积分**一律用字符串传输**(JSON number 到 Dart 会变 double,精度丢)。

## 5. 测试

```bash
go test ./...                                  # 全量
go test ./internal/fresh-meat/... -run TestX  # 单包 / 单 case
```

- **本仓只写分模块单元 / 集成测试**(`internal/<svc>/**/*_test.go`):httptest + fake 依赖
  (`internal/cubeclient/cubeclientfake`),不 mock dapr sidecar(SDK 走 gRPC)。
- **契约 / e2e 测试不进本仓** —— 走公网 nginx 入口的 contract case 在 `../deployer/tests/contract/`。
- 依赖外部系统的模块用 fake(`cubeclientfake` / `cubereadfake`),不连生产库。
- 断言要盯**失败形态**,不只盯成功形态:静默截断、字段缺失、级别错配、状态码错,都要有断言。

## 6. 文档索引

| 文档 | 作用 |
|---|---|
| [`docs/REQUIREMENTS.md`](docs/REQUIREMENTS.md) | 业务需求契约(唯一真相源) |
| [`docs/DESIGN.md`](docs/DESIGN.md) | 微服务拆分 + 技术方案 |
| [`docs/FIELD_SPEC.md`](docs/FIELD_SPEC.md) | 字段契约(DB ↔ cube) |
| [`docs/EVENT-CATALOG.md`](docs/EVENT-CATALOG.md) | Dapr pub/sub 事件契约(唯一真相源) |
| [`docs/PRD-SALES-INSIGHT-2026-10-16.md`](docs/PRD-SALES-INSIGHT-2026-10-16.md) | AI 销售日报/周报/月报 PRD |
| [`docs/FRESH-MEAT-CUT-LEVEL-COST-CHAIN-2026-10-27.md`](docs/FRESH-MEAT-CUT-LEVEL-COST-CHAIN-2026-10-27.md) | 鲜肉毛利**部位级**口径决议(在用) |
| [`docs/FRESH-MEAT-CUT-LEVEL-ROLLOUT-2026-10-27.md`](docs/FRESH-MEAT-CUT-LEVEL-ROLLOUT-2026-10-27.md) | 部位级改造上线说明 + 门店指引 |
| [`docs/FRESH-MEAT-DAY1-CHECKLIST-2026-10-25.md`](docs/FRESH-MEAT-DAY1-CHECKLIST-2026-10-25.md) | 门店第一天试跑清单 |
| [`docs/PLAN-2026-10-22-archive-transactions-reports.md`](docs/PLAN-2026-10-22-archive-transactions-reports.md) | 档案/流水/促销/采购/报表 跨仓契约 |
| [`docs/settlement-approval-items.md`](docs/settlement-approval-items.md) | 结算付款审批:收付款明细 |
| [`docs/settlement-approval-attachment.md`](docs/settlement-approval-attachment.md) | 结算付款审批:附件链路 |
| [`docs/catalog-product-picker-contract.md`](docs/catalog-product-picker-contract.md) | catalog 商品选择控件 4 个只读端点契约 |

### 开发规约(`.claude/rules/`)

| 文件 | 管什么 |
|---|---|
| [`00-app-catalog.md`](.claude/rules/00-app-catalog.md) | app ↔ aud ↔ dapr app-id ↔ 公网前缀 |
| [`01-cmd-bootstrap.md`](.claude/rules/01-cmd-bootstrap.md) | `cmd/<svc>/main.go` 入口规范 |
| [`02-handler-routes.md`](.claude/rules/02-handler-routes.md) | 路由注册 + X-Branch-ID + 加端点清单 |
| [`03-cross-service.md`](.claude/rules/03-cross-service.md) | dapr/go-sdk 跨服务调用 |
| [`04-nginx-map.md`](.claude/rules/04-nginx-map.md) | deployer nginx map 同步 |

## 7. 外部仓

| 仓 | 提供什么 |
|---|---|
| `../auth` | userd / authkit:身份、权限、JWT 签发与验签 |
| `../cube` | 思迅 ERP 语义层(cube) |
| `../deployer` | nginx 入口、systemd unit、**契约 / e2e 测试** |
| `../wechat-bot` | 企微出口(审批卡片、附件) |
| `../core` | `testrun` 等基础库 |
