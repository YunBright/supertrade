package service_test

// 2026-10-24(门店口径第四轮补充):**预售价的人工修改** + **审计幂等**。
//
// 门店口径:
// 「取不到也按0生成，会经过一轮人工修改，门店审计人负责。」
//
// ⇒ 审计时取不到售价的货号按 0 元结转 ⇒ 审计人随后改成真实价;
//   改完要重算:本期毛利 / 按猪分摊 / 下一期期初单价 / 当前余额。
//
// ⇒ 并且:同一个截止日重复审计 ⇒ **幂等**(直接返回已完成的结转结果),
//   门店在手机上点两次不该看到"失败"。

import (
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

// TestUpdatePresale_ChangesPriceAndRederives 锁"改预售价 ⇒ 重算 + 留痕"。
func TestUpdatePresale_ChangesPriceAndRederives(t *testing.T) {
	svc, ctx, sku, _ := fmPresaleSetup(t)
	// 审计时取不到价 ⇒ 按 0 结转(毛利 = −2000)。
	svc.SetCubeClient(&fmFakeCube{prices: map[string]fmFakePrice{}})
	if _, err := svc.AuditBusinessDays(ctx, "S001", service.BizAuditInput{CutoffDate: "2026-10-02"}, "u-1"); err != nil {
		t.Fatalf("审计: %v", err)
	}

	// 审计人改成真实价 60 元/kg(门店口径:「经过一轮人工修改」)。
	updated, err := svc.UpdatePresale(ctx, "S001", service.UpdatePresaleInput{
		BizDate: "2026-10-02",
		Items: []service.UpdatePresaleItem{
			{CubeProductID: sku, ExpectedUnitYuan: d("60")},
		},
		Note: "思迅已维护零售价 60",
	}, "auditor-1")
	mustNoErr(t, err, "UpdatePresale")
	if updated == nil || len(updated.Rows) == 0 {
		t.Fatalf("改完应返回完整预售视图: %+v", updated)
	}
	row := updated.Rows[0].Skus[0]
	if !row.ExpectedUnitYuan.Equal(d("60")) {
		t.Errorf("预期单价应改成 60,got %s", row.ExpectedUnitYuan)
	}
	if !row.ExpectedRevenueYuan.Equal(d("1200")) {
		t.Errorf("预期收入应重算成 20kg × 60 = 1200,got %s", row.ExpectedRevenueYuan)
	}
	if !row.MarginYuan.Equal(d("-800")) {
		t.Errorf("预售毛利应重算成 1200 − 2000 = −800,got %s", row.MarginYuan)
	}
	if row.PriceSource != service.PresalePriceSourceManual {
		t.Errorf("改过的单价来源应是 manual,got %q", row.PriceSource)
	}
	// 本期毛利跟着重算。
	rep, err := svc.ComputeGrossMargin(ctx, "S001", fmDay2)
	mustNoErr(t, err, "ComputeGrossMargin")
	if !rep.Total.GrossMarginYuan.Equal(d("-800")) {
		t.Errorf("本期毛利应重算成 −800,got %s", rep.Total.GrossMarginYuan)
	}
	// 留痕。
	var edits []model.CutPresaleEdit
	if err := testDBSingleton.Where("branch_id = ?", "S001").Find(&edits).Error; err != nil {
		t.Fatalf("查 cut_presale_edits: %v", err)
	}
	if len(edits) != 1 {
		t.Fatalf("应有 1 条修改痕迹,got %d", len(edits))
	}
	if !edits[0].OldUnitYuan.IsZero() {
		t.Errorf("痕迹里的旧价应是 0,got %s", edits[0].OldUnitYuan)
	}
	if !edits[0].NewUnitYuan.Equal(d("60")) {
		t.Errorf("痕迹里的新价应是 60,got %s", edits[0].NewUnitYuan)
	}
	if edits[0].OperatorID != "auditor-1" {
		t.Errorf("痕迹应记下是谁改的,got %q", edits[0].OperatorID)
	}
	// 下一期期初跟着改(成本口径 2000 + 重估(60 − 100)×20 = 1200)。
	svc.SetClock(fmClock(fmDay2.Add(24 * time.Hour)))
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		TakenDate:  "2026-10-03",
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("20")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("10-03 盘点: %v", err)
	}
	rep3, err := svc.ComputeGrossMargin(ctx, "S001", fmDay2.Add(24*time.Hour))
	mustNoErr(t, err, "ComputeGrossMargin(10-03)")
	for _, r := range rep3.PerSku {
		if r.CubeProductID != sku {
			continue
		}
		if !r.OpeningAmount.Equal(d("1200")) {
			t.Errorf("改价后 10-03 期初应是 2000 + (60−100)×20 = 1200,got %s", r.OpeningAmount)
		}
	}
}

