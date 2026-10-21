// Package model / product_barcode.go —— 商品条码(1 商品 : N 条码)。
//
// # 为什么不能把条码塞进 products.barcode
//
// 思迅的**条码不在商品主表上**:
//   - `t_bd_item_info`(65 列)**没有 barcode 列**;
//   - 条码在独立表 `t_bd_item_barcode(item_no, item_barcode, …)`,实测**只有 153 行**。
//
// 而这 153 行里,同一个商品会出现**多次** —— 实测样例:
//
//	6914782121881 → 6908946288183   (other1='草莓味')
//	6914782121881 → 6908946288185   (other1='桔子味')
//
// ⇒ **一个商品可以有多条码**(口味/规格变体各一个),
// 单列 `products.barcode` **装不下**源库的数据。
//
// # 那 153 行以外呢
//
// 只有 153 个商品有独立条码;**其余商品的条码就是 `item_no` 本身**
// (POS 扫的 `6957583900828` 就是 item_no)。
//
// ⇒ 导入规则:
//   - `t_bd_item_barcode` 里有 ⇒ 用它,并把其中一条标为主条码;
//   - 没有 ⇒ **用 item_no 兜底**当条码(必须兜底,否则绝大多数商品扫不出来)。
//
// # products.barcode 为什么还留着
//
// 冗余**主条码**,让"扫条码 → 找商品"在多数情况下一次命中(不用先查条码表)。
// 两处不一致时以本表为准(条码表是全集)。
//
// # 索引设计要点
//
//   - 复合 PK (product_id, branch_id, barcode)
//   - idx_product_barcode_branch_code(branch_id, barcode):**扫码查商品的主路径**
//
// 时间戳字段用 datetime(不用 timestamptz)以兼容 SQLite 单测;
// 生产 PG 会自动用 timestamp with time zone,GORM 双向兼容。
package model

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// ProductBarcode 是 catalog 服务的商品条码表。
//
// Label 是该条码的说明(思迅 `other1`,实测值如 "草莓味"/"桔子味")——
// 同一条码的变体靠它区分。
type ProductBarcode struct {
	ProductID string `gorm:"primaryKey;column:product_id;type:varchar(64)" json:"product_id"`
	BranchID  string `gorm:"primaryKey;column:branch_id;type:varchar(64)" json:"branch_id"`
	Barcode   string `gorm:"primaryKey;column:barcode;type:varchar(64);index:idx_product_barcode_branch_code" json:"barcode"`
	// IsPrimary 标记主条码(= products.barcode 冗余的那一条)。
	//
	// 一个商品**至多一条** is_primary=true;导入时保证。
	IsPrimary bool           `gorm:"column:is_primary;type:boolean;not null;default:false" json:"is_primary"`
	Label     string         `gorm:"column:label;type:varchar(64)" json:"label"`
	CreatedBy string         `gorm:"column:created_by;type:varchar(64);not null" json:"created_by"`
	CreatedAt time.Time      `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time      `gorm:"column:updated_at;not null" json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"column:deleted_at;index" json:"deleted_at"`
}

// TableName 显式指定表名。
func (ProductBarcode) TableName() string { return "product_barcodes" }

// ---- 业务错误 sentinel ----

var (
	// ErrBarcodeNotFound:按 (branch_id, barcode) 未命中。
	ErrBarcodeNotFound = errors.New("catalog: 条码不存在")
)
