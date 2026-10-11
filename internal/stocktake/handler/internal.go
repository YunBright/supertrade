// Package handler / internal.go —— S2S 内部端点(不挂 nginx、不走 JWT)。
//
//	POST /internal/stocktake-plan-headers —— 建计划盘点单(Type=plan)+ 计划商品。
//
// 调用方:settlement-auditor(结算付款审批机器审核建议盘点时,dapr invoke)。
// 保护方式:仅在 RegisterDapr 窗口注册(auth 中间件之前),靠两道防线:
//  1. nginx `location ^~ /api/v1/stocktake/internal/ { return 404; }`
//     (deployer/nginx/yun-bright.conf)—— 公网路径直接 404;
//  2. dapr 内网 invoke 不经 nginx,sidecar mTLS + 内网边界。
// 与 auth-internal 的 /internal/users/* 不同范式(那套走 sidecar bearer-auth)。
package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/stocktake/model"
	"github.com/YunBright/supertrade/internal/stocktake/service"
)

// CreatePlanHeaderRequest 建计划盘点单入参(settlement-auditor wire 契约)。
type CreatePlanHeaderRequest struct {
	BranchID   string   `json:"branch_id" binding:"required"`
	OperatorID string   `json:"operator_id" binding:"required"`
	Remark     string   `json:"remark"`
	ProductIDs []string `json:"product_ids"`
}

// CreatePlanHeaderResponse 建单结果。
type CreatePlanHeaderResponse struct {
	ID       string `json:"id"`
	BranchID string `json:"branch_id"`
	Type     string `json:"type"`
	Status   string `json:"status"`
}

// RegisterInternalRoutes 挂 S2S 内部端点。
//
// 必须由 cmd/stocktake/main.go 在 registerDaprRoutes(auth 中间件之前)调用 —
// 内部调用不带 Authorization 头,挂上鉴权中间件就会 401。
func (h *Handler) RegisterInternalRoutes(r *gin.Engine) {
	r.POST("/internal/stocktake-plan-headers", h.CreatePlanHeaderInternal)
}

// CreatePlanHeaderInternal POST /internal/stocktake-plan-headers
//
// 行为:CreateHeader(Type=plan) → AddPlanItems(product_ids,缺省名称由服务端
// 从 cube 现拉)。product_ids 为空合法(supplier/category scope 的计划由盘点人
// 自行圈品);重复 product_id → 400 duplicated。
func (h *Handler) CreatePlanHeaderInternal(c *gin.Context) {
	var req CreatePlanHeaderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体非法: "+err.Error())
		return
	}
	header, err := h.svc.CreateHeader(c.Request.Context(), service.CreateHeaderInput{
		BranchID:   req.BranchID,
		Type:       model.TypePlan,
		OperatorID: req.OperatorID,
		Remark:     req.Remark,
	})
	if err != nil {
		h.mapInternalErr(c, err)
		return
	}
	if len(req.ProductIDs) > 0 {
		in := service.AddPlanItemsInput{
			Items: make([]service.PlanItemInput, 0, len(req.ProductIDs)),
		}
		for i, pid := range req.ProductIDs {
			in.Items = append(in.Items, service.PlanItemInput{ProductID: pid, SortOrder: i})
		}
		if _, err := h.svc.AddPlanItems(c.Request.Context(), header.ID, in); err != nil {
			h.mapInternalErr(c, err)
			return
		}
	}
	c.JSON(http.StatusCreated, CreatePlanHeaderResponse{
		ID:       header.ID,
		BranchID: header.BranchID,
		Type:     string(header.Type),
		Status:   string(header.Status),
	})
}

// mapInternalErr 内部端点错误映射(复用 service 错误,保持 400/404/500 语义)。
func (h *Handler) mapInternalErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrHeaderNotFound):
		writeError(c, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, service.ErrInvalidStatus),
		errors.Is(err, service.ErrPlanItemDuplicated),
		errors.Is(err, service.ErrRecheckRequiresParent):
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
	default:
		writeError(c, http.StatusInternalServerError, "internal_error", err.Error())
	}
}
