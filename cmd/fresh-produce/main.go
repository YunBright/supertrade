// Package main 是 fresh-produce dapr app 入口(蔬果盘点驱动毛利,REQUIREMENTS §3)。
//
// 启动:`dapr run --app-id fresh-produce --app-port 8080 -- go run ./cmd/fresh-produce`
package main

import (
	"net/http"

	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/gin-gonic/gin"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID: "fresh-produce",
		Register: func(r *gin.Engine) {
			// 蔬果盘点(可选,周期不定)
			r.POST("/produce-stocktake", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"stocktake_id": "placeholder", "app": "fresh-produce"})
			})
			// 显性报损(可选录入)
			r.POST("/waste-logs", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"waste_log_id": "placeholder", "app": "fresh-produce"})
			})
			// LLM 损耗推演上下文(预留 API,本期只暴露不集成)
			r.GET("/llm/loss-context", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"data": gin.H{}, "note": "placeholder", "app": "fresh-produce"})
			})
		},
	})
}