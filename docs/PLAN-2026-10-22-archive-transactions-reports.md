# 商品档案 / 流水查询 / 促销方案 / 采购收货 / 销售报表 —— 跨仓实现契约

> 日期：2026-10-22　状态：**实施中**
> 涉及仓库：`supertrade`（后端）/ `cube`（语义层）/ `deployer`（契约测试 + 部署）/ `tradewind_app`（前端）/ `auth`（权限点迁移）
> 本文是这 5 个功能域**跨仓契约的唯一真相源**。四个仓的实现在本文定稿后并行推进，字段名以本文为准。

---

## 0. 范围与口径

| # | 功能域 | 参考图 | 本次范围 | 数据源 |
|---|---|---|---|---|
| A | 商品档案管理 | 图1 | 商品列表 + 类别树 + **供货信息页签** | 商品属性**复用现有 supertrade 商品档案**，本次**不新增属性** |
| B | 商品销售流水 | 图2/图3 | 查询 + 明细表 | cube `sale_detail` |
| B' | 收银流水 | 图6 | 查询 + 明细表 | cube `payflow` |
| C | 促销方案 | 图4 | **只读**（列表 + 详情） | cube `promotion_plan` |
| D | 采购收货 | 图5 | **只读**（单据列表 + 单据详情）；左侧单据列表**可关闭** | cube `purchase_sheet` |
| E | 销售报表 | 图7 | **左侧 11 个菜单项全部实现** | cube `sales_report` / `sale_day` |

**分流规则（贯穿全部 5 个域）**：门店在 `branch_cube_sources` 里配了数据源 ⇒ 走该门店专属 cube 实例；没配 ⇒ 走本域 PG。判据只有一个实现（`internal/cubesource`），三个状态严格区分：

| 状态 | 读端点 | 写端点 |
|---|---|---|
| 没配数据源（自营店 10） | 读本域 PG | 放行 |
| 配了但被停用 | **503**，不回落本地 | **拒** |
| 问不到路由 | **503**，不回落本地 | 放行（只记 warn） |

> ⚠️ **回落本地 = 把一次链路故障显示成"这个门店没有数据"**。这是 2026-10-21 生产事故的形状，禁止回落。

**两条路径必须给逐字相同的答案**：本地路径与 cube 路径**共用同一批响应结构体**（照抄 `internal/catalog/service/picker_local.go` 的做法：用 `cubeclient.ProductDTO` 这类 DTO 保证形状一致），并**必须有测试断言两条路径的输出结构相等**。

---

## 1. 源库实测结论（2026-10-22 实测，两库真库）

勘察工具：cube 仓 `cmd/sqlq`（只读，源库是生产库）。ysx 库走 `ssh gyy` 隧道（`192.168.1.200:1433` / hbposv10），hbposv7 库直连（`172.12.1.6:59527` / hbposepro）。

### 1.1 逐笔销售流水 ⇒ `t_rm_saleflow`（**不是**现有的 `t_mp_saleflow`）

| 表 | ysx (hbposv10) | hbposv7 (hbposepro) |
|---|---|---|
| `t_rm_saleflow` | **3,327,514** | **1,743,882** |
| `t_rm_payflow` | **1,143,219** | **550,213** |
| `t_mp_saleflow`（现 `sale_detail` 配的） | 0 | — |
| `t_rm_order_saleflow`（现 hbposv7 配的） | — | **存在但是空壳**:0 行、只有 3 列(com_no / flow_id / flow_no) |

⇒ 现有 `sale_detail` model **两库都拿不到数据**：ysx 配的表是空的，hbposv7 配的表是空壳。
⇒ **`t_rm_saleflow` 两库都有、都有真实数据、列大体一致** ⇒ 可以进**共享 schema**（只映射两库共有的列）。

> ⚠️ **2026-10-23 订正**：`t_rm_order_saleflow` **不是"表不存在"**，而是**空壳表**
> （0 行 / 3 列）。结论不变（取不到数据），但"表不存在"会让排障时去查
> "是不是表名写错了" ⇒ 真实原因是那张表**从来没用过**。

` t_rm_saleflow` 列（两库共有 25 列，⚠️ **只映射共有列**）：

| canonical | 源列 | 类型 | 图2/图3 口径 |
|---|---|---|---|
| `id` | `flow_id` | numeric | 流水 ID |
| `order_no` | `flow_no` | char | **单号** |
| `random_order_no` | `flowno_rand` | char/varchar | **随机单号** |
| `branch_no` | `branch_no` | char(6) | 分店 |
| `product_id` | `item_no` | char(20) | 货号 |
| `source_price` | `source_price` | numeric | **原价**（单价） |
| `sale_price` | `sale_price` | numeric | **销售价**（单价） |
| `qty` | `sale_qnty` | numeric | 数量 |
| `sale_amount` | `sale_money` | numeric | **销售金额** |
| `returned_qty` | `ret_qnty` | numeric | 退货数量（⚠️ 仅 ysx 有 ⇒ **不进映射**） |
| `sale_way` | `sell_way` | char(1) | **销售方式** |
| `cashier_id` | `oper_id` | char(4) | **收银员编码** |
| `salesman_id` | `sale_man` | char(4) | **营业员编码** |
| `counter_no` | `counter_no` | char(4) | **柜台号** |
| `sold_at` | `oper_date` | datetime | **销售时间** |
| `online_flag` | `remote_flag` | char(1) | **断网标志** |
| `discount_amount` | `pref_amt` | numeric | **折让金额** |
| `cost_price` | `in_price` | numeric | 进价 |
| `shift_no` | `shift_no` | char(9) | 班次 |
| `pos_id` | `posid` | char(4) | POS 号 |
| `spec_sheet_no` | `spec_sheet_no` | char | 促销单号 |

两库**不一致**、**不映射**的列：`n_stan`/`flowno_type`/`ret_qnty`/`cic_sheet_no`（仅 ysx）；`paysum_flag`/`voucher_no`/`pr_spec_*`/`ot_spec_*`/`item_no_Fresh`（仅 hbposv7）。

