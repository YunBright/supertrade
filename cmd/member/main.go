// Package main 是 member dapr app 入口(会员域:主档 / 积分 / 储值 / 会员类别 / 门店配置)。
//
// 启动:`dapr run --app-id supertrade-member --app-port 8113 -- go run ./cmd/member`
//
// # 端口算术
//
//	app 8113 · dapr-http 3113(app−5000)· dapr-grpc 6113(app−2000)
//
// ⚠️ 8113 是**新 app 的起点**(8101–8112 已被占用)。
//
// # 配置
//
//	POSTGRES_DSN              必填(本服务拥有 6 张表)
//	AUTH_SERVICE_CLIENT_ID    机器身份(调 master-data 校验越权授权的工号+口令)
//	AUTH_SERVICE_CLIENT_SECRET
//	MASTER_DATA_APP_ID        可选,默认 supertrade-member 的对端 supertrade-master-data
//
// # 事件
//
//	发布:member.opened / member.points.changed / member.updated(经 pkg/outbox)
//	订阅:sale.paid(累计会员积分,幂等)
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

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/member/db"
	"github.com/YunBright/supertrade/internal/member/handler"
	"github.com/YunBright/supertrade/internal/member/model"
	"github.com/YunBright/supertrade/internal/member/service"
	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/YunBright/supertrade/pkg/middleware"
	"github.com/YunBright/supertrade/pkg/outbox"
	"gorm.io/gorm"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID:    "member",
		Port:     cmdbootstrap.AppPort(":8113"),
		Audience: "pos",
		OnStart:  initApp,
		// ⚠️ Dapr 内部路由**必须在 auth 中间件之前**注册:
		//   sidecar 调 /dapr/subscribe 与 /events/* 不带 Authorization 头,
		//   带鉴权就 401 ⇒ sidecar 永远拿不到订阅清单 ⇒ 事件一条也推不出去。
		RegisterDapr: registerDaprRoutes,
		Register:     registerRoutes,
	})
}

// appDeps 持有运行时单例。
var (
	appDB    *gorm.DB
	appSvc   *service.Service
	appUsers *userinfo.Client
)

// initApp 装配依赖(数据库 / 服务 / userd 客户端 / 发件箱发布器)。
func initApp() error {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		return fmt.Errorf("member: POSTGRES_DSN 必填(本服务拥有 members / member_types / member_point_ledger / member_account_ledger / member_settings / payment_ways 六张表)")
	}
	var err error
	appDB, err = db.OpenPostgres(dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	if err := appDB.AutoMigrate(
		&model.Member{},
		&model.MemberType{},
		&model.MemberPointLedger{},
		&model.MemberAccountLedger{},
		&model.MemberSettings{},
		&model.PaymentWay{},
		// 发件箱(与 pkg/outbox 的结构逐字一致 —— pos 与 member 各有一份)。
		&outbox.Outbox{},
	); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}
	appSvc = service.New(appDB)

	// 发件箱发布器:事件在**事务内**入队,后台 goroutine 轮询投递。
	//
	// ⚠️ publisher 创建失败 ⇒ 启动失败:sidecar 没起时事件永远投不出去,
	//   而症状是"会员事件不响",极难归因。
	pub, err := outbox.NewDaprPublisherFromEnv()
	if err != nil {
		return fmt.Errorf("dapr publisher: %w (确认 dapr run 已起)", err)
	}
	runner := outbox.NewRunner(appDB, pub, slog.Default())
	go runner.Start(context.Background())
	slog.Info("member: outbox 发布器已启动")

	// 越权授权链路:member 自己调 master-data 校验"工号 + 口令"(图4)。
	//
	// ⚠️ 没配机器身份时**不阻断启动**:授权链路不可用只影响"带口令的越权授权",
	//   而正常的"操作人自己有权限"的写操作不受影响。
	verifier, err := service.NewMasterDataVerifier()
	if err != nil {
		slog.Warn("member: dapr 客户端不可用,越权授权(工号+口令)链路降级", "err", err)
	} else {
		appSvc.SetEmployeeVerifier(verifier)
		slog.Info("member: 越权授权校验器已装配")
	}

	// userinfo 客户端:per-branch 权限矩阵(scope 守门 + 越权授权判定)。
	appUsers, err = userinfo.New("userd")
	if err != nil {
		return fmt.Errorf("userinfo client: %w", err)
	}
	// Warmup userd 跨主机冷握手(与 catalog / pricing 同一个理由:
	// 实测首次 7~12s 会撞 userinfo.Client.timeout 默认 10s)。
	warmupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := appUsers.Warmup(warmupCtx); err != nil {
		slog.Warn("userinfo warmup 失败,业务首次 userd 调用将走 cold path", "err", err)
	} else {
		slog.Info("userinfo warmup ok")
	}

	// 会员档案的 **cube 读路径**:
	//   该门店在 cube-router 里配了数据源(富百家 00 / 佳乐 01)⇒
	//   它的会员档案在外部系统里,本域 PG 没有行 ⇒ 读必须走 cube。
	//   没配数据源的自营店(10)⇒ 仍读本域 PG。
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
	cube := cubeclient.NewDaprCubeClient(
		daprCli,
		cubeclient.DefaultCubeAppID(),
		cubeclient.DefaultCubeQueryPath(),
	)
	// ⚠️ **两个一起注入**(解析器 + 读端):只注入一个 ⇒ 仍走本地 PG
	//   ⇒ 有数据源的门店又变成"没有会员档案"(本次要修的那个症状)。
	//
	// ⚠️ 解析器带 60s 缓存 + stale-on-error:每次读都打一次 dapr 往返不划算,
	//   而一次网络抖动不该让整个门店的会员服务挂掉。
	appSvc.SetCubeSource(cubesource.NewCachedResolver(cube, cubesource.DefaultTTL, slog.Default()), cube)
	// 归档导出器:配了数据源的门店导出会员档案时**原样转发**上游
	// (cube-router 的 /v1/export，字段名与导入契约逐字一致)。
	appSvc.SetArchiveExporter(service.NewDaprArchiveExporter(daprCli, cubeclient.DefaultCubeAppID()))
	slog.Info("member: cube 读路径已启用", "app_id", cubeclient.DefaultCubeAppID())

	slog.Info("member app initialized",
		"db_driver", "postgres",
		"schema", "member (6 tables) + outbox",
		"port", cmdbootstrap.AppPort(":8113"),
	)
	return nil
}

