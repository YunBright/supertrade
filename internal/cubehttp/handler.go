// Package cubehttp 把 cubeclient.Client 包装为 Gin handler,供各 dapr app
// 复用 cube 转发端点(REQUIREMENTS §7.5 / DESIGN §3)。
//
// 不写任何 PG/SQLite — 全部从 cube 拉,本系统不维护 product/stock/supplier 表。
//
// 使用示例(cmd/catalog/main.go):
//
//	cube, _ := cubehttp.NewClientFromEnv()
//	h := cubehttp.New(cube)
//	cmdbootstrap.Run(cmdbootstrap.Options{
//	    AppID: "catalog",
//	    Register: func(r *gin.Engine) {
//	        api := r.Group("/api/v1")
//	        h.Register(api, cubehttp.RegisterOptions{Products: true})
//	    },
//	})
package cubehttp

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"

	"github.com/YunBright/authkit/claims"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/gin-gonic/gin"
)

// Handler 是 cube 转发 HTTP handler。
type Handler struct {
	cube cubeclient.Client
}

// New 构造 Handler。
func New(c cubeclient.Client) *Handler { return &Handler{cube: c} }

// RegisterOptions 选择本次注册哪些路由组。
//
// 各 cmd 按需选:
//
//	catalog     → {Products: true}
//	inventory   → {Stock: true}
//	master-data → {Suppliers: true}
//	stocktake   → 已自带 Service.Handler,无需 cubehttp
type RegisterOptions struct {
	Products  bool
	Stock     bool
	Suppliers bool
}

// Register 把选中的路由组注册到 r。
//
// 所有路由挂在 r 之下,各 cmd 通常把 r 设成 r.Group("/api/v1")。
func (h *Handler) Register(r gin.IRouter, opts RegisterOptions) {
	if opts.Products {
		r.GET("/products", h.listProducts)
		r.GET("/products/search", h.searchProducts)
		r.GET("/products/:id", h.getProduct)
	}
	if opts.Stock {
		r.GET("/stock/:branch_id/:product_id", h.getStock)
	}
	if opts.Suppliers {
		r.GET("/suppliers", h.listSuppliers)
	}
}

// ---- /products ----

// listProducts GET /products?category_id=&limit=
//
// 列表返回(本期简化:可空只返前 limit 个商品的 summary)。
// 后续可加 filter / 分页 / 排序。
func (h *Handler) listProducts(c *gin.Context) {
	// 本期 cube product 查询走 dimensions+equals 路径 —— 简化:limit 用 item_no 范围
	limit := atoiDefault(c.Query("limit"), 50)
	out := []cubeclient.ProductDTO{}
	for i := 1; i <= limit; i++ {
		id := fmt.Sprintf("P-%04d", i)
		p, err := h.cube.GetProduct(c.Request.Context(), id)
		if err != nil {
			break // 命中边界
		}
		out = append(out, *p)
	}
	c.JSON(http.StatusOK, gin.H{
		"products": out,
		"count":    len(out),
		"app":      "cube-forwarded",
	})
}

// getProduct GET /products/:id
func (h *Handler) getProduct(c *gin.Context) {
	id := c.Param("id")
	p, err := h.cube.GetProduct(c.Request.Context(), id)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, p)
}

// searchProducts GET /products/search?barcode=&branch_id=&limit=
//
// 对齐 collect-ai SearchProducts 契约(REQUIREMENTS §2.1.4.1):
//   - barcode 长度策略:<5 返空 / 5~12 后缀匹配 / ≥13 精确
//   - branch_id 默认取自 claims.BranchID
//   - 权限过滤:inventory:view → stock_qty / supplier:view → supplier_*
func (h *Handler) searchProducts(c *gin.Context) {
	barcode := c.Query("barcode")
	if barcode == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"code": "missing_barcode"})
		return
	}
	branchID := c.Query("branch_id")
	if branchID == "" {
		cl, _ := claims.FromContext(c.Request.Context())
		if cl == nil || cl.BranchID == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"code": "missing_branch_id",
			})
			return
		}
		branchID = cl.BranchID
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	if limit <= 0 {
		limit = 10
	}

	rows, err := h.cube.SearchProductsByBarcode(c.Request.Context(), barcode, branchID, limit)
	if err != nil {
		h.mapErr(c, err)
		return
	}

	// 权限过滤(跟 stocktake 一致)
	cl, _ := claims.FromContext(c.Request.Context())
	invViewable := hasScope(cl, "inventory:view")
	supplierViewable := hasScope(cl, "supplier:view")

	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		if r.Product == nil {
			continue
		}
		row := gin.H{
			"barcode":      r.Product.Barcode,
			"product_id":   r.Product.ID,
			"product_name": r.Product.Name,
			"category":     r.Product.CategoryID,
			"unit":         r.Product.Unit,
		}
		if invViewable && r.Stock != nil {
			row["stock_qty"] = r.Stock.Quantity
			row["avg_cost_yuan"] = r.Stock.AvgCostYuan
		}
		if supplierViewable && r.Product.SupplierID != "" {
			row["supplier_id"] = r.Product.SupplierID
			supps, _ := h.cube.SearchSuppliers(c.Request.Context(), r.Product.SupplierID, 1)
			for _, s := range supps {
				if s.ID == r.Product.SupplierID {
					row["supplier_name"] = s.Name
					break
				}
			}
		}
		out = append(out, row)
	}

	c.JSON(http.StatusOK, gin.H{
		"products": out,
		"count":    len(out),
		"meta": gin.H{
			"inv_viewable":      invViewable,
			"supplier_viewable": supplierViewable,
			"barcode_query":     barcode,
		},
	})
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

// ---- /suppliers ----

// listSuppliers GET /suppliers?q=&limit=
func (h *Handler) listSuppliers(c *gin.Context) {
	q := c.Query("q")
	limit, _ := strconv.Atoi(c.Query("limit"))
	if limit <= 0 {
		limit = 50
	}
	out, err := h.cube.SearchSuppliers(c.Request.Context(), q, limit)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"suppliers": out,
		"count":     len(out),
		"q":         q,
	})
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

func hasScope(cl *claims.Claims, scope string) bool {
	if cl == nil {
		return false
	}
	return cl.HasScope(scope)
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
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
// 在 cmd/catalog / cmd/inventory / cmd/master-data 等非 stocktake 服务复用。
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
