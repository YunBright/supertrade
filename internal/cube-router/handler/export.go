// Package handler / export.go —— 档案导出端点。
//
// # 它是"导出方"
//
// 门店 → cube 实例的映射只有 cube-router 知道(`branch_cube_sources`),
// 所以"把某门店的档案导出成结构化数据"这件事天然归它。
//
// 路由:
//
//	GET /v1/export?scope=products&offset=0&limit=5000
//
// # 基准日(as_of)
//
//	GET /v1/export?scope=promotions&as_of=yyyy-mm-dd
//
// ⚠️ **只有时效数据(promotions)按基准日过滤** ——
//
//	促销源表(t_pub_plan_flow)实测 31371 行,覆盖 2021-09-16 ~ 2026-10-21,
//	其中对 2026-10-21 有效的只有 741 行。
//	不过滤 ⇒ 一次同步把 3 万多条历史灌进下游库,而查价时只剩 741 条能用。
//
//	as_of 缺省 = 今天(服务器本地日期);**非法格式 ⇒ 400**(不静默回落)。
//	其它 scope(products / categories / suppliers / item_barcodes / prices / stock)
//	**不带**任何时间过滤。
//
//	⚠️ stock(库存快照)也**不带**:它的 model 是 `storage: live`
//	(透传源表当前值)⇒ 导出的天然就是"导出那一刻的库存"。
//
// 守门:`cube:read` scope(per-branch),与 /v1/load 同一档。
//
// ⚠️ **为什么不用 admin 角色守门**:auth 建服务账号时是
// "不分配任何角色(least privilege)"⇒ 服务账号过不了 `RequireRole("admin")`。
// 而这个端点就是给同步服务账号用的 ⇒ 必须走 scope。
//
// # 分页由调用方负责
//
// 端点**无状态**:只按 offset/limit 返回一页 + total。
// 拉全量是调用方(sync app)的循环。
// 端点自己管分页会有两个问题:
//   - 谁来记"已经拉到哪了"?(无状态端点记不了)
//   - 一次拉 27565 行会把 sidecar 的内存吃满。
//
// # 金额零精度损失
//
// cube 响应里的 `product.avg_price_yuan` 是 JSON number。
// 常规 `json.Unmarshal` 会把它变成 float64 ⇒ 9.90 变成 9.899999999999999。
// ⇒ 这里用 `json.Decoder.UseNumber()` 解码,拿到的是**原始十进制文本**
// ("9.900000"),导出契约里金额一律是**字符串**。
package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"google.golang.org/grpc/metadata"

	dapr "github.com/dapr/go-sdk/client"

	"github.com/YunBright/supertrade/internal/archivedto"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// exportInvokeTimeout 是导出单次调 cube 的超时。
//
// ⚠️ 比 /v1/load 的 30s 长:导出页大小 5000 行,比业务查询重得多。
// 但也不能无限长 —— sidecar 那边也有默认超时。
const exportInvokeTimeout = 120 * time.Second

// ExportDefaultLimit 是导出的默认每页行数。
//
// ⚠️ 不是"一次拉完":门店 00 实测 27565 个商品,一次拉完会让 sidecar 内存爆。
const ExportDefaultLimit = 5000

// ExportMaxLimit 是导出的每页行数上限。
const ExportMaxLimit = 10000

