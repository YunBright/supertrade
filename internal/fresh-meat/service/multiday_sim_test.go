package service_test

// 2026-10-25：**连续多日**营业模拟（模拟一周营业）。
//
// # 为什么要有这个文件
//
// 现有用例都是**单日**的 ⇒ 抓不到跨日才暴露的问题：
//
//	① 余额链逐日自洽：冷库 = 上期 + 结转 − 搬上台 − 大宗出库 + 大宗进冷库；
//	② 袋子水位不超卖：零售 + 大宗**共用** consumed_kg ⇒ 同一批货不被卖两遍；
//	③ 队列剩余与冷库余额是**同一本账**（差额 = 0）—— 差了就说明"记了两本账"；
//	④ 审计把**实时算**的结论固化成快照（数字不因审计而变）+ 冻结生效；
//	⑤ 跨日**顺序校验**：前一天没盘点 ⇒ 今天不能录猪/搬台（且错误点名要补哪天）。
//
// # 跑在哪
//
// ⇒ **服务层（内存 SQLite）** ⇒ 不碰生产、零压力，可以随意造多天数据。
//
// # 场景（一周，1 店 2 部位）
//
//	期初（10-01 生效）：五花 100kg、板油 40kg
//	每天 10-01 .. 10-07：
//	  ① 进猪 200kg @ 20 元/kg（成本侧）
//	  ② 分割补录：五花 50kg、板油 10kg（成本侧 + 归属快照）
//	  ③ 搬台：把**前一天的袋子整袋搬上猪肉台**（门店口径：整袋搬）
//	  ④ 零售：卖五花 10kg（POS 事件 ⇒ 毛利按 FIFO 归摊到猪）
//	  ⑤ 日终盘点：五花剩 10kg（贴标签）+ 板油 10kg（不装袋）⇒ 夜里进冷库
//	10-07：统收结算把攒着的板油结掉 30kg（6 元/kg）→ 审计结转（截止 10-07）

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/shopspring/decimal"
)

const (
	simBranch   = "S001"
	simBellySKU = "P-BELLY"
	simFatSKU   = "P-FAT"
	simDays     = 7
)

// simDay 第 n 天（n=0 ⇒ 2026-10-01）的营业时刻（+08 下午）。
func simDay(n int) time.Time {
	return time.Date(2026, 10, 1+n, 14, 15, 0, 0, fmLoc)
}

// simKey 第 n 天的营业日文本（YYYY-MM-DD）。
func simKey(n int) string { return simDay(n).Format("2006-01-02") }

// simSetup 铺好「期初 + 部位映射 + 大宗品项」。
func simSetup(t *testing.T) (*service.Service, context.Context) {
	t.Helper()
	svc, _ := setupTestService(t)
	ctx := context.Background()
	svc.SetClock(fmClock(simDay(0)))

	for _, m := range []struct {
		cut model.CutType
		sku string
	}{
		{model.CutBelly, simBellySKU},
		{model.CutFat, simFatSKU},
	} {
		if _, err := svc.CreateBranchCutMapping(ctx, simBranch,
			model.CreateBranchCutMappingInput{CutType: m.cut, CubeProductID: m.sku}); err != nil {
			t.Fatalf("CreateBranchCutMapping(%s): %v", m.cut, err)
		}
	}
	// 大宗品项：板油（fat）—— 大宗结算走它。
	if _, err := svc.CreateBulkProduct(ctx, simBranch,
		service.BulkProductInput{CutType: model.CutFat}); err != nil {
		t.Fatalf("CreateBulkProduct: %v", err)
	}
	return svc, ctx
}

