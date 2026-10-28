package model

import "time"

// ApprovalPayeeAccount 是「本次付款付给谁、付到哪个账户」的快照。
//
// # ⚠️ 为什么**独立建表**，而不是在 approval_instances 上加两列
//
// 用户口径（2026-10-28）：「settlement-auditor 服务的付款审批模块，
// **不向机器人审核（LLM）发送账号信息**。」
//
// 这条要求**必须靠结构保证**，不能靠"记得别把它塞进 prompt"。
//
// 现状：`AuditInput.Approval` 是 `*model.ApprovalInstance` **整体**
// （见 service/llm.go:40 与 pipeline.go:724）。
// ⇒ 只要账号信息挂在 ApprovalInstance 上，它就**自动在 LLM 的输入面里**：
//   - `buildAuditUserPrompt` 现在没打印它 ⇒ 安全；
//   - 但明天有人加一行 `fmt.Fprintf(&sb, "…%v", ap)` 就漏了；
//   - 而且这种泄漏**没有任何测试会红**（prompt 文本变了，但没人断言"不含账号"）。
//
// ⇒ 独立表 ⇒ `ApprovalInstance` 里没有账号字段 ⇒
//   `AuditInput.Approval` 里**结构上就没有**账号信息 ⇒
//   无论 prompt 怎么改都不可能带上账号。
//
// 这是"把约束编码进类型"而不是"编码进纪律"。
//
// # 这张表存的是什么
//
// 它是**提交那一刻**的收款账号快照（开户行 / 账号 / 户名），
// 不是引用：账号之后可能被改被删，但审批单上要显示**当时付到哪个户**。
// 语义与 ApprovalSettlement.GoodsTotalYuan（cube 金额快照）同款。
//
// # 数据来源
//
// 发起人在 H5「申请付款审批」页选了供应商收款账号 ⇒ 前端传
// `bank_account_id` ⇒ **服务端**（本服务）按 id 回查 bank-account 服务拿
// 开户行/账号/户名并落这张表。
//
// ⚠️ 前端**只传 id**，不传账号文本 —— 否则客户端可以伪造收款账号。
type ApprovalPayeeAccount struct {
	ID          string `gorm:"primaryKey;size:36" json:"id"`
	ApprovalID  string `gorm:"size:32;index" json:"approval_id"`
	BranchID    string `gorm:"size:32;index" json:"branch_id"`
	ApplicantUserID string `gorm:"size:64;index" json:"applicant_user_id"`
	// BankAccountID 是所选收款账号在 bank-account 服务里的 id。
	//
	// ⚠️ 它是**跨服务引用**，不是本库外键(那张表在 bank-account 服务的域里)。
	BankAccountID string `gorm:"size:36;index" json:"bank_account_id"`
	// SupplierID 是供应商 id(快照)。
	//
	// ⚠️ 命名空间按"有无 cube 源"决定(有源→cube id / 无源→本地 suppliers id),
	// 与 bank-account 服务的规则一致 —— 见该服务的 model 包注释。
	SupplierID string `gorm:"size:64;index" json:"supplier_id"`
	// SupplierName 是供应商名(快照,展示用)。
	SupplierName string `gorm:"size:255" json:"supplier_name"`
	// BankName 是**开户行**。
	BankName string `gorm:"size:128" json:"bank_name"`
	// AccountNo 是**银行账号**。
	AccountNo string `gorm:"size:64" json:"account_no"`
	// AccountName 是**户名**。
	AccountName string `gorm:"size:128" json:"account_name"`
	// CreatedAt 是落库时刻。
	CreatedAt time.Time `json:"created_at"`
}

// TableName 显式指定表名。
func (ApprovalPayeeAccount) TableName() string { return "approval_payee_accounts" }
