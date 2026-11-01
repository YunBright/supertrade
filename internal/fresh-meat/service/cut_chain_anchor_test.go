package service_test

// 成本链「**锚点快路径**」的判别力测试（2026-11-01）。
//
// # 为什么要有这个文件
//
// 生产实测（店 00）：毛利看板耗时随日期**线性增长**
//
//	10-28 ≈450ms / 10-29 ≈780ms / 10-30 ≈1.2s / 10-31 ≈1.6s / 11-01 ≈2.0s / 11-05 ≈3.3s
//
// 而**区间长度不影响**（10-28..10-31 四天 1.46s，比 10-31..11-01 两天 1.96s 还快）
// ⇒ 说明"每往后一天就多一次远程销售查询"，不是"区间里有几天就查几次"。
//
// 根因：`cutChainAnchor` 用 `Select("biz_date")` 查锚点、却用 `latest.ID == ""`
// 判断"到底查到没查到行" ⇒ **ID 从未被查出来** ⇒ 锚点恒为"不存在"
// ⇒ `computeCutChainDays` 每次都从链起点**逐日重算**整条链。
//
// # 为什么必须单独锁
//
// ⚠️ 这个 bug **数字全对、只是慢** ⇒ 端到端断言"数字对"永远抓不到它
// ⇒ 只能**直接锁锚点是否被找到**。
//
// ⚠️ 反过来也有一半要锁：**锚点路径算出来的数必须和回溯路径一样**
// ⇒ 否则"用快照接续"就会把数字算歪（生产实测过：断链的锚点让成本从 8.4 万变 19.4 万）。

