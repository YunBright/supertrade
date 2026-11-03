package service_test

// 2026-11-03:**外采**(额外采购某个部位的货)的回归锁。
//
// # 外采是什么(owner 原话)
//
// 「在每日作业录入猪和搬上台之间增加一项录入外采,针对单独部位进行额外采购,
//   如 11 月 1 日采购了 20 斤货号 01401 对应部位肋排/排骨。」
//
// ⇒ 一笔外采 = (货号 → 部位、重量、单价)。
//
// # 剩下的不变量
//
//	① 录一笔 ⇒ 三处落库:外采单(事实)、位置表**猪肉台** +kg、结转记录(source='external');
//	② **单价必须 > 0**(否则这批货零成本 ⇒ 卖出去全是利润 ⇒ 毛利虚高);
//	③ **货号必须有部位映射**(猜错部位 ⇒ 成本摊到错的部位上);
//	④ 改一笔 ⇒ 位置表按**差额**调;撤一笔 ⇒ 位置表回退 + 结转记录删掉;
//	⑤ 看板:外采**单列一栏**,**不动进猪口径**(owner 拍板);
//	⑥ 外采**直接上猪肉台、不占冷库**(owner 2026-11-03 拍板)
//	   ⇒ 卖不完的随日终盘点进冷库,卖完的(盘点 0)进不了冷库。
//
// ⚠️ 位置口径在 2026-11-03 改过一次:原先是「先进冷库、再搬一次台」,
//   owner 拍板改成「直接上猪肉台」⇒ 门店不用多搬一次台,
//   也不会因为漏搬而**假盘亏**。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/shopspring/decimal"
)

// fmExtSetup 起服务 + 部位映射(belly → P-BELLY),时钟停在 10-01。
//
// ⚠️ **不录猪**:外采是"已经切好的某个部位",与进猪无关 ⇒ 场景要干净。
func fmExtSetup(t *testing.T) (*service.Service, context.Context, string) {
	t.Helper()
	svc, _ := setupTestService(t)
	ctx := context.Background()
	svc.SetClock(fmClock(fmDay1))
	if _, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-BELLY"}); err != nil {
		t.Fatalf("CreateBranchCutMapping: %v", err)
	}
	return svc, ctx, "P-BELLY"
}

// fmExtRows 查某店的外采单(全部)。
func fmExtRows(t *testing.T, branchID string) []model.ExternalPurchase {
	t.Helper()
	var rows []model.ExternalPurchase
	if err := testDBSingleton.Where("branch_id = ?", branchID).
		Order("created_at ASC, id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("query external_purchases: %v", err)
	}
	return rows
}

// ---- ① 录一笔 ⇒ 三处落库 ----

// TestExternalPurchase_RecordWritesFactStockAndCarryover 锁 ①。
func TestExternalPurchase_RecordWritesFactStockAndCarryover(t *testing.T) {
	svc, ctx, sku := fmExtSetup(t)

	view, err := svc.RecordExternalPurchase(ctx, "S001", service.ExternalPurchaseInput{
		CubeProductID: sku, QtyKg: d("10"), UnitPriceYuan: d("24"), Note: "供应商 X 送的",
	}, "u-1")
	mustNoErr(t, err, "RecordExternalPurchase")

	// 部位**自动带出**(门店只录货号,不挑部位)。
	if view.CutType != model.CutBelly {
		t.Errorf("部位应自动带出 %q, got %q", model.CutBelly, view.CutType)
	}
	// 总价 = 重量 × 单价(服务端算,不可填)。
	if !view.TotalAmountYuan.Equal(d("240")) {
		t.Errorf("总价应是 10×24=240, got %s", view.TotalAmountYuan)
	}

	// 事实行落库。
	rows := fmExtRows(t, "S001")
	if len(rows) != 1 {
		t.Fatalf("外采单应落 1 行, got %d", len(rows))
	}
	if rows[0].CubeProductID != sku || !rows[0].QtyKg.Equal(d("10")) {
		t.Errorf("外采单字段不对: %+v", rows[0])
	}

	// 位置表:**猪肉台** +10kg(冷库不动)⇒ 录完直接上架,不用再搬一次台。
	cold, counter := fmPositions(t, "S001", sku)
	if !cold.IsZero() {
		t.Errorf("外采不占冷库,冷库应是 0, got %s", cold)
	}
	if !counter.Equal(d("10")) {
		t.Errorf("猪肉台应是 10kg(录完直接上架), got %s", counter)
	}

	// 结转记录:source='external'、归属快照为空(外采没有产出猪)。
	records := fmRecords(t)
	if len(records) != 1 {
		t.Fatalf("应登记 1 行结转记录, got %d: %+v", len(records), records)
	}
	rec := records[0]
	if rec.Source != service.CarryoverSourceExternal {
		t.Errorf("结转记录来源应是 %q, got %q", service.CarryoverSourceExternal, rec.Source)
	}
	if rec.Location != string(model.StockLocationCounter) {
		t.Errorf("结转记录的位置应是猪肉台 %q, got %q", model.StockLocationCounter, rec.Location)
	}
	if len(rec.PigAlloc) != 0 {
		t.Errorf("外采没有产出猪 ⇒ 归属快照必须为空, got %s", string(rec.PigAlloc))
	}
	if !rec.UnitCostYuan.Equal(d("24")) {
		t.Errorf("结转记录的成本快照应是门店给的单价 24, got %s", rec.UnitCostYuan)
	}
}

