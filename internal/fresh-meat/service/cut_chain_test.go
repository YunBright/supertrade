// Package service_test —— **成本链逐日接续**的不变量(2026-10-27)。
//
// # 为什么锁这几条
//
// 成本链的权威载体是 `cut_cost_daily`(上一营业日的部位结余),
// 但那张表**只在审计结转时被固化** ⇒ 零盘点门店的链**每天断一次**
// ⇒ 成本塌成"期初那点残值" ⇒ 毛利虚高。
//
// 生产实测(店 00,2026-10-27):
//
//	09-01 ~ 09-30  成本恒 0    ⇒ 期间毛利看起来 = 期间营收(100% 毛利)
//	10-03 ~ 10-27  成本 ~100   ⇒ 毛利率 97%(链又断回期初残值)
//
// ⇒ 锁三条:
//
//	① 链必须**逐日接续**:次日期初 == 上日期末(kg 与金额**精确相等**)。
//	② **没有基线**的天,成本是"算不出来",不是"成本为 0" ⇒ 必须显式标注。
//	③ **长期毛利 = 每日毛利之和**(链式路径下也要成立)。
package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cuberead/cubereadfake"
	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/shopspring/decimal"
)

// fmChainSetup 起一个"有源门店"的毛利环境,**销售按日期返回**(链式回溯每天都要问一次)。
//
// ⚠️ 假身必须按 oper_date 返回:成本链现在**逐日回溯**(基线日 → 今天),
//   每一天都会问一次 cube。若对任何日期都返回同一批销售 ⇒ 销售被重复计入每一天。
func fmChainSetup(t *testing.T, rowsByDate map[string][]map[string]any) (*service.Service, context.Context) {
	t.Helper()
	svc, _ := setupTestService(t)
	ctx := context.Background()
	if _, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-BELLY"}); err != nil {
		t.Fatalf("CreateBranchCutMapping: %v", err)
	}
	cube := &cubereadfake.Client{}
	cube.Handler = func(m string, q cubeclient.CubeQuery) ([]map[string]any, int, error) {
		if m != "sales_report" {
			return nil, 0, nil
		}
		rows := rowsByDate[fmCubeOperDate(q)]
		return rows, len(rows), nil
	}
	svc.SetCubeReader(cube)
	svc.SetSourceResolver(fmFakeResolver{src: cubesource.Source{
		BranchID: "S001", Configured: true, Enabled: true, CubeSourceName: "cube-sixun-ysx-fb",
	}})
	return svc, ctx
}

// TestCutChain_ConsecutiveDays_CarryOpeningExactly 锁 ①。
//
// # 场景
//
//	基线日 10-01:门店录 belly 100kg @ 18 元/kg。
//	10-02 / 10-03 / 10-04 各卖 10kg。
//
// # 期望
//
//	10-02 期初 = 100kg(基线日,门店录的值)
//	10-03 期初 == 10-02 期末(kg 与金额**精确相等**)
//	10-04 期初 == 10-03 期末(kg 与金额**精确相等**)
//	⇒ 链没断 ⇒ 成本不会塌成"期初那点残值"。
//
// ⚠️ 旧行为(链每天断):10-03 的期初会从"门店期初结转"重新起算 ⇒ 恒为 100kg
//   ⇒ 卖了两天还是 100kg ⇒ 账面应有虚高 ⇒ 成本被稀释 ⇒ 毛利虚高。
func TestCutChain_ConsecutiveDays_CarryOpeningExactly(t *testing.T) {
	day2 := fmDay1.Add(24 * time.Hour) // 2026-10-02
	day3 := fmDay1.Add(48 * time.Hour) // 2026-10-03
	day4 := fmDay1.Add(72 * time.Hour) // 2026-10-04

	svc, ctx := fmChainSetup(t, map[string][]map[string]any{
		"2026-10-02": {fmCubeSalesRow("P-BELLY", "10", "180")},
		"2026-10-03": {fmCubeSalesRow("P-BELLY", "10", "180")},
		"2026-10-04": {fmCubeSalesRow("P-BELLY", "10", "180")},
	})
	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-01",
			ColdStoreKg: d("100"), UnitPriceYuan: d("18")},
	}); err != nil {
		t.Fatalf("SaveStockOpenings: %v", err)
	}

	// 逐日算(每天都是"看当天"的视角 ⇒ 每天的期初都该接上昨天)。
	// ⚠️ 本场景只有一个部位(belly)⇒ 全店口径 = 该部位口径。
	var prevKg, prevClosingAmt decimal.Decimal
	for _, day := range []time.Time{day2, day3, day4} {
		svc.SetClock(fmClock(day))
		rep, err := svc.ComputeGrossMargin(ctx, "S001", day)
		mustNoErr(t, err, "ComputeGrossMargin")
		row := fmCutRowFor(t, rep, model.CutBelly)

		if !prevKg.IsZero() {
			// ★ 次日期初 kg == 上日期末 kg(精确)。
			if !row.OpeningKg.Equal(prevKg) {
				t.Errorf("★ %s 的期初 kg 应精确接上上一天的期末 %s, got %s —— 链断了",
					day.Format("01-02"), prevKg, row.OpeningKg)
			}
			// ★ 次日期初金额 == 上日期末金额(精确)。
			if !rep.Reconciliation.OpeningInventoryCostYuan.Equal(prevClosingAmt) {
				t.Errorf("★ %s 的期初金额应精确接上上一天的期末金额 %s, got %s —— 链断了",
					day.Format("01-02"), prevClosingAmt, rep.Reconciliation.OpeningInventoryCostYuan)
			}
		} else {
			// 基线日之后的第一天:期初 = 门店录的 100kg。
			if !row.OpeningKg.Equal(d("100")) {
				t.Errorf("★ 基线日之后的期初应是门店录的 100kg, got %s", row.OpeningKg)
			}
		}
		// 卖出的货必须带成本(不是零成本 ⇒ 不是毛利虚高)。
		if !row.SalesCostYuan.IsPositive() {
			t.Errorf("★ %s 的销售成本应 > 0(卖出的货要带成本), got %s",
				day.Format("01-02"), row.SalesCostYuan)
		}
		prevKg = row.ExpectedRemainKg
		prevClosingAmt = rep.Reconciliation.ClosingInventoryCostYuan
	}
}

