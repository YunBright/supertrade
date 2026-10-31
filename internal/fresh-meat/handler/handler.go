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
	"fmt"
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
// 中间件链(对齐 stocktake):
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
	// POST /whole-pigs/:pig_id/advice —— 重新生成分割建议(异步,立即返回 202)。
	r.POST("/whole-pigs/:pig_id/advice", requireBranch, h.RegeneratePigAdvice)
	// DELETE /whole-pigs/:pig_id —— 删掉**录错了**的猪。
	//   ⚠️ 只允许删**干净**的猪：没有分割补录、没有销售归属、且不在已审计期间。
	r.DELETE("/whole-pigs/:pig_id", requireBranch, h.DeleteWholePig)

	// 单品补录
	r.POST("/pig-cuts", requireBranch, h.RecordPigCut)
	r.GET("/pigs/:pig_id/cuts", requireBranch, h.ListPigCutsByPig)

	// 日终按部位盘点
	r.POST("/pork-cuts-stocktake", requireBranch, h.RecordPorkCutsStocktake)
	r.GET("/pork-cuts-stocktake/latest", requireBranch, h.GetLatestPorkCutsStocktake)
	r.GET("/pork-cuts-stocktake/:id", requireBranch, h.GetPorkCutsStocktake)

	// 报损
	r.POST("/waste-logs", requireBranch, h.RecordWasteLog)

	// **搭赠**(2026-10-31):买肉送猪肝这类「有成本、无收入、没过称」的出库。
	// GET —— 列当天**疑似搭赠**的部位(倒推建议值,门店确认用)。
	r.GET("/gift-suggestions", requireBranch, h.ListGiftSuggestions)

	// 销售聚合
	r.GET("/line-sales-by-pig", requireBranch, h.ListLineSalesByPig)

	// 门店 cut 部位 ↔ cube SKU 映射(初始化 / 拓店时维护)
	r.GET("/branch-cut-mappings", requireBranch, h.ListBranchCutMappings)
	r.POST("/branch-cut-mappings", requireBranch, h.CreateBranchCutMapping)
	r.PUT("/branch-cut-mappings/:id", requireBranch, h.UpdateBranchCutMapping)
	r.DELETE("/branch-cut-mappings/:id", requireBranch, h.DeleteBranchCutMapping)

	// 库存位置:猪肉台 / 冷库
	//
	// POST /stock/counter-load —— 开市准备:把冷库的货搬上猪肉台(可部分、可不上)。
	// GET  /stock               —— 库存查询:默认猪肉台,scope=all 为全店。
	//                              ⚠️ **不提供"只查冷库"**(门店明确:没有查冷库的说法)。
	r.POST("/stock/counter-load", requireBranch, h.CounterLoad)
	// GET /stock/counter-load —— **搬台流水**:当天搬了哪些货。
	//                              前端第②步(已搬台?)与休市判定都靠它。
	r.GET("/stock/counter-load", requireBranch, h.ListCounterLoads)
	// PUT/DELETE /stock/counter-load/:id —— 改/撤**一行**搬台。
	//   (门店口径:「一个 sku 当天可以有多条记录,早上一袋中午加一袋,可以随时修改,
	//     甚至过一天修改」⇒ 改出来的负库存在每日毛利的 alert 里红字显示。)
	r.PUT("/stock/counter-load/:id", requireBranch, h.UpdateCounterLoad)
	r.DELETE("/stock/counter-load/:id", requireBranch, h.DeleteCounterLoad)
	// GET/PUT /stock/openings —— **期初结转**(鲜肉-设置页):
	//   按营业日推导的冷库余额是链式的 ⇒ 链的起点(期初)由门店在这里录(按部位)。
	r.GET("/stock/openings", requireBranch, h.ListStockOpenings)
	r.PUT("/stock/openings", requireBranch, h.SaveStockOpenings)
	r.DELETE("/stock/openings", requireBranch, h.DeleteStockOpening)
	// POST /stock/audit —— **审计结转**(指定截止日期):
	//   截止日期及之前的每日日终盘点必须齐全 ⇒ 通过则落快照并**冻结**(不能再补录)。
	//   审计同时做**预期毛利(预售)结转** + **重放截止日之后的动作** + 校准当前余额。
	r.POST("/stock/audit", requireBranch, h.AuditBusinessDays)
	// GET /stock/audit —— 审计状态(最近一次截止日 + 历次审计)。
	r.GET("/stock/audit", requireBranch, h.GetBizAuditStatus)
	// GET /stock/audit/precheck —— **审计前**的预检(条件 / 结余 / 预期毛利测算 / 提示文案)。
	r.GET("/stock/audit/precheck", requireBranch, h.PrecheckBizAudit)
	// GET /stock/presale —— **预期毛利(预售结转)**明细(按部位 / 按 SKU / 按猪)。
	r.GET("/stock/presale", requireBranch, h.ListPresale)
	// PUT /stock/presale —— 改**已审计**那一天的预售单价(门店审计人的人工修改;
	// 每次改都留痕,并自动重算毛利 / 按猪分摊 / 重放截止日之后的天)。
	r.PUT("/stock/presale", requireBranch, h.UpdatePresale)
	// GET /stock/bags —— 袋子(标签号)清单:冷库里有哪些袋、标签号是什么。
	r.GET("/stock/bags", requireBranch, h.ListStockBags)
	r.GET("/stock", requireBranch, h.ListStock)

	// GET /business-day —— **营业日状态机**:
	// 未进猪 → 已进猪 → 已搬台 → 已盘点(售罄|休市)。
	// 顺带给出"前一个营业日是否已盘点"(不补录昨天就无法录今天)。
	r.GET("/business-day", requireBranch, h.GetBusinessDay)

	// 特价档货号登记(长期配置,像部位 SKU 映射一样)
	//
	// 登记过的货号,其 POS 销售行**不再被丢弃** ⇒ 金额与重量进得来。
	// ⚠️ 特价档是"价格槽位",装的是当天滞销的部位 ⇒ **不静态映射到某个部位**,
	// 未指定部位时归到 `promo` 桶,毛利报告单列"特价(未归属)"。
	r.GET("/promo-skus", requireBranch, h.ListPromoSkus)
	r.POST("/promo-skus", requireBranch, h.CreatePromoSku)
	r.DELETE("/promo-skus/:id", requireBranch, h.DeletePromoSku)

	// 大宗台账(板油等:分割产出 → 冷库 → 酒店统收结算)
	r.GET("/bulk-products", requireBranch, h.ListBulkProducts)
	r.POST("/bulk-products", requireBranch, h.CreateBulkProduct)
	r.DELETE("/bulk-products/:id", requireBranch, h.DeleteBulkProduct)
	r.GET("/bulk-productions", requireBranch, h.ListBulkProductions)
	r.POST("/bulk-productions", requireBranch, h.RecordBulkProduction)
	r.GET("/bulk-settlements", requireBranch, h.ListBulkSettlements)
	r.POST("/bulk-settlements", requireBranch, h.CreateBulkSettlement)
	r.GET("/bulk-balances", requireBranch, h.ListBulkBalances)

	// 长期毛利(周/月/季)
	r.GET("/gross-margin/period", requireBranch, h.ComputePeriodMargin)

	// 分割重量统计基线(替掉 LLM 做数值回归)
	r.GET("/forecast-baselines", requireBranch, h.ListForecastBaselines)
	// DELETE —— 删除不准确/脏基线(默认本店;可加 ?branch_id= 删别家店的)。
	r.DELETE("/forecast-baselines", requireBranch, h.DeleteForecastBaseline)
	// POST —— 从**样本库**全量拟合出新的基线(覆盖所有分层)。
	r.POST("/forecast-baselines/recompute", requireBranch, h.RecomputeForecastBaselines)
	// GET —— 多门店分级基线对比(按权重段 × 部位并排)。
	r.GET("/forecast-baselines/compare", requireBranch, h.CompareForecastBaselines)

	// **样本库**(2026-10-30 owner 拍板:样本库是一等公民,基线是派生结果)
	// GET —— 列样本库(按天 × 体重段分组)。
	r.GET("/forecast-samples", requireBranch, h.ListForecastSamples)
	// POST —— **按一天加入样本**(跑那天的引擎落样本;不自动重算)。
	r.POST("/forecast-samples/collect", requireBranch, h.CollectForecastSamples)
	// DELETE —— 删除样本(打墓碑):?date=&weight_band= 整组删 / ?pig_id= 单头猪删。
	r.DELETE("/forecast-samples", requireBranch, h.DeleteForecastSamples)
	// POST —— 恢复被删(墓碑)的样本。
	r.POST("/forecast-samples/restore", requireBranch, h.RestoreForecastSamples)
	// POST —— **已下线**的「按一天生成基线」:回 **410 Gone** 并告诉客户端该用哪个端点
	//   (404 会让人以为是 URL 写错了 ⇒ 410 + 说明才是"这条语义被换掉了")。
	r.POST("/forecast-baselines/generate", requireBranch, h.GenerateForecastBaselines)

	// ---- 单价通道(2026-10-31 owner:「分割基线加入单价」)----
	//
	// 与重量通道**同构**:样本库是一等公民,基线是派生结果。
	// GET —— 列单价基线(店 × 部位)。
	r.GET("/forecast-price-baselines", requireBranch, h.ListForecastPriceBaselines)
	// POST —— 从单价日样本重算单价基线。
	r.POST("/forecast-price-baselines/recompute", requireBranch, h.RecomputeForecastPriceBaselines)
	// GET —— 列单价日样本(店 × 日 × 部位)。
	r.GET("/forecast-price-samples", requireBranch, h.ListForecastPriceSamples)
	// POST —— **按一天采集**单价样本(跑那天的引擎落样本;不自动重算)。
	r.POST("/forecast-price-samples/collect", requireBranch, h.CollectForecastPriceSamples)
	// DELETE —— 删除单价样本(打墓碑,不物理删):?date=&cut_type=
	r.DELETE("/forecast-price-samples", requireBranch, h.DeleteForecastPriceSample)
	// POST —— 退取链③:让 AI 重新给某个部位的进货单价(写进缓存,之后退取链②就能命中)。
	r.POST("/cut-unit-prices/refresh", requireBranch, h.RefreshCutUnitPrice)

	// 跨门店选用基线的配置(本店借用别店的基线)
	r.GET("/forecast-baseline-borrows", requireBranch, h.ListForecastBaselineBorrows)
	r.POST("/forecast-baseline-borrows", requireBranch, h.UpsertForecastBaselineBorrow)
	r.DELETE("/forecast-baseline-borrows/:id", requireBranch, h.DeleteForecastBaselineBorrow)

	// 成本链逐日物化:落 `cut_cost_daily` ⇒ 长期毛利变快。
	// ⚠️ 幂等的派生数据重算,不绑盘点(绑盘点 ⇒ 零盘点门店的链每天断一次)。
	r.POST("/cost-chain/materialize", requireBranch, h.MaterializeCostChain)

	// dapr cron binding 触发(deployer 仓加 bindings.cron.yaml)—
	// 走 dapr 自身 cron binding,无 JWT 无 X-Branch-ID;**不挂** requireBranch。
	r.GET("/cron/daily-reminder", h.CronDailyReminder)

	// 毛利查询(per cut + total)
	r.GET("/gross-margin", requireBranch, h.GetGrossMargin)
}

