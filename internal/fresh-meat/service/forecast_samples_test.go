package service_test

// 2026-10-30:**样本库**（owner 重新定义模型：样本库是一等公民，基线是派生结果）。
//
// # 锁的是什么（口径，不是实现细节）
//
//	① **加入样本**（按一天）= 跑那天的引擎落估计样本 ⇒ **不自动重算**；
//	② **删除样本**（按天+体重段整组 / 按单头猪）= **打墓碑**，物理删会被引擎重跑复活；
//	③ **重算** = 从样本库全量拟合 ⇒ **覆盖所有分层**（不再跳过 manual_generated）；
//	④ **可用门槛 = n ≥ 1**（有样本就算用）⇒ 同段两头猪 = n=2；
//	⑤ **合并观察**（实录 ∪ 估计，按 (猪,部位) 去重、实录优先）⇒ sample_source 会出现 mixed；
//	⑥ 样本被删光 ⇒ **该层基线行随之消失**（否则脏数字一直给出错建议）。
//
// ⚠ 这些用例都走**公开 API** + 真实 SQLite ⇒ 引擎接线错了也会红。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/shopspring/decimal"
	"gorm.io/datatypes"
)

// fmSeedDerivedDay 铺一个"引擎能倒推出分割重量"的一天 ⇒ 落下估计样本。
//
// # ⚠️ 为什么先 seed 一条**可用的**基线
//
//	单测里 LLM 兜底不出数 ⇒ 建议分割为 0 ⇒ 部位会被显示规则隐藏
//	⇒ 倒推值算不出来 ⇒ 样本为 0（用例测不出任何东西 ⇒ 假绿）。
//
// # ⚠️ 为什么还要铺**搬台 + 销售**
//
//	「加入样本」的前置校验要求那一天**四项齐全**
//	（不然门店点一下就被"缺:搬台"挡回去 ⇒ 用例根本走不到引擎）。
//
// 返回当天进场的猪 ID。
func fmSeedDerivedDay(t *testing.T, svc *service.Service, ctx context.Context, sku string,
	grossKg ...string) []string {
	t.Helper()
	// 一条可用的基线（让建议分割非 0）。
	estBaselineSeed(t, "S001", model.CutBelly, "200-249", "20")
	pigs := make([]string, 0, len(grossKg))
	for i, g := range grossKg {
		pigs = append(pigs, estPig(t, svc, ctx, "S001", "EB-S"+string(rune('A'+i)), g, splitDay))
	}
	// 搬台：0 kg 的「今天不搬台」标记（完整性校验要它）。
	_, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		Lines: []service.CounterLoadLine{{QtyKg: decimal.Zero}},
	}, "u-1")
	mustNoErr(t, err, "搬台标记")
	// 销售：一笔零售（完整性校验要它）。
	splitSell(t, svc, sku, "L-SAMPLE", "10", "200")
	estStocktake(t, svc, ctx, "S001", sku, "5", "")
	_, err = svc.PersistCutCostDaily(ctx, "S001", splitDay)
	mustNoErr(t, err, "PersistCutCostDaily")
	return pigs
}

// ---- ① 加入样本 ----

