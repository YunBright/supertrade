// Package model / model.go —— 超市流水对账域（cash-recon）的全部表模型与状态常量。
//
// # 这一域和别的域的根本差别
//
//	收银流水   → **不落本地库**：有 cube 源查 cube、无源读 transaction 域的 pay_flows（只读）；
//	银行流水   → 落本域库（导入即事实）；
//	对账结果   → 落本域库（批次/匹配/差异），**差异只 UPSERT 状态，永不 DELETE**。
//
// # 三条红线在本域的实现约束
//
//  1. 差异不允许直接删除 —— recon_diffs 没有 DELETE 端点；人工动作写 recon_audit_logs。
//  2. 共享表格不是账 —— 看板只读；二期才做表格同步。
//  3. 手续费必须单列 —— recon_matches / recon_diffs / recon_period_snapshots
//     永远把 fee 单独存一列，不允许把"到账金额 = 应收金额"硬凑平。
//
// # 期初余额为什么是独立表
//
//	期末 = 期初 + 流入 - 流出 - 手续费
//
// 期初是**外部事实**（银行对账单余额 / 钱箱现金），系统算不出来。缺期初时
// **不允许默认 0**（那会让资金日报的余额静默算错且没有任何告警）——
// 一律 409 阻断，由出纳按 §10.1 建账。
//
// # 金额一律 decimal.Decimal + numeric(18,4)
//
// 与 transaction / settlement-auditor 同款：JSON 侧交给 handler 格式化，
// 不在模型层做舍入。
package model

import (
	"errors"
	"time"

	"github.com/shopspring/decimal"
)

// ---- 错误 sentinel（handler 层映 4xx / 5xx） ----
var (
	// ErrInvalidInput：参数不合法 ⇒ 400。
	ErrInvalidInput = errors.New("cash-recon: 参数不合法")
	// ErrRangeTooLarge：查询区间超过上限 ⇒ 400 range_too_large。
	ErrRangeTooLarge = errors.New("cash-recon: 查询区间过大")
	// ErrPeriodFrozen：目标日期落在已冻结区间 ⇒ 409 period_frozen。
	ErrPeriodFrozen = errors.New("cash-recon: 该日期区间已冻结")
	// ErrOpeningBalanceMissing：缺期初余额 ⇒ 409 opening_balance_missing。
	ErrOpeningBalanceMissing = errors.New("cash-recon: 缺少期初余额")
	// ErrCashCountMissing：缺当期实盘 ⇒ 409 cash_count_missing。
	ErrCashCountMissing = errors.New("cash-recon: 缺少当期现金实盘")
	// ErrOpeningItemsIncomplete：建账缺账户 ⇒ 400 opening_items_incomplete。
	ErrOpeningItemsIncomplete = errors.New("cash-recon: 期初建账缺账户")
	// ErrCubeSourceUnavailable：问不到数据源（链路故障）⇒ 503，**不回落本地**。
	ErrCubeSourceUnavailable = errors.New("cash-recon: 数据源路由暂不可用")
	// ErrCubeSourceDisabled：门店配了数据源但被停用 ⇒ 503，**不回落本地**。
	//
	// ⚠️ 与"没配"（正常自营店，读本地 PG）严格区分：回落会让用户看到
	// 一份空流水，而真相是"这个门店现在查不了"。
	ErrCubeSourceDisabled = errors.New("cash-recon: 该门店的数据源已停用")
	// ErrAlreadyExists：唯一键冲突 ⇒ 409 already_exists。
	ErrAlreadyExists = errors.New("cash-recon: 记录已存在")
	// ErrCubeQueryFailed：cube 查询失败 ⇒ 503（不静默返回 0）。
	ErrCubeQueryFailed = errors.New("cash-recon: 收银流水查询失败")
	// ErrNotFound ⇒ 404。
	ErrNotFound = errors.New("cash-recon: 记录不存在")
)

// ---- 批次状态 ----
const (
	BatchStatusDraft     = "draft"
	BatchStatusFrozen    = "frozen"
	BatchStatusAuditing  = "auditing"
	BatchStatusCompleted = "completed"
	BatchStatusVoid      = "void"
)

// ---- 批次类型 ----
const (
	// BatchKindNormal 是常规对账批次。
	BatchKindNormal = "normal"
	// BatchKindAdjust 是已归档期次的调整批次（只能新建，不能改历史）。
	BatchKindAdjust = "adjust"
)

