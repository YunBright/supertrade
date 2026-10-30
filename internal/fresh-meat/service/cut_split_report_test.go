package service_test

// 2026-10-29:「鲜肉毛利看板 · 部位明细」的**分割算法**在报表层的回归锁。
//
// # 锁的是 owner 的口径,不是实现细节
//
//	① 建议分割 = 纯算法建议(基线 p50 优先、LLM 兜底),**不含 pig_cuts 实录**。
//	② 实际分割 = 实录优先,否则倒推(有实盘走全店恒等式;无实盘走台面近似)。
//	③ 调整后权重 W 接管入库量 ⇒ 销售成本/毛利跟着变,但**对账恒等式必须仍然闭合**。
//	④ 两条守卫(没进猪 / 无处可挪)不许把**账面短卖**掩盖掉。
//	⑤ 报表的「实盘剩余」= **猪肉台实盘**(不含冷库);「应有」= 全店应有 − 冷库量。
//
// ⚠ 这些用例都是**端到端**的(走公开 API + 真实 SQLite)⇒ 引擎接线错了也会红。

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

// splitDay 是本文件用的营业日(2026-10-29 14:00 +08)。
var splitDay = time.Date(2026, 10, 29, 6, 0, 0, 0, time.UTC)

// splitSetup 起服务 + 配 belly 映射。
func splitSetup(t *testing.T) (*service.Service, context.Context, string) {
	t.Helper()
	svc, _ := setupTestService(t)
	ctx := context.Background()
	svc.SetClock(fmClock(splitDay))
	_, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-BELLY"})
	mustNoErr(t, err, "CreateBranchCutMapping")
	return svc, ctx, "P-BELLY"
}

// splitSell 记一笔零售销售(本地事件表)。
func splitSell(t *testing.T, svc *service.Service, sku string, lineID, qty, amount string) {
	t.Helper()
	_, err := svc.OnSaleCompleted(context.Background(), service.SaleCompletedPayload{
		SaleID: "SO-" + lineID, BranchID: "S001", CompletedAt: splitDay.Add(time.Hour),
		Lines: []service.SaleLine{{
			LineID: lineID, SKUID: sku, FreshType: "meat",
			Qty: d(qty), AmountYuan: d(amount), PigID: "PIG-1", CutType: model.CutBelly,
		}},
	})
	mustNoErr(t, err, "OnSaleCompleted "+lineID)
}

// splitBaseline 直接写一条**统计基线**(省掉"先造 5 头历史猪"的麻烦)。
func splitBaseline(t *testing.T, cut model.CutType, band, p50 string) {
	t.Helper()
	mustNoErr(t, testDBSingleton.Create(&model.PigForecastBaseline{
		ID: "PFB-" + string(cut) + "-" + band, BranchID: "S001",
		WeightBand: band, CutType: cut, SampleCount: 5,
		MeanKg: d(p50), P10Kg: d(p50), P50Kg: d(p50), P90Kg: d(p50),
		ComputedAt: time.Now(),
	}).Error, "插统计基线")
}

// splitJSONRows 取报表序列化后的 per_cut 行(**查 JSON,不是查 Go 值**)。
func splitJSONRows(t *testing.T, report *service.GrossMarginReport) []map[string]any {
	t.Helper()
	b, err := json.Marshal(report)
	mustNoErr(t, err, "marshal report")
	var m map[string]any
	mustNoErr(t, json.Unmarshal(b, &m), "unmarshal report")
	rows, ok := m["per_cut"].([]any)
	if !ok {
		t.Fatalf("per_cut 不是数组: %v", m["per_cut"])
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		r, _ := row.(map[string]any)
		out = append(out, r)
	}
	return out
}

// ---- 生产现实回归(守卫①)----

