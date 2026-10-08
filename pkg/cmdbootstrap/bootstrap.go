// Package cmdbootstrap 提供 15 个 dapr app 共用的启动骨架。
//
// 用法:
//
//	cmdbootstrap.Run(cmdbootstrap.Options{
//	    AppID: "pos",
//	    Register: func(r *gin.Engine) {
//	        r.GET("/sales", handler.ListSales)
//	    },
//	})
//
// 行为:
//   - 监听 :8080(可改)
//   - 接入 authkit.claims.GinMiddleware(解析 JWT claims 到 ctx,Dapr sidecar 已验签)
//   - 接入 rbac.RequireAudience(AppID)(拒 aud 不含本服务的 token)
//   - 注册 /healthz(公开,不走 auth 中间件)
//   - 优雅退出(SIGINT/SIGTERM,5s 超时)
//
// 不集成 dapr SDK:pub/sub 订阅与 service invocation 由 dapr sidecar 代理。
// 后续真要落地 pub/sub 订阅,加个 /dapr/subscribe 端点 + 业务 handler 即可。
package cmdbootstrap

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/YunBright/authkit/claims"
	"github.com/YunBright/authkit/rbac"
	"github.com/gin-gonic/gin"
)

// Options 描述一个 dapr app 的启动配置。
type Options struct {
	AppID    string                 // dapr app-id,必填
	Port     string                 // 监听地址,默认 ":8080"
	Audience string                 // rbac.RequireAudience target,默认 = AppID
	Logger   *slog.Logger           // 自定义 logger;nil 用 JSON stdout
	Register func(r *gin.Engine)    // 业务路由注册;nil 仅暴露 /healthz

	// RegisterDapr 注册 **Dapr 内部路由**,在 auth 中间件**之前**执行。
	//
	// 存在的唯一理由:sidecar 调 /dapr/subscribe 时不带 Authorization 头
	// (它是 sidecar 自己,不是终端用户),而 RequireAudience 对缺 claims 的请求
	// 一律 401。2026-10-07 实测:fresh-meat 与 stocktake 的
	// /dapr/subscribe 都返回 401,整条 pubsub 链路静默失效。
	// 详见 daprRegisterWindow 的说明。
	//
	// 只有真正提供 Dapr 订阅的服务才需要填它(目前是 stocktake /
	// notification-gateway / fresh-meat);其余服务留 nil 即完全不受影响,
	// 不存在"默认放开 /dapr/*"的隐式行为。
	RegisterDapr func(r *gin.Engine)

	OnStart  func() error           // server.ListenAndServe 之后回调(注册外部资源);nil 跳过
	OnStop   func(ctx context.Context) error // server.Shutdown 之前回调(释放资源);nil 跳过
}