// ---- dapr cron binding 触发 ----

// CronDailyReminder GET /cron/daily-reminder
//
// dapr cron binding 触发(无 JWT、无 X-Branch-ID ⇒ 不挂 requireBranch)。
// 当前为占位实现:返回 ok + note,不产生告警。
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
		errors.Is(err, service.ErrBranchCutMappingNotFound),
		errors.Is(err, service.ErrPromoSkuNotFound):
		writeError(c, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, service.ErrInvalidInput),
		errors.Is(err, service.ErrBranchMismatch),
		errors.Is(err, service.ErrBranchCutMappingConflict):
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, service.ErrCubeUnavailable):
		writeError(c, http.StatusServiceUnavailable, "cube_unavailable", err.Error())
	case errors.Is(err, service.ErrUserInfoUnavailable):
		// ⚠️ 不透出 err.Error():错误含 gRPC 码与内部 app-id,对门店无意义;细节留服务端日志。
		writeError(c, http.StatusServiceUnavailable, "userd_unavailable",
			"权限服务暂时不可用，请稍后重试")
	case errors.Is(err, service.ErrBulkProductConflict):
		// 409:资源已存在(唯一键冲突),不是"输入非法"(400)。
		// 落 500 的后果是"操作者以为服务端坏了",而实际只是重复登记。
		writeError(c, http.StatusConflict, "conflict", err.Error())
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
		writeError(c, http.StatusBadRequest, "bad_json", badJSONMessage(err))
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

