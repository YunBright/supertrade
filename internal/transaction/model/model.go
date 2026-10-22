// Package model / model.go —— 流水查询域(图2 / 图3 / 图6)的模型与错误。
//
// # 这一域的两张流水表
//
//	商品销售流水 → cube `sale_detail` model(源表 t_rm_saleflow)
//	收银流水     → cube `payflow`     model(源表 t_rm_payflow)
//
// ⚠️ PLAN §1.1 的实测结论(两库真库,2026-10-22):
//
//	t_rm_saleflow  ysx 3,327,514 行 / hbposv7 1,743,882 行
//	t_rm_payflow   ysx 1,143,219 行 / hbposv7   550,213 行
//
// 而**现有** sale_detail model 配的表两库都拿不到数据
// (ysx 配的 t_mp_saleflow 是空表、hbposv7 配的 t_rm_order_saleflow 压根不存在)
// ⇒ cube 侧按 §1.1 重写这个 model 是本域能出数的前提。
//
// # ⚠️ 金额一律 JSON 字符串(PLAN §3.2)
//
//	"金额一律 JSON 字符串(后端 decimal),前端用 parseDecimal(),不要当 number"。
//
// ⇒ 本包所有金额字段都是 *string(decimal 格式化后的字符串),
// nil ⇒ null ⇒ **没维护 / 不适用**,不是 0 元。
//
// # ⚠️ discount_rate 是**派生列**,而且可能为 null
//
//	折扣度 = sale_price / source_price * 100
//
// 源库**没有**这一列。`source_price = 0` 时折扣度**必须为 null**,
// 绝不能编一个 100%(那会让"没维护原价"的商品显示成"零折")。
//
// # 本地表(没配数据源的门店走这里)
//
// 自营门店(门店 10)没有 cube 源 ⇒ 流水没有别的地方可取。
// 本域建两张表承接,写入方是 POS 的收款事件(本轮**没有**写端点)。
package model

import (
	"errors"
	"time"

	"github.com/shopspring/decimal"
)

// ---- 错误 sentinel(handler 层映 4xx / 5xx) ----
//
// ⚠️ 与 catalog / pricing **同名同义**:同一个判据的三个结局必须
// 在所有服务里给同一个解释,否则同一个门店在不同页面表现不一致。
var (
	// ErrCubeSourceUnavailable:问不到"这个门店配了哪个数据源"(链路故障)⇒ 503。
	//
	// ⚠️ 与"没配数据源"严格区分:没配是**正常状态**(自营店),
	// 问不到是**链路故障**。回落成本地 = 把一次故障显示成"这家店没有流水"。
	ErrCubeSourceUnavailable = errors.New("transaction: 数据源路由暂不可用")
	// ErrCubeSourceDisabled:该门店的数据源被管理员停用 ⇒ 503。
	ErrCubeSourceDisabled = errors.New("transaction: 该门店的数据源已停用")
	// ErrLocalStoreNotReady:本地流水表未装配 ⇒ 503(部署错误,不是业务结论)。
	ErrLocalStoreNotReady = errors.New("transaction: 本地流水库未装配")
	// ErrSaleFlowNotFound:按 id 查不到销售流水 ⇒ 404。
	ErrSaleFlowNotFound = errors.New("transaction: 销售流水不存在")
	// ErrPayFlowNotFound:按 id 查不到收款流水 ⇒ 404。
	ErrPayFlowNotFound = errors.New("transaction: 收款流水不存在")
)

