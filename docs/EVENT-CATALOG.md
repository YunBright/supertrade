# EVENT-CATALOG — Dapr pub/sub 事件契约

> **唯一真相源**:所有跨 dapr app 的 pub/sub 事件以本文件为准。
> 新增/修改 topic 须 PR 同步本文件 + `docs/DESIGN.md` §2.3。
>
> **传输协议**:Dapr pub/sub binding,**CloudEvents 1.0** 兼容(JSON 序列化)。
> 所有事件均含 `id` / `source` / `type` / `time` / `datacontenttype` 标准字段,
> payload 在 `data` 字段内(本文件 §2 列出 data schema)。
>
> **版本规则**:`specversion` + topic 名内的语义版本字段;
> 非破坏性新增字段时**不升级版本**,破坏性变更才升级。

---

## §0 全局约定

### 0.1 envelope(CloudEvents 1.0)

```jsonc
{
  "specversion": "1.0",
  "type":         "com.yunbright.supertrade.<event-type>",  // 见 §2
  "source":       "<dapr-app-id>/<entity-id>",             // 例如 "pos/SO20260917001"
  "id":           "<uuid>",                                 // 全局唯一,用于幂等
  "time":         "2026-09-17T10:23:45Z",                   // RFC3339 UTC
  "datacontenttype": "application/json",
  "subject":      "<entity-type>/<entity-id>",              // 例如 "sale_order/SO..."
  "data":         { ... 业务 payload ... }                  // 见 §2 各 topic
}
```

### 0.2 字段约定

- 所有 ID 字段(`sale_id` / `pig_id` / `batch_id` 等)使用 UUID v4 或业务领域唯一码(如 `SO20260917001`)。
- 所有时间字段 RFC3339 UTC,以 `*_at` 后缀。
- 所有金额字段 `*_yuan` 后缀,decimal,单位元。
- 所有重量字段 `*_kg` 后缀,decimal,单位 kg。

### 0.3 幂等 / 重试 / DLQ

- **幂等**:订阅方必须按 `id` 去重,重复事件不重复处理(可写 idempotency table)。
- **重试**:Dapr 默认重试策略(指数退避 3 次)。
- **DLQ**:失败超过 N 次进入 `<topic>.dlq`(`pubsub.redis.yaml` 配置);运维侧定期检查。

### 0.4 topic 命名

`<domain>.<verb>`(全部小写,点分),例如:
- `inventory.stock.moved`
- `sale.completed`
- `pig.arrived`

---

## §1 topic 索引

| topic | 发布方 | 订阅方 | payload 关键字段 | spec |
|---|---|---|---|---|
| `inventory.stock.moved` | inventory | fresh-produce / fresh-meat / sales-agg | sku_id, qty, move_type | §2.1 |
| `purchase.completed` | procurement | inventory / fresh-produce | grn_id, lines[] | §2.2 |
| `sale.completed` | pos | inventory / fresh-meat / sales-agg | sale_id, branch_id, lines[] | §2.3 |
| `stocktake.completed` | stocktake | inventory / fresh-produce / fresh-meat | task_id, type, branch_id | §2.4 |
| `produce.stocktake.completed` | fresh-produce | sales-agg / bi-gateway / notification | stocktake_id, is_complete, profit_period | §2.5 |
| `waste.log.recorded` | fresh-produce / fresh-meat | sales-agg / bi-gateway | waste_id, sku_id, qty_kg, reason | §2.6 |
| `pig.arrived` | fresh-meat | llm-gw(早盘) | pig_id, ear_tag, gross_weight_kg | §2.7 |
| `pork.cuts.stocktaken` | fresh-meat | llm-gw(日终) / sales-agg / notification | stocktake_id, is_complete, cuts[] | §2.8 |
| `pig.analysis.completed` | llm-gw | fresh-meat / notification / dashboard | pig_id, analysis_json, task | §2.9 |
| `erp.sale.ingested` | erp-connector | sales-agg | source, batch_id, count, batch_at | §2.10 |
| `sale.aggregated` | sales-agg | bi-gateway + 其它 dapr app | branch_id, period, kpis | §2.11 |
| `stocktake.line.added` | stocktake | notification-gateway | header_id, line_id, branch_id | §2.12 |
| `stocktake.line.updated` | stocktake | notification-gateway | header_id, line_id, branch_id | §2.12 |
| `stocktake.line.deleted` | stocktake | notification-gateway | header_id, line_id, branch_id | §2.12 |
| `stocktake.header.submitted` | stocktake | notification-gateway | header_id, branch_id, type | §2.13 |
| `stocktake.header.approved` | stocktake | notification-gateway | header_id, branch_id, type | §2.13 |
| `stocktake.plan_item.added` | stocktake | notification-gateway | header_id, items_count | §2.13 |
| `auth.user.permissions_changed` | auth (userd) | notification-gateway | user_id, tenant_id, changed_scopes, changed_roles | §2.14 |