// simRunDay 跑完第 n 天（进猪 → 分割 → 搬台 → 零售 → 日终盘点）。
//
// ⚠️ 每天的**盘点实盘** = 柜台上的货：五花 10kg（贴标签）+ 板油 10kg（不装袋）。
func simRunDay(t *testing.T, svc *service.Service, ctx context.Context, n int) {
	t.Helper()
	svc.SetClock(fmClock(simDay(n)))
	day := simKey(n)

	// ① 进猪 200kg @ 20 元/kg。
	pig, err := svc.RecordWholePig(ctx, simBranch, model.RecordWholePigInput{
		EarTag:                fmt.Sprintf("SIM-%02d", n+1),
		GrossWeightKg:         d("200"),
		PurchaseUnitPriceYuan: d("20"),
		SupplierID:            "SUP-1",
		ArrivedAt:             simDay(n).Add(-6 * time.Hour),
	}, "u-1")
	mustNoErr(t, err, "RecordWholePig")

	// ② 分割补录：五花 50kg、板油 10kg。
	for _, c := range []struct {
		cut model.CutType
		sku string
		kg  string
	}{
		{model.CutBelly, simBellySKU, "50"},
		{model.CutFat, simFatSKU, "10"},
	} {
		_, err := svc.RecordPigCut(ctx, simBranch, model.RecordPigCutInput{
			PigID: pig.ID, CutType: c.cut, Barcode: fmt.Sprintf("SIMBC-%d-%s", n+1, c.sku),
			WeightKg: d(c.kg), ExpiresAt: simDay(n).Add(48 * time.Hour), CubeProductID: c.sku,
		}, "u-1")
		mustNoErr(t, err, "RecordPigCut")
	}

	// ③ 搬台：把**前一天的袋子**整袋搬上猪肉台（D1 没有袋子 ⇒ 不搬）。
	if n > 0 {
		_, err := svc.CounterLoad(ctx, simBranch, service.CounterLoadInput{
			TakenDate: day,
			Lines: []service.CounterLoadLine{
				{Label: fmt.Sprintf("BAG-BELLY-%s", simKey(n-1))},
			},
		}, "u-1")
		mustNoErr(t, err, "CounterLoad(整袋搬)")
	}

	// ④ 零售：卖五花 10kg @ 30 元（D1 不卖：柜台上还没有货）。
	if n > 0 {
		_, err := svc.OnSaleCompleted(ctx, service.SaleCompletedPayload{
			BranchID:    simBranch,
			SaleID:      fmt.Sprintf("SIM-ORDER-%d", n+1),
			OrderStatus: "S",
			PaidAt:      simDay(n).Add(time.Hour),
			Lines: []service.SaleLine{{
				LineID:        fmt.Sprintf("SIM-LINE-%d", n+1),
				ProductID:     simBellySKU,
				Qty:           d("10"),
				UnitPriceYuan: d("30"),
				AmountYuan:    d("300"),
			}},
		})
		mustNoErr(t, err, "OnSaleCompleted")
	}

	// ⑤ 日终盘点：五花 10kg（贴标签）+ 板油 10kg（不装袋）⇒ 夜里进冷库。
	_, err = svc.RecordPorkCutsStocktake(ctx, simBranch, model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		TakenDate:  day,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: simBellySKU, ActualRemainKg: d("10"),
				Label: fmt.Sprintf("BAG-BELLY-%s", day)},
			{CutType: model.CutFat, CubeProductID: simFatSKU, ActualRemainKg: d("10")},
		},
	}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake")
}

// simBags 读该店的全部结转记录（按产生日、id 排序）。
func simBags(t *testing.T) []model.StockBag {
	t.Helper()
	var bags []model.StockBag
	if err := testDBSingleton.Where("branch_id = ?", simBranch).
		Order("origin_date ASC, id ASC").Find(&bags).Error; err != nil {
		t.Fatalf("query stock_bags: %v", err)
	}
	return bags
}

