// Package model —— pos 域的本地表(销售单头 / 明细 / 收付)。
//
// # 域边界
//
// pos 拥有**收银事实**:销售单头、明细、收付流水。
//
// 门店 / 收银员 / 班次归 master-data;商品 / 价格归 catalog / pricing;
// 会员归 member;库存归 inventory。
//
// ⇒ 本包**不复制**别人的档案字段:明细里的 product_name / barcode / unit 是
// **成交时的快照**(下单那一刻商品可能改名、换条码),不是外键实时 join。
//
// # 金额
//
// ⚠️ **金额一律 decimal(Shopspring),DB 列 numeric(18,4),绝不用 float**。
//
// decimal.Decimal 的 MarshalJSON 默认输出**带引号的字符串**(`"22.2"`)⇒
// JSON 里金额天然是字符串,Dart 侧不会变成 double。
//
// # 软删
//
// 用 `gorm.DeletedAt`(不是 *time.Time)⇒ 删掉的行查不到。
package model

import (
	"errors"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// SalesOrder 是销售单头。
//
// ⚠️ **ID = SO<yyyymmdd><6 位当天序号>**,由 service 层生成(见 ids.go)。
type SalesOrder struct {
	// ID 是销售单号(SO20261021000001)。
	ID string `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	// BranchID 是门店。
	BranchID string `gorm:"column:branch_id;type:varchar(64);not null;index:idx_sales_order_branch_sold" json:"branch_id"`
	// MemberID 是会员号(可空)。
	//
	// ⚠️ 空 ⇒ **不计积分**(下游 member 按 member_id 是否为空决定加不加)。
	MemberID string `gorm:"column:member_id;type:varchar(64);index:idx_sales_order_member" json:"member_id"`
	// ShiftID 是班次 id(可空;班次本体在 master-data)。
	ShiftID string `gorm:"column:shift_id;type:varchar(64)" json:"shift_id"`
	// OperatorID 是收银员(auth user_id)。
	OperatorID string `gorm:"column:operator_id;type:varchar(64);not null" json:"operator_id"`
	// AuthorizedBy 是**越权授权人**(图4/图5 审计)。
	//
	// ⚠️ 收银员没有 pos:price-override 时,需由有权限的人授权,
	//   授权人记在这里(业务表内留痕,不依赖 auth 的 audit_logs)。
	AuthorizedBy string `gorm:"column:authorized_by;type:varchar(64)" json:"authorized_by"`
	// TotalAmountYuan 是应收合计(必须等于 Σ 明细 amount_yuan)。
	TotalAmountYuan decimal.Decimal `gorm:"column:total_amount_yuan;type:numeric(18,4);not null;default:0" json:"total_amount_yuan"`
	// DiscountAmountYuan 是优惠合计(展示 / 审计用,不参与 total 的推导)。
	DiscountAmountYuan decimal.Decimal `gorm:"column:discount_amount_yuan;type:numeric(18,4);not null;default:0" json:"discount_amount_yuan"`
	// PaidAmountYuan 是实收。
	PaidAmountYuan decimal.Decimal `gorm:"column:paid_amount_yuan;type:numeric(18,4);not null;default:0" json:"paid_amount_yuan"`
	// ChangeAmountYuan 是找零(= paid - total)。
	ChangeAmountYuan decimal.Decimal `gorm:"column:change_amount_yuan;type:numeric(18,4);not null;default:0" json:"change_amount_yuan"`
	// Status 是 draft / paid / returned / void。
	Status string `gorm:"column:status;type:varchar(16);not null;default:'paid';index:idx_sales_order_branch_sold" json:"status"`
	// SoldAt 是成交时刻(业务日按 +08:00 切分)。
	SoldAt time.Time `gorm:"column:sold_at;not null;index:idx_sales_order_branch_sold" json:"sold_at"`
	// OpID 是**幂等键**(uuid,唯一索引)。
	//
	// ⚠️ 同 op_id 重复提交 ⇒ 返回**首次那张单**,不新建、不重复发事件。
	OpID string `gorm:"column:op_id;type:varchar(64);not null;uniqueIndex:idx_sales_order_op_id" json:"op_id"`
	// ServerVersion 是服务端版本号(同步水位;不用客户端时间戳)。
	ServerVersion int64 `gorm:"column:server_version;type:bigint;not null;default:0" json:"server_version"`
	// 审计列。
	CreatedBy string         `gorm:"column:created_by;type:varchar(64);not null" json:"created_by"`
	UpdatedBy string         `gorm:"column:updated_by;type:varchar(64)" json:"updated_by"`
	CreatedAt time.Time      `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time      `gorm:"column:updated_at;not null" json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"column:deleted_at;index" json:"-"`
}

// TableName 显式指定表名。
func (SalesOrder) TableName() string { return "sales_orders" }

// SaleLine 是销售明细。
//
// ⚠️ 商品名 / 货号 / 条码 / 单位是**成交快照**,不是外键实时 join。
type SaleLine struct {
	// ID 是明细 id(= <sale_id>-L<n>,全局唯一)。
	ID string `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	// SaleID 是所属销售单。
	SaleID string `gorm:"column:sale_id;type:varchar(64);not null;index:idx_sale_line_sale" json:"sale_id"`
	// BranchID 是门店(冗余,便于按店统计)。
	BranchID string `gorm:"column:branch_id;type:varchar(64);not null;index:idx_sale_line_branch" json:"branch_id"`
	// ProductID 是商品 id(catalog.products.id)。
	ProductID string `gorm:"column:product_id;type:varchar(64)" json:"product_id"`
	// ProductName 是商品名快照。
	ProductName string `gorm:"column:product_name;type:varchar(255)" json:"product_name"`
	// ProductNo 是货号快照。
	ProductNo string `gorm:"column:product_no;type:varchar(64)" json:"product_no"`
	// Barcode 是条码快照。
	Barcode string `gorm:"column:barcode;type:varchar(64)" json:"barcode"`
	// Unit 是单位快照。
	Unit string `gorm:"column:unit;type:varchar(32)" json:"unit"`
	// Qty 是数量。
	Qty decimal.Decimal `gorm:"column:qty;type:numeric(18,4);not null;default:0" json:"qty"`
	// OriginalPriceYuan 是原价(划线价)。
	OriginalPriceYuan decimal.Decimal `gorm:"column:original_price_yuan;type:numeric(18,4);not null;default:0" json:"original_price_yuan"`
	// UnitPriceYuan 是成交单价。
	UnitPriceYuan decimal.Decimal `gorm:"column:unit_price_yuan;type:numeric(18,4);not null;default:0" json:"unit_price_yuan"`
	// DiscountAmountYuan 是本行优惠额。
	DiscountAmountYuan decimal.Decimal `gorm:"column:discount_amount_yuan;type:numeric(18,4);not null;default:0" json:"discount_amount_yuan"`
	// AmountYuan 是本行成交金额(已扣行级优惠)。
	AmountYuan decimal.Decimal `gorm:"column:amount_yuan;type:numeric(18,4);not null;default:0" json:"amount_yuan"`
	// IsExchange 是换购品标记。
	IsExchange bool `gorm:"column:is_exchange;type:boolean;not null;default:false" json:"is_exchange"`
	// IsVoid 是停用品标记。
	IsVoid bool `gorm:"column:is_void;type:boolean;not null;default:false" json:"is_void"`
	// 审计列。
	CreatedBy string    `gorm:"column:created_by;type:varchar(64);not null" json:"created_by"`
	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式指定表名。
func (SaleLine) TableName() string { return "sale_lines" }

// Payment 是收付流水(一张单可有多条:现金 + 微信 + 积分抵扣 …)。
type Payment struct {
	// ID 是收付 id(= <sale_id>-P<n>;退货为 <sale_id>-RF<n>)。
	ID string `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	// SaleID 是所属销售单。
	SaleID string `gorm:"column:sale_id;type:varchar(64);not null;index:idx_payment_sale" json:"sale_id"`
	// BranchID 是门店。
	BranchID string `gorm:"column:branch_id;type:varchar(64);not null;index:idx_payment_branch" json:"branch_id"`
	// PayWay 是付款方式(RMB / SWX / SZF / SAV / JFP / EKT …)。
	PayWay string `gorm:"column:pay_way;type:varchar(32);not null" json:"pay_way"`
	// AmountYuan 是本方式收付金额。
	//
	// ⚠️ **退货行取负值**(负 = 退给顾客)。
	AmountYuan decimal.Decimal `gorm:"column:amount_yuan;type:numeric(18,4);not null;default:0" json:"amount_yuan"`
	// ExchangeRate 是汇率(本位币 = 1;外币填实际汇率)。
	ExchangeRate decimal.Decimal `gorm:"column:exchange_rate;type:numeric(18,4);not null;default:1" json:"exchange_rate"`
	// ConvertedAmountYuan 是折算成人民币的金额。
	ConvertedAmountYuan decimal.Decimal `gorm:"column:converted_amount_yuan;type:numeric(18,4);not null;default:0" json:"converted_amount_yuan"`
	// RefFlowNo 是外部流水号(第三方支付 / 原收付流水)。
	RefFlowNo string `gorm:"column:ref_flow_no;type:varchar(64)" json:"ref_flow_no"`
	// 审计列。
	CreatedBy string    `gorm:"column:created_by;type:varchar(64);not null" json:"created_by"`
	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式指定表名。
func (Payment) TableName() string { return "payments" }

// ---- 业务错误 sentinel(handler 层映 4xx / 5xx) ----

var (
	// ErrSaleNotFound:按 id(且门店)未命中。
	ErrSaleNotFound = errors.New("pos: 销售单不存在")
	// ErrLineNotFound:按 id 未命中明细。
	ErrLineNotFound = errors.New("pos: 销售明细不存在")
	// ErrSaleAlreadyReturned:该单已退货,不可重复退。
	ErrSaleAlreadyReturned = errors.New("pos: 销售单已退货")
	// ErrAmountMismatch:金额不自洽(合计 ≠ 明细之和 / 找零 ≠ 实收-合计 / 欠款)。
	ErrAmountMismatch = errors.New("pos: 金额不自洽")
	// ErrInvalidInput:入参非法。
	ErrInvalidInput = errors.New("pos: 入参非法")
)
