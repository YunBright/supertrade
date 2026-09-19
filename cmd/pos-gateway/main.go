// Package main 是 pos-gateway dapr app 入口(BFF,前端对接)。
//
// 启动:`dapr run --app-id pos-gateway --app-port 8080 -- go run ./cmd/pos-gateway`
package main

import (
	"net/http"

	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/gin-gonic/gin"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID: "pos-gateway",
		Register: func(r *gin.Engine) {
			// BFF 聚合示例:前端 POS 首页所需的聚合数据
			r.GET("/v1/home", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{
					"app":     "pos-gateway",
					"section": "home",
					"ok":      true,
				})
			})
		},
	})
}