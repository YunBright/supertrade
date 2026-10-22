// Package main 是 **cube-async** dapr app 入口(服务层数据搬运编排器)。
//
// 启动:`dapr run --app-id supertrade-cube-async --app-port 8118 -- go run ./cmd/cube-async`
//
// # 它与 cube-sync 的区别（这是本服务存在的理由）
//
//	cube-sync  : 导出直接问 **cube-router**（GET /v1/export）
//	             ⇒ 它只能搬「cube 里的数据」，而且它知道 cube 的存在。
//
//	cube-async : 导出问**各服务自己的归档导出端口**（GET /archive/export）
//	             ⇒ 搬运方**不认 cube / 本地**这件事：
//	                源门店配了数据源 ⇒ 服务的导出端口给 cube 的实时数据；
//	                源门店没配数据源   ⇒ 给源门店本地表的数据。
//	             ⇒ 同一个编排器既能搬「外部系统的数据」，也能搬「本域的数据」。
//
// # 为什么要有它（2026-10-21 决策）
//
//	门店 00 / 01 配了数据源 ⇒ 它们的商品 / 价格 / 库存 / 会员都在外部 ERP 里，
//	本域 PG **不该**留副本（副本会与外部数据漂移 ⇒ 读出来的是旧价）。
//	而门店 10（自营店）需要一份**可用的基础数据**才能开张。
//
//	⇒ 搬运方向：**00（走 cube）→ 10（走本地）**，覆盖式。
//	⇒ 而"停掉对 00 的导入 + 删掉 00 的本地副本"由主同步实例下线完成。
//
// # 职责边界
//
//	本服务**只做编排**,不拥有任何数据、不连任何数据库。
//
//	导出 ← 各服务  GET  /archive/export  （catalog / pricing / inventory / member）
//	导入 ← 各服务  POST /archive/import （同上一批服务）
//
// # 端口分配
//
//	cube-async :8118
//
// ⚠️ 8116/8117 已被 cube-sync / cube-sync-member 占 ⇒ 8118 是新号。
//
// # 配置（全部环境变量，见 internal/cubesync/config.go）
//
//	SYNC_SOURCE_BRANCH_ID   源门店（默认 00）
//	SYNC_TARGET_BRANCH_ID   目标门店（默认 10）
//	SYNC_SCOPES             资料域，逗号分隔
//	SYNC_MODE               incremental / overwrite（默认 overwrite）
//	SYNC_CRON               "HH:MM"（本地时刻）或 6 字段 UTC cron（默认 03:30）
//	SYNC_JOB_PREFIX         job 名前缀（默认 cube-async）
//	SYNC_PAGE_SIZE          导出每页行数（默认 5000）
//	SYNC_JOB_TOKEN          回调共享密钥（**必填**）
//	SYNC_INCREMENTAL_SCOPES 走增量的资料域（默认空 = 全走全量）
//	SYNC_WATERMARK_FILE     水位文件路径（空 = 不启用水位 = 全走全量）
//	SYNC_WATERMARK_OVERLAP_SECONDS 水位回退秒数（默认 5）
//	SYNC_WATERMARK_MAX_AGE_DAYS  距上次全量校准的天数（默认 7，超期强制全量）
//	AUTH_SERVICE_CLIENT_ID / AUTH_SERVICE_CLIENT_SECRET  机器身份（**必填**）
//	CATALOG_APP_ID / PRICING_APP_ID / INVENTORY_APP_ID / MEMBER_APP_ID  下游 app-id
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
		AppID:        "cube-async",
		Port:         cmdbootstrap.AppPort(":8118"),
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

// archiveExportPath 是各服务的**归档导出**路径（相对 app 根）。
//
// ⚠️ 不能带 "/api/v1/<svc>/" 前缀：dapr service invocation 打到 app 的**根**路径，
// nginx 剥掉的那段前缀在 app 侧不存在 ⇒ 那样写必然 404。
const archiveExportPath = "archive/export"

