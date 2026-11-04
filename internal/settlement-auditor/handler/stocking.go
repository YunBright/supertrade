// Package handler / stocking.go —— 备货系数维护端点（2026-10-31 用户口径）。
//
// # 端点
//
//	GET    /stocking-coefficients               列表（按货号/名称包含匹配 + 分页）
//	PUT    /stocking-coefficients/:product_id   人工修改倍数/周期
//	DELETE /stocking-coefficients/:product_id   删除（下次审批自动重算）
//	POST   /stocking-coefficients/regenerate    批量重新生成（人工行跳过）
//
// # 权限
//
//	备货系数是**商品**维度的经营参数 ⇒ 读走 `product:view`、写走 `product:manage`。
//	这两个 action 都**不在 merchant 的清单里**（见 auth 迁移 001/002）——
//	店长改不了系数，也就没法通过压低倍数让「进货过量」的提示消失。
package handler

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/YunBright/authkit/claims"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
	"github.com/YunBright/supertrade/internal/settlement-auditor/service"
)

// 备货系数端点使用的 scope。
const (
	ScopeStockingRead  = "product:view"
	ScopeStockingWrite = "product:manage"
)

// stockingUpdateReq 是人工修改备货系数的请求体。
//
// ⚠️ multiplier 用字符串接收（decimal 的 JSON 编码是字符串，前端也按字符串发）。
type stockingUpdateReq struct {
	Multiplier string `json:"multiplier"`
	PeriodDays int    `json:"period_days"`
	Note       string `json:"note"`
}

type stockingRegenerateReq struct {
	ProductIDs []string `json:"product_ids"`
}

type stockingListResp struct {
	Items []stockingRow `json:"items"`
	Total int64         `json:"total"`
}

type stockingRow struct {
	ID             string `json:"id"`
	BranchID       string `json:"branch_id"`
	ProductID      string `json:"product_id"`
	ProductName    string `json:"product_name"`
	Multiplier     string `json:"multiplier"`
	PeriodDays     int    `json:"period_days"`
	SampleDays     int    `json:"sample_days"`
	AvgDailySales  string `json:"avg_daily_sales"`
	PeriodSalesQty string `json:"period_sales_qty"`
	ReceiptCount   int    `json:"receipt_count"`
	IntervalDays   int    `json:"interval_days"`
	DailySalesCV   string `json:"daily_sales_cv"`
	Source         string `json:"source"`
	Note           string `json:"note,omitempty"`
	OperatorID     string `json:"operator_id,omitempty"`
	ComputedAt     string `json:"computed_at,omitempty"`
	UpdatedAt      string `json:"updated_at,omitempty"`
}

func toStockingRow(r model.StockingCoefficient) stockingRow {
	return stockingRow{
		ID:             r.ID,
		BranchID:       r.BranchID,
		ProductID:      r.ProductID,
		ProductName:    r.ProductName,
		Multiplier:     r.Multiplier.StringFixed(2),
		PeriodDays:     r.PeriodDays,
		SampleDays:     r.SampleDays,
		AvgDailySales:  r.AvgDailySales.StringFixed(3),
		PeriodSalesQty: r.PeriodSalesQty.StringFixed(3),
		ReceiptCount:   r.ReceiptCount,
		IntervalDays:   r.IntervalDays,
		DailySalesCV:   r.DailySalesCV.StringFixed(3),
		Source:         r.Source,
		Note:           r.Note,
		OperatorID:     r.OperatorID,
		ComputedAt:     formatStockingTime(r.ComputedAt),
		UpdatedAt:      formatStockingTime(r.UpdatedAt),
	}
}

// formatStockingTime 是零值安全的 RFC3339 输出（零值输出空串）。
func formatStockingTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// operatorOf 取当前登录用户 id（维护留痕用；取不到返回空串）。
func operatorOf(c *gin.Context) string {
	if cl, ok := claims.FromContext(c.Request.Context()); ok && cl != nil {
		return cl.Sub
	}
	return ""
}

// ListStockingCoefficients GET /stocking-coefficients
func (h *Handler) ListStockingCoefficients(c *gin.Context) {
	branchID := h.branchIDOf(c)
	if !h.requireScope(c, branchID, ScopeStockingRead) {
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	rows, total, err := h.svc.ListStockingCoefficients(c.Request.Context(), branchID, c.Query("q"), limit, offset)
	if err != nil {
		mapErr(c, err)
		return
	}
	out := make([]stockingRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, toStockingRow(r))
	}
	c.JSON(http.StatusOK, stockingListResp{Items: out, Total: total})
}

