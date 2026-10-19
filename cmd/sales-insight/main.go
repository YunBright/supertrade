// Package main 是 sales-insight dapr app 入口（AI 销售日报/周报/月报）。
//
// 启动：`dapr run --app-id supertrade-sales-insight --app-port 8112 -- go run ./cmd/sales-insight`
//
// # 职责边界（与 salesagg / wecom-bot 的分工）
//
//	本服务：取数 → 组装事实包 → 调 LLM 出结论。**不发任何消息。**
//	sales-agg：按维度聚合成报表（既有服务，本服务不重复它的职责）
//	wechat-bot：企微出口 + 调度 + 报告历史存储
//
// 之所以不把 LLM 放进 sales-agg：它的包注释明确写着
// "不发企业微信消息、不存业务数据"，加 LLM 会让那个边界失守。
//
// # 生产端口
//
// 8112。**不要**改成 8109/8110/8111 —— 8109 已被 settlement-auditor 占用,
// 8110 是 sales-agg,8111 是 pricing(见各自 cmd/*/main.go 的端口注释)。
// 两个 unit 抢同一端口的结果不是"谁后起谁失败"那么温和:两个 sidecar 与 app
// 互相抢 listen,业务表现为"时好时坏",极难归因。
// deployer/tests/contract 的 TestPricingPortIsNotTaken 就是为这类冲突兜底的。
// dapr run 会把 --app-port 注入成 APP_PORT,所以线上以 unit 为准。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	dapr "github.com/dapr/go-sdk/client"
	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/salesdaily"
	"github.com/YunBright/supertrade/internal/salesinsight/build"
	"github.com/YunBright/supertrade/internal/salesinsight/handler"
	"github.com/YunBright/supertrade/internal/salesinsight/ignore"
	"github.com/YunBright/supertrade/internal/salesinsight/insight"
	"github.com/YunBright/supertrade/internal/salesinsight/weather"
	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// DefaultPort 是本地裸跑（无 sidecar）时的回退端口。
const DefaultPort = ":8112"

// appSvc 是 handler 与 service 共享的单例。
var appSvc *handler.ServiceImpl

// appH 是 HTTP handler 单例（与 appSvc 分开，因为要额外注入名单存储）。
var appH *handler.Handler

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID: "sales-insight",
		Port:  cmdbootstrap.AppPort(DefaultPort),
		OnStart: func() error {
			return initApp()
		},
		Register: registerRoutes,
	})
}

