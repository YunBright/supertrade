package service_test

// 「今天整日都不需要搬台」必须**能表达**（2026-10-30 owner）。
//
// # 为什么要这个能力
//
// 门店原话：「有时候整日都不需要搬台，我找不到 0 搬台的操作办法」。
// ⇒ 之前 `qty_kg 必须 > 0` ⇒ 门店根本没法把每日作业第②步标成"已完成"
//   ⇒ 第②步永远显示"未搬台"。
//
// # 口径：0 kg 的搬台流水 =「**门店确认今天不搬台**」的标记
//
//	- **数值上等于"没搬"** ⇒ 所有计算都是加减 0 ⇒ 不影响毛利 / 成本链 / 冷库余额;
//	- 但它**不是**"搬过台" ⇒ 两个状态判定必须把它排除:
//	  ① 营业日动作(hasBizDayActions)—— 否则期初生效日会从"纯基线日"变成"营业日"
//	     ⇒ 那天的日终就必须盘点 ⇒ 链被硬推;
//	  ② 休市判定(isRestDay)—— 否则"今天不搬台"会把那天判成"不是休市" ⇒ 口径反了。
//
// 本文件锁这三条:能提交 / 数值零影响 / 状态副作用被摘掉。

import (
	"context"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

// fmCounterLoadZero 提交一条 0 kg 的搬台流水(「今天不搬台」)。
func fmCounterLoadZero(t *testing.T, svc *service.Service, branchID string, day time.Time) {
	t.Helper()
	_, err := svc.CounterLoad(context.Background(), branchID, service.CounterLoadInput{
		Lines: []service.CounterLoadLine{
			// ⚠️ 0 kg ⇒ 没有货号(没搬任何货)⇒ 也不填标签号。
			{QtyKg: d("0")},
		},
		TakenDate: day.Format("2006-01-02"),
	}, "u-1")
	mustNoErr(t, err, "CounterLoad(qty=0)")
}

// TestCounterLoad_ZeroKg_Accepted 锁「0 搬台能提交」(之前是 400:qty_kg 必须 > 0)。
func TestCounterLoad_ZeroKg_Accepted(t *testing.T) {
	svc, _ := setupTestService(t)
	day := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)

	fmCounterLoadZero(t, svc, "S001", day)

	view, err := svc.ListCounterLoads(context.Background(), "S001", day)
	mustNoErr(t, err, "ListCounterLoads")
	if len(view.Lines) != 1 {
		t.Fatalf("应落 1 行流水, got %d", len(view.Lines))
	}
	if !view.Lines[0].QtyKg.IsZero() {
		t.Errorf("流水重量应为 0, got %s", view.Lines[0].QtyKg)
	}
	// 「已确认今天不搬台」这个信号要给前端。
	if !view.CounterLoadSkipped {
		t.Errorf("★ 0 kg 流水应让 counter_load_skipped = true(前端据此显示'已确认今天不搬台')")
	}
	// ⚠️ 但**不能**变成"已搬台"。
	if view.HasCounterLoad {
		t.Errorf("★ 0 kg 不该算'已搬台'(has_counter_load 应为 false)")
	}
	if !view.TotalKg.IsZero() {
		t.Errorf("总搬台量应为 0, got %s", view.TotalKg)
	}
}

// TestCounterLoad_ZeroKg_NoPositionChange 锁「0 搬台不动库存」。
//
// ⚠️ 0 kg 的行**没有货号**⇒ 位置表里不能凭空多出一行(或把某个 SKU 的量改了)。
func TestCounterLoad_ZeroKg_NoPositionChange(t *testing.T) {
	svc, _ := setupTestService(t)
	day := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)

	fmCounterLoadZero(t, svc, "S001", day)

	snap, err := svc.ListStock(context.Background(), "S001", model.StockScopeAll)
	mustNoErr(t, err, "ListStock")
	if !snap.TotalKg.IsZero() {
		t.Errorf("★ 0 搬台不该产生任何库存, total_kg = %s", snap.TotalKg)
	}
	if len(snap.Lines) != 0 {
		t.Errorf("★ 0 搬台不该产生库存行, got %d", len(snap.Lines))
	}
}

