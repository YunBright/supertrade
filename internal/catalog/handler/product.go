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
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/catalog/model"
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

// exportProductDTO 是离线商品库里的一行。
//
// # ⚠️ barcode 与 barcodes 是**两回事**
//
//	barcode  = **单个**扫码键(客户端按它建索引)
//	barcodes = 该商品的**全部**扫码键(1~7 条,扫码枪可能扫到任一条)
//
// # ⚠️ barcode 为什么要**回落到 id**
//
// `products.barcode` 在生产**根本没赋值**(导入时不写这一列,
// 见 service/archive.go 建 model.Product 的地方)⇒ 全是空串。
// 而绝大多数商品的扫码键就是 `products.id`(思迅 item_no,形如 6922303199721)
// ⇒ 空条码时用 id 兜底,否则离线 POS 扫什么都扫不出来。
//
// 兜底后的 id 也会进 barcodes 数组 ⇒ 客户端一张表就能建"码 → 商品"索引。
type exportProductDTO struct {
	// ID 是货号(思迅 item_no),也是**扫码键**之一。
	ID string `json:"id"`
	// Name 是品名(已去掉思迅的「(换购)」「（停用）」前缀)。
	Name string `json:"name"`
	// SubCode 是自编码/助记码(**不唯一**,见 model.Product.SubCode)。
	SubCode string `json:"sub_code"`
	// Barcode 是主扫码键(products.barcode 为空时回落成 id)。
	Barcode string `json:"barcode"`
	// Barcodes 是该商品的**全部**扫码键(子表条码 + 回落后的 id,去重)。
	//
	// ⚠️ 永远是**数组**(不返回 null)—— 客户端遍历时不用判空。
	Barcodes []string `json:"barcodes"`
	// Unit 是基本单位(实测值直接就是单位名本身如"提")。
	Unit string `json:"unit"`
	// Spec 是规格(思迅 item_size,实测有 "1*10" 这类脏值 —— 原样存)。
	Spec string `json:"spec"`
	// CategoryID / SupplierID 是分类与供应商(外键)。
	CategoryID string `json:"category_id"`
	SupplierID string `json:"supplier_id"`
	// Status 是商品状态(导入时从思迅状态码映射而来,**不翻译**成 active/inactive)。
	Status string `json:"status"`
	// UpdatedAt 是最后修改时刻(RFC3339,与 model.Product 的 JSON 编码一致)。
	UpdatedAt time.Time `json:"updated_at"`
}

// exportLocalProducts GET /products/export —— 离线商品库(一次给全)。
//
// # 与 listLocalProducts 的关系
//
//	GET /products/local   分页,默认过滤换购品/停用品 —— 给"商品选择控件"用
//	GET /products/export  **不分页**,含换购品/停用品 —— 给"离线数据包"用
//
// ⚠️ **不改动 listLocalProducts 的现有行为**:商品选择控件与 tradewind_app
// 都在用那条路径,响应形状不能动。
//
// # ⚠️ 门店只从 X-Branch-ID header 取
//
// body / query 里**不认**门店 —— 否则 A 店的登录态能把 B 店的商品导走。
func (h *Handler) exportLocalProducts(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "branch_required",
			"message": "X-Branch-ID header 必填",
		})
		return
	}
	ctx := c.Request.Context()
	rows, err := h.products.ExportAll(ctx, branchID)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	// 条码一次取全(1 商品 : 1~7 条 ⇒ 逐个商品查是 N 次往返)。
	byProduct := map[string][]string{}
	if h.barcodes != nil {
		byProduct, err = h.barcodes.ListByBranch(ctx, branchID)
		if err != nil {
			h.mapErr(c, err)
			return
		}
	}

	out := make([]exportProductDTO, 0, len(rows))
	for _, p := range rows {
		out = append(out, buildExportProduct(p, byProduct[p.ID]))
	}
	c.JSON(http.StatusOK, gin.H{
		"branch_id": branchID,
		"products":  out,
		"count":     len(out),
		"total":     len(out),
	})
}

// buildExportProduct 组装一行离线商品。
//
// ⚠️ 扫码键的组装顺序:
//  1. 子表条码(主条码在前)
//  2. 回落后的主扫码键(products.barcode 非空用它,否则用 id)
//
// 去重 ⇒ 同一商品的多个条码不重复出现。
func buildExportProduct(p model.Product, subBarcodes []string) exportProductDTO {
	// ⚠️ barcode 回落:products.barcode 在生产是空串(导入不赋值),
	//   而绝大多数商品的扫码键就是 id ⇒ 空则用 id 兜底。
	code := strings.TrimSpace(p.Barcode)
	if code == "" {
		code = p.ID
	}
	codes := make([]string, 0, len(subBarcodes)+1)
	seen := make(map[string]bool, len(subBarcodes)+1)
	for _, b := range subBarcodes {
		if b == "" || seen[b] {
			continue
		}
		seen[b] = true
		codes = append(codes, b)
	}
	if code != "" && !seen[code] {
		seen[code] = true
		codes = append(codes, code)
	}
	return exportProductDTO{
		ID:         p.ID,
		Name:       p.Name,
		SubCode:    p.SubCode,
		Barcode:    code,
		Barcodes:   codes,
		Unit:       p.Unit,
		Spec:       p.Spec,
		CategoryID: p.CategoryID,
		SupplierID: p.SupplierID,
		Status:     p.Status,
		UpdatedAt:  p.UpdatedAt,
	}
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
