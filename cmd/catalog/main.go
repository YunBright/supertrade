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
//	catalog      :8103    inventory :8102    stocktake    :8106
//
// 配置:
//
//	POSTGRES_DSN    必填(只支持 PostgreSQL)
//	CUBE_CLIENT_MODE = memory(默认,本地)/ http
//	DAPR_ENDPOINT    = "http://localhost:3500"
//	CUBE_APP_ID      = "cube-gateway"
package main

import (
	"fmt"
	"os"

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
		Port:    ":8103",
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
	appUser = userinfo.New("userd")
	return nil
}

func registerRoutes(r *gin.Engine) {
	if appSup == nil {
		panic("catalog: appSup 未初始化")
	}
	// 全局挂 X-Branch-ID 解析(供 supplier / product handler 用)
	r.Use(middleware.XBranchID())
	handler.New(appSup, appProd, appCube, appUser, nil).RegisterRoutes(r)
}