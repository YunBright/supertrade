# DEPLOY + FRONTEND — 2026-09-24 branch 重构 + cube 多源 + 本地表 + 三元权限

> **配套文档**:
> - [REQUIREMENTS.md](REQUIREMENTS.md) — 业务契约与权限模型
> - [DESIGN.md](DESIGN.md) — 微服务拆分与技术方案
> - [FIELD_SPEC.md](FIELD_SPEC.md) — 字段契约
> - [EVENT-CATALOG.md](EVENT-CATALOG.md) — Dapr 事件契约
> - [`.claude/rules/00-app-catalog.md`](../.claude/rules/00-app-catalog.md) — 16 个 dapr app 快查表
>
> **变更范围**:PR 1 (cube-router + branch_cube_sources) / PR 2 (catalog 本地表)
> / PR 3 (authkit 三元权限) / PR 4 (docs sync + 占位 cmd 不删)。

---

## §1 部署步骤

### 1.1 前置条件

| 依赖 | 版本 / 说明 |
|---|---|
| Go | 1.26+ |
| PostgreSQL | 13+(所有 dapr app 共用一个 schema,各 app 自己一个 PG schema) |
| Redis | 任意(本机 `dapr init` 自带) |
| dapr CLI | 1.14+(`dapr init` 一次性) |
| authkit | 本地替换 `replace github.com/YunBright/authkit => ../authkit`(已在 go.mod,保留) |
| userd | auth 仓库的 userd 需运行,且 `/internal/users/{id}/permissions?branch_id=...` 端点可用 |

### 1.2 数据库迁移(各服务 GORM AutoMigrate)

每个 dapr app 启动时自带 GORM AutoMigrate(本仓约定,**不写独立 SQL migration**):
- `cmd/stocktake/main.go` → `stocktake_headers` / `stocktake_lines` / `stocktake_line_operations` / `stocktake_plan_items` / `stocktake_branch_defaults`
- `cmd/catalog/main.go` → `catalog.suppliers`(复合主键 `(id, branch_id)`) + `catalog.products`(复合主键)
- `cmd/cube-router/main.go` → `branch_cube_sources`(主键 `branch_id`)

**首次启动**:
```bash
# 建库
psql -U postgres -c "CREATE DATABASE supertrade;"
# 各 schema 由各 app AutoMigrate 自建,无需预建
```

### 1.3 启动顺序(本地)

```bash
# 1) dapr 初始化(一次性)
dapr init

# 2) 启 userd(auth 仓库,外部依赖)
cd ../auth && dapr run --app-id userd --app-port 8081 \
  --components-path ./dapr/components -- go run ./cmd/userd &

# 3) 启 cube-gateway(cube 仓库,外部依赖)
cd ../cube && dapr run --app-id cube-gateway --app-port 8082 \
  --components-path ./dapr/components -- go run ./cmd/cube-gateway &

# 4) 启本仓新增 / 改造的 4 个 app
cd ../supertrade

# 4a) cube-router(新增,PR 1)
#     注:SDK 自动从 DAPR_GRPC_PORT 拿 sidecar 地址,dapr run 注入,无需 DAPR_ENDPOINT
POSTGRES_DSN="postgres://postgres@localhost/supertrade?sslmode=disable" \
HTTP_LISTEN_PORT=":8116" \
dapr run --app-id cube-router --app-port 8116 \
  --components-path ~/.dapr/components -- \
  go run ./cmd/cube-router &

# 4b) catalog(改造,PR 2 — 加本地表 + 鉴权中间件)
POSTGRES_DSN="postgres://postgres@localhost/supertrade?sslmode=disable" \
dapr run --app-id catalog --app-port 8103 \
  --components-path ~/.dapr/components -- \
  go run ./cmd/catalog &

# 4c) inventory(改造,PR 3 — getStock 加 RequireBranch)
dapr run --app-id inventory --app-port 8102 \
  --components-path ~/.dapr/components -- \
  go run ./cmd/inventory &

# 4d) stocktake(改造,PR 3 — 13 端点 scope 守门 + 订阅 auth.user.access_changed)
#     publisher fail-fast:缺 sidecar → 启动退出非 0(必须先 dapr run)
POSTGRES_DSN="postgres://postgres@localhost/supertrade?sslmode=disable" \
dapr run --app-id stocktake --app-port 8106 \
  --components-path ~/.dapr/components -- \
  go run ./cmd/stocktake &

# 5) 启其它服务(按需,本期可不启)
# notification-gateway / 12 个占位 cmd ...
```

