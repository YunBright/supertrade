# 增量档案同步（2026-10-22）

## 一句话

档案同步从"每晚全量 8.4 万行"改成"只导上次水位之后变过的行"，
分两步落地：**① 库存**（23837 → 54 行，0.5 秒）、**② 商品/售价**（55138 → 十几行）。

---

## 1. 起因

生产实测：同步库存时 **17 秒**，超市收银数据上传延迟。

实测定位（不是源库慢）：

| 环节 | 实测 |
|---|---|
| 源库 `t_im_branch_stock` | 23837 行，一次 live 查询几百毫秒 |
| 本地 PG 写入 | **一个大事务里 23837 次 SELECT + 23837 次 upsert**，全程持锁 |

`stock_levels` 与 `sales_orders / sale_lines / payments / outbox` **同库**
⇒ 同步期间 POS 开单被拖住。

**第一层修复（已上线）**：分批提交（1000/批）+ 批量 upsert + 每批一次 IN 查询
⇒ SELECT 23837→24、INSERT 23837→24、事务 1→24，**语义不变**。
实测全量同步 7.0s，同步期间 POS p50=98ms / p95=105ms / max=119ms ⇒ 延迟症状消失。

**第二层（本文件）**：增量 ⇒ 白天也不再搬 23837 行。

---

## 2. 方案取舍：为什么不按"收银流水"反推

最初的设想是"查上次同步以来的收银流水，过滤出涉及的商品做库存同步"。

**这条路有洞**：库存变动**不止来自销售**。

| 变动来源 | 收银流水能覆盖吗 |
|---|---|
| 销售 | ✅ |
| 退货 | ✅（但退货台单据不一定带完整商品行） |
| 采购入库 | ❌ |
| 盘点调整 | ❌ |
| 调拨 | ❌ |
| 损耗 | ❌ |

⇒ 漏掉后面四类 ⇒ 本地库存**越来越偏**，而且**全程没有一行报错**。

**采用的口径**：源库 `t_im_branch_stock.oper_date`（= cube 的 `stock.updated_at`）
—— 它是**该行最后一次库存变动的时间**，覆盖**全部**变动来源。

实测（源库 ysx/hbposv10）：

```
t_im_branch_stock  23837 行
  今天变动              91 行
  最近 2 小时变动        54 行
  oper_date IS NULL   4191 行（库存全为 0 ⇒ 增量跳过无害）
```

---

## 3. 数据流

```
cube-async(编排器)
  │  ① 读水位:watermarks.stock = "2026-10-22 20:56:55"
  │  ② 带 since 导出
  ▼
GET /api/v1/inventory/archive/export?scope=stock&since=...
  │  ③ 门店配了数据源 ⇒ 转发 cube-router
  ▼
GET /v1/export?scope=stock&since=2026-10-22 20:56:55
  │  ④ cube 查询加 `stock.updated_at gt <since>`
  ▼
cube-sixun-ysx-fb（思迅源库）  → 只回 54 行
  │
  ▼
POST /api/v1/inventory/archive/import   → 只 upsert 这 54 行
  │
  ▼
成功 ⇒ 水位推进到"本次导出开始时刻 − 5s"
```

⚠️ **库存导入是纯 upsert，从不删行**（守卫：`TestImportStock_NeverDeletesRows`）
⇒ 增量导入不会动到"没变过的那些行"。

---

## 4. 关键设计

### 4.1 "哪些 scope 能增量"只有一份表

`internal/archivedto/dto.go` → `ChangedMembers`：

```go
var ChangedMembers = map[string]string{
    ScopeStock: "stock.updated_at",
}
```

导出端（cube-router）按它决定**加不加** `gt` 过滤；
编排端（cubesync）按它决定**允不允许推进水位**。

两边各写一份的后果：编排端以为 `products` 支持增量 ⇒ 带上 since ⇒
导出端**忽略** since（仍全量导出）⇒ 但**水位被推进了** ⇒
下次只导"最近变过的一部分"却当成全量 ⇒ **漏数据，且全程零报错**。