// TestCollectSamples_CompleteDay_WritesSamples 锁①。
//
// ⚠️ 加入样本**不自动重算** ⇒ 基线表此时不该有任何新行。
func TestCollectSamples_CompleteDay_WritesSamples(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	fmSeedCompleteDay(t, svc, ctx, sku)

	out, err := svc.CollectForecastSamples(ctx, "S001", service.ForecastSampleCollectInput{Date: fmGenDate})
	mustNoErr(t, err, "★ 数据齐全的一天应能加入样本")
	if out.Written < 1 {
		t.Fatalf("★ 应至少落下 1 条估计样本,got %d: %+v", out.Written, out)
	}
	if out.PigCount != 1 {
		t.Errorf("★ 当天 1 头猪,pig_count 应是 1,got %d", out.PigCount)
	}
	if out.Date != fmGenDate {
		t.Errorf("★ 返回的 date 应是 %s,got %s", fmGenDate, out.Date)
	}
	if !strings.Contains(out.Message, "重算") {
		t.Errorf("★ 提示要点名「重算」(加入样本不自动重算),got %q", out.Message)
	}
	// ⚠️ **不自动重算** ⇒ 基线表不该被这次调用写出行。
	rows, err := svc.ListForecastBaselines(ctx, "S001")
	mustNoErr(t, err, "ListForecastBaselines")
	for _, r := range rows {
		if r.WeightBand == "200-249" && r.CutType == model.CutBelly {
			t.Errorf("★ 加入样本不该自动写基线(重算是另一个动作),got %+v", r)
		}
	}
}

// TestCollectSamples_MissingAnyItem_400NamesIt 锁「加入样本前要数据完整」。
//
// ⚠️ 报错必须**点名**缺哪一项 —— 只说"数据不完整"门店没法行动。
func TestCollectSamples_MissingAnyItem_400NamesIt(t *testing.T) {
	cases := []struct {
		name string
		skip string // 故意不铺哪一项
		want string // 期望被点名的关键词
	}{
		{"缺录入(当天没进猪)", "录入", "录入"},
		{"缺搬台", "搬台", "搬台"},
		{"缺销售", "销售", "销售"},
		{"缺盘点(只有半盘也不行)", "盘点", "盘点"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, ctx, sku := splitSetup(t)
			if tc.skip != "录入" {
				estPig(t, svc, ctx, "S001", "EB-GEN", "200", splitDay)
			}
			if tc.skip != "搬台" {
				_, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
					Lines: []service.CounterLoadLine{{QtyKg: decimal.Zero}},
				}, "u-1")
				mustNoErr(t, err, "搬台标记")
			}
			if tc.skip != "销售" {
				splitSell(t, svc, sku, "L-GEN", "10", "200")
			}
			if tc.skip != "盘点" {
				estStocktake(t, svc, ctx, "S001", sku, "5", "")
			}

			_, err := svc.CollectForecastSamples(ctx, "S001", service.ForecastSampleCollectInput{Date: fmGenDate})
			if err == nil {
				t.Fatalf("★ 缺[%s]时不该加入成功", tc.skip)
			}
			if !errors.Is(err, service.ErrInvalidInput) {
				t.Errorf("应是输入校验错(400),got %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("★ 报错要点名缺[%s](门店才知道要补什么),got %v", tc.want, err)
			}
		})
	}
}

// TestCollectSamples_NoPigs_NamesMissingInputOnly 锁「没进猪那天**不**额外报分割样本」。
//
// ⚠️ 没猪 ⇒ 天然没有分割重量 ⇒ 那是"缺录入"的**后果**,不是另一个独立缺口
// ⇒ 一次报全 ≠ 报一堆重复项(别让门店以为要补两遍)。
func TestCollectSamples_NoPigs_NamesMissingInputOnly(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	// 不进猪;其余三项都铺齐。
	_, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		Lines: []service.CounterLoadLine{{QtyKg: decimal.Zero}},
	}, "u-1")
	mustNoErr(t, err, "搬台标记")
	splitSell(t, svc, sku, "L-NOPIG", "10", "200")
	estStocktake(t, svc, ctx, "S001", sku, "5", "")

	_, err = svc.CollectForecastSamples(ctx, "S001", service.ForecastSampleCollectInput{Date: fmGenDate})
	if err == nil {
		t.Fatalf("★ 没进猪那天该报错")
	}
	if strings.Contains(err.Error(), "分割样本") {
		t.Errorf("★ 没进猪那天不该额外报[分割样本](那是缺录入的后果),got %v", err)
	}
	if !strings.Contains(err.Error(), "录入") {
		t.Errorf("★ 该点名缺[录入],got %v", err)
	}
}

