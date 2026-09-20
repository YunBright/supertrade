// Package handler 实现 stocktake 服务的 HTTP handler(Gin)。
//
// 路径与端点对应 REQUIREMENTS §2.1 / DESIGN §4.5.1:
//
//	POST   /stocktake-headers
//	GET    /stocktake-headers/:id
//	POST   /stocktake-headers/:id/lines
//	PUT    /stocktake-lines/:id
//	DELETE /stocktake-lines/:id
//	GET    /stocktake-headers/:id/diff-report
//	POST   /stocktake-headers/:id/submit
//	POST   /stocktake-headers/:id/approve
//
// 错误映射:
//
//	ErrHeaderNotFound / ErrLineNotFound → 404
//	ErrInvalidStatus / ErrInvalidTransition → 400
//	ErrCubeUnavailable → 503
//	ErrProductNotFound / ErrStockNotFound → 400(数据问题)
//	其它 → 500
package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/YunBright/authkit/claims"
	"github.com/YunBright/supertrade/internal/stocktake/model"
	"github.com/YunBright/supertrade/internal/stocktake/service"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

// Handler 是 stocktake 服务的 HTTP handler 聚合。
type Handler struct {
	svc *service.Service
}

// New 构造 handler。
func New(svc *service.Service) *Handler {
	return &Handler{svc: svc}
}

// RegisterRoutes 把所有 stocktake 路由注册到指定 router group。
//
// 不接管 /healthz(由 cmdbootstrap 注入)。
//
// 盘点单 H5 新增端点(2026-09-19):
//   - GET  /stocktake-headers                 列表 + 分页
//   - GET  /stocktake-headers/:id/history      操作历史
//   - GET  /stocktake-headers/:id/plan-items   计划盘点商品
//   - POST /stocktake-headers/:id/plan-items   批量加计划商品
func (h *Handler) RegisterRoutes(r gin.IRouter) {
	r.POST("/stocktake-headers", h.CreateHeader)
	r.GET("/stocktake-headers", h.ListHeaders)
	r.GET("/stocktake-headers/:id", h.GetHeader)
	r.POST("/stocktake-headers/:id/lines", h.AddLine)
	r.GET("/stocktake-headers/:id/diff-report", h.DiffReport)
	r.GET("/stocktake-headers/:id/history", h.ListHistory)
	r.GET("/stocktake-headers/:id/plan-items", h.GetPlanItems)
	r.POST("/stocktake-headers/:id/plan-items", h.AddPlanItems)
	r.POST("/stocktake-headers/:id/submit", h.Submit)
	r.POST("/stocktake-headers/:id/approve", h.Approve)

	r.PUT("/stocktake-lines/:id", h.UpdateLine)
	r.DELETE("/stocktake-lines/:id", h.DeleteLine)

	// 商品搜索 / 扫条码(对齐 scan.html 后端 SearchProducts,REQUIREMENTS §2.1.4.1)
	r.GET("/products/search", h.SearchProducts)
}

// ---- 错误响应 ----

type errBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, errBody{Code: code, Message: msg})
}

// mapErr 把 service 错误映射为 HTTP 状态码。
func mapErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrHeaderNotFound),
		errors.Is(err, service.ErrLineNotFound),
		errors.Is(err, service.ErrPlanItemNotFound):
		writeError(c, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, service.ErrInvalidStatus),
		errors.Is(err, service.ErrInvalidTransition),
		errors.Is(err, service.ErrProductNotFound),
		errors.Is(err, service.ErrStockNotFound),
		errors.Is(err, service.ErrPlanItemDuplicated),
		errors.Is(err, service.ErrRecheckRequiresParent),
		errors.Is(err, service.ErrInvalidOpType):
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, service.ErrCubeUnavailable):
		writeError(c, http.StatusServiceUnavailable, "cube_unavailable", err.Error())
	default:
		writeError(c, http.StatusInternalServerError, "internal_error", err.Error())
	}
}

// ---- 请求 / 响应 DTO ----

type createHeaderReq struct {
	BranchID       string `json:"branch_id" binding:"required"`
	CountDate      string `json:"count_date"` // YYYY-MM-DD;为空用 today
	Type           string `json:"type"`
	Remark         string `json:"remark"`
	ParentHeaderID string `json:"parent_header_id"` // type=recheck 时必填
}

type addLineReq struct {
	ProductID  string          `json:"product_id" binding:"required"`
	ActualQty  decimal.Decimal `json:"actual_qty" binding:"required"`
	DiffReason string          `json:"diff_reason"`
	Remark     string          `json:"remark"`
	OpType     string          `json:"op_type"`    // create/overwrite/accumulate;default=create
	Method     string          `json:"method"`     // scan/manual/import;default=manual
	ActorName  string          `json:"actor_name"` // 冗余写入 StocktakeLineOperation
}

