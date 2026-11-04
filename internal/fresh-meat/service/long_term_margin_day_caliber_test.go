package service_test

// long_term_margin_day_caliber_test.go —— 长期毛利页的**每日行**切「当日口径」的判别力测试。
//
// # 背景(owner 2026-11-04 拍板:「长期毛利页的也切」)
//
//	长期毛利页的每日行原来用的是「**猪肉台口径**」
//	(营收 − 当日采购 + 期末台面剩余×采购均价)
//	⇒ 与「每日毛利看板」的**表头**只差「**搬出估算成本**」一项
//	  (生产实测 店 00:10-29 差 1.58 元、11-01 差 114.84 元)
//	⇒ owner 拍板:每日行**也切当日口径** ⇒ 两页的每日数**必须是同一个数**。
//
// # 锁的是**口径**(不是实现细节)
//
//	① 长期毛利页每日行 **==** 每日毛利看板的**表头**毛利(逐位相等)
//	② 每日行 **== 旧口径 − 搬出估算成本**
//	   (搬出估算成本 = Σ(搬上台 kg × 该部位**冷库移动加权成本**))
//	③ 当天**没营收** ⇒ 毛利**算不出来**(不是 0)
//	④ 三个守卫与看板完全一致(没进货 / 没完整盘点 / 没营收 ⇒ 都不给数)
//
// ⚠️ **用例 ② 是判别力锚点**:搬出台 = 0 时新旧口径**恰好相等**
//   ⇒ 用例会假绿 ⇒ 所以用例必须断言「搬出估算成本 > 0」(前置条件)。
//
// ⚠️ 端到端(走公开 API + 真实 SQLite)⇒ 引擎接线错了也会红。

import (
	"context"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/shopspring/decimal"
)

// dcmDay 是本文件用的营业日(2026-10-29 14:00 +08)。
var dcmDay = time.Date(2026, 10, 29, 6, 0, 0, 0, time.UTC)

// dcmSetup 铺一个**完整的营业日**(2026-10-29):
//
//	进猪 20kg @ 20 元/kg(= 400 元)
//	→ 分割 belly 20kg(统计基线 belly/0-49 p50 = 20 ⇒ 建议分割 == 实录)
//	→ 搬上台 4kg(冷库 → 猪肉台)
//	→ 卖 15kg @ 34 元(= 510 元)
//	→ 日终盘点:belly 柜台实盘 4kg(完整)
//
// # 为什么数字这么选
//
//	① **搬上台 4kg > 0** —— 本轮口径的差别**就在这一项**
//	   ⇒ 搬上台 = 0 时新旧口径恰好相等 ⇒ 用例没有判别力。
//	② 期末柜台实盘 4kg ⇒ 盘点估算成本 = 4 × 采购均价(= 20)= 80 元。
//	③ 采购均价 = 400 ÷ 20 = 20 元/kg ⇒ **冷库移动加权成本**也是 20
//	   (期初 0 + 入库 20kg/400元 ⇒ (0+400) ÷ (0+20) = 20)
//	   ⇒ 搬出估算成本 = 4 × 20 = 80 元。
//	④ 手算(不看实现):
//	   旧口径(猪肉台口径)= 营收 510 − 采购 400 + 盘点估算 80 = **190 元**
//	   当日口径          = 营收 510 − (采购 400 + 搬出估算 80 − 盘点估算 80) = **110 元**
//	   ⇒ 每日行 = 110 元(= 旧口径 190 元 − 搬出估算成本 80 元)。
//
// # 归属日 = 2026-10-29(唯一营业日)
//
//	⚠️ 首日豁免:`hasBizDayHistory` 只看 **target 之前**的痕迹
//	  ⇒ 10-29 之前没有任何猪/盘点/搬台 ⇒ 10-29 可以录猪、可以搬台。
func dcmSetup(t *testing.T, sellQty, sellAmount string) (*service.Service, context.Context, string) {
	t.Helper()
	svc, _ := setupTestService(t)
	ctx := context.Background()
	svc.SetClock(fmClock(dcmDay))
	if _, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-BELLY"}); err != nil {
		t.Fatalf("CreateBranchCutMapping: %v", err)
	}
	// 统计基线:20kg 的猪落在体重段 "0-49" ⇒ belly 建议分割 20kg。
	splitBaseline(t, model.CutBelly, "0-49", "20")
	pig, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-1", GrossWeightKg: d("20"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: dcmDay,
	}, "u-1")
	mustNoErr(t, err, "RecordWholePig")
	_, err = svc.RecordPigCut(ctx, "S001", model.RecordPigCutInput{
		PigID: pig.ID, CutType: model.CutBelly, Barcode: "B-1",
		WeightKg: d("20"), ExpiresAt: dcmDay.Add(48 * time.Hour), CubeProductID: "P-BELLY",
	}, "u-1")
	mustNoErr(t, err, "RecordPigCut")
	// 搬上台 4kg(冷库 → 猪肉台)。
	if _, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		TakenDate: "2026-10-29",
		Lines:     []service.CounterLoadLine{{CubeProductID: "P-BELLY", QtyKg: d("4")}},
	}, "u-1"); err != nil {
		t.Fatalf("CounterLoad: %v", err)
	}
	// 卖(sellQty 为空 ⇒ 这一天**没有营收** ⇒ 触发守卫③)。
	if sellQty != "" {
		if _, err := svc.OnSaleCompleted(ctx, service.SaleCompletedPayload{
			SaleID: "SO-1", BranchID: "S001", CompletedAt: dcmDay.Add(time.Hour),
			Lines: []service.SaleLine{{
				LineID: "L-1", SKUID: "P-BELLY", FreshType: "meat",
				Qty: d(sellQty), AmountYuan: d(sellAmount), PigID: pig.ID, CutType: model.CutBelly,
			}},
		}); err != nil {
			t.Fatalf("OnSaleCompleted: %v", err)
		}
	}
	// 日终盘点(完整)⇒ 期末柜台实盘 4kg。
	_, err = svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		TakenDate:  "2026-10-29",
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: "P-BELLY", ActualRemainKg: d("4")},
		},
	}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake")
	return svc, ctx, "P-BELLY"
}