// TestCollectSamples_SplitSampleGapCondition 锁「分割样本缺口的判据」。
//
// ⚠️ 判据是"**两个池都空**才算缺",不是"任一为空":
//
//	有实录(门店补录过)⇒ 能拟合 ⇒ 不该报缺;
//	有估计(自动倒推摊出来的)⇒ 能拟合 ⇒ 不该报缺。
//	⇒ 写成"任一为空"⇒ 门店白忙一场(去补录了还是被拦)。
//
// ⚠️ 为什么用直接判定而不是端到端:
//
//	端到端场景里**几乎构造不出**"两个池都空"——
//	引擎会把没登记映射的销售归进"无名桶",倒推值仍为正 ⇒ 总有样本。
//	⇒ 判据抽成小函数直接锁(见 service.forecastSplitSampleGap)。
func TestCollectSamples_SplitSampleGapCondition(t *testing.T) {
	cases := []struct {
		name       string
		recordedN  int
		estimatedN int
		wantGap    bool
	}{
		{"两个池都空 ⇒ 缺", 0, 0, true},
		{"只有实录 ⇒ 不缺", 3, 0, false},
		{"只有估计 ⇒ 不缺", 0, 2, false},
		{"两个池都有 ⇒ 不缺", 1, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gap := service.ForecastSplitSampleGapForTest(tc.recordedN, tc.estimatedN)
			if tc.wantGap {
				if gap == "" {
					t.Fatalf("★ 应报缺分割样本")
				}
				if !strings.Contains(gap, "分割样本") {
					t.Errorf("★ 缺口文案要点名[分割样本],got %q", gap)
				}
				return
			}
			if gap != "" {
				t.Errorf("★ 不该报缺分割样本,got %q", gap)
			}
		})
	}
}

// ---- ② 样本库列表 ----

// TestListSamples_GroupedByDayAndBand 锁②。
//
// ⚠️ 分组单位 = **（营业日 × 体重段）** —— 这正是 owner 要的删除粒度。
func TestListSamples_GroupedByDayAndBand(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	// 一天两头猪:一头 200kg(200-249 段)、一头 260kg(250-299 段)⇒ 两组。
	fmSeedDerivedDay(t, svc, ctx, sku, "200", "260")

	lib, err := svc.ListForecastSamples(ctx, "S001", service.ForecastSampleQuery{})
	mustNoErr(t, err, "ListForecastSamples")
	if len(lib.Groups) != 2 {
		t.Fatalf("★ 一天两个体重段 ⇒ 应有 2 组,got %d: %+v", len(lib.Groups), lib.Groups)
	}
	bands := map[string]bool{}
	for _, g := range lib.Groups {
		bands[g.WeightBand] = true
		if g.Date != fmGenDate {
			t.Errorf("★ 组的日期应是 %s,got %s", fmGenDate, g.Date)
		}
		if g.PigCount != 1 {
			t.Errorf("★ 每组 1 头猪,pig_count 应是 1,got %d", g.PigCount)
		}
		if g.SampleCount < 1 {
			t.Errorf("★ 每组至少 1 条样本,got %d", g.SampleCount)
		}
		for _, p := range g.Pigs {
			if p.EarTag == "" {
				t.Errorf("★ 样本应带耳标(门店要靠它认出是哪头猪): %+v", p)
			}
			if len(p.Cuts) == 0 {
				t.Errorf("★ 每头猪应列出部位样本: %+v", p)
			}
		}
	}
	if !bands["200-249"] || !bands["250-299"] {
		t.Errorf("★ 应按体重段分成两组,got %+v", bands)
	}
}

// ---- ③ 删除样本(墓碑) ----

