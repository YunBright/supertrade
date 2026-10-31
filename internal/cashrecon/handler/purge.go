// purge.go —— cash-recon 的兜底清理(契约测试第二层 sweep)。
//
// 第一层清理挂在每个测试的 t.Cleanup 上(删掉本次造的批次/导入/实盘…),
// 但**进程被 Ctrl-C / kill / panic 时一行都不执行**——而契约测试打的是生产,
// 那一次跑造出来的批次就永久留在对账工作台里。
//
// # 安全边界不是 KeepSQL,而是旁表记账
//
// 本域的表全是**业务事实**(批次 / 匹配 / 差异 / 期初 / 快照),任何一张都不该
// 被一句"清理测试数据"无条件清空。core/purge 的定位方式是:
//
//	DELETE FROM <表> WHERE CAST(<主键> AS text) IN (
//	    SELECT row_id FROM testrun_rows WHERE run_id = ? AND table_name = ?)
//
// 即**只删本次测试运行造出来的行**(RunID 由 cmdbootstrap 的 testrun 中间件
// 从请求头带进来记账)。真实对账数据没有记账 ⇒ 永远不会被这条路径删掉。
//
// 所以这里把 17 张表**全部登记**:漏登记一张,表现是"清理报告里少一张表"且不报错,
// 而漏掉的那张表会悄悄积累测试垃圾。
//
// # 免鉴权端点的防线在 nginx
//
// POST /internal/datastore/purge 注册在 RegisterDapr 窗口(auth 中间件之前),
// 由 deployer 的 `location ^~ /api/v1/cash-recon/internal/ { return 404; }`
// 挡在公网之外(回归锁见 deployer/tests/config/nginx_internal_test.go)。
package handler

import (
	"github.com/YunBright/core/purge"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cashrecon/model"
)

// PurgeRegistry 是 cash-recon 的可清理实体清单。
//
// 顺序随便:每张表都按自己的记账独立定位(core/purge 的 Registry 顺序不参与语义)。
func PurgeRegistry() *purge.Registry {
	return purge.NewRegistry(
		purge.Model{Table: "recon_batches", Model: &model.ReconBatch{}},
		purge.Model{Table: "recon_cash_flows", Model: &model.ReconCashFlow{}},
		purge.Model{Table: "recon_bank_flows", Model: &model.ReconBankFlow{}},
		purge.Model{Table: "recon_imports", Model: &model.ReconImport{}},
		purge.Model{Table: "recon_other_receipts", Model: &model.ReconOtherReceipt{}},
		purge.Model{Table: "recon_attachments", Model: &model.ReconAttachment{}},
		purge.Model{Table: "recon_matches", Model: &model.ReconMatch{}},
		purge.Model{Table: "recon_diffs", Model: &model.ReconDiff{}},
		purge.Model{Table: "recon_cash_counts", Model: &model.ReconCashCount{}},
		purge.Model{Table: "recon_cash_variances", Model: &model.ReconCashVariance{}},
		purge.Model{Table: "recon_payment_records", Model: &model.ReconPaymentRecord{}},
		purge.Model{Table: "recon_audit_logs", Model: &model.ReconAuditLog{}},
		purge.Model{Table: "recon_rules", Model: &model.ReconRule{}},
		purge.Model{Table: "recon_period_snapshots", Model: &model.ReconPeriodSnapshot{}},
		purge.Model{Table: "recon_period_locks", Model: &model.ReconPeriodLock{}},
		purge.Model{Table: "recon_accounts", Model: &model.ReconAccount{}},
		purge.Model{Table: "recon_opening_balances", Model: &model.ReconOpeningBalance{}},
	)
}

// RegisterPurgeRoutes 挂 POST /internal/datastore/purge。
//
// 免鉴权,且公网**不可达**(nginx internal 404)。
// 注册在 RegisterDapr 窗口:收尾清理时未必还持有 token。
func RegisterPurgeRoutes(r *gin.Engine, db *gorm.DB) {
	r.POST("/internal/datastore/purge", purge.Handler(db, PurgeRegistry()))
}
