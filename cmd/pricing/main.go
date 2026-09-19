// Package main 是 pricing dapr app 入口(售价 / 促销 / 会员 / 改价审批)。
//
// 启动:`dapr run --app-id pricing --app-port 8080 -- go run ./cmd/pricing`
package main

import (
	"net/http"

	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/gin-gonic/gin"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID: "pricing",
		Register: func(r *gin.Engine) {
			r.GET("/price-lists", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"price_lists": []any{}, "app": "pricing"})
			})
		},
	})
}