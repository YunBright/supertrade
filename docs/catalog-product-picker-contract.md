# catalog 商品选择控件端点契约

> 面向 Flutter 商品选择控件(ProductPicker,`lib/ui/shared/product_picker.dart`)的
> 4 个只读端点契约。本文是**实现依据**与**判据来源**:
> 契约测试在 `deployer/tests/contract/catalog_product_picker_test.go`。
>
> 2026-10-20 首版。此前 `product-picker-contract.md`(flutter 仓)里的若干说法
> 与实际不符,本文以**生产实测**为准,差异见 §7。

---

## 一、数据来源与路由链路

catalog 本期**没有实现商品 CRUD**,基础数据全部来自思迅语义层 cube:

```
catalog (supertrade-catalog)
  → supertrade-cube-router   POST /v1/load     ← dapr service invocation
      → branch_cube_sources[branch_id].cube_source_name
          → 该门店的 cube 语义 app   POST /query
              → DuckDB 物化表 product / category / supplier
```

### 1.1 cube-gateway 不在这条链路上

`branch_cube_sources.cube_source_name` 存的是 **dapr app-id**,不是 gateway 的
wire source id。生产实测:

| branch_id | cube_source_name | cube 语义 app 的商品数 |
|---|---|---|
| `00` | `cube-sixun-ysx-fb` | 27565 |
| `01` | `cube-sixun-hbposv7-jiale` | 44569 |

cube-gateway 是"**源注册表**"(谁注册了哪个源、源叫什么 wire id),只有把
`CUBE_APP_ID` 配成 `cube-gateway` 时才会经过它。当前 `supertrade-catalog` 的
环境里**没有** `CUBE_APP_ID`,走的是 `internal/cubeclient/errors.go` 的默认值
`supertrade-cube-router` + 路径 `v1/load`。

⇒ **不要为了"补上 cube-gateway"去改路由**。门店隔离靠 cube-router 读
`branch_cube_sources` 实现,与 gateway 无关。

### 1.2 门店隔离的真正机制

门店隔离**不靠 cube 查询里的 branch 维度**(思迅 `stock.branch_id` 是
`branch_no`,与本系统 `branch_id` 是两套完全无关的编码)。它靠的是:

```
X-Branch-ID → cube-router → branch_cube_sources → 该门店专属 cube 实例
```

一个 cube 实例 = 一家门店。到达 cube 的查询天然只含本店数据。

---

## 二、cube 侧可用能力(2026-10-20 生产实测)

### 2.1 model 与成员

| model | DuckDB 表 | 源表 | 可用维度 | 可用度量 |
|---|---|---|---|---|
| `product` | `product` | `t_bd_item_info` | `id` `name` `category_id` `supplier_id` `unit` `status` `sub_code` `created_at` | `count` `avg_price_yuan` `max_price_yuan` `min_price_yuan` `total_cost_yuan` |
| `category` | `category` | `t_bd_item_cls` | `id` `name` `parent_id` | `count` |
| `supplier` | `supplier` | `t_bd_supcust_info` | `id` `name` `type` `cust_flag` `supply_way` `cust_clsno` `contact` `phone` `email` `address` `status` `modified_at` | `count` `total_credit_yuan` |
| `sale_way` | `sale_way` | `t_bd_base_code`(`type_no='SW'` 子集) | `id` `name` | `count` |

> ⚠️ `supplier.supply_way` = 思迅 `t_bd_supcust_info.sale_way`(销售方式代码),
> 名字在 `sale_way` model 里(购销/代销/联营/租赁/自产/扣率代销)。
> `supplier.cust_clsno` 是**客户类别**(内部领用/批发/…),**不是** App 的"供应商类别"。

引用形式是 `<model>.<ref>`,例如 `product.id`。**写错会得到 400
`QUERY_INVALID`(message `dimension not found: xxx`),不是静默空结果。**

### 2.2 查询协议要点(`cube/pkg/cubequery`)

- `limit` —— 行数上限。
- `offset` —— 偏移量。**必须同时给 `order`**,否则 400:
  `offset=2 需要显式 order:分页必须建立在稳定排序上,否则每页可能重复或漏行`。
