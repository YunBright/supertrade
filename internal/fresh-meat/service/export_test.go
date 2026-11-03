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

// ClampMarginPctForTest 暴露 [clampMarginPct](毛利率钳进 numeric(8,4) 的范围)。
//
// ⚠️ 为什么值得单独暴露:
//
//	列类型是 `numeric(8,4)` ⇒ 装不下超过 ±9999.99 的毛利率
//	⇒ 不钳会**直接写库失败**、打挂整条成本链物化
//	(2026-10-31 实测:切「只用基线」后某部位营收 30 元 / 成本 4000 元
//	 ⇒ 毛利率 -13233.33% ⇒ PG 报 numeric field overflow ⇒ 端点 500)。
//	⇒ 这条只有**直接锁判据**才可靠(端到端很难稳定构造出爆表的场景)。
func ClampMarginPctForTest(pct float64) float64 {
	return clampMarginPct(pct)
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
//
// ⚠️ 封顶标志(capped)**故意不导出**:现有用例锁的是"单价 + 来源"这一对契约,
//
//	封顶行为由 [SuggestedCutUnitPricesCappedForTest] 单独锁。
func SuggestedCutUnitPricesForTest(ctx context.Context, s *Service, branchID string,
	pigs []model.WholePig, splitPerPig map[string]map[string]decimal.Decimal, bizDate time.Time) (
	unit map[string]decimal.Decimal, source map[string]string, err error,
) {
	unit, source, _, err = s.suggestedCutUnitPrices(ctx, branchID, pigs, splitPerPig, bizDate)
	return unit, source, err
}

// SuggestedCutUnitPricesCappedForTest 暴露"本次是否触发封顶"。
//
// ⚠️ 为什么单独导出:封顶是**静默**的(单价被按比例砍下去,数值上看不出来)
//
//	⇒ 少了这个标志,owner 给的边界②被破坏时没有任何信号。
func SuggestedCutUnitPricesCappedForTest(ctx context.Context, s *Service, branchID string,
	pigs []model.WholePig, splitPerPig map[string]map[string]decimal.Decimal, bizDate time.Time) (
	capped bool, err error,
) {
	_, _, capped, err = s.suggestedCutUnitPrices(ctx, branchID, pigs, splitPerPig, bizDate)
	return capped, err
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

// ReferenceDayTotalsForTest 造一个 [ReferenceDayTotals](参考口径的当日输入)。
//
// ⚠️ 为什么值得单独暴露:参考口径的**总采 / 期末剩余 / 采购均价**
//
//	  是外部测试包造不出来的(要真的建猪、真的做盘点)⇒ 单独给个构造器。
//
//		典型用法:
//
//		ref := service.ReferenceDayTotalsForTest("227.1", "3315.66", "27.65", true)
//		rows, total := service.RollUpPerCutCutsForTest(cutRows, nil, ref)
//
// ⚠️ purchaseAvg 是**采购金额 ÷ 采购重量**算出来的 ⇒ 这里按同样的口径给。
func ReferenceDayTotalsForTest(purchasedKg, purchasedYuan, remainKg string, hasRemain bool) ReferenceDayTotals {
	out := ReferenceDayTotals{HasRemain: hasRemain}
	if v, err := decimal.NewFromString(purchasedKg); err == nil {
		out.PurchasedKg = v
	}
	if v, err := decimal.NewFromString(purchasedYuan); err == nil {
		out.PurchasedYuan = v
	}
	if v, err := decimal.NewFromString(remainKg); err == nil {
		out.RemainKg = v
	}
	if out.PurchasedKg.IsPositive() {
		out.PurchaseAvgYuan = out.PurchasedYuan.Div(out.PurchasedKg)
	}
	return out
}

// ReferenceDayTotalsWithExternalForTest 造一个**带外采**的 [ReferenceDayTotals]。
//
// ⚠️ 为什么值得单独暴露:owner 2026-11-03 要求外采进成本口径
//   (长期毛利的期间采购 / 参考毛利的按量加权 / 采样的均价)
//   ⇒ 这三处都要在单测里能造出"当天有外采"的输入。
//
// 参数:(进猪kg, 进猪元, 外采kg, 外采元, 期末台面kg, 有盘点?)
// ⇒ 外采**按一个部位**记(belly)—— 单测里够用。
func ReferenceDayTotalsWithExternalForTest(purchasedKg, purchasedYuan,
	externalKg, externalYuan, remainKg string, hasRemain bool) ReferenceDayTotals {
	out := ReferenceDayTotalsForTest(purchasedKg, purchasedYuan, remainKg, hasRemain)
	out.ExternalByCut = map[string]ExternalCutTotals{}
	if v, err := decimal.NewFromString(externalKg); err == nil {
		out.ExternalKg = v
	}
	if v, err := decimal.NewFromString(externalYuan); err == nil {
		out.ExternalYuan = v
	}
	if out.ExternalKg.IsPositive() {
		byCut := ExternalCutTotals{Kg: out.ExternalKg, AmountYuan: out.ExternalYuan}
		byCut.UnitPriceYuan = byCut.AmountYuan.Div(byCut.Kg)
		out.ExternalByCut[string(model.CutBelly)] = byCut
	}
	return out
}

// RollUpPerCutCutsForTest 暴露 [rollUpPerCutCuts](部位行装配)。
//
// ⚠️ 为什么值得单独暴露:「日终剩余 / 售罄率 / **参考口径毛利**」几项
//
//	**全部**在这个函数里算出来(它们要跨部位才算得出)
//	⇒ 不导出就没法从外部测试包锁它们的口径。
func RollUpPerCutCutsForTest(rows []CutDailyCost, promoAlloc *PromoAllocation, ref ReferenceDayTotals) (
	[]PerCutGrossMargin, TotalGrossMargin,
) {
	return rollUpPerCutCuts(rows, promoAlloc, ref)
}

// BuildLongTermMarginReportForTest 暴露 [buildLongTermMarginReport](长期毛利的纯汇总)。
//
// ⚠️ 为什么值得单独暴露:长期毛利页的**期间公式**
//
//	期间毛利 = 期间营收 − 期间采购 + (期末冷库 − 期初冷库) × 期间采购均价
//
//	**只在这个函数里**算 ⇒ 不导出就没法从外部测试包锁它。
//	(IO 部分——逐日事实与冷库链——留给端到端场景。)
func BuildLongTermMarginReportForTest(branchID, from, to string, rows []LongTermMarginDay,
	openingCold, closingCold decimal.Decimal) *LongTermMarginReport {
	return buildLongTermMarginReport(branchID, from, to, rows, openingCold, closingCold)
}

// FocusCutsForPigForTest 暴露 [focusCutsForPig](LLM 只补**缺销售数据**的部位)。
//
// ⚠️ 为什么值得单独暴露:「哪些部位缺销售数据」这条判定
//
//	(本店在售部位 − 该体重段基线已覆盖部位)
//
//	**只在这个函数里**发生 ⇒ 不导出就没法从外部测试包锁它
//	⇒ owner 2026-11-01 D8「LLM 只补缺销售数据的部位」会被静默破坏
//	⇒ 而破坏的表征是"LLM 又开始问整副猪了"(没人看得出来,只是慢一点、贵一点)。
func FocusCutsForPigForTest(ctx context.Context, s *Service, branchID, band, mode string) (
	[]model.CutType, error,
) {
	return s.focusCutsForPig(ctx, branchID, band, mode)
}

// BuildSystemPromptForTest 暴露 [buildSystemPrompt](提示词只列焦点部位)。
//
// ⚠️ 为什么值得单独暴露:owner D8 的**可观测面**就是提示词本身 ——
//	「不再产整猪切割方案」= 提示词里**不再出现全集**。
func BuildSystemPromptForTest(focus []model.CutType) string {
	return buildSystemPrompt(focus)
}

// CutChainAnchorForTest 暴露 [cutChainAnchor](成本链的**锚点**查找)。
//
// ⚠️ 为什么值得单独暴露:锚点是"有已物化快照 ⇒ 直接拿前一日结余当期初"的**快路径**。
//
//	2026-11-01 生产实测发现它**从来没生效过** —— 查锚点时只 SELECT 了日期列,
//	却用 `ID == ""` 判断"到底查到没查到行" ⇒ ID 永远是零值 ⇒ 锚点恒为"不存在"
//	⇒ 每次请求都从链起点**逐日重算**整条链(每多一天 = 一次远程销售查询)
//	⇒ 毛利看板耗时随日期**线性增长**(店 00:10-30 ≈1.2s / 11-01 ≈2.0s / 11-05 ≈3.3s)。
//
//	⚠️ 这个 bug **数字全对、只是慢** ⇒ 端到端断言"数字对"永远抓不到它
//	⇒ 只能**直接锁锚点是否被找到**。
//
// 返回 (锚点日期, 是否找到)。
func CutChainAnchorForTest(ctx context.Context, s *Service, branchID string, from time.Time) (string, bool, error) {
	day, prev, err := s.cutChainAnchor(ctx, branchID, from)
	if err != nil {
		return "", false, err
	}
	if prev == nil {
		return "", false, nil
	}
	return day.In(s.bizTZOrUTC()).Format("2006-01-02"), true, nil
}

// CutChainBaselineForTest 暴露 [cutChainBaseline](链的起点)。
//
// ⚠️ 为什么值得单独暴露:链起点决定"回溯时从哪天开始算" ⇒ 它错了,看板就慢一截
//	(或者把上线前的旧账接回链里)。
//
//	同 [CutChainAnchorForTest]:2026-11-01 修掉了"只 SELECT 日期列却拿 ID 判空"的 bug
//	⇒ 这个函数**曾经**永远拿不到"最早快照日"那个兜底。
//
// 返回 (起点日期, 是否找到)。
func CutChainBaselineForTest(ctx context.Context, s *Service, branchID string) (string, bool, error) {
	day, err := s.cutChainBaseline(ctx, branchID)
	if err != nil {
		return "", false, err
	}
	return day, day != "", nil
}

// ColdStoreBySkuForTest 暴露 [deriveColdStoreBySku](按 SKU 推导的冷库余额)。
//
// ⚠️ 为什么值得单独暴露:「按 SKU 推导的冷库余额」与「按部位推导的余额链」
//
//	**必须逐位一致** —— 毛利看板用按部位的那条,搬台页用按 SKU 的这条
//	⇒ 不一致 ⇒ 门店对着两个数字对不上账。
//
//	两条都在私有方法里 ⇒ 外部测试包没法直接比 ⇒ 这里给个入口。
//
// 返回:SKU → 冷库余额(kg)。
func ColdStoreBySkuForTest(ctx context.Context, s *Service, branchID string) (
	map[string]decimal.Decimal, error,
) {
	return s.deriveColdStoreBySku(ctx, branchID)
}

// BuildUserPromptForTest 暴露 [buildUserPrompt](user 侧点名要补的部位)。
func BuildUserPromptForTest(pig *model.WholePig, history []model.WholePig, focus []model.CutType) string {
	return buildUserPrompt(pig, history, focus)
}

// CounterBySkuForTest 暴露 [deriveCounterBySku](按 SKU 推导的柜台余额)。
//
// ⚠️ 为什么值得单独暴露:柜台余额改按记录层推导之后,「位置表台账」与「推导值」
//
//	必须能**分别**断言 —— 否则测出来的只是"某一个数",抓不到口径被改回去。
//
// 返回:SKU → 柜台余额(kg)。
func CounterBySkuForTest(ctx context.Context, s *Service, branchID string) (
	map[string]decimal.Decimal, error,
) {
	return s.deriveCounterBySku(ctx, branchID)
}

// SetCounterDeriveDisabledForTest 打开/关闭**柜台侧推导**(判别力实测用)。
//
// ⚠️ 开关必须是**运行期变量**(包级 var)—— 写成 `if false` 会被 Go 编译器
//
//	把整个分支消掉 ⇒ "去掉实现"其实没去掉 ⇒ 判别力实测是假的。
func SetCounterDeriveDisabledForTest(disabled bool) {
	counterDeriveDisabled = disabled
}

// SetCounterWindowIncludeAnchorForTest 打开/关闭「窗口包含锚点当天」(判别力实测用)。
//
// ⚠️ 同上:必须是**运行期变量**。
func SetCounterWindowIncludeAnchorForTest(on bool) {
	counterWindowIncludeAnchor = on
}
