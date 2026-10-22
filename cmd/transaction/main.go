// Package main 是 transaction dapr app 入口(商品销售流水 + 收银流水)。
//
// 启动:`dapr run --app-id supertrade-transaction --app-port 8119 -- go run ./cmd/transaction`
//
// # 端口分配
//
//	transaction 8119(dapr http 3119 / grpc 6119)
//
// 约定:app-port − 5000 = dapr http 端口,app-port − 2000 = dapr grpc 端口。
//
// # 为什么这个服务是**新**服务
//
// 2026-10-22 之前,流水查询散在各处(catalog 的商品选择、pos 的单据、
// settlement-auditor 的结算核对),每个域自己查自己那份 ⇒ 同一个"今天的销售"
// 在三个页面上是三个数。
//
// ⇒ 本服务把"逐笔流水"收成一条读路径:
// 商品销售流水(cube sale_detail)+ 收银流水(cube payflow)。
//
// # ⚠️ 数据来源按「门店配没配数据源」分流(判据唯一:internal/cubesource)
//
//	配了源 ⇒ 走 cube(门店专属 cube 实例)
//	没配源 ⇒ 走本域 PG(sale_flows / pay_flows)
//	问不到 / 被停用 ⇒ **503**,不回落本地。
//
// ⚠️ **回落本地 = 把一次链路故障显示成"这家店没有流水"** ——
// 那是 2026-10-21 生产事故的形状,禁止回落。
//
// # ⚠️ POSTGRES_DSN 缺失即拒绝启动
//
// 缺数据库却照常起来 ⇒ 自营店(门店 10)的流水永远查不了,
// 而症状是"查不出来",真因(DSN 没配)不出现在任何日志里。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/YunBright/authkit/daprclient"
	"github.com/YunBright/authkit/userinfo"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/transaction/db"
	"github.com/YunBright/supertrade/internal/transaction/handler"
	"github.com/YunBright/supertrade/internal/transaction/model"
	"github.com/YunBright/supertrade/internal/transaction/service"
	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/YunBright/supertrade/pkg/middleware"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		// ⚠️ AppID 必须是**短名**(与 JWT_AUDIENCE 对齐),不是 dapr app-id。
		//
		//	aud 的值 = Go 侧 cmdbootstrap.AppID 的短名,不是 dapr app-id。
		//	本项目两者**故意不同**:dapr 寻址用 `--app-id supertrade-transaction`,
		//	而签发的 token 的 aud 里必须带 `transaction`。
		//	⇒ 写成 "supertrade-transaction" ⇒ RequireAudience 报
		//	  401 invalid_audience「aud 不包含目标服务 supertrade-transaction」,
		//	  而它自己的 /healthz 仍 200(公开端点在中间件之前注册)。
		AppID:    "transaction",
		Port:     cmdbootstrap.AppPort(":8119"),
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
		return fmt.Errorf("transaction: POSTGRES_DSN 必填(没配数据源的门店读本域流水表;无 fallback)")
	}
	var err error
	appDB, err = db.OpenPostgres(dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	if err := appDB.AutoMigrate(
		&model.SaleFlow{},
		&model.PayFlow{},
	); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}
	appSvc = service.New(appDB, slog.Default())

	// 业务时区:源库(思迅 MSSQL)的 oper_date 记的是**门店当地的墙上时间**,
	// 不带时区 ⇒ 前端发 UTC 时必须换算成墙钟再比,否则差 8 小时。
	// 默认 Asia/Shanghai(UTC+8),可用 TRANSACTION_BIZ_TZ 覆盖
	// (与 fresh-meat 的 FRESHMEAT_BIZ_TZ / sales-insight 的 SALESINSIGHT_BIZ_TZ 同一套约定)。
	if tzName := strings.TrimSpace(os.Getenv("TRANSACTION_BIZ_TZ")); tzName != "" {
		if loc, err := time.LoadLocation(tzName); err == nil {
			service.SetBizLoc(loc)
			slog.Info("transaction: bizTZ from env", "name", tzName)
		} else {
			slog.Warn("transaction: TRANSACTION_BIZ_TZ 解析失败,沿用默认", "name", tzName, "err", err)
		}
	}

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
	//   查到的不是它自己的数据(而是本地那份空表),
	//   症状是"富百家门店看不到流水",而真因是"没装判据"。
	//
	// ⚠️ 解析器带 60s 缓存 + stale-on-error:每次读都打一次 dapr 往返不划算,
	//   而一次网络抖动不该让整个门店的服务挂掉。
	appSrc = cubesource.NewCachedResolver(appCube, cubesource.DefaultTTL, slog.Default())
	// ⚠️ **解析器与 cube 读端必须一起注入**:只注入解析器 ⇒ 走 cube 但没有读端;
	//   只注入读端 ⇒ 有数据源的门店又变成本地。症状完全一样,零报错。
	appSvc.SetCubeSource(appSrc, appCube)
	slog.Info("transaction: 门店数据源路由已启用",
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
		panic("transaction: appSvc 未初始化")
	}
	// 全局挂 X-Branch-ID 解析(所有业务端点的门店来源)。
	r.Use(middleware.XBranchID())
	r.Use(forwardBearerToOutgoing())
	handler.New(appSvc, appUser, slog.Default()).RegisterRoutes(r)
}

// forwardBearerToOutgoing 把 caller 的 Authorization / X-Branch-ID header 注入 ctx,
// 让 cubeclient / userinfo 内部调用自动 forward 给下游。
//
// ⚠️ X-Branch-ID 必须一起注入(2026-10-16 的生产故障):
// middleware.XBranchID() 只把门店写进 **gin.Context**,而 cubeclient 从
// **request.Context** 读门店再拼 outgoing metadata。少注入一次,
// cube-router 就收不到 X-Branch-ID ⇒ 400 branch_required ⇒ 被 sidecar 折叠成
// gRPC Internal ⇒ 错误分类器把它翻成 502/503「库存服务暂不可用」,
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
