// Package model / crossdomain.go —— 跨域**只读**投影（计划 §11）。
//
// # 为什么单独定义结构体，而不是 import settlement-auditor 的 model
//
//	业务域之间不互相 import model 是仓库的既有约定:两个域的表结构会各自演进,
//	直接复用结构体会让"改一个字段"变成跨服务的编译期耦合。
//
//	这里只声明**本域真正读的列**,并显式 TableName 指到对方的表。
//	多一列不代表契约,少一列也不影响 oracle —— SELECT 出来即可。
//
// # 红线：只 SELECT，禁止写
//
//	approval_items / approval_instances / approval_monthly_items / settlement_pay_states
//	的唯一写方是 settlement-auditor。本域只读它们算出付款台账与应付,
//	任何写回都会与对方的写入/状态机冲突(双写必然漂移)。
package model

import (
	"time"

	"github.com/shopspring/decimal"
)

// CrossApprovalInstance 是 approval_instances 的只读投影。
type CrossApprovalInstance struct {
	ID        string    `gorm:"column:id"`
	BranchID  string    `gorm:"column:branch_id"`
	Status    string    `gorm:"column:status"`
	SpNo      string    `gorm:"column:sp_no"`
	SpStatus  int       `gorm:"column:sp_status"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// TableName 指向 settlement-auditor 的表（只读）。
func (CrossApprovalInstance) TableName() string { return "approval_instances" }

// CrossApprovalItem 是 approval_items 的只读投影。
type CrossApprovalItem struct {
	ID         string          `gorm:"column:id"`
	ApprovalID string          `gorm:"column:approval_id"`
	BranchID   string          `gorm:"column:branch_id"`
	Direction  string          `gorm:"column:direction"`
	ItemType   string          `gorm:"column:item_type"`
	Remark     string          `gorm:"column:remark"`
	AmountYuan decimal.Decimal `gorm:"column:amount_yuan"`
	CreatedAt  time.Time       `gorm:"column:created_at"`
}

// TableName 指向 settlement-auditor 的表（只读）。
func (CrossApprovalItem) TableName() string { return "approval_items" }

// CrossMonthlyItem 是 approval_monthly_items 的只读投影（月度总表）。
type CrossMonthlyItem struct {
	ID             string          `gorm:"column:id"`
	BranchID       string          `gorm:"column:branch_id"`
	ApprovalID     string          `gorm:"column:approval_id"`
	ItemID         string          `gorm:"column:item_id"`
	Direction      string          `gorm:"column:direction"`
	ItemType       string          `gorm:"column:item_type"`
	Remark         string          `gorm:"column:remark"`
	AmountYuan     decimal.Decimal `gorm:"column:amount_yuan"`
	YearMonth      string          `gorm:"column:year_month"`
	ParseStatus    string          `gorm:"column:parse_status"`
	SpStatus       int             `gorm:"column:sp_status"`
	InstanceStatus string          `gorm:"column:instance_status"`
	PayState       string          `gorm:"column:pay_state"`
	SpNo           string          `gorm:"column:sp_no"`
	DecidedAt      *time.Time      `gorm:"column:decided_at"`
	CreatedAt      time.Time       `gorm:"column:created_at"`
}

// TableName 指向 settlement-auditor 的表（只读）。
func (CrossMonthlyItem) TableName() string { return "approval_monthly_items" }

// CrossSettlementPayState 是 settlement_pay_states 的只读投影。
type CrossSettlementPayState struct {
	ID           string     `gorm:"column:id"`
	BranchID     string     `gorm:"column:branch_id"`
	SettlementID string     `gorm:"column:settlement_id"`
	SupplierID   string     `gorm:"column:supplier_id"`
	PaidStatus   string     `gorm:"column:paid_status"`
	SetMethod    string     `gorm:"column:set_method"`
	ApprovalID   string     `gorm:"column:approval_id"`
	PaidAt       *time.Time `gorm:"column:paid_at"`
	CreatedAt    time.Time  `gorm:"column:created_at"`
	UpdatedAt    time.Time  `gorm:"column:updated_at"`
}

// TableName 指向 settlement-auditor 的表（只读）。
func (CrossSettlementPayState) TableName() string { return "settlement_pay_states" }
