package service_test

// 2026-10-25:大宗结算的 FIFO 队列统一到**袋子**的回归锁。
//
// # 为什么锁这条
//
// 旧实现:大宗结算 FIFO 吃的是 **bulk_productions 台账**,而零售/搬台吃的是
// **stock_bags 袋子** ⇒ 同一批冷库货有**两份"还剩多少"的账**
// ⇒ 结算按台账扣、零售按袋子扣 ⇒ **同一批货能被卖两遍**
// (结算说"还有 10kg 可结",零售把那 10kg 也卖了 ⇒ 收入算两遍、库存算负)。
//
// # 三条不变量
//
//  ① 结算的「领用」落在**袋子**上:分摊凭据带 bag_id、袋子的 consumed_kg 增加;
//  ② 结存(balance_kg)与冷库余额一致 —— 同一本账;
//  ③ 同一批货**不能结两遍**:第二次结算报「可领用量不足」。
//
// # 顺带锁住
//
//  ④ 大宗台账(bulk_productions)**停写**:结转记录改由**结转记录(袋子)**派生;
//  ⑤ 手工兜底产出也进队列(它冷库里有货、账上也得有,否则结不出去)。

import (
	"context"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/shopspring/decimal"
)

// fmBulkBagSetup 铺一个「一头猪 + 一袋板油」的场景。
//
// 返回 (服务, ctx, 部位主 SKU, 猪 ID, 营业日 "YYYY-MM-DD")。
func fmBulkBagSetup(t *testing.T) (*service.Service, context.Context, string, string, string) {
	t.Helper()
	svc, _ := setupTestService(t)
	ctx := context.Background()
	bizLoc := time.FixedZone("Asia/Shanghai", 8*3600)
	svc.SetClock(func() time.Time { return time.Date(2026, 10, 1, 14, 15, 0, 0, bizLoc) })

	const sku = "P-FAT"
	if _, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutFat, CubeProductID: sku}); err != nil {
		t.Fatalf("CreateBranchCutMapping: %v", err)
	}
	if _, err := svc.CreateBulkProduct(ctx, "S001", service.BulkProductInput{
		CutType: model.CutFat,
	}); err != nil {
		t.Fatalf("CreateBulkProduct: %v", err)
	}
	pig, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-FAT-1", GrossWeightKg: d("200"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC),
	}, "u-1")
	if err != nil {
		t.Fatalf("RecordWholePig: %v", err)
	}
	// 分割补录:这头猪出 10kg 板油 ⇒ 盘点那天的归属快照里就有它。
	if _, err := svc.RecordPigCut(ctx, "S001", model.RecordPigCutInput{
		PigID: pig.ID, CutType: model.CutFat, Barcode: "BC-FAT-1",
		WeightKg: d("10"), ExpiresAt: time.Date(2026, 10, 3, 6, 0, 0, 0, time.UTC),
		CubeProductID: sku,
	}, "u-1"); err != nil {
		t.Fatalf("RecordPigCut: %v", err)
	}
	// 日终盘点:板油实盘 10kg ⇒ 登记成一行结转记录(进冷库)。
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutFat, CubeProductID: sku, ActualRemainKg: d("10")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("RecordPorkCutsStocktake: %v", err)
	}
	return svc, ctx, sku, pig.ID, "2026-10-01"
}

