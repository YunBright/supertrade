// Package main 是 cube-router dapr app 入口(per-branch cube 多源路由)。
//
// 启动:`dapr run --app-id cube-router --app-port 8080 -- go run ./cmd/cube-router`
//
// 业务定位:
//   - 替代 catalog / inventory / master-data 的硬编码 CUBE_APP_ID,统一收口
//     所有 /v1/load 转发;按 X-Branch-ID 路由到正确的 cube 实例。
//   - 通过 admin CRUD 在 branch_cube_sources 表配置"门店 → cube 实例"映射。
//
// 数据存储:**只支持 PostgreSQL**。POSTGRES_DSN 必填,缺失启动失败。
//
// 配置项:
//
//	DAPR_ENDPOINT    = "http://localhost:3500"   // dapr sidecar
//	HTTP_LISTEN_PORT = ":8116"                    // 默认,deployer nginx map 也按此
//
// nginx map 加一行:`~^/api/v1/cube-router/  cube-router`(.claude/rules/04-nginx-map.md)。
//
// 鉴权:
//   - /v1/load 业务端点 → rbac.RequireScopeWithBranch("cube:read", X-Branch-ID, userinfo)
//   - /admin/* 端点    → rbac.RequireRole("admin")(由 handler 内部挂)
package main

import (
	"fmt"
	"os"

	"github.com/YunBright/authkit/userinfo"
	"github.com/YunBright/supertrade/internal/cube-router/db"
	"github.com/YunBright/supertrade/internal/cube-router/handler"
	"github.com/YunBright/supertrade/internal/cube-router/model"
	"github.com/YunBright/supertrade/internal/cube-router/service"
	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID:    "cube-router",
		Port:     defaultPort(),
		OnStart:  initApp,
		Register: registerRoutes,
	})
}

func defaultPort() string {
	if v := os.Getenv("HTTP_LISTEN_PORT"); v != "" {
		return v
	}
	return ":8116"
}

// appDeps 持有运行时单例,供 handler / service 共享。
//
// 跟 cmd/stocktake/main.go 同样的 pattern(package var 因为 cmdbootstrap.Options
// 回调签名不返回外部状态)。
var (
	appDB   *gorm.DB
	appSvc  *service.Service
	appUser *userinfo.Client
)

func initApp() error {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		return fmt.Errorf("cube_router: POSTGRES_DSN 必填(只支持 PostgreSQL)")
	}

	var err error
	appDB, err = db.OpenPostgres(dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	if err := appDB.AutoMigrate(&model.BranchCubeSource{}); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}

	appSvc = service.NewService(appDB)
	appUser = userinfo.New("userd")
	return nil
}

func registerRoutes(r *gin.Engine) {
	if appSvc == nil {
		panic("cube_router: appSvc 未初始化,可能是 OnStart 失败")
	}
	h := handler.New(appSvc, appUser, daprEndpoint(), nil)
	h.RegisterRoutes(r)
}

// daprEndpoint 拿 dapr sidecar 地址;默认 :3500。
func daprEndpoint() string {
	if v := os.Getenv("DAPR_ENDPOINT"); v != "" {
		return v
	}
	return "http://localhost:3500"
}