func initApp() error {
	daprCli, err := dapr.NewClient()
	if err != nil {
		return fmt.Errorf("dapr.NewClient: %w (确认 dapr run 已起)", err)
	}

	cube := cubeclient.NewDaprCubeClient(
		daprCli,
		cubeclient.DefaultCubeAppID(),
		cubeclient.DefaultCubeQueryPath(),
	)

	// ---- 供应商忽略名单（人工名单，排除自用渠道的唯一依据）----
	//
	// ⚠️ 路径默认在缓存目录下，按门店拆成独立文件。
	//
	//	必须落盘：名单是"配一次用很久"的配置，
	//	放内存的话重启后名单空掉 → 自用供应商全部涌回榜单，
	//	而报告里没有任何异常信号（见 ignore 包的 package 注释）。
	ignores := newIgnoreStore()
	src := build.NewDaprSource(cube)
	builder := build.NewBuilder(src, weatherProvider(daprCli)).
		WithIgnores(ignores)

	// ---- 已定稿销售事实（PG 只读）----
	//
	// ⚠️ 读侧**只**读 PG，不写、不 AutoMigrate、不回补：
	//
	//	写者是 sales-agg（唯一写者，见 internal/salesdaily 的包注释）。
	//	两边的模型是**同一份 Go 定义**，所以写侧加字段而读侧没跟上
	//	会在编译期就炸，而不是上线后读出零值。
	//
	// ⚠️ 装配失败**不阻止启动**：
	//
	//	DSN 缺失时本服务退化成"每次现查源库"，报告照出（只是数字会随
	//	源库补写而变）。那比"起不来"好 —— 而"PG 没接"这件事由
	//	health 端点显式暴露（pg_ready=false），不会被静默。
	//	读侧与写侧对"缺失"的处理在这里是**刻意不同**的：
	//	写侧不启动会让采集静默失败（数据永远攒不出来），
	//	读侧不启动会让日报发不出去。
	dailyStore, dailyErr := openDailyStore()
	if dailyErr != nil {
		slog.Warn("sales-insight: 未接入每日销售事实库，销售数字将现查源库"+
			"（同一营业日可能两次查到不同的数）", "reason", dailyErr)
	} else {
		builder = builder.WithRangeLoader(dailyStore)
	}

	// ---- 推理引擎 ----
	//
	// ⚠️ **没有 dapr client 也要能跑**：
	//   EnableAI 为 false 或 conversation 组件缺失时，
	//   引擎会走规则降级，报告照出（标注"规则模式"）。
	//   这比"因为 LLM 没配好就整份日报发不出去"好得多 ——
	//   日报的价值 60% 在数据（排行/环比/缺货）。
	enableAI := envBool("SALESINSIGHT_ENABLE_AI", true)
	var conv insight.ConverseFunc
	if enableAI {
		conv = insight.DaprConverseFn(daprCli, envStr("SALESINSIGHT_CONVERSATION", "conversation"))
		if conv == nil {
			slog.Warn("sales-insight: dapr client 不可用，AI 解读将走规则降级")
		}
	} else {
		slog.Info("sales-insight: AI 解读已显式关闭（SALESINSIGHT_ENABLE_AI=false）")
	}

	timeout := time.Duration(envInt("SALESINSIGHT_LLM_TIMEOUT_SEC", 45)) * time.Second
	engine := insight.NewEngine(enableAI && conv != nil, timeout, conv)

	appSvc = handler.NewServiceImpl(builder, engine)
	appH = handler.New(appSvc).WithIgnores(ignores).WithDict(src).
		WithDailyStore(dailyErr == nil)

	// ---- 营业时区（决定 daily 算哪天的「今日」）----
	//
	// ⚠️ 必须注入：calendar.ResolvePeriod 用 `now.Location()` 推算日期，
	//   time.Now 默认 UTC；早班 0:30 CST = 16:30 UTC（前一日），
	//   不注入会让 daily 落到「昨日」(现场 2026-10-19 22:50 推送 → 区间 2026-10-18)。
	//
	// 装配 + env 解析抽到 wireBizTZ，便于 cmd/sales-insight 的单测覆盖
	// (main.go 主路径不可达 —— dapr.NewClient 需要 sidecar)。
	if err := wireBizTZ(appSvc); err != nil {
		return err
	}

	slog.Info("sales-insight app initialized",
		"cube_app_id", cubeclient.DefaultCubeAppID(),
		"ai_enabled", enableAI,
		"prompt_version", insight.PromptVersion,
		"llm_timeout", timeout.String(),
		"store_city", os.Getenv("STORE_CITY"),
		"ignore_store_dir", ignores.Dir(),
		"daily_store_ready", dailyErr == nil,
		"biz_tz", appSvc.BizTZ().String(),
	)
	return nil
}

// wireBizTZ 把营业时区装配到 ServiceImpl。
//
// ⚠️ **必须注入**：calendar.ResolvePeriod 用 `now.Location()` 推算日期，
//   time.Now 默认 UTC；早班 0:30 CST = 16:30 UTC（前一日），
//   不注入会让 daily 落到「昨日」(现场 2026-10-19 22:50 推送 → 区间 2026-10-18)。
//
// env SALESINSIGHT_BIZ_TZ 可覆盖（默认 Asia/Shanghai UTC+8），
// 与 fresh-meat 的 FRESHMEAT_BIZ_TZ 同语义。
//
// 反证（详见 main_test.go）：
//   - 默认值改成 "UTC" → TestWireBizTZDefaultsToShanghai 红
//   - 删 env 读取          → TestWireBizTZRespectsEnv 红
//   - 删 WithBizTZ 调用     → TestWireBizTZInjectsShanghai 红
//   - LoadLocation 失败不报错 → TestWireBizTZInvalidEnvErrors 红
func wireBizTZ(svc *handler.ServiceImpl) error {
	name := strings.TrimSpace(envStr("SALESINSIGHT_BIZ_TZ", "Asia/Shanghai"))
	loc, err := time.LoadLocation(name)
	if err != nil {
		return fmt.Errorf("sales-insight: 加载营业时区 %q 失败: %w", name, err)
	}
	svc.WithBizTZ(loc)
	slog.Info("sales-insight: 营业时区已注入", "tz", name)
	return nil
}

