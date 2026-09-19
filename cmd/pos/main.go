// Package main 是 pos dapr app 入口(销售开单 / 收款 / 改价 / 退货 / 班次)。
//
// 启动:`dapr run --app-id pos --app-port 8080 -- go run ./cmd/pos`
package main

import (
	"net/http"

	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/gin-gonic/gin"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID: "pos",
		Register: func(r *gin.Engine) {
			r.POST("/sales", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"sale_id": "placeholder", "app": "pos"})
			})
		},
	})
}