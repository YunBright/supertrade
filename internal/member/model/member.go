// Package model —— member 域的本地表(6 张)。
//
// # 域边界
//
// member 拥有**会员域的全部事实**:
//
//	members               会员主档(主键 (id, branch_id))
//	member_types          会员类别(门店各自维护,不跨店合并)
//	member_point_ledger   积分流水(append-only)
//	member_account_ledger 储值流水(append-only)
//	member_settings       门店级会员配置
//	payment_ways          付款方式字典
//
// # 三条不可动摇的约定
//
//  1. **卡号非全局唯一** —— 思迅的 card_id 长度 3~11(会员卡)/1~18(储值卡)/6(VIP),
//     同一张卡号可以出现在不同门店 ⇒ 主键必须是 `(id, branch_id)` 复合主键。
//  2. **积分口径只有两个列** —— `points_accumulated`(累计获得)与 `points_used`(累计扣减),
//     **剩余积分 = 两者之差**。⛔ 不另立 `points_balance` 列:多一个列就多一处会漂移的真相源。
//  3. **软删用 `gorm.DeletedAt`** —— 不用 `*time.Time`(那样 GORM **不会**自动过滤软删行,
//     表现是"删了还能查到")。
//
// # 金额
//
// 金额一律 `decimal.Decimal` + `numeric(18,4)`,⛔ 绝不用 float64。
// `decimal.Decimal` 的 `MarshalJSON` 默认输出**带引号的字符串**(`"12.50"`)
// ⇒ JSON 里的金额天然是字符串,Dart 侧不会把它变成 double。**保持这样,不要改成数字。**
package model

