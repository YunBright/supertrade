// Package main 是 sales-agg dapr app 入口（销售报表 + 每日群推送数据源）。
//
// 启动：`dapr run --app-id supertrade-sales-agg --app-port 8110 -- go run ./cmd/sales-agg`
//
// ⚠️ 生产端口是 **8110**，不是下面 AppPort 的回退值 8109 —— 8109 已被
// `supertrade-settlement-auditor` 占用。dapr run 会把 `--app-port` 注入成
// `APP_PORT`，`AppPort` 优先读它，所以线上实际绑 8110；`:8109` 只是本地
// 裸 `go run`（无 sidecar）时的回退。本地调试时不会撞 8109，因为那时
// settlement-auditor 没跑。
//
// # 职责边界
//
// 本服务只做一件事：**把 cube 里的销售数据按维度聚合成报表**。
//
// 它**不**做（这些是别人的边界，越界就是架构腐化）：
//   - 不发企业微信消息 —— 企微出口唯一在 wechat-bot 仓（见 docs/16 §5 服务边界表）。
//     本服务只提供 GET /reports/sales/today，wechat-bot 经 dapr service
//     invocation 取数后自己渲染并发送。
//   - 存储只有**一张表** sales_compare_templates（门店对比模板，2026-10-17
//     门店对比功能引入的唯一持久化）—— 报表本体仍是即席查询。模板是
//     全局共享配置，不是业务流水；POSTGRES_DSN 由此而来。
//   - 不做门店到 cube 实例的路由 —— 那是 cube-router 的职责，
//     本服务只带 X-Branch-ID 过去。
//
// # 毛利口径（重要）
//
// 「参考毛利」是基于 base_price1 的**估算值**，覆盖率极低
// （✅ 实测当日 25.3%，64 个类别组里多组为 0%）。详见
// internal/salesagg/service 包注释，以及本服务的响应里的
// `cost_coverage` / `margin_available` 字段。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	dapr "github.com/dapr/go-sdk/client"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/salesagg/compare"
	"github.com/YunBright/supertrade/internal/salesagg/handler"
	"github.com/YunBright/supertrade/internal/salesagg/service"
	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/YunBright/supertrade/pkg/middleware"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID:        "sales-agg",
		Port:         cmdbootstrap.AppPort(":8109"),
		OnStart:      initApp,
		RegisterDapr: registerDaprRoutes,
		Register:     registerRoutes,
	})
}

// appSvc 是 handler 与 service 共享的单例。
//
// 用 package var 是因为 cmdbootstrap 的 OnStart / Register 回调签名不返回
// 外部状态；与 supertrade 其余服务（stocktake / settlement-auditor）一致。
var (
	appSvc   *service.Service
	appDB    *gorm.DB
	appStore *compare.Store
)

func initApp() error {
	// ⚠️ 这里**不能**用 cubehttp.NewClient()：它返回的是窄接口
	// cubeclient.Client（本服务需要其中的 GetProduct/GetStock 等方法），
	// 而报表要用的是 DaprCubeClient.LoadCubeQuery —— 那个方法不在窄接口上。
	// 窄接口存在的意义是"只暴露业务真正用到的那几个方法"，
	// 报表服务用到的是更底层的通用查询，所以自己构造完整客户端。
	daprCli, err := dapr.NewClient()
	if err != nil {
		return fmt.Errorf("dapr.NewClient: %w (确认 dapr run 已起)", err)
	}
	cube := cubeclient.NewDaprCubeClient(
		daprCli,
		cubeclient.DefaultCubeAppID(),
		cubeclient.DefaultCubeQueryPath(),
	)
	appSvc = service.New(service.NewDaprCubeQuerier(cube))

	// 门店对比模板的持久化（唯一一张表）。DSN 必填 fail-fast，
	// 与 stocktake / settlement-auditor 同口径 —— 不允许"起来但存不了"。
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		return fmt.Errorf("POSTGRES_DSN 必填(sales-agg 门店对比模板需要持久化,无 fallback)")
	}
	appDB, err = compare.OpenDB(dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	if err := appDB.AutoMigrate(&compare.Template{}); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}
	appStore = compare.NewStore(appDB)

	slog.Info("sales-agg app initialized",
		"cube_app_id", cubeclient.DefaultCubeAppID(),
		"cube_query_path", cubeclient.DefaultCubeQueryPath(),
		"db_driver", "postgres",
	)
	return nil
}

func registerRoutes(r *gin.Engine) {
	if appSvc == nil {
		panic("sales-agg: appSvc 未初始化，可能是 OnStart 失败")
	}
	// 读 X-Branch-ID，供 handler 透传给 cube-router 做 per-branch 路由。
	// 不强求存在：报表允许"该 cube 实例内全部门店"（branch 留空）。
	// 门店对比 handler 不读它 —— 那组端点的授权只认 token claims。
	r.Use(middleware.XBranchID())
	// 把 caller 的 Authorization 注入 ctx，向 cube 透传 JWT。
	//
	// ⚠️ cube-gateway 的 sidecar 配了 middleware.http.bearer，无 token 必被
	// 401（Unauthenticated）。而 wechat-bot 走 dapr service invocation 调本服务时
	// **不带用户 JWT** —— 那条路径下 cube 也会 401。
	// 解法是给 cube 配 api-token（见 deployer 的 dapr/authkey.yaml），
	// 而不是让本服务在无 token 时伪造一个。
	r.Use(forwardBearerToOutgoing())
	handler.New(appSvc).RegisterRoutes(r)
	// 门店对比（模板 CRUD + 逐店取数）。依赖 appStore（OnStart 已建）。
	handler.NewCompare(appSvc, appStore).RegisterRoutes(r)
}

// registerDaprRoutes 在 auth 中间件**之前**注册免鉴权端点。
//
// 本服务不订阅任何 topic（无 /dapr/subscribe），开这个窗口只为挂兜底
// 清理端点：契约测试收尾清理时未必持有 token。公网安全由 nginx 的
// `location ^~ /api/v1/sales-agg/internal/ { return 404; }` 承担
// （回归锁见 deployer/tests/config/nginx_internal_test.go）。
func registerDaprRoutes(r *gin.Engine) {
	handler.RegisterPurgeRoutes(r, appDB)
}

// forwardBearerToOutgoing 把 gin request 的 Authorization header 注入 ctx。
//
// 与 stocktake 的同名函数逐字一致（都是 cubeclient.WithBearer 的标准用法）：
// handler 收到的 Authorization → ctx → cubeclient 拼进 outgoing gRPC metadata
// → sidecar 转成 outgoing HTTP Authorization 给 cube-gateway。
func forwardBearerToOutgoing() gin.HandlerFunc {
	return func(c *gin.Context) {
		if bearer := c.Request.Header.Get("Authorization"); bearer != "" {
			c.Request = c.Request.WithContext(
				cubeclient.WithBearer(c.Request.Context(), bearer),
			)
		}
		c.Next()
	}
}

// 静默引用 context(未来接 dapr job 时用)。
var _ = context.Background
var _ = os.Getenv