> ⚠️ **2026-10-23 补：退货口径（重要，之前 PLAN 里没写清）**
>
> 实测两个生产源库：
>
> - 逐笔流水表里**没有"退货行"**：`sale_qnty` / `sale_money` / `sale_price` 的
>   **负值行都是 0 行** ⇒ **不能用"负数 = 退货"**（那样恒等于"全是销售"）。
> - 退货是**同一行里的另一列**：ysx 的 `ret_qnty`
>   （实测 1939 行非 0，且**恒等于同行 `sale_qnty`** ⇒ 卖 2.545 件、其中 2.545 件被退）。
>   hbposv7 **没有**这一列 ⇒ 该库逐笔流水里没有退货。
> - ⇒ **共享 schema 给不出"S/R"**（没有两库共有的可判列）⇒
>   `order_status` 维度**已删除**（它此前恒为 'S'，是个会骗人的假维度）。
> - ⇒ **净额**由 family 私有的 live 片段算（`net_qty` / `net_amount_yuan`）：
>   ysx = (sale_qnty − ret_qnty) × sale_price；hbposv7 = sale_qnty × sale_price。
>   ⇒ 片段见 `cube/semantic-layers/sixun/internal/source/sale_detail_sql.go`，
>   配置项 `source.sale_detail_return_qty_col`（ysx = ret_qnty；hbposv7 不配）。
> - ⇒ **单据级退货**走 `purchase_sheet` 的 `purchase_return` segment；
>   **销售报表的净销售**走 `sales_report`（`t_rm_daysum` 的 `ret_amt`/`ret_qty`，
>   实测 1217894 天里 1801 天有退货）。

### 1.2 收银流水 ⇒ `t_rm_payflow`（两库共有 25 列）

| canonical | 源列 | 图6 口径 |
|---|---|---|
| `id` | `flow_id` | — |
| `order_no` | `flow_no` | **单号** |
| `random_order_no` | `flowno_rand` | **随机单号** |
| `sale_amount` | `sale_amount` | **单据金额** |
| `branch_no` | `branch_no` | 分店 |
| `pay_way` | `pay_way` | **付款方式**（实测 `SWX`） |
| `sale_way` | `sell_way` | **销售方式** |
| `card_no` | `card_no` | **付款卡(券)号** |
| `vip_no` | `vip_no` | **会员卡号** |
| `currency` | `coin_no` | 币种（`RMB`） |
| `exchange_rate` | `coin_rate` | **汇率** |
| `pay_amount` | `pay_amount` | **付款金额** |
| `paid_at` | `oper_date` | **销售时间** |
| `cashier_id` / `salesman_id` / `counter_no` | `oper_id`/`sale_man`/`counter_no` | 收银员/营业员/柜台 |
| `online_flag` | `remote_flag` | 断网标志 |

### 1.3 促销方案头 ⇒ `t_pub_plan_master`（ysx 2,646 行）

| canonical | 源列 | 图4 口径 |
|---|---|---|
| `plan_no` | `plan_no` | **方案编号**（实测 `PS002109160001`） |
| `plan_name` | `plan_name` | **方案名称** |
| `plan_memo` | `plan_memo` | **方案摘要** |
| `begin_date` / `end_date` | `begin_date`/`end_date` | 开始/结束日期 |
| `created_at` | `oper_date` | **制单日期** |
| `created_by` | `oper_man` | **制单人** |
| `approved_at` | `confirm_date` | **审核日期** |
| `approved_by` | `confirm_man` | **审核人** |
| `stopped_at` | `stop_date` | **终止日期** |
| `stopped_by` | `stop_man` | **终止人** |
| `approve_flag` | `approve_flag` | **审核状态**（实测 `2` = 已审核） |
| `member_level` | `vip_type` | **会员等级**（实测 `无`） |
| `rule_no` | `rule_no` | 规则类型（`PS` = 促销） |

现有 `promotion` model = `t_pub_plan_flow` = **商品行**表（方案 → 商品 → 价），**不是**方案头。图4 需要的是方案头 ⇒ **新增 `promotion_plan` model，两者并存**。

### 1.4 商品供货信息 ⇒ `t_bd_supcust_item`（ysx 43,751 行）

| canonical | 源列 | 图1 供货信息页签 |
|---|---|---|
| `product_id` | `item_no` | 货号 |
| `branch_no` | `branch_no` | 分店 |
| `supplier_id` | `supcust_no` | 供应商编码 |
| `appointed_price` | `appointed_price` | **约定价格** |
| `top_price` | `top_price` | **最高价** |
| `bottom_price` | `bottom_price` | **最低价** |
| `last_price` | `last_price` | **最近价** |
| `sale_way` | `sale_way` | **经营方式** |
| `contract_date` | `contract_date` | **合同有效期** |

> ⚠️ cube 仓 `sales_report/schema.yaml:70-71` 写着「实测无商品-供应商关联表」——**该结论已过期**，表名是 `t_bd_supcust_item`，不是 `t_bd_item_supply`/`t_bd_supply`。**该注释要一并订正**（否则下一个人还会去重找一遍）。

### 1.5 商品档案价格 ⇒ `t_pc_branch_price`（ysx 27,569 行）

`branch_no` + `item_no` + `supcust_no` + `sale_way` + `price`（批发价）+ `base_price`（进价）+ `sale_price`（零售价）+ `vip_price`（会员价）+ `sup_ly_rate` + `status`。

⇒ 图1 主表的 **进价 / 批发价 / 零售价 / 会员价** 四列全部出自此表。

### 1.6 采购收货 / 销售报表 —— cube 已覆盖，本次**不改 schema**

- `purchase_sheet`（`t_pm_sheet_master`）+ `purchase_sheet_line`（`t_pm_sheet_detail`），带 `purchase_receipt`/`purchase_return` segment。
  ⚠️ 净额必须用 `total_net_amount_yuan`（采购侧 RO 是**正数**，结算侧是负数）。
  ⚠️ `purchase_sheet_line` **没有** `trans_no` ⇒ 退货明细必须 join `purchase_sheet` 才能判段。
