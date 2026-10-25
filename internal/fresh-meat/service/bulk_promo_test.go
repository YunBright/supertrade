package service_test

// 2026-10-23:特价缺口分摊 / 大宗台账 / 长期毛利 / 分割统计基线 的回归锁。
//
// 每条用例锁的是**业务口径**,不是"函数返回不返回":
//   - 特价:缺口指向哪个部位,分摊就落到哪个部位;分摊成功后 promo 桶**不再单列**(不重复计收入)
//   - 大宗:产出 → 冷库 → 结算 → 收入落到产出它的猪身上
//   - 长期:期间收入 − 期间成本 = 期间毛利
//   - 基线:样本够时用统计均值,**不用** LLM

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

// ---- ① 特价缺口分摊 ----

// TestPromoAlloc_GapPointsToTheCut 锁"账面缺口指向哪个部位,特价就分摊到哪个部位"。
//
// 场景(用户举的原例):
//	当日两头猪应分割出后腿肉 30 斤,正常卖 20 斤,特价卖 8 斤,日终盘点剩 2 斤
//	⇒ gap_后腿 = 30 − 20 − 2 = 8 ⇒ 这 8 斤就是后腿被特价处理掉的
//	⇒ 特价的 8kg **100% 分摊到后腿**,不该落到别的部位。
func TestPromoAlloc_GapPointsToTheCut(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()
	bizLoc := time.FixedZone("Asia/Shanghai", 8*3600)
	day := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return time.Date(2026, 10, 1, 14, 15, 0, 0, bizLoc) })

	// 部位映射
	_, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutLegRear, CubeProductID: "P-2001"})
	mustNoErr(t, err, "CreateBranchCutMapping")
	// 特价货号登记
	_, err = svc.CreatePromoSku(ctx, "S001", service.PromoSkuInput{CubeProductID: "P-PROMO"})
	mustNoErr(t, err, "CreatePromoSku")

	pig, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-1", GrossWeightKg: d("200"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: day,
	}, "u-1")
	mustNoErr(t, err, "RecordWholePig")
	// 分割出后腿 30kg
	_, err = svc.RecordPigCut(ctx, "S001", model.RecordPigCutInput{
		PigID: pig.ID, CutType: model.CutLegRear, Barcode: "B-1",
		WeightKg: d("30"), ExpiresAt: day.Add(48 * time.Hour), CubeProductID: "P-2001",
	}, "u-1")
	mustNoErr(t, err, "RecordPigCut")

	// 正常卖 20kg
	_, err = svc.OnSaleCompleted(ctx, service.SaleCompletedPayload{
		SaleID: "SO-1", BranchID: "S001", CompletedAt: day.Add(2 * time.Hour),
		Lines: []service.SaleLine{
			{LineID: "L-1", SKUID: "P-2001", FreshType: "meat",
				Qty: d("20"), AmountYuan: d("480"), PigID: pig.ID, CutType: model.CutLegRear},
		},
	})
	mustNoErr(t, err, "OnSaleCompleted 正常")
	// 特价卖 8kg
	_, err = svc.OnSaleCompleted(ctx, service.SaleCompletedPayload{
		SaleID: "SO-2", BranchID: "S001", CompletedAt: day.Add(3 * time.Hour),
		Lines: []service.SaleLine{
			{LineID: "L-2", SKUID: "P-PROMO", FreshType: "meat",
				Qty: d("8"), AmountYuan: d("64")},
		},
	})
	mustNoErr(t, err, "OnSaleCompleted 特价")

	// 日终盘点:后腿剩 2kg
	_, err = svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutLegRear, CubeProductID: "P-2001", ActualRemainKg: d("2")},
		},
	}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake")

	rep, err := svc.ComputeGrossMargin(ctx, "S001", day)
	mustNoErr(t, err, "ComputeGrossMargin")

	// 分摊明细
	pa := rep.PromoAllocation
	if pa == nil {
		t.Fatalf("报告应含 promo_allocation")
	}
	if !pa.PromoKgTotal.Equal(d("8")) {
		t.Errorf("promo_kg_total = %s, want 8", pa.PromoKgTotal)
	}
	if pa.IsEstimated {
		t.Fatalf("有缺口证据时不应是 estimated: %+v", pa)
	}
	if len(pa.Allocated) != 1 || pa.Allocated[0].CutType != model.CutLegRear {
		t.Fatalf("特价应 100%% 分摊到后腿, got %+v", pa.Allocated)
	}
	if !pa.Allocated[0].PromoKg.Equal(d("8")) {
		t.Errorf("分摊到后腿的特价 kg = %s, want 8", pa.Allocated[0].PromoKg)
	}
	if !pa.Allocated[0].PromoAmount.Equal(d("64")) {
		t.Errorf("分摊到后腿的特价金额 = %s, want 64", pa.Allocated[0].PromoAmount)
	}
	if !pa.GapKgTotal.Equal(d("8")) {
		t.Errorf("gap_kg_total = %s, want 8", pa.GapKgTotal)
	}
	if !pa.UnexplainedKg.IsZero() {
		t.Errorf("unexplained_kg = %s, want 0(缺口证据与实测特价一致)", pa.UnexplainedKg)
	}

	// ★ 部位行:后腿的销量/收入含分摊进来的特价;**promo 桶不再单列**(不重复计收入)
	leg := findCutRow(rep.PerCut, model.CutLegRear)
	if leg == nil {
		t.Fatalf("per_cut 里应有后腿: %+v", rep.PerCut)
	}
	if !leg.SoldKg.Equal(d("28")) {
		t.Errorf("后腿 sold_kg = %s, want 28(20 正常 + 8 特价)", leg.SoldKg)
	}
	if !leg.RevenueYuan.Equal(d("544")) {
		t.Errorf("后腿 revenue = %s, want 544(480 正常 + 64 特价)", leg.RevenueYuan)
	}
	if findCutRow(rep.PerCut, model.CutPromo) != nil {
		t.Errorf("分摊成功后 promo 桶不应再单列(否则同一笔收入算两次): %+v", rep.PerCut)
	}
	// 全店收入 = 480 + 64 = 544(没有重复计)
	if !rep.Total.RevenueYuan.Equal(d("544")) {
		t.Errorf("total revenue = %s, want 544(不能重复计)", rep.Total.RevenueYuan)
	}
}

