package service_test

// 2026-10-24:**袋子 / 标签号**的回归锁。
//
// 门店口径:「盘点时将剩余的鲜肉商品按 sku 打包过秤然后登记,每个袋子夹上标签,
// 方便次日搬上猪肉台继续售卖和毛利回归。」
//
// ⇒ 三条不变量:
//  ① 盘点带标签的行 ⇒ 登记成一袋(SKU + 重量 + 标签号 + 产生日);
//  ② 按标签**整袋搬**上台 ⇒ 冷库 −整袋、猪肉台 +整袋、**标签号释放**(可复用);
//  ③ 袋子是"搬上台流水"的来源 ⇒ 每日毛利的分摊重量靠它(老货不能再摊一次猪成本)。

import (
	"context"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

// fmBagSetup 起一个服务并铺好"一个 SKU + 一头猪"的最小场景。
func fmBagSetup(t *testing.T) (*service.Service, context.Context, string) {
	t.Helper()
	svc, _ := setupTestService(t)
	ctx := context.Background()
	svc.SetClock(fmClock(fmDay1))
	if _, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-BELLY"}); err != nil {
		t.Fatalf("CreateBranchCutMapping: %v", err)
	}
	if _, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-1", GrossWeightKg: d("200"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: fmDay1,
	}, "u-1"); err != nil {
		t.Fatalf("RecordWholePig: %v", err)
	}
	return svc, ctx, "P-BELLY"
}

// TestStockBag_RegisterAndMoveByLabel 锁"盘点建袋 + 按标签整袋搬 + 标签释放"。
func TestStockBag_RegisterAndMoveByLabel(t *testing.T) {
	svc, ctx, sku := fmBagSetup(t)

	// 盘点:五花肉剩 12kg,贴标签 BAG-001
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("12"), Label: "BAG-001"},
		},
	}, "u-1"); err != nil {
		t.Fatalf("RecordPorkCutsStocktake: %v", err)
	}

	bags, err := svc.ListStockBags(ctx, "S001", "cold_store", "", "")
	if err != nil {
		t.Fatalf("ListStockBags: %v", err)
	}
	if len(bags) != 1 {
		t.Fatalf("应登记 1 个袋子, got %d: %+v", len(bags), bags)
	}
	bg := bags[0]
	if bg.Label != "BAG-001" || bg.CubeProductID != sku {
		t.Errorf("袋子字段不对: %+v", bg)
	}
	if !bg.QtyKg.Equal(d("12")) {
		t.Errorf("袋子重量应为 12kg, got %s", bg.QtyKg)
	}
	if bg.Status != "active" {
		t.Errorf("新登记的袋子应是 active(标签被占用), got %q", bg.Status)
	}

	// 次日:按标签整袋搬上台
	svc.SetClock(fmClock(fmDay2))
	if _, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		Lines: []service.CounterLoadLine{{Label: "BAG-001"}},
	}, "u-1"); err != nil {
		t.Fatalf("CounterLoad(按标签): %v", err)
	}

	// 冷库 −12、猪肉台 +12
	snap, err := svc.ListStock(ctx, "S001", model.StockScopeAll)
	if err != nil {
		t.Fatalf("ListStock: %v", err)
	}
	var cold, counter string
	for _, l := range snap.Lines {
		if l.CubeProductID != sku {
			continue
		}
		cold = l.ColdStoreKg.String()
		counter = l.CounterKg.String()
	}
	if cold != "0" {
		t.Errorf("整袋搬走后冷库应是 0(盘点时已从柜台搬进冷库), got %s", cold)
	}
	if counter != "12" {
		t.Errorf("整袋搬走后猪肉台应为 12kg, got %s", counter)
	}

	// 标签号**释放**了 ⇒ 状态 released
	all, err := svc.ListStockBags(ctx, "S001", "", "BAG-001", "")
	if err != nil {
		t.Fatalf("ListStockBags(label): %v", err)
	}
	if len(all) != 1 || all[0].Status != "released" {
		t.Errorf("搬上台后标签号应释放(status=released), got %+v", all)
	}
	if all[0].Location != string(model.StockLocationCounter) {
		t.Errorf("搬上台后袋子的位置应是 counter, got %q", all[0].Location)
	}
}

// TestStockBag_LabelReusableAfterRelease 锁"标签号搬上台后可复用"。
func TestStockBag_LabelReusableAfterRelease(t *testing.T) {
	svc, ctx, sku := fmBagSetup(t)

	// 10-01 建袋 BAG-001
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("10"), Label: "BAG-001"},
		},
	}, "u-1"); err != nil {
		t.Fatalf("RecordPorkCutsStocktake: %v", err)
	}
	// 10-02 搬上台 ⇒ 标签释放
	svc.SetClock(fmClock(fmDay2))
	if _, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		Lines: []service.CounterLoadLine{{Label: "BAG-001"}},
	}, "u-1"); err != nil {
		t.Fatalf("CounterLoad: %v", err)
	}
	// 10-03 再用同一个标签号贴新袋
	svc.SetClock(fmClock(fmDay1.Add(48 * time.Hour)))
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("6"), Label: "BAG-001"},
		},
	}, "u-1"); err != nil {
		t.Fatalf("标签号复用应成功(搬上台已释放): %v", err)
	}
	active, err := svc.ListStockBags(ctx, "S001", "", "BAG-001", "")
	if err != nil {
		t.Fatalf("ListStockBags: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("应有 2 条记录(旧袋 released + 新袋 active), got %d: %+v", len(active), active)
	}
}

