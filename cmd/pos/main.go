// Package main 是 pos dapr app 入口(销售开单 / 收款 / 改价 / 退货)。
//
// 启动:`dapr run --app-id supertrade-pos --app-port 8114 -- go run ./cmd/pos`
//
// # 它是什么
//
// 商云 POS 的**交易主体**:一笔销售 = 一个本地事务里写
// 销售单头 + 明细 + 收付 + **一条 sale.paid 事件**(事务性发件箱)。
//
// ⇒ 事件与业务同事务 ⇒ 进程在 commit 与 publish 之间被 kill,事件**不丢**
// (丢一条 sale.paid = 库存不扣 + 积分不加,而且没有任何日志说这件事)。
//
// # 下游
//
//	inventory  订阅 sale.paid ⇒ 扣库存
//	member     订阅 sale.paid ⇒ 加积分(member_id 为空 ⇒ 不计积分)
//
// # 配置
//
//	POSTGRES_DSN              必填(本服务拥有 4 张表)
//	AUTH_SERVICE_CLIENT_ID    机器身份(调 auth 的 /internal/* 查权限矩阵)
//	AUTH_SERVICE_CLIENT_SECRET
//	DAPR_PUBSUB               可选(默认 tradewind-pubsub,即 pkg/eventbus.Name)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/YunBright/authkit/daprclient"
	"github.com/YunBright/authkit/rbac"
	"github.com/YunBright/authkit/userinfo"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/pos/db"
	"github.com/YunBright/supertrade/internal/pos/handler"
	"github.com/YunBright/supertrade/internal/pos/model"
	"github.com/YunBright/supertrade/internal/pos/service"
	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/YunBright/supertrade/pkg/middleware"
	"github.com/YunBright/supertrade/pkg/outbox"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID:    "pos",
		Port:     cmdbootstrap.AppPort(":8114"),
		Audience: "pos",
		OnStart:  initApp,
		OnStop:   stopApp,
		Register: registerRoutes,
	})
}

// appDeps 持有运行时单例。
var (
	appDB      *gorm.DB
	appSales   *service.SalesService
	appArchive *service.ArchiveService
	appUser    *userinfo.Client
	appRun     *outbox.Runner
	runCtx     context.Context
	runStop    context.CancelFunc
)

// initApp 建 DB / service / 权限客户端,并起**发件箱投递器**。
func initApp() error {
	var err error
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		return fmt.Errorf("pos: POSTGRES_DSN 必填(本服务拥有 sales_orders / sale_lines / payments / outbox 表)")
	}
	appDB, err = db.OpenPostgres(dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	// ⚠️ outbox 表也要建 —— 事件与业务同事务写入 ⇒ 表必须先在。
	if err := appDB.AutoMigrate(
		&model.SalesOrder{},
		&model.SaleLine{},
		&model.Payment{},
		&outbox.Outbox{},
	); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}
	appSales = service.NewSalesService(appDB)

	// userinfo 客户端:per-branch scope 守门用(权限矩阵在 auth)。
	//
	// ⚠️ 它用**自己的服务账号**换机器 token(没配就回落透传调用方 bearer)。
	appUser, err = userinfo.New("userd")
	if err != nil {
		return fmt.Errorf("userinfo client: %w", err)
	} // Warmup userd 跨主机冷握手(与 catalog / pricing 同一个理由:
	// 实测首次 7~12s 会撞 userinfo.Client.timeout 默认 10s)。
	warmupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := appUser.Warmup(warmupCtx); err != nil {
		slog.Warn("userinfo warmup 失败,业务首次 userd 调用将走 cold path", "err", err)
	} else {
		slog.Info("userinfo warmup ok")
	}

	// 发件箱投递器:后台轮询未发布的 sale.paid 事件 → publish → 标记已发布。
	//
	// ⚠️ **不在业务事务里直接 publish**(那是仓里的反面教材):
	//	那样"提交成功但进程被杀"⇒ 事件永久丢失。
	pub, err := outbox.NewDaprPublisherFromEnv()
	if err != nil {
		return fmt.Errorf("dapr publisher: %w (确认 dapr run 已起)", err)
	}
	appRun = outbox.NewRunner(appDB, pub, slog.Default())
	runCtx, runStop = context.WithCancel(context.Background())
	go appRun.Start(runCtx)

	// ---- 归档导出/导入的「这个门店配了哪个数据源」解析器 ----
	//
	// ⚠️ **只用它解析数据源,不拿它查业务数据**:
	//   销售单头 / 明细 / 收付是**本域自己产生的**,cube 里没有这些 model。
	//
	// ⚠️ 用 daprclient.New() 而不是 dapr.NewClient():
	//   go-sdk 的 api-token 拦截器会**整块替换** outgoing metadata,
	//   把本客户端转发给 cube 的 Authorization 头抹掉
	//   (2026-10-20 全网加 DAPR_API_TOKEN 之后暴露的)。
	//   详见 authkit/daprclient 的包注释。
	daprCli, err := daprclient.New()
	if err != nil {
		return fmt.Errorf("创建 dapr 客户端失败: %w (确认 dapr run 已起)", err)
	}
	cubeCli := cubeclient.NewDaprCubeClient(
		daprCli, cubeclient.DefaultCubeAppID(), cubeclient.DefaultCubeQueryPath())
	appArchive = service.NewArchiveService(appDB)
	// ⚠️ 解析器带 60s 缓存 + stale-on-error:每次导出都打一次 dapr 往返不划算,
	//   而一次网络抖动不该让整个门店的导出挂掉。
	appArchive.SetSourceResolver(cubesource.NewCachedResolver(cubeCli, cubesource.DefaultTTL, slog.Default()))
	slog.Info("pos: 归档导出/导入已启用", "app_id", cubeclient.DefaultCubeAppID())
	return nil
}

// stopApp 停掉发件箱投递器。
func stopApp(context.Context) error {
	if runStop != nil {
		runStop()
	}
	return nil
}

// registerRoutes 挂业务路由。
func registerRoutes(r *gin.Engine) {
	if appSales == nil {
		panic("pos: appSales 未初始化,可能是 OnStart 失败")
	}
	r.Use(middleware.XBranchID())
	r.Use(forwardBearerToOutgoing())
	handler.New(appSales, appUser, slog.Default()).RegisterRoutes(r)
	// 归档导出/导入(独立的契约 + 独立的守卫 scope)。
	handler.NewArchiveHandler(appArchive, rbac.HasAnyScopeWithBranch(appUser)).RegisterRoutes(r)
}

// forwardBearerToOutgoing 把 caller's Authorization / X-Branch-ID 注入 ctx,
// 让 userinfo 内部调用自动 forward 给 auth。
//
// ⚠️ 与 catalog / master-data 同构。
func forwardBearerToOutgoing() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		if bearer := c.Request.Header.Get("Authorization"); bearer != "" {
			ctx = userinfo.WithBearer(ctx, bearer)
			// ⚠️ **同时写进 cubeclient 的 ctx key**(不只是 userinfo 的):
			//   归档端点要问 cube-router「这个门店配了哪个数据源」,
			//   而 cube-router 的 /v1/branch-source 挂着 cube:read 守门
			//   ⇒ JWT 必须被透传过去,否则 401
			//   (而症状是"导出报暂不可用",排查方向会被带偏)。
			ctx = cubeclient.WithBearer(ctx, bearer)
		}
		if branchID := c.Request.Header.Get("X-Branch-ID"); branchID != "" {
			ctx = cubeclient.WithBranchID(ctx, branchID)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
