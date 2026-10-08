# DEPLOY — fresh-meat 2026-10-01 优化(时区/下水/退货/盘点 per-SKU/报损持久化/毛利端点)

> **配套文档**:
> - [REQUIREMENTS.md](REQUIREMENTS.md) — 业务契约与权限模型
> - [DESIGN.md](DESIGN.md) — 微服务拆分与技术方案
> - [FIELD_SPEC.md](FIELD_SPEC.md) — 字段契约
> - [EVENT-CATALOG.md](EVENT-CATALOG.md) — Dapr 事件契约
> - [`.claude/rules/00-app-catalog.md`](../.claude/rules/00-app-catalog.md) — dapr app 快查表
>
> **变更范围**:Plan `reflective-floating-nova.md` Stage A→D 全部实装;29/29 单元测试通过。
>
> **契约不变项**:
> - 全链路不引入、不遗留 `tenant_id`(业务单租户,branch 即隔离边界)
> - 不发 `pig.arrived` 事件给 LLM 网关 — LLM 通过 Dapr Conversation API 同步调,无外部订阅方
> - `sales-agg` / `bi-gateway` 仍是空 stub cmd,**毛利由 fresh-meat 自供**(`GET /gross-margin`)
> - nginx `map $uri $dapr_appid` 不动 — `<app>` 段没变

---

## §1 部署步骤

### 1.1 前置条件

| 依赖 | 版本 / 说明 |
|---|---|
| Go | 1.26+ |
| PostgreSQL | 13+(`fresh_meat` schema 由 GORM AutoMigrate 自建,**无独立 SQL migration**) |
| Redis | 任意(`dapr init` 自带) |
| dapr CLI | 1.18+(`dapr init` 一次性;**1.18** 起 OPA + bearer 中间件语义生效,见 §1.6) |
| authkit | 本地替换 `replace github.com/YunBright/authkit => ../authkit`(go.mod 已有) |
| userd | auth 仓库,**必须运行**(`/internal/users/{id}/permissions?branch_id=...` 端点可用) |
| cube-gateway | cube 仓库,**必须运行**(fresh-meat 通过 cube-router 校验 supplier_id) |
| dapr Conversation 组件 | `FRESHMEAT_CONVERSATION` 默认 `conversation`;负责 LLM `predict_cuts` 推演 |

### 1.2 数据库迁移(GORM AutoMigrate)

`cmd/fresh-meat/main.go::initApp` 启动时跑:

```go
appDB.AutoMigrate(
    &model.WholePig{},
    &model.PigCut{},
    &model.PorkCutsStocktake{},
    &model.LineSalesByPig{},
    &model.BranchCutMapping{},
    &model.WasteLog{},   // ← 2026-10-01 新增
)
```

**新增字段(向后兼容,无破坏性 DDL)**:

| 表 | 新增列 | 类型 | 备注 |
|---|---|---|---|
| `whole_pigs` | `half_pig` | bool default false | 场景一(半头猪) |
| `whole_pigs` | `offal_included` | bool default false | 场景二(带全下水) |
| `whole_pigs` | `purchase_group_id` | varchar(32) + idx_branch_group | 场景三(同组多猪) |
| `line_sales_by_pig` | `unit_price_yuan` | numeric(10,2) | POS 已发,本服务之前丢弃 |
| `line_sales_by_pig` | `order_status` | varchar(2) default 'S' | "S"=销售 / "R"=退货 |
| `pork_cuts_stocktakes.cuts` JSON | `cube_product_id` | string | per-SKU 粒度 |

**新增表**:`fresh_meat.waste_logs`(11 字段 + 2 索引)。详见 [model/model.go](../internal/fresh-meat/model/model.go)。

**首次启动**:
```bash
# 库已建即可,schema 由 AutoMigrate 自建
psql -U postgres -c "CREATE DATABASE supertrade;"  # 首次
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

# 4) 启 fresh-meat(本仓)
cd ../supertrade
# 注意:生产 deployer systemd unit 引用了 /opt/YunBright/supertrade/apps/fresh-meat/components/
# 本地开发用 ~/.dapr/components/(`dapr init` 自带 pubsub.yaml / statestore.yaml)
dapr run --app-id fresh-meat --app-port 8108 \
  --components-path ~/.dapr/components \
  --config ~/.dapr/config.yaml \
  -- go run ./cmd/fresh-meat
```

