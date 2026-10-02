// Package model 定义 fresh-meat 服务用到的所有领域类型。
//
// 本包包含:
//   - 6 张 GORM 模型:whole_pigs / pig_cuts / pork_cuts_stocktakes / line_sales_by_pigs /
//     branch_cut_mappings / waste_logs
//   - 输入 DTO:CreateXxxInput / UpdateXxxInput(POST/PUT body 解)
//   - CutSnapshot:盘点行 JSON 元素(per-SKU:含 cube_product_id)
//   - EventData:发到 Dapr pub/sub 的 payload(EventData 与本仓其它服务**不**携带 tenant_id;
//     见 docs/EVENT-CATALOG.md §2.6/§2.8)。
//
// 命名:所有 GORM 表显式 lowercase + 下划线复数(实际 PG 默认 lowercase,本系统 schema 是 fresh_meat)。
//
// 设计要点:
//   - cut_type 枚举 12 类(含 6 类下水):belly / tenderloin / rib / leg_front / leg_rear /
//     trotter / liver / intestine / heart / lung / kidney / stomach
//   - whole_pig 加 3 字段:half_pig / offal_included / purchase_group_id(场景一/二/三区分)
//   - whole_pig.llm_advice_json 是 jsonb,字段实际写入时由 service 在 insert 后调
//     Dapr Conversation API(见 service/llm.go)同步写入;阶段阶段1 留空。
//   - line_sales_by_pig 用于 POS sale.completed 中 fresh_type=meat 行的本地聚合宽表,
//     含 unit_price_yuan 供毛利计算;order_status 区分 S/R(退货 R 行 qty/amount 直存负数)。
//   - waste_logs 表报损持久化(避免 dapr 失败丢数据)。
package model

import (
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/datatypes"
)

// ---- 枚举 ----

// CutType 鲜肉部位。
//
// 与 docs/REQUIREMENTS.md §4.1 / docs/EVENT-CATALOG.md §2.3/§2.8 一致。
type CutType string

const (
	CutBelly      CutType = "belly"      // 五花
	CutTenderloin CutType = "tenderloin" // 里脊
	CutRib        CutType = "rib"        // 排骨
	CutLegFront   CutType = "leg_front"  // 前腿
	CutLegRear    CutType = "leg_rear"   // 后腿
	CutTrotter    CutType = "trotter"    // 猪蹄
	CutLiver      CutType = "liver"      // 猪肝(下水 1/6)

	// 下水(场景二"半头猪带全猪下水"用):
	CutIntestine CutType = "intestine" // 大肠
	CutHeart     CutType = "heart"     // 心
	CutLung      CutType = "lung"      // 肺
	CutKidney    CutType = "kidney"    // 肾
	CutStomach   CutType = "stomach"   // 肚(猪肚)
)

// AllCutTypes 全部 cut 枚举集合(给 RecordPigCut / stocktake 拒绝未知值)。
var AllCutTypes = []CutType{
	CutBelly, CutTenderloin, CutRib, CutLegFront, CutLegRear,
	CutTrotter, CutLiver, CutIntestine, CutHeart, CutLung, CutKidney, CutStomach,
}

// Valid 判断 cut 是否在 AllCutTypes 集合内。
func (c CutType) Valid() bool {
	for _, v := range AllCutTypes {
		if v == c {
			return true
		}
	}
	return false
}

// DataSource 标识 whole_pig.llm_advice_json 的来源(BI 标注 / sales-agg 推算依据)。
type DataSource string

const (
	DataSourceLLM        DataSource = "llm"         // 由 Dapr Conversation API 同步返回
	DataSourceHistoryAvg DataSource = "history_avg" // LLM 失败/超时降级:近 30 天 ±10% 重量段均值
	DataSourceStub       DataSource = "stub"        // 阶段1 骨架用;阶段3 起 LLM 真接后不再出现
)

// ---- 表1:whole_pig ----