---

## §2 payload schema(各 topic)

### 2.1 inventory.stock.moved

**触发时机**:inventory 服务写入一条 `stock_moves` 行后立即发出。

```jsonc
// data:
{
  "move_id":    "uuid",
  "branch_id":  "S001",
  "sku_id":     "P-1001",
  "qty":        12.5,           // 正=入库,负=出库;单位与 sku 一致
  "move_type":  "purchase" | "sale" | "transfer" | "waste" | "stocktake_adjust",
  "ref_type":   "purchase" | "sale" | "stocktake" | "transfer",
  "ref_id":     "<对应单据 PK>",
  "at":         "2026-09-17T10:23:45Z",
  "unit_cost_yuan": 28.0         // 入库时填;出库时为批次成本
}
```

### 2.2 purchase.completed

```jsonc
// data:
{
  "grn_id":     "GRN20260917001",
  "branch_id":  "S001",
  "supplier_id": "SUP-001",
  "purchase_order_id": "PO20260916001",
  "lines": [
    { "sku_id": "P-1001", "qty": 50.0, "unit_cost_yuan": 12.5, "expires_at": "..." }
  ],
  "completed_at": "2026-09-17T10:23:45Z",
  "operator_id":  "user-uuid"
}
```

### 2.3 sale.completed

```jsonc
// data:
{
  "sale_id":   "SO20260917001",
  "branch_id": "S001",
  "operator_id": "user-uuid",
  "total_amount_yuan": 86.5,
  "lines": [
    {
      "sku_id": "P-2001",
      "qty":    2.0,
      "unit_price_yuan": 18.0,
      "amount_yuan": 36.0,
      "fresh_type": "none" | "produce" | "meat",  // 决定是否要落 fresh-* 表
      "pig_id": "uuid",                            // 仅 fresh_type=meat 时填,关联整猪
      "cut_type": "belly" | "tenderloin" | ...     // 仅 fresh_type=meat 时填
    }
  ],
  "completed_at": "2026-09-17T10:23:45Z",
  "order_status": "S" | "R"                        // S=销售 / R=退货
}
```

### 2.4 stocktake.completed

```jsonc
// data:
{
  "task_id":   "ST20260917001",
  "branch_id": "S001",
  "type":      "produce" | "meat_cuts" | "general",  // 决定下游订阅
  "stocktake_id": "uuid",
  "is_complete": true,                                 // 仅 meat_cuts 时用
  "completed_at": "2026-09-17T22:10:00Z",
  "operator_id":  "user-uuid"
}
```

### 2.5 produce.stocktake.completed

**触发时机**:fresh-produce 服务完成一次蔬果盘点并计算本期毛利后。

```jsonc
// data:
{
  "stocktake_id":      "uuid",
  "branch_id":         "S001",
  "sku_id":            "P-3001",     // 蔬果 SKU
  "cycle_id":          "uuid",      // 所属盘点周期
  "is_complete":       true,
  "period": {
    "begin_at":         "2026-09-10T22:00:00Z",
    "end_at":           "2026-09-17T22:00:00Z",
    "opening_cost_yuan": 240.0,
    "purchase_cost_yuan": 800.0,
    "sales_revenue_yuan": 1200.0,
    "closing_value_yuan": 260.0,
    "gross_profit_yuan":  420.0,
    "loss_yuan":          40.0,      // 隐性损耗
    "explicit_waste_yuan": 12.0     // 来自 waste_logs
  },
  "completed_at": "2026-09-17T22:10:00Z"
}
```

### 2.6 waste.log.recorded

```jsonc
// data:
{
  "waste_id":   "uuid",
  "source":     "fresh-produce" | "fresh-meat",
  "branch_id":  "S001",
  "sku_id":     "P-3001",
  "qty_kg":     0.8,
  "reason":     "rot" | "damage" | "expired" | "other",
  "occurred_at":"2026-09-17T16:30:00Z",
  "recorded_by":"user-uuid"
}
```

### 2.7 pig.arrived

**触发时机**:fresh-meat 服务写入 `whole_pig` 后(早盘)。

```jsonc
// data:
{
  "pig_id":      "uuid",
  "ear_tag":     "EB-2026-001",
  "branch_id":   "S001",
  "gross_weight_kg": 312.5,
  "purchase_unit_price_yuan": 28.0,
  "purchase_cost_yuan": 8750.0,
  "arrived_at":  "2026-09-17T06:15:00Z",
  "recorded_by": "user-uuid"
}
```

