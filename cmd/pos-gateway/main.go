// Package main 是 supertrade-pos-gateway dapr app 入口(POS 客户端的 BFF / 聚合层)。
//
// 启动:`dapr run --app-id supertrade-pos-gateway --app-port 8115 -- go run ./cmd/pos-gateway`
//
// # 它做什么
//
//	GET  /home                    收银主页聚合(门店 + 班次 + 收银员 + 会员开关 + 付款方式)
//	GET  /config                  POS 端配置聚合(含数字键盘布局)
//	GET  /members/{id}/last-order 图7「上一单」一次返回{member, sale_lines[], payments[]}
//	GET  /products/search         商品搜索代理
//	GET  /prices                  查价代理
//	POST /sales                   开单代理(op_id 原样透传)
//
// # ⚠️ **它不拥有任何表** ⇒ 不需要 POSTGRES_DSN
//
// 这是**有意的**,不是漏配:少给 DSN 不是问题,多给 DSN 才是问题 ——
// 会让后人以为"它自己存数据",于是把状态放进 BFF ⇒ 状态散在两个进程里
// ⇒ 事务边界无从谈起。
//
// # ⚠️ **内部路径不带 /v1/**
//
// nginx 只剥一层前缀 ⇒ 内部再带 /v1/ 会让前端路径出现双层 v1。
// 前端调 `/api/v1/pos-gateway/home`。
//
// # ⚠️ 跨服务调用一律用 `authkit/daprclient`
//
// ⛔ **不要**用 `dapr.NewClient()`:它的 api-token 拦截器用
// `metadata.NewOutgoingContext` **整块替换** outgoing metadata,
// 把转发的 Authorization 抹掉 ⇒ 下游全 401 且**零报错**
// (2026-10-20 全网加 DAPR_API_TOKEN 之后立刻变成事故)。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/YunBright/authkit/daprclient"
	"github.com/YunBright/authkit/userinfo"
	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/posgateway/handler"
	"github.com/YunBright/supertrade/internal/posgateway/service"
	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// appDeps 持有运行时单例(在 initApp 里建立,registerRoutes 里消费)。
var (
	appCaller *service.DaprCaller
	appSvc    *service.Service
	appUser   *userinfo.Client
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID:    "pos-gateway",
		Audience: "pos",
		Port:     cmdbootstrap.AppPort(":8115"),
		OnStart:  initApp,
		Register: registerRoutes,
	})
}

// initApp 建立运行时依赖。
func initApp() error {
	// ⚠️ 必须用 authkit/daprclient(合并式 api-token 拦截器),不能用 dapr.NewClient()。
	cli, err := daprclient.New()
	if err != nil {
		return fmt.Errorf("dapr client: %w", err)
	}
	appCaller = service.NewDaprCaller(cli)
	appSvc = service.New(appCaller, slog.Default())

	// userd 客户端:per-branch scope 解析要用它
	// (rbac.HasAnyScopeWithBranch → userd /internal/users/{id}/permissions)。
	appUser, err = userinfo.New("userd")
	if err != nil {
		return fmt.Errorf("userinfo client: %w", err)
	}

	// Warmup userd 跨主机冷握手(与 catalog / master-data / pricing 同一个理由:
	// 实测首次 7~12s 会撞 userinfo.Client.timeout 默认 10s)。
	warmupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := appUser.Warmup(warmupCtx); err != nil {
		slog.Warn("userinfo warmup 失败,业务首次 userd 调用将走 cold path", "err", err)
	} else {
		slog.Info("userinfo warmup ok")
	}
	return nil
}

// registerRoutes 挂业务路由。
func registerRoutes(r *gin.Engine) {
	if appSvc == nil {
		panic("pos-gateway: appSvc 未初始化,可能是 OnStart 失败")
	}
	// 全局挂 X-Branch-ID 解析中间件(handler 用 SingleBranchFromCtx 读)。
	r.Use(middleware.XBranchID())
	// caller 的令牌 / 门店透传给下游。
	r.Use(forwardBearerToOutgoing())
	handler.New(appSvc, appUser, slog.Default()).RegisterRoutes(r)
}

// forwardBearerToOutgoing 把 caller's Authorization / X-Branch-ID 注入 ctx,
// 让下游调用自动 forward 给 member / pos / master-data / catalog / pricing。
//
// ⚠️ 与 catalog / master-data / pos / pricing 同构。
//
// ⚠️ **必须原样转发 caller's 令牌**:下游的权限是 per-branch **实时解析**的,
// 换成机器身份就丢了"这个人在这家店到底有没有权限"这条语义。
//
// 空 header(内部 job 路径)下游会以 401 拒绝,符合预期。
func forwardBearerToOutgoing() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		if bearer := c.Request.Header.Get("Authorization"); bearer != "" {
			// userinfo:per-branch scope 解析(userd 的 /internal/* 要 Authorization)。
			ctx = userinfo.WithBearer(ctx, bearer)
			// 本包:下游 dapr 调用要转发 authorization + x-branch-id。
			ctx = service.WithBearer(ctx, bearer)
		}
		if branchID := c.Request.Header.Get("X-Branch-ID"); branchID != "" {
			ctx = service.WithBranchID(ctx, branchID)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
