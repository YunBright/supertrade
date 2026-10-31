// Package main 是 fresh-meat dapr app 入口(鲜肉管理服务,REQUIREMENTS §4 / DESIGN §6)。
//
// 启动:`dapr run --app-id fresh-meat --app-port 8080 -- go run ./cmd/fresh-meat`
//
// 业务域:早盘整猪录入 + 白天单品补录 + POS 销售事件本地聚合 + 日终按部位盘点 +
// 报损 + 门店 cut 部位 ↔ cube product 映射维护。
//
// 数据存储:**只支持 PostgreSQL**。POSTGRES_DSN 必填,缺失直接启动失败。
// 不提供 SQLite fallback;单元测试用 *_test.go 内部 SQLite(与生产隔离)。
//
// 集成:dapr go-sdk NewClient + DaprConversationPredictFn(同步调 Conversation API);
// cube 客户端(走 cube-router,见 internal/cubehttp)校验 supplier_id。
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
	freshmeat "github.com/YunBright/supertrade/internal/fresh-meat"
	"github.com/YunBright/supertrade/internal/fresh-meat/handler"
	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/YunBright/supertrade/pkg/middleware"
	dapr "github.com/dapr/go-sdk/client"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID: "fresh-meat",
		Port:  cmdbootstrap.AppPort(":8108"),
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
	appArchive *service.ArchiveService
)