// openDailyStore 打开每日销售事实库的**只读**通道。
//
// ⚠️ 这里返回 nil, nil 表示"没接"，而不是"接了但表不存在"：
//
//	两者在调用方那里需要不同的处置 —— 前者是一次性部署问题，
//	后者说明 sales-agg 那个进程没起来或 DSN 没配，
//	而排查方向完全不同。所以这里只把"打不开库"折叠成同一个错误，
//	表存在性交由实际查询去暴露（那条路径上有明确的 ErrRangeIncomplete / SQL 错）。
func openDailyStore() (*salesdaily.Store, error) {
	dsn := strings.TrimSpace(os.Getenv("POSTGRES_DSN"))
	if dsn == "" {
		return nil, fmt.Errorf("POSTGRES_DSN 未配置")
	}
	db, err := salesdaily.OpenDB(dsn)
	if err != nil {
		return nil, err
	}
	// ⚠️ 只 Ping 不 AutoMigrate：表归写侧管。
	//   读侧建表会把"写侧忘了跑"这件事永久掩盖掉 ——
	//   而那恰恰是最需要被发现的情况。
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	if err := sqlDB.Ping(); err != nil {
		return nil, fmt.Errorf("ping: %w", err)
	}
	return salesdaily.NewStore(db, nil), nil
}

// newIgnoreStore 构造供应商忽略名单存储。
//
// 目录可用 SUPPLIER_IGNORE_DIR 覆盖 —— 生产上建议指向
// /opt/YunBright/.../data 这类**会被备份**的目录，
// 否则重装系统时所有门店的名单都要重新配一遍。
func newIgnoreStore() *ignore.Store {
	dir := strings.TrimSpace(os.Getenv("SUPPLIER_IGNORE_DIR"))
	if dir == "" {
		if base, err := os.UserCacheDir(); err == nil {
			dir = filepath.Join(base, "supertrade-sales-insight", "supplier-ignore")
		}
	}
	return ignore.NewStore(dir)
}

func registerRoutes(r *gin.Engine) {
	if appH == nil {
		panic("sales-insight: appH 未初始化，可能是 OnStart 失败")
	}
	// 读 X-Branch-ID 供透传给 cube-router 做 per-branch 路由。
	//
	// ⚠️ salesagg 也用这个中间件，但两者各注册一份是有意为之：
	//   middleware 是 gin.HandlerFunc，按服务装配最简单，
	//   而共享一个实例会引入装配顺序依赖。
	r.Use(middleware.XBranchID())
	r.Use(forwardBearerToOutgoing())

	// ⚠️ 用 OnStart 里建好的 appH（已注入名单存储），不要在这里新建：
	//   新建出来的 handler 拿不到 ignores，名单端点会一律 503，
	//   而 generate 仍然正常 —— 一个"只有配置功能坏了"的服务
	//   比整体挂掉更难排查。
	appH.RegisterRoutes(r)
}

// forwardBearerToOutgoing 把 gin request 的 Authorization 注入 ctx。
//
// 与 stocktake / salesagg 的同名函数逐字一致（都是 cubeclient.WithBearer
// 的标准用法）：handler 收到的 Authorization → ctx → cubeclient 拼进
// outgoing gRPC metadata → sidecar 转成给 cube-gateway 的 HTTP 头。
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

// weatherProvider 构造天气源。
//
// # 降级链（三级，逐级更保守）
//
//	有 key + 位置有效 → 和风天气真实查询
//	有位置、缺 key    → 明确说"未配置 key"，报告照出
//	位置无效/未配置    → 明确说"位置未配置"，**不发起请求**
//
// 无论哪一级，失败都不会编造天气 —— 见 weather 包的注释。
//
// ⚠️ 位置无效时**不发起请求**是有意为之：
//
//	0/0 会命中几内亚湾，天气 API 正常返回"那里的天气"，
//	而报告里会写"晴，26℃"，没有任何异常信号。
//	门店位置配错是这个功能最可能的配置错误，必须让它显形。
func weatherProvider(daprCli dapr.Client) weather.Provider {
	loc := weather.Location{
		Name: strings.TrimSpace(os.Getenv("STORE_CITY")),
		Lat:  envFloat("STORE_LAT"),
		Lon:  envFloat("STORE_LON"),
	}
	if !loc.Valid() {
		return weather.NewUnavailableProvider(
			"门店位置未配置或超出中国境内范围（STORE_CITY/STORE_LAT/STORE_LON），" +
				"天气上下文不可用 —— 请检查这三个环境变量")
	}

	qw := weather.NewQWeatherProvider(os.Getenv("WEATHER_QWEATHER_KEY"))
	if qw == nil {
		return weather.NewUnavailableProvider(
			"未配置天气 API key（WEATHER_QWEATHER_KEY），天气上下文不可用")
	}
	// ⚠️ base URL 必须来自 env，不是硬编码：
	//   和风按订阅套餐分配 host，生产那个 key 在 devapi/api/geoapi
	//   上一律 403 invalid-host（不是 key 错，是这个 host 不在订阅里）。
	//   专属 host 形如 xxxxx.re.qweatherapi.com，写进代码就等于换账号即失效。
	//   空值会回落到 DefaultQWeatherBaseURL（同样不可用，但错误信息里
	//   能看出是没配 host 而不是配错了 key）。
	qw = qw.WithBaseURL(os.Getenv(weather.EnvQWeatherBaseURL))
	qw.Quota = newQuotaGuard(daprCli)
	return qw
}