// initApp 装配依赖。
func initApp() error {
	appCfg = cubesync.LoadConfig()
	if err := appCfg.Validate(); err != nil {
		// ⚠️ 配置不齐**拒绝启动**：缺 SYNC_JOB_TOKEN 时回调端点在免鉴权窗口，
		// 那种"起来了但等于对内网开放"的状态比"起不来"危险得多。
		return err
	}
	if appCfg.SidecarBase == "" {
		return fmt.Errorf("cube-async: 没有 sidecar(DAPR_HTTP_PORT 未设置)—— " +
			"本服务靠 dapr 调下游与注册 job,脱离 sidecar 没有意义")
	}

	token := cubesync.NewDaprTokenIssuer(
		appCfg.SidecarBase, appCfg.AuthAppID,
		appCfg.ServiceAccountID, appCfg.ServiceAccountSecret)

	// 导出方：**各服务自己的**归档导出端口。
	//
	// ⚠️ scope → 服务的映射必须与导入方**同一份**：
	// 导出与导入不是同一个服务 ⇒ 拼错了只会表现为"导出了 A 的数据、写进了 B 的库"。
	endpoints := cubesync.ArchiveExportEndpoints(cubesync.ArchiveOwners{
		CatalogAppID:   appCfg.CatalogAppID,
		PricingAppID:   appCfg.PricingAppID,
		InventoryAppID: appCfg.InventoryAppID,
		MemberAppID:    appCfg.MemberAppID,
	}, archiveExportPath)
	exporter := cubesync.NewServiceExporter(
		appCfg.SidecarBase, appCfg.SourceBranchID, endpoints)

	// 导入方:一个 scope 一个归属方(与 cube-sync 同款路由表)。
	catalogImporter := cubesync.NewSidecarImporter(
		appCfg.SidecarBase, appCfg.CatalogAppID, appCfg.TargetBranchID)
	pricingImporter := cubesync.NewSidecarImporter(
		appCfg.SidecarBase, appCfg.PricingAppID, appCfg.TargetBranchID)
	inventoryImporter := cubesync.NewSidecarImporter(
		appCfg.SidecarBase, appCfg.InventoryAppID, appCfg.TargetBranchID)
	// ⚠️ member 的导入端点是 **POST /import**(不是 /archive/import)。
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

	slog.Info("cube-async app initialized",
		"source_branch", appCfg.SourceBranchID,
		"target_branch", appCfg.TargetBranchID,
		"mode", appCfg.Mode,
		"scopes", appCfg.Scopes,
		// ⚠️ 增量三件套必须打进日志:
		//   排障时第一个要确认的就是"这一轮到底走没走增量"
		//   —— 而它在**导出响应与水位文件**里,服务日志里没有 ⇒
		//   症状是"同步好像没变快",却查不出配置有没有生效。
		"incremental_scopes", appCfg.IncrementalScopes,
		"watermark_file", appCfg.WatermarkFile,
		"watermark_overlap_seconds", appCfg.WatermarkOverlapSeconds,
		"watermark_max_age_days", appCfg.WatermarkMaxAgeDays,
		"cron_spec", appCfg.CronSpec,
		"cron_utc", appCfg.Cron,
		// ⚠️ 用 appH.JobName()(与回调路径同源),不用 appCfg.JobName() ——
		//   两者一旦分叉,dapr 打的路径就没人接 ⇒ 定时同步静默消失。
		"job", appH.JobName(),
		"job_callback_path", appH.JobCallbackPath(),
	)

	// 注册定时 job。
	//
	// ⚠️ **注册失败不阻断启动**：sidecar 可能比 app 晚几百毫秒就绪。
	startJobRegistrar()
	return nil
}

// jobReconcileInterval 是重新注册 job 的周期。
//
// ⚠️ 不能只在启动时注册一次：dapr scheduler 的 job 存在 sidecar 的本地存储里,
// sidecar 重启后 job 就没了 ⇒ 症状是"服务一直正常,但同步从某天起再也没跑过"。
const jobReconcileInterval = 10 * time.Minute

// jobFastRetryInterval 是**启动阶段**的重试间隔。
const jobFastRetryInterval = 10 * time.Second

// jobFastRetryWindow 是启动快重试的持续时长。
const jobFastRetryWindow = 3 * time.Minute

// startJobRegistrar 启动 job 注册循环。
func startJobRegistrar() {
	go func() {
		deadline := time.Now().Add(jobFastRetryWindow)
		for {
			registerJobWithLog()
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(jobFastRetryInterval)
		}
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
		slog.Warn("cube-async: 定时同步 job 注册失败(会重试)", "err", err,
			"job", appCfg.JobName())
		return
	}
	slog.Info("cube-async: 定时同步 job 已注册",
		"job", appH.JobName(),
		"cron_utc", appCfg.Cron,
		"address", appCfg.JobCallbackURL())
}

// registerJob 注册(或重注册)定时同步 job。
func registerJob(ctx context.Context) error {
	data := map[string]any{
		"trigger":       "dapr-job",
		// ⚠️ job 名用 appH.JobName() —— 回调路径由它算出,
		//   两边必须同源(见 internal/cubesync/handler.go 的 JobCallbackPath)。
		"job":           appH.JobName(),
		"source_branch": appCfg.SourceBranchID,
		"target_branch": appCfg.TargetBranchID,
		// ⚠️ 共享密钥放在 job data 里:回调端点在免鉴权窗口,
		// 而 dapr scheduler 的回调**不带任何 header**(job spec 无 headers 字段)
		// ⇒ 回调时能拿到的凭据只有 body。
		// 它只授予"触发本机跑一次同步"这一项能力,不是服务账号凭据。
		"token": appCfg.JobToken,
	}
	return appJobs.Register(ctx, appH.JobName(), appCfg.Cron,
		appCfg.JobCallbackURL(), data)
}

// registerDaprRoutes 在 auth 中间件**之前**注册免鉴权端点。
//
// ⚠️ 顺序是安全不变量:改到中间件之后 ⇒ 每次同步都 401,
// 而症状是"服务正常、同步从未发生"。
func registerDaprRoutes(r *gin.Engine) {
	if appH == nil {
		panic("cube-async: appH 未初始化,可能是 OnStart 失败")
	}
	appH.RegisterDaprRoutes(r)
}
