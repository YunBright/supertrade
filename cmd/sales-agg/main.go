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
//   - 存储是 **sales_compare_templates**（门店对比模板，全局共享配置）
//     与 **sales_daily_*（每日销售事实包，2026-10-17 新增）**。
//     报表本体仍是即席查询；每日事实包是**唯一被物化的销售数据**，
//     写者只有本服务，读者是 sales-insight（见 internal/salesdaily）。
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
	"strings"
	"time"

	dapr "github.com/dapr/go-sdk/client"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/salesagg/collect"
	"github.com/YunBright/supertrade/internal/salesagg/compare"
	"github.com/YunBright/supertrade/internal/salesagg/handler"
	"github.com/YunBright/supertrade/internal/salesagg/service"
	"github.com/YunBright/supertrade/internal/salesdaily"
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
	// appDaily 是每日销售事实包的存储（**唯一写者**）。
	appDaily *salesdaily.Store
	// appCube 是采集用的 cube 数据源（报表用 service.NewDaprCubeQuerier，
	// 采集用 collect.NewDaprSource —— 两者要查的维度组合不同）。
	appCube *cubeclient.DaprCubeClient
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
	appCube = cube
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

	// ---- 每日销售事实包落库（唯一写者）----
	//
	// ⚠️ 写者只能是 sales-agg，不能是 sales-insight：
	//
	//	每日销售统计卡片与 AI 销售日报是**同一份销售事实的两种呈现**。
	//	改造前两边各自现查 cube，于是 2026-10-16 出现同一个营业日的
	//	两个口径（品类 2024.6 元 vs 供应商 13003.8 元），差 6 倍、零报错。
	//	→ 只采一次、只落一次，两个消费方读同一行。
	//
	// AutoMigrate 放在**写者**这里：读侧（sales-insight）不建表，
	// 缺表时它会明确报错，而不会被静默地 AutoMigrate 成一个空壳。
	appDaily = salesdaily.NewStore(appDB, nil)
	if err := appDB.AutoMigrate(salesdaily.Models()...); err != nil {
		return fmt.Errorf("auto migrate sales_daily_*: %w", err)
	}

	// 报表服务**优先读已定稿事实库**，覆盖不全才整段现查源库。
	//
	// ⚠️ 换的是**数据来源**而不是接口：wechat-bot 的每日销售统计卡片与
	// Flutter 报表页调的都是 /reports/sales 与 /reports/sales/today，
	// 另开一个"读 PG"的端点就必须改两个调用方 ——
	// 而只要有一个没改，就又出现"卡片读 PG、报表页读 cube"的口径分叉。
	//
	// 这样一来，**卡片与 AI 日报读的是同一行**（两边都经过 PG）。
	appSvc = appSvc.WithDailyRange(appDaily)

	slog.Info("sales-agg app initialized",
		"cube_app_id", cubeclient.DefaultCubeAppID(),
		"cube_query_path", cubeclient.DefaultCubeQueryPath(),
		"db_driver", "postgres",
		"daily_collect", true,
	)

	// ---- 采集定时任务：sales-agg 自己注册，与 wechat-bot 无关 ----
	//
	// ⚠️ 为什么**不**由 wechat-bot 注册（这是 2026-10-18 的一次修正）：
	//
	//	采集的触发不能依赖推送服务。wechat-bot 挂了 / 配置写错 / 企微侧故障
	//	⇒ 数据不再落库，而**所有报表看起来完全正常**
	//	（消费侧会回退查源库）—— 这正是"静默降级让问题永远不被发现"
	//	的又一个结构性实例。
	//
	//	门店清单来自 auth 的 GET /auth/branches（在营门店），
	//	而不是 wechat-bot 的应用配置（推送开关）——
	//	后者回答的是"谁想看报告"，而采集是"在营门店级"的事。
	//
	// ⚠️ 注册失败**不阻断启动**：sidecar 可能比 app 晚几百毫秒就绪，
	//	auth 也可能还没起。让整个服务起不来只会得到一个没有线索的崩溃。
	startJobReconciler()
	return nil
}

// reconcileInterval 是重新对齐采集 job 的周期。
//
// ⚠️ 不能只在启动时注册一次：
//
//	auth 里新增一家门店后，没有人会再去通知 sales-agg。
//	每小时一次足够（门店开通不是分钟级的事），
//	而每次只是"按清单重注册"，幂等，代价可忽略。
const reconcileInterval = time.Hour