- `sales_report`（`t_rm_daysum`，三段 join item → item_cls → item_cls_parent）+ `sale_day`。
  ⚠️ 成本必须配 `costed_rows` 一起用（`total_ref_cost` 是估算值）。

---

## 2. cube 仓改动清单

### 2.1 model 清单

| model | 动作 | 源表 | storage | 用途 |
|---|---|---|---|---|
| `sale_detail` | **改**：源表 `t_mp_saleflow` → `t_rm_saleflow`；schema 按 §1.1 重写维度；live 源是**片段**(净额要扣退货，见 §1.1 补注) | `t_rm_saleflow` | live | 图2/图3 |
| `payflow` | **新增** | `t_rm_payflow` | live | 图6 |
| `promotion_plan` | **新增** | `t_pub_plan_master` | live | 图4 |
| `supplier_item` | **新增** | `t_bd_supcust_item` | live | 图1 供货信息 |
| `branch_price` | **新增** | `t_pc_branch_price` | live | 图1 价格四列 |
| `product` | **改**：加 `brand_name` 维度（源列 `item_brandname`，两库都有、覆盖率 100%） | `t_bd_item_info` | live | 图1 品牌 |
| `purchase_sheet(_line)` | 不改 | — | live | 图5 |
| `sales_report` / `sale_day` | 不改 | — | live | 图7 |

### 2.2 每个新/改 model 的完整文件集（照 cube 仓 `docs/data-source-mapping-playbook.md` §3.4）

1. `sixun-models/<model>/schema.yaml`（live model 不写 `preagg.go`；duck 改动才需要）
2. `semantic-layers/sixun/cmd/sixun-ysx/mapping/mapping-<model>.yaml`
3. `semantic-layers/sixun/cmd/sixun-hbposv7/mapping/mapping-<model>.yaml`
4. 两个 `cmd/sixun-*/main.go`：`appConfig.Source.TableX` + `liveTables()` 条目
5. 两个 `cmd/sixun-*/config.example.yaml` 加 `table_<model>`
6. 两个 `cmd/sixun-*/config.yaml`（工作区，不入库）加 `table_<model>`
7. `cmd/sqlcheck/main.go`：加 check case + `srcTables` 条目（live model 必加，否则 `livetables_test.go` 红）

⚠️ **`sale_detail` 改源表时，`config.yaml` 的 `table_sale` 必须一起改**（ysx `t_mp_saleflow` → `t_rm_saleflow`；hbposv7 `t_rm_order_saleflow` → `t_rm_saleflow`）。漏改 ⇒ 查询打到空表/不存在的表，**零报错**。

### 2.3 自动生效、不用改

`embed.go`（`//go:embed */schema.yaml` 自动发现）、`mappingAssets`（`//go:embed mapping/*.yaml`）、`pkg/cubequery/registry.go`（运行时构造）。

### 2.4 cube 自检（`AGENTS.md` §7）

- [ ] 两个 family 的 mapping **target 集合逐字相同**（`mapping_parity_test.go`）
- [ ] measure/segment 的 SQL 里只出现 canonical 名
- [ ] live model 的 `table_*` 在**三份拷贝**（两个 `main.go` 的 `liveTables` + sqlcheck 的 `srcTables`）齐全（`livetables_test.go`）
- [ ] `embedcheck` 双向齐全
- [ ] **真机跑** `go run ./cmd/sqlcheck -family ysx|hbposv7`（单测只比对字符串，不执行）
- [ ] 部署后 `COUNT(*)` 对账：`t_rm_saleflow` = 3327514 / 1743882，`t_rm_payflow` = 1143219 / 550213，`t_pub_plan_master` = 2646，`t_bd_supcust_item` = 43751，`t_pc_branch_price` = 27569

---

## 3. supertrade 端点契约（前端按本文实现）

> 路径从 root 起（nginx 已剥 `/api/v1/<app>/`）；资源复数 + kebab-case；**不注册** `/healthz`；branch 一律走 `X-Branch-ID` header。

### 3.1 catalog（AppID `catalog`，dapr app-id `supertrade-catalog`）—— 商品档案

```
GET /product-archive?q=&category_id=&supplier_id=&page=1&page_size=50
GET /product-archive/:id/supplier-prices
```

`GET /product-archive` 响应：

```json
{
  "items": [{
    "id": "00001", "item_no": "00001", "sub_code": "00001",
    "name": "小白菜", "is_points": false,
    "category_id": "010201", "category_name": "叶菜类",
    "supplier_id": "019", "supplier_name": "自采蔬菜",
    "spec": "500g", "brand_name": "其它品牌", "unit": "kg",
    "cost_yuan": "4.9600", "wholesale_yuan": "3.6000",
    "retail_yuan": "4.7600", "member_price_yuan": null,
    "status": "active"
  }],
  "count": 50, "page": 1, "page_size": 50, "total": 27569
}
```

`GET /product-archive/:id/supplier-prices` 响应：

```json
{
  "items": [{
    "item_no": "00001", "branch_id": "00", "branch_name": "总部仓库",
    "supplier_id": "019", "supplier_name": "自采蔬菜",
    "appointed_price_yuan": "0.0000", "top_price_yuan": "8.0000",
    "bottom_price_yuan": "1.4000", "last_price_yuan": "4.7600",
    "sale_way": "A", "contract_date": "2026-10-24"
  }],
  "count": 1
}
```

### 3.2 transaction（**新增服务**，AppID `transaction`，dapr app-id `supertrade-transaction`，端口 8119）—— 流水查询

```
GET /transactions/sales?q=&from=&to=&branch_id=&product_id=&category_id=
                   &sale_way=&cashier_id=&salesman_id=&counter_no=
                   &page=1&page_size=50
GET /transactions/sales/:id
GET /transactions/payments?q=&from=&to=&branch_id=&pay_way=&cashier_id=
                     &card_no=&vip_no=&page=1&page_size=50
```

销售流水行（`GET /transactions/sales` 的 `items[]`）：

