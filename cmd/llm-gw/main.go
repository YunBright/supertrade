// Package main 是 llm-gw dapr app 入口(智谱 / DeepSeek 统一调用 + 缓存 + 失败降级)。
//
// 启动:`dapr run --app-id llm-gw --app-port 8080 -- go run ./cmd/llm-gw`
package main

import (
	"net/http"

	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/gin-gonic/gin"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID: "llm-gw",
		Register: func(r *gin.Engine) {
			// LLM 调用入口(其它 dapr app 通过 dapr service invocation 调)
			r.POST("/chat", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"content": "placeholder", "app": "llm-gw"})
			})
		},
	})
}