// TestCounterLoad_ZeroKg_NotBizDayAction 锁「0 搬台**不算**营业日动作」。
//
// ⚠️ 这是最容易出事的一条:期初生效日若是"纯基线日"(没有任何营业日动作)
//
//	⇒ 那天的日终 = 期初本身 ⇒ 链从那里开始 ⇒ **不需要盘点单**。
//	⇒ 一条 0 搬台若被当成"搬过台"⇒ 那天变成营业日 ⇒ 门店被迫补一张盘点单
//	⇒ 而"补录"不了(期初那天的货还没卖过)⇒ 链会断。
func TestCounterLoad_ZeroKg_NotBizDayAction(t *testing.T) {
	svc, _ := setupTestService(t)
	day := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)

	fmCounterLoadZero(t, svc, "S001", day)

	// 当天既没进猪、也没盘点 ⇒ 只剩搬台这一个可能的"动作"。
	// 0 搬台不算 ⇒ 那天的营业日状态不该被推进到 counter_loaded。
	bd, err := svc.GetBusinessDay(context.Background(), "S001", day)
	mustNoErr(t, err, "GetBusinessDay")
	if bd.HasCounterLoad {
		t.Errorf("★ 0 搬台不该置 has_counter_load")
	}
	if bd.Status == "counter_loaded" {
		t.Errorf("★ 0 搬台不该把状态推进到 counter_loaded, got %q", bd.Status)
	}
	if !bd.CounterLoadKg.IsZero() {
		t.Errorf("★ counter_load_kg 应为 0, got %s", bd.CounterLoadKg)
	}
}

// TestCounterLoad_ZeroKg_StillRestDay 锁「0 搬台仍是休市」。
//
// ⚠️ 门店口径：「当天既没有进猪、也没有搬冷库商品上猪肉台」⇒ 休市。
//
//	"确认今天不搬台"恰恰说明**没搬** ⇒ 那天还是休市。
//	⇒ 若把它判成"不是休市"⇒ 盘点单会被标成"有货"，口径反了。
func TestCounterLoad_ZeroKg_StillRestDay(t *testing.T) {
	svc, _ := setupTestService(t)
	day := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)

	fmCounterLoadZero(t, svc, "S001", day)

	// 没进猪 ⇒ 提交盘点单时后端应标"鲜肉休市"。
	_, err := svc.CreateBranchCutMapping(context.Background(), "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-2001"})
	mustNoErr(t, err, "CreateBranchCutMapping")

	take, err := svc.RecordPorkCutsStocktake(context.Background(), "S001",
		model.RecordPorkCutsStocktakeInput{
			IsComplete: true,
			Cuts: []model.CutInput{
				{CutType: model.CutBelly, CubeProductID: "P-2001", ActualRemainKg: d("0")},
			},
		}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake")
	if !take.RestDay {
		t.Errorf("★ 0 搬台(没搬货)+ 没进猪 ⇒ 那天仍应是休市(rest_day)")
	}
}

