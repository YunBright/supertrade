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
	// ApproverUserIDs / HandlerUserIDs / CcUserIDs 是本次发起时申请人在
	// H5 用企微原生选人器选定的**各节点成员**(企微 userid 列表,JSON 数组),
	// 随单快照;提交企微时映射为 process.node_list
	// (type=1 审批人 / type=3 办理人 / type=2 抄送人)。
	//
	// ⚠️ 三个节点都存在是因为企微模板的流程里三个节点都是「申请人自选」——
	// 成员由发起人在提交时选,不是模板写死。缺一个节点 = 企微那边少一个人
	// (办理人就是这样丢的)。
	ApproverUserIDs datatypes.JSON `gorm:"type:jsonb" json:"approver_userids"`
	HandlerUserIDs  datatypes.JSON `gorm:"type:jsonb" json:"handler_userids"`
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
	ApplicantBearer string `gorm:"size:2048" json:"-"`
	RejectReason    string `gorm:"size:1024" json:"reject_reason"`
	Error           string `gorm:"size:1024" json:"error"`
	// 溯源走旁表 testrun_rows(见 core/testrun),业务表零改动。
	//
	// 本服务的审批是**异步**的(POST 返 202 后跑流水线,一次提交会写
	// approval_settlements / audit_signals / audit_logs / llm_verdicts 等
	// 多张表),但每张表都被独立记账 —— 收尾 sweep 不需要"先认领主表再级联",
	// 直接按各自记下的主键删即可,也就没有"从表排在主表之前"那条顺序约束。
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `gorm:"index" json:"updated_at"`
}

func (ApprovalInstance) TableName() string { return "approval_instances" }

// ApprovalSettlement 审批 ↔ 结算单多对多(含 cube 侧货款金额快照)。
type ApprovalSettlement struct {
	ID           string `gorm:"primaryKey;size:36" json:"id"`
	ApprovalID   string `gorm:"size:32;index" json:"approval_id"`
	SettlementID string `gorm:"size:64;index" json:"settlement_id"`
	SupplierID   string `gorm:"size:64" json:"supplier_id"`
	// SupplierName 是**读时补上**的供应商名称(不落库,gorm:"-" 不参与读写)。
	//
	// # 为什么不给它加列
	//
	// 审批快照表的语义是"这笔审批勾了哪些单、货款多少"。供应商名称是
	// **字典里的当前值**,不是审批当时的值:
	//   - 历史行要回填也只能拿**当前**字典名回填 ⇒ 并不比读时 join 更"准",
	//     一样不是历史快照;
	//   - 而加列意味着:生产表迁移 + 历史回填 + 把审核流水线
	//     (最敏感的那条路,runAudit)也拖进来取名字。
	// ⇒ 详情端点读出来时用 Go 侧 join 补(cube JOIN 不了 live↔duck,
	//   见 service/cube.go 的 fillSupplierNames)。
	//
	// ⚠️ 读时 join 拿不到(cube 不可用)时该字段为空串,详情页其余内容照常。
	SupplierName    string          `gorm:"-" json:"supplier_name"`
	DocumentType    string          `gorm:"size:8" json:"document_type"` // DP/CP
	GoodsAmountYuan decimal.Decimal `gorm:"type:numeric(14,2)" json:"goods_amount_yuan"`
	// Checked 表示机器审核时该结算单在 cube 中存在且已审核。
	Checked   bool      `json:"checked"`
	CreatedAt time.Time `json:"created_at"`
}

func (ApprovalSettlement) TableName() string { return "approval_settlements" }

// ApprovalAttachment 审批附件（**发起人自己上传**的图片/文件）。
//
// # 为什么落库
//
// 机器审核是**异步**的（go s.runAudit），而企微「附件」控件的素材必须由
// wechat-bot 用 **OA token** 上传（media_id 绑定上传它的应用，别的应用用不了）
// ⇒ 字节必须从"提交那一刻"一路活到"机器审核通过、准备提交企微"那一刻。
// 提交请求返回后前端就走了 ⇒ 只能落库。
//
// # 只存发起人上传的
//
// 自动生成的「结算单明细清单」是机器审核通过那一刻才生成的（用 cube 的当前金额），
// 不入库，直接在 pass 分支现算现传。
type ApprovalAttachment struct {
	ID             string `gorm:"primaryKey;size:36" json:"id"`
	ApprovalID     string `gorm:"size:32;index" json:"approval_id"`
	BranchID       string `gorm:"size:32;index" json:"branch_id"`
	ApplicantUserID string `gorm:"size:64;index" json:"applicant_user_id"`
	// FileName 是文件名（审批单上显示的名字）；已去掉路径分隔符。
	FileName string `gorm:"size:256" json:"file_name"`
	// ContentType 是 MIME（image/jpeg 等），给下游判断用。
	ContentType string `gorm:"size:128" json:"content_type"`
	// Size 是字节数（校验过上限）。
	Size int `json:"size"`
	// Data 是文件内容。
	//
	// ⚠️ json:"-" ⇒ **不出现在任何 API 响应里**（几百 KB 的图片不该进审批详情 JSON）。
	Data      []byte    `gorm:"type:bytea" json:"-"`
	CreatedAt time.Time `json:"created_at"`
}