// RegeneratePigAdvice POST /whole-pigs/:pig_id/advice
//
// 手动重跑分割建议(异步,立即返回 202 + advice_status=generating)。
//
// 权限:freshmeat:write。
func (h *Handler) RegeneratePigAdvice(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	pigID := strings.TrimSpace(c.Param("pig_id"))
	if pigID == "" {
		writeError(c, http.StatusBadRequest, "missing_pig_id", "pig_id 必填")
		return
	}
	out, err := h.svc.RegeneratePigAdvice(c.Request.Context(), branchID, pigID)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{
		"ok":            true,
		"pig_id":        pigID,
		"advice_status": out.AdviceStatus,
	})
}

// DeleteWholePig DELETE /whole-pigs/:pig_id
//
// 删掉**录错了**的猪（早盘多录一头 / 毛重打错）。
//
// 权限：freshmeat:write。
//
// ⚠️ 只允许删**干净**的猪：没有分割补录、没有销售归属、不在已审计期间
// (理由见 service/pig_delete.go)⇒ 否则 400 + 说清为什么不能删。
func (h *Handler) DeleteWholePig(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	pigID := strings.TrimSpace(c.Param("pig_id"))
	if pigID == "" {
		writeError(c, http.StatusBadRequest, "missing_pig_id", "pig_id 必填")
		return
	}
	if err := h.svc.DeleteWholePig(c.Request.Context(), branchID, pigID); err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "pig_id": pigID})
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
		writeError(c, http.StatusBadRequest, "bad_json", badJSONMessage(err))
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
//
// 权限:freshmeat:view(只读端点;写端点用 freshmeat:write)。
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
		writeError(c, http.StatusBadRequest, "bad_json", badJSONMessage(err))
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

// ---- 搭赠(2026-10-31)----

