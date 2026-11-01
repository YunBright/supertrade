package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// 本文件覆盖备货系数（2026-10-31 用户口径）。
//
// ## 最容易假绿的三处
//
//  1. **只测「能算出数」**。真正要保证的是口径：
//     周期销售数 = 日均（按采样天数）× 周期；采样天数 = max(今天−收货日, 周期)×2。
//     口径写错算出来的数「看着像」却会让审批给出错误建议。
//

func TestPurchaseIntervals_Median(t *testing.T) {
	med, n := purchaseIntervals([]string{"2026-09-01", "2026-09-08", "2026-09-15"})
	if n != 2 {
		t.Fatalf("intervals = %d, want 2", n)
	}
	if med != 7 {
		t.Errorf("median = %v, want 7", med)
	}
}

func TestPurchaseIntervals_DedupSameDay(t *testing.T) {
	// 同一天录两张单 ≠ 进了两次货（录单员补录很常见）。
	med, n := purchaseIntervals([]string{"2026-09-01", "2026-09-01"})
	if n != 0 || med != 0 {
		t.Errorf("同一天应无间隔, got med=%v n=%d", med, n)
	}
}

func TestPurchaseIntervals_SingleOrEmpty(t *testing.T) {
	if med, n := purchaseIntervals([]string{"2026-09-01"}); n != 0 || med != 0 {
		t.Errorf("单次进货应无间隔, got med=%v n=%d", med, n)
	}
	if med, n := purchaseIntervals(nil); n != 0 || med != 0 {
		t.Errorf("空应无间隔, got med=%v n=%d", med, n)
	}
}

func TestDailySalesStats_AvgBySampleDays(t *testing.T) {
	from := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	rows := []DailySaleRow{
		{ProductID: "P1", BusinessDate: "2026-09-10", Quantity: dec("10")},
		{ProductID: "P1", BusinessDate: "2026-09-20", Quantity: dec("20")},
		// 窗口外的行不参与统计。
		{ProductID: "P1", BusinessDate: "2026-08-01", Quantity: dec("100")},
	}
	avg, cv, total := dailySalesStats(rows, from, to, 30)
	if total != 30 {
		t.Errorf("total = %v, want 30", total)
	}
	if !decimal.NewFromFloat(avg).Equal(decimal.NewFromFloat(1.0)) {
		t.Errorf("avg = %v, want 1.0 (30/30)", avg)
	}
	// 两天的销量 10/20：均值 15，标准差 5 ⇒ CV = 5/15 = 1/3。
	if !decimal.NewFromFloat(cv).Round(3).Equal(dec("0.333")) {
		t.Errorf("cv = %v, want 0.333", cv)
	}
}

func TestComputeStockingCoefficient_PeriodFromIntervals(t *testing.T) {
	today := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	daily := []DailySaleRow{
		{ProductID: "P1", BusinessDate: "2026-09-28", Quantity: dec("7")},
		{ProductID: "P1", BusinessDate: "2026-09-29", Quantity: dec("7")},
	}
	row := computeStockingCoefficient("B1", "P1", "可乐", daily,
		[]string{"2026-09-01", "2026-09-08", "2026-09-15"}, decimal.Zero, false, today, today)

	if row.PeriodDays != 7 {
		t.Errorf("PeriodDays = %d, want 7（进货间隔中位数）", row.PeriodDays)
	}
	// sampleDays = max(2×7, 30) = 30。
	if row.SampleDays != 30 {
		t.Errorf("SampleDays = %d, want 30", row.SampleDays)
	}
	// avg = 14/30；周期销售数 = avg×7 ≈ 3.267。
	if !row.AvgDailySales.Equal(dec("0.467")) {
		t.Errorf("AvgDailySales = %s, want 0.467", row.AvgDailySales)
	}
	if !row.PeriodSalesQty.Equal(dec("3.267")) {
		t.Errorf("PeriodSalesQty = %s, want 3.267", row.PeriodSalesQty)
	}
	if row.Source != model.StockingSourceAuto {
		t.Errorf("Source = %s, want auto", row.Source)
	}
	if row.ReceiptCount != 3 {
		t.Errorf("ReceiptCount = %d, want 3", row.ReceiptCount)
	}
}

func TestComputeStockingCoefficient_DefaultsWithoutHistory(t *testing.T) {
	today := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	row := computeStockingCoefficient("B1", "P1", "新品", nil, nil, decimal.Zero, false, today, today)
	if row.PeriodDays != stockingDefaultPeriodDays {
		t.Errorf("无进货历史 ⇒ 周期应为默认 %d, got %d", stockingDefaultPeriodDays, row.PeriodDays)
	}
	if row.Multiplier.LessThan(dec("1.0")) || row.Multiplier.GreaterThan(dec("3.0")) {
		t.Errorf("倍数应在 [1,3] 之间, got %s", row.Multiplier)
	}
	if row.ReceiptCount != 0 {
		t.Errorf("ReceiptCount = %d, want 0", row.ReceiptCount)
	}
}

