// Package model 是 settlement-auditor 的数据模型(PostgreSQL,gorm)。
//
// 表归属:全部落 supertrade PG(与 fresh-meat / stocktake 同库,无 schema 前缀)。
// cube 不承载审批数据(业务数据不需要进数据治理层)。
package model

import (
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/datatypes"
)

// ---- 状态机 ----

// 审批实例状态机:
//
//	received → auditing → approved(机器通过,已发 apply_event 命令,等 command_result)
//	                    → rejected(机器拒绝,已通知申请人)
//	                    → failed(流水线异常,可 retry / rescan)
//	approved →(command_result ok)→ human_pending 语义合并:SpNo 回填后等企微回调
//	企微回调(sp_status) → human_approved / human_rejected / cancelled
const (
	StatusReceived      = "received"
	StatusAuditing      = "auditing"
	StatusApproved      = "approved"       // 机器一审通过(企微申请已提交/提交中)
	StatusRejected      = "rejected"       // 机器一审拒绝
	StatusFailed        = "failed"         // 流水线异常
	StatusHumanApproved = "human_approved" // 企微终审通过
	StatusHumanRejected = "human_rejected" // 企微终审驳回
	StatusCancelled     = "cancelled"      // 企微撤销/通过后撤销
)

// 信号级别。
const (
	SignalInfo  = "info"
	SignalWarn  = "warn"
	SignalBlock = "block"
)

// LLM 结论来源。
const (
	VerdictSourceLLM          = "llm"
	VerdictSourceRuleFallback = "rule_fallback"
)

