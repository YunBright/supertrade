// Package service_test —— 长期毛利(期间毛利)的**部位级回归**(2026-10-27)。
//
// # 为什么单独锁这一条
//
// 生产实测(2026-10-27 改完后立刻踩到):店 00 从 9 月至今**没有任何盘点单**
// ⇒ `cut_cost_daily`(部位级成本日快照)是**空的**
// ⇒ 只读快照的期间毛利返回**全 0**(`group_by=day|cut` rows=[],
//   `group_by=sku` 成本恒 0)
// ⇒ **接口看起来"正常"(HTTP 200)但其实没有数据** —— 最坏的一种坏法:
//   没人会报错,门店只会看到"毛利是 0"。
//
// ⇒ 缺快照的那天**实时算**(与每日毛利同一个引擎)⇒ 数字不为 0。
package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

// TestPeriodMargin_NoSnapshot_StillRealNumbers 锁"没有日快照的店,期间毛利仍是真实数字"。
//
// 场景:进猪 + 分割 + 卖货,但**不录日终盘点**
// ⇒ 没有成本日快照(快照只在日终盘点后落库)
// ⇒ 期间毛利必须**实时算**出来,而不是 0。
func TestPeriodMargin_NoSnapshot_StillRealNumbers(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()
	bizLoc := time.FixedZone("Asia/Shanghai", 8*3600)
	day := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return time.Date(2026, 10, 1, 14, 15, 0, 0, bizLoc) })

	_, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-2001"})
	mustNoErr(t, err, "CreateBranchCutMapping")
	pig, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-1", GrossWeightKg: d("200"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: day,
	}, "u-1")
	mustNoErr(t, err, "RecordWholePig")
	_, err = svc.RecordPigCut(ctx, "S001", model.RecordPigCutInput{
		PigID: pig.ID, CutType: model.CutBelly, Barcode: "B-1",
		WeightKg: d("30"), ExpiresAt: day.Add(48 * time.Hour), CubeProductID: "P-2001",
	}, "u-1")
	mustNoErr(t, err, "RecordPigCut")
	_, err = svc.OnSaleCompleted(ctx, service.SaleCompletedPayload{
		SaleID: "SO-1", BranchID: "S001", CompletedAt: day.Add(2 * time.Hour),
		Lines: []service.SaleLine{
			{LineID: "L-1", SKUID: "P-2001", FreshType: "meat",
				Qty: d("10"), AmountYuan: d("340"), PigID: pig.ID, CutType: model.CutBelly},
		},
	})
	mustNoErr(t, err, "OnSaleCompleted")
	// ⚠️ **不录日终盘点** ⇒ 没有成本日快照 ⇒ 走实时算路径。

	// 前提:库里确实没有部位级成本日快照。
	var nSnap int64
	if err := testDBSingleton.Model(&model.CutCostDaily{}).
		Where("branch_id = ?", "S001").Count(&nSnap).Error; err != nil {
		t.Fatalf("count cut_cost_daily: %v", err)
	}
	if nSnap != 0 {
		t.Fatalf("前提:没录日终盘点就不该有成本日快照,got %d 行", nSnap)
	}

	daily, err := svc.ComputeGrossMargin(ctx, "S001", day)
	mustNoErr(t, err, "ComputeGrossMargin")

	// ---- 按天 ----
	period, err := svc.ComputePeriodMargin(ctx, "S001", "2026-10-01", "2026-10-01", "day")
	mustNoErr(t, err, "ComputePeriodMargin(day)")
	if period.Total.RevenueYuan.IsZero() {
		t.Errorf("★ 期间营收不该是 0(没有快照 ⇒ 必须实时算),got %s —— "+
			"返回 0 = 只读了空的快照表,门店会看到'毛利是 0'却没有任何报错", period.Total.RevenueYuan)
	}
	if period.Total.SalesCostYuan.IsZero() {
		t.Errorf("★ 期间销售成本不该是 0,got %s —— 成本没入账 = 毛利虚高", period.Total.SalesCostYuan)
	}
	// 期间 = 每日(同一引擎 ⇒ 口径一致)。
	const tol = "0.01"
	if !period.Total.RevenueYuan.Sub(daily.Total.RevenueYuan).Abs().LessThanOrEqual(d(tol)) {
		t.Errorf("期间收入 %s ≠ 单日收入 %s", period.Total.RevenueYuan, daily.Total.RevenueYuan)
	}
	if !period.Total.SalesCostYuan.Sub(daily.Total.SalesCostYuan).Abs().LessThanOrEqual(d(tol)) {
		t.Errorf("期间成本 %s ≠ 单日成本 %s", period.Total.SalesCostYuan, daily.Total.SalesCostYuan)
	}

	// ---- 按部位 ----
	byCut, err := svc.ComputePeriodMargin(ctx, "S001", "2026-10-01", "2026-10-01", "cut")
	mustNoErr(t, err, "ComputePeriodMargin(cut)")
	if byCut.Total.RevenueYuan.IsZero() {
		t.Errorf("★ 按部位的期间营收不该是 0,got %s", byCut.Total.RevenueYuan)
	}
	if byCut.Total.SalesCostYuan.IsZero() {
		t.Errorf("★ 按部位的期间销售成本不该是 0,got %s", byCut.Total.SalesCostYuan)
	}
	if len(byCut.Rows) == 0 {
		t.Errorf("★ 按部位聚合不该是空行集,got %d 行", len(byCut.Rows))
	}

	// ---- 按货号 ----
	bySku, err := svc.ComputePeriodMargin(ctx, "S001", "2026-10-01", "2026-10-01", "sku")
	mustNoErr(t, err, "ComputePeriodMargin(sku)")
	if bySku.Total.RevenueYuan.IsZero() {
		t.Errorf("★ 按货号的期间营收不该是 0,got %s", bySku.Total.RevenueYuan)
	}
	if bySku.Total.SalesCostYuan.IsZero() {
		t.Errorf("★ 按货号的期间销售成本不该是 0(生产实测过这个坑:成本恒 0 = 毛利 100%%),got %s",
			bySku.Total.SalesCostYuan)
	}
}