// ---- ② 单价必须 > 0 ----

// TestExternalPurchase_RejectsZeroUnitPrice 锁 ②。
//
// ⚠️ 单价 0 ⇒ 这批货**零成本** ⇒ 卖出去全是利润 ⇒ 毛利虚高。
// ⇒ 必须拒,且要说清为什么。
func TestExternalPurchase_RejectsZeroUnitPrice(t *testing.T) {
	svc, ctx, sku := fmExtSetup(t)

	_, err := svc.RecordExternalPurchase(ctx, "S001", service.ExternalPurchaseInput{
		CubeProductID: sku, QtyKg: d("10"), UnitPriceYuan: d("0"),
	}, "u-1")
	if err == nil {
		t.Fatalf("单价 0 必须被拒(否则这批货零成本、毛利虚高)")
	}
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Errorf("应是输入校验错(400), got %v", err)
	}
	// 事实没落库。
	if rows := fmExtRows(t, "S001"); len(rows) != 0 {
		t.Errorf("被拒的外采不该落库, got %d 行", len(rows))
	}
}

// ---- ③ 货号必须有部位映射 ----

// TestExternalPurchase_RejectsUnmappedSku 锁 ③。
func TestExternalPurchase_RejectsUnmappedSku(t *testing.T) {
	svc, ctx, _ := fmExtSetup(t)

	_, err := svc.RecordExternalPurchase(ctx, "S001", service.ExternalPurchaseInput{
		CubeProductID: "NO-SUCH-SKU", QtyKg: d("10"), UnitPriceYuan: d("24"),
	}, "u-1")
	if err == nil {
		t.Fatalf("没配部位映射的货号必须被拒(猜错部位 ⇒ 成本摊到错的部位上)")
	}
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Errorf("应是输入校验错(400), got %v", err)
	}
}

// ---- ④ 改 / 撤 ----

// TestExternalPurchase_UpdateAndDelete 锁 ④。
func TestExternalPurchase_UpdateAndDelete(t *testing.T) {
	svc, ctx, sku := fmExtSetup(t)

	view, err := svc.RecordExternalPurchase(ctx, "S001", service.ExternalPurchaseInput{
		CubeProductID: sku, QtyKg: d("10"), UnitPriceYuan: d("24"),
	}, "u-1")
	mustNoErr(t, err, "RecordExternalPurchase")

	// 改数量 10 → 15 ⇒ 猪肉台 10 + 5 = 15(按**差额**调,不是覆盖成 15)。
	if _, err := svc.UpdateExternalPurchase(ctx, "S001", view.ID, service.ExternalPurchaseInput{
		CubeProductID: sku, QtyKg: d("15"), UnitPriceYuan: d("24"),
	}); err != nil {
		t.Fatalf("UpdateExternalPurchase: %v", err)
	}
	cold, counter := fmPositions(t, "S001", sku)
	if !cold.IsZero() {
		t.Errorf("外采不该占冷库, got %s", cold)
	}
	if !counter.Equal(d("15")) {
		t.Errorf("改后猪肉台应是 15kg, got %s", counter)
	}

	// 撤 ⇒ 位置表回退、结转记录删掉、事实行删掉。
	if err := svc.DeleteExternalPurchase(ctx, "S001", view.ID); err != nil {
		t.Fatalf("DeleteExternalPurchase: %v", err)
	}
	_, counter = fmPositions(t, "S001", sku)
	if !counter.IsZero() {
		t.Errorf("撤后猪肉台应回到 0, got %s", counter)
	}
	if records := fmRecords(t); len(records) != 0 {
		t.Errorf("撤后结转记录应删掉, got %d 行", len(records))
	}
	if rows := fmExtRows(t, "S001"); len(rows) != 0 {
		t.Errorf("撤后外采单应删掉, got %d 行", len(rows))
	}
}