// WholePig 整猪录入(早盘,一头一行)。
//
// 主键格式:`WP<yyyymmdd><8-hex random>`,由 service.RecordWholePig 生成。
// LLMAdviceJSON 同步调 Dapr Conversation API(阶段3)后写入;阶段1 留 NULL + DataSourceStub。
// BranchID 录入后不可改;ArrivedAt 索引 (branch_id, arrived_at) 便于按门店按日查。
type WholePig struct {
	ID                    string          `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	EarTag                string          `gorm:"column:ear_tag;type:varchar(64);not null;index" json:"ear_tag"`
	GrossWeightKg         decimal.Decimal `gorm:"column:gross_weight_kg;type:decimal(10,3);not null" json:"gross_weight_kg"`
	PurchaseUnitPriceYuan decimal.Decimal `gorm:"column:purchase_unit_price_yuan;type:decimal(10,2);not null" json:"purchase_unit_price_yuan"`
	PurchaseCostYuan      decimal.Decimal `gorm:"column:purchase_cost_yuan;type:decimal(12,2);not null" json:"purchase_cost_yuan"`
	SupplierID            string          `gorm:"column:supplier_id;type:varchar(64);not null" json:"supplier_id"`
	BranchID              string          `gorm:"column:branch_id;type:varchar(64);not null;index:idx_branch_arrived,priority:1" json:"branch_id"`
	ArrivedAt             time.Time       `gorm:"column:arrived_at;not null;index:idx_branch_arrived,priority:2" json:"arrived_at"`
	RecordedBy            string          `gorm:"column:recorded_by;type:varchar(64);not null" json:"recorded_by"`
	LLMAdviceJSON         datatypes.JSON  `gorm:"column:llm_advice_json;type:jsonb" json:"llm_advice_json,omitempty"`
	DataSource            DataSource      `gorm:"column:data_source;type:varchar(16);not null;default:'stub'" json:"data_source"`
	// 场景一/二/三区分:
	HalfPig         bool   `gorm:"column:half_pig;not null;default:false" json:"half_pig"`                            // 半头猪
	OffalIncluded   bool   `gorm:"column:offal_included;not null;default:false" json:"offal_included"`                // 带全猪下水
	PurchaseGroupID string `gorm:"column:purchase_group_id;type:varchar(64);index:idx_branch_group,priority:2" json:"purchase_group_id,omitempty"` // 场景三 2 头同 group
	CreatedAt       time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt       time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式表名。
func (WholePig) TableName() string { return "whole_pigs" }

// ---- 表2:pig_cuts ----

// PigCut 单品补录(部分追加条码;一头猪一个 cut 一行)。
//
// (pig_id, cut_type) 唯一:同一头猪的同一部位不重复补录(若同部位多次切分,合并为多行;
// 现阶段按物理唯一)。
//
// BranchID 冗余 whole_pig.branch_id,避免每次 join;输入由 service.RecordPigCut
// 从 pig 拷贝,不可由 caller 改(防跨店)。
type PigCut struct {
	ID        string          `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	PigID     string          `gorm:"column:pig_id;type:varchar(64);not null;index" json:"pig_id"`
	CutType   CutType         `gorm:"column:cut_type;type:varchar(32);not null;uniqueIndex:idx_pig_cut,priority:2" json:"cut_type"`
	BranchID  string          `gorm:"column:branch_id;type:varchar(64);not null;index" json:"branch_id"`
	Barcode   string          `gorm:"column:barcode;type:varchar(64);not null;index" json:"barcode"`
	WeightKg  decimal.Decimal `gorm:"column:weight_kg;type:decimal(10,3);not null" json:"weight_kg"`
	ExpiresAt time.Time       `gorm:"column:expires_at;not null" json:"expires_at"`
	CreatedAt time.Time       `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName 显式表名。
func (PigCut) TableName() string { return "pig_cuts" }

// ---- 表3:pork_cuts_stocktake ----

// PorkCutsStocktake 日终盘点(整店按 cut_type;**可选**,缺失不阻断销售)。
//
// 粒度:branch_id + taken_at(日);不是一头猪一行,而是按 cut_type 一行。
// IsComplete 决定 BI 标注(✓ 已盘点 / ⚠ 未盘点 数据为推演)。
// Cuts JSONB 数组:CutSnapshot;LLMReviewJSON 由阶段4 日终反推后写入。
type PorkCutsStocktake struct {
	ID            string         `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID      string         `gorm:"column:branch_id;type:varchar(64);not null;index:idx_branch_taken,priority:1" json:"branch_id"`
	TakenAt       time.Time      `gorm:"column:taken_at;not null;index:idx_branch_taken,priority:2" json:"taken_at"`
	TakenBy       string         `gorm:"column:taken_by;type:varchar(64);not null" json:"taken_by"`
	IsComplete    bool           `gorm:"column:is_complete;not null" json:"is_complete"`
	Cuts          datatypes.JSON `gorm:"column:cuts;type:jsonb;not null" json:"cuts"`
	LLMReviewJSON datatypes.JSON `gorm:"column:llm_review_json;type:jsonb" json:"llm_review_json,omitempty"`
	CreatedAt     time.Time      `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName 显式表名。
func (PorkCutsStocktake) TableName() string { return "pork_cuts_stocktakes" }

// CutSnapshot 盘点单 cuts JSON 数组元素(per-SKU:每 tray/hook 一行)。
//
// 仓管"精确到具体 SKU 和数量"要求按 cube_product_id 维度;同 cut_type
// 下不同 SKU(如 五花肉 / 梅肉 / 五花片)各一条记录。
type CutSnapshot struct {
	CutType          CutType         `json:"cut_type"`
	CubeProductID    string          `json:"cube_product_id"`                  // 必填;由 (branch, cut_type) 在 branch_cut_mapping 校验
	ActualRemainKg   decimal.Decimal `json:"actual_remain_kg"`
	ExpectedRemainKg decimal.Decimal `json:"expected_remain_kg"`
	VarianceKg       decimal.Decimal `json:"variance_kg"`
}

// ---- 表4:line_sales_by_pig ----

// LineSalesByPig POS 销售事件中 fresh_type=meat 行的本地聚合宽表(FIELD_SPEC §1.6 未列,
// DESIGN.md:577 明确要求)。
//
// UNIQUE(branch_id, pos_line_id) — 同一 pos_line 二次投递 idempotent (do nothing)。
// SaleDate 是 datatypes.Date(只存日期部分),不存时分秒,与 BI 按日聚合对齐。
// CubeProductID 是冗余(sale.completed 可能不含 cube product id;从 branch_cut_mapping 补) ,
// 阶段2 实现时由 OnSaleCompleted 写入。
// UnitPriceYuan 从 sale.completed.unit_price_yuan 直接拷贝,供后续 per-SKU 单价 / 毛利计算。
// OrderStatus "S"=销售 "R"=退货;POS 已自翻符号(负 qty/amount);毛利计算时 R 行不进 revenue。
type LineSalesByPig struct {
	ID           uint64          `gorm:"primaryKey;column:id;autoIncrement" json:"id"`
	BranchID      string         `gorm:"column:branch_id;type:varchar(64);not null;index:idx_branch_date,priority:1" json:"branch_id"`
	SaleDate      datatypes.Date `gorm:"column:sale_date;not null;index:idx_branch_date,priority:2" json:"sale_date"`
	PigID         string         `gorm:"column:pig_id;type:varchar(64);not null;index" json:"pig_id"`
	CutType       CutType        `gorm:"column:cut_type;type:varchar(32);not null" json:"cut_type"`
	QtyKg         decimal.Decimal `gorm:"column:qty_kg;type:decimal(12,3);not null" json:"qty_kg"`
	AmountYuan    decimal.Decimal `gorm:"column:amount_yuan;type:decimal(14,2);not null" json:"amount_yuan"`
	UnitPriceYuan decimal.Decimal `gorm:"column:unit_price_yuan;type:decimal(10,2);not null;default:0" json:"unit_price_yuan"`
	Source        string          `gorm:"column:source;type:varchar(32);not null;default:'pos'" json:"source"` // pos | erp
	PosLineID     string          `gorm:"column:pos_line_id;type:varchar(64);uniqueIndex:idx_pos_line_branch,priority:1" json:"pos_line_id"`
	PosBranchID   string          `gorm:"column:pos_branch_id;type:varchar(64);uniqueIndex:idx_pos_line_branch,priority:2" json:"pos_branch_id"`
	CubeProductID string          `gorm:"column:cube_product_id;type:varchar(64);index" json:"cube_product_id,omitempty"`
	OrderStatus   string          `gorm:"column:order_status;type:varchar(2);not null;default:'S'" json:"order_status"` // "S" | "R"
	CreatedAt     time.Time       `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName 显式表名。
func (LineSalesByPig) TableName() string { return "line_sales_by_pigs" }

// ---- 表5:branch_cut_mapping ----

// BranchCutMapping 门店 cut 部位 ↔ cube product 对应表。
//
// UNIQUE(branch_id, cut_type) — 一店一部位只有一个 cube product id。
// CubeProductName 冗余缓存 cube.GetProduct 的 name,避免每次 join cubeclient(高频读)。
//
// 初始化:拓店 / 改品时由仓管 admin 通过 POST/PUT /branch-cut-mappings 维护。
type BranchCutMapping struct {
	ID              string    `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID        string    `gorm:"column:branch_id;type:varchar(64);not null;uniqueIndex:idx_branch_cut,priority:1" json:"branch_id"`
	CutType         CutType   `gorm:"column:cut_type;type:varchar(32);not null;uniqueIndex:idx_branch_cut,priority:2" json:"cut_type"`
	CubeProductID   string    `gorm:"column:cube_product_id;type:varchar(64);not null" json:"cube_product_id"`
	CubeProductName string    `gorm:"column:cube_product_name;type:varchar(255)" json:"cube_product_name,omitempty"`
	CreatedAt       time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt       time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式表名。
func (BranchCutMapping) TableName() string { return "branch_cut_mappings" }

// ---- 表6:waste_logs ----

// WasteLog 报损日志(实际持久化,避免 dapr 失败丢数据)。
//
// ID 格式:`WL<yyyymmdd><8-hex>`(同 stocktake / fresh-meat 其它表)。
// (PigID, CutType) 可空:报损可能是批量处理(如批次过期)、不是单 SKU。
// CubeProductID 可空;若填,直接记实际报废的 cube SKU。
// RecordedAt 由 caller 传(saleDate-like);若空,service 用 ttl=now。
type WasteLog struct {
	ID            string          `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID      string          `gorm:"column:branch_id;type:varchar(64);not null;index:idx_branch_recorded,priority:1" json:"branch_id"`
	PigID         string          `gorm:"column:pig_id;type:varchar(64);index" json:"pig_id,omitempty"`
	CutType       CutType         `gorm:"column:cut_type;type:varchar(32)" json:"cut_type,omitempty"`
	CubeProductID string          `gorm:"column:cube_product_id;type:varchar(64)" json:"cube_product_id,omitempty"`
	QtyKg         decimal.Decimal `gorm:"column:qty_kg;type:decimal(10,3);not null" json:"qty_kg"`
	Reason        string          `gorm:"column:reason;type:varchar(255);not null" json:"reason"`
	RecordedAt    time.Time       `gorm:"column:recorded_at;not null;index:idx_branch_recorded,priority:2" json:"recorded_at"`
	OperatorID    string          `gorm:"column:operator_id;type:varchar(64);not null" json:"operator_id"`
	CreatedAt     time.Time       `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName 显式表名。
func (WasteLog) TableName() string { return "waste_logs" }

// ---- 输入 DTO ----

// RecordWholePigInput POST /whole-pigs 入参。
//
// 场景区分(用户决策 2026-10-01):
//   - 半头猪:        half_pig=true,offal_included=false
//   - 半头带下水:     half_pig=true,offal_included=true
//   - 1 头整猪:      half_pig=false,offal_included=false (默认)
//   - 2 头整猪同批:  两次调用,purchase_group_id 同值
type RecordWholePigInput struct {
	EarTag                string          `json:"ear_tag" binding:"required"`
	GrossWeightKg         decimal.Decimal `json:"gross_weight_kg" binding:"required"`
	PurchaseUnitPriceYuan decimal.Decimal `json:"purchase_unit_price_yuan" binding:"required"`
	SupplierID            string          `json:"supplier_id" binding:"required"`
	ArrivedAt             time.Time       `json:"arrived_at" binding:"required"`
	HalfPig               bool            `json:"half_pig"`
	OffalIncluded         bool            `json:"offal_included"`
	PurchaseGroupID       string          `json:"purchase_group_id"`
}

// RecordPigCutInput POST /pig-cuts 入参。
type RecordPigCutInput struct {
	PigID     string          `json:"pig_id" binding:"required"`
	CutType   CutType         `json:"cut_type" binding:"required"`
	Barcode   string          `json:"barcode" binding:"required"`
	WeightKg  decimal.Decimal `json:"weight_kg" binding:"required"`
	ExpiresAt time.Time       `json:"expires_at" binding:"required"`
}

// CutInput POST /pork-cuts-stocktake 入参的 cuts 数组元素(per-SKU 一行)。
type CutInput struct {
	CutType        CutType         `json:"cut_type" binding:"required"`
	CubeProductID  string          `json:"cube_product_id" binding:"required"` // 必填;后端校验在 branch_cut_mapping 中存在
	ActualRemainKg decimal.Decimal `json:"actual_remain_kg" binding:"required"`
}

// RecordPorkCutsStocktakeInput POST /pork-cuts-stocktake 入参。
type RecordPorkCutsStocktakeInput struct {
	IsComplete bool      `json:"is_complete"`
	Cuts      []CutInput `json:"cuts" binding:"required"`
}

// RecordWasteLogInput POST /waste-logs 入参。
type RecordWasteLogInput struct {
	PigID         string          `json:"pig_id"`
	CutType       CutType         `json:"cut_type"`
	CubeProductID string          `json:"cube_product_id"` // 可选;实际报废的 cube SKU
	QtyKg         decimal.Decimal `json:"qty_kg" binding:"required"`
	Reason        string          `json:"reason" binding:"required"`
	RecordedAt    time.Time       `json:"recorded_at"` // 可选;空则用 service.now()
}

// CreateBranchCutMappingInput POST /branch-cut-mappings 入参。
type CreateBranchCutMappingInput struct {
	CutType       CutType `json:"cut_type" binding:"required"`
	CubeProductID  string  `json:"cube_product_id" binding:"required"`
}

// UpdateBranchCutMappingInput PUT /branch-cut-mappings/:id 入参。
type UpdateBranchCutMappingInput struct {
	CutType       CutType `json:"cut_type"`
	CubeProductID string  `json:"cube_product_id"`
}

// ---- 事件 payload(无 tenant_id) ----

// PorkCutsStocktakenEventData 发送给 sales-agg / BI 的盘点完成事件(EVENT-CATALOG §2.8)。
//
// **不携带 tenant_id**(本仓不持久化 tenant;见 service/events.go)。
// OpeningKg / WasteKg 是新键(优化 2026-10-01):key=cut_type,value=decimal.Decimal;下游若不读可忽略。
type PorkCutsStocktakenEventData struct {
	StocktakeID string        `json:"stocktake_id"`
	BranchID    string        `json:"branch_id"`
	IsComplete  bool          `json:"is_complete"`
	Cuts        []CutSnapshot `json:"cuts"`
	OpeningKg   map[string]decimal.Decimal `json:"opening_kg,omitempty"`
	WasteKg     map[string]decimal.Decimal `json:"waste_kg,omitempty"`
	LLMReviewed bool          `json:"llm_reviewed"`
	DataSource  string        `json:"data_source"` // actual | llm_reviewed | history_avg
	CompletedAt time.Time     `json:"completed_at"`
	OperatorID  string        `json:"operator_id"`
}

// WasteLogRecordedEventData 报损事件(EVENT-CATALOG §2.6)。
//
// **不携带 tenant_id**。
type WasteLogRecordedEventData struct {
	LogID         string          `json:"log_id"`
	BranchID      string          `json:"branch_id"`
	PigID         string          `json:"pig_id,omitempty"`
	CutType       CutType         `json:"cut_type,omitempty"`
	CubeProductID string          `json:"cube_product_id,omitempty"`
	QtyKg         decimal.Decimal `json:"qty_kg"`
	Reason        string          `json:"reason"`
	RecordedAt    time.Time       `json:"recorded_at"`
	OperatorID    string          `json:"operator_id"`
}