// exportScope 描述一个可导出的资料域。
type exportScope struct {
	// Name 是 scope 名(query 参数 scope=的值)。
	Name string
	// Model 是 cube model 名。
	Model string
	// Dimensions 是要 SELECT 的维度(全限定名)。
	Dimensions []string
	// Measures 是要 SELECT 的度量(至少一个,cube 要求)。
	Measures []string
	// RowKey 是每行里"主键"那个字段的 canonical 名(用于解析)。
	RowKey string
	// Fields 把 cube 行(形如 "product.id")映射成导出契约的字段名。
	Fields map[string]string
	// Window 是该 scope 的**时效窗口**配置;nil = 该 scope 不是时效数据。
	//
	// ⚠️ **时效数据必须过滤** —— 不然一次同步会把整段历史灌进下游库。
	//   实测:促销源表(t_pub_plan_flow)31371 行、覆盖 2021-09 ~ 2026-10,
	//   其中对基准日有效的只有 741 行;不过滤 ⇒ 库里躺着 3 万多条永远用不上的行。
	//
	// ⚠️ 其它 scope(商品 / 分类 / 供应商 / 条码 / 售价)**不带**时间过滤 ——
	//   误加会让它们永远导不出行(而且不报错)。
	Window *exportWindow
	// ChangedMember 是该 scope 的"**最后变动时间**"成员(全限定名)。
	//
	// ⚠️ **它与 Window 是两回事**:
	//   - Window  = "在基准日**有效**"(促销的生效期)⇒ 用 inDateRange;
	//   - ChangedMember = "在基准日**变过**"(库存的最后变动时间)⇒ 用 gt。
	//
	// # 为什么库存需要它(2026-10-22 生产实测)
	//
	//	源库 `t_im_branch_stock` 23837 行,而**一天只变 91 行**
	//	(最近 2 小时 54 行)⇒ 全量导出 + 全量 upsert 是**两个数量级的浪费**,
	//	而且会把本地 PG 压出长事务 ⇒ 拖慢同库的 POS 开单(实测:同步时收银上传延迟)。
	//
	//	⇒ 带 since 导出时,cube 查询加 `stock.updated_at gt <since>`
	//	   ⇒ 只导"变过的那些行"(实测与源库直查**行数完全一致**)。
	//
	// ⚠️ 源列是 `t_im_branch_stock.oper_date`(= cube 的 stock.updated_at),
	//   覆盖**全部**库存变动来源(销售/退货/采购入库/盘点/调拨/损耗)
	//   —— 比"从收银流水反推商品"更准(后者会漏采购与盘点)。
	//
	// ⚠️ `oper_date IS NULL` 的 4191 行**库存全是 0** ⇒ 增量跳过它们无害。
	ChangedMember string
}

// exportWindow 描述一个 scope 的时效窗口列(全限定名)。
type exportWindow struct {
	// BeginMember 是"起始日 ≤ 基准日"那一列。
	BeginMember string
	// EndMember 是"截止日 ≥ 基准日"那一列。
	EndMember string
}

// 时效窗口的哨兵边界。
//
// ⚠️ 只是"够宽的哨兵",不代表业务边界:
//
//	begin_date <= 基准日 → inDateRange ["1900-01-01", 基准日]
//	end_date   >= 基准日 → inDateRange [基准日, "2999-12-31"]
//
// ⚠️ **必须用 inDateRange,不能用 gte / lte** ——
//
//	后者编译成 `RTRIM(col) >= ?`(字符串比较),
//	对 datetime 恒不匹配 ⇒ 返 0 行且**不报错**。
//	inDateRange 会 CAST 成 DATE 再比(真机实测:同表同数据 gte/lte 返 0 行、inDateRange 返 731 行)。
//	同一个写法见 internal/cubeclient 的 GetActivePromotion。
const (
	exportWindowLower = "1900-01-01"
	exportWindowUpper = "2999-12-31"
)