> **关于 dapr/components 路径**(关键,务必看清楚):
> - 本仓 **`dapr/components/` 不存在**;`fresh-meat/main.go` 不读任何本地组件文件,
>   只通过 sidecar 拿(SDK 自动从 `DAPR_GRPC_PORT` 连 sidecar;sidecar 从 `--components-path` 加载组件)。
> - userd / cube-gateway 用的是**它们各自仓的** `./dapr/components/`(本仓不依赖)。
> - 本地开发:`dapr init` 后 `~/.dapr/components/` 自带 `pubsub.yaml`(redis) + `statestore.yaml`(redis);
>     缺 `conversation.yaml` 要新建,见 §1.5.2;`~/.dapr/config.yaml` 由 deployer 仓拷过来。
> - 生产:deployer 仓 `systemd/supertrade-fresh-meat.service` 写死:
>     - `--resources-path /opt/YunBright/supertrade/apps/fresh-meat/components`
>     - `--config /opt/YunBright/.dapr/config.yaml`
>     但 **deployer 仓当前不包含**这俩目录里的 yaml — 见 §3.2 与 §4.1(部署方需创建)。
```

### 1.4 环境变量(`cmd/fresh-meat/main.go` 读取)

| Env | 必填 | 默认 | 说明 |
|---|---|---|---|
| `POSTGRES_DSN` | **是** | — | `host=... user=... password=... dbname=supertrade port=5432 sslmode=disable` |
| `FRESHMEAT_BIZ_TZ` | 否 | `Asia/Shanghai` (UTC+8) | IANA 时区名;营业日界划线。`time.LoadLocation` 失败则 fallback 默认 |
| `FRESHMEAT_CONVERSATION` | 否 | `conversation` | Dapr Conversation 组件名(LLM 推演) |
| `FRESHMEAT_LLM_TIMEOUT` | 否 | `30s` | `time.ParseDuration`;LLM predict 超时,触发 history_avg 兜底 |
| `DAPR_GRPC_ENDPOINT` | 否 | dapr run 注入 | SDK 自动从 env 拿 sidecar 地址,**勿手设** |

**`fresh-meat` 启动失败模式**(fail-fast,确保部署方一眼定位):

| 启动日志关键字 | 原因 | 处理 |
|---|---|---|
| `POSTGRES_DSN 必填` | env 漏配 | 注入 DSN 后重启 |
| `open db: ...` | PG 不可达 / 凭证错 | 检查 PG 状态 + 凭证 |
| `dapr publisher: ...` | dapr sidecar 未起 / 端口错 | `dapr run --app-port` 与启动脚本一致 |
| `cube client: ...` | cube-router 未起 / 配置错 | `cubehttp.NewClient` 失败 |
| `FRESHMEAT_BIZ_TZ load failed` | IANA 名拼错 | fallback Asia/Shanghai,**不阻断**启动 |

成功启动日志:`fresh-meat app initialized` + `db_driver=postgres` + `schema="fresh_meat (6 tables)"` + `biz_tz="Asia/Shanghai"`。

### 1.5 Dapr 组件与 Configuration

fresh-meat 启动期必备的 dapr component / binding:

| 组件名 | 类型 | 用途 | 必需 |
|---|---|---|---|
| `pubsub` | `pubsub.redis` / `pubsub.kafka` | sale.completed 订阅 + pork.cuts.stocktaken / waste.log.recorded 发布 | **是** |
| `conversation` | `conversation.openai` / `conversation.local` | LLM predict_cuts 推演 | **是**(失败走 history_avg 兜底,不阻断) |
| `state` | `state.redis` | future use(目前未用,Optional) | 否 |
| `fresh-meat-cron` | `bindings.cron` | `GET /cron/daily-reminder`(阶段4 占位) | 否 |

**配置装载**(systemd unit 已写死):

```bash
--resources-path /opt/YunBright/supertrade/apps/fresh-meat/components  # Component 列表(pubsub/conversation/...)
--config        /opt/YunBright/.dapr/config.yaml                       # dapr Configuration(bearer middleware / mTLS / features)
```

> **当前状态(2026-10-02)**:deployer 仓的 `systemd/supertrade-fresh-meat.service` 引用了上述路径,
> 但 deployer 仓**不包含**这些 yaml(`apps/fresh-meat/components/` 目录与 `.dapr/config.yaml` 文件都不存在)。
> 部署方需**手工创建** — 见 §4.1。

#### 1.5.1 `pubsub.yaml`(必需)— Component

> **部署位置**:
> - 本地: `~/.dapr/components/pubsub.yaml`
> - 生产: `/opt/YunBright/supertrade/apps/fresh-meat/components/pubsub.yaml`
>   (deployer 仓需要新建该目录;systemd unit 写死 `--resources-path` 指向这)

**Redis**(本地或单 Redis 集群):
```yaml
apiVersion: dapr.io/v1alpha1
kind: Component
metadata:
  name: pubsub
  namespace: default
