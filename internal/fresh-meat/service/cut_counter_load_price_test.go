package service_test

// 2026-10-30:「搬上台 / 单价(建议单价 · 实际均价)」三列的口径锁。
//
// # 背景(owner 看报表后提的)
//
//	「这张表(甚至整个鲜肉-毛利看板页)都是**猪肉台**的数据」
//	⇒ ① 「期初」列(账面链值,可以是负数 = 账面短卖)门店看不懂 ⇒ 改成「搬上台」;
//	② 「冷库单价」这一列不涉及冷库 ⇒ 换成「单价」:上=建议单价、下=实际均价。
//
// # 锁的是 owner 的口径(不是实现细节)
//
//	① 搬上台 = 当日**搬上台流水**按部位求和(门店自己搬的量);
//	   没搬 = 0(是**真 0**,不是「—」—— 今天确实没搬)。
//	② 建议单价(元/kg) = (期初金额 + 今日入库金额按**建议**比例摊到的部分)
//	                   ÷ (期初kg + 建议分割kg)
//	   ⇒ nil = 当天**没有建议分割**(没进猪)⇒ 前端显示「—」(不是 0)。
//	③ 实际均价(元/kg) = 营收 ÷ 销量(零售 + 统收)⇒ 与「营收」列**自洽**
//	   (均价 × 销量 = 营收,门店能自己验算);
//	   ⇒ nil = 当天这个部位**没卖** ⇒ 前端显示「—」。
//
// ⚠ 这些用例都是**端到端**的(走公开 API + 真实 SQLite)⇒ 引擎接线错了也会红。

import (
	"context"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"gorm.io/datatypes"
)

// cpSplitDay 是本文件用的营业日(2026-10-29 14:00 +08)。
var cpSplitDay = time.Date(2026, 10, 29, 6, 0, 0, 0, time.UTC)

// cpSetup 起服务 + 配 belly 映射 + 录一条**期初结转**(100kg @ 18 元)。
//
// ⚠ 期初结转是**链的起点**(生效日 2026-10-28)⇒
//
//	门店在生效日重新定义了 belly 的库存与进价 ⇒ 这一天(10-29)的期初
//	= 100kg / 1800 元(= 门店给的进价定价)。
func cpSetup(t *testing.T) (*service.Service, context.Context, string) {
	t.Helper()
	svc, _ := setupTestService(t)
	ctx := context.Background()
	svc.SetClock(fmClock(cpSplitDay))
	_, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-BELLY"})
	mustNoErr(t, err, "CreateBranchCutMapping")
	_, err = svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-28", ColdStoreKg: d("100"), UnitPriceYuan: d("18")},
	})
	mustNoErr(t, err, "SaveStockOpenings")
	return svc, ctx, "P-BELLY"
}

// cpSell 记一笔零售销售(本地事件表)。
func cpSell(t *testing.T, svc *service.Service, sku string, lineID, qty, amount string) {
	t.Helper()
	_, err := svc.OnSaleCompleted(context.Background(), service.SaleCompletedPayload{
		SaleID: "SO-" + lineID, BranchID: "S001", CompletedAt: cpSplitDay.Add(time.Hour),
		Lines: []service.SaleLine{{
			LineID: lineID, SKUID: sku, FreshType: "meat",
			Qty: d(qty), AmountYuan: d(amount), PigID: "PIG-1", CutType: model.CutBelly,
		}},
	})
	mustNoErr(t, err, "OnSaleCompleted "+lineID)
}

// cpBulk 记一笔**统收**(本地登记,source='bulk';零售走 cube、统收永远走本地)。
func cpBulk(t *testing.T, qty, amount string) {
	t.Helper()
	mustNoErr(t, testDBSingleton.Create(&model.LineSalesByPig{
		BranchID: "S001", SaleDate: datatypes.Date(cpSplitDay), PigID: "PIG-1",
		CutType: model.CutBelly, QtyKg: d(qty), AmountYuan: d(amount),
		UnitPriceYuan: d("0"), Source: "bulk", PosLineID: "BULK-1",
		PosBranchID: "S001", CubeProductID: "P-BELLY", OrderStatus: "S",
		CreatedAt: time.Now(),
	}).Error, "插本地统收行")
}

// ---- 搬上台列 ----

