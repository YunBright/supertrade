// Package handler 实现 cash-recon 服务的 HTTP handler(Gin)。
//
// 端点(nginx 已剥过 /api/v1/cash-recon/):
//
//	GET  /batches                    批次列表        scope recon:view
//	GET  /batches/:id                批次详情        scope recon:view
//	POST /batches/preview            dry-run 匹配    scope recon:operate
//
// # 鉴权链路
//
//	rbac.RequireScopeWithBranch(scope, branchFromHeader, resolver)
//	  - branchFn 取 X-Branch-ID(缺 → 400 branch_required)
//	  - claims 缺失 → 401 unauthenticated
//	  - resolver 报错(userd 不可用) → 503 userd_unavailable
//	  - 该 branch 下无此 scope → 403 forbidden
//
// scope 清单(recon:view / operate / review / audit / export)由 auth migration 006 落库。
package handler

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/YunBright/authkit/claims"
	"github.com/YunBright/authkit/rbac"
	"github.com/YunBright/authkit/userinfo"
	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/cashrecon/model"
	"github.com/YunBright/supertrade/internal/cashrecon/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// 端点使用的 scope。
const (
	// ScopeView 是读端点(批次/差异/日报/台账)。
	ScopeView = "recon:view"
	// ScopeOperate 是日常操作(导入、生成批次、实盘、其他收款、付款登记)。
	ScopeOperate = "recon:operate"
	// ScopeReview 是差异人工动作(复核)。
	ScopeReview = "recon:review"
	// ScopeAudit 是审核结算/冻结/账户与期初维护。
	ScopeAudit = "recon:audit"
	// ScopeExport 是导出(含敏感信息的 CSV)。
	ScopeExport = "recon:export"
)

// Handler 是 cash-recon 的 HTTP handler 聚合。
type Handler struct {
	svc *service.Service
	// users 是 userd 客户端(per-branch effective scopes 校验)。
	// nil ⇒ 带 scope 守门的端点一律 503(fail-closed)。
	users *userinfo.Client
}

// New 构造 handler。
func New(svc *service.Service, users *userinfo.Client) *Handler {
	return &Handler{svc: svc, users: users}
}

// branchFromHeader 取目标 branch_id。
func branchFromHeader(c *gin.Context) (string, bool) {
	b := middleware.SingleBranchFromCtx(c)
	if b == "" {
		return "", false
	}
	return b, true
}

// RegisterRoutes 注册业务路由。
//
// X-Branch-ID 由 cmd main 的 middleware.XBranchID() 注入 ctx;
// 这里每路由挂 RequireBranch 拦截缺失。
func (h *Handler) RegisterRoutes(r gin.IRouter) {
	requireBranch := middleware.RequireBranch()
	resolver := rbac.HasAnyScopeWithBranch(h.users)

	r.POST("/batches/preview",
		requireBranch,
		rbac.RequireScopeWithBranch(ScopeOperate, branchFromHeader, resolver),
		h.PreviewBatch)
	r.GET("/batches",
		requireBranch,
		rbac.RequireScopeWithBranch(ScopeView, branchFromHeader, resolver),
		h.ListBatches)
	r.GET("/batches/:id",
		requireBranch,
		rbac.RequireScopeWithBranch(ScopeView, branchFromHeader, resolver),
		h.GetBatch)
}

// errBody 是错误响应体(与 settlement-auditor 同形:code + message)。
type errBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, errBody{Code: code, Message: msg})
}

// mapErr 把 service 错误映成 HTTP 状态码。
//
// ⚠️ 期初缺失 / 实盘缺失一律 **409 且 body 带 code**:
// 前端靠 code 跳"期初建账"页 —— 500 会让出纳以为系统坏了,
// 而实际只是"还没维护期初"。
func mapErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, model.ErrRangeTooLarge):
		writeError(c, http.StatusBadRequest, "range_too_large", err.Error())
	case errors.Is(err, model.ErrOpeningItemsIncomplete):
		writeError(c, http.StatusBadRequest, "opening_items_incomplete", err.Error())
	case errors.Is(err, model.ErrInvalidInput):
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, model.ErrNotFound):
		writeError(c, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, model.ErrPeriodFrozen):
		writeError(c, http.StatusConflict, "period_frozen", err.Error())
	case errors.Is(err, model.ErrOpeningBalanceMissing):
		writeError(c, http.StatusConflict, "opening_balance_missing", err.Error())
	case errors.Is(err, model.ErrCashCountMissing):
		writeError(c, http.StatusConflict, "cash_count_missing", err.Error())
	case errors.Is(err, model.ErrCubeSourceUnavailable):
		writeError(c, http.StatusServiceUnavailable, "cube_source_unavailable", err.Error())
	case errors.Is(err, model.ErrCubeQueryFailed):
		writeError(c, http.StatusServiceUnavailable, "cube_query_failed", err.Error())
	default:
		writeError(c, http.StatusInternalServerError, "internal_error", err.Error())
	}
}