spec:
  type: pubsub.redis
  version: v1
  metadata:
    - name: redisHost
      value: "172.12.1.1:6379"   # 生产 Redis 地址
    - name: consumerID
      value: "{appID}"
    - name: enableDeadLetter
      value: "true"
    - name: deadLetterTTL
      value: "86400"
```

**Kafka**(生产可选):
```yaml
apiVersion: dapr.io/v1alpha1
kind: Component
metadata:
  name: pubsub
  namespace: default
spec:
  type: pubsub.kafka
  version: v1
  metadata:
    - name: brokers
      value: "kafka-0.kafka:9092,kafka-1.kafka:9092"
    - name: consumerID
      value: "{appID}"
    - name: authType
      value: "password"
    - name: saslUsername
      secretKeyRef: { name: kafka-creds, key: username }
    - name: saslPassword
      secretKeyRef: { name: kafka-creds, key: password }
```

> **注意**:`{appID}` 是 dapr sidecar 自动注入,勿手填;每个 app 的 consumerID 等于各自 `--app-id`。

**本服务订阅 + 发布的 topic**(硬编码在 [events.go](../internal/fresh-meat/service/events.go)):

| topic | 方向 | 来源 | 含义 |
|---|---|---|---|
| `sale.completed` | **订阅** | POS | POS 结账事件,落 `line_sales_by_pig` |
| `auth.user.access_changed` | **订阅** | userd | 用户权限变更,清本服务 scope 缓存 |
| `pork.cuts.stocktaken` | **发布** | fresh-meat | 日终盘点(is_complete=true 时发) |
| `waste.log.recorded` | **发布** | fresh-meat | 报损记录 |

#### 1.5.2 `conversation.yaml`(必需)— Component

> **部署位置**:`/opt/YunBright/supertrade/apps/fresh-meat/components/conversation.yaml`
> (deployer 仓新建)。
> 本服务 `main.go:113` 读 env `FRESHMEAT_CONVERSATION`,**默认组件名 `conversation`**。
> 缺失 → predict 调不通 → 全部走 `history_avg` 兜底(降级不阻断)。

**生产**(内网 LLM 网关,OpenAI 协议):
```yaml
apiVersion: dapr.io/v1alpha1
kind: Component
metadata:
  name: conversation
  namespace: default
spec:
  type: conversation.openai
  version: v1
  metadata:
    - name: endpoint
      value: "http://llm-gw.internal/v1"
    - name: apiKey
      secretKeyRef: { name: llm-gw-cred, key: apikey }
    - name: model
      value: "yunbright/predict-cuts-v1"
    - name: cacheTTL
      value: "30m"
```

**本地开发**(公网 OpenAI):
```yaml
apiVersion: dapr.io/v1alpha1
kind: Component
metadata:
  name: conversation
  namespace: default
spec:
  type: conversation.openai
  version: v1
  metadata:
    - name: endpoint
      value: "https://api.openai.com/v1"
    - name: apiKey
      secretKeyRef: { name: openai-cred, key: apikey }
    - name: model
      value: "gpt-4o-mini"
    - name: cacheTTL
      value: "30m"
```

> **注意**:之前 `pig.arrived` 事件走外部 LLM 网关异步推演的方式已**废弃**;
> 现通过 Dapr Conversation API **同步调**(`cmd/fresh-meat/main.go:123::DaprConversationPredictFn`)。

#### 1.5.3 `fresh-meat-config.yaml`(强烈推荐)— Configuration

> **部署位置**:`/opt/YunBright/.dapr/config.yaml`(systemd unit 写死 `--config` 指向这)
> 注意:**整个 `/opt/YunBright/.dapr/` 是所有 supertrade-* app 共享的**,不是 fresh-meat 独有。
> deployer 仓需新建该文件。

```yaml
apiVersion: dapr.io/v1alpha1
kind: Configuration
metadata:
  name: appconfig
  namespace: default