// exportScopes 是可导出的资料域全集。
//
// ⚠️ **新增 domain 时必须同时在两个地方加**:这里的表 + 下面的 buildExportRow 映射。
// 只加一半的症状是"导出成功但某个字段恒为空"—— 不报错,只是数据缺一块。
var exportScopes = map[string]exportScope{
	"products": {
		Name:  "products",
		Model: "product",
		Measures: []string{
			"product.avg_price_yuan",
			"product.total_cost_yuan",
		},
		Dimensions: []string{
			"product.id", "product.name", "product.category_id",
			"product.supplier_id", "product.unit", "product.status",
			"product.sub_code", "product.spec",
		},
		RowKey: "product.id",
		// ⚠️ **变动成员**:支持"只导 since 之后变过的行"(增量)。
		//
		//	cube 侧 product model 的 updated_at = COALESCE(modify_date, created_at)
		//	(新建商品 80% 的 modify_date 是 NULL ⇒ 用建档时间兜底,
		//	 否则新商品永远导不出去 ⇒ POS 上搜不到新上的货)。
		//
		//	⚠️ 该成员的 gt 比较是 **ISO 文本比较**(不是 CAST AS DATE):
		//	   CAST 成 DATE 会抹掉时分秒 ⇒ 当天的变动被漏掉。
		//	   详见 cube 仓 pkg/cubequery/product_updated_at_test.go。
		//
		//	⚠️ **部署顺序**:cube 那边必须先做过一次全量刷新
		//	   (DuckDB 表结构变了才有 modify_date 列)⇒ 否则这里会 column not found。
		ChangedMember: archivedto.ChangedMembers[archivedto.ScopeProducts],
		Fields: map[string]string{
			"product.id":              "id",
			"product.name":            "name",
			"product.category_id":     "category_id",
			"product.supplier_id":     "supplier_id",
			"product.unit":            "unit",
			"product.status":          "status",
			"product.sub_code":        "sub_code",
			"product.spec":            "spec",
			"product.avg_price_yuan":  "price_yuan",
			"product.total_cost_yuan": "cost_yuan",
		},
	},
	"categories": {
		Name:       "categories",
		Model:      "category",
		Measures:   []string{"category.count"},
		Dimensions: []string{"category.id", "category.name", "category.parent_id"},
		RowKey:     "category.id",
		Fields: map[string]string{
			"category.id":        "id",
			"category.name":      "name",
			"category.parent_id": "parent_id",
		},
	},
	"suppliers": {
		Name:     "suppliers",
		Model:    "supplier",
		Measures: []string{"supplier.count"},
		Dimensions: []string{
			"supplier.id", "supplier.name", "supplier.type",
			"supplier.contact", "supplier.phone", "supplier.email",
			"supplier.address", "supplier.status",
		},
		RowKey: "supplier.id",
		Fields: map[string]string{
			"supplier.id":      "id",
			"supplier.name":    "name",
			"supplier.type":    "type",
			"supplier.contact": "contact",
			"supplier.phone":   "phone",
			"supplier.email":   "email",
			"supplier.address": "address",
			"supplier.status":  "status",
		},
	},
	"item_barcodes": {
		Name:       "item_barcodes",
		Model:      "item_barcode",
		Measures:   []string{"item_barcode.count"},
		Dimensions: []string{"item_barcode.barcode", "item_barcode.product_id", "item_barcode.label"},
		RowKey:     "item_barcode.barcode",
		Fields: map[string]string{
			"item_barcode.barcode":    "barcode",
			"item_barcode.product_id": "product_id",
			"item_barcode.label":      "label",
		},
	},
	// ⚠️ 售价是**独立的一个 scope**,不从 products 里拆。
	//
	//   理由:**一个 scope 一个归属方**。
	//   products 归 catalog(商品档案),prices 归 pricing(价格)。
	//   混在一起就得让 catalog 跨服务调 pricing 去落价 ⇒ 两个真相源。
	//
	//   ⚠️ 金额在导出契约里是**字符串**(见 exportValue 的 _yuan 约定):
	//     调用方拿到 float64 就已经丢精度了(9.90 → 9.899999999999999)。
	//
	//   ⚠️ avg_price_yuan 是 measure ⇒ cube **不**给它包 RTRIM ⇒ 它是 JSON number,
	//     导出时转成原始十进制文本("12.5")。
	"prices": {
		Name:       "prices",
		Model:      "product",
		Measures:   []string{"product.avg_price_yuan", "product.total_cost_yuan"},
		Dimensions: []string{"product.id"},
		RowKey:     "product.id",
		// ⚠️ **变动成员**:与 products 用**同一个** product model
		//   ⇒ 同一个"最后变动时间"成员。
		//   ⚠️ 价与商品在**源库的同一行**里(思迅 t_bd_item_info.sale_price)
		//   ⇒ 改了价就等于改了那一行 ⇒ 商品的变动时间就是价的变动时间 ✅。
		ChangedMember: archivedto.ChangedMembers[archivedto.ScopePrices],
		Fields: map[string]string{
			"product.id":              "product_id",
			"product.avg_price_yuan":  "price_yuan",
			"product.total_cost_yuan": "cost_yuan",
		},
	},
	// ⚠️ 促销的**起止日**必须用 begin_day / end_day,
	//   **不能**用 begin_date / end_date ——
	//   后者是 datetime,cube 会给维度列包 RTRIM ⇒ 变成
	//   "Jul  1 2026 12:00AM"(依赖 SQL Server 的 SET LANGUAGE,换个实例就挂)。
	//   begin_day / end_day 是 CONVERT(varchar(10), ..., 23) 的固定 ISO 写法。
	//
	// ⚠️ promo_price / orig_price 是**维度**(不是 measure):
	//   cube 会给它们包 RTRIM ⇒ 值是字符串("10.8")。
	//   导出契约统一转成字符串金额,调用方按 decimal 解析 ✅。
	"promotions": {
		Name:     "promotions",
		Model:    "promotion",
		Measures: []string{"promotion.count"},
		Dimensions: []string{
			"promotion.item_no", "promotion.plan_no", "promotion.plan_name",
			"promotion.promo_price", "promotion.orig_price",
			"promotion.begin_day", "promotion.end_day",
		},
		RowKey: "promotion.plan_no",
		Fields: map[string]string{
			"promotion.item_no":     "product_id",
			"promotion.plan_no":     "plan_no",
			"promotion.plan_name":   "plan_name",
			"promotion.promo_price": "price_yuan",
			"promotion.orig_price":  "orig_price_yuan",
			"promotion.begin_day":   "begin_day",
			"promotion.end_day":     "end_day",
		},
		// ⚠️ 促销是**时效数据**:导出必须按基准日过滤。
		//
		//   不过滤的实测代价:源表 31371 行(2021-09-16 ~ 2026-10-21),
		//   对 2026-10-21 有效的只有 741 行 ——
		//   全量同步会把 3 万多条历史灌进 pricing.promotions,
		//   而查价时按基准日过滤后**只剩 741 条可用**。
		Window: &exportWindow{
			BeginMember: "promotion.begin_date",
			EndMember:   "promotion.end_date",
		},
	},
	// ⚠️ 库存快照:model 是 stock(`storage: live` ⇒ 透传思迅源表的**当前值**)。
	//
	//   需求:"库存同步也只同步至同步时的时间的库存" ——
	//   live storage **天然满足**:导出的就是导出那一刻的库存,
	//   源表每行 = 某分店某商品的当前库存(已含采购/销售/调整/退货的累计结果)。
	//   ⇒ **不加 as_of 过滤**(stock 没有时间维度;
	//   误加 inDateRange 会恒不匹配 ⇒ 导不出行且**不报错**)。
	//
	//   ⚠️ **不要按 stock.branch_id 过滤** ——
	//   那是思迅的 branch_no(值形如 "0001"),与本系统的 branch_id 无关;
	//   cube 的 GetStock 注释里明确写了"不得按 stock.branch_id 过滤"。
	//   ⇒ 一个 cube 实例 = 一家门店,天然只含本店数据 ⇒ **不加门店过滤**。
	//
	//   ⚠️ 度量不会被包 RTRIM(维度才会)⇒ total_quantity / avg_cost 是真的 number;
	//     avg_cost_yuan 走金额约定导出成**字符串**(见 exportValue 的 _yuan 约定)。
	"stock": {
		Name:       "stock",
		Model:      "stock",
		Measures:   []string{"stock.total_quantity", "stock.avg_cost"},
		Dimensions: []string{"stock.product_id"},
		// ⚠️ 分页要**全序**:唯一的维度就是排序键 ⇒ 天然全序,
		//   且它同时在 SELECT 里(cube 的硬要求)。
		RowKey: "stock.product_id",
		// ⚠️ **变动成员**:支持"只导 since 之后变过的行"(增量)。
		//   源列 `t_im_branch_stock.oper_date`(= cube 的 stock.updated_at)。
		//   实测:23837 行里**一天只变 91 行** ⇒ 增量省掉 99.6% 的导出与写入。
		//
		//   ⚠️ 它覆盖**全部**库存变动来源(销售/退货/采购入库/盘点/调拨/损耗)
		//   —— 比"从收银流水反推商品"更准(后者会漏采购与盘点)。
		//
		// ⚠️ **从 archivedto.ChangedMembers 取**(不写死字面量):
		//   它是"哪些 scope 能做增量"的唯一真相源(导出端 + 编排端共用)
		//   ⇒ 两边各写一份时,改了一边不会编译报错
		//   ⇒ 症状是"编排端推进了水位,而导出端没加过滤" ⇒ 漏数据且零报错。
		ChangedMember: archivedto.ChangedMembers[archivedto.ScopeStock],
		Fields: map[string]string{
			"stock.product_id":     "product_id",
			"stock.total_quantity": "qty",
			"stock.avg_cost":       "avg_cost_yuan",
		},
	},
	// ⚠️ 会员档案:model 是 `member`(思迅 t_pub_member 的会员主档)。
	//
	//   **不加 Window**:会员主档是"当前状态"而不是流水,
	//   导出出来天然就是源库此刻的会员 ⇒ 加时间过滤会**永远导不出行**(而且不报错)。
	//
	//   ⚠️ **列名是规范名,值是源值** —— `status` 带的是思迅 card_status 的原值
	//     ('0'/'1'/'2'/'3'/'5'),`limited_use_count` 带的是 use_num 的原值
	//     (含 999998/999999 哨兵),`balance_yuan` 带的是 residual_amt 的原值
	//     (**可能是脏值**,如 "@><:6642675")。
	//     ⇒ 语义归一化在**导入端**做(见 internal/member/service/import.go):
	//       导出端"顺手翻译"会把"读不懂"变成"余额 0",而余额 0 没人会去查。
	//
	//   ⚠️ 会员的**积分 / 储值 / 消费**在本域是 POS 业务的事实源
	//     ⇒ 导入端对**已存在**的会员只更新身份字段,不覆盖财务列。
	"members": {
		Name:       "members",
		Model:      "member",
		Measures:   []string{"member.count"},
		Dimensions: memberDimensions,
		RowKey:     "member.id",
		Fields:     memberFields,
	},
	// ⚠️ 会员类别:model 是 `member_type`(思迅的类别字典)。
	//
	//   与 members 分开的原因同其它域:**一个 scope 一个归属方**,
	//   而且类别字典**必须先导**(会员引用类别)。
	//
	//   ⚠️ 本域 id 与思迅 id 体系不同(本域 MT-STD-10 / MT-SAV-10 …)⇒ 导入端按**名字**对齐。
	"member_types": {
		Name:     "member_types",
		Model:    "member_type",
		Measures: []string{"member_type.count"},
		Dimensions: []string{
			"member_type.id", "member_type.name", "member_type.discount_rate",
			"member_type.allow_stored_value", "member_type.allow_points_pay",
		},
		RowKey: "member_type.id",
		Fields: map[string]string{
			"member_type.id":                 "id",
			"member_type.name":               "name",
			"member_type.discount_rate":      "discount_rate",
			"member_type.allow_stored_value": "allow_stored_value",
			"member_type.allow_points_pay":   "allow_points_pay",
		},
	},
}

