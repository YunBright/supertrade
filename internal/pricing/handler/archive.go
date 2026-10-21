// Package handler / archive.go —— 价格域的导入端点。
//
// 路由:
//
//	POST /archive/import
//	  X-Branch-ID: <目标门店>
//	  body: { source_branch_id, mode, as_of, data: { prices[], promotions[] } }
//
// # 基准日(as_of)
//
//	body.as_of = "yyyy-mm-dd",缺省 = 今天(服务端本地日期)。
//
//	促销是**时效数据**:导入端点只 upsert"对基准日有效"的行,
//	无效的行计入 skipped 并给一条 warning;
//	overwrite 模式下还会清掉该门店**不在有效集合内**的历史行
//	(只删同一 branch_id、只删 promotions 表、且必须本次 scope 里真的有 promotions)。
//
// ⚠️ **非法 as_of ⇒ 400**(不静默回落到今天)。
//
// 守门:`pricing:manage` scope(per-branch)。
//
// ⚠️ **请求体与 catalog 的 /archive/import 完全同构**
//
//	(同一个 archivedto.ImportRequest)。
//	⇒ 编排方(cubesync)可以用**一套**代码打给所有下游,每个下游只读自己那几个 key。
//	⇒ 代价:catalog 的 handler 必须忽略 data.prices / data.promotions,
//	  pricing 的 handler 必须忽略 data.products / data.categories ……
//	(JSON 对不认识的字段静默忽略 ⇒ 不会报错,但也**不会**误写)。
//
// ⚠️ **门店只从 header 取**,不从 body 取 —— body 里带一份门店,
// A 店的登录态就能把数据写进 B 店。
package handler

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/archivedto"
	"github.com/YunBright/supertrade/internal/pricing/model"
	"github.com/YunBright/supertrade/internal/pricing/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// importArchive POST /archive/import