spec:
  tracing:
    samplingRate: "1"
  features:
    - name: conversation
      enabled: true
  nameResolution:
    component: "consul"
    configuration:
      client:
        address: "172.12.1.1:8500"
        scheme: "http"
      selfRegister: false
      queryOptions:
        useCache: true
      advancedRegistration:
        name: "${APP_ID}"
        port: ${APP_PORT}
        address: "172.12.1.5"
        check:
          name: "Dapr Health Status"
          checkID: "daprHealth:${APP_ID}"
          interval: "15s"
          http: "http://172.12.1.5:${APP_PORT}/healthz"
        meta:
          DAPR_METRICS_PORT: "${DAPR_METRICS_PORT}"
          DAPR_PROFILE_PORT: "${DAPR_PROFILE_PORT}"
        tags:
          - "dapr"
```

> 模板参考 deployer 仓已有 `dapr/baiyuan-config.yaml` / `dapr/gyy-config.yaml`
> (这两个是 Configuration 模板,**不是** Component — 别混淆)。

#### 1.5.4 组件加载失败排查

| 启动日志 | 原因 | 处理 |
|---|---|---|
| `pubsub not configured` | `apps/fresh-meat/components/pubsub.yaml` 缺失 | 见 §1.5.1 + §4.1 |
| `conversation not configured` | 组件缺失或名字不是 `conversation` | 确认 `FRESHMEAT_CONVERSATION` env 与组件 `metadata.name` 一致 |
| `subscribe to sale.completed: topic not allowed` | pubsub 类型不支持 | 换 redis/kafka |
| `publish 502 / context deadline exceeded` | sidecar 没拿到 pubsub endpoint | 检查 `pubsub.yaml` 里 `redisHost` / `brokers` 联通性 |
| `config not found` | `/opt/YunBright/.dapr/config.yaml` 缺失 | 见 §1.5.3 + §4.1 |

### 1.6 dapr 1.18 OPA / bearer 注意点

本仓库内存约束(`memory/dapr-opa-constraints.md`):
- **dapr 1.18 OPA 中** `http.send` 不可用;**io.jwt.verify_\*** 只吃 PEM 格式公钥;**bearer 中间件强校验 token**;
- userd 的 `claims.GinMiddleware` **只 parse 不 verify**(verify 在 sidecar bearer middleware 完成)。
- 部署 dapr 1.18 时,`fresh-meat` 端 bearer 中间件 `app-protocol: grpc` 不能绕过;若验签失败,nginx map 会 401/403,本服务**不会**看到请求。

### 1.7 nginx 与公网路由

不动。`.claude/rules/00-app-catalog.md` 已固化:
- AppID = `fresh-meat`
- 公网前缀 = `/api/v1/fresh-meat/`
- nginx `rewrite ^/api/v1/[^/]+/(.*)$ /$1 break` 剥前缀

---

## §2 依赖服务确认清单

部署方在 `fresh-meat` 上线前需逐项确认:

### 2.1 强依赖(missing 即启动失败)

| # | 服务 | 仓库 | 校验方式 | 失败现象 |
|---|---|---|---|---|
| 1 | **PostgreSQL** | 同机 / 同集群 | `psql "$POSTGRES_DSN" -c '\dt'` 能连 | `open db` 失败,fail-fast |
| 2 | **dapr sidecar** | dapr CLI | `dapr run --app-id fresh-meat --app-port 8080` 注入 `DAPR_GRPC_PORT` | `dapr publisher: ...` / `dapr client: ...` fail-fast |
| 3 | **userd** | `../auth` | `curl http://localhost:8081/healthz` 200 + `GET /internal/users/{id}/permissions?branch_id=...` 返回有效 scope | `freshmeat:view` / `freshmeat:write` scope 解析失败,handler `requireScope` 返 403 |
| 4 | **cube-gateway**(经 cube-router) | `../cube` + 本仓 `internal/cube-router` | `cubehttp.NewClient()` 启动通过 | `cube client: ...` fail-fast |
| 5 | **Dapr Conversation 组件** | dapr components | `dapr components-path` 下有 `conversation.yaml` | predict 调不通,降级 history_avg(`data_source="history_avg"`),**不阻断** |

### 2.2 软依赖(可选 / 故障兜底)

| # | 服务 | 缺失行为 | 日志关键字 |
|---|---|---|---|
| 6 | **Dapr Conversation LLM** | predict 失败 → `defaultPredictFn` 兜底 → `data_source="history_avg"` | `RecordWholePig: predictFn failed, fallback to history_avg` |
| 7 | **上一营业日 stocktake** | 无 → `opening_kg=0` → 毛利 `data_source="estimated"`(无 actual/variance) | 客户端可接受 |

### 2.3 上游事件源(订阅方需依赖本服务的发布)

