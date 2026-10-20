// Package handler / product.go —— 商品 HTTP handler(单个商品 + 老路径的共用件)。
//
// # 路由分工
//
//	GET /products/:id          查单个商品(查 cube)
//	GET /products/search?barcode=  按条码查商品 —— 实现在 catalog.go
//
// searchProducts 之前在本文件里,2026-10-20 挪到 catalog.go:同一个 URL 现在
// 同时承载"老路径(?barcode=)"与"新路径(?q=,商品选择控件)",两者的分派逻辑
// 必须放在一起看,拆成两个文件反而更难读。
//
// getProduct 当前仍走 cube 兜底(本地 product CRUD 留待下一 Phase 实装)。
package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/cubeclient"
)

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
