package service_test

// 2026-10-31:分割基线的**单价**通道 —— 单价日样本 → 单价基线 → 建议成本单价。
//
// # 锁的是什么(owner 的口径,不是实现细节)
//
//	① **倒推**:进货单价 = 实际销售单价 × (1 − 当日整体毛利率);
//	② **没有销售记录的部位不产样本**(搭赠等 ⇒ 该部位保持"无分割单价基线");
//	③ 整体毛利率越界(≤0 或 ≥0.95)⇒ **整天不产样本**(宁缺毋滥,别把基线教歪);
//	④ 单价基线 = 多日样本**按销量加权**;同时记下**参考猪进价**(缩放基准);
//	⑤ **应用基线时结合单猪采购价按比例缩放**(owner 原话:「基线是 14.6/kg 的,
//	   那 16/kg 的猪每个部位单价都要按比例上涨」);
//	⑥ **断环**:算某营业日的建议成本单价时,单价基线**只用该日之前**的样本
//	   ⇒ 当天的建议不依赖当天的事实 ⇒ 结构性不自证;
//	⑦ **边界②**(owner 给定):各部位合计价值 **≤ 猪的总进货金额**;
//	⑧ 退取链:没有基线 ⇒ 门店整店加权平均价 ⇒ 再没有 ⇒ LLM 缓存单价。
//
// ⚠ 这些用例都走**真实 SQLite** + 公开/导出 API ⇒ 引擎接线错了也会红。

import (
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/shopspring/decimal"
)

// priceDay 是本文件用的营业日(2026-10-29 14:00 +08 = 2026-10-29 06:00 UTC)。
var priceDay = time.Date(2026, 10, 29, 6, 0, 0, 0, time.UTC)

// priceDayBefore 是 priceDay 的前一天(基线样本所在的日子)。
var priceDayBefore = time.Date(2026, 10, 28, 6, 0, 0, 0, time.UTC)

// pricePig 直接插一头猪(要精确控制进价与总进货金额 ⇒ 不用 RecordWholePig)。
func pricePig(t *testing.T, id, grossKg, unitPrice string, arrivedAt time.Time) model.WholePig {
	t.Helper()
	kg := d(grossKg)
	pig := model.WholePig{
		ID:                    id,
		EarTag:                "E-" + id,
		GrossWeightKg:         kg,
		PurchaseUnitPriceYuan: d(unitPrice),
		PurchaseCostYuan:      kg.Mul(d(unitPrice)),
		SupplierID:            "SUP-1",
		BranchID:              "S001",
		ArrivedAt:             arrivedAt,
		RecordedBy:            "u-1",
		DataSource:            model.DataSourceStub,
		AdviceStatus:          model.AdviceStatusReady,
		AdviceMode:            model.AdviceModeAuto,
	}
	mustNoErr(t, testDBSingleton.Create(&pig).Error, "insert pig "+id)
	return pig
}

// priceSample 直接插一条**单价日样本**(省掉"先跑引擎"的麻烦)。
func priceSample(t *testing.T, bizDate string, cut model.CutType,
	soldKg, revenue, costPrice, pigPrice string) {
	t.Helper()
	rec := model.PigCutPriceSample{
		ID:              "PCP-" + bizDate + "-" + string(cut),
		BranchID:        "S001",
		BizDate:         bizDate,
		CutType:         cut,
		SoldKg:          d(soldKg),
		RevenueYuan:     d(revenue),
		RetailPriceYuan: d(revenue).Div(d(soldKg)),
		MarginRate:      d("0.3"),
		CostPriceYuan:   d(costPrice),
		PigPriceYuan:    d(pigPrice),
	}
	mustNoErr(t, testDBSingleton.Create(&rec).Error, "insert price sample "+bizDate)
}

// priceSetup 起服务(时钟停在 priceDay)+ 配 belly 映射。
func priceSetup(t *testing.T) (*service.Service, string) {
	t.Helper()
	svc, _, _ := splitSetup(t)
	return svc, "S001"
}

// ---- ① 倒推口径 ----