// ListGiftSuggestions GET /gift-suggestions?date=YYYY-MM-DD
//
// 列出当天**疑似搭赠**的部位:账面应有 − 实盘 > 0 的部分。
//
// ⚠️ 它是**建议**:缺口 = 搭赠 + 真盘亏 ⇒ 必须门店确认。
//
//	确认成搭赠 ⇒ POST /waste-logs(waste_type='gift')⇒ 缺口归零、不再提示。
func (h *Handler) ListGiftSuggestions(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	day, err := h.parseDay(c.Query("date"))
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_date", err.Error())
		return
	}
	out, err := h.svc.ListGiftSuggestions(c.Request.Context(), branchID, day)
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
		writeError(c, http.StatusBadRequest, "bad_json", badJSONMessage(err))
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
		writeError(c, http.StatusBadRequest, "bad_json", badJSONMessage(err))
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
		writeError(c, http.StatusBadRequest, "bad_json", badJSONMessage(err))
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

// ---- 库存位置(猪肉台 / 冷库)---- //

// CounterLoad POST /stock/counter-load
//
// 开市准备:把冷库的货搬上猪肉台。
//
// ⚠️ 门店可以**只搬一部分,甚至不上**(货在冷库压着,过几天再卖或走酒店统收)
// ⇒ 冷库余额是累加的,只有记录了搬台量才算得准。
func (h *Handler) CounterLoad(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	var in service.CounterLoadInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "bad_json", badJSONMessage(err))
		return
	}
	cl, _ := claims.FromContext(c.Request.Context())
	out, err := h.svc.CounterLoad(c.Request.Context(), branchID, in, actorIDFromClaims(cl))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// ListCounterLoads GET /stock/counter-load?date=YYYY-MM-DD
//
// **搬台流水**:某营业日搬上台的货。
//
// ⇒ 前端第②步(今天搬台了吗?)与休市判定都靠它。
//
// `date` 省略 = 今天(营业日界时区)。
func (h *Handler) ListCounterLoads(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	day, err := h.parseDay(c.Query("date"))
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_date", err.Error())
		return
	}
	out, err := h.svc.ListCounterLoads(c.Request.Context(), branchID, day)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// GetBusinessDay GET /business-day?date=YYYY-MM-DD
//
// **营业日状态机**:未进猪 → 已进猪 → 已搬台 → 已盘点(售罄|休市)。
//
// 顺带给出"前一个营业日是否已盘点"(门店口径:不补录昨天就无法录今天)
// 与"现在能不能录猪 / 搬台"。
func (h *Handler) GetBusinessDay(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	day, err := h.parseDay(c.Query("date"))
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_date", err.Error())
		return
	}
	out, err := h.svc.GetBusinessDay(c.Request.Context(), branchID, day)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// UpdateCounterLoad PUT /stock/counter-load/:id
//
// 改一行搬台的重量(门店口径:「可以随时修改,甚至过一天修改」)。
//
// ⚠️ 改出来**负库存不拦** ⇒ 在每日毛利的告警里红字显示
// (service.MarginAlert / code=negative_inventory)。
func (h *Handler) UpdateCounterLoad(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		writeError(c, http.StatusBadRequest, "missing_counter_load_id", "id 必填")
		return
	}
	var in service.CounterLoadUpdateInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "bad_json", badJSONMessage(err))
		return
	}
	out, err := h.svc.UpdateCounterLoad(c.Request.Context(), branchID, id, in)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// DeleteCounterLoad DELETE /stock/counter-load/:id
//
// 撤掉一行搬台(货搬回冷库)。
func (h *Handler) DeleteCounterLoad(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		writeError(c, http.StatusBadRequest, "missing_counter_load_id", "id 必填")
		return
	}
	out, err := h.svc.DeleteCounterLoad(c.Request.Context(), branchID, id)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// ListStockOpenings GET /stock/openings