⇒ 一份表 + 两侧派生 + `Config.Validate()` 在**启动时**就拒掉配错的 scope。

### 4.2 水位语义

| 项 | 值 | 为什么 |
|---|---|---|
| 存在哪 | `/opt/YunBright/.dapr/cube-async-watermark.json` | 编排器自己记"我搬到哪"；编排器不连数据库 |
| 回退 | 5 秒 | 源库 `oper_date` 精确到秒 ⇒ 同秒两次更新会漏后一次 ⇒ 回退 ⇒ 重导一次（幂等，无害） |
| 推进 | 本次**导出开始**时刻 | 导出期间的变动落在水位之后 ⇒ 下次再导一次（幂等） |
| 丢文件 | 退化成**全量** | 宁可多导一万倍，也不能漏一行 |
| 定期校准 | 7 天 | 增量只保证"不漏"，但**任何一次**故障漏掉的行都会**一直缺着** ⇒ 必须定期全量兜底 |

### 4.3 增量轮次的 0 行 **不是失败**

源库一天只变 91 行 ⇒ 绝大多数夜晚增量窗口里就是 **0 行**。

- **全量轮次**的 0 行 = "没拉到" ⇒ **报错**（覆盖模式会把目标门店清空）
- **增量轮次**的 0 行 = "今天没变动" ⇒ **正常**，水位照常推进

判据是"这一轮有没有带 since"，不是"行数是不是 0"。

### 4.4 人工触发与定时回调**互斥**

有了水位之后，并发两轮同步从"重复劳动"升级成"永久漏数据"：

```
轮 A 读水位 W_old（启动得早）⇒ 导 (W_old, …]
轮 B 读水位 W_new > W_old ⇒ 导 (W_new, …]
若 A 失败、B 成功 ⇒ 水位推进到 W_new ⇒ (W_old, W_new] 永远补不回来
```

⇒ 人工触发也抢"运行中"标记，撞上就回 `409 sync_busy`。

### 4.5 人工可以**强制全量**（`force_full`）

`POST /v1/sync/run` body 多一个字段：

```json
{"token":"…","scopes":["stock"],"force_full":true}
```

⚠️ **只对人工触发生效**（定时回调不接受它：定时任务搬什么由环境变量决定，
不该被 body 改）。

**为什么需要它**：

| 场景 | 症状 |
|---|---|
| 换门店 / 重建目标库 | **增量导不出基础数据**（它只导"变过的行"）⇒ 目标门店永远缺着那些"没变过"的行，**且零报错** |
| 怀疑数据漂了 | 需要"立刻全量对齐"，而不是等 7 天的定期校准 |

⚠️ **增量不能用于初始化** —— 这是它的固有边界，不是 bug。
任何"目标端被清空/换店"的场景，**必须先跑一次全量**：

```bash
# 方式一(推荐):人工触发时带 force_full
# 方式二:删掉水位文件 ⇒ 下一轮自动全量(读不到水位 ⇒ 当作"没有水位")
rm /opt/YunBright/.dapr/cube-async-watermark.json
```

---

## 5. 配置

`systemd/supertrade-cube-async.service`：

```ini
Environment="SYNC_INCREMENTAL_SCOPES=stock"
Environment="SYNC_WATERMARK_FILE=/opt/YunBright/.dapr/cube-async-watermark.json"
Environment="SYNC_WATERMARK_OVERLAP_SECONDS=5"
Environment="SYNC_WATERMARK_MAX_AGE_DAYS=7"
```

⚠️ **名单写错一个字母**的后果是"该 scope 静默走全量"（慢，无害），不是漏数据
⇒ 白名单是安全的。（真正危险的是"名单里写了但 cube 侧不支持" ⇒ 启动时被拒。）

---

## 6. 实测结果（生产，2026-10-22）