// TestPrice_SampleDerivesCostPrice 锁「进货单价 = 实际销售单价 ÷ (1 + 整体毛利率)」。
//
// ⚠️ 这是 owner 2026-10-31 亲口定的口径(统计栏「日终剩余」那一格:
// 「用零售均价/(1+今日毛利率)」)⇒ 单价样本的倒推**必须**跟它一致。
//
// 数据:营收 100、销量 10kg ⇒ 零售单价 10;销售成本 70 ⇒ 整体毛利率 30%
// ⇒ 倒推的进货单价 = 10 ÷ 1.3 = **7.6923… 元/kg**。
func TestPrice_SampleDerivesCostPrice(t *testing.T) {
	svc, branch := priceSetup(t)
	pricePig(t, "PIG-1", "100", "14.6", priceDay)

	rows := []service.CutDailyCost{{
		CutType:       model.CutBelly,
		SoldKg:        d("10"),
		RevenueYuan:   d("100"),
		SalesCostYuan: d("70"),
	}}
	res, err := service.CollectCutPriceSamplesForTest(t.Context(), svc, branch, priceDay, rows)
	mustNoErr(t, err, "CollectCutPriceSamples")
	if res.Written != 1 {
		t.Fatalf("★ 应写入 1 条单价样本,got %d(%s)", res.Written, res.Message)
	}
	samples, err := svc.ListForecastPriceSamples(t.Context(), branch, "2026-10-29")
	mustNoErr(t, err, "ListForecastPriceSamples")
	if len(samples) != 1 {
		t.Fatalf("★ 应有 1 条样本,got %d", len(samples))
	}
	got := samples[0]
	if !got.RetailPriceYuan.Equal(d("10")) {
		t.Errorf("★ 零售单价应是 10,got %s", got.RetailPriceYuan)
	}
	if !got.MarginRate.Equal(d("0.3")) {
		t.Errorf("★ 整体毛利率应是 0.3,got %s", got.MarginRate)
	}
	if !priceEq(got.CostPriceYuan, d("10").Div(d("1.3"))) {
		t.Errorf("★ 倒推进货单价应是 10 ÷ 1.3(owner 口径),got %s", got.CostPriceYuan)
	}
}

// priceEq 按**列精度**(4 位)比对单价。
//
// ⚠️ 为什么不能直接用 Equal(2026-10-31 实测踩到的坑):
//
//	样本是**从库里读回来**的(不是内存里的计算结果)
//	⇒ SQLite 的 decimal(12,4) 列是 NUMERIC 亲和 ⇒ 存进去的是 **float64**
//	⇒ 读回来 = 7.6923076923076925,而精确值 = 7.6923076923076923(差 2e-16)
//	⇒ Equal 是**精确比较** ⇒ 必然不相等 ⇒ 假红。
//
// ⚠️ 生产是 PG(decimal(12,4) = 真 numeric)⇒ 落库就是 7.6923
//
//	⇒ 两边都按 4 位比 ⇒ 单测与生产**同一个口径**。
func priceEq(got, want decimal.Decimal) bool {
	return got.Round(4).Equal(want.Round(4))
}

// ---- ② 没有销售记录的部位 ⇒ 不产样本 ----

// TestPrice_NoSaleCutHasNoSample 锁「当天没卖出过的部位**不产单价样本**」。
//
// owner:「被搭赠等无记录的则保持无分割单价基线」⇒ 猪肝只搭赠没卖 ⇒ 它**不能**有样本
// ⇒ 也就没有基线 ⇒ 应用时走退取链。
func TestPrice_NoSaleCutHasNoSample(t *testing.T) {
	svc, branch := priceSetup(t)
	pricePig(t, "PIG-1", "100", "14.6", priceDay)

	rows := []service.CutDailyCost{
		{CutType: model.CutBelly, SoldKg: d("10"), RevenueYuan: d("100"), SalesCostYuan: d("70")},
		// 猪肝:只搭赠、没卖出 ⇒ 销量 0 ⇒ 不该有样本。
		{CutType: model.CutLiver, SoldKg: d("0"), RevenueYuan: d("0"), SalesCostYuan: d("0")},
	}
	res, err := service.CollectCutPriceSamplesForTest(t.Context(), svc, branch, priceDay, rows)
	mustNoErr(t, err, "CollectCutPriceSamples")
	if res.Written != 1 {
		t.Fatalf("★ 只该给卖过的 belly 产 1 条样本,got %d", res.Written)
	}
	if res.NoSaleCutCount != 1 {
		t.Errorf("★ 应该有 1 个部位被记成「没卖过」,got %d", res.NoSaleCutCount)
	}
	samples, err := svc.ListForecastPriceSamples(t.Context(), branch, "2026-10-29")
	mustNoErr(t, err, "ListForecastPriceSamples")
	for _, s := range samples {
		if s.CutType == model.CutLiver {
			t.Fatalf("★ 猪肝没卖出过 ⇒ 不该有单价样本")
		}
	}
}

// ---- ③ 毛利率越界 ⇒ 整天不产样本 ----

