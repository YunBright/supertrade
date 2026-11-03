package service_test

// 2026-10-23:移动加权平均核算的**回归锁**。
//
// 这些用例锁的是"毛利算得对不对",不是"函数返回不返回"。
// 断言写成**业务口径**(卖昨天的货成本不能是 0 / 对账行差额为 0),
// 而不是"某字段等于某个魔法数" —— 后者会在口径演进时变成噪音。

import (
	"context"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

// findCutRow 取部位行。
func findCutRow(rows []service.PerCutGrossMargin, cut model.CutType) *service.PerCutGrossMargin {
	for i := range rows {
		if rows[i].CutType == cut {
			return &rows[i]
		}
	}
	return nil
}

// setupTwoDays 起服务并把时钟拨到 D1(2026-10-01)与 D2(2026-10-02)。
func setupTwoDays(t *testing.T) (*service.Service, time.Time, time.Time) {
	t.Helper()
	svc, _ := setupTestService(t)
	bizLoc := time.FixedZone("Asia/Shanghai", 8*3600)
	day1 := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)  // 2026-10-01 14:00 +08
	day2 := time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC)  // 2026-10-02 14:00 +08
	svc.SetClock(func() time.Time { return time.Date(2026, 10, 1, 14, 15, 0, 0, bizLoc) })
	return svc, day1, day2
}

// TestGrossMargin_SellingYesterdaysStock_CostNotZero 是**最关键**的一条回归锁。
//
// # 它守的是什么 bug
//
// 旧口径把"今天卖的钱"按"今天进场的猪"摊成本:
//
//	pigs := 当日进场的猪
//	costForCut[cut] = Σ pig(今天的猪)(pig_cost × 该猪该部位占比)
//	case purch.IsZero(): ratio = 1        ← 今天没产出该部位 ⇒ 比例按 100%
//	consumedCost := costForCut[cut].Mul(ratio)   ← 但 costForCut = 0
//
// ⇒ **今天卖昨天剩的货(今天没进猪)= 成本 0、毛利率 100%**。
// 不是偶发 —— 只要店里在卖隔夜货就**每天都发生**。
//
// # 场景
//
//	D1: 进 1 头猪(50kg × 20 元/kg = 1000 元)→ 分割出 belly 10kg → 日终盘点柜台 belly 10kg
//	D2: **不进猪**,卖 belly 5kg @ 34 元
//
// # 期望
//
//	期初 = 10kg,期初金额 = 1000 元(昨天的加权成本 100 元/kg)
//	本期无入库
//	账面期末 = 10 − 5 = 5kg ⇒ 期末金额 = 5 × 100 = 500
//	销售成本 = 1000 − 500 = 500 元(= 5kg × 100)
//	毛利 = 170 − 500 = −330
//
// ⇒ **成本必须是 500,不是 0;毛利率绝不能是 100%。**
func TestGrossMargin_SellingYesterdaysStock_CostNotZero(t *testing.T) {
	svc, day1, day2 := setupTwoDays(t)
	ctx := context.Background()

	// 部位映射(belly → P-2001)
	_, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-2001"})
	mustNoErr(t, err, "CreateBranchCutMapping belly")

	// ---- D1:进猪 + 分割 + 日终盘点 ----
	pig, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-1", GrossWeightKg: d("50"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: day1,
	}, "u-1")
	mustNoErr(t, err, "RecordWholePig")

	// 单品补录:belly 10kg(成本 1000 元全归它)
	_, err = svc.RecordPigCut(ctx, "S001", model.RecordPigCutInput{
		PigID: pig.ID, CutType: model.CutBelly, Barcode: "B-1",
		WeightKg: d("10"), ExpiresAt: day1.Add(48 * time.Hour),
		CubeProductID: "P-2001",
	}, "u-1")
	mustNoErr(t, err, "RecordPigCut")

	_, err = svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: "P-2001", ActualRemainKg: d("10")},
		},
	}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake D1")

	// ---- D2:不进猪,卖 5kg ----
	svc.SetClock(func() time.Time { return time.Date(2026, 10, 2, 10, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*3600)) })
	_, err = svc.OnSaleCompleted(ctx, service.SaleCompletedPayload{
		SaleID: "SO-2", BranchID: "S001", CompletedAt: day2,
		Lines: []service.SaleLine{
			{LineID: "L-1", SKUID: "P-2001", FreshType: "meat",
				Qty: d("5"), AmountYuan: d("170"), PigID: pig.ID, CutType: model.CutBelly},
		},
	})
	mustNoErr(t, err, "OnSaleCompleted D2")

	report, err := svc.ComputeGrossMargin(ctx, "S001", day2)
	mustNoErr(t, err, "ComputeGrossMargin D2")

	belly := findCutRow(report.PerCut, model.CutBelly)
	if belly == nil {
		t.Fatalf("belly 不在 per_cut: %+v", report.PerCut)
	}

	// ★ 核心断言:卖昨天的货,成本**不能是 0**。
	if belly.SalesCostYuan.IsZero() {
		t.Fatalf("卖昨天的货时销售成本 = 0 —— 这正是旧口径的 100%% 毛利 bug(今天没进猪 ⇒ 成本归零)")
	}
	// 精确值:5kg × 100 元/kg = 500
	if !belly.SalesCostYuan.Equal(d("500")) {
		t.Errorf("belly sales_cost = %s, want 500(5kg × 昨日加权单价 100)", belly.SalesCostYuan)
	}
	// 毛利率不能是 100%(这里收入 170、成本 500 ⇒ 负毛利)
	if belly.GrossMarginPct >= 100 {
		t.Errorf("毛利率 = %v%%, 绝不能是 100%%", belly.GrossMarginPct)
	}
	if !belly.GrossMarginYuan.Equal(d("-330")) {
		t.Errorf("belly gm = %s, want -330(170 − 500)", belly.GrossMarginYuan)
	}
	// 期初必须是昨天的 10kg(而不是 0)
	if !belly.OpeningKg.Equal(d("10")) {
		t.Errorf("belly opening_kg = %s, want 10(昨夜结转)", belly.OpeningKg)
	}
}