| 轮次 | 导出行数 | 耗时 | POS 读延迟 p50 / p95 / max |
|---|---|---|---|
| 全量（基线） | 23837 | 7.1s | — |
| 增量（窗口内无变动） | 0 | 0.42s | 203 / — / 203 ms |
| 增量（窗口=今日 12:00 至今） | **54** | 0.51s | — |
| **全量校准**（强制） | 23837 | 5.7s | **175 / 198 / 208 ms** |

空闲期基线：p50=184ms / max=194ms。

**⇒ 全量校准期间 POS 延迟 max=208ms，与空闲期基本无差别。**

### 交叉验证

增量导出行数必须与**源库直查**逐行一致：

```sql
SELECT COUNT(*) FROM t_im_branch_stock
WHERE oper_date > CONVERT(datetime, '2026-10-22 12:00:00', 121);
-- 54
```

增量导出 = **54 行** ⇒ 一致 ⇒ cube 的 `gt` 过滤与源库口径完全对齐。

---

## 7. 还没做的（下一个杠杆）

**商品 / 售价 / 会员**仍然每晚全量：

| scope | 行数 | 一天变动 | 障碍 |
|---|---|---|---|
| products | 27569 | 4 | 源库有 `t_bd_item_info.modify_date`，但 **cube 的 product model 未暴露该维度** |
| prices | 27569 | 4 | 同上 |
| members | 4133 | 5 | 源库有 `t_rm_vip_info.modify_date`，cube 的 member model 同理 |

⇒ 要让它们增量，得**先给 cube 的 model 加"最后修改时间"维度**
（cube 仓 `sixun-models/product/schema.yaml` + mapping），
然后在 `archivedto.ChangedMembers` 里登记 ⇒ 编排端与导出端自动跟着生效。

这是当前**写入量的大头**（products + prices ≈ 55k 行/晚），
比库存的 24k 更值得做。

---

## 7bis. 第二阶段：商品 / 售价增量（2026-10-22 夜）

### 为什么它比库存难

库存的源一直是 **live**（直接查思迅 `t_im_branch_stock` 的当前值）⇒ 导出那一刻就是最新。
商品的源原本是 **cube 的离线 DuckDB 快照** ⇒ 有**两层**坑。

> ⚠️ **2026-10-22 晚：cube 已去掉 DuckDB，product 改成 live（实时透传源库）。**
> ⇒ 下面"坑②"（DuckDB 侧的时间比较抹掉时分秒）**已经不存在**：
>    现在只有 T-SQL 路径，`type: time` 的 gt 编译成 `CAST(col AS DATETIME)`（保留时分秒）✅。
> ⇒ 坑①（新建商品 `modify_date` 为 NULL）**依然存在**，且**更重要**了 ——
>    因为现在是实时透传，"新商品永远同步不到"会立刻体现在 POS 上。

### 坑①：新建商品 80% 的 `modify_date` 是 NULL

实测（两个源库）：

| 源库 | 行数 | `modify_date` 为 NULL | 今天的变动 |
|---|---|---|---|
| ysx（hbposv10） | 27569 | **6816（25%）** | 2 行 |
| hbposv7（hbposepro） | 44623 | **12607（28%）** | 0 行 |

思迅**建档时不写 `modify_date`** ⇒ 只按 `modify_date` 过滤 ⇒
**新商品永远导不出去** ⇒ POS 上搜不到新上新的货，**且没有任何一行报错**。

⇒ 维度定义成 `COALESCE(modify_date, created_at)`：
新建商品用**建档时间**兜底（`build_date` 一定有值），老商品用最后修改时间。

### 坑②：DuckDB 侧的时间比较会**抹掉时分秒**（⚠️ 已随 DuckDB 一起去掉）

