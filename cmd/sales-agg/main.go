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

	"github.com/YunBright/authkit/daprclient"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/salesagg/collect"
	"github.com/YunBright/supertrade/internal/salesagg/compare"
	// ⚠️ 别名：internal/salesagg/service 也叫 service，不加别名编译不过。
	//	（别名本身不是风格问题，是两个同名包的必然冲突。）
	cubesrvsvc "github.com/YunBright/supertrade/internal/cube-router/service"
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
	// appCreds 是门店采集的服务账号凭据存储（**共享 PG**，非本机文件）。
	appCreds *collect.CredentialStore
	// appBearer 为某门店换机器 token。主密钥缺失时为 nil，
	// 此时采集会明确失败而不是无声地不带 token 去打 cube。
	appBearer collect.BearerResolver
	// appCipher 是凭据的加解密器（主密钥来自环境变量）。
	// 主密钥缺失时为 nil ⇒ 采集开关端点会明确拒绝开启。
	appCipher *collect.Cipher
	// appCtl 是采集开关的编排器（开启 / 关停 / 查状态）。
	appCtl *collect.Control
)

func initApp() error {
	// ⚠️ 这里**不能**用 cubehttp.NewClient()：它返回的是窄接口
	// cubeclient.Client（本服务需要其中的 GetProduct/GetStock 等方法），
	// 而报表要用的是 DaprCubeClient.LoadCubeQuery —— 那个方法不在窄接口上。
	// 窄接口存在的意义是"只暴露业务真正用到的那几个方法"，
	// 报表服务用到的是更底层的通用查询，所以自己构造完整客户端。
	// ⚠️ 用 daprclient.New() 而不是 dapr.NewClient():
	//   go-sdk 的 api-token 拦截器会**整块替换** outgoing metadata,
	//   把本客户端转发给 cube 的 Authorization 头抹掉
	//   (2026-10-20 全网加 DAPR_API_TOKEN 之后暴露的)。
	//   采集身份(门店服务账号的 bearer)正是靠这个头过去的。
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

	// ---- 门店采集的服务账号凭据（机器身份）----
	//
	// dapr 的 job 回调不带 Authorization 头（实测 job spec 无 headers 字段），
	// 而 cube-gateway 侧有 auth-validator（aud=userd）⇒ 无 token 必 401。
	// 这正是本轮改造前的真实故障：
	//	collect_failed: cube 拒绝调用者凭证: Unauthenticated: Unauthorized
	//
	// 凭据存在**共享 PG** 而不是本机文件，理由是 dapr scheduler 的投递语义
	// （HA 3 副本、无 leader election、触发投递到**随机**副本）：
	// 本机文件在多副本下会变成"有的节点有、有的没有"，
	// 症状是**随机 401**，而一天两次采集落在哪个副本不确定 ⇒ 极难排查。
	appCreds = collect.NewCredentialStore(appDB)
	if err := appDB.AutoMigrate(collect.CredentialModels()...); err != nil {
		return fmt.Errorf("auto migrate branch_collect_credentials: %w", err)
	}

	// 主密钥缺失**不阻断启动**，但必须响亮地 Warn：
	// 报表、对比、实时查询都不依赖它，起来了才能让用户看到界面与报错；
	// 而采集会以"换不到 token"失败，真因指向这里的一行日志。
	//
	// ⚠️ 绝不把主密钥写进日志（只写长度）。
	if raw := envStr("SALES_COLLECT_SECRET_KEY", ""); raw == "" {
		slog.Warn("sales-agg: 未配置 SALES_COLLECT_SECRET_KEY —— 门店采集的定时任务无法换机器 token，" +
			"每日销售事实将采集失败（报表仍可实时查源库）。开启「每日销售数据」前必须先配置它")
		appBearer = nil
	} else {
		ci, err := collect.CipherFromEnvKey(raw)
		if err != nil {
			return fmt.Errorf("采集凭据主密钥不可用（拒绝以降级密钥继续）: %w", err)
		}
		appCipher = ci
		appBearer = collect.NewStoreCipherIssuer(appCreds, ci,
			collect.NewDaprTokenIssuer(sidecarBaseURL(), envStr("AUTH_TOKEN_APP_ID", collect.TokenAuthAppID)))
	}

	// ---- 采集开关（开启 / 关停）----
	//
	// 前置校验用 cube-router 的数据源映射（同一个 PG 的 cube_router schema），
	// 所以本服务直接建一个只读解析器。
	appCtl = collect.NewControl(
		collect.NewCubeSourceChecker(cubesrvsvc.NewService(appDB)),
		collect.NewDaprServiceAccountClient(sidecarBaseURL(), envStr("AUTH_USERD_APP_ID", collect.AuthAppID)),
		appCreds,
		appCipher,
		collect.NewBranchJobRegistrar(collect.NewDaprJobClient(sidecarBaseURL()), collectCallbackAddress()),
		nil,
	)

	slog.Info("sales-agg app initialized",
		"cube_app_id", cubeclient.DefaultCubeAppID(),
		"cube_query_path", cubeclient.DefaultCubeQueryPath(),
		"db_driver", "postgres",
		"daily_collect", true,
	)

	// 采集凭据健康自检：**点名**报出解不开的门店。
	//
	// ⚠️ 为什么必须在启动时做:
	//
	//	主密钥被换过 ⇒ 旧密文永久解不开 ⇒ 采集端点每次 503,
	//	而"开启采集"在旧代码里一直返回成功 ⇒ 用户点了没反应、
	//	界面显示"已开启"、采集永远坏着,而且**没有任何一行日志**指向
	//	"主密钥被换过"这件事。
	//
	//	⇒ 把它放到启动日志的第一行,而不是让人从 503 的密码学文案反推。
	logBrokenCredentials(appCtl)

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

// logBrokenCredentials 记录"已开启但密文解不开"的门店。
//
// ⚠️ 用 Error 级别而不是 Warn：这不是"可能有风险"，而是
//
//	"这个门店的采集**现在就是坏的**"——每次采集都会 503，
//	而报表还能实时查源库 ⇒ 数字看起来正常，只是没落库。
//
//	文案必须点出"主密钥被更换"这个真因：采集端点返的
//	"cipher: message authentication failed" 指向密码学，
//	会让人去查密文有没有被改，而真因是运维换了 EnvironmentFile 里的主密钥。
func logBrokenCredentials(ctl *collect.Control) {
	if ctl == nil {
		return
	}
	broken, err := ctl.BrokenCredentials(context.Background())
	if err != nil {
		slog.Warn("sales-agg: 检查采集凭据可解密性失败", "err", err)
		return
	}
	if len(broken) == 0 {
		return
	}
	slog.Error("sales-agg: 这些门店的采集凭据解不开（主密钥可能已被更换）—— 采集会持续 503，"+
		"请对它们重新开启一次采集（会自动轮换服务账号密钥并用当前主密钥重新加密落库）",
		"branches", broken)
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
	rec := collect.NewReconciler(branchLister, appCreds, jobs, collectCallbackAddress())

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
		// ⚠️ 每次对账都顺手查一次凭据可解密性:
		//	主密钥是在**运行期**被换的(换 unit + 重启)⇒ 只在启动时查一次不够,
		//	而对账每小时跑一次,正好覆盖"服务没重启但密钥换了"的情况。
		logBrokenCredentials(appCtl)
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
	// 采集开关（开启 / 关停 / 查状态）。
	//
	// ⚠️ 这些端点在**鉴权链内**（走 users 组的 JWT 校验），
	//	而采集端点 /sales/daily/collect 在免鉴权窗口（registerDaprRoutes）。
	//	两者身份来源不同（真人 vs 服务账号），挂错窗口的后果分别是
	//	"任何人都能开/关采集" 与 "每晚采集必 401"。
	handler.NewCollectControlHandler(appCtl).RegisterRoutes(r)
}

// registerDaprRoutes 在 auth 中间件**之前**注册免鉴权端点。
//
// # 两类端点共用这个窗口
//
// ① 兜底清理端点（契约测试收尾未必持有 token）；
// ② **每日销售事实采集端点**。
//
// # ⚠️ 为什么采集端点必须在窗口内（2026-10-18 上线前发现，之前挂错了）
//
// ✅ MEASURED 2026-10-18（gyy 生产实测）：
//
//	curl http://127.0.0.1:8110/sales/daily/collect?store=00
//	→ 401 {"code":"unauthenticated","message":"缺少已签 token"}
//
// 而 dapr scheduler 的回调**不带任何凭据**（job 注册体只有
// schedule/data/address 三项，dapr scheduler 的 job spec 也没有 headers 字段），
// wechat-bot 的同类回调端点同样是"挂 engine 根且不挂 JWT"。
//
// → 挂在鉴权链内的后果是：每晚 22:45 与次日 01:05 的采集**每一次都 401**，
//
//	而症状极其安静：服务照常启动、healthz 200、job 注册成功
//	（"采集定时任务已对齐 branches=7 jobs=14"）、
//	报表照出（sales-insight 回退 cube）。**没有任何一行说采集没跑过。**
//
// # ⚠️ 之前那条"必须挂在鉴权链内否则 cube 调用缺 api-token"的推理是**错的**
//
// 它假设 cube 拒绝无 token 调用，但 `cubeclient.WithBearer` 的注释写着
// 「空字符串表示"无 token"(**e.g. 内部定时任务**)」——
// 内部定时任务调 cube 本来就不带凭据，那条路径是设计内支持的。
//
// ⚠️ 保留一条真实边界说明（不要自我安慰）：开窗后本端点不要求 token，
//
//	**同网段（172.12.1.0/24）内任何主机**都能触发一次采集
//	（app 绑 0.0.0.0:8110，`--app-channel-address` 约束的是 sidecar 出站、
//	**不约束 app 绑在哪** —— 同 pkg/cmdbootstrap 对 /events/* 的说明）。
//	公网侧由 nginx 挡住：
//	    location = /api/v1/sales-agg/sales/daily/collect { return 404; }
//	（回归锁见 deployer/tests/config/nginx_internal_test.go）。
//	最坏后果是浪费一次 cube 查询并覆盖同一个 (branch, oper_date) 的行 ——
//	采集本身幂等，不会写坏数据。
func registerDaprRoutes(r *gin.Engine) {
	handler.RegisterPurgeRoutes(r, appDB)
	// 每日销售事实落库。依赖 appDaily + appCube + appBearer（OnStart 已建）。
	//
	// ⚠️ appBearer 可能为 nil（主密钥没配）：那样每次采集都会明确返回
	//	"采集凭据：bearer 解析器未装配"，而不是无声地不带 token 打 cube。
	handler.NewDailyCollectHandler(collect.NewCollector(collect.NewDaprSource(appCube), appDaily), appBearer).
		RegisterRoutes(r)
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