// SaleFlowItem 是商品销售流水的一行(PLAN §3.2 的响应项)。
//
// ⚠️ 字段名是**冻结契约**(前端按它解析)⇒ 不许改名。
type SaleFlowItem struct {
	// ID 是流水 ID(思迅 flow_id)。
	ID string `json:"id"`
	// OrderNo 是单号(思迅 flow_no)。
	OrderNo string `json:"order_no"`
	// RandomOrderNo 是随机单号(思迅 flowno_rand)。
	RandomOrderNo string `json:"random_order_no"`
	// BranchID 是源库的分店号(思迅 branch_no)。
	//
	// ⚠️ **不是**本系统的 X-Branch-ID:后者是路由键("00"/"01"/"10"),
	// 而这里是源库的区域编码("0001")。两套编码无关,不可混用。
	BranchID string `json:"branch_id"`
	// SoldAt 是销售时刻(RFC3339)。
	SoldAt string `json:"sold_at"`
	// ProductID 是货号(思迅 item_no)。
	ProductID string `json:"product_id"`
	// SubCode 是自编码 / 助记码(思迅 item_subno)。
	SubCode string `json:"sub_code"`
	// ProductName 是商品名。
	ProductName string `json:"product_name"`
	// Spec 是规格。
	Spec string `json:"spec"`
	// BrandName 是品牌。
	//
	// ⚠️ cube 侧 product model 的 brand_name 由 PLAN §2.1 新增;
	// 本地路径恒为空(本域流水表不存品牌)。
	BrandName string `json:"brand_name"`
	// Unit 是基本单位。
	Unit string `json:"unit"`
	// SaleWay 是销售方式(思迅 sell_way)。
	SaleWay string `json:"sale_way"`
	// CounterNo 是柜台号(思迅 counter_no)。
	CounterNo string `json:"counter_no"`
	// CounterName 是柜台名。
	//
	// ⚠️ cube 侧**没有**柜台 / 员工字典 model ⇒ 本轮恒为空串。
	// 判据交给前端(有 ID 就够显示)。
	CounterName string `json:"counter_name"`
	// CashierID 是收银员编码(思迅 oper_id)。
	CashierID string `json:"cashier_id"`
	// CashierName 是收银员姓名(同 CounterName 的说明:本轮恒为空串)。
	CashierName string `json:"cashier_name"`
	// SalesmanID 是营业员编码(思迅 sale_man)。
	SalesmanID string `json:"salesman_id"`
	// SalesmanName 是营业员姓名(本轮恒为空串)。
	SalesmanName string `json:"salesman_name"`
	// Qty 是销售数量。
	Qty *string `json:"qty"`
	// SaleAmount 是销售金额(思迅 sale_money)。
	SaleAmount *string `json:"sale_amount"`
	// SourceAmount 是**原价金额**(= source_price × qty,派生)。
	//
	// ⚠️ 源库没有这一列 ⇒ 派生。派生口径写在服务层,两条路径同款。
	SourceAmount *string `json:"source_amount"`
	// DiscountRate 是**折扣度**(= sale_price / source_price × 100,派生)。
	//
	// ⚠️ source_price = 0 ⇒ **null**,绝不编 100%。
	DiscountRate *string `json:"discount_rate"`
	// DiscountAmount 是折让金额(思迅 pref_amt)。
	DiscountAmount *string `json:"discount_amount"`
	// SalePrice 是销售价(思迅 sale_price)。
	SalePrice *string `json:"sale_price"`
	// SourcePrice 是原价(思迅 source_price)。
	SourcePrice *string `json:"source_price"`
	// OnlineFlag 是断网标志(思迅 remote_flag)。
	OnlineFlag string `json:"online_flag"`
	// PayCardNo 是付款卡(券)号。
	//
	// ⚠️ 销售流水表(思迅 t_rm_saleflow)**没有**这一列 ⇒ 本轮恒为空串。
	// 收款流水那边有(见 PayFlowItem.CardNo)。
	PayCardNo string `json:"pay_card_no"`
	// LetGoodsType 是赠品类型。
	//
	// ⚠️ 思迅 t_rm_saleflow 无对应列(两库共有列里没有)⇒ 本轮恒为空串。
	LetGoodsType string `json:"let_goods_type"`
}

// SaleFlowPage 是商品销售流水分页响应(PLAN §3.2)。
type SaleFlowPage struct {
	Items []SaleFlowItem `json:"items"`
	// Count 是**本页**条数。
	Count int `json:"count"`
	// Page / PageSize 回显请求值。
	Page     int `json:"page"`
	PageSize int `json:"page_size"`
	// Total 是过滤后的总条数。
	Total int `json:"total"`
	// DataSource 是 "cube" / "local"(PLAN §3.2 之外的附加字段)。
	DataSource string `json:"data_source,omitempty"`
}