### 1.4 nginx map 同步(deployer 端,必备)

`../deployer/nginx/yun-bright.conf` 的 `map $uri $dapr_appid` 必须新增:

```nginx
map $uri $dapr_appid {
    ~^/api/v1/userd/         "userd";
    ~^/api/v1/cube/          "cube-gateway";
    ~^/api/v1/cube-router/   "cube-router";        # ← PR 1 新增
    ~^/api/v1/catalog/       "catalog";
    ~^/api/v1/inventory/     "inventory";
    ~^/api/v1/stocktake/     "stocktake";
    ~^/api/v1/notification-gateway/ "notification-gateway";
    # ... 12 占位 cmd 按 .claude/rules/00-app-catalog.md 全量保留 ...
    default "";
}
```

**检查**:reload nginx 后 `curl http://<host>/api/v1/cube-router/admin/branch-cube-sources -H "Authorization: Bearer <admin-token>"` 应返 200(空列表)。

### 1.5 初始化 cube 路由映射(admin 一次性)

```bash
# 假设分店 UUID = S001 的门店用 sixun-hbposv7 cube 实例
# 走公网 nginx 入口(推荐,header-based proxy mode):
curl -X POST https://<host>/api/v1/cube-router/admin/branch-cube-sources \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "branch_id": "S001",
    "cube_source_name": "sixun-hbposv7",
    "enabled": true
  }'
# → 201 Created
# 后续 stocktake / catalog / inventory 的 X-Branch-ID: S001 请求会自动路由到 sixun-hbposv7
#
# 直连 sidecar 仅供 host 内调试用 (同一台跑 cube-router 的机器):
#   curl -X POST http://localhost:3500/admin/branch-cube-sources \
#     -H "dapr-app-id: cube-router" ...
# dapr-app-id header 是 proxy mode 等价于 legacy /v1.0/invoke/<id>/method/<rest> URL 的现代写法。
```

### 1.6 健康检查与冒烟

```bash
# 各 app healthz(都返 200;不走鉴权)
# 走 nginx 入口(推荐):
for app in cube-router catalog inventory stocktake notification-gateway; do
  curl -s -o /dev/null -w "$app: %{http_code}\n" \
    https://<host>/api/v1/$app/healthz
done

# cube-router 转发冒烟
curl -X POST https://<host>/api/v1/cube-router/v1/load \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Branch-ID: S001" \
  -H "Content-Type: application/json" \
  -d '{"measures":["product.count"]}'
# → 200 + cube 真实响应(说明 cube_router 已正确路由到 sixun-hbposv7)

# catalog 鉴权冒烟(无 X-Branch-ID → 400 branch_required)
curl https://<host>/api/v1/catalog/suppliers \
  -H "Authorization: Bearer $TOKEN"
# → 400 branch_required

# catalog 鉴权冒烟(带 X-Branch-ID + 无 supplier:view → 403 forbidden)
curl https://<host>/api/v1/catalog/suppliers \
  -H "Authorization: Bearer $TOKEN" -H "X-Branch-ID: S001"
# → 403 forbidden
```

### 1.7 升级 / 回滚

| 场景 | 操作 |
|---|---|
| **首次上线**(从 PR 0 老版升级) | 按 §1.3 启动顺序启;老数据可丢(用户决策"不考虑对旧数据兼容");userd 需先升级到支持 `/internal/users/{id}/permissions` 的版本,否则 `userinfo.GetBranchPermissions` 返 503 |
| **回滚到 PR 0** | 关新 app,把所有 cmd 退回 `dapr run --app-id <old> -- go run ./cmd/<old>`;`branch_cube_sources` 表保留(无副作用);`catalog.suppliers/products` 保留(无副作用,stocktake 不读);userd 降级到旧版时 `userinfo.GetBranchPermissions` 自动返 ErrPermissionsUnavailable(中间件按 503 处理) |
| **分支权限变更** | admin 在 userd 改 scope / branch 后会自动发 `auth.user.access_changed` 事件;stocktake 订阅后失效 scope cache;前端 WebSocket 收到 `meController.load()` 重拉 `/auth/me` → UI 重新评估 PermissionGate |

---

## §2 前端对接说明(Flutter Web / 移动端)

### 2.1 必须新增的请求头

所有业务请求必须带 `X-Branch-ID`(合法 UUID):

