// Package main 是 cube-sync dapr app 入口(档案同步编排器)。
//
// 启动:`dapr run --app-id supertrade-cube-sync --app-port 8116 -- go run ./cmd/cube-sync`
//
// # 职责边界
//
// 本服务**只做编排**,不拥有任何数据:
//
//	导出  ← cube-router  GET  /v1/export      (它知道 branch → cube 实例的路由)
//	导入  → catalog      POST /archive/import (它拥有 products / categories /
//	                                          product_barcodes / suppliers 表)
//
// ⇒ 本服务**不连任何数据库**。它只需要两个下游的 dapr app-id + 一个服务账号。
//
// # 端口分配
//
//	cube-sync :8116
//
// ⚠️ 与 supertrade 其余 app 的端口号段相邻(8110~8115 已占),8116 是新号。
//
// # 配置(全部环境变量,见 internal/cubesync/config.go)
//
//	SYNC_SOURCE_BRANCH_ID   源门店(默认 00)
//	SYNC_TARGET_BRANCH_ID   目标门店(默认 00)
//	CUBE_ROUTER_APP_ID      导出方 dapr app-id(默认 supertrade-cube-router)
//	CATALOG_APP_ID          导入方 dapr app-id(默认 supertrade-catalog)
//	MEMBER_APP_ID           会员域导入方 dapr app-id(默认 supertrade-member)
//	SYNC_SCOPES             资料域,逗号分隔(默认 categories,suppliers,products,item_barcodes,prices,promotions,member_types,members)
//	SYNC_MODE               incremental / overwrite(默认 overwrite)
//	SYNC_CRON               "HH:MM"(本地时刻)或 6 字段 UTC cron(默认 03:30)
//	SYNC_PAGE_SIZE          导出每页行数(默认 5000)
//	SYNC_JOB_TOKEN          回调共享密钥(**必填**)
//	AUTH_SERVICE_CLIENT_ID / AUTH_SERVICE_CLIENT_SECRET  机器身份(**必填**)
//	AUTH_TOKEN_APP_ID       签发 token 的 auth app-id(默认 userd)
//	SYNC_CALLBACK_HOST      回调地址主机(默认 127.0.0.1)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/cubesync"
	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID:        "cube-sync",
		Port:         cmdbootstrap.AppPort(":8116"),
		OnStart:      initApp,
		RegisterDapr: registerDaprRoutes,
	})
}

// appDeps 持有运行时单例。
var (
	appCfg  cubesync.Config
	appSync *cubesync.Syncer
	appH    *cubesync.Handler
	appJobs cubesync.JobClient
)

// initApp 装配依赖。
func initApp() error {
	appCfg = cubesync.LoadConfig()
	if err := appCfg.Validate(); err != nil {
		// ⚠️ 配置不齐**拒绝启动**。
		// 理由:缺 SYNC_JOB_TOKEN 时回调端点在免鉴权窗口,
		// 那种"起来了但等于对内网开放"的状态比"起不来"危险得多。
		return err
	}
	if appCfg.SidecarBase == "" {
		return fmt.Errorf("cube-sync: 没有 sidecar(DAPR_HTTP_PORT 未设置)—— " +
			"本服务靠 dapr 调下游与注册 job,脱离 sidecar 没有意义")
	}

	token := cubesync.NewDaprTokenIssuer(
		appCfg.SidecarBase, appCfg.AuthAppID,
		appCfg.ServiceAccountID, appCfg.ServiceAccountSecret)

	exporter := cubesync.NewSidecarExporter(
		appCfg.SidecarBase, appCfg.CubeRouterAppID, appCfg.SourceBranchID)

	// 导入方:**一个 scope 一个归属方**。
	//
	// ⚠️ 售价与促销**不归 catalog** —— catalog.products 不存价(两个真相源)。
	// ⚠️ 库存快照归 **inventory** —— 它拥有 stock_levels 表。
	// ⚠️ 会员档案归 **member** —— 它拥有 members / member_types 表。
	//   ⇒ 这里有四个下游,靠路由表把它们分开。
	catalogImporter := cubesync.NewSidecarImporter(
		appCfg.SidecarBase, appCfg.CatalogAppID, appCfg.TargetBranchID)
	pricingImporter := cubesync.NewSidecarImporter(
		appCfg.SidecarBase, appCfg.PricingAppID, appCfg.TargetBranchID)
	inventoryImporter := cubesync.NewSidecarImporter(
		appCfg.SidecarBase, appCfg.InventoryAppID, appCfg.TargetBranchID)
	// ⚠️ member 的导入端点是 **POST /import**(不是 /archive/import)——
	//   前端路径是 /api/v1/member/import,nginx 剥掉 /api/v1/member/ 前缀后
	//   就是 /import ⇒ 这里必须按下游切路径,写死 /archive/import 会 404。
	memberImporter := cubesync.NewSidecarImporterAt(
		appCfg.SidecarBase, appCfg.MemberAppID, appCfg.TargetBranchID, "import")

	appSync = cubesync.NewSyncer(appCfg, token, exporter, []cubesync.Owner{
		{Importer: catalogImporter, Scopes: cubesync.DefaultCatalogScopes()},
		{Importer: pricingImporter, Scopes: cubesync.DefaultPricingScopes()},
		{Importer: inventoryImporter, Scopes: cubesync.DefaultInventoryScopes()},
		{Importer: memberImporter, Scopes: cubesync.DefaultMemberScopes()},
	}, slog.Default())
	appH = cubesync.NewHandler(appSync, appCfg.JobToken, slog.Default())
	appJobs = cubesync.NewDaprJobClient(appCfg.SidecarBase)

	slog.Info("cube-sync app initialized",
		"source_branch", appCfg.SourceBranchID,
		"target_branch", appCfg.TargetBranchID,
		"mode", appCfg.Mode,
		"scopes", appCfg.Scopes,
		"cron_spec", appCfg.CronSpec,
		"cron_utc", appCfg.Cron,
		"cube_router_app_id", appCfg.CubeRouterAppID,
		"catalog_app_id", appCfg.CatalogAppID,
		// ⚠️ 用 appH.JobName()(与回调路径同源),不用 appCfg.JobName() ——
		//   两者一旦分叉,dapr 打的路径就没人接 ⇒ 定时同步静默消失。
		"job", appH.JobName(),
		"job_callback_path", appH.JobCallbackPath(),
	)

	// 注册定时 job。
	//
	// ⚠️ **注册失败不阻断启动**:sidecar 可能比 app 晚几百毫秒就绪。
	// 让整个服务起不来只会得到一个没有线索的崩溃循环。
	startJobRegistrar()
	return nil
}

