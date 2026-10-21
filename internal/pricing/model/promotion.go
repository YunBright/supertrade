// Package model / promotion.go —— 促销表。
//
// # 促销是**时效数据**,但仍然要落表
//
// 思迅的促销单在 `t_pub_plan_flow`:随时录入、随时停用、有起止日期。
// cube 的 promotion model 是 **live**(查询时透传源库),因为"落快照必然过期"。
//
// 那为什么还要表?因为**自营门店(门店 10)没有 cube 源** ——
// 门店 10 的促销单在自己的系统里录,必须落在自己的库里。
//
// ⇒ 两套读路径并存:
//   - 门店 00 / 01(有 cube 源):促销仍**实时查 cube**(时效性最好)
//   - 门店 10(自营):促销查本表
//
// # 同一商品多个促销单 ⇒ 取最低价
//
// cube 的 promotion model 有 `min_promo_price` measure,注释写得很直白:
// 「同一 item 可能被多个促销单覆盖。调用方取 min = 用户实际能买到的最低价」。
// ⇒ 本表**不阻止**一个商品有多条生效促销,但查价时取**促销价最低**的那条。
//
// # 金额:decimal,永不用 double
//
// 同 price_list.go 的说明(DB numeric / Go decimal / JSON 字符串)。
package model

import (
	"errors"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// Promotion 是 pricing 服务的促销表。
//
// 状态(status):
//   - "active"   = 生效中
//   - "inactive" = 已停用(不参与查价)
//
// 软删:用 gorm.DeletedAt。
type Promotion struct {
	// ID 是促销单号(思迅 `plan_no`)。
	//
	// ⚠️ 它与 (branch_id, product_id) 一起构成 PK ——
	// 一个促销单可能覆盖多个商品,所以不能只拿 plan_no 当主键。
	ID string `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	// BranchID 是门店。
	BranchID string `gorm:"primaryKey;column:branch_id;type:varchar(64);index:idx_promotion_branch_product" json:"branch_id"`
	// ProductID 是商品货号(思迅 item_no)。
	ProductID string `gorm:"primaryKey;column:product_id;type:varchar(64);index:idx_promotion_branch_product" json:"product_id"`

	// PlanName 是促销单名称(印在价签上用于人工追溯"这个价哪来的")。
	PlanName string `gorm:"column:plan_name;type:varchar(255)" json:"plan_name"`
	// PriceYuan 是**促销价**(元)。
	//
	// ⚠️ 思迅源列是 `price`,而 `sale_price` 是**原价**——
	// 两者语义相反(cube 的 mapping 强制改名,见 cube sixun-models/promotion)。
	// 拿反了价签会印出「原价 ¥3.50 / 促销价 ¥9.90」,而且**全程不报错**。
	PriceYuan decimal.Decimal `gorm:"column:price_yuan;type:numeric(18,4);not null" json:"price_yuan"`
	// OrigPriceYuan 是**原价**(元);0 = 源库没给原价(前端回退到零售价)。
	OrigPriceYuan decimal.Decimal `gorm:"column:orig_price_yuan;type:numeric(18,4);not null;default:0" json:"orig_price_yuan"`

	// BeginAt / EndAt 是促销的起止时刻(含端点)。
	//
	// ⚠️ 查价时判定 `begin_at <= 基准日 <= end_at`。
	// 基准日是**日期**粒度(价签按天),所以这里存的是当天的 00:00:00。
	BeginAt time.Time `gorm:"column:begin_at;not null;index:idx_promotion_window" json:"begin_at"`
	EndAt   time.Time `gorm:"column:end_at;not null;index:idx_promotion_window" json:"end_at"`

	// Status 是 active / inactive。
	Status string `gorm:"column:status;type:varchar(16);not null;default:'active'" json:"status"`

	CreatedBy string         `gorm:"column:created_by;type:varchar(64);not null" json:"created_by"`
	UpdatedBy string         `gorm:"column:updated_by;type:varchar(64)" json:"updated_by"`
	CreatedAt time.Time      `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time      `gorm:"column:updated_at;not null" json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"column:deleted_at;index" json:"deleted_at"`
}

// TableName 显式指定表名。
func (Promotion) TableName() string { return "promotions" }

// ErrPromotionNotFound:按 (id, branch_id, product_id) 未命中。
var ErrPromotionNotFound = errors.New("pricing: 促销不存在")