// ---- ② 大宗台账(板油):产出 → 冷库 → 统收结算 ----

// TestBulk_SettlementLandsOnProducingPig 锁"统收收入落回产出它的那头猪",
// 以及**统收成本必须进损益**(2026-10-23 修的真 bug)。
//
// # 它守的是什么 bug
//
// ① 大宗库存/收入原来记在伪 SKU `bulk:<code>` 上 ⇒ 伪 SKU 没有入库金额
//    ⇒ 移动加权单价恒为 0 ⇒ **统收成本 = 0** ⇒ 毛利率 **100%**。
// ② 毛利公式原来 gm = 收入 − 销售成本,而销售成本里**已经把统收成本减掉了**
//    ⇒ 统收收入成了"零成本收入" ⇒ 同样毛利虚高。
//
// 实测(修前):卖 10kg 板油收 60 元、成本 1000 元 ⇒ 毛利显示 **+60(100%)**,
// 真实应是 **−940**。
//
// # 现在的流程(门店口径)
//
//	日终盘点 → 板油自动结转进冷库(记成本快照)→ 累积 → 卖给酒店(结算出库)
func TestBulk_SettlementLandsOnProducingPig(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()
	bizLoc := time.FixedZone("Asia/Shanghai", 8*3600)
	day := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return time.Date(2026, 10, 1, 14, 15, 0, 0, bizLoc) })

	// 部位映射(fat → P-FAT):大宗库存要记在**部位主 SKU** 上,必须有映射。
	_, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutFat, CubeProductID: "P-FAT"})
	mustNoErr(t, err, "CreateBranchCutMapping fat")

	// 登记大宗品项:板油 → fat
	_, err = svc.CreateBulkProduct(ctx, "S001", service.BulkProductInput{
		CutType: model.CutFat,
	})
	mustNoErr(t, err, "CreateBulkProduct")

	pig, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-1", GrossWeightKg: d("200"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: day,
	}, "u-1")
	mustNoErr(t, err, "RecordWholePig")

	// 分割补录:这头猪出 8kg 板油(200kg × 20 元/kg = 4000 元,成本全落在板油上
	// —— 因为只补录了一个部位 ⇒ 加权单价 = 4000 / 8 = 500 元/kg)。
	_, err = svc.RecordPigCut(ctx, "S001", model.RecordPigCutInput{
		PigID: pig.ID, CutType: model.CutFat, Barcode: "B-FAT",
		WeightKg: d("8"), ExpiresAt: day.Add(48 * time.Hour),
		CubeProductID: "P-FAT",
	}, "u-1")
	mustNoErr(t, err, "RecordPigCut fat")

	// 日终盘点:板油实盘 8kg ⇒ **自动结转**进冷库 + 记成本快照(不用手工录产出)
	_, err = svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutFat, CubeProductID: "P-FAT", ActualRemainKg: d("8")},
		},
	}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake")

	// 结存:产出 8、已结 0、结存 8;冷库实际余额也应是 8kg
	bal, err := svc.ListBulkBalances(ctx, "S001")
	mustNoErr(t, err, "ListBulkBalances")
	if len(bal) != 1 || !bal[0].BalanceKg.Equal(d("8")) {
		t.Fatalf("结存应为 8kg: %+v", bal)
	}
	if !bal[0].ColdStoreKg.Equal(d("8")) {
		t.Errorf("冷库实际余额应为 8kg, got %s", bal[0].ColdStoreKg)
	}
	// 台账结余与冷库余额必须一致(不一致就是"说不清")
	if !bal[0].LedgerVsColdDiffKg.IsZero() {
		t.Errorf("台账结余与冷库余额差额应为 0, got %s", bal[0].LedgerVsColdDiffKg)
	}
	// 冷库位置里应有这 8kg,记在**部位主 SKU** 上(不是 bulk:lard 伪 SKU)
	snap, err := svc.ListStock(ctx, "S001", model.StockScopeAll)
	mustNoErr(t, err, "ListStock all")
	found := false
	for _, l := range snap.Lines {
		if l.CubeProductID == "P-FAT" {
			found = true
			if !l.ColdStoreKg.Equal(d("8")) {
				t.Errorf("冷库板油应为 8kg, got %s", l.ColdStoreKg)
			}
			if l.CutType != model.CutFat {
				t.Errorf("板油应挂在 fat 部位, got %s", l.CutType)
			}
		}
		if l.CubeProductID == "bulk:lard" {
			t.Errorf("不应再有 bulk:lard 伪 SKU(它没有入库金额⇒成本算 0): %+v", l)
		}
	}
	if !found {
		t.Errorf("库存里应有 P-FAT 一行: %+v", snap.Lines)
	}

	// 手工录产出应当被拒(当天已有盘点自动结转 ⇒ 两个入口会记两遍)
	_, err = svc.RecordBulkProduction(ctx, "S001", service.BulkProductionInput{
		ProductCode: "fat", QtyKg: d("8"),
	}, "u-1")
	if err == nil {
		t.Errorf("当天已有盘点自动结转时,手工录产出应被拒(否则同一批货记两遍)")
	}

	// 酒店统收:**表头 + 明细**(2026-10-23 门店口径)
	// 表头:结算日期 / 对方 / 备注;明细:大宗品项 / 数量 / 单价
	view, err := svc.CreateBulkSettlement(ctx, "S001", service.BulkSettlementInput{
		SettleDate:   "2026-10-01",
		Counterparty: "某酒店",
		Note:         "月底统收",
		Mode:         service.BulkModeDelivery,
		Items: []service.BulkSettlementItemInput{
			{ProductCode: "fat", QtyKg: d("8"), UnitPriceYuan: d("6")},
		},
	}, "u-1")
	mustNoErr(t, err, "CreateBulkSettlement")
	if len(view.Items) != 1 {
		t.Fatalf("结算单应有 1 条明细, got %d", len(view.Items))
	}
	if !view.TotalAmountYuan.Equal(d("48")) {
		t.Errorf("总金额应为 48(8kg × 6 元), got %s", view.TotalAmountYuan)
	}
	if view.Counterparty != "某酒店" || view.Note != "月底统收" {
		t.Errorf("表头字段没带上: %+v", view)
	}
	if view.ID == "" {
		t.Error("结算单应有单号")
	}

	// 结存归零
	bal, err = svc.ListBulkBalances(ctx, "S001")
	mustNoErr(t, err, "ListBulkBalances")
	if !bal[0].BalanceKg.IsZero() {
		t.Errorf("结算后结存应为 0, got %s", bal[0].BalanceKg)
	}
	// 冷库位置归零
	snap, err = svc.ListStock(ctx, "S001", model.StockScopeAll)
	mustNoErr(t, err, "ListStock all 2")
	for _, l := range snap.Lines {
		if l.CubeProductID == "P-FAT" && !l.ColdStoreKg.IsZero() {
			t.Errorf("结算后冷库板油应为 0, got %s", l.ColdStoreKg)
		}
	}
	// 收入行落到产出它的猪身上(记在**产出日**)
	rep, err := svc.ComputeGrossMargin(ctx, "S001", day)
	mustNoErr(t, err, "ComputeGrossMargin")
	var fat *service.PerCutGrossMargin
	for i := range rep.PerCut {
		if rep.PerCut[i].CutType == model.CutFat {
			fat = &rep.PerCut[i]
		}
	}
	if fat == nil {
		t.Fatalf("per_cut 里应有 fat(板油): %+v", rep.PerCut)
	}
	if !fat.RevenueYuan.Equal(d("48")) {
		t.Errorf("fat revenue = %s, want 48(统收 8kg × 6 元)", fat.RevenueYuan)
	}
	if !fat.BulkSoldKg.Equal(d("8")) {
		t.Errorf("fat bulk_sold_kg = %s, want 8", fat.BulkSoldKg)
	}
	// ★ 核心断言:**统收成本必须不为 0**。
	//
	// 8kg × 500 元/kg(这头猪 4000 元成本全落在板油上)= 4000 元
	if !fat.BulkCostYuan.Equal(d("4000")) {
		t.Errorf("fat bulk_cost = %s, want 4000(8kg × 500 元/kg)", fat.BulkCostYuan)
	}
	// 毛利 = 收入 48 − 统收成本 4000 = −3952(绝不能是 +48 / 100%)
	if !fat.GrossMarginYuan.Equal(d("-3952")) {
		t.Errorf("fat gm = %s, want -3952(48 收入 − 4000 统收成本)", fat.GrossMarginYuan)
	}
	if fat.GrossMarginPct >= 100 {
		t.Errorf("毛利率 = %v%%, 绝不能是 100%%", fat.GrossMarginPct)
	}
	// 对账行必须闭合
	if !rep.Reconciliation.DiffYuan.IsZero() {
		t.Errorf("对账行差额应为 0, got %s", rep.Reconciliation.DiffYuan)
	}
	_ = pig
}