// TestDeleteSamples_Group_ThenRecompute_BaselineGone ⭐ 锁③ + ⑥。
//
// # owner 的原话
//
//	「我也可以选择删除某天某个重量的样本（如果觉得它不准，分割不合理）」
//	⇒ 删完之后**重算** ⇒ 那一层就不该再给出错建议。
//
// # 判别力自检
//
//	把重算的清理段退回去(不删"样本被删光的层")⇒ 本用例立刻红
//	(基线行还在 ⇒ p50 还是那个不准的数)。
func TestDeleteSamples_Group_ThenRecompute_BaselineGone(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	fmSeedDerivedDay(t, svc, ctx, sku, "200")

	// 先重算一次 ⇒ 这一层有基线(样本数 > 0)。
	_, err := svc.RecomputeForecastBaselines(ctx, "S001")
	mustNoErr(t, err, "RecomputeForecastBaselines")
	if _, ok := estBaseline(t, "S001", "200-249", model.CutBelly); !ok {
		t.Fatalf("★ 前提:应有 200-249 段的 belly 基线")
	}

	// 整组删(那天 + 那个体重段)。
	n, err := svc.DeleteForecastSamples(ctx, "S001", service.ForecastSampleTarget{
		Date: fmGenDate, WeightBand: "200-249",
	})
	mustNoErr(t, err, "DeleteForecastSamples")
	if n < 1 {
		t.Fatalf("★ 应删掉至少 1 条样本,got %d", n)
	}

	// 默认列表里不该再出现它。
	lib, err := svc.ListForecastSamples(ctx, "S001", service.ForecastSampleQuery{})
	mustNoErr(t, err, "ListForecastSamples")
	if lib.SampleCount != 0 {
		t.Errorf("★ 删完后样本库应是空的,got %d 条", lib.SampleCount)
	}

	// ⚠️ 重算 ⇒ **该层基线行消失**(样本没了就不该再给建议)。
	_, err = svc.RecomputeForecastBaselines(ctx, "S001")
	mustNoErr(t, err, "RecomputeForecastBaselines(删样本后)")
	if row, ok := estBaseline(t, "S001", "200-249", model.CutBelly); ok {
		t.Errorf("★ 样本被删光 ⇒ 该层基线行应消失(否则一直给出错建议),got %+v", row)
	}
}

// TestDeleteSamples_SinglePig_OnlyThatPig 锁「按单头猪删」。
//
// ⚠️ 两种粒度都要（owner：「两种都要」）：整组删 + 单头猪删。
func TestDeleteSamples_SinglePig_OnlyThatPig(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	pigs := fmSeedDerivedDay(t, svc, ctx, sku, "200", "240")

	n, err := svc.DeleteForecastSamples(ctx, "S001", service.ForecastSampleTarget{PigID: pigs[0]})
	mustNoErr(t, err, "DeleteForecastSamples")
	if n < 1 {
		t.Fatalf("★ 应删掉至少 1 条样本,got %d", n)
	}

	lib, err := svc.ListForecastSamples(ctx, "S001", service.ForecastSampleQuery{
		IncludeExcluded: true,
	})
	mustNoErr(t, err, "ListForecastSamples(含已删)")
	for _, g := range lib.Groups {
		for _, p := range g.Pigs {
			if p.PigID == pigs[0] {
				if !p.Excluded {
					t.Errorf("★ %s 应被标成已删除: %+v", pigs[0], p)
				}
			} else if p.Excluded {
				t.Errorf("★ %s 不该被删(只删了一头猪): %+v", p.PigID, p)
			}
		}
	}
}

// TestDeleteSamples_RequiresTarget 锁「参数不全 ⇒ 400（别一把删光）」。
func TestDeleteSamples_RequiresTarget(t *testing.T) {
	svc, ctx, _ := splitSetup(t)
	for _, tc := range []service.ForecastSampleTarget{
		{},
		{Date: fmGenDate},       // 只给日期(不给体重段)⇒ 语义未定义
		{WeightBand: "200-249"}, // 只给体重段(不给日期)
		{Date: "不是日期", WeightBand: "200-249"},
	} {
		_, err := svc.DeleteForecastSamples(ctx, "S001", tc)
		if err == nil {
			t.Errorf("★ target=%+v 时应报错(别把整店样本删光)", tc)
		}
		if !errors.Is(err, service.ErrInvalidInput) {
			t.Errorf("应是输入校验错(400),got %v", err)
		}
	}
}