| 事件 | 方向 | 订阅方 | 失败影响 |
|---|---|---|---|
| `pork.cuts.stocktaken` | **发** | sales-agg(标记"已盘点") | publish 失败仅 warn,DB 已提交 |
| `waste.log.recorded` | **发** | sales-agg(扣减预期库存) | 同上 |
| `auth.user.access_changed` | **收**(来自 userd) | 本服务清 scope 缓存 | 缓存陈旧最长 60s |
| `sale.completed` | **收**(来自 POS) | 本服务落 `line_sales_by_pig` | 落库失败 → 毛利 revenue 偏低 |

### 2.4 数据库表依赖

fresh-meat 启动期 `AutoMigrate` 自动建:`whole_pigs` / `pig_cuts` / `pork_cuts_stocktakes` / `line_sales_by_pig` / `branch_cut_mappings` / `waste_logs`(2026-10-01 新增)。

**重要**:`waste_logs` 是新表,首次启动若权限不足 `CREATE TABLE`,AutoMigrate 失败 → 启动 fail-fast。

### 2.5 端口与网络

| 端口 | 用途 | 谁开 |
|---|---|---|
| `:8080`(本仓) | HTTP backend(nginx + dapr 都打这) | `cmd/fresh-meat` |
| `:3500`(dapr sidecar) | dapr gRPC API | `dapr run` 注入 |

容器化时:`/healthz`(由 `cmdbootstrap` 注册,绕过 auth)— K8s readinessProbe / livenessProbe 走这。

---

## §3 配置变更清单(部署方)

### 3.1 代码侧

- **`internal/fresh-meat/model/model.go`** — CutType +5 / WholePig +3 / CutSnapshot +1 / LineSalesByPig +2 / 新 WasteLog
- **`internal/fresh-meat/service/service.go`** — `bizTZ` + `businessDayBounds` + per-SKU 盘点 + RecordWasteLog Tx
- **`internal/fresh-meat/service/stocktake_calc.go`** — bizTZ-aware 边界 + opening carry-forward + waste_logs 数据源
- **`internal/fresh-meat/service/subscribe.go`** — R 行处理 + LLM advice 优先 pig 绑定 + ASC fallback
- **`internal/fresh-meat/service/gross_margin.go`** — 新文件,`ComputeGrossMargin`
- **`internal/fresh-meat/handler/handler.go`** — `GET /gross-margin` + `parseDay` 用 bizTZ
- **`cmd/fresh-meat/main.go`** — AutoMigrate 加 WasteLog + bizTZ env 注入

### 3.2 配置侧(deployer 端)

| 文件 / 目录 | 改动 | 备注 |
|---|---|---|
| `deployer/apps/fresh-meat/components/pubsub.yaml` | **新增**(必需) | 见 §1.5.1 |
| `deployer/apps/fresh-meat/components/conversation.yaml` | **新增**(必需) | 见 §1.5.2;endpoint 指向外部 LLM 服务(本仓已无内部 LLM 网关;`llm-gw` 占位 cmd 于 2026-09-29 删除) |
| `deployer/.dapr/config.yaml`(or apps/fresh-meat/components/config.yaml) | **新增**(强烈推荐) | 见 §1.5.3;现成的 `dapr/baiyuan-config.yaml` 可参考 |
| `deployer/nginx/yun-bright.conf` | **不动** | `<app>` 段无变 |
| `deployer/systemd/supertrade-fresh-meat.service` | 已存在,无需动 | `--resources-path` 与 `--config` 路径已正确 |
| `deployer/tests/contract/fresh-meat_test.go` | 加 e2e(阶段4 + Stage D) | 见 [REQUIREMENTS §4.6] |
| userd scope 列表 | `freshmeat:view` / `freshmeat:write` | ⚠️ **需确认存在**,见 §3.2.1 |
| K8s secret / Pod env | `POSTGRES_DSN` / `OPENAI_API_KEY` / `FRESHMEAT_BIZ_TZ` | systemd unit 已写死 POSTGRES_DSN;其它 env 在 unit 加 |

> **重要**:**`deployer/apps/` 目录当前不存在**(无 apps/ 在 git 中;由 `deploy-supertrade.ps1` 在
> remote 端 `mkdir -p` 后才生成),所以新增 `apps/fresh-meat/components/*.yaml` 是部署方手工创建,
> 不走脚本。

### 3.2.1 ⚠️ 权限点必须存在(上线阻塞项)