// UpdateStockingCoefficient PUT /stocking-coefficients/:product_id
func (h *Handler) UpdateStockingCoefficient(c *gin.Context) {
	branchID := h.branchIDOf(c)
	if !h.requireScope(c, branchID, ScopeStockingWrite) {
		return
	}
	var req stockingUpdateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体非法: "+err.Error())
		return
	}
	mult, err := decimal.NewFromString(strings.TrimSpace(req.Multiplier))
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "multiplier 必须是数字")
		return
	}
	row, err := h.svc.UpdateStockingCoefficient(c.Request.Context(), branchID, c.Param("product_id"), service.StockingUpdateInput{
		Multiplier: mult,
		PeriodDays: req.PeriodDays,
		Note:       req.Note,
		OperatorID: operatorOf(c),
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toStockingRow(*row))
}

// DeleteStockingCoefficient DELETE /stocking-coefficients/:product_id
func (h *Handler) DeleteStockingCoefficient(c *gin.Context) {
	branchID := h.branchIDOf(c)
	if !h.requireScope(c, branchID, ScopeStockingWrite) {
		return
	}
	if err := h.svc.DeleteStockingCoefficient(c.Request.Context(), branchID, c.Param("product_id")); err != nil {
		mapErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// RegenerateStockingCoefficients POST /stocking-coefficients/regenerate
//
// product_ids 为空 ⇒ 重算本门店已有系数的全部 SKU（人工修改过的跳过）。
func (h *Handler) RegenerateStockingCoefficients(c *gin.Context) {
	branchID := h.branchIDOf(c)
	if !h.requireScope(c, branchID, ScopeStockingWrite) {
		return
	}
	var req stockingRegenerateReq
	// 空 body 合法（= 全量重算）。
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			writeError(c, http.StatusBadRequest, "bad_request", "请求体非法: "+err.Error())
			return
		}
	}
	res, err := h.svc.RegenerateStockingCoefficients(c.Request.Context(), branchID, req.ProductIDs, operatorOf(c))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// ---- 正常陈列保有量（2026-11-02 用户口径）----

// stockingDisplayUpdateReq 是人工修改陈列保有量的请求体。
type stockingDisplayUpdateReq struct {
	DisplayQty string `json:"display_qty"`
	Note       string `json:"note"`
}

type stockingDisplayRow struct {
	ID           string `json:"id"`
	BranchID     string `json:"branch_id"`
	GroupKey     string `json:"group_key"`
	ProductID    string `json:"product_id"`
	ProductName  string `json:"product_name"`
	DisplayQty   string `json:"display_qty"`
	SampleDays   int    `json:"sample_days"`
	ReceiptCount int    `json:"receipt_count"`
	Source       string `json:"source"`
	Note         string `json:"note,omitempty"`
	OperatorID   string `json:"operator_id,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
}

type stockingDisplayListResp struct {
	Items []stockingDisplayRow `json:"items"`
	Total int64                `json:"total"`
}

func toStockingDisplayRow(r model.StockingDisplay) stockingDisplayRow {
	return stockingDisplayRow{
		ID:           r.ID,
		BranchID:     r.BranchID,
		GroupKey:     r.GroupKey,
		ProductID:    r.ProductID,
		ProductName:  r.ProductName,
		DisplayQty:   r.DisplayQty.StringFixed(3),
		SampleDays:   r.SampleDays,
		ReceiptCount: r.ReceiptCount,
		Source:       r.Source,
		Note:         r.Note,
		OperatorID:   r.OperatorID,
		UpdatedAt:    formatStockingTime(r.UpdatedAt),
	}
}

// ListStockingDisplays GET /stocking-displays
func (h *Handler) ListStockingDisplays(c *gin.Context) {
	branchID := h.branchIDOf(c)
	if !h.requireScope(c, branchID, ScopeStockingRead) {
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	rows, total, err := h.svc.ListStockingDisplays(c.Request.Context(), branchID, c.Query("q"), c.Query("source"), limit, offset)
	if err != nil {
		mapErr(c, err)
		return
	}
	out := make([]stockingDisplayRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, toStockingDisplayRow(r))
	}
	c.JSON(http.StatusOK, stockingDisplayListResp{Items: out, Total: total})
}

// UpdateStockingDisplay PUT /stocking-displays/:product_id
func (h *Handler) UpdateStockingDisplay(c *gin.Context) {
	branchID := h.branchIDOf(c)
	if !h.requireScope(c, branchID, ScopeStockingWrite) {
		return
	}
	var req stockingDisplayUpdateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体非法: "+err.Error())
		return
	}
	qty, err := decimal.NewFromString(strings.TrimSpace(req.DisplayQty))
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "display_qty 必须是数字")
		return
	}
	row, err := h.svc.UpdateStockingDisplay(c.Request.Context(), branchID, c.Param("product_id"), service.StockingDisplayUpdateInput{
		DisplayQty: qty,
		Note:       req.Note,
		OperatorID: operatorOf(c),
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toStockingDisplayRow(*row))
}

// DeleteStockingDisplay DELETE /stocking-displays/:product_id
//
// 删除人工行 ⇒ 下次审批按历史单批 P50 自动重建。
func (h *Handler) DeleteStockingDisplay(c *gin.Context) {
	branchID := h.branchIDOf(c)
	if !h.requireScope(c, branchID, ScopeStockingWrite) {
		return
	}
	if err := h.svc.DeleteStockingDisplay(c.Request.Context(), branchID, c.Param("product_id")); err != nil {
		mapErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// ---- 商品档位（可售上限天数）维护 ----

type stockingProfileUpdateReq struct {
	CapDays       int    `json:"cap_days"`
	ShelfLifeDays int    `json:"shelf_life_days"`
	Note          string `json:"note"`
}

type stockingProfileRow struct {
	ID            string `json:"id"`
	BranchID      string `json:"branch_id"`
	ProductID     string `json:"product_id"`
	ProductName   string `json:"product_name"`
	Class         string `json:"class"`
	CapDays       int    `json:"cap_days"`
	ShelfLifeDays int    `json:"shelf_life_days"`
	Source        string `json:"source"`
	Note          string `json:"note,omitempty"`
	OperatorID    string `json:"operator_id,omitempty"`
	UpdatedAt     string `json:"updated_at,omitempty"`
}

type stockingProfileListResp struct {
	Items []stockingProfileRow `json:"items"`
	Total int64                `json:"total"`
}

func toStockingProfileRow(r model.ProductProfile) stockingProfileRow {
	return stockingProfileRow{
		ID:            r.ID,
		BranchID:      r.BranchID,
		ProductID:     r.ProductID,
		ProductName:   r.ProductName,
		Class:         r.Class,
		CapDays:       r.CapDays,
		ShelfLifeDays: r.ShelfLifeDays,
		Source:        r.Source,
		Note:          r.Note,
		OperatorID:    r.OperatorID,
		UpdatedAt:     formatStockingTime(r.UpdatedAt),
	}
}

// ListStockingProfiles GET /stocking-profiles
func (h *Handler) ListStockingProfiles(c *gin.Context) {
	branchID := h.branchIDOf(c)
	if !h.requireScope(c, branchID, ScopeStockingRead) {
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	rows, total, err := h.svc.ListProductProfiles(c.Request.Context(), branchID, c.Query("q"), c.Query("source"), limit, offset)
	if err != nil {
		mapErr(c, err)
		return
	}
	out := make([]stockingProfileRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, toStockingProfileRow(r))
	}
	c.JSON(http.StatusOK, stockingProfileListResp{Items: out, Total: total})
}

// UpdateStockingProfile PUT /stocking-profiles/:product_id
func (h *Handler) UpdateStockingProfile(c *gin.Context) {
	branchID := h.branchIDOf(c)
	if !h.requireScope(c, branchID, ScopeStockingWrite) {
		return
	}
	var req stockingProfileUpdateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体非法: "+err.Error())
		return
	}
	row, err := h.svc.UpdateProductProfile(c.Request.Context(), branchID, c.Param("product_id"), service.ProductProfileUpdateInput{
		CapDays:       req.CapDays,
		ShelfLifeDays: req.ShelfLifeDays,
		Note:          req.Note,
		OperatorID:    operatorOf(c),
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toStockingProfileRow(*row))
}

// DeleteStockingProfile DELETE /stocking-profiles/:product_id
func (h *Handler) DeleteStockingProfile(c *gin.Context) {
	branchID := h.branchIDOf(c)
	if !h.requireScope(c, branchID, ScopeStockingWrite) {
		return
	}
	if err := h.svc.DeleteProductProfile(c.Request.Context(), branchID, c.Param("product_id")); err != nil {
		mapErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
