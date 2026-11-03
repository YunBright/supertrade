// 2026-11-03:锁「**午夜后提交的日终盘点**」也能搬台(生产事故)。
//
// # 事故(生产实测,店 00)
//
//	11-02 的日终盘点 **created_at = 11-03 00:27**(归属日 11-02)
//	⇒ 旧判据「归属日 == 日历日」把它判成「补录往日」
//	⇒ 过夜搬运(柜台 → 冷库)被跳过 ⇒ 位置表的冷库余额缺这批货
//	⇒ 搬台页只显示「冷库共 1 个 SKU / 2.40 kg」
//	⇒ 门店 11-03 要搬的 3 斤排骨(11-02 日终进冷库的 1.5kg)**搬不了**。
//
// # 两条修法(都要锁)
//
//	① 冷库余额改成**按记录层推导**(cold_store_derive.go)
//	   ⇒ 提交得再晚也算得对(补录/午夜提交都生效)⇒ 存量数据自动修好;
//	② 收盘窗口(isClosingStocktake)
//	   ⇒ 午夜后提交的那次**也**算「收盘盘点」⇒ 过夜搬运照跑、当前成本状态照刷。
//
// # 判别力(两条用例都要能抓住"去掉实现")
//
//	① 把 loadSkuPositions 的冷库侧改回读位置表 ⇒ 用例①红;
//	② 把 isClosing 改回 isToday ⇒ 用例②红。
package service_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/shopspring/decimal"
)

// fmColdSetup 起服务 + 两个部位的货号映射(排骨 / 后腿肉)。
func fmColdSetup(t *testing.T) (*service.Service, context.Context) {
	t.Helper()
	svc, _ := setupTestService(t)
	ctx := context.Background()
	for _, m := range []model.CreateBranchCutMappingInput{
		{CutType: model.CutRib, CubeProductID: "P-RIB"},
		{CutType: model.CutLegRear, CubeProductID: "P-LEG"},
	} {
		if _, err := svc.CreateBranchCutMapping(ctx, "S001", m); err != nil {
			t.Fatalf("CreateBranchCutMapping %s: %v", m.CutType, err)
		}
	}
	return svc, ctx
}

// fmColdLineOf 取库存快照里某个 SKU 的冷库余额(搬台页看到的就是它)。
func fmColdLineOf(t *testing.T, svc *service.Service, sku string) decimal.Decimal {
	t.Helper()
	snap, err := svc.ListStock(context.Background(), "S001", model.StockScopeAll)
	mustNoErr(t, err, "ListStock")
	for _, l := range snap.Lines {
		if l.CubeProductID == sku {
			return l.ColdStoreKg
		}
	}
	return decimal.Zero
}

// TestColdStore_MidnightStocktake_CarryoverIsVisible 锁 ①:午夜后提交的收盘盘点
// ⇒ 那批货**必须**在冷库里(门店才搬得动)。
//
// # 场景(复刻生产:店 00 的 11-02 / 11-03)
//
//	门店 11-03 00:27 录 11-02 的日终盘点:柜台实盘 排骨 1.5kg(= 3 斤)
//	⇒ 11-03 早上要搬这 1.5kg 上柜台卖。
//
// # 期望
//
//	`GET /stock?scope=all` 里 P-RIB 的冷库余额 = 1.5kg。
//
// # 判别力
//
//	把 loadSkuPositions 的冷库侧改回读位置表 ⇒ 过夜搬运没跑 ⇒ 冷库 0 ⇒ 本用例红。
func TestColdStore_MidnightStocktake_CarryoverIsVisible(t *testing.T) {
	svc, ctx := fmColdSetup(t)
	// 时钟 = 2026-11-03 00:27(+08)= 门店在午夜后录**昨天**的日终盘点。
	svc.SetClock(fmClock(time.Date(2026, 11, 2, 16, 27, 0, 0, time.UTC)))

	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		TakenDate:  "2026-11-02",
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutRib, CubeProductID: "P-RIB", ActualRemainKg: d("1.5")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("RecordPorkCutsStocktake: %v", err)
	}

	if got := fmColdLineOf(t, svc, "P-RIB"); !got.Equal(d("1.5")) {
		t.Errorf("★ 11-02 日终进冷库的 1.5kg 必须在冷库里(门店要搬它上柜台),got %s", got)
	}
}

