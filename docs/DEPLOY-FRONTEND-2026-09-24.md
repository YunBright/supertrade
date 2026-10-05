# DEPLOY + FRONTEND — 2026-09-24 branch 重构 + cube 多源 + 本地表 + 三元权限

> **历史记录**:2026-09-24 的 cube-router / catalog 本地表 / authkit 三元权限 / X-Branch-ID 迁移已合入并投产。
> 本文件只保留迁移当时的影响面与上线 smoke 摘要;新工作请看下方 canonical 文档。

## Canonical 文档

- [REQUIREMENTS.md](REQUIREMENTS.md) — 业务契约与权限模型
- [DESIGN.md](DESIGN.md) — 微服务拆分
- [FIELD_SPEC.md](FIELD_SPEC.md) — 字段契约
- [EVENT-CATALOG.md](EVENT-CATALOG.md) — Dapr 事件契约
- [`.claude/rules/00-app-catalog.md`](../.claude/rules/00-app-catalog.md) — dapr app 快查表

## 4 个 PR 一句话总结

- PR 1 — 新增 `cube-router` dapr app,按 X-Branch-ID 路由到具体 cube 实例
- PR 2 — `catalog` 加本地 suppliers/products 表,`/products/search` 走"本地 + cube 兜底"
- PR 3 — `authkit` 三元权限(effective scopes / branches / default branch),stocktake / inventory 加 RequireBranch 中间件
- PR 4 — 文档同步 + 占位 cmd 保留(通知 / llm-gw 由外部服务承担)

## 前端关键改动(已固化到 flutter 仓)

- 所有业务请求必带 `X-Branch-ID` header(后端 middleware 注入 ctx)
- `/stocktake/products/search` → `/catalog/products/search`(扫条码入口迁移)
- `master-data` 不再提供 `/suppliers`,供应商改走 catalog
- `meController.handleAccessChanged` 增量支持 `changed_branches` / `changed_default_branch`
- `PermissionGate` 从 `JWT.scopes` 改为 `me.effectiveBranches[branchId].scopes`
- 错误响应统一形如 `{"code": "...", "message": "..."}`

## 上线 smoke(已固化到 e2e)

1. `curl -H "X-Branch-ID: S001" -H "Authorization: Bearer …" /api/v1/cube-router/v1/load` → 命中 sixun-hbposv7
2. 未带 X-Branch-ID 调 `/catalog/suppliers` → 400 `branch_required`
3. 带 X-Branch-ID + 无 `supplier:view` → 403 `forbidden`
4. 扫码查商品 → `/catalog/products/search?barcode=…` 正常返回
5. admin 改用户 scope → 自动发 `access_changed` → 前端 ws 收到 → PermissionGate 重评
6. admin 在 cube-router admin 加新门店映射 → 该门店 `/v1/load` 自动生效

完整端点清单 / 前端示例代码 / nginx map 模板 → 见 canonical 文档。
