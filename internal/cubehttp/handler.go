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
	"net/http"
	"os"

	"github.com/YunBright/authkit/claims"
	"github.com/YunBright/authkit/rbac"
	"github.com/YunBright/supertrade/internal/cubeclient"
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
	RequireBranch bool // /stock/:branch_id/:product_id 走 rbac.RequireBranch(从 claims.AccessibleBranches 校验)
}

// Register 把选中的路由组注册到 r。
//
// 所有路由挂在 r 之下,各 cmd 通常把 r 设成 r.Group("/api/v1")。
func (h *Handler) Register(r gin.IRouter, opts RegisterOptions) {
	if opts.Stock {
		handlers := []gin.HandlerFunc{}
		if opts.RequireBranch {
			handlers = append(handlers, rbac.RequireBranch(branchFromParam, accessibleBranchesFromClaims))
		}
		handlers = append(handlers, h.getStock)
		r.GET("/stock/:branch_id/:product_id", handlers...)
	}
}

// branchFromParam 从 gin.Context path 取 :branch_id(req.branch_id 必填,中间件按 400 拒)。
func branchFromParam(c *gin.Context) (string, bool) {
	id := c.Param("branch_id")
	return id, id != ""
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

// getStock GET /stock/:branch_id/:product_id
//
// 不锁库,只读 cube 当前快照。
func (h *Handler) getStock(c *gin.Context) {
	branchID := c.Param("branch_id")
	productID := c.Param("product_id")
	if branchID == "" || productID == "" {
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

// NewClientFromEnv 按 CUBE_CLIENT_MODE 选 InMemoryClient 或 HTTPCubeClient。
//
//	CUBE_CLIENT_MODE=memory  (默认) InMemoryClient(mock 数据,本地 / 测试用)
//	CUBE_CLIENT_MODE=http    HTTPCubeClient,经 DAPR_ENDPOINT 调 cube-gateway /v1/load
//
// 配置项:
//
//	DAPR_ENDPOINT = "http://localhost:3500"   // 默认
//	CUBE_APP_ID   = "cube-gateway"             // 默认
//
// 调用方:catalog / inventory / (历史)master-data / stocktake.SearchProducts
// 等所有需转发 cube 的服务。
func NewClientFromEnv() (cubeclient.Client, error) {
	daprEP := os.Getenv("DAPR_ENDPOINT")
	if daprEP == "" {
		daprEP = "http://localhost:3001"
	}
	appID := os.Getenv("CUBE_APP_ID")
	if appID == "" {
		appID = "cube-gateway"
	}
	return cubeclient.NewHTTPCubeClient(daprEP, appID), nil
}