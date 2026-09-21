// Package main 是 stocktake dapr app 入口(盘点管理,本期重点,实时盘点/营业中)。
//
// 启动:`dapr run --app-id stocktake --app-port 8080 -- go run ./cmd/stocktake`
//
// 本期实现(REQUIREMENTS §2.1):
//   - 盘点表 + 盘点明细 CURD
//   - 按条码逐行录入 + 实时拉 cube 库存快照
//   - 实时差异表生成
//   - 状态机:counting → adjusted → approved
//   - /api/v1/products/search 扫条码接口(REQUIREMENTS §2.1.4.1)
//
// 数据存储:**只支持 PostgreSQL**。POSTGRES_DSN 必填,缺失直接启动失败。
// 不提供 SQLite fallback;单元测试用 *_test.go 内部 SQLite(与生产隔离)。
//
// Cube client:通过 CUBE_CLIENT_MODE 切换:
//
//	CUBE_CLIENT_MODE=memory  (默认) InMemoryClient(mock,真实六讯思迅 schema)
//	CUBE_CLIENT_MODE=http    HTTPCubeClient,经 DAPR_ENDPOINT 调 cube-gateway /v1/load
//
// 配置项:
//
//	DAPR_ENDPOINT = "http://localhost:3500"   // dapr sidecar 默认
//	CUBE_APP_ID   = "cube-gateway"             // 或 "sixun-hbposv7" 等具体实例
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubehttp"
	"github.com/YunBright/supertrade/internal/stocktake"
	"github.com/YunBright/supertrade/internal/stocktake/handler"
	"github.com/YunBright/supertrade/internal/stocktake/model"
	"github.com/YunBright/supertrade/internal/stocktake/service"
	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID: "stocktake",
		Port:  ":8106",
		OnStart: func() error {
			return initApp()
		},
		Register: registerRoutes,
	})
}

// appDeps 持有运行时单例,供 handler / service 共享。
//
// 用 package var 是因为 cmdbootstrap.Options.OnStart / Register 回调签名不返回外部状态;
// 后续可改成显式 dependency injection。
var (
	appDB   *gorm.DB
	appSvc  *service.Service
	appCube cubeclient.Client
)

func initApp() error {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		return fmt.Errorf("POSTGRES_DSN 必填(stocktake 服务只支持 PostgreSQL,无 fallback)")
	}

	var err error
	appDB, err = stocktake.OpenPostgres(dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	if err := appDB.AutoMigrate(
		&model.StocktakeHeader{},
		&model.StocktakeLine{},
		&model.StocktakeLineOperation{},
		&model.StocktakePlanItem{},
	); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}

	appCube, err = initCubeClient()
	if err != nil {
		return fmt.Errorf("init cube client: %w", err)
	}

	appSvc = service.New(appDB, appCube)

	// 注入 Dapr pub/sub publisher;缺环境变量时禁用广播
	if os.Getenv("DAPR_ENDPOINT") != "" || os.Getenv("ENABLE_DAPR_PUBLISH") == "1" {
		pub := service.NewDaprPublisherFromEnv()
		appSvc.SetPublisher(pub)
		slog.Info("dapr publisher enabled", "endpoint", os.Getenv("DAPR_ENDPOINT"))
	} else {
		slog.Info("dapr publisher disabled (set DAPR_ENDPOINT or ENABLE_DAPR_PUBLISH=1)")
	}

	slog.Info("stocktake app initialized",
		"db_driver", "postgres",
	)
	return nil
}

// initCubeClient 走 cubehttp.NewClientFromEnv(catalog / inventory / master-data 复用)。
func initCubeClient() (cubeclient.Client, error) {
	return cubehttp.NewClientFromEnv()
}

func registerRoutes(r *gin.Engine) {
	if appSvc == nil {
		panic("stocktake: appSvc 未初始化,可能是 OnStart 失败")
	}
	handler.New(appSvc).RegisterRoutes(r)
}

// 静默引用 context(为后续按需扩展预留)
var _ = context.Background
