// Package main 是 notification dapr app 入口(企微 / 钉钉 / 短信通知)。
//
// 启动:`dapr run --app-id notification --app-port 8080 -- go run ./cmd/notification`
package main

import (
	"net/http"

	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/gin-gonic/gin"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID: "notification",
		Register: func(r *gin.Engine) {
			r.POST("/send", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"message_id": "placeholder", "app": "notification"})
			})
		},
	})
}