// TestTombstone_NotResurrectedByCollect ⭐ 锁「删除必须是墓碑」。
//
// # 为什么这是核心
//
//	引擎每次跑那天的成本链都会**重建**那天的估计样本
//	⇒ 物理删掉的样本会在下一次重建时**原样复活** ⇒ "删除"等于没删。
//
// # 判别力自检
//
//	把重建里的"跳过墓碑行"去掉 ⇒ 本用例立刻红(样本复活 ⇒ 库里又有它了)。
func TestTombstone_NotResurrectedByCollect(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	fmSeedDerivedDay(t, svc, ctx, sku, "200")

	n, err := svc.DeleteForecastSamples(ctx, "S001", service.ForecastSampleTarget{
		Date: fmGenDate, WeightBand: "200-249",
	})
	mustNoErr(t, err, "DeleteForecastSamples")
	if n < 1 {
		t.Fatalf("★ 应删掉至少 1 条样本,got %d", n)
	}

	// 再跑一次那天的引擎(= 再点一次「加入样本」)⇒ 墓碑行**不许复活**。
	out, err := svc.CollectForecastSamples(ctx, "S001", service.ForecastSampleCollectInput{Date: fmGenDate})
	mustNoErr(t, err, "CollectForecastSamples(第二次)")
	if out.SkippedExcluded < 1 {
		t.Errorf("★ 重建时应跳过墓碑行,got skipped_excluded=%d", out.SkippedExcluded)
	}
	if !strings.Contains(out.Message, "已被删除") {
		t.Errorf("★ 提示要说清「这些样本是你自己删的、没有自动恢复」(否则门店会以为系统坏了),got %q", out.Message)
	}
	lib, err := svc.ListForecastSamples(ctx, "S001", service.ForecastSampleQuery{})
	mustNoErr(t, err, "ListForecastSamples")
	if lib.SampleCount != 0 {
		t.Errorf("★ 墓碑行不许复活(引擎重跑链会重建那天的样本),got %d 条", lib.SampleCount)
	}
	// 但**看得见**它被删了(include_excluded=true)。
	libAll, err := svc.ListForecastSamples(ctx, "S001", service.ForecastSampleQuery{IncludeExcluded: true})
	mustNoErr(t, err, "ListForecastSamples(含已删)")
	if libAll.ExcludedSampleCount < 1 {
		t.Errorf("★ 墓碑行应能被看到(可恢复),got %d", libAll.ExcludedSampleCount)
	}
}

// TestRestoreSamples_Restores ⭐ 锁「墓碑可恢复」。
func TestRestoreSamples_Restores(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	fmSeedDerivedDay(t, svc, ctx, sku, "200")

	_, err := svc.DeleteForecastSamples(ctx, "S001", service.ForecastSampleTarget{
		Date: fmGenDate, WeightBand: "200-249",
	})
	mustNoErr(t, err, "DeleteForecastSamples")

	n, err := svc.RestoreForecastSamples(ctx, "S001", service.ForecastSampleTarget{
		Date: fmGenDate, WeightBand: "200-249",
	})
	mustNoErr(t, err, "RestoreForecastSamples")
	if n < 1 {
		t.Fatalf("★ 应恢复至少 1 条样本,got %d", n)
	}
	lib, err := svc.ListForecastSamples(ctx, "S001", service.ForecastSampleQuery{})
	mustNoErr(t, err, "ListForecastSamples")
	if lib.SampleCount < 1 {
		t.Errorf("★ 恢复后样本应回到库里,got %d", lib.SampleCount)
	}
	// 重算 ⇒ 该层基线又回来了。
	_, err = svc.RecomputeForecastBaselines(ctx, "S001")
	mustNoErr(t, err, "RecomputeForecastBaselines(恢复后)")
	if _, ok := estBaseline(t, "S001", "200-249", model.CutBelly); !ok {
		t.Errorf("★ 恢复后重算 ⇒ 该层基线应回来")
	}
}