// startJobReconciler 启动采集 job 的对账循环。
func startJobReconciler() {
	// ⚠️ 门店清单走 **dapr service invocation** 打 app-id=userd
	//   （auth 的 userd 引擎），不直连 :8082 ——
	//   直连会绕过服务寻址这条边界，
	//   而本仓其它跨服务调用一律走 dapr，这里保持一致。
	//
	// ⚠️ app-id 必须是 **userd** 而不是 login（2026-10-18 实测）：
	//   auth-login 的 systemd unit 没有 `--config`，dapr 用默认 nameResolution，
	//   注册不进 gyy 用的 consul ⇒ 按 app-id=login 寻址必然失败
	//   （ERR_DIRECT_INVOKE: no healthy services found with AppID 'login'）。
	//   详见 collect.AuthAppID 的注释。
	sidecar := sidecarBaseURL()
	branchLister := collect.NewAuthBranchLister(sidecar, envStr("AUTH_USERD_APP_ID", collect.AuthAppID))
	if branchLister == nil {
		slog.Warn("sales-agg: 没有 sidecar（DAPR_HTTP_PORT 未设置），采集定时任务不会注册" +
			"（销售数字仍会实时查源库，只是不落库）")
		return
	}
	jobs := collect.NewDaprJobClient(sidecar)
	if jobs == nil {
		slog.Warn("sales-agg: 没有 sidecar，采集定时任务不会注册")
		return
	}
	rec := collect.NewReconciler(branchLister, jobs, collectCallbackAddress())

	ctx := context.Background()
	runOnce := func() {
		res, err := rec.Reconcile(ctx)
		if err != nil {
			// ⚠️ 只 Warn 不 panic：一次对账失败不该拖垮报表服务。
			//	而"采集没注册"这件事由 health 端点与 PG 里有没有新行暴露。
			slog.Warn("sales-agg: 采集定时任务对账失败", "err", err)
			return
		}
		lvl := slog.LevelInfo
		if len(res.Failed) > 0 {
			lvl = slog.LevelWarn
		}
		slog.Log(ctx, lvl, "sales-agg: 采集定时任务已对齐",
			"branches", res.Branches, "jobs", res.Jobs, "failed", res.Failed)
	}
	runOnce()

	go func() {
		t := time.NewTicker(reconcileInterval)
		defer t.Stop()
		for range t.C {
			runOnce()
		}
	}()
}

// collectCallbackAddress 是 job 回调打到的地址。
//
// ⚠️ 用 **app 端口**（APP_PORT），不是 DAPR_HTTP_PORT：
//
//	后者是 sidecar 端口，填进去会打到自己都打不通的地方。
//	（wechat-bot 的 job.go 有同样的告警，踩过一次。）
func collectCallbackAddress() string {
	port := strings.TrimPrefix(strings.TrimSpace(os.Getenv("APP_PORT")), ":")
	if port == "" {
		port = strings.TrimPrefix(defaultPort(), ":")
	}
	return "http://127.0.0.1:" + port + "/sales/daily/collect"
}

// defaultPort 是本地裸跑（无 sidecar）时的回退端口。
//
// ⚠️ 生产端口是 **8110**（8109 已被 settlement-auditor 占用），
//
//	线上以 unit 注入的 APP_PORT 为准。
func defaultPort() string { return ":8110" }

// sidecarBaseURL 是 sidecar 的 HTTP 根地址。
func sidecarBaseURL() string {
	port := strings.TrimSpace(os.Getenv("DAPR_HTTP_PORT"))
	if port == "" {
		return ""
	}
	return "http://127.0.0.1:" + port
}

// envStr 取环境变量，非空时返回它，否则返回默认值。
func envStr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
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
	// 每日销售事实落库。依赖 appDaily + appCube（OnStart 已建）。
	//
	// ⚠️ 注册在**鉴权链之内**（与上面两组一致），因为它同样会经
	//   forwardBearerToOutgoing 向 cube 发起调用 —— 侧写一条绕过它的路径
	//   会让这条调用缺 api-token，症状是"采集永远 401"而不是"没权限"。
	handler.NewDailyCollectHandler(collect.NewCollector(collect.NewDaprSource(appCube), appDaily)).
		RegisterRoutes(r)
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
