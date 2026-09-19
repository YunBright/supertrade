// Package main 是 sales-agg dapr app 入口(多端销售聚合:自营 POS + erp-connector 拉取的思迅等,供 BI / 其它 dapr app 拉取)。
//
// 启动:`dapr run --app-id sales-agg --app-port 8080 -- go run ./cmd/sales-agg`
package main

import (
	"net/http"

	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/gin-gonic/gin"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID: "sales-agg",
		Register: func(r *gin.Engine) {
			// 销售视图查询(分钟 / 日,BI 反查用)
			r.GET("/sales/view", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"rows": []any{}, "app": "sales-agg"})
			})
		},
	})
}