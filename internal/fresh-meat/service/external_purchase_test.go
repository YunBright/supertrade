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
//	① 录一笔 ⇒ 三处落库:外采单(事实)、位置表冷库 +kg、结转记录(source='external');
//	② **单价必须 > 0**(否则这批货零成本 ⇒ 卖出去全是利润 ⇒ 毛利虚高);
//	③ **货号必须有部位映射**(猜错部位 ⇒ 成本摊到错的部位上);
//	④ 改一笔 ⇒ 位置表按**差额**调;撤一笔 ⇒ 位置表回退 + 结转记录删掉;
//	⑤ 看板:外采**单列一栏**,**不动进猪口径**(owner 拍板);
//	⑥ 外采的货要**进冷库链**(否则"没搬上台的外采"会被算成假盘亏)。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
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

	// 位置表:冷库 +10kg(柜台不动)⇒ 门店能把它跟猪分割的货一起搬上台。
	cold, counter := fmPositions(t, "S001", sku)
	if !cold.Equal(d("10")) {
		t.Errorf("冷库应是 10kg, got %s", cold)
	}
	if !counter.IsZero() {
		t.Errorf("柜台不该有货(还没搬上台), got %s", counter)
	}

	// 结转记录:source='external'、归属快照为空(外采没有产出猪)。
	bags := fmBags(t)
	if len(bags) != 1 {
		t.Fatalf("应登记 1 行结转记录, got %d: %+v", len(bags), bags)
	}
	bag := bags[0]
	if bag.Source != service.BagSourceExternal {
		t.Errorf("结转记录来源应是 %q, got %q", service.BagSourceExternal, bag.Source)
	}
	if len(bag.PigAlloc) != 0 {
		t.Errorf("外采没有产出猪 ⇒ 归属快照必须为空, got %s", string(bag.PigAlloc))
	}
	if !bag.UnitCostYuan.Equal(d("24")) {
		t.Errorf("结转记录的成本快照应是门店给的单价 24, got %s", bag.UnitCostYuan)
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

	// 改数量 10 → 15 ⇒ 冷库 10 + 5 = 15(按**差额**调,不是覆盖成 15)。
	if _, err := svc.UpdateExternalPurchase(ctx, "S001", view.ID, service.ExternalPurchaseInput{
		CubeProductID: sku, QtyKg: d("15"), UnitPriceYuan: d("24"),
	}); err != nil {
		t.Fatalf("UpdateExternalPurchase: %v", err)
	}
	cold, _ := fmPositions(t, "S001", sku)
	if !cold.Equal(d("15")) {
		t.Errorf("改后冷库应是 15kg, got %s", cold)
	}

	// 撤 ⇒ 位置表回退、结转记录删掉、事实行删掉。
	if err := svc.DeleteExternalPurchase(ctx, "S001", view.ID); err != nil {
		t.Fatalf("DeleteExternalPurchase: %v", err)
	}
	cold, _ = fmPositions(t, "S001", sku)
	if !cold.IsZero() {
		t.Errorf("撤后冷库应回到 0, got %s", cold)
	}
	if bags := fmBags(t); len(bags) != 0 {
		t.Errorf("撤后结转记录应删掉, got %d 行", len(bags))
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

// ---- ⑥ 外采进冷库链 ----

// TestExternalPurchase_EntersColdStoreChain 锁 ⑥。
//
// ⚠️ owner 口径:「跟录猪一样,卖不完会在日终盘点体现,随日终盘点一起进冷库」
// ⇒ 外采的货**真的躺在冷库里** ⇒ 冷库链必须记它
// ⇒ 不记 ⇒ 「没搬上台的外采」会被算成**假盘亏**。
func TestExternalPurchase_EntersColdStoreChain(t *testing.T) {
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

	// 10-02 日终:外采的 10kg 还在冷库里(没搬上台、没卖)。
	_, cold := fmMarginClosing(t, "S001", day2, model.CutBelly)
	if !cold.Equal(d("10")) {
		t.Errorf("10-02 日终冷库应是 10kg(外采进链), got %s", cold)
	}
}