//
// **期初结转**清单(鲜肉-设置页读端)。
func (h *Handler) ListStockOpenings(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	out, err := h.svc.ListStockOpenings(c.Request.Context(), branchID)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// SaveStockOpenings PUT /stock/openings
//
// 保存**期初结转**(鲜肉-设置页写端)⇒ 重推按营业日的余额链。
func (h *Handler) SaveStockOpenings(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	var in struct {
		Openings []service.StockOpeningInput `json:"openings"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "bad_json", badJSONMessage(err))
		return
	}
	// ⚠️ 空 openings 一律 400(字段名写错时反序列化会得到空列表,静默写入等于"保存成功但没变")。
	if len(in.Openings) == 0 {
		writeError(c, http.StatusBadRequest, "bad_request",
			"openings 不能为空(每个部位一行:{cut_type, cold_store_kg, unit_price_yuan, biz_date?, note?})"+
				" —— 有货就要给单价(unit_price_yuan > 0):期初是已经付过钱的货,单价 0 会让这批货零成本、毛利虚高")
		return
	}
	out, err := h.svc.SaveStockOpenings(c.Request.Context(), branchID, in.Openings)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// DeleteStockOpening DELETE /stock/openings?cut_type=xxx
//
// 删掉某个部位的期初结转(把该部位从链的起点里去掉)。
func (h *Handler) DeleteStockOpening(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	cutType := model.CutType(strings.TrimSpace(c.Query("cut_type")))
	if cutType == "" {
		writeError(c, http.StatusBadRequest, "bad_request", "cut_type 必填")
		return
	}
	out, err := h.svc.DeleteStockOpening(c.Request.Context(), branchID, cutType)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// AuditBusinessDays POST /stock/audit
//
// **审计结转**:指定截止日期 ⇒ 该日期及之前的每日日终盘点必须齐全,
// 通过则落快照(=结转)并**冻结**(截止日期及之前不能再改,含补录)。
func (h *Handler) AuditBusinessDays(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	var in service.BizAuditInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "bad_json", badJSONMessage(err))
		return
	}
	cl, _ := claims.FromContext(c.Request.Context())
	out, err := h.svc.AuditBusinessDays(c.Request.Context(), branchID, in, actorIDFromClaims(cl))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// GetBizAuditStatus GET /stock/audit
//
// 审计状态:最近一次截止日(该日及之前已冻结)+ 历次审计。
func (h *Handler) GetBizAuditStatus(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	out, err := h.svc.GetBizAuditStatus(c.Request.Context(), branchID)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// PrecheckBizAudit GET /stock/audit/precheck?cutoff_date=YYYY-MM-DD
//
// 审计**前**的预检:条件是否满足(缺哪几天的日终盘点)、本次会不会产生结余、
// 预期毛利(预售结转)的测算、审计提示文案。
//
// ⚠️ 审计是**不可逆**的一刀 ⇒ 门店应当在按下确认之前看到这些。
func (h *Handler) PrecheckBizAudit(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	cutoff := strings.TrimSpace(c.Query("cutoff_date"))
	if cutoff == "" {
		writeError(c, http.StatusBadRequest, "bad_request", "cutoff_date 必填(YYYY-MM-DD)")
		return
	}
	out, err := h.svc.PrecheckBizAudit(c.Request.Context(), branchID, cutoff)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// ListPresale GET /stock/presale?date=YYYY-MM-DD
//
// 某个营业日的**预期毛利(预售结转)**明细:按部位 / 按 SKU / 按猪。
func (h *Handler) ListPresale(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	day, err := h.parseDay(c.Query("date"))
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_date", err.Error())
		return
	}
	out, err := h.svc.ListPresale(c.Request.Context(), branchID, day)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// UpdatePresale PUT /stock/presale
//
// **改已审计那一天的预售单价**(门店审计人的人工修改)。
//
// ⚠️ 冻结的是**源数据**与**成本口径快照**;预售价是「审计结转」这个**产物**的
// 一项(审计人拍板"这批货按什么价卖掉")⇒ 改它是**修正审计结论**。
// 每次改都留痕(cut_presale_edits)⇒ 改完自动重算毛利 / 按猪分摊 /
// 重放截止日之后的天 / 校准当前余额。
func (h *Handler) UpdatePresale(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	var in service.UpdatePresaleInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "bad_json", badJSONMessage(err))
		return
	}
	cl, _ := claims.FromContext(c.Request.Context())
	out, err := h.svc.UpdatePresale(
		c.Request.Context(),
		branchID,
		in,
		actorIDFromClaims(cl),
	)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// ListStockBags GET /stock/bags?location=&label=
//
// 袋子(标签号)清单:冷库里有哪些袋、各多少 kg、标签号是什么。
//
// ⚠️ `location` 省略 = 全部;`cold_store` 只看冷库,`counter` 只看猪肉台。
func (h *Handler) ListStockBags(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	out, err := h.svc.ListStockBags(c.Request.Context(), branchID,
		c.Query("location"), c.Query("label"), c.Query("origin_date"))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// ListStock GET /stock
//
// 库存查询:默认**猪肉台**;`?scope=all` 为全店(猪肉台 + 冷库)。
//
// ⚠️ **不提供"只查冷库"** —— 冷库单独看没有业务含义(2026-10-23 门店明确)。
func (h *Handler) ListStock(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	scope := model.StockScope(c.Query("scope"))
	switch scope {
	case "":
		scope = model.StockScopeCounter
	case model.StockScopeCounter, model.StockScopeAll:
	default:
		writeError(c, http.StatusBadRequest, "bad_request",
			"scope 只支持 counter | all(没有'只查冷库'这个口径)")
		return
	}
	out, err := h.svc.ListStock(c.Request.Context(), branchID, scope)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// ListPromoSkus GET /promo-skus
func (h *Handler) ListPromoSkus(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	out, err := h.svc.ListPromoSkus(c.Request.Context(), branchID)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// CreatePromoSku POST /promo-skus
func (h *Handler) CreatePromoSku(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	var in service.PromoSkuInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "bad_json", badJSONMessage(err))
		return
	}
	out, err := h.svc.CreatePromoSku(c.Request.Context(), branchID, in)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// DeletePromoSku DELETE /promo-skus/:id(停用,不是物理删)
func (h *Handler) DeletePromoSku(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		writeError(c, http.StatusBadRequest, "missing_promo_id", "id 必填")
		return
	}
	if err := h.svc.DeletePromoSku(c.Request.Context(), branchID, id); err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- 大宗台账 ----

// ListBulkProducts GET /bulk-products
func (h *Handler) ListBulkProducts(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	out, err := h.svc.ListBulkProducts(c.Request.Context(), branchID)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// CreateBulkProduct POST /bulk-products
func (h *Handler) CreateBulkProduct(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	var in service.BulkProductInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "bad_json", badJSONMessage(err))
		return
	}
	out, err := h.svc.CreateBulkProduct(c.Request.Context(), branchID, in)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// DeleteBulkProduct DELETE /bulk-products/:id(停用,不是物理删)
func (h *Handler) DeleteBulkProduct(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		writeError(c, http.StatusBadRequest, "missing_bulk_product_id", "id 必填")
		return
	}
	if err := h.svc.DeleteBulkProduct(c.Request.Context(), branchID, id); err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ListBulkProductions GET /bulk-productions
func (h *Handler) ListBulkProductions(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	out, err := h.svc.ListBulkProductions(c.Request.Context(), branchID)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// RecordBulkProduction POST /bulk-productions(录当日产出 → 进冷库)
func (h *Handler) RecordBulkProduction(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	var in service.BulkProductionInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "bad_json", badJSONMessage(err))
		return
	}
	cl, _ := claims.FromContext(c.Request.Context())
	out, err := h.svc.RecordBulkProduction(c.Request.Context(), branchID, in, actorIDFromClaims(cl))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// ListBulkSettlements GET /bulk-settlements
func (h *Handler) ListBulkSettlements(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	out, err := h.svc.ListBulkSettlements(c.Request.Context(), branchID)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// CreateBulkSettlement POST /bulk-settlements(酒店统收结算)
func (h *Handler) CreateBulkSettlement(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	var in service.BulkSettlementInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "bad_json", badJSONMessage(err))
		return
	}
	cl, _ := claims.FromContext(c.Request.Context())
	out, err := h.svc.CreateBulkSettlement(c.Request.Context(), branchID, in, actorIDFromClaims(cl))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// ListBulkBalances GET /bulk-balances(结存:产出 − 已结)
func (h *Handler) ListBulkBalances(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	out, err := h.svc.ListBulkBalances(c.Request.Context(), branchID)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// ---- 长期毛利 ----

// ComputePeriodMargin GET /gross-margin/period?from=&to=&group_by=day|cut|sku
func (h *Handler) ComputePeriodMargin(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	loc := h.svc.BizTZ()
	now := time.Now().In(loc)
	to := c.Query("to")
	if to == "" {
		to = now.Format("2006-01-02")
	}
	from := c.Query("from")
	if from == "" {
		fromT, err := time.ParseInLocation("2006-01-02", to, loc)
		if err != nil {
			writeError(c, http.StatusBadRequest, "bad_request", "to 格式应为 YYYY-MM-DD")
			return
		}
		from = fromT.AddDate(0, 0, -29).Format("2006-01-02")
	}
	out, err := h.svc.ComputePeriodMargin(c.Request.Context(), branchID, from, to, c.Query("group_by"))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// ---- 分割重量统计基线 ----

// ListForecastBaselines GET /forecast-baselines
func (h *Handler) ListForecastBaselines(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	out, err := h.svc.ListForecastBaselines(c.Request.Context(), branchID)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// RecomputeForecastBaselines POST /forecast-baselines/recompute
func (h *Handler) RecomputeForecastBaselines(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	n, err := h.svc.RecomputeForecastBaselines(c.Request.Context(), branchID)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"baselines": n})
}

// DeleteForecastBaseline DELETE /forecast-baselines?weight_band=&cut_type=
//
// 删除不准确/脏的基线(只删**本店**那一行;带 ?branch_id= 才删别家店的)。
//
// ⚠️ 目标是谁就按谁判权限 ⇒ 不能靠这个端点删掉没权限的店的基线。
func (h *Handler) DeleteForecastBaseline(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	target := strings.TrimSpace(c.Query("branch_id"))
	if target == "" {
		target = branchID
	}
	if !h.requireScope(c, target, "freshmeat:write") {
		return
	}
	band := strings.TrimSpace(c.Query("weight_band"))
	cut := strings.TrimSpace(c.Query("cut_type"))
	n, err := h.svc.DeleteForecastBaseline(c.Request.Context(), target, band, cut)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"deleted":     n,
		"branch_id":   target,
		"weight_band": band,
		"cut_type":    cut,
	})
}

// GenerateForecastBaselines POST /forecast-baselines/generate
//
// ⚠️ **已移除**（2026-10-30 owner 重新定义模型：样本库是一等公民，基线是派生结果）。
//
//	「按一天生成基线」把**样本采集**和**基线拟合**混成了一个动作 ⇒ 语义错、覆盖不全。
//	⇒ 拆成两个端点：
//	  - `POST /forecast-samples/collect` —— 按一天**加入样本**（跑引擎落样本，不重算）；
//	  - `POST /forecast-baselines/recompute` —— 从样本库**重算**基线（覆盖所有分层）。
//
//	保留这个函数只是为了让老客户端拿到一个**明确的 410**（而不是 404 让人猜）。
func (h *Handler) GenerateForecastBaselines(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	writeError(c, http.StatusGone, "endpoint_removed",
		"「按一天生成基线」已下线：请改用「按一天加入样本」(POST /forecast-samples/collect) + 「重算」(POST /forecast-baselines/recompute)")
}

// ---- 单价通道(2026-10-31 owner:「分割基线加入单价」)----

// ListForecastPriceBaselines GET /forecast-price-baselines
//
// 列**单价基线**(店 × 部位)。
//
// ⚠️ 列表端点统一返**对象**(前端 `_expectMap` 统一解析)。
func (h *Handler) ListForecastPriceBaselines(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	out, err := h.svc.ListForecastPriceBaselines(c.Request.Context(), branchID)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"baselines": out})
}

// RecomputeForecastPriceBaselines POST /forecast-price-baselines/recompute
//
// 从**单价日样本**重算单价基线(覆盖所有部位)。
func (h *Handler) RecomputeForecastPriceBaselines(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	n, err := h.svc.RecomputeCutPriceBaselines(c.Request.Context(), branchID)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"baselines": n})
}

// ListForecastPriceSamples GET /forecast-price-samples?date=
//
// 列**单价日样本**(店 × 日 × 部位)。
func (h *Handler) ListForecastPriceSamples(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	out, err := h.svc.ListForecastPriceSamples(c.Request.Context(), branchID, c.Query("date"))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"samples": out})
}

// CollectForecastPriceSamples POST /forecast-price-samples/collect
//
// 「按一天采集单价样本」:入参 {"date":"2026-10-29"}
//
// ⚠️ **不自动重算** —— 重算是另一个明确动作(与重量样本同口径)。
func (h *Handler) CollectForecastPriceSamples(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	var in struct {
		Date string `json:"date"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "bad_json", badJSONMessage(err))
		return
	}
	res, err := h.svc.CollectCutPriceSamplesForDay(c.Request.Context(), branchID, in.Date)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// DeleteForecastPriceSample DELETE /forecast-price-samples?date=&cut_type=
//
// 删除不准确的单价样本(**打墓碑**,不物理删 —— 物理删会被引擎重跑复活)。
func (h *Handler) DeleteForecastPriceSample(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	date := strings.TrimSpace(c.Query("date"))
	cut := model.CutType(strings.TrimSpace(c.Query("cut_type")))
	if !cut.Valid() {
		writeError(c, http.StatusBadRequest, "invalid_cut_type",
			"cut_type 必填且必须在枚举内")
		return
	}
	n, err := h.svc.DeleteCutPriceSample(c.Request.Context(), branchID, date, cut)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"deleted":   n,
		"date":      date,
		"cut_type":  string(cut),
		"branch_id": branchID,
	})
}

