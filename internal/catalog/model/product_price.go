// Package model / product_price.go —— 商品档案的**价格**与**供货信息**本地表。
//
// # 为什么这两张表现在还是空的
//
// 图1 的主表要四列价(进价 / 批发价 / 零售价 / 会员价)与一个「供货信息」页签。
// 有源门店(00 富百家 / 01 佳乐)这两块数据在**外部系统**里:
//
//	价格      → cube `branch_price` model(源表 t_pc_branch_price)
//	供货信息  → cube `supplier_item` model(源表 t_bd_supcust_item)
//
// 而**没配数据源的门店(自营店 10)没有外部系统可查** ⇒ 必须有一张本域表,
// 否则本地路径只能永远返空 —— 那就分不清"这家店没维护价"与"这条路没实现"。
//
// ⚠️ 本轮**没有写端点**:这两张表由后续的档案导入 / 价格同步写入。
// ⇒ 本地路径今天查它们会得到空结果,这是**诚实的空**
// (表在、列在、查询在,只是还没有人往里写),
// 而不是"查不到"的假象。
//
// # ⚠️ 金额一律 decimal,且"没维护"用 NULL 不用 0
//
// `cost_yuan` 等四列都是 **nullable**:NULL = 没维护,0 = 维护过且就是 0 元。
//
//	把没维护写成 0 ⇒ 前端显示 ¥0.00 ⇒ 仓管当真价贴到货架上。
//	这是本仓反复强调的「未维护 ≠ 0 元」(见 service/picker_local.go)。
//
// # 命名遵循 FIELD_SPEC
//
//	金额 <name>_yuan、数量 <name>_qty、时间 <name>_at、日期 <name>_date。
package model

import (
	"time"

	"github.com/shopspring/decimal"
)

// ProductBranchPrice 是商品的**分店价格**本地表。
//
// 一行 = 一个 (门店, 商品, 供应商, 销售方式) 的四个价。
//
// ⚠️ 复合主键含 sale_way:思迅 `t_pc_branch_price` 的唯一性是
// branch_no + item_no + supcust_no + sale_way —— 同一个商品在同一个门店
// 可以按不同销售方式(购销 / 代销 / 联营)维护不同的价。
type ProductBranchPrice struct {
	// BranchID 是门店(与本系统 X-Branch-ID 同源)。
	BranchID string `gorm:"primaryKey;column:branch_id;type:varchar(64)" json:"branch_id"`
	// ProductID 是商品货号(思迅 item_no)。
	ProductID string `gorm:"primaryKey;column:product_id;type:varchar(64);index:idx_branch_price_product" json:"product_id"`
	// SupplierID 是供应商编码(思迅 supcust_no)。
	SupplierID string `gorm:"primaryKey;column:supplier_id;type:varchar(64)" json:"supplier_id"`
	// SaleWay 是销售方式代码(思迅 sale_way:A 购销 / B 代销 / C 联营 / …)。
	SaleWay string `gorm:"primaryKey;column:sale_way;type:varchar(8)" json:"sale_way"`

	// CostYuan 是**进价**(思迅 base_price)。NULL = 没维护。
	CostYuan *decimal.Decimal `gorm:"column:cost_yuan;type:numeric(18,4)" json:"cost_yuan"`
	// WholesaleYuan 是**批发价**(思迅 price)。NULL = 没维护。
	WholesaleYuan *decimal.Decimal `gorm:"column:wholesale_yuan;type:numeric(18,4)" json:"wholesale_yuan"`
	// RetailYuan 是**零售价**(思迅 sale_price)。NULL = 没维护。
	RetailYuan *decimal.Decimal `gorm:"column:retail_yuan;type:numeric(18,4)" json:"retail_yuan"`
	// MemberPriceYuan 是**会员价**(思迅 vip_price)。NULL = 没维护。
	MemberPriceYuan *decimal.Decimal `gorm:"column:member_price_yuan;type:numeric(18,4)" json:"member_price_yuan"`

	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式指定表名。
func (ProductBranchPrice) TableName() string { return "product_branch_prices" }

// ProductSupplierPrice 是商品的**供货信息**本地表(图1 的「供货信息」页签)。
//
// 一行 = 一个 (门店, 商品, 供应商, 销售方式) 的供货约定。
type ProductSupplierPrice struct {
	BranchID   string `gorm:"primaryKey;column:branch_id;type:varchar(64)" json:"branch_id"`
	ProductID  string `gorm:"primaryKey;column:product_id;type:varchar(64);index:idx_supplier_price_product" json:"product_id"`
	SupplierID string `gorm:"primaryKey;column:supplier_id;type:varchar(64)" json:"supplier_id"`
	SaleWay    string `gorm:"primaryKey;column:sale_way;type:varchar(8)" json:"sale_way"`

	// AppointedPriceYuan 是**约定价格**(思迅 appointed_price)。
	AppointedPriceYuan *decimal.Decimal `gorm:"column:appointed_price_yuan;type:numeric(18,4)" json:"appointed_price_yuan"`
	// TopPriceYuan 是**最高价**(思迅 top_price)。
	TopPriceYuan *decimal.Decimal `gorm:"column:top_price_yuan;type:numeric(18,4)" json:"top_price_yuan"`
	// BottomPriceYuan 是**最低价**(思迅 bottom_price)。
	BottomPriceYuan *decimal.Decimal `gorm:"column:bottom_price_yuan;type:numeric(18,4)" json:"bottom_price_yuan"`
	// LastPriceYuan 是**最近价**(思迅 last_price)。
	LastPriceYuan *decimal.Decimal `gorm:"column:last_price_yuan;type:numeric(18,4)" json:"last_price_yuan"`
	// ContractDate 是**合同有效期**(思迅 contract_date)。
	//
	// ⚠️ 是**日期**不是时刻 ⇒ 零值时给 null,不给 "0001-01-01"。
	ContractDate *time.Time `gorm:"column:contract_date;type:date" json:"contract_date"`

	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式指定表名。
func (ProductSupplierPrice) TableName() string { return "product_supplier_prices" }