- `order` —— `[{id, order}]`,`id` 必须是**本次 SELECT 里的**成员。
  不给 `order` 时 `limit` 是任意截取。
- `total: true` —— 返回不受 limit/offset 影响的总行数(响应顶层 `total`)。
  不给时 `total` 恒为 0。
- 过滤算子:`equals` `notEquals` `contains` `notContains` `startsWith`
  `in` `notIn` `gt` `gte` `lt` `lte` `inDateRange` `notInDateRange`
  `onTheDate` `beforeDate` `afterDate`。
  其它一律 400 `unsupported operator`。
- **没有 OR 算子**。`product.id contains q` 与 `product.name contains q`
  必须分成两次查询,再在 Go 里合并。
- 维度回来是**字符串**(带 RTRIM),度量回来是数字。

### 2.3 ⛔ 没有跨 model join

`product` / `category` / `supplier` 都是 **duck** 存储模型;cube 的 join 引擎
只支持 `storage: live` 模型之间的 join。因此:

- `{"dimensions":["product.id","category.name"]}` → 400
  `dimension not found: category.name`(实测)。
- `product` 的 schema 里**声明了** `joins: supplier`,但 duck 路径不消费它。

⇒ **分类名 / 供应商名必须在 supertrade 侧用 Go join。**
每个端点的 cube 查询次数:

| 端点 | cube 查询数 |
|---|---|
| `/products` `/products/search` | 3(商品页 + 分类字典 + 供应商字典) |
| `/categories` | 2(分类字典 + 按分类的商品数) |
| `/suppliers/summary` | 2(供应商字典 + 按供应商的商品数) |

⚠️ **不要写成 N+1**。现有 `internal/catalog/handler/product.go:93-102`
对每个商品行调一次 `SearchSuppliers` —— 新端点不要照抄那个形状。

### 2.4 ⚠️ `product.status` 不是 active/inactive

实测两个源的状态码分布**完全不同**:

| status | ysx(00 店) | hbposv7(01 店) |
|---|---|---|
| `0` | 1 | 5059 |
| `1` | 27518 | 21651 |
| `2` | 14 | 17600 |
| `3` | 12 | 3 |
| `4` | 20 | 79 |
| `5` | – | 177 |

hbposv7 里 status=2 的是"精品五花肉""西红柿""娃娃菜"这类**正常在售**商品。

⇒ **不得把 `status == "1"` 当成"在售"过滤**:那会隐藏 hbposv7 的 17600 个在售
商品。也不得把 status 映射成 `active`/`inactive` —— 没有可信的映射表。

**本契约的口径**:
- `status` 原样返回思迅的状态码字符串(`"1"` `"2"` …),不翻译、不折叠。
- **不按 status 过滤商品**。46 个非 1 状态商品混在 27565 个里,不值得为它
  引入一个会隐藏上万在售商品的错误过滤。
- `with_counts` 的 count 与列表 total 用**同一口径**(都不过滤),保证
  「侧栏写 87、点进去只有 12」这种自相矛盾不会发生。

### 2.5 ✅ 大小写无关（2026-10-20 已解决）

cube 的 `contains` 编译成 `RTRIM(col) LIKE '%v%'`,而 **DuckDB 的 LIKE 大小写敏感**。
实测:`Face` 命中、`face` 不命中;`Z005` 命中、`z00` 不命中。

思迅商品名里混着拉丁品牌名(洁柔Face / C&S / H01-04),商品编码惯例大写
(`Z005-69`) —— 用户在小键盘 / 中文输入法下打出小写是常事,**搜不到就等于功能坏掉**。

**解法:cube 新增 `containsCI` 算子**(2026-10-20,`cube/pkg/cubequery/build.go`):

```sql
LOWER(RTRIM(name)) LIKE '%face%'   -- 模式在编译期 lower,仍走参数绑定
```

- 列侧 `LOWER(col)`、模式侧**在编译期** lower(不是 SQL 的 `LOWER(?)` ——
  「值永远不被拼进 SQL 文本」是这套编译器的铁律,值唯一的入参通道是参数绑定)。
