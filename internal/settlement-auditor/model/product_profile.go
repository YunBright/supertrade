// Package model / product_profile.go —— 商品经营属性 + 陈列保有量 + 历史缓存
// （2026-11-01 / 2026-11-02 用户口径）。
//
// # 三张表各管一件事
//
//	product_profiles    这个商品的「可售上限天数」与保质期 —— 自动按思迅类别标记、
//	                    品名关键词兜底，人工可修正；审批时按**组**取最严的那一个当上限。
//	stocking_displays   这个**组**的「正常陈列保有量」—— 货架上的陈列不算可售库存，
//	                    自动按历史单批进货量 P50 算；人工改过的不再自动覆盖。
//	stocking_fact_cache 近 180 天历史聚合的缓存（TTL 12 小时）——
//	                    同一批 SKU 一天内被多次审批命中时不再重复打源库。
//
// # 为什么都要自动生成
//
// 用户口径（2026-11-01）：「系统功能要够，但操作要足够少、足够简单」——
// 品类/保质期/陈列量不能靠人工录，否则等于虚设。
// ⇒ 三张表都是**审批时被动触发**写入，人工只在「阻断后修正清单」里改个别行。
//
// ⚠️ 2026-11-02：旧的「一次进货下限」（stocking_floors）已并入陈列保有量——
//
//	单批最小包装进货由陈列保有量覆盖，不再单独维护下限参数。
//
// # 为什么落在本域 PG 而不是 cube
//
// 它们是**审核用的参数与判据**，不是门店业务单据：源库里没有这些字段，
// 读取方就是本进程自己（同库零跨服务调用）。
// ⚠️ 这些表的内容**不进 AuditInput** ⇒ 不进 LLM 输入面（只有算出来的事实进）。
package model

import (
	"time"

	"github.com/shopspring/decimal"
)

// 商品经营属性/陈列量的来源（与备货系数共用同一套取值）。
const (
	// ProfileSourceAuto 是系统按品名关键词自动标记（审批通过时写入）。
	ProfileSourceAuto = "auto"
	// ProfileSourcePending 是本次审批被阻断，等人工在「待人工确认」清单里修正。
	ProfileSourcePending = "pending"
	// ProfileSourceManual 是人工改过 —— 自动标记**不再覆盖**。
	ProfileSourceManual = "manual"
)

// 品类档位（决定可售上限天数）。
const (
	ProfileClassFresh    = "fresh"    // 生鲜：7 天
	ProfileClassChilled  = "chilled"  // 冷藏/短保：20 天
	ProfileClassFrozen   = "frozen"   // 冷冻：30 天
	ProfileClassBeverage = "beverage" // 饮料/酒水：45 天
	ProfileClassDaily    = "daily"    // 日用百货：90 天
	ProfileClassGeneral  = "general"  // 其它：45 天
)

