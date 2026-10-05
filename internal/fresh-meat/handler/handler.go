// Package handler 实现 fresh-meat 服务的 HTTP handler(Gin)。
//
// 路径与端点对齐 docs/REQUIREMENTS.md §4 / docs/DESIGN.md §6:
//
//	POST   /whole-pigs                              早盘整猪录入
//	GET    /whole-pigs                              按日列整猪
//	GET    /whole-pigs/:pig_id                      单猪详情
//	POST   /pig-cuts                                单品补录
//	GET    /pigs/:pig_id/cuts                       单猪的所有 cuts
//	POST   /pork-cuts-stocktake                       日终按部位盘点
//	GET    /pork-cuts-stocktake/latest                某日最近一次盘点
//	GET    /pork-cuts-stocktake/:id                   按 ID 查盘点
//	POST   /waste-logs                              报损
//	GET    /line-sales-by-pig                       按 pig/cut/date 查
//	GET    /branch-cut-mappings                     门店 cut 部位映射列表
//	POST   /branch-cut-mappings                     新建映射
//	PUT    /branch-cut-mappings/:id                 改映射
//	DELETE /branch-cut-mappings/:id                 删映射
//
// 错误映射:
//
//	ErrWholePigNotFound / ErrPigCutNotFound / ErrPorkCutsStocktakeNotFound /
//	  ErrBranchCutMappingNotFound → 404 not_found
//	ErrInvalidInput / ErrBranchMismatch / ErrBranchCutMappingConflict → 400 bad_request
//	ErrCubeUnavailable → 503 cube_unavailable
//	ErrUserInfoUnavailable → 503 userd_unavailable
//	其它 → 500 internal_error
package handler

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/YunBright/authkit/claims"
	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/YunBright/supertrade/pkg/middleware"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

// Handler 是 fresh-meat 服务的 HTTP handler 聚合。
type Handler struct {
	svc *service.Service
}

// New 构造 handler。
func New(svc *service.Service) *Handler {
	return &Handler{svc: svc}
}

// RegisterRoutes 把所有 fresh-meat 路由注册到指定 router。
//
// 不接管 /healthz(由 cmdbootstrap 注入)。
//
// 路径全部从 root 起(nginx 已剥 /api/v1/fresh-meat/ 前缀)。
//
// 中间件链(2026-10 重构对齐 stocktake):
//
//	middleware.RequireBranch()           // 拦 X-Branch-ID 缺失 → 400 branch_required
//	h.requireScope(c, branchID, scope)  // 走 svc.HasEffectiveScope(60s 缓存)→ 401/503/403
//
// X-Branch-ID 全局由 cmd/fresh-meat/main.go::registerRoutes 的 middleware.XBranchID()
// 注入 ctx,本方法只挂 per-route 的 RequireBranch。
//
// 不挂中间件的位置:
//   - /cron/daily-reminder:dapr cron binding 调用,不走业务鉴权。
func (h *Handler) RegisterRoutes(r gin.IRouter) {
	requireBranch := middleware.RequireBranch()

	// 整猪
	r.POST("/whole-pigs", requireBranch, h.RecordWholePig)
	r.GET("/whole-pigs", requireBranch, h.ListWholePigsByDay)
	r.GET("/whole-pigs/:pig_id", requireBranch, h.GetWholePig)

	// 单品补录
	r.POST("/pig-cuts", requireBranch, h.RecordPigCut)
	r.GET("/pigs/:pig_id/cuts", requireBranch, h.ListPigCutsByPig)

	// 日终按部位盘点
	r.POST("/pork-cuts-stocktake", requireBranch, h.RecordPorkCutsStocktake)
	r.GET("/pork-cuts-stocktake/latest", requireBranch, h.GetLatestPorkCutsStocktake)
	r.GET("/pork-cuts-stocktake/:id", requireBranch, h.GetPorkCutsStocktake)

	// 报损
	r.POST("/waste-logs", requireBranch, h.RecordWasteLog)

	// 销售聚合
	r.GET("/line-sales-by-pig", requireBranch, h.ListLineSalesByPig)

	// 门店 cut 部位 ↔ cube SKU 映射(初始化 / 拓店时维护)
	r.GET("/branch-cut-mappings", requireBranch, h.ListBranchCutMappings)
	r.POST("/branch-cut-mappings", requireBranch, h.CreateBranchCutMapping)
	r.PUT("/branch-cut-mappings/:id", requireBranch, h.UpdateBranchCutMapping)
	r.DELETE("/branch-cut-mappings/:id", requireBranch, h.DeleteBranchCutMapping)

	// dapr cron binding 触发(deployer 仓加 bindings.cron.yaml)—
	// 走 dapr 自身 cron binding,无 JWT 无 X-Branch-ID;**不挂** requireBranch。
	r.GET("/cron/daily-reminder", h.CronDailyReminder)

	// 毛利查询(per cut + total;2026-10-01 新增)
	r.GET("/gross-margin", requireBranch, h.GetGrossMargin)
}

// ---- dapr cron binding 触发 ----

