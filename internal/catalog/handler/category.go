// Package handler / category.go —— 分类 HTTP handler(本地表 CRUD)。
//
// # 路由分工
//
//	GET    /categories?with_counts=    分类字典(查 cube)—— **既有端点,不动**
//	GET    /categories/:id            查单个分类(本地表)
//	POST   /categories                新建分类
//	PUT    /categories/:id            改分类
//	DELETE /categories/:id            软删分类
//
// ⚠️ `GET /categories` 与 `POST /categories` **同路径不同 method**,gin 允许
// (只有同 method + 同 path 才 panic "handlers are already registered")。
// `GET /categories` 是既有的 cube 端点,tradewind_app 在用 ⇒ **语义不动**。
//
// # 门店口径
//
// 写侧一律按 X-Branch-ID 落到**单一具体门店**(与 supplier 写侧一致)。
// 读侧 `GET /categories/:id` 同样按 header 过滤,不接受 body 里的门店 ——
// body 里带一份门店,A 店的登录态就能改 B 店的数据。
package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/catalog/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// createCategoryReq POST /categories 的入参。
type createCategoryReq struct {
	ID        string `json:"id" binding:"required"`
	Name      string `json:"name" binding:"required"`
	ParentID  string `json:"parent_id"`
	SortOrder int    `json:"sort_order"`
}

// updateCategoryReq PUT /categories/:id 的入参(部分字段可选)。
type updateCategoryReq struct {
	Name      *string `json:"name"`
	ParentID  *string `json:"parent_id"`
	SortOrder *int    `json:"sort_order"`
	Status    *string `json:"status"`
}

// getCategory GET /categories/:id
func (h *Handler) getCategory(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "branch_required",
			"message": "X-Branch-ID header 必填",
		})
		return
	}
	row, err := h.categories.Get(c.Request.Context(), branchID, c.Param("id"))
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, row)
}

// createCategory POST /categories
func (h *Handler) createCategory(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "branch_required",
			"message": "X-Branch-ID header 必填",
		})
		return
	}
	var req createCategoryReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "bad_request",
			"message": err.Error(),
		})
		return
	}
	row, err := h.categories.Create(c.Request.Context(), service.CreateCategoryInput{
		ID:        req.ID,
		BranchID:  branchID,
		Name:      req.Name,
		ParentID:  req.ParentID,
		SortOrder: req.SortOrder,
		CreatedBy: callerSub(c),
	})
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, row)
}

// updateCategory PUT /categories/:id
func (h *Handler) updateCategory(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "branch_required",
			"message": "X-Branch-ID header 必填",
		})
		return
	}
	var req updateCategoryReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "bad_request",
			"message": err.Error(),
		})
		return
	}
	row, err := h.categories.Update(c.Request.Context(), branchID, c.Param("id"), service.UpdateCategoryInput{
		Name:      req.Name,
		ParentID:  req.ParentID,
		SortOrder: req.SortOrder,
		Status:    req.Status,
		UpdatedBy: callerSub(c),
	})
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, row)
}

// deleteCategory DELETE /categories/:id
func (h *Handler) deleteCategory(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "branch_required",
			"message": "X-Branch-ID header 必填",
		})
		return
	}
	if err := h.categories.Delete(c.Request.Context(), branchID, c.Param("id")); err != nil {
		h.mapErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
