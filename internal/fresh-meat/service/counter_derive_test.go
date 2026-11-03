// 2026-11-04:锁「**柜台(猪肉台)余额按记录层推导**」。
//
// # 生产事故(店 00)
//
//	位置表(sku_stock_positions)的**柜台侧只累加、不扣零售卖出** ⇒ 余额只涨不跌。
//	11-02 外采了 11.4kg 肋排并**全部卖掉**,而位置表 01401/counter 停在 **44.350 kg**
//	(= 10-30/10-31/11-01/11-02 四笔外采之和)⇒ 门店在「库存」页看到的是**幽灵货**。
//
//	成因:位置表柜台侧只在**过夜搬运**(盘点夜)时置零,而门店是 11-03 上午
//	一次性**补录**了 10-30~11-02 四天的外采(created_at 全在 11-03 09:30 之后)
//	⇒ 全部写在 11-02 收盘盘点(11-03 00:27 置零)之后 ⇒ 再也掉不下去。
//
// # 修法
//
//	柜台余额改成**按记录层推导**(counter_derive.go):
//	窗口 = (最近一次日终盘点的归属日, 今天]
//	柜台 = Σ(搬上台 + 外采 + 分割产出 − 零售卖出 − 大宗柜台领用 − 报损 − 搭赠),裁 0
//
// # 判别力
//
//	① 把 loadCounterBalances 改回读位置表 ⇒ 用例 ① 红(幽灵余额又回来了);
//	② 把窗口起点从「归属日」改成别的 ⇒ 用例 ② 红。
package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/shopspring/decimal"
	"gorm.io/datatypes"
)

// cdSetup 起服务 + 登记「肋排 → P-RIB」的货号映射。
func cdSetup(t *testing.T) (*service.Service, context.Context) {
	t.Helper()
	svc, _ := setupTestService(t)
	ctx := context.Background()
	if _, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutRib, CubeProductID: "P-RIB"}); err != nil {
		t.Fatalf("CreateBranchCutMapping rib: %v", err)
	}
	return svc, ctx
}

// cdCounterOf 取推导出来的柜台余额(某 SKU)。
func cdCounterOf(t *testing.T, svc *service.Service, sku string) decimal.Decimal {
	t.Helper()
	bySku, err := service.CounterBySkuForTest(context.Background(), svc, "S001")
	mustNoErr(t, err, "CounterBySkuForTest")
	return bySku[sku]
}

// cdSell 往本地销售事件表塞一笔零售(无源门店走这条)。
func cdSell(t *testing.T, day string, sku string, qty string) {
	t.Helper()
	d, err := time.ParseInLocation("2006-01-02", day, time.UTC)
	mustNoErr(t, err, "parse sale day")
	mustNoErr(t, testDBSingleton.Create(&model.LineSalesByPig{
		BranchID: "S001", SaleDate: datatypes.Date(d), PigID: "PIG-1",
		CutType: model.CutRib, QtyKg: d2(qty), AmountYuan: d2("100"),
		UnitPriceYuan: d2("20"), Source: "pos", PosLineID: "L-" + day + "-" + sku,
		PosBranchID: "S001", CubeProductID: sku, OrderStatus: "S",
		CreatedAt: time.Now(),
	}).Error, "插本地销售")
}

