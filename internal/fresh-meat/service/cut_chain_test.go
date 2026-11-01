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
	"strings"
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
		// ⚠️ 2026-11-01:链现在先问一次**区间查询**(gte/lte)⇒ 假身也得认区间
		//   ⇒ 统一走 fmCubeRowsForRange(逐日 / 区间两种形态都认)。
		rows, n := fmCubeRowsForRange(rowsByDate, q)
		return rows, n, nil
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
//	① 每日毛利报告 cost_incomplete = true。
//	② 有一句人话说明"成本算不出来"⇒ **不能**让门店看到"毛利 100%"却没人提示。
//	③ 长期毛利(期间)也必须标 cost_incomplete ⇒ 期间数同样不完整。
//
// ⚠️ 2026-11-01 owner：账面期间毛利端点(`/gross-margin/period`)已下线
//
//	⇒ 原用例(用 `ComputePeriodMargin` 的 days_without_cost_baseline 与行级 cost_incomplete)
//	  改由**每日毛利报告**(`ComputeGrossMargin`)+ **长期毛利**(`ComputeLongTermMargin`)承接。
func TestCutChain_NoBaseline_CostMarkedUncomputable(t *testing.T) {
	day2 := fmDay1.Add(24 * time.Hour) // 2026-10-02
	svc, ctx := fmChainSetup(t, map[string][]map[string]any{
		"2026-10-02": {fmCubeSalesRow("P-BELLY", "10", "180")},
	})
	svc.SetClock(fmClock(day2))

	// ① 每日毛利报告必须标"成本算不出来"。
	rep, err := svc.ComputeGrossMargin(ctx, "S001", day2)
	mustNoErr(t, err, "ComputeGrossMargin")
	if !rep.CostIncomplete {
		t.Errorf("★ 没有基线的那天必须标 cost_incomplete=true, got false —— " +
			"店长会把「毛利 100%%」当真")
	}
	// ② 人话说明必须非空(前端要原样显示)。
	if rep.CostIncompleteNote == "" {
		t.Error("★ CostIncompleteNote 必须说明\"成本算不出来\",否则门店会以为毛利 100% 是真的")
	}
	if len(rep.PerCut) == 0 {
		t.Fatalf("每日毛利应至少有一行: %+v", rep)
	}
	// ③ 长期毛利(期间)同样不完整 ⇒ 必须标出来。
	period, err := svc.ComputeLongTermMargin(ctx, "S001", "2026-10-02", "2026-10-02")
	mustNoErr(t, err, "ComputeLongTermMargin")
	if !period.Total.CostIncomplete {
		t.Errorf("★ 长期毛利也必须标 cost_incomplete=true(没有完整盘点 ⇒ 剩余算不出来), got false")
	}
	if period.Total.CostIncompleteNote == "" {
		t.Error("★ 长期毛利必须带一句人话说明")
	}
	if len(period.Rows) == 0 {
		t.Fatalf("长期毛利应至少有一行: %+v", period)
	}
}

// TestCutChain_LongTermMarginRevenueEqualsSumOfDaily 锁 ③。
//
// ⚠️ 2026-11-01 owner：账面期间毛利端点(`/gross-margin/period`)已下线
//
//	⇒ 原用例(锁"期间毛利 == 每日毛利之和")改由**长期毛利**(`/long-term-margin`)承接。
//
// # 口径变化(为什么断言从"毛利相等"变成"营收相等")
//
//	旧:账面期间毛利(移动加权链口径) == 每日毛利之和 —— 两数**同口径**,可直接相等。
//	新:长期毛利是**全店口径**(含冷库)⇒ **期间毛利 ≠ 每日之和** —— 这是**设计**,不是 bug
//	    (每日行故意不含冷库 ⇒ 跨期才平,见 long_term_margin.go)。
//	⇒ 仍然成立、且必须锁的交叉校验是 **营收**:
//	   期间营收 == 每日营收之和(两条路径读同一份销售流水 ⇒ 不许一个有数一个没数)。
//	⇒ 另外锁:报告自带的"每日之和" == 各日行之和(口径自洽)。
//
// # 判别力
//
//	若有人把长期毛利改成从另一张表读销售 ⇒ 营收对不上 ⇒ 立刻红。
func TestCutChain_LongTermMarginRevenueEqualsSumOfDaily(t *testing.T) {
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
	_ = sumDaily // 每日毛利与期间毛利**口径不同**(含冷库)⇒ 不可直接比;保留求和仅为留痕。

	// 长期毛利(期间)。
	svc.SetClock(fmClock(day4))
	period, err := svc.ComputeLongTermMargin(ctx, "S001", "2026-10-02", "2026-10-04")
	mustNoErr(t, err, "ComputeLongTermMargin")

	// ① 跨路径营收一致(两条路径读同一份销售流水)。
	if !period.Total.RevenueYuan.Equal(sumDailyRev) {
		t.Errorf("★ 期间营收应是每日营收之和: 期间 %s vs 每日之和 %s",
			period.Total.RevenueYuan, sumDailyRev)
	}
	// ② 报告自洽:"每日之和"必须等于各日行之和。
	var rowSum decimal.Decimal
	for _, r := range period.Rows {
		if r.HasStocktake && r.HasPurchase {
			rowSum = rowSum.Add(r.GrossMarginYuan)
		}
	}
	// 金额有 1e-16 级尾数(采购均价是除法结果)⇒ 用容差比。
	if period.Total.DailySumGrossMarginYuan.Sub(rowSum).Abs().GreaterThan(d("0.000001")) {
		t.Errorf("★ 期间报告里的每日之和 %s ≠ 各日行之和 %s",
			period.Total.DailySumGrossMarginYuan, rowSum)
	}
	// ③ 期间毛利**必然不等于**每日之和(全店口径含冷库)⇒ 差得有理由,不是 bug。
	if period.Total.GrossMarginYuan.Equal(sumDaily) {
		t.Logf("本期恰好相等(可能没有冷库变动): %s", period.Total.GrossMarginYuan)
	}
}