// NewRouter 按固定顺序装配 gin engine 并注册全部路由。
//
// **注册顺序本身就是一个安全不变量**,改这里之前先读下面三段的说明。
//
// 1) /healthz 先于 auth —— 它是公开健康检查,注册在 auth 中间件之前所以不受保护
//    (否则负载均衡/容器探针拿不到状态)。
//
// 2) Dapr 内部路由(Options.RegisterDapr)先于 auth ——
//    Gin 的 `r.Use()` 只对**之后**注册的路由生效,所以只要把
//    /dapr/subscribe 与 /events/<topic> 注册在 auth 中间件之前,
//    它们就天然不带 claims / RequireAudience。
//
//    不能反过来用"在中间件里判断路径就 skip"的做法:Gin 的 Use 是线性链,
//    中间件里无法"跳过链上后续的中间件但仍让最后的 handler 执行"
//    (c.Abort() 会把 handler 一起拦掉)。注册顺序是 Gin 里唯一干净的机制。
//
//    为什么必须开这个窗口(2026-10-07 生产实测):
//
//    	curl http://127.0.0.1:8106/dapr/subscribe → 401 {"code":"unauthenticated"}
//    	curl http://127.0.0.1:8108/dapr/subscribe → 401("正常"的 fresh-meat 同样中招)
//
//    sidecar 拉订阅清单时不带 token,claims.GinMiddleware() 对缺头请求直接放行、
//    不注入 claims,RequireAudience() 随即 401。sidecar 于是永远拿不到订阅,
//    事件一条也推不出去 —— 而 /healthz 仍 200、业务仍正常,只有"通知不响"
//    一个症状,极易被误判成"通知服务没实现"。这就是本 bug 藏了这么久的原因。
//
//    ⚠️ 安全边界(写清楚真实暴露面,不要自我安慰):
//    开窗后 /dapr/subscribe 与 /events/* 不要求 token,**任何能 TCP 连到 app 端口
//    的人都能 POST 伪造事件**。
//    注意 `--app-channel-address` 约束的是 sidecar **出站**调用,
//    **不约束 app 绑在哪个地址** —— 本仓各服务用 `Addr: ":8106"` 这类写法,
//    实测 ss 输出是 `LISTEN *:8106`,即 0.0.0.0。
//    所以"nginx 不暴露 app 端口"并不等于安全:同网段(172.12.1.0/24)内任何主机
//    都能直连 8106 伪造盘点事件,并被 gateway FanoutAll 推给所有在线客户端。
//    要真正收口,应把 app 绑到 127.0.0.1 / unix socket 让 sidecar 走本地,
//    或给 /events/* 加 dapr-api-token / mTLS。
//    当前部署依赖"内网可信"这一前提 —— 这是已知且被接受的风险,不是已解决。
//
// 3) 业务路由(Options.Register)最后注册 —— 必然带 auth。
func NewRouter(opts Options) *gin.Engine {
	if opts.AppID == "" {
		opts.AppID = "unknown"
	}
	if opts.Audience == "" {
		opts.Audience = opts.AppID
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	// (1) 公开健康检查
	r.GET("/healthz", healthzHandler(opts.AppID))

	// (2) Dapr 内部路由 —— 必须在 auth 之前
	if opts.RegisterDapr != nil {
		opts.RegisterDapr(r)
	}

	// auth 中间件:解析 claims + 强制 aud 包含本服务
	r.Use(claims.GinMiddleware())
	r.Use(rbac.RequireAudience(opts.Audience))

	// (3) 业务路由不在这里注册 —— 见下方 MountBusiness 的说明。

	return r
}

// MountBusiness 注册业务路由。
//
// 必须在 auth 中间件**之后**(NewRouter 已装好)、OnStart **之后**调用。
//
// 拆出来单独一个函数有两个原因:
//  1. 保住 Run 的启动时序 —— handler 依赖的 DB / cube client / publisher 由
//     OnStart 建立,业务路由必须等它们就绪之后再挂;
//  2. 让 NewRouter 可以被单测直接调用 —— 装配顺序是可测的不变量。
//
// ⚠️ Run 与 NewRouter 只能各调用一次 opts.Register。曾经两处都调,结果是
// gin panic "handlers are already registered for path '/ws'",
// 而这个 panic 只在真机启动时出现、任何单测都覆盖不到(见 Run 里的注释)。
func MountBusiness(r *gin.Engine, opts Options) {
	if opts.Register != nil {
		opts.Register(r)
	}
}

// Run 启动一个 dapr app 进程,阻塞至收到退出信号。
func Run(opts Options) {
	if err := opts.Validate(); err != nil {
		panic(err)
	}
	if opts.Port == "" {
		opts.Port = ":8080"
	}
	if opts.Audience == "" {
		opts.Audience = opts.AppID
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))
	}
	slog.SetDefault(logger)

	// ── OnStart 必须最先跑 ──────────────────────────────────────────────
	//
	// 它建立 appSvc / appDB / appCube / publisher 这些**包级变量**,
	// 而 NewRouter 里的 Options.RegisterDapr 与 MountBusiness 里的
	// Options.Register 都要用它们(handler.New(appSvc) 是在注册时取值的)。
	//
	// 2026-10-07 首版把 NewRouter 放在 OnStart 之前,后果是
	// stocktake 的 /events/* 永久绑在 svc == nil 的 Handler 上:
	// 收到 auth.user.access_changed(带 branch_id)时
	// InvalidateScopeCache 走到 s.scopeMu.Lock() → nil 解引用 → gin Recovery 500,
	// 而 GET /dapr/subscribe 仍返 200 —— 订阅建起来了但每条事件都失败,
	// Dapr 无限重试。**"看起来部署成功、实际一条事件都推不出去"。**
	//
	// 顺带好处:OnStart 失败时进程在监听端口**之前**就退出(fail fast),
	// 不会出现"端口已开但后端还没连上"的窗口。
	if opts.OnStart != nil {
		if err := opts.OnStart(); err != nil {
			logger.Error("OnStart failed", "err", err, "app", opts.AppID)
			os.Exit(1)
		}
	}

	// 路由装配抽成 NewRouter,让"注册顺序"这个关键不变量可以被单测覆盖 ——
	// 2026-10-07 之前 /dapr/subscribe 返回 401 藏了很久,正是因为 Run()
	// 阻塞在 ListenAndServe 上,没有任何用例能观察到中间件的装配顺序。
	r := NewRouter(opts)

	// 业务路由同样必须在开始监听**之前**全部挂好 —— 否则从 NewRouter 返回到
	// MountBusiness 之间有一个窗口,进来的请求会拿到 404。
	//
	// ⚠️ 业务路由全进程**只在这里注册一次**。曾经 NewRouter 与 Run 各调一次,
	// 真机启动直接 gin panic("handlers are already registered for path '/ws'"),
	// 而单测走的是另一条装配路径,完全测不到这个重复注册。
	MountBusiness(r, opts)

	srv := &http.Server{
		Addr:              opts.Port,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("starting dapr app", "app", opts.AppID, "addr", opts.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received", "app", opts.AppID)
	case err := <-errCh:
		logger.Error("server failed, exit", "err", err, "app", opts.AppID)
		os.Exit(1)
	}

	// 释放回调(关 DB / Redis 等)
	if opts.OnStop != nil {
		stopCtx, sCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := opts.OnStop(stopCtx); err != nil {
			logger.Error("OnStop failed", "err", err, "app", opts.AppID)
		}
		sCancel()
	}

	shutdownCtx, sCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer sCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "err", err, "app", opts.AppID)
	}
	logger.Info("stopped", "app", opts.AppID)
}

func healthzHandler(appID string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status": "ok",
			"app":    appID,
			"at":     time.Now().UTC().Format(time.RFC3339),
		})
	}
}

// Validate 检查 Options 必填字段。
//
// 返回 error 而非 panic,便于单元测试;Run() 在收到 error 时 panic 退出 main。
func (o Options) Validate() error {
	if o.AppID == "" {
		return errors.New("cmdbootstrap: AppID 必填")
	}
	return nil
}