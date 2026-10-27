---
paths: ["cmd/**/*.go", "dapr/**/*.yaml", "deploy/**/*.yaml"]
---

# App ↔ URL 快查表

## 三个名字,三个命名空间(别混)

| 名字 | 定义在哪 | 例 |
|---|---|---|
| **aud 短名** | `cmdbootstrap.Options.AppID`(缺省即 `Audience`);authd `JWT_AUDIENCE` 收录它 | `pricing` |
| **dapr app-id** | `dapr run --app-id`;deployer nginx map 的**值** | `supertrade-pricing` |
| **公网前缀** | nginx map 的 **key**(URL 段) | `/api/v1/pricing/` |

链路:`公网前缀` →(nginx map)→ `dapr app-id` →(sidecar 找进程)→ 应用层校验 `aud`。

⚠️ aud 与 dapr app-id 混用 ⇒ **401 `invalid_audience`**(不是 403,极易误判成"登录过期")。
真源:`../deployer/docs/service-accounts.md` §接入步骤 3 —— aud 用短名,不是 dapr app-id。

## 本仓 dapr app(公网可达)

| cmd | aud 短名 | dapr app-id | 公网前缀 |
|---|---|---|---|
| `cmd/catalog` | `catalog` | `supertrade-catalog` | `/api/v1/catalog/` |
| `cmd/stocktake` | `stocktake` | `supertrade-stocktake` | `/api/v1/stocktake/` |
| `cmd/inventory` | `inventory` | `supertrade-inventory` | `/api/v1/inventory/` |
| `cmd/cube-router` | `cube-router` | `supertrade-cube-router` | `/api/v1/cube-router/` |
| `cmd/master-data` | `master-data` | `supertrade-master-data` | `/api/v1/suppliers/` ⚠ |
| `cmd/notification-gateway` | `notification-gateway` | `supertrade-notification-gateway` | `/api/v1/notification/` ⚠ |
| `cmd/fresh-meat` | `fresh-meat` | `supertrade-fresh-meat` | `/api/v1/fresh-meat/` |
| `cmd/settlement-auditor` | `settlement-auditor` | `supertrade-settlement-auditor` | `/api/v1/settlement-auditor/` |
| `cmd/sales-agg` | `sales-agg` | `supertrade-sales-agg` | `/api/v1/sales-agg/` |
| `cmd/pricing` | `supertrade-pricing` ⚠ | `supertrade-pricing` | `/api/v1/pricing/` |
| `cmd/sales-insight` | `sales-insight` | `supertrade-sales-insight` | `/api/v1/sales-insight/` |
| `cmd/member` | `pos` ⚠ | `supertrade-member` | `/api/v1/member/` |
| `cmd/pos` | `pos` | `supertrade-pos` | `/api/v1/pos/` |
| `cmd/pos-gateway` | `pos` ⚠ | `supertrade-pos-gateway` | `/api/v1/pos-gateway/` |
| `cmd/transaction` | `transaction` | `supertrade-transaction` | `/api/v1/transaction/` |
| `cmd/procurement` | `procurement` | `supertrade-procurement` | `/api/v1/procurement/` |

⚠ = 与其它行不同,原因已查清,改动前先读下面两行注:

- **`cmd/pricing`**:aud 写成了 dapr app-id(`supertrade-pricing`),与"aud 用短名"的约定相反
  ⇒ 若 authd `JWT_AUDIENCE` 收录的是 `pricing`,pricing 端点会 401 `invalid_audience`。
  修法:`cmd/pricing/main.go` 的 `AppID` 改成 `pricing`(`Audience` 显式写 `pricing`),
  同时确认生产 `JWT_AUDIENCE` 里有 `pricing`。
- **`cmd/member` / `cmd/pos-gateway`**:显式 `Audience: "pos"` —— 这两个服务的 aud 是 `pos`,
  不是自己的名字。改 aud 前先确认调用方 token 的 `aud` 里有没有 `pos`。

## 本仓 dapr app(不经公网)

| cmd | 用途 | 路由 |
|---|---|---|
| `cmd/cube-sync` | 定时全量/增量档案同步 | 仅 Dapr 内部路由(`RegisterDapr`) |
| `cmd/cube-async` | 异步 cube 作业 | 仅 Dapr 内部路由(`RegisterDapr`) |

## 占位 cmd(未实现,不要往里加业务)

| cmd | 占位内容 |
|---|---|
| `cmd/bi-gateway` | `/dashboard/daily` 返回 placeholder |
| `cmd/erp-connector` | `/pull` `/sync-logs` 返回 placeholder |
| `cmd/fresh-produce` | 蔬果盘点端点返回 placeholder |

## 一次性 CLI(不是 dapr app,没有 AppID / 没有 HTTP 路由)

| cmd | 用途 |
|---|---|
| `cmd/stocktake-seed` | 一次性灌种子数据 |
| `cmd/fresh-meat-seed` | 一次性灌种子数据 |

## 外部仓的 app(不在本仓,但本仓会调)

| dapr app-id | 在哪 | 本仓调用入口 |
|---|---|---|
| `userd` | `../auth` | `authkit/userinfo`(`userinfo.New("userd")`) |
| `login` | `../auth`(不走 dapr,nginx 旁路) | 前端直连 nginx `/api/v1/login/auth/...` |
| `cube-gateway` | `../cube` | `internal/cubeclient` / `internal/cubehttp`(实际经 cube-router) |

## 加新 app 的清单

1. `cmd/<name>/main.go`:`AppID` 写 **aud 短名**。
2. deployer nginx `map $uri $dapr_appid` 加一行:`~^/api/v1/<prefix>/ "<dapr-app-id>";`。
3. authd `JWT_AUDIENCE` 加 **aud 短名**(短名!不是 dapr app-id)。
4. 本表加一行。