func TestComputeStockingCoefficient_PeriodClamped(t *testing.T) {
	today := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	// 间隔 200 天 ⇒ 周期应被夹到上限 60。
	row := computeStockingCoefficient("B1", "P1", "慢销", nil,
		[]string{"2026-01-01", "2026-07-20"}, decimal.Zero, false, today, today)
	if row.PeriodDays != stockingMaxPeriodDays {
		t.Errorf("PeriodDays = %d, want clamp 到 %d", row.PeriodDays, stockingMaxPeriodDays)
	}
}

func TestWriteStockingFacts_Prompt(t *testing.T) {
	var sb strings.Builder
	writeStockingFacts(&sb, []StockingGroupFact{{
		GroupKey:      "P1|P2",
		ProductID:     "P1",
		Label:         "可乐-P1(组合商品 2 个)",
		MemberIDs:     []string{"P1", "P2"},
		BaseUnit:      "瓶",
		SampleDays:    180,
		InStockDays:   30,
		SalesQty:      dec("60"),
		AvgDailySales: dec("2"),
		StockQty:      dec("20"),
		ReceivedQty:   dec("120"),
		CapDays:       45,
		CapQty:        dec("90"),
		AfterQty:      dec("140"),
		CoverageDays:  dec("70"),
		FloorQty:      dec("12"),
		SingleMedian:  dec("60"),
	}})
	got := sb.String()
	for _, want := range []string{
		"组级备货核对", "可乐-P1", "本次进货 120 瓶", "组内库存 20 瓶",
		"可售上限 45 天", "补货后可售 70.0 天", "一次进货下限 12 瓶",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt 缺少 %q:\n%s", want, got)
		}
	}
}

// ---- 落库 + CRUD ----

func TestEnsureStockingCoefficients_PersistsAuto(t *testing.T) {
	svc, db, cube, _, _ := newTestService(t)
	today := svc.now()
	cube.dailySales = []DailySaleRow{
		{ProductID: "P1", BusinessDate: "2026-09-29", Quantity: dec("12")},
	}
	got := svc.ensureStockingCoefficients(context.Background(), "B1", []string{"P1"}, map[string]string{"P1": "可乐"}, today)
	if _, ok := got["P1"]; !ok {
		t.Fatalf("应算出 P1 的系数")
	}
	var row model.StockingCoefficient
	if err := db.Where("branch_id = ? AND product_id = ?", "B1", "P1").First(&row).Error; err != nil {
		t.Fatalf("应落库: %v", err)
	}
	if row.Source != model.StockingSourceAuto || row.ProductName != "可乐" {
		t.Errorf("落库内容不对: %+v", row)
	}
}