func (ApprovalAttachment) TableName() string { return "approval_attachments" }

// ApprovalItem 付款审批里的**手工收付款明细行**（2026-10-25 用户需求）。
//
// # 业务形态
//
// 「付款审批」不再只有"付供应商货款"这一种：门店还会有
// 付款项（水电费 / 租金 / 物业费 / 垃圾处理费 / 软件服务费 / 其它）与
// 收款项（陈列费 / 堆头费 / 补差价 / 兑换 / 纸皮回收 / 其它）。
// 收款项**冲减**付款金额 ⇒ 应付金额 = 货款 + 付款项 − 收款项（不为负）。
//
// # 为什么只存"手工行"，货款不存
//
// 货款来自 ERP 结算单（cube），事实源是 approval_settlements 快照 ——
// 再抄一份进明细表就会出现**两个金额口径**，迟早对不上。
// ⇒ 明细表只存人填的行；货款行在读取/审核时**派生**（类型固定「货款」）。
//
// # 为什么类型/备注是文本而不是枚举
//
// 用户口径：「收款类型」「备注」**皆保存为文本**。而且「其它」允许用户
// 把"其它"两字改成自定义文字（如"临期处理费"）⇒ 用枚举/字典表就得先建
// 码表才能存一个字。文本列 ⇒ 新增类型不改表、不迁移。
type ApprovalItem struct {
	ID              string `gorm:"primaryKey;size:36" json:"id"`
	ApprovalID      string `gorm:"size:32;index" json:"approval_id"`
	BranchID        string `gorm:"size:32;index" json:"branch_id"`
	ApplicantUserID string `gorm:"size:64;index" json:"applicant_user_id"`
	// Direction 是方向：payment=付款项 / receipt=收款项。
	Direction string `gorm:"size:16;index" json:"direction"`
	// ItemType 是**类型文本**（"陈列费"/"水电费"/自定义文字…）。
	ItemType string `gorm:"size:64" json:"item_type"`
	// Remark 是备注（提示用户填写所属日期等补充信息）。
	Remark string `gorm:"size:256" json:"remark"`
	// AmountYuan 是金额（元）。恒 > 0（校验在 service 层）。
	AmountYuan decimal.Decimal `gorm:"type:numeric(14,2)" json:"amount_yuan"`
	CreatedAt  time.Time       `json:"created_at"`
}

func (ApprovalItem) TableName() string { return "approval_items" }

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
	ID              string         `gorm:"primaryKey;size:128" json:"id"`
	BranchID        string         `gorm:"size:32;index" json:"branch_id"`
	ApplicantUserID string         `gorm:"size:64;index" json:"applicant_user_id"`
	ApproverUserIDs datatypes.JSON `gorm:"type:jsonb" json:"approver_userids"`
	HandlerUserIDs  datatypes.JSON `gorm:"type:jsonb" json:"handler_userids"`
	CcUserIDs       datatypes.JSON `gorm:"type:jsonb" json:"cc_userids"`
	UpdatedAt       time.Time      `gorm:"index" json:"updated_at"`
}

func (ApprovalApproverPreference) TableName() string { return "approval_approver_preferences" }

// ---- 门店级「盘点计划推送群」配置 ----

