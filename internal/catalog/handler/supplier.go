// Package handler / supplier.go —— 供应商 HTTP handler。
package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/catalog/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// createSupplierReq POST /suppliers 的入参。
type createSupplierReq struct {
	ID      string `json:"id" binding:"required"`
	Name    string `json:"name" binding:"required"`
	Type    string `json:"type"`
	Contact string `json:"contact"`
	Phone   string `json:"phone"`
	Email   string `json:"email"`
	Address string `json:"address"`
}

// updateSupplierReq PUT /suppliers/:id 的入参(部分字段可选)。
type updateSupplierReq struct {
	Name    *string `json:"name"`
	Type    *string `json:"type"`
	Contact *string `json:"contact"`
	Phone   *string `json:"phone"`
	Email   *string `json:"email"`
	Address *string `json:"address"`
	Status  *string `json:"status"`
}

// listSuppliers GET /suppliers?q=&limit=
//
// 强制按 X-Branch-ID 过滤(中间件保证 header 是合法 UUID)。
func (h *Handler) listSuppliers(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "missing_branch_id",
			"message": "X-Branch-ID header 必填",
		})
		return
	}
	q := c.Query("q")
	limit, _ := strconv.Atoi(c.Query("limit"))
	if limit <= 0 {
		limit = 50
	}
	rows, err := h.suppliers.List(c.Request.Context(), branchID, q, limit)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"suppliers": rows,
		"count":     len(rows),
	})
}

// getSupplier GET /suppliers/:id
func (h *Handler) getSupplier(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "missing_branch_id",
			"message": "X-Branch-ID header 必填",
		})
		return
	}
	row, err := h.suppliers.Get(c.Request.Context(), branchID, c.Param("id"))
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, row)
}

// createSupplier POST /suppliers
func (h *Handler) createSupplier(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "missing_branch_id",
			"message": "X-Branch-ID header 必填",
		})
		return
	}
	var req createSupplierReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "bad_request",
			"message": err.Error(),
		})
		return
	}
	row, err := h.suppliers.Create(c.Request.Context(), service.CreateSupplierInput{
		ID:        req.ID,
		BranchID:  branchID,
		Name:      req.Name,
		Type:      req.Type,
		Contact:   req.Contact,
		Phone:     req.Phone,
		Email:     req.Email,
		Address:   req.Address,
		CreatedBy: callerSub(c),
	})
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, row)
}

// updateSupplier PUT /suppliers/:id
func (h *Handler) updateSupplier(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "missing_branch_id",
			"message": "X-Branch-ID header 必填",
		})
		return
	}
	var req updateSupplierReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "bad_request",
			"message": err.Error(),
		})
		return
	}
	row, err := h.suppliers.Update(c.Request.Context(), branchID, c.Param("id"), service.UpdateSupplierInput{
		Name:      req.Name,
		Type:      req.Type,
		Contact:   req.Contact,
		Phone:     req.Phone,
		Email:     req.Email,
		Address:   req.Address,
		Status:    req.Status,
		UpdatedBy: callerSub(c),
	})
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, row)
}

// deleteSupplier DELETE /suppliers/:id
func (h *Handler) deleteSupplier(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "missing_branch_id",
			"message": "X-Branch-ID header 必填",
		})
		return
	}
	if err := h.suppliers.Delete(c.Request.Context(), branchID, c.Param("id")); err != nil {
		h.mapErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}