// ProductProfile 是一个 SKU 在某个门店的经营属性。
//
// ⚠️ 唯一键 (branch_id, product_id)：同一个 SKU 在不同门店的动销档位不同。
type ProductProfile struct {
	ID        string `gorm:"primaryKey;size:36" json:"id"`
	BranchID  string `gorm:"size:32;uniqueIndex:ux_product_profile,priority:1" json:"branch_id"`
	ProductID string `gorm:"size:64;uniqueIndex:ux_product_profile,priority:2" json:"product_id"`
	// ProductName 是冗余快照（纯展示；商品改名后不会自动跟）。
	ProductName string `gorm:"size:128" json:"product_name"`
	// Class 是品类档位（ProfileClass*），决定 CapDays 的默认值。
	Class string `gorm:"size:16" json:"class"`
	// CapDays 是该商品允许的**可售天数上限**（进货后库存可卖多少天）。
	CapDays int `json:"cap_days"`
	// ShelfLifeDays 是保质期天数（0 = 未标记；自动标记有值后可人工修正）。
	ShelfLifeDays int `json:"shelf_life_days"`
	// Source 是 ProfileSourceAuto / Pending / Manual。
	Source string `gorm:"size:16;index" json:"source"`
	// Note / OperatorID 是人工修正留痕。
	Note       string    `gorm:"size:256" json:"note"`
	OperatorID string    `gorm:"size:64" json:"operator_id"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (ProductProfile) TableName() string { return "product_profiles" }

// StockingDisplay 是一个**商品组**的正常陈列保有量（2026-11-02 用户口径）。
//
// # 它解决什么
//
//	货架上的陈列量不是"可售库存"：除非卖空，否则这批货要一直摆在架上。
//	只按「库存 / 日均销量」算可售天数，会把正常陈列当成压货
//	（生产实测毛笔库存 19 支、日均 0.09 支 ⇒ 判 415 天）。
//
// ⇒ 判超量前先把陈列保有量从可售量里扣掉：
//
//	可售量 = 补货后库存 − 陈列保有量
//
// # 怎么生成
//
//	DisplayQty = P50(近 180 天单批净进货量)  ← 大约等于"一次正常铺货的量"
//	无历史进货 ⇒ 1。
//
// ⚠️ 自动生成只在审批触发时刷新 Source=auto 的行；
//
//	人工在后台改过（Source=manual）的行**永不覆盖**，且与日均/上限天数的
//	180 天学习完全独立 —— 改陈列量不会影响可售天数算法本身。
//
// ⚠️ 有陈列费/堆头费时，运行时把 DisplayQty ×2（见 service/stocking_group.go），
//
//	存的是**未乘倍数的基数**，这样人工修正与放宽倍数互不污染。
type StockingDisplay struct {
	ID       string `gorm:"primaryKey;size:36" json:"id"`
	BranchID string `gorm:"size:32;uniqueIndex:ux_stocking_display,priority:1" json:"branch_id"`
	GroupKey string `gorm:"size:160;uniqueIndex:ux_stocking_display,priority:2" json:"group_key"`
	// ProductID 是组内代表货号（纯展示/检索用）。
	ProductID string `gorm:"size:64;index" json:"product_id"`
	// ProductName 是代表货号的名称快照（纯展示）。
	ProductName string `gorm:"size:128" json:"product_name"`
	// DisplayQty 是正常陈列保有量（**组基准单位**，未乘陈列费倍数）。
	DisplayQty decimal.Decimal `gorm:"type:numeric(14,3)" json:"display_qty"`
	// SampleDays / ReceiptCount 是算出陈列量所用的历史窗口与进货次数。
	SampleDays   int `json:"sample_days"`
	ReceiptCount int `json:"receipt_count"`
	// Source 是 ProfileSourceAuto / Pending / Manual。
	Source string `gorm:"size:16;index" json:"source"`
	Note   string `gorm:"size:256" json:"note"`
	// OperatorID 是人工修正留痕。
	OperatorID string    `gorm:"size:64" json:"operator_id"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (StockingDisplay) TableName() string { return "stocking_displays" }

// StockingFactCache 是单个商品的**近 180 天历史聚合**缓存（按商品，不按组）。
//
// # 为什么要缓存
//
// 一次审批可能涉及几百个 SKU，每个都要算「180 天销量 / 最后销售日 /
// 进货次数与单次进货量」。这些是**缓变数据**，同一批 SKU 一天内可能被
// 多张审批单命中（2026-11-01 一天就 44 张）⇒ 重复打源库没有必要。
//
// TTL 12 小时：跨半天再取一次，既不陈旧到影响判断，也不会把源库打爆。
// ⚠️ 按**商品**缓存而不是按组：组的成员会随组合关系变化，缓存组会把
// 变化藏起来。
type StockingFactCache struct {
	ID        string `gorm:"primaryKey;size:36" json:"id"`
	BranchID  string `gorm:"size:32;uniqueIndex:ux_stocking_cache,priority:1" json:"branch_id"`
	ProductID string `gorm:"size:64;uniqueIndex:ux_stocking_cache,priority:2" json:"product_id"`
	// WindowFrom 是历史窗口起点（YYYY-MM-DD，含）。窗口终点固定是取数当天。
	WindowFrom string `gorm:"size:10;uniqueIndex:ux_stocking_cache,priority:3" json:"window_from"`
	// Payload 是 ProductHistory 的 JSON（见 service/stocking_group.go）。
	Payload string `gorm:"type:text" json:"payload"`
	// ComputedAt / ExpiresAt 决定缓存是否还有效。
	ComputedAt time.Time `gorm:"index" json:"computed_at"`
	ExpiresAt  time.Time `gorm:"index" json:"expires_at"`
}

func (StockingFactCache) TableName() string { return "stocking_fact_cache" }
