// Package handler / forecast_price_preview.go —— 「按基线预览 / 重算」的 HTTP 层(2026-10-31 owner)。
//
// owner 原话:
//
//	「我打算用 29 日的 2 个分割样本、1 个 7.3 元/斤 的 29 日猪单价样本倒推出的
//	  『分割基线 + 部位进货单价基线』应用到 30 日去,
//	  需要你在录入猪的时候提供使用基线重算建议分割和建议成本单价的功能
//	  (目前只能靠删除和重录实现)。」
//
// ⇒ 两个端点：预览(不写库) + 重算(改已录的猪)。
package handler

import (
	"net/http"
	"strings"

	"github.com/YunBright/supertrade/pkg/middleware"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

// PreviewCutPrice GET /forecast-price-preview?date=&weight_kg=&pig_price_yuan=
//
// 预览:按**基线**算一遍「建议分割 + 建议成本单价」(**不写库**)。
//
// 入参：
//
//   - date           营业日(默认今日,bizTZ)。⚠️ 单价基线只用**该日之前**的日样本。
//   - weight_kg      这头猪的毛重(kg)—— **必填**。
//   - pig_price_yuan 这头猪的进价(元/kg)—— **必填**(基线要按它等比缩放)。
//
// ⚠️ 预览一律用**基线口径**(不取 LLM 兜底)⇒ 门店看到的就是"纯基线会给什么"。
func (h *Handler) PreviewCutPrice(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:view") {
		return
	}
	day, err := h.parseDay(c.Query("date"))
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_date", err.Error())
		return
	}
	weightKg, ok := parseDecimalParam(c.Query("weight_kg"))
	if !ok {
		writeError(c, http.StatusBadRequest, "bad_weight_kg", "weight_kg 必填且必须是数字(kg)")
		return
	}
	if !weightKg.IsPositive() {
		writeError(c, http.StatusBadRequest, "bad_weight_kg", "weight_kg 必须大于 0")
		return
	}
	pigPrice, ok := parseDecimalParam(c.Query("pig_price_yuan"))
	if !ok {
		writeError(c, http.StatusBadRequest, "bad_pig_price_yuan", "pig_price_yuan 必填且必须是数字(元/kg)")
		return
	}
	if !pigPrice.IsPositive() {
		writeError(c, http.StatusBadRequest, "bad_pig_price_yuan", "pig_price_yuan 必须大于 0")
		return
	}
	out, err := h.svc.PreviewCutPrice(c.Request.Context(), branchID, day, weightKg, pigPrice)
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// ApplyBaselineToPig POST /whole-pigs/:pig_id/apply-baseline
//
// 把**已录的猪**切成「只用基线」口径 + 重算当天成本链(不用删了重录)。
//
// 入参 {"mode":"baseline"} —— 可省(默认 baseline);传 "auto" 改回自动(基线优先 + LLM 兜底)。
//
// ⚠️ 已审计结转的期间**冻结** ⇒ 400 + 说清为什么不能改。
func (h *Handler) ApplyBaselineToPig(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, "freshmeat:write") {
		return
	}
	pigID := strings.TrimSpace(c.Param("pig_id"))
	if pigID == "" {
		writeError(c, http.StatusBadRequest, "missing_pig_id", "pig_id 必填")
		return
	}
	var in struct {
		Mode string `json:"mode"`
	}
	// ⚠️ body 可空(默认 baseline)⇒ 允许没有 body 的请求。
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&in); err != nil {
			writeError(c, http.StatusBadRequest, "bad_json", badJSONMessage(err))
			return
		}
	}
	out, err := h.svc.ApplyBaselineToPig(c.Request.Context(), branchID, pigID, strings.TrimSpace(in.Mode))
	if err != nil {
		mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// parseDecimalParam 解析一个十进制 query 参数。
//
// ⚠️ 空串 ⇒ ok=false(调用方据此判"必填缺失")⇒ 不返回 0 让上层误当成"用户填了 0"。
func parseDecimalParam(s string) (decimal.Decimal, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return decimal.Zero, false
	}
	v, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero, false
	}
	return v, true
}