// TestUpdatePresale_RejectsWrongDayOrSku 锁"只能改已审计那天的、且要改得动"。
func TestUpdatePresale_RejectsWrongDayOrSku(t *testing.T) {
	svc, ctx, sku, _ := fmPresaleSetup(t)
	svc.SetCubeClient(&fmFakeCube{prices: map[string]fmFakePrice{
		sku: {retail: "150"},
	}})
	if _, err := svc.AuditBusinessDays(ctx, "S001", service.BizAuditInput{CutoffDate: "2026-10-02"}, "u-1"); err != nil {
		t.Fatalf("审计: %v", err)
	}
	// 改一个没有预售行的货号 ⇒ 400。
	if _, err := svc.UpdatePresale(ctx, "S001", service.UpdatePresaleInput{
		BizDate: "2026-10-02",
		Items: []service.UpdatePresaleItem{
			{CubeProductID: "P-NOT-EXIST", ExpectedUnitYuan: d("60")},
		},
	}, "auditor-1"); err == nil {
		t.Errorf("改不存在的货号应被拒")
	}
	// 改一个没审计过的日期 ⇒ 400。
	if _, err := svc.UpdatePresale(ctx, "S001", service.UpdatePresaleInput{
		BizDate: "2026-10-01",
		Items: []service.UpdatePresaleItem{
			{CubeProductID: sku, ExpectedUnitYuan: d("60")},
		},
	}, "auditor-1"); err == nil {
		t.Errorf("改没审计过的日期应被拒")
	}
	// 负单价 ⇒ 400。
	if _, err := svc.UpdatePresale(ctx, "S001", service.UpdatePresaleInput{
		BizDate: "2026-10-02",
		Items: []service.UpdatePresaleItem{
			{CubeProductID: sku, ExpectedUnitYuan: d("-1")},
		},
	}, "auditor-1"); err == nil {
		t.Errorf("负单价应被拒")
	}
}

// TestAudit_IsIdempotentForSameCutoff 锁"同一个截止日重复审计 ⇒ 幂等"。
//
// ⇒ 门店在手机上点两次很常见 ⇒ 第二次不该报"失败"。
func TestAudit_IsIdempotentForSameCutoff(t *testing.T) {
	svc, ctx, sku, _ := fmPresaleSetup(t)
	svc.SetCubeClient(&fmFakeCube{prices: map[string]fmFakePrice{
		sku: {retail: "150"},
	}})

	first, err := svc.AuditBusinessDays(ctx, "S001", service.BizAuditInput{CutoffDate: "2026-10-02"}, "u-1")
	mustNoErr(t, err, "第一次审计")
	if !first.Audited {
		t.Fatalf("第一次审计应成功: %+v", first)
	}
	second, err := svc.AuditBusinessDays(ctx, "S001", service.BizAuditInput{CutoffDate: "2026-10-02"}, "u-1")
	mustNoErr(t, err, "重复审计(幂等)")
	if !second.Audited {
		t.Errorf("重复审计应返回已完成的状态,got %+v", second)
	}
	if second.LastCutoffDate != first.LastCutoffDate {
		t.Errorf("重复审计的截止日应不变: %s vs %s", second.LastCutoffDate, first.LastCutoffDate)
	}
	// 审计记录仍然只有一条。
	var audits []model.BizAudit
	if err := testDBSingleton.Where("branch_id = ?", "S001").Find(&audits).Error; err != nil {
		t.Fatalf("查 biz_audits: %v", err)
	}
	if len(audits) != 1 {
		t.Errorf("审计记录应只有 1 条,got %d", len(audits))
	}
}
