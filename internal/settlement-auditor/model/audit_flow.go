// Package model / audit_flow.go —— 机器审核「流程定制」的配置表。
//
// # 这套配置解决什么
//
// 用户口径（2026-10-28）：
//
//	「审批结算」的机器审核先按**有无结算单**分路：
//	  - 有结算单 → 按**供应商**查配置
//	  - 无结算单 → 按**付款项目**查配置
//	  命中配置的走「直接通过」或「逻辑判断流程」；
//	  **没配置**的才走现在这套机器人（LLM）审核流程。
//
// # 为什么配置放 supertrade PG 而不是 cube
//
// ① 它是**审核策略**，不是业务单据 —— cube 承载的是门店的业务事实；
// ② 读取方就是 settlement-auditor 自己的流水线（同进程同库 ⇒ 零跨服务调用）；
// ③ 「按门店配置」这件事天然需要一个 branch 维度，cube 那边没有这个键。
//
// # 与 LLM 输入面的隔离
//
// 这三张表的内容**不进入 AuditInput** ⇒ 配置里的任何文本都不会发给 LLM。
// 它们的唯一作用是**决定走哪条决策路径**。
package model

import "time"

// ---- 流程模式（mode）----
const (
	// FlowModeAutoPass 是「直接通过」：命中后不再问机器人，直接判通过。
	//
	// ⚠️ 仍会被**数据正确性类**信号硬拦（结算单不存在/未审核、金额勾稽不符…），
	//    详见 service/audit_flow_decide.go 的 flowHardStopSignals。
	FlowModeAutoPass = "auto_pass"
	// FlowModeLogic 是「逻辑判断流程」：命中后跑配置里指定的逻辑条件。
	FlowModeLogic = "logic"
)

// ---- 逻辑条件类型（logic_kind）----
const (
	// LogicDailySingle 是「每日有且仅有一单」（当前只用于供应商分支）。
	//
	// 语义由 service/audit_flow_decide.go 的 evaluateDailySingle 决定。
	LogicDailySingle = "daily_single"
	// LogicMonthlySingle 是「每月有且仅有一单」（当前只用于付款项分支）。
	LogicMonthlySingle = "monthly_single"
)

