// Package main 是 catalog dapr app 入口(SKU / 品类 / 单位 / 条码 / 供应商)。
//
// 启动:`dapr run --app-id catalog --app-port 8103 -- go run ./cmd/catalog`
//
// 本期定位(2026-09 重构后):
//   - 拥有本地 suppliers 表 + products 表(本系统数据)
//   - products/search 走"本地表 + cube 兜底"双层(数据迁移期)
//   - cube 转发仍保留(兜底用)—— 由 cubeclient 直接调用,不走 cube-router
//     (cube-router 收口的是 stocktake.SearchProducts 等高级查询;catalog
//     本期直接调 cube 即可)
//
// 端口分配:
//
//	catalog      :8103    inventory :8105    stocktake    :8106
//
// 配置:
//
//	POSTGRES_DSN    必填(只支持 PostgreSQL)
//	CUBE_CLIENT_MODE = memory(默认,本地)/ dapr(SDK 模式)
//	CUBE_APP_ID      = "cube-router"   // 默认 (走 cube-router 多源路由)
//
// 2026-09 PR 5 重构:跨服务调用经 dapr/go-sdk;无需 DAPR_ENDPOINT。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/YunBright/authkit/userinfo"
	"github.com/YunBright/supertrade/internal/catalog/db"
	"github.com/YunBright/supertrade/internal/catalog/handler"
	"github.com/YunBright/supertrade/internal/catalog/model"
	"github.com/YunBright/supertrade/internal/catalog/service"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubehttp"
	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/YunBright/supertrade/pkg/middleware"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID:   "catalog",
		Port:    cmdbootstrap.AppPort(":8103"),
		OnStart: initApp,
		Register: registerRoutes,
	})
}

// appDeps 持有运行时单例。
var (
	appDB   *gorm.DB
	appSup  *service.SupplierService
	appProd *service.ProductService
	appCube cubeclient.Client
	appUser *userinfo.Client
)

func initApp() error {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		return fmt.Errorf("catalog: POSTGRES_DSN 必填(只支持 PostgreSQL)")
	}

	var err error
	appDB, err = db.OpenPostgres(dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	if err := appDB.AutoMigrate(&model.Supplier{}, &model.Product{}); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}

	appSup = service.NewSupplierService(appDB)
	appProd = service.NewProductService(appDB)

	// cube client 用作 products/search 兜底(本期未实装本地 product CRUD)
	if appCube, err = cubehttp.NewClientFromEnv(); err != nil {
		return fmt.Errorf("cube client: %w", err)
	}
	appUser, err = userinfo.New("userd")
	if err != nil {
		return fmt.Errorf("userinfo client: %w", err)
	}

	// Warmup userd 跨主机冷握手(Consul DNS + mTLS + HTTP/2 SETTINGS + userd 进程
	// DB pool),实测首次 7~12s 撞 userinfo.Client.timeout 默认 10s。提前烧掉,
	// 业务请求进来时已 warm。失败仅 warn,业务首次走 cold path(已知行为)。
	warmupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := appUser.Warmup(warmupCtx); err != nil {
		slog.Warn("userinfo warmup 失败,业务首次 userd 调用将走 cold path",
			"err", err)
	} else {
		slog.Info("userinfo warmup ok (userd 跨主机链路预热完成)")
	}
	return nil
}

func registerRoutes(r *gin.Engine) {
	if appSup == nil {
		panic("catalog: appSup 未初始化")
	}
	// 全局挂 X-Branch-ID 解析(供 supplier / product handler 用)
	r.Use(middleware.XBranchID())
	// 把 caller 的 Authorization header 注入 ctx,后续 cubeclient / userinfo 内部
	// 自动 forward 给下游(cube-gateway / userd)。userd 端 middleware.http.bearer
	// 必需要 Authorization 头,无 token 会 401 Unauthenticated。
	r.Use(forwardBearerToOutgoing())
	handler.New(appSup, appProd, appCube, appUser, nil).RegisterRoutes(r)
}

// forwardBearerToOutgoing 把 gin request 的 Authorization header 注入 ctx。
//
// 用 cubeclient.WithBearer + userinfo.WithBearer 双写,两者各自的 dapr 调用路径里
// 会读 ctx 拼到 outgoing gRPC metadata → sidecar 转 outgoing HTTP Authorization 给
// 目标 app。这是跨 dapr app 调用透传 caller JWT 的标准做法(同 cube-gateway 调用的
// cubeclient.WithBearer 模式)。
//
// 空 header 表示无 token(内部 job 路径),下游会以 401 拒绝,符合预期。
func forwardBearerToOutgoing() gin.HandlerFunc {
	return func(c *gin.Context) {
		bearer := c.Request.Header.Get("Authorization")
		if bearer != "" {
			ctx := c.Request.Context()
			ctx = cubeclient.WithBearer(ctx, bearer)
			ctx = userinfo.WithBearer(ctx, bearer)
			c.Request = c.Request.WithContext(ctx)
		}
		c.Next()
	}
}