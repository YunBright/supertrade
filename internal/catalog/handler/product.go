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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/archivedto"
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
// # 与商品选择控件那两个端点的关系
//
//	GET /products、/products/search  分页,商品选择控件在用(已按数据源分流)
//	GET /products/export             **不分页**,含换购品/停用品 —— 给"离线数据包"用
//
// # ⚠️ 门店只从 X-Branch-ID header 取
//
// body / query 里**不认**门店 —— 否则 A 店的登录态能把 B 店的商品导走。
//
// # 数据来源按「门店配没配数据源」切
//
//	配了源 ⇒ 上游导出端点(cube-router)的商品(该门店的档案在外部系统里)
//	没配源 ⇒ 本域 PG
//
// ⚠️ **路径与响应形状都不变**(前端 / POS 离线包在用)⇒ 前端不动。
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
	useCube, err := h.archive.UseCubeForRead(ctx, branchID)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	if useCube {
		h.exportProductsFromCube(c, branchID)
		return
	}
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

// exportProductsFromCube 从上游导出端点取该门店的**全部**商品(离线商品库)。
//
// ⚠️ **响应形状与本地路径逐字一致**(同一批字段名),只有数据来源不同。
//
// ⚠️ 条码子表(item_barcodes)是**尽力而为**:取不到就只用"barcode 回落 id"
// (绝大多数商品的扫码键就是 id),而不是让整个导出失败。
func (h *Handler) exportProductsFromCube(c *gin.Context, branchID string) {
	ctx := c.Request.Context()
	bearer := c.Request.Header.Get("Authorization")

	rows, err := h.archive.ExportScopeAll(ctx, bearer, branchID, archivedto.ScopeProducts)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	barRows, err := h.archive.ExportScopeAll(ctx, bearer, branchID, archivedto.ScopeItemBarcodes)
	if err != nil {
		// 条码是补充信息(主扫码键 = id)⇒ 取不到不阻断导出,但要留下痕迹。
		h.logger.Warn("catalog: 取商品条码失败,离线包只带 id 兜底条码",
			"branch_id", branchID, "err", err)
		barRows = nil
	}
	byProduct := map[string][]string{}
	for _, r := range barRows {
		pid := rowString(r, "product_id")
		code := rowString(r, "barcode")
		if pid == "" || code == "" {
			continue
		}
		byProduct[pid] = append(byProduct[pid], code)
	}

	out := make([]exportProductDTO, 0, len(rows))
	for _, r := range rows {
		id := rowString(r, "id")
		out = append(out, buildExportProductFromRow(r, byProduct[id]))
	}
	c.JSON(http.StatusOK, gin.H{
		"branch_id": branchID,
		"products":  out,
		"count":     len(out),
		"total":     len(out),
	})
}

// buildExportProductFromRow 从上游的契约行组装离线商品。
//
// ⚠️ 与本地路径(buildExportProduct)**同一批字段名** —— 响应形状不许变。
func buildExportProductFromRow(row map[string]any, subBarcodes []string) exportProductDTO {
	id := rowString(row, "id")
	// ⚠️ 上游的 product 模型**没有 barcode 列** ⇒ 空 ⇒ 回落成 id
	// (领域事实:思迅 item_no 就是条码)。
	code := rowString(row, "barcode")
	if code == "" {
		code = id
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
		ID:         id,
		Name:       rowString(row, "name"),
		SubCode:    rowString(row, "sub_code"),
		Barcode:    code,
		Barcodes:   codes,
		Unit:       rowString(row, "unit"),
		Spec:       rowString(row, "spec"),
		CategoryID: rowString(row, "category_id"),
		SupplierID: rowString(row, "supplier_id"),
		Status:     rowString(row, "status"),
		// ⚠️ updated_at 恒为零值:上游的 product 模型只有 created_at,
		// 没有 updated_at ⇒ 不拿 created_at 冒充(那会让客户端以为
		// "这件商品从建档起就没变过")。
		UpdatedAt: time.Time{},
	}
}

// rowString 从契约行里取一个字符串值。
//
// ⚠️ 契约行的值是 any(上游是 JSON)⇒ 可能是 string,也可能是数字。
// 统一转成字符串,免得下游拿到 float64(9.9 → 9.899999999999999)。
func rowString(row map[string]any, key string) string {
	v, ok := row[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	default:
		return fmt.Sprintf("%v", t)
	}
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

// getProduct GET /products/:id —— 查单个商品(按数据源分流)。
//
// ⚠️ 数据来源按「门店配没配数据源」切(见 service/source.go):
//
//	配了源 ⇒ cube(cube-router → 该门店的 cube 实例)
//	没配源 ⇒ 本域 PG(自营店 10)
//
// ⚠️ 两条路径返回**同一个 DTO**(cubeclient.ProductDTO)⇒ 响应形状不变,
// 前端不用改。
func (h *Handler) getProduct(c *gin.Context) {
	id := c.Param("id")
	branchID := middleware.SingleBranchFromCtx(c)
	// ⚠️ 把**已解析的**门店写回 ctx:本域路径按它查表,
	// cube 路径按它路由 —— 两条路径看同一个门店。
	ctx := cubeclient.WithBranchID(c.Request.Context(), branchID)
	p, err := h.catalog.GetProduct(ctx, branchID, id)
	if err != nil {
		h.mapProductErr(c, err)
		return
	}
	c.JSON(http.StatusOK, p)
}

// mapProductErr 把单个商品查询的错误映到 HTTP。
//
// ⚠️ "查不到"在两条路径上是**同一个结论**(404 product_not_found):
// 前端不需要知道这个商品是从 cube 还是本域 PG 找到的。
func (h *Handler) mapProductErr(c *gin.Context, err error) {
	if h.writeSourceErr(c, err) {
		return
	}
	switch {
	case errors.Is(err, model.ErrProductNotFound), errors.Is(err, cubeclient.ErrProductNotFound):
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"code":    "product_not_found",
			"message": err.Error(),
		})
	case errors.Is(err, cubeclient.ErrStockNotFound):
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"code":    "stock_not_found",
			"message": err.Error(),
		})
	case errors.Is(err, service.ErrCubeUnavailable):
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code":    "cube_unavailable",
			"message": err.Error(),
		})
	case errors.Is(err, model.ErrInvalidInput):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "bad_request",
			"message": err.Error(),
		})
	default:
		h.logger.Error("catalog 查商品失败", "err", err, "path", c.Request.URL.Path)
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{
			"code":    "cube_error",
			"message": err.Error(),
		})
	}
}