// ---- ④ 重算的拟合口径 ----

// TestRecompute_TwoPigsSameBand_SampleCountTwo ⭐ 锁④（n ≥ 1 与「两头猪 = n=2」）。
//
// # owner 的原话
//
//	「如果每天有两头猪，那么就有两个重量样本」
//
// ⚠️ 若按「体重」合并样本 ⇒ 两条互相覆盖 ⇒ n 永远卡在 1
// ⇒ 门槛 5 时永远"样本不足" ⇒ 死锁。
func TestRecompute_TwoPigsSameBand_SampleCountTwo(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	fmSeedDerivedDay(t, svc, ctx, sku, "200", "240") // 两头都在 200-249 段

	_, err := svc.RecomputeForecastBaselines(ctx, "S001")
	mustNoErr(t, err, "RecomputeForecastBaselines")
	row, ok := estBaseline(t, "S001", "200-249", model.CutBelly)
	if !ok {
		t.Fatalf("★ 应有 200-249 段的 belly 基线")
	}
	if row.SampleCount != 2 {
		t.Errorf("★ 同段两头猪 ⇒ 样本数应是 2(一头猪一个样本),got %d", row.SampleCount)
	}
	if row.EstimatedSampleCount != 2 {
		t.Errorf("★ estimated_sample_count 应是 2,got %d", row.EstimatedSampleCount)
	}
}

// TestRecompute_SingleSampleIsUsable ⭐ 锁④（门槛 = 1）。
//
// ⚠️ 门槛是 5 时 ⇒ 一头猪永远"不可用" ⇒ 建议分割永远回退 LLM ⇒ 样本白攒。
func TestRecompute_SingleSampleIsUsable(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	fmSeedDerivedDay(t, svc, ctx, sku, "200")

	_, err := svc.RecomputeForecastBaselines(ctx, "S001")
	mustNoErr(t, err, "RecomputeForecastBaselines")
	row, ok := estBaseline(t, "S001", "200-249", model.CutBelly)
	if !ok {
		t.Fatalf("★ 应有基线行")
	}
	if row.SampleCount != 1 {
		t.Fatalf("★ 样本数应是 1,got %d", row.SampleCount)
	}
	// ⚠️ **可用** ⇒ 建议分割取基线 p50(不再回退 LLM)。
	_, usable, err := svc.ForecastFor(ctx, "S001", d("200"), model.CutBelly)
	mustNoErr(t, err, "ForecastFor")
	if !usable {
		t.Errorf("★ 有 1 个样本就该可用(门槛 n≥1),got 不可用")
	}
}

