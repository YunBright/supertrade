// Package main 是 master-data dapr app 入口(门店 / 收银员 / 班次)。
//
// 启动:`dapr run --app-id supertrade-master-data --app-port 8102 -- go run ./cmd/master-data`
//
// # 2026-10-21:从 501 占位变成真实服务
//
// 此前它只有一个 `/_placeholder` 返 501 —— 门店 / 收银员 / 班次三张表都不存在。
//
// 现在实装,原因是 **POS 自营店(门店 10)需要它们**:
//   - stores    门店经营参数(盘点周期 / 是否启用会员 / 收银台)
//   - employees 收银员业务档案(工号 / 角色 / 折扣权限 / 收银台)
//   - shifts    班次(开班 / 交班 / 长短款)
//
// # 域边界
//
// ⚠️ **门店定义的真源在 auth.branches**(含 `pos_enabled`),
//
//	本服务的 `stores` 只是它的**经营参数镜像**。
//	⇒ 两处不重复:身份与开关在 auth,经营参数在这里。
//
// ⚠️ **收银员身份在 auth**(users.employee_code + 令牌),
//
//	本服务的 `employees` 存业务档案,主键用 auth user_id。
//
// # 配置
//
//	POSTGRES_DSN              必填(本服务拥有三张表)
//	AUTH_SERVICE_CLIENT_ID    机器身份(调 auth 的 /internal/* 查权限矩阵)
//	AUTH_SERVICE_CLIENT_SECRET
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/YunBright/authkit/userinfo"
	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/masterdata/db"
	"github.com/YunBright/supertrade/internal/masterdata/handler"
	"github.com/YunBright/supertrade/internal/masterdata/model"
	"github.com/YunBright/supertrade/internal/masterdata/service"
	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/YunBright/supertrade/pkg/middleware"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID:    "master-data",
		Port:     cmdbootstrap.AppPort(":8102"),
		OnStart:  initApp,
		Register: registerRoutes,
	})
}

// appDeps 持有运行时单例。
var (
	appStores *service.StoreService
	appEmps   *service.EmployeeService
	appShifts *service.ShiftService
	appUser   *userinfo.Client
)

func initApp() error {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		return fmt.Errorf("master-data: POSTGRES_DSN 必填(本服务拥有 stores / employees / shifts 表)")
	}
	appDB, err := db.OpenPostgres(dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	if err := appDB.AutoMigrate(
		&model.Store{},
		&model.Employee{},
		&model.Shift{},
	); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}
	appStores = service.NewStoreService(appDB)
	appEmps = service.NewEmployeeService(appDB)
	appShifts = service.NewShiftService(appDB)

	// userinfo 客户端:① 给 scope 守门用(per-branch 权限矩阵);
	// ② 它的 VerifyCredentials 用于越权授权链路(验口令 + 取 scope)。
	//
	// ⚠️ 它用**自己的服务账号**换机器 token(没配就回落透传调用方 bearer)。
	appUser, err = userinfo.New("userd")
	if err != nil {
		return fmt.Errorf("userinfo client: %w", err)
	}

	// Warmup userd 跨主机冷握手(与 catalog / pricing 同一个理由:
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
	if appStores == nil {
		panic("master-data: appStores 未初始化,可能是 OnStart 失败")
	}
	r.Use(middleware.XBranchID())
	r.Use(forwardBearerToOutgoing())
	handler.New(appStores, appEmps, appShifts, appUser, appUser, slog.Default()).RegisterRoutes(r)
}

// forwardBearerToOutgoing 把 caller's Authorization / X-Branch-ID 注入 ctx,
// 让 userinfo 内部调用自动 forward 给 auth。
//
// ⚠️ 与 catalog / inventory 同构。
func forwardBearerToOutgoing() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		if bearer := c.Request.Header.Get("Authorization"); bearer != "" {
			ctx = userinfo.WithBearer(ctx, bearer)
		}
		if branchID := c.Request.Header.Get("X-Branch-ID"); branchID != "" {
			ctx = cubeclient.WithBranchID(ctx, branchID)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