import (
	"context"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

// anchorScenario 造一条 4 天链（10-28 期初结转 → 10-31）。
//
// materialize = true ⇒ 把整条链物化（落 cut_cost_daily）⇒ 之后查后面的日子
// **可以**走锚点快路径；false ⇒ 没有快照 ⇒ 只能从链起点逐日重算（回溯路径）。
//
// ⚠️ 两个场景的数据必须**完全一样** ⇒ 数字才可比（这是"锚点 vs 回溯"的前提）。
func anchorScenario(t *testing.T, materialize bool) (*service.Service, context.Context, time.Time) {
	t.Helper()
	svc, ctx := ssSetup(t)

	// ① 期初结转 = 链起点（10-28）⇒ 给 20kg，让这一天**有快照行**
	//    （锚点的连续性校验要求"基线 ~ 锚点日每一天都有快照"）。
	svc.SetClock(fmClock(ssDay1))
	ssOpening(t, svc, ssBaseDay, model.CutBelly, "20", "16")

	// ② 10-29：进猪切 20kg + 日终盘点 40kg（20 期初 + 20 进货）。
	pigID := ssSeedPig(t, svc, ssDay1, "EB-1", map[model.CutType]string{model.CutBelly: "20"})
	ssStocktake(t, svc, ssDay1, map[model.CutType]string{model.CutBelly: "40"})

	// ③ 10-30 / 10-31：卖货（让链真的动起来）。
	svc.SetClock(fmClock(ssDay2))
	ssSell(t, svc, ssDay2, pigID, model.CutBelly, "10", "300")
	svc.SetClock(fmClock(ssDay3))
	ssSell(t, svc, ssDay3, pigID, model.CutBelly, "5", "150")

	if materialize {
		// 物化 10-28 → 今天（时钟在 10-31）⇒ 四天都有快照。
		if _, err := svc.MaterializeCostChain(ctx, "S001", "2026-10-28"); err != nil {
			t.Fatalf("MaterializeCostChain: %v", err)
		}
	}
	return svc, ctx, ssDay3
}

// TestCutChainAnchor_FoundWhenSnapshotExists 锁「**锚点必须被找到**」。
//
// # 判别力
//
// 把 `Select("id", "biz_date")` 改回 `Select("biz_date")` ⇒ 本用例立刻红
// （锚点被判成"不存在" ⇒ 看板每次从链起点逐日重算 ⇒ 耗时线性增长）。
//
// ⚠️ 注意：这条断言**不涉及任何数字** ⇒ 它锁的是"快路径有没有被走到"，
// 而快路径失效时数字仍然全对 ⇒ 只有这样才抓得住。
func TestCutChainAnchor_FoundWhenSnapshotExists(t *testing.T) {
	svc, ctx, day3 := anchorScenario(t, true)

	day, found, err := service.CutChainAnchorForTest(ctx, svc, "S001", day3)
	mustNoErr(t, err, "CutChainAnchorForTest")
	if !found {
		t.Fatalf("★ 10-31 之前有连续快照(10-28/10-29/10-30)⇒ 锚点必须被找到"+
			"(找不到 ⇒ 看板每次从链起点逐日重算 ⇒ 耗时随日期线性增长)")
	}
	if day != "2026-10-30" {
		t.Errorf("★ 锚点应是 10-30(10-31 之前最近的一个快照日), got %s", day)
	}

	// 链起点 = 期初结转的生效日（10-28）⇒ 兜底的"最早快照日"分支用不上。
	base, hasBase, err := service.CutChainBaselineForTest(ctx, svc, "S001")
	mustNoErr(t, err, "CutChainBaselineForTest")
	if !hasBase || base != "2026-10-28" {
		t.Errorf("★ 链起点应是期初结转的生效日 2026-10-28, got %q(has=%v)", base, hasBase)
	}
}

// TestCutChainAnchor_SameNumbersAsBacktrack 锁「**锚点路径的数 == 回溯路径的数**」。
//
// # 为什么要有
//
// 锚点是**用快照接续**的 ⇒ 一旦快照与当前引擎口径不一致，
// 看板上的数就会悄悄变（生产实测过：断链的锚点让成本从 8.4 万变 19.4 万）。
//
// ⇒ 用**两个完全一样的场景**（一个有快照、一个没有）比同一天的报表：
// 数字必须**逐部位相等**。
//
// # 判别力
//
// ① 物化时若用了不同的口径 ⇒ 两条路径的数会不同 ⇒ 本用例红；
// ② 锚点读错行（比如把锚点当天当成期初）⇒ 期初/期末立刻不同 ⇒ 红。
func TestCutChainAnchor_SameNumbersAsBacktrack(t *testing.T) {
	svcSnap, ctxSnap, day3 := anchorScenario(t, true)
	repSnap, err := svcSnap.ComputeGrossMargin(ctxSnap, "S001", day3)
	mustNoErr(t, err, "ComputeGrossMargin(有快照 ⇒ 走锚点)")

	svcBack, ctxBack, _ := anchorScenario(t, false)
	repBack, err := svcBack.ComputeGrossMargin(ctxBack, "S001", day3)
	mustNoErr(t, err, "ComputeGrossMargin(无快照 ⇒ 回溯重算)")

	if len(repSnap.PerCut) == 0 {
		t.Fatal("★ 报表不该没有部位行")
	}
	if len(repSnap.PerCut) != len(repBack.PerCut) {
		t.Fatalf("★ 两条路径的部位行数应相同: 锚点 %d 行 / 回溯 %d 行",
			len(repSnap.PerCut), len(repBack.PerCut))
	}
	for i := range repSnap.PerCut {
		a := repSnap.PerCut[i]
		b := findPerCut(t, repBack.PerCut, a.CutType)
		if !a.OpeningKg.Equal(b.OpeningKg) {
			t.Errorf("★ %s 期初: 锚点 %s vs 回溯 %s(必须一致)",
				a.CutType, a.OpeningKg, b.OpeningKg)
		}
		if !a.ExpectedRemainKg.Equal(b.ExpectedRemainKg) {
			t.Errorf("★ %s 账面应有: 锚点 %s vs 回溯 %s(必须一致)",
				a.CutType, a.ExpectedRemainKg, b.ExpectedRemainKg)
		}
		if !a.RevenueYuan.Equal(b.RevenueYuan) {
			t.Errorf("★ %s 营收: 锚点 %s vs 回溯 %s(必须一致)",
				a.CutType, a.RevenueYuan, b.RevenueYuan)
		}
	}
	if !repSnap.Total.RevenueYuan.Equal(repBack.Total.RevenueYuan) {
		t.Errorf("★ 全店营收: 锚点 %s vs 回溯 %s(必须一致)",
			repSnap.Total.RevenueYuan, repBack.Total.RevenueYuan)
	}
	if !repSnap.Total.SalesCostYuan.Equal(repBack.Total.SalesCostYuan) {
		t.Errorf("★ 全店销售成本: 锚点 %s vs 回溯 %s(必须一致)",
			repSnap.Total.SalesCostYuan, repBack.Total.SalesCostYuan)
	}
}
