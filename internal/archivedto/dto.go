// Package archivedto —— 档案传输契约(导出页 + 导入请求/响应)。
//
// # 为什么单独一个包
//
// 导出方在 internal/cube-router/handler,导入方在 internal/catalog/service,
// 编排方在 internal/cubesync。三方要对**同一份 JSON 字段名**达成一致。
//
// ⚠️ 各写一份 struct 的代价是**静默丢数据**:
//
//	JSON 反序列化对**不认识的字段**静默忽略、对**缺失的字段**留零值。
//	⇒ "商品名字段名拼错"的后果是 27565 个商品的 name 全是空,
//	  而**没有一行报错**,导入报告显示"成功 27565 条"。
//	  POS 界面上是一列没有名字的商品。
//
// ⇒ 三方共用本包的类型,字段名只有一处定义。
// catalog 的 service 包用**类型别名**转出,老调用点零改动。
//
// # 契约方向
//
//	cube-router  GET  /v1/export      → ExportPage      → cubesync
//	cubesync     POST /archive/import → ImportRequest   → catalog
//	catalog      200                 → ImportResult    → cubesync
package archivedto

import (
	"fmt"
	"strings"
	"time"
)

// 资料域(scope)名。
//
// ⚠️ **同一批数据在两个方向上必须用同一个名字** ——
// 导出端点的 scope= 与导入端点的 scopes[] 逐字对应。
// 两边各写一份字面量时,改了一边不会编译报错,
// 症状是"导出了 27565 条,导入了 0 条"。
const (
	// ScopeCategories 是分类。
	ScopeCategories = "categories"
	// ScopeSuppliers 是供应商。
	ScopeSuppliers = "suppliers"
	// ScopeProducts 是商品。
	ScopeProducts = "products"
	// ScopeItemBarcodes 是商品条码。
	ScopeItemBarcodes = "item_barcodes"
	// ScopePrices 是售价(归 pricing 服务)。
	//
	// ⚠️ 与 ScopeProducts **分开**:售价归 pricing,商品档案归 catalog。
	//   放一起就得让 catalog 跨服务调 pricing 去落价 —— 两个真相源。
	ScopePrices = "prices"
	// ScopePromotions 是促销(归 pricing 服务)。
	ScopePromotions = "promotions"
	// ScopeStock 是库存快照(归 inventory 服务)。
	//
	// ⚠️ **不是时效数据**:cube 的 stock model 是 `storage: live`
	//   (透传思迅 t_im_branch_stock 的**当前值**),
	//   导出来的天然就是"同步时刻的库存快照" ⇒ **不需要任何时间过滤**。
	//
	// ⚠️ 与 ScopeProducts 分开:商品档案归 catalog,库存归 inventory。
	ScopeStock = "stock"
	// ScopeMemberTypes 是会员类别(归 member 服务)。
	//
	// ⚠️ **必须排在 ScopeMembers 之前** —— 会员引用类别
	//   (members.card_type_id → member_types.id),
	//   顺序反了 ⇒ 会员落库时引用一个还不存在的类别。
	//
	// ⚠️ 与 ScopeMembers 分开:会员类别是**门店自己维护的字典**
	//   (会员卡 / 储值卡 / VIP储值卡 …),与会员主档不是一回事。
	ScopeMemberTypes = "member_types"
	// ScopeMembers 是会员档案(归 member 服务)。
	//
	// ⚠️ **不是时效数据**:会员主档是"当前状态"(不是流水),
	//   导出来的就是源库此刻的会员 ⇒ **不需要任何时间过滤**。
	//
	// ⚠️ 会员的**积分 / 储值 / 消费**在本域是 POS 业务的事实源
	//   (sale.paid 订阅给会员加积分)⇒ 导入时**不得覆盖已存在会员的财务列**。
	ScopeMembers = "members"
)

// AllScopes 是全部资料域,**按依赖顺序**排列。
//
// ⚠️ 顺序不是随意的:分类与供应商是商品的引用对象,条码引用商品。
// ⇒ 先引用方后被引用方,这样"引用不存在"的问题在最后一步才暴露。
// 导出方与导入方都必须按这个顺序处理。
var AllScopes = []string{
	ScopeCategories,
	ScopeSuppliers,
	ScopeProducts,
	ScopeItemBarcodes,
	ScopePrices,
	ScopePromotions,
	ScopeStock,
	// ⚠️ member_types 在 members **之前**:会员引用类别,
	//   反过来就是"引用了一个还不存在的对象"。
	ScopeMemberTypes,
	ScopeMembers,
}