// memberDimensions 是会员档案 scope 要 SELECT 的维度(全限定名)。
//
// ⚠️ **与 archivedto.ExportColumns[ScopeMembers] 逐字对应** ——
// 两边各写一份字面量时,改了一边不会编译报错,
// 症状是"导出了 4128 条会员,导入后名字/手机全是空的",**且零报错**。
var memberDimensions = []string{
	"member.id", "member.card_face_no", "member.card_type", "member.name",
	"member.gender", "member.phone", "member.mobile", "member.birthday",
	"member.id_card_no", "member.status", "member.fee_amount_yuan",
	"member.limited_use_count", "member.points_accumulated", "member.points_used",
	"member.balance_yuan", "member.consumed_amount_yuan", "member.consumed_count",
	"member.favorite", "member.remark", "member.branch_no", "member.operator_id",
	"member.operated_at", "member.valid_from", "member.valid_until", "member.open_id",
}

// memberFields 把会员行(形如 "member.id")映射成导出契约的字段名。
//
// ⚠️ 与 memberDimensions 一一对应(cube 的 order 要求 order 成员在 SELECT 里,
// 且维度元组是 GROUP BY 的键 ⇒ 按它排序必是全序,翻页不会重复/漏行)。
var memberFields = map[string]string{
	"member.id":                   "id",
	"member.card_face_no":         "card_face_no",
	"member.card_type":            "card_type",
	"member.name":                 "name",
	"member.gender":               "gender",
	"member.phone":                "phone",
	"member.mobile":               "mobile",
	"member.birthday":             "birthday",
	"member.id_card_no":           "id_card_no",
	"member.status":               "status",
	"member.fee_amount_yuan":      "fee_amount_yuan",
	"member.limited_use_count":    "limited_use_count",
	"member.points_accumulated":   "points_accumulated",
	"member.points_used":          "points_used",
	"member.balance_yuan":         "balance_yuan",
	"member.consumed_amount_yuan": "consumed_amount_yuan",
	"member.consumed_count":       "consumed_count",
	"member.favorite":             "favorite",
	"member.remark":               "remark",
	"member.branch_no":            "branch_no",
	"member.operator_id":          "operator_id",
	"member.operated_at":          "operated_at",
	"member.valid_from":           "valid_from",
	"member.valid_until":          "valid_until",
	"member.open_id":              "open_id",
}

