// Package main 是 pricing dapr app 入口(售价 / 促销 / 会员 / 改价审批)。
//
// 启动:`dapr run --app-id supertrade-pricing --app-port 8111 -- go run ./cmd/pricing`
//
// # 2026-10-15:从空壳变成第一个真实端点
//
// 此前本服务只有一个 `GET /price-lists` 返空数组,且**没有 systemd unit、
// 没有 nginx 路由、从未部署过**。本次为 POP 价签打印落地促销查询,
// 把它变成一个真正在跑的 dapr app。
//
// # 无本地数据库
//
// 与 catalog / stocktake 不同,pricing **不需要 POSTGRES_DSN**:
// 它不拥有任何表,全部数据实时来自 cube(product 档案 + promotion 促销)。
// 促销时效性极强(门店随时在思迅里录促销单),落本地快照必然过期,
// 所以刻意不建表 —— 这也是这里没有 AutoMigrate 的原因。
//
// 端口分配(8101~8110 已被占用,8111 起):
//
//	notification-gateway 8101   master-data        8102   catalog        8103
//	inventory            8105   stocktake          8106   cube-router    8107
//	fresh-meat           8108   settlement-auditor 8109   sales-agg      8110
//	pricing              8111
package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/YunBright/authkit/userinfo"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubehttp"
	"github.com/YunBright/supertrade/internal/pricing/handler"
	"github.com/YunBright/supertrade/internal/pricing/service"
	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/YunBright/supertrade/pkg/middleware"
	"github.com/gin-gonic/gin"
)

// appDeps 持有运行时单例(在 initApp 里建立,registerRoutes 里消费)。
var (
	appCube  cubeclient.Client
	appPrice *service.PriceService
	appUser  *userinfo.Client
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID:    "supertrade-pricing",
		Port:     cmdbootstrap.AppPort(":8111"),
		OnStart:  initApp,
		Register: registerRoutes,
	})
}

func initApp() error {
	var err error
	if appCube, err = cubehttp.NewClient(); err != nil {
		return fmt.Errorf("cube client: %w", err)
	}
	appPrice = service.NewPriceService(appCube)

	if appUser, err = userinfo.New("userd"); err != nil {
		return fmt.Errorf("userinfo client: %w", err)
	}

	// Warmup userd 跨主机冷握手(与 catalog 同一个理由:实测首次 7~12s
	// 会撞 userinfo.Client.timeout 默认 10s)。提前烧掉,业务首次即 warm。
	warmupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := appUser.Warmup(warmupCtx); err != nil {
		slog.Warn("userinfo warmup 失败,业务首次 userd 调用将走 cold path", "err", err)
	} else {
		slog.Info("userinfo warmup ok")
	}
	return nil
}

func registerRoutes(r *gin.Engine) {
	if appPrice == nil {
		panic("pricing: appPrice 未初始化")
	}
	r.Use(middleware.XBranchID())
	r.Use(forwardBearerToOutgoing())
	handler.New(appPrice, appUser, nil).RegisterRoutes(r)
}

// forwardBearerToOutgoing 把 caller's Authorization / X-Branch-ID 注入 ctx,
// 让 cubeclient / userinfo 内部调用自动 forward 给下游
// (sidecar 把 outgoing gRPC metadata 转成 outgoing HTTP 头)。
//
// 与 catalog/main.go、inventory/main.go 同构。空 header(内部 job 路径)下游会 401,符合预期。
//
// # ⚠️ X-Branch-ID 必须一起注入(2026-10-16)
//
// middleware.XBranchID() 写的是 **gin.Context**,而 cubeclient.LoadCubeQuery 从
// **request.Context** 读门店再拼 outgoing metadata。少注入一次,cube-router 就收不到
// X-Branch-ID,返 400 `branch_required`,sidecar 把它折叠成 gRPC `Internal: Bad Request`,
// pricing 的错误映射再当成上游故障翻成 503 `cube_unavailable` —— 现场表现是
// "库存服务暂不可用",而 cube / cube-router / 源库全部健康。
//
// 同一天在 catalog 上踩到的同一个坑(它的 /products/search 对任何条码都失败)。
// 代价是一次真机部署 + 一次端到端验证,所以这里在写第一行 handler 时就带上。
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