// TestPartialCostIncomplete_ReportLevelFlag 锁 ④。
//
// ⚠️ 2026-11-01 owner：账面期间毛利端点(`/gross-margin/period`)已下线
//
//	⇒ 原用例(用 `ComputePeriodMargin` 的报告级 cost_incomplete / days_without_cost_baseline)
//	  改由**每日毛利报告**(`ComputeGrossMargin`)承接。
//
// # 场景(生产真实形态:店 00 的 10-01)
//
//	belly   有期初结转(10-01 录 100kg@18)⇒ 成本**算得出来**
//	trotter 既无期初结转、当天也没进货、只卖货 ⇒ 成本**算不出来**
//	⇒ 那天是"**部分**算不出来",不是"**完全**算不出来"。
//
// # 期望
//
//	① 报告级 cost_incomplete = true(宽口径 ⇒ 必须报)
//	② cost_incomplete_note 非空,且**只点名算不出来的那几个部位**
//	  (belly 算得出来 ⇒ 不该出现在名单里)
//	③ cost_incomplete_note 不能出现"0 天"这种自相矛盾的说法
//
// # 为什么锁这条
//
//	前端若只看"整天算不出来" ⇒ 店 00 的 10 月区间会**一条提示都不显示**
//	⇒ 店长只看得到"毛利 13.47%",完全不知道 10-01 那天的成本是被低估的。
//	⇒ 提示必须由**报告级** cost_incomplete 驱动,且要**点名部位**。
func TestPartialCostIncomplete_ReportLevelFlag(t *testing.T) {
	day2 := fmDay1.Add(24 * time.Hour) // 2026-10-02
	svc, ctx := fmChainSetup(t, map[string][]map[string]any{
		"2026-10-02": {
			fmCubeSalesRow("P-BELLY", "10", "180"),
			fmCubeSalesRow("P-TROTTER", "5", "120"),
		},
	})
	// 第二个部位(trotter):**没有**期初结转 ⇒ 它的成本算不出来。
	if _, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutTrotter, CubeProductID: "P-TROTTER"}); err != nil {
		t.Fatalf("CreateBranchCutMapping(trotter): %v", err)
	}
	// 只有 belly 有基线。
	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-01",
			ColdStoreKg: d("100"), UnitPriceYuan: d("18")},
	}); err != nil {
		t.Fatalf("SaveStockOpenings: %v", err)
	}
	svc.SetClock(fmClock(day2))

	rep, err := svc.ComputeGrossMargin(ctx, "S001", day2)
	mustNoErr(t, err, "ComputeGrossMargin")

	// ① 报告级标志(宽口径)。
	if !rep.CostIncomplete {
		t.Errorf("★ 区间里有部位成本算不出来 ⇒ 报告级 cost_incomplete 必须为 true, got false —— " +
			"前端会用它决定要不要给店长提示(缺了 ⇒ 门店看不到毛利被高估)")
	}
	// ② 人话说明:非空 + 只点名算不出来的那几个部位。
	if rep.CostIncompleteNote == "" {
		t.Error("★ cost_incomplete_note 必须非空 —— 前端要原样显示它(口径是后端的职责)")
	}
	if !strings.Contains(rep.CostIncompleteNote, string(model.CutTrotter)) {
		t.Errorf("★ cost_incomplete_note 必须点名算不出来的那几个部位(%s): %q",
			model.CutTrotter, rep.CostIncompleteNote)
	}
	if strings.Contains(rep.CostIncompleteNote, string(model.CutBelly)) {
		t.Errorf("★ belly 有期初结转 ⇒ 成本算得出来 ⇒ 不该出现在名单里: %q",
			rep.CostIncompleteNote)
	}
	// ③ 不能出现"0 天"(自相矛盾)。
	if strings.Contains(rep.CostIncompleteNote, "0 天") {
		t.Errorf("★ cost_incomplete_note 不能出现「0 天」(自相矛盾): %q", rep.CostIncompleteNote)
	}
}
