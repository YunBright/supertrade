// Package model —— 采购收货域(图5)的模型与错误。
//
// # 这一域读什么
//
//	cube `purchase_sheet`      采购单头(源表 t_pm_sheet_master)
//	cube `purchase_sheet_line` 采购单行(源表 t_pm_sheet_detail)
//
// ⚠️ **本轮只读**(PLAN §3.4):只有列表 + 详情,没有创建 / 修改 / 审核。
//
// # ⚠️ 金额符号:采购侧与结算侧**相反**
//
// 实测(2026-10-10):
//
//	采购侧 t_pm_sheet_master 的 RO 退货单是**正数**:ysx 3565 张合计 +175.2 万
//	结算侧 t_fm_recpay_detail 的 RO 是**负数**:ysx 3474 行合计 -171.7 万
//
// 直接把两边的退货额对比 ⇒ **差一个符号、约 346 万**。
//
// ⇒ 本域的净额一律取 `total_net_amount_yuan`
// (cube 侧已经把 RO 取负,与结算侧同口径)。
//
// # ⚠️ purchase_sheet_line **没有** trans_no
//
// 实测两库的 t_pm_sheet_detail 都没有 trans_no ⇒ 退货明细**不能**在本表内
// 用 segment 过滤 ⇒ 要判"这是收货行还是退货行"必须 join purchase_sheet。
//
// ⇒ 本域的详情端点是"按单号查头 + 按头查行",段信息由**头**给出。
package model

import (
	"errors"
	"time"

	"github.com/shopspring/decimal"
)

// ---- 错误 sentinel ----
var (
	// ErrCubeSourceUnavailable:问不到"这个门店配了哪个数据源"(链路故障)⇒ 503。
	ErrCubeSourceUnavailable = errors.New("procurement: 数据源路由暂不可用")
	// ErrCubeSourceDisabled:该门店的数据源被管理员停用 ⇒ 503。
	ErrCubeSourceDisabled = errors.New("procurement: 该门店的数据源已停用")
	// ErrLocalStoreNotReady:本地采购单库未装配 ⇒ 503(部署错误,不是业务结论)。
	ErrLocalStoreNotReady = errors.New("procurement: 本地采购单库未装配")
	// ErrPurchaseSheetNotFound:按单号查不到采购单 ⇒ 404。
	ErrPurchaseSheetNotFound = errors.New("procurement: 采购单不存在")
)

