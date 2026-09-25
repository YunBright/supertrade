// Package cubehttp 把 cubeclient.Client 包装为 Gin handler,供 inventory
// (stock) 复用 cube 转发端点(REQUIREMENTS §7.5 / DESIGN §3)。
//
// 不写任何 PG/SQLite — 全部从 cube 拉,本系统不维护 product/stock/supplier 表
// (supplier / product 已在 PR 2 搬到 catalog 服务,见 internal/catalog)。
//
// 本包本期**仅保留 stock 转发**;product/supplier 端点已删除。
//
// 使用示例(cmd/inventory/main.go):
//
//	cube, _ := cubehttp.NewClientFromEnv()
//	h := cubehttp.New(cube)
//	cmdbootstrap.Run(cmdbootstrap.Options{
//	    AppID: "inventory",
//	    Register: func(r *gin.Engine) {
//	        api := r.Group("/api/v1")
//	        h.Register(api, cubehttp.RegisterOptions{Stock: true})
//	    },
//	})
package cubehttp

import (
	"errors"
	"fmt"
	"net/http"
	"os"

	dapr "github.com/dapr/go-sdk/client"
	"github.com/YunBright/authkit/claims"
	"github.com/YunBright/authkit/rbac"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/pkg/middleware"
	"github.com/gin-gonic/gin"
)

// Handler 是 cube 转发 HTTP handler(仅 stock)。
type Handler struct {
	cube cubeclient.Client
}

// New 构造 Handler。
func New(c cubeclient.Client) *Handler { return &Handler{cube: c} }

// RegisterOptions 选择本次注册哪些路由组 + 守门选项。
//
// 各 cmd 按需选:
//
//	inventory → {Stock: true, RequireBranch: true}
//
// 已删除选项:Products / Suppliers(PR 2 后 catalog 服务拥有本地表)。
type RegisterOptions struct {
	Stock         bool
	RequireBranch bool // /stock/:product_id 走 rbac.RequireBranch(从 claims.AccessibleBranches 校验,branch 从 X-Branch-ID header 取)
}

// Register 把选中的路由组注册到 r。
//
// 所有路由挂在 r 之下,各 cmd 通常把 r 设成 r.Group("/api/v1")。
//
// 2026-09 简化:path 不再有 `:branch_id`,branch 全部走 X-Branch-ID header。
func (h *Handler) Register(r gin.IRouter, opts RegisterOptions) {
	if opts.Stock {
		handlers := []gin.HandlerFunc{}
		if opts.RequireBranch {
			handlers = append(handlers, rbac.RequireBranch(branchFromHeader, accessibleBranchesFromClaims))
		}
		handlers = append(handlers, h.getStock)
		r.GET("/stock/:product_id", handlers...)
	}
}

// branchFromHeader 从 gin.Context 拿 X-Branch-ID header(单店)。
//
// rbac.RequireBranch 签名要求 (string, bool);这里返单店字符串。
func branchFromHeader(c *gin.Context) (string, bool) {
	b := c.GetHeader("X-Branch-ID")
	if b == "" {
		return "", false
	}
	return b, true
}

// accessibleBranchesFromClaims 静态读 claims.AccessibleBranches(JWT snapshot)。
//
// 静态 vs 动态(从 userinfo 读)的取舍:
//   - inventory 跨服务读 stock 是高频请求(扫描/查库存);每次调 userinfo 不划算
//   - 走 claims 缓存由 dapr JWT middleware 控制,claims.AccessibleBranches 在 token 过期前稳定
//   - 权限变更需重新签 token 才能生效(已通过订阅 access_changed 让前端重登录实现)
func accessibleBranchesFromClaims(c *gin.Context) ([]string, bool) {
	cl, ok := claims.FromContext(c.Request.Context())
	if !ok || cl == nil {
		return nil, false
	}
	return cl.AccessibleBranches, true
}

// ---- /stock ----

// getStock GET /stock/:product_id
//
// branch 从 X-Branch-ID header 直接读(c.GetHeader 兜底,middleware 已注入 ctx
// 时 SingleBranchFromCtx 也可走)。handler 不依赖 ctx 注入,测试 / 内部 cron 也能调。
//
// 不锁库,只读 cube 当前快照。
func (h *Handler) getStock(c *gin.Context) {
	// 优先走 SingleBranchFromCtx(middleware 注入 + 多店拆分),再兜底 c.GetHeader。
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		branchID = c.GetHeader("X-Branch-ID")
	}
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "missing_branch_id",
			"message": "X-Branch-ID header 必填",
		})
		return
	}
	productID := c.Param("product_id")
	if productID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"code": "missing_param"})
		return
	}
	s, err := h.cube.GetStock(c.Request.Context(), branchID, productID)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, s)
}

// ---- helpers ----

func (h *Handler) mapErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, cubeclient.ErrProductNotFound):
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"code": "product_not_found", "message": err.Error()})
	case errors.Is(err, cubeclient.ErrStockNotFound):
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"code": "stock_not_found", "message": err.Error()})
	case errors.Is(err, cubeclient.ErrCubeNotFound):
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"code": "cube_not_found", "message": err.Error()})
	default:
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"code": "cube_error", "message": err.Error()})
	}
}

// NewClientFromEnv 按 CUBE_CLIENT_MODE 选 InMemoryClient 或 DaprCubeClient。
//
//	CUBE_CLIENT_MODE=memory  (默认) InMemoryClient(mock 数据,本地 / 测试用)
//	CUBE_CLIENT_MODE=dapr    DaprCubeClient,经 dapr/go-sdk 调 cube /v1/load
//
// 配置项:
//
//	CUBE_APP_ID   = "cube-router"    // 默认 (走 cube-router 多源路由)
//
// SDK 模式不需要 DAPR_ENDPOINT — dapr.NewClient() 自动读 DAPR_GRPC_PORT (默认 :50001);
// dapr run 自动注入 DAPR_GRPC_PORT 到 app 进程 env。
//
// 调用方:catalog / inventory / stocktake.SearchProducts 等所有需转发 cube 的服务。
func NewClientFromEnv() (cubeclient.Client, error) {
	mode := os.Getenv("CUBE_CLIENT_MODE")
	if mode == "" {
		mode = "memory"
	}
	if mode == "memory" {
		return cubeclient.NewInMemoryClient(), nil
	}
	daprCli, err := dapr.NewClient()
	if err != nil {
		return nil, fmt.Errorf("dapr.NewClient: %w (确认 dapr run 已起)", err)
	}
	appID := os.Getenv("CUBE_APP_ID")
	if appID == "" {
		appID = "cube-router"
	}
	return cubeclient.NewDaprCubeClient(daprCli, appID), nil
}