// ---- ⑤ 看板:外采单列一栏,不动进猪口径 ----

// TestExternalPurchase_GrossMarginTotalHasSeparateExternalColumn 锁 ⑤。
//
// ⚠️ owner 2026-11-03 拍板:「进猪口径不动,新增独立『外采进货』字段、界面合计显示」
// ⇒ 断言两件事:① 外采字段有值;② **进猪字段不受影响**。
func TestExternalPurchase_GrossMarginTotalHasSeparateExternalColumn(t *testing.T) {
	svc, ctx, sku := fmExtSetup(t)

	if _, err := svc.RecordExternalPurchase(ctx, "S001", service.ExternalPurchaseInput{
		CubeProductID: sku, QtyKg: d("10"), UnitPriceYuan: d("24"),
	}, "u-1"); err != nil {
		t.Fatalf("RecordExternalPurchase: %v", err)
	}

	rep, err := svc.ComputeGrossMargin(ctx, "S001", fmDay1)
	mustNoErr(t, err, "ComputeGrossMargin")

	if !rep.Total.ExternalPurchaseKg.Equal(d("10")) {
		t.Errorf("外采重量应是 10kg, got %s", rep.Total.ExternalPurchaseKg)
	}
	if !rep.Total.ExternalPurchaseYuan.Equal(d("240")) {
		t.Errorf("外采金额应是 240, got %s", rep.Total.ExternalPurchaseYuan)
	}
	// ⚠️ 进猪口径不动:当天没进猪 ⇒ PurchasedKg 仍是 0(外采**不并进去**)。
	if !rep.Total.PurchasedKg.IsZero() {
		t.Errorf("外采不该并进进猪口径, PurchasedKg=%s", rep.Total.PurchasedKg)
	}
	if !rep.Total.PurchasedYuan.IsZero() {
		t.Errorf("外采不该并进进猪口径, PurchasedYuan=%s", rep.Total.PurchasedYuan)
	}
}

// ---- ⑥ 外采直接上猪肉台,不占冷库 ----

// TestExternalPurchase_StaysOutOfColdStoreUntilStocktake 锁 ⑥。
//
// ⚠️ owner 2026-11-03 拍板:「跟录猪一样,卖不完会在日终盘点体现,
//   随日终盘点一起进冷库,部位卖完,日终盘点为 0,就进不到冷库了。」
//
// ⇒ 录外采 ⇒ 货在**猪肉台**、**不占冷库** ⇒ 录完就能卖,不用再搬一次台。
// ⇒ 没卖完 ⇒ 日终盘点把它盘进「柜台实盘」⇒ 过夜搬运(后端自动)进冷库。
// ⇒ 卖完了(盘点 0)⇒ 进不了冷库。
//
// ⚠️ 判别力:把冷库链的 externalIn 加回去 ⇒ 下面第①段的 cold=0 立刻红。
func TestExternalPurchase_StaysOutOfColdStoreUntilStocktake(t *testing.T) {
	svc, ctx, sku := fmExtSetup(t)

	// 10-02 录一笔外采 10kg(10-01 没有前一营业日 ⇒ 第一天录没问题;
	// 这里先做 10-01 盘点,让 10-02 的链能接上)。
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("0")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("10-01 盘点: %v", err)
	}

	day2 := fmDay1.Add(24 * time.Hour)
	svc.SetClock(fmClock(day2))
	if _, err := svc.RecordExternalPurchase(ctx, "S001", service.ExternalPurchaseInput{
		CubeProductID: sku, QtyKg: d("10"), UnitPriceYuan: d("24"),
	}, "u-1"); err != nil {
		t.Fatalf("RecordExternalPurchase(10-02): %v", err)
	}

	// ① 录完、还没盘点 ⇒ 货在猪肉台,**冷库链上没有它**。
	cold, counter := fmPositions(t, "S001", sku)
	if !cold.IsZero() {
		t.Errorf("外采不该占冷库, got %s", cold)
	}
	if !counter.Equal(d("10")) {
		t.Errorf("录完外采猪肉台应是 10kg, got %s", counter)
	}
	_, chainCold := fmMarginClosing(t, "S001", day2, model.CutBelly)
	if !chainCold.IsZero() {
		t.Errorf("★ 外采不该进冷库链,10-02 日终冷库应是 0, got %s", chainCold)
	}

	// ② 日终盘点把它盘进「柜台实盘」(没卖完)⇒ 过夜搬运进冷库。
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("10")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("10-02 盘点: %v", err)
	}
	cold, counter = fmPositions(t, "S001", sku)
	if !cold.Equal(d("10")) {
		t.Errorf("★ 盘点后外采的 10kg 应过夜进冷库, got %s", cold)
	}
	if !counter.IsZero() {
		t.Errorf("盘点夜柜台应清空, got %s", counter)
	}
	_, chainCold = fmMarginClosing(t, "S001", day2, model.CutBelly)
	if !chainCold.Equal(d("10")) {
		t.Errorf("★ 10-02 日终冷库应是 10kg(随盘点结转进来的), got %s", chainCold)
	}
}

