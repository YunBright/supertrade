// purge.go —— settlement-auditor 的兜底清理（契约测试第二层 sweep）。
//
// 第一层清理挂在每个测试的 t.Cleanup 上（DELETE /settlement-approvals/:id），
// 但**进程被 Ctrl-C / kill / panic 时一行都不执行**——而契约测试打的是生产，
// 那一次跑造出来的审批就永久留下了。
//
// # 本服务与另两个服务的根本差别：它**有后台流水线在写**
//
// stocktake / auth 的数据只被同步 API 读写，删就删了。这里不一样：
// POST /settlement-approvals 返 202 之后，一条异步流水线（cube 核对 → 信号分级
// → LLM 研判）会持续往 audit_signals / audit_logs / llm_verdicts 写，状态从
// received 走到 auditing。
//
// 流水线跑一半时把主表删掉，它下一次写从表会撞"审批实例不存在"，于是一次本该
// 成功的审核变成 failed——**清理反而制造了新的脏数据**。所以这里必须跳过在途
// 状态，与 service.DeleteApproval 的 terminalStatuses 保持同一套判据。
//
// 主表 + 5 张从表都被旁表**独立记账**，清理按各自记下的主键删，registry 的顺序
// 不再是语义。
//
// approval_approver_preferences **不在范围内**：主键是
// `<branchID>:<applicantUserID>`，记的是"这人上次选了谁当审批人"，
// 与某一次审批无关，是按人的偏好缓存。
package handler

import (
	"github.com/YunBright/core/purge"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// deletableStatuses 是 sweep 可删的终态，与 service.terminalStatuses 同源。
//
// ⚠️ approved **不在**这里：它是"机器一审通过、企微申请已提交/提交中"，后面还
// 可能转 human_approved / human_rejected / cancelled——企微回调仍可能回来并写入。
// 删它等于让一次在途的外部回调写进虚空。
//
// 从 model 常量构造而不是抄一段字面量：抄写的那份一旦与
// service.terminalStatuses 漂移，后果是"少删/多删一类审批"且不报错。
var deletableStatuses = []string{
	model.StatusRejected,
	model.StatusFailed,
	model.StatusHumanApproved,
	model.StatusHumanRejected,
	model.StatusCancelled,
}

// childOfDeletableApproval 是 5 张从表共用的"只删可删审批的行"条件。
//
// # 为什么从表不能只靠自己的账
//
// 旁表记账让每张表都被独立定位——这本来意味着「主表跳过 → 从表照删」。在
// auth / stocktake 上那没问题（它们没有"跳过父行"这种语义）；但这里恰恰有：
// received / auditing / approved 三种状态在途，不能删。若只跳过主表，它的
// audit_signals / llm_verdicts 会被删光，而父行还在 → 流水线下一次写入撞
// "审批实例不存在"，**清理制造了新的脏数据**。
//
// 判据：一个"跳过"决定必须传播到它保护的所有行，否则它保护的就只是自己那一行。
func childOfDeletableApproval() (string, any) {
	return "approval_id IN (SELECT id FROM approval_instances WHERE status IN ?)", deletableStatuses
}

// PurgeRegistry 是 settlement-auditor 的可清理实体清单。顺序随便（见文件头）。
func PurgeRegistry() *purge.Registry {
	childSQL, childArg := childOfDeletableApproval()
	return purge.NewRegistry(
		purge.Model{Table: "approval_settlements", Model: &model.ApprovalSettlement{}, KeepSQL: childSQL, KeepArg: childArg},
		purge.Model{Table: "audit_signals", Model: &model.AuditSignal{}, KeepSQL: childSQL, KeepArg: childArg},
		purge.Model{Table: "audit_logs", Model: &model.AuditLog{}, KeepSQL: childSQL, KeepArg: childArg},
		purge.Model{Table: "llm_verdicts", Model: &model.LLMVerdict{}, KeepSQL: childSQL, KeepArg: childArg},
		purge.Model{Table: "approval_stocktake_links", Model: &model.ApprovalStocktakeLink{}, KeepSQL: childSQL, KeepArg: childArg},
		purge.Model{
			Table:   "approval_instances",
			Model:   &model.ApprovalInstance{},
			KeepSQL: "status IN ?",
			KeepArg: deletableStatuses,
		},
	)
}

// RegisterPurgeRoutes 挂 POST /internal/datastore/purge。
//
// 免鉴权，且公网**不可达**：nginx 有
// `location ^~ /api/v1/settlement-auditor/internal/ { return 404; }`
// （回归锁见 deployer/tests/config/nginx_internal_test.go）。
// 与 stocktake 同一范式——端点注册在 RegisterDapr 窗口（auth 中间件之前），
// 因为收尾清理时未必还持有 token。
func RegisterPurgeRoutes(r *gin.Engine, db *gorm.DB) {
	r.POST("/internal/datastore/purge", purge.Handler(db, PurgeRegistry()))
}
