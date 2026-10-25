// Package cubeclient 提供本系统 → cube sixun 的客户端接口 + 共享 DTO。
//
// 本期实现:
//   - cubeclientfake.Client(测试替身,仅 _test 引用;不在生产路径)
//   - DaprCubeClient(经 dapr/go-sdk 调 cube /v1/load)
//
// cube 响应 DTO(本包内定义):
//   - ProductDTO / StockSnapshotDTO / SupplierDTO 与 cube 各 model schema 一一对应
//   - 本系统不创建对应的 PG 表(REQUIREMENTS §7.5),只通过 Go struct 做类型契约
//
// 关键设计(REQUIREMENTS §7.5):
//   - cube product.id 即思迅 item_no(string),与本系统 StocktakeLine.ProductID 同类型
//   - cube stock 维度 (product_id, branch_id) 必须存在,否则视为跨店阻断
//   - book_qty 必须是录入该行时刻的 cube 库存快照,带快照时间 book_qty_at
package cubeclient

import (
	"errors"
	"time"

	"github.com/shopspring/decimal"
)

// ErrProductNotFound 业务侧:商品在 cube 中不存在。
var ErrProductNotFound = errors.New("cubeclient: 商品在 cube 中不存在")

// ErrStockNotFound 业务侧:某 (branch_id, product_id) 在 cube stock 中不存在
//
// 触发场景:跨店盘点(product_id 不在该 branch_id 下存在 cube stock)。
var ErrStockNotFound = errors.New("cubeclient: 该分店下的商品 stock 不存在(疑似跨店盘点)")

// ErrCubeNotFound 表示**目标 cube 明确返回"没有这条数据"**(HTTP 404)。
// 各 GetX 方法负责包成具体错误(ErrProductNotFound / ErrStockNotFound 等)。
//
// 引入原因:DaprCubeClient.LoadCubeQuery 的 404 不能直接定为"商品"或"库存"——
// cube 本身只说"找不到",具体语义由调用方决定。
//
// ⚠️ 只允许用于"数据确实不存在"。调用链故障(方法名写错、source 未注册、
// 连接被拒、超时、5xx)必须走 ErrCubeUnavailable。把故障当成 not-found,
// 会让上层把基础设施问题翻译成"本门店没有这个商品"——
// 历史上就是这么把一次 404 故障显示成了业务结论。
var ErrCubeNotFound = errors.New("cubeclient: cube 404 not found")

// ErrCubeUnavailable 表示**调用链路故障**,与数据是否存在无关。
//
// 涵盖:方法未实现(Unimplemented)、连接失败(Unavailable)、超时(DeadlineExceeded)、
// sidecar / cube 进程 5xx。任何情况下都不得翻译成"商品不存在"。
var ErrCubeUnavailable = errors.New("cubeclient: cube 调用链路不可用")

// ErrCubeBadRequest 表示查询本身不合法(schema 字段不存在、filters 写错等),
// 属于调用方 bug,应暴露为 400 而不是"查不到"。
var ErrCubeBadRequest = errors.New("cubeclient: cube 拒绝该查询")

// ErrCubeAuthRejected 表示 cube 端 middleware.http.bearer **拒了透传过去的
// caller JWT**(gRPC Unauthenticated → cube 401)。
//
// ⚠️ 与 ErrCubeUnavailable 严格区分:这是"你的登录态没了",重新登录能解决;
// 而 ErrCubeUnavailable 是"cube 挂了",重试也没用。混在一起会让一个登录态问题
// 被显示成「查询失败: HTTP 503」,排查时全部跑去查 cube 健康度。
var ErrCubeAuthRejected = errors.New("cubeclient: cube 拒绝调用者凭证")

