// Package model —— master-data 域的本地表。
//
// # 域边界
//
// master-data 拥有**业务属性**层的三样东西:
//
//	stores    门店档案(业务属性;门店**定义本体**在 auth.branches)
//	employees 收银员业务档案(身份与令牌在 auth,这里是业务属性)
//	shifts    班次(开班 / 交班)
//
// # 为什么"门店定义"有两份
//
// ⚠️ **门店定义的真源是 auth.branches**(tradewind_app 的「系统-门店」页
// 读写的就是 auth 的 8 个 admin/branches 端点)。
//
// 而 master_data.stores 是**业务属性镜像**:盘点周期、是否启用会员、
// 收银台编号这些"门店自己的经营参数"在 auth 里没有地方放。
//
// ⇒ 身份/开关在 auth,经营参数在 master_data。两者**不重复**。
package model

import (
	"errors"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// Store 是门店业务档案。
//
// PK = branch_id(与 auth.branches.id 同值)。
type Store struct {
	// BranchID 是门店 id(与 auth.branches.id 一致)。
	BranchID string `gorm:"primaryKey;column:branch_id;type:varchar(64)" json:"branch_id"`
	// Code 是门店业务码(可空)。
	Code string `gorm:"column:code;type:varchar(32)" json:"code"`
	// Name 是门店名。
	Name string `gorm:"column:name;type:varchar(80);not null" json:"name"`
	// PosEnabled 是"是否启用云商POS"的**镜像**。
	//
	// ⚠️ 真源在 auth.branches.pos_enabled(见 D1 决策);
	//   这里冗余一份,是为了让 POS 一次拉齐门店配置(少一次跨服务调用)。
	//   ⇒ **以 auth 为准**,本字段只作缓存/展示。
	PosEnabled bool `gorm:"column:pos_enabled;type:boolean;not null;default:false" json:"pos_enabled"`
	// StocktakeCycleDays 是盘点周期(天)。
	StocktakeCycleDays int `gorm:"column:stocktake_cycle_days;type:integer;not null;default:7" json:"stocktake_cycle_days"`
	// MemberEnabled 是门店是否启用会员(业务开关,与 auth 的 pos_enabled 正交)。
	MemberEnabled bool `gorm:"column:member_enabled;type:boolean;not null;default:false" json:"member_enabled"`
	// CounterNos 是该门店的收银台编号列表(逗号分隔)。
	//
	// ⚠️ 逗号分隔而不是子表:收银台数量是个位数,且**只在这里读**。
	//   为它建子表会让"门店有多少个收银台"这个唯一的问题多一次 join。
	CounterNos string `gorm:"column:counter_nos;type:varchar(255)" json:"counter_nos"`
	// Remark 是备注。
	Remark    string         `gorm:"column:remark;type:varchar(255)" json:"remark"`
	CreatedBy string         `gorm:"column:created_by;type:varchar(64);not null" json:"created_by"`
	UpdatedBy string         `gorm:"column:updated_by;type:varchar(64)" json:"updated_by"`
	CreatedAt time.Time      `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time      `gorm:"column:updated_at;not null" json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"column:deleted_at;index" json:"deleted_at"`
}

// TableName 显式指定表名。
func (Store) TableName() string { return "stores" }

// Employee 是收银员业务档案。
//
// ⚠️ **ID = auth.users.id**(不是工号)。
//
//	工号(EmployeeCode)是**业务标识**,会因人事变动而改;
//	而 auth user_id 是**身份主键**,永不变。
//	⇒ 用 user_id 做主键 ⇒ 改工号不会丢档案。
type Employee struct {
	// ID 是 auth.users.id。
	ID string `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	// BranchID 是门店。
	BranchID string `gorm:"primaryKey;column:branch_id;type:varchar(64)" json:"branch_id"`
	// EmployeeCode 是工号(= auth.users.employee_code)。
	//
	// ⚠️ **在门店内唯一**,不是全局唯一 —— 不同门店可以有同名工号。
	EmployeeCode string `gorm:"column:employee_code;type:varchar(32);not null;index:idx_employee_branch_code" json:"employee_code"`
	// Name 是姓名。
	Name string `gorm:"column:name;type:varchar(80);not null" json:"name"`
	// Role 是角色:cashier(收银员)/ supervisor(店长助理)/ manager(店长)。
	Role string `gorm:"column:role;type:varchar(32);not null" json:"role"`
	// Status 是 active / inactive。
	Status string `gorm:"column:status;type:varchar(16);not null;default:'active'" json:"status"`
	// DiscountRate 是**折扣权限**(思迅 `oper.output_rate`)。
	//
	// ⚠️ 语义是"**能打几折**"(0.95 = 95 折),不是"能减多少"。
	//   越权改价(图4/图5)时用它判定:打 8 折需要 supervisor 及以上。
	DiscountRate decimal.Decimal `gorm:"column:discount_rate;type:numeric(9,4);not null;default:1" json:"discount_rate"`
	// CounterNo 是默认收银台号(空 = 不限)。
	CounterNo string `gorm:"column:counter_no;type:varchar(16)" json:"counter_no"`
	// Remark 是备注。
	Remark    string         `gorm:"column:remark;type:varchar(255)" json:"remark"`
	CreatedBy string         `gorm:"column:created_by;type:varchar(64);not null" json:"created_by"`
	UpdatedBy string         `gorm:"column:updated_by;type:varchar(64)" json:"updated_by"`
	CreatedAt time.Time      `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time      `gorm:"column:updated_at;not null" json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"column:deleted_at;index" json:"deleted_at"`
}

// TableName 显式指定表名。
func (Employee) TableName() string { return "employees" }

// Shift 是班次(开班 / 交班)。
//
// PK = id(SH<yyyymmdd><seq>)。
type Shift struct {
	// ID 是班次 id。
	ID string `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	// BranchID 是门店。
	BranchID string `gorm:"column:branch_id;type:varchar(64);not null;index:idx_shift_branch_opened" json:"branch_id"`
	// EmployeeID 是开班的收银员(auth user_id)。
	EmployeeID string `gorm:"column:employee_id;type:varchar(64);not null" json:"employee_id"`
	// CounterNo 是收银台号。
	CounterNo string `gorm:"column:counter_no;type:varchar(16)" json:"counter_no"`
	// OpenedAt 是开班时刻。
	OpenedAt time.Time `gorm:"column:opened_at;not null;index:idx_shift_branch_opened" json:"opened_at"`
	// ClosedAt 是交班时刻(nil = 未交班)。
	ClosedAt *time.Time `gorm:"column:closed_at" json:"closed_at,omitempty"`
	// Status 是 open / closed。
	Status string `gorm:"column:status;type:varchar(16);not null;index:idx_shift_branch_opened" json:"status"`
	// OpeningAmountYuan 是开班**备用金**(收银员带的现金)。
	OpeningAmountYuan decimal.Decimal `gorm:"column:opening_amount_yuan;type:numeric(18,4);not null;default:0" json:"opening_amount_yuan"`
	// ExpectedAmountYuan 是应收合计(系统算出来的)。
	//
	// ⚠️ 它是**交班对账的基准**:实收 - 备用金 ≠ 应收 ⇒ 长短款。
	ExpectedAmountYuan decimal.Decimal `gorm:"column:expected_amount_yuan;type:numeric(18,4);not null;default:0" json:"expected_amount_yuan"`
	// ClosingAmountYuan 是交班**实收**(含备用金)。
	ClosingAmountYuan decimal.Decimal `gorm:"column:closing_amount_yuan;type:numeric(18,4);not null;default:0" json:"closing_amount_yuan"`
	// Remark 是备注(常记"长短款原因")。
	Remark    string         `gorm:"column:remark;type:varchar(255)" json:"remark"`
	CreatedBy string         `gorm:"column:created_by;type:varchar(64);not null" json:"created_by"`
	UpdatedBy string         `gorm:"column:updated_by;type:varchar(64)" json:"updated_by"`
	CreatedAt time.Time      `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time      `gorm:"column:updated_at;not null" json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"column:deleted_at;index" json:"deleted_at"`
}

// TableName 显式指定表名。
func (Shift) TableName() string { return "shifts" }

// ---- 业务错误 sentinel(handler 层映 4xx / 5xx) ----

var (
	// ErrStoreNotFound:按 branch_id 未命中。
	ErrStoreNotFound = errors.New("master-data: 门店不存在")
	// ErrEmployeeNotFound:按 (id, branch_id) 未命中。
	ErrEmployeeNotFound = errors.New("master-data: 收银员不存在")
	// ErrEmployeeCodeNotFound:按 (branch_id, employee_code) 未命中。
	ErrEmployeeCodeNotFound = errors.New("master-data: 工号不存在")
	// ErrShiftNotFound:按 id 未命中。
	ErrShiftNotFound = errors.New("master-data: 班次不存在")
	// ErrShiftAlreadyOpen:该收银员已有未交班的班次。
	ErrShiftAlreadyOpen = errors.New("master-data: 该收银员已有未交班的班次")
	// ErrShiftClosed:班次已交班(不可重复交班)。
	ErrShiftClosed = errors.New("master-data: 班次已交班")
	// ErrInvalidInput:入参非法。
	ErrInvalidInput = errors.New("master-data: 入参非法")
	// ErrEmployeeCodeConflict:该门店已有同工号的收银员(409)。
	ErrEmployeeCodeConflict = errors.New("master-data: 该门店已有同工号的收银员")
)