// TestCutChain_WritePathMaterializesChain 锁"写路径会把链物化"。
//
// # 为什么重要
//
// 成本链是**逐日接续**的 ⇒ 录猪/删猪/大宗/搬上台之后,后面所有天的成本都要跟着变。
// ⇒ 写路径必须把链重算一遍并落库,否则门店看到的成本是旧的。
//
// ⚠️ **不要求当天有盘点单** —— 盘点只是"实盘锚定",不是"链的开关"。
//   (旧口径把落库绑在盘点上 ⇒ 零盘点门店的链每天断一次。)
//
// # 判别力
//
// 摘掉写路径里的 ScheduleChainMaterialize 调用 ⇒ 本用例立即变红。
func TestCutChain_WritePathMaterializesChain(t *testing.T) {
	day2 := fmDay1.Add(24 * time.Hour) // 2026-10-02(基线日 10-01 不能录猪)
	svc, ctx := fmChainSetup(t, map[string][]map[string]any{})
	// 注入**同步**物化执行器 ⇒ 断言是确定性的(不用 sleep 轮询)。
	svc.SetChainMaterializer(func(f func()) { f() })
	svc.SetClock(fmClock(day2))

	// 基线:门店录 belly 100kg @ 18(链的起点)。
	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-01",
			ColdStoreKg: d("100"), UnitPriceYuan: d("18")},
	}); err != nil {
		t.Fatalf("SaveStockOpenings: %v", err)
	}

	// 录猪 + 分割(写路径 ⇒ 触发链物化)。
	pig, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-1", GrossWeightKg: d("50"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: day2,
	}, "u-1")
	mustNoErr(t, err, "RecordWholePig")
	_, err = svc.RecordPigCut(ctx, "S001", model.RecordPigCutInput{
		PigID: pig.ID, CutType: model.CutBelly, Barcode: "B-1",
		WeightKg: d("10"), ExpiresAt: day2.Add(48 * time.Hour), CubeProductID: "P-BELLY",
	}, "u-1")
	mustNoErr(t, err, "RecordPigCut")

	// ★ 链必须被物化(即使**没有盘点单**)。
	var rows []model.CutCostDaily
	if err := testDBSingleton.Where("branch_id = ?", "S001").
		Find(&rows).Error; err != nil {
		t.Fatalf("query cut_cost_daily: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("★ 写路径后成本链必须被物化(落 cut_cost_daily)—— " +
			"不落 ⇒ 链断在每个非盘点日 ⇒ 成本塌成期初残值 ⇒ 毛利虚高")
	}
	// 物化出来的是**有内容的**快照(不是空壳)。
	var belly *model.CutCostDaily
	for i := range rows {
		if rows[i].CutType == model.CutBelly {
			belly = &rows[i]
			break
		}
	}
	if belly == nil {
		t.Fatalf("★ 物化的快照应含 belly 行, got %+v", rows)
	}
	if belly.OpeningKg.IsZero() && belly.ProducedKg.IsZero() {
		t.Errorf("★ 物化的快照是空壳(期初/产出全是 0): %+v", *belly)
	}
}