// d2 是测试里的 decimal 辅助(与本包已有的 d() 同义,避免命名冲突)。
func d2(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// TestCounter_BackdatedExternalPurchase_NoGhostBalance 锁 ①:
// **补录的外采**(归属日在最近一次盘点之前/当天)不许变成柜台上的幽灵货。
//
// # 场景(复刻生产:店 00 的 11-02 / 11-03)
//
//	11-03 00:27:门店提交 **11-02** 的日终盘点(午夜后提交 ⇒ 收盘盘点)
//	            ⇒ 过夜搬运照跑 ⇒ 位置表柜台侧被**置零**
//	11-03 09:30:门店**补录**一笔外采:归属日 11-02、01401 肋排 11.4kg
//	            ⇒ 位置表柜台侧 += 11.4 ⇒ 幽灵余额形成
//
// # 期望
//
//	位置表(物理留痕) = 11.4kg(生产形态,复现成功)
//	推导值(对外口径) = 0 —— 那 11.4kg 归属日是 11-02,已被当晚的实盘吸收
//
// # 判别力
//
//	把 loadCounterBalances 改回读位置表 ⇒ 柜台 = 11.4 ⇒ 本用例红。
func TestCounter_BackdatedExternalPurchase_NoGhostBalance(t *testing.T) {
	svc, ctx := cdSetup(t)

	// ① 11-03 00:27 提交 11-02 的收盘盘点(午夜后提交 ⇒ 走收盘窗口)。
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

	// ② 11-03 09:30 补录一笔归属日为 **11-02** 的外采 11.4kg。
	svc.SetClock(fmClock(time.Date(2026, 11, 3, 1, 30, 0, 0, time.UTC)))
	if _, err := svc.RecordExternalPurchase(ctx, "S001", service.ExternalPurchaseInput{
		CubeProductID: "P-RIB", QtyKg: d("11.4"), UnitPriceYuan: d("35"),
		TakenDate: "2026-11-02", Note: "backfill",
	}, "u-1"); err != nil {
		t.Fatalf("RecordExternalPurchase: %v", err)
	}

	// 前置:位置表柜台侧确实攒出了幽灵余额(生产形态)。
	if q := fcPositionQty(t, "S001", "P-RIB", model.StockLocationCounter); !q.Equal(d("11.4")) {
		t.Fatalf("前置:位置表柜台侧应是 11.4(补录的外采写进去了),got %s", q)
	}

	// 对外口径:推导值必须**没有**这 11.4kg(归属日 11-02 ⇒ 已被当晚实盘吸收)。
	if got := cdCounterOf(t, svc, "P-RIB"); !got.IsZero() {
		t.Errorf("★ 补录的外采不该留在柜台上(货早卖完了),got %s", got)
	}
	// 库存页(门店看到的那一层)也必须是 0。
	snap, err := svc.ListStock(ctx, "S001", model.StockScopeCounter)
	mustNoErr(t, err, "ListStock")
	for _, l := range snap.Lines {
		if l.CubeProductID == "P-RIB" && !l.CounterKg.IsZero() {
			t.Errorf("★ 库存页的猪肉台不该有幽灵货,got %s", l.CounterKg)
		}
	}
}

// TestCounter_TodayLoadAndSale_NetBalance 锁 ①的正向口径:
// **搬上台 − 零售卖出** = 柜台上剩下的量。
//
// # 场景
//
//	10-29 日终盘点(锚点);10-30 搬上台 10kg、当天卖掉 6kg ⇒ 柜台应剩 4kg。
//
// # 判别力
//
//	把 loadCounterBalances 改回读位置表 ⇒ 位置表柜台侧是 10(它不扣卖出)
//	⇒ 柜台 = 10 ⇒ 本用例红。
func TestCounter_TodayLoadAndSale_NetBalance(t *testing.T) {
	svc, ctx := cdSetup(t)

	// 10-29 收盘盘点(锚点):柜台实盘 0。
	svc.SetClock(fmClock(time.Date(2026, 10, 29, 15, 0, 0, 0, time.UTC)))
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		TakenDate:  "2026-10-29",
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutRib, CubeProductID: "P-RIB", ActualRemainKg: d("0")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("RecordPorkCutsStocktake: %v", err)
	}

	// 10-30 搬上台 10kg。
	svc.SetClock(fmClock(time.Date(2026, 10, 30, 1, 0, 0, 0, time.UTC)))
	if _, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		TakenDate: "2026-10-30",
		Lines:     []service.CounterLoadLine{{CubeProductID: "P-RIB", QtyKg: d("10")}},
	}, "u-1"); err != nil {
		t.Fatalf("CounterLoad: %v", err)
	}

	// 10-30 卖掉 6kg。
	cdSell(t, "2026-10-30", "P-RIB", "6")

	if got := cdCounterOf(t, svc, "P-RIB"); !got.Equal(d("4")) {
		t.Errorf("★ 柜台应是 搬上台 10 − 卖出 6 = 4,got %s", got)
	}
}

// TestCounter_PigProduction_ShowsOnCounter 锁 ①的另一项:
// **当天切出来的猪��**要算在柜台上(不然柜台恒显示 0,门店看不到自己刚切出来的货)。
//
// # 场景
//
//	10-30 进猪 1 头(50kg)并补录分割:肋排 10kg ⇒ 柜台应有 10kg。
//
// # 判别力
//
//	把「分割产出」这一项去掉 ⇒ 柜台 = 0 ⇒ 本用例红。
func TestCounter_PigProduction_ShowsOnCounter(t *testing.T) {
	svc, ctx := cdSetup(t)

	// 10-29 收盘盘点(锚点)。
	svc.SetClock(fmClock(time.Date(2026, 10, 29, 15, 0, 0, 0, time.UTC)))
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		TakenDate:  "2026-10-29",
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutRib, CubeProductID: "P-RIB", ActualRemainKg: d("0")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("RecordPorkCutsStocktake: %v", err)
	}

	// 10-30 进猪 + 补录分割明细(肋排 10kg)。
	// ⚠️ 猪的进场时间必须落在 **10-30 的营业日窗口**里。
	//   窗口 = [10-30 00:00 +08, 11-01 00:00 +08) = [10-29 16:00Z, 10-30 16:00Z)。
	//   ⚠️ 测试库是 SQLite:`arrived_at >= ?` 走的是**字符串比较**
	//     ⇒ 存成什么时区偏移就得是什么(生产是 PG,按时间戳比较,没这个问题)。
	//   这里用 10-30 02:00Z(= 10-30 10:00 +08)⇒ 两种比较都落在窗口内。
	pigDay := time.Date(2026, 10, 30, 2, 0, 0, 0, time.UTC)
	svc.SetClock(fmClock(time.Date(2026, 10, 30, 1, 0, 0, 0, time.UTC)))
	pig, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-C1", GrossWeightKg: d("50"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: pigDay,
	}, "u-1")
	mustNoErr(t, err, "RecordWholePig")
	_, err = svc.RecordPigCut(ctx, "S001", model.RecordPigCutInput{
		PigID: pig.ID, CutType: model.CutRib, Barcode: "B-C1",
		WeightKg: d("10"), ExpiresAt: pigDay.Add(48 * time.Hour), CubeProductID: "P-RIB",
	}, "u-1")
	mustNoErr(t, err, "RecordPigCut")

	if got := cdCounterOf(t, svc, "P-RIB"); !got.Equal(d("10")) {
		t.Errorf("★ 当天切出来的 10kg 肋排要算在柜台上,got %s", got)
	}
}