- 两侧通配,与 `contains` 一致;`%` `_` `[` `]` `\` 照常转义。
- lower 只影响 ASCII 字母,中文与数字原样通过。
- **`contains` 本身语义不变**(仍大小写敏感)—— 既有调用方(supplier / promotion 等)
  的查询范围不能被悄悄放宽。

⇒ supertrade 侧:`/products/search?q=` 的**编码与名称两个子查询都用 `containsCI`**,
一次查询就够,不再需要"原样 q + `UPPER(q)`"两次查询(那个绕法只覆盖全大写这一种形态)。

**为什么加算子而不是加派生维度**(`name_lower` / `id_lower`):
派生维度要动 `sixun-models/product` 这个 **ysx + hbposv7 共享 schema**,
得同时改两个 family 的 mapping 并**重建 product 预聚合表** ——
该表被 stocktake / inventory / pricing / sales-agg / settlement-auditor 共用,
改坏了会同时打挂 5 个服务。加算子只动查询编译器,**不碰 schema / mapping / 数据**。

> ⚠️ 加算子的实测踩坑:2026-10-20 部署 cube 时,ysx 实例的 DuckDB **WAL 损坏**
> 导致 app 崩溃循环(与本次改动无关,用改动前的二进制复现同样打不开)。
> 详见 agent memory 的 `yunbright-prod` topic。
>
> ⚠️⚠️ **同日第二次事故(2026-10-20 22:24)**:部署 cube 后 cube 三个服务又崩溃循环,
> 这次**不是** WAL,而是 cube 归档里的 `components/bearer-auth.yaml` ——
> 一份 `middleware.http.bearer` 组件,jwksURL 指向 auth 合并后**已下线**的 `:8081`
> (现在是 `:8082`)。components 目录里的 Component YAML 是 **daprd 自动加载**的
> ⇒ 拉不到 JWKS ⇒ **daprd fatal 退出** ⇒ 整个 app 跟着死。
> 而 goreleaser 的 gateway 归档**带着** `dapr/components`
> ⇒ **每次部署 cube 都会把仓里那份覆盖回服务器** ⇒ 每次部署都会再炸一次。
> ⇒ 已在 **cube 仓**加回归锁(`dapr/components_test.go`)。
> ⚠️ deployer 仓那份锁**锁不住**:它扫的是 deployer 仓的 `dapr/`,
> 而被打包的是 cube 仓的 `dapr/components/` —— 两个不同文件。

### 2.6 `avg_price_yuan` 与 null

`price_yuan` 未维护时,`AVG(price_yuan)` 为 NULL,cube 的响应里**该 key 直接
不存在**(不是 `null`)。后端必须把它显式写成 `"price_yuan": null`。

---

## 三、端点契约

### 3.0 通用规则

| 项 | 规定 |
|---|---|
| 鉴权 | `Authorization: Bearer <token>` |
| 门店 | `X-Branch-ID` 必填,且必须是**单个具体门店** |
| 缺失 header | 400 `{"code":"branch_required"}` |
| `*` 通配 | 400 `{"code":"invalid_branch_scope"}` |
| `00,01` 多店 | 400 `{"code":"invalid_branch_scope"}` |
| scope | `/products*` → `product:view`;`/categories` `/suppliers/summary` → `supplier:view` |
| 响应 key | 一律 snake_case |
| 错误信封 | `{"code": "...", "message": "..."}` |
| cube 故障 | 502 `{"code":"cube_error"}`;cube client 未配置 503 `{"code":"cube_unavailable"}` |

> ⚠️ **不能用 `middleware.SingleBranchFromCtx` 判门店**:
> 它对 `*` 返回 `"*"` 本身,对 `"00,01"` 返回**第一项** `"00"`。
> 用它做"严格单店"校验,多店请求会**静默按 00 查** —— 放行了不该放行的请求。
> 正确做法:读 `middleware.BranchFromCtx(c)`,要求 `len == 1` 且该项不是 `*`。

> ⚠️ **`GET /suppliers/summary` 与 `GET /suppliers/:id` 的路由冲突**:
> 只注册 `:id` 时,`/suppliers/summary` 会被 `:id` 吃掉并返回
> `404 supplier_not_found`(实测)。新增 summary 路由后 gin 的静态段优先
> 规则会让它正确命中 —— 但要**在注册后实测一次**,别假设。

### 3.1 `GET /products`

全量浏览模式(前端 `ProductRepository.list`)。无 `q`。

| Query | 必填 | 默认 | 上限 |
|---|---|---|---|
| `category_id` | 否 | – | – |
| `supplier_id` | 否 | – | – |
| `supplier_category_id` | 否 | – | – |
| `page` | 否 | 1 | ≥1 |
| `page_size` | 否 | 50 | 200 |

> 三个过滤参数同时给时是**交集(AND)**,不是互斥、也不是"同时发就空列表"。
>
> 生产实测(门店 00,供应商 `074` 共 2192 个商品):
> `category_id=01` → 229、`=02` → 403、`=03` → 1560,
> 229+403+1560 = 2192 ✓ —— 三个顶级分类的子树把这家供应商的商品**完整切开**了。
>
> ⚠️ 前端一次只发一个(筛选槽位是**带种类**的 `({kind,id})`),
> 所以交集语义在 App 里走不到;但**后端契约是交集**,写文档时别写成"互斥"。

#### 分类过滤的语义是**子树**

`category_id=X` = 「X **以及它的所有子孙分类**」(分类是树,见 §3.3)。
后端把树展开成 id 集合,用 cube 的 `in` 算子一次查完 ——
不是 N 次 `equals`(那会让 cube 把同一张表扫 N 遍)。

⇒ 所以 `/products?category_id=01` 的 `total` **必然等于**分类树里 `01` 节点的
`count`(实测门店 00:两者都是 1850)。这条等式是"侧栏写 2450、点进去只有 35"
不会发生的原因。

#### 供应商过滤的语义

| 参数 | 语义 |
|---|---|
| `supplier_id=X` | 精确等于某个供应商 |
| `supplier_category_id=X` | 该**供应商类别**下的全部供应商(后端展开成 id 集合,同样用 `in`) |

#### 供应商类别 = **销售方式**(sale_way),不是客户类别(cust_clsno)

思迅里有**两个**容易混的字段,选错的症状是"下拉框里大部分供应商挤在未分类里"
—— 而**接口不报错**:

| 字段 | 字典 | 生产实测 | 能不能当"供应商类别" |
|---|---|---|---|
| `sale_way`(cube `supplier.supply_way`) | `t_bd_base_code` 的 `type_no='SW'` 子集:**购销 / 代销 / 联营 / 租赁 / 自产 / 扣率代销** | 两个库的供应商**全部有值**(ysx 219 / hbposv7 300) | ✅ **就是这个** |
| `cust_clsno`(cube `supplier.cust_clsno`) | `t_bd_cust_type`:**内部领用 / 批发 / 批发1 / 批发2** | ysx 219 个里 **170 个是 NULL**(78% 没归类) | ❌ 覆盖不了大部分供应商 |

⇒ 本期口径用 **`sale_way`**。2026-10-20 之前这里误用的是 `cust_clsno`
(症状:门店 00 出来 5 组 —— 内部领用 1 / 批发 26 / 批发1 21 / 批发2 1 / 未分类 170,
其中 170 个未分类装的正是**全部 27565 个商品**的供应商)。

cube 侧新增 `sale_way` model:取数时按 `type_no='SW'` 过滤
(⚠️ `t_bd_base_code` 是通用字典,`code_id` 跨类型重复,不过滤的话
primary_key 撞车 ⇒ 某些行被静默吞掉,id→name 对应关系是错的);
过滤后为空会**报错**而不是产出一个空字典。

`supplier_category_id=__unclassified__` 是「未分类」组的哨兵值 ——
⚠️ 不能用空串:空串在 query 参数里与"没传"分不开,于是"按未分类过滤"永远表达不出来。
「未分类」= 没有销售方式,或销售方式的代码在字典里查不到(源库加了新代码、字典还没同步)。

#### ⚠️ 过滤命中 0 个 ⇒ 必须返回 **0 行**,不是"不过滤"

2026-10-20 在生产上查到的一个真 bug:按**供应商类别**过滤时,后端把类别
展开成该类别下的全部供应商 id,展开出来可能是**空集**
(实测门店 00 的"扣率代销"一个供应商都没有 —— 销售方式字典有 6 个码,
那个门店没有扣率代销的供应商)。

而空集被当成了"不过滤" ⇒ 接口返回了**全店 27565 个商品**,全程不报错。
用户点一个空类别,看到的是整个门店的商品 —— 极具迷惑性。

⇒ 契约:**任何过滤条件命中 0 个商品,`total` 必须是 0**。
实现上"要不要加过滤"用 `!= nil` 判断(不是 `len() > 0`),
空集合用一条恒假条件表达(cube 的 `in` 至少要一个值,否则 400)。

契约测试:`TestCatalogProductFilterMatchingNothingReturnsZeroRows`。

⚠️ `supplier_id` 与 `supplier_category_id` **同时给是两个过滤都发**(交集),
不是二选一。这跟**分类那边刻意相反**:

- 分类:`CategoryIDs`(子树展开)与 `CategoryID`(精确等于)**必须二选一** ——
  两个都加会变成「精确等于 X」∩「属于子树」= 精确等于 X,**子树语义被吃掉**,
  症状是"选了一个父分类却只看到它自己的商品"。
- 供应商:两个不冲突 —— 「属于该类别」∩「属于该供应商」是有意义的语义,
  X ∈ 该类别时就是 X 自己的商品,否则为空。

⇒ 实现在 `internal/cubeclient/catalog.go` 的 `productPageQuery`:
分类走 `switch`(二选一),供应商走两个独立的 `if`(都加)。
**别把这两处"对齐"成同一种写法** —— 对齐的方向恰好相反。

> 2026-10-20 修掉的一个缺陷:供应商那段曾经写成
> `switch { case SupplierIDs: … case SupplierID: … }` 后面又跟了一个
> `if SupplierID != "" { 追加同一条 equals }` —— 只传 `supplier_id` 时
> **同一个 equals 过滤被加了两次**。它在 SQL 语义上等价(`x = ? AND x = ?`),
> **结果集一模一样、没有任何症状**,而且 `filterOf` 这类"取第一个匹配"的
> 辅助函数**看不见重复** —— 只有**数条数**才抓得到。
> 见 `internal/cubeclient/catalog_query_test.go` 的两条用例。

**200**

```json
{
  "products": [
    {
      "id": "6957583900828",
      "name": "凤派XL码抽纸8包",
      "barcode": "6957583900828",
      "category_id": "030601",
      "category_name": "纸品清洁",
      "supplier_id": "019",
      "supplier_name": "某某商行",
      "unit": "提",
      "price_yuan": 9.9,
      "status": "1"
    }
  ],
  "count": 20,
  "page": 1,
  "page_size": 50,
  "total": 27565
}
```

- `barcode` = `id`(领域事实:思迅 `item_no` 就是条码,实测
  `6957583900828` → `item_no` 同值)。
- `category_name` / `supplier_name` 由后端 join 好,前端不二次请求;
  字典里查不到时给 `null`(不编造 `"分类-<id>"` 这种占位名)。
- `price_yuan`:`null` = 未维护,`0` = 真的是 0 元。**两者必须能区分**。
- `count` = 本页实际条数;`total` = 过滤后总条数。

**400**:`page < 1`、`page_size > 200` → `{"code":"bad_request"}`

### 3.2 `GET /products/search`

与 3.1 同形,多一个 `q`。

| Query | 必填 | 默认 | 说明 |
|---|---|---|---|
| `q` | 否 | `""` | 子串,命中 `product.id`(货号)**或** `product.sub_code`(自编码)**或** `product.name` |
| `category_id` | 否 | – | |
| `supplier_id` | 否 | – | |
| `page` | 否 | 1 | |
| `page_size` | 否 | 20 | 上限 200 |

- `q` 空 → 等价于 3.1(前端"空关键词 + 全量浏览"走的是 `list`)。
- `q` 非空 → **3 次 cube 查询**:`id containsCI q`、`sub_code containsCI q`、
  `name containsCI q`;按 id 去重合并,`total` 取三者之和
  (重叠时是上界,前端只用它判断"还有没有下一页",可接受)。
- 大小写:见 §2.5。

#### ⚠️ 搜**自编码**会命中**多个**商品(这是设计,不是 bug)

思迅的搜索框是「输入编码、名称或助记码」,而"编码"对它来说是**两个**东西:

| 字段 | cube 维度 | 唯一性 | 生产实测 |
|---|---|---|---|
| 货号 `item_no` | `product.id` | **唯一** | ysx 27565 行 / distinct(item_no) == 27565 |
| 自编码 `item_subno` | `product.sub_code` | **不唯一** | ysx 9 个 / hbposv7 72 个自编码对应 2 个货号 |

那 9 / 72 个**全部是「正常品 + 换购品」配对**(源库自己也只给 `item_subno`
建了**非唯一**索引 ⇒ 1:N 是设计,不是脏数据):

```
6904406383122 → 02000110 远航九江双蒸酒精品500ml       (16.50)
              → 02000111 (换购)远航九江双蒸酒精品500ml   ( 5.00)