> ⚠️ **`id` = 单号（`order_no`），不是 `flow_id`**（2026-10-23 实测后修正）。
> 实测 `t_rm_saleflow` 330 万行里 **`flow_id` 只有 96 个不同值**（它是复用序号，不是键），
> `flow_no`（单号）有 1,057,434 个不同值 ⇒ **单号是唯一可寻址的单位**。
> ⇒ 早先用 `flow_id` 当 id ⇒ 详情端点 `id = <flow_id>` 会命中**别的单**
> （实测：列表里 `id=9 / order_no=06202610220364`，详情却返回 `order_no=01202110010025`，
> 2021 年的另一张单）。
> ⇒ 现在 `id` = 单号，列表/详情一致；`ORDER BY` 也从 `flow_id desc` 改成 `order_no desc`
> （单号含日期，字典序≈时间序；`flow_id desc` 原本等于**乱序**）。
> ⇒ 前端把 `id` 当**不透明串**回传给详情端点即可 ⇒ 语义变更对前端无感。

```json
{
  "id": "9", "order_no": "06202610220435", "random_order_no": "06429097872618",
  "branch_id": "0001", "sold_at": "2026-10-22T21:54:52+08:00",
  "product_id": "6911754103296", "sub_code": "00001", "product_name": "小白菜",
  "spec": "500g", "brand_name": "其它品牌", "unit": "kg",
  "sale_way": "A", "counter_no": "9999", "counter_name": "缺省柜台",
  "cashier_id": "1012", "cashier_name": "谢梅坤",
  "salesman_id": "9999", "salesman_name": "缺省营业员",
  "qty": "1.0000", "sale_amount": "2.5000", "source_amount": "2.5000",
  "discount_rate": "100.00", "discount_amount": "0.0000",
  "sale_price": "2.5000", "source_price": "2.5000",
  "online_flag": "0", "pay_card_no": "", "let_goods_type": ""
}
```

收款流水行（`GET /transactions/payments` 的 `items[]`）：

```json
{
  "id": "1", "order_no": "06202610220435", "random_order_no": "06429097872618",
  "branch_id": "0001", "paid_at": "2026-10-22T21:54:52+08:00",
  "sale_way": "A", "pay_way": "SWX", "pay_way_name": "思迅Pay-微信",
  "sale_amount": "22.0000", "exchange_rate": "1.0000", "pay_amount": "22.0000",
  "rmb_amount": "22.0000", "card_no": "", "vip_no": "",
  "cashier_id": "1012", "cashier_name": "谢梅坤",
  "counter_no": "9999", "salesman_id": "9999"
}
```

> ⚠️ **金额一律 JSON 字符串**（后端 decimal），前端用 `parseDecimal()`，**不要当 number**。
> ⚠️ **折扣度 = `sale_price / source_price * 100`**（源库没有这一列，是派生列）。`source_price = 0` 时折扣度为空，**不编造 100%**。

### 3.3 pricing（AppID `pricing`）—— 促销方案（只读）

```
GET /promotion-plans?from=&to=&plan_no=&plan_name=&approve_status=&created_by=&member_level=&exclude_expired=true&page=1&page_size=50
GET /promotion-plans/:plan_no
```

> ⚠️ **2026-10-23 补 `member_level`**:前端一直在发这个参数,后端此前**静默忽略**
> ⇒ "点了筛选没反应"与"参数被忽略"长得一模一样。
> 现在后端真的过滤(cube 侧 `promotion_plan.member_level` ← 思迅 `vip_type`)。
> ⚠️ 实测两个源库的取值**恒为"无"** ⇒ 过滤它通常不改变结果,但**必须真的在过滤里**。

列表行：

```json
{
  "plan_no": "PS002109160001", "plan_name": "开业促销", "plan_memo": "开业促销",
  "begin_date": "2021-09-17", "end_date": "2021-09-18",
  "created_at": "2021-09-17T09:55:13+08:00", "created_by": "2001",
  "approve_status": "approved", "approve_status_label": "已审核",
  "approved_at": "2021-09-17T09:56:32+08:00", "approved_by": "2001",
  "member_level": "无", "stopped_by": "5001", "stopped_at": "2021-09-24T11:13:56+08:00"
}
```

`approve_status` 取值：`draft`（未审核）/ `approved`（已审核）/ `stopped`（已终止）/ `unknown`。
⚠️ 源列 `approve_flag` 是 varchar(1)，实测 `2` = 已审核。**映射必须显式写死判据**（`CASE WHEN approve_flag='2' THEN 'approved' ELSE 'draft' END`），不要把源码直接透给前端。

### 3.4 procurement（**新增域**，AppID `procurement`，dapr app-id `supertrade-procurement`，端口 8120）—— 采购收货（只读）

```
GET /purchase-sheets?from=&to=&branch_id=&supplier_id=&approve_status=&order_no=&document_type=&created_by=&page=1&page_size=50
GET /purchase-sheets/:sheet_no
```

> ⚠️ **2026-10-23 补 `created_by`**:前端一直在发这个参数,后端此前**静默忽略**
> ⇒ 现在真的过滤(cube 侧 `purchase_sheet.operator_id` ← 思迅 `oper_id`;
> 本域 PG 侧 `created_by`,两条路径**同一个口径**:制单人)。
>
> ⚠️ **`amount_gte` 仍然不支持**(前端已标注):金额是**度量**
> (`total_net_amount_yuan`),而 cube 的过滤只作用于**维度**
> ⇒ "金额 ≥ X"没法表达。
> ⇒ **不要**悄悄改成按度量近似过滤(那会让 cube 路径与本域路径给出不同答案)。

列表行：

```json
{
  "sheet_no": "PI002610224944", "branch_id": "0001", "branch_name": "总部仓库",
  "supplier_id": "084", "supplier_name": "水果（自营）",
  "document_type": "PO", "document_type_label": "采购收货单",
  "approve_status": "approved", "order_status": "finished",
  "created_at": "2026-10-22T00:00:00+08:00", "created_by": "3001",
  "confirmed_at": "2026-10-22T00:00:00+08:00", "confirmed_by": "3001",
  "total_amount_yuan": "241.00", "total_net_amount_yuan": "241.00",
  "currency": "CNY", "pay_way": "CASH", "remark": ""
}
```