// newQuotaGuard 装配每日调用配额守卫。
//
// # ⚠️ 主实现是 **dapr state（Redis 支撑）**，不是本地文件
//
//	和风按 **API key** 计费 —— 同一个 key 被 N 个服务用，就是**一份** 1500 的额度。
//	若每个服务各自计数，实际发出量可以是 N×1500，
//	而 402 的表现只是"天气突然全无"，没有任何告警指向"配额用光了"。
//
//	→ 所以计数必须跨服务共享：这正是本计数器存在的理由。
//
// # 降级链（三级，逐级更保守）
//
//	dapr state 可用 → 跨服务共享计数（正常）
//	dapr client 无 → 本地文件计数（⚠️ 每服务一份，降级**不安全**，但仍能防住单服务打爆）
//	两者都无      → 内存计数（同上，仅测试/裸跑）
//
// ⚠️ 后两级**不**满足"跨服务共享"的语义，所以必须把降级打进日志：
//
//	否则运维看到的是"配额保护在跑"，而它其实只护住了一个进程。
func newQuotaGuard(daprCli weather.StateClient) *weather.Quota {
	opts := weather.QuotaOptions{
		Limit: envInt(weather.EnvQWeatherDailyLimit, weather.DefaultQWeatherDailyLimit),
	}

	storeName := envStr("WEATHER_QUOTA_STATE_STORE", weather.DefaultStateStoreName)
	store := weather.NewDaprCounterStore(weather.DaprCounterStoreOptions{
		Client: daprCli,
		Store:  storeName,
	})
	// ⚠️ 必须**探一次**，不能只看 client 非 nil：
	//   "组件 yaml 没加载"只有真发一次请求才暴露，而那种形态不是报错，
	//   是**每天每份报告都取不到天气**，只在文案里留一句
	//   「天气查询失败」—— 看起来像天气坏了，真相是配额守卫把天气锁死了。
	probeCtx, cancelProbe := context.WithTimeout(context.Background(), 3*time.Second)
	probeErr := store.Probe(probeCtx)
	cancelProbe()
	if store != nil && probeErr == nil {
		opts.Store = store
		slog.Info("sales-insight: 天气配额使用 dapr state（跨服务共享）",
			"state_store", storeName)
		return weather.NewQuota(opts)
	}

	// ⚠️ 这一级降级让配额**只护住本进程**，必须显式告警。
	reason := "dapr client 不可用"
	if probeErr != nil {
		reason = "dapr state 组件不可用"
	}
	dir := strings.TrimSpace(os.Getenv("WEATHER_QUOTA_FILE"))
	if dir == "" {
		if base, err := os.UserCacheDir(); err == nil {
			dir = filepath.Join(base, "supertrade-sales-insight")
		}
	}
	opts.Store = weather.NewFileCounterStore(dir)
	slog.Warn("sales-insight: "+reason+"，天气配额降级为**本地文件计数**"+
		" —— 它只护住本进程，无法跨服务共享",
		"file_dir", dir, "reason", probeErr)
	return weather.NewQuota(opts)
}

// ---- env helpers ----

func envStr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// envBool 解析布尔环境变量。
//
// ⚠️ 只认 true/1/yes/on，其余一律为 false ——
//
//	"SALESINSIGHT_ENABLE_AI=True"（Go 风格大小写）会被判为 false，
//	于是 AI 静默关闭而报告照发（只是标注规则模式）。
//	那个症状很难被立刻察觉，所以这里显式接受多种写法。
func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "":
		return def
	case "1", "true", "yes", "on", "y":
		return true
	case "0", "false", "no", "off", "n":
		return false
	}
	return def
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func envFloat(key string) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0
	}
	return f
}
