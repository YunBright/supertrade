// Package main 是 procurement dapr app 入口(采购收货,**只读**)。
//
// 启动:`dapr run --app-id supertrade-procurement --app-port 8120 -- go run ./cmd/procurement`
//
// # 端口分配
//
//	procurement 8120(dapr http 3120 / grpc 6120)
//
// 约定:app-port − 5000 = dapr http 端口,app-port − 2000 = dapr grpc 端口。
//
// # 2026-10-22:从 22 行的空壳变成一个真服务
//
// 此前 cmd/procurement/main.go 只有一个 `GET /purchase-orders` 返
// `{"purchase_orders":[]}` —— 一个恒返空数组的端点。
//
// ⚠️ 恒返空数组是最坏的一种"未实现":它**看起来**是正常的业务结论
// ("这家店没有采购单"),而真相是"这条路根本没实现"。
//
// ⇒ 现在它是:有表、有 cube 查询、有分店判据、有真实响应形状的只读服务。
//
// # ⚠️ 数据来源按「门店配没配数据源」分流(判据唯一:internal/cubesource)
//
//	配了源 ⇒ 走 cube(门店专属 cube 实例)
//	没配源 ⇒ 走本域 PG(purchase_sheets / purchase_sheet_lines)
//	问不到 / 被停用 ⇒ **503**,不回落本地。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/YunBright/authkit/daprclient"
	"github.com/YunBright/authkit/userinfo"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/procurement/db"
	"github.com/YunBright/supertrade/internal/procurement/handler"
	"github.com/YunBright/supertrade/internal/procurement/model"
	"github.com/YunBright/supertrade/internal/procurement/service"
	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/YunBright/supertrade/pkg/middleware"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		// ⚠️ AppID 必须是**短名**(与 JWT_AUDIENCE 对齐),不是 dapr app-id。
		//	详见 cmd/transaction/main.go 里同一处的说明。
		AppID:    "procurement",
		Port:     cmdbootstrap.AppPort(":8120"),
		OnStart:  initApp,
		Register: registerRoutes,
	})
}

// appDeps 持有运行时单例。
var (
	appSvc  *service.Service
	appDB   *gorm.DB
	appCube *cubeclient.DaprCubeClient
	appSrc  cubesource.Resolver
	appUser *userinfo.Client
)

func initApp() error {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		return fmt.Errorf("procurement: POSTGRES_DSN 必填(没配数据源的门店读本域采购单表;无 fallback)")
	}
	var err error
	appDB, err = db.OpenPostgres(dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	if err := appDB.AutoMigrate(
		&model.PurchaseSheet{},
		&model.PurchaseSheetLineRow{},
	); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}
	appSvc = service.New(appDB, slog.Default())

	// ⚠️ 用 daprclient.New() 而不是 dapr.NewClient():
	//   go-sdk 的 api-token 拦截器会**整块替换** outgoing metadata,
	//   把本客户端转发给 cube 的 Authorization 头抹掉(2026-10-20 全网加
	//   DAPR_API_TOKEN 之后暴露的)。详见 authkit/daprclient 的包注释。
	daprCli, err := daprclient.New()
	if err != nil {
		return fmt.Errorf("创建 dapr 客户端失败: %w (确认 dapr run 已起)", err)
	}
	appCube = cubeclient.NewDaprCubeClient(
		daprCli,
		cubeclient.DefaultCubeAppID(),
		cubeclient.DefaultCubeQueryPath(),
	)
	// ⚠️ **解析器必须装配**:没装 ⇒ 所有门店都走本地 ⇒ 有源门店(00/01)
	//   查到的不是它自己的采购单。
	//
	// ⚠️ 解析器与 cube 读端必须**一起**注入:只注入一个 ⇒ 症状相同、零报错。
	appSrc = cubesource.NewCachedResolver(appCube, cubesource.DefaultTTL, slog.Default())
	appSvc.SetCubeSource(appSrc, appCube)
	slog.Info("procurement: 门店数据源路由已启用",
		"app_id", cubeclient.DefaultCubeAppID())

	appUser, err = userinfo.New("userd")
	if err != nil {
		return fmt.Errorf("userinfo client: %w", err)
	}
	warmupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := appUser.Warmup(warmupCtx); err != nil {
		slog.Warn("userinfo warmup 失败,业务首次 userd 调用将走 cold path", "err", err)
	} else {
		slog.Info("userinfo warmup ok")
	}
	return nil
}

// registerRoutes 挂载路由。
func registerRoutes(r *gin.Engine) {
	if appSvc == nil {
		panic("procurement: appSvc 未初始化")
	}
	r.Use(middleware.XBranchID())
	r.Use(forwardBearerToOutgoing())
	handler.New(appSvc, appUser, slog.Default()).RegisterRoutes(r)
}

// forwardBearerToOutgoing 把 caller 的 Authorization / X-Branch-ID header 注入 ctx。
//
// ⚠️ X-Branch-ID 必须一起注入(2026-10-16 的生产故障):少注入一次,
// cube-router 收不到门店 ⇒ 400 branch_required ⇒ 被 sidecar 折叠成
// gRPC Internal ⇒ 错误分类器把它翻成「库存服务暂不可用」,
// 而 cube / cube-router / 源库全部健康。
func forwardBearerToOutgoing() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		if bearer := c.Request.Header.Get("Authorization"); bearer != "" {
			ctx = cubeclient.WithBearer(ctx, bearer)
			ctx = userinfo.WithBearer(ctx, bearer)
		}
		if branchID := c.Request.Header.Get("X-Branch-ID"); branchID != "" {
			ctx = cubeclient.WithBranchID(ctx, branchID)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