详情 = 列表行 + `lines[]`：

```json
{
  "line_no": 1, "product_id": "000552", "product_name": "凯特芒", "unit": "kg",
  "order_qty": "19.60", "received_qty": "19.60",
  "valid_price_yuan": "6.22", "amount_yuan": "122.00"
}
```

### 3.5 sales-agg（AppID `sales-agg`）—— 销售报表（图7 左侧 11 项）

```
GET /reports/sales/menu-items
GET /reports/sales/by-code?code=&from=&to=&branch_id=&product_id=&category_id=&supplier_id=&sale_way=&brand_name=&page=1&page_size=100
```

`menu-items` 返回前端菜单（前端不硬编码菜单文案）：

```json
{ "items": [
  {"code": "product_barcode_group", "label": "商品销售条码分组统计"},
  {"code": "product_price_group", "label": "商品销售价格分组统计"},
  {"code": "product_branch",        "label": "商品-分部汇总"},
  {"code": "category_branch",       "label": "类别-分部汇总"},
  {"code": "branch",                "label": "分部汇总"},
  {"code": "location",              "label": "库位销售汇总"},
  {"code": "category_large",        "label": "大类汇总"},
  {"code": "category",              "label": "类别汇总"},
  {"code": "daily_average",         "label": "日均销售"},
  {"code": "product_detail",        "label": "商品明细"},
  {"code": "supplier",              "label": "供应商销售汇总"}
] }
```

`by-code` 响应（所有报表共用一个壳，列随 `code` 变）：

```json
{
  "code": "product_barcode_group",
  "columns": [{"key":"label","title":"品名","type":"string"},
              {"key":"net_amount_yuan","title":"销售金额","type":"decimal","align":"right"}],
  "rows": [{"label":"茄子","product_id":"00001","unit":"kg","net_qty":"11.09",
            "net_amount_yuan":"52.80","amount_ratio":"0.36%"}],
  "summary": {"net_amount_yuan":"18896.38","net_qty":"3573.85"},
  "count": 1, "page": 1, "page_size": 100, "total": 1
}
```

> ⚠️ **所有报表端点必须挂 `rbac.RequireScopeWithBranch`**（现有 `/reports/sales` **漏挂了** —— 新端点不要沿用这个疏漏）。

### 3.6 权限 scope（auth 仓迁移 005 已加）

`product:view`（已有）/ `pricing:view`（已有）/ `transaction:view`（新）/ `procurement:view`（新）/ `sales:view`（新）。

已落地：`F:\go\src\github.com\YunBright\auth\migrations\005_transaction_procurement_sales_scopes.up.sql`
—— 新增 3 个权限点；admin 全量；merchant（店长/店长助理）三个都给；**cashier 一个不给**（收银员不需要跨单据的流水/报表/采购查询，且 `procurement:view` 能看到进价）。

> ⚠️ **三个新 scope 必须与 `cube:read` 一起给**：`internal/cubesource` 问 cube-router 的 `/v1/branch-source`（该端点挂 `cube:read` 守门）⇒ **连"这家店没有 cube、该查本地 PG"这句话本身都要先有 cube:read 才问得到**。merchant 已有 `cube:read`（001 给过）⇒ 满足。admin 走全量 ⇒ 满足。
> ⚠️ 现有 `product:manage` / `pricing:manage` 在 auth seed 里**还不存在** ⇒ 写端点对所有人 403（fail-closed，有意为之）。本次只读功能不依赖新写 scope。
> ⚠️ **生产还要改 JWT_AUDIENCE**：baiyuan-i `/opt/auth/.env` 的 `JWT_AUDIENCE` 必须加**短名** `transaction` 与 `procurement`（**不是** dapr app-id `supertrade-transaction` / `supertrade-procurement`）。缺了 ⇒ 目标服务 401 `invalid_audience`，而它自己的 `/healthz` 仍 200（公开端点注册在中间件之前）⇒ 看起来"服务是活的"。改完必须**重新登录**（`JWT_TTL=1h` 内旧 token 仍是老 aud）。

### 3.7 错误码

沿用各域既有 code，**不得**改成通用 `not_found`（`supplier_not_found` / `product_not_found` 等是线上契约）。

新增/复用：

| sentinel | HTTP | code |
|---|---|---|
| `ErrCubeSourceUnavailable` | 503 | `cube_unavailable` |
| `ErrCubeSourceDisabled` | 503 | `cube_source_disabled` |
| `ErrExternalSourceReadOnly` | 409 | `member_source_is_external` |
| `ErrCubeForbidden` / `ErrCubeAuthRejected` | 403 / 401 | `forbidden` / `unauthorized` |

---

## 4. deployer 仓改动清单

| 项 | 动作 |
|---|---|
| `systemd/supertrade-transaction.service` | **新增**（照抄 `supertrade-member.service`：`Wants=` 而非 `Requires=`；`--app-port 8119` / `--dapr-http-port 3119` / `--dapr-grpc-port 6119`；`--resources-path` 必须含 shared-components） |
| `systemd/supertrade-procurement.service` | **新增**（同上，端口 8120） |
| `dapr/gyy/shared-components/authkey.yaml` | **新增两个 app-id**（`supertrade-transaction` / `supertrade-procurement`）—— `tests/config/jwks_state_test.go::TestAuthkeyScopesCoverEveryDaprApp` 会反向解析 unit 的 `--app-id` 校验 |
| `nginx/yun-bright.conf` | **map 加两行**（`~^/api/v1/transaction/ "supertrade-transaction";` / `~^/api/v1/procurement/ "supertrade-procurement";`）。⚠️ **只加 map 行**，不加 location —— 加新 app 只动这一行 |
| `tests/contract/` | **新增** `transaction_test.go` / `promotion_plans_test.go` / `purchase_sheets_test.go` / `product_archive_test.go` / `sales_reports_test.go` |
| `tests/config/` | 若新增 unit，`systemd_unit_test.go` / `dapr_sync_test.go` 会自动覆盖 |