订阅方 `llm-gw` 收到后:
1. 查近 30 天 ±10% 重量段的 history_pigs
2. 调 llm-gw.ChatCompletion(task="predict_cuts")
3. 写回 whole_pig.llm_advice_json
4. 发 `pig.analysis.completed`(§2.9)

### 2.8 pork.cuts.stocktaken

**触发时机**:fresh-meat 服务写入 `pork_cuts_stocktake` 后(**可选**,缺失不阻断销售,见 REQUIREMENTS §4.4)。

```jsonc
// data:
{
  "stocktake_id":  "uuid",
  "branch_id":     "S001",
  "is_complete":   true,                // 关键字段:BI 据此标注"已盘点" / "未盘点,推演"
  "cuts": [
    {
      "cut_type":             "belly",
      "actual_remain_kg":     8.2,
      "expected_remain_kg":   7.5,
      "variance_kg":          0.7
    },
    { "cut_type": "tenderloin", "actual_remain_kg": 2.1, "expected_remain_kg": 2.0, "variance_kg": 0.1 }
  ],
  "llm_reviewed": true,                // 是否完成 LLM 反推(可选)
  "completed_at": "2026-09-17T22:10:00Z",
  "operator_id":  "user-uuid"
}
```

> 当日**未**录入盘点 → sales-agg 用 llm 推演 / 历史均值估算 fresh-meat 毛利,
> BI 在鲜猪毛利卡片显示"⚠ 未盘点,数据为推演"。

### 2.9 pig.analysis.completed

**触发时机**:llm-gw 完成早盘预测或日终反推后。

```jsonc
// data:
{
  "analysis_id":  "uuid",
  "task":         "predict_cuts" | "review_cuts",
  "branch_id":    "S001",
  "pig_ids":      ["uuid", "uuid"],          // 关联的整猪 PK 列表
  "stocktake_id": "uuid",                     // task=review_cuts 时填
  "analysis": {
    "predicted_cuts": [                       // task=predict_cuts 时填
      { "pig_id": "uuid", "cut_type": "belly", "predicted_kg": 92.0 }
    ],
    "review": {                              // task=review_cuts 时填
      "anomaly_pigs": ["uuid"],              // 标记分割异常的猪
      "tomorrow_suggestion": "..."           // 明日分割建议(自由文本)
    }
  },
  "llm_latency_ms": 1234,
  "fallback_used":  false,                    // true=LLM 失败,降级到历史均值
  "completed_at":   "2026-09-17T06:18:30Z"
}
```

### 2.10 erp.sale.ingested

**触发时机**:erp-connector 一次差量拉取完成,新行落 `erp_sales_raw` 后。

```jsonc
// data:
{
  "source":        "sixun-hbposv7" | "sixun-ysx" | ...,
  "branch_id":     "S001",
  "batch_id":      "uuid",
  "batch_window":  { "begin": "2026-09-17T09:55:00Z", "end": "2026-09-17T10:00:00Z" },
  "ingested_count": 42,
  "total_amount_yuan": 1850.5,
  "ingested_at":   "2026-09-17T10:00:30Z"
}
```

### 2.11 sale.aggregated

**触发时机**:sales-agg 完成一次(分钟/小时/日)聚合窗口后。

```jsonc
// data:
{
  "branch_id":   "S001",
  "period":      "minute" | "hour" | "day",
  "period_start":"2026-09-17T10:00:00Z",
  "period_end":  "2026-09-17T10:01:00Z",
  "kpis": {
    "total_amount_yuan": 3250.0,
    "total_qty":         128.0,
    "sale_lines":        47,
    "unique_skus":       32,
    "by_source": {                            // 自营 / ERP 来源拆分
      "pos":           { "amount_yuan": 2500.0, "qty": 100.0 },
      "sixun-hbposv7": { "amount_yuan":  600.0, "qty":  20.0 },
      "sixun-ysx":     { "amount_yuan":  150.0, "qty":   8.0 }
    },
    "fresh_meat": {
      "is_complete":        false,            // 当日是否盘点
      "data_source":        "llm_inferred",  // actual | llm_inferred | history_avg
      "gross_profit_yuan":  480.0
    },
    "produce": {
      "is_complete":        true,
      "data_source":        "actual",
      "gross_profit_yuan":  120.0
    }
  },
  "aggregated_at": "2026-09-17T10:01:05Z"
}
```

### 2.12 stocktake.line.{added,updated,deleted}

**触发时机**:stocktake 服务在 AddLine / UpdateLine / DeleteLine 任一写操作成功后立即发出。
**订阅方**:notification-gateway(用于实时推送给同一 branch 在线用户)。