> **本节修正了原文一处错误描述。** 原文写「userd scope 列表 `freshmeat:edit`
> 阶段1 已加,无需动」—— **两处都不对**:
>
> 1. **字面量错**:handler 实际校验的是 `freshmeat:write`(全仓 16 处
>    `requireScope` 均为 `freshmeat:write` / `freshmeat:view`),没有 `freshmeat:edit`。
> 2. **状态错**:截至 2026-10-02,`auth/migrations/001_init_schema.up.sql` 的
>    `permissions` 种子数据里**没有** `freshmeat:*`(连 `stocktake:*` 也没有),
>    全 `YunBright` workspace 的 Go/SQL/YAML 里搜不到任何 `freshmeat` 字面量。
>    这些权限点是运行时经 admin 后台建的,不是 migration seed。
>
> **为什么这是硬阻塞**:`Service.HasEffectiveScope`
> (`service/service.go:876`)是**纯 scope 集合比对,没有 admin 角色兜底**。
> 权限点缺失时,**包括 admin 在内的所有用户**读端点 403、写端点 403,
> Flutter 端表现为「有入口、点进去全失败」。
>
> **上线前必须核对**(对每个要用鲜肉的角色执行):
>
> ```sql
> SELECT name FROM permissions WHERE name LIKE 'freshmeat:%';
> -- 期望恰好两行:freshmeat:view / freshmeat:write
> ```
>
> 缺则补(字面量必须与 handler 一致,**不要**用 `freshmeat:edit`):
>
> ```sql
> INSERT INTO permissions (id, name, display_name, description) VALUES
>   (gen_random_uuid(), 'freshmeat:view',  '查鲜肉', '查看鲜肉毛利 / 整猪 / 盘点 / 销售'),
>   (gen_random_uuid(), 'freshmeat:write', '改鲜肉', '录入整猪 / 单品补录 / 日终盘点 / 报损 / 映射维护')
> ON CONFLICT (name) DO NOTHING;
>
> -- 授给目标角色(按需替换 'admin' / 'merchant' 等)
> INSERT INTO role_permissions (role_id, permission_id)
> SELECT r.id, p.id FROM roles r, permissions p
> WHERE r.name = 'admin' AND p.name IN ('freshmeat:view','freshmeat:write')
> ON CONFLICT DO NOTHING;
> ```
>
> 改完权限后 fresh-meat 侧有 60s scope 缓存(`auth.user.access_changed` 事件可
> 主动失效,兜底最长 60s),验证时等一下或重发该事件。
>
> Flutter 端已按 handler 字面量实现:`Scopes.freshMeatView` /
> `Scopes.freshMeatWrite`,见 `lib/domain/permission/scopes.dart`。

### 3.3 数据迁移

**无破坏性** — 所有变更通过 GORM AutoMigrate + 默认值兼容:
- `line_sales_by_pig.order_status` 默认 `'S'`(历史行视为销售)
- `whole_pigs.half_pig / offal_included` 默认 `false`(历史单头无标记)
- `whole_pigs.purchase_group_id` 默认 `''`(历史单头无分组)
- `waste_logs` 是新表,空表启动

---

## §4 上线步骤

```bash
# 1) 拉代码(本仓 main 分支)
git pull origin main

# 2) 单测 + vet 自检
go test ./internal/fresh-meat/... -count=1   # 期望:29 passed
go vet ./...

# 3) 编译产物
go build -o /tmp/fresh-meat ./cmd/fresh-meat

# 4) 替换 deployer 仓内的 fresh-meat 镜像 / 二进制
cp /tmp/fresh-meat <deployer-artifact-path>

# 5) 滚动重启(dapr sidecar 跟着起)
kubectl rollout restart deployment/fresh-meat   # 或 supervisorctl restart fresh-meat

# 6) 健康检查
curl -s http://<host>/api/v1/fresh-meat/healthz  # 200

# 7) 冒烟:1 笔整猪录入 → 1 笔单品补录 → 1 笔 sale → 1 笔盘点 → GET /gross-margin
```

### 4.1 上线前必做(deployer 端,补齐缺失的 dapr 资源)

> **现状(2026-10-02)**:deployer 仓的 `systemd/supertrade-fresh-meat.service` 已写死两条路径,
> 但路径下的 yaml **不在 git 里** — 必须手工创建,然后 `scp` 上 remote。

