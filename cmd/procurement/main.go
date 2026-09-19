// Package main 是 procurement dapr app 入口(采购订单 / 收货 / 退货 / 账期)。
//
// 启动:`dapr run --app-id procurement --app-port 8080 -- go run ./cmd/procurement`
package main

import (
	"net/http"

	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/gin-gonic/gin"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID: "procurement",
		Register: func(r *gin.Engine) {
			r.GET("/purchase-orders", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"purchase_orders": []any{}, "app": "procurement"})
			})
		},
	})
}