import (
	"fmt"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// Member 是会员主档。
//
// ⚠️ **复合主键 (id, branch_id)** —— 思迅卡号非全局唯一(见包注释第 1 条)。
type Member struct {
	// ID 是会员号(= 思迅 card_id 规范化后的值)。
	ID string `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	// BranchID 是门店(复合主键的第二列)。
	BranchID string `gorm:"primaryKey;column:branch_id;type:varchar(64)" json:"branch_id"`
	// CardTypeID 是会员类别 → member_types.id。
	//
	// ⚠️ **两库字典不同**(思迅 ysx `2=VIP储值卡` / hbposv7 `wx=微信会员`)
	// ⇒ 类别按**源门店**建,不跨店合并。
	CardTypeID string `gorm:"column:card_type_id;type:varchar(64);not null;index:idx_member_branch_type" json:"card_type_id"`
	// CardFaceNo 是卡面卡号(思迅源库大面积为空,本域自管)。
	CardFaceNo string `gorm:"column:card_face_no;type:varchar(64)" json:"card_face_no"`
	// OpID 是**开卡的幂等键**(唯一索引)。
	//
	// ⚠️ 它只记"这张卡是哪个 op_id 开出来的" ⇒ 同一 op_id 重复提交开卡
	//   不会开出第二张卡(靠唯一索引 + ON CONFLICT DO NOTHING)。
	OpID string `gorm:"column:op_id;type:varchar(64);uniqueIndex:idx_member_op" json:"op_id"`
	// Name 是姓名(索引:门店 + 姓名)。
	Name string `gorm:"column:name;type:varchar(80);not null;index:idx_member_branch_name" json:"name"`
	// Phone 是电话号码(索引:门店 + 电话)。
	Phone string `gorm:"column:phone;type:varchar(32);index:idx_member_branch_phone" json:"phone"`
	// Mobile 是手机号码。
	Mobile string `gorm:"column:mobile;type:varchar(32)" json:"mobile"`
	// Gender 是性别(男/女)。
	Gender string `gorm:"column:gender;type:varchar(8)" json:"gender"`
	// Birthday 是生日。
	Birthday *time.Time `gorm:"column:birthday" json:"birthday,omitempty"`
	// CalendarType 是历法(公历/农历)。
	CalendarType string `gorm:"column:calendar_type;type:varchar(8)" json:"calendar_type"`
	// Status 是状态:normal / frozen / lost / cancelled。
	//
	// ⚠️ 思迅的 card_status(0/1/2/3/5)**不照搬** —— 映射到我们自己的枚举。
	Status string `gorm:"column:status;type:varchar(16);not null;default:'normal';index:idx_member_branch_status" json:"status"`
	// ValidFrom 是入会日期。
	ValidFrom *time.Time `gorm:"column:valid_from" json:"valid_from,omitempty"`
	// ValidUntil 是有效期至。
	ValidUntil *time.Time `gorm:"column:valid_until" json:"valid_until,omitempty"`
	// Favorite 是爱好。
	Favorite string `gorm:"column:favorite;type:varchar(255)" json:"favorite"`
	// IDCardNo 是身份证(查询条件之一)。
	IDCardNo string `gorm:"column:id_card_no;type:varchar(32)" json:"id_card_no"`
	// Remark 是备注(查询条件之一)。
	Remark string `gorm:"column:remark;type:varchar(500)" json:"remark"`
	// FeeAmountYuan 是会员费。
	FeeAmountYuan decimal.Decimal `gorm:"column:fee_amount_yuan;type:numeric(18,4);not null;default:0" json:"fee_amount_yuan"`
	// LimitedUseCount 是限用次数(0 = 不限次)。
	//
	// ⚠️ 思迅 `use_num` 的 `999998` 是"不限次"哨兵 ⇒ 导入时转成 0。
	LimitedUseCount int `gorm:"column:limited_use_count;type:integer;not null;default:0" json:"limited_use_count"`
	// UsedCount 是已用次数。
	UsedCount int `gorm:"column:used_count;type:integer;not null;default:0" json:"used_count"`
	// PointsAccumulated 是**累计获得**积分。
	PointsAccumulated decimal.Decimal `gorm:"column:points_accumulated;type:numeric(18,4);not null;default:0" json:"points_accumulated"`
	// PointsUsed 是**累计扣减**积分。
	//
	// ⚠️ 剩余积分 = PointsAccumulated − PointsUsed(**派生**,不落列)。
	PointsUsed decimal.Decimal `gorm:"column:points_used;type:numeric(18,4);not null;default:0" json:"points_used"`
	// BalanceYuan 是零钱包余额(储值卡余额)。
	BalanceYuan decimal.Decimal `gorm:"column:balance_yuan;type:numeric(18,4);not null;default:0" json:"balance_yuan"`
	// ConsumedAmountYuan 是累计消费金额。
	ConsumedAmountYuan decimal.Decimal `gorm:"column:consumed_amount_yuan;type:numeric(18,4);not null;default:0" json:"consumed_amount_yuan"`
	// ConsumedCount 是消费次数。
	ConsumedCount int `gorm:"column:consumed_count;type:integer;not null;default:0" json:"consumed_count"`
	// PasswordHash 是会员口令哈希(pbkdf2-sha256)。
	//
	// ⚠️ **绝不出现在 JSON 里**(json:"-")—— 哈希泄漏 = 会员口令泄漏。
	PasswordHash string `gorm:"column:password_hash;type:varchar(255)" json:"-"`
	// FaceID 是面部特征标识。
	FaceID string `gorm:"column:face_id;type:varchar(255)" json:"face_id"`
	// OpenID 是微信/支付宝 openid。
	OpenID string `gorm:"column:open_id;type:varchar(128)" json:"open_id"`
	// AuthorizedBy 是最近一次**越权授权**的授权人。
	//
	// ⚠️ 它是**审计列**,不是权限开关:权限判定走 auth 的 per-branch scope 矩阵,
	//   这里只回答"这次写操作是谁替他批的"。
	AuthorizedBy string `gorm:"column:authorized_by;type:varchar(64)" json:"authorized_by"`
	// ServerVersion 是**服务端版本号(水位)**。
	//
	// ⚠️ 拉取水位用服务端版本号,**不用客户端时间戳**(门店机器时钟会漂)。
	ServerVersion int64 `gorm:"column:server_version;type:bigint;not null;default:0" json:"server_version"`
	// CreatedBy / UpdatedBy 是审计列(链路:claims.Sub → handler.callerSub → service 输入)。
	CreatedBy string `gorm:"column:created_by;type:varchar(64);not null" json:"created_by"`
	UpdatedBy string `gorm:"column:updated_by;type:varchar(64)" json:"updated_by"`
	// CreatedAt / UpdatedAt 是时间戳。
	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
	// DeletedAt 是软删标记。
	//
	// ⚠️ 必须是 `gorm.DeletedAt` —— `*time.Time` 不会被 GORM 自动过滤。
	DeletedAt gorm.DeletedAt `gorm:"column:deleted_at;index" json:"-"`

	// RemainingPoints 是**剩余积分(派生)**。
	//
	// ⚠️ `gorm:"-"` ⇒ 不落库。它由 service 在读出后填充。
	RemainingPoints decimal.Decimal `gorm:"-" json:"remaining_points"`
}

// TableName 显式指定表名。
func (Member) TableName() string { return "members" }

// MaxIDLen 是 id / op_id 的**列宽上限**（members.id / members.op_id 都是
// varchar(64)）。
//
// ⚠️ 为什么要显式校验（2026-10-21 契约测试实测踩到）：
//
//	超长 op_id 写进去 ⇒ **数据库报错** ⇒ handler 的 mapErr 落到 default 分支
//	⇒ 返 **500 internal_error**（"系统繁忙,请稍后重试"）。
//	症状与"参数写错了"毫无关系，现场根本想不到是长度问题。
//
//	⇒ 在落库前拦住并返 `model.ErrInvalidInput`（⇒ 400 bad_request）。
//
//	判据：**列宽是事实**，与"调用方应该传短的"这种假设无关 ——
//	任何超出列宽的写入都是 500，而不是 400。
const MaxIDLen = 64

// BeforeSave 校验 id / op_id 的长度。
//
// ⚠️ 放在**模型 hook** 而不是某个 service 方法里：开卡 / 改状态 / 改密 /
//
//	人脸 / 改档案 / 导入都写这张表 —— 单点校验才盖得住全部写路径
//	（每处 service 方法各判一次，漏一处就还是 500）。
func (m *Member) BeforeSave(tx *gorm.DB) error {
	if len(m.ID) > MaxIDLen {
		return fmt.Errorf("%w: 会员号长度 %d 超过上限 %d", ErrInvalidInput, len(m.ID), MaxIDLen)
	}
	if len(m.OpID) > MaxIDLen {
		return fmt.Errorf("%w: op_id 长度 %d 超过上限 %d", ErrInvalidInput, len(m.OpID), MaxIDLen)
	}
	return nil
}

// MemberType 是会员类别(门店各自维护)。
type MemberType struct {
	// ID 是类别 id(复合主键第一列)。
	ID string `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	// BranchID 是门店(复合主键第二列)。
	BranchID string `gorm:"primaryKey;column:branch_id;type:varchar(64)" json:"branch_id"`
	// Name 是类别名(会员卡 / 储值卡 / 微信会员 …)。
	Name string `gorm:"column:name;type:varchar(80);not null;index:idx_member_type_branch_name" json:"name"`
	// DiscountRate 是会员折扣(0.95 = 95 折)。
	DiscountRate decimal.Decimal `gorm:"column:discount_rate;type:numeric(9,4);not null;default:1" json:"discount_rate"`
	// FeeAmountYuan 是会员费。
	FeeAmountYuan decimal.Decimal `gorm:"column:fee_amount_yuan;type:numeric(18,4);not null;default:0" json:"fee_amount_yuan"`
	// LimitedUseCount 是限用次数(0 = 不限次)。
	LimitedUseCount int `gorm:"column:limited_use_count;type:integer;not null;default:0" json:"limited_use_count"`
	// AllowStoredValue 是"是否储值卡"。
	AllowStoredValue bool `gorm:"column:allow_stored_value;type:boolean;not null;default:false" json:"allow_stored_value"`
	// AllowPointsPay 是"是否可用积分抵扣"。
	AllowPointsPay bool `gorm:"column:allow_points_pay;type:boolean;not null;default:false" json:"allow_points_pay"`
	// Priority 是排序权重(小的在前)。
	Priority int `gorm:"column:priority;type:integer;not null;default:0" json:"priority"`
	// Status 是 active / inactive。
	Status string `gorm:"column:status;type:varchar(16);not null;default:'active'" json:"status"`
	// CreatedBy / UpdatedBy / CreatedAt / UpdatedAt / DeletedAt 是审计与软删列。
	CreatedBy string         `gorm:"column:created_by;type:varchar(64);not null" json:"created_by"`
	UpdatedBy string         `gorm:"column:updated_by;type:varchar(64)" json:"updated_by"`
	CreatedAt time.Time      `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time      `gorm:"column:updated_at;not null" json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"column:deleted_at;index" json:"-"`
}

// TableName 显式指定表名。
func (MemberType) TableName() string { return "member_types" }

// MemberPointLedger 是积分流水(append-only)。
//
// ⚠️ **append-only**:只追加,不更新不删除(纠错靠反向流水)。
// ⚠️ `OpID` 有唯一索引 ⇒ 它就是**幂等键**:同一 op_id 重复投递只落一行。
type MemberPointLedger struct {
	// ID 是流水行 id。
	ID string `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	// BranchID 是门店。
	BranchID string `gorm:"column:branch_id;type:varchar(64);not null;index:idx_member_points_member" json:"branch_id"`
	// MemberID 是会员 id。
	MemberID string `gorm:"column:member_id;type:varchar(64);not null;index:idx_member_points_member" json:"member_id"`
	// OpID 是**幂等键**(业务 op_id / uuid)。
	//
	// ⚠️ 用 varchar(64) 而不是 uuid 类型:测试库是 SQLite(无原生 uuid),
	//   且调用方给的 op_id 不保证是标准 uuid 形态。
	OpID string `gorm:"column:op_id;type:varchar(64);not null;uniqueIndex:idx_member_points_op" json:"op_id"`
	// Direction 是方向:earn(获得) / deduct(扣减)。
	Direction string `gorm:"column:direction;type:varchar(8);not null" json:"direction"`
	// Points 是**本次变动**(earn 正数 / deduct 负数)。
	Points decimal.Decimal `gorm:"column:points;type:numeric(18,4);not null" json:"points"`
	// PointsAfter 是**变动后的累计获得值**(members.points_accumulated)。
	//
	// ⚠️ 剩余积分**不在这里冗余**(否则和 members 上的两列会漂移)——
	//   剩余积分 = points_accumulated − points_used,现算。
	PointsAfter decimal.Decimal `gorm:"column:points_after;type:numeric(18,4);not null" json:"points_after"`
	// Reason 是原因:sale / manual_deduct / manual_grant。
	Reason string `gorm:"column:reason;type:varchar(32);not null" json:"reason"`
	// RefFlowNo 是关联单号(小票 / 销售单)。
	RefFlowNo string `gorm:"column:ref_flow_no;type:varchar(64)" json:"ref_flow_no"`
	// OperatorID 是实际操作人(auth user id)。
	OperatorID string `gorm:"column:operator_id;type:varchar(64);not null" json:"operator_id"`
	// AuthorizedBy 是**越权授权人**(图4/图5 审计);空 = 操作人自己有权限。
	AuthorizedBy string `gorm:"column:authorized_by;type:varchar(64)" json:"authorized_by"`
	// OperatedAt 是操作时刻。
	OperatedAt time.Time `gorm:"column:operated_at;not null" json:"operated_at"`
	// CreatedAt 是入库时刻。
	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName 显式指定表名。
func (MemberPointLedger) TableName() string { return "member_point_ledger" }

// MemberAccountLedger 是储值流水(append-only)。
//
// ⚠️ `OpID` 唯一索引 ⇒ 幂等键(同 member_point_ledger)。
type MemberAccountLedger struct {
	// ID 是流水行 id。
	ID string `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	// BranchID 是门店。
	BranchID string `gorm:"column:branch_id;type:varchar(64);not null;index:idx_member_account_member" json:"branch_id"`
	// MemberID 是会员 id。
	MemberID string `gorm:"column:member_id;type:varchar(64);not null;index:idx_member_account_member" json:"member_id"`
	// OpID 是**幂等键**。
	OpID string `gorm:"column:op_id;type:varchar(64);not null;uniqueIndex:idx_member_account_op" json:"op_id"`
	// Direction 是方向:recharge(充值) / refund(退储)。
	Direction string `gorm:"column:direction;type:varchar(8);not null" json:"direction"`
	// AmountYuan 是本次金额(恒正;方向由 Direction 表达)。
	AmountYuan decimal.Decimal `gorm:"column:amount_yuan;type:numeric(18,4);not null" json:"amount_yuan"`
	// BalanceAfterYuan 是变动后的余额。
	BalanceAfterYuan decimal.Decimal `gorm:"column:balance_after_yuan;type:numeric(18,4);not null" json:"balance_after_yuan"`
	// Reason 是原因:recharge / refund。
	Reason string `gorm:"column:reason;type:varchar(32);not null" json:"reason"`
	// OperatorID 是实际操作人。
	OperatorID string `gorm:"column:operator_id;type:varchar(64);not null" json:"operator_id"`
	// AuthorizedBy 是**越权授权人**;空 = 操作人自己有权限。
	AuthorizedBy string `gorm:"column:authorized_by;type:varchar(64)" json:"authorized_by"`
	// OperatedAt 是操作时刻。
	OperatedAt time.Time `gorm:"column:operated_at;not null" json:"operated_at"`
	// CreatedAt 是入库时刻。
	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName 显式指定表名。
func (MemberAccountLedger) TableName() string { return "member_account_ledger" }

// MemberSettings 是**门店级**会员配置(由 tradewind_app 写,POS 只读)。
type MemberSettings struct {
	// BranchID 是门店(主键)。
	BranchID string `gorm:"primaryKey;column:branch_id;type:varchar(64)" json:"branch_id"`
	// MembershipEnabled 是"该门店是否启用会员"。
	MembershipEnabled bool `gorm:"column:membership_enabled;type:boolean;not null;default:false" json:"membership_enabled"`
	// PointsRate 是消费积分率(1.0 = 消费 1 元得 1 积分)。
	PointsRate decimal.Decimal `gorm:"column:points_rate;type:numeric(9,4);not null;default:1" json:"points_rate"`
	// PointsDeductRate 是积分抵扣率(1 积分抵多少元)。
	PointsDeductRate decimal.Decimal `gorm:"column:points_deduct_rate;type:numeric(9,4);not null;default:0" json:"points_deduct_rate"`
	// PasswordRequired 是"开卡是否必须设密码"。
	PasswordRequired bool `gorm:"column:password_required;type:boolean;not null;default:false" json:"password_required"`
	// FaceRequired 是"开卡是否必须录人脸"。
	FaceRequired bool `gorm:"column:face_required;type:boolean;not null;default:false" json:"face_required"`
	// MaxFacePerMember 是每个会员最多录几个人脸特征。
	MaxFacePerMember int `gorm:"column:max_face_per_member;type:integer;not null;default:1" json:"max_face_per_member"`
	// UpdatedBy / UpdatedAt 是审计列。
	UpdatedBy string    `gorm:"column:updated_by;type:varchar(64)" json:"updated_by"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式指定表名。
func (MemberSettings) TableName() string { return "member_settings" }

// PaymentWay 是付款方式字典(可从思迅 `t_bd_payment_info` 导入)。
type PaymentWay struct {
	// ID 是字典项 id(复合主键第一列)。
	ID string `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	// BranchID 是门店(复合主键第二列)。
	BranchID string `gorm:"primaryKey;column:branch_id;type:varchar(64)" json:"branch_id"`
	// Code 是付款方式码(RMB / SWX / SZF / JFP / EKT …)。
	Code string `gorm:"column:code;type:varchar(16);not null" json:"code"`
	// Name 是付款方式名。
	Name string `gorm:"column:name;type:varchar(80);not null" json:"name"`
	// Rate 是汇率(1 = 1:1)。
	Rate decimal.Decimal `gorm:"column:rate;type:numeric(9,4);not null;default:1" json:"rate"`
	// IsIntegral 是"是否积分支付"(思迅 is_integral=1,例:JFP)。
	IsIntegral bool `gorm:"column:is_integral;type:boolean;not null;default:false" json:"is_integral"`
	// SortOrder 是排序权重。
	SortOrder int `gorm:"column:sort_order;type:integer;not null;default:0" json:"sort_order"`
	// Status 是 active / inactive。
	Status string `gorm:"column:status;type:varchar(16);not null;default:'active'" json:"status"`
	// CreatedBy / UpdatedBy / CreatedAt / UpdatedAt / DeletedAt 是审计与软删列。
	CreatedBy string         `gorm:"column:created_by;type:varchar(64);not null" json:"created_by"`
	UpdatedBy string         `gorm:"column:updated_by;type:varchar(64)" json:"updated_by"`
	CreatedAt time.Time      `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time      `gorm:"column:updated_at;not null" json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"column:deleted_at;index" json:"-"`
}

// TableName 显式指定表名。
func (PaymentWay) TableName() string { return "payment_ways" }
