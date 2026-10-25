package service_test

// 2026-10-23:大宗台账「**表头 + 明细**」改造 + 「部位不可重复」的回归锁。
//
// # 为什么改
//
// 门店原话:「整个统收结算设计为表头,明细模式,表头有结算日期、对方、备注,
// 明细为大宗品项、结算数量,单价」⇒ 一次可以把板油和水油**一起**交给酒店。
//
// 「大宗品项,可以添加,删除,**从部位中选,不可重复**」⇒ 同一部位只能登记
// 一个大宗品项(否则"板油"与"猪油"都挂 fat ⇒ 结存与结算各算一半、说不清)。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

// setupBulkTwoCuts 起一个服务,登记两个大宗品项(fat / belly),
// 各自有部位映射,好让结算能落到**两个**部位上。
func setupBulkTwoCuts(t *testing.T) (*service.Service, context.Context, string) {
	t.Helper()
	svc, _ := setupTestService(t)
	ctx := context.Background()
	bizLoc := time.FixedZone("Asia/Shanghai", 8*3600)
	svc.SetClock(func() time.Time { return time.Date(2026, 10, 1, 14, 15, 0, 0, bizLoc) })

	for _, p := range []struct {
		code string
		name string
		cut  model.CutType
		sku  string
	}{
		{"fat", "板油", model.CutFat, "P-FAT"},
		{"belly", "猪油", model.CutBelly, "P-BELLY"},
	} {
		if _, err := svc.CreateBranchCutMapping(ctx, "S001",
			model.CreateBranchCutMappingInput{CutType: p.cut, CubeProductID: p.sku}); err != nil {
			t.Fatalf("CreateBranchCutMapping %s: %v", p.cut, err)
		}
		if _, err := svc.CreateBulkProduct(ctx, "S001", service.BulkProductInput{
			CutType: p.cut,
		}); err != nil {
			t.Fatalf("CreateBulkProduct %s: %v", p.code, err)
		}
	}
	return svc, ctx, "2026-10-01"
}

// TestBulkProduct_SameCutConflict 锁"同一部位不能登记两个大宗品项"。
func TestBulkProduct_SameCutConflict(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()

	if _, err := svc.CreateBulkProduct(ctx, "S001", service.BulkProductInput{
		CutType: model.CutFat,
	}); err != nil {
		t.Fatalf("CreateBulkProduct: %v", err)
	}
	// 同部位再登记 ⇒ 必须是冲突(2026-10-24:编码由服务端自动填 ⇒ 冲突只看部位)
	_, err := svc.CreateBulkProduct(ctx, "S001", service.BulkProductInput{
		CutType: model.CutFat,
	})
	if err == nil {
		t.Fatalf("同一部位再登记大宗品项应被拒(部位不可重复)")
	}
	if !errors.Is(err, service.ErrBulkProductConflict) {
		t.Errorf("应是冲突错误(ErrBulkProductConflict), got %v", err)
	}
}