// PayFlowItem 是收银(收款)流水的一行(PLAN §3.2 的响应项)。
type PayFlowItem struct {
	// ID 是流水 ID。
	ID string `json:"id"`
	// OrderNo 是单号。
	OrderNo string `json:"order_no"`
	// RandomOrderNo 是随机单号。
	RandomOrderNo string `json:"random_order_no"`
	// BranchID 是源库分店号(思迅 branch_no)。
	BranchID string `json:"branch_id"`
	// PaidAt 是收款时刻(RFC3339)。
	PaidAt string `json:"paid_at"`
	// SaleWay 是销售方式(思迅 sell_way)。
	SaleWay string `json:"sale_way"`
	// PayWay 是付款方式代码(思迅 pay_way,实测 "SWX")。
	PayWay string `json:"pay_way"`
	// PayWayName 是付款方式名称(如 "思迅Pay-微信")。
	//
	// ⚠️ cube 侧**没有**付款方式字典 model ⇒ 本轮恒为空串。
	PayWayName string `json:"pay_way_name"`
	// SaleAmount 是单据金额(思迅 sale_amount)。
	SaleAmount *string `json:"sale_amount"`
	// ExchangeRate 是汇率(思迅 coin_rate)。
	ExchangeRate *string `json:"exchange_rate"`
	// PayAmount 是付款金额(思迅 pay_amount)。
	PayAmount *string `json:"pay_amount"`
	// RmbAmount 是**折人民币金额**(= pay_amount × exchange_rate,派生)。
	//
	// ⚠️ 源库没有这一列 ⇒ 派生。两条路径同款。
	RmbAmount *string `json:"rmb_amount"`
	// CardNo 是付款卡(券)号(思迅 card_no)。
	CardNo string `json:"card_no"`
	// VipNo 是会员卡号(思迅 vip_no)。
	VipNo string `json:"vip_no"`
	// CashierID 是收银员编码。
	CashierID string `json:"cashier_id"`
	// CashierName 是收银员姓名(本轮恒为空串,同 SaleFlowItem.CashierName)。
	CashierName string `json:"cashier_name"`
	// CounterNo 是柜台号。
	CounterNo string `json:"counter_no"`
	// SalesmanID 是营业员编码。
	SalesmanID string `json:"salesman_id"`
}

// PayFlowPage 是收银流水分页响应(PLAN §3.2)。
type PayFlowPage struct {
	Items []PayFlowItem `json:"items"`
	// Count 是**本页**条数。
	Count int `json:"count"`
	// Page / PageSize 回显请求值。
	Page     int `json:"page"`
	PageSize int `json:"page_size"`
	// Total 是过滤后的总条数。
	Total int `json:"total"`
	// DataSource 是 "cube" / "local"(PLAN §3.2 之外的附加字段)。
	DataSource string `json:"data_source,omitempty"`
}

// ---- 本地表(没配数据源的门店走这里) ----