// CronDailyReminder GET /cron/daily-reminder
//
// dapr cron binding 配置示例(部署仓):
//
//	apiVersion: dapr.io/v1alpha1
//	kind: Component
//	metadata:
//	  name: fresh-meat-cron
//	spec:
//	  type: bindings.cron
//	version: v1
//	metadata:
//	  - name: schedule
//	    value: "@daily 23:00"
//	  - name: direction
//	    value: "input"
//	  - name: path
//	    value: "cron/daily-reminder"
//
// 本接口逻辑:
//   - 列出所有"今日未提交 is_complete=true 盘点"的 branch(扫描 record);
//
// 日终 22:00 推送仓管待办:早盘录入失败 / 销售 / 报损 / 库存不平衡等告警;
//
// 占位 — 真实清单生成待 BI / sales-agg 接入后实装;
// 本阶段只返 "skipped: all branches covered" 状态(便于 dapr 校验可达)。
func (h *Handler) CronDailyReminder(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"ok":      true,
		"note":    "占位;真实清单生成待 BI 接入后实装",
		"trigger": "dapr-cron",
	})
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
	case errors.Is(err, service.ErrWholePigNotFound),
		errors.Is(err, service.ErrPigCutNotFound),
		errors.Is(err, service.ErrPorkCutsStocktakeNotFound),
		errors.Is(err, service.ErrBranchCutMappingNotFound):
		writeError(c, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, service.ErrInvalidInput),
		errors.Is(err, service.ErrBranchMismatch),
		errors.Is(err, service.ErrBranchCutMappingConflict):
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, service.ErrCubeUnavailable):
		writeError(c, http.StatusServiceUnavailable, "cube_unavailable", err.Error())
	case errors.Is(err, service.ErrUserInfoUnavailable):
		writeError(c, http.StatusServiceUnavailable, "userd_unavailable", err.Error())
	default:
		writeError(c, http.StatusInternalServerError, "internal_error", err.Error())
	}
}

// requireScope 在 handler 顶部做 per-branch 守门。
//
// 决策矩阵:
//   - claims 缺失                    → 401 unauthenticated
//   - branchID 空                    → 400 branch_required
//   - svc.HasEffectiveScope 返 error → mapErr
//   - scope 不在该 branch 下         → 403 forbidden
//   - 命中                            → 返 true
func (h *Handler) requireScope(c *gin.Context, branchID, scope string) bool {
	cl, ok := claims.FromContext(c.Request.Context())
	if !ok || cl == nil {
		writeError(c, http.StatusUnauthorized, "unauthenticated", "缺少已签 token")
		return false
	}
	if branchID == "" {
		writeError(c, http.StatusBadRequest, "branch_required", "branch_id 必填")
		return false
	}
	allowed, err := h.svc.HasEffectiveScope(c.Request.Context(), cl.Sub, branchID, scope)
	if err != nil {
		mapErr(c, err)
		return false
	}
	if !allowed {
		writeError(c, http.StatusForbidden, "forbidden",
			"用户在该 branch 下无 "+scope+" scope")
		return false
	}
	return true
}

func actorIDFromClaims(cl *claims.Claims) string {
	if cl == nil || cl.Sub == "" {
		return "unknown"
	}
	return cl.Sub
}

// ---- 整猪 ----