### 4.1 契约测试必须断言的（不是"端点在、字段对"）

⚠️ 记忆里的老教训：**"查询端点返回了数据"不等于"查询条件生效了"**。每个查询端点要**同时断言命中数**，且用**客户端真实发的那份查询串**。

| 断言 | 判据 |
|---|---|
| 分流正确 | 有源门店（00/01）走 cube；无源门店（10）走本域 PG；两条路径**响应结构逐字相同** |
| 三态 | 没配 ⇒ 本地；停用 ⇒ 503 `cube_source_disabled`；问不到 ⇒ 503 `cube_unavailable`（**不回落本地**） |
| 空值语义 | 「没有维护」是 `null`，**不是 `0`** |
| 命中数 | 用查询条件过滤后 `count` 显著小于不过滤时的 `total`（证明过滤真的生效） |
| 分店隔离 | `X-Branch-ID: 00` 与 `10` 返回的行**不相交** |

**门店 → 数据源（2026-10-22 实测）**：

| 门店 | 数据源 | 行为 |
|---|---|---|
| `00` 富百家 | `cube-sixun-ysx-fb` | 走 cube |
| `01` 佳乐 | `cube-sixun-hbposv7-jiale` | 走 cube |
| `10` 云商 | 无 | 走本域 PG |
| `B01` 测试 | 无且无数据 | 空列表是**正确**行为 |

---

## 5. tradewind_app 前端

### 5.1 分层与复用（硬约束）

- `ui/` 只放 widget；`application/` 放 notifier；`domain/` 放实体 + repository 接口（**不得 import `package:flutter/*`**）；`infra/` 放 dio 实现。
- **必须复用** `lib/theme/app_theme.dart` + `lib/widgets/tw_*.dart`，**不许重写**。可用的：
  `TwDataTable`（表格）、`TwSectionCard`（卡片）、`TwFormField`/`TwDropdownField`/`TwDateField`（筛选）、`TwSearchBar`、`TwEmptyState`、`TwSummaryCard`、`TwStatusBadge`、`TwAppBar`、`TwActionFooter`、`TwOperateBranchPicker`、`lib/ui/shared/product_group_tree.dart`（类别树）、`lib/ui/shared/permission_gate.dart`。
- ⚠️ **没有**通用筛选面板 / 通用 tab 条 / 可折叠面板 / 时间选择器 / 图表库。
  - 筛选面板照抄 `lib/ui/stocktake/list/stocktake_filter_bar.dart`
  - tab 条照抄 `lib/ui/warehouse/settings/warehouse_settings_screen.dart:151-208`
  - 左侧树 + 右侧列表的 split pane 照抄 `lib/ui/shared/product_picker.dart:584-597`
  - 表格 = Material `DataTable` + 横向 `SingleChildScrollView`（照抄 `lib/ui/sales_report/widgets/sales_detail_table.dart`）
- **不新增第三方依赖**（`AGENTS.md:223`）。
- **客户端不做 cube/本地分流** —— 一律发 `X-Branch-ID`，由后端分流。**不许加 `if (isCube)`**。
- 金额用 `parseDecimal()`；列表可能是 `null` 不是 `[]`，用 `parseList()`。
- 文案**硬编码中文**（暂无 `lib/l10n/`）。

### 5.2 新增页面与路由

| 页面 | 路由 | nav group | scope |
|---|---|---|---|
| 商品档案 | `/product/archive` | 新增 `product` 组 | `product:view` |
| 商品销售流水 | `/transactions/sales` | 新增 `transaction` 组 | `transaction:view` |
| 收银流水 | `/transactions/payments` | 同上（子项） | `transaction:view` |
| 促销方案 | `/pricing/promotion-plans` | 新增 `pricing` 组 | `pricing:view` |
| 采购收货 | `/procurement/purchase-sheets` | 新增 `procurement` 组 | `procurement:view` |
| 销售报表 | `/sales/reports` | `sales` 组（已有） | `sales:view` |

⚠️ **`StatefulShellRoute` 分支只能往后追加**（插入会挪动 `_systemTabIndex = 2` 打乱底部导航）。
⚠️ 底部导航**上限 5 项**（`test/widgets/tw_app_shell_test.dart` 强制）⇒ 第 6 个模块起走 **sidebar 组 + 首页入口卡片**（"系统"组是现成范例）。
⚠️ `nav_items.dart` 里已有两个 `route: null` 占位：`warehouse.purchase`（采购收货单）、`warehouse.inventory` —— 本次把 `warehouse.purchase` 接上真实路由。

### 5.3 文件布局（照现有约定）

```
lib/domain/<feature>/       实体 + repository 接口 + 查询对象（纯 Dart）
lib/application/<feature>/  Riverpod notifier（AsyncNotifierProvider.autoDispose.family）
lib/infra/<feature>/        dio 实现
lib/ui/<feature>/           页面 + _widgets/ 子目录
test/domain/<feature>/      单测
test/application/<feature>/ 单测
test/infra/<feature>/       单测
test/ui/<feature>/          widget 测
```

---

## 6. 验收闭环（修复 → 部署 → 契约测试）

```
1. cube 改完 → go build ./...（各 module 目录）→ go test ./...
              → 真机 sqlcheck → 部署（deploy-cube.ps1）→ 重启 → COUNT(*) 对账
2. supertrade 改完 → go build ./... → go test ./... → 部署（deploy-supertrade.ps1）
                  → 轮询健康端点（dapr_ok/auth_ok = true，90s 超时）
3. deployer 加 unit + nginx → 手工同步到服务器（**只改那一个参数，diff 确认**）
                        → 同步 authkey.yaml 的 scopes
4. 跑契约测试：pwsh tests/run-all.ps1 -Filter "Transaction|Promotion|Purchase|ProductArchive|SalesReport"
5. 红 → 定位 → 修 → 重跑，直到全绿
6. 前端：flutter analyze 0 warning + flutter test 全绿
7. 通知用户前端验收
```

