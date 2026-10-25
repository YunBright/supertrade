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
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/YunBright/authkit/daprclient"
	"github.com/YunBright/authkit/rbac"
	"github.com/YunBright/authkit/userinfo"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubehttp"
	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/stocktake"
	"github.com/YunBright/supertrade/internal/stocktake/handler"
	"github.com/YunBright/supertrade/internal/stocktake/model"
	"github.com/YunBright/supertrade/internal/stocktake/seed"
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
		RegisterDapr: registerDaprRoutes,
		Register:     registerRoutes,
	})
}

// appDeps 持有运行时单例,供 handler / service 共享。
//
// 用 package var 是因为 cmdbootstrap.Options.OnStart / Register 回调签名不返回外部状态;
// 后续可改成显式 dependency injection。
var (
	appDB      *gorm.DB
	appSvc     *service.Service
	appCube    cubeclient.Client
	appArchive *service.ArchiveService
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
		&model.StocktakeExcludedScope{},
	); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}

	// 排除域配置(默认:两家生产门店的猪肉分类)幂等装配一次。
	//
	// ⚠️ 为什么不放在"运维手工跑一次"的脚本里:这张表是**守卫**,
	// 漏装 ⇒ 猪肉被两条流程各盘一次 ⇒ 两个数、没人说得清哪个对,
	// 而失败是**静默**的。启动时装配 ⇒ 守卫永远在。
	// 装配失败 ⇒ 启动失败(宁可起不来,也不要静默地少一道守卫)。
	created, updated, skipped, err := seed.Apply(appDB, time.Now())
	if err != nil {
		return fmt.Errorf("seed excluded scopes: %w", err)
	}
	slog.Info("排除域配置已装配",
		"created", created, "updated", updated, "skipped", skipped)

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

	// ---- 归档导出/导入的「这个门店配了哪个数据源」解析器 ----
	//
	// ⚠️ **只用它解析数据源,不拿它查业务数据**:
	//   盘点单 / 盘点明细 / 操作历史是**本域自己产生的**,cube 里没有这些 model。
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
	slog.Info("stocktake: 归档导出/导入已启用", "app_id", cubeclient.DefaultCubeAppID())

	slog.Info("stocktake app initialized",
		"db_driver", "postgres",
	)
	return nil
}

// initCubeClient 走 cubehttp.NewClient(catalog / inventory / fresh-meat 复用)。
func initCubeClient() (cubeclient.Client, error) {
	return cubehttp.NewClient()
}

func registerRoutes(r *gin.Engine) {
	if appSvc == nil {
		panic("stocktake: appSvc 未初始化,可能是 OnStart 失败")
	}
	// 全局挂 X-Branch-ID 解析中间件(只读 header,不强求存在;handler 自选 source)。
	r.Use(middleware.XBranchID())
	// 把 caller 的 Authorization header 注入 ctx,后续 cubeclient / userinfo 内部
	// 自动 forward 给下游(cube-gateway / userd)。userd 端的应用层验签
	// 必需要 Authorization 头,无 token 会 401 Unauthenticated。
	r.Use(forwardBearerToOutgoing())
	h := handler.New(appSvc)
	h.RegisterRoutes(r)
	// 归档导出/导入(独立的契约 + 独立的守卫 scope)。
	//
	// ⚠️ 守门复用 service 层的 effective scopes(带 60s 缓存),
	//   与本域既有的 inventory:view / inventory:manage 同一套口径。
	handler.NewArchiveHandler(appArchive, rbac.ScopeResolver(
		func(c *gin.Context, userID, branchID, scope string) (bool, error) {
			return appSvc.HasEffectiveScope(c.Request.Context(), userID, branchID, scope)
		})).RegisterRoutes(r)
}

// registerDaprRoutes 在 auth 中间件**之前**注册 Dapr 订阅端点。
//
//	GET  /dapr/subscribe  → 订阅清单
//	POST /events/<topic>  → 事件分发(本服务订阅 auth.user.access_changed)
//
// 必须早于 cmdbootstrap 的 claims.GinMiddleware + rbac.RequireAudience:
// sidecar 调这两个端点不带 Authorization 头,带上鉴权中间件就会 401,
// sidecar 于是永远拿不到订阅清单 —— 事件推不出去,但 /healthz 仍 200。
// 详见 pkg/cmdbootstrap.Options.RegisterDapr 的注释。
func registerDaprRoutes(r *gin.Engine) {
	h := handler.New(appSvc)
	h.RegisterSubscribeRoutes(r, slog.Default())
	// S2S 内部端点(settlement-auditor 建计划盘点单):同样不能走 JWT,
	// 靠 dapr 内网 + nginx 不暴露 /internal/* 保护。
	//
	// ⚠️ **必须显式传 forwardBearerToOutgoing()**:
	// 本路由注册在 RegisterDapr 窗口,早于上面的 `r.Use(forwardBearerToOutgoing())`,
	// 而 Gin 的 r.Use 只对**之后**注册的路由生效 ⇒ 全局中间件不在本路由的链上。
	// 少了它:AddPlanItems 从 cube 现拉商品名时没有凭证 ⇒ cube 返 Unauthenticated
	// ⇒ 本端点 500 ⇒ 结算审批的"建议盘点"永远建不出盘点单(2026-10-22 生产实踩)。
	h.RegisterInternalRoutes(r, appDB, forwardBearerToOutgoing())
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
		// ⚠️ X-Branch-ID 也要写进 **cubeclient 的 ctx key**:
		//   归档端点要问 cube-router「这个门店配了哪个数据源」,
		//   而 cube-router 的 /v1/branch-source 挂着 cube:read 守门
		//   ⇒ JWT 与门店都要被透传过去,否则 401 / 路由到错的门店。
		if branchID := strings.TrimSpace(c.Request.Header.Get("X-Branch-ID")); branchID != "" {
			ctx := c.Request.Context()
			ctx = cubeclient.WithBranchID(ctx, branchID)
			c.Request = c.Request.WithContext(ctx)
		}
		c.Next()
	}
}

// 静默引用 context(为后续按需扩展预留)
var _ = context.Background
