// Package handler 实现 inventory 服务的 HTTP handler。
//
// 路由(nginx 已剥过 /api/v1/inventory/):
//
//	GET /stock-levels?branch_id=&product_id=&keyword=&page=&page_size=  库存快照列表(inventory:view)
//	GET /stock-levels/:product_id                                    单个商品库存(inventory:view)
//	GET /stock-moves?branch_id=&product_id=&move_type=&occurred_from=&occurred_to=&page=&page_size=
//	                                                                   库存流水列表(inventory:view)
//
// # 权限
//
// 三个端点全部挂 `rbac.RequireScopeWithBranch("inventory:view", ...)` ——
// scope 是 **per-branch 实时解析**的(走 auth 的 /internal/users/{id}/permissions)。
//
// ⚠️ **scope 名来自 auth 的 permissions 表 seed**
// (auth/migrations/001_init_schema.up.sql:`inventory:view` = "查库存")。
//
//	同批还有 `inventory:manage`(改库存 / 盘点单)与 `inventory:approve`(审核盘点单),
//	本服务只读 ⇒ 只用 `inventory:view`。
//
// # 门店从哪来
//
// ⚠️ **门店只认 X-Branch-ID header(query 的 branch_id 只能与它一致)**。
//
//	body/query 里带一份别的门店就能看别人的库存 ⇒ 直接 400 branch_mismatch。
//
// # 错误映射
//
//	ErrStockLevelNotFound → 404 stock_level_not_found
//	ErrInvalidInput       → 400 bad_request
//	其它                  → 500 internal_error
package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/YunBright/authkit/rbac"
	"github.com/YunBright/authkit/userinfo"
	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/inventory/model"
	"github.com/YunBright/supertrade/internal/inventory/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// Handler 是 inventory 的 HTTP handler 聚合。
type Handler struct {
	stock  *service.StockService
	users  *userinfo.Client
	logger *slog.Logger
}

// New 构造 Handler。users 为 nil 时带 scope 守门的端点一律 503。
func New(stock *service.StockService, users *userinfo.Client, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{stock: stock, users: users, logger: logger}
}

// RegisterRoutes 挂载全部业务路由。
func (h *Handler) RegisterRoutes(r gin.IRouter) {
	resolver := rbac.HasAnyScopeWithBranch(h.users)

	r.GET("/stock-levels",
		rbac.RequireScopeWithBranch("inventory:view", branchFromHeader, resolver),
		h.listStockLevels)
	r.GET("/stock-levels/:product_id",
		rbac.RequireScopeWithBranch("inventory:view", branchFromHeader, resolver),
		h.getStockLevel)
	r.GET("/stock-moves",
		rbac.RequireScopeWithBranch("inventory:view", branchFromHeader, resolver),
		h.listStockMoves)

	// 2026-10-21 新增：库存快照的**导入**（一次性归档同步用）。
	//
	// ⚠️ 守门是 `inventory:manage`（不是 view）—— 导入会**改写**库存数量。
	// ⚠️ 目标门店只从 X-Branch-ID header 取（body 里带门店就能跨店写）。
	r.POST("/archive/import",
		rbac.RequireScopeWithBranch("inventory:manage", branchFromHeader, resolver),
		h.importArchive)
}

// branchFromHeader 取 X-Branch-ID(单店)。
func branchFromHeader(c *gin.Context) (string, bool) {
	b := middleware.SingleBranchFromCtx(c)
	if b == "" {
		return "", false
	}
	return b, true
}

// resolveBranch 定下本次请求作用的门店。
//
// ⚠️ **query 的 branch_id 只允许与 header 一致**:
// header 缺失 → 400 branch_required;两者不一致 → 400 branch_mismatch。
//
//	不一致就放行的话,body/query 里带一份别的门店就能看别人的库存。
func resolveBranch(c *gin.Context) (string, bool) {
	header := strings.TrimSpace(middleware.SingleBranchFromCtx(c))
	if header == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "branch_required",
			"message": "X-Branch-ID header 必填",
		})
		return "", false
	}
	q := strings.TrimSpace(c.Query("branch_id"))
	if q != "" && q != header {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "branch_mismatch",
			"message": "branch_id 与 X-Branch-ID 不一致",
		})
		return "", false
	}
	return header, true
}

// listStockLevels GET /stock-levels
func (h *Handler) listStockLevels(c *gin.Context) {
	branchID, ok := resolveBranch(c)
	if !ok {
		return
	}
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	rows, total, err := h.stock.ListLevels(c.Request.Context(), service.StockLevelQuery{
		BranchID:  branchID,
		ProductID: strings.TrimSpace(c.Query("product_id")),
		Keyword:   strings.TrimSpace(c.Query("keyword")),
		Page:      page,
		PageSize:  pageSize,
	})
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"stock_levels": rows,
		"count":        len(rows),
		"total":        total,
		"page":         page,
		"page_size":    pageSize,
	})
}

// getStockLevel GET /stock-levels/:product_id
func (h *Handler) getStockLevel(c *gin.Context) {
	branchID, ok := resolveBranch(c)
	if !ok {
		return
	}
	productID := strings.TrimSpace(c.Param("product_id"))
	if productID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "bad_request",
			"message": "product_id 必填",
		})
		return
	}
	row, err := h.stock.GetLevel(c.Request.Context(), branchID, productID)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, row)
}

// listStockMoves GET /stock-moves
func (h *Handler) listStockMoves(c *gin.Context) {
	branchID, ok := resolveBranch(c)
	if !ok {
		return
	}
	from, err := parseTimeParam(c.Query("occurred_from"))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "bad_request",
			"message": "occurred_from 非法: " + err.Error(),
		})
		return
	}
	to, err := parseTimeParam(c.Query("occurred_to"))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "bad_request",
			"message": "occurred_to 非法: " + err.Error(),
		})
		return
	}
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	rows, total, err := h.stock.ListMoves(c.Request.Context(), service.StockMoveQuery{
		BranchID:     branchID,
		ProductID:    strings.TrimSpace(c.Query("product_id")),
		MoveType:     strings.TrimSpace(c.Query("move_type")),
		OccurredFrom: from,
		OccurredTo:   to,
		Page:         page,
		PageSize:     pageSize,
	})
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"stock_moves": rows,
		"count":       len(rows),
		"total":       total,
		"page":        page,
		"page_size":   pageSize,
	})
}

// ---- 错误信封 ----

// errBody 是统一的错误响应体。
type errBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// mapErr 把 service 错误映射为 HTTP 状态码 + {code, message}。
func (h *Handler) mapErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, model.ErrStockLevelNotFound):
		c.AbortWithStatusJSON(http.StatusNotFound, errBody{
			Code:    "stock_level_not_found",
			Message: err.Error(),
		})
	case errors.Is(err, model.ErrInvalidInput):
		c.AbortWithStatusJSON(http.StatusBadRequest, errBody{
			Code:    "bad_request",
			Message: err.Error(),
		})
	default:
		h.logger.Error("inventory handler error", "err", err, "path", c.Request.URL.Path)
		c.AbortWithStatusJSON(http.StatusInternalServerError, errBody{
			Code:    "internal_error",
			Message: "系统繁忙,请稍后重试",
		})
	}
}

// parseTimeParam 解析时间参数:接受 RFC3339 或 "2006-01-02"。
//
// 空串 → (nil, nil)(不限)。
func parseTimeParam(s string) (*time.Time, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return nil, nil
	}
	if parsed, err := time.Parse(time.RFC3339, t); err == nil {
		return &parsed, nil
	}
	parsed, err := time.Parse("2006-01-02", t)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}