// ---- 差异状态（没有"删除"这一态） ----
const (
	DiffStatusOpen      = "open"
	DiffStatusHandled   = "handled"
	DiffStatusReasoned  = "reasoned"
	DiffStatusHung      = "hung"
	DiffStatusEscalated = "escalated"
)

// DiffSettledStatuses 是"人工已经处理过"的状态集合。
//
// ⚠️ 重算时这些状态**必须保留**（按 dedupe_key 只更新引擎字段），
// 否则出纳每天的工作会被下一次 apply 冲掉。
var DiffSettledStatuses = []string{DiffStatusHandled, DiffStatusReasoned, DiffStatusHung, DiffStatusEscalated}

// ---- 差异类型 ----
const (
	DiffKindAmountDiff     = "amount_diff"
	DiffKindSingleCash     = "single_sided_cash"
	DiffKindSingleBank     = "single_sided_bank"
	DiffKindDuplicate      = "duplicate"
	DiffKindFeeDiff        = "fee_diff"
	DiffKindUnmappedPayWay = "unmapped_pay_way"
	DiffKindTimeLag        = "time_lag"
)

// ---- 匹配类型 ----
const (
	MatchKindExact    = "exact"
	MatchKindFeeAware = "fee_aware"
	MatchKindManual   = "manual"
)

// ---- 支付方式分组（cube pay_way → 标准 method） ----
const (
	MethodCash        = "cash"
	MethodWechat      = "wechat"
	MethodBankCard    = "bank_card"
	MethodStoredValue = "stored_value"
	MethodCredit      = "credit"
	MethodChange      = "change" // 找零：**不计收款**
	MethodUnmapped    = "unmapped"
)

// ---- 账户类型 ----
const (
	AccountTypeCash    = "cash"
	AccountTypeBank    = "bank"
	AccountTypeChannel = "channel"
)

// ---- 期初来源 ----
const (
	OpeningSourceManual = "manual"
	OpeningSourceCarry  = "carry"
	OpeningSourceAdjust = "adjust"
)

// ---- 付款台账 ----
const (
	PaymentSourceWecom  = "wecom_approval"
	PaymentSourceManual = "manual"
)

// ---- 导入类型 ----
const (
	ImportKindBank    = "bank"
	ImportKindChannel = "channel"
)