// ---- 基准日(as_of)----

// AsOfLayout 是基准日(as_of)的日期格式。
//
// ⚠️ **促销是时效数据** —— 基准日决定"哪些促销在生效"。
//
//	导出端点用它过滤(cube 侧 inDateRange),导入端点用它筛选与清理。
//	商品 / 分类 / 供应商 / 条码 / 售价**不是**时效数据 ⇒ 不带基准日。
const AsOfLayout = "2006-01-02"

// NormalizeAsOf 规整 as_of 参数:空 ⇒ 今天(now 的本地日期);非法 ⇒ 报错。
//
// ⚠️ **非法值必须报错,不能静默回落到今天** ——
//
//	静默回落会让"这批数据是哪一天的"变得不可知。
//	而促销是时效数据:基准日错了 ⇒ 导出的促销全错(要么漏,要么多),
//	症状是"导入报告成功 741 条,但 POS 上的特价对不上",**没有任何一行报错**。
//
// ⚠️ 导出侧与导入侧**共用这一个实现** ——
//
//	两边各写一份格式校验时,改了一边不会编译报错,
//	症状是"导出按 A 规则过滤、导入按 B 规则保留",数据静默对不上。
func NormalizeAsOf(raw string, now time.Time) (string, error) {
	t := strings.TrimSpace(raw)
	if t == "" {
		return now.Format(AsOfLayout), nil
	}
	parsed, err := time.ParseInLocation(AsOfLayout, t, time.Local)
	if err != nil {
		return "", fmt.Errorf("%q 不是 yyyy-mm-dd", raw)
	}
	return parsed.Format(AsOfLayout), nil
}

// ---- 导出方向 ----

// ExportColumns 是每个 scope 的**导出列**(契约)。
//
// ⚠️ 它同时是"导出端点吐什么列"与"导入端点吃什么列"的**唯一真相源**。
//
// 导出方(cube-router)按它把 cube 成员名映射成契约列名;
// 编排方(cubesync)按它把契约列名读进行结构体。
// 两边各写一份字面量时,改了一边不会编译报错 ——
// 症状是"导出了 27565 条,导入后商品名字全是空的",**没有一行报错**。
var ExportColumns = map[string][]string{
	ScopeCategories: {"id", "name", "parent_id"},
	ScopeSuppliers: {
		"id", "name", "type", "contact", "phone", "email", "address", "status",
	},
	ScopeProducts: {
		"id", "name", "category_id", "supplier_id", "unit", "status",
		"sub_code", "spec", "price_yuan", "cost_yuan",
	},
	ScopeItemBarcodes: {"barcode", "product_id", "label"},
	// ⚠️ 售价与促销的列**不含**商品身份(名字/单位/条码)——
	//   那些归 catalog.products。本 scope 只带"价"本身。
	ScopePrices: {"product_id", "price_yuan", "cost_yuan"},
	// ⚠️ promotions 是**时效数据**:导出端点按基准日(as_of)过滤,
	//   只导"对基准日有效"的行(cube 侧 inDateRange,见 cube-router 的
	//   exportScope.Window);导入端点按**同一个**基准日筛选与清理
	//   (见 ImportRequest.AsOf)。
	//
	//   不过滤的代价(实测):思迅 t_pub_plan_flow 有 31371 行,
	//   覆盖 2021-09-16 ~ 2026-10-21,其中对 2026-10-21 有效的只有 741 行。
	//   一次全量同步会把 3 万多条历史灌进 pricing.promotions,
	//   而查价时按基准日过滤后**只剩 741 条可用**。
	ScopePromotions: {
		"product_id", "plan_no", "plan_name",
		"price_yuan", "orig_price_yuan", "begin_day", "end_day",
	},
	// ⚠️ 库存快照:只有**数量 + 平均成本**两个度量 + 商品号。
	//
	//   没有时间列(不是时效数据,见 ScopeStock 的说明),
	//   也没有门店列 —— 一个 cube 实例 = 一家门店,天然只含本店数据。
	ScopeStock: {"product_id", "qty", "avg_cost_yuan"},
	// ⚠️ 会员档案:**列名是规范名,值是源值(未归一化)**。
	//
	//	`status` 带的是思迅 card_status 的原值('0'/'1'/'2'/'3'/'5'),
	//	`limited_use_count` 带的是 use_num 的原值(含 999998/999999 哨兵),
	//	`balance_yuan` 带的是 residual_amt 的原值(**可能是脏值**)。
	//	⇒ **语义归一化在导入端做**(见 internal/member/service/import.go),
	//	  导出端擅自"翻译"会把"解析失败"变成"静默当 0"。
	ScopeMembers: {
		"id", "card_face_no", "card_type", "name", "gender", "phone", "mobile",
		"birthday", "id_card_no", "status", "fee_amount_yuan", "limited_use_count",
		"points_accumulated", "points_used", "balance_yuan", "consumed_amount_yuan",
		"consumed_count", "favorite", "remark", "branch_no", "operator_id",
		"operated_at", "valid_from", "valid_until", "open_id",
	},
	// ⚠️ 会员类别:**只带字典**,不带 id 与本域 id 的对应关系
	//   (两边 id 体系不同:思迅是 0/1/2,本域是 MT-STD-10 / MT-SAV-10 …)。
	//   ⇒ 导入端按**类别名**对齐。
	ScopeMemberTypes: {
		"id", "name", "discount_rate", "allow_stored_value", "allow_points_pay",
	},
}