// ---- ⑦ 外采的量与金额真的进了「本期入库」 ----

// TestExternalPurchase_AddsToProducedKgAndAmount 锁 ⑦。
//
// ⚠️ owner 最关心的那一句:「进货成本变高了」
//   ⇒ 外采必须真的进**本期入库**(kg + 金额)⇒ 卖掉的那些成本进销售成本。
//
// # 为什么用「差值」断言
//
// ⚠️ 本期入库量里的猪分割部分 = **调整后分割权重 W**(applySplitWeights 的结果),
//   而 W 取决于建议/实际分割 ⇒ 在单测里不好直接写死。
// ⇒ 改用**差值**:同一场景跑两遍(有/无外采)⇒ 差值必须**精确等于**外采的 kg 与金额。
//
// ⚠️ 判别力:把 cut_cost.go 的 8c-2 叠加去掉 ⇒ 差值变成 0 ⇒ 本用例必红。
func TestExternalPurchase_AddsToProducedKgAndAmount(t *testing.T) {
	baseKg, baseAmount := fmExtProduced(t, false)
	withKg, withAmount := fmExtProduced(t, true)

	diffKg := withKg.Sub(baseKg)
	diffAmount := withAmount.Sub(baseAmount)
	if !diffKg.Equal(d("10")) {
		t.Errorf("★ 外采的 10kg 必须进本期入库量,实际只多了 %s kg", diffKg)
	}
	if !diffAmount.Equal(d("240")) {
		t.Errorf("★ 外采的 240 元必须进本期入库金额,实际只多了 %s 元", diffAmount)
	}
}

// fmExtProduced 跑一遍「录猪 + 分割」场景,返回该部位的**本期入库**(kg / 元)。
//
// withExternal=true 时额外录一笔 10kg / 24 元每kg 的外采。
func fmExtProduced(t *testing.T, withExternal bool) (decimal.Decimal, decimal.Decimal) {
	t.Helper()
	svc, ctx, sku := fmExtSetup(t)
	// ⚠️ 注入**同步**物化执行器(生产是异步)⇒ 断言不用等后台任务。
	svc.SetChainMaterializer(func(f func()) { f() })

	pigID := fmSeedPigAt(t, svc, "EB-1", "50", fmDay1)
	fmSeedPigCut(t, svc, pigID, sku, "10")
	if withExternal {
		if _, err := svc.RecordExternalPurchase(ctx, "S001", service.ExternalPurchaseInput{
			CubeProductID: sku, QtyKg: d("10"), UnitPriceYuan: d("24"),
		}, "u-1"); err != nil {
			t.Fatalf("RecordExternalPurchase: %v", err)
		}
	}
	// ⚠️ **两个场景必须走同一条物化触发路径**，否则差值没意义。
	//   `ComputeGrossMargin` 是**读**路径(不落快照)⇒ 这里用**盘点**触发链物化
	//   (盘 0kg ⇒ 不改变本期入库量,只负责把快照写出来)。
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("0")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("10-01 盘点(触发链物化): %v", err)
	}
	rec, ok := fmCutCostDaily(t, "S001", "2026-10-01", model.CutBelly)
	if !ok {
		t.Fatalf("成本日快照里没有五花肉这一行(withExternal=%v)", withExternal)
	}
	return rec.ProducedKg, rec.ProducedAmount
}