// TestPeriodMargin_AuditMaterializesEveryDay 锁"**审计结转**逐日物化成本快照"。
//
// # 为什么重要(2026-10-27 生产实测)
//
//	长期毛利要快 ⇒ 得有现成的日序列可读;
//	而日快照的物化点有三个候选:日终盘点 / 审计结转 / 定时任务。
//
//	**审计结转**天然就是这个点:
//	  · 审计的前置条件 = 区间内**每一天**都有完整的日终盘点
//	    ⇒ 审计过的区间,**每一天**都会落 `cut_cost_daily`
//	  · 审计结转本来就是"这一天算完了"的时点(门店主动做的动作)
//	  · 审计后该区间被**冻结** ⇒ 不用反复重算
//
// ⇒ 有审计的店:长期毛利直接读日序列 ⇒ 秒回。
// ⇒ 没审计的店:走实时算兜底(见上一条用例)⇒ 正确但慢。
func TestPeriodMargin_AuditMaterializesEveryDay(t *testing.T) {
	svc, ctx, sku, _ := fmPresaleSetup(t)
	svc.SetCubeClient(&fmFakeCube{prices: map[string]fmFakePrice{
		sku: {retail: "150"},
	}})

	// 审计前:区间内还没有成本日快照。
	var nBefore int64
	if err := testDBSingleton.Model(&model.CutCostDaily{}).
		Where("branch_id = ?", "S001").Count(&nBefore).Error; err != nil {
		t.Fatalf("count cut_cost_daily(before): %v", err)
	}

	// 审计结转(10-02):区间内两天都有完整日终盘点 ⇒ 审计通过。
	if _, err := svc.AuditBusinessDays(ctx, "S001",
		service.BizAuditInput{CutoffDate: "2026-10-02"}, "u-1"); err != nil {
		t.Fatalf("AuditBusinessDays: %v", err)
	}

	// ★ 审计后:10-01 与 10-02 **每一天**都有部位级成本日快照。
	for _, day := range []string{"2026-10-01", "2026-10-02"} {
		var rows []model.CutCostDaily
		if err := testDBSingleton.Where("branch_id = ? AND biz_date LIKE ?", "S001", day+"%").
			Find(&rows).Error; err != nil {
			t.Fatalf("query cut_cost_daily(%s): %v", day, err)
		}
		if len(rows) == 0 {
			t.Errorf("★ 审计结转后 %s 应有部位级成本日快照(审计的逐日物化),got 0 行", day)
			continue
		}
		// 物理事实(分割产出/期初/期末)必须非零 ⇒ 快照不是空壳。
		for _, r := range rows {
			if r.ProducedKg.IsZero() && r.OpeningKg.IsZero() && r.ClosingKg.IsZero() {
				t.Errorf("★ %s 的快照是空壳(期初/进货/期末全是 0)—— 快照为空 = 长期毛利要现算(慢)",
					day)
			}
		}
	}

	// 有了日序列 ⇒ 长期毛利按天聚合出两行。
	period, err := svc.ComputePeriodMargin(ctx, "S001", "2026-10-01", "2026-10-02", "day")
	mustNoErr(t, err, "ComputePeriodMargin(day)")
	if len(period.Rows) != 2 {
		t.Errorf("★ 审计过的区间按天聚合应是 2 行,got %d", len(period.Rows))
	}

	// ★ 不变量:**长期毛利 = 每日毛利之和**。
	// (这是"长期"这一层的全部意义;口径一旦分叉,长期数就不能信。)
	d1, err := svc.ComputeGrossMargin(ctx, "S001", fmDay1)
	mustNoErr(t, err, "ComputeGrossMargin(10-01)")
	d2, err := svc.ComputeGrossMargin(ctx, "S001", fmDay2)
	mustNoErr(t, err, "ComputeGrossMargin(10-02)")
	sumRevenue := d1.Total.RevenueYuan.Add(d2.Total.RevenueYuan)
	sumCost := d1.Total.SalesCostYuan.Add(d2.Total.SalesCostYuan)
	sumGM := d1.Total.GrossMarginYuan.Add(d2.Total.GrossMarginYuan)
	const tol = "0.01"
	if !period.Total.RevenueYuan.Sub(sumRevenue).Abs().LessThanOrEqual(d(tol)) {
		t.Errorf("★ 期间收入 %s ≠ 每日之和 %s", period.Total.RevenueYuan, sumRevenue)
	}
	if !period.Total.SalesCostYuan.Sub(sumCost).Abs().LessThanOrEqual(d(tol)) {
		t.Errorf("★ 期间成本 %s ≠ 每日之和 %s", period.Total.SalesCostYuan, sumCost)
	}
	if !period.Total.GrossMarginYuan.Sub(sumGM).Abs().LessThanOrEqual(d(tol)) {
		t.Errorf("★ 期间毛利 %s ≠ 每日之和 %s", period.Total.GrossMarginYuan, sumGM)
	}
}