// TestCounter_StocktakeDayEvents_AreAbsorbed 锁 ②:窗口按**归属日**切,
// 盘点日当天发生的搬台**被当晚的实盘吸收** ⇒ 不留在柜台上。
//
// # 为什么这条重要
//
//	门店的盘点夜会把柜台**置零**、实盘搬进冷库 ⇒ 盘点日当天搬上台的货
//	已经进了当晚的实盘 ⇒ 再算一次就是**双算**。
//
// # 判别力
//
//	把窗口起点从「归属日」改成「归属日的前一天」(即把盘点日当天也算进窗口)
//	⇒ 柜台 = 10 ⇒ 本用例红。
func TestCounter_StocktakeDayEvents_AreAbsorbed(t *testing.T) {
	svc, ctx := cdSetup(t)

	// 10-29 当天:搬上台 10kg(归属日 10-29),随后 10-29 收盘盘点(实盘 0 ⇒ 卖光了)。
	svc.SetClock(fmClock(time.Date(2026, 10, 29, 1, 0, 0, 0, time.UTC)))
	if _, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		TakenDate: "2026-10-29",
		Lines:     []service.CounterLoadLine{{CubeProductID: "P-RIB", QtyKg: d("10")}},
	}, "u-1"); err != nil {
		t.Fatalf("CounterLoad: %v", err)
	}
	svc.SetClock(fmClock(time.Date(2026, 10, 29, 15, 0, 0, 0, time.UTC)))
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		TakenDate:  "2026-10-29",
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutRib, CubeProductID: "P-RIB", ActualRemainKg: d("0")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("RecordPorkCutsStocktake: %v", err)
	}

	// 盘点日当天的搬台被当晚实盘吸收 ⇒ 柜台上不该再有它。
	if got := cdCounterOf(t, svc, "P-RIB"); !got.IsZero() {
		t.Errorf("★ 盘点日当天的搬台被当晚实盘吸收,不该留在柜台上,got %s", got)
	}
}

// TestCounter_DeriveDisabled_FallsBackToPositionTable 锁**降级路径**:
// 推不出来(这家店一张盘点单都没有)⇒ 退回位置表,并且**大声**记日志。
//
// # 为什么这条重要
//
//	降级必须是**可见**的:悄悄给一个旧口径的数 ⇒ 门店看到的还是幽灵余额,
//	而没人知道 ⇒ 这正是 2026-11-04 修的那个坑。
func TestCounter_DeriveDisabled_FallsBackToPositionTable(t *testing.T) {
	svc, ctx := cdSetup(t)

	// 没有任何盘点单 ⇒ 推不出来(没有归零的起点)⇒ 退回位置表。
	svc.SetClock(fmClock(time.Date(2026, 10, 30, 1, 0, 0, 0, time.UTC)))
	if _, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		TakenDate: "2026-10-30",
		Lines:     []service.CounterLoadLine{{CubeProductID: "P-RIB", QtyKg: d("7")}},
	}, "u-1"); err != nil {
		t.Fatalf("CounterLoad: %v", err)
	}

	// 推导值:没有盘点单 ⇒ 推不出来。
	if _, err := service.CounterBySkuForTest(ctx, svc, "S001"); err == nil {
		t.Errorf("★ 没有盘点单时柜台余额**推不出来**,不该返回一个数")
	}
	// 对外口径:退回位置表(旧口径)= 7kg,并且不报错。
	snap, err := svc.ListStock(ctx, "S001", model.StockScopeCounter)
	mustNoErr(t, err, "ListStock")
	found := false
	for _, l := range snap.Lines {
		if l.CubeProductID == "P-RIB" {
			found = true
			if !l.CounterKg.Equal(d("7")) {
				t.Errorf("★ 推不出来时要退回位置表(7),got %s", l.CounterKg)
			}
		}
	}
	if !found {
		t.Errorf("★ 退回位置表时也要把 P-RIB 返给门店")
	}
}