// TestColdStore_LateBackfill_CarryoverIsVisible 锁 ①的**判别力**版本:
// **补录往日**(提交时刻离归属日超过收盘窗口)⇒ 那批货**照样**在冷库里。
//
// # 为什么这条才抓得住"推导"
//
//	午夜后提交(上面那条)会走**过夜搬运**⇒ 位置表也被写对了
//	⇒ 只把冷库来源改回位置表,上面那条**照样绿**。
//
//	真正抓得住的是**过夜搬运没跑**的场景:
//   - 隔天才补录(窗口外)⇒ 不做过夜搬运;
//   - 生产上已经发生过的历史漏记(见下面那条)。
//
// # 场景
//
//	11-02 的日终盘点,门店到 **11-05 上午**才补录(过夜搬运不会跑)
//	⇒ 冷库余额必须算得出那 1.5kg。
//
// # 判别力
//
//	把 loadSkuPositions 的冷库侧改回读位置表 ⇒ 冷库 0 ⇒ 本用例红。
func TestColdStore_LateBackfill_CarryoverIsVisible(t *testing.T) {
	svc, ctx := fmColdSetup(t)
	// 时钟 = 2026-11-05 10:00(+08)= 隔天才补录 ⇒ 不在收盘窗口内。
	svc.SetClock(fmClock(time.Date(2026, 11, 5, 2, 0, 0, 0, time.UTC)))

	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		TakenDate:  "2026-11-02",
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutRib, CubeProductID: "P-RIB", ActualRemainKg: d("1.5")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("RecordPorkCutsStocktake: %v", err)
	}

	// 前置:补录往日**不做**过夜搬运 ⇒ 位置表的冷库侧是空的(这正是生产的形态)。
	if q := fcPositionQty(t, "S001", "P-RIB", model.StockLocationColdStore); !q.IsZero() {
		t.Fatalf("前置:补录往日不该做过夜搬运,位置表冷库却是 %s", q)
	}
	// 但对外的冷库余额必须算得出来。
	if got := fmColdLineOf(t, svc, "P-RIB"); !got.Equal(d("1.5")) {
		t.Errorf("★ 补录往日的盘点也要算进冷库(门店要搬它上柜台),got %s", got)
	}
}

// TestColdStore_HistoricalMissedOvernightMove_CarryoverIsVisible 锁 ①的
// **生产形态**:库里**已经**有一张午夜后提交的盘点单,位置表却**没有**那批货
// ⇒ 冷库余额必须算得出来(不用等门店重录)。
//
// # 场景(复刻生产:店 00 的 11-02 / 11-03)
//
//	11-02 的盘点单 created_at = 11-03 00:27(归属日 11-02)
//	⇒ 旧判据判成「补录往日」⇒ 过夜搬运被跳过
//	⇒ 位置表的冷库侧**永远没有**这批货(存量,门店不会重录)。
//
// # 前置(复刻存量形态)
//
//	直接写一张盘点单(不经过服务 ⇒ 不做物理搬运)⇒ 位置表冷库为空。
//
// # 判别力
//
//	把 loadSkuPositions 的冷库侧改回读位置表 ⇒ 冷库 0 ⇒ 本用例红。
func TestColdStore_HistoricalMissedOvernightMove_CarryoverIsVisible(t *testing.T) {
	svc, _ := fmColdSetup(t)

	// 直接写盘点单:归属日 11-02(taken_at = 11-02 00:00 +08),
	// 柜台实盘 排骨 1.5kg —— 与生产那条单同形。
	cuts, err := json.Marshal([]model.CutSnapshot{
		{CutType: model.CutRib, CubeProductID: "P-RIB", ActualRemainKg: d("1.5")},
	})
	mustNoErr(t, err, "marshal cuts")
	row := model.PorkCutsStocktake{
		ID:         service.StocktakeID("S001", "2026-11-02"),
		BranchID:   "S001",
		TakenAt:    time.Date(2026, 11, 2, 0, 0, 0, 0, fmLoc),
		TakenBy:    "u-1",
		IsComplete: true,
		Cuts:       cuts,
		CreatedAt:  time.Date(2026, 11, 2, 16, 27, 0, 0, time.UTC),
	}
	mustNoErr(t, testDBSingleton.Create(&row).Error, "写盘点单")

	// 前置:位置表**没有**那批货(生产实测:过夜搬运被跳过 ⇒ 冷库侧只有 1 个 SKU)。
	if q := fcPositionQty(t, "S001", "P-RIB", model.StockLocationColdStore); !q.IsZero() {
		t.Fatalf("前置:位置表不该有这批货,却有 %s", q)
	}
	// 但对外的冷库余额必须算得出来 ⇒ 门店今天就能搬这 3 斤排骨。
	if got := fmColdLineOf(t, svc, "P-RIB"); !got.Equal(d("1.5")) {
		t.Errorf("★ 存量:11-02 日终进冷库的 1.5kg 必须算进冷库(不用等门店重录),got %s", got)
	}
}