// TestCutChain_ShortSell_UnitCarriesOverSoCostNotZero 锁"短卖之后成本不能塌成 0"。
//
// # 场景(2026-10-27 生产实测的症状)
//
//	基线 belly 10kg @ 18;10-02 卖 20kg ⇒ 账面应有 −10kg(**账面短卖**)。
//	10-03 再卖 5kg ⇒ "可售数量" = 期初(−15) + 产出(0) = **负数**
//	⇒ 移动加权单价算不出来(除以非正数)。
//
// # 期望
//
//	① 10-03 的**销售成本 > 0** —— 链必须把上期的移动加权单价带下来
//	   ⇒ 短卖那部分照上期成本结转。
//	② 对账差额 = 0。
//
// ⚠️ 旧行为(链不带单价):10-03 起销售成本恒 0 ⇒ **毛利率 100%**(虚高)
//   且对账差额 −335.70(生产实测)。
func TestCutChain_ShortSell_UnitCarriesOverSoCostNotZero(t *testing.T) {
	day3 := fmDay1.Add(48 * time.Hour) // 2026-10-03

	svc, ctx := fmChainSetup(t, map[string][]map[string]any{
		"2026-10-02": {fmCubeSalesRow("P-BELLY", "20", "360")},
		"2026-10-03": {fmCubeSalesRow("P-BELLY", "5", "90")},
	})
	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-01",
			ColdStoreKg: d("10"), UnitPriceYuan: d("18")},
	}); err != nil {
		t.Fatalf("SaveStockOpenings: %v", err)
	}

	svc.SetClock(fmClock(day3))
	rep, err := svc.ComputeGrossMargin(ctx, "S001", day3)
	mustNoErr(t, err, "ComputeGrossMargin(10-03)")
	row := fmCutRowFor(t, rep, model.CutBelly)

	// ① ★ 成本必须 > 0(不能塌成 0 ⇒ 不能是 100% 毛利)。
	if !row.SalesCostYuan.IsPositive() {
		t.Errorf("★ 10-03 的销售成本应 > 0(短卖照上期成本结转), got %s —— "+
			"0 ⇒ 卖超的部分白送 ⇒ 毛利 100%%(虚高)", row.SalesCostYuan)
	}
	// 短卖 10kg 之后的账面应有仍是负数(不许裁 0)。
	if !row.ExpectedRemainKg.IsNegative() {
		t.Errorf("★ 账面应是负数(短卖), got %s", row.ExpectedRemainKg)
	}
	// ② 对账差额 = 0。
	if rep.Reconciliation.DiffYuan.Abs().GreaterThan(d("0.01")) {
		t.Errorf("★ 对账差额应是 0, got %s", rep.Reconciliation.DiffYuan)
	}
}

// TestGrossMargin_CostIncompleteMarked 锁"每日毛利也要标出'成本算不出来'"。
//
// ⚠️ 生产实测(店 00 的 10-01):当期只有 9.23kg 有价、其余部位卖了很多却没基线
//   ⇒ 销售成本 120.41 元、毛利率 96.84% ⇒ 店长会以为赚疯了。
//   ⇒ 每日毛利接口必须**显式标注**,不能只靠"成本是 0"让人自己猜。
func TestGrossMargin_CostIncompleteMarked(t *testing.T) {
	day2 := fmDay1.Add(24 * time.Hour) // 2026-10-02
	// ⚠️ **不录期初结转** ⇒ 没有基线 ⇒ 成本算不出来。
	svc, ctx := fmChainSetup(t, map[string][]map[string]any{
		"2026-10-02": {fmCubeSalesRow("P-BELLY", "10", "180")},
	})
	svc.SetClock(fmClock(day2))

	rep, err := svc.ComputeGrossMargin(ctx, "S001", day2)
	mustNoErr(t, err, "ComputeGrossMargin")

	if !rep.CostIncomplete {
		t.Errorf("★ 没有基线时每日毛利必须标 cost_incomplete=true, got false —— "+
			"店长会看到「毛利 100%%」却没有任何提示")
	}
	if rep.CostIncompleteNote == "" {
		t.Error("★ 必须给出人话说明(cost_incomplete_note),否则门店不知道为什么")
	}
	row := fmCutRowFor(t, rep, model.CutBelly)
	if !row.CostIncomplete {
		t.Errorf("★ 部位行也要标 cost_incomplete, got false: %+v", row)
	}
}