// exportQuery 是发给 cube 的查询体。
//
// ⚠️ 字段名必须与 cube 的 `/query` 契约逐字一致
// (measures / dimensions / filters / limit / offset / order / total)。
type exportQuery struct {
	Measures   []string       `json:"measures"`
	Dimensions []string       `json:"dimensions"`
	Filters    []exportFilter `json:"filters"`
	Limit      int            `json:"limit"`
	Offset     int            `json:"offset"`
	Order      []exportOrder  `json:"order"`
	Total      bool           `json:"total"`
}

type exportFilter struct {
	Member   string `json:"member"`
	Operator string `json:"operator"`
	Values   []any  `json:"values"`
}

type exportOrder struct {
	ID    string `json:"id"`
	Order string `json:"order"`
}

// exportQueryResponse 是 cube 的响应形状。
//
// ⚠️ 行里的值用 `json.RawMessage` 保留**原始字面量**:
// 数字保持 "9.900000" 的文本形态,不走 float64。
type exportQueryResponse struct {
	Data  []map[string]json.RawMessage `json:"data"`
	Total int                          `json:"total"`
}

// handleExport GET /v1/export?scope=&offset=&limit=&as_of=yyyy-mm-dd
func (h *Handler) handleExport(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "branch_required",
			"message": "X-Branch-ID header 必填",
		})
		return
	}

	scopeName := strings.TrimSpace(c.Query("scope"))
	sc, ok := exportScopes[scopeName]
	if !ok {
		names := make([]string, 0, len(exportScopes))
		for n := range exportScopes {
			names = append(names, n)
		}
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "unsupported_export_scope",
			"message": fmt.Sprintf("scope=%q 不支持;可用:%s", scopeName, strings.Join(names, ",")),
		})
		return
	}

	// ⚠️ 基准日必须在**调 cube 之前**校验:
	//
	//	非法值当场 400 —— 不能静默回落到今天,
	//	否则"这批数据是哪一天的"不可知(促销是时效数据)。
	asOf, err := archivedto.NormalizeAsOf(c.Query("as_of"), time.Now())
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "bad_as_of",
			"message": "as_of 必须是 yyyy-mm-dd",
		})
		return
	}

	limit, _ := strconv.Atoi(c.Query("limit"))
	if limit <= 0 {
		limit = ExportDefaultLimit
	}
	if limit > ExportMaxLimit {
		limit = ExportMaxLimit
	}
	offset, _ := strconv.Atoi(c.Query("offset"))
	if offset < 0 {
		offset = 0
	}

	// ⚠️ **since = 增量导出的水位**("只导这个时间点之后变过的东西")。
	//
	//	空 ⇒ 全量(默认行为,不变)。
	//	非空 ⇒ cube 查询加 `stock.updated_at gt <since>` ⇒ 只导变过的行。
	//
	//	⚠️ 格式必须**先校验再传给 cube** —— 非法值不能静默回落成"全量"
	//	(那会让调用方以为导了增量,其实导了全量,而且没有一行报错)。
	since, err := archivedto.NormalizeSince(c.Query("since"))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "bad_since",
			"message": err.Error(),
		})
		return
	}

	cubeSourceName, _, err := h.svc.ResolveCubeSource(c.Request.Context(), branchID)
	if err != nil {
		h.mapErr(c, err)
		return
	}

	rows, total, err := h.exportFromCube(c.Request.Context(), cubeSourceName, branchID, sc, asOf, since, limit, offset)
	if err != nil {
		h.mapErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"scope":       sc.Name,
		"branch_id":   branchID,
		"cube_source": cubeSourceName,
		// as_of 回给调用方:让"导了哪一天的数据"从响应里就能看出来。
		"as_of":  asOf,
		// ⚠️ since 也要回显:增量导出时,调用方要能确认"我这次要的是哪个水位之后的数据"。
		"since": since,
		"rows":   rows,
		"count":  len(rows),
		"total":  total,
		"offset": offset,
		"limit":  limit,
	})
}