> ⚠️ **本节已过时**（2026-10-22 晚）：cube 去掉 DuckDB 后**只剩 T-SQL 路径**，
>    `type: time` 的 gt 编译成 `CAST(col AS DATETIME)` ⇒ **保留**时分秒 ✅。
>    维度因此可以直接声明成 `type: time`（不需要 CAST 成 VARCHAR）。
>
> 保留本节是因为它记录了**为什么不能写成 VARCHAR**：
> cube 的 DuckDB 路径对 `type: time` 的 gt/gte 编译成 `CAST(col AS DATE) > ?`
> ⇒ **CAST 成 DATE 只剩日期** ⇒ "今天 17:20 变过的商品" 与 "今天 12:00 的水位"
> 比较时落在同一天 ⇒ 当天的变动**被漏掉**。
> 而 T-SQL 路径用的是 `CAST(RTRIM(col) AS DATETIME)`（**保留**时分秒）
> ⇒ **同一语义的两条路径给出不同答案**。

⚠️ 绝不能写成 `CAST(... AS VARCHAR)` 再做文本比较：
   MSSQL 的 `datetime→VARCHAR` 是**本地化格式**（`Jul 22 2026  5:20PM`），
   字典序 ≠ 时间序 ⇒ 增量过滤**静默漏数据**。
   （守卫：`cube/pkg/cubequery/product_live_test.go` —— 断言 SQL 里**不得**出现 `AS VARCHAR)`。）

### 坑③：水位必须减去**源数据滞后**（⚠️ 现在全为 0）

我们的水位语义是"**我导到哪了**"，而 since 的语义是"**源数据最后变动的时刻**"。
两者**只在源数据实时**时才相等。

源是 cube 快照时：导出那一刻，cube 里的数据可能还是 30 分钟前的
⇒ 水位若按"导出时刻"记，那 30 分钟里的变更**永远落在水位之前** ⇒ **永久漏**。

⇒ 水位 = 导出起点 − **源数据滞后**
⇒ 下一轮的窗口 = (上一轮导出起点 − 滞后, ∞) ⇒ 窗口**连续** ⇒ 不漏。

| scope | 源类型 | 滞后 | 取值理由 |
|---|---|---|---|
| stock | live（直接查源库） | 0 秒 | 无滞后 |
| products / prices | duck（cube 快照，30 分钟刷新） | **24 小时** | 必须**大于**源的刷新间隔（30 分钟）才有安全余量；取大没有代价（重复导的行幂等） |

实现：`archivedto.ChangedMemberLag` + `WatermarkStore.NowAt(lag)`。

守卫：`internal/archivedto/incremental_meta_test.go`
（每个"能增量"的 scope 都必须登记滞后 ⇒ 漏登记 = 永久漏数据，且零报错）。

### 改了什么

**cube 仓**
- `sixun-models/product/schema.yaml`：加 `updated_at` 维度
  （`sql: "CAST(COALESCE(modify_date, created_at) AS VARCHAR)"`，`type: string`）
- `cmd/sixun-ysx/mapping/mapping-product.yaml` + `cmd/sixun-hbposv7/...`：加 `modify_date`
  （⚠️ **两个 family 必须同时加**，否则 mapping parity 测试红）

**supertrade 仓**
- `internal/archivedto/dto.go`：`ChangedMembers` 登记 products/prices；新增 `ChangedMemberLag`
- `internal/cube-router/handler/export.go`：products/prices 的 `ChangedMember`
- `internal/cubesync/watermark.go`：`NowAt(lag)`（水位减去源数据滞后）
- `internal/cubesync/syncer.go`：`syncScope` 用 `NowAt(lag)`

### ⚠️ 部署顺序（反了会打挂导出）

cube 的 DuckDB 表**要重建**才有 `modify_date` 列（mapping 变了 ⇒ 表结构变了）
⇒ **在重建之前查 `product.updated_at` 会 column not found**。

```
① 部署 cube（新二进制：内嵌 schema + mapping）
② 触发一次全量刷新 ⇒ 重建 product 表（带上新列）
③ 验证 product.updated_at 可查 + gt 过滤行数与源库直查一致
④ 部署 supertrade 的 cube-router（登记 products/prices）
⑤ unit 的 SYNC_INCREMENTAL_SCOPES 加 products,prices ⇒ 重启 cube-async
```