// TestRecompute_MergedObservations_RecordedWins ⭐ 锁⑤（合并观察 + 去重）。
//
// # 场景
//
//	先跑引擎 ⇒ 两头猪各有 1 条**估计**样本;
//	再补录一头猪的**实录**分割(门店手工补录)⇒ 同一头猪同一部位**两个池子都有**。
//
// # 期望
//
//	⇒ **实录优先** ⇒ 那头猪只算 **1** 条样本(不是 2 条)
//	⇒ 样本总数 = 2(不是 3)⇒ sample_source = mixed。
//
// # 为什么重要
//
//	两个池子同时计入 = **重复计样本** ⇒ 均值被拉偏、n 被虚高。
func TestRecompute_MergedObservations_RecordedWins(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	pigs := fmSeedDerivedDay(t, svc, ctx, sku, "200", "240")

	// ⚠️ **直接写库**补一条实录(走公开 API 会触发引擎重跑 ⇒ 估计样本被清掉 ⇒ 造不出重叠)。
	mustNoErr(t, testDBSingleton.Create(&model.PigCut{
		ID: "PC-OVERLAP", PigID: pigs[0], CutType: model.CutBelly, BranchID: "S001",
		Barcode: "B-OVERLAP", WeightKg: d("30"), ExpiresAt: splitDay.Add(48 * time.Hour),
		CubeProductID: sku, CreatedAt: time.Now(),
	}).Error, "插实录")

	_, err := svc.RecomputeForecastBaselines(ctx, "S001")
	mustNoErr(t, err, "RecomputeForecastBaselines")
	row, ok := estBaseline(t, "S001", "200-249", model.CutBelly)
	if !ok {
		t.Fatalf("★ 应有基线行")
	}
	if row.SampleCount != 2 {
		t.Errorf("★ 同一头猪同一部位只算 1 条(实录优先)⇒ 样本数应是 2,got %d", row.SampleCount)
	}
	if row.RecordedSampleCount != 1 {
		t.Errorf("★ recorded_sample_count 应是 1,got %d", row.RecordedSampleCount)
	}
	if row.EstimatedSampleCount != 1 {
		t.Errorf("★ estimated_sample_count 应是 1(重叠的那条被实录顶掉),got %d", row.EstimatedSampleCount)
	}
	if row.SampleSource != service.SampleSourceMixed {
		t.Errorf("★ 两个池子都有贡献 ⇒ sample_source 应是 mixed,got %q", row.SampleSource)
	}
}

// TestRecompute_OverwritesManualRow_KeepsMarker ⭐ 锁③（重算覆盖所有分层）。
//
// # ⚠️ 这是**故意**改掉的行为（owner 2026-10-30 重新定义模型）
//
//	旧口径：「管理员手动生成的行整层跳过」(manual_generated 保护)
//	新口径：**基线是派生物** ⇒ 重算就该把它算回去
//	         ⇒ `manual_generated` 退化成"曾手工触发"的**留痕**，不再保护任何东西。
//
// # 期望
//
//	① 数字被重算覆盖(不再钉死 99);
//	② `manual_generated` 标记**保留**(可追溯)。
func TestRecompute_OverwritesManualRow_KeepsMarker(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	fmSeedDerivedDay(t, svc, ctx, sku, "200")

	// ⚠️ 只能**改**已有那一行(不能插):(店, 体重段, 部位) 是唯一键。
	// 模拟"这一层曾经被人工设过"(manual_generated + 一个手改的 p50)。
	mustNoErr(t, testDBSingleton.Model(&model.PigForecastBaseline{}).
		Where("branch_id = ? AND weight_band = ? AND cut_type = ?", "S001", "200-249", model.CutBelly).
		Updates(map[string]any{
			"manual_generated":    true,
			"generated_from_date": fmGenDate,
			"p50_kg":              d("99"), "mean_kg": d("99"),
			"p10_kg": d("99"), "p90_kg": d("99"),
		}).Error, "改手动标记")

	_, err := svc.RecomputeForecastBaselines(ctx, "S001")
	mustNoErr(t, err, "RecomputeForecastBaselines")
	row, ok := estBaseline(t, "S001", "200-249", model.CutBelly)
	if !ok {
		t.Fatalf("★ 应有基线行")
	}
	if row.P50Kg.Equal(d("99")) {
		t.Errorf("★ 基线是派生物 ⇒ 重算应覆盖它(不该钉死在 99)")
	}
	if !row.ManualGenerated {
		t.Errorf("★ manual_generated 应保留为「曾手工触发」的留痕")
	}
	if row.GeneratedFromDate != fmGenDate {
		t.Errorf("★ generated_from_date 应保留,got %q", row.GeneratedFromDate)
	}
}

// ---- ⑤ 跨店：加入别店那天的样本 ⇒ 落在别店自己的样本库 ----

