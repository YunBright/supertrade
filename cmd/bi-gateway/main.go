// Package main 是 bi-gateway dapr app 入口(BI 出口:读 cube-gateway /v1/load + 本系统聚合数据)。
//
// 启动:`dapr run --app-id bi-gateway --app-port 8080 -- go run ./cmd/bi-gateway`
package main

import (
	"net/http"

	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/gin-gonic/gin"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID: "bi-gateway",
		Register: func(r *gin.Engine) {
			// BI 看板聚合(forward cube + 调本系统其它 dapr app)
			r.GET("/dashboard/daily", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"date": "placeholder", "app": "bi-gateway"})
			})
		},
	})
}