// StocktakeNotifyGroup 是某个门店的"结算审核发现库存异常 → 盘点计划推送到哪个群"的配置。
//
// # 为什么存 app_id + chat_id 两个字段
//
// 企业微信的群(appchat)有一个硬约束:**chatid 必须由持有当前 Secret 的那个自建应用创建**,
// 用别的应用的 access_token 发这个群会被拒(301002 一族)。
// ⇒ 只存 chat_id 是不够的,必须连"这个群属于哪个自建应用"一起存。
//
// # 谁来填
//
// Flutter 管理后台 →「仓库-设置」→ 选定门店 → 点「设置盘点计划推送群」
// → 层级选择控件(自建应用 → 群聊)→ 保存。
type StocktakeNotifyGroup struct {
	// BranchID 是门店 id(主键:一店一条)。
	BranchID string `gorm:"primaryKey;column:branch_id;type:varchar(64)"`
	// ChatID 是企业微信群 id(appchat 的 chatid)。
	ChatID string `gorm:"column:chat_id;type:varchar(64)"`
	// ChatName 是群名(只用于回显,发消息不用)。
	ChatName string `gorm:"column:chat_name;type:varchar(128)"`
	// AppID 是这个群所属的**自建应用** handle(不是 agentid)。
	AppID string `gorm:"column:app_id;type:varchar(64)"`
	// AppName 是自建应用名称(只用于回显)。
	AppName   string    `gorm:"column:app_name;type:varchar(128)"`
	UpdatedBy string    `gorm:"column:updated_by;type:varchar(64)"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (StocktakeNotifyGroup) TableName() string { return "stocktake_notify_groups" }

// ---- 结算付款状态台账(初始化记账 / 已付款过滤的共同事实源) ----

// 付款状态的**设置方式**。
//
// 这一列是本功能存在的理由:同一个 paid 状态有两个来源,业务含义完全不同 ——
//
//	init     初始化记账。管理员批量补录「本系统上线之前就已经付掉款」的结算单。
//	          这些单在思迅源库里查不到任何付款痕迹(hbposv7 的
//	          t_fm_account_sum.pay_flag 实测恒 0,ysx 根本没有这张表),
//	          只能由人按账面补录。补录的人、时刻、筛选口径全部留痕。
//	approval 正常审批流程。审批实例终审通过(human_approved)时由流水线写入,
//	          带上 ApprovalID 关联回那次审批。
//
// 两者混在一起就分不清"这张单是走流程付的,还是有人手工补的",而这正是
// 初始化页要回答的第一个问题。
const (
	PayMethodInit     = "init"
	PayMethodApproval = "approval"
)

// 付款状态取值。
const (
	PayStatusUnpaid = "unpaid"
	PayStatusPaid   = "paid"
)

// SettlementPayState 一张结算单的付款状态(按门店 + 结算单号唯一)。
//
// # 为什么台账在 PG,而不是写回思迅
//
// 实测两个思迅源库都没有可用的付款标记(2026-10-15):
//   - t_fm_recpay_gx_master 的 settle_date 全表有值、get_amt 恒 0、com_flag 恒 0;
//   - 真正登记付款的 t_fm_account_sum(voucher_no=结款单号,fk_sheet_no=付款单号,
//     pay_flag)hbposv7 有这张表但 776 行 pay_flag 全 0、fk_sheet_no 全空;
//   - ysx(hbposv10)**没有** t_fm_account_sum 这张表。
//
// 而 supertrade 的 go.mod 里没有任何 SQL Server 驱动(cube 只读),应用层根本
// 没有写回思迅的通路。所以"已付款"这件事在本系统只能、也只应该落在 PG。
type SettlementPayState struct {
	ID string `gorm:"primaryKey;size:36" json:"id"`
	// BranchID + SettlementID 唯一:cube 的结算单号只在单个门店内唯一,
	// 且源表 sheet_no 是 char(20)(hbposv7 尾部补空格),写入前统一 Trim。
	BranchID     string `gorm:"size:32;uniqueIndex:ux_pay_branch_settlement,priority:1" json:"branch_id"`
	SettlementID string `gorm:"size:64;uniqueIndex:ux_pay_branch_settlement,priority:2" json:"settlement_id"`
	SupplierID   string `gorm:"size:64" json:"supplier_id"`
	DocumentType string `gorm:"size:8" json:"document_type"` // DP/CP
	PaidStatus   string `gorm:"size:16;index" json:"paid_status"`
	SetMethod    string `gorm:"size:16;index" json:"set_method"`
	// OperatorID / OperatorName 是**设置人**:初始化时为当前登录管理员,
	// 审批流程写入时为该审批的发起人(auth user id)。
	OperatorID   string `gorm:"size:64;index" json:"operator_id"`
	OperatorName string `gorm:"size:64" json:"operator_name"`
	// ApprovalID 仅 SetMethod=approval 时有值,用于回溯到具体哪次审批付的款。
	ApprovalID string `gorm:"size:32;index" json:"approval_id,omitempty"`
	// GoodsAmountYuan 是标记那一刻的货款净额快照(cube 口径 total_goods_yuan)。
	// 快照而非引用:初始化页要按金额回显"本次影响 N 张、合计 ¥X",而源单金额
	// 之后可能变化,审计要能还原当时看到的数字。
	GoodsAmountYuan decimal.Decimal `gorm:"type:numeric(14,2)" json:"goods_amount_yuan"`
	Note            string          `gorm:"size:512" json:"note"`
	PaidAt          *time.Time      `json:"paid_at"`
	// RevertedAt / RevertedBy 记录撤销初始化的时间与人。
	// 撤销**不删行**:改了 paid_status 但保留原 operator / set_method,
	// 这样"谁在什么时候把哪张单补录成已付款"永远查得到。
	RevertedAt *time.Time `json:"reverted_at"`
	RevertedBy string     `gorm:"size:64" json:"reverted_by"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `gorm:"index" json:"updated_at"`
}

func (SettlementPayState) TableName() string { return "settlement_pay_states" }

// IsPaid 是唯一判据:撤销后 PaidStatus 回到 unpaid,行仍在表里。
func (p *SettlementPayState) IsPaid() bool { return p.PaidStatus == PayStatusPaid }
