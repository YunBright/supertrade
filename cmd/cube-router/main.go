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
// 2026-09 PR 5 重构:
//   - 跨服务调用经 dapr/go-sdk(不再读 DAPR_ENDPOINT)
//   - dapr.NewClient() 自动从 DAPR_GRPC_PORT 拿 sidecar
//   - userinfo.New() 同样走 SDK,无 DAPR_ENDPOINT 配置
//
// nginx map 加一行:`~^/api/v1/cube-router/  cube-router`(.claude/rules/04-nginx-map.md)。
//
// 鉴权:
//   - /v1/load 业务端点 → rbac.RequireScopeWithBranch("cube:read", X-Branch-ID, userinfo)
//   - /admin/* 端点    → rbac.RequireRole("admin")(由 handler 内部挂)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	dapr "github.com/dapr/go-sdk/client"
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
		Port:     cmdbootstrap.AppPort(":8107"),
		OnStart:  initApp,
		Register: registerRoutes,
	})
}

// appDeps 持有运行时单例,供 handler / service 共享。
//
// 跟 cmd/stocktake/main.go 同样的 pattern(package var 因为 cmdbootstrap.Options
// 回调签名不返回外部状态)。
var (
	appDB     *gorm.DB
	appSvc    *service.Service
	appUser   *userinfo.Client
	appDapr   dapr.Client
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
	appUser, err = userinfo.New("userd")
	if err != nil {
		return fmt.Errorf("userinfo client: %w", err)
	}
	appDapr, err = dapr.NewClient()
	if err != nil {
		return fmt.Errorf("dapr.NewClient: %w (确认 dapr run 已起)", err)
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
	slog.Info("cube_router initialized", "dapr_client", "sdk")
	return nil
}

func registerRoutes(r *gin.Engine) {
	if appSvc == nil {
		panic("cube_router: appSvc 未初始化,可能是 OnStart 失败")
	}
	// 把 caller 的 Authorization header 注入 ctx,后续 cubeclient / userinfo 内部
	// 自动 forward 给下游(cube-gateway / userd)。userd 端 middleware.http.bearer
	// 必需要 Authorization 头,无 token 会 401 Unauthenticated。
	r.Use(forwardBearerToOutgoing())
	h := handler.New(appSvc, appUser, appDapr, nil)
	h.RegisterRoutes(r)
}

// forwardBearerToOutgoing 把 gin request 的 Authorization header 注入 ctx。
//
// 用 userinfo.WithBearer 写(此 cmd 不直连 cubeclient,但 userd 端需要 bearer)。
// 这是跨 dapr app 调用透传 caller JWT 的标准做法(同 cube-gateway 调用的
// cubeclient.WithBearer 模式)。
//
// 空 header 表示无 token(内部 job 路径),下游会以 401 拒绝,符合预期。
func forwardBearerToOutgoing() gin.HandlerFunc {
	return func(c *gin.Context) {
		bearer := c.Request.Header.Get("Authorization")
		if bearer != "" {
			ctx := c.Request.Context()
			ctx = userinfo.WithBearer(ctx, bearer)
			c.Request = c.Request.WithContext(ctx)
		}
		c.Next()
	}
}