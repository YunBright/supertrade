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
//	CUBE_CLIENT_MODE = memory(默认)/ http
//	DAPR_ENDPOINT    = "http://localhost:3500"
//	CUBE_APP_ID      = "cube-gateway"
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
		Port:  ":8105",
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
	cubehttp.New(appCube).Register(r, cubehttp.RegisterOptions{
		Stock:         true,
		RequireBranch: true, // /stock/:branch_id/:product_id 走 claims.AccessibleBranches 守门
	})
}
