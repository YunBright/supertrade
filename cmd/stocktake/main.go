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
// 2026-09 PR 5 重构:
//   - 跨服务调用经 dapr/go-sdk(DaprCubeClient + DaprPublisher);无需 DAPR_ENDPOINT
//   - dapr.NewClient() 自动从 DAPR_GRPC_PORT 拿 sidecar;dapr run 自动注入
//   - publisher fail-fast: dapr.NewClient() 失败 → 启动直接退出非 0
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/YunBright/authkit/userinfo"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubehttp"
	"github.com/YunBright/supertrade/internal/stocktake"
	"github.com/YunBright/supertrade/internal/stocktake/handler"
	"github.com/YunBright/supertrade/internal/stocktake/model"
	"github.com/YunBright/supertrade/internal/stocktake/service"
	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/YunBright/supertrade/pkg/middleware"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID: "stocktake",
		Port:  cmdbootstrap.AppPort(":8106"),
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
		&model.StocktakeBranchDefault{},
	); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}

	appCube, err = initCubeClient()
	if err != nil {
		return fmt.Errorf("init cube client: %w", err)
	}

	appSvc = service.New(appDB, appCube)

	// 注入 userinfo 客户端(供 effective scopes 校验)。
	// 经 dapr sidecar 调 userd /internal/users/{id};SDK 自动从 DAPR_GRPC_PORT 拿地址。
	// 未注入时 GetEffectiveScopes 返 ErrUserInfoUnavailable(503 userd_unavailable)。
	ui, err := userinfo.New("userd")
	if err != nil {
		return fmt.Errorf("userinfo client: %w", err)
	}
	appSvc.SetUserInfo(ui)

	// Warmup userd 跨主机冷握手(Consul DNS + mTLS + HTTP/2 SETTINGS + userd 进程
	// DB pool),实测首次 7~12s 撞 userinfo.Client.timeout 默认 10s。提前烧掉,
	// 业务请求进来时已 warm,首个 contract test 不再 503 userd_unavailable。
	// 不阻断启动:失败仅 warn,业务首次走 cold path(已知行为)。
	warmupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := ui.Warmup(warmupCtx); err != nil {
		slog.Warn("userinfo warmup 失败,业务首次 userd 调用将走 cold path",
			"err", err)
	} else {
		slog.Info("userinfo warmup ok (userd 跨主机链路预热完成)")
	}

	// 注入 Dapr pub/sub publisher (fail-fast)。
	// stocktake 必须有 publisher 才能消费 auth.user.access_changed;sidecar 不可达
	// → 启动失败,提醒开发者 dapr run 没起。
	pub, err := service.NewDaprPublisherFromEnv()
	if err != nil {
		return fmt.Errorf("dapr publisher: %w (确认 dapr run 已起)", err)
	}
	appSvc.SetPublisher(pub)
	slog.Info("dapr publisher enabled")

	slog.Info("stocktake app initialized",
		"db_driver", "postgres",
	)
	return nil
}

// initCubeClient 走 cubehttp.NewClientFromEnv(catalog / inventory 复用)。
func initCubeClient() (cubeclient.Client, error) {
	return cubehttp.NewClientFromEnv()
}

func registerRoutes(r *gin.Engine) {
	if appSvc == nil {
		panic("stocktake: appSvc 未初始化,可能是 OnStart 失败")
	}
	// 全局挂 X-Branch-ID 解析中间件(只读 header,不强求存在;handler 自选 source)。
	r.Use(middleware.XBranchID())
	// 把 caller 的 Authorization header 注入 ctx,后续 cubeclient / userinfo 内部
	// 自动 forward 给下游(cube-gateway / userd)。userd 端 middleware.http.bearer
	// 必需要 Authorization 头,无 token 会 401 Unauthenticated。
	r.Use(forwardBearerToOutgoing())
	h := handler.New(appSvc)
	h.RegisterRoutes(r)
	// Dapr pub/sub 订阅(挂在 engine,不走业务路由组):
	//   GET  /dapr/subscribe  → 订阅清单
	//   POST /events/<topic>  → 事件分发(本服务订阅 auth.user.access_changed)
	h.RegisterSubscribeRoutes(r, slog.Default())
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

// 静默引用 context(为后续按需扩展预留)
var _ = context.Background