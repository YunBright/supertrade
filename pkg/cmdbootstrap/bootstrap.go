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
	OnStart  func() error           // server.ListenAndServe 之后回调(注册外部资源);nil 跳过
	OnStop   func(ctx context.Context) error // server.Shutdown 之前回调(释放资源);nil 跳过
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

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	// 公开端点(必须在 auth 中间件之前注册,绕过鉴权)
	r.GET("/healthz", healthzHandler(opts.AppID))
	// auth 中间件:解析 claims + 强制 aud 包含本服务
	r.Use(claims.GinMiddleware())
	r.Use(rbac.RequireAudience(opts.Audience))

	if opts.Register != nil {
		opts.Register(r)
	}

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

	// 启动回调(注册外部资源)
	if opts.OnStart != nil {
		if err := opts.OnStart(); err != nil {
			logger.Error("OnStart failed", "err", err, "app", opts.AppID)
			os.Exit(1)
		}
	}

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