// TestCounterLoad_ZeroKg_ChainStartRejected 锁「0 搬台也进不了链起点那天」。
//
// ⚠️ 为什么这条要紧:链起点(期初生效日)那天**不允许有任何营业日动作**
// —— 它的日终 = 期初本身 ⇒ 链从那里开始 ⇒ 不需要盘点单。
// ⇒ 连"确认今天不搬台"的 0 kg 标记行都不许落在那天
// ⇒ 否则 hasBizDayActions 会把它算成"有动作"⇒ 纯基线日被破坏。
//
// ⚠️ 注意:链起点会**随改期初而前移**(改期初 = 换链起点)
// ⇒ 新链起点那天**可能已经有搬台流水**(那是旧链起点+1 的日子)
// ⇒ 那时 hasBizDayActions 的 `qty_kg > 0` 过滤才真正起作用
// (见 business_day.go 的注释)。
func TestCounterLoad_ZeroKg_ChainStartRejected(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()

	// 期初结转(生效日 = 测试时钟的今天 = 2026-10-01)⇒ 链的起点 = 10-01。
	_, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, ColdStoreKg: d("10"), UnitPriceYuan: d("16")},
	})
	mustNoErr(t, err, "SaveStockOpenings")

	// 链起点当天录搬台(哪怕 0 kg)⇒ 必须被拒。
	_, err = svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		Lines:     []service.CounterLoadLine{{QtyKg: d("0")}},
		TakenDate: "2026-10-01",
	}, "u-1")
	if err == nil {
		t.Errorf("★ 链起点当天(期初生效日)不该允许录搬台(哪怕 0 kg):" +
			"那天的日终 = 期初本身 ⇒ 链从那里开始 ⇒ 不需要盘点单")
	}
}

// TestCounterLoad_ZeroKg_NegativeRejected 锁「负数仍然拒绝」。
//
// ⚠️ 放宽的是 0（= 确认不搬台），**不是**负数 ⇒ 负重量是脏数据，必须挡。
func TestCounterLoad_ZeroKg_NegativeRejected(t *testing.T) {
	svc, _ := setupTestService(t)
	day := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)

	_, err := svc.CounterLoad(context.Background(), "S001", service.CounterLoadInput{
		Lines: []service.CounterLoadLine{
			{CubeProductID: "P-2001", QtyKg: d("-1")},
		},
		TakenDate: day.Format("2006-01-02"),
	}, "u-1")
	if err == nil {
		t.Errorf("★ 负重量必须被拒(qty_kg >= 0)")
	}
}

// TestCounterLoad_ZeroKg_BypassesPrevStocktakeCheck 锁「0 搬台**不受**跨日顺序校验约束」。
//
// # 为什么这条要紧
//
// 「前一个营业日必须已盘点」这条校验是为**真搬台**服务的:
// 搬货要拿昨天的日终当基线 ⇒ 昨天没盘 ⇒ 今天的搬台没有依据。
//
// 但「今天不搬台」的 0 kg 标记**不动货、不改余额链** ⇒ 没有基线问题
// ⇒ 不能因为昨天没盘点,就把门店"记一笔今天没搬台"这件事也挡住
// (那会让门店在"整日不搬台"的日子点不动按钮)。
//
// ⚠️ 反过来:**真搬台**仍必须被挡 ⇒ 校验不能被 0 搬台"带跑"。
func TestCounterLoad_ZeroKg_BypassesPrevStocktakeCheck(t *testing.T) {
	svc, ctx, sku := fmFifoSetup(t)

	// 10-01 盘过一次 ⇒ 门店有营业日历史(lastComplete = 10-01)。
	_, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("5")},
		},
	}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake(10-01)")

	// 10-02 **故意不盘点** ⇒ 10-03 的真搬台会被跨日顺序校验挡住。
	svc.SetClock(fmClock(fmDay1.Add(48 * time.Hour))) // 10-03

	// ① 真搬台 ⇒ 仍被挡（这条校验对"搬货"必须保留）。
	_, err = svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		Lines:     []service.CounterLoadLine{{CubeProductID: sku, QtyKg: d("5")}},
		TakenDate: "2026-10-03",
	}, "u-1")
	if err == nil {
		t.Errorf("★ 10-02 没盘点 ⇒ 10-03 的**真搬台**仍应被拒" +
			"(跨日顺序校验不能被 0 搬台带跑)")
	}

	// ② 「今天不搬台」的 0 kg 标记 ⇒ **放行**（它不动货、不改余额链）。
	_, err = svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		Lines:     []service.CounterLoadLine{{QtyKg: d("0")}},
		TakenDate: "2026-10-03",
	}, "u-1")
	mustNoErr(t, err, "CounterLoad(0 kg 标记;前一个营业日没盘点)")
}
