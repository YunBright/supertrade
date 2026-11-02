package service_test

// 2026-10-24:**按日结转记录**(袋子台账)的回归锁。
//
// ⚠️ 2026-11-02 owner:标签号玩法整套下线 ⇒ 这里不再有"袋子 / 标签号"的概念,
// 盘点只按 SKU 登记**一行结转记录**(归属日 + SKU + 重量 + 猪归属快照)。
//
// ⇒ 剩下的不变量:
//  ① 盘点每一行 ⇒ 登记一行结转记录(进冷库、FIFO 队列的一员);
//  ② 同一天重复提交同一份盘点 = **覆盖**同一行(不多出一行 ⇒ 冷库余额不虚高);
//  ③ 同一 SKU 盘多行(老 APK 拆袋)⇒ **合并求和**(与位置表同口径 ⇒ 不丢数据);
//  ④ 能按产生日过滤(结转台账 / FIFO 排队都按产生日)。

import (
	"context"
	"testing"

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

// TestStockBag_RegisterOnStocktake 锁 ①:盘点 ⇒ 登记一行结转记录。
func TestStockBag_RegisterOnStocktake(t *testing.T) {
	svc, ctx, sku := fmBagSetup(t)

	// 盘点:五花肉剩 12kg
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("12")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("RecordPorkCutsStocktake: %v", err)
	}

	bags, err := svc.ListStockBags(ctx, "S001", "cold_store", "")
	if err != nil {
		t.Fatalf("ListStockBags: %v", err)
	}
	if len(bags) != 1 {
		t.Fatalf("应登记 1 行结转记录, got %d: %+v", len(bags), bags)
	}
	bg := bags[0]
	if bg.CubeProductID != sku {
		t.Errorf("结转记录字段不对: %+v", bg)
	}
	if !bg.QtyKg.Equal(d("12")) {
		t.Errorf("结转重量应为 12kg, got %s", bg.QtyKg)
	}
	if bg.Status != "active" {
		t.Errorf("新登记的结转记录应是 active, got %q", bg.Status)
	}
	if bg.Source != "stocktake" {
		t.Errorf("来源应是 stocktake, got %q", bg.Source)
	}
}

// TestStockBag_SameDayResubmitIsUpsert 锁 ②:同一天重复提交同一份盘点 = 覆盖,不多出一行。
//
// ⇒ 多一行 = 冷库余额虚高 ⇒ 盘点重提交会一次比一次高。
func TestStockBag_SameDayResubmitIsUpsert(t *testing.T) {
	svc, ctx, sku := fmBagSetup(t)

	submit := func(kg string) {
		t.Helper()
		if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
			IsComplete: true,
			Cuts: []model.CutInput{
				{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d(kg)},
			},
		}, "u-1"); err != nil {
			t.Fatalf("RecordPorkCutsStocktake: %v", err)
		}
	}
	submit("10")
	submit("8")

	bags, err := svc.ListStockBags(ctx, "S001", "", "")
	if err != nil {
		t.Fatalf("ListStockBags: %v", err)
	}
	if len(bags) != 1 {
		t.Fatalf("重复提交同一天的盘点应只有 1 行, got %d: %+v", len(bags), bags)
	}
	if !bags[0].QtyKg.Equal(d("8")) {
		t.Errorf("重量应被覆盖成 8kg, got %s", bags[0].QtyKg)
	}
}

// TestStockBag_SameSkuRowsMerged 锁 ③:同一 SKU 盘多行 ⇒ **合并求和**,不丢数据。
//
// ⚠️ 2026-11-02 owner:拆袋(同 SKU 分多袋)在前端随标签号一起下线,
// 但**老 APK** 仍可能拆着提交 ⇒ 后端必须合并,而不是静默覆盖。
// (静默覆盖 ⇒ "位置表 20kg / 结转记录 5kg" ⇒ FIFO 归摊用错重量。)
func TestStockBag_SameSkuRowsMerged(t *testing.T) {
	svc, ctx, sku := fmBagSetup(t)

	_, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("10")},
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("5")},
		},
	}, "u-1")
	mustNoErr(t, err, "同 SKU 多行应被合并(老 APK 拆袋提交不丢数据)")

	bags, err := svc.ListStockBags(ctx, "S001", "", "")
	mustNoErr(t, err, "ListStockBags")
	if len(bags) != 1 {
		t.Fatalf("同 SKU 多行应合并成 1 行结转记录, got %d: %+v", len(bags), bags)
	}
	if !bags[0].QtyKg.Equal(d("15")) {
		t.Errorf("合并后的结转重量应是 10+5=15kg, got %s", bags[0].QtyKg)
	}
}

// TestStockBag_ListByOriginDate 锁 ④「按产生日过滤结转台账」。
//
// ⚠️ 为什么需要:结转台账按产生日排队(FIFO 归摊 / 大宗领用都按产生日)。
func TestStockBag_ListByOriginDate(t *testing.T) {
	svc, ctx, sku := fmBagSetup(t)

	// 10-01：一行 10kg
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("10")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("RecordPorkCutsStocktake: %v", err)
	}
	// 10-02：另一行 7kg
	svc.SetClock(fmClock(fmDay2))
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("7")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("RecordPorkCutsStocktake(次日): %v", err)
	}

	day1, err := svc.ListStockBags(ctx, "S001", "", "2026-10-01")
	mustNoErr(t, err, "ListStockBags(origin_date=10-01)")
	if len(day1) != 1 || !day1[0].QtyKg.Equal(d("10")) {
		t.Fatalf("10-01 应只有一行 10kg, got %+v", day1)
	}
	day2, err := svc.ListStockBags(ctx, "S001", "", "2026-10-02")
	mustNoErr(t, err, "ListStockBags(origin_date=10-02)")
	if len(day2) != 1 || !day2[0].QtyKg.Equal(d("7")) {
		t.Fatalf("10-02 应只有一行 7kg, got %+v", day2)
	}
	// 非法日期 ⇒ 400
	if _, err := svc.ListStockBags(ctx, "S001", "", "2026/10/01"); err == nil {
		t.Fatalf("非法 origin_date 应报错")
	}
}
