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
// 阶段3 集成:dapr go-sdk NewClient + DaprConversationPredictFn(同步调 Conversation API);
// cube-gateway 客户端(走 cube-router,见 internal/cubehttp)校验 supplier_id。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/YunBright/authkit/userinfo"
	freshmeat "github.com/YunBright/supertrade/internal/fresh-meat"
	"github.com/YunBright/supertrade/internal/fresh-meat/handler"
	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubehttp"
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
		Register: registerRoutes,
	})
}

// appDeps 持有运行时单例,供 handler / service 共享。
//
// 用 package var 是因为 cmdbootstrap.Options.OnStart / Register 回调签名不返回外部状态;
// 后续可改成显式 dependency injection。
var (
	appDB  *gorm.DB
	appSvc *service.Service
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
	if err := appDB.AutoMigrate(
		&model.WholePig{},
		&model.PigCut{},
		&model.PorkCutsStocktake{},
		&model.LineSalesByPig{},
		&model.BranchCutMapping{},
		&model.WasteLog{},
	); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
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

	// 阶段3 注入 cube 客户端(走 cube-router)。
	cube, err := cubehttp.NewClientFromEnv()
	if err != nil {
		return fmt.Errorf("cube client: %w", err)
	}
	appSvc.SetCubeClient(cube)
	slog.Info("cube client enabled")

	// 阶段3 注入 Dapr Conversation API predict fn(同步调 LLM)。
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

	// 注入 userinfo 客户端(走 dapr app-id `auth-internal`,调 /internal/users/{id}/permissions)。
	//
	// fresh-meat 是 supertrade 仓内**首个**切到 `auth-internal` 的服务:
	// 不再走 userd(:8082),改打 auth-internal(:8083) — 该端口只暴露
	// `/internal/users/*` + `/internal/sms-codes/*` + `/healthz`,由 dapr mTLS +
	// auth-validator-internal(aud=internal)双重保护(参考 auth 文档 §1.1 + §1.2)。
	//
	// SDK 行为:userinfo.New 内部调 dapr.NewClient() 自动从 DAPR_GRPC_PORT 拿
	// 本地 sidecar gRPC 客户端,无需任何 endpoint 配置。未注入时 svc.HasEffectiveScope
	// 返 ErrUserInfoUnavailable(503 userd_unavailable) — 历史行为。
	//
	// ⚠️ 切到 `auth-internal` 后,caller JWT 必须含 `aud: auth-internal`,否则
	// auth-internal 端 auth-validator-internal bearer 中间件会拒(401)。
	// dapr/config.yaml policy(本仓不启用)+ auth 仓库 components/bearer-auth.yaml
	// 的 auth-validator-internal component 需同步就绪(详见 auth 仓库 SOP §1.2)。
	ui, err := userinfo.New("auth-internal")
	if err != nil {
		return fmt.Errorf("userinfo client: %w (确认 dapr run 已起)", err)
	}
	appSvc.SetUserInfo(ui)

	// Warmup auth-internal 跨主机冷握手(Consul DNS + mTLS + HTTP/2 SETTINGS +
	// auth-internal 进程 DB pool),实测首次 7~12s 撞 userinfo.Client.timeout 默认
	// 10s。提前烧掉,业务请求进来时已 warm,首个 contract test 不再 503。
	//
	// 不阻断启动:失败仅 warn,业务首次走 cold path(已知行为)。
	warmupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := ui.Warmup(warmupCtx); err != nil {
		slog.Warn("userinfo warmup 失败 (auth-internal 链路未预热),业务首次调用走 cold path",
			"err", err)
	} else {
		slog.Info("userinfo warmup ok (auth-internal 链路预热完成)")
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
	// bearer 透传:阶段2+ 接入 userd / cube 后,持 / userinfo.WithBearer 双写,与 stocktake cmd 模式一致。
	r.Use(forwardBearerToOutgoing())
	h := handler.New(appSvc)
	h.RegisterRoutes(r)
	// Dapr pub/sub 订阅(挂在 engine,不走业务路由组):
	//   GET  /dapr/subscribe  → 订阅清单
	//   POST /events/<topic>  → 事件分发(本服务订阅 auth.user.access_changed + sale.completed)
	h.RegisterSubscribeRoutes(r, slog.Default())
}

// forwardBearerToOutgoing 把 caller 的 Authorization header 注入 ctx,供下游
// dapr 调用 (cubeclient → cube-gateway,userinfo → auth-internal) 自动转发。
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
		c.Next()
	}
}

// 静默引用 context(为后续按需扩展预留)
var _ = context.Background