type updateLineReq struct {
	ActualQty  *decimal.Decimal `json:"actual_qty"`
	DiffReason *string          `json:"diff_reason"`
	Remark     *string          `json:"remark"`
	OpType     string           `json:"op_type"` // overwrite/accumulate/create;default=overwrite
	Method     string           `json:"method"`  // scan/manual/import;default=manual
	ActorName  string           `json:"actor_name"`
}

type approveReq struct {
	AuditorID string `json:"auditor_id" binding:"required"`
}

type deleteLineReq struct {
	Method    string `json:"method"`     // scan/manual/import;default=manual
	ActorName string `json:"actor_name"` // 冗余写入 StocktakeLineOperation
}

// addPlanItemsReq POST /stocktake-headers/:id/plan-items
type addPlanItemsReq struct {
	Items []struct {
		ProductID   string `json:"product_id" binding:"required"`
		ProductName string `json:"product_name"`
		Unit        string `json:"unit"`
		Barcode     string `json:"barcode"`
		SortOrder   int    `json:"sort_order"`
	} `json:"items"`
}

// ---- Handlers ----

// CreateHeader POST /stocktake-headers
func (h *Handler) CreateHeader(c *gin.Context) {
	var req createHeaderReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	cl := claims.MustFromContext(c.Request.Context())

	tt := model.StocktakeType(req.Type)
	if tt == "" {
		tt = model.TypeGeneral
	}

	var countDate time.Time
	if req.CountDate != "" {
		t, err := time.Parse("2006-01-02", req.CountDate)
		if err != nil {
			writeError(c, http.StatusBadRequest, "bad_count_date", "count_date 必须 YYYY-MM-DD")
			return
		}
		countDate = t
	}

	hdr, err := h.svc.CreateHeader(c.Request.Context(), service.CreateHeaderInput{
		BranchID:       req.BranchID,
		CountDate:      countDate,
		Type:           tt,
		OperatorID:     cl.Sub,
		Remark:         req.Remark,
		ParentHeaderID: req.ParentHeaderID,
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, hdr)
}

// ListHeaders GET /stocktake-headers
//
// 过滤:branch_id / status / type / operator_id / count_date(YYYY-MM-DD)
// 分页:page (default 1) / page_size (default 20, max 100)
func (h *Handler) ListHeaders(c *gin.Context) {
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	out, err := h.svc.ListHeaders(c.Request.Context(), service.ListHeadersFilter{
		BranchID:   c.Query("branch_id"),
		Status:     model.StocktakeStatus(c.Query("status")),
		Type:       model.StocktakeType(c.Query("type")),
		OperatorID: c.Query("operator_id"),
		CountDate:  c.Query("count_date"),
	}, page, pageSize)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// GetHeader GET /stocktake-headers/:id
func (h *Handler) GetHeader(c *gin.Context) {
	id := c.Param("id")
	hdr, err := h.svc.GetHeaderWithLines(c.Request.Context(), id)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, hdr)
}

// ListHistory GET /stocktake-headers/:id/history?limit=50
func (h *Handler) ListHistory(c *gin.Context) {
	headerID := c.Param("id")
	limit, _ := strconv.Atoi(c.Query("limit"))
	ops, err := h.svc.ListLineOperations(c.Request.Context(), headerID, limit)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"operations": ops})
}

// GetPlanItems GET /stocktake-headers/:id/plan-items
func (h *Handler) GetPlanItems(c *gin.Context) {
	headerID := c.Param("id")
	items, err := h.svc.GetPlanItems(c.Request.Context(), headerID)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// AddPlanItems POST /stocktake-headers/:id/plan-items
func (h *Handler) AddPlanItems(c *gin.Context) {
	headerID := c.Param("id")
	var req addPlanItemsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	itemsIn := make([]service.PlanItemInput, 0, len(req.Items))
	for _, it := range req.Items {
		itemsIn = append(itemsIn, service.PlanItemInput{
			ProductID:   it.ProductID,
			ProductName: it.ProductName,
			Unit:        it.Unit,
			Barcode:     it.Barcode,
			SortOrder:   it.SortOrder,
		})
	}
	items, err := h.svc.AddPlanItems(c.Request.Context(), headerID, service.AddPlanItemsInput{Items: itemsIn})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"items": items})
}

