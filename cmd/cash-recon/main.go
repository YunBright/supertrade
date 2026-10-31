// Package main 是 cash-recon dapr app 入口(超市流水对账域)。
//
// 启动:`dapr run --app-id supertrade-cash-recon --app-port 8121 -- go run ./cmd/cash-recon`
//
// # 业务域
//
//	收银流水聚合 → 银行/渠道流水导入 → 自动匹配 → 差异工作台 → 长短款 →
//	其他收款 → 付款核销 → 供应商应付 → 账户台账与期初余额 → 审核结算冻结与期末快照
//
// # 收银流水**不落本域库**
//
//	有 cube 源 → 查 cube 的 payflow(按业务日 × pay_way 聚合,防慢查询)
//	无 cube 源 → 只读 transaction 域的 pay_flows(本地 PG)
//
// 本域库(recon_*)只存**本系统自己产生的**对账结果:银行流水、匹配、差异、
// 批次、台账、快照、审计日志。
//
// # 配置
//
//	POSTGRES_DSN  必填(本服务拥有 recon_* 共 17 张表)
//
// 其余 env(CASH_RECON_BIZ_TZ / CUBE_TIMEOUT / MAX_RANGE_DAYS / CACHE_TTL /
// ATTACHMENT_MAX_BYTES / DEFAULT_TOLERANCE_DAYS / DEFAULT_CASH_TOLERANCE_YUAN)
// 在 S1 起逐步接入,形状见 deploy/cash-recon.env.example。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/YunBright/authkit/userinfo"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cashrecon/db"
	"github.com/YunBright/supertrade/internal/cashrecon/handler"
	"github.com/YunBright/supertrade/internal/cashrecon/model"
	"github.com/YunBright/supertrade/internal/cashrecon/service"
	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/YunBright/supertrade/pkg/middleware"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		// AppID 是**短名**(进 authd 的 JWT_AUDIENCE),不是 dapr app-id。
		// dapr app-id = supertrade-cash-recon(见 systemd unit)。
		AppID:        "cash-recon",
		Port:         cmdbootstrap.AppPort(":8121"),
		OnStart:      initApp,
		RegisterDapr: registerDaprRoutes,
		Register:     registerRoutes,
	})
}

var (
	appDB   *gorm.DB
	appSvc  *service.Service
	appUser *userinfo.Client
)

func initApp() error {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		return fmt.Errorf("cash-recon: POSTGRES_DSN 必填(本服务拥有 recon_* 表)")
	}
	var err error
	appDB, err = db.OpenPostgres(dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	if err := appDB.AutoMigrate(
		&model.ReconBatch{},
		&model.ReconCashFlow{},
		&model.ReconBankFlow{},
		&model.ReconImport{},
		&model.ReconOtherReceipt{},
		&model.ReconAttachment{},
		&model.ReconMatch{},
		&model.ReconDiff{},
		&model.ReconCashCount{},
		&model.ReconCashVariance{},
		&model.ReconPaymentRecord{},
		&model.ReconAuditLog{},
		&model.ReconRule{},
		&model.ReconPeriodSnapshot{},
		&model.ReconPeriodLock{},
		// 账户台账 + 期初余额(审计冻结的前置,见计划 §10.1):
		// 缺期初一律 409,不默认 0 —— 期初是外部事实(银行对账单/钱箱现金)。
		&model.ReconAccount{},
		&model.ReconOpeningBalance{},
	); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}
	appSvc = service.New(appDB)

	// userinfo 客户端:per-branch effective scopes 校验(recon:view / operate / …)。
	//
	// ⚠️ 它用**自己的服务账号**换机器 token(没配就回落透传调用方 bearer)。
	appUser, err = userinfo.New("userd")
	if err != nil {
		return fmt.Errorf("userinfo client: %w", err)
	}

	// Warmup userd 跨主机冷握手(与 bank-account / catalog 同一个理由:
	// 实测首次 7~12s 会撞 userinfo.Client.timeout 默认 10s)。
	warmupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := appUser.Warmup(warmupCtx); err != nil {
		slog.Warn("userinfo warmup 失败,业务首次 userd 调用将走 cold path", "err", err)
	} else {
		slog.Info("userinfo warmup ok")
	}
	return nil
}

func registerRoutes(r *gin.Engine) {
	if appSvc == nil {
		panic("cash-recon: appSvc 未初始化,可能是 OnStart 失败")
	}
	r.Use(middleware.XBranchID())
	handler.New(appSvc, appUser).RegisterRoutes(r)
}

// registerDaprRoutes 注册免鉴权端点(在 auth 中间件之前)。
//
// 只有 purge 一个:契约测试收尾清理时未必还持有 token。
// 公网不可达由 nginx 保证(`location ^~ /api/v1/cash-recon/internal/ { return 404; }`)。
func registerDaprRoutes(r *gin.Engine) {
	if appDB == nil {
		panic("cash-recon: appDB 未初始化,可能是 OnStart 失败")
	}
	handler.RegisterPurgeRoutes(r, appDB)
}
