// Package model 定义 catalog 服务的 GORM 模型。
//
// 数据归属(catalog 服务本期拥有):
//   - suppliers  本地表,每个门店一份(同供应商可在不同门店分别维护)
//   - products   本地表(留 entry point,未来实装)
//
// 索引设计要点:
//   - 复合 PK (id, branch_id):同供应商名在不同门店可重复(子门店独立维护)
//   - idx_supplier_phone(branch_id, phone):按电话查询常用场景(进货单录入)
//   - idx_supplier_status(branch_id, status):过滤 active 供应商列表
//
// 时间戳字段用 datetime(不用 timestamptz)以兼容 SQLite 单测;
// 生产 PG 会自动用 timestamp with time zone,GORM 双向兼容。
package model

import (
	"errors"
	"time"
)

// Supplier 是 catalog 服务的供应商本地表。
//
// 类型(type):
//   - "0" = 供应商(给本门店供货)
//   - "1" = 客户(从本门店进货)
//
// 状态(status):
//   - "active"  = 启用
//   - "inactive" = 停用(软删,DeletedAt 才是硬删)
type Supplier struct {
	ID        string    `gorm:"primaryKey;column:id;type:varchar(64)"`
	BranchID  string    `gorm:"primaryKey;column:branch_id;type:varchar(64)"`
	Name      string    `gorm:"column:name;type:varchar(255);not null;index:idx_supplier_branch_name"`
	Type      string    `gorm:"column:type;type:varchar(16);not null;default:'0'"`
	Contact   string    `gorm:"column:contact;type:varchar(128)"`
	Phone     string    `gorm:"column:phone;type:varchar(32);index:idx_supplier_phone"`
	Email     string    `gorm:"column:email;type:varchar(255)"`
	Address   string    `gorm:"column:address;type:text"`
	Status    string    `gorm:"column:status;type:varchar(16);not null;index:idx_supplier_status"`
	CreatedBy string    `gorm:"column:created_by;type:varchar(64);not null"`
	UpdatedBy string    `gorm:"column:updated_by;type:varchar(64)"`
	CreatedAt time.Time `gorm:"column:created_at;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null"`
	DeletedAt *time.Time `gorm:"column:deleted_at;index"`
}

// TableName 显式指定表名。
func (Supplier) TableName() string { return "suppliers" }

// ---- 业务错误 sentinel(handler 层映 4xx / 5xx) ----

var (
	// ErrSupplierNotFound:按 (id, branch_id) 未命中。
	ErrSupplierNotFound = errors.New("catalog: supplier 不存在")
	// ErrSupplierAlreadyExists:同 (id, branch_id) 已存在。
	ErrSupplierAlreadyExists = errors.New("catalog: supplier 已存在")
	// ErrInvalidInput:必填字段缺失或非法。
	ErrInvalidInput = errors.New("catalog: 入参非法")
)