```bash
# === A. 本地(deployer 仓工作目录) ===
# 创建组件目录 + 写 yaml
mkdir -p apps/fresh-meat/components
mkdir -p .dapr

# 拷 deployer/dapr/baiyuan-config.yaml 做模板,改名为 appconfig
cp dapr/baiyuan-config.yaml .dapr/config.yaml

# 写 pubsub.yaml(见 §1.5.1)
cat > apps/fresh-meat/components/pubsub.yaml <<'EOF'
apiVersion: dapr.io/v1alpha1
kind: Component
metadata:
  name: pubsub
  namespace: default
spec:
  type: pubsub.redis
  version: v1
  metadata:
    - {name: redisHost, value: "172.12.1.1:6379"}
    - {name: consumerID, value: "{appID}"}
    - {name: enableDeadLetter, value: "true"}
    - {name: deadLetterTTL, value: "86400"}
EOF

# 写 conversation.yaml(见 §1.5.2)
cat > apps/fresh-meat/components/conversation.yaml <<'EOF'
apiVersion: dapr.io/v1alpha1
kind: Component
metadata:
  name: conversation
  namespace: default
spec:
  type: conversation.openai
  version: v1
  metadata:
    - {name: endpoint, value: "http://llm-gw.internal/v1"}
    - {name: apiKey, secretKeyRef: {name: llm-gw-cred, key: apikey}}
    - {name: model, value: "yunbright/predict-cuts-v1"}
    - {name: cacheTTL, value: "30m"}
EOF

# === B. 推送到 remote ===
scp apps/fresh-meat/components/pubsub.yaml       gyy:/opt/YunBright/supertrade/apps/fresh-meat/components/pubsub.yaml
scp apps/fresh-meat/components/conversation.yaml gyy:/opt/YunBright/supertrade/apps/fresh-meat/components/conversation.yaml
scp .dapr/config.yaml                           gyy:/opt/YunBright/.dapr/config.yaml

# === C. remote 上确保目录权限 ===
ssh gyy "chmod -R 644 /opt/YunBright/supertrade/apps/fresh-meat/components/ /opt/YunBright/.dapr/config.yaml"

# === D. systemctl daemon-reload + restart ===
ssh gyy "systemctl --user daemon-reload && systemctl --user restart supertrade-fresh-meat.service"
```

**回滚**:清掉 `/opt/YunBright/supertrade/apps/fresh-meat/components/` 与 `/opt/YunBright/.dapr/config.yaml`,
service 启动会报 `pubsub not configured` 失败但不影响其它 app。

### 4.2 冒烟脚本(场景三:2 头整猪 + 半头 + 下水 + 退货 + 毛利)

```bash
BR='X-Branch-ID: S001'

# 0. 配门店 cut 部位 ↔ cube SKU
curl -sX POST http://localhost:8080/branch-cut-mappings \
  -H "$BR" -H 'Authorization: Bearer dev' \
  -d '{"cut_type":"belly","cube_product_id":"P-2001"}'

# 1. 早盘 4:30 北京时间,场景三:2 头同 group
GRP=GRP_20261001_001
P1=$(curl -sX POST http://localhost:8080/whole-pigs \
  -H "$BR" -H 'Authorization: Bearer dev' \
  -d "{\"ear_tag\":\"EB-001\",\"gross_weight_kg\":120,\"purchase_unit_price_yuan\":28,\"supplier_id\":\"SUP-101\",\"arrived_at\":\"2026-10-01T04:30:00+08:00\",\"half_pig\":false,\"offal_included\":true,\"purchase_group_id\":\"$GRP\"}" \
  | jq -r .pig_id)

# 2. 仓管扫码 belly
curl -sX POST http://localhost:8080/pig-cuts \
  -H "$BR" -H 'Authorization: Bearer dev' \
  -d "{\"pig_id\":\"$P1\",\"cut_type\":\"belly\",\"barcode\":\"BC-001\",\"weight_kg\":48,\"cube_product_id\":\"P-2001\"}"

# 3. POS 销售(belly 20kg)
curl -sX POST http://localhost:8080/events/sale.completed \
  -H "$BR" -d '{"branch_id":"S001","order_id":"P-001","order_status":"S",
    "lines":[{"line_id":"L-1","fresh_type":"meat","cube_product_id":"P-2001","cut_type":"belly","qty_kg":20,"amount_yuan":680,"unit_price_yuan":34}]}'

# 4. 退货
curl -sX POST http://localhost:8080/events/sale.completed \
  -H "$BR" -d '{"branch_id":"S001","order_id":"P-002","order_status":"R",
    "lines":[{"line_id":"L-4","fresh_type":"meat","cube_product_id":"P-2001","cut_type":"belly","qty_kg":-1,"amount_yuan":-34,"unit_price_yuan":34}]}'

# 5. 报损
curl -sX POST http://localhost:8080/waste-logs \
  -H "$BR" -H 'Authorization: Bearer dev' \
  -d '{"cut_type":"belly","cube_product_id":"P-2001","qty_kg":0.3,"reason":"过期"}'

# 6. 日终盘点
curl -sX POST http://localhost:8080/pork-cuts-stocktake \
  -H "$BR" -H 'Authorization: Bearer dev' \
  -d '{"is_complete": true, "cuts":[{"cut_type":"belly","cube_product_id":"P-2001","actual_remain_kg":26.7}]}'

# 7. 毛利(必查)
curl -s "http://localhost:8080/gross-margin?date=2026-10-01" -H "$BR" | jq .
# 期望:
#   data_source="actual"(is_complete=true)
#   belly.purchased_kg=48 / sold_kg=19(20-1) / waste_kg=0.3 / actual_remain_kg=26.7
#   revenue=646 (680-34)
#   gross_margin_pct 与 cost 摊销金额按 pig purchase_cost 比例算
```

