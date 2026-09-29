---
paths: ["**/*.go", "**/*.yaml", "**/*.conf"]
---

# App ↔ URL 快查表

每个 `cmd/<name>/main.go` 的 `AppID` = 公网 URL 里的 `<app>` 段。

## 本仓 14 个 dapr app

> 2026-09 新增 `cube-router`(按 X-Branch-ID 路由到不同 cube 实例)。
> 10 个占位 cmd(pos-gateway / pos / pricing / procurement / sales-agg / fresh-produce /
> fresh-meat / erp-connector / bi-gateway / master-data)作为后续 Sprint 入口保留。
> `notification`(企微/钉钉占位)与 `llm-gw`(LLM 网关占位)已于 2026-09-29 删除 —
> 占位 cmd 不再保留,这两个能力由外部服务承担。

| cmd 目录 | AppID | 公网前缀 |
|---|---|---|
| `cmd/pos-gateway` | `pos-gateway` | `/api/v1/pos-gateway/` |
| `cmd/pos` | `pos` | `/api/v1/pos/` |
| `cmd/stocktake` | `stocktake` | `/api/v1/stocktake/` |
| `cmd/catalog` | `catalog` | `/api/v1/catalog/` |
| `cmd/inventory` | `inventory` | `/api/v1/inventory/` |
| `cmd/cube-router` | `cube-router` | `/api/v1/cube-router/` |
| `cmd/master-data` | `master-data` | `/api/v1/master-data/` |
| `cmd/pricing` | `pricing` | `/api/v1/pricing/` |
| `cmd/procurement` | `procurement` | `/api/v1/procurement/` |
| `cmd/sales-agg` | `sales-agg` | `/api/v1/sales-agg/` |
| `cmd/fresh-produce` | `fresh-produce` | `/api/v1/fresh-produce/` |
| `cmd/fresh-meat` | `fresh-meat` | `/api/v1/fresh-meat/` |
| `cmd/erp-connector` | `erp-connector` | `/api/v1/erp-connector/` |
| `cmd/bi-gateway` | `bi-gateway` | `/api/v1/bi-gateway/` |
| `cmd/notification-gateway` | `notification-gateway` | `/api/v1/notification-gateway/` |

## 外部仓的 app（不归本仓管，但本仓业务会调）

| AppID | 在哪 | 本仓调用入口 |
|---|---|---|
| `userd` | `../auth` | nginx `/api/v1/userd/*` → `proxy_set_header dapr-app-id userd` → sidecar `:3500` proxy → userd:8082（header-based proxy mode,dapr 现代路径） |
| `login` | `../auth`（不走 dapr，nginx 旁路） | 不在库内直连；前端走 nginx |
| `cube-gateway` | `../cube` | `internal/cubehttp` / `internal/cubeclient` |

## URL 形式

```
/api/v1/<app>/<rest>
```

nginx `map $uri $dapr_appid` 把 `<app>` 段映成 dapr app-id，`rewrite` 剥前缀把 `<rest>` 转给 dapr-sidecar。

加新 app：只动 deployer/nginx/yun-bright.conf map 加一行；其它不动。