// ExportPage 是导出端点返回的一页。
//
// ⚠️ Rows 用 `map[string]any` 而不是定长 struct:
// 导出端的行字段**随 scope 变化**(分类没有 category_id,商品有),
// 用 map 才不用为每个 scope 各写一个行类型。
// 代价是字段名靠约定 —— 编排方(cubesync)负责按约定整形。
type ExportPage struct {
	// Scope 是本次导出的资料域。
	Scope string `json:"scope"`
	// BranchID 是导出方解析出的源门店。
	BranchID string `json:"branch_id"`
	// CubeSource 是该门店路由到的 cube 实例名(排障用)。
	CubeSource string `json:"cube_source"`
	// Rows 是本页数据行。
	Rows []map[string]any `json:"rows"`
	// Count 是本页行数。
	Count int `json:"count"`
	// Total 是**不受 limit/offset 影响**的总行数。
	Total int `json:"total"`
	// Offset / Limit 是本次分页位置。
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
}

// ---- 导入方向 ----

// ImportCategoryRow 是分类行。
type ImportCategoryRow struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	ParentID string `json:"parent_id"`
}

// ImportProductRow 是商品行。
//
// ⚠️ PriceYuan / CostYuan **不在 catalog.products 里落库**(价格归 pricing 服务,
// 避免两个真相源)。保留字段是为了让"导出带了但没接住"这件事**能被报告出来**,
// 而不是静默丢。
type ImportProductRow struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	CategoryID string `json:"category_id"`
	SupplierID string `json:"supplier_id"`
	Unit       string `json:"unit"`
	Status     string `json:"status"`
	SubCode    string `json:"sub_code"`
	Spec       string `json:"spec"`
	PriceYuan  string `json:"price_yuan"`
	CostYuan   string `json:"cost_yuan"`
}

// ImportSupplierRow 是供应商行。
type ImportSupplierRow struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Contact string `json:"contact"`
	Phone   string `json:"phone"`
	Email   string `json:"email"`
	Address string `json:"address"`
	Status  string `json:"status"`
}

// ImportBarcodeRow 是商品条码行。
type ImportBarcodeRow struct {
	Barcode   string `json:"barcode"`
	ProductID string `json:"product_id"`
	Label     string `json:"label"`
}