// TestCutSplit_NoPig_ShortSellNotMasked 是**生产现实回归**:
//
// 生产实测(店 00 / 2026-10-29)的 belly 行:
//
//	opening 0 / purchased 0 / sold 27.5813 / expected −27.5813 / cost_incomplete true
//
// 当天**没有进猪** ⇒ 建议分割全是 0,但销量 27.58 ⇒ 倒推出的"实际分割"= 27.58 > 0
// ⇒ 若拿它当入库量,账面的**短卖**(应有 −27.5813)会被撑成 0 ⇒ **把损失掩盖掉了**。
//
// ⇒ 守卫①:没有建议分割 ⇒ 不重分配 ⇒ 短卖必须原样留着。
func TestCutSplit_NoPig_ShortSellNotMasked(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	splitSell(t, svc, sku, "L-1", "27.5813", "440.2")

	rep, err := svc.ComputeGrossMargin(ctx, "S001", splitDay)
	mustNoErr(t, err, "ComputeGrossMargin")
	belly := findPerCut(t, rep.PerCut, model.CutBelly)

	if !belly.PurchasedKg.IsZero() {
		t.Errorf("★ 没有进猪时进货必须是 0(守卫①), got %s", belly.PurchasedKg)
	}
	if !belly.ExpectedRemainKg.Equal(d("-27.5813")) {
		t.Errorf("★ 应有必须仍是 −27.5813(短卖不被掩盖), got %s", belly.ExpectedRemainKg)
	}
	if !belly.CostIncomplete {
		t.Errorf("★ cost_incomplete 仍应为 true(成本算不出来), got false")
	}
	// 毛利数字不受这次改造影响(成本算不出来 ⇒ 成本 0 ⇒ 毛利 = 收入 ⇒ 100%)。
	if belly.GrossMarginPct != 100 {
		t.Errorf("★ 毛利率不该因本次改造变化: got %v, want 100", belly.GrossMarginPct)
	}
	if !belly.SuggestedSplitKg.IsZero() {
		t.Errorf("没有进猪 ⇒ 建议分割应是 0, got %s", belly.SuggestedSplitKg)
	}
}

// ---- 守卫②(无处可挪 ⇒ 不重分配 + 告警)----

// TestCutSplit_GuardNoPool_NotRedistributed 锁**守卫②**。
//
// belly 有建议 20(基线 p50)且实出 50(实录)⇒ 超出建议;但**只有这一个部位**
// ⇒ 别的部位没有可压缩量 ⇒ **没处可挪** ⇒ 不重分配(W 保持建议 20)
// ⇒ 而且**必须出告警**(门店要看得见"今天的进货不是实际切出来的量")。
func TestCutSplit_GuardNoPool_NotRedistributed(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	splitBaseline(t, model.CutBelly, "200-249", "20")

	pig, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-1", GrossWeightKg: d("200"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: splitDay,
	}, "u-1")
	mustNoErr(t, err, "RecordWholePig")
	_, err = svc.RecordPigCut(ctx, "S001", model.RecordPigCutInput{
		PigID: pig.ID, CutType: model.CutBelly, Barcode: "B-1",
		WeightKg: d("50"), ExpiresAt: splitDay.Add(48 * time.Hour), CubeProductID: sku,
	}, "u-1")
	mustNoErr(t, err, "RecordPigCut")

	rep, err := svc.ComputeGrossMargin(ctx, "S001", splitDay)
	mustNoErr(t, err, "ComputeGrossMargin")
	belly := findPerCut(t, rep.PerCut, model.CutBelly)

	if !belly.SuggestedSplitKg.Equal(d("20")) {
		t.Errorf("建议分割应是基线 p50 = 20, got %s", belly.SuggestedSplitKg)
	}
	if !belly.AdjustedSplitKg.Equal(d("20")) {
		t.Errorf("★ 守卫②:没处可挪 ⇒ 调整后权重应保持建议 20, got %s", belly.AdjustedSplitKg)
	}
	if !belly.PurchasedKg.Equal(d("20")) {
		t.Errorf("★ 守卫②:进货(= 调整后权重)应是 20, got %s", belly.PurchasedKg)
	}
	// 告警必须出现(不许静默)。
	found := false
	for _, a := range rep.Alerts {
		if a.Code == service.MarginAlertSplitWeightUnbalanced {
			found = true
		}
	}
	if !found {
		t.Errorf("★ 守卫②必须出告警(%s), got %+v", service.MarginAlertSplitWeightUnbalanced, rep.Alerts)
	}
}

// ---- 实际分割的显隐规则 ----