⚠️ **部署顺序**：先部署"不再引用旧东西"的代码，再改 schema/配置（反过来 = 立刻 500）。
⚠️ **unit 同步纪律**：改启动参数**只动那一个参数**，不要拿仓库模板整份覆盖线上（2026-10-12 事故：catalog 起不来）。
⚠️ **行尾**：systemd unit 必须 LF（`tests/config/nginx_internal_test.go::TestDeploymentFilesUseLFNotCRLF` 拦回归）。
⚠️ **dapr job schedule**：本批功能**不需要**新的定时任务（全是同步查询端点）。若后续加异步搬运，走 `dapr/rebuild-scheduler.ps1`（幂等），回调路径恒为 `POST /job/<job 名>`。

---

## 7. 生产联调纪要（2026-10-22/23 实测，**五个新端点上线要改的五处**）

> 这五条**都不是**新功能本身的逻辑，而是"把一个新 dapr app 接进这套体系"必须补的东西。
> 每一条的症状都**指不到真因**，所以按症状查会查很久。

### 7.1 `AppID` 必须是**短名**（dapr app-id 不是 aud）

新服务照抄 `--app-id supertrade-transaction` 就把 Go 侧 `AppID` 也写成了 `supertrade-transaction`
⇒ token 的 aud 里带的是 `transaction` ⇒ **`RequireAudience` 报
401 `invalid_audience`「aud 不包含目标服务 supertrade-transaction」**，
而它自己的 `/healthz` 仍 200（公开端点注册在中间件之前）⇒ 看起来"服务是活的"。

⇒ **Go 侧 `AppID` = 短名**（`transaction` / `procurement`），
**dapr 侧 `--app-id` = `supertrade-transaction` / `supertrade-procurement`**，
**auth 的 `JWT_AUDIENCE` = 短名**。三处各写各的，别抄错。

### 7.2 `AUTH_S2S_ALLOWED_CALLERS` 必须登记新 app-id

`authkit/userinfo` 对 `/internal/*` **优先用机器身份**（服务账号换 token），
而机器身份只被**白名单里的 app-id**允许调用。
⇒ 新 app-id 不在白名单 ⇒ userd 拒收 ⇒ 本服务 **503 `userd_unavailable`**。

⚠️ **症状极具误导性**：连**纯静态**的 `/reports/sales/menu-items` 也 503 ——
因为 per-branch 守门要回头问 userd「这个用户在这家店有什么 scope」。
而管理台看到的只有"数据源暂不可用"。

⇒ 白名单登记的是 **dapr app-id**（`supertrade-sales-agg` / `supertrade-transaction` / `supertrade-procurement`）。

### 7.3 带机器身份的 unit 必须有 `EnvironmentFile=.../supertrade-auth-service-account.env`

没有它 ⇒ userinfo 回落成"透传调用方的真人 token" ⇒
`/internal/*` 要的是**机器专属 aud** ⇒ 真人 token 永远拿不到 ⇒ 401。

⇒ 判据：**代码里调了 `userinfo.New("userd")` 的服务 = 必须带这个 env 文件的服务**，
两个列表必须一致（`deployer/tests/config/service_account_secret_test.go` 钉住它）。

### 7.4 出站调用要**两个头都注入**：`Authorization` + `X-Branch-ID`

`middleware.XBranchID()` 只把门店写进 **gin.Context**，
而 `cubeclient` 出站时从 **request.Context** 读门店 ⇒ **少注入一次** ⇒
cube-router 收不到 `X-Branch-ID` ⇒ 返 400 `branch_required` ⇒
被翻成 **503「数据源路由暂不可用」**（文案完全指不到"少一个门店头"）。

⇒ 抄 `cmd/catalog/main.go` 的 `forwardBearerToOutgoing`：
`cubeclient.WithBearer` + `userinfo.WithBearer` + `cubeclient.WithBranchID`，**三个都要**。

### 7.5 cube 的 `order` 成员只认 **base model** 的成员

实测（直打 cube app `:8083/query`）：

| 查询 | 结果 |
|---|---|
| `dims=[item_cls.id]` + `order=[item_cls.id]` | **400** `QUERY_INVALID: order member not found: item_cls.id` |
| 同 dims，**去掉 order** | **200** |
| `order=[sales_report.oper_date]`（base model 成员，但没被 select） | **400** |

⇒ `item_cls` / `item_cls_parent` 是 **join 进来的** model ⇒ 不能拿它们的成员做 order。
⇒ 后端侧：**排序成员不是本 model 的 ⇒ 就不发 order**（行序由 cube 决定）。
⇒ cube 侧：order 校验与自己文档写的「order 成员必须被 select」不一致，**待放宽**。

### 7.6 顺带修掉的一个真 bug：`labelDim` 从来没被 SELECT

行标签取自 `cuberead.Str(r, spec.labelDim)`，但 `labelDim` **从来没进过查询的维度列表**
⇒ 四个 code 的标签恒为空（`product_barcode_group` / `product_price_group` / `category` / `category_large`）。
⇒ 修法：`labelDim` 非空且不在 dims 里 ⇒ 追加进 dims。

### 7.7 admin API 建不出「已停用」的数据源映射（真 bug）

`POST /admin/branch-cube-sources` 传 `"enabled": false` ⇒ **落库仍是 `true`**。

根因：`cube-router/service.Create` 里有一句 `in.Enabled = true // 默认启用;要禁用请在 Create 后调 Update`
⇒ service 无条件覆盖；`model.BranchCubeSource.Enabled` 是普通 bool，
**分不出"没传"与"显式 false"** ⇒ 只把 enabled 塞进 Create 的入参是**没用的**。

后果不是"多一个小功能"，而是**「配了但被停用」这一态压根建不出来**：
管理员临时关停某店数据源（维护）⇒ 读端点本该回 503「该门店的数据源已停用」，
实际却照常走 cube ⇒ 门店在维护期间照样查得到数据。

⇒ 修法：**handler 层**在 Create 之后补一次 `Update(enabled=false)`
（两次写只发生在 admin 操作上）；service 的"默认 true"保留给直接调用方。

### 7.8 逐笔流水没有"行级唯一键"（`flow_id` 不可用）