// ---- ③ 长期毛利 ----

// TestPeriodMargin_MatchesDailySnapshot 锁"长期毛利 = 日快照累加"。
func TestPeriodMargin_MatchesDailySnapshot(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()
	bizLoc := time.FixedZone("Asia/Shanghai", 8*3600)
	day := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return time.Date(2026, 10, 1, 14, 15, 0, 0, bizLoc) })

	_, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-2001"})
	mustNoErr(t, err, "CreateBranchCutMapping")
	pig, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-1", GrossWeightKg: d("200"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: day,
	}, "u-1")
	mustNoErr(t, err, "RecordWholePig")
	_, err = svc.RecordPigCut(ctx, "S001", model.RecordPigCutInput{
		PigID: pig.ID, CutType: model.CutBelly, Barcode: "B-1",
		WeightKg: d("30"), ExpiresAt: day.Add(48 * time.Hour), CubeProductID: "P-2001",
	}, "u-1")
	mustNoErr(t, err, "RecordPigCut")
	_, err = svc.OnSaleCompleted(ctx, service.SaleCompletedPayload{
		SaleID: "SO-1", BranchID: "S001", CompletedAt: day.Add(2 * time.Hour),
		Lines: []service.SaleLine{
			{LineID: "L-1", SKUID: "P-2001", FreshType: "meat",
				Qty: d("10"), AmountYuan: d("340"), PigID: pig.ID, CutType: model.CutBelly},
		},
	})
	mustNoErr(t, err, "OnSaleCompleted")
	_, err = svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: "P-2001", ActualRemainKg: d("20")},
		},
	}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake")

	// 单日
	daily, err := svc.ComputeGrossMargin(ctx, "S001", day)
	mustNoErr(t, err, "ComputeGrossMargin")
	// 期间(同一天)
	period, err := svc.ComputePeriodMargin(ctx, "S001", "2026-10-01", "2026-10-01", "day")
	mustNoErr(t, err, "ComputePeriodMargin")
	// ⚠️ 允许 1 分钱误差:日快照列是 decimal(16,2)(入库即四舍五入到分),
	// 而单日路径是内存里的全精度值 ⇒ 两者天然有 ≤0.01 的差。
	const tol = "0.01"
	if !period.Total.RevenueYuan.Sub(daily.Total.RevenueYuan).Abs().LessThanOrEqual(d(tol)) {
		t.Errorf("期间收入 %s ≠ 单日收入 %s", period.Total.RevenueYuan, daily.Total.RevenueYuan)
	}
	if !period.Total.SalesCostYuan.Sub(daily.Total.SalesCostYuan).Abs().LessThanOrEqual(d(tol)) {
		t.Errorf("期间成本 %s ≠ 单日成本 %s", period.Total.SalesCostYuan, daily.Total.SalesCostYuan)
	}
	if !period.Total.GrossMarginYuan.Sub(daily.Total.GrossMarginYuan).Abs().LessThanOrEqual(d(tol)) {
		t.Errorf("期间毛利 %s ≠ 单日毛利 %s", period.Total.GrossMarginYuan, daily.Total.GrossMarginYuan)
	}
	if len(period.Rows) != 1 {
		t.Errorf("按天聚合应 1 行, got %d", len(period.Rows))
	}
}