//
// ⚠️ **有源门店只读**:该门店的价格来自外部系统(富百家 / 佳乐)⇒
// 写进本域 PG 的一行"没人看的价",而前台还显示"写成功了"。
// ⇒ 有源时一律 409(与 member 域同一套闸门)。
//
// ⚠️ **解析失败(问不到路由)⇒ 放行**:导入路径自己会兜底
// (本域 PG 里没有这个门店的行 ⇒ 导入 0 条),那是**诚实**的失败。
// ⇒ 顺带:cube-router 挂掉时不会挡住自营店(门店 10)的正常导入。
func (h *Handler) importArchive(c *gin.Context) {
	if h.priceLists == nil || h.promos == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code": "not_ready", "message": "本地价格表未装配",
		})
		return
	}
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "branch_required", "message": "X-Branch-ID header 必填",
		})
		return
	}
	// ⚠️ 闸门在**写库之前**:有源门店的导入一律拒(409)。
	if err := h.prices.EnsureLocalSource(c.Request.Context(), branchID); err != nil {
		h.mapErr(c, err)
		return
	}
	var req archivedto.ImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "bad_request", "message": err.Error(),
		})
		return
	}
	mode := req.Mode
	if mode == "" {
		mode = service.ModeIncremental
	}
	if mode != service.ModeIncremental && mode != service.ModeOverwrite {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "bad_mode", "message": "mode 只能是 incremental / overwrite",
		})
		return
	}

	// ⚠️ 基准日必须在**写库之前**校验:非法值当场 400。
	//
	//	静默回落到今天 ⇒ "这批数据是哪一天的"不可知,
	//	而促销是时效数据(源表 31371 行,对某一天有效的只有 741 行)。
	asOf, err := archivedto.NormalizeAsOf(req.AsOf, time.Now())
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "bad_as_of", "message": "as_of 必须是 yyyy-mm-dd",
		})
		return
	}

	res := archivedto.ImportResult{
		SourceBranchID: req.SourceBranchID,
		TargetBranchID: branchID,
		Mode:           mode,
	}

	// ① 售价。
	priceInputs := make([]service.PriceInput, 0, len(req.Data.Prices))
	for _, p := range req.Data.Prices {
		priceInputs = append(priceInputs, service.PriceInput{
			ProductID:       p.ProductID,
			PriceYuan:       p.PriceYuan,
			MemberPriceYuan: p.MemberPriceYuan,
			CostYuan:        p.CostYuan,
		})
	}
	priceCounts, priceErrs, err := h.priceLists.UpsertPrices(c.Request.Context(), branchID, mode, priceInputs)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	res.ByScope = append(res.ByScope, archivedto.ImportScopeRow{
		Scope: archivedto.ScopePrices, Received: priceCounts.Received,
		Inserted: priceCounts.Inserted, Updated: priceCounts.Updated,
		Skipped: priceCounts.Skipped, Deleted: priceCounts.Deleted,
	})
	res.Errors = append(res.Errors, toImportErrors(priceErrs)...)
	res.Warnings = append(res.Warnings, priceCounts.Warnings...)

	// ② 促销。
	//
	// ⚠️ **promotions 在不在本次 scope 里**决定能不能清理历史行:
	//
	//	一次只导 prices 的局部导入,若顺手把促销清了,
	//	症状是"同步跑完之后 POS 上的特价全没了",而报告里 promotions 一行都没有。
	promoScope := scopeRequested(req.Scopes, archivedto.ScopePromotions, len(req.Data.Promotions) > 0)
	promoInputs := make([]service.PromotionInput, 0, len(req.Data.Promotions))
	for _, p := range req.Data.Promotions {
		promoInputs = append(promoInputs, service.PromotionInput{
			ID:            p.PlanNo,
			ProductID:     p.ProductID,
			PlanName:      p.PlanName,
			PriceYuan:     p.PriceYuan,
			OrigPriceYuan: p.OrigPriceYuan,
			BeginDay:      p.BeginDay,
			EndDay:        p.EndDay,
		})
	}
	promoCounts, promoErrs, err := h.promos.UpsertPromotions(
		c.Request.Context(), branchID, mode, asOf, promoInputs, promoScope)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	res.ByScope = append(res.ByScope, archivedto.ImportScopeRow{
		Scope: archivedto.ScopePromotions, Received: promoCounts.Received,
		Inserted: promoCounts.Inserted, Updated: promoCounts.Updated,
		Skipped: promoCounts.Skipped, Deleted: promoCounts.Deleted,
	})
	res.Errors = append(res.Errors, toImportErrors(promoErrs)...)
	res.Warnings = append(res.Warnings, promoCounts.Warnings...)
	res.AsOf = asOf

	c.JSON(http.StatusOK, res)
}

// scopeRequested 判断某个 scope 在本次导入里**在不在**。
//
// 判据:
//   - scopes 非空 ⇒ 以 scopes 为准(调用方显式列了才算在);
//   - scopes 为空(= 全部)⇒ 以"该 scope 带了行"为准。
//
// ⚠️ **空集合 ≠ 不过滤**的同族坑:scopes 为空且没带任何行时,
// 该 scope **不在**本次导入里 ⇒ 不能触发任何"清理"动作。
func scopeRequested(scopes []string, name string, hasRows bool) bool {
	if len(scopes) == 0 {
		return hasRows
	}
	for _, s := range scopes {
		if strings.TrimSpace(s) == name {
			return true
		}
	}
	return false
}

// toImportErrors 把 service 的逐行错误转成契约里的形状。
func toImportErrors(errs []model.ImportRowError) []archivedto.ImportErrorItem {
	if len(errs) == 0 {
		return nil
	}
	out := make([]archivedto.ImportErrorItem, 0, len(errs))
	for _, e := range errs {
		out = append(out, archivedto.ImportErrorItem{
			Scope: e.Scope, ID: e.ID, Error: e.Error,
		})
	}
	return out
}