// PurchaseSheetItem 是采购单头的一行(PLAN §3.4 的响应项)。
//
// ⚠️ 字段名是**冻结契约** ⇒ 不许改名。
type PurchaseSheetItem struct {
	// SheetNo 是单据号(思迅 sheet_no,实测形如 PI002610224944)。
	SheetNo string `json:"sheet_no"`
	// BranchID 是分店号(思迅 branch_no)。
	BranchID string `json:"branch_id"`
	// BranchName 是分店名。
	//
	// ⚠️ cube 侧**没有**分店名字典 model ⇒ 查不到时为 **null**(不编造)。
	BranchName *string `json:"branch_name"`
	// SupplierID 是供应商编码(思迅 supcust_no)。
	SupplierID string `json:"supplier_id"`
	// SupplierName 是供应商名(cube supplier 字典,查不到时 null)。
	SupplierName *string `json:"supplier_name"`
	// DocumentType 是单据类型(思迅 trans_no:PI / RO / PO / WP / RT / XJ / YX)。
	DocumentType string `json:"document_type"`
	// DocumentTypeLabel 是单据类型的中文标签(前端不硬编码文案)。
	DocumentTypeLabel string `json:"document_type_label"`
	// ApproveStatus 是审核状态(draft / approved)。
	//
	// ⚠️ cube 侧存的是 "0" / "1" ⇒ 这里**翻译**过(规则见 service 层)。
	ApproveStatus string `json:"approve_status"`
	// ApproveStatusLabel 是审核状态的中文标签。
	ApproveStatusLabel string `json:"approve_status_label"`
	// OrderStatus 是订单状态(思迅 order_status,原样透传)。
	OrderStatus string `json:"order_status"`
	// CreatedAt 是制单时刻(RFC3339)。
	CreatedAt string `json:"created_at"`
	// CreatedBy 是制单人(思迅 oper_man)。
	CreatedBy string `json:"created_by"`
	// ConfirmedAt 是审核完成时刻(RFC3339;没审核过 ⇒ null)。
	ConfirmedAt *string `json:"confirmed_at"`
	// ConfirmedBy 是审核人(思迅 confirm_man)。
	ConfirmedBy string `json:"confirmed_by"`
	// TotalAmountYuan 是单据金额(元)。
	//
	// ⚠️ 采购侧**退货单是正数** ⇒ 这一列对 RO 是正的。
	TotalAmountYuan *string `json:"total_amount_yuan"`
	// TotalNetAmountYuan 是单据**净额**(元,退货取负)。
	//
	// ⚠️ **对账一律用这一列**(与结算侧同口径)。
	TotalNetAmountYuan *string `json:"total_net_amount_yuan"`
	// Currency 是币种(思迅 coin_no,实测 RMB;本域按 CNY 显示)。
	Currency string `json:"currency"`
	// PayWay 是付款方式(思迅 pay_way)。
	PayWay string `json:"pay_way"`
	// Remark 是备注(思迅 other1,实测装的是异常说明文本)。
	Remark string `json:"remark"`
}

// PurchaseSheetLine 是采购单的一行明细(PLAN §3.4 的详情行)。
type PurchaseSheetLine struct {
	// LineNo 是行号(单内序号,从 1 开始)。
	//
	// ⚠️ cube 侧**没有**行号维度(源表没有行序号列)⇒ 在 Go 侧按行 id 排序后编号。
	LineNo int `json:"line_no"`
	// ProductID 是商品货号。
	ProductID string `json:"product_id"`
	// ProductName 是商品名(查不到时 null)。
	ProductName *string `json:"product_name"`
	// Unit 是单位(查不到时 null)。
	Unit *string `json:"unit"`
	// OrderQty 是订货数量。
	OrderQty *string `json:"order_qty"`
	// ReceivedQty 是实收数量。
	ReceivedQty *string `json:"received_qty"`
	// ValidPriceYuan 是有效单价(元)。
	ValidPriceYuan *string `json:"valid_price_yuan"`
	// AmountYuan 是本行金额(元)。
	//
	// ⚠️ 采购侧退货行的金额同样是**正数** ⇒ 净额看**头**的 total_net_amount_yuan。
	AmountYuan *string `json:"amount_yuan"`
}

// PurchaseSheetDetail 是采购单详情(= 列表行 + 明细行)。
type PurchaseSheetDetail struct {
	PurchaseSheetItem
	// Lines 是该单的明细行。
	Lines []PurchaseSheetLine `json:"lines"`
}

// PurchaseSheetPage 是采购单列表的响应。
type PurchaseSheetPage struct {
	Items []PurchaseSheetItem `json:"items"`
	// Count 是**本页**条数。
	Count int `json:"count"`
	// Page / PageSize 回显请求值。
	Page     int `json:"page"`
	PageSize int `json:"page_size"`
	// Total 是过滤后的总条数。
	Total int `json:"total"`
	// DataSource 是 "cube" / "local"(PLAN §3.4 之外的附加字段)。
	DataSource string `json:"data_source,omitempty"`
}

// ---- 本地表(没配数据源的门店走这里) ----