// TestCutChain_UncomputableCut_DoesNotPoisonNextDay 锁"成本算不出来的部位不得污染次日"。
//
// # 场景(2026-10-27 生产实测的症状)
//
//	belly 在 10-02 **既没有期初库存、也没有进货**,但卖了货
//	⇒ 它的成本算不出来 ⇒ 当天落库的期末金额 = 期末 kg × 单价 0 = **0**
//	(0 不是真实价值,是"不知道")。
//
//	10-03 若把那个 0 当成上日期末接过来
//	⇒ 可售金额被抹平、可售数量被压小 ⇒ **移动加权单价爆炸**
//	⇒ 实测 10-02 的成本从 3061.57 变成 10151.24、毛利率 −191%。
//
// # 期望
//
//	① 快照里该部位带 `cost_uncomputable` 标记(链的断点说明)。
//	② 10-03 的该部位期初 = 0(链在那里自然断开)⇒ 单价不爆炸、成本合理。
func TestCutChain_UncomputableCut_DoesNotPoisonNextDay(t *testing.T) {
	day2 := fmDay1.Add(24 * time.Hour) // 2026-10-02
	day3 := fmDay1.Add(48 * time.Hour) // 2026-10-03

	svc, ctx := fmChainSetup(t, map[string][]map[string]any{
		"2026-10-02": {fmCubeSalesRow("P-BELLY", "10", "180")},
		"2026-10-03": {fmCubeSalesRow("P-BELLY", "4", "72")},
	})
	// 注入**同步**物化执行器 ⇒ 快照立刻落库 ⇒ 能验证"读快照接链"这条路。
	svc.SetChainMaterializer(func(f func()) { f() })
	svc.SetClock(fmClock(day2))

	// 录猪 + 分割(触发物化);注意猪在 belly 上有产出 ⇒ belly **有**产出。
	// 这里要的是"没有基线"的部位,所以用另一个部位?——不:直接用 belly 的
	// "成本算不出来"分支需要既无期初也无产出 ⇒ 这里改用**只卖不进货**的一天。
	// ⇒ 因此不录猪,让 belly 在 10-02 只卖不进货(成本算不出来)。
	// 物化由查询后的显式调用触发。
	_ = ctx

	// 显式物化 10-02(落快照)。
	if _, err := svc.MaterializeCostChain(context.Background(), "S001", "2026-10-01"); err != nil {
		t.Fatalf("MaterializeCostChain: %v", err)
	}

	// ① 快照里 belly 必须带 cost_uncomputable 标记。
	var snap model.CutCostDaily
	if err := testDBSingleton.Where("branch_id = ? AND cut_type = ? AND biz_date LIKE ?",
		"S001", model.CutBelly, "2026-10-02%").First(&snap).Error; err != nil {
		t.Fatalf("查 10-02 的 belly 快照: %v", err)
	}
	if !snap.CostUncomputable {
		t.Errorf("★ 成本算不出来的部位必须在快照里带 cost_uncomputable 标记 —— "+
			"否则次日会把那个 0 当成真实结余接过去 ⇒ 单价爆炸(实测成本 3061 → 10151)")
	}

	// ② 10-03 的 belly 期初 = 0(链在那里自然断开)⇒ 成本不爆炸。
	svc.SetClock(fmClock(day3))
	rep, err := svc.ComputeGrossMargin(ctx, "S001", day3)
	mustNoErr(t, err, "ComputeGrossMargin(10-03)")
	row := fmCutRowFor(t, rep, model.CutBelly)
	if !row.OpeningKg.IsZero() {
		t.Errorf("★ 10-03 的 belly 期初应是 0(上一天的成本算不出来 ⇒ 链在那里断开), got %s —— "+
			"接了那个 0 就会把可售数量压小 ⇒ 单价爆炸", row.OpeningKg)
	}
	if row.SalesCostYuan.GreaterThan(d("1000")) {
		t.Errorf("★ 10-03 的 belly 销售成本应是合理量级, got %s —— 单价爆炸了", row.SalesCostYuan)
	}
}

