// Package main 是 inventory dapr app 入口(批次库存 / 保质期 / 出入库流水 / cube stock 转发)。
//
// 启动:`dapr run --app-id inventory --app-port 8102 -- go run ./cmd/inventory`
//
// 本期定位(REQUIREMENTS §7.5 / DESIGN §3):
//
//	inventory 是 cube stock 的**转发 / 聚合 / 短期缓存**出口。
//	**不维护库存表**,所有读走 cube-gateway /v1/load;写入(出入库)本期不实现,
//	留待 Phase 2 与 erp-connector / pos 联动。
//
// 端口分配见 cmd/catalog/main.go 注释。
//
// 配置:
//
//	CUBE_CLIENT_MODE = memory(默认)/ dapr(SDK 模式,经 dapr sidecar 调 cube-router / cube-gateway)
//	CUBE_APP_ID      = "cube-router"   // 默认 (走多源路由);改 "cube-gateway" 直连
//
// 2026-09 PR 5 重构:跨服务调用经 dapr/go-sdk;无需 DAPR_ENDPOINT。
package main

import (
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubehttp"
	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/gin-gonic/gin"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID: "inventory",
		Port:  cmdbootstrap.AppPort(":8105"),
		OnStart: func() error {
			cube, err := cubehttp.NewClientFromEnv()
			if err != nil {
				return err
			}
			appCube = cube
			return nil
		},
		Register: registerRoutes,
	})
}

var appCube cubeclient.Client

func registerRoutes(r *gin.Engine) {
	// 把 caller 的 Authorization header 注入 ctx,后续 cubeclient 内部自动 forward
	// 给下游 cube-router(cube-gateway)。cube-router 的 userinfo 调用会再次
	// WithBearer 转给 userd —— 跨 dapr app 链式透传 caller JWT 的标准做法。
	r.Use(forwardBearerToOutgoing())
	cubehttp.New(appCube).Register(r, cubehttp.RegisterOptions{
		Stock:         true,
		RequireBranch: true, // /stock/:product_id 走 claims.AccessibleBranches 守门(branch 从 X-Branch-ID header 取)
	})
}

// forwardBearerToOutgoing 把 gin request 的 Authorization header 注入 ctx。
//
// 此 cmd 不直连 userinfo,只走 cubeclient 转发给 cube-router / cube-gateway。
// cube 端是否要 Authorization 取决于具体实例(本机 cube-gateway 可能不挂
// bearer middleware,但 userd 必须有);保守起见一律 forward。
func forwardBearerToOutgoing() gin.HandlerFunc {
	return func(c *gin.Context) {
		bearer := c.Request.Header.Get("Authorization")
		if bearer != "" {
			ctx := cubeclient.WithBearer(c.Request.Context(), bearer)
			c.Request = c.Request.WithContext(ctx)
		}
		c.Next()
	}
}