```

⇒ 按自编码搜返回**候选列表**是正常结果,前端**不能只取第一条**;
要唯一请用货号。响应里 `id`(货号)与 `sub_code`(自编码)**都要带**。

> ⚠️ 字段名为什么是 `sub_code` 而不是 `barcode`:本仓的 `barcode` 指**另一件事**
> (商品条码 `t_bd_item_barcode`,一品多码补充表,只在本系统维护,实测只覆盖
> 0.4% 的商品)。两件事的来源表、唯一性、维护方全都不同 ⇒ 不能共用一个名字。

### 3.3 `GET /categories`

分类**树**(2026-10-20 起)。picker 侧栏第一行 / 分组选择器的主体。

| Query | 默认 | 说明 |
|---|---|---|
| `with_counts` | `false` | `true` 时每个节点带 `count` / `own_count` |

**200**

```json
{
  "categories": [
    {
      "id": "01", "name": "生鲜", "parent_id": "",
      "count": 1850, "own_count": 35,
      "children": [
        { "id": "0101", "name": "水果", "parent_id": "01",
          "count": 230, "own_count": 227, "children": [] }
      ]
    }
  ],
  "total": 186
}
```

- `categories` 是**顶级节点**组成的树(不是平铺列表);
  `total` 是**全部**节点数(实测门店 00 = 186,3 个根 + 39 个子节点,最深 3 层:
  生鲜 → 水果 → 进口水果)。
- `count` = 该节点**含所有子孙**的商品数;`own_count` = **本级直接挂**的商品数。
  用户点一个节点看到的是它的子树,所以主数字是 `count`。
- `count` 与 `/products?category_id=<节点>` 的 `total` **必须相等**(同一套子树口径)。
- `children` 是子节点(叶子是**空数组**不是 null)。
- 父节点在字典里不存在(数据不一致)→ 该节点当**顶级**处理,**不能凭空消失**;
  父链成环 → 建树时切断,成环的节点降级成顶级。
- `name` 来自 cube `category` model。**绝不能**是 `"分类-<id>"` 这类占位名。

### 3.4 `GET /suppliers/summary`

供应商**两级**结构(类别 → 供应商)。picker 侧栏第二行 / 分组选择器。无 Query。

**200**

```json
{
  "suppliers": [
    { "id": "074", "name": "超市", "count": 2192,
      "supplier_category_id": "A", "supplier_category_name": "购销" },
    { "id": "023", "name": "梧州威航（志亿）", "count": 1289,
      "supplier_category_id": "C", "supplier_category_name": "联营" }
  ],
  "supplier_categories": [
    { "id": "A", "name": "购销", "count": 20000,
      "suppliers": [ { "id": "074", "name": "超市", "count": 2192 } ] },
    { "id": "C", "name": "联营", "count": 7000,
      "suppliers": [ { "id": "023", "name": "梧州威航（志亿）", "count": 1289 } ] },
    { "id": "__unclassified__", "name": "未分类", "count": 0,
      "suppliers": [] }
  ],
  "total": 219
}
```

- `suppliers` 是**平铺**列表(向后兼容 + 简单场景直接用);
  `supplier_categories` 是**两级**结构(第一级 = 供应商类别 = 销售方式)。
- ⚠️ **没有类别的供应商必须进「未分类」组,不能丢**:
  实测两个库的 `sale_way` 都**有值**(所以「未分类」通常是空的),
  但降级路径(字典查不到 / 源库加了新代码)必须能兜住 ——
  丢掉的话侧栏会少一批供应商,用户会以为"这家店只有几个供应商"。
- 类别字典查不到**不算失败**:那只是"没有类别名"这一层信息缺失,
  供应商本身还在(降级成"全部进未分类")。
- `count` = 该供应商下的商品数(与 `/products?supplier_id=` 的 `total` **必须相等**)。
- `supplier.cust_flag` 是"是不是供应商"的第一道闸(`S`=供应商 / `C`=客户 / `A`=其他),
  `type`(`1`=供应商 / `5`=自用直供)是第二道。本期口径是**不过滤**
  (picker 是"这家店在卖什么",客户条目也在商品上挂着),但 `cust_flag` 必须带出来。

---

## 四、后续:本地 CRUD 上线后的双源判定

catalog 将来实现本地 `products` / `suppliers` / `categories` CRUD 后,
同一个端点会有两个数据源。判定顺序必须是:

1. 先看 `X-Branch-ID` 对应的门店**是否由本系统自己维护**(门店有本地
   主数据覆盖标记 / 本地表有该门店的行)。
2. 命中 → 走本地表(本地数据更新、更全、含人工维护的字段)。
3. 未命中 → 走 cube 兜底(与现在一样)。

⚠️ 判定**必须按门店**做,不能按"表非空"做。一张表里有别的门店的行,不代表
本门店有本地主数据。判定错了的表现是**静默串店**:在 A 店看到 B 店的商品,
而两边都"看起来正常"。

⇒ 这条判定要**独立写单测**,覆盖"本地表非空但不含本门店"这个关键分支。

---

## 四之二、前端已统一到本契约（2026-10-20）

Flutter 侧已把计划盘点单的「按分类 / 按供应商」也接到本契约的端点上:

| 前端位置 | 用的端点 |
|---|---|
| 商品选择控件 `showProductPicker` / `showProductPickerMulti` | `/products` `/products/search` |
| 分组选择模式 `showProductGroupPicker` | `/categories` `/suppliers/summary` + `/products?category_id=` / `?supplier_id=` |
| 计划盘点单展开计划项 | `/products?category_id=X` **服务端过滤 + 翻页取全量** |

旧的前端栈(`CatalogRepository` / `SupplierRepository` + 两个独立 picker 页)
已删除 —— 它们在生产上两条路都是坏的:

- `GET /catalog/products?limit=500` **不带 `X-Branch-ID`** → 400 `branch_required`
- `GET /catalog/suppliers`(本地空表)→ `{"count":0,"suppliers":[]}`,供应商列表恒为空
- 拉 500 条再客户端过滤 ⇒ 门店 00 有 27565 个商品、最大的分类有 3299 个,
  **静默丢掉两千多个商品**,界面上看不出异常

⇒ 前端现在拿到的分类名/供应商名/商品数**都是本契约的端点给的真实值**,
不再是客户端派生的 `分类-<id>` 占位名。

---

## 五、契约测试

`deployer/tests/contract/catalog_product_picker_test.go`

| 用例 | 断言 |
|---|---|
| `TestCatalogEndpointsAreReachable` | 4 个端点都真的注册(不是 404) |
| `TestCatalogProductList` | 形状、`barcode == id`、`count`/`total` |
| `TestCatalogProductListPagination` | 第 1/2 页不重复,`total` 一致 |
| `TestCatalogProductSearchByName` | 按商品名子串能命中 |
| `TestCatalogProductSearchByID` | 按编码能命中 |
| `TestCatalogProductSearchByIDIsCaseInsensitive` | 小写编码能搜到大写编码 |
| `TestCatalogProductListFilterByCategory` | 按分类过滤真的过滤 |
| `TestCatalogProductListFilterBySupplier` | 按供应商过滤真的过滤 |
| `TestCatalogCategories` | 有真名字、不是占位名、count ≥ 0 |
| `TestCatalogCategoryCountMatchesProductList` | **侧栏 count == 列表 total** |
| `TestCatalogSuppliersSummary` | 有真名字、count ≥ 0 |
| `TestCatalogSupplierCountMatchesProductList` | **侧栏 count == 列表 total** |
| `TestCatalogProductEnrichedNames` | **商品行上的名字 == 字典里的名字** |
| `TestCatalogPriceNullIsDistinctFromZero` | null 与 0 可区分 |
| `TestCatalogRequiresBranchHeader` | 缺 header → 400 `branch_required` |
| `TestCatalogRejectsWildcardBranch` | `*` → 400 `invalid_branch_scope` |
| `TestCatalogRejectsMultiBranch` | `00,01` → 400 `invalid_branch_scope` |
| `TestCatalogRejectsInvalidPagination` | page<1 / page_size>200 → 400 `bad_request` |
| `TestCatalogBranchIsolation` | 两个门店商品**不重合**(端到端证明门店隔离生效) |
| `TestCatalogErrorBodyIsStructured` | 错误是结构化 JSON,不是 HTML |

跑法:

```powershell
cd F:\go\src\github.com\YunBright\deployer
$env:SKIP_PRO='1'; $env:SKIP_CANARY='1'
go test -count=1 -run 'TestCatalog' ./tests/contract/
```

---

## 六、cubic 侧要改的东西(本次**不做**,记录在此)

`internal/cubeclient` 的 `CubeQuery` 目前只有 `Measures` / `Dimensions` /
`Filters` / `Limit`(`internal/cubeclient/dapr_client.go:161-166`),
**没有 `order` / `offset` / `total`**。而 cube 的分页**强制要求** `order`。

⇒ 实装前必须先扩展 `CubeQuery`:

```go
type CubeQuery struct {
    Measures   []string
    Dimensions []string
    Filters    []CubeFilter
    Limit      int
    Offset     int      // 需要 order
    Order      []CubeOrder  // [{id, order}]
    Total      bool     // 要总行数时置 true
}
```

同时 `cubeclient.Client` 接口要新增商品列表/分类/供应商摘要的方法。

---

## 七、与 flutter 仓旧文档的差异

`F:\flutter\tradewind_app\docs\product-picker-contract.md` 里的说法与实测不符:

| 旧文档 | 实测 |
|---|---|
| "`status`:cube 现状为 `active` / `inactive`" | **错**。是思迅状态码 `"0"`~`"6"`,且两个 family 语义不同(§2.4) |
| "本期 cube 是否有 `category` model 待 catalog 仓确认" | **已有**。`category` model,186 条 |
| "本地表 `SELECT ... FROM products WHERE status='active'`" | 本期**没有本地表**,数据全在 cube(§一) |
| "`q` 大小写无关子串" | ✅ **已实现**(2026-10-20)。cube 新增 `containsCI` 算子,`face`/`Face`/`fAcE` 命中**同一批**商品(见 §2.5) |
| "商品只有一套编码" | **两套**:货号 `item_no`(cube `product.id`,唯一)+ 自编码 `item_subno`(cube `product.sub_code`,**不唯一**)。搜索与列表都要覆盖两者(见 §3.2) |
| "`price_yuan`:未维护返 `null`" | 对。但要显式写 `null`(cube 是**缺 key**,§2.6) |
| "`category_name`:实时查 cube `category.name`" | 对,但**不能**在一次 cube 查询里 join 出来(§2.3) |

— END —