// TestStockBag_SameLabelSameDayIsUpsert 锁"同一天重复提交同一标签 ⇒ 覆盖,不多出一袋"。
//
// ⇒ 多一袋 = 冷库余额虚高 ⇒ 盘点重提交会一次比一次高。
func TestStockBag_SameLabelSameDayIsUpsert(t *testing.T) {
	svc, ctx, sku := fmBagSetup(t)

	submit := func(kg string) {
		t.Helper()
		if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
			IsComplete: true,
			Cuts: []model.CutInput{
				{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d(kg), Label: "BAG-001"},
			},
		}, "u-1"); err != nil {
			t.Fatalf("RecordPorkCutsStocktake: %v", err)
		}
	}
	submit("10")
	submit("8")

	bags, err := svc.ListStockBags(ctx, "S001", "", "BAG-001", "")
	if err != nil {
		t.Fatalf("ListStockBags: %v", err)
	}
	if len(bags) != 1 {
		t.Fatalf("重复提交同一天的同一标签应只有 1 袋, got %d: %+v", len(bags), bags)
	}
	if !bags[0].QtyKg.Equal(d("8")) {
		t.Errorf("重量应被覆盖成 8kg, got %s", bags[0].QtyKg)
	}
}

// TestStockBag_LabelClashRejected 锁"在用的标签号不能同时贴两袋"。
func TestStockBag_LabelClashRejected(t *testing.T) {
	svc, ctx, sku := fmBagSetup(t)

	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("10"), Label: "BAG-001"},
		},
	}, "u-1"); err != nil {
		t.Fatalf("RecordPorkCutsStocktake: %v", err)
	}
	// 另一天再贴同一个标签号(还没搬上台 ⇒ 标签还被占用)
	//
	// ⚠️ 盘点**不会因此失败**(盘点是仓管已经完成的事实),但那一袋**不会被登记**
	// ⇒ 标签打错 ⇒ 这一袋退回"按 SKU 汇总"的老路(日志里有 Error)。
	svc.SetClock(fmClock(fmDay2))
	_, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("5"), Label: "BAG-001"},
		},
	}, "u-1")
	mustNoErr(t, err, "盘点本身不应因标签冲突失败")
	bags, err := svc.ListStockBags(ctx, "S001", "", "BAG-001", "")
	mustNoErr(t, err, "ListStockBags")
	active := 0
	for _, b := range bags {
		if b.Status == "active" { active++ }
	}
	if active != 1 {
		t.Fatalf("标签被占用时不应再登记第二袋, active 袋数应仍为 1, got %d: %+v", active, bags)
	}
}

// TestStockBag_ListByOriginDate 锁「按产生日过滤袋子」。
//
// ⚠️ 为什么需要:盘点单的快照是**按 SKU 聚合**的 ⇒ 一个 SKU 拆成多袋时,
// 快照里只有一个 label ⇒ **还原不了多袋结构** ⇒ 前端「载入并编辑」要靠
// `origin_date` 把当天的袋子全查出来才能还原。
func TestStockBag_ListByOriginDate(t *testing.T) {
	svc, ctx, sku := fmBagSetup(t)

	// 10-01：一袋
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("10"), Label: "BAG-001"},
		},
	}, "u-1"); err != nil {
		t.Fatalf("RecordPorkCutsStocktake: %v", err)
	}
	// 10-02：另一袋
	svc.SetClock(fmClock(fmDay2))
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("7"), Label: "BAG-002"},
		},
	}, "u-1"); err != nil {
		t.Fatalf("RecordPorkCutsStocktake(次日): %v", err)
	}

	day1, err := svc.ListStockBags(ctx, "S001", "", "", "2026-10-01")
	mustNoErr(t, err, "ListStockBags(origin_date=10-01)")
	if len(day1) != 1 || day1[0].Label != "BAG-001" {
		t.Fatalf("10-01 应只有 BAG-001, got %+v", day1)
	}
	day2, err := svc.ListStockBags(ctx, "S001", "", "", "2026-10-02")
	mustNoErr(t, err, "ListStockBags(origin_date=10-02)")
	if len(day2) != 1 || day2[0].Label != "BAG-002" {
		t.Fatalf("10-02 应只有 BAG-002, got %+v", day2)
	}
	// 非法日期 ⇒ 400
	if _, err := svc.ListStockBags(ctx, "S001", "", "", "2026/10/01"); err == nil {
		t.Fatalf("非法 origin_date 应报错")
	}
}