// registerRoutes 注册业务路由(在 auth 中间件之后)。
func registerRoutes(r *gin.Engine) {
	if appSvc == nil {
		panic("member: appSvc 未初始化,可能是 OnStart 失败")
	}
	// 全局挂 X-Branch-ID 解析中间件(handler 用 SingleBranchFromCtx 读)。
	r.Use(middleware.XBranchID())
	// bearer 透传:让 userinfo / master-data 的下游调用自动带上调用方身份。
	r.Use(forwardBearerToOutgoing())
	handler.New(appSvc, appUsers, slog.Default()).RegisterRoutes(r)
}

// registerDaprRoutes 在 auth 中间件**之前**注册 Dapr 订阅端点。
//
//	GET  /dapr/subscribe  → 订阅清单
//	POST /events/sale.paid → 事件分发(累计会员积分)
func registerDaprRoutes(r *gin.Engine) {
	if appSvc == nil {
		panic("member: appSvc 未初始化,可能是 OnStart 失败")
	}
	handler.New(appSvc, appUsers, slog.Default()).RegisterSubscribeRoutes(r, slog.Default())
}

// forwardBearerToOutgoing 把 caller's Authorization 头注入 ctx,
// 让 userinfo 内部调用自动 forward 给 auth(userd)。
//
// ⚠️ 与 catalog / inventory / master-data 同构。
//
// ⚠️ **同时**写进 cubeclient 的 ctx key:
// 该门店配了数据源(富百家 / 佳乐)时,会员档案走 cube,
// 而 cube-router 与 cube 都有 bearer 中间件 ⇒ JWT 必须被透传过去,
// 否则 401(而症状是"列表空了",排查方向会被带偏)。
func forwardBearerToOutgoing() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		if bearer := c.Request.Header.Get("Authorization"); bearer != "" {
			ctx = userinfo.WithBearer(ctx, bearer)
			ctx = cubeclient.WithBearer(ctx, bearer)
		}
		// X-Branch-ID 也要透传:cube-router 按它做 per-branch 路由。
		if b := middleware.SingleBranchFromCtx(c); b != "" {
			ctx = cubeclient.WithBranchID(ctx, b)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