func TestStockingCRUD_ManualNotOverwritten(t *testing.T) {
	svc, db, cube, _, _ := newTestService(t)
	// 一条人工行 + 一条自动行。
	if err := db.Create(&model.StockingCoefficient{
		ID: "id-manual", BranchID: "B1", ProductID: "P1", ProductName: "可乐",
		Multiplier: dec("2.00"), PeriodDays: 10, Source: model.StockingSourceManual,
	}).Error; err != nil {
		t.Fatalf("seed manual: %v", err)
	}
	if err := db.Create(&model.StockingCoefficient{
		ID: "id-auto", BranchID: "B1", ProductID: "P2", ProductName: "雪碧",
		Multiplier: dec("1.10"), PeriodDays: 7, Source: model.StockingSourceAuto,
	}).Error; err != nil {
		t.Fatalf("seed auto: %v", err)
	}
	cube.dailySales = []DailySaleRow{
		{ProductID: "P1", BusinessDate: "2026-09-29", Quantity: dec("12")},
		{ProductID: "P2", BusinessDate: "2026-09-29", Quantity: dec("12")},
	}
	res, err := svc.RegenerateStockingCoefficients(context.Background(), "B1", []string{"P1", "P2"}, "u-admin")
	if err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	if res.SkippedManual != 1 || res.Regenerated != 1 {
		t.Errorf("人工行应跳过: %+v", res)
	}
	var manual model.StockingCoefficient
	if err := db.First(&manual, "id = ?", "id-manual").Error; err != nil {
		t.Fatalf("读 manual: %v", err)
	}
	if manual.Source != model.StockingSourceManual || !manual.Multiplier.Equal(dec("2.00")) {
		t.Errorf("人工行被覆盖了: %+v", manual)
	}

	// 读列表。
	rows, total, err := svc.ListStockingCoefficients(context.Background(), "B1", "", 10, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 2 || len(rows) != 2 {
		t.Errorf("列表 = %d/%d, want 2/2", len(rows), total)
	}

	// 修改 → manual。
	upd, err := svc.UpdateStockingCoefficient(context.Background(), "B1", "P2", StockingUpdateInput{
		Multiplier: dec("1.80"), PeriodDays: 14, Note: "促销期垫高", OperatorID: "u-admin",
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Source != model.StockingSourceManual || upd.PeriodDays != 14 || !upd.Multiplier.Equal(dec("1.80")) {
		t.Errorf("修改结果不对: %+v", upd)
	}

	// 参数校验。
	if _, err := svc.UpdateStockingCoefficient(context.Background(), "B1", "P2", StockingUpdateInput{
		Multiplier: dec("1.50"), PeriodDays: 2,
	}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("周期 2 天应被拒, got %v", err)
	}
	if _, err := svc.UpdateStockingCoefficient(context.Background(), "B1", "P2", StockingUpdateInput{
		Multiplier: dec("5.00"), PeriodDays: 7,
	}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("倍数 5 应被拒, got %v", err)
	}

	// 删除。
	if err := svc.DeleteStockingCoefficient(context.Background(), "B1", "P2"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := svc.DeleteStockingCoefficient(context.Background(), "B1", "P2"); !errors.Is(err, ErrStockingNotFound) {
		t.Errorf("重复删除应 404, got %v", err)
	}
}

func TestStockingFactsForAudit_SampleDaysUsesReceiptDate(t *testing.T) {
	svc, _, cube, _, _ := newTestService(t)
	today := svc.now()
	// 收货日 2026-09-20（实际收货日）→ daysSince=11；周期 7 ⇒ sampleDays = max(11,7)×2 = 22。
	cube.dailySales = []DailySaleRow{
		{ProductID: "P1", BusinessDate: "2026-09-28", Quantity: dec("22")},
	}
	cube.purchaseDates = []ProductPurchaseDateRow{
		{ProductID: "P1", SheetDate: "2026-09-20"},
	}
	facts := svc.stockingFactsForAudit(context.Background(), "B1", []string{"P1"},
		map[string]purchaseBasis{"P1": {NetQty: dec("30"), Days: 1}},
		map[string]string{"PI-1": "2026-09-20"},
		map[string][]string{"P1": {"PI-1"}},
		map[string]string{"P1": "可乐"}, today)
	if len(facts) != 1 {
		t.Fatalf("应给出 1 条事实, got %d", len(facts))
	}
	f := facts[0]
	if f.SampleDays != 22 {
		t.Errorf("SampleDays = %d, want 22", f.SampleDays)
	}
	// 有货天数 = 首收货日 09-20 ~ 今天 10-01 = 12 天（fake 不返库存 ⇒ 按"还在卖"处理）。
	if f.InStockDays != 12 {
		t.Errorf("InStockDays = %d, want 12(收货日到今天)", f.InStockDays)
	}
	// 日均 = 22/12 = 1.833（分母是有货天数，不是采样天数 22）；周期销售数 = 12.833。
	if !f.AvgDailySales.Equal(dec("1.833")) || !f.PeriodSalesQty.Equal(dec("12.833")) {
		t.Errorf("日均/周期销售数不对: %+v", f)
	}
	if !f.ReasonableQty.Equal(dec("12.833")) {
		t.Errorf("合理备货量 = %s, want 12.833", f.ReasonableQty)
	}
	// 可售天数 = (库存 0(fake 不返库存) + 进货 30)/1.833 = 16.4。
	if !f.CoverageDays.Equal(dec("16.4")) {
		t.Errorf("CoverageDays = %s, want 16.4", f.CoverageDays)
	}
}

// TestInStockDays_SoldOutStopsAtLastSale 是 2026-11-01 用户口径的回归锁：
// 卖完就断货的 SKU，缺货期不能算进"有货天数"（水果木瓜-00418 实单：6 天）。
func TestInStockDays_SoldOutStopsAtLastSale(t *testing.T) {
	today := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	from := today.AddDate(0, 0, -53) // 采样 54 天
	// 10-05 收货、10-10 最后一天有销量、当前库存 0 ⇒ 有货 6 天。
	if got := inStockDays(from, 54, decimal.Zero, true, "2026-10-05", "2026-10-10", today); got != 6 {
		t.Errorf("有货天数 = %d, want 6(10-05~10-10)", got)
	}
}

// TestInStockDays_StillInStockCountsToToday 还有库存 ⇒ 算到今天（断货期不存在）。
func TestInStockDays_StillInStockCountsToToday(t *testing.T) {
	today := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	from := today.AddDate(0, 0, -53)
	if got := inStockDays(from, 54, dec("5"), true, "2026-10-05", "2026-10-10", today); got != 28 {
		t.Errorf("有货天数 = %d, want 28(10-05~11-01)", got)
	}
}

// TestInStockDays_StockUnknownTreatedAsInStock 取不到库存 ⇒ 按还在卖（偏严）。
func TestInStockDays_StockUnknownTreatedAsInStock(t *testing.T) {
	today := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	from := today.AddDate(0, 0, -53)
	if got := inStockDays(from, 54, decimal.Zero, false, "2026-10-05", "2026-10-10", today); got != 28 {
		t.Errorf("有货天数 = %d, want 28(库存未知按还在卖)", got)
	}
}

// TestInStockDays_NoReceiptOrSaleFallsBackToWindow 取不到收货/销量 ⇒ 兜底按窗口长度。
func TestInStockDays_NoReceiptOrSaleFallsBackToWindow(t *testing.T) {
	today := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	from := today.AddDate(0, 0, -53)
	if got := inStockDays(from, 54, decimal.Zero, true, "", "", today); got != 54 {
		t.Errorf("有货天数 = %d, want 54(起点=窗口起点)", got)
	}
}