// TestCutSplit_ActualSplitVisibility 锁**实际分割的显示规则**(owner 定案)。
//
//	recorded(实录)⇒ 总是给值(哪怕实际 < 建议);
//	derived(倒推)+ 有完整日终盘点 ⇒ 给值;
//	derived + 无完整日终盘点 且 实际 < 建议 ⇒ **键消失**(前端显示「—」)。
func TestCutSplit_ActualSplitVisibility(t *testing.T) {
	t.Run("recorded 总是有值", func(t *testing.T) {
		svc, ctx, sku := splitSetup(t)
		splitBaseline(t, model.CutBelly, "200-249", "20")
		pig, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
			EarTag: "EB-1", GrossWeightKg: d("200"), PurchaseUnitPriceYuan: d("20"),
			SupplierID: "SUP-1", ArrivedAt: splitDay,
		}, "u-1")
		mustNoErr(t, err, "RecordWholePig")
		_, err = svc.RecordPigCut(ctx, "S001", model.RecordPigCutInput{
			PigID: pig.ID, CutType: model.CutBelly, Barcode: "B-1",
			WeightKg: d("10"), ExpiresAt: splitDay.Add(48 * time.Hour), CubeProductID: sku,
		}, "u-1")
		mustNoErr(t, err, "RecordPigCut")

		rep, err := svc.ComputeGrossMargin(ctx, "S001", splitDay)
		mustNoErr(t, err, "ComputeGrossMargin")
		belly := findPerCut(t, rep.PerCut, model.CutBelly)
		if belly.ActualSplitSource != "recorded" {
			t.Errorf("来源应是 recorded, got %q", belly.ActualSplitSource)
		}
		if belly.ActualSplitKg == nil {
			t.Fatalf("★ 实录来源 ⇒ actual_split_kg 必须有值(哪怕实际 10 < 建议 20)")
		}
		if !belly.ActualSplitKg.Equal(d("10")) {
			t.Errorf("实际分割应是实录 10, got %s", *belly.ActualSplitKg)
		}
	})

	t.Run("derived + 有实盘 ⇒ 有值", func(t *testing.T) {
		svc, ctx, _ := splitSetup(t)
		splitBaseline(t, model.CutBelly, "200-249", "20")
		// 猪进来了但**没录分割** ⇒ 该部位没有实录 ⇒ 来源是 derived。
		_, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
			EarTag: "EB-1", GrossWeightKg: d("200"), PurchaseUnitPriceYuan: d("20"),
			SupplierID: "SUP-1", ArrivedAt: splitDay,
		}, "u-1")
		mustNoErr(t, err, "RecordWholePig")
		_, err = svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
			IsComplete: true,
			Cuts: []model.CutInput{
				{CutType: model.CutBelly, CubeProductID: "P-BELLY", ActualRemainKg: d("5")},
			},
		}, "u-1")
		mustNoErr(t, err, "RecordPorkCutsStocktake")

		rep, err := svc.ComputeGrossMargin(ctx, "S001", splitDay)
		mustNoErr(t, err, "ComputeGrossMargin")
		belly := findPerCut(t, rep.PerCut, model.CutBelly)
		if belly.ActualSplitSource != "derived" {
			t.Errorf("来源应是 derived, got %q", belly.ActualSplitSource)
		}
		if belly.ActualSplitKg == nil {
			t.Fatalf("★ 有完整日终盘点 ⇒ actual_split_kg 必须有值")
		}
	})

	t.Run("derived + 无实盘 + 实际 < 建议 ⇒ 键消失", func(t *testing.T) {
		svc, ctx, _ := splitSetup(t)
		splitBaseline(t, model.CutBelly, "200-249", "20")
		_, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
			EarTag: "EB-1", GrossWeightKg: d("200"), PurchaseUnitPriceYuan: d("20"),
			SupplierID: "SUP-1", ArrivedAt: splitDay,
		}, "u-1")
		mustNoErr(t, err, "RecordWholePig")

		rep, err := svc.ComputeGrossMargin(ctx, "S001", splitDay)
		mustNoErr(t, err, "ComputeGrossMargin")
		belly := findPerCut(t, rep.PerCut, model.CutBelly)
		if belly.ActualSplitKg != nil {
			t.Errorf("★ 无实盘且实际(0) < 建议(20) ⇒ actual_split_kg 必须为空, got %s", *belly.ActualSplitKg)
		}
		// JSON 里这个键也必须**真的消失**(omitempty 对 decimal 无效 ⇒ 必须用指针)。
		for _, row := range splitJSONRows(t, rep) {
			if v, ok := row["actual_split_kg"]; ok && v != nil {
				t.Errorf("★ actual_split_kg 键必须是 null/缺失, got %v", v)
			}
		}
	})
}

// ---- 对账恒等式 ----