实测 `t_rm_saleflow`（330 万行）：

| 候选键 | distinct | 结论 |
|---|---|---|
| `flow_id` | **96** | ❌ 完全不是键（复用序号） |
| `flow_no`（单号） | 1,057,434 | ✅ **单号是唯一可寻址的单位**（一单约 3.15 行） |
| `(flow_id, item_no, oper_date)` | 3,165,811 | ❌ 仍有 16 万重复 |
| `chr_stan` | 0 | ❌ 全 NULL |

⇒ ⇒ **详情端点必须按"单号"寻址**，不是按行。
⇒ ⇒ 列表行的 `id` 改成**单号**（`order_no`），`ORDER BY` 也从 `flow_id desc` 改成 `order_no desc`
（单号含日期，字典序≈时间序；`flow_id desc` 原本等于**乱序**）。
⇒ ⇒ 前端把 `id` 当**不透明串**回传即可 ⇒ 对前端无感。



---

## 8. 交付状态（2026-10-23 生产验收）

### 8.1 验证结果

| 仓 | 验证 | 结果 |
|---|---|---|
| cube | `go build ./...`（5 个 module）+ `go test ./...` | 全绿 |
| cube | `cmd/sqlcheck -family ysx` / `-family hbposv7`（真源库） | **各 42/42 通过** |
| supertrade | `go build ./...` + `go vet ./...` + `go test ./...` | 全绿（exit 0） |
| deployer | `go test ./...`（含全部契约测试） | **ok 408.99s** |
| tradewind_app | `flutter analyze` | **No issues found!** |
| tradewind_app | `flutter test` | **+2011 ~10: All other tests passed!** |
| 生产 | 5 个功能域端点实调 | 全部 200 + 真实数据 |
| 生产 | 销售报表 11 个菜单项 | 11/11 = 200 |

### 8.2 生产实测（门店 00 富百家 / cube-sixun-ysx-fb）

| 端点 | 结果 |
|---|---|
| `GET /catalog/product-archive` | 200，商品/类别/供应商/进价/批发价/零售价/会员价齐 |
| `GET /catalog/product-archive/{id}/supplier-prices` | 200，约定价/最高价/最低价/最近价/经营方式/合同有效期齐 |
| `GET /transaction/transactions/sales` | 200，单号/随机单号/销售时间/货号/品名/规格/品牌/销售金额齐 |
| `GET /transaction/transactions/payments` | 200，单据金额/付款方式/汇率/付款金额/会员卡号齐 |
| `GET /pricing/promotion-plans` | 200，方案编号/名称/摘要/起止/制单人/审核状态/终止人齐 |
| `GET /procurement/purchase-sheets` | 200，单号/仓库/供应商/审核标志/单据金额齐（含详情 lines[]） |
| `GET /sales-agg/reports/sales/menu-items` | 200，11 个 code 全在 |
| `GET /sales-agg/reports/sales/by-code` | 200（11 个 code 逐个验过） |

### 8.3 生产上改过的东西（清单）

| 位置 | 改动 |
|---|---|
| `auth/migrations/005_*.sql` | **新增**：3 个权限点 + admin/merchant 授权；已 `auth-migrate up` 应用 |
| `/opt/auth/.env` | `JWT_AUDIENCE` += `transaction,procurement`；`AUTH_S2S_ALLOWED_CALLERS` += `supertrade-sales-agg,supertrade-transaction,supertrade-procurement`；auth 服务已重启 |
| `deployer/systemd/supertrade-transaction.service` | **新增**（8119/3119/6119） |
| `deployer/systemd/supertrade-procurement.service` | **新增**（8120/3120/6120） |
| `deployer/systemd/supertrade-sales-agg.service` | 加 `EnvironmentFile=.../supertrade-auth-service-account.env`（机器身份） |
| `deployer/nginx/yun-bright.conf` | map 加 2 行；`nginx -t` exit 0 后 reload |
| `deployer/dapr/gyy/shared-components/authkey.yaml` | scopes 加 2 个 app-id（已同步生产） |
| gyy `~/.config/systemd/user/` | 装 2 个新 unit + `daemon-reload` + `enable` |
| 部署 | cube ×2；supertrade（catalog / pricing / sales-agg / transaction / procurement / cube-router） |

### 8.4 已知遗留（不影响本次交付，但前端能看出来）

1. **合同外的筛选项仍是「待后端开放」**：§3 的端点签名没覆盖参考图上的全部筛选字段
   （流水有 `pos_id/brand_name/order_no/vip_no/pay_card_no/random_order_no/sub_code`；
   促销有 `member_level`；采购有 `created_by/amount_gte`）。
   前端照发（Go 的 `ShouldBindQuery` 忽略未知键 ⇒ 不 400），并在 UI 上标了「待后端开放」。
2. **「库位销售汇总」是用分店号近似的** —— cube 里**没有任何库位列**。
3. **「商品销售价格分组」的价格段边界（0-1/1-3/3-5/5-10/10-20/20-50/50+）是实现时选的** ⇒ 业务需确认。
4. **「AI 推送状态」列没有后端字段** ⇒ 前端渲染 `-`，不编造。
5. **采购收货底部页签「库位信息 / 其它」没有后端端点** ⇒ 显式空态。
6. **会员价/约定价在源库是 `0.0000` 而不是 NULL** ⇒ 「未维护」由**消费侧**规则翻译成 `null`。
7. **「品牌」区分度为零**：两库 `item_brandname` 覆盖率 100% 但**只有 1 个取值**（`PM`）。
8. **流水端点不带时间区间 ⇒ 默认「今天」**：逐笔表 330 万 / 114 万行，不带区间就是全表扫 ⇒ 撞超时
   （口径与思迅老系统一致：图2 的默认就是 00:00 → 23:59）。
9. **另一条工作流在同时改 cube**（会话 "Remove DuckDB layer from cube repo"）——
   本次交付把 DuckDB 路径**按 HEAD 恢复**并保持 `duck` 模型可用。
   **那条工作流若继续推进，cube 仓会再次漂移**，需与它协调后再合并。