// simSnapshot 读审计落下的**按营业日结余快照**（按部位）。
func simSnapshot(t *testing.T) map[string]map[string]float64 {
	t.Helper()
	type row struct {
		BizDate     string
		CutType     string
		ColdStoreKg float64
	}
	var rows []row
	if err := testDBSingleton.Table("cut_stock_daily").
		Select("biz_date, cut_type, cold_store_kg").
		Where("branch_id = ?", simBranch).
		Order("biz_date ASC").Scan(&rows).Error; err != nil {
		t.Fatalf("query cut_stock_daily: %v", err)
	}
	out := map[string]map[string]float64{}
	for _, r := range rows {
		day := r.BizDate
		if len(day) > 10 {
			day = day[:10] // datatypes.Date 落库带时分秒 ⇒ 只取日期部分
		}
		if out[day] == nil {
			out[day] = map[string]float64{}
		}
		out[day][r.CutType] = r.ColdStoreKg
	}
	return out
}

// TestMultiDaySim_OneWeekBusiness 模拟一周营业，逐条核对跨日不变量。
func TestMultiDaySim_OneWeekBusiness(t *testing.T) {
	svc, ctx := simSetup(t)

	// 期初（**基线日 = 第一期的前一天**：期初生效日是链的起点，它不是营业日）：
	// 五花 100kg @ 18 元、板油 40kg @ 6 元（生效日 2026-09-30）。
	//
	// ⚠️ 单价是**门店给的**：期初是上线前就在冷库里的货，
	//   它的成本不在本系统流水里 ⇒ 不给单价 ⇒ 那批货零成本 ⇒ 毛利虚高。
	if _, err := svc.SaveStockOpenings(ctx, simBranch, []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: simKey(-1), ColdStoreKg: d("100"), UnitPriceYuan: d("18")},
		{CutType: model.CutFat, BizDate: simKey(-1), ColdStoreKg: d("40"), UnitPriceYuan: d("6")},
	}); err != nil {
		t.Fatalf("SaveStockOpenings: %v", err)
	}

	// 一周。
	for n := 0; n < simDays; n++ {
		simRunDay(t, svc, ctx, n)
	}

	// ---- ⑦ 统收结算：把攒着的板油结掉 30kg（6 元/kg）----
	//
	// ⚠️ 这一步会暴露「期初的货没有进 FIFO 队列」的问题：
	//   期初的板油 40kg 在**冷库位置**里，但没有结转记录(袋子)
	//   ⇒ 队列里只有攒出来的 70kg ⇒ 要看 30kg 能不能领到。
	svc.SetClock(fmClock(simDay(simDays - 1)))
	settle, err := svc.CreateBulkSettlement(ctx, simBranch, service.BulkSettlementInput{
		SettleDate:   simKey(simDays - 1),
		Counterparty: "测试酒店",
		Mode:         service.BulkModeDelivery,
		Items: []service.BulkSettlementItemInput{
			{ProductCode: "fat", QtyKg: d("30"), UnitPriceYuan: d("6")},
		},
	}, "u-1")
	mustNoErr(t, err, "CreateBulkSettlement")
	if !settle.TotalAmountYuan.Equal(d("180")) {
		t.Errorf("结算总额应 = 30×6 = 180, got %s", settle.TotalAmountYuan)
	}

	// ---- ① 余额链逐日自洽（用审计快照核对）----
	//
	// 五花：期初 100 + 结转 10×7 − 搬台 10×6 = 110
	// 板油：期初 40 + 结转 10×7 − 大宗出库 30 = 80
	svc.SetClock(fmClock(simDay(simDays - 1)))
	pre, err := svc.PrecheckBizAudit(ctx, simBranch, simKey(simDays-1))
	mustNoErr(t, err, "PrecheckBizAudit")
	if !pre.Ready {
		t.Fatalf("审计预检应就绪: %+v", pre)
	}
	status, err := svc.AuditBusinessDays(ctx, simBranch,
		service.BizAuditInput{CutoffDate: simKey(simDays - 1)}, "u-1")
	mustNoErr(t, err, "AuditBusinessDays")
	if !status.Audited {
		t.Fatalf("审计后应有审计记录: %+v", status)
	}

	snap := simSnapshot(t)
	for n := 0; n < simDays; n++ {
		day := simKey(n)
		got, ok := snap[day]
		if !ok {
			t.Fatalf("%s 没有结余快照", day)
		}
		// 五花：期初 100 + 结转 10×(n+1) − 搬台 10×n = 110（每天净 −10，被结转抵平）
		//   ⇒ 恒为 110。
		// 板油：期初 40 + 结转 10×(n+1) − 大宗出库 30（只有最后一天）⇒ 逐日 +10。
		bellyWant := 110.0
		fatWant := 40.0 + 10.0*float64(n+1)
		if n == simDays-1 {
			fatWant -= 30
		}
		if got[string(model.CutBelly)] != bellyWant {
			t.Errorf("%s 五花冷库应 = %.0f kg, got %v", day, bellyWant, got[string(model.CutBelly)])
		}
		if got[string(model.CutFat)] != fatWant {
			t.Errorf("%s 板油冷库应 = %.0f kg, got %v", day, fatWant, got[string(model.CutFat)])
		}
	}

	// ---- ② 袋子水位不超卖：零售 + 大宗 共用一个 consumed_kg ----
	//
	// 零售卖掉的：6 天 × 10kg = 60kg；大宗结掉的：30kg ⇒ Σ consumed = 90kg。
	var totalQty, totalConsumed decimal.Decimal
	for _, b := range simBags(t) {
		totalQty = totalQty.Add(b.QtyKg)
		totalConsumed = totalConsumed.Add(b.ConsumedKg)
	}
	if totalConsumed.GreaterThan(totalQty) {
		t.Errorf("袋子水位超卖: consumed %s > qty %s", totalConsumed, totalQty)
	}
	if !totalConsumed.Equal(d("90")) {
		t.Errorf("Σ consumed 应 = 60(零售) + 30(大宗) = 90, got %s", totalConsumed)
	}

	// ---- ③ 队列剩余 == 冷库余额（同一本账）----
	balances, err := svc.ListBulkBalances(ctx, simBranch)
	mustNoErr(t, err, "ListBulkBalances")
	for _, b := range balances {
		if !b.LedgerVsColdDiffKg.IsZero() {
			t.Errorf("★ %s: 队列剩余与冷库余额应一致(差额 0), got %s —— "+
				"说明零售/大宗扣的是**另一批**货(位置混用)", b.ProductCode, b.LedgerVsColdDiffKg)
		}
	}

	// ---- ④ 审计后：截止日及之前**冻结**（补录/搬台/结算/改期初 全被拒）----
	frozen := []struct {
		what string
		do   func() error
	}{
		{"补录截止日之前的盘点", func() error {
			_, err := svc.RecordPorkCutsStocktake(ctx, simBranch, model.RecordPorkCutsStocktakeInput{
				IsComplete: true, TakenDate: simKey(2),
				Cuts: []model.CutInput{{CutType: model.CutBelly, CubeProductID: simBellySKU, ActualRemainKg: d("1")}},
			}, "u-1")
			return err
		}},
		{"改截止日之前的搬台", func() error {
			_, err := svc.CounterLoad(ctx, simBranch, service.CounterLoadInput{
				TakenDate: simKey(2),
				Lines:     []service.CounterLoadLine{{CubeProductID: simBellySKU, QtyKg: d("1")}},
			}, "u-1")
			return err
		}},
		{"改截止日之前的大宗结算", func() error {
			_, err := svc.CreateBulkSettlement(ctx, simBranch, service.BulkSettlementInput{
				SettleDate: simKey(2), Mode: service.BulkModeDelivery,
				Items: []service.BulkSettlementItemInput{{ProductCode: "fat", QtyKg: d("1"), UnitPriceYuan: d("6")}},
			}, "u-1")
			return err
		}},
		{"改截止日之前的期初结转", func() error {
			_, err := svc.SaveStockOpenings(ctx, simBranch, []service.StockOpeningInput{
				{CutType: model.CutBelly, BizDate: simKey(0), ColdStoreKg: d("999"), UnitPriceYuan: d("18")},
			})
			return err
		}},
	}
	for _, f := range frozen {
		if err := f.do(); err == nil {
			t.Errorf("★ 审计之后 %s 必须被拒(冻结不可逆)", f.what)
		}
	}

	// ---- ⑤ 预期毛利：这里**不验**（单测没装配价格客户端 ⇒ 不做预售结转）
	//
	// 「取不到也按 0 生成」是**生产**口径（价格客户端在位）。
	// 单测环境没有价格源 ⇒ 预售结转被跳过（"不猜价"）⇒ 断言不了。
	// ⇒ 预期毛利与人工改价由**生产契约**验：
	//    deployer/tests/contract/fresh_meat_bizday_test.go
	//    （TestFreshMeatBizDay_AuditLifecycle 覆盖 审计→冻结→预期毛利改价）。
}