// TestBulkFifo_SettlementConsumesBag 锁「结算的领用落在袋子上」。
func TestBulkFifo_SettlementConsumesBag(t *testing.T) {
	svc, ctx, _, pigID, date := fmBulkBagSetup(t)

	// 盘点后:一行结转记录 10kg,还没被消耗。
	bags, err := svc.ListStockBags(ctx, "S001", "", date)
	mustNoErr(t, err, "ListStockBags")
	if len(bags) != 1 {
		t.Fatalf("应登记 1 行结转记录, got %d: %+v", len(bags), bags)
	}
	bag := bags[0]
	if !bag.ConsumedKg.IsZero() {
		t.Fatalf("新登记的结转行不该有消耗量, got %s", bag.ConsumedKg)
	}
	if bag.Source != "stocktake" {
		t.Errorf("结转行来源应是 stocktake, got %q", bag.Source)
	}

	// 结存:可领用 10kg、冷库 10kg(同一本账 ⇒ 差额 0)。
	bal, err := svc.ListBulkBalances(ctx, "S001")
	mustNoErr(t, err, "ListBulkBalances")
	if len(bal) != 1 {
		t.Fatalf("应有 1 个结存行, got %d", len(bal))
	}
	if !bal[0].BalanceKg.Equal(d("10")) {
		t.Errorf("可领用应 = 10kg, got %s", bal[0].BalanceKg)
	}
	if !bal[0].ColdStoreKg.Equal(d("10")) {
		t.Errorf("冷库余额应 = 10kg, got %s", bal[0].ColdStoreKg)
	}
	if !bal[0].LedgerVsColdDiffKg.IsZero() {
		t.Errorf("队列与冷库应一致(差额 0), got %s", bal[0].LedgerVsColdDiffKg)
	}

	// 结算 6kg × 6 元。
	view, err := svc.CreateBulkSettlement(ctx, "S001", service.BulkSettlementInput{
		SettleDate:   date,
		Counterparty: "某酒店",
		Mode:         service.BulkModeDelivery,
		Items: []service.BulkSettlementItemInput{
			{ProductCode: "fat", QtyKg: d("6"), UnitPriceYuan: d("6")},
		},
	}, "u-1")
	mustNoErr(t, err, "CreateBulkSettlement")
	if !view.TotalAmountYuan.Equal(d("36")) {
		t.Errorf("总金额应 = 6×6 = 36, got %s", view.TotalAmountYuan)
	}

	// ★ 袋子被扣了 6kg —— 结算的「领用」落在**袋子**上。
	after, err := svc.ListStockBags(ctx, "S001", "", date)
	mustNoErr(t, err, "ListStockBags(after)")
	if len(after) != 1 {
		t.Fatalf("应有 1 行结转记录, got %d", len(after))
	}
	if !after[0].ConsumedKg.Equal(d("6")) {
		t.Errorf("袋子的已消耗量应 = 6kg, got %s", after[0].ConsumedKg)
	}

	// 分摊凭据必须指向**袋子**(bag_id 非空)—— 这是"领用与袋子对齐"的凭据。
	var allocCount int64
	if err := testDBSingleton.Model(&model.BulkSettlementAllocation{}).
		Where("settlement_id = ? AND bag_id <> ''", view.ID).
		Count(&allocCount).Error; err != nil {
		t.Fatalf("count allocations: %v", err)
	}
	if allocCount == 0 {
		t.Fatalf("分摊凭据应带 bag_id(结算领用的是袋子), got 0")
	}

	// 收入归到**产出它的那头猪**(归属快照)。
	var rows []model.LineSalesByPig
	if err := testDBSingleton.Where("pos_branch_id = ? AND source = ?", "S001", "bulk").
		Find(&rows).Error; err != nil {
		t.Fatalf("query line_sales_by_pigs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("应有 1 行大宗收入, got %d", len(rows))
	}
	if rows[0].PigID != pigID {
		t.Errorf("收入应归到产出它的那头猪 %s, got %s", pigID, rows[0].PigID)
	}
	if !rows[0].QtyKg.Equal(d("6")) || !rows[0].AmountYuan.Equal(d("36")) {
		t.Errorf("收入行不对: qty=%s amount=%s", rows[0].QtyKg, rows[0].AmountYuan)
	}

	// 结存:可领用 4kg、冷库 4kg。
	bal2, err := svc.ListBulkBalances(ctx, "S001")
	mustNoErr(t, err, "ListBulkBalances(after)")
	if !bal2[0].BalanceKg.Equal(d("4")) {
		t.Errorf("结算后可领用应 = 4kg, got %s", bal2[0].BalanceKg)
	}
	if !bal2[0].ColdStoreKg.Equal(d("4")) {
		t.Errorf("结算后冷库应 = 4kg, got %s", bal2[0].ColdStoreKg)
	}
}

// TestBulkFifo_CannotSettleSameGoodsTwice 锁「同一批货不能结两遍」。
//
// ⚠️ 这就是老实现最大的洞:结算按台账扣、零售按袋子扣 ⇒ 同一批货能被卖两遍。
// ⇒ 现在两边共用 consumed_kg 水位 ⇒ 结完就没了 ⇒ 再结必报"可领用量不足"。
func TestBulkFifo_CannotSettleSameGoodsTwice(t *testing.T) {
	svc, ctx, _, _, date := fmBulkBagSetup(t)

	// 第一次:结掉 8kg。
	if _, err := svc.CreateBulkSettlement(ctx, "S001", service.BulkSettlementInput{
		SettleDate:   date,
		Counterparty: "某酒店",
		Mode:         service.BulkModeDelivery,
		Items: []service.BulkSettlementItemInput{
			{ProductCode: "fat", QtyKg: d("8"), UnitPriceYuan: d("6")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("第一次结算: %v", err)
	}

	// 第二次:只剩 2kg,却要结 8kg ⇒ 必须报错(而不是把同一批货再结一遍)。
	_, err := svc.CreateBulkSettlement(ctx, "S001", service.BulkSettlementInput{
		SettleDate:   date,
		Counterparty: "某酒店",
		Mode:         service.BulkModeDelivery,
		Items: []service.BulkSettlementItemInput{
			{ProductCode: "fat", QtyKg: d("8"), UnitPriceYuan: d("6")},
		},
	}, "u-1")
	if err == nil {
		t.Fatalf("同一批货不该能被结两遍")
	}

	// 第二次结算**没有留下半张单**(整单回滚)。
	list, err := svc.ListBulkSettlements(ctx, "S001")
	mustNoErr(t, err, "ListBulkSettlements")
	if len(list) != 1 {
		t.Errorf("应只有第一次结算那 1 张单, got %d", len(list))
	}

	// 收入也只有一次(6×8=48)。
	var rows []model.LineSalesByPig
	if err := testDBSingleton.Where("pos_branch_id = ? AND source = ?", "S001", "bulk").
		Find(&rows).Error; err != nil {
		t.Fatalf("query line_sales_by_pigs: %v", err)
	}
	total := decimal.Zero
	for _, r := range rows {
		total = total.Add(r.AmountYuan)
	}
	if !total.Equal(d("48")) {
		t.Errorf("大宗收入合计应 = 48(只结一次), got %s", total)
	}
}

// TestBulkFifo_ManualProductionEntersQueue 锁「手工兜底产出也进队列」。
//
// ⚠️ 手工补录的货**在冷库里** ⇒ 必须**能结出去** ⇒ 必须也在同一个 FIFO 队列里。
func TestBulkFifo_ManualProductionEntersQueue(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()
	bizLoc := time.FixedZone("Asia/Shanghai", 8*3600)
	svc.SetClock(func() time.Time { return time.Date(2026, 10, 1, 14, 15, 0, 0, bizLoc) })

	const sku = "P-FAT"
	if _, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutFat, CubeProductID: sku}); err != nil {
		t.Fatalf("CreateBranchCutMapping: %v", err)
	}
	if _, err := svc.CreateBulkProduct(ctx, "S001", service.BulkProductInput{
		CutType: model.CutFat,
	}); err != nil {
		t.Fatalf("CreateBulkProduct: %v", err)
	}
	// 手工补录 4kg 进冷库。
	prod, err := svc.RecordBulkProduction(ctx, "S001", service.BulkProductionInput{
		ProductCode: "fat", QtyKg: d("4"),
	}, "u-1")
	mustNoErr(t, err, "RecordBulkProduction")

	// 它必须在队列里(结转记录 source=bulk_manual)。
	var bags []model.StockBag
	if err := testDBSingleton.Where("branch_id = ? AND source = ?", "S001", "bulk_manual").
		Find(&bags).Error; err != nil {
		t.Fatalf("query stock_bags: %v", err)
	}
	if len(bags) != 1 {
		t.Fatalf("手工补录应登记 1 行结转记录, got %d: %+v", len(bags), bags)
	}
	if !bags[0].QtyKg.Equal(d("4")) {
		t.Errorf("结转记录重量应 = 4kg, got %s", bags[0].QtyKg)
	}

	// 大宗台账**停写** ⇒ 表里不该有这行(同一事实只记一处)。
	var ledgerRows int64
	if err := testDBSingleton.Model(&model.BulkProduction{}).
		Where("branch_id = ?", "S001").Count(&ledgerRows).Error; err != nil {
		t.Fatalf("count bulk_productions: %v", err)
	}
	if ledgerRows != 0 {
		t.Errorf("大宗台账已停写,不应再有新行, got %d", ledgerRows)
	}
	// 但返回体仍是老形状(前端/契约测试不用改)。
	if prod.ProductCode != "fat" || !prod.QtyKg.Equal(d("4")) {
		t.Errorf("手工补录返回体形状应不变: %+v", prod)
	}

	// 结存页能看到它(可领用 4kg)。
	bal, err := svc.ListBulkBalances(ctx, "S001")
	mustNoErr(t, err, "ListBulkBalances")
	if len(bal) != 1 || !bal[0].BalanceKg.Equal(d("4")) {
		t.Fatalf("可领用应 = 4kg, got %+v", bal)
	}

	// ⇒ 能结出去。
	if _, err := svc.CreateBulkSettlement(ctx, "S001", service.BulkSettlementInput{
		SettleDate:   "2026-10-01",
		Counterparty: "某酒店",
		Mode:         service.BulkModeDelivery,
		Items: []service.BulkSettlementItemInput{
			{ProductCode: "fat", QtyKg: d("4"), UnitPriceYuan: d("6")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("结算手工补录的货: %v", err)
	}
}