// ErrCubeForbidden 表示 cube-router 的 per-branch 守门拒绝了这次调用
// (gRPC PermissionDenied → cube 403),最常见是缺 `cube:read` scope。
//
// ⚠️ 同样不是"cube 不可用":cube 活得好好的,只是这个账号没被授予读数据的权限。
// 对应到终端用户 = 权限不足(403),不是服务故障(503)。
var ErrCubeForbidden = errors.New("cubeclient: 调用者缺少该门店的 cube 读权限")

// ErrPromotionNotFound 表示该商品**当前没有生效的促销**。
//
// ⚠️ 这不是错误,是一个正常的业务结论(绝大多数商品都没有促销),
// 所以 GetActivePromotion 用 (nil, nil) 表示"无促销",不用 ErrPromotionNotFound。
// 后者只留给"调用方明确要区分'查不到'与'故障'"的场景。
var ErrPromotionNotFound = errors.New("cubeclient: 该商品当前没有生效促销")

// ---- cube 响应 DTO(非 GORM,只作类型契约 / 响应类型) ----

// ProductDTO 是 cube `product` model 的响应子集,作为本系统的响应类型契约。
//
// 字段含义与 cube `F:\go\src\github.com\YunBright\cube\sixun-models\product\schema.yaml` 一致;
// 本系统不创建对应的 PG 表(REQUIREMENTS §7.5),但 Go struct 字段名 / 类型 / JSON tag 全部对齐。
type ProductDTO struct {
	ID         string `json:"id"`                // cube product.id = 思迅 item_no(货号,唯一)
	Name       string `json:"name"`              // cube product.name
	CategoryID string `json:"category_id"`       // cube product.category_id
	SupplierID string `json:"supplier_id"`       // cube product.supplier_id
	Unit       string `json:"unit,omitempty"`    // 商品基本单位(见下方说明)
	Spec       string `json:"spec,omitempty"`    // 规格(扩展字段)
	Status     string `json:"status"`            // cube product.status
	Barcode    string `json:"barcode,omitempty"` // 主条码(扩展,给扫码用)
	// SubCode 是**自编码 / 助记码**(思迅 `t_bd_item_info.item_subno`)。
	//
	// ⚠️ **不是唯一的**:实测 ysx 27565 个商品里 9 个自编码对应 2 个货号、
	// hbposv7 44623 个里 72 个 —— 全部是「正常品 + 换购品」配对
	// (例:`6904406383122` → `02000110` 远航九江双蒸酒精品500ml(16.50)
	// / `02000111` (换购)远航九江双蒸酒精品500ml(5.00))。
	// 源库自己也只给 `item_subno` 建了**非唯一**索引 ⇒ 1:N 是设计,不是脏数据。
	//
	// ⇒ 按自编码查商品会命中**多个候选**;唯一键是 [ID](货号)。
	SubCode string `json:"sub_code,omitempty"`
	// PriceYuan 商品当前售价(元),来自 cube 的 product.avg_price_yuan。
	//
	// 2026-10-08 新增。此前 GetProduct 不查价,wx-h5 的「商品当前售价」永远是 "-"。
	// 用指针:nil = cube 没给(该商品未维护价格),与"售价就是 0 元"是两件事。
	PriceYuan *decimal.Decimal `json:"price_yuan,omitempty"`
}

// Unit 从哪来(2026-10-09 已打通,别再重复调研):
//
//	cube product.unit ← 思迅 t_bd_item_info.unit_no (char),ysx + hbposv7 两个 family 都映射了。
//
// 这个列**值直接就是单位名本身**(实测 '提'),不是单位编号,
// 所以不需要 join 单位表,也不需要 enum_map / transform。
//
// ⚠️ cube 的 product 表列由 mapping-product.yaml **白名单**决定 —— schema 里
// 加了 dimension 不会自动有数据,mapping 没映射的列 DuckDB 里根本不存在。
// 而且 sixun-models/product/schema.yaml 是 ysx + hbposv7 **共享**的,
// 两个 family 的 mapping 必须同时补,漏一边则查 product.unit 时该 family 直接 SQL 报错。
// 改完还必须**重新全量拉取**才会把新列写进 DuckDB。
//
// 仍可能为空的情况:老数据 / 该商品未维护单位。wx-h5 对此回退显示「件」(见前端 formatQty)。
//
// Spec 目前**仍然恒为空** —— cube product 表没有规格列(ysx 的 item_size 是 '1*10'
// 这类字符串,不适合直接当规格维度用),要接需要单独评估。

