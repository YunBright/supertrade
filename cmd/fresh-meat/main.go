// Package main 是 fresh-meat dapr app 入口(整猪 + 早盘 LLM + 整店按部位盘点,REQUIREMENTS §4)。
//
// 启动:`dapr run --app-id fresh-meat --app-port 8080 -- go run ./cmd/fresh-meat`
package main

import (
	"net/http"

	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/gin-gonic/gin"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID: "fresh-meat",
		Register: func(r *gin.Engine) {
			// 早盘整猪录入(一头一行)
			r.POST("/whole-pigs", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"pig_id": "placeholder", "app": "fresh-meat"})
			})
			// 部分追加条码
			r.POST("/pig-cuts", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"cut_id": "placeholder", "app": "fresh-meat"})
			})
			// 整店按部位盘点(可选,不阻断销售)
			r.POST("/pork-cuts-stocktake", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"stocktake_id": "placeholder", "app": "fresh-meat"})
			})
		},
	})
}