// TestGrossMargin_Reconciliation_Closes 锁**成本对账行**。
//
// 恒等式:期初存货金额 + 本期入库金额 == 销售成本 + 统收成本 + 报损成本 + 期末存货金额。
// 差额只允许来自**显式化的损耗**(切分损耗 / 隔夜失重·盘亏)。
//
// ⇒ 这条是"毛利准不准一眼可判"的守门指标。
func TestGrossMargin_Reconciliation_Closes(t *testing.T) {
	svc, day1, _ := setupTwoDays(t)
	ctx := context.Background()

	_, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-2001"})
	mustNoErr(t, err, "CreateBranchCutMapping belly")

	pig, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-1", GrossWeightKg: d("50"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: day1,
	}, "u-1")
	mustNoErr(t, err, "RecordWholePig")
	_, err = svc.RecordPigCut(ctx, "S001", model.RecordPigCutInput{
		PigID: pig.ID, CutType: model.CutBelly, Barcode: "B-1",
		WeightKg: d("10"), ExpiresAt: day1.Add(48 * time.Hour), CubeProductID: "P-2001",
	}, "u-1")
	mustNoErr(t, err, "RecordPigCut")

	_, err = svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: "P-2001", ActualRemainKg: d("8")},
		},
	}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake")

	report, err := svc.ComputeGrossMargin(ctx, "S001", day1)
	mustNoErr(t, err, "ComputeGrossMargin")

	rec := report.Reconciliation
	// 期初 0 + 入库 1000 == 销售成本 + 报损 0 + 期末 800
	if !rec.OpeningInventoryCostYuan.Add(rec.ProducedCostYuan).
		Equal(rec.SalesCostYuan.Add(rec.BulkCostYuan).Add(rec.WasteCostYuan).Add(rec.ClosingInventoryCostYuan)) {
		t.Errorf("对账不平:期初 %s + 入库 %s ≠ 销售 %s + 统收 %s + 报损 %s + 期末 %s",
			rec.OpeningInventoryCostYuan, rec.ProducedCostYuan,
			rec.SalesCostYuan, rec.BulkCostYuan, rec.WasteCostYuan, rec.ClosingInventoryCostYuan)
	}
	if !rec.DiffYuan.IsZero() {
		t.Errorf("对账差额 = %s, want 0", rec.DiffYuan)
	}
	// 猪的采购成本必须被完整摊出去(没有"成本凭空消失")
	if !rec.PigPurchaseCostYuan.Equal(d("1000")) {
		t.Errorf("pig_purchase_cost = %s, want 1000", rec.PigPurchaseCostYuan)
	}
	if !rec.UnallocatedPigCostYuan.IsZero() {
		t.Errorf("unallocated_pig_cost = %s, want 0(这头猪有产出数据,应全部摊出去)", rec.UnallocatedPigCostYuan)
	}
	// 切分损耗 = 进猪毛重 50 − 产出 10 = 40kg
	if !rec.CuttingLossKg.Equal(d("40")) {
		t.Errorf("cutting_loss_kg = %s, want 40(50 − 10)", rec.CuttingLossKg)
	}
}

