// Package main 是 bank-account dapr app 入口(供应商收款银行账号维护)。
//
// 启动:`dapr run --app-id supertrade-bank-account --app-port 8117 -- go run ./cmd/bank-account`
//
// # 业务域
//
// 维护各门店的**供应商收款银行账号**(门店付款给供应商时打到哪个账户):
// 开户行 / 账号 / 户名,外键到供应商 id。
//
// # 与其它服务的**故意不同**:账号数据永远查本地 PG
//
//	catalog   → 有源查 cube、无源查本地 pg
//	本服务     → **不管有无 cube 源，一律查本地 pg**
//
// 理由见 internal/bankaccount/service 的包注释:
// 收款账号是本系统自己产生的业务事实(不是外部 ERP 的镜像)。
//
// # supplier_id 的命名空间
//
//	有 cube 源   → cube 源的 supplier id
//	无 cube 源   → pg 本地表 suppliers 的 supplier id
//
// 判据只有一个(internal/cubesource);本服务不自己判断"有没有 cube 源"。
// 门店 10(自营店,无 cube 源)的供应商是从门店 00 导入到本地 suppliers 表的
// ⇒ 它落在"无 cube 源"这一侧,绑定**本地** supplier id。
//
// # 配置
//
//	POSTGRES_DSN  必填(本服务拥有 supplier_bank_accounts 表)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/YunBright/authkit/userinfo"
	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/bankaccount/db"
	"github.com/YunBright/supertrade/internal/bankaccount/handler"
	"github.com/YunBright/supertrade/internal/bankaccount/model"
	"github.com/YunBright/supertrade/internal/bankaccount/service"
	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/YunBright/supertrade/pkg/middleware"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		// AppID 是**短名**(进 authd 的 JWT_AUDIENCE),不是 dapr app-id。
		// dapr app-id = supertrade-bank-account(见 systemd unit)。
		AppID:    "bank-account",
		Port:     cmdbootstrap.AppPort(":8117"),
		OnStart:  initApp,
		Register: registerRoutes,
	})
}

// appDeps 持有运行时单例。
var (
	appSvc  *service.Service
	appUser *userinfo.Client
)

func initApp() error {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		return fmt.Errorf("bank-account: POSTGRES_DSN 必填(本服务拥有 supplier_bank_accounts 表)")
	}
	appDB, err := db.OpenPostgres(dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	if err := appDB.AutoMigrate(
		&model.BankAccount{},
	); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}
	appSvc = service.New(appDB)

	// userinfo 客户端:per-branch effective scopes 校验(读端点 merchant 要用
	// settlement:view,写端点要 supplier:manage)。
	//
	// ⚠️ 它用**自己的服务账号**换机器 token(没配就回落透传调用方 bearer)。
	appUser, err = userinfo.New("userd")
	if err != nil {
		return fmt.Errorf("userinfo client: %w", err)
	}

	// Warmup userd 跨主机冷握手(与 catalog / master-data 同一个理由:
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
		panic("bank-account: appSvc 未初始化,可能是 OnStart 失败")
	}
	r.Use(middleware.XBranchID())
	handler.New(appSvc, appUser).RegisterRoutes(r)
}