// jobReconcileInterval 是重新注册 job 的周期。
//
// ⚠️ 不能只在启动时注册一次:
// dapr scheduler 的 job 存在 sidecar 的本地存储里,
// sidecar 重启后 job 就没了 ⇒ 症状是"服务一直正常,但同步从某天起再也没跑过",
// 而**没有任何一行日志**说这件事。
//
// ⇒ 周期性地重注册(DELETE → POST,幂等)。
const jobReconcileInterval = 10 * time.Minute

// jobFastRetryInterval 是**启动阶段**的重试间隔。
//
// ⚠️ 为什么启动阶段要更快:
// 启动时 sidecar 通常还没监听 HTTP 端口(实测首次 POST /v1.0-alpha1/jobs
// 报 "connection refused")—— 而"job 没注册"= "每晚同步静默消失"。
// 等 10 分钟才重试 ⇒ 上线后第一个同步窗口已经错过。
//
// ⇒ 前 jobFastRetryWindow 内每 10s 试一次,之后回到对账节奏。
const jobFastRetryInterval = 10 * time.Second

// jobFastRetryWindow 是启动快重试的持续时长。
const jobFastRetryWindow = 3 * time.Minute

// startJobRegistrar 启动 job 注册循环。
func startJobRegistrar() {
	go func() {
		// ① 启动快重试:sidecar 还没起来时,每 10s 试一次。
		deadline := time.Now().Add(jobFastRetryWindow)
		for {
			registerJobWithLog()
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(jobFastRetryInterval)
		}
		// ② 之后回到对账节奏(每 10 分钟一次,幂等)。
		t := time.NewTicker(jobReconcileInterval)
		defer t.Stop()
		for range t.C {
			registerJobWithLog()
		}
	}()
}

// registerJobWithLog 注册(或重注册)定时同步 job,并把结果打进日志。
func registerJobWithLog() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := registerJob(ctx); err != nil {
		slog.Warn("cube-sync: 定时同步 job 注册失败(会重试)", "err", err,
			"job", appCfg.JobName())
		return
	}
	slog.Info("cube-sync: 定时同步 job 已注册",
		"job", appH.JobName(),
		"cron_utc", appCfg.Cron,
		"address", appCfg.JobCallbackURL())
}

// registerJob 注册(或重注册)定时同步 job。
func registerJob(ctx context.Context) error {
	data := map[string]any{
		"trigger": "dapr-job",
		// ⚠️ job 名用 appH.JobName() —— 回调路径由它算出,
		//   两边必须同源(见 internal/cubesync/handler.go 的 JobCallbackPath)。
		"job":           appH.JobName(),
		"source_branch": appCfg.SourceBranchID,
		"target_branch": appCfg.TargetBranchID,
		// ⚠️ 共享密钥放在 job data 里:回调端点在免鉴权窗口,
		// 而 dapr scheduler 的回调**不带任何 header**(job spec 无 headers 字段)
		// ⇒ 回调时能拿到的凭据只有 body。
		// 它只授予"触发本机跑一次同步"这一项能力,不是服务账号凭据。
		// 详见 internal/cubesync/handler.go 的包注释。
		"token": appCfg.JobToken,
	}
	return appJobs.Register(ctx, appH.JobName(), appCfg.Cron,
		appCfg.JobCallbackURL(), data)
}

// registerDaprRoutes 在 auth 中间件**之前**注册免鉴权端点。
//
// ⚠️ 顺序是安全不变量:改到中间件之后 ⇒ 每晚的同步都 401,
// 而症状是"服务正常、同步从未发生"(同款故障见 sales-agg 的 registerDaprRoutes)。
func registerDaprRoutes(r *gin.Engine) {
	if appH == nil {
		panic("cube-sync: appH 未初始化,可能是 OnStart 失败")
	}
	appH.RegisterDaprRoutes(r)
}