// NormalizeSince 校验并规整增量导出的水位参数。
//
// ⚠️ **实现放在 internal/archivedto**(三跳共用同一份):见那里的说明。
func NormalizeSince(raw string) (string, error) {
	return archivedto.NormalizeSince(raw)
}

// exportScopeFilters 造该 scope 的 cube 过滤条件。
//
// ⚠️ **只有时效数据才带时间过滤**(Window != nil)——
//
//	商品 / 分类 / 供应商 / 条码 / 售价都不是时效数据,
//	误加时间过滤会让它们**永远导不出行**(而且不报错)。
//
// ⚠️ 返回**非 nil** 切片:exportQuery.Filters 没有 omitempty,
//
//	返 nil 会让 cube 收到 "filters": null。
func exportScopeFilters(sc exportScope, asOf string, since string) []exportFilter {
	out := []exportFilter{}
	if sc.Window != nil {
		out = append(out,
			exportFilter{
				Member:   sc.Window.BeginMember,
				Operator: "inDateRange",
				Values:   []any{exportWindowLower, asOf},
			},
			exportFilter{
				Member:   sc.Window.EndMember,
				Operator: "inDateRange",
				Values:   []any{asOf, exportWindowUpper},
			},
		)
	}
	// ⚠️ **变动过滤与时效过滤是两回事**:
	//   时效(Window)= "在基准日**有效**" ⇒ inDateRange;
	//   变动(ChangedMember)= "在基准日**变过**" ⇒ gt。
	//
	// ⚠️ 只有**该 scope 自己声明了** ChangedMember 才加 ⇒
	//   给没声明的 scope 乱加时间过滤 ⇒ **永远导不出行**(而且不报错)。
	if sc.ChangedMember != "" && since != "" {
		out = append(out, exportFilter{
			Member:   sc.ChangedMember,
			Operator: "gt",
			Values:   []any{since},
		})
	}
	return out
}