// TestCounterLoadColumn_ShowsMovedKg 锁「搬上台」列 = 当日**搬上台流水**按部位求和。
//
// 门店搬了 20kg belly 上台 ⇒ 该列就是 20(门店自己认得的数)。
func TestCounterLoadColumn_ShowsMovedKg(t *testing.T) {
	svc, ctx, sku := cpSetup(t)
	if _, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		TakenDate: "2026-10-29",
		Lines:     []service.CounterLoadLine{{CubeProductID: sku, QtyKg: d("20")}},
	}, "u-1"); err != nil {
		t.Fatalf("CounterLoad: %v", err)
	}

	rep, err := svc.ComputeGrossMargin(ctx, "S001", cpSplitDay)
	mustNoErr(t, err, "ComputeGrossMargin")
	belly := findPerCut(t, rep.PerCut, model.CutBelly)
	if !belly.CounterLoadKg.Equal(d("20")) {
		t.Errorf("★ 搬上台应是 20, got %s", belly.CounterLoadKg)
	}
}

// TestCounterLoadColumn_ZeroWhenNothingMoved 锁「没搬 = **真 0**」(不是「—」)。
//
// ⚠ 为什么必须是 0 而不是 nil:这一列取代的是「期初」列,「期初」没有值时也是 0
//
//	⇒ 门店看到「—」会以为"系统没算出来" ⇒ 0 才是"今天确实没搬"。
func TestCounterLoadColumn_ZeroWhenNothingMoved(t *testing.T) {
	svc, ctx, _ := cpSetup(t)

	rep, err := svc.ComputeGrossMargin(ctx, "S001", cpSplitDay)
	mustNoErr(t, err, "ComputeGrossMargin")
	belly := findPerCut(t, rep.PerCut, model.CutBelly)
	if !belly.CounterLoadKg.IsZero() {
		t.Errorf("★ 没搬台 ⇒ 搬上台应是 0, got %s", belly.CounterLoadKg)
	}
}

// TestCounterLoadColumn_NotTheBookOpening 锁「搬上台」**不是**账面期初。
//
// 门店搬了 20kg,账面期初 100kg(门店自己录的冷库结转)⇒
// 两列是**不同的数**:搬上台 20 / 期初 100 ⇒ 不能互相串。
func TestCounterLoadColumn_NotTheBookOpening(t *testing.T) {
	svc, ctx, sku := cpSetup(t)
	if _, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		TakenDate: "2026-10-29",
		Lines:     []service.CounterLoadLine{{CubeProductID: sku, QtyKg: d("20")}},
	}, "u-1"); err != nil {
		t.Fatalf("CounterLoad: %v", err)
	}

	rep, err := svc.ComputeGrossMargin(ctx, "S001", cpSplitDay)
	mustNoErr(t, err, "ComputeGrossMargin")
	belly := findPerCut(t, rep.PerCut, model.CutBelly)
	if !belly.CounterLoadKg.Equal(d("20")) {
		t.Errorf("★ 搬上台应是 20, got %s", belly.CounterLoadKg)
	}
	if !belly.OpeningKg.Equal(d("100")) {
		t.Errorf("★ 账面期初仍应是 100(别被搬上台串了), got %s", belly.OpeningKg)
	}
}

// ---- 建议单价 ----