// ReconBatch 是一次对账批次。
//
// 一个日期区间可以有多个批次（重算追加），但只有 status=completed 的那个
// 会产出期末快照；completed 之后不可变，调整走 BatchKindAdjust。
type ReconBatch struct {
	ID       string `gorm:"primaryKey;column:id;type:varchar(36)" json:"id"`
	BranchID string `gorm:"column:branch_id;type:varchar(32);not null;uniqueIndex:ux_batch_branch_no,priority:1;index:idx_batch_branch_dates,priority:1" json:"branch_id"`
	// BatchNo 形如 CR-00-20261030-01（出纳口头沟通要对得上号）。
	BatchNo     string    `gorm:"column:batch_no;type:varchar(64);not null;uniqueIndex:ux_batch_branch_no,priority:2" json:"batch_no"`
	Kind        string    `gorm:"column:kind;type:varchar(16);not null;default:normal" json:"kind"`
	BizDateFrom time.Time `gorm:"column:biz_date_from;type:date;not null;index:idx_batch_branch_dates,priority:2" json:"biz_date_from"`
	BizDateTo   time.Time `gorm:"column:biz_date_to;type:date;not null;index:idx_batch_branch_dates,priority:3" json:"biz_date_to"`
	Status      string    `gorm:"column:status;type:varchar(16);not null;default:draft;index" json:"status"`
	// MatchRate 是匹配置信度（0~1，4 位小数）。
	MatchRate   decimal.Decimal `gorm:"column:match_rate;type:numeric(10,4);not null;default:0" json:"match_rate"`
	DiffCount   int             `gorm:"column:diff_count;not null;default:0" json:"diff_count"`
	CashTotal   decimal.Decimal `gorm:"column:cash_total;type:numeric(18,4);not null;default:0" json:"cash_total"`
	BankTotal   decimal.Decimal `gorm:"column:bank_total;type:numeric(18,4);not null;default:0" json:"bank_total"`
	FeeTotal    decimal.Decimal `gorm:"column:fee_total;type:numeric(18,4);not null;default:0" json:"fee_total"`
	CreatedBy   string          `gorm:"column:created_by;type:varchar(64)" json:"created_by"`
	FrozenAt    *time.Time      `gorm:"column:frozen_at" json:"frozen_at"`
	CompletedAt *time.Time      `gorm:"column:completed_at" json:"completed_at"`
	CreatedAt   time.Time       `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt   time.Time       `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式指定表名。
func (ReconBatch) TableName() string { return "recon_batches" }

// ReconCashFlow 是**冻结时的收银流水快照**（append-only）。
//
// ⚠️ 只在 freeze 时写入：平时收银流水是实时查的（cube / 本地 PG），
// 只有"审核计算"才生成本表的行。
type ReconCashFlow struct {
	ID       string    `gorm:"primaryKey;column:id;type:varchar(36)" json:"id"`
	BatchID  string    `gorm:"column:batch_id;type:varchar(36);not null;uniqueIndex:ux_cash_batch_dim,priority:1" json:"batch_id"`
	BranchID string    `gorm:"column:branch_id;type:varchar(32);not null;index" json:"branch_id"`
	BizDate  time.Time `gorm:"column:biz_date;type:date;not null;uniqueIndex:ux_cash_batch_dim,priority:2" json:"biz_date"`
	PayWay   string    `gorm:"column:pay_way;type:varchar(16);not null;uniqueIndex:ux_cash_batch_dim,priority:3" json:"pay_way"`
	Method   string    `gorm:"column:method;type:varchar(16);not null" json:"method"`
	// AmountNet 是当日该付款方式的**净额**（负数 = 冲销大于收款）。
	AmountNet decimal.Decimal `gorm:"column:amount_net;type:numeric(18,4);not null;default:0" json:"amount_net"`
	TxnCount  int             `gorm:"column:txn_count;not null;default:0" json:"txn_count"`
	Source    string          `gorm:"column:source;type:varchar(8);not null" json:"source"`
	// SnapshotAt 是快照时刻（由 service 传入，引擎不取 time.Now）。
	SnapshotAt time.Time `gorm:"column:snapshot_at;not null" json:"snapshot_at"`
}

// TableName 显式指定表名。
func (ReconCashFlow) TableName() string { return "recon_cash_flows" }

// ReconBankFlow 是一条银行/渠道到账流水（导入即事实）。
type ReconBankFlow struct {
	ID        string          `gorm:"primaryKey;column:id;type:varchar(36)" json:"id"`
	ImportID  string          `gorm:"column:import_id;type:varchar(36);index" json:"import_id"`
	BranchID  string          `gorm:"column:branch_id;type:varchar(32);not null;uniqueIndex:ux_bank_dedupe,priority:1;index:idx_bank_branch_channel_date,priority:1" json:"branch_id"`
	Channel   string          `gorm:"column:channel;type:varchar(32);not null;index:idx_bank_branch_channel_date,priority:2" json:"channel"`
	ValueDate time.Time       `gorm:"column:value_date;type:date;not null;index:idx_bank_branch_channel_date,priority:3" json:"value_date"`
	Amount    decimal.Decimal `gorm:"column:amount;type:numeric(18,4);not null;default:0" json:"amount"`
	// Fee 是**实际手续费**（银行/渠道结算单上写明的）。
	Fee           decimal.Decimal `gorm:"column:fee;type:numeric(18,4);not null;default:0" json:"fee"`
	SettleBatchNo string          `gorm:"column:settle_batch_no;type:varchar(64)" json:"settle_batch_no"`
	ExternalID    string          `gorm:"column:external_id;type:varchar(64)" json:"external_id"`
	RawLine       string          `gorm:"column:raw_line;type:varchar(1024)" json:"raw_line"`
	// DedupeKey = sha256(channel|value_date|amount|fee|external_id|raw_line)，导入幂等键。
	DedupeKey string    `gorm:"column:dedupe_key;type:varchar(64);not null;uniqueIndex:ux_bank_dedupe,priority:2" json:"dedupe_key"`
	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName 显式指定表名。
func (ReconBankFlow) TableName() string { return "recon_bank_flows" }

// ReconImport 是一次文件导入的批次记录。
type ReconImport struct {
	ID          string    `gorm:"primaryKey;column:id;type:varchar(36)" json:"id"`
	BranchID    string    `gorm:"column:branch_id;type:varchar(32);not null;index" json:"branch_id"`
	Kind        string    `gorm:"column:kind;type:varchar(16);not null" json:"kind"`
	FileName    string    `gorm:"column:file_name;type:varchar(255)" json:"file_name"`
	RowTotal    int       `gorm:"column:row_total;not null;default:0" json:"row_total"`
	RowOK       int       `gorm:"column:row_ok;not null;default:0" json:"row_ok"`
	RowFailed   int       `gorm:"column:row_failed;not null;default:0" json:"row_failed"`
	CreatedBy   string    `gorm:"column:created_by;type:varchar(64)" json:"created_by"`
	ErrorSample string    `gorm:"column:error_sample;type:varchar(1024)" json:"error_sample"`
	CreatedAt   time.Time `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName 显式指定表名。
func (ReconImport) TableName() string { return "recon_imports" }

// ReconOtherReceipt 是其他收款（FR-05）。
type ReconOtherReceipt struct {
	ID          string          `gorm:"primaryKey;column:id;type:varchar(36)" json:"id"`
	BranchID    string          `gorm:"column:branch_id;type:varchar(32);not null;index:idx_receipt_branch_date,priority:1" json:"branch_id"`
	BizDate     time.Time       `gorm:"column:biz_date;type:date;not null;index:idx_receipt_branch_date,priority:2" json:"biz_date"`
	Amount      decimal.Decimal `gorm:"column:amount;type:numeric(18,4);not null;default:0" json:"amount"`
	Method      string          `gorm:"column:method;type:varchar(16);not null" json:"method"`
	Payer       string          `gorm:"column:payer;type:varchar(255)" json:"payer"`
	Reason      string          `gorm:"column:reason;type:varchar(512)" json:"reason"`
	Source      string          `gorm:"column:source;type:varchar(16);not null;default:manual" json:"source"`
	Status      string          `gorm:"column:status;type:varchar(16);not null;default:draft;index" json:"status"`
	ConfirmedBy string          `gorm:"column:confirmed_by;type:varchar(64)" json:"confirmed_by"`
	CreatedAt   time.Time       `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt   time.Time       `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式指定表名。
func (ReconOtherReceipt) TableName() string { return "recon_other_receipts" }

// ReconAttachment 是收款凭证照片。
//
// ⚠️ 一期存 PG bytea（单文件 ≤5MB、每单 ≤5 个）；二期换对象存储时
// 只需新增 storage_key 列并保留本表作为元数据。
type ReconAttachment struct {
	ID          string    `gorm:"primaryKey;column:id;type:varchar(36)" json:"id"`
	ReceiptID   string    `gorm:"column:receipt_id;type:varchar(36);not null;index:idx_attachment_receipt" json:"receipt_id"`
	FileName    string    `gorm:"column:file_name;type:varchar(255)" json:"file_name"`
	ContentType string    `gorm:"column:content_type;type:varchar(64)" json:"content_type"`
	SizeBytes   int64     `gorm:"column:size_bytes;not null;default:0" json:"size_bytes"`
	Data        []byte    `gorm:"column:data;type:bytea" json:"-"`
	CreatedAt   time.Time `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName 显式指定表名。
func (ReconAttachment) TableName() string { return "recon_attachments" }

// ReconMatch 是一条匹配结果。
type ReconMatch struct {
	ID         string    `gorm:"primaryKey;column:id;type:varchar(36)" json:"id"`
	BatchID    string    `gorm:"column:batch_id;type:varchar(36);not null;uniqueIndex:ux_match_dedupe,priority:1" json:"batch_id"`
	BranchID   string    `gorm:"column:branch_id;type:varchar(32);not null;index" json:"branch_id"`
	Channel    string    `gorm:"column:channel;type:varchar(32);not null;index" json:"channel"`
	BizDate    time.Time `gorm:"column:biz_date;type:date;not null" json:"biz_date"`
	BankFlowID string    `gorm:"column:bank_flow_id;type:varchar(36)" json:"bank_flow_id"`
	// CashKey 是收银侧聚合键（pay_way@yyyymmdd）。
	CashKey        string          `gorm:"column:cash_key;type:varchar(64)" json:"cash_key"`
	ExpectedAmount decimal.Decimal `gorm:"column:expected_amount;type:numeric(18,4);not null;default:0" json:"expected_amount"`
	ActualAmount   decimal.Decimal `gorm:"column:actual_amount;type:numeric(18,4);not null;default:0" json:"actual_amount"`
	// Fee 永远单列（红线 3）。
	Fee       decimal.Decimal `gorm:"column:fee;type:numeric(18,4);not null;default:0" json:"fee"`
	Kind      string          `gorm:"column:kind;type:varchar(16);not null" json:"kind"`
	DedupeKey string          `gorm:"column:dedupe_key;type:varchar(64);not null;uniqueIndex:ux_match_dedupe,priority:2" json:"dedupe_key"`
	CreatedAt time.Time       `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName 显式指定表名。
func (ReconMatch) TableName() string { return "recon_matches" }

// ReconDiff 是一条差异（FR-10）。
//
// ⚠️ **没有 DELETE**：status 只前进不后退；人工动作写 recon_audit_logs。
type ReconDiff struct {
	ID       string          `gorm:"primaryKey;column:id;type:varchar(36)" json:"id"`
	BatchID  string          `gorm:"column:batch_id;type:varchar(36);not null;uniqueIndex:ux_diff_dedupe,priority:1" json:"batch_id"`
	BranchID string          `gorm:"column:branch_id;type:varchar(32);not null;index:idx_diff_branch_status,priority:1" json:"branch_id"`
	Kind     string          `gorm:"column:kind;type:varchar(24);not null" json:"kind"`
	Channel  string          `gorm:"column:channel;type:varchar(32)" json:"channel"`
	CashRef  string          `gorm:"column:cash_ref;type:varchar(64)" json:"cash_ref"`
	BankRef  string          `gorm:"column:bank_ref;type:varchar(64)" json:"bank_ref"`
	Expected decimal.Decimal `gorm:"column:expected;type:numeric(18,4);not null;default:0" json:"expected"`
	Actual   decimal.Decimal `gorm:"column:actual;type:numeric(18,4);not null;default:0" json:"actual"`
	// DiffAmount = actual - expected（正数 = 多到账）。
	DiffAmount     decimal.Decimal `gorm:"column:diff_amount;type:numeric(18,4);not null;default:0" json:"diff_amount"`
	Status         string          `gorm:"column:status;type:varchar(16);not null;default:open;index:idx_diff_branch_status,priority:2" json:"status"`
	ResolutionCode string          `gorm:"column:resolution_code;type:varchar(32)" json:"resolution_code"`
	ResolutionNote string          `gorm:"column:resolution_note;type:varchar(512)" json:"resolution_note"`
	HandledBy      string          `gorm:"column:handled_by;type:varchar(64)" json:"handled_by"`
	HandledAt      *time.Time      `gorm:"column:handled_at" json:"handled_at"`
	DedupeKey      string          `gorm:"column:dedupe_key;type:varchar(64);not null;uniqueIndex:ux_diff_dedupe,priority:2" json:"dedupe_key"`
	CreatedAt      time.Time       `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt      time.Time       `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式指定表名。
func (ReconDiff) TableName() string { return "recon_diffs" }

// ReconCashCount 是某门店某日某收银员的现金实盘（FR-04）。
type ReconCashCount struct {
	ID           string          `gorm:"primaryKey;column:id;type:varchar(36)" json:"id"`
	BranchID     string          `gorm:"column:branch_id;type:varchar(32);not null;uniqueIndex:ux_cash_count,priority:1" json:"branch_id"`
	BizDate      time.Time       `gorm:"column:biz_date;type:date;not null;uniqueIndex:ux_cash_count,priority:2" json:"biz_date"`
	CashierID    string          `gorm:"column:cashier_id;type:varchar(32);not null;uniqueIndex:ux_cash_count,priority:3" json:"cashier_id"`
	ExpectedCash decimal.Decimal `gorm:"column:expected_cash;type:numeric(18,4);not null;default:0" json:"expected_cash"`
	CountedCash  decimal.Decimal `gorm:"column:counted_cash;type:numeric(18,4);not null;default:0" json:"counted_cash"`
	Variance     decimal.Decimal `gorm:"column:variance;type:numeric(18,4);not null;default:0" json:"variance"`
	Tolerance    decimal.Decimal `gorm:"column:tolerance;type:numeric(18,4);not null;default:0" json:"tolerance"`
	Status       string          `gorm:"column:status;type:varchar(16);not null;default:open;index" json:"status"`
	ConfirmBy    string          `gorm:"column:confirm_by;type:varchar(64)" json:"confirm_by"`
	ConfirmAt    *time.Time      `gorm:"column:confirm_at" json:"confirm_at"`
	ReasonCode   string          `gorm:"column:reason_code;type:varchar(32)" json:"reason_code"`
	CreatedAt    time.Time       `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt    time.Time       `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式指定表名。
func (ReconCashCount) TableName() string { return "recon_cash_counts" }

// ReconCashVariance 是长短款台账（按收银员可汇总）。
type ReconCashVariance struct {
	ID         string          `gorm:"primaryKey;column:id;type:varchar(36)" json:"id"`
	BranchID   string          `gorm:"column:branch_id;type:varchar(32);not null;index:idx_variance_branch_date,priority:1" json:"branch_id"`
	BizDate    time.Time       `gorm:"column:biz_date;type:date;not null;index:idx_variance_branch_date,priority:2" json:"biz_date"`
	CashierID  string          `gorm:"column:cashier_id;type:varchar(32);not null;index:idx_variance_cashier" json:"cashier_id"`
	Kind       string          `gorm:"column:kind;type:varchar(8);not null" json:"kind"`
	Amount     decimal.Decimal `gorm:"column:amount;type:numeric(18,4);not null;default:0" json:"amount"`
	ReasonCode string          `gorm:"column:reason_code;type:varchar(32)" json:"reason_code"`
	Status     string          `gorm:"column:status;type:varchar(16);not null;default:open;index" json:"status"`
	CreatedAt  time.Time       `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName 显式指定表名。
func (ReconCashVariance) TableName() string { return "recon_cash_variances" }

// ReconPaymentRecord 是付款登记/核销台（FR-07）。
type ReconPaymentRecord struct {
	ID           string          `gorm:"primaryKey;column:id;type:varchar(36)" json:"id"`
	BranchID     string          `gorm:"column:branch_id;type:varchar(32);not null;uniqueIndex:ux_pay_source,priority:1;index:idx_pay_branch_date,priority:1" json:"branch_id"`
	Source       string          `gorm:"column:source;type:varchar(16);not null;uniqueIndex:ux_pay_source,priority:2" json:"source"`
	SourceID     string          `gorm:"column:source_id;type:varchar(64);not null;uniqueIndex:ux_pay_source,priority:3" json:"source_id"`
	SupplierName string          `gorm:"column:supplier_name;type:varchar(255)" json:"supplier_name"`
	Amount       decimal.Decimal `gorm:"column:amount;type:numeric(18,4);not null;default:0" json:"amount"`
	RequestDate  *time.Time      `gorm:"column:request_date;type:date" json:"request_date"`
	PayDate      *time.Time      `gorm:"column:pay_date;type:date;index:idx_pay_branch_date,priority:2" json:"pay_date"`
	Method       string          `gorm:"column:method;type:varchar(16)" json:"method"`
	BankFlowID   string          `gorm:"column:bank_flow_id;type:varchar(36)" json:"bank_flow_id"`
	Verified     bool            `gorm:"column:verified;not null;default:false;index" json:"verified"`
	VerifiedBy   string          `gorm:"column:verified_by;type:varchar(64)" json:"verified_by"`
	Note         string          `gorm:"column:note;type:varchar(512)" json:"note"`
	CreatedAt    time.Time       `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt    time.Time       `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式指定表名。
func (ReconPaymentRecord) TableName() string { return "recon_payment_records" }

// ReconAuditLog 是人工覆盖审计（FR-11，append-only）。
type ReconAuditLog struct {
	ID         string    `gorm:"primaryKey;column:id;type:varchar(36)" json:"id"`
	BranchID   string    `gorm:"column:branch_id;type:varchar(32);not null;index:idx_audit_branch_time,priority:1" json:"branch_id"`
	ObjectType string    `gorm:"column:object_type;type:varchar(32);not null;index:idx_audit_object,priority:1" json:"object_type"`
	ObjectID   string    `gorm:"column:object_id;type:varchar(64);not null;index:idx_audit_object,priority:2" json:"object_id"`
	Action     string    `gorm:"column:action;type:varchar(32);not null" json:"action"`
	BeforeJSON string    `gorm:"column:before_json;type:text" json:"before_json"`
	AfterJSON  string    `gorm:"column:after_json;type:text" json:"after_json"`
	Reason     string    `gorm:"column:reason;type:varchar(512)" json:"reason"`
	Actor      string    `gorm:"column:actor;type:varchar(64)" json:"actor"`
	ActorName  string    `gorm:"column:actor_name;type:varchar(64)" json:"actor_name"`
	CreatedAt  time.Time `gorm:"column:created_at;not null;index:idx_audit_branch_time,priority:2" json:"created_at"`
}

// TableName 显式指定表名。
func (ReconAuditLog) TableName() string { return "recon_audit_logs" }

// ReconRule 是规则配置（FR-16）。
//
// ⚠️ BranchID 用**空串**表示全局默认值（而不是 NULL）：
// NULL 在 PG 唯一索引里不参与去重，会让"同一规则被插两次都成功"，
// 而空串让 ux_rule 真正生效。
type ReconRule struct {
	ID        string    `gorm:"primaryKey;column:id;type:varchar(36)" json:"id"`
	BranchID  string    `gorm:"column:branch_id;type:varchar(32);not null;default:'';uniqueIndex:ux_rule,priority:1" json:"branch_id"`
	RuleKey   string    `gorm:"column:rule_key;type:varchar(64);not null;uniqueIndex:ux_rule,priority:2" json:"rule_key"`
	ValueJSON string    `gorm:"column:value_json;type:text;not null" json:"value_json"`
	UpdatedBy string    `gorm:"column:updated_by;type:varchar(64)" json:"updated_by"`
	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式指定表名。
func (ReconRule) TableName() string { return "recon_rules" }

// ReconPeriodSnapshot 是期末快照（下期期初取上一期 closing）。
type ReconPeriodSnapshot struct {
	ID               string          `gorm:"primaryKey;column:id;type:varchar(36)" json:"id"`
	BranchID         string          `gorm:"column:branch_id;type:varchar(32);not null;uniqueIndex:ux_snapshot,priority:1" json:"branch_id"`
	PeriodStart      time.Time       `gorm:"column:period_start;type:date;not null;uniqueIndex:ux_snapshot,priority:2" json:"period_start"`
	AccountType      string          `gorm:"column:account_type;type:varchar(16);not null;uniqueIndex:ux_snapshot,priority:3" json:"account_type"`
	AccountKey       string          `gorm:"column:account_key;type:varchar(64);not null;uniqueIndex:ux_snapshot,priority:4" json:"account_key"`
	PeriodEnd        time.Time       `gorm:"column:period_end;type:date;not null" json:"period_end"`
	Opening          decimal.Decimal `gorm:"column:opening;type:numeric(18,4);not null;default:0" json:"opening"`
	OpeningSource    string          `gorm:"column:opening_source;type:varchar(16);not null;default:manual" json:"opening_source"`
	AdjustmentAmount decimal.Decimal `gorm:"column:adjustment_amount;type:numeric(18,4);not null;default:0" json:"adjustment_amount"`
	Inflow           decimal.Decimal `gorm:"column:inflow;type:numeric(18,4);not null;default:0" json:"inflow"`
	Outflow          decimal.Decimal `gorm:"column:outflow;type:numeric(18,4);not null;default:0" json:"outflow"`
	Fee              decimal.Decimal `gorm:"column:fee;type:numeric(18,4);not null;default:0" json:"fee"`
	Closing          decimal.Decimal `gorm:"column:closing;type:numeric(18,4);not null;default:0" json:"closing"`
	SourceBatchID    string          `gorm:"column:source_batch_id;type:varchar(36);index" json:"source_batch_id"`
	CreatedAt        time.Time       `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName 显式指定表名。
func (ReconPeriodSnapshot) TableName() string { return "recon_period_snapshots" }

// ReconPeriodLock 是冻结区间（写操作前置校验 409 period_frozen）。
type ReconPeriodLock struct {
	ID          string    `gorm:"primaryKey;column:id;type:varchar(36)" json:"id"`
	BranchID    string    `gorm:"column:branch_id;type:varchar(32);not null;index:idx_lock_branch_range,priority:1" json:"branch_id"`
	BizDateFrom time.Time `gorm:"column:biz_date_from;type:date;not null;index:idx_lock_branch_range,priority:2" json:"biz_date_from"`
	BizDateTo   time.Time `gorm:"column:biz_date_to;type:date;not null;index:idx_lock_branch_range,priority:3" json:"biz_date_to"`
	BatchID     string    `gorm:"column:batch_id;type:varchar(36);not null" json:"batch_id"`
	LockedAt    time.Time `gorm:"column:locked_at;not null" json:"locked_at"`
}

// TableName 显式指定表名。
func (ReconPeriodLock) TableName() string { return "recon_period_locks" }

// ReconAccount 是账户台账（现金 / 银行 / 渠道）。
type ReconAccount struct {
	ID          string `gorm:"primaryKey;column:id;type:varchar(36)" json:"id"`
	BranchID    string `gorm:"column:branch_id;type:varchar(32);not null;uniqueIndex:ux_account,priority:1" json:"branch_id"`
	AccountType string `gorm:"column:account_type;type:varchar(16);not null;uniqueIndex:ux_account,priority:2" json:"account_type"`
	AccountKey  string `gorm:"column:account_key;type:varchar(64);not null;uniqueIndex:ux_account,priority:3" json:"account_key"`
	Name        string `gorm:"column:name;type:varchar(128);not null" json:"name"`
	Currency    string `gorm:"column:currency;type:varchar(8);not null;default:CNY" json:"currency"`
	IsActive    bool   `gorm:"column:is_active;not null;default:true" json:"is_active"`
	SortOrder   int    `gorm:"column:sort_order;not null;default:0" json:"sort_order"`
	// Note 记录建账锚点（中途启用时写"以某对账单日为锚点"）。
	Note      string    `gorm:"column:note;type:varchar(512)" json:"note"`
	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式指定表名。
func (ReconAccount) TableName() string { return "recon_accounts" }

// ReconOpeningBalance 是期初余额（append-only）。
//
// ⚠️ 修正历史**不 UPDATE**：写一条新的 adjust 记录，或对错误记录 void；
// 两者都写 recon_audit_logs。
type ReconOpeningBalance struct {
	ID            string          `gorm:"primaryKey;column:id;type:varchar(36)" json:"id"`
	BranchID      string          `gorm:"column:branch_id;type:varchar(32);not null;index:idx_opening_lookup,priority:1;uniqueIndex:ux_opening,priority:1" json:"branch_id"`
	AccountType   string          `gorm:"column:account_type;type:varchar(16);not null;index:idx_opening_lookup,priority:2;uniqueIndex:ux_opening,priority:2" json:"account_type"`
	AccountKey    string          `gorm:"column:account_key;type:varchar(64);not null;index:idx_opening_lookup,priority:3;uniqueIndex:ux_opening,priority:3" json:"account_key"`
	EffectiveDate time.Time       `gorm:"column:effective_date;type:date;not null;index:idx_opening_lookup,priority:4;uniqueIndex:ux_opening,priority:4" json:"effective_date"`
	Amount        decimal.Decimal `gorm:"column:amount;type:numeric(18,4);not null;default:0" json:"amount"`
	Source        string          `gorm:"column:source;type:varchar(16);not null;uniqueIndex:ux_opening,priority:5" json:"source"`
	BatchID       string          `gorm:"column:batch_id;type:varchar(36)" json:"batch_id"`
	Note          string          `gorm:"column:note;type:varchar(512)" json:"note"`
	VoidedAt      *time.Time      `gorm:"column:voided_at" json:"voided_at"`
	VoidedBy      string          `gorm:"column:voided_by;type:varchar(64)" json:"voided_by"`
	VoidReason    string          `gorm:"column:void_reason;type:varchar(512)" json:"void_reason"`
	CreatedBy     string          `gorm:"column:created_by;type:varchar(64)" json:"created_by"`
	CreatedAt     time.Time       `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName 显式指定表名。
func (ReconOpeningBalance) TableName() string { return "recon_opening_balances" }