// exportFromCube 调目标 cube 实例取一页,再把 cube 行整形成导出契约。
func (h *Handler) exportFromCube(ctx context.Context, cubeSourceName, branchID string,
	sc exportScope, asOf string, since string, limit, offset int) ([]map[string]any, int, error) {

	body, err := json.Marshal(exportQuery{
		Measures:   sc.Measures,
		Dimensions: sc.Dimensions,
		Filters:    exportScopeFilters(sc, asOf, since),
		Limit:      limit,
		Offset:     offset,
		// ⚠️ order 必须给:cube 的 offset>0 强制要求 order,
		// 且 order 的 id 必须**同时在 SELECT 里**(否则 GROUP BY 查询会报 not a grouped column)。
		//
		// ⚠️ **按全部维度排序**,不只按主键那一个。
		//
		//	分页的稳定性取决于排序键是否**全序**。
		//	只按 supplier.id 排序时,同一个 id 的多行(name 不同)之间顺序未定义
		//	⇒ 翻页可能**重复**某行或**漏掉**某行。
		//	而症状是"导出了 219 个供应商,导入后少了 3 个",
		//	且**没有任何一行报错** —— 导入端点只报"成功 219 条"。
		//
		//	维度元组本身就是 GROUP BY 的键 ⇒ 按它排序必是全序。
		Order: exportOrderFor(sc.Dimensions),
		Total: true,
	})
	if err != nil {
		return nil, 0, err
	}

	// 透传门店:cube-router → cube 必须知道走哪个门店数据。
	ctx = metadata.AppendToOutgoingContext(ctx, "x-branch-id", branchID)
	callCtx, cancel := context.WithTimeout(ctx, exportInvokeTimeout)
	defer cancel()

	resp, err := h.daprClient.InvokeMethodWithContent(callCtx, cubeSourceName, cubeQueryMethod, "POST",
		&dapr.DataContent{ContentType: "application/json", Data: body})
	if err != nil {
		h.logger.Error("cube export invoke failed",
			"target_app_id", cubeSourceName, "scope", sc.Name, "branch_id", branchID, "err", err)
		code, status, msg := classifyInvokeError(err)
		return nil, 0, &exportError{code: code, status: status, message: msg}
	}

	// ⚠️ 用 UseNumber 解码:金额要的是**原始十进制文本**,不是 float64。
	dec := json.NewDecoder(bytes.NewReader(resp))
	dec.UseNumber()
	var parsed exportQueryResponse
	if err := dec.Decode(&parsed); err != nil {
		return nil, 0, fmt.Errorf("解析 cube 响应失败: %w", err)
	}

	out := make([]map[string]any, 0, len(parsed.Data))
	for _, raw := range parsed.Data {
		out = append(out, buildExportRow(raw, sc))
	}
	return out, parsed.Total, nil
}

// exportOrderFor 把维度列表变成 ORDER BY 子句。
//
// ⚠️ 全部维度都排,而不是只排主键 —— 理由见 exportFromCube 里的注释。
// cube 的 buildOrder 要求每个 order 成员**同时在 SELECT 里**,
// 而它们都是维度 ⇒ 天然满足。
func exportOrderFor(dims []string) []exportOrder {
	out := make([]exportOrder, 0, len(dims))
	for _, d := range dims {
		out = append(out, exportOrder{ID: d, Order: "asc"})
	}
	return out
}

// buildExportRow 把一行 cube 数据整形成导出契约的一行。
//
// ⚠️ 金额类字段导出成**字符串**(原始十进制文本),
// 避免调用方拿到 float64 之后精度就丢了。
func buildExportRow(raw map[string]json.RawMessage, sc exportScope) map[string]any {
	row := make(map[string]any, len(sc.Fields))
	for cubeKey, outKey := range sc.Fields {
		rawVal, ok := raw[cubeKey]
		if !ok {
			continue
		}
		row[outKey] = exportValue(rawVal, outKey)
	}
	return row
}

// exportAmountFields 是导出成字符串(而非数字)的字段。
//
// ⚠️ 判定依据是"字段名以 _yuan 结尾"—— 这是一个**约定**,
// 新增金额字段时沿用这个后缀就会被自动纳入。
func isAmountField(name string) bool {
	return strings.HasSuffix(name, "_yuan")
}

