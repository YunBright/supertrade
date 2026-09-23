// Package model 定义 stocktake 服务用到的所有领域类型。
//
// 本包包含三类对象:
//   - GORM 模型:stocktake_headers / stocktake_lines(本系统 PG 建表)
//   - 差异表响应 DTO:DiffReport / Summary / ReasonAgg(非 GORM,实时生成不落库)
//
// cube 响应 DTO(ProductDTO / StockSnapshotDTO / SupplierDTO / ProductWithStock)
// 已迁到 internal/cubeclient(共享给 stocktake / catalog / inventory / master-data),
// 见 internal/cubeclient/dto.go。
//
// 设计要点(REQUIREMENTS §2.1 / §7.5.0):
//   - cube product.id 即思迅 item_no(string),本系统不建 product 表,
//     但 StocktakeLine.ProductID 必须与其类型 / 取值范围完全一致;
//     Go struct 字段作为类型契约,保证编译期一致。
//   - book_qty / book_qty_at 是该行录入时刻的 cube 库存快照,不同行快照时间不同。
package model

import (
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// StocktakeStatus 盘点单状态。
//
// 状态机:Counting → Adjusted → Approved(REQUIREMENTS §2.1.5)。
// Counting/Adjusted 可回退;Approved 不可改。
type StocktakeStatus string

const (
	StatusCounting StocktakeStatus = "counting" // 录入中,可改明细
	StatusAdjusted StocktakeStatus = "adjusted" // 已提交,冻结差异快照
	StatusApproved StocktakeStatus = "approved" // 已审核,不可改
)

// StocktakeType 盘点类型。
type StocktakeType string

const (
	TypeGeneral StocktakeType = "general" // 普通全盘
	TypeProduce StocktakeType = "produce" // 蔬果(联动 fresh-produce)
	TypePlan    StocktakeType = "plan"    // 计划盘点单(高权用户预选清单)
	TypeRecheck StocktakeType = "recheck" // 复盘点单(每次写入自动填"复盘"备注)
)

// DiffReason 差异原因。
type DiffReason string

const (
	ReasonLoss      DiffReason = "loss"       // 损耗
	ReasonOverage   DiffReason = "overage"    // 盘盈
	ReasonDamage    DiffReason = "damage"     // 破损
	ReasonWrongUnit DiffReason = "wrong_unit" // 单位错
	ReasonOther     DiffReason = "other"      // 其它
)

// StocktakeHeader 盘点单据头(本系统 PG 建表)。
//
// 主键格式:`ST<yyyymmdd><seq>`(例:ST20260917001),由 service 在 CreateHeader 时生成。
// BranchID 整单锁定,创建后不可改;Status 严格走状态机。
//
// ParentHeaderID:复盘点单(recheck)指向其原始盘点单,可为空;plan 不用此字段。
type StocktakeHeader struct {
	ID                  string          `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID            string          `gorm:"column:branch_id;type:varchar(64);not null;index" json:"branch_id"`
	CountDate           time.Time       `gorm:"column:count_date;type:date;not null" json:"count_date"`
	Status              StocktakeStatus `gorm:"column:status;type:varchar(16);not null;index" json:"status"`
	Type                StocktakeType   `gorm:"column:type;type:varchar(16);not null" json:"type"`
	OperatorID          string          `gorm:"column:operator_id;type:varchar(64);not null" json:"operator_id"`
	AuditorID           string          `gorm:"column:auditor_id;type:varchar(64)" json:"auditor_id,omitempty"`
	ParentHeaderID      *string         `gorm:"column:parent_header_id;type:varchar(64);index" json:"parent_header_id,omitempty"`
	TotalDiffQty        decimal.Decimal `gorm:"column:total_diff_qty;type:decimal(20,4);default:0" json:"total_diff_qty"`
	TotalDiffAmountYuan decimal.Decimal `gorm:"column:total_diff_amount_yuan;type:decimal(20,4);default:0" json:"total_diff_amount_yuan"`
	Remark              string          `gorm:"column:remark;type:text" json:"remark,omitempty"`
	CreatedAt           time.Time       `gorm:"column:created_at;type:timestamptz;not null" json:"created_at"`
	UpdatedAt           time.Time       `gorm:"column:updated_at;type:timestamptz;not null" json:"updated_at"`
	Lines               []StocktakeLine `gorm:"foreignKey:HeaderID;references:ID;constraint:OnDelete:CASCADE" json:"lines,omitempty"`
}

// TableName 显式表名(避免 GORM 默认复数推断)。
func (StocktakeHeader) TableName() string { return "stocktake_headers" }

// StocktakeLine 盘点单据行(本系统 PG 建表)。
//
// 每行录入时:
//   - ProductID 由营业员扫码 / 输入获得(从 cube product 取,需校验存在)
//   - BookQty / BookQtyAt 是录入该行时刻从 cube stock 拉的快照
//   - DiffQty = ActualQty - BookQty(录入时算,改 actual 时重算)
//
// 不允许单独改 BookQty(快照在录入瞬间固定);改 actual 必须重算 diff。
type StocktakeLine struct {
	ID             string          `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	HeaderID       string          `gorm:"column:header_id;type:varchar(64);not null;index" json:"header_id"`
	ProductID      string          `gorm:"column:product_id;type:varchar(64);not null" json:"product_id"`
	ProductName    string          `gorm:"column:product_name;type:varchar(255);not null" json:"product_name"`
	Unit           string          `gorm:"column:unit;type:varchar(32)" json:"unit,omitempty"`
	Spec           string          `gorm:"column:spec;type:varchar(255)" json:"spec,omitempty"`
	BookQty        decimal.Decimal `gorm:"column:book_qty;type:decimal(20,4);not null" json:"book_qty"`
	BookQtyAt      time.Time       `gorm:"column:book_qty_at;type:timestamptz;not null" json:"book_qty_at"`
	ActualQty      decimal.Decimal `gorm:"column:actual_qty;type:decimal(20,4);not null" json:"actual_qty"`
	DiffQty        decimal.Decimal `gorm:"column:diff_qty;type:decimal(20,4);not null" json:"diff_qty"`
	AvgCostYuan    decimal.Decimal `gorm:"column:avg_cost_yuan;type:decimal(20,4);not null" json:"avg_cost_yuan"`
	DiffAmountYuan decimal.Decimal `gorm:"column:diff_amount_yuan;type:decimal(20,4);not null" json:"diff_amount_yuan"`
	DiffReason     DiffReason      `gorm:"column:diff_reason;type:varchar(32)" json:"diff_reason,omitempty"`
	Remark         *string         `gorm:"column:remark;type:text" json:"remark,omitempty"`
	CreatedAt      time.Time       `gorm:"column:created_at;type:timestamptz;not null" json:"created_at"`
	UpdatedAt      time.Time       `gorm:"column:updated_at;type:timestamptz;not null" json:"updated_at"`
	DeletedAt      gorm.DeletedAt  `gorm:"column:deleted_at;type:timestamptz;index" json:"-"`
}

// TableName 显式表名。
func (StocktakeLine) TableName() string { return "stocktake_lines" }

// ---- 差异表响应 DTO(非 GORM,实时生成) ----

// DiffSummary 差异汇总。
type DiffSummary struct {
	TotalLines          int             `json:"total_lines"`
	TotalDiffQty        decimal.Decimal `json:"total_diff_qty"`
	TotalDiffAmountYuan decimal.Decimal `json:"total_diff_amount_yuan"`
	LossLines           int             `json:"loss_lines"`    // diff_qty < 0
	OverageLines        int             `json:"overage_lines"` // diff_qty > 0
	NoDiffLines         int             `json:"no_diff_lines"` // diff_qty == 0
}

// ReasonAgg 按 diff_reason 聚合。
type ReasonAgg struct {
	Reason     DiffReason      `json:"reason"`
	Lines      int             `json:"lines"`
	Qty        decimal.Decimal `json:"qty"`
	AmountYuan decimal.Decimal `json:"amount_yuan"`
}

// DiffReportLine 单行差异详情(只列有差异的行)。
type DiffReportLine struct {
	ProductID      string          `json:"product_id"`
	ProductName    string          `json:"product_name"`
	BookQty        decimal.Decimal `json:"book_qty"`
	BookQtyAt      time.Time       `json:"book_qty_at"`
	ActualQty      decimal.Decimal `json:"actual_qty"`
	DiffQty        decimal.Decimal `json:"diff_qty"`
	AvgCostYuan    decimal.Decimal `json:"avg_cost_yuan"`
	DiffAmountYuan decimal.Decimal `json:"diff_amount_yuan"`
	DiffReason     DiffReason      `json:"diff_reason,omitempty"`
}

// DiffReport 差异表响应(实时生成,不落库,任意状态可调)。
//
// 字段顺序与 REQUIREMENTS §2.1.3 一致。
type DiffReport struct {
	HeaderID    string           `json:"header_id"`
	BranchID    string           `json:"branch_id"`
	CountDate   time.Time        `json:"count_date"`
	Status      StocktakeStatus  `json:"status"`
	Summary     DiffSummary      `json:"summary"`
	ByReason    []ReasonAgg      `json:"by_reason"`
	Lines       []DiffReportLine `json:"lines"`
	GeneratedAt time.Time        `json:"generated_at"`
}

// ---- 盘点操作历史 (StocktakeLineOperation) ----

// LineOpType 明细行的操作类型(用户态:扫描/录入/覆盖/累加;系统态:删除)。
type LineOpType string

const (
	OpCreate     LineOpType = "create"     // 首次录入
	OpOverwrite  LineOpType = "overwrite"  // 覆盖式修改(直接设 actual)
	OpAccumulate LineOpType = "accumulate" // 累加式修改(actual += delta)
	OpDelete     LineOpType = "delete"     // 删除该行
)

// OpMethod 客户端通道:扫码 / 手动输入 / 导入。
type OpMethod string

const (
	MethodScan   OpMethod = "scan"   // 扫码录入
	MethodManual OpMethod = "manual" // 手动输入
	MethodImport OpMethod = "import" // 批量导入
)

// StocktakeLineOperation 盘点明细的操作历史(append-only 审计日志)。
//
// 每次 AddLine / UpdateLine / DeleteLine 在同一事务内 insert 一行;
// actor_name 在写入时冗余(审计不可改,无须 JOIN 渲染)。
//
// prev_qty / new_qty / qty_delta 单位均为实盘数量;
//   - create:prev_qty=0, new_qty=actual, qty_delta=actual
//   - overwrite:prev_qty=旧 actual, new_qty=新 actual, qty_delta=新-旧
//   - accumulate:prev_qty=旧 actual, new_qty=新 actual, qty_delta=新-旧
//   - delete:prev_qty=旧 actual, new_qty=0, qty_delta=-旧 actual
type StocktakeLineOperation struct {
	ID         string          `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	HeaderID   string          `gorm:"column:header_id;type:varchar(64);not null;index" json:"header_id"`
	LineID     string          `gorm:"column:line_id;type:varchar(64);not null;index" json:"line_id"`
	OpType     LineOpType      `gorm:"column:op_type;type:varchar(16);not null;index" json:"op_type"`
	ActorID    string          `gorm:"column:actor_id;type:varchar(64);not null;index" json:"actor_id"`
	ActorName  string          `gorm:"column:actor_name;type:varchar(128);not null" json:"actor_name"`
	PrevQty    decimal.Decimal `gorm:"column:prev_qty;type:decimal(20,4);not null" json:"prev_qty"`
	NewQty     decimal.Decimal `gorm:"column:new_qty;type:decimal(20,4);not null" json:"new_qty"`
	QtyDelta   decimal.Decimal `gorm:"column:qty_delta;type:decimal(20,4);not null" json:"qty_delta"`
	OpAt       time.Time       `gorm:"column:op_at;type:timestamptz;not null;index" json:"op_at"`
	Method     OpMethod        `gorm:"column:method;type:varchar(16);not null" json:"method"`
	Remark     string          `gorm:"column:remark;type:text" json:"remark,omitempty"`
	CreatedAt  time.Time       `gorm:"column:created_at;type:timestamptz;not null" json:"created_at"`
}

// TableName 显式表名。
func (StocktakeLineOperation) TableName() string { return "stocktake_line_operations" }

// ---- 计划盘点商品 (StocktakePlanItem) ----

// StocktakePlanItem 计划盘点单的预选商品(高权用户预先录入,营业员扫描时提示)。
//
// (header_id, product_id) 上有 uniqueIndex,避免重复加同一商品。
// 排序按 sort_order ASC, null 视为 0。
type StocktakePlanItem struct {
	ID          string    `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	HeaderID    string    `gorm:"column:header_id;type:varchar(64);not null;uniqueIndex:idx_plan_header_product" json:"header_id"`
	ProductID   string    `gorm:"column:product_id;type:varchar(64);not null;uniqueIndex:idx_plan_header_product" json:"product_id"`
	ProductName string    `gorm:"column:product_name;type:varchar(255);not null" json:"product_name"`
	Unit        string    `gorm:"column:unit;type:varchar(32)" json:"unit,omitempty"`
	Barcode     string    `gorm:"column:barcode;type:varchar(64);index" json:"barcode,omitempty"`
	SortOrder   int       `gorm:"column:sort_order;type:int;not null;default:0" json:"sort_order"`
	CreatedAt   time.Time `gorm:"column:created_at;type:timestamptz;not null" json:"created_at"`
}

// TableName 显式表名。
func (StocktakePlanItem) TableName() string { return "stocktake_plan_items" }

// ---- 门店默认盘点单 (StocktakeBranchDefault) ----

// StocktakeBranchDefault 每店当前默认盘点单(由仓管设置,供前端快速进入盘点)。
//
// 一店同时只能有**一个**默认盘点单(单点概念),用 BranchID 作主键,upsert 覆盖。
//
// 业务约束:
//   - 设置时校验 HeaderID 存在 + HeaderID.BranchID == BranchID + Header.Status == counting
//   - 一旦 header 进入 adjusted / approved,前端在拉取时会感知到并提示重新设置
//
// 字段语义:
//   - UpdatedBy: 设置人 user id(冗余字段,便于审计)
//   - UpdatedAt: 最近一次设置时间
type StocktakeBranchDefault struct {
	BranchID  string    `gorm:"primaryKey;column:branch_id;type:varchar(64)" json:"branch_id"`
	HeaderID  string    `gorm:"column:header_id;type:varchar(64);not null;index" json:"header_id"`
	UpdatedBy string    `gorm:"column:updated_by;type:varchar(64);not null" json:"updated_by"`
	UpdatedAt time.Time `gorm:"column:updated_at;type:timestamptz;not null" json:"updated_at"`
}

// TableName 显式表名。
func (StocktakeBranchDefault) TableName() string { return "stocktake_branch_defaults" }