// AddLine POST /stocktake-headers/:id/lines
func (h *Handler) AddLine(c *gin.Context) {
	headerID := c.Param("id")
	var req addLineReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	cl, _ := claims.FromContext(c.Request.Context())
	line, err := h.svc.AddLine(c.Request.Context(), headerID, service.AddLineInput{
		ProductID:  req.ProductID,
		ActualQty:  req.ActualQty,
		DiffReason: model.DiffReason(req.DiffReason),
		Remark:     req.Remark,
		OpType:     model.LineOpType(req.OpType),
		Method:     model.OpMethod(req.Method),
		ActorID:    actorIDFromClaims(cl),
		ActorName:  defaultActorName(cl, req.ActorName),
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, line)
}

// UpdateLine PUT /stocktake-lines/:id
func (h *Handler) UpdateLine(c *gin.Context) {
	id := c.Param("id")
	var req updateLineReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	cl, _ := claims.FromContext(c.Request.Context())
	in := service.UpdateLineInput{
		ActualQty: req.ActualQty,
		Remark:    req.Remark,
		OpType:    model.LineOpType(req.OpType),
		Method:    model.OpMethod(req.Method),
		ActorID:   actorIDFromClaims(cl),
		ActorName: defaultActorName(cl, req.ActorName),
	}
	if req.DiffReason != nil {
		dr := model.DiffReason(*req.DiffReason)
		in.DiffReason = &dr
	}
	line, err := h.svc.UpdateLine(c.Request.Context(), id, in)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, line)
}

// DeleteLine DELETE /stocktake-lines/:id
//
// 可选 body:{actor_name?, method?};不传则从 claims 推断。
func (h *Handler) DeleteLine(c *gin.Context) {
	id := c.Param("id")
	var req deleteLineReq
	_ = c.ShouldBindJSON(&req) // body 可选,空 body 不报错
	cl, _ := claims.FromContext(c.Request.Context())
	method := model.OpMethod(req.Method)
	if method == "" {
		method = model.MethodManual
	}
	if err := h.svc.DeleteLine(c.Request.Context(), id,
		actorIDFromClaims(cl),
		defaultActorName(cl, req.ActorName),
		method); err != nil {
		mapErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// DiffReport GET /stocktake-headers/:id/diff-report
func (h *Handler) DiffReport(c *gin.Context) {
	id := c.Param("id")
	rep, err := h.svc.ComputeDiffReport(c.Request.Context(), id)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, rep)
}

// Submit POST /stocktake-headers/:id/submit
func (h *Handler) Submit(c *gin.Context) {
	id := c.Param("id")
	hdr, err := h.svc.Submit(c.Request.Context(), id)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, hdr)
}

// Approve POST /stocktake-headers/:id/approve
func (h *Handler) Approve(c *gin.Context) {
	id := c.Param("id")
	var req approveReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	hdr, err := h.svc.Approve(c.Request.Context(), id, req.AuditorID)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, hdr)
}

// SearchProducts GET /api/v1/products/search
//
// 跟 collect-ai SearchProducts 契约对齐(REQUIREMENTS §2.1.4.1):
//   - ?barcode=xxx   必填
//   - ?branch_id=xxx 必填(用于合并 stock_qty;默认取自 claims.BranchID,前端也可覆盖)
//   - ?limit=N       可选,默认 10
//
// 权限过滤(读自 claims.Scopes):
//   - inventory:view  → 返 stock_qty / avg_cost_yuan
//   - supplier:view   → 返 supplier_id / supplier_name
func (h *Handler) SearchProducts(c *gin.Context) {
	barcode := strings.TrimSpace(c.Query("barcode"))
	if barcode == "" {
		writeError(c, http.StatusBadRequest, "missing_barcode", "?barcode= 必填")
		return
	}
	branchID := strings.TrimSpace(c.Query("branch_id"))
	if branchID == "" {
		// 默认取自 JWT claims
		cl, ok := claims.FromContext(c.Request.Context())
		if !ok || cl.BranchID == "" {
			writeError(c, http.StatusBadRequest, "missing_branch_id",
				"?branch_id= 必填,或 JWT claims 包含 branch_id")
			return
		}
		branchID = cl.BranchID
	}
	limit, _ := strconv.Atoi(c.Query("limit"))

	// 读 claims scopes
	cl, _ := claims.FromContext(c.Request.Context())
	invViewable := hasScope(cl, "inventory:view")
	supplierViewable := hasScope(cl, "supplier:view")

	out, err := h.svc.SearchProducts(c.Request.Context(), service.SearchProductsInput{
		Barcode:  barcode,
		BranchID: branchID,
		Limit:    limit,
	}, invViewable, supplierViewable)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// hasScope 检查 JWT claims 是否包含某 scope(空 claims → false)。
func hasScope(cl *claims.Claims, scope string) bool {
	if cl == nil {
		return false
	}
	return cl.HasScope(scope)
}

// actorIDFromClaims 拿 user id 作审计 actor;空 claims → "unknown"。
func actorIDFromClaims(cl *claims.Claims) string {
	if cl == nil || cl.Sub == "" {
		return "unknown"
	}
	return cl.Sub
}

// defaultActorName 优先用客户端传的 actor_name,否则从 claims.Sub,最后 "unknown"。
//
// authkit.Claims 当前没有 Name 字段(只有 Sub + Profile fields);将来若加,
// 这里改成 fallback chain:fallback > Name > Sub > "unknown"。
func defaultActorName(cl *claims.Claims, fallback string) string {
	if fallback != "" {
		return fallback
	}
	if cl != nil && cl.Sub != "" {
		return cl.Sub
	}
	return "unknown"
}