// StockSnapshotDTO 是某门店 cube 实例里某商品的库存快照。
//
// 本系统调用 cube 时,book_qty 必须用该 DTO 的 Quantity,快照时间用 UpdatedAt。
//
// 门店作用域说明:
//   - BranchID 是**本系统的 branch_id**(X-Branch-ID 的值),是路由键,
//     不是从 cube 响应里取的。门店隔离由 X-Branch-ID → cube-router →
//     branch_cube_sources → 该门店专属 cube 实例完成。
//   - cube 的 stock.branch_id 是思迅 t_im_branch_stock.branch_no(实测 "0001"),
//     与本系统 branch_id("00")是无关的两套编码,不可混用、不可用作过滤条件。
//
// 跨店阻断:stocktake 新增明细时保证"该门店的 cube 里存在该商品的库存记录",
// 不存在则 ErrStockNotFound。
type StockSnapshotDTO struct {
	ProductID   string          `json:"product_id"`    // cube stock.product_id
	BranchID    string          `json:"branch_id"`     // 本系统 branch_id(X-Branch-ID),非思迅 branch_no
	Quantity    decimal.Decimal `json:"quantity"`      // cube stock.total_quantity
	AvgCostYuan decimal.Decimal `json:"avg_cost_yuan"` // cube stock.avg_cost
	UpdatedAt   time.Time       `json:"updated_at"`    // cube stock.updated_at(快照时间)
}

// SupplierDTO 是 cube `supplier` model 的响应子集。
//
// 字段含义与 cube `F:\go\src\github.com\YunBright\cube\sixun-models\supplier\schema.yaml` 对齐;
// 本系统不创建 PG `supplier` 表(REQUIREMENTS §7.5),只作为响应类型契约。
type SupplierDTO struct {
	ID      string `json:"id"`                // cube supplier.id
	Name    string `json:"name"`              // cube supplier.name
	Type    string `json:"type"`              // "0"=供应商 / "1"=客户
	Contact string `json:"contact,omitempty"` // 联系人
	Phone   string `json:"phone,omitempty"`   // 电话
	Email   string `json:"email,omitempty"`   // 邮箱
	Address string `json:"address,omitempty"` // 地址
	Status  string `json:"status,omitempty"`  // cube supplier.status
	// CustFlag 是"是不是供应商"的第一道闸(S=供应商 / C=客户 / A=其他)。
	//
	// 2026-10-20 新增:商品选择控件的供应商摘要要把它带出来,便于以后按
	// 供应商口径收窄(本期不过滤 —— picker 回答的是"这家店在卖什么",
	// 客户条目也挂在商品上)。
	CustFlag string `json:"cust_flag,omitempty"`
	// SupplyWay 是供应商的**销售方式**代码(思迅 t_bd_supcust_info.sale_way,
	// cube 维度 supplier.supply_way),指向 sale_way model 的 id。
	//
	// 2026-10-20 实测口径:业务要的「购销 / 联营 / 代销 / 租赁 / 自产 / 扣率代销」
	// 就是这个字段(思迅 t_bd_base_code 里 type_no='SW' 的那几行)。
	// 两个生产库的供应商**全部有值**(ysx 219 / hbposv7 300)。
	//
	// ⚠️ **别用 cust_clsno(客户类别)当"供应商类别"**:实测 ysx 219 个供应商里
	// 170 个是 NULL(78% 没归类),用它做第一级的话下拉框里大部分供应商
	// 挤在"未分类"里。名字在另一张表(sale_way),cube 的 duck 模型之间不能
	// join,所以这里只带**代码**,名字由上层(supertrade catalog)Go 侧 join 补。
	SupplyWay string `json:"supply_way,omitempty"`
}

