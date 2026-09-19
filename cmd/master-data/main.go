// Package main 是 master-data dapr app 入口(门店 / 员工 / 班次 / 供应商 / 客户)。
//
// 启动:`dapr run --app-id master-data --app-port 8103 -- go run ./cmd/master-data`
//
// 本期定位(REQUIREMENTS §7.5 / DESIGN §3):
//
//	master-data 是 cube supplier / customer 的**转发**出口,同时维护
//	**门店 / 员工 / 班次** 本系统数据(后续 Phase 实现)。
//
// 本期只实现 cube 转发:GET /suppliers 查 cube supplier。
// 门店 / 员工 CURD 留待 Phase 2(需新增 GORM model + PG 表)。
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
		AppID: "master-data",
		Port:  ":8103",
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
	api := r.Group("/api/v1")
	cubehttp.New(appCube).Register(api, cubehttp.RegisterOptions{
		Suppliers: true,
	})
}