// ImportData 是各 scope 的行集合。
//
// ⚠️ **一份请求体服务所有下游**:每个下游只读自己那几个 key,
//
//	其余字段对它无意义(会被忽略,不会报错)。
//
//	⇒ 编排方(cubesync)可以用**同一个结构**打给不同服务,不用为每个下游写一套 DTO。
//	而"哪个 scope 归谁"由**路由表**决定(见 internal/cubesync 的 scopeOwners)。
type ImportData struct {
	Categories   []ImportCategoryRow `json:"categories"`
	Products     []ImportProductRow  `json:"products"`
	Suppliers    []ImportSupplierRow `json:"suppliers"`
	ItemBarcodes []ImportBarcodeRow  `json:"item_barcodes"`
	// Prices 是售价行(归 **pricing** 服务)。
	//
	// ⚠️ 售价**不归 catalog** —— catalog.products 不存价(避免两个真相源)。
	Prices []ImportPriceRow `json:"prices"`
	// Promotions 是促销行(归 **pricing** 服务)。
	Promotions []ImportPromotionRow `json:"promotions"`
	// Stocks 是库存快照行(归 **inventory** 服务)。
	//
	// ⚠️ 库存**不归 catalog** —— catalog.products 不存库存(避免两个真相源)。
	// ⚠️ 数量与金额一律**字符串**:JSON number 到 Dart 侧会变成 double,精度当场丢。
	Stocks []ImportStockRow `json:"stocks"`
	// MemberTypes 是会员类别行(归 **member** 服务)。
	//
	// ⚠️ 会员类别是**门店自己维护的字典** ⇒ 导入端**不新建类别**,
	//   只按类别名对齐已有类别(见 internal/member/service/import.go)。
	MemberTypes []ImportMemberTypeRow `json:"member_types"`
	// Members 是会员档案行(归 **member** 服务)。
	Members []ImportMemberRow `json:"members"`
}

// ImportMemberTypeRow 是会员类别行(思迅的类别字典)。
//
// ⚠️ 折扣率一律**字符串**:JSON number 到 Dart 侧会变成 double,精度当场丢。
//
// ⚠️ allow_stored_value / allow_points_pay 也用**字符串**("1"/"0"/"true"/"false")
//
//	—— 源库那两个列是 0/1 的 char,不是布尔;用 bool 接收时
//	源值是字符串就会反序列化失败(整批导入报错)。
type ImportMemberTypeRow struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// DiscountRate 是会员折扣(0.95 = 95 折;字符串)。
	DiscountRate string `json:"discount_rate"`
	// AllowStoredValue 是"是否储值卡"(字符串布尔)。
	AllowStoredValue string `json:"allow_stored_value"`
	// AllowPointsPay 是"是否可用积分抵扣"(字符串布尔)。
	AllowPointsPay string `json:"allow_points_pay"`
}

// ImportMemberRow 是会员档案行。
//
// # ⚠️ **列名是规范名,值是源值(未归一化)**
//
//	`status`           = 思迅 card_status 的原值('0'/'1'/'2'/'3'/'5')
//	`limited_use_count`= 思迅 use_num 的原值(含 999998/999999 "不限次"哨兵)
//	`balance_yuan`     = 思迅 residual_amt 的原值(⚠️ **可能是脏值**,如 "@><:6642675")
//	`points_accumulated`/`points_used` = 思迅 acc_num / dec_num 的原值
//	`valid_from`/`valid_until`         = 思迅 vip_start_date / vip_end_date
//
// ⇒ **语义归一化一律在导入端做**(见 internal/member/service/import.go):
// 导出端"顺手翻译"会把"这个值我读不懂"变成"余额 0",而余额 0 没人会去查。
//
// ⚠️ 金额 / 积分 / 数量一律**字符串**:JSON number 到 Dart 侧会变成 double,精度当场丢。
type ImportMemberRow struct {
	// ID 是会员号(思迅 card_id;导入端去反引号与前导零)。
	ID string `json:"id"`
	// CardFaceNo 是卡面卡号。
	CardFaceNo string `json:"card_face_no"`
	// CardType 是思迅的类别码('0'=会员卡 / '1'=储值卡 / '2'=VIP储值卡)。
	CardType string `json:"card_type"`
	// Name 是姓名。
	Name string `json:"name"`
	// Gender 是性别。
	Gender string `json:"gender"`
	// Phone / Mobile 是电话 / 手机。
	Phone  string `json:"phone"`
	Mobile string `json:"mobile"`
	// Birthday 是生日(源值;导入端尽力解析)。
	Birthday string `json:"birthday"`
	// IDCardNo 是身份证。
	IDCardNo string `json:"id_card_no"`
	// Status 是**思迅 card_status 的原值**(不是本域的 normal/frozen/…)。
	Status string `json:"status"`
	// FeeAmountYuan 是会员费(字符串)。
	FeeAmountYuan string `json:"fee_amount_yuan"`
	// LimitedUseCount 是限用次数(字符串;999998/999999 = 不限次 ⇒ 导入端转 0)。
	LimitedUseCount string `json:"limited_use_count"`
	// PointsAccumulated 是累计获得积分(字符串)。
	PointsAccumulated string `json:"points_accumulated"`
	// PointsUsed 是累计扣减积分(字符串)。
	PointsUsed string `json:"points_used"`
	// BalanceYuan 是储值余额(字符串;⚠️ 源值可能是脏值 ⇒ 解析失败要**报告**,不能静默当 0)。
	BalanceYuan string `json:"balance_yuan"`
	// ConsumedAmountYuan 是累计消费金额(字符串)。
	ConsumedAmountYuan string `json:"consumed_amount_yuan"`
	// ConsumedCount 是消费次数(字符串)。
	ConsumedCount string `json:"consumed_count"`
	// Favorite 是爱好。
	Favorite string `json:"favorite"`
	// Remark 是备注。
	Remark string `json:"remark"`
	// BranchNo 是**源库**的门店号(思迅里全是 '00')。
	//
	// ⚠️ **只作审计记录**:目标门店只认 X-Branch-ID。
	//   源库里 branch_no 与本系统的 branch_id 毫无关系(那是思迅的门店编号)。
	BranchNo string `json:"branch_no"`
	// OperatorID 是源库的操作人工号(审计用)。
	OperatorID string `json:"operator_id"`
	// OperatedAt 是源库的操作时刻(审计用;解析得出就当 updated_at)。
	OperatedAt string `json:"operated_at"`
	// ValidFrom / ValidUntil 是有效期(源值;导入端解析成 RFC3339)。
	ValidFrom  string `json:"valid_from"`
	ValidUntil string `json:"valid_until"`
	// OpenID 是微信 / 支付宝 openid。
	OpenID string `json:"open_id"`
}

