// Package main 是 erp-connector dapr app 入口(拉取 cube-gateway /v1/load 的思迅等 ERP 销售数据,REQUIREMENTS §5)。
//
// 启动:`dapr run --app-id erp-connector --app-port 8080 -- go run ./cmd/erp-connector`
package main

import (
	"net/http"

	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/gin-gonic/gin"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID: "erp-connector",
		Register: func(r *gin.Engine) {
			// 手动触发一次拉取(平时是 Dapr cron binding 定时拉)
			r.POST("/pull", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"batch_id": "placeholder", "app": "erp-connector"})
			})
			// 同步日志查询
			r.GET("/sync-logs", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"logs": []any{}, "app": "erp-connector"})
			})
		},
	})
}