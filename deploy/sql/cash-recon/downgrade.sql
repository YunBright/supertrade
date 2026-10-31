-- cash-recon 一期 回滚脚本（降级）
--
-- ⚠️⚠️⚠️ 破坏性:执行即**物理删除**全部对账数据(批次/匹配/差异/期初/快照)。
--   只在"要整体下线这个功能"时使用,执行前**必须**先做 pg_dump -Fc 备份,
--   并按 `pg_restore --list <dump> | grep -c 'TABLE DATA'` 验证备份可读。
--
-- 只删**本功能新增**的表:
--   cash-recon 17 张 recon_*
--   settlement-auditor 2 张企微月度总表(approval_monthly_items / backfills)
-- **不触碰任何既有表**,也不删任何既有索引 —— 既有表的改造在代码里全部是
-- "新增读路径 + 懒回填",没有列级变更。
--
-- 顺序:按依赖反序(先明细后主表)。本期模型**未声明外键约束**,
-- 顺序只是为了保证"哪张表先删都成立",不依赖级联行为。
--
-- 执行:psql "$POSTGRES_DSN" -f deploy/sql/cash-recon/downgrade.sql

BEGIN;

-- ── cash-recon 明细表 ─────────────────────────────────────────────────
-- 凭证依附于其他收款单
DROP TABLE IF EXISTS recon_attachments;
DROP TABLE IF EXISTS recon_other_receipts;
-- 匹配/差异依附于批次
DROP TABLE IF EXISTS recon_matches;
DROP TABLE IF EXISTS recon_diffs;
-- 收银快照依附于批次;导出行依附于导入批次
DROP TABLE IF EXISTS recon_cash_flows;
DROP TABLE IF EXISTS recon_bank_flows;
DROP TABLE IF EXISTS recon_imports;
-- 长短款台账依附于现金实盘
DROP TABLE IF EXISTS recon_cash_variances;
DROP TABLE IF EXISTS recon_cash_counts;
-- 付款台账 / 审计日志 / 规则 / 快照 / 冻结区间
DROP TABLE IF EXISTS recon_payment_records;
DROP TABLE IF EXISTS recon_audit_logs;
DROP TABLE IF EXISTS recon_rules;
DROP TABLE IF EXISTS recon_period_snapshots;
DROP TABLE IF EXISTS recon_period_locks;
-- 期初依附于账户台账(逻辑依附,无外键)
DROP TABLE IF EXISTS recon_opening_balances;
DROP TABLE IF EXISTS recon_accounts;
-- 批次是主表,最后删
DROP TABLE IF EXISTS recon_batches;

-- ── settlement-auditor 的企微月度总表(计划 §9)────────────────────────
-- 这两张是**派生表**:删掉不影响审批单主链(approval_instances / approval_items
-- 仍在),重新部署后懒回填会按版本号重建。
DROP TABLE IF EXISTS approval_monthly_backfills;
DROP TABLE IF EXISTS approval_monthly_items;

COMMIT;

-- 回滚后的核对:
--   SELECT count(*) FROM information_schema.tables
--    WHERE table_schema='public'
--      AND (table_name LIKE 'recon_%' OR table_name LIKE 'approval_monthly_%');
-- 期望:0