```dart
// lib/core/http/supertrade_client.dart
class SupertradeClient {
  String? currentBranchId;  // 启动时从 /auth/me 的 default_branch_id 拿

  Map<String, String> buildHeaders({bool withBranch = true}) {
    final h = <String, String>{
      'Authorization': 'Bearer $accessToken',
      'Content-Type': 'application/json',
    };
    if (withBranch && currentBranchId != null) {
      h['X-Branch-ID'] = currentBranchId!;
    }
    return h;
  }
}
```

**注意事项**:
- ❌ 老代码里所有 `?branch_id=<uuid>` query 参数(原 stocktake.SearchHeaders 的兜底逻辑)保留兼容,但**优先用 header**(server-side middleware 注入 ctx,header 优先级最高)
- ✅ `X-Branch-ID` 支持**自编码字符串**(migration 009 起),不再是 UUID 形态;可以是 `01` / `B001` / `S001` 等 ≤ 64 字符的任意字符串,只要与 DB `branches.id` 一致即可
- ✅ `X-Branch-ID: 01,02,03` **逗号分隔**代表多店 union scopes(后端逐店拉 per-branch matrix 再 union 返 `branches: [...]` 明细)
- ✅ `X-Branch-ID: *` **通配符**代表该用户权限范围内的全部门店(auth 中间件层用 JWT.AccessibleBranches 展开;business service 永远拿到具体 branch_id list,不感知通配语义)
- ✅ 切换门店时同步更新 `currentBranchId`(后端所有响应按新门店 scope 校验)

### 2.2 错误码映射(统一收口)

| HTTP | code | 含义 | 前端处理 |
|---|---|---|---|
| 400 | `bad_request` / `branch_required` / `missing_branch_id` / `missing_param` | 请求参数 / header 缺失 | Toast 提示,引导重新选门店或检查 URL |
| 401 | `unauthenticated` | JWT 缺失或过期 | 跳登录 |
| 403 | `forbidden` | 该用户在当前 branch 无 `xxx:view/manage` scope | Toast「无权限,请联系店长」;触发 `meController.load()` 重拉 |
| 404 | `supplier_not_found` / `stocktake_header_not_found` / `cube_source_not_configured` / ... | 资源不存在 | Toast「资源不存在」;若 `cube_source_not_configured` → 提示联系 admin 配置分支 |
| 409 | `supplier_already_exists` / `conflict` | 资源冲突 | Toast「已存在」 |
| 503 | `userd_unavailable` / `cube_unavailable` / `cube_source_disabled` | 依赖服务不可用 | Toast「系统繁忙」+ 重试按钮;`userd_unavailable` 触发刷新 me 重试 |

错误响应统一形如:

```jsonc
{
  "code":    "forbidden",        // 机器可读,前端 switch
  "message": "用户在该 branch 下无 inventory:view scope"  // 给用户看的中文
}
```

### 2.3 端点变更清单(本期影响前端的)

#### 2.3.1 新增 / 迁移端点

| 端点 | 服务 | 变更 | 前端影响 |
|---|---|---|---|
| `POST /api/v1/cube-router/admin/branch-cube-sources` | cube-router | **新增** | admin 后台「门店 cube 配置」页 |
| `GET /api/v1/cube-router/admin/branch-cube-sources` | cube-router | **新增** | 同上列表 |
| `PUT /api/v1/cube-router/admin/branch-cube-sources/:branch_id` | cube-router | **新增** | 编辑 |
| `DELETE /api/v1/cube-router/admin/branch-cube-sources/:branch_id` | cube-router | **新增** | 删除 |
| `POST /api/v1/cube-router/v1/load` | cube-router | **新增**(替代各服务的 cube 直连) | 若前端有直接调 cube 的入口,改为调 cube-router |
| `GET /api/v1/catalog/suppliers` | catalog | **迁移**(原 master-data 占位转发 → catalog 本地表) | 把前端 `BASE_URL/api/v1/master-data/suppliers` 改成 `BASE_URL/api/v1/catalog/suppliers` |
| `POST /api/v1/catalog/suppliers` | catalog | **新增** | 供应商新增页 |
| `PUT /api/v1/catalog/suppliers/:id` | catalog | **新增** | 编辑 |
| `DELETE /api/v1/catalog/suppliers/:id` | catalog | **新增** | 删除(软删) |
| `GET /api/v1/catalog/products/search?barcode=...` | catalog | **迁移**(原 stocktake → catalog) | **扫条码** 入口从 `/stocktake/products/search` 改 `/catalog/products/search`,响应字段保持兼容 |