// TestStock_CounterLoad_MovesFromColdStore 锁两位置库存的流转。
//
// 场景:日终盘点后货在冷库;次日"搬上台"(可部分、可不上);
// ⇒ 冷库余额必须准确(它是累加的,少记一次就只增不减)。
func TestStock_CounterLoad_MovesFromColdStore(t *testing.T) {
	svc, day1, day2 := setupTwoDays(t)
	ctx := context.Background()

	_, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-2001"})
	mustNoErr(t, err, "CreateBranchCutMapping belly")

	pig, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-1", GrossWeightKg: d("50"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: day1,
	}, "u-1")
	mustNoErr(t, err, "RecordWholePig")
	_, err = svc.RecordPigCut(ctx, "S001", model.RecordPigCutInput{
		PigID: pig.ID, CutType: model.CutBelly, Barcode: "B-1",
		WeightKg: d("10"), ExpiresAt: day1.Add(48 * time.Hour), CubeProductID: "P-2001",
	}, "u-1")
	mustNoErr(t, err, "RecordPigCut")
	_, err = svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: "P-2001", ActualRemainKg: d("10")},
		},
	}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake")

	// 日终后:柜台清空,货在冷库
	snap, err := svc.ListStock(ctx, "S001", model.StockScopeAll)
	mustNoErr(t, err, "ListStock all")
	if len(snap.Lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(snap.Lines))
	}
	if !snap.Lines[0].CounterKg.IsZero() {
		t.Errorf("日终后柜台应为 0(货收进冷库过夜), got %s", snap.Lines[0].CounterKg)
	}
	if !snap.Lines[0].ColdStoreKg.Equal(d("10")) {
		t.Errorf("日终后冷库应为 10, got %s", snap.Lines[0].ColdStoreKg)
	}

	// ⚠️ 2026-11-04:搬台记在**次日**(本用例的场景本意就是「次日搬台上」,
	//   见上面那段注释)。
	//
	//   为什么必须挪到次日:柜台余额改成**按记录层推导**之后,柜台是
	//   「**当天之内**」的量 —— 盘点夜(收盘)柜台被**置零**,那一晚实盘进冷库
	//   ⇒ **归属日 == 盘点日**的搬台已经被当晚的实盘吸收
	//   ⇒ 只有归属日 > 最近盘点日的搬台才还在柜台上。
	//
	//   (测试的时钟原本冻结在 D1 ⇒ 搬台被记成 D1 ⇒ 与场景本意不符。)
	svc.SetClock(fmClock(day2))

	// 搬上台 4kg(只搬一部分)
	_, err = svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		Lines: []service.CounterLoadLine{{CubeProductID: "P-2001", QtyKg: d("4")}},
	}, "u-1")
	mustNoErr(t, err, "CounterLoad")

	// 搬超冷库余额**只告警不拦截**(门店口径:「负库存只告警不拦截就可以」)
	// ⇒ 余额会变成负数,由每日毛利的 alert 红字显示。
	_, err = svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		Lines: []service.CounterLoadLine{{CubeProductID: "P-2001", QtyKg: d("99")}},
	}, "u-1")
	if err != nil {
		t.Errorf("搬超冷库余额只告警不拦截,不该报错:%v", err)
	}

	snap, err = svc.ListStock(ctx, "S001", model.StockScopeAll)
	mustNoErr(t, err, "ListStock all 2")
	// 搬了 4kg + 99kg = 103kg ⇒ 柜台 103、冷库 10 − 103 = −93。
	if !snap.Lines[0].CounterKg.Equal(d("103")) {
		t.Errorf("柜台应是 103(4 + 99), got %s", snap.Lines[0].CounterKg)
	}
	if !snap.Lines[0].ColdStoreKg.Equal(d("-93")) {
		t.Errorf("冷库应是 -93(负库存,只告警不拦截), got %s", snap.Lines[0].ColdStoreKg)
	}
	// 全店 = 10(转库不改变全店)
	if !snap.Lines[0].TotalKg.Equal(d("10")) {
		t.Errorf("全店应为 10(转库不改变全店), got %s", snap.Lines[0].TotalKg)
	}

	// 默认查的是猪肉台(scope 默认 counter)
	snapCounter, err := svc.ListStock(ctx, "S001", model.StockScopeCounter)
	mustNoErr(t, err, "ListStock counter")
	if !snapCounter.Lines[0].ColdStoreKg.IsZero() {
		t.Errorf("scope=counter 不应返回冷库量, got %s", snapCounter.Lines[0].ColdStoreKg)
	}
	// ⚠️ 没有"只查冷库"这个口径
	if _, err := svc.ListStock(ctx, "S001", model.StockScope("cold_store")); err == nil {
		t.Errorf("scope=cold_store 应被拒(门店明确:没有查冷库的说法)")
	}
}
