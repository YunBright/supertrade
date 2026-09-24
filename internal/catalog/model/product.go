// Package model / product.go —— 商品本地表(本期 entry point,handler 暂未实现)。
//
// 设计要点(对齐 supplier.go 的索引策略):
//   - 复合 PK (id, branch_id):同商品名在不同门店可独立维护
//   - idx_product_branch_name(branch_id, name):按商品名查询
//   - idx_product_branch_category(branch_id, category_id):按品类过滤
//   - idx_product_branch_barcode(branch_id, barcode):扫条码精确查
//
// 为什么本期只建表不实现:
//   - 商品数据现由 cube 提供(./internal/cubehttp 在 stocktake.SearchProducts
//     调用 SearchProductsByBarcode),迁移需要 cube 仓库扩展 barcode 维度的
//     dimensions,不在 supertrade 范围。
//   - AutoMigrate 先建表,后续 Phase 直接填充数据即可,无需 schema migration。
package model

import (
	"time"
)

// Product 是 catalog 服务的商品本地表(本期 entry)。
type Product struct {
	ID         string    `gorm:"primaryKey;column:id;type:varchar(64)"`
	BranchID   string    `gorm:"primaryKey;column:branch_id;type:varchar(64)"`
	Name       string    `gorm:"column:name;type:varchar(255);not null;index:idx_product_branch_name"`
	CategoryID string    `gorm:"column:category_id;type:varchar(64);index:idx_product_branch_category"`
	SupplierID string    `gorm:"column:supplier_id;type:varchar(64)"`
	Barcode    string    `gorm:"column:barcode;type:varchar(64);index:idx_product_branch_barcode"`
	Unit       string    `gorm:"column:unit;type:varchar(32)"`
	Spec       string    `gorm:"column:spec;type:varchar(255)"`
	Status     string    `gorm:"column:status;type:varchar(16);not null;index:idx_product_branch_status"`
	CreatedBy  string    `gorm:"column:created_by;type:varchar(64);not null"`
	UpdatedBy  string    `gorm:"column:updated_by;type:varchar(64)"`
	CreatedAt  time.Time `gorm:"column:created_at;not null"`
	UpdatedAt  time.Time `gorm:"column:updated_at;not null"`
	DeletedAt  *time.Time `gorm:"column:deleted_at;index"`
}

// TableName 显式指定表名。
func (Product) TableName() string { return "products" }