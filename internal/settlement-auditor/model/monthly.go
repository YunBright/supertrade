// Package model / monthly.go —— 企微审批月度总表（计划 §9）。
//
// # 为什么要有这张表
//
// `countHistoricalMonthlyItems` 原来在每次机器审核时实时 JOIN
// `approval_items` + `approval_instances`：审批单多年累积后（10 万行级），
// 每审一单就扫一遍全表，是审核流水线最慢的一步。
//
// 月度总表把「所属年月 / 审批状态 / 付款状态」**在写入与状态变更时就固化**，
// 读侧只按 (branch_id, item_type, year_month) 走索引 ⇒ 从 O(N) 降到 O(命中行)。
//
// # 单一写方
//
// 只有 settlement-auditor 写这张表；cash-recon 通过同库**只读**它
// （见 internal/cashrecon/model/crossdomain.go）。双写必然漂移。
//
// # 与 approval_items 的关系
//
// 月度总表是 approval_items 的**派生投影**（追加解析出的 YearMonth 与审批状态），
// 不是替代品：approval_items 仍是明细事实源。派生行带 item_id ⇒ 可回查明细。
// 任何不一致都可以用 POST /internal/monthly-items/rebuild 重建。
package model

import (
	"time"

	"github.com/shopspring/decimal"
)

// 月度总表的解析状态。
const (
	// MonthlyParseOK 表示从备注里解析出了所属年月。
	MonthlyParseOK = "ok"
	// MonthlyParseUnmatched 表示解析不出所属年月。
	//
	// ⚠️ 这类行**仍要参与**「每月有且仅有一单」的存在性判据（fail-closed）：
	// 静默跳过等于把这笔历史付款从统计里抹掉 ⇒ 同月已付过也会判成 1 笔。
	MonthlyParseUnmatched = "unmatched"
)

// 付款状态。
const (
	PayStateUnpaid = "unpaid"
	PayStatePaid   = "paid"
)

// MonthlyBackfillVersion 是懒回填的版本号。
//
// 解析口径变更（例如补了新的备注日期写法）后把它 +1，各门店会在下一次
// 读取时重新回填一遍历史行。
//
//	2 —— 付款状态口径修正：sp_status=2（通过/接单未付）改为 unpaid，
//	     只有 10（已支付）才是 paid；旧行必须重投影。
//	3 —— 回滚 2 的口径：付款审批的办理人处理完即 sp_status=2（企微流程结束），
//	     2 与 10 都算 paid；旧行必须重投影。
const MonthlyBackfillVersion = "3"

// ApprovalMonthlyItem 是审批明细的月度投影（按 branch + 审批 + 明细唯一）。
type ApprovalMonthlyItem struct {
	ID         string `gorm:"primaryKey;column:id;size:36" json:"id"`
	BranchID   string `gorm:"column:branch_id;size:32;not null;uniqueIndex:ux_monthly_item,priority:1;index:idx_monthly_branch_type,priority:1" json:"branch_id"`
	ApprovalID string `gorm:"column:approval_id;size:32;not null;uniqueIndex:ux_monthly_item,priority:2;index" json:"approval_id"`
	// ItemID 指回 approval_items.id（派生关系可追溯）。
	ItemID     string          `gorm:"column:item_id;size:36;not null;uniqueIndex:ux_monthly_item,priority:3" json:"item_id"`
	Direction  string          `gorm:"column:direction;size:16;not null;index" json:"direction"`
	ItemType   string          `gorm:"column:item_type;size:64;not null;index:idx_monthly_branch_type,priority:2" json:"item_type"`
	Remark     string          `gorm:"column:remark;size:256" json:"remark"`
	AmountYuan decimal.Decimal `gorm:"column:amount_yuan;type:numeric(14,2)" json:"amount_yuan"`
	// YearMonth 形如 2026-09；解析失败为空。
	YearMonth string `gorm:"column:year_month;size:8;index:idx_monthly_branch_type,priority:3" json:"year_month"`
	// ParseStatus ∈ ok / unmatched。
	ParseStatus string `gorm:"column:parse_status;size:16;not null;index" json:"parse_status"`
	// SpStatus 是企微审批状态（1审批中 2通过 3驳回 4撤销 6通过后撤销 10已支付）。
	SpStatus int `gorm:"column:sp_status;not null;default:0" json:"sp_status"`
	// InstanceStatus 是内部审批状态机（human_rejected / cancelled 等会被排除）。
	InstanceStatus string `gorm:"column:instance_status;size:24;index" json:"instance_status"`
	// PayState ∈ unpaid / paid。
	PayState  string     `gorm:"column:pay_state;size:16;index" json:"pay_state"`
	SpNo      string     `gorm:"column:sp_no;size:64" json:"sp_no"`
	DecidedAt *time.Time `gorm:"column:decided_at" json:"decided_at"`
	CreatedAt time.Time  `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time  `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式指定表名。
func (ApprovalMonthlyItem) TableName() string { return "approval_monthly_items" }

// ApprovalMonthlyBackfill 记录某门店的历史回填已完成（懒回填只跑一次）。
type ApprovalMonthlyBackfill struct {
	BranchID string    `gorm:"primaryKey;column:branch_id;size:32" json:"branch_id"`
	Version  string    `gorm:"column:version;size:16;not null" json:"version"`
	DoneAt   time.Time `gorm:"column:done_at;not null" json:"done_at"`
}

// TableName 显式指定表名。
func (ApprovalMonthlyBackfill) TableName() string { return "approval_monthly_backfills" }