// dcmBoard 取当天的**每日毛利看板**。
func dcmBoard(t *testing.T, svc *service.Service, ctx context.Context) *service.GrossMarginReport {
	t.Helper()
	rep, err := svc.ComputeGrossMargin(ctx, "S001", dcmDay)
	mustNoErr(t, err, "ComputeGrossMargin")
	return rep
}

// dcmLongTerm 取当天的**长期毛利**每日行。
func dcmLongTerm(t *testing.T, svc *service.Service, ctx context.Context) *service.LongTermMarginDay {
	t.Helper()
	rep, err := svc.ComputeLongTermMargin(ctx, "S001", "2026-10-29", "2026-10-29")
	mustNoErr(t, err, "ComputeLongTermMargin")
	if len(rep.Rows) != 1 {
		t.Fatalf("长期毛利应只有 1 行(2026-10-29),got %d", len(rep.Rows))
	}
	return &rep.Rows[0]
}

// dcmLoadCost 看板口径下的**搬出估算成本**
// = Σ(搬上台 kg × 该部位**冷库移动加权成本**)。
//
// ⚠️ `ColdStoreUnitCostYuan` 就是该部位的移动加权单价
//
//	(owner:「冷库单价不是新的部位单价,是移动加权单价」)
//	⇒ 外部也能算出这一项 ⇒ 用例不必依赖内部实现。
func dcmLoadCost(t *testing.T, rep *service.GrossMarginReport) decimal.Decimal {
	t.Helper()
	total := decimal.Zero
	for _, row := range rep.PerCut {
		if row.CounterLoadKg.IsZero() {
			continue
		}
		total = total.Add(row.CounterLoadKg.Mul(row.ColdStoreUnitCostYuan))
	}
	return total
}

// ---- ① 两页的每日数必须是同一个数 ----

