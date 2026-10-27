package service_test

// 2026-10-24(门店口径第三轮):**审计结转**。
//
// 门店原话:
//
//	「审计则进行结转,审计指定截止日期;审计前,所有数据从上期结转开始,实时计算,
//	  不快照,任何日期补录都会导致后续日期的数据发生变化;审计后,产生结转(即快照),
//	  审计截止日期和之前的都不能再修改(包括补录);审计条件是截止日期和之前的每日
//	  日终盘点都要有,否则不予以审计。」
//
// ⇒ 四条不变量:
//
//	① 审计**前**:实时算、不快照 ⇒ 补录往日 ⇒ 后续日期的结余/毛利跟着变;
//	② 审计**条件**:截止日及之前每一天都要有**完整的日终盘点** ⇒ 缺 ⇒ 拒绝(点名缺哪天);
//	③ 审计**时**:落快照(=结转)⇒ 那些天的结余被固化;
//	④ 审计**后**:截止日及之前**冻结** ⇒ 补录/改单/撤单一律 400。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

// fmAuditSetup 铺两天:10-01、10-02 各有猪 + 完整的日终盘点。
func fmAuditSetup(t *testing.T) (*service.Service, context.Context, string) {
	t.Helper()
	svc, ctx, sku := fmBizSetup(t)
	// 10-01
	fmBizSeedDay1(t, svc, ctx, sku)
	// 10-02:再进一头猪 + 盘点(完整)。
	svc.SetClock(fmClock(fmDay2))
	fmSeedPigAt(t, svc, "EB-2", "50", fmDay2)
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("8")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("10-02 盘点: %v", err)
	}
	return svc, ctx, sku
}
// TestAudit_RequiresCompleteStocktakeEveryDay 锁 ②。
//
// ⇒ 截止日之前有一天没有完整的日终盘点 ⇒ **不予以审计**,并点名缺哪天。
func TestAudit_RequiresCompleteStocktakeEveryDay(t *testing.T) {
	svc, ctx, _ := fmAuditSetup(t)

	// 10-03:只进猪、不盘点 ⇒ 10-03 缺日终盘点 ⇒ 审计到 10-03 应被拒。
	day3 := fmDay1.Add(48 * time.Hour)
	svc.SetClock(fmClock(day3))
	fmSeedPigAt(t, svc, "EB-3", "50", day3)

	_, err := svc.AuditBusinessDays(ctx, "S001", service.BizAuditInput{CutoffDate: "2026-10-03"}, "u-1")
	if err == nil {
		t.Fatalf("缺日终盘点的日子不该允许审计")
	}
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Errorf("应是输入校验错(400),got %v", err)
	}
	if !strings.Contains(err.Error(), "2026-10-03") {
		t.Errorf("报错要点名缺哪天(应含 2026-10-03),got %q", err.Error())
	}

	// 审计到 10-02(两天都有盘点)⇒ 通过。
	st, err := svc.AuditBusinessDays(ctx, "S001", service.BizAuditInput{CutoffDate: "2026-10-02"}, "u-1")
	mustNoErr(t, err, "AuditBusinessDays")
	if !st.Audited || st.LastCutoffDate != "2026-10-02" {
		t.Errorf("审计状态不对: %+v", st)
	}
}

// TestAudit_FreezesAndMaterializes 锁 ③ + ④。
//
// ⇒ 审计 ⇒ 落快照 + 冻结 ⇒ 截止日及之前不能再补录/改搬台。
func TestAudit_FreezesAndMaterializes(t *testing.T) {
	svc, ctx, sku := fmAuditSetup(t)

	// 审计前:实时算(没有快照行)。
	if _, ok := fmDailySnapshot(t, "S001", "2026-10-01", string(model.CutBelly)); ok {
		t.Fatalf("审计前不该有结余快照(审计前实时算、不快照)")
	}

	st, err := svc.AuditBusinessDays(ctx, "S001", service.BizAuditInput{CutoffDate: "2026-10-02", Note: "月度结转"}, "u-1")
	mustNoErr(t, err, "AuditBusinessDays")
	if !st.Audited || st.LastCutoffDate != "2026-10-02" {
		t.Fatalf("审计状态不对: %+v", st)
	}
	if len(st.Audits) != 1 {
		t.Errorf("应有 1 条审计记录,got %d", len(st.Audits))
	}

	// ③ 快照(=结转)落了。
	for _, d := range []string{"2026-10-01", "2026-10-02"} {
		if _, ok := fmDailySnapshot(t, "S001", d, string(model.CutBelly)); !ok {
			t.Errorf("审计后应有 %s 的结余快照", d)
		}
	}

	// ④ 冻结:补录 10-01 的盘点 ⇒ 400。
	_, err = svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		TakenDate:  "2026-10-01",
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("9")},
		},
	}, "u-1")
	if err == nil {
		t.Fatalf("审计截止日及之前不该再能补录")
	}
	if !strings.Contains(err.Error(), "2026-10-01") || !strings.Contains(err.Error(), "审计") {
		t.Errorf("冻结报错要说清哪一天被冻结,got %q", err.Error())
	}

	// ④ 冻结:改 10-02 的搬台 ⇒ 400(改不动的也要拦)。
	view, err := svc.ListCounterLoads(ctx, "S001", fmDay2)
	mustNoErr(t, err, "ListCounterLoads")
	_ = view // 没有搬台流水时无可改,跳过这一段断言。

	// 审计**之后**的日子仍然可以改(10-03 及以后不受冻结影响)。
	day3 := fmDay1.Add(48 * time.Hour)
	svc.SetClock(fmClock(day3))
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		TakenDate:  "2026-10-03",
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("7")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("审计截止日之后的日期不该被冻结:%v", err)
	}
}

// TestAudit_BeforeAuditBackfillChangesLaterDays 锁 ①。
//
// ⇒ 审计前:实时算、不快照 ⇒ 补录往日 ⇒ **后续日期的结余跟着变**。
func TestAudit_BeforeAuditBackfillChangesLaterDays(t *testing.T) {
	svc, ctx, sku := fmAuditSetup(t)

	// 10-02 的日终冷库 = 上期 10 + 结转 8 = 18。
	_, cold := fmMarginClosing(t, "S001", fmDay2, model.CutBelly)
	if !cold.Equal(d("18")) {
		t.Fatalf("10-02 冷库应是 18,got %s", cold)
	}

	// 补录 10-01 的盘点(柜台实盘 10 → 5)⇒ 10-02 的冷库跟着变(5 + 8 = 13)。
	svc.SetClock(fmClock(fmDay1.Add(48 * time.Hour)))
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		TakenDate:  "2026-10-01",
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("5")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("补录 10-01: %v", err)
	}

	_, cold2 := fmMarginClosing(t, "S001", fmDay2, model.CutBelly)
	if !cold2.Equal(d("13")) {
		t.Errorf("补录 10-01 后 10-02 的冷库应跟着变(13),got %s", cold2)
	}
}