⇒ **④⑤ 必须在 ②③ 之后** ⇒ 否则导出端一加过滤就 column not found ⇒ 导出全挂。

⚠️ ② 的全量刷新会**重建表**（重建期间查询失败）⇒ 挑**非营业时间**做。
⚠️ 且 cube 的 `loadFull` 明确写着"**空结果会把表清空,这是有意的**"
⇒ 触发前必须先确认源库能查到完整的 27569 行（否则门店 00 的商品表会被清空）。

### 保持全量的 scope（各有硬理由）

| scope | 行数 | 为什么不能增量 |
|---|---|---|
| item_barcodes | 155 | 覆盖模式是**按商品整体替换** ⇒ 增量导出会把该商品的其他条码删掉 |
| promotions | 741 | 覆盖模式会**清掉不在有效集合内的历史行** ⇒ 增量等于删促销 |
| members | 4133 | 收益小；会员导入有"不覆盖财务列"的特殊语义 |

---

## 8. 守卫测试

| 测试 | 守什么 |
|---|---|
| `internal/cubesync/watermark_test.go` | 文件丢了/坏了 ⇒ 没有水位；scope 隔离；回退秒数；定期校准 |
| `internal/cubesync/syncer_incremental_test.go` | 增量带水位 / 全量不带；增量 0 行不是失败；导入失败水位不动；白名单外不带 since；定期全量；`force_full` 无视水位 |
| `internal/cubesync/handler_concurrency_test.go` | 人工触发与定时回调不得并发（409 sync_busy） |
| `internal/cube-router/handler/export_incremental_test.go` | 导出端与编排端同一份 `ChangedMembers`；`gt` 过滤；没声明的不加过滤 |
| `internal/inventory/service/import_test.go::TestImportStock_NeverDeletesRows` | 库存导入**从不删行**（增量安全的前提） |
| `internal/inventory/service/export_test.go::TestExportStock_LocalBranchHonorsSince` | 本地路径也认 `since`（两条路径口径一致） |
| `deployer/tests/contract/cube_async_wiring_test.go::TestCubeAsyncIncrementalConfig` | unit 的增量配置自洽（名单 ⊆ 可增量 scope、水位文件已配、校准天数 > 0） |

## 9. 契约测试怎么验增量（deployer 仓）

`deployer/tests/contract/pos_replay_test.go` 分成**两组**：

| 组 | 走哪条路 | 为什么 |
|---|---|---|
| `同步/cube-async 把门店 00 的档案搬到门店 10` | **强制全量**（`force_full=true`） | 要断言"同步后门店 10 == 源库" ⇒ 需要**确定性基线** |
| `增量/库存_增量同步_不会把没变的行抹掉` | **增量**（只跑 `stock` 一个 scope，0.5 秒） | 生产夜间跑的就是这条路 ⇒ 必须单独验 |

⚠️ **为什么主同步那一组要强制全量**：

增量那一轮**不会重置**门店 10 里历次回放扣掉的量（增量只导"源库变过的行"，
而回放扣的是本地）⇒ 那些历史扣减与源库的差额会混进对账容差
⇒ **夜里（源库不卖货 ⇒ 容差≈0）必然对不上**。

⇒ 契约测试的两个断言分工：

- 主同步组：全量基线 ⇒ 逐行比对"门店 10 == 门店 00 经 cube"。
- 增量组：验"导出的行都落库了" + **"库存行数没变少"**（增量不得抹掉没变的行）。

⚠️ 增量组里 `exported == 0` 是**正常**的（源库没变 ⇒ 本地无须更新）；
但**非增量** scope 的 `exported == 0` 是缺陷（端点没导到数据）。
⇒ 增量白名单从 **unit 的 env** 里读，不写死在测试里（写死会与部署配置漂移）。