// SaleWayDTO 是 cube `sale_way` model 的响应子集。
//
// 「销售方式」字典(思迅 t_bd_base_code,type_no='SW'):
// 商品选择控件的供应商两级结构的**第一层**。
type SaleWayDTO struct {
	ID   string `json:"id"`   // cube sale_way.id(思迅 code_id:A/B/C/D/E/F)
	Name string `json:"name"` // cube sale_way.name(思迅 code_name:购销/联营/…)
}

// CategoryDTO 是 cube `category` model 的响应子集。
//
// 商品分类(思迅 t_bd_item_cls):picker 侧栏第一行的数据源。
type CategoryDTO struct {
	ID       string `json:"id"`        // cube category.id(思迅 item_clsno)
	Name     string `json:"name"`      // cube category.name(思迅 item_clsname)
	ParentID string `json:"parent_id"` // 父分类 id(NULL = 顶级)
}

// ProductPageDTO 是一页商品 + 过滤后的总条数。
//
// Total 来自 cube 响应顶层的 total(需要 CubeQuery.Total = true 才拿得到真值)。
type ProductPageDTO struct {
	Rows  []ProductDTO
	Total int
}

// CategoryListDTO 是分类字典 + 字典总条数。
type CategoryListDTO struct {
	Rows  []CategoryDTO
	Total int
}

// SupplierListDTO 是供应商字典 + 字典总条数。
type SupplierListDTO struct {
	Rows  []SupplierDTO
	Total int
}

// SaleWayListDTO 是「销售方式」字典 + 字典总条数。
type SaleWayListDTO struct {
	Rows  []SaleWayDTO
	Total int
}

// ProductQuery 是商品列表 / 搜索的查询条件(cube 侧口径)。
//
// Offset / Limit 直接落到 cube 的 offset / limit;Offset > 0 时实现方**必须**
// 同时给 order(见 CubeQuery.Offset 的注释)。
type ProductQuery struct {
	// Keyword 是子串关键词,命中 product.id 或 product.name。
	//
	// 空 = 不过滤(纯浏览模式)。
	Keyword string
	// CategoryID / SupplierID 是精确过滤(空 = 不过滤)。
	CategoryID string
	SupplierID string
	// CategoryIDs 是**一组**分类 id。
	//
	// 2026-10-20 新增:分类改成树之后,"按某个分类看商品"的语义是
	// **该分类 + 它的所有子孙分类**。上层把树展开成一组 id 传下来,
	// 这里用 cube 的 `in` 算子一次查完(而不是 N 次 equals)。
	//
	// ⚠️ CategoryIDs 优先于 CategoryID:两者都给了时以它为准。
	//
	// ⚠️ **nil 与空切片的语义不同**:
	//
	//	nil    = 不过滤(没有这个筛选维度)
	//	空切片 = "按这个集合过滤",而集合是空的 ⇒ **命中 0 行**
	//
	// 空集合当成"不过滤"会返回**全店商品**(实测门店 00 是 27565 个),
	// 而且**全程不报错**。实现方判断"要不要加过滤"必须用 `!= nil`。
	CategoryIDs []string
	// SupplierIDs 是**一组**供应商 id。
	//
	// 供应商也是两级(类别 → 供应商),所以"按某个供应商类别看商品"的语义
	// 是「该类别下的全部供应商」。上层把类别展开成一组 id 传下来。
	//
	// ⚠️ **SupplierIDs 与 SupplierID 是 AND,不是"优先"**:
	// 两个都给就取交集("属于这组 且 属于这个供应商")。
	// 真实实现是两个独立的 if,不是 switch。
	//
	// ⚠️ nil / 空切片的语义与 CategoryIDs **完全一样**
	// (nil = 不过滤;空切片 = 命中 0 行)。
	//
	// 这一条不是洁癖:实测门店 00 的"扣率代销"类别**一个供应商都没有**
	// (销售方式字典有 6 个码,那个门店没有扣率代销的供应商),
	// 展开出来是空集 —— 当成"不过滤"的话,用户点这个类别会看到
	// **全店 27565 个商品**。
	SupplierIDs []string
	Offset      int
	Limit       int
}

