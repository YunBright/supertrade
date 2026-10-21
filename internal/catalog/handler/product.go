// Package handler / product.go —— 商品 HTTP handler(本地表 CRUD + cube 兜底件)。
//
// # 路由分工
//
//	GET    /products/:id                   查单个商品(查 cube)
//	GET    /products/search?barcode=        按条码查商品 —— 实现在 catalog.go
//	POST   /products                        新建商品(本地表)
//	PUT    /products/:id                    改商品(本地表)
//	DELETE /products/:id                    软删商品(本地表)
//
// searchProducts 之前在本文件里,2026-10-20 挪到 catalog.go:同一个 URL 现在
// 同时承载"老路径(?barcode=)"与"新路径(?q=,商品选择控件)",两者的分派逻辑
// 必须放在一起看,拆成两个文件反而更难读。
//
// getProduct 当前仍走 cube 兜底(本地 product CRUD 留待下一 Phase 实装)。
//
// # 写侧是本地的,读侧仍是 cube
//
// 2026-10-20 实装了本地商品的增删改,但**读端点没有切**:
//
//   - `GET /products` / `GET /products/search` / `GET /categories` /
//     `GET /suppliers/summary` 是**商品选择控件**的 4 条端点,响应形状由
//     Flutter 端(`lib/domain/product/product_models.dart`)依赖,
//     且 tradewind_app 在用 ⇒ **本切片不动它们**。
//   - 自营门店(门店 10)没有 cube 源 ⇒ picker 对它返 503
//     `cube_source_not_configured`。这是**已知状态**,不是回归 ——
//     切到"本地优先,cube 兜底"是下一个切片,必须同时改响应契约与前端。
//
// ⇒ 本切片只增加**写侧**能力:建档、改档、删档。导入工具靠它落数据。
package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/catalog/service"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// createProductReq POST /products 的入参。
type createProductReq struct {
	ID         string `json:"id" binding:"required"`
	Name       string `json:"name" binding:"required"`
	CategoryID string `json:"category_id"`
	SupplierID string `json:"supplier_id"`
	// SubCode 是自编码/助记码(**不唯一**,见 model.Product.SubCode)。
	SubCode string `json:"sub_code"`
	// IsExchange / IsVoid 是从思迅品名前缀解析出来的标记。
	IsExchange bool   `json:"is_exchange"`
	IsVoid     bool   `json:"is_void"`
	Barcode    string `json:"barcode"`
	Unit       string `json:"unit"`
	Spec       string `json:"spec"`
}

// updateProductReq PUT /products/:id 的入参(部分字段可选)。
type updateProductReq struct {
	Name       *string `json:"name"`
	CategoryID *string `json:"category_id"`
	SupplierID *string `json:"supplier_id"`
	SubCode    *string `json:"sub_code"`
	IsExchange *bool   `json:"is_exchange"`
	IsVoid     *bool   `json:"is_void"`
	Barcode    *string `json:"barcode"`
	Unit       *string `json:"unit"`
	Spec       *string `json:"spec"`
	Status     *string `json:"status"`
}

// createProduct POST /products
func (h *Handler) createProduct(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "branch_required",
			"message": "X-Branch-ID header 必填",
		})
		return
	}
	var req createProductReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "bad_request",
			"message": err.Error(),
		})
		return
	}
	row, err := h.products.Create(c.Request.Context(), service.CreateProductInput{
		ID:         req.ID,
		BranchID:   branchID,
		Name:       req.Name,
		CategoryID: req.CategoryID,
		SupplierID: req.SupplierID,
		SubCode:    req.SubCode,
		IsExchange: req.IsExchange,
		IsVoid:     req.IsVoid,
		Barcode:    req.Barcode,
		Unit:       req.Unit,
		Spec:       req.Spec,
		CreatedBy:  callerSub(c),
	})
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, row)
}

// updateProduct PUT /products/:id
func (h *Handler) updateProduct(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "branch_required",
			"message": "X-Branch-ID header 必填",
		})
		return
	}
	var req updateProductReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "bad_request",
			"message": err.Error(),
		})
		return
	}
	row, err := h.products.Update(c.Request.Context(), branchID, c.Param("id"), service.UpdateProductInput{
		Name:       req.Name,
		CategoryID: req.CategoryID,
		SupplierID: req.SupplierID,
		SubCode:    req.SubCode,
		IsExchange: req.IsExchange,
		IsVoid:     req.IsVoid,
		Barcode:    req.Barcode,
		Unit:       req.Unit,
		Spec:       req.Spec,
		Status:     req.Status,
		UpdatedBy:  callerSub(c),
	})
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, row)
}

// deleteProduct DELETE /products/:id
func (h *Handler) deleteProduct(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "branch_required",
			"message": "X-Branch-ID header 必填",
		})
		return
	}
	if err := h.products.Delete(c.Request.Context(), branchID, c.Param("id")); err != nil {
		h.mapErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// listLocalProducts GET /products/local —— 本地表商品列表(分页)。
//
// ⚠️ 与既有的 `GET /products`(cube,商品选择控件在用)是**两个端点**。
// 之所以不合并到一条路径上分派,是因为两者响应形状不同
// (本地是裸数组 + count,Picker 是 products[] + total + page + page_size),
// 合并会让前端两套解析逻辑都得留着。
//
// 单独开一条路径,前端按"数据源"显式选:自营门店用这条,cube 门店用老的。
func (h *Handler) listLocalProducts(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "branch_required",
			"message": "X-Branch-ID header 必填",
		})
		return
	}
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	// ⚠️ **不传 = 默认值**;越界 = 400(不是静默截断)。
	//
	//	原因:之前是"越界静默截断 + **回显请求值**"
	//	⇒ 客户端请求 page_size=500,服务端只给 200 条,响应里却写 page_size=500
	//	⇒ 客户端以为这一页有 500 条 ⇒ **漏数据,且零报错**。
	//
	//	⇒ 现在:越界直接 400;回显**生效值**(归一化之后)。
	if page == 0 {
		page = 1
	}
	if pageSize == 0 {
		pageSize = service.DefaultProductPageSize
	}
	if page < 1 {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "bad_request", "message": "page 必须 >= 1",
		})
		return
	}
	if pageSize < 1 || pageSize > service.MaxProductPageSize {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "bad_request",
			"message": "page_size 必须在 1.." + strconv.Itoa(service.MaxProductPageSize) + " 之间",
		})
		return
	}
	rows, total, err := h.products.List(c.Request.Context(), branchID, service.LocalProductQuery{
		Keyword:    c.Query("q"),
		CategoryID: c.Query("category_id"),
		SupplierID: c.Query("supplier_id"),
		Status:     c.Query("status"),
		Page:       page,
		PageSize:   pageSize,
	})
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"products":  rows,
		"count":     len(rows),
		"total":     total,
		"page":      page,
		"page_size": pageSize,
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
