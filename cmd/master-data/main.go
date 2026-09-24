// Package main 是 master-data dapr app 入口(门店 / 员工 / 班次)。
//
// 本期定位(2026-09 重构后):
//   - **不再**转发 supplier(已搬到 catalog 服务)
//   - master-data 后续 Phase 负责"门店 / 员工 / 班次"本地表(暂未实装)
//
// 当前唯一职责:留 cmd 占位,待 PR 4 删除整个 cmd。
//
// 启动:`dapr run --app-id master-data --app-port 8102 -- go run ./cmd/master-data`
package main

import (
	"net/http"

	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/gin-gonic/gin"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID:    "master-data",
		Port:     ":8102",
		Register: registerRoutes,
	})
}

// registerRoutes 仅保留 /healthz(cmdbootstrap 已自动注册),
// 不再挂业务路由(原 /suppliers 转发已在 PR 2 删除,搬到 catalog 服务)。
//
// healthz 之外无路由 → 所有请求都会被 cmdbootstrap 的 auth 中间件拦截到 401;
// 这符合"占位 cmd"语义(PR 4 整删)。
func registerRoutes(r *gin.Engine) {
	// 显式声明一个 placeholder 路由,避免 gin 在空 Engine 上启动时报 "no routes" 警告。
	// cmdbootstrap 已注册 /healthz;此处加 / 占位以确保 no route 兜底走 404 而非 panic。
	r.GET("/_placeholder", func(c *gin.Context) {
		c.JSON(http.StatusNotImplemented, gin.H{
			"code":    "not_implemented",
			"message": "master-data 占位 cmd,门店/员工/班次待下一 Phase 实装",
		})
	})
}