// TestBulkSettlement_HeaderAndItems 锁"表头 + 明细"。
func TestBulkSettlement_HeaderAndItems(t *testing.T) {
	svc, ctx, date := setupBulkTwoCuts(t)

	// 两条产出:fat 5kg、belly 3kg(各自走盘点自动结转)
	for _, p := range []struct {
		code string
		sku  string
		kg   string
	}{
		{"fat", "P-FAT", "5"},
		{"belly", "P-BELLY", "3"},
	} {
		if _, err := svc.RecordBulkProduction(ctx, "S001", service.BulkProductionInput{
			ProductCode: p.code, QtyKg: d(p.kg),
		}, "u-1"); err != nil {
			t.Fatalf("RecordBulkProduction %s: %v", p.code, err)
		}
	}

	// 一张单、两条明细
	view, err := svc.CreateBulkSettlement(ctx, "S001", service.BulkSettlementInput{
		SettleDate:   date,
		Counterparty: "某酒店",
		Note:         "月底统收",
		Mode:         service.BulkModeDelivery,
		Items: []service.BulkSettlementItemInput{
			{ProductCode: "fat", QtyKg: d("5"), UnitPriceYuan: d("6")},
			{ProductCode: "belly", QtyKg: d("3"), UnitPriceYuan: d("4")},
		},
	}, "u-1")
	if err != nil {
		t.Fatalf("CreateBulkSettlement: %v", err)
	}

	// 表头
	if view.ID == "" {
		t.Error("结算单应有单号")
	}
	if view.Counterparty != "某酒店" || view.Note != "月底统收" {
		t.Errorf("表头字段没带上: %+v", view)
	}
	// 5×6 + 3×4 = 42
	if !view.TotalAmountYuan.Equal(d("42")) {
		t.Errorf("总金额应 = 5×6 + 3×4 = 42, got %s", view.TotalAmountYuan)
	}
	// 明细
	if len(view.Items) != 2 {
		t.Fatalf("应有 2 条明细, got %d", len(view.Items))
	}
	if !view.Items[0].AmountYuan.Equal(d("30")) {
		t.Errorf("明细1金额应 = 5×6 = 30, got %s", view.Items[0].AmountYuan)
	}
	if !view.Items[1].AmountYuan.Equal(d("12")) {
		t.Errorf("明细2金额应 = 3×4 = 12, got %s", view.Items[1].AmountYuan)
	}

	// 列表:表头 + 明细都要能取回
	list, err := svc.ListBulkSettlements(ctx, "S001")
	if err != nil {
		t.Fatalf("ListBulkSettlements: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("应有 1 张结算单, got %d", len(list))
	}
	if len(list[0].Items) != 2 {
		t.Errorf("列表里的结算单应带 2 条明细, got %d", len(list[0].Items))
	}

	// 两条明细分别结掉各自的结存
	bal, err := svc.ListBulkBalances(ctx, "S001")
	if err != nil {
		t.Fatalf("ListBulkBalances: %v", err)
	}
	for _, b := range bal {
		if !b.BalanceKg.IsZero() {
			t.Errorf("%s 结算后结存应为 0, got %s", b.ProductCode, b.BalanceKg)
		}
	}
}

// TestBulkSettlement_ItemUnderflowRollsBack 锁"一条明细产出不足 ⇒ 整单回滚"。
//
// ⚠️ 不能出现"半张单已出库":一条明细不足时,另一条明细也不能扣掉冷库。
func TestBulkSettlement_ItemUnderflowRollsBack(t *testing.T) {
	svc, ctx, date := setupBulkTwoCuts(t)

	// 只进 fat 5kg;belly 一条都不进 ⇒ 第二条明细必然不足
	if _, err := svc.RecordBulkProduction(ctx, "S001", service.BulkProductionInput{
		ProductCode: "fat", QtyKg: d("5"),
	}, "u-1"); err != nil {
		t.Fatalf("RecordBulkProduction: %v", err)
	}
	_, err := svc.CreateBulkSettlement(ctx, "S001", service.BulkSettlementInput{
		SettleDate:   date,
		Counterparty: "某酒店",
		Mode:         service.BulkModeDelivery,
		Items: []service.BulkSettlementItemInput{
			{ProductCode: "fat", QtyKg: d("5"), UnitPriceYuan: d("6")},
			{ProductCode: "belly", QtyKg: d("3"), UnitPriceYuan: d("4")},
		},
	}, "u-1")
	if err == nil {
		t.Fatalf("第二条明细产出不足时应报错")
	}
	// 整单回滚 ⇒ 板油那 5kg 的结存**还在**
	bal, err := svc.ListBulkBalances(ctx, "S001")
	if err != nil {
		t.Fatalf("ListBulkBalances: %v", err)
	}
	for _, b := range bal {
		if b.ProductCode == "fat" && b.BalanceKg.IsZero() {
			t.Errorf("整单回滚后板油结存应仍是 5kg, got %s", b.BalanceKg)
		}
	}
	// 也没有半张单
	list, err := svc.ListBulkSettlements(ctx, "S001")
	if err != nil {
		t.Fatalf("ListBulkSettlements: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("回滚后不应留下结算单, got %d", len(list))
	}
}