// branchIDOf 取本次请求的门店 id(X-Branch-ID 由中间件注入 ctx)。
func branchIDOf(c *gin.Context) string {
	if v := middleware.SingleBranchFromCtx(c); v != "" {
		return v
	}
	return strings.TrimSpace(c.GetHeader("X-Branch-ID"))
}

// operatorOf 取操作人 auth user id。
//
// ⚠️ 取 claims.Sub —— **不信任客户端自称的 operator**,否则审计字段等于谁都能填。
func operatorOf(c *gin.Context) string {
	cl, _ := claims.FromContext(c.Request.Context())
	if cl == nil {
		return ""
	}
	return cl.Sub
}

// previewReq 是 dry-run 请求体。
type previewReq struct {
	BizDateFrom string `json:"biz_date_from"`
	BizDateTo   string `json:"biz_date_to"`
}

// previewResp 是 dry-run 响应体。
//
// 金额一律**字符串**(十进制精确值);不用 float —— 0.01 的浮点误差
// 会让"汇总=明细"的断言偶发失败,而这类失败最容易被当成"数据错"。
type previewResp struct {
	DryRun  bool           `json:"dry_run"`
	Summary previewSummary `json:"summary"`
	Matches []any          `json:"matches"`
	Diffs   []any          `json:"diffs"`
}

type previewSummary struct {
	MatchRate     string `json:"match_rate"`
	DiffCount     int    `json:"diff_count"`
	CashTotal     string `json:"cash_total"`
	BankTotal     string `json:"bank_total"`
	FeeTotal      string `json:"fee_total"`
	MatchedCount  int    `json:"matched_count"`
	EligibleCount int    `json:"eligible_count"`
}

// PreviewBatch POST /batches/preview
//
// **dry-run,不落库**。S0 无收银/银行流水接入时返回全零 summary。
func (h *Handler) PreviewBatch(c *gin.Context) {
	var req previewReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体必须是 JSON: {biz_date_from,biz_date_to}")
		return
	}
	from, err := time.Parse("2006-01-02", req.BizDateFrom)
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "biz_date_from 必须是 YYYY-MM-DD")
		return
	}
	to, err := time.Parse("2006-01-02", req.BizDateTo)
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "biz_date_to 必须是 YYYY-MM-DD")
		return
	}
	out, err := h.svc.Preview(c.Request.Context(), service.PreviewInput{
		BranchID:    branchIDOf(c),
		BizDateFrom: from,
		BizDateTo:   to,
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, previewResp{
		DryRun: true,
		Summary: previewSummary{
			MatchRate:     out.Summary.MatchRate.String(),
			DiffCount:     out.Summary.DiffCount,
			CashTotal:     out.Summary.CashTotal.String(),
			BankTotal:     out.Summary.BankTotal.String(),
			FeeTotal:      out.Summary.FeeTotal.String(),
			MatchedCount:  out.Summary.MatchedCount,
			EligibleCount: out.Summary.EligibleCount,
		},
		Matches: []any{},
		Diffs:   []any{},
	})
}

// ListBatches GET /batches
func (h *Handler) ListBatches(c *gin.Context) {
	rows, err := h.svc.ListBatches(c.Request.Context(), branchIDOf(c), 0)
	if err != nil {
		mapErr(c, err)
		return
	}
	if rows == nil {
		rows = []model.ReconBatch{}
	}
	c.JSON(http.StatusOK, gin.H{"items": rows, "total": len(rows)})
}

// GetBatch GET /batches/:id
func (h *Handler) GetBatch(c *gin.Context) {
	row, err := h.svc.GetBatch(c.Request.Context(), branchIDOf(c), c.Param("id"))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, row)
}