// RecordWholePig POST /whole-pigs
//
// 仓管早盘录入(一头一行)+ LLM 同步返回预期分割建议。
//
// 权限:freshmeat:write;branch 从 X-Branch-ID header 取。
func (h *Handler) RecordWholePig(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	var in model.RecordWholePigInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	cl, _ := claims.FromContext(c.Request.Context())
	out, err := h.svc.RecordWholePig(c.Request.Context(), branchID, in, actorIDFromClaims(cl))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// GetWholePig GET /whole-pigs/:pig_id
//
// 查单猪。权限:freshmeat:view。
func (h *Handler) GetWholePig(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	pigID := strings.TrimSpace(c.Param("pig_id"))
	if pigID == "" {
		writeError(c, http.StatusBadRequest, "missing_pig_id", "pig_id 必填")
		return
	}
	out, err := h.svc.GetWholePig(c.Request.Context(), branchID, pigID)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// ListWholePigsByDay GET /whole-pigs?date=YYYY-MM-DD
//
// 查某店某日所有整猪(默认今日 UTC)。
func (h *Handler) ListWholePigsByDay(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	day, err := h.parseDay(c.Query("date"))
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_date", err.Error())
		return
	}
	out, err := h.svc.ListWholePigsByDay(c.Request.Context(), branchID, day)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// ---- pig_cuts ----

// RecordPigCut POST /pig-cuts
//
// 单品补录(扫码补打部位条码)。权限:freshmeat:write。
func (h *Handler) RecordPigCut(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	var in model.RecordPigCutInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	cl, _ := claims.FromContext(c.Request.Context())
	out, err := h.svc.RecordPigCut(c.Request.Context(), branchID, in, actorIDFromClaims(cl))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// ListPigCutsByPig GET /pigs/:pig_id/cuts
func (h *Handler) ListPigCutsByPig(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	pigID := strings.TrimSpace(c.Param("pig_id"))
	if pigID == "" {
		writeError(c, http.StatusBadRequest, "missing_pig_id", "pig_id 必填")
		return
	}
	out, err := h.svc.ListPigCutsByPig(c.Request.Context(), branchID, pigID)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// ---- pork_cuts_stocktake ----

// RecordPorkCutsStocktake POST /pork-cuts-stocktake
//
// 日终按部位盘点(整店;可选,不阻断销售)。
// is_complete=true 时 publish pork.cuts.stocktaken。
func (h *Handler) RecordPorkCutsStocktake(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	var in model.RecordPorkCutsStocktakeInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	cl, _ := claims.FromContext(c.Request.Context())
	out, err := h.svc.RecordPorkCutsStocktake(c.Request.Context(), branchID, in, actorIDFromClaims(cl))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// GetLatestPorkCutsStocktake GET /pork-cuts-stocktake/latest?date=YYYY-MM-DD
func (h *Handler) GetLatestPorkCutsStocktake(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	day, err := h.parseDay(c.Query("date"))
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_date", err.Error())
		return
	}
	out, err := h.svc.GetLatestPorkCutsStocktake(c.Request.Context(), branchID, day)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// GetGrossMargin GET /gross-margin?date=YYYY-MM-DD
//
// 返回 per-cut + total 毛利;data_source ∈ {actual, partial, estimated}。
// 当日无 stocktake → estimated;partial → partial;is_complete=true → actual。
func (h *Handler) GetGrossMargin(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	day, err := h.parseDay(c.Query("date"))
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_date", err.Error())
		return
	}
	out, err := h.svc.ComputeGrossMargin(c.Request.Context(), branchID, day)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// GetPorkCutsStocktake GET /pork-cuts-stocktake/:id
func (h *Handler) GetPorkCutsStocktake(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		writeError(c, http.StatusBadRequest, "missing_stocktake_id", "id 必填")
		return
	}
	out, err := h.svc.GetPorkCutsStocktake(c.Request.Context(), branchID, id)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// ---- waste_log ----

// RecordWasteLog POST /waste-logs
func (h *Handler) RecordWasteLog(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	var in model.RecordWasteLogInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	cl, _ := claims.FromContext(c.Request.Context())
	out, err := h.svc.RecordWasteLog(c.Request.Context(), branchID, in, actorIDFromClaims(cl))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// ---- line_sales_by_pig ----

// ListLineSalesByPig GET /line-sales-by-pig?pig_id=&cut_type=&from=&to=
func (h *Handler) ListLineSalesByPig(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	pigID := strings.TrimSpace(c.Query("pig_id"))
	if pigID == "" {
		writeError(c, http.StatusBadRequest, "missing_pig_id", "?pig_id= 必填")
		return
	}
	var cutPtr *model.CutType
	if ct := strings.TrimSpace(c.Query("cut_type")); ct != "" {
		c := model.CutType(ct)
		cutPtr = &c
	}
	out, err := h.svc.ListLineSalesByPig(c.Request.Context(), branchID, pigID, cutPtr, time.Time{}, time.Time{})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// ---- branch_cut_mappings ----

// ListBranchCutMappings GET /branch-cut-mappings
func (h *Handler) ListBranchCutMappings(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	out, err := h.svc.ListBranchCutMappings(c.Request.Context(), branchID)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// CreateBranchCutMapping POST /branch-cut-mappings
func (h *Handler) CreateBranchCutMapping(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	var in model.CreateBranchCutMappingInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	out, err := h.svc.CreateBranchCutMapping(c.Request.Context(), branchID, in)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// UpdateBranchCutMapping PUT /branch-cut-mappings/:id
func (h *Handler) UpdateBranchCutMapping(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		writeError(c, http.StatusBadRequest, "missing_mapping_id", "id 必填")
		return
	}
	var in model.UpdateBranchCutMappingInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	out, err := h.svc.UpdateBranchCutMapping(c.Request.Context(), branchID, id, in)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// DeleteBranchCutMapping DELETE /branch-cut-mappings/:id
func (h *Handler) DeleteBranchCutMapping(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		writeError(c, http.StatusBadRequest, "missing_mapping_id", "id 必填")
		return
	}
	if err := h.svc.DeleteBranchCutMapping(c.Request.Context(), branchID, id); err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- 工具 ----

// parseDay 解析 ?date=YYYY-MM-DD(默认今日,按 service.bizTZ 时区解析)。
//
// 用 bizTZ 而非 UTC:仓管在 Asia/Shanghai 下查"2026-10-01"应是 2026-10-01T00:00:00+08:00,
// 而非 2026-10-01T00:00:00Z(后者在 bizTZ 下换算是 09-30)。
func (h *Handler) parseDay(s string) (time.Time, error) {
	loc := h.svc.BizTZ()
	if s == "" {
		now := time.Now().In(loc)
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc), nil
	}
	return time.ParseInLocation("2006-01-02", s, loc)
}

// 静默引用 decimal / service 避免 unused import 警告(若有人剥离部分代码)。
var _ = decimal.Zero
var _ = service.ErrInvalidInput