// TestColdStore_MidnightStocktake_RunsOvernightMove 锁 ②:午夜后提交的那次
// **也**算「收盘盘点」⇒ 过夜搬运照跑(柜台置零 + 冷库 +=)。
//
// # 为什么这条独立于上面那条
//
//	冷库余额改成**按记录层推导**之后,「过夜搬运有没有跑」不再影响冷库数字
//	⇒ 光看冷库余额**抓不到**判据被改回去 ⇒ 必须直接锁**物理留痕**
//	  (位置表的冷库侧 = 过夜搬运写的)。
//
// # 判别力
//
//	把 isClosing 改回 isToday ⇒ 午夜后提交被判成「补录往日」
//	⇒ 过夜搬运被跳过 ⇒ 位置表冷库 0 ⇒ 本用例红。
func TestColdStore_MidnightStocktake_RunsOvernightMove(t *testing.T) {
	svc, ctx := fmColdSetup(t)
	svc.SetClock(fmClock(time.Date(2026, 11, 2, 16, 27, 0, 0, time.UTC)))

	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		TakenDate:  "2026-11-02",
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutRib, CubeProductID: "P-RIB", ActualRemainKg: d("1.5")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("RecordPorkCutsStocktake: %v", err)
	}

	// 位置表(物理留痕):过夜搬运必须跑 ⇒ 冷库 += 1.5。
	if q := fcPositionQty(t, "S001", "P-RIB", model.StockLocationColdStore); !q.Equal(d("1.5")) {
		t.Errorf("★ 午夜后提交的收盘盘点也要做过夜搬运(柜台 → 冷库),位置表冷库 got %s(应为 1.5)", q)
	}
}

// TestColdStore_SumBySkuEqualsPerCutChain 锁 ③:按 SKU 推导的冷库余额
// 与按部位的余额链**逐位一致**。
//
// # 为什么必须一致
//
//	毛利看板/每日毛利读的是**按部位**的链;搬台页读的是**按 SKU**的推导
//	⇒ 两边不一致 ⇒ 门店对不上账(「库存查询 70kg、看板 65kg」)。
//
// # 场景
//
//	期初 10-28:排骨 10kg;10-29 日终盘点:排骨 4kg;10-30 搬上台 1kg
//	⇒ 链上(按部位)排骨 = 10 + 4 − 1 = 13
//	⇒ 按 SKU 推导:P-RIB = 13 ⇒ 两者必须相等。
func TestColdStore_SumBySkuEqualsPerCutChain(t *testing.T) {
	svc, ctx := fmColdSetup(t)

	// 期初(链的起点):排骨 10kg。
	svc.SetClock(fmClock(time.Date(2026, 10, 29, 1, 0, 0, 0, time.UTC)))
	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutRib, BizDate: "2026-10-28", ColdStoreKg: d("10"), UnitPriceYuan: d("16")},
	}); err != nil {
		t.Fatalf("SaveStockOpenings: %v", err)
	}

	// 10-29 日终盘点:柜台实盘 排骨 4kg。
	svc.SetClock(fmClock(time.Date(2026, 10, 29, 15, 0, 0, 0, time.UTC)))
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		TakenDate:  "2026-10-29",
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutRib, CubeProductID: "P-RIB", ActualRemainKg: d("4")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("RecordPorkCutsStocktake(10-29): %v", err)
	}

	// 10-30 搬上台 1kg。
	svc.SetClock(fmClock(time.Date(2026, 10, 30, 1, 0, 0, 0, time.UTC)))
	if _, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		TakenDate: "2026-10-30",
		Lines:     []service.CounterLoadLine{{CubeProductID: "P-RIB", QtyKg: d("1")}},
	}, "u-1"); err != nil {
		t.Fatalf("CounterLoad: %v", err)
	}

	// 按 SKU 推导的冷库余额。
	bySku, err := service.ColdStoreBySkuForTest(ctx, svc, "S001")
	mustNoErr(t, err, "ColdStoreBySkuForTest")
	if got := bySku["P-RIB"]; !got.Equal(d("13")) {
		t.Errorf("★ 按 SKU 推导:期初 10 + 结转 4 − 搬上台 1 = 13,got %s", got)
	}

	// 按部位的链(毛利看板用的那条)必须给出同一个数。
	day := time.Date(2026, 10, 30, 12, 0, 0, 0, fmLoc)
	_, coldByCut, err := service.ClosingByCutForTest(ctx, svc, "S001", day)
	mustNoErr(t, err, "ClosingByCutForTest")
	if got := coldByCut[string(model.CutRib)]; !got.Equal(d("13")) {
		t.Errorf("★ 按部位的链:排骨应为 13,got %s", got)
	}
	// ⚠️ 逐位一致(不只是"看起来差不多")。
	var sum decimal.Decimal
	for _, l := range svcListStockLines(t, svc) {
		if l.CutType == model.CutRib {
			sum = sum.Add(l.ColdStoreKg)
		}
	}
	if !sum.Equal(coldByCut[string(model.CutRib)]) {
		t.Errorf("★ 按 SKU 的合计(%s)必须等于链上该部位的余额(%s)—— 两边口径不一致,门店对不上账",
			sum, coldByCut[string(model.CutRib)])
	}
}

// svcListStockLines 取库存快照的行。
func svcListStockLines(t *testing.T, svc *service.Service) []service.StockLine {
	t.Helper()
	snap, err := svc.ListStock(context.Background(), "S001", model.StockScopeAll)
	mustNoErr(t, err, "ListStock")
	return snap.Lines
}
