package service_test

// 2026-11-02:单价基线**物化表**的自动刷新(owner 拍板「采集完自动重算」)。
//
// # 锁的是什么
//
//	「采集完自动重算单价基线物化表」
//	⇒ 门店界面上看到的"部位进货单价基线"的样本数/单价,
//	  必须**当场**跟上样本库,不用再手动点「重算」。
//
// # 为什么要专门锁它
//
//	⚠️ 2026-11-02 owner 生产实测:录完 11-01 的猪 ⇒ 分割基线样本数已经是 5,
//	  而"部位进货单价基线"的样本数还是 1
//	  ⇒ **采集跑了、重算没跑** ⇒ 差异**不是**显示问题,是物化表压根没被刷新。
//
//	⚠️ 而这一步缺失**不会让任何看板数字变化** ⇒ 看板一切正常
//	  ⇒ 不专门锁它,它会一直缺着(而且没人看得出来)。
//
// # 判别力
//
//	去掉自动重算(采集/删除都不触发)⇒ 本文件的用例立刻红:
//	  - 采集后物化表**没有行**;
//	  - 删除后物化表的样本数**不变**。

import (
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

// TestPrice_CollectAutoRecomputesMaterializedBaseline 锁「采集完**自动重算**物化表」。
//
// ⚠️ 这里**故意不调** RecomputeCutPriceBaselines ⇒ 物化表必须**自己**已经是新的。
//
// 造数:猪 146kg @ 10 元(均价 10);营收 360;期末台面实盘 122kg
// ⇒ m = (360 − 1460 + 122×10) ÷ 360 = 120 ÷ 360 = 1/3
// ⇒ 零售均价 = 360 ÷ 30 = 12 ⇒ 倒推进货单价 = 12 ÷ (1+1/3) = **9 元/kg**。
func TestPrice_CollectAutoRecomputesMaterializedBaseline(t *testing.T) {
	svc, branch := priceSetup(t)
	pricePig(t, "PIG-1", "146", "10", priceDay)

	rows := []service.CutDailyCost{{
		CutType:         model.CutBelly,
		SoldKg:          d("30"),
		RevenueYuan:     d("360"),
		SalesCostYuan:   d("200"),
		DataSource:      "actual",
		CounterActualKg: d("122"),
	}}
	res, err := service.CollectCutPriceSamplesForTest(t.Context(), svc, branch, priceDay, rows)
	mustNoErr(t, err, "CollectCutPriceSamples")
	if res.Written != 1 {
		t.Fatalf("★ 应写入 1 条单价样本,got %d(%s)", res.Written, res.Message)
	}
	if res.BaselineError != "" {
		t.Fatalf("★ 自动重算不该失败:%s", res.BaselineError)
	}
	// ⚠️ **不手动点重算** ⇒ 物化表该自己已经是新的。
	bl, err := svc.ListForecastPriceBaselines(t.Context(), branch)
	mustNoErr(t, err, "ListForecastPriceBaselines")
	if len(bl) != 1 {
		t.Fatalf("★ 采集完应自动重算出 1 条单价基线(不用手动点重算),got %d", len(bl))
	}
	if bl[0].SampleCount != 1 {
		t.Errorf("★ 基线样本数应是 1,got %d", bl[0].SampleCount)
	}
	if !priceEq(bl[0].UnitCostYuan, d("9")) {
		t.Errorf("★ 基线单价应是 9(12 ÷ (1+1/3)),got %s", bl[0].UnitCostYuan)
	}
	// res.Baselines 是"自动重算真的跑过"的可见证据 ⇒ 门店不用猜。
	if res.Baselines != 1 {
		t.Errorf("★ result.Baselines 应是 1,got %d", res.Baselines)
	}
}

// TestPrice_DeleteSampleAutoRecomputesMaterializedBaseline 锁「删掉样本 ⇒ 基线当场跟着变」。
//
// ⚠️ 墓碑**不参与拟合** ⇒ 删掉一条样本,那个部位的样本数就该 -1、单价要重算。
//   门店删了不准确的样本却看不到变化 ⇒ 会以为删除没生效。
//
// 造数:10-28 卖 10kg@7;10-29 卖 30kg@9 ⇒ 基线 = (7×10 + 9×30) ÷ 40 = **8.5**(n=2)
// 删掉 10-28 那条 ⇒ 只剩 10-29 ⇒ 基线 = **9**(n=1)。
func TestPrice_DeleteSampleAutoRecomputesMaterializedBaseline(t *testing.T) {
	svc, branch := priceSetup(t)
	pricePig(t, "PIG-1", "146", "10", priceDay)

	// 先造两条样本(直接插库:省掉"先跑引擎"的麻烦)。
	priceSample(t, "2026-10-28", model.CutBelly, "10", "100", "7", "14.6")
	// 再让引擎路径采一次 10-29 ⇒ 顺手把基线物化表刷新出来。
	rows := []service.CutDailyCost{{
		CutType:         model.CutBelly,
		SoldKg:          d("30"),
		RevenueYuan:     d("360"),
		SalesCostYuan:   d("200"),
		DataSource:      "actual",
		CounterActualKg: d("122"),
	}}
	res, err := service.CollectCutPriceSamplesForTest(t.Context(), svc, branch, priceDay, rows)
	mustNoErr(t, err, "CollectCutPriceSamples")
	if res.Written != 1 {
		t.Fatalf("★ 应写入 1 条单价样本,got %d(%s)", res.Written, res.Message)
	}

	bl, err := svc.ListForecastPriceBaselines(t.Context(), branch)
	mustNoErr(t, err, "ListForecastPriceBaselines")
	if len(bl) != 1 {
		t.Fatalf("★ 应有 1 条单价基线,got %d", len(bl))
	}
	if bl[0].SampleCount != 2 {
		t.Fatalf("★ 基线样本数应是 2,got %d", bl[0].SampleCount)
	}
	if !priceEq(bl[0].UnitCostYuan, d("8.5")) {
		t.Fatalf("★ 基线单价应是 8.5((7×10+9×30)/40),got %s", bl[0].UnitCostYuan)
	}

	// ---- 删掉 10-28 那条 ⇒ 基线必须当场跟着变 ----
	n, err := svc.DeleteCutPriceSample(t.Context(), branch, "2026-10-28", model.CutBelly)
	mustNoErr(t, err, "DeleteCutPriceSample")
	if n != 1 {
		t.Fatalf("★ 应删掉 1 条,got %d", n)
	}
	bl2, err := svc.ListForecastPriceBaselines(t.Context(), branch)
	mustNoErr(t, err, "ListForecastPriceBaselines")
	if len(bl2) != 1 {
		t.Fatalf("★ 删掉样本后基线仍应是 1 条,got %d", len(bl2))
	}
	if bl2[0].SampleCount != 1 {
		t.Errorf("★ 删掉一条后基线样本数应是 1(不用手动点重算),got %d", bl2[0].SampleCount)
	}
	if !priceEq(bl2[0].UnitCostYuan, d("9")) {
		t.Errorf("★ 删掉 10-28 那条后基线单价应是 9,got %s", bl2[0].UnitCostYuan)
	}
}

// TestPrice_EnginePathAutoRecomputesMaterializedBaseline 锁「**引擎自动路径**也会刷新物化表」。
//
// ⚠️ 这才是 owner 的真实场景:录猪 → 引擎跑 → 采集样本 ⇒ 界面上基线要跟着变。
//   (之前缺的正是这一段:采集有跑,但物化表没人重算。)
//
// 造数:猪 200kg @ 20 元(均价 20);卖 belly 30kg 得 900 元;台面实盘 170kg
// ⇒ m = (900 − 4000 + 170×20) ÷ 900 = 300 ÷ 900 = 1/3
// ⇒ 零售均价 = 900 ÷ 30 = 30 ⇒ 倒推进货单价 = 30 ÷ (1+1/3) = **22.5 元/kg**。
func TestPrice_EnginePathAutoRecomputesMaterializedBaseline(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	estPig(t, svc, ctx, "S001", "EB-AUTO", "200", splitDay)
	estStocktake(t, svc, ctx, "S001", sku, "170", "")
	estSell(t, svc, "S001", sku, "L-AUTO-1", "30", "900", splitDay.Add(time.Hour))

	// 引擎跑那一天 ⇒ 采集样本 ⇒ 顺带刷新基线物化表。
	if _, err := svc.PersistCutCostDaily(ctx, "S001", splitDay); err != nil {
		t.Fatalf("PersistCutCostDaily: %v", err)
	}

	bl, err := svc.ListForecastPriceBaselines(t.Context(), "S001")
	mustNoErr(t, err, "ListForecastPriceBaselines")
	if len(bl) == 0 {
		t.Fatalf("★ 引擎跑完应自动刷新单价基线物化表(不用手动点重算),got 0 条")
	}
	var belly *model.PigCutPriceBaseline
	for i := range bl {
		if bl[i].CutType == model.CutBelly {
			belly = &bl[i]
			break
		}
	}
	if belly == nil {
		t.Fatalf("★ 应有 belly 的单价基线,got %v", bl)
	}
	if belly.SampleCount != 1 {
		t.Errorf("★ belly 基线样本数应是 1,got %d", belly.SampleCount)
	}
	if !priceEq(belly.UnitCostYuan, d("22.5")) {
		t.Errorf("★ belly 基线单价应是 22.5(30 ÷ (1+1/3)),got %s", belly.UnitCostYuan)
	}
}
