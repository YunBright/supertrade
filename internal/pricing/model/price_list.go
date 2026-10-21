// Package model / price_list.go —— 售价表。
//
// # pricing 从"只读 cube 代理"变成"价格的拥有者"(2026-10-21)
//
// 此前 pricing **不建表**:售价来自 cube product 档案、促销来自 cube promotion,
// 全部实时透传。理由是"促销时效性极强,落本地快照必然过期"。
//
// 现在建表,原因是**自营门店(门店 10)没有 cube 源**:
// `branch_cube_sources` 实测只有 `00` → cube-sixun-ysx-fb 与
// `01` → cube-sixun-hbposv7-jiale ⇒ 门店 10 的价格**没有别的地方可取**。
//
// ⚠️ 促销仍保持"每次现查源库"的**读**路径(时效性优先),
// 但**门店自营的价格必须有本地真相源** ——
// 否则 POS 扫一个条码只能问 cube,而门店 10 的 cube 路径必然 503。
//
// # 价格与商品档案分表(两个真相源问题)
//
// `catalog.products` **不存价**。理由:价格有生效期与改价历史
// (小票是对账凭据,改了价不能让历史单对不上),而商品档案是"当前值"。
// 两处都存价 ⇒ 两个真相源。
//
// # 金额:decimal,永不用 double
//
//   - DB:numeric(18,4)(不是 float / double —— PG 的 double 有二进制误差,
//     0.1 + 0.2 != 0.3,收银对账会差一分钱)
//   - Go:shopspring/decimal
//   - JSON:**字符串**(见 archivedto 的金额约定)——
//     JSON number 到了 Dart 侧会被 jsonDecode 变成 double,精度当场丢掉
//
// # 一行 = 一个商品在一个价目表里的当前价
//
// PK = (id, branch_id, product_id)。
//   - id 是**价目表编码**(默认 "default"):预留将来"会员价目表""团购价目表"
//   - 同一商品在一个价目表里**只有一行** —— 重复导入是 upsert,不会堆行
//
// ⚠️ 改价**不留历史版本**:历史由**销售单行上的成交价快照**承担
// (小票记的是当时的价格)。价格表是"当前值",不是流水。
package model

import (
	"errors"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// PriceList 是 pricing 服务的售价表。
//
// 状态(status):
//   - "active"   = 生效中
//   - "inactive" = 停用(不参与查价)
//
// 软删:用 gorm.DeletedAt(**不是** *time.Time)——
// *time.Time 不会被 GORM 自动过滤,删掉的行照样查得到。
type PriceList struct {
	// ID 是价目表编码(默认 "default")。
	ID string `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	// BranchID 是门店。同一个商品在不同门店可以有不同售价。
	BranchID string `gorm:"primaryKey;column:branch_id;type:varchar(64)" json:"branch_id"`
	// ProductID 是商品货号(思迅 item_no,与 catalog.products.id 同一套编码)。
	ProductID string `gorm:"primaryKey;column:product_id;type:varchar(64)" json:"product_id"`

	// PriceYuan 是零售价(元)。
	//
	// ⚠️ "没维护价"与"售价 0 元"是两件事:
	// 前者 = **这个商品在 price_lists 里根本没有行**;
	// 后者 = 有一行,price_yuan = 0。
	// 查价时按"有没有行"区分 ⇒ POS 印「未维护售价」而不是印 ¥0.00。
	PriceYuan decimal.Decimal `gorm:"column:price_yuan;type:numeric(18,4);not null" json:"price_yuan"`
	// MemberPriceYuan 是会员价(元)。0 = 没有会员价(不是"会员价 0 元")。
	MemberPriceYuan decimal.Decimal `gorm:"column:member_price_yuan;type:numeric(18,4);not null;default:0" json:"member_price_yuan"`
	// CostYuan 是成本价(元)。0 = 不知道成本。
	//
	// ⚠️ 成本价**只用于毛利估算**,不参与收银计价。
	// 毛利是"卖价 - 成本",成本错不影响收款金额。
	CostYuan decimal.Decimal `gorm:"column:cost_yuan;type:numeric(18,4);not null;default:0" json:"cost_yuan"`

	// EffectiveAt 是这个价**开始生效的那天**(日粒度,存当天零点)。
	//
	// ⚠️ 查价时 `effective_at <= 基准日` 才算数 ⇒ 未来生效的价不会提前印出去。
	//
	// ⚠️ **日粒度**而不是"时刻":价签按天印,查价的基准时刻也是
	// "基准日的 00:00:00"。若这里存"10:00 导入"这种带时分的值,
	// 则当天导入的价当天查不到(10:00 > 00:00)。
	EffectiveAt time.Time `gorm:"column:effective_at;not null;index:idx_price_branch_product" json:"effective_at"`
	// ExpiredAt 是这个价**失效的那天**(日粒度);nil = 长期有效。
	//
	// ⚠️ 失效**不删行**:删了就看不出"这个价什么时候结束的"。
	//
	// ⚠️ 判定是 `expired_at >= 基准日` ⇒ **失效当天仍算有效**
	// (与"活动截止日当天仍生效"同一口径,见 model/promotion.go)。
	ExpiredAt *time.Time `gorm:"column:expired_at" json:"expired_at,omitempty"`
	// Status 是 active / inactive。
	Status string `gorm:"column:status;type:varchar(16);not null;default:'active'" json:"status"`

	CreatedBy string    `gorm:"column:created_by;type:varchar(64);not null" json:"created_by"`
	UpdatedBy string    `gorm:"column:updated_by;type:varchar(64)" json:"updated_by"`
	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
	// DeletedAt 是软删标记(gorm.DeletedAt,不是 *time.Time —— 后者不被自动过滤)。
	DeletedAt gorm.DeletedAt `gorm:"column:deleted_at;index" json:"deleted_at"`
}

// TableName 显式指定表名。
func (PriceList) TableName() string { return "price_lists" }

// ---- 业务错误 sentinel(handler 层映 4xx / 5xx) ----

var (
	// ErrPriceListNotFound:按 (id, branch_id, product_id) 未命中。
	ErrPriceListNotFound = errors.New("pricing: 售价不存在")
	// ErrInvalidInput:入参非法。
	ErrInvalidInput = errors.New("pricing: 入参非法")
)