// ImportStockRow 是库存快照行。
//
// ⚠️ **Qty 与 AvgCostYuan 都是字符串** —— 与其它金额字段同一约定:
// JSON number 到了 Dart 侧会被 jsonDecode 变成 double,精度当场丢掉。
//
// ⚠️ 一行 = 某分店某商品的**当前**库存快照(源表 t_im_branch_stock 的当前值),
//
//	**不带时间戳**:它表达的是"导出那一刻的库存"。
type ImportStockRow struct {
	ProductID string `json:"product_id"`
	// Qty 是当前库存数量(字符串)。
	Qty string `json:"qty"`
	// AvgCostYuan 是平均成本,单位元(字符串)。
	AvgCostYuan string `json:"avg_cost_yuan"`
}

// ImportRequest 是导入请求。
type ImportRequest struct {
	// SourceBranchID 是**来源门店**(仅用于审计与报告;数据本身由 cube 决定)。
	SourceBranchID string `json:"source_branch_id"`
	// Mode 是 incremental / overwrite。
	Mode string `json:"mode"`
	// Scopes 限定本次导入哪些 scope;空 = 全部(以 data 里非空的为准)。
	Scopes []string `json:"scopes"`
	// AsOf 是**基准日**(yyyy-mm-dd):本次导入按"这一天"判定促销是否有效。
	//
	// ⚠️ 空 = 今天(服务端本地日期)。
	// ⚠️ 非法格式必须 400(不静默回落)—— 见 NormalizeAsOf。
	//
	// ⚠️ **促销是时效数据**:导入端点只 upsert"对基准日有效"的行,
	//   无效的行计入 skipped 并给一条 warning。
	//   ⇒ **过滤在服务端做** —— 不能指望每个调用方都记得传 as_of,
	//   否则库里会留一堆永远用不上的历史行。
	AsOf string `json:"as_of,omitempty"`
	// Data 是各 scope 的行。
	Data ImportData `json:"data"`
}

// ImportScopeRow 是单个 scope 的导入结果。
type ImportScopeRow struct {
	Scope    string `json:"scope"`
	Received int    `json:"received"`
	Inserted int    `json:"inserted"`
	Updated  int    `json:"updated"`
	Skipped  int    `json:"skipped"`
	Deleted  int    `json:"deleted"`
}

// ImportErrorItem 是单行的失败明细。
type ImportErrorItem struct {
	Scope string `json:"scope"`
	ID    string `json:"id"`
	Error string `json:"error"`
}

