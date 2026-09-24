// Package handler / product.go —— 商品 HTTP handler(本期 entry,部分实装)。
//
// searchProducts 走"本地表优先 + cube 兜底"双层:
//   - 本地 products 表按 barcode 查 → 命中则返
//   - 未命中 → 走 cubeclient.SearchProductsByBarcode(用于本期未迁移完的存量数据)
//   - cube client == nil → 503 cube_unavailable
//
// getProduct 当前返 501(本地 product CRUD 留待下一 Phase 实装)。
package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// searchProducts GET /products/search?barcode=&limit=
//
// 默认按 X-Branch-ID header 取 branch;cube 兜底时把 branchID 透传,
// cube stock 维度按门店过滤。
func (h *Handler) searchProducts(c *gin.Context) {
	branchID := middleware.BranchFromCtx(c)
	if branchID == nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "missing_branch_id",
			"message": "X-Branch-ID header 必填",
		})
		return
	}
	barcode := c.Query("barcode")
	if barcode == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "missing_barcode",
			"message": "barcode query 必填",
		})
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	if limit <= 0 {
		limit = 10
	}

	// 本地表查(本期 supplier 服务已有数据,product 表当前是空的;
	// 未来实装 product service 后,这里优先查本地表;现在直接走 cube 兜底)。
	if h.cube == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code":    "cube_unavailable",
			"message": "cube client 未配置,本地 products 表为空",
		})
		return
	}
	rows, err := h.cube.SearchProductsByBarcode(c.Request.Context(), barcode, branchID.String(), limit)
	if err != nil {
		h.logger.Error("cube SearchProductsByBarcode 失败", "err", err, "barcode", barcode)
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{
			"code":    "cube_error",
			"message": err.Error(),
		})
		return
	}

	// 透传到 gin 响应(对齐旧 cubehttp.searchProducts 的字段)
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
		if r.Stock != nil {
			row["stock_qty"] = r.Stock.Quantity
			row["avg_cost_yuan"] = r.Stock.AvgCostYuan
		}
		if r.Product.SupplierID != "" && h.cube != nil {
			supps, _ := h.cube.SearchSuppliers(c.Request.Context(), r.Product.SupplierID, 1)
			for _, s := range supps {
				if s.ID == r.Product.SupplierID {
					row["supplier_id"] = s.ID
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
			"barcode_query": barcode,
			"branch_id":     branchID.String(),
		},
	})
}

// getProduct GET /products/:id —— 本期返 501,留待未来实装本地 CRUD。
func (h *Handler) getProduct(c *gin.Context) {
	// 兜底走 cube(原 cubehttp.getProduct 的行为),保留现有 stocktake
	// SearchProducts / 后续 product CRUD 的访问入口。
	id := c.Param("id")
	if h.cube == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code":    "cube_unavailable",
			"message": "cube client 未配置",
		})
		return
	}
	p, err := h.cube.GetProduct(c.Request.Context(), id)
	if err != nil {
		h.mapCubeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, p)
}

// mapCubeErr 把 cubeclient 错误映到 HTTP。
func (h *Handler) mapCubeErr(c *gin.Context, err error) {
	switch {
	case err == cubeclient.ErrProductNotFound:
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"code":    "product_not_found",
			"message": err.Error(),
		})
	case err == cubeclient.ErrStockNotFound:
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"code":    "stock_not_found",
			"message": err.Error(),
		})
	default:
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{
			"code":    "cube_error",
			"message": err.Error(),
		})
	}
}