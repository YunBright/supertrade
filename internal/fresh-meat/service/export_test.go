package service

import (
	"context"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
)

// export_test.go —— **仅测试用**的导出(不进生产二进制)。
//
// # 为什么需要
//
// 「按营业日推导的日终结余」(`closingForDay`)是**私有**方法,而它返回的是
// **部位级**的结余(2026-10-27 起)⇒ 外部测试包(package service_test)
// 需要一个入口才能断言「某营业日的冷库余额是多少」。
//
// ⚠️ 标准库的 export_test.go 同款做法:内部测试文件里的**导出**标识符
//
//	对外部测试包可见,但只在测试构建里存在。
//
// # 口径
//
// 返回 (柜台, 冷库) —— 都是**部位(cut_type) → kg**。
// ⚠️ 日终(夜里"过夜搬运"之后)⇒ 柜台已清空 ⇒ 柜台那一张永远是空的。
func ClosingByCutForTest(ctx context.Context, s *Service, branchID string, day time.Time) (
	counter, cold map[string]decimal.Decimal, err error,
) {
	counter, cold, _, err = s.closingForDay(ctx, branchID, day)
	return counter, cold, err
}

// ClampNonNegativeForTest 暴露 [clampNonNegative](负的期末 → 0)。
//
// ⚠️ 为什么它值得单独暴露:「读到负的期末要裁成 0」是 owner「期初不能有负数」
//
//	这条硬要求的**最后一道闸** —— 存量快照里真的还有旧口径写的负数
//	(生产实测 店 00 2026-10-29:8 个部位为负)
//	⇒ 不裁 ⇒ 负期初从快照/锚点漏回链里 ⇒ 只对新建门店成立。
func ClampNonNegativeForTest(kg, amount decimal.Decimal) (decimal.Decimal, decimal.Decimal) {
	return clampNonNegative(kg, amount)
}

// ForecastSplitSampleGapForTest 暴露 [forecastSplitSampleGap](分割样本缺口的判定)。
//
// ⚠️ 为什么它值得单独暴露:
//
//	「两个样本池都空 ⇒ 报缺:分割样本」这条判定**在端到端场景里几乎构造不出来**——
//	引擎很会把销售归到"无名桶"里,倒推值仍为正 ⇒ 总是有样本。
//	⇒ 而判据本身有个**容易写错**的地方:是"**都**空"才算缺,不是"任一为空"。
//	⇒ 把它抽成小函数 + 直接锁,比硬造场景可靠。
//
// 返回:空串 = 不缺;非空 = 缺口文案(门店看到的原话)。
func ForecastSplitSampleGapForTest(recordedN, estimatedN int) string {
	return forecastSplitSampleGap(recordedN, estimatedN)
}

// SuggestedSplitKgForTest 暴露 [loadSuggestedSplitKg](逐猪按「建议生成方式」取数)。
//
// ⚠️ 为什么它值得单独暴露:
//
//	「auto / baseline / llm」三种模式的差别**只发生在**这个函数里
//	(基线覆盖的部位取谁、基线没覆盖的部位取谁、要不要看 LLM)
//	⇒ 不导出就没法从外部测试包锁它
//	⇒ 门店"选了没用"(选项摆在那儿、后端照旧)会**静默**发生 ⇒ 比没有这个选项更坏。
func SuggestedSplitKgForTest(ctx context.Context, s *Service, branchID string, pigs []model.WholePig) (
	map[string]decimal.Decimal, error,
) {
	return s.loadSuggestedSplitKg(ctx, branchID, pigs)
}

// ProducedBySkuForTest 暴露 [computeProducedBySku](兜底产出口径)。
//
// ⚠️ 为什么值得单独暴露:「只用基线 / 只用 AI」这两个模式在**两处**取数
//
//	① loadSuggestedSplitKg(建议)⇒ 决定报表的入库量(W 接管);
//	② computeProducedBySku(兜底产出)⇒ 决定"这头猪有没有产出数据"。
//
// ⇒ 只锁①的话,②里"只用基线的猪照样拿到 LLM 的部位"就是**静默**的
//
//	⇒ 门店选了没用 ⇒ 功能等于没做。
func ProducedBySkuForTest(ctx context.Context, s *Service, branchID string,
	start, end time.Time, pigs []model.WholePig, pigCost map[string]decimal.Decimal,
) (
	producedKg map[string]decimal.Decimal, byPig map[string]map[string]decimal.Decimal,
	unallocated decimal.Decimal, cuttingLoss decimal.Decimal, err error,
) {
	return s.computeProducedBySku(ctx, branchID, start, end, pigs, pigCost)
}

// SuggestedCutUnitPricesForTest 暴露 [suggestedCutUnitPrices](建议成本单价 + 来源)。
//
// ⚠️ 为什么值得单独暴露:「基线单价 × 单猪进价缩放 + 封顶 + 退取链」这条链
//
//	**只在这里**发生 ⇒ 不导出就没法从外部测试包锁它
//	⇒ owner 给的两条边界(各部位合计价值 ≤ 猪总进货金额)会被静默破坏。
func SuggestedCutUnitPricesForTest(ctx context.Context, s *Service, branchID string,
	pigs []model.WholePig, splitPerPig map[string]map[string]decimal.Decimal, bizDate time.Time) (
	unit map[string]decimal.Decimal, source map[string]string, err error,
) {
	return s.suggestedCutUnitPrices(ctx, branchID, pigs, splitPerPig, bizDate)
}

// PriceMarginRateOfRowsForTest 暴露 [priceMarginRateOfRows](整体毛利率 + 是否可用于倒推)。
//
// ⚠️ 为什么值得单独暴露:「毛利率越界 ⇒ 整天不产样本」这条判定
//
//	在端到端场景里很难构造(引擎给的毛利率通常正常)⇒ 直接锁判据更可靠。
func PriceMarginRateOfRowsForTest(rows []CutDailyCost) (decimal.Decimal, bool) {
	return priceMarginRateOfRows(rows)
}

// CollectCutPriceSamplesForTest 暴露 [CollectCutPriceSamples](采集单价日样本)。
//
// ⚠️ 为什么值得单独暴露:倒推口径(零售价 × (1 − 毛利率))与
//
//	「没有销售记录的部位不产样本」这两条**只在采集时**发生。
func CollectCutPriceSamplesForTest(ctx context.Context, s *Service, branchID string, day time.Time,
	rows []CutDailyCost) (*CutPriceSampleResult, error) {
	return s.CollectCutPriceSamples(ctx, branchID, day, rows)
}

// RollUpPerCutCutsForTest 暴露 [rollUpPerCutCuts](部位行装配)。
//
// ⚠️ 为什么值得单独暴露:「日终剩余 / 售罄率 / 建议毛利」三项
//
//	**全部**在这个函数里算出来(它们要跨部位才算得出)
//	⇒ 不导出就没法从外部测试包锁它们的口径。
func RollUpPerCutCutsForTest(rows []CutDailyCost, promoAlloc *PromoAllocation) (
	[]PerCutGrossMargin, TotalGrossMargin,
) {
	return rollUpPerCutCuts(rows, promoAlloc)
}
