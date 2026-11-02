package service_test

// 2026-10-25:**大宗线的 POS 货号不按零售算**（门店口径）。
//
// # 为什么
//
// `03808 水油`是**每 9~11 天整一笔 400~870kg**的 POS 行（佳乐）。
// 它是「攒着卖给酒店」的货 ⇒ 口径上属于**大宗**，不是零售：
//
//	按零售收 ⇒ 收入落到「当日某一头猪」⇒ **那头猪毛利虚高**（几百公斤的金额）
//
// ⇒ 2026-10-25 拍板：**这条线一律走手工统收结算**
//
//	POS 卖 03808 ⇒ **不进** line_sales_by_pigs（不按零售算、不吃 FIFO）
//	⇒ 改由门店在「统收结算」里手工登记（领用冷库重量 + 记收入）
//
// # 安全网
//
// 那批货还挂在冷库里 ⇒ 结存页的「**可领用**」一直显示它
// ⇒ 门店看结存就知道「还有 N kg 没结」。
//
// # 互斥
//
// 同一部位的**零售线货号**（`00032 猪板油`，每天 2~25kg）**照常按零售收**
// ⇒ 两条线互不干扰。

import (
	"context"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

func TestBulkLinePosSaleIsNotRetail(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()
	bizLoc := time.FixedZone("Asia/Shanghai", 8*3600)
	svc.SetClock(func() time.Time { return time.Date(2026, 10, 1, 14, 15, 0, 0, bizLoc) })

	const retailSKU = "P-FAT"  // 零售线货号
	const bulkSKU = "03808"    // 大宗线货号（POS 上卖的那笔）
	for _, sku := range []string{retailSKU, bulkSKU} {
		if _, err := svc.CreateBranchCutMapping(ctx, "S001",
			model.CreateBranchCutMappingInput{CutType: model.CutFat, CubeProductID: sku}); err != nil {
			t.Fatalf("CreateBranchCutMapping(%s): %v", sku, err)
		}
	}
	if _, err := svc.CreateBulkProduct(ctx, "S001",
		service.BulkProductInput{CutType: model.CutFat}); err != nil {
		t.Fatalf("CreateBulkProduct: %v", err)
	}
	// ★ 装配「大宗线的 POS 货号」—— **门店不填**，由 seed/装配写入。
	if err := testDBSingleton.Model(&model.BulkProduct{}).
		Where("branch_id = ? AND cut_type = ?", "S001", model.CutFat).
		Update("pos_cube_product_id", bulkSKU).Error; err != nil {
		t.Fatalf("装配大宗线 POS 货号: %v", err)
	}

	// 零售线要有**能摊到的货**（猪 + 分割 + 日终盘点 ⇒ 一批），
	// 否则零售那一行会因"反查不到猪"被丢 ⇒ 就分不清是"被大宗线规则排除"还是"没猪"。
	pig, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "BL-1", GrossWeightKg: d("200"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: time.Date(2026, 10, 1, 6, 0, 0, 0, bizLoc),
	}, "u-1")
	mustNoErr(t, err, "RecordWholePig")
	if _, err := svc.RecordPigCut(ctx, "S001", model.RecordPigCutInput{
		PigID: pig.ID, CutType: model.CutFat, Barcode: "BL-BC-1",
		WeightKg: d("50"), ExpiresAt: time.Date(2026, 10, 3, 6, 0, 0, 0, bizLoc),
		CubeProductID: retailSKU,
	}, "u-1"); err != nil {
		t.Fatalf("RecordPigCut: %v", err)
	}
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutFat, CubeProductID: retailSKU, ActualRemainKg: d("50")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("RecordPorkCutsStocktake: %v", err)
	}

	// 一笔 POS 销售：两行 —— 大宗线 400kg + 零售线 5kg。
	inserted, err := svc.OnSaleCompleted(ctx, service.SaleCompletedPayload{		BranchID:    "S001",
		SaleID:      "SIM-BULKLINE",
		OrderStatus: "S",
		PaidAt:      time.Date(2026, 10, 1, 15, 0, 0, 0, bizLoc),
		Lines: []service.SaleLine{
			{LineID: "L-BULK", ProductID: bulkSKU, Qty: d("400"),
				UnitPriceYuan: d("1.8"), AmountYuan: d("720")},
			{LineID: "L-RETAIL", ProductID: retailSKU, Qty: d("5"),
				UnitPriceYuan: d("30"), AmountYuan: d("150")},
		},
	})
	mustNoErr(t, err, "OnSaleCompleted")
	if inserted != 1 {
		t.Errorf("只应收**零售线**那一行(大宗线不按零售算), got %d 行", inserted)
	}

	// 查落库的销售行：只能有零售线那个货号。
	var rows []model.LineSalesByPig
	if err := testDBSingleton.Where("branch_id = ? AND source = ?", "S001", "pos").
		Find(&rows).Error; err != nil {
		t.Fatalf("query line_sales_by_pigs: %v", err)
	}
	for _, r := range rows {
		if r.CubeProductID == bulkSKU {
			t.Errorf("★ 大宗线的 POS 货号 %s 不该按零售落销售行(要走手工统收结算): %+v", bulkSKU, r)
		}
	}
	if len(rows) != 1 || rows[0].CubeProductID != retailSKU {
		t.Errorf("应只落零售线那一行, got %+v", rows)
	}
}
