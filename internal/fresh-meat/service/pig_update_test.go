package service_test

// internal/fresh-meat/service/pig_update_test.go —— **改已录入的猪**的回归锁。
//
// # owner 原话（2026-11-03）
//
// 「录入猪，需要可以修改已录入的猪，包括重量和价格，需要新增一样触发其它逻辑。」
//
// # 三条不变量
//
//	① 改**毛重 / 采购单价** ⇒ 猪的**总价**(= 毛重 × 单价)跟着重算；
//	② 改完**必须触发成本链重算**（与录猪同一条路径）⇒ 当天的成本日快照跟着变；
//	③ 毛重 ≤ 0 / 单价 ≤ 0 / 两个都不传 ⇒ **400 并说清为什么**。
//
// ⚠️ 判别力：把 UpdateWholePig 里的 `recomputeCostSnapshots` 调用去掉
//   ⇒ ② 的快照断言立刻红（快照停在改之前的值）。

import (
	"errors"
	"testing"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/shopspring/decimal"
)

// fmPtr 取 decimal 的指针（可选字段用指针 ⇒ 能表达"没传这个字段"）。
func fmPtr(d decimal.Decimal) *decimal.Decimal { return &d }

// TestUpdateWholePig_ChangesWeightAndRecomputesChain 锁 ①②。
func TestUpdateWholePig_ChangesWeightAndRecomputesChain(t *testing.T) {
	svc, ctx, sku := fmBizSetup(t)
	// ⚠️ 注入**同步**物化执行器(生产是异步)⇒ 断言不用等后台任务。
	svc.SetChainMaterializer(func(f func()) { f() })

	pigID := fmSeedPigAt(t, svc, "EB-UPD", "180", fmDay1)
	fmSeedPigCut(t, svc, pigID, sku, "50")
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("0")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("10-01 盘点: %v", err)
	}

	before, ok := fmCutCostDaily(t, "S001", "2026-10-01", model.CutBelly)
	if !ok {
		t.Fatalf("成本日快照里没有五花肉这一行")
	}

	// 改毛重 180 → 200（单价不动）。
	out, err := svc.UpdateWholePig(ctx, "S001", pigID, model.UpdateWholePigInput{
		GrossWeightKg: fmPtr(d("200")),
	}, "u-1")
	mustNoErr(t, err, "UpdateWholePig")
	if !out.GrossWeightKg.Equal(d("200")) {
		t.Errorf("改后毛重应是 200kg, got %s", out.GrossWeightKg)
	}
	if !out.PurchaseUnitPriceYuan.Equal(d("20")) {
		t.Errorf("只改毛重时单价不该动, got %s", out.PurchaseUnitPriceYuan)
	}
	// 总价 = 毛重 × 单价 ⇒ 180×20=3600 → 200×20=4000。
	if !out.PurchaseCostYuan.Equal(d("4000")) {
		t.Errorf("★ 总价应重算成 200×20=4000, got %s", out.PurchaseCostYuan)
	}

	// ② 成本链重算 ⇒ 当天的成本日快照**跟着变**。
	after, ok := fmCutCostDaily(t, "S001", "2026-10-01", model.CutBelly)
	if !ok {
		t.Fatalf("成本日快照里没有五花肉这一行")
	}
	if after.ProducedAmount.Equal(before.ProducedAmount) {
		t.Errorf("★ 改毛重后成本链必须重算(本期入库金额应从 %s 变掉), 但没变",
			before.ProducedAmount)
	}
}

// TestUpdateWholePig_ChangesPriceOnly 锁 ①（只改单价 ⇒ 毛重不动、总价重算）。
func TestUpdateWholePig_ChangesPriceOnly(t *testing.T) {
	svc, ctx, _ := fmBizSetup(t)
	pigID := fmSeedPigAt(t, svc, "EB-PRC", "180", fmDay1)

	out, err := svc.UpdateWholePig(ctx, "S001", pigID, model.UpdateWholePigInput{
		PurchaseUnitPriceYuan: fmPtr(d("22")),
	}, "u-1")
	mustNoErr(t, err, "UpdateWholePig")
	if !out.GrossWeightKg.Equal(d("180")) {
		t.Errorf("只改单价时毛重不该动, got %s", out.GrossWeightKg)
	}
	if !out.PurchaseUnitPriceYuan.Equal(d("22")) {
		t.Errorf("改后单价应是 22, got %s", out.PurchaseUnitPriceYuan)
	}
	if !out.PurchaseCostYuan.Equal(d("3960")) {
		t.Errorf("★ 总价应重算成 180×22=3960, got %s", out.PurchaseCostYuan)
	}
}

// TestUpdateWholePig_RejectsBadInput 锁 ③。
func TestUpdateWholePig_RejectsBadInput(t *testing.T) {
	svc, ctx, _ := fmBizSetup(t)
	pigID := fmSeedPigAt(t, svc, "EB-BAD", "180", fmDay1)

	cases := []struct {
		name string
		in   model.UpdateWholePigInput
	}{
		{"毛重 0", model.UpdateWholePigInput{GrossWeightKg: fmPtr(d("0"))}},
		{"毛重负数", model.UpdateWholePigInput{GrossWeightKg: fmPtr(d("-1"))}},
		{"单价 0", model.UpdateWholePigInput{PurchaseUnitPriceYuan: fmPtr(d("0"))}},
		{"两个都不传", model.UpdateWholePigInput{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.UpdateWholePig(ctx, "S001", pigID, tc.in, "u-1")
			if err == nil {
				t.Fatalf("★ %s 必须被拒(零成本/负重量 ⇒ 成本链算错)", tc.name)
			}
			if !errors.Is(err, service.ErrInvalidInput) {
				t.Errorf("应是输入校验错(400), got %v", err)
			}
		})
	}
	// 猪没被改坏。
	got, err := svc.GetWholePig(ctx, "S001", pigID)
	mustNoErr(t, err, "GetWholePig")
	if !got.GrossWeightKg.Equal(d("180")) || !got.PurchaseCostYuan.Equal(d("3600")) {
		t.Errorf("★ 被拒的修改不该落库, got 毛重=%s 总价=%s", got.GrossWeightKg, got.PurchaseCostYuan)
	}
}

// TestUpdateWholePig_NotFound 锁"改不存在的猪 ⇒ 404"。
func TestUpdateWholePig_NotFound(t *testing.T) {
	svc, ctx, _ := fmBizSetup(t)
	_, err := svc.UpdateWholePig(ctx, "S001", "NO-SUCH-PIG", model.UpdateWholePigInput{
		GrossWeightKg: fmPtr(d("200")),
	}, "u-1")
	if err == nil {
		t.Fatalf("改不存在的猪应报错")
	}
	if !errors.Is(err, service.ErrWholePigNotFound) {
		t.Errorf("应是 not_found, got %v", err)
	}
}