func initApp() error {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		return fmt.Errorf("fresh-meat: POSTGRES_DSN 必填(只支持 PostgreSQL,不提供 SQLite fallback)")
	}

	var err error
	appDB, err = freshmeat.OpenPostgres(dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	// ⚠️ **先**跑 pre-migrate 修正:把"形状变了"的旧索引删掉
	// (pig_cuts.idx_pig_cut 曾经是 UNIQUE(cut_type) 单列 ⇒ 每个部位只能补录一行),
	// 让下面的 AutoMigrate 能按正确的 UNIQUE(pig_id, cut_type) 重建。
	if err := freshmeat.ApplyPreMigrateFixups(appDB); err != nil {
		return fmt.Errorf("apply pre-migrate fixups: %w", err)
	}
	if err := appDB.AutoMigrate(
		&model.WholePig{},
		&model.PigCut{},
		&model.PorkCutsStocktake{},
		&model.LineSalesByPig{},
		&model.BranchCutMapping{},
		&model.WasteLog{},
		// 2026-10-23:SKU 级移动加权成本 + 两位置库存(猪肉台 / 冷库)。
		&model.SkuStockPosition{},
		&model.SkuCostState{},
		&model.SkuCostDaily{},
		&model.PromoSku{},
		// 2026-10-23:大宗台账(板油等) + 分割重量统计基线。
		&model.StockBag{},
	&model.BulkProduct{},
		&model.BulkProduction{},
		&model.BulkSettlement{},
		&model.BulkSettlementItem{},
		&model.BulkSettlementAllocation{},
		&model.PigForecastBaseline{},
		// 2026-10-24:营业日状态机 + 搬台流水 + 按营业日的结余快照 + 期初结转 + 审计结转。
		&model.BusinessDay{},
		&model.CounterLoadRecord{},
		&model.CutStockDaily{},
		&model.SkuStockOpening{},
		&model.BizAudit{},
		// 2026-10-24:预期毛利(预售)结转 + 预售毛利按猪分摊。
		&model.CutPresale{},
		&model.PigPresaleMargin{},
		&model.CutPresaleEdit{},
		// 2026-10-27:成本链改**部位级** ⇒ 部位级成本日快照 + 部位级成本状态。
		//
		// ⚠️ **必须在这里**:AutoMigrate 是本服务自己建表(生产上 fresh-meat 与其它服务
		//   共用同一个 PG)⇒ 漏登记 ⇒ 部署后第一次写快照就报
		//   "relation cut_cost_daily does not exist" ⇒ **上线即炸**。
		//
		// ⚠️ 旧的 sku_cost_daily / sku_cost_states **停写**(历史保留只读)
		//   ⇒ 上面两行保留在列表里(表还得在,历史数据还要能读)。
		&model.CutCostDaily{},
		&model.CutCostState{},
		// 2026-10-30:分割基线的**估计样本**(A 方案)+ 跨门店选用的预留表。
		//
		// ⚠️ `pig_cut_estimates` 是**新表** ⇒ 生产上第一次部署时由 AutoMigrate 建
		//   ⇒ 漏登记 ⇒ 盘点提交时写估计样本会报 "relation pig_cut_estimates does not exist"。
		&model.PigCutEstimate{},
		&model.PigForecastBaselineBorrow{},
		// 2026-10-31:分割基线的**单价**通道(owner:「分割基线加入单价」)。
		//
		// ⚠️ 三张都是**新表** ⇒ 生产上第一次部署时由 AutoMigrate 建
		// ⇒ 漏登记 ⇒ 第一次写单价样本就报
		//   "relation pig_cut_price_samples does not exist" ⇒ **上线即炸**。
		&model.PigCutPriceSample{},
		&model.PigCutPriceBaseline{},
		&model.CutUnitPriceCache{},
	); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}
	// AutoMigrate 只建索引不删索引 ⇒ 旧的 UNIQUE(branch_id, cut_type) 残留
	// 会挡住"同一部位挂第二个 SKU"(真源库两家店都是常态)。显式删掉。
	// 顺序:必须在 AutoMigrate **之后** —— 先让新索引建好(数据违反新约束时大声失败),
	// 再删旧索引(不留"旧约束还在"的窗口)。见 internal/fresh-meat/migrate.go。
	if err := freshmeat.ApplySchemaFixups(appDB); err != nil {
		return fmt.Errorf("apply schema fixups: %w", err)
	}

	appSvc = service.New(appDB)

	// 营业日界时区:env FRESHMEAT_BIZ_TZ 覆盖(默认 Asia/Shanghai UTC+8)。
	// 4:30 北京时间进场的猪需落在"今日"窗口而非 UTC 前一日。
	if tzName := os.Getenv("FRESHMEAT_BIZ_TZ"); tzName != "" {
		if loc, err := time.LoadLocation(tzName); err == nil {
			appSvc.SetBizTZ(loc)
			slog.Info("fresh-meat: bizTZ from env", "name", tzName)
		} else {
			slog.Warn("fresh-meat: FRESHMEAT_BIZ_TZ load failed, fallback to default",
				"tz", tzName, "err", err)
		}
	}

	// 注入 Dapr pub/sub publisher (fail-fast)。
	// fresh-meat 必须有 publisher 才能消费 sale.completed + 发 pork.cuts.stocktaken /
	// waste.log.recorded;sidecar 不可达 → 启动失败,提醒开发者 dapr run 没起。
	pub, err := service.NewDaprPublisherFromEnv()
	if err != nil {
		return fmt.Errorf("dapr publisher: %w (确认 dapr run 已起)", err)
	}
	appSvc.SetPublisher(pub)
	slog.Info("dapr publisher enabled")

	// ---- 归档导出/导入的「这个门店配了哪个数据源」解析器 ----
	//
	// ⚠️ **只用它解析数据源,不拿它查业务数据**:
	//   整猪 / 部位 / 日终盘点 / 按猪聚合的销售 / 报损
	//   是**本域自己产生的**,cube 里没有这些 model。
	//
	// ⚠️ 用 daprclient.New() 而不是 dapr.NewClient():
	//   go-sdk 的 api-token 拦截器会**整块替换** outgoing metadata,
	//   把本客户端转发给 cube 的 Authorization 头抹掉
	//   (2026-10-20 全网加 DAPR_API_TOKEN 之后暴露的)。
	//   详见 authkit/daprclient 的包注释。
	archiveDaprCli, err := daprclient.New()
	if err != nil {
		return fmt.Errorf("创建 dapr 客户端失败: %w (确认 dapr run 已起)", err)
	}
	cubeCli := cubeclient.NewDaprCubeClient(
		archiveDaprCli, cubeclient.DefaultCubeAppID(), cubeclient.DefaultCubeQueryPath())
	appArchive = service.NewArchiveService(appDB)
	// ⚠️ 解析器带 60s 缓存 + stale-on-error:每次导出都打一次 dapr 往返不划算,
	//   而一次网络抖动不该让整个门店的导出挂掉。
	appArchive.SetSourceResolver(cubesource.NewCachedResolver(cubeCli, cubesource.DefaultTTL, slog.Default()))
	slog.Info("fresh-meat: 归档导出/导入已启用", "app_id", cubeclient.DefaultCubeAppID())

	// 注入 cube 客户端(走 cube-router)。
	cube, err := cubehttp.NewClient()
	if err != nil {
		return fmt.Errorf("cube client: %w", err)
	}
	appSvc.SetCubeClient(cube)
	slog.Info("cube client enabled")

	// ---- 有源门店的**销售读取**(2026-10-26 门店反馈)----
	//
	// ⚠️ 为什么必须装配:
	//   有源门店(店 00 / 店 01)的销售在**它自己的 cube 源**里,
	//   而本域的 line_sales_by_pigs 只收**本地 POS 的 sale.paid 事件**
	//   ⇒ 对有源门店它永远是空的 ⇒ 毛利看板恒 0 销售。
	//
	// ⚠️ 用同一个 cubeCli:它是 *cubeclient.DaprCubeClient,
	//   本身就实现了 cuberead.Reader(LoadCubeQueryPage)
	//   ⇒ 不需要再包一层适配器,也不需要第二次 dapr 往返。
	appSvc.SetCubeReader(cubeCli)
	// ⚠️ 解析器带 60s 缓存 + stale-on-error:毛利看板每次都要判"这个店配了源吗",
	//   不缓存的话每次请求都多一次 dapr 往返(而它是 10s 超时的链路)。
	appSvc.SetSourceResolver(cubesource.NewCachedResolver(cubeCli, cubesource.DefaultTTL, slog.Default()))
	slog.Info("fresh-meat: 有源门店销售读取已启用",
		"app_id", cubeclient.DefaultCubeAppID())

	// 注入 Dapr Conversation API predict fn(同步调 LLM)。
	daprCli, err := dapr.NewClient()
	if err != nil {
		return fmt.Errorf("dapr client: %w (确认 dapr run 已起)", err)
	}
	convName := os.Getenv("FRESHMEAT_CONVERSATION")
	if convName == "" {
		convName = "conversation"
	}
	llmTimeout := 30 * time.Second
	if env := os.Getenv("FRESHMEAT_LLM_TIMEOUT"); env != "" {
		if d, err := time.ParseDuration(env); err == nil && d > 0 {
			llmTimeout = d
		}
	}
	appSvc.SetPredictFn(service.DaprConversationPredictFn(daprCli, convName, llmTimeout))
	slog.Info("dapr conversation client enabled", "component", convName, "timeout", llmTimeout)

	// 注入 userinfo 客户端(走 dapr app-id `userd`,调 /internal/users/{id}/permissions)。
	//
	// 2026-10-12:本服务从 `auth-internal` 切到 `userd`。auth 侧把独立的
	// app-id=auth-internal 进程合并进了 userd(`--mode=internal` 已废弃并映射为
	// userd),/internal/* 现在挂在 userd engine 上,共用同一个 app、同一 sidecar、
	// 同一个应用层验签器(authn)—— 鉴权只有一套实现,不存在"哪条链路漏了"。
	//
	// 调用路径本身没变:userinfo 一律打 `internal/users/*`,所以只换 app-id。
	//
	// SDK 行为:userinfo.New 内部调 dapr.NewClient() 自动从 DAPR_GRPC_PORT 拿
	// 本地 sidecar gRPC 客户端,无需任何 endpoint 配置。未注入时 svc.HasEffectiveScope
	// 返 ErrUserInfoUnavailable(503 userd_unavailable) — 历史行为。
	ui, err := userinfo.New("userd")
	if err != nil {
		return fmt.Errorf("userinfo client: %w (确认 dapr run 已起)", err)
	}
	appSvc.SetUserInfo(ui)

	// Warmup userd 跨主机冷握手(Consul DNS + mTLS + HTTP/2 SETTINGS +
	// userd 进程 DB pool),实测首次 7~12s 撞 userinfo.Client.timeout 默认
	// 10s。提前烧掉,业务请求进来时已 warm,首个 contract test 不再 503。
	//
	// 不阻断启动:失败仅 warn,业务首次走 cold path(已知行为)。
	warmupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := ui.Warmup(warmupCtx); err != nil {
		slog.Warn("userinfo warmup 失败 (userd 链路未预热),业务首次调用走 cold path",
			"err", err)
	} else {
		slog.Info("userinfo warmup ok (userd 链路预热完成)")
	}

	slog.Info("fresh-meat app initialized",
		"db_driver", "postgres",
		"schema", "fresh_meat (6 tables)",
		"biz_tz", appSvc.BizTZ().String(),
	)
	return nil
}