// AuditSupplierFlow 是**供应商**级流程配置（有结算单的那条路）。
//
// ⚠️ 唯一键是 (branch_id, supplier_id)：
//
//	同一个供应商在 A 店和 B 店可以配不同的流程 ——
//	用户口径「以上判断都和门店有关」。
type AuditSupplierFlow struct {
	ID       string `gorm:"primaryKey;size:36" json:"id"`
	BranchID string `gorm:"size:32;index:idx_audit_supplier_flow,unique" json:"branch_id"`
	// SupplierID 是**结算单上的供应商 id**（cube 口径，不是 PG 本地 suppliers.id）。
	//
	// ⚠️ 口径提示:有 cube 源的门店走 cube 供应商 id，无 cube 源的走本地
	//    suppliers.id —— 判据是 internal/cubesource 既有规则，本表不重复定义。
	SupplierID string `gorm:"size:64;index:idx_audit_supplier_flow,unique" json:"supplier_id"`
	// SupplierName 是**冗余快照**（纯展示用）。
	//
	// 为什么不 join:供应商名在 cube 的 supplier 字典里（duck model），
	// 结算查询 join 不到（cube 只支持 live↔live join），每次列表都去查 cube
	// 会让"打开配置页"变成一次跨服务调用 ⇒ 存一份名字，配置页直接显示。
	// ⚠️ 名字会过期（供应商改名后这里不会自动跟），只影响展示，不影响判断。
	SupplierName string `gorm:"size:128" json:"supplier_name"`
	// Mode 是 FlowModeAutoPass / FlowModeLogic。
	Mode string `gorm:"size:16" json:"mode"`
	// LogicKind 是逻辑条件类型（Mode=logic 时有意义）。
	LogicKind string `gorm:"size:32" json:"logic_kind"`
	// Enabled=false 等同「没配置」⇒ 走机器人审核流程。
	//
	// 保留这条而不是直接删行，是为了"临时停用某个供应商的定制流程"不用重建。
	Enabled   bool      `gorm:"default:true" json:"enabled"`
	Note      string    `gorm:"size:256" json:"note"`
	CreatedBy string    `gorm:"size:64" json:"created_by"`
	UpdatedBy string    `gorm:"size:64" json:"updated_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (AuditSupplierFlow) TableName() string { return "audit_supplier_flows" }

// AuditPaymentItemFlow 是**付款项目**级流程配置（无结算单的那条路）。
//
// ⚠️ 唯一键是 (branch_id, item_type)：
//
//	item_type 是**类型文本**（"水电费"/"租金"/自定义文字…），
//	与 approval_items.item_type 同一口径 —— 用户口径「收款类型/备注皆保存为文本」。
type AuditPaymentItemFlow struct {
	ID       string `gorm:"primaryKey;size:36" json:"id"`
	BranchID string `gorm:"size:32;index:idx_audit_item_flow,unique" json:"branch_id"`
	ItemType string `gorm:"size:64;index:idx_audit_item_flow,unique" json:"item_type"`
	// Mode 是 FlowModeAutoPass / FlowModeLogic。
	Mode string `gorm:"size:16" json:"mode"`
	// LogicKind 是逻辑条件类型（Mode=logic 时有意义）。
	LogicKind string `gorm:"size:32" json:"logic_kind"`
	// RegexID 指向 AuditRegexPattern.ID —— 用来从付款项备注里提取**款项所属日期**。
	//
	// ⚠️ 只有 Mode=logic 且 LogicKind=monthly_single 时才会用到：
	//    「每月有且仅有一单」需要知道这笔付款项属于哪个月，
	//    而月份写在备注里（例："9月水电费"）。
	//
	// ⚠️ 空 ⇒ 提取不出日期 ⇒ 该条件**判为不满足**（而不是跳过判断）：
	//    判据是"宁可不通过，也不要按错误的月份放款"。
	RegexID string `gorm:"size:36;index" json:"regex_id"`
	// Enabled=false 等同「没配置」。
	Enabled   bool      `gorm:"default:true" json:"enabled"`
	Note      string    `gorm:"size:256" json:"note"`
	CreatedBy string    `gorm:"size:64" json:"created_by"`
	UpdatedBy string    `gorm:"size:64" json:"updated_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (AuditPaymentItemFlow) TableName() string { return "audit_payment_item_flows" }

// AuditRegexPattern 是**正则表达式维护**表。
//
// # 它解决什么
//
// 付款项的「所属日期」在**备注**里（自由文本，例："9月水电费"）。
// 要判「每月有且仅有一单」必须先从备注里把月份抠出来 ——
// 而抠法的写法不该写死在代码里（各店各人的写法不一样：9月 / 9月份 / 2026年9月）。
//
// ⇒ 正则做成**可维护的配置**，并在管理后台提供"拿一段样例试跑"的功能。
type AuditRegexPattern struct {
	ID       string `gorm:"primaryKey;size:36" json:"id"`
	BranchID string `gorm:"size:32;index" json:"branch_id"`
	// Name 是这条正则的名字（管理后台列表里显示）。
	Name string `gorm:"size:128" json:"name"`
	// Pattern 是正则本体（RE2 语法 —— Go 的 regexp 包，不支持回溯与反向引用）。
	Pattern string `gorm:"size:512" json:"pattern"`
	// GroupIndex 是取哪个捕获组（0=整个匹配，1=第一个捕获组…），默认 1。
	//
	// ⚠️ 越界时测试接口会明确报错，而不是静默返回空串 ——
	//    "正则写了但组号填错"是最常见的配置错误，必须当场看得见。
	GroupIndex int `gorm:"default:1" json:"group_index"`
	// Enabled=false 的正则不参与提取（但仍可在管理后台看到与试跑）。
	Enabled   bool      `gorm:"default:true" json:"enabled"`
	Note      string    `gorm:"size:256" json:"note"`
	CreatedBy string    `gorm:"size:64" json:"created_by"`
	UpdatedBy string    `gorm:"size:64" json:"updated_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (AuditRegexPattern) TableName() string { return "audit_regex_patterns" }
