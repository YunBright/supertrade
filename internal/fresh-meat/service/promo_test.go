package service_test

// 2026-10-23:特价档货号的回归锁。
//
// # 它守的是什么 bug
//
// `resolvePigByCubeProduct` 查不到部位映射时**直接丢整行**
// (`return "", "", nil` ⇒ caller `continue`)⇒ 特价销售的**金额和重量一起消失**。
//
// 实测影响(佳乐,近 14 个完整营业日):
//
//	成交金额 约 5%(3257 元 / 65311 元)
//	成交重量 约 10%(440 斤 / 约 4300 斤)
//
// ⇒ 重量比金额严重:kg 少了 ⇒ expected_remain 虚高 + 成本摊销比例偏小
// ⇒ 部位毛利与整猪毛利**同时失真**。

import (
	"context"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

// TestPromoSku_SaleIsNotDropped 锁"登记过的特价货号,销售行必须落库"。
func TestPromoSku_SaleIsNotDropped(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()
	bizLoc := time.FixedZone("Asia/Shanghai", 8*3600)
	day := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return time.Date(2026, 10, 1, 14, 15, 0, 0, bizLoc) })

	// 进 1 头猪(给"这笔销售属于哪头猪"一个落点)
	pig, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-1", GrossWeightKg: d("200"), PurchaseUnitPriceYuan: d("15"),
		SupplierID: "SUP-1", ArrivedAt: day,
	}, "u-1")
	mustNoErr(t, err, "RecordWholePig")

	// ⚠️ **不**给这个货号建部位映射 —— 它是特价档。
	const promoSKU = "00070"

	// 未登记 ⇒ 销售行被丢弃(旧行为:查不到映射就丢)。
	// 这是"为什么会丢"的复现,也是登记前后对比的基线。
	n, err := svc.OnSaleCompleted(ctx, service.SaleCompletedPayload{
		SaleID: "SO-1", BranchID: "S001", CompletedAt: day.Add(2 * time.Hour),
		Lines: []service.SaleLine{
			{LineID: "L-1", SKUID: promoSKU, FreshType: "meat",
				Qty: d("5"), AmountYuan: d("40")},
		},
	})
	mustNoErr(t, err, "OnSaleCompleted 未登记")
	if n != 0 {
		t.Errorf("未登记的货号应被丢弃(丢 0 行), got %d", n)
	}

	// 登记特价档 ⇒ 销售行必须落库,且 cut_type = promo
	_, err = svc.CreatePromoSku(ctx, "S001", service.PromoSkuInput{
		CubeProductID: promoSKU,
		ProductName:   "特价猪肉",
		PriceTierText: "特价猪肉8元",
	})
	mustNoErr(t, err, "CreatePromoSku")

	n, err = svc.OnSaleCompleted(ctx, service.SaleCompletedPayload{
		SaleID: "SO-2", BranchID: "S001", CompletedAt: day.Add(3 * time.Hour),
		Lines: []service.SaleLine{
			{LineID: "L-2", SKUID: promoSKU, FreshType: "meat",
				Qty: d("5"), AmountYuan: d("40")},
		},
	})
	mustNoErr(t, err, "OnSaleCompleted 已登记")
	if n != 1 {
		t.Fatalf("登记为特价档后,销售行必须落库(1 行), got %d —— 这就是「特价收入+重量一起丢」的 bug", n)
	}

	// 落库行的 cut_type 必须是 promo(不摊到具体部位)
	rep, err := svc.ComputeGrossMargin(ctx, "S001", day)
	mustNoErr(t, err, "ComputeGrossMargin")
	var promo *service.PerCutGrossMargin
	for i := range rep.PerCut {
		if rep.PerCut[i].CutType == model.CutPromo {
			promo = &rep.PerCut[i]
		}
	}
	if promo == nil {
		t.Fatalf("per_cut 里应有 promo 桶: %+v", rep.PerCut)
	}
	if !promo.SoldKg.Equal(d("5")) {
		t.Errorf("promo sold_kg = %s, want 5(重量不能再丢)", promo.SoldKg)
	}
	if !promo.RevenueYuan.Equal(d("40")) {
		t.Errorf("promo revenue = %s, want 40(金额不能再丢)", promo.RevenueYuan)
	}
	_ = pig
}

// TestPromoSku_Idempotent 锁"重复登记是幂等更新,不是报错"。
//
// "补一条货号说明"是常见日常操作(门店新开一个特价档)。
func TestPromoSku_Idempotent(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		_, err := svc.CreatePromoSku(ctx, "S001", service.PromoSkuInput{
			CubeProductID: "00070",
			ProductName:   "特价猪肉",
		})
		mustNoErr(t, err, "CreatePromoSku 重复登记")
	}
	rows, err := svc.ListPromoSkus(ctx, "S001")
	mustNoErr(t, err, "ListPromoSkus")
	if len(rows) != 1 {
		t.Errorf("重复登记应幂等(1 行), got %d", len(rows))
	}
}
