// Package model 定义 bank-account 服务的 GORM 模型。
//
// # 域边界：这是「门店付给供应商的钱怎么走」的档案
//
// 收款银行账号 = **门店付款给供应商**时的账户信息（开户行 / 账号 / 户名）。
// 与「供应商档案」是两件事：
//   - 供应商档案（名字 / 联系人 / 电话）在 catalog 服务的 suppliers 表；
//   - 收款账号（本表）是**付款执行**用的信息。
//
// # ⚠️ 本服务的读路径**永远查本地 PG**，不看 cube
//
// 这是与 catalog / procurement 等服务的**故意不同**：
//
//	catalog   → 有源查 cube、无源查本地 pg（供应商档案在外部系统里）
//	本服务     → 不管有无 cube 源，**一律查本地 pg**
//
// 为什么：收款账号是**本系统自己产生的业务数据**（不是外部 ERP 的镜像）。
// 一个有 cube 源的门店，其供应商档案在 sixun 里，但「我们付钱到哪个账户」
// 是本系统的独立事实 ⇒ 必须落在本域 PG，不能跟着 cube 走。
//
// # 供应商 id 的绑定规则（本表的核心语义）
//
//	supplier_id 指向哪个命名空间，取决于**该门店有没有 cube 源**：
//	  - 有 cube 源   → 绑定 **cube 源**的 supplier id
//	  - 没有 cube 源 → 绑定 **pg 本地表** suppliers 的 supplier id
//
// ⚠️ **判据只有一个**：internal/cubesource（见 catalog 的 source.go）。
// 本服务**不自己**判断"有没有 cube 源"——
// 判据散落在多个服务里就会出现"同一门店在不同服务里 id 空间不一样"。
//
// # 门店 10 的特例
//
// 门店 10（自营店）**没有** cube 源，它的供应商是从门店 00 导入到本地
// suppliers 表的 ⇒ 它落在「无 cube 源」这一侧，绑定**本地** supplier id。
// 而同一个供应商在门店 00（有 cube 源）绑定的是 cube 的 supplier id。
// ⇒ **同一供应商在不同门店的 supplier_id 可能不同**，这是设计使然，不是 bug。
//
// # 索引设计要点
//
//   - 复合唯一 idx_supplier_account(branch_id, supplier_id, account_no):
//     同一门店同一供应商同一账号只能有一条（软删后可重建）
//   - idx_bank_account_supplier(branch_id, supplier_id):按供应商查
//     （H5 付款审批页要问"这个供应商有哪些收款账号"）
//
// ⚠️ **唯一索引必须含 branch_id**：不同门店是各自维护的账号档案
// （同一家供应商在门店 00 和门店 10 可能有同一个银行账号），
// 漏掉 branch_id 会让"门店 10 建账号"被门店 00 的行挡掉。
package model

import "time"

// BankAccount 是供应商收款银行账号的一行。
//
// # ⚠️ json tag 是**契约的一部分**,不是可选装饰
//
// 本模型**必须**带 json tag(snake_case),因为它是本服务的**出参**:
// handler 直接把 model 序列化给前端(c.JSON(..., rows))。
//
// ⚠️ supertrade 里有两套"wire 风格":
//   - **cube 查询结果**(如 settlement-auditor 的 SettlementRow)⇒ 没有 json tag,
//     wire 上就是 Go 导出名(ID / SupplierID / SupplierName);
//   - **本域自己的模型**(如本表)⇒ 带 snake_case json tag。
//   两者混用会让前端"字段永远读不到" ——
//   前端按 a.id / a.account_no 取值,而 wire 上是 ID / AccountNo
//   ⇒ 取到 undefined ⇒ 列表被过滤成空 ⇒ 页面显示"暂无可选收款账号",
//   **且零报错**(HTTP 200 + items 数组,只是内容对不上)。
//
// ⇒ 前端契约(两个前端都按它写):
//
//	id / branch_id / supplier_id / supplier_name / bank_name / account_no /
//	account_name / is_default / remark / created_at / updated_at
type BankAccount struct {
	// ID 是本表主键(uuid v4,36 字符)。
	ID string `gorm:"primaryKey;column:id;type:varchar(36)" json:"id"`
	// BranchID 是所属门店 id(外键到 auth.branches,业务上不建约束)。
	//
	// ⚠️ 它是唯一索引的**第一段** ⇒ 不同门店可各自维护同一个银行账号。
	BranchID string `gorm:"column:branch_id;type:varchar(64);not null;index:idx_bank_account_supplier;uniqueIndex:idx_supplier_account,priority:1" json:"branch_id"`
	// SupplierID 是供应商 id,命名空间按"有无 cube 源"决定(见包注释)。
	//
	// ⚠️ **不建外键**:supplier_id 可能指向 cube 的 supplier 维度
	// (有源门店) ⇒ 本地表里根本没有那一行,建外键会插不进数据。
	SupplierID string `gorm:"column:supplier_id;type:varchar(64);not null;index:idx_bank_account_supplier;uniqueIndex:idx_supplier_account,priority:2" json:"supplier_id"`
	// SupplierName 是**供应商名快照**(建/改账号那一刻的显示值)。
	//
	// ⚠️ 故意落库、故意不 JOIN:本服务**不查 cube 拿供应商名**
	// (见 service 包注释"本服务不查 cube 拿供应商名")⇒ 名字只能是快照。
	// 它是**展示用**,不是外键:供应商改名后旧行仍显示旧名,
	// 重新编辑该账号即刷新(与 settlement-auditor 的 SupplierName 同款处理)。
	SupplierName string `gorm:"column:supplier_name;type:varchar(255)" json:"supplier_name"`
	// BankName 是**开户行**(如"中国银行南宁分行")。
	BankName string `gorm:"column:bank_name;type:varchar(128);not null" json:"bank_name"`
	// AccountNo 是**银行账号**(数字串,用字符串存以免丢前导零)。
	AccountNo string `gorm:"column:account_no;type:varchar(64);not null;uniqueIndex:idx_supplier_account,priority:3" json:"account_no"`
	// AccountName 是**户名**(账户名,如"某某贸易有限公司")。
	AccountName string `gorm:"column:account_name;type:varchar(128);not null" json:"account_name"`
	// IsDefault 是是否该供应商的**默认收款账号**。
	//
	// ⚠️ 同一 (branch_id, supplier_id) 下最多一条 default
	// (由 service 层保证,见 SetDefault 的事务逻辑)。
	IsDefault bool `gorm:"column:is_default;type:boolean;not null;default:false" json:"is_default"`
	// Remark 是备注(可选,如"对公账户"/"仅限货款")。
	Remark string `gorm:"column:remark;type:varchar(512)" json:"remark"`
	// CreatedBy / UpdatedBy 是操作人 auth user id。
	CreatedBy string `gorm:"column:created_by;type:varchar(64);not null" json:"created_by"`
	UpdatedBy string `gorm:"column:updated_by;type:varchar(64);not null" json:"updated_by"`
	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
	// DeletedAt 是软删标记(nil = 未删)。
	//
	// ⚠️ 唯一索引 idx_supplier_account 含 account_no ⇒ 软删后**不能**再用
	// 同一账号号建行(否则撞唯一键)。
	// ⇒ service 层对软删行的复用采取"复活"语义(见 Upsert 的说明)。
	DeletedAt *time.Time `gorm:"column:deleted_at;index" json:"deleted_at"`
}

// TableName 显式指定表名。
func (BankAccount) TableName() string { return "supplier_bank_accounts" }
