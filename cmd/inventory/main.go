// Package main 是 inventory dapr app 入口(库存快照 / 库存流水 / sale.paid 订阅扣减)。
//
// 启动:`dapr run --app-id inventory --app-port 8105 -- go run ./cmd/inventory`
//
// # 2026-10-21:从"cube stock 转发器"变成"真库存服务"
//
// 此前它只做一件事:把 /stock/:product_id 转发给 cube-router。
// 而"门店 10(云商 POS 自营店)**没有 cube 源**"⇒ 转发拿不到任何库存。
//
// 现在实装自己的两张表:
//   - stock_levels  当前库存快照(POS 查商品时带库存)
//   - stock_moves   库存流水(append-only,一次库存变动一行)
//
// 并且订阅 `sale.paid`,**幂等**扣减库存(见 internal/inventory/service/subscribe.go)。
//
// # 域边界
//
// 商品档案 / 售价归 catalog / pricing;库存数量与流水归本服务。
//
// # 配置
//
//	POSTGRES_DSN              必填(本服务拥有 stock_levels / stock_moves 两张表)
//	AUTH_SERVICE_CLIENT_ID    机器身份(调 auth 的 /internal/* 查 per-branch 权限矩阵)
//	AUTH_SERVICE_CLIENT_SECRET
//
// # 端口
//
// 8105(dapr-http 3105 / dapr-grpc 6105)—— **不要改**,部署侧按这个配。
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

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubehttp"
	"github.com/YunBright/supertrade/internal/inventory/db"
	"github.com/YunBright/supertrade/internal/inventory/handler"
	"github.com/YunBright/supertrade/internal/inventory/model"
	"github.com/YunBright/supertrade/internal/inventory/service"
	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/YunBright/supertrade/pkg/middleware"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID:    "inventory",
		Audience: "inventory",
		Port:     cmdbootstrap.AppPort(":8105"),
		OnStart:  initApp,
		Register: registerRoutes,
		// ⚠️ Dapr 内部路由必须走 RegisterDapr —— 它在 auth 中间件**之前**注册。
		//
		// sidecar 调 /dapr/subscribe 与 /events/sale.paid **不带 Authorization 头**,
		// 带鉴权就 401 ⇒ sidecar 永远拿不到订阅清单(2026-10-07 本仓真踩过)。
		RegisterDapr: registerDaprRoutes,
	})
}

// appDeps 持有运行时单例。
var (
	appDB    *gorm.DB
	appStock *service.StockService
	appUser  *userinfo.Client
	appCube  cubeclient.Client
)

func initApp() error {
	// ⚠️ POSTGRES_DSN 缺失 ⇒ **拒绝启动**。
	//
	// 不做"没有库也起来"的降级:库存是收银事实,没有库的 inventory
	// 会静默变成"查不到库存 / 扣不了库存",而 /healthz 仍然 200。
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		return fmt.Errorf("inventory: POSTGRES_DSN 必填(本服务拥有 stock_levels / stock_moves 表)")
	}
	var err error
	appDB, err = db.OpenPostgres(dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	if err := appDB.AutoMigrate(
		&model.StockLevel{},
		&model.StockMove{},
	); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}
	appStock = service.NewStockService(appDB)

	// userinfo 客户端:per-branch scope 守门用(inventory:view 是实时解析的)。
	appUser, err = userinfo.New("userd")
	if err != nil {
		return fmt.Errorf("userinfo client: %w", err)
	}
	// Warmup userd 跨主机冷握手(与 catalog / pricing / master-data 同一个理由:
	// 实测首次 7~12s 会撞 userinfo.Client.timeout 默认 10s)。
	warmupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := appUser.Warmup(warmupCtx); err != nil {
		slog.Warn("userinfo warmup 失败,业务首次 userd 调用将走 cold path", "err", err)
	} else {
		slog.Info("userinfo warmup ok")
	}

	// cube 转发客户端(老路径 /stock/:product_id 仍在用)。
	appCube, err = cubehttp.NewClient()
	if err != nil {
		return err
	}
	return nil
}

// registerRoutes 挂**业务路由**(在 auth 中间件之后)。
func registerRoutes(r *gin.Engine) {
	if appStock == nil {
		panic("inventory: appStock 未初始化,可能是 OnStart 失败")
	}
	r.Use(middleware.XBranchID())
	r.Use(forwardOutgoingHeaders())

	// 本地库存端点(inventory:view,per-branch 实时解析)。
	handler.New(appStock, appUser, slog.Default()).RegisterRoutes(r)

	// 保留老路径:cube stock 转发(cube 门店仍走它)。
	cubehttp.New(appCube).Register(r, cubehttp.RegisterOptions{
		Stock:         true,
		RequireBranch: true, // /stock/:product_id 走 claims.AccessibleBranches 守门(branch 从 X-Branch-ID header 取)
	})
}

// registerDaprRoutes 挂 **Dapr 内部路由**(在 auth 中间件之前)。
//
// ⚠️ 由 `cmdbootstrap.Options.RegisterDapr` 调用 —— 装配顺序由 cmdbootstrap 保证。
func registerDaprRoutes(r *gin.Engine) {
	if appStock == nil {
		panic("inventory: appStock 未初始化,可能是 OnStart 失败")
	}
	handler.New(appStock, appUser, slog.Default()).RegisterSubscribeRoutes(r, slog.Default())
}

// forwardOutgoingHeaders 把 caller 的 Authorization + X-Branch-ID 注入 ctx,
// 后续 cubeclient / userinfo 内部自动 forward。
//
// X-Branch-ID:**必须一起透传**。cube-router 的 /v1/load 靠这个头解析门店,
// 缺了直接返 400 branch_required,而 dapr 会把 400 包成 gRPC
// "Internal: Bad Request" —— 现象是本服务返 500 cube_error,完全看不出是少了门店头。
func forwardOutgoingHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		if bearer := c.Request.Header.Get("Authorization"); bearer != "" {
			ctx = cubeclient.WithBearer(ctx, bearer)
		}
		if branchID := c.Request.Header.Get("X-Branch-ID"); branchID != "" {
			ctx = cubeclient.WithBranchID(ctx, branchID)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