```jsonc
// data (added / updated):
{
  "header_id":    "ST20260917001",
  "line_id":      "uuid",
  "branch_id":    "S001",
  "tenant_id":    "T001",
  "sku_id":       "P-2001",
  "product_name": "五花肉",
  "barcode":      "6901234567890",
  "system_qty":   10.0,              // 账面数量(updated 时为最新值)
  "actual_qty":   9.5,               // 实盘数量(updated 时为最新值;added 可能为 0)
  "diff_qty":     -0.5,              // actual - system(后端算好)
  "unit":         "kg" | "piece",
  "operator_id":  "user-uuid",
  "operator_name":"张三",
  "occurred_at":  "2026-09-17T10:23:45Z"
}

// data (deleted):
{
  "header_id":   "ST20260917001",
  "line_id":     "uuid",
  "branch_id":   "S001",
  "tenant_id":   "T001",
  "sku_id":      "P-2001",
  "operator_id": "user-uuid",
  "occurred_at": "2026-09-17T10:23:45Z"
}
```

Flutter 端 `WsEventTypes.stocktakeLineAdded/Updated/Deleted` 与 type 严格对应。
每个事件 `data.header_id` 用于 routing 到当前 detail controller 的 `headerId`。
notification-gateway 的 TenantRouter 仅在 `branch_id` 落在 client 的 `effective_branches` 内才投递。

### 2.13 stocktake.header.{submitted,approved} + stocktake.plan_item.added

**触发时机**:
- `stocktake.header.submitted`:`Submit(header_id)` 成功后(counting → adjusted)
- `stocktake.header.approved`:`Approve(header_id)` 成功后(adjusted → approved)
- `stocktake.plan_item.added`:`AddPlanItems(header_id, items)` 每批成功后(批量生成计划项时)

```jsonc
// data (header.submitted / header.approved):
{
  "header_id":   "ST20260917001",
  "branch_id":   "S001",
  "tenant_id":   "T001",
  "type":        "general" | "produce" | "plan" | "recheck",
  "status":      "counting" | "adjusted" | "approved" | "cancelled",
  "parent_id":   "ST20260915001",        // 仅 recheck 类型有
  "operator_id": "user-uuid",
  "occurred_at": "2026-09-17T18:00:00Z"
}

// data (plan_item.added):
{
  "header_id":     "ST20260917001",
  "branch_id":     "S001",
  "tenant_id":     "T001",
  "items_count":   42,
  "batch_index":   1,
  "batch_total":   3,
  "operator_id":   "user-uuid",
  "occurred_at":   "2026-09-17T09:55:00Z"
}
```

### 2.14 auth.user.permissions_changed

**触发时机**:auth 服务(userd)在 admin 修改某用户的 scope / role 后立即发出。
**订阅方**:notification-gateway → 推送给该 user 的所有在线客户端 → Flutter `meController.load()` 自动重拉 `/auth/me` → UI 重新评估 PermissionGate。

```jsonc
// data:
{
  "user_id":         "user-uuid",
  "tenant_id":       "T001",
  "branch_id":       "S001",
  "changed_scopes":  ["stocktake:write"],      // 可选:增量提示前端哪些变动了
  "changed_roles":   ["stocktake_supervisor"], // 可选
  "snapshot_version": 17,                       // 单调递增;客户端可丢弃 <= 本地版本的事件
  "changed_at":      "2026-09-17T16:30:00Z"
}
```

> 若 `changed_scopes` / `changed_roles` 为空,客户端应触发完整 `meController.load()` 重新拉全量;
> 若非空,客户端可走增量更新(只 patch 本地 me 的对应字段,见 CLAUDE.md §4)。

---

## §3 订阅注册

每个 dapr app 通过 Dapr app config 或代码声明订阅。

### 3.1 声明式(推荐,放 `cmd/<app>/dapr.yaml` 或 dapr component)

```yaml
# 例:pos 的订阅声明
subscriptions:
  - pubsubname: pubsub
    topic: sale.completed
    route: /events/sale-completed
```

### 3.2 代码式(`/dapr/subscribe`)

骨架阶段不实现,后续按 topic 落地 handler 时补:

```go
r.GET("/dapr/subscribe", func(c *gin.Context) {
    c.JSON(200, []map[string]string{
        {"pubsubname": "pubsub", "topic": "sale.completed", "route": "/events/sale-completed"},
    })
})
r.POST("/events/sale-completed", handleSaleCompleted)
```

---

## §4 修订记录

| 日期 | 修订人 | 内容 |
|---|---|---|
| 2026-09-17 | Mavis | 初版,基于 DESIGN §2.3 topic 表展开 schema |
| 2026-09-19 | Mavis | 新增 §2.12 stocktake.line.{added,updated,deleted} + §2.13 stocktake.header.{submitted,approved} + stocktake.plan_item.added + §2.14 auth.user.permissions_changed;同步 stocktake/service.go 6 个 publish 调用点与 notification-gateway 订阅实现 |