func registerRoutes(r *gin.Engine) {
	if appSvc == nil {
		panic("fresh-meat: appSvc 未初始化,可能是 OnStart 失败")
	}
	// 全局挂 X-Branch-ID 解析中间件(handler 用 SingleBranchFromCtx 读)。
	r.Use(middleware.XBranchID())
	// bearer 透传:与 stocktake cmd 模式一致。
	r.Use(forwardBearerToOutgoing())
	h := handler.New(appSvc)
	h.RegisterRoutes(r)
	// 归档导出/导入(独立的契约 + 独立的守卫 scope)。
	//
	// ⚠️ 守门复用 service 层的 effective scopes(带 60s 缓存),
	//   与本域既有的 freshmeat:view / freshmeat:write 同一套口径。
	handler.NewArchiveHandler(appArchive, rbac.ScopeResolver(
		func(c *gin.Context, userID, branchID, scope string) (bool, error) {
			return appSvc.HasEffectiveScope(c.Request.Context(), userID, branchID, scope)
		})).RegisterRoutes(r)
}

// registerDaprRoutes 在 auth 中间件**之前**注册 Dapr 订阅端点。
//
//	GET  /dapr/subscribe  → 订阅清单
//	POST /events/<topic>  → 事件分发(本服务订阅 auth.user.access_changed + sale.completed)
//
// 必须早于 claims.GinMiddleware + rbac.RequireAudience:sidecar 调这两个端点
// 不带 Authorization 头,带上鉴权中间件就会 401(sidecar 于是永远拿不到订阅)。
// 2026-10-07 生产实测本服务 /dapr/subscribe 一直返回 401,即由此而来。
// 详见 pkg/cmdbootstrap.Options.RegisterDapr 的注释。
func registerDaprRoutes(r *gin.Engine) {
	h := handler.New(appSvc)
	h.RegisterSubscribeRoutes(r, slog.Default())
	// 契约测试 sweep 的兜底清理端点(2026-10-23 新增)。
	//
	// ⚠️ 必须早于 claims.GinMiddleware:它要免鉴权(调用方走 dapr 内网 invoke)。
	// 公网可达性由 nginx 挡死:`location ^~ /api/v1/fresh-meat/internal/ { return 404; }`
	handler.RegisterPurgeRoutes(r, appDB)
}

// forwardBearerToOutgoing 把 caller 的 Authorization header 注入 ctx,供下游
// dapr 调用 (cubeclient → cube-gateway,userinfo → userd) 自动转发。
//
// 用 cubeclient.WithBearer + userinfo.WithBearer 双写,两者各自的 dapr 调用路径里
// 会读 ctx 拼到 outgoing gRPC metadata → sidecar 转 outgoing HTTP Authorization
// 给目标 app。auth-internal 端的 middleware.http.bearer (auth-validator-internal,
// aud=internal) 必须有 Authorization 头,无 token 会 401 Unauthenticated。
//
// 空 header 表示无 token(内部 job 路径),下游会以 401 拒绝,符合预期。
//
// 参考:cmd/stocktake/main.go::forwardBearerToOutgoing、cmd/catalog/main.go 同名。
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