// TestCutSplit_ReconciliationStillBalances 锁"换了入库量口径,**对账恒等式仍然闭合**"。
//
// 恒等式:期初 + 入库 = 销售成本 + 统收成本 + 报损成本 + 期末存货金额。
// ⇒ 差额必须恒为 0(销售成本是代数恒等式算出来的 ⇒ 改 prodK 不会破坏它)。
func TestCutSplit_ReconciliationStillBalances(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	splitBaseline(t, model.CutBelly, "200-249", "20")
	_, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-1", GrossWeightKg: d("200"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: splitDay,
	}, "u-1")
	mustNoErr(t, err, "RecordWholePig")
	splitSell(t, svc, sku, "L-1", "8", "160")
	_, err = svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("12")},
		},
	}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake")

	rep, err := svc.ComputeGrossMargin(ctx, "S001", splitDay)
	mustNoErr(t, err, "ComputeGrossMargin")
	if !rep.Reconciliation.DiffYuan.IsZero() {
		t.Errorf("★ 对账差额必须是 0(换了入库量口径也不许破恒等式), got %s", rep.Reconciliation.DiffYuan)
	}
}

// ---- 报表口径:实盘剩余 = 猪肉台实盘;应有 = 全店应有 − 冷库量 ----

// TestCutSplit_ReportActualRemainIsCounterScope 锁 owner 的口径变更:
//
//	「实盘剩余」= **猪肉台实盘**(盘点单的 actual_remain_kg,**不含冷库**);
//	「应有」= 全店应有 − 冷库量(柜台口径);
//	「差异」= 实盘 − 应有。
func TestCutSplit_ReportActualRemainIsCounterScope(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	_, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-1", GrossWeightKg: d("200"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: splitDay,
	}, "u-1")
	mustNoErr(t, err, "RecordWholePig")
	_, err = svc.RecordPigCut(ctx, "S001", model.RecordPigCutInput{
		PigID: mustPigID(t, svc), CutType: model.CutBelly, Barcode: "B-1",
		WeightKg: d("40"), ExpiresAt: splitDay.Add(48 * time.Hour), CubeProductID: sku,
	}, "u-1")
	mustNoErr(t, err, "RecordPigCut")
	splitSell(t, svc, sku, "L-1", "10", "200")
	_, err = svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("30")},
		},
	}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake")

	rep, err := svc.ComputeGrossMargin(ctx, "S001", splitDay)
	mustNoErr(t, err, "ComputeGrossMargin")
	belly := findPerCut(t, rep.PerCut, model.CutBelly)

	// ① 实盘剩余 = 猪肉台实盘(30),**不是**全店结余。
	if belly.ActualRemainKg == nil {
		t.Fatalf("★ 有完整盘点 ⇒ 实盘剩余必须有值")
	}
	if !belly.ActualRemainKg.Equal(d("30")) {
		t.Errorf("★ 实盘剩余应是猪肉台实盘 30, got %s", *belly.ActualRemainKg)
	}
	if !belly.CounterActualKg.Equal(d("30")) {
		t.Errorf("counter_actual_kg 应是 30, got %s", belly.CounterActualKg)
	}
	// ② 应有 = 全店应有 − 冷库量(全店应有 = 期初 + 入库 − 销售 − 统收 − 报损)。
	wholeStoreExpected := belly.OpeningKg.Add(belly.PurchasedKg).
		Sub(belly.SoldKg).Sub(belly.BulkSoldKg).Sub(belly.WasteKg)
	if !belly.ExpectedRemainKg.Add(belly.ColdStoreKg).Equal(wholeStoreExpected) {
		t.Errorf("★ 应有(%s) + 冷库量(%s) 应等于全店应有 %s",
			belly.ExpectedRemainKg, belly.ColdStoreKg, wholeStoreExpected)
	}
	// ③ 差异 = 实盘 − 应有。
	if belly.VarianceKg == nil {
		t.Fatalf("★ 有完整盘点 ⇒ 差异必须有值")
	}
	if !belly.VarianceKg.Equal(belly.ActualRemainKg.Sub(belly.ExpectedRemainKg)) {
		t.Errorf("★ 差异 %s 应 = 实盘 %s − 应有 %s",
			*belly.VarianceKg, *belly.ActualRemainKg, belly.ExpectedRemainKg)
	}
}

// ---- 实际分割的公式 ----

// TestCutSplit_ActualSplitFormula_WithStocktake 锁"有日终实盘 ⇒ 全店恒等式"。
//
//	实际分割 = 全店期末结余 + 销售 + 统收 + 报损 − 全店期初
//	其中 全店期末结余 = 猪肉台实盘 + 冷库量
//
// ⚠ 这条同时说明"**为什么不能用台面口径**":台面恒等式里没有"今日分割"。
func TestCutSplit_ActualSplitFormula_WithStocktake(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	_, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-1", GrossWeightKg: d("200"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: splitDay,
	}, "u-1")
	mustNoErr(t, err, "RecordWholePig")
	splitSell(t, svc, sku, "L-1", "6", "120")
	_, err = svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("18")},
		},
	}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake")

	rep, err := svc.ComputeGrossMargin(ctx, "S001", splitDay)
	mustNoErr(t, err, "ComputeGrossMargin")
	belly := findPerCut(t, rep.PerCut, model.CutBelly)
	if belly.ActualSplitKg == nil {
		t.Fatalf("有完整盘点 ⇒ 实际分割必须有值")
	}
	want := belly.CounterActualKg.Add(belly.ColdStoreKg).
		Add(belly.SoldKg).Add(belly.BulkSoldKg).Add(belly.WasteKg).
		Sub(belly.OpeningKg)
	if belly.ActualSplitKg.Sub(want).Abs().GreaterThan(d("0.0001")) {
		t.Errorf("★ 实际分割应 = (猪肉台实盘 %s + 冷库 %s) + 销售 %s + 统收 %s + 报损 %s − 期初 %s = %s, got %s",
			belly.CounterActualKg, belly.ColdStoreKg, belly.SoldKg, belly.BulkSoldKg,
			belly.WasteKg, belly.OpeningKg, want, *belly.ActualSplitKg)
	}
}

// TestCutSplit_CounterLoadOldGoodsNotTodaySplit 锁"**搬上台的老货不算今日分割**"。
//
// 期初 100kg(冷库里的老货)+ 搬上台 20kg + 卖出 130kg,没有盘点、没有进猪
// ⇒ 实际分割 = max(0, 130 − 100 − 20) = 10
// (不扣那 20kg 的话会算成 30 ⇒ 老货被算成今天的分割。)
func TestCutSplit_CounterLoadOldGoodsNotTodaySplit(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-28", ColdStoreKg: d("100"), UnitPriceYuan: d("18")},
	}); err != nil {
		t.Fatalf("SaveStockOpenings: %v", err)
	}
	if _, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		TakenDate: "2026-10-29",
		Lines:     []service.CounterLoadLine{{CubeProductID: sku, QtyKg: d("20")}},
	}, "u-1"); err != nil {
		t.Fatalf("CounterLoad: %v", err)
	}
	splitSell(t, svc, sku, "L-1", "130", "2600")

	rep, err := svc.ComputeGrossMargin(ctx, "S001", splitDay)
	mustNoErr(t, err, "ComputeGrossMargin")
	belly := findPerCut(t, rep.PerCut, model.CutBelly)
	if belly.ActualSplitKg == nil {
		t.Fatalf("实际分割应有值(实际 > 建议 0)")
	}
	if !belly.ActualSplitKg.Equal(d("10")) {
		t.Errorf("★ 实际分割应 = 130−100−20 = 10(搬上台是老货,要扣掉), got %s", *belly.ActualSplitKg)
	}
}

// TestCutSplit_CostUncomputableSemanticsUnchanged 锁"成本算不出来"的既有语义没被破坏。
//
// 既没有期初库存、当天也没有进货、又没有可用单价 ⇒ 销售成本必然是 0
// ⇒ **不是"成本是 0",是"不知道成本"** ⇒ 必须显式标记 cost_incomplete。
func TestCutSplit_CostUncomputableSemanticsUnchanged(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	splitSell(t, svc, sku, "L-1", "3", "60")

	rep, err := svc.ComputeGrossMargin(ctx, "S001", splitDay)
	mustNoErr(t, err, "ComputeGrossMargin")
	belly := findPerCut(t, rep.PerCut, model.CutBelly)
	if !belly.CostIncomplete {
		t.Errorf("★ 没有期初也没有进货 ⇒ cost_incomplete 必须为 true")
	}
	if !rep.CostIncomplete {
		t.Errorf("★ 报告级 cost_incomplete 必须为 true")
	}
	if rep.CostIncompleteNote == "" {
		t.Errorf("★ 必须给门店一句人话说明")
	}
}

// ---- 冷库量:有盘点单取盘点单,没有取引擎链值;「应有」只在有柜台读数时换口径 ----

// TestCutSplit_ColdFallbackAndExpectedScope 锁两件事:
//
//	① 报表的「冷库量」:没有盘点单时**退回引擎链值**(owner 口径);
//	② 报表的「应有」:只有**有完整盘点**(有柜台读数)时才换成**柜台口径**
//	   (全店应有 − 冷库量);没有盘点单时保持**全店口径**。
//
// ⚠ 为什么②不能一刀切:链值**不含今天切出来的量**(今天的分割先进冷库、
//
//	没搬上台就不在链里)⇒ 拿它去减,会算出一个"今天的分割已经摆上台面了"的假数。
func TestCutSplit_ColdFallbackAndExpectedScope(t *testing.T) {
	svc, ctx, _ := splitSetup(t)
	// 期初结转:100kg 全在**冷库**(门店口径:期初结转录的就是冷库余额)。
	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-28", ColdStoreKg: d("100"), UnitPriceYuan: d("18")},
	}); err != nil {
		t.Fatalf("SaveStockOpenings: %v", err)
	}

	rep, err := svc.ComputeGrossMargin(ctx, "S001", splitDay)
	mustNoErr(t, err, "ComputeGrossMargin")
	belly := findPerCut(t, rep.PerCut, model.CutBelly)

	// ① 没有盘点单 ⇒ 冷库量退回引擎链值(这里 = 期初的 100kg)。
	if !belly.ColdStoreKg.Equal(d("100")) {
		t.Errorf("★ 没有盘点单时冷库量应退回引擎链值 100, got %s", belly.ColdStoreKg)
	}
	// ② 没有柜台读数 ⇒「应有」保持全店口径(100),**不能**被链值减成 0。
	if !belly.ExpectedRemainKg.Equal(d("100")) {
		t.Errorf("★ 没有完整盘点时「应有」应保持全店口径 100, got %s", belly.ExpectedRemainKg)
	}
}

// ---- wire 契约 ----

// TestCutSplit_WireContractKeys 锁**前端依赖的 8 个新键**(名字/类型不许变)。
//
// ⚠ 数字类字段一律是 **string**(decimal)⇒ 前端按字符串解析。
// ⚠ `actual_split_kg` 可空 ⇒ 只能是 **string 或 null/缺失**(不能是 "0" 冒充)。
func TestCutSplit_WireContractKeys(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	splitBaseline(t, model.CutBelly, "200-249", "20")
	_, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-1", GrossWeightKg: d("200"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: splitDay,
	}, "u-1")
	mustNoErr(t, err, "RecordWholePig")
	splitSell(t, svc, sku, "L-1", "5", "100")
	_, err = svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("15")},
		},
	}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake")

	rep, err := svc.ComputeGrossMargin(ctx, "S001", splitDay)
	mustNoErr(t, err, "ComputeGrossMargin")
	rows := splitJSONRows(t, rep)
	if len(rows) == 0 {
		t.Fatalf("per_cut 不该是空的")
	}
	row := rows[0]
	for _, key := range []string{
		"suggested_split_kg", "actual_split_source", "adjusted_split_kg",
		"bulk_revenue_yuan", "counter_actual_kg", "cold_store_kg", "cold_store_unit_cost_yuan",
	} {
		v, ok := row[key]
		if !ok {
			t.Errorf("★ wire 键 %s 缺失", key)
			continue
		}
		if _, isStr := v.(string); !isStr {
			t.Errorf("★ wire 键 %s 必须是 string(decimal), got %T(%v)", key, v, v)
		}
	}
	// actual_split_kg:要么 string,要么 null/缺失(不能是别的类型)。
	if v, ok := row["actual_split_kg"]; ok && v != nil {
		if _, isStr := v.(string); !isStr {
			t.Errorf("★ actual_split_kg 必须是 string 或 null, got %T(%v)", v, v)
		}
	}
	// 既有键不许被改掉。
	for _, key := range []string{
		"cut_type", "opening_kg", "purchased_kg", "sold_kg", "bulk_sold_kg",
		"expected_remain_kg", "actual_remain_kg", "variance_kg",
		"revenue_yuan", "sales_cost_yuan", "gross_margin_yuan", "gross_margin_pct",
		"moving_avg_unit_cost",
	} {
		if _, ok := row[key]; !ok {
			t.Errorf("★ 既有键 %s 不该消失", key)
		}
	}
}

// ---- 辅助 ----

// mustPigID 取当天那头猪的 ID(测试里只录一头)。
func mustPigID(t *testing.T, svc *service.Service) string {
	t.Helper()
	pigs, err := svc.ListWholePigsByDay(context.Background(), "S001", splitDay)
	mustNoErr(t, err, "ListWholePigsByDay")
	if len(pigs) == 0 {
		t.Fatalf("当天应该有猪")
	}
	return pigs[0].ID
}