// PurchaseSheet 是本域的采购单头表。
//
// ⚠️ **本轮没有写端点**:采购单的写入方是采购下单流程(下一轮)。
// ⇒ 表在、列在、查询在,只是还没有行 —— 这是**诚实的空**。
type PurchaseSheet struct {
	SheetNo   string `gorm:"primaryKey;column:sheet_no;type:varchar(64)" json:"sheet_no"`
	BranchID  string `gorm:"primaryKey;column:branch_id;type:varchar(64)" json:"branch_id"`
	SourceBranch string `gorm:"column:source_branch_no;type:varchar(64)" json:"source_branch_no"`
	BranchName string `gorm:"column:branch_name;type:varchar(128)" json:"branch_name"`
	SupplierID   string `gorm:"column:supplier_id;type:varchar(64);index:idx_purchase_sheet_supplier" json:"supplier_id"`
	SupplierName string `gorm:"column:supplier_name;type:varchar(255)" json:"supplier_name"`
	DocumentType string `gorm:"column:document_type;type:varchar(16)" json:"document_type"`
	ApproveStatus string `gorm:"column:approve_status;type:varchar(16)" json:"approve_status"`
	OrderStatus string `gorm:"column:order_status;type:varchar(32)" json:"order_status"`
	CreatedAt   *time.Time `gorm:"column:created_at" json:"created_at"`
	CreatedBy   string     `gorm:"column:created_by;type:varchar(64)" json:"created_by"`
	ConfirmedAt *time.Time `gorm:"column:confirmed_at" json:"confirmed_at"`
	ConfirmedBy string     `gorm:"column:confirmed_by;type:varchar(64)" json:"confirmed_by"`
	TotalAmountYuan    decimal.Decimal `gorm:"column:total_amount_yuan;type:numeric(18,4);not null;default:0" json:"total_amount_yuan"`
	TotalNetAmountYuan decimal.Decimal `gorm:"column:total_net_amount_yuan;type:numeric(18,4);not null;default:0" json:"total_net_amount_yuan"`
	Currency   string `gorm:"column:currency;type:varchar(8)" json:"currency"`
	PayWay     string `gorm:"column:pay_way;type:varchar(16)" json:"pay_way"`
	Remark     string `gorm:"column:remark;type:varchar(255)" json:"remark"`
	CreatedAt2 time.Time `gorm:"column:created_at_ts;not null" json:"-"`
	UpdatedAt2 time.Time `gorm:"column:updated_at_ts;not null" json:"-"`
}

// TableName 显式指定表名。
func (PurchaseSheet) TableName() string { return "purchase_sheets" }

// PurchaseSheetLineRow 是本域的采购单行表。
type PurchaseSheetLineRow struct {
	SheetID    string `gorm:"primaryKey;column:sheet_id;type:varchar(64)" json:"sheet_id"`
	LineNo     int    `gorm:"primaryKey;column:line_no" json:"line_no"`
	BranchID   string `gorm:"column:branch_id;type:varchar(64)" json:"branch_id"`
	ProductID  string `gorm:"column:product_id;type:varchar(64)" json:"product_id"`
	ProductName string `gorm:"column:product_name;type:varchar(255)" json:"product_name"`
	Unit       string `gorm:"column:unit;type:varchar(32)" json:"unit"`
	OrderQty   decimal.Decimal `gorm:"column:order_qty;type:numeric(18,4);not null;default:0" json:"order_qty"`
	ReceivedQty decimal.Decimal `gorm:"column:received_qty;type:numeric(18,4);not null;default:0" json:"received_qty"`
	ValidPriceYuan decimal.Decimal `gorm:"column:valid_price_yuan;type:numeric(18,4);not null;default:0" json:"valid_price_yuan"`
	AmountYuan decimal.Decimal `gorm:"column:amount_yuan;type:numeric(18,4);not null;default:0" json:"amount_yuan"`
	CreatedAt2 time.Time `gorm:"column:created_at_ts;not null" json:"-"`
	UpdatedAt2 time.Time `gorm:"column:updated_at_ts;not null" json:"-"`
}

// TableName 显式指定表名。
func (PurchaseSheetLineRow) TableName() string { return "purchase_sheet_lines" }