// TestMultiDaySim_MissingStocktakeBlocksNextDay 锁⑤：跨日**顺序校验**。
//
// 门店口径：「不补录昨天，就无法录今天」⇒ 前一个营业日没盘点 ⇒
// 下一个营业日**不能录猪、不能搬台**，而且错误信息**要点名要补哪天**。
func TestMultiDaySim_MissingStocktakeBlocksNextDay(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()
	const branch = "S-ORDER"
	svc.SetClock(fmClock(simDay(0)))
	if _, err := svc.CreateBranchCutMapping(ctx, branch,
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: simBellySKU}); err != nil {
		t.Fatalf("CreateBranchCutMapping: %v", err)
	}

	// 10-01：正常营业（有进猪 + 有盘点）。
	if _, err := svc.RecordWholePig(ctx, branch, model.RecordWholePigInput{
		EarTag: "ORD-1", GrossWeightKg: d("100"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: simDay(0).Add(-6 * time.Hour),
	}, "u-1"); err != nil {
		t.Fatalf("RecordWholePig(10-01): %v", err)
	}
	if _, err := svc.RecordPorkCutsStocktake(ctx, branch, model.RecordPorkCutsStocktakeInput{
		IsComplete: true, TakenDate: simKey(0),
		Cuts: []model.CutInput{{CutType: model.CutBelly, CubeProductID: simBellySKU, ActualRemainKg: d("5")}},
	}, "u-1"); err != nil {
		t.Fatalf("RecordPorkCutsStocktake(10-01): %v", err)
	}

	// 10-02：**故意漏盘点**。
	svc.SetClock(fmClock(simDay(1)))
	if _, err := svc.RecordWholePig(ctx, branch, model.RecordWholePigInput{
		EarTag: "ORD-2", GrossWeightKg: d("100"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: simDay(1).Add(-6 * time.Hour),
	}, "u-1"); err != nil {
		t.Fatalf("10-02 进猪应成功(当天还没盘点): %v", err)
	}

	// 10-03：10-02 没盘点 ⇒ 录猪 / 搬台都必须被拒，且**点名要补 10-02**。
	svc.SetClock(fmClock(simDay(2)))
	_, err := svc.RecordWholePig(ctx, branch, model.RecordWholePigInput{
		EarTag: "ORD-3", GrossWeightKg: d("100"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: simDay(2).Add(-6 * time.Hour),
	}, "u-1")
	if err == nil {
		t.Fatalf("★ 前一个营业日没盘点 ⇒ 10-03 录猪必须被拒")
	}
	if !strings.Contains(err.Error(), simKey(1)) {
		t.Errorf("★ 错误信息必须点名要补哪天(%s), got %v", simKey(1), err)
	}
	_, err = svc.CounterLoad(ctx, branch, service.CounterLoadInput{
		TakenDate: simKey(2),
		Lines:     []service.CounterLoadLine{{CubeProductID: simBellySKU, QtyKg: d("1")}},
	}, "u-1")
	if err == nil {
		t.Fatalf("★ 前一个营业日没盘点 ⇒ 10-03 搬台必须被拒")
	}
	if !strings.Contains(err.Error(), simKey(1)) {
		t.Errorf("★ 搬台的错误信息也必须点名要补哪天(%s), got %v", simKey(1), err)
	}
}