// ProductWithStock 是 product + 该门店 stock 的复合(用于一次性聚合接口)。
//
// 用于 `/api/v1/products/search`(REQUIREMENTS §2.1.4.1)。
type ProductWithStock struct {
	Product *ProductDTO       `json:"product"`
	Stock   *StockSnapshotDTO `json:"stock,omitempty"` // 可能 nil(该门店无 stock 数据)
}

// PromotionDTO 是某个商品**当前生效**的一条促销(cube `promotion` model)。
//
// 字段对应 cube F:\go\src\github.com\YunBright\cube\sixun-models\promotion\schema.yaml。
//
// # ⚠️ Price 是**促销价**,OrigPrice 是**原价** —— 源库列名是反的
//
// 思迅 t_pub_plan_flow 里 `sale_price` 是原价、`price` 是促销价。
// cube 的 mapping 已经把它们翻译成 canonical 的 orig_price / promo_price,
// 这里再翻成 Go 字段名。**不要**按源列名直觉去赋值 —— 写反了价签会印出
// 「原价 ¥3.50 / 促销价 ¥9.90」,而且全程不报错。
//
// # 日期用 begin_day / end_day 而不是 begin_date / end_date
//
// cube 给维度列统一包 RTRIM,真 datetime 会以 "Jul  1 2026 12:00AM" 这种
// **依赖 DB language** 的串回来。begin_day / end_day 是 schema 里
// CONVERT(...,23) 出来的固定 ISO 串,才是能安全下发的形态。
type PromotionDTO struct {
	ItemNo    string           `json:"item_no"`              // cube promotion.item_no(= 思迅 item_no = 商品条码)
	PlanNo    string           `json:"plan_no"`              // 促销单号
	PlanName  string           `json:"plan_name,omitempty"`  // 促销单名称
	Price     decimal.Decimal  `json:"price"`                // **促销价**(元)
	OrigPrice *decimal.Decimal `json:"orig_price,omitempty"` // **原价**(元);源列为 NULL 时不返
	BeginDay  string           `json:"begin_day"`            // 活动起始日 ISO yyyy-mm-dd
	EndDay    string           `json:"end_day"`              // 活动截止日 ISO yyyy-mm-dd
}

// ProductPriceQuote 是某个商品**当前售价**的一条结论(零售价 + 促销价)。
//
// # 为什么单独一个 DTO(而不是复用 ProductDTO)
//
// `ProductDTO.PriceYuan` 是"这个商品卖多少钱"(可能为 nil = 没维护价),
// 而**促销价**是**时效数据**:同一时刻有的商品有促销、有的没有。
// 预售结转要的是"**有促销价按促销价**"这条结论 ⇒ 必须把两者放在一起给。
type ProductPriceQuote struct {
	// ProductID 是商品 id(= 思迅 item_no = 商品条码)。
	ProductID string `json:"product_id"`
	// PriceYuan 是**零售价**;nil = 该商品**没维护售价**(与"售价 0 元"是两件事)。
	PriceYuan *decimal.Decimal `json:"price_yuan,omitempty"`
	// PromoPriceYuan 是**当前生效促销价**;nil = 没有生效促销(绝大多数商品的常态)。
	//
	// ⚠️ 同一商品多张促销单生效时取**最低**那条(与 GetActivePromotion 同口径)。
	PromoPriceYuan *decimal.Decimal `json:"promo_price_yuan,omitempty"`
}