// RefreshCutUnitPrice POST /cut-unit-prices/refresh
//
// 退取链③:让 AI **重新给**某个部位的进货单价(写进缓存,之后退取链②就能命中)。
//
// 入参 {"cut_type":"liver"}
//
// ⚠️ 这一步要调 LLM(10~30s)⇒ 只在**门店主动触发**时跑,
//   绝不放进报表路径(会把毛利看板拖到超时)。
func (h *Handler) RefreshCutUnitPrice(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	var in struct {
		CutType model.CutType `json:"cut_type"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "bad_json", badJSONMessage(err))
		return
	}
	price, err := h.svc.RefreshCutUnitPriceFromLLM(c.Request.Context(), branchID, in.CutType)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"cut_type":       string(in.CutType),
		"unit_price_yuan": price,
	})
}

// ListForecastSamples GET /forecast-samples?date=&weight_band=&include_excluded=
//
// 列**样本库**(按 天 × 体重段 分组)。
func (h *Handler) ListForecastSamples(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	out, err := h.svc.ListForecastSamples(c.Request.Context(), branchID, service.ForecastSampleQuery{
		Date:            c.Query("date"),
		WeightBand:      c.Query("weight_band"),
		IncludeExcluded: queryBool(c, "include_excluded"),
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// CollectForecastSamples POST /forecast-samples/collect
//
// 「按一天加入样本」:入参 {"date":"2026-10-29","source_branch_id":""}
//
// ⚠️ 那天缺"录入/搬台/销售/盘点"任一项 ⇒ 400 并点名缺哪一项。
// ⚠️ **不自动重算** —— 重算是另一个明确动作。
func (h *Handler) CollectForecastSamples(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	var in service.ForecastSampleCollectInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "bad_json", badJSONMessage(err))
		return
	}
	out, err := h.svc.CollectForecastSamples(c.Request.Context(), branchID, in)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// DeleteForecastSamples DELETE /forecast-samples?date=&weight_band=  |  ?pig_id=
//
// 删除样本(**打墓碑**,不物理删 —— 物理删会被引擎重跑复活)。
func (h *Handler) DeleteForecastSamples(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	target := service.ForecastSampleTarget{
		Date:       c.Query("date"),
		WeightBand: c.Query("weight_band"),
		PigID:      c.Query("pig_id"),
	}
	n, err := h.svc.DeleteForecastSamples(c.Request.Context(), branchID, target)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"excluded": n,
		"message":  fmt.Sprintf("已删除 %d 条样本，点「重算」后生效", n),
	})
}

// RestoreForecastSamples POST /forecast-samples/restore
//
// 恢复被删(墓碑)的样本:入参 {"date":"2026-10-29","weight_band":"","pig_id":""}
func (h *Handler) RestoreForecastSamples(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	var target service.ForecastSampleTarget
	// ⚠️ body 可选(空 body 也行)⇒ 解析失败不报错,改用 query 参数。
	_ = c.ShouldBindJSON(&target)
	if v := strings.TrimSpace(c.Query("date")); v != "" {
		target.Date = v
	}
	if v := strings.TrimSpace(c.Query("weight_band")); v != "" {
		target.WeightBand = v
	}
	if v := strings.TrimSpace(c.Query("pig_id")); v != "" {
		target.PigID = v
	}
	n, err := h.svc.RestoreForecastSamples(c.Request.Context(), branchID, target)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"restored": n,
		"message":  fmt.Sprintf("已恢复 %d 条样本，点「重算」后生效", n),
	})
}

// queryBool 读一个布尔 query 参数(1/true/yes ⇒ true)。
func queryBool(c *gin.Context, key string) bool {
	v := strings.TrimSpace(c.Query(key))
	if v == "" {
		return false
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "y":
		return true
	default:
		return false
	}
}

// CompareForecastBaselines GET /forecast-baselines/compare?branches=00,01
//
// 多门店分级基线对比(按权重段 × 部位并排)。
//
// ⚠️ **请求的每一个门店**都必须有 freshmeat:view ⇒ 任一无权 ⇒ 403。
//
//	(绝不能让一个门店看到别家的基线。)
func (h *Handler) CompareForecastBaselines(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	ids := splitCSV(c.Query("branches"))
	if len(ids) == 0 {
		// 不给门店号 ⇒ 只看本店(单店自查)。
		ids = []string{branchID}
	}
	for _, b := range ids {
		if !h.requireScope(c, b, "freshmeat:view") {
			return
		}
	}
	out, err := h.svc.CompareForecastBaselines(c.Request.Context(), ids)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// ListForecastBaselineBorrows GET /forecast-baseline-borrows
//
// 本店的"借用别店基线"配置清单。
func (h *Handler) ListForecastBaselineBorrows(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	out, err := h.svc.ListForecastBaselineBorrows(c.Request.Context(), branchID)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// UpsertForecastBaselineBorrow POST /forecast-baseline-borrows
//
// 新增/更新借用配置。
//
// 入参:{"source_branch_id":"01","weight_band":"","cut_type":"","enabled":true}
//
// ⚠️ weight_band 与 cut_type 都空 = 整店借用;只给 weight_band = 该体重段全部部位;
//
//	都给 = 精确分层。
func (h *Handler) UpsertForecastBaselineBorrow(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	var in service.ForecastBaselineBorrowInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "bad_json", badJSONMessage(err))
		return
	}
	out, err := h.svc.UpsertForecastBaselineBorrow(c.Request.Context(), branchID, in)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// DeleteForecastBaselineBorrow DELETE /forecast-baseline-borrows/:id
func (h *Handler) DeleteForecastBaselineBorrow(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if err := h.svc.DeleteForecastBaselineBorrow(c.Request.Context(), branchID, id); err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// splitCSV 拆逗号分隔的字符串(去空白、去空项)。
func splitCSV(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		p := strings.TrimSpace(part)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// MaterializeCostChain POST /cost-chain/materialize?from=YYYY-MM-DD
//
// ⚠️ 派生数据重算 ⇒ **幂等、可重复调用**;不像审计结转那样不可逆(不冻结数据)。
// ⚠️ **不绑盘点**:盘点只是"实盘锚定",不是"链的开关"(绑盘点 ⇒ 零盘点门店的链每天断一次)。
func (h *Handler) MaterializeCostChain(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	from := c.Query("from")
	n, err := h.svc.MaterializeCostChain(c.Request.Context(), branchID, from)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"rows": n})
}
