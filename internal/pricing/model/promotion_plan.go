// Package model / promotion_plan.go —— 促销**方案头**的本地表。
//
// # 为什么方案头要单独一张表
//
// 思迅的促销有两层:
//
//	t_pub_plan_master  方案头(方案号 / 名称 / 起止 / 审核 / 终止)
//	t_pub_plan_flow    方案行(方案 → 商品 → 价)
//
// 本域已有的 `promotions` 表存的是**行**(plan_no + product_id + 价 + 生效期),
// 图4 要的是**头**(方案名称 / 摘要 / 审核人 / 终止人 …)。
//
// ⚠️ 两者**并存**,不要合并:合并之后"一个方案有几个商品"这个信息就丢了,
// 而促销行要按商品取最低价(见 promotions 表的注释)。
//
// # 为什么本地也要有这张表
//
// 自营门店(门店 10)**没有 cube 源** ⇒ 它的促销方案没有别的地方可取。
// 而促销方案是**时效数据**(随时录入、随时停用),落快照必然过期 ⇒
// 本地表由本域自己维护(与 promotions 表同一套生命周期)。
//
// # ⚠️ approve_flag 的语义(**不要**把源码直接透给前端)
//
// 源列 `approve_flag` 是 varchar(1),实测取值 `2` = 已审核。
// 本表存的是**翻译后**的状态(draft / approved / stopped / unknown),
// 翻译规则见 service/promotion_plan.go 的 approveStatusOf。
//
// 存译文而不是存源码 ⇒ 本域只有一个真相源;否则每条读路径都要重新翻译一遍,
// 而翻译规则一旦漂移("2"在一处是 approved、在另一处是 approved 之外的),
// 症状是"列表说已审核、详情说未审核"。
package model

import (
	"time"

	"github.com/shopspring/decimal"
)

// PromotionPlan 是促销方案头本地表。
//
// 状态(approve_status):
//
//	"draft"    未审核
//	"approved" 已审核
//	"stopped"  已终止
//	"unknown"  源库给了看不懂的值(不许猜)
type PromotionPlan struct {
	// PlanNo 是方案编号(思迅 plan_no,实测形如 PS002109160001)。
	PlanNo string `gorm:"primaryKey;column:plan_no;type:varchar(64)" json:"plan_no"`
	// BranchID 是门店(复合主键的一部分:同号方案可在不同门店独立维护)。
	BranchID string `gorm:"primaryKey;column:branch_id;type:varchar(64)" json:"branch_id"`

	// PlanName 是方案名称(印在价签上用于人工追溯"这个价哪来的")。
	PlanName string `gorm:"column:plan_name;type:varchar(255)" json:"plan_name"`
	// PlanMemo 是方案摘要。
	PlanMemo string `gorm:"column:plan_memo;type:varchar(255)" json:"plan_memo"`
	// BeginDate / EndDate 是方案起止**日期**。
	BeginDate *time.Time `gorm:"column:begin_date;type:date" json:"begin_date"`
	EndDate   *time.Time `gorm:"column:end_date;type:date" json:"end_date"`

	// CreatedAt 是制单时刻;CreatedBy 是制单人(思迅 oper_man)。
	CreatedAt *time.Time `gorm:"column:created_at" json:"created_at"`
	CreatedBy string     `gorm:"column:created_by;type:varchar(64)" json:"created_by"`
	// ApprovedAt / ApprovedBy 是审核时刻 / 审核人(思迅 confirm_date / confirm_man)。
	ApprovedAt *time.Time `gorm:"column:approved_at" json:"approved_at"`
	ApprovedBy string     `gorm:"column:approved_by;type:varchar(64)" json:"approved_by"`
	// StoppedAt / StoppedBy 是终止时刻 / 终止人(思迅 stop_date / stop_man)。
	StoppedAt *time.Time `gorm:"column:stopped_at" json:"stopped_at"`
	StoppedBy string     `gorm:"column:stopped_by;type:varchar(64)" json:"stopped_by"`

	// ApproveStatus 是**翻译后**的审核状态(draft / approved / stopped / unknown)。
	ApproveStatus string `gorm:"column:approve_status;type:varchar(16);not null;default:'unknown';index:idx_promotion_plan_status" json:"approve_status"`
	// ApproveFlag 是源库原值(留档;**不**对外暴露给前端)。
	ApproveFlag string `gorm:"column:approve_flag;type:varchar(4)" json:"approve_flag"`
	// MemberLevel 是会员等级(思迅 vip_type,实测恒 "无")。
	MemberLevel string `gorm:"column:member_level;type:varchar(64)" json:"member_level"`
	// RuleNo 是规则类型(思迅 rule_no,`PS` = 促销)。
	RuleNo string `gorm:"column:rule_no;type:varchar(16)" json:"rule_no"`

	// TotalAmountYuan 是方案总金额(元)。本地没有金额口径时留空。
	TotalAmountYuan *decimal.Decimal `gorm:"column:total_amount_yuan;type:numeric(18,4)" json:"total_amount_yuan"`
}

// TableName 显式指定表名。
func (PromotionPlan) TableName() string { return "promotion_plans" }
