// Package handler / product_barcode.go —— 商品条码 HTTP handler。
//
// # 路由
//
//	GET    /products/:id/barcodes              列某商品的全部条码
//	POST   /products/:id/barcodes              写一条条码(幂等)
//	DELETE /products/:id/barcodes/:barcode     软删一条条码
//	GET    /products/by-barcode/:barcode       **扫条码查商品**(POS 主路径)
//
// # 为什么扫条码单独一条路径
//
// POS 扫码是最高频的调用(每扫一次商品一次),路径里带上条码
// ⇒ 不用 query 参数,也不用"先查条码表再查商品"两次往返。
// 命中后直接返回商品行。
//
// ⚠️ 命中顺序:先查条码表(全集),查不到再退到 `products.barcode`(冗余主条码)。
// 退路存在是因为**绝大多数商品只有一条条码、且条码 = item_no**,
// 而那部分不一定都进了条码表(导入兜底规则见 model/product_barcode.go)。
package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/catalog/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// barcodeReq POST /products/:id/barcodes 的入参。
type barcodeReq struct {
	Barcode   string `json:"barcode" binding:"required"`
	IsPrimary bool   `json:"is_primary"`
	Label     string `json:"label"`
}

// listProductBarcodes GET /products/:id/barcodes
func (h *Handler) listProductBarcodes(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "branch_required",
			"message": "X-Branch-ID header 必填",
		})
		return
	}
	rows, err := h.barcodes.ListByProduct(c.Request.Context(), branchID, c.Param("id"))
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"barcodes": rows,
		"count":    len(rows),
	})
}

// createProductBarcode POST /products/:id/barcodes
func (h *Handler) createProductBarcode(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "branch_required",
			"message": "X-Branch-ID header 必填",
		})
		return
	}
	var req barcodeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "bad_request",
			"message": err.Error(),
		})
		return
	}
	row, err := h.barcodes.Upsert(c.Request.Context(), service.BarcodeInput{
		ProductID: c.Param("id"),
		BranchID:  branchID,
		Barcode:   req.Barcode,
		IsPrimary: req.IsPrimary,
		Label:     req.Label,
	})
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, row)
}

// deleteProductBarcode DELETE /products/:id/barcodes/:barcode
func (h *Handler) deleteProductBarcode(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "branch_required",
			"message": "X-Branch-ID header 必填",
		})
		return
	}
	if err := h.barcodes.Delete(c.Request.Context(), branchID, c.Param("id"), c.Param("barcode")); err != nil {
		h.mapErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// getProductByBarcode GET /products/by-barcode/:code —— **扫编码查商品(候选列表)**。
//
// # 为什么返回**列表**而不是单个商品
//
// 一个编码可能命中多个商品(思迅实测):
//
//   - **自编码**(`item_subno`)**不唯一**:9 组自编码对应 2 个货号,
//     全部是「正常品 + 换购品」配对。
//   - **条码**(`t_bd_item_barcode`):1 个条码只对应 1 个货号,但
//     **一个货号有 1~7 个条码** ⇒ 反查条码时也可能命中多个货号。
//
// ⇒ 收银员**必须能选**。返回 404 只表示"本店没有任何商品匹配这个编码"。
//
// 命中顺序:货号 → 自编码 → 条码(全部 OR,去重后按货号排序)。
func (h *Handler) getProductByBarcode(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "branch_required",
			"message": "X-Branch-ID header 必填",
		})
		return
	}
	code := strings.TrimSpace(c.Param("code"))
	rows, err := h.products.FindCandidatesByCode(c.Request.Context(), branchID, code)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	if len(rows) == 0 {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"code":    "product_not_found",
			"message": "本店没有匹配该编码的商品: " + code,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"products": rows,
		"count":    len(rows),
	})
}