// TestCutChain_NoBaseline_CostMarkedUncomputable 锁 ②。
//
// # 场景
//
//	**没有任何基线**:既没有门店期初结转,当天也没有进货 —— 只有销售。
//	(生产真实形态:店 00 从 9 月起就是这个样子。)
//
// # 期望
//
//	① 期间毛利报告里 days_without_cost_baseline ≥ 1。
//	② 那一行的 cost_incomplete = true。
//	③ Note 里说清楚"成本算不出来"⇒ **不能**让门店看到"毛利 100%"却没人提示。
func TestCutChain_NoBaseline_CostMarkedUncomputable(t *testing.T) {
	day2 := fmDay1.Add(24 * time.Hour) // 2026-10-02
	svc, ctx := fmChainSetup(t, map[string][]map[string]any{
		"2026-10-02": {fmCubeSalesRow("P-BELLY", "10", "180")},
	})
	svc.SetClock(fmClock(day2))

	rep, err := svc.ComputePeriodMargin(ctx, "S001", "2026-10-02", "2026-10-02", "day")
	mustNoErr(t, err, "ComputePeriodMargin")

	if rep.DaysWithoutCostBaseline < 1 {
		t.Errorf("★ 没有基线的那天必须被标出来(days_without_cost_baseline ≥ 1), got %d",
			rep.DaysWithoutCostBaseline)
	}
	if !rep.CostIncomplete {
		t.Errorf("★ 区间里有成本算不出来 ⇒ 必须标 cost_incomplete=true, got false —— "+
			"店长会把「毛利 100%%」当真")
	}
	if len(rep.Rows) == 0 {
		t.Fatalf("期间毛利应至少有一行: %+v", rep)
	}
	if !rep.Rows[0].CostIncomplete {
		t.Errorf("★ 没有基线的那行必须 cost_incomplete=true(成本算不出来 ≠ 成本为 0), got %+v",
			rep.Rows[0])
	}
	if rep.Note == "" {
		t.Error("★ Note 必须说明\"成本算不出来\",否则门店会以为毛利 100% 是真的")
	}
}

// TestCutChain_PeriodMarginEqualsSumOfDaily 锁 ③。
//
// **长期毛利 = 每日毛利之和**(链式路径下也要成立)。
//
// ⚠️ 这条是"每日毛利"与"长期毛利"两套代码的唯一交叉校验:
//   两者若各自算各自的,门店会看到"这个月加起来 ≠ 每天加起来"。
func TestCutChain_PeriodMarginEqualsSumOfDaily(t *testing.T) {
	day2 := fmDay1.Add(24 * time.Hour)
	day3 := fmDay1.Add(48 * time.Hour)
	day4 := fmDay1.Add(72 * time.Hour)

	svc, ctx := fmChainSetup(t, map[string][]map[string]any{
		"2026-10-02": {fmCubeSalesRow("P-BELLY", "10", "180")},
		"2026-10-03": {fmCubeSalesRow("P-BELLY", "10", "180")},
		"2026-10-04": {fmCubeSalesRow("P-BELLY", "10", "180")},
	})
	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-01",
			ColdStoreKg: d("100"), UnitPriceYuan: d("18")},
	}); err != nil {
		t.Fatalf("SaveStockOpenings: %v", err)
	}

	// 每日毛利之和。
	var sumDaily, sumDailyRev decimal.Decimal
	for _, day := range []time.Time{day2, day3, day4} {
		svc.SetClock(fmClock(day))
		rep, err := svc.ComputeGrossMargin(ctx, "S001", day)
		mustNoErr(t, err, "ComputeGrossMargin")
		sumDaily = sumDaily.Add(rep.Total.GrossMarginYuan)
		sumDailyRev = sumDailyRev.Add(rep.Total.RevenueYuan)
	}

	// 长期毛利。
	svc.SetClock(fmClock(day4))
	period, err := svc.ComputePeriodMargin(ctx, "S001", "2026-10-02", "2026-10-04", "day")
	mustNoErr(t, err, "ComputePeriodMargin")

	if !period.Total.RevenueYuan.Equal(sumDailyRev) {
		t.Errorf("★ 期间营收应是每日营收之和: 期间 %s vs 每日之和 %s",
			period.Total.RevenueYuan, sumDailyRev)
	}
	// 金额有 1e-16 级尾数(移动加权单价是除法结果)⇒ 用容差比。
	if period.Total.GrossMarginYuan.Sub(sumDaily).Abs().GreaterThan(d("0.000001")) {
		t.Errorf("★ 长期毛利应是每日毛利之和: 期间 %s vs 每日之和 %s",
			period.Total.GrossMarginYuan, sumDaily)
	}
}
