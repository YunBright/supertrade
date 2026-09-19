// Package main 是 catalog dapr app 入口(SKU / 品类 / 单位 / 条码)。
//
// 启动:`dapr run --app-id catalog --app-port 8101 -- go run ./cmd/catalog`
//
// 本期定位(REQUIREMENTS §7.5 / DESIGN §3):
//
//	catalog **不维护** SKU / category / unit / barcode 表,**只做 cube 转发**。
//	所有读写走 cube-gateway /v1/load;本系统通过 dapr service invocation 调用。
//
// 端口分配(同机跑多个 dapr app 时避免冲突):
//
//	catalog      :8101    inventory :8102    master-data :8103
//	stocktake    :8080    pos       :8104    ...
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
		AppID: "catalog",
		Port:  ":8101",
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

// appCube 持有 cube client(catalog 只读 cube,不连 PG)。
var appCube cubeclient.Client

func registerRoutes(r *gin.Engine) {
	api := r.Group("/api/v1")
	cubehttp.New(appCube).Register(api, cubehttp.RegisterOptions{
		Products: true,
	})
}