// ImportResult 是导入结果。
type ImportResult struct {
	SourceBranchID string `json:"source_branch_id"`
	TargetBranchID string `json:"target_branch_id"`
	Mode           string `json:"mode"`
	// AsOf 是本次导入实际用的**基准日**(yyyy-mm-dd)。
	//
	// ⚠️ 回给调用方 ⇒ "这批数据是哪一天的"从响应里就能看出来,
	// 不用去翻请求日志。
	AsOf    string           `json:"as_of,omitempty"`
	ByScope []ImportScopeRow `json:"by_scope"`
	// Warnings 是**不阻断但必须让人知道**的事
	// (例如价格没落库、跳过了 N 行对基准日无效的促销)。
	Warnings []string `json:"warnings,omitempty"`
	// Errors 是逐行失败明细(可空)。
	Errors []ImportErrorItem `json:"errors,omitempty"`
}

// Total 汇总三态计数。
func (r *ImportResult) Total() (inserted, updated, skipped int) {
	if r == nil {
		return 0, 0, 0
	}
	for _, s := range r.ByScope {
		inserted += s.Inserted
		updated += s.Updated
		skipped += s.Skipped
	}
	return
}

// ---- 价格域(pricing 服务拥有) ----
//
// ⚠️ **售价/促销不归 catalog** —— catalog.products 不存价(避免两个真相源)。
// ⇒ 价格的导入有**自己的一份契约**,导入目标是 pricing 服务。

// ImportPriceRow 是售价行。
//
// ⚠️ 金额一律**字符串**:JSON number 到了 Dart 侧会被 jsonDecode 变成 double,
// 精度当场丢掉(9.90 → 9.899999999999999)。
//
// ⚠️ PriceYuan **空 = 该商品没维护价**(不是"售价 0 元")——
// 导入端据此**不建行**,POS 才印「未维护售价」而不是印 ¥0.00。
type ImportPriceRow struct {
	ProductID string `json:"product_id"`
	// PriceYuan 是零售价(字符串)。空 = 没维护价。
	PriceYuan string `json:"price_yuan"`
	// MemberPriceYuan 是会员价(字符串)。空 = 没有会员价。
	MemberPriceYuan string `json:"member_price_yuan,omitempty"`
	// CostYuan 是成本价(字符串)。空 = 不知道成本。
	CostYuan string `json:"cost_yuan,omitempty"`
	// EffectiveAt 是生效时刻(ISO);空 = 导入即生效。
	EffectiveAt string `json:"effective_at,omitempty"`
	// ExpiredAt 是失效时刻(ISO);空 = 长期有效。
	ExpiredAt string `json:"expired_at,omitempty"`
}

// ImportPromotionRow 是促销行。
type ImportPromotionRow struct {
	// PlanNo 是促销单号(思迅 plan_no),与 (branch_id, product_id) 一起构成 PK。
	PlanNo string `json:"plan_no"`
	// ProductID 是商品货号。
	ProductID string `json:"product_id"`
	// PlanName 是促销单名称(印在价签上用于人工追溯)。
	PlanName string `json:"plan_name,omitempty"`
	// PriceYuan 是**促销价**(字符串)。
	PriceYuan string `json:"price_yuan"`
	// OrigPriceYuan 是**原价**(字符串)。空 = 没有原价。
	//
	// ⚠️ 思迅源列 `sale_price` 是原价、`price` 是促销价 —— 语义相反,
	// 拿反了会印出「原价 ¥3.50 / 促销价 ¥9.90」且全程不报错。
	OrigPriceYuan string `json:"orig_price_yuan,omitempty"`
	// BeginDay / EndDay 是活动起止日(ISO yyyy-mm-dd)。
	BeginDay string `json:"begin_day"`
	EndDay   string `json:"end_day"`
}

// PriceImportRequest 是价格域的导入请求(pricing 服务的 POST /archive/import)。
//
// ⚠️ 它是 ImportRequest 的**别名**,不是另一套结构。
//
//	一份请求体服务所有下游:编排方(cubesync)用同一个结构打给 catalog 与 pricing,
//	每个下游只读自己那几个 key(Data.Prices / Data.Promotions),
//	其余字段对它无意义(被忽略,不会报错)。
//
//	⇒ 为什么不用两套 DTO:两套就要两套字段名,而"改了一边忘了另一边"
//	  的症状是**静默丢数据**(JSON 对不认识的字段静默忽略)。
type PriceImportRequest = ImportRequest

// PriceImportResult 是价格域的导入结果(ImportResult 的别名)。
//
// ⚠️ pricing 返的是**同一个** ImportResult(by_scope 里两行:prices / promotions),
//
//	这样编排方可以用**一套**代码解析所有下游的导入响应。
type PriceImportResult = ImportResult