// isNumericField 判定"这个字段的导出值应该是**数字字符串**"。
//
// ⚠️ 判定依据同样是约定：
//   - 以 `_yuan` 结尾 → 金额
//   - `qty` → 数量（库存快照）
//   - numericContractFields → 会员域的积分 / 次数 / 折扣率
//
// ⚠️ **其它字段绝不能按数字处理**（条码、名称、助记码…）——
//
//	它们是思迅的 char 列，误当数字会走 base64 解码分支，得到乱码。
func isNumericField(name string) bool {
	return isAmountField(name) || name == "qty" || numericContractFields[name]
}

// numericContractFields 是"导出值应该是**数字字符串**"的列名集合(除 `_yuan` / `qty` 之外)。
//
// ⚠️ 为什么会员的积分 / 次数列也要走数字通道:
//  1. **精度**:JSON number 到 Dart 侧会变成 double(9.90 → 9.899999999999999)。
//  2. **契约**:导入端的字段是 string ⇒ number 会让整批反序列化**直接失败**。
//  3. **兼容**:思迅 live 源的 decimal 列经 MSSQL 驱动出来是 `[]byte`
//     ⇒ `encoding/json` 序列化成 **base64** ⇒ 必须走 decodeNumeric 的兼容分支。
//
// ⚠️ 脏值(如 residual_amt 的 "@><:6642675")**原样带出**:
//
//	decodeNumeric 解析不了就返回 false ⇒ exportValue 落到字符串分支 ⇒ 保留原文
//	⇒ 导入端能识别"这个值我读不懂"并报告它(而不是静默当 0)。
var numericContractFields = map[string]bool{
	"points_accumulated": true,
	"points_used":        true,
	"limited_use_count":  true,
	"consumed_count":     true,
	"discount_rate":      true,
}

// decodeNumeric 把 cube 的原始值转成**数字字符串**。
//
// # ⚠️ 为什么需要 base64 解码
//
// **MSSQL live 源**（思迅直查：stock / settlement / sale_day）的 decimal 列，
// `go-mssqldb` 以 `[]byte` 返回，`encoding/json` 把 `[]byte` 序列化成 **base64**：
//
//	stock.total_quantity = "MC4wMDAw"      → base64 → "0.0000"
//	settlement SUM(sheet_amt) = "NDY0OTIuMDIwMA==" → "46492.0200"
//
// ⇒ 而 **DuckDB 源**（product）的同一类值是**明文 number**。
//
// ⇒ 消费方**两种都必须收**：先按字面数字试，解析不了再试 base64 解码。
// （与 `internal/cubeclient` 的 `ParseDecimal` 同款语义。）
//
// # ⚠️ 明文 number 保留**原始字面量**
//
// `"12.50"` 不变成 `"12.5"` —— 导出契约说金额是**原样字符串**。
func decodeNumeric(raw json.RawMessage) (string, bool) {
	// 明文 number：保留原始字面量。
	var num json.Number
	if err := json.Unmarshal(raw, &num); err == nil {
		return num.String(), true
	}
	// 字符串：先按字面数字试（保留原文本，如 "12.50"）。
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		s = strings.TrimSpace(s)
		if _, err := decimal.NewFromString(s); err == nil {
			return s, true
		}
		// 再试 base64（MSSQL live 源的 decimal）。
		if decoded, err := base64.StdEncoding.DecodeString(s); err == nil {
			if d, err := decimal.NewFromString(strings.TrimSpace(string(decoded))); err == nil {
				return d.String(), true
			}
		}
	}
	return "", false
}

// exportValue 把一个 cube 原始值转成导出契约的值。
func exportValue(raw json.RawMessage, outKey string) any {
	// null → nil(调用方写 null,不是 0 / "" ——
	// "没维护价"与"价是 0"必须能区分,否则 POS 会把没维护价的商品显示成 ¥0.00)。
	if string(raw) == "null" {
		return nil
	}
	if isNumericField(outKey) {
		if s, ok := decodeNumeric(raw); ok {
			return s
		}
	}
	// 其余按字符串解:思迅的 char 列普遍补空格,
	// 导出契约统一给**去空格的文本**,免得调用方再处理一遍。
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	// 非字符串(极少数情况):原样给原始字面量。
	return string(raw)
}

// exportError 带 HTTP 状态与 code 的错误(供 handler 直接映射)。
type exportError struct {
	code    string
	status  int
	message string
}

func (e *exportError) Error() string { return e.message }
