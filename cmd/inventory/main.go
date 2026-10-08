// Package main 是 inventory dapr app 入口(批次库存 / 保质期 / 出入库流水 / cube stock 转发)。
//
// 启动:`dapr run --app-id inventory --app-port 8102 -- go run ./cmd/inventory`
//
// 本期定位(REQUIREMENTS §7.5 / DESIGN §3):
//
//	inventory 是 cube stock 的**转发 / 聚合 / 短期缓存**出口。
//	**不维护库存表**,所有读走 cube-router /v1/load(cube-router 按 X-Branch-ID
//	路由到具体 cube 实例);写入(出入库)本期不实现,留待 Phase 2 与 erp-connector / pos 联动。
//
// 端口分配见 cmd/catalog/main.go 注释。
//
// 配置:
//
//	CUBE_APP_ID      = "supertrade-cube-router"  // 默认(走 cube-router 多源路由)
//	CUBE_QUERY_PATH  = "v1/load"                 // 默认(该 app-id 上的查询路径)
//
// 两者必须成对,且都取自实际部署身份,详见 internal/cubeclient/errors.go 的对照表。
// 没有 mock / 内存模式:测试用 internal/cubeclient/cubeclientfake。
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
			cube, err := cubehttp.NewClient()
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
	// 把 caller 的 Authorization + X-Branch-ID 注入 ctx,后续 cubeclient 内部自动 forward。
	// X-Branch-ID 缺了 cube-router 会 400,见 forwardOutgoingHeaders 注释。
	r.Use(forwardOutgoingHeaders())
	cubehttp.New(appCube).Register(r, cubehttp.RegisterOptions{
		Stock:         true,
		RequireBranch: true, // /stock/:product_id 走 claims.AccessibleBranches 守门(branch 从 X-Branch-ID header 取)
	})
}

// forwardOutgoingHeaders 把 caller 的 Authorization + X-Branch-ID 两个头都注入 ctx。
//
// Authorization:cubeclient 读出来拼成 outgoing gRPC metadata,sidecar 转成下游
// cube-router 的 Authorization 头。
//
// X-Branch-ID:**必须一起透传**。cube-router 的 /v1/load 靠这个头解析门店
// (branch_cube_sources → 该门店专属 cube 实例),缺了直接返 400 branch_required,
// 而 dapr 会把 400 包成 gRPC "Internal: Bad Request" —— 现象是本服务返 500
// cube_error,完全看不出是少了门店头。
//
// 这个漏传长期没被发现,是因为本服务此前走 CUBE_CLIENT_MODE 默认的 InMemoryClient,
// 从来没有真的发过请求到 cube。2026-10-08 移除 mock 后才暴露。
func forwardOutgoingHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		if bearer := c.Request.Header.Get("Authorization"); bearer != "" {
			ctx = cubeclient.WithBearer(ctx, bearer)
		}
		if branchID := c.Request.Header.Get("X-Branch-ID"); branchID != "" {
			ctx = cubeclient.WithBranchID(ctx, branchID)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