#### 2.3.2 鉴权变更端点(行为变化)

所有以下端点**新增** `X-Branch-ID` 必填校验(老版本可缺失),且响应 403 区分于 401:

| 端点 | 必填 scope | 业务模块 |
|---|---|---|
| `POST /api/v1/stocktake/stocktake-headers` | `inventory:manage`(仓管) | 创建盘点单 |
| `GET /api/v1/stocktake/stocktake-headers` | `inventory:view` | 列表 |
| `GET /api/v1/stocktake/stocktake-headers/search` | `inventory:view` | 搜索 |
| `GET /api/v1/stocktake/stocktake-headers/:id` | `inventory:view` | 详情 |
| `GET /api/v1/stocktake/stocktake-headers/:id/history` | `inventory:view` | 操作历史 |
| `GET /api/v1/stocktake/stocktake-headers/:id/plan-items` | `inventory:view` | 计划商品 |
| `POST /api/v1/stocktake/stocktake-headers/:id/plan-items` | `inventory:manage` | 加计划商品 |
| `POST /api/v1/stocktake/stocktake-headers/:id/lines` | `inventory:manage` | 录明细 |
| `PUT /api/v1/stocktake/stocktake-lines/:id` | `inventory:manage` | 改明细 |
| `DELETE /api/v1/stocktake/stocktake-lines/:id` | `inventory:manage` | 删明细 |
| `GET /api/v1/stocktake/stocktake-headers/:id/diff-report` | `inventory:view` | 差异表 |
| `POST /api/v1/stocktake/stocktake-headers/:id/submit` | `inventory:manage` | 提交 |
| `POST /api/v1/stocktake/stocktake-headers/:id/approve` | `inventory:approve`(店长) | 审核 |
| `GET /api/v1/stocktake/products/search` | `inventory:view` + `supplier:view`(组合) | 扫条码 |
| `GET /api/v1/stocktake/default-stocktake` | `inventory:view` | 查当前门店默认盘点单(branch 走 `X-Branch-ID` header) |
| `PUT /api/v1/stocktake/default-stocktake` | `inventory:manage` | 设置当前门店默认盘点单 |
| `GET /api/v1/inventory/stock/:product_id` | `inventory:view`(走 claims.AccessibleBranches;branch 走 `X-Branch-ID` header) | 查实时库存 |
| `GET /api/v1/catalog/suppliers[/:id]` | `supplier:view` | 查供应商 |
| `POST/PUT/DELETE /api/v1/catalog/suppliers[/:id]` | `supplier:manage` | 改供应商 |
| `GET /api/v1/catalog/products[/:id]` | `product:view` | 查商品 |
| `POST /api/v1/cube-router/v1/load` | `cube:read` | cube 多源转发 |

### 2.4 `meController` 增量更新逻辑(2026-09 事件优化)

```dart
// lib/core/me/me_controller.dart
class MeController extends GetxController {
  Future<void> handleAccessChanged(Map<String, dynamic> data) async {
    // 来自 wsmsg.TypeUserAccessChanged(data: 详见 docs/EVENT-CATALOG.md §2.14)
    final changedScopes    = (data['changed_scopes'] as List?)?.cast<String>();
    final changedBranches  = (data['changed_branches'] as List?)?.cast<String>();
    final changedDefault   = data['changed_default_branch'] == true;

    // 任一字段为空 → 完整重拉
    if ((changedScopes?.isEmpty ?? true) ||
        (changedBranches?.isEmpty ?? true)) {
      await load();
      _applyToUI();
      return;
    }
    // 增量 patch
    me.updateScopes(changedScopes!);
    me.updateBranches(changedBranches!);
    if (changedDefault) {
      me.defaultBranchId = data['branch_id'];
      SupertradeClient.instance.currentBranchId = me.defaultBranchId;
    }
    _applyToUI();
  }
}
```

### 2.5 PermissionGate 用法

```dart
// lib/widgets/permission_gate.dart
class PermissionGate extends StatelessWidget {
  final String scope;             // e.g. 'inventory:manage'
  final Widget child;
  final Widget? fallback;

  @override
  Widget build(BuildContext context) {
    final me = Get.find<MeController>().me;
    final branchId = SupertradeClient.instance.currentBranchId;
    if (me == null || branchId == null) return fallback ?? SizedBox.shrink();

    // 关键:从 per-branch 矩阵查(不是 JWT.scopes)
    final hasScope = me.effectiveBranches
        .firstWhere((b) => b.branchId == branchId, orElse: () => null)
        ?.scopes
        ?.contains(scope) ?? false;
    return hasScope ? child : (fallback ?? SizedBox.shrink());
  }
}

// 用法
PermissionGate(
  scope: 'inventory:approve',
  child: ElevatedButton(onPressed: _onApprove, child: Text('审核盘点单')),
)
```