// ---- ④ 分割重量统计基线 ----

// TestForecastBaseline_UsedWhenNoPigCuts 锁"样本够时用统计基线,不用 LLM"。
//
// 场景:同一体重段已有 5 头猪的分割实录(各 20kg 五花)⇒
// 第 6 头猪(同体重段、**没录** pig_cuts、没有 LLM advice)
// ⇒ 它的五花产出应当用基线 p50 = 20kg,而不是 0。
func TestForecastBaseline_UsedWhenNoPigCuts(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()
	bizLoc := time.FixedZone("Asia/Shanghai", 8*3600)
	day := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return time.Date(2026, 10, 1, 14, 15, 0, 0, bizLoc) })

	_, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-2001"})
	mustNoErr(t, err, "CreateBranchCutMapping")

	// 5 头"历史猪",各分割出五花 20kg(用于拟合基线)
	for i := 0; i < 5; i++ {
		p, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
			EarTag: "EB-H" + string(rune('A'+i)), GrossWeightKg: d("200"), PurchaseUnitPriceYuan: d("20"),
			SupplierID: "SUP-1", ArrivedAt: day,
		}, "u-1")
		mustNoErr(t, err, "RecordWholePig 历史猪")
		_, err = svc.RecordPigCut(ctx, "S001", model.RecordPigCutInput{
			PigID: p.ID, CutType: model.CutBelly, Barcode: "B-H" + string(rune('A'+i)),
			WeightKg: d("20"), ExpiresAt: day.Add(48 * time.Hour), CubeProductID: "P-2001",
		}, "u-1")
		mustNoErr(t, err, "RecordPigCut 历史猪")
	}

	// 重算基线
	n, err := svc.RecomputeForecastBaselines(ctx, "S001")
	mustNoErr(t, err, "RecomputeForecastBaselines")
	if n == 0 {
		t.Fatalf("基线应算出至少 1 行")
	}
	bl, err := svc.ListForecastBaselines(ctx, "S001")
	mustNoErr(t, err, "ListForecastBaselines")
	found := false
	for _, b := range bl {
		if b.CutType == model.CutBelly && b.SampleCount >= 5 {
			found = true
			if !b.MeanKg.Equal(d("20")) {
				t.Errorf("基线 mean = %s, want 20", b.MeanKg)
			}
			if !b.P50Kg.Equal(d("20")) {
				t.Errorf("基线 p50 = %s, want 20", b.P50Kg)
			}
		}
	}
	if !found {
		t.Fatalf("应有 belly 的基线(n>=5): %+v", bl)
	}

	// 第 6 头猪:同体重段、**没有** pig_cuts、**没有** LLM advice
	// ⇒ 它的五花产出应当用基线 p50 = 20kg(而不是 0)
	pig6, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-NEW", GrossWeightKg: d("200"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: day,
	}, "u-1")
	mustNoErr(t, err, "RecordWholePig 新猪")
	_ = pig6

	rep, err := svc.ComputeGrossMargin(ctx, "S001", day)
	mustNoErr(t, err, "ComputeGrossMargin")
	belly := findCutRow(rep.PerCut, model.CutBelly)
	if belly == nil {
		t.Fatalf("per_cut 里应有 belly: %+v", rep.PerCut)
	}
	// 5 头历史猪各 20 = 100,加新猪的基线 20 ⇒ 120
	if !belly.PurchasedKg.Equal(d("120")) {
		t.Errorf("belly 入库 = %s, want 120(5×20 实录 + 新猪基线 20)", belly.PurchasedKg)
	}
}