// TestCollectSamples_FromOtherBranch_LandsInThatBranch 锁「样本归猪的门店」。
//
// ⚠️ 不能把别店的猪写进本店样本库 ⇒ 本店要用别店的数字 ⇒ 走「跨店选用」。
func TestCollectSamples_FromOtherBranch_LandsInThatBranch(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	// S002 也要有映射。
	_, err := svc.CreateBranchCutMapping(ctx, "S002",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: sku})
	mustNoErr(t, err, "CreateBranchCutMapping(S002)")

	svc.SetClock(fmClock(splitDay))
	estPig(t, svc, ctx, "S002", "EB-S002", "220", splitDay)
	_, err = svc.CounterLoad(ctx, "S002", service.CounterLoadInput{
		Lines: []service.CounterLoadLine{{QtyKg: decimal.Zero}},
	}, "u-1")
	mustNoErr(t, err, "S002 搬台标记")
	fmSellBranch(t, svc, "S002", sku, "L-S002")
	fmStocktakeBranch(t, svc, "S002", sku)

	out, err := svc.CollectForecastSamples(ctx, "S001", service.ForecastSampleCollectInput{
		Date: fmGenDate, SourceBranchID: "S002",
	})
	mustNoErr(t, err, "CollectForecastSamples(跨店)")
	if out.SourceBranchID != "S002" {
		t.Errorf("★ 返回的 source_branch_id 应回显 %q,got %q", "S002", out.SourceBranchID)
	}
	// 样本落在 **S002** 自己的库里。
	libS2, err := svc.ListForecastSamples(ctx, "S002", service.ForecastSampleQuery{})
	mustNoErr(t, err, "ListForecastSamples(S002)")
	if libS2.SampleCount < 1 {
		t.Errorf("★ 样本应落在 S002 自己的样本库,got %d", libS2.SampleCount)
	}
	libS1, err := svc.ListForecastSamples(ctx, "S001", service.ForecastSampleQuery{})
	mustNoErr(t, err, "ListForecastSamples(S001)")
	if libS1.SampleCount != 0 {
		t.Errorf("★ 本店样本库不该有别店的猪,got %d", libS1.SampleCount)
	}
}

// ---- ⑥ 墓碑列本身 ----

// TestForecastSample_TombstoneColumnExists 锁「墓碑列真的落库了」。
//
// ⚠️ 删样本是**软删** ⇒ 必须有一列记着"删了 + 什么时候 + 为什么"。
func TestForecastSample_TombstoneColumnExists(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	fmSeedDerivedDay(t, svc, ctx, sku, "200")

	_, err := svc.DeleteForecastSamples(ctx, "S001", service.ForecastSampleTarget{
		Date: fmGenDate, WeightBand: "200-249",
	})
	mustNoErr(t, err, "DeleteForecastSamples")

	var rows []model.PigCutEstimate
	mustNoErr(t, testDBSingleton.Where("branch_id = ?", "S001").Find(&rows).Error, "读估计样本")
	if len(rows) == 0 {
		t.Fatalf("★ 墓碑不是物理删 ⇒ 行还在")
	}
	for _, r := range rows {
		if r.ExcludedAt == nil {
			t.Errorf("★ 该行应有墓碑时间: %+v", r)
		}
		if r.ExcludedReason != "manual" {
			t.Errorf("★ 墓碑原因应是 manual,got %q", r.ExcludedReason)
		}
	}
	// ⚠️ 墓碑行**不参与拟合** ⇒ 读样本时看不到它。
	rows2, err := svc.ListForecastSamples(ctx, "S001", service.ForecastSampleQuery{})
	mustNoErr(t, err, "ListForecastSamples")
	if rows2.SampleCount != 0 {
		t.Errorf("★ 墓碑行不参与拟合,got %d", rows2.SampleCount)
	}
	_ = datatypes.Date{}
}