// TestPrice_InvalidMarginProducesNoSample 锁「整体毛利率不可用 ⇒ 整天不产样本」。
//
// 成本算不出来的那天(销售成本 ≈ 0)会让毛利率接近 100% ⇒ 倒推出的单价接近零售价
// ⇒ 这样的样本会把基线教歪 ⇒ **宁缺毋滥**。
func TestPrice_InvalidMarginProducesNoSample(t *testing.T) {
	svc, branch := priceSetup(t)
	pricePig(t, "PIG-1", "100", "14.6", priceDay)

	for _, tc := range []struct {
		name string
		rows []service.CutDailyCost
	}{
		{"毛利率为 0(成本算错)", []service.CutDailyCost{{
			CutType: model.CutBelly, SoldKg: d("10"), RevenueYuan: d("100"), SalesCostYuan: d("100"),
		}}},
		{"毛利率 95%(成本几乎为 0)", []service.CutDailyCost{{
			CutType: model.CutBelly, SoldKg: d("10"), RevenueYuan: d("100"), SalesCostYuan: d("5"),
		}}},
		{"当天没有营收", []service.CutDailyCost{{
			CutType: model.CutBelly, SoldKg: d("0"), RevenueYuan: d("0"),
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := service.CollectCutPriceSamplesForTest(t.Context(), svc, branch, priceDay, tc.rows)
			mustNoErr(t, err, "CollectCutPriceSamples")
			if res.Written != 0 {
				t.Fatalf("★ 毛利率不可用时不该产样本,got %d 条", res.Written)
			}
			if res.Message == "" {
				t.Errorf("★ 要说清为什么没产出样本(门店要看得懂)")
			}
		})
	}
}

// ---- ④ 单价基线 = 按销量加权 ----

// TestPrice_BaselineWeightedBySoldKg 锁「单价基线按**销量加权**平均」。
//
// 两天样本:10kg@7 元 与 30kg@9 元 ⇒ 基线 = (7×10 + 9×30) / 40 = **8.5 元/kg**。
func TestPrice_BaselineWeightedBySoldKg(t *testing.T) {
	svc, branch := priceSetup(t)
	priceSample(t, "2026-10-28", model.CutBelly, "10", "100", "7", "14.6")
	priceSample(t, "2026-10-29", model.CutBelly, "30", "360", "9", "16")

	n, err := svc.RecomputeCutPriceBaselines(t.Context(), branch)
	mustNoErr(t, err, "RecomputeCutPriceBaselines")
	if n != 1 {
		t.Fatalf("★ 应写出 1 条单价基线,got %d", n)
	}
	rows, err := svc.ListForecastPriceBaselines(t.Context(), branch)
	mustNoErr(t, err, "ListForecastPriceBaselines")
	if len(rows) != 1 {
		t.Fatalf("★ 应有 1 条单价基线,got %d", len(rows))
	}
	if !rows[0].UnitCostYuan.Equal(d("8.5")) {
		t.Errorf("★ 单价基线应是 8.5((7×10+9×30)/40),got %s", rows[0].UnitCostYuan)
	}
	// 参考猪进价同样按销量加权:(14.6×10 + 16×30) / 40 = 15.65
	if !rows[0].RefPigPriceYuan.Equal(d("15.65")) {
		t.Errorf("★ 参考猪进价应是 15.65,got %s", rows[0].RefPigPriceYuan)
	}
	if rows[0].SampleCount != 2 {
		t.Errorf("★ 样本数应是 2,got %d", rows[0].SampleCount)
	}
}

// ---- ⑤ 应用基线 ⇒ 按单猪采购价缩放 ----

// TestPrice_SuggestedScalesWithPigPrice 锁「结合**单猪的采购价**按比例缩放」。
//
// 基线:参考猪 14.6 元/kg 时,该部位进货单价 20 元/kg。
// 今天这头猪进 29.2 元/kg(= 2 倍)⇒ 建议成本单价应是 **40 元/kg**。
func TestPrice_SuggestedScalesWithPigPrice(t *testing.T) {
	svc, branch := priceSetup(t)
	priceSample(t, "2026-10-28", model.CutBelly, "10", "200", "20", "14.6")
	pig := pricePig(t, "PIG-1", "100", "29.2", priceDay)

	split := map[string]map[string]decimal.Decimal{
		pig.ID: {string(model.CutBelly): d("10")},
	}
	unit, source, err := service.SuggestedCutUnitPricesForTest(
		t.Context(), svc, branch, []model.WholePig{pig}, split, priceDay)
	mustNoErr(t, err, "SuggestedCutUnitPrices")
	got, ok := unit[string(model.CutBelly)]
	if !ok {
		t.Fatalf("★ 应该有 belly 的建议成本单价,got %v", unit)
	}
	if !got.Equal(d("40")) {
		t.Errorf("★ 建议成本单价应是 40(20 × 29.2/14.6),got %s", got)
	}
	if source[string(model.CutBelly)] != service.PriceSourceBaseline {
		t.Errorf("★ 来源应是 baseline,got %q", source[string(model.CutBelly)])
	}
}

// ---- ⑥ 断环:当天的样本不影响当天的建议 ----

// TestPrice_AsOfBreaksCircularDependency 锁「**当天的事实不影响当天的建议**」。
//
// ⚠️ 这是 owner 点名的「数据依赖循环问题」的**结构性**解法:
//
//	单价的倒推需要当日毛利率 ⇒ 毛利率来自成本链
//	⇒ 若基线把当天自己的样本算进去,就成了"今天的毛利率 → 今天的单价 → 今天的毛利率"。
//
//	⇒ 算 D 日的建议时,基线**只用 < D 的样本**。
//
// 断言:昨天样本给出 20;今天再插一条 999 的样本 ⇒ 今天的建议**仍然是 20**(不含缩放)。
func TestPrice_AsOfBreaksCircularDependency(t *testing.T) {
	svc, branch := priceSetup(t)
	priceSample(t, "2026-10-28", model.CutBelly, "10", "200", "20", "14.6")
	// ⚠️ 猪要**够大**(1000kg @ 14.6 = 14600 元)⇒ 下面算出来的荒谬单价
	//   (999 元/kg × 10kg = 9990)才不会先被"合计价值 ≤ 猪总进货额"封顶
	//   ⇒ 断言测的是**断环**本身,不是封顶。
	pig := pricePig(t, "PIG-1", "1000", "14.6", priceDay)
	split := map[string]map[string]decimal.Decimal{
		pig.ID: {string(model.CutBelly): d("10")},
	}

	unit, _, err := service.SuggestedCutUnitPricesForTest(
		t.Context(), svc, branch, []model.WholePig{pig}, split, priceDay)
	mustNoErr(t, err, "SuggestedCutUnitPrices")
	if !unit[string(model.CutBelly)].Equal(d("20")) {
		t.Fatalf("★ 昨天样本的基线单价应是 20,got %s", unit[string(model.CutBelly)])
	}

	// ⚠️ 今天的样本(荒谬的 999)⇒ **不许**影响今天的建议。
	priceSample(t, "2026-10-29", model.CutBelly, "10", "9990", "999", "14.6")
	unit2, _, err := service.SuggestedCutUnitPricesForTest(
		t.Context(), svc, branch, []model.WholePig{pig}, split, priceDay)
	mustNoErr(t, err, "SuggestedCutUnitPrices")
	if !unit2[string(model.CutBelly)].Equal(d("20")) {
		t.Fatalf("★ ★ 当天的样本不许影响当天的建议(断环失效),got %s", unit2[string(model.CutBelly)])
	}

	// ⇒ 而**明天**看今天时,今天的样本就该参与(base = (20×10 + 999×10)/20 = 509.5)。
	tomorrow := priceDay.AddDate(0, 0, 1)
	unit3, _, err := service.SuggestedCutUnitPricesForTest(
		t.Context(), svc, branch, []model.WholePig{pig}, split, tomorrow)
	mustNoErr(t, err, "SuggestedCutUnitPrices")
	if !unit3[string(model.CutBelly)].Equal(d("509.5")) {
		t.Errorf("★ 明天应当把今天的样本算进去(509.5),got %s", unit3[string(model.CutBelly)])
	}
}

// ---- ⑦ 边界②:各部位合计价值 ≤ 猪的总进货金额 ----

// TestPrice_CapByPigPurchaseCost 锁 owner 给的第二条边界。
//
// 猪 10kg @ 10 元/kg ⇒ 总进货金额 **100 元**;
// 基线单价 1000 元/kg × 建议分割 8kg = 8000 元 ⇒ **远超** 100 元
// ⇒ 必须封顶:单价缩到 100/8 = **12.5 元/kg**。
func TestPrice_CapByPigPurchaseCost(t *testing.T) {
	svc, branch := priceSetup(t)
	priceSample(t, "2026-10-28", model.CutBelly, "10", "10000", "1000", "10")
	pig := pricePig(t, "PIG-1", "10", "10", priceDay) // 总进货金额 = 100 元

	split := map[string]map[string]decimal.Decimal{
		pig.ID: {string(model.CutBelly): d("8")},
	}
	unit, _, err := service.SuggestedCutUnitPricesForTest(
		t.Context(), svc, branch, []model.WholePig{pig}, split, priceDay)
	mustNoErr(t, err, "SuggestedCutUnitPrices")
	got := unit[string(model.CutBelly)]
	// 封顶后:单价 × 8kg = 100 元 ⇒ 单价 = 12.5
	if !got.Equal(d("12.5")) {
		t.Errorf("★ 各部位合计价值不得超过猪的总进货金额 ⇒ 单价应是 12.5(100/8),got %s", got)
	}
	if got.Mul(d("8")).GreaterThan(pig.PurchaseCostYuan) {
		t.Errorf("★ 合计价值 %s 超过了猪的总进货金额 %s",
			got.Mul(d("8")), pig.PurchaseCostYuan)
	}
}

// ---- ⑧ 退取链 ----

// TestPrice_FallbackToStoreWeightedAverage 锁退取链①。
//
// 部位没有单价基线(那天没卖过)⇒ 用**门店整店加权平均价**兜底。
func TestPrice_FallbackToStoreWeightedAverage(t *testing.T) {
	svc, branch := priceSetup(t)
	// belly 有样本(8 元/kg)⇒ 整店加权平均价 = 8。
	priceSample(t, "2026-10-28", model.CutBelly, "10", "80", "8", "14.6")
	pig := pricePig(t, "PIG-1", "100", "14.6", priceDay)

	split := map[string]map[string]decimal.Decimal{
		pig.ID: {string(model.CutLiver): d("3")},
	}
	unit, source, err := service.SuggestedCutUnitPricesForTest(
		t.Context(), svc, branch, []model.WholePig{pig}, split, priceDay)
	mustNoErr(t, err, "SuggestedCutUnitPrices")
	got := unit[string(model.CutLiver)]
	if !got.Equal(d("8")) {
		t.Errorf("★ 没有基线时应退取门店整店加权平均价 8,got %s", got)
	}
	if source[string(model.CutLiver)] != service.PriceSourceStoreAvg {
		t.Errorf("★ 来源应是 store_avg,got %q", source[string(model.CutLiver)])
	}
}

// TestPrice_FallbackToLLMCache 锁退取链②。
//
// 店里**一个样本都没有**(全新店)⇒ 整店均价也取不到 ⇒ 用 **LLM 缓存单价**。
func TestPrice_FallbackToLLMCache(t *testing.T) {
	svc, branch := priceSetup(t)
	mustNoErr(t, testDBSingleton.Create(&model.CutUnitPriceCache{
		ID:            "CUP-S001-liver",
		BranchID:      branch,
		CutType:       model.CutLiver,
		UnitPriceYuan: d("30"),
		Source:        "llm",
	}).Error, "insert cut unit price cache")
	pig := pricePig(t, "PIG-1", "100", "14.6", priceDay)

	split := map[string]map[string]decimal.Decimal{
		pig.ID: {string(model.CutLiver): d("3")},
	}
	unit, source, err := service.SuggestedCutUnitPricesForTest(
		t.Context(), svc, branch, []model.WholePig{pig}, split, priceDay)
	mustNoErr(t, err, "SuggestedCutUnitPrices")
	got := unit[string(model.CutLiver)]
	if !got.Equal(d("30")) {
		t.Errorf("★ 没有基线也没有整店均价时应退取 LLM 缓存单价 30,got %s", got)
	}
	if source[string(model.CutLiver)] != service.PriceSourceLLMCache {
		t.Errorf("★ 来源应是 llm_cache,got %q", source[string(model.CutLiver)])
	}
}

// TestPrice_NoPriceAtAllIsOmitted 锁「三步退取链都走不通 ⇒ 该部位**没有**建议成本单价」。
//
// ⚠️ 必须是"**没有这个键**"(nil),不是 0 —— 0 会被门店误读成"这个部位值 0 元/kg"。
func TestPrice_NoPriceAtAllIsOmitted(t *testing.T) {
	svc, branch := priceSetup(t)
	pig := pricePig(t, "PIG-1", "100", "14.6", priceDay)
	split := map[string]map[string]decimal.Decimal{
		pig.ID: {string(model.CutLiver): d("3")},
	}
	unit, source, err := service.SuggestedCutUnitPricesForTest(
		t.Context(), svc, branch, []model.WholePig{pig}, split, priceDay)
	mustNoErr(t, err, "SuggestedCutUnitPrices")
	if _, ok := unit[string(model.CutLiver)]; ok {
		t.Fatalf("★ 取不到价时该部位不该有建议成本单价,got %v", unit)
	}
	if source[string(model.CutLiver)] != service.PriceSourceNone {
		t.Errorf("★ 来源应是 none,got %q", source[string(model.CutLiver)])
	}
}