// SaleFlow 是本域的商品销售流水表。
//
// ⚠️ **本轮没有写端点**:写入方是 POS 的收款事件(下一轮)。
// ⇒ 表在、列在、查询在,只是还没有行 —— 这是**诚实的空**。
type SaleFlow struct {
	ID            string `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID      string `gorm:"primaryKey;column:branch_id;type:varchar(64);index:idx_sale_flow_branch_sold" json:"branch_id"`
	OrderNo       string `gorm:"column:order_no;type:varchar(64);index:idx_sale_flow_branch_sold" json:"order_no"`
	RandomOrderNo string `gorm:"column:random_order_no;type:varchar(64)" json:"random_order_no"`
	SourceBranch  string `gorm:"column:source_branch_no;type:varchar(64)" json:"source_branch_no"`
	ProductID     string `gorm:"column:product_id;type:varchar(64);index:idx_sale_flow_product" json:"product_id"`
	// CategoryID 是**销售当时**的分类。
	//
	// ⚠️ 冗余字段(商品档案的分类会变,而流水要保留当时的分类),
	// 但**必须有**:cube 路径能 join product 拿 category_id,
	// 本地路径没有商品档案表可 join ⇒ 不存这一列就**没法按分类过滤**,
	// 而"过滤真的生效"是契约测试明确要断言的(PLAN §4.1)。
	CategoryID   string `gorm:"column:category_id;type:varchar(64);index:idx_sale_flow_category" json:"category_id"`
	SubCode       string `gorm:"column:sub_code;type:varchar(64)" json:"sub_code"`
	ProductName   string `gorm:"column:product_name;type:varchar(255)" json:"product_name"`
	Spec          string `gorm:"column:spec;type:varchar(255)" json:"spec"`
	BrandName     string `gorm:"column:brand_name;type:varchar(255)" json:"brand_name"`
	Unit          string `gorm:"column:unit;type:varchar(32)" json:"unit"`
	SaleWay       string `gorm:"column:sale_way;type:varchar(8)" json:"sale_way"`
	CounterNo     string `gorm:"column:counter_no;type:varchar(32)" json:"counter_no"`
	CounterName   string `gorm:"column:counter_name;type:varchar(64)" json:"counter_name"`
	CashierID     string `gorm:"column:cashier_id;type:varchar(32)" json:"cashier_id"`
	CashierName   string `gorm:"column:cashier_name;type:varchar(64)" json:"cashier_name"`
	SalesmanID    string `gorm:"column:salesman_id;type:varchar(32)" json:"salesman_id"`
	SalesmanName  string `gorm:"column:salesman_name;type:varchar(64)" json:"salesman_name"`
	Qty           decimal.Decimal `gorm:"column:qty;type:numeric(18,4);not null;default:0" json:"qty"`
	SaleAmount    decimal.Decimal `gorm:"column:sale_amount;type:numeric(18,4);not null;default:0" json:"sale_amount"`
	DiscountAmount decimal.Decimal `gorm:"column:discount_amount;type:numeric(18,4);not null;default:0" json:"discount_amount"`
	SalePrice     decimal.Decimal `gorm:"column:sale_price;type:numeric(18,4);not null;default:0" json:"sale_price"`
	SourcePrice   decimal.Decimal `gorm:"column:source_price;type:numeric(18,4);not null;default:0" json:"source_price"`
	OnlineFlag    string          `gorm:"column:online_flag;type:varchar(8)" json:"online_flag"`
	PayCardNo     string          `gorm:"column:pay_card_no;type:varchar(64)" json:"pay_card_no"`
	LetGoodsType  string          `gorm:"column:let_goods_type;type:varchar(32)" json:"let_goods_type"`
	SoldAt        time.Time       `gorm:"column:sold_at;not null;index:idx_sale_flow_branch_sold" json:"sold_at"`
	CreatedAt     time.Time       `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName 显式指定表名。
func (SaleFlow) TableName() string { return "sale_flows" }

// PayFlow 是本域的收银流水表。
type PayFlow struct {
	ID            string `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID      string `gorm:"primaryKey;column:branch_id;type:varchar(64);index:idx_pay_flow_branch_paid" json:"branch_id"`
	OrderNo       string `gorm:"column:order_no;type:varchar(64);index:idx_pay_flow_branch_paid" json:"order_no"`
	RandomOrderNo string `gorm:"column:random_order_no;type:varchar(64)" json:"random_order_no"`
	SourceBranch  string `gorm:"column:source_branch_no;type:varchar(64)" json:"source_branch_no"`
	SaleWay       string `gorm:"column:sale_way;type:varchar(8)" json:"sale_way"`
	PayWay        string `gorm:"column:pay_way;type:varchar(16)" json:"pay_way"`
	PayWayName    string `gorm:"column:pay_way_name;type:varchar(64)" json:"pay_way_name"`
	SaleAmount    decimal.Decimal `gorm:"column:sale_amount;type:numeric(18,4);not null;default:0" json:"sale_amount"`
	ExchangeRate  decimal.Decimal `gorm:"column:exchange_rate;type:numeric(18,4);not null;default:1" json:"exchange_rate"`
	PayAmount     decimal.Decimal `gorm:"column:pay_amount;type:numeric(18,4);not null;default:0" json:"pay_amount"`
	CardNo        string          `gorm:"column:card_no;type:varchar(64)" json:"card_no"`
	VipNo         string          `gorm:"column:vip_no;type:varchar(64)" json:"vip_no"`
	CashierID     string          `gorm:"column:cashier_id;type:varchar(32)" json:"cashier_id"`
	CashierName   string          `gorm:"column:cashier_name;type:varchar(64)" json:"cashier_name"`
	CounterNo     string          `gorm:"column:counter_no;type:varchar(32)" json:"counter_no"`
	SalesmanID    string          `gorm:"column:salesman_id;type:varchar(32)" json:"salesman_id"`
	PaidAt        time.Time       `gorm:"column:paid_at;not null;index:idx_pay_flow_branch_paid" json:"paid_at"`
	CreatedAt     time.Time       `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName 显式指定表名。
func (PayFlow) TableName() string { return "pay_flows" }