// TestLongTermMargin_DailyRowEqualsBoardHeader 锁①:长期毛利页的每日行
// **== 每日毛利看板的表头毛利**(逐位相等)。
//
// ⚠️ 为什么必须锁:owner 2026-11-04「长期毛利页的也切」的**全部目的**
//
//	就是让门店在两页之间对得上 ⇒ 一旦两页漂移 ⇒ 这个改动就白做了。
//
// ⚠️ 本用例**单独**不足以判别(两页都用同一个公式 ⇒ 把公式改错两页一起错,
//
//	用例照样绿)⇒ 真正的判别力在下面的用例 ②。
func TestLongTermMargin_DailyRowEqualsBoardHeader(t *testing.T) {
	svc, ctx, _ := dcmSetup(t, "15", "510")

	board := dcmBoard(t, svc, ctx)
	if board.Total.ReferenceGrossMarginYuan == nil {
		t.Fatalf("★ 前置:看板表头毛利必须有值(有进货 + 有完整盘点 + 有营收)")
	}
	day := dcmLongTerm(t, svc, ctx)

	if !day.GrossMarginYuan.Equal(*board.Total.ReferenceGrossMarginYuan) {
		t.Errorf("★ 长期毛利每日行(%s)应 == 看板表头毛利(%s)",
			day.GrossMarginYuan, *board.Total.ReferenceGrossMarginYuan)
	}
	// 毛利率也要同一个数(前端两页各显示一个百分比)。
	diff := *board.Total.ReferenceGrossMarginPct - day.GrossMarginPct
	if diff < 0 {
		diff = -diff
	}
	if diff > 0.0001 {
		t.Errorf("★ 长期毛利每日毛利率(%v)应 == 看板表头毛利率(%v)",
			day.GrossMarginPct, *board.Total.ReferenceGrossMarginPct)
	}
}

// ---- ② 判别力锚点:每日行 == 旧口径 − 搬出估算成本 ----

// TestLongTermMargin_DailyRowSubtractsLoadCost 锁②:每日行
// **== 旧口径(猪肉台口径) − 搬出估算成本**。
//
// # 手算(不看实现)
//
//	营收      = 15kg × 34 元          = 510 元
//	采购      = 20kg × 20 元          = 400 元
//	期末台面  = 4kg                   ⇒ 盘点估算成本 = 4 × 20 = 80 元
//	搬出估算  = 搬上台 4kg × 20 元     = 80 元
//
//	旧口径(猪肉台口径)= 510 − 400 + 80 = **190 元**
//	当日口径          = 510 − (400 + 80 − 80) = **110 元**
//	⇒ 每日行 = 110 元(= 旧口径 190 元 − 搬出估算成本 80 元)
//
// ⚠️ **这个用例才是判别力锚点**:把「搬出估算成本」这一项去掉
//
//	(退回旧口径)⇒ 每日行会变成 190 元 ≠ 110 元 ⇒ **必红**。
//
// ⚠️ 前置断言「搬出估算成本 > 0」:如果它是 0 ⇒ 新旧口径相等
//
//	⇒ 用例会假绿(改坏实现也绿)⇒ 必须先把前置钉死。
func TestLongTermMargin_DailyRowSubtractsLoadCost(t *testing.T) {
	svc, ctx, _ := dcmSetup(t, "15", "510")

	board := dcmBoard(t, svc, ctx)
	day := dcmLongTerm(t, svc, ctx)

	loadCost := dcmLoadCost(t, board)
	if !loadCost.IsPositive() {
		t.Fatalf("★ 前置:搬出估算成本必须 > 0(否则新旧口径相等,本用例没有判别力),got %s", loadCost)
	}
	// 旧口径(猪肉台口径)= 营收 − 当日采购(含外采) + 期末台面剩余估值。
	oldCaliber := board.Total.RevenueYuan.
		Sub(board.Total.PurchasedYuan.Add(board.Total.ExternalPurchaseYuan)).
		Add(board.Total.ReferenceRemainYuan)
	want := oldCaliber.Sub(loadCost)

	if !day.GrossMarginYuan.Equal(want) {
		t.Errorf("★ 长期毛利每日行应 = 旧口径(%s) − 搬出估算成本(%s) = %s,got %s",
			oldCaliber, loadCost, want, day.GrossMarginYuan)
	}
	// 手算复核:营收 510、采购 400、盘点估算 80、搬出估算 80 ⇒ 110。
	if !day.GrossMarginYuan.Equal(d("110")) {
		t.Errorf("★ 长期毛利每日行应 = 510 − (400 + 80 − 80) = 110,got %s", day.GrossMarginYuan)
	}
	// 看板表头与长期毛利页是同一个数(用例 ① 的最小复述)。
	if board.Total.ReferenceGrossMarginYuan == nil ||
		!day.GrossMarginYuan.Equal(*board.Total.ReferenceGrossMarginYuan) {
		t.Errorf("★ 看板表头(%v)与长期毛利每日行(%s)必须相等",
			board.Total.ReferenceGrossMarginYuan, day.GrossMarginYuan)
	}
}