### 4.3 验收

- [ ] 启动日志含 `fresh-meat app initialized` + `schema="fresh_meat (6 tables)"` + `biz_tz="Asia/Shanghai"`
- [ ] `GET /healthz` 200
- [ ] 冒烟脚本 7 步全过;`GET /gross-margin` 返 `data_source="actual"`
- [ ] 退货 R 行不入 revenue(`revenue=646` 而非 680)
- [ ] `fresh_meat.waste_logs` 有 1 行
- [ ] 日终盘后 `pork.cuts_stocktakes` 有 1 行 `is_complete=true`

### 4.4 回滚

无破坏性 DDL,可安全回滚到上一版本。回滚期间:
- 历史 `line_sales_by_pig` 的 `order_status='S'` 默认值保留(无 R 标记 = 默认销售,符合业务)
- 历史 `whole_pigs` 的 `half_pig=false / offal_included=false / purchase_group_id=''` 保留
- `waste_logs` 表保留(回滚版本不读这张表,无副作用)

---

## §5 监控与告警(部署方接入)

### 5.1 业务指标(slog → 收集器)

| 指标 | 触发 | 告警阈值 |
|---|---|---|
| `predictFn failed` | LLM predict 失败 | 连续 5min > 50% 触发 → 上游 LLM 网关故障 |
| `computeExpectedRemainKgByCut failed` | stocktake 服务端算 expected 失败 | > 1% → DB 索引 / schema 异常 |
| `waste.log.recorded` publish 失败 | dapr 投递失败 | > 5/min → sidecar / pubsub 异常 |
| `pork.cuts.stocktaken` 含 data_source="actual_fallback" | 计算降级 | > 0 → 计算路径异常 |

### 5.2 Liveness / Readiness

- `/healthz`(cmdbootstrap 自动注册,公开)— K8s livenessProbe + readinessProbe
- 启动期 fail-fast:DB / dapr / cube client 任一失败立即退出,readiness 不会切到 True

### 5.3 排错

| 现象 | 排查 |
|---|---|
| 时区仍错(4:30 进猪落前一日) | `appSvc.BizTZ().String()` 日志;env `FRESHMEAT_BIZ_TZ` 是否生效 |
| 毛利 revenue=0 | `line_sales_by_pig` 看 `order_status` 列是否落 `S`;POS payload 是否带 `order_status` |
| 昨夜库存(opening)总 0 | `previousBizDayBounds` bizTZ 应用;昨夜的 `pork_cuts_stocktakes.is_complete=true` 是否落库 |
| 报损记录丢 | 看 Tx 是否走通;`fresh_meat.waste_logs` 表是否 AutoMigrate 建出 |
| `cuts[].cube_product_id 必填` 拒绝 | 客户端补 cube_product_id;POS / 仓管 SDK 升级到 2026-10-01 后版本 |

---

## §6 引用

- 优化方案:`.claude/plans/reflective-floating-nova.md`
- 业务契约:`docs/REQUIREMENTS.md:383-456 §4`
- 字段规范:`docs/FIELD_SPEC.md:169-213 §1.6`
- 事件契约:`docs/EVENT-CATALOG.md:200-255 §2.7 / §2.8`
- 路由规范:`.claude/rules/02-handler-routes.md`
- 应用清单:`.claude/rules/00-app-catalog.md`
- dapr 1.18 约束:`memory/dapr-opa-constraints.md`
- X-Branch-ID 约定:`memory/x-branch-id-convention.md`