// ---- 大宗品项重复登记 =----

// TestBulk_DuplicateProductIsConflict 锁"同 (branch, code) 再登记 ⇒ 冲突,不是 500"。
//
// 曾经的行为:PG 唯一键冲突 ⇒ mapErr 落到 default 分支 ⇒ **500 internal_error**。
// 症状与真因完全不像(操作者以为服务端坏了,实际只是重复登记一次)。
//
// 为什么不学 promo_skus 做幂等更新:大宗品项的 code 是**业务标识**(板油=lard),
// 而 cut_type 决定"这批产出记到哪个部位的账上"。
// 同 code 再登记一次却带着别的 cut_type ⇒ 静默覆盖会让**历史结算**的部位归属前后不一致
// ⇒ 必须让操作者看见冲突。
func TestBulk_DuplicateProductIsConflict(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()

	_, err := svc.CreateBulkProduct(ctx, "S001", service.BulkProductInput{
		CutType: model.CutFat,
	})
	mustNoErr(t, err, "CreateBulkProduct")

	// 同**部位**再登记 ⇒ 必须是"已存在"冲突(2026-10-24:大宗品项 = 一个部位)
	_, err = svc.CreateBulkProduct(ctx, "S001", service.BulkProductInput{
		CutType: model.CutFat,
	})
	if !errors.Is(err, service.ErrBulkProductConflict) {
		t.Fatalf("同部位重复登记应返 ErrBulkProductConflict, got %v", err)
	}
}