// ---- ③ 守卫:当天没营收 ⇒ 算不出来(不是 0) ----

// TestLongTermMargin_NoRevenueMeansNoMargin 锁③:当天**没营收** ⇒ 毛利**算不出来**。
//
// ⚠️ 为什么锁:守卫③与看板**完全一致**(当天没营收 ⇒ 毛利率没有分母)
//
//	⇒ 长期毛利页也必须显示「—」,**不是 0**
//	(0 会被门店误读成"当天不赚钱")。
//
// ⚠️ 判别力:旧口径在这里会给 **−320 元**(0 − 400 + 80)
//
//	⇒ 若守卫③被去掉,每日行会变成 −320 ≠ 0 ⇒ **必红**。
func TestLongTermMargin_NoRevenueMeansNoMargin(t *testing.T) {
	svc, ctx, _ := dcmSetup(t, "", "")

	board := dcmBoard(t, svc, ctx)
	if !board.Total.RevenueYuan.IsZero() {
		t.Fatalf("★ 前置:这天不该有营收,got %s", board.Total.RevenueYuan)
	}
	if board.Total.ReferenceGrossMarginYuan != nil {
		t.Fatalf("★ 前置:看板表头在没营收时必须**不给数**(nil),got %v", *board.Total.ReferenceGrossMarginYuan)
	}
	day := dcmLongTerm(t, svc, ctx)

	if !day.GrossMarginYuan.IsZero() {
		t.Errorf("★ 没营收 ⇒ 每日毛利必须**算不出来**(0 + 前端显示「—」),got %s", day.GrossMarginYuan)
	}
	// 前端靠 revenue_yuan 判断「当天有没有营收」⇒ 这里把它钉住。
	if !day.RevenueYuan.IsZero() {
		t.Errorf("★ 前置:每日行的营收必须是 0,got %s", day.RevenueYuan)
	}
}

// ---- ④ 守卫:没有完整日终盘点 ⇒ 算不出来 ----

// TestLongTermMargin_NoStocktakeMeansNoMargin 锁④:没有**完整**日终盘点
// ⇒ 期末台面实盘拿不到 ⇒ 毛利**算不出来**(不是 0)。
//
// ⚠️ 与看板同源:看板表头在同样情形下给 nil。
func TestLongTermMargin_NoStocktakeMeansNoMargin(t *testing.T) {
	svc, ctx, _ := dcmSetup(t, "15", "510")

	// 把当天的盘点单改成**不完整** ⇒ 期末实盘不认。
	if err := testDBSingleton.Model(&model.PorkCutsStocktake{}).
		Where("branch_id = ?", "S001").
		Updates(map[string]interface{}{"is_complete": false}).Error; err != nil {
		t.Fatalf("改盘点单为不完整: %v", err)
	}

	board := dcmBoard(t, svc, ctx)
	if board.Total.ReferenceGrossMarginYuan != nil {
		t.Fatalf("★ 前置:看板表头在没完整盘点时必须不给数,got %v", *board.Total.ReferenceGrossMarginYuan)
	}
	day := dcmLongTerm(t, svc, ctx)

	if day.HasStocktake {
		t.Fatalf("★ 前置:这天的 has_stocktake 必须是 false")
	}
	if !day.GrossMarginYuan.IsZero() {
		t.Errorf("★ 没完整盘点 ⇒ 每日毛利必须**算不出来**(0 + 前端显示「—」),got %s", day.GrossMarginYuan)
	}
}