### 2.6 WebSocket 推送(已有,本期无变更)

`notification-gateway` 的 WebSocket 推送行为不变;新增订阅:

- `cube.source.changed`(§EVENT-CATALOG.md §2.15)—— 当 admin 改了门店 cube 映射时触发,**前端无需监听**(由业务侧自己处理 cache)
- `auth.user.access_changed`(§2.14)—— 已在前端订阅,本期 payload 字段扩展(`changed_branches` / `changed_default_branch`),前端按 §2.4 处理

### 2.7 前端自测清单

部署后跑一遍:

1. **未登录调业务端点** → 401 → 跳登录
2. **登录后未带 X-Branch-ID 调 `/catalog/suppliers`** → 400 `branch_required` → Toast「请选择门店」
3. **登录后带 X-Branch-ID + 无 supplier:view** → 403 `forbidden` → Toast「无权限」
4. **登录后带 X-Branch-ID + 有 supplier:view** → 200 → 列表正常
5. **admin 改某用户的 supplier:view → 自动发 access_changed → 前端 ws 收到 → PermissionGate 重新评估**
6. **admin 在 cube-router admin 加新门店映射 → 该门店的 /v1/load 自动生效**
7. **扫码查商品** → `/catalog/products/search?barcode=...`(注意域名变 catalog)
8. **切换门店** → 更新 `currentBranchId` → 所有后续请求自动用新门店 scope

### 2.8 已知不兼容 / 必须前端改动

| 改动 | 影响范围 | 改造量 |
|---|---|---|
| 所有业务请求必带 `X-Branch-ID` header | 全局 http client | 小(集中改) |
| `/stocktake/products/search` → `/catalog/products/search` | 扫码模块 | 小(改 base path) |
| `master-data` 不再提供 `/suppliers` | 供应商相关页面 | 中(若用了 master-data,改 catalog) |
| `meController` 增量更新支持 `changed_branches` / `changed_default_branch` | 权限模块 | 小 |
| 错误响应统一用 `{code, message}` 形如,新增 7 种 code | 全局错误处理 | 小(集中 switch) |
| `PermissionGate` 从 JWT.scopes 改为 me.effectiveBranches[branchId].scopes | 权限组件 | 中(全量替换) |

---

## §3 关键代码引用

| 关注点 | 文件 |
|---|---|
| cube-router 启动 | [cmd/cube-router/main.go](../cmd/cube-router/main.go) |
| cube-router 路由 + admin | [internal/cube-router/handler/handler.go](../internal/cube-router/handler/handler.go) |
| cube-router 表 | [internal/cube-router/model/model.go](../internal/cube-router/model/model.go) |
| catalog 启动 | [cmd/catalog/main.go](../cmd/catalog/main.go) |
| catalog handler(scope 守门) | [internal/catalog/handler/handler.go](../internal/catalog/handler/handler.go) |
| inventory 启动 | [cmd/inventory/main.go](../cmd/inventory/main.go) |
| inventory /stock RequireBranch | [internal/cubehttp/handler.go](../internal/cubehttp/handler.go) |
| stocktake 启动(含 /dapr/subscribe) | [cmd/stocktake/main.go](../cmd/stocktake/main.go) |
| stocktake 13 端点 scope 守门 | [internal/stocktake/handler/handler.go](../internal/stocktake/handler/handler.go) |
| stocktake 订阅实现 | [internal/stocktake/handler/subscribe.go](../internal/stocktake/handler/subscribe.go) |
| X-Branch-ID 中间件 | [pkg/middleware/x_branch_id.go](../pkg/middleware/x_branch_id.go) |
| authkit 三元权限中间件 | `../authkit/rbac/scope_branch.go`(外部仓) |

---

## §4 修订记录

| 日期 | 修订人 | 内容 |
|---|---|---|
| 2026-09-24 | Tinkler | 初版,汇总 PR 1+2+3+4 部署步骤与前端对接要点;nginx map 同步 cube-router;前端必带 X-Branch-ID + 错误码映射 + PermissionGate 改 me.effectiveBranches |