// TestSuggestedUnitCost_Formula 锁「建议单价」的公式。
//
// 场景(手算,不看实现):
//
//	期初结转:100kg @ 18 元 ⇒ 期初金额 = 1800 元
//	进猪:20kg @ 20 元/kg ⇒ 猪成本 = 400 元;实录 belly 20kg
//	统计基线:20kg 的猪 ⇒ belly 建议分割 = 20kg(= 实际分割 ⇒ 不重分配)
//
//	今日入库金额 = 400 元(按 W 重分后总量不变)
//	建议单价 = (期初金额 1800 + 400 × 20/20) ÷ (期初kg 100 + 建议分割 20)
//	         = 2200 ÷ 120 = 18.333333…
//
// ⇒ 这就是「如果今天的猪按建议分割切,这刀货的成本单价」。
func TestSuggestedUnitCost_Formula(t *testing.T) {
	svc, ctx, sku := cpSetup(t)
	// 统计基线:20kg 的猪落在体重段 "0-49" ⇒ belly 建议 20kg。
	splitBaseline(t, model.CutBelly, "0-49", "20")
	pig, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-1", GrossWeightKg: d("20"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: cpSplitDay,
	}, "u-1")
	mustNoErr(t, err, "RecordWholePig")
	_, err = svc.RecordPigCut(ctx, "S001", model.RecordPigCutInput{
		PigID: pig.ID, CutType: model.CutBelly, Barcode: "B-1",
		WeightKg: d("20"), ExpiresAt: cpSplitDay.Add(48 * time.Hour), CubeProductID: sku,
	}, "u-1")
	mustNoErr(t, err, "RecordPigCut")

	rep, err := svc.ComputeGrossMargin(ctx, "S001", cpSplitDay)
	mustNoErr(t, err, "ComputeGrossMargin")
	belly := findPerCut(t, rep.PerCut, model.CutBelly)

	if !belly.SuggestedSplitKg.Equal(d("20")) {
		t.Fatalf("前置:建议分割应是基线 p50 = 20, got %s", belly.SuggestedSplitKg)
	}
	if belly.SuggestedUnitCostYuan == nil {
		t.Fatalf("★ 有建议分割 ⇒ 建议单价必须有值")
	}
	want := d("2200").Div(d("120"))
	if belly.SuggestedUnitCostYuan.Sub(want).Abs().GreaterThan(d("0.0001")) {
		t.Errorf("★ 建议单价应 = (1800 + 400) ÷ (100 + 20) = %s, got %s",
			want, *belly.SuggestedUnitCostYuan)
	}
}

// TestSuggestedUnitCost_NilWhenNoSuggestion 锁「没有建议分割 ⇒ **没有**建议单价」。
//
// ⚠ 必须是 nil(前端显示「—」),**不是 0** ——
//
//	0 元/kg 会被门店读成"这刀货不要钱",而真实含义是"今天没进猪,没有建议可算"。
func TestSuggestedUnitCost_NilWhenNoSuggestion(t *testing.T) {
	svc, ctx, _ := cpSetup(t)

	rep, err := svc.ComputeGrossMargin(ctx, "S001", cpSplitDay)
	mustNoErr(t, err, "ComputeGrossMargin")
	belly := findPerCut(t, rep.PerCut, model.CutBelly)
	if belly.SuggestedUnitCostYuan != nil {
		t.Errorf("★ 没进猪 ⇒ 建议单价必须是 nil(前端显示「—」), got %s", *belly.SuggestedUnitCostYuan)
	}
	// JSON 里也不能出现 "0" 冒充。
	for _, row := range splitJSONRows(t, rep) {
		if v, ok := row["suggested_unit_cost_yuan"]; ok && v != nil {
			t.Errorf("★ suggested_unit_cost_yuan 键必须是 null/缺失, got %v", v)
		}
	}
}

// ---- 实际均价 ----

// TestActualAvgUnitPrice_EqualsRevenueOverQty 锁「实际均价」= 营收 ÷ 销量。
//
// 卖 10kg 收 200 元 ⇒ 均价 20 元/kg。
func TestActualAvgUnitPrice_EqualsRevenueOverQty(t *testing.T) {
	svc, ctx, sku := cpSetup(t)
	cpSell(t, svc, sku, "L-1", "10", "200")

	rep, err := svc.ComputeGrossMargin(ctx, "S001", cpSplitDay)
	mustNoErr(t, err, "ComputeGrossMargin")
	belly := findPerCut(t, rep.PerCut, model.CutBelly)

	if belly.ActualAvgUnitPriceYuan == nil {
		t.Fatalf("★ 卖了货 ⇒ 实际均价必须有值")
	}
	if !belly.ActualAvgUnitPriceYuan.Equal(d("20")) {
		t.Errorf("★ 实际均价应 = 200 ÷ 10 = 20, got %s", *belly.ActualAvgUnitPriceYuan)
	}
	// 与「营收」列**自洽**:均价 × 销量 = 营收(门店能自己验算)。
	want := belly.ActualAvgUnitPriceYuan.Mul(belly.SoldKg.Add(belly.BulkSoldKg))
	if want.Sub(belly.RevenueYuan).Abs().GreaterThan(d("0.0001")) {
		t.Errorf("★ 均价 × 销量(%s)应等于营收(%s)", want, belly.RevenueYuan)
	}
}

// TestActualAvgUnitPrice_IncludesBulk 锁「统收也计入均价」(与「营收」列同口径)。
//
// 零售 10kg / 200 元 + 统收 5kg / 30 元
// ⇒ 营收 230 元、销量 15kg ⇒ 均价 = 230 ÷ 15 = 15.333333…
func TestActualAvgUnitPrice_IncludesBulk(t *testing.T) {
	svc, ctx, sku := cpSetup(t)
	cpSell(t, svc, sku, "L-1", "10", "200")
	cpBulk(t, "5", "30")

	rep, err := svc.ComputeGrossMargin(ctx, "S001", cpSplitDay)
	mustNoErr(t, err, "ComputeGrossMargin")
	belly := findPerCut(t, rep.PerCut, model.CutBelly)

	if belly.ActualAvgUnitPriceYuan == nil {
		t.Fatalf("★ 卖了货 ⇒ 实际均价必须有值")
	}
	want := d("230").Div(d("15"))
	if belly.ActualAvgUnitPriceYuan.Sub(want).Abs().GreaterThan(d("0.0001")) {
		t.Errorf("★ 实际均价应 = (零售 200 + 统收 30) ÷ (10 + 5) = %s, got %s",
			want, *belly.ActualAvgUnitPriceYuan)
	}
	if !belly.RevenueYuan.Equal(d("230")) {
		t.Errorf("★ 营收应含统收 = 230, got %s", belly.RevenueYuan)
	}
}

// TestActualAvgUnitPrice_NilWhenNotSold 锁「没卖 ⇒ 没有实际均价」。
func TestActualAvgUnitPrice_NilWhenNotSold(t *testing.T) {
	svc, ctx, _ := cpSetup(t)

	rep, err := svc.ComputeGrossMargin(ctx, "S001", cpSplitDay)
	mustNoErr(t, err, "ComputeGrossMargin")
	belly := findPerCut(t, rep.PerCut, model.CutBelly)
	if belly.ActualAvgUnitPriceYuan != nil {
		t.Errorf("★ 没卖 ⇒ 实际均价必须是 nil(前端显示「—」), got %s", *belly.ActualAvgUnitPriceYuan)
	}
	for _, row := range splitJSONRows(t, rep) {
		if v, ok := row["actual_avg_unit_price_yuan"]; ok && v != nil {
			t.Errorf("★ actual_avg_unit_price_yuan 键必须是 null/缺失, got %v", v)
		}
	}
}

// ---- wire 契约 ----

// TestCounterLoadPrice_WireContractKeys 锁前端依赖的**三个新键**(名字/类型不许变)。
//
// ⚠ `counter_load_kg` 是**值**(永远是 string,0 也得有);
// ⚠ 两个单价字段**可空** ⇒ 只能是 string 或 null/缺失(不能是 "0" 冒充)。
func TestCounterLoadPrice_WireContractKeys(t *testing.T) {
	svc, ctx, sku := cpSetup(t)
	splitBaseline(t, model.CutBelly, "0-49", "20")
	pig, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-1", GrossWeightKg: d("20"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: cpSplitDay,
	}, "u-1")
	mustNoErr(t, err, "RecordWholePig")
	_, err = svc.RecordPigCut(ctx, "S001", model.RecordPigCutInput{
		PigID: pig.ID, CutType: model.CutBelly, Barcode: "B-1",
		WeightKg: d("20"), ExpiresAt: cpSplitDay.Add(48 * time.Hour), CubeProductID: sku,
	}, "u-1")
	mustNoErr(t, err, "RecordPigCut")
	cpSell(t, svc, sku, "L-1", "5", "100")

	rep, err := svc.ComputeGrossMargin(ctx, "S001", cpSplitDay)
	mustNoErr(t, err, "ComputeGrossMargin")
	rows := splitJSONRows(t, rep)
	if len(rows) == 0 {
		t.Fatalf("per_cut 不该是空的")
	}
	row := rows[0]
	// 搬上台:永远是 string。
	v, ok := row["counter_load_kg"]
	if !ok {
		t.Fatalf("★ wire 键 counter_load_kg 缺失")
	}
	if _, isStr := v.(string); !isStr {
		t.Errorf("★ wire 键 counter_load_kg 必须是 string(decimal), got %T(%v)", v, v)
	}
	// 两个单价:string 或 null/缺失。
	for _, key := range []string{"suggested_unit_cost_yuan", "actual_avg_unit_price_yuan"} {
		v, ok := row[key]
		if !ok || v == nil {
			continue
		}
		if _, isStr := v.(string); !isStr {
			t.Errorf("★ wire 键 %s 必须是 string 或 null, got %T(%v)", key, v, v)
		}
	}
}