// ApprovalInstance 一次付款审批的机器审核实例(审批单快照)。
type ApprovalInstance struct {
	ID               string          `gorm:"primaryKey;size:32" json:"id"`
	CorpID           string          `gorm:"size:64;index" json:"corp_id"` // wcorp 友好 id
	BranchID         string          `gorm:"size:32;index" json:"branch_id"`
	ApplicantUserID  string          `gorm:"size:64;index" json:"applicant_user_id"` // auth user id
	ApplicantWecomID string          `gorm:"size:64" json:"applicant_wecom_id"`      // 企微 userid(发通知用)
	ManualAmountYuan decimal.Decimal `gorm:"type:numeric(14,2)" json:"manual_amount_yuan"`
	Remark           string          `gorm:"size:512" json:"remark"`
	Status           string          `gorm:"size:24;index" json:"status"`
	// SpNo 是企微审批单号(apply_event command_result 回填)。
	SpNo string `gorm:"size:64;index" json:"sp_no"`
	// ApproverUserIDs / CcUserIDs 是本次发起时申请人选定的审批人/抄送人
	// 企微 userid 列表(JSON 数组),随单快照;提交企微时映射为
	// process.node_list(type=1 审批人 / type=2 抄送人)。
	ApproverUserIDs datatypes.JSON `gorm:"type:jsonb" json:"approver_userids"`
	CcUserIDs       datatypes.JSON `gorm:"type:jsonb" json:"cc_userids"`
	// SpStatus 是企微审批状态:1审批中 2通过 3驳回 4撤销 6通过后撤销 10已支付。
	SpStatus int `gorm:"default:0" json:"sp_status"`
	// LastCommandID 是最近一次 apply_event 命令 id(command_result 关联键)。
	LastCommandID string `gorm:"size:64" json:"-"`
	// ApplicantBearer 是提交时申请人的 Authorization 头(JWT),供异步流水线
	// 以申请人身份调 cube(cube 实例 sidecar 的 middleware.http.bearer 强制
	// JWT,JWT_TTL=1h 覆盖正常审核窗口;重跑时由 Retry 请求刷新)。
	// 机器终态(approved/rejected/failed)即清空,缩短凭证落库时间窗。
	// 永不序列化(json:"-")。
	ApplicantBearer string   `gorm:"size:2048" json:"-"`
	RejectReason    string   `gorm:"size:1024" json:"reject_reason"`
	Error           string   `gorm:"size:1024" json:"error"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `gorm:"index" json:"updated_at"`
}

func (ApprovalInstance) TableName() string { return "approval_instances" }

// ApprovalSettlement 审批 ↔ 结算单多对多(含 cube 侧货款金额快照)。
type ApprovalSettlement struct {
	ID              string          `gorm:"primaryKey;size:36" json:"id"`
	ApprovalID      string          `gorm:"size:32;index" json:"approval_id"`
	SettlementID    string          `gorm:"size:64;index" json:"settlement_id"`
	SupplierID      string          `gorm:"size:64" json:"supplier_id"`
	DocumentType    string          `gorm:"size:8" json:"document_type"` // DP/CP
	GoodsAmountYuan decimal.Decimal `gorm:"type:numeric(14,2)" json:"goods_amount_yuan"`
	// Checked 表示机器审核时该结算单在 cube 中存在且已审核。
	Checked   bool      `json:"checked"`
	CreatedAt time.Time `json:"created_at"`
}

func (ApprovalSettlement) TableName() string { return "approval_settlements" }

// AuditSignal 一条核对信号(证据快照存 Evidence JSON)。
type AuditSignal struct {
	ID           string         `gorm:"primaryKey;size:36" json:"id"`
	ApprovalID   string         `gorm:"size:32;index" json:"approval_id"`
	SettlementID string         `gorm:"size:64;index" json:"settlement_id"`
	ProductID    string         `gorm:"size:64;index" json:"product_id,omitempty"`
	SignalType   string         `gorm:"size:48" json:"signal_type"`
	Level        string         `gorm:"size:8;index" json:"level"` // info/warn/block
	Message      string         `gorm:"size:1024" json:"message"`
	Evidence     datatypes.JSON `gorm:"type:jsonb" json:"evidence"`
	CreatedAt    time.Time      `json:"created_at"`
}

func (AuditSignal) TableName() string { return "audit_signals" }

// AuditLog 推理分步日志(Flutter 实时监控读取;每步同步 publish settlement.audit.log)。
type AuditLog struct {
	ID            string    `gorm:"primaryKey;size:36" json:"id"`
	ApprovalID    string    `gorm:"size:32;index" json:"approval_id"`
	Step          string    `gorm:"size:48" json:"step"`
	InputSummary  string    `gorm:"size:2048" json:"input_summary"`
	OutputSummary string    `gorm:"size:4096" json:"output_summary"`
	LLMTrace      string    `gorm:"size:8192" json:"llm_trace,omitempty"`
	DurationMs    int64     `json:"duration_ms"`
	CreatedAt     time.Time `json:"created_at"`
}

func (AuditLog) TableName() string { return "audit_logs" }

// LLMVerdict LLM(或规则降级)对一次审批的结论,一审批一条。
type LLMVerdict struct {
	ID         string          `gorm:"primaryKey;size:36" json:"id"`
	ApprovalID string          `gorm:"size:32;uniqueIndex" json:"approval_id"`
	Conclusion string          `gorm:"size:16" json:"conclusion"` // pass / reject
	Reason     string          `gorm:"size:2048" json:"reason"`
	Confidence decimal.Decimal `gorm:"type:numeric(4,3)" json:"confidence"`
	// SuggestStocktake 形如 {"scope":"category|supplier|product","ids":[...]}。
	SuggestStocktake datatypes.JSON `gorm:"type:jsonb" json:"suggest_stocktake,omitempty"`
	RawJSON          datatypes.JSON `gorm:"type:jsonb" json:"raw_json"`
	Source           string         `gorm:"size:16" json:"source"` // llm / rule_fallback
	CreatedAt        time.Time      `json:"created_at"`
}

func (LLMVerdict) TableName() string { return "llm_verdicts" }

// ApprovalStocktakeLink 审批 ↔ 盘点计划绑定(FR-3.3)。
type ApprovalStocktakeLink struct {
	ID                string    `gorm:"primaryKey;size:36" json:"id"`
	ApprovalID        string    `gorm:"size:32;index" json:"approval_id"`
	StocktakeHeaderID string    `gorm:"size:32;index" json:"stocktake_header_id"`
	SignalID          string    `gorm:"size:36" json:"signal_id,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
}

func (ApprovalStocktakeLink) TableName() string { return "approval_stocktake_links" }

// ApprovalApproverPreference 记住每个申请人在某 branch 下**上次**选定的
// 审批人/抄送人(对齐企微官方审批应用"再次发起默认带出上次人员"的体验)。
// 提交审批成功时自动覆盖;发起页 GET 偏好端点带出。
type ApprovalApproverPreference struct {
	// ID 形如 "<branchID>:<applicantUserID>"。
	ID              string    `gorm:"primaryKey;size:128" json:"id"`
	BranchID        string    `gorm:"size:32;index" json:"branch_id"`
	ApplicantUserID string    `gorm:"size:64;index" json:"applicant_user_id"`
	ApproverUserIDs datatypes.JSON `gorm:"type:jsonb" json:"approver_userids"`
	CcUserIDs       datatypes.JSON `gorm:"type:jsonb" json:"cc_userids"`
	UpdatedAt       time.Time `gorm:"index" json:"updated_at"`
}

func (ApprovalApproverPreference) TableName() string { return "approval_approver_preferences" }
