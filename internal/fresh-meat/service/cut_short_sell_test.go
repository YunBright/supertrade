package service_test

// 2026-10-30(owner 定稿):**短卖**。
//
// # 背景(生产实测 店 00,已查证 —— 别重查)
//
//	2026-10-29:Σ进货 181.709kg、Σ销售 178.149kg、Σ期初 10kg、Σ期末 20.879kg
//	  ⇒ **总量够** ⇒ 那 8 个"期末为负"的部位是「建议分割与门店实际卖的不匹配」的
//	    **分摊口径**问题,不是真短卖;
//	  ⇒ 但引擎把**负的账面推演**写进了 cut_cost_daily.closing_kg
//	    ⇒ 链把它当次日期初 ⇒ 门店看到"期初有负数"。
//	2026-10-30:无盘点、Σ期初 20.879kg、Σ销售 83.391kg ⇒ 缺口 **62.512kg** ⇒ **真短卖**。
//
// # 锁的六条口径
//
//	R1 期末量永远 ≥0(盘到的部位用实盘,**含 0 kg**)⇒ 次日期初不会有负数;
//	R2 分部位的账面不足只作提示(cut_short_sell_kg),**不进链**;
//	R3 当日**总量**短卖才算数(short_sell_kg);
//	R4 真短卖才结转(累计短卖),且**绝不回写成负库存**;
//	R5 短卖的那部分货**必须计成本**(不许成本算不出来 ⇒ 不许毛利虚高);
//	R6 描红(报表出参)+ 阻断审计结转(POST /stock/audit 拒绝、precheck 列出那天)。
//
// # 为什么这些用例必须走**公开 API + 真实 SQLite**
//
//	引擎接线错了(期初没接上、盘点没读到、链没往下传)也会红
//	⇒ 锁的是**口径**,不是实现细节。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

// 本文件用的四个营业日(2026-10-28 ~ 10-31 14:00 +08)。
//
// ⚠️ 10-28 是**基线日**(门店录期初结转的那天)⇒ 成本链从那天起算
//
//	(见 cutChainBaseline:「期初生效日就是链的起点」)
//	⇒ 链才真的**逐日接续**(否则每份报表各算各的,期初会走"快照/余额链"兜底路径)。
var (
	ssBaseDay = time.Date(2026, 10, 28, 6, 0, 0, 0, time.UTC)
	ssDay1    = time.Date(2026, 10, 29, 6, 0, 0, 0, time.UTC)
	ssDay2    = time.Date(2026, 10, 30, 6, 0, 0, 0, time.UTC)
	ssDay3    = time.Date(2026, 10, 31, 6, 0, 0, 0, time.UTC)
)

// ssSKU 部位 → 货号(与 ssSetup 的映射一致)。
func ssSKU(cut model.CutType) string {
	if cut == model.CutLegRear {
		return "P-LEG"
	}
	return "P-BELLY"
}

// ssSetup 起服务 + 登记 belly / 后腿两个部位的货号映射。
func ssSetup(t *testing.T) (*service.Service, context.Context) {
	t.Helper()
	svc, _ := setupTestService(t)
	ctx := context.Background()
	for _, cut := range []model.CutType{model.CutBelly, model.CutLegRear} {
		_, err := svc.CreateBranchCutMapping(ctx, "S001",
			model.CreateBranchCutMappingInput{CutType: cut, CubeProductID: ssSKU(cut)})
		mustNoErr(t, err, "CreateBranchCutMapping "+string(cut))
	}
	return svc, ctx
}

// ssSeedPig 录一头猪 + 切出指定部位(权威的"这头猪产出了这些部位多少 kg")。
//
// ⚠️ 分割建议(基线/LLM)在这个环境里是空的 ⇒ 引擎不接管入库量
//
//	⇒ 入库量 = 这里补录的重量 ⇒ 用例里的数是确定的。
func ssSeedPig(t *testing.T, svc *service.Service, day time.Time, earTag string,
	cuts map[model.CutType]string,
) string {
	t.Helper()
	pig, err := svc.RecordWholePig(context.Background(), "S001", model.RecordWholePigInput{
		EarTag: earTag, GrossWeightKg: d("100"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: day,
	}, "u-1")
	mustNoErr(t, err, "RecordWholePig "+earTag)
	for cut, kg := range cuts {
		_, err := svc.RecordPigCut(context.Background(), "S001", model.RecordPigCutInput{
			PigID: pig.ID, CutType: cut, Barcode: "B-" + earTag + "-" + string(cut),
			WeightKg: d(kg), ExpiresAt: day.Add(48 * time.Hour), CubeProductID: ssSKU(cut),
		}, "u-1")
		mustNoErr(t, err, "RecordPigCut "+string(cut))
	}
	return pig.ID
}

// ssSell 记一笔零售销售(本地 POS 事件)。
//
// ⚠️ **必须带 pigID**:POS 事件在"当天找不到猪"时会把整行**丢掉**
//
//	(见 subscribe.go 的 resolvePigByCubeProduct:「反查失败 → drop 行」)
//	⇒ 那样销售根本进不了账 ⇒ 用例会"莫名其妙地没有短卖"。
//	⇒ 这里显式带上产出这批货的那头猪(业务上就是"这批货是那头猪出的")。
func ssSell(t *testing.T, svc *service.Service, day time.Time, pigID string,
	cut model.CutType, qty, amount string,
) {
	t.Helper()
	_, err := svc.OnSaleCompleted(context.Background(), service.SaleCompletedPayload{
		SaleID:      "SO-" + string(cut) + "-" + day.Format("0102") + "-" + qty,
		BranchID:    "S001",
		CompletedAt: day.Add(time.Hour),
		Lines: []service.SaleLine{{
			LineID: "L-" + string(cut) + "-" + qty, SKUID: ssSKU(cut), FreshType: "meat",
			Qty: d(qty), AmountYuan: d(amount), PigID: pigID, CutType: cut,
		}},
	})
	mustNoErr(t, err, "OnSaleCompleted "+string(cut))
}

// ssStocktake 录一张**完整**的日终盘点(实盘 = 猪肉台上的重量;0 = 售罄)。
//
// ⚠️ 归属日 = 服务端时钟所在那天 ⇒ 调用前必须先把时钟拨到那一天。
func ssStocktake(t *testing.T, svc *service.Service, day time.Time, cuts map[model.CutType]string) {
	t.Helper()
	in := make([]model.CutInput, 0, len(cuts))
	for cut, kg := range cuts {
		in = append(in, model.CutInput{
			CutType: cut, CubeProductID: ssSKU(cut), ActualRemainKg: d(kg),
		})
	}
	_, err := svc.RecordPorkCutsStocktake(context.Background(), "S001",
		model.RecordPorkCutsStocktakeInput{IsComplete: true, Cuts: in}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake")
}

// ssCounterLoad 记一次搬上台(冷库 → 猪肉台)。
func ssCounterLoad(t *testing.T, svc *service.Service, day time.Time, cut model.CutType, qty string) {
	t.Helper()
	_, err := svc.CounterLoad(context.Background(), "S001", service.CounterLoadInput{
		Lines:     []service.CounterLoadLine{{CubeProductID: ssSKU(cut), QtyKg: d(qty)}},
		TakenDate: day.Format("2006-01-02"),
	}, "u-1")
	mustNoErr(t, err, "CounterLoad "+string(cut))
}

// ssOpening 录门店的**期初结转**(链的起点:门店录了哪天,链就从哪天起算)。
//
// ⚠️ 期初结转**不许填未来日期** ⇒ 调用前必须把时钟拨到那一天之后。
func ssOpening(t *testing.T, svc *service.Service, day time.Time, cut model.CutType, kg, unitPrice string) {
	t.Helper()
	_, err := svc.SaveStockOpenings(context.Background(), "S001", []service.StockOpeningInput{{
		CutType: cut, BizDate: day.Format("2006-01-02"),
		ColdStoreKg: d(kg), UnitPriceYuan: d(unitPrice),
	}})
	mustNoErr(t, err, "SaveStockOpenings "+string(cut))
}

// ssAssertNoNegativeOpening 断言报表里**没有负期初**(门店最直观的那个坑)。
func ssAssertNoNegativeOpening(t *testing.T, rep *service.GrossMarginReport, day string) {
	t.Helper()
	for _, r := range rep.PerCut {
		if r.OpeningKg.IsNegative() {
			t.Errorf("★ %s 期初不该是负数(门店看到「期初有负数」就是这个坑): %s 期初 = %s",
				day, r.CutType, r.OpeningKg)
		}
	}
}

// ssAssertReconBalanced 断言对账行仍然闭合(加了短卖分量之后也不能破)。
func ssAssertReconBalanced(t *testing.T, rep *service.GrossMarginReport, day string) {
	t.Helper()
	if rep.Reconciliation.DiffYuan.Abs().GreaterThan(d("0.01")) {
		t.Errorf("★ %s 对账差额应仍为 0(加了短卖分量之后), got %s", day, rep.Reconciliation.DiffYuan)
	}
}

// TestShortSell_StocktakeZero_ClosingNotNegative 锁 **R1**。
//
// # 场景(复刻生产:盘到 0 kg 的部位)
//
//	10-29:进猪切 belly 10kg,卖了 15kg(卖超了 5kg)⇒ **账面推演的期末 = −5kg**;
//	      盘点单写 belly = **0 kg**(售罄)⇒ 实盘就是 0。
//
// # 期望
//
//	① 账面推演(报表的"应有")仍然是负数 —— 它是**提示**,必须看得见;
//	② **期末量(链上的结余)必须是 0**,不是 −5
//	   ⇒ 旧实现把负的账面推演写进 closing_kg ⇒ 链把它当次日期初;
//	③ 次日期初 ≥ 0(门店不再看到负数)。
//
// # 判别力
//
//	去掉"盘到的部位用实盘"那一步(退回负的账面推演)⇒ ② 立刻红(closing_kg = −5)。
func TestShortSell_StocktakeZero_ClosingNotNegative(t *testing.T) {
	svc, ctx := ssSetup(t)
	svc.SetClock(fmClock(ssDay1))
	pigID := ssSeedPig(t, svc, ssDay1, "EB-1", map[model.CutType]string{model.CutBelly: "10"})
	ssSell(t, svc, ssDay1, pigID, model.CutBelly, "15", "450")
	ssStocktake(t, svc, ssDay1, map[model.CutType]string{model.CutBelly: "0"})

	rep, err := svc.ComputeGrossMargin(ctx, "S001", ssDay1)
	mustNoErr(t, err, "ComputeGrossMargin(10-29)")
	belly := findPerCut(t, rep.PerCut, model.CutBelly)
	// ① 账面推演仍然是负数(它是提示)。
	if !belly.ExpectedRemainKg.IsNegative() {
		t.Errorf("★ 账面应有(提示)仍应是负数, got %s", belly.ExpectedRemainKg)
	}
	ssAssertReconBalanced(t, rep, "2026-10-29")

	// ② 期末量 = 0(实盘),不是 −5。
	svc.SetClock(fmClock(ssDay2))
	if _, err := svc.MaterializeCostChain(ctx, "S001", "2026-10-29"); err != nil {
		t.Fatalf("MaterializeCostChain: %v", err)
	}
	snap, ok := fmCutCostDaily(t, "S001", "2026-10-29", model.CutBelly)
	if !ok {
		t.Fatal("★ 10-29 应该有 belly 的成本日快照")
	}
	if !snap.ClosingKg.Equal(d("0")) {
		t.Errorf("★ 盘到 0 kg 的部位期末量必须是 0, got %s"+
			"(负的期末量会被链当成次日期初 ⇒ 门店看到'期初有负数')", snap.ClosingKg)
	}

	// ③ 次日期初 ≥ 0。
	rep2, err := svc.ComputeGrossMargin(ctx, "S001", ssDay2)
	mustNoErr(t, err, "ComputeGrossMargin(10-30)")
	ssAssertNoNegativeOpening(t, rep2, "2026-10-30")
	if !findPerCut(t, rep2.PerCut, model.CutBelly).OpeningKg.Equal(d("0")) {
		t.Errorf("★ 10-30 的 belly 期初应是 0(上日结余), got %s",
			findPerCut(t, rep2.PerCut, model.CutBelly).OpeningKg)
	}
}

// TestShortSell_PerCutShortage_NotRealShortSell 锁 **R2 + R3**。
//
// # 场景(复刻生产 店 00 2026-10-29)
//
//	进猪切 belly 5kg + 后腿 20kg(共 25kg);
//	卖 belly **8kg**(账面只有 5kg ⇒ 账面不足 3kg)+ 后腿 15kg;
//	盘点:belly = 0(售罄)、后腿 = 5。
//
// # 期望
//
//	① **总量够**(25kg ≥ 23kg)⇒ **不是真短卖** ⇒ total.short_sell_kg = 0;
//	② 分部位的账面不足**要看得见**(cut_short_sell_kg = 3kg)—— 只作提示;
//	③ **不传递到下一天**(次日的累计短卖 = 0)。
//
// # 判别力
//
//	把 R3 换成"分部位账面不足之和"⇒ ① 立刻红(会算成 3kg);
//	把 R2 去掉 ⇒ ② 立刻红。
func TestShortSell_PerCutShortage_NotRealShortSell(t *testing.T) {
	svc, ctx := ssSetup(t)
	svc.SetClock(fmClock(ssDay1))
	pigID := ssSeedPig(t, svc, ssDay1, "EB-1", map[model.CutType]string{
		model.CutBelly: "5", model.CutLegRear: "20",
	})
	ssSell(t, svc, ssDay1, pigID, model.CutBelly, "8", "240")
	ssSell(t, svc, ssDay1, pigID, model.CutLegRear, "15", "450")
	ssStocktake(t, svc, ssDay1, map[model.CutType]string{
		model.CutBelly: "0", model.CutLegRear: "5",
	})

	rep, err := svc.ComputeGrossMargin(ctx, "S001", ssDay1)
	mustNoErr(t, err, "ComputeGrossMargin(10-29)")

	// ① 总量够 ⇒ 不是真短卖。
	if !rep.Total.ShortSellKg.IsZero() {
		t.Errorf("★ 总量够(25kg 可售 ≥ 23kg 卖出)就不算短卖, got %s", rep.Total.ShortSellKg)
	}
	if rep.Total.ShortSell {
		t.Errorf("★ 总量够时 total.short_sell 必须是 false")
	}
	// ② 分部位的账面不足要看得见。
	belly := findPerCut(t, rep.PerCut, model.CutBelly)
	if !belly.CutShortSellKg.Equal(d("3")) {
		t.Errorf("★ belly 的账面不足应是 3kg(卖 8 − 有 5), got %s", belly.CutShortSellKg)
	}
	ssAssertReconBalanced(t, rep, "2026-10-29")

	// ③ 不传递到下一天。
	svc.SetClock(fmClock(ssDay2))
	rep2, err := svc.ComputeGrossMargin(ctx, "S001", ssDay2)
	mustNoErr(t, err, "ComputeGrossMargin(10-30)")
	if !rep2.Total.CarriedShortSellKg.IsZero() {
		t.Errorf("★ 分部位的账面不足**不传递**到下一天, got %s", rep2.Total.CarriedShortSellKg)
	}
	if !rep2.Total.ShortSellKg.IsZero() {
		t.Errorf("★ 次日也不该有短卖, got %s", rep2.Total.ShortSellKg)
	}
}

// TestShortSell_TotalShortSell_Carried 锁 **R3 + R4**。
//
// # 场景(复刻生产 店 00 2026-10-30)
//
//	10-29:进猪切 belly 20kg,盘点 20kg ⇒ 期末 20kg;
//	10-30:搬上台 50kg、卖 50kg(**当天没有盘点单**)⇒ 账面只有 20kg ⇒ **短卖 30kg**。
//
// # 期望
//
//	① 当日**总量**短卖 = 30kg(卖 50 − 可售 20)⇒ 报表描红;
//	② **结转到下一期**(累计短卖 30kg)⇒ 但**绝不回写成负库存**(次日期初 ≥ 0)。
//
// # 判别力
//
//	去掉 R4 的结转 ⇒ ② 立刻红(次日累计短卖 = 0);
//	去掉 R3 的总量口径(改成按部位)⇒ ① 立刻红。
//
// ⚠️ 10-30 **故意不录盘点单**:录了就会落成本日快照 ⇒ 次日报告会走**锚点**
//
//	(锚点从快照读累计短卖)⇒ 那条路依赖快照里那三列(见交付报告「需要父任务补的东西」)。
//	这里走**内存链**(从链的起点逐日重算)⇒ 结转逻辑不依赖落库。
func TestShortSell_TotalShortSell_Carried(t *testing.T) {
	svc, ctx := ssSetup(t)
	svc.SetClock(fmClock(ssDay1))
	// ★ **期初结转 = 链的起点**(见 cutChainBaseline:「期初生效日就是链的起点」)。
	//
	//	没有它 ⇒ 链从"被查的那天"起算(10-31)⇒ 链里**没有 10-30**
	//	⇒ prev.shortSell = 0 ⇒ 10-30 的 30kg 短卖传不到 10-31。
	//	而 10-30 又**故意不录盘点单**⇒ 也就没有快照能把累计值读回来
	//	⇒ 累计短卖必然断链(这正是本用例最初跑红的原因)。
	//
	//	生产上店 00 **有**期初结转(10-28 生效)⇒ 链真的逐日接续 ⇒ 这里照抄生产形态。
	//
	// ⚠️ 冷库量录 **0**：基线日本来就没有货
	//	⇒ 10-29 的「期初+进货 = 20」正好等于盘点单的 20 ⇒ 10-29 **不是**短卖
	//	(否则 10-31 的累计会变成 30+20=50,而不是 30)。
	ssOpening(t, svc, ssBaseDay, model.CutBelly, "0", "0")
	pigID := ssSeedPig(t, svc, ssDay1, "EB-1", map[model.CutType]string{model.CutBelly: "20"})
	ssStocktake(t, svc, ssDay1, map[model.CutType]string{model.CutBelly: "20"})

	// 10-30:搬上台 50kg(冷库 → 猪肉台)、卖 50kg ⇒ 账面只剩 20kg。
	svc.SetClock(fmClock(ssDay2))
	ssCounterLoad(t, svc, ssDay2, model.CutBelly, "50")
	ssSell(t, svc, ssDay2, pigID, model.CutBelly, "50", "1500")

	rep, err := svc.ComputeGrossMargin(ctx, "S001", ssDay2)
	mustNoErr(t, err, "ComputeGrossMargin(10-30)")
	// ① 当日总量短卖 = 30kg。
	if !rep.Total.ShortSellKg.Equal(d("30")) {
		t.Errorf("★ 当日总量短卖应是 30kg(卖 50 − 可售 20), got %s", rep.Total.ShortSellKg)
	}
	if !rep.Total.ShortSell {
		t.Errorf("★ 有短卖时 total.short_sell 必须是 true")
	}
	ssAssertReconBalanced(t, rep, "2026-10-30")

	// ② 结转到下一期,但期初仍 ≥ 0。
	svc.SetClock(fmClock(ssDay3))
	rep3, err := svc.ComputeGrossMargin(ctx, "S001", ssDay3)
	mustNoErr(t, err, "ComputeGrossMargin(10-31)")
	if !rep3.Total.CarriedShortSellKg.Equal(d("30")) {
		t.Errorf("★ 短卖应结转到下一期(累计 30kg), got %s", rep3.Total.CarriedShortSellKg)
	}
	ssAssertNoNegativeOpening(t, rep3, "2026-10-31")
}

// TestShortSell_AuditRejected 锁 **R6②③**(审计结转被拒 + 预检点名)。
//
// # 场景
//
//	10-29:进猪切 belly 20kg、盘点 20kg;
//	10-30:搬上台 50kg、卖 50kg、盘点 0kg ⇒ 账面只有 20kg ⇒ **短卖 30kg**。
//
// # 期望
//
//	① **审计结转被拒**(400)且消息点名是哪几天
//	(⚠️ 两天都有完整日终盘点 ⇒ 拒审的**唯一**理由就是短卖);
//	② 预检(short_sell_days)列出那天,且 Ready=false。
//
// # 判别力
//
//	去掉 R6 的审计拦截 ⇒ ① 立刻红(审计会通过)。
func TestShortSell_AuditRejected(t *testing.T) {
	svc, ctx := ssSetup(t)
	svc.SetClock(fmClock(ssDay1))
	pigID := ssSeedPig(t, svc, ssDay1, "EB-1", map[model.CutType]string{model.CutBelly: "20"})
	ssStocktake(t, svc, ssDay1, map[model.CutType]string{model.CutBelly: "20"})

	svc.SetClock(fmClock(ssDay2))
	ssCounterLoad(t, svc, ssDay2, model.CutBelly, "50")
	ssSell(t, svc, ssDay2, pigID, model.CutBelly, "50", "1500")
	ssStocktake(t, svc, ssDay2, map[model.CutType]string{model.CutBelly: "0"})

	svc.SetClock(fmClock(ssDay3))
	// ① 审计结转被拒(400),消息点名哪天。
	_, err := svc.AuditBusinessDays(ctx, "S001",
		service.BizAuditInput{CutoffDate: "2026-10-30"}, "u-1")
	if err == nil {
		t.Fatalf("★ 有短卖(10-30)的日子不该允许审计结转")
	}
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Errorf("★ 应是输入校验错(⇒ 400), got %v", err)
	}
	if !strings.Contains(err.Error(), "2026-10-30") {
		t.Errorf("★ 报错要点名短卖的是哪几天, got %q", err.Error())
	}
	// ② 预检列出短卖的那几天。
	pre, err := svc.PrecheckBizAudit(ctx, "S001", "2026-10-30")
	mustNoErr(t, err, "PrecheckBizAudit")
	if len(pre.ShortSellDays) != 1 || pre.ShortSellDays[0] != "2026-10-30" {
		t.Errorf("★ 预检应列出短卖的日子 [2026-10-30], got %v", pre.ShortSellDays)
	}
	if pre.Ready {
		t.Errorf("★ 有短卖时预检不该说可以审计结转")
	}
}

// TestShortSell_PrecheckReportsMissingStocktakeAndShortSellTogether 锁「两个坑一次报全」。
//
// # 场景(生产实测后改的)
//
//	10-29:进猪切 belly 20kg、盘点 20kg;
//	10-30:搬上台 50kg、卖 50kg、**不录盘点单**
//	⇒ 那天**既缺完整盘点**,**又**有短卖(账面只有 20kg 却卖了 50kg)。
//
// # 期望
//
//	预检**同时**给出:
//	① `missing_stocktake_days` 含 2026-10-30;
//	② `short_sell_days` 含 2026-10-30;
//	③ error 里**两样都提到**(缺盘点 + 短卖)。
//
// # 为什么必须"一次报全"
//
//	门店经常同时踩两个坑 ⇒ 只报一个 ⇒ 门店补完盘点再点一次 ⇒ 又冒出短卖
//	⇒ 来回好几轮才改得完 ⇒ 门店体验极差。
//
// # 判别力
//
//	把「短卖检查」挪到「缺盘点」的 early return **之后** ⇒ ② 立刻红(短卖日根本没算出来)。
func TestShortSell_PrecheckReportsMissingStocktakeAndShortSellTogether(t *testing.T) {
	svc, ctx := ssSetup(t)
	svc.SetClock(fmClock(ssDay1))
	pigID := ssSeedPig(t, svc, ssDay1, "EB-1", map[model.CutType]string{model.CutBelly: "20"})
	ssStocktake(t, svc, ssDay1, map[model.CutType]string{model.CutBelly: "20"})

	// 10-30:搬上台 50kg、卖 50kg,**故意不录盘点单**。
	svc.SetClock(fmClock(ssDay2))
	ssCounterLoad(t, svc, ssDay2, model.CutBelly, "50")
	ssSell(t, svc, ssDay2, pigID, model.CutBelly, "50", "1500")

	svc.SetClock(fmClock(ssDay3))
	pre, err := svc.PrecheckBizAudit(ctx, "S001", "2026-10-30")
	mustNoErr(t, err, "PrecheckBizAudit")

	if len(pre.MissingStocktakeDays) == 0 {
		t.Errorf("★ 10-30 没录盘点单 ⇒ 缺盘点清单里必须有它: %+v", pre)
	}
	if len(pre.ShortSellDays) == 0 {
		t.Errorf("★ 10-30 卖了 50kg 而账面只有 20kg ⇒ 短卖清单里必须有它"+
			"(缺盘点的 early return 把它吞掉了 ⇒ 门店得来回好几轮才改得完): %+v", pre)
	}
	if !strings.Contains(pre.Error, "短卖") {
		t.Errorf("★ 提示里要点名短卖: %q", pre.Error)
	}
	if pre.Ready {
		t.Errorf("★ 两个坑都在 ⇒ 不该说可以审计")
	}
}

// TestShortSell_ClampNonNegative 锁「读到负的期末 ⇒ 裁成 0(kg 与金额一起)」。
//
// # 为什么要有这个裁剪
//
//	新口径(R1)规定期末量永远 ≥0,但**存量快照**是旧口径写的 —— 里面真的有负数
//	(生产实测 店 00 2026-10-29:8 个部位为负,最大 −3.311kg)
//	⇒ 从快照/锚点读期末时不裁 ⇒ **负期初又漏回链里**
//	⇒ owner 说的"期初不能有负数"只对新建的门店成立 ⇒ 存量门店照旧犯。
//
// # 为什么金额一起裁
//
//	kg = 0 而金额是负数 ⇒ 可售金额为负 ⇒ 移动加权单价(金额÷kg)算不出来
//	⇒ 又变成"成本算不出来 ⇒ 毛利虚高"的老坑。
func TestShortSell_ClampNonNegative(t *testing.T) {
	// 负数 ⇒ kg 与金额都归零。
	kg, amt := service.ClampNonNegativeForTest(d("-5"), d("-100"))
	if !kg.IsZero() || !amt.IsZero() {
		t.Errorf("★ 负的期末应裁成 0/0,got %s / %s", kg, amt)
	}
	// 非负 ⇒ 原样保留(金额可以独立定价)。
	kg, amt = service.ClampNonNegativeForTest(d("5"), d("100"))
	if !kg.Equal(d("5")) || !amt.Equal(d("100")) {
		t.Errorf("★ 非负的期末不该被裁,got %s / %s", kg, amt)
	}
	// kg 非负但金额为负(定价兜底场景)⇒ kg 保留、金额照原样(由定价逻辑处理)。
	kg, amt = service.ClampNonNegativeForTest(d("5"), d("-100"))
	if !kg.Equal(d("5")) || !amt.Equal(d("-100")) {
		t.Errorf("★ kg 非负时不该动,got %s / %s", kg, amt)
	}
}

// TestShortSell_ShortSoldGoodsMustHaveCost 锁 **R5**(成本不许算不出来 ⇒ 毛利不许虚高)。
//
// # 场景(复刻生产 店 00 2026-10-30 的症状)
//
//	10-29:进猪切 belly 20kg、盘点 20kg ⇒ 期末 20kg;
//	10-30:搬上台 20kg、卖 20kg ⇒ 期末 0kg;
//	10-31:**账面一点货都没有**(期初 0)却卖了 10kg
//	      ⇒ 旧口径:可售数量 0 ⇒ 加权单价算不出来 ⇒ **销售成本 0 ⇒ 毛利率 100%**
//	      (生产实测 10-30 有 14 个部位 sales_cost_yuan = 0)
//	      ⇒ 新口径:短卖的那 10kg **照上期单价计成本** ⇒ 成本 > 0 ⇒ 毛利不虚高。
//
// # 期望
//
//	① 10-31 的 belly 销售成本 > 0(= 10kg × 上期单价 100 元/kg = 1000 元);
//	② 对账差额仍 = 0(加了短卖分量之后)。
//
// # 判别力
//
//	去掉"短卖按当期可用单价计成本"那一步 ⇒ ① 立刻红(销售成本 = 0)。
func TestShortSell_ShortSoldGoodsMustHaveCost(t *testing.T) {
	svc, ctx := ssSetup(t)
	svc.SetClock(fmClock(ssDay1))
	pigID := ssSeedPig(t, svc, ssDay1, "EB-1", map[model.CutType]string{model.CutBelly: "20"})
	ssStocktake(t, svc, ssDay1, map[model.CutType]string{model.CutBelly: "20"})

	// 10-30:把 20kg 全搬上台并卖掉 ⇒ 期末 0kg。
	svc.SetClock(fmClock(ssDay2))
	ssCounterLoad(t, svc, ssDay2, model.CutBelly, "20")
	ssSell(t, svc, ssDay2, pigID, model.CutBelly, "20", "600")
	ssStocktake(t, svc, ssDay2, map[model.CutType]string{model.CutBelly: "0"})

	// 10-31:账面一点货都没有,却卖了 10kg。
	svc.SetClock(fmClock(ssDay3))
	ssSell(t, svc, ssDay3, pigID, model.CutBelly, "10", "300")

	rep, err := svc.ComputeGrossMargin(ctx, "S001", ssDay3)
	mustNoErr(t, err, "ComputeGrossMargin(10-31)")
	belly := findPerCut(t, rep.PerCut, model.CutBelly)

	// ① 短卖的那部分必须有成本(上期单价 = 2000 元 ÷ 20kg = 100 元/kg)。
	if !belly.SalesCostYuan.IsPositive() {
		t.Errorf("★ 短卖的货必须有成本(卖 10kg 却算出成本 %s)⇒ 毛利虚高", belly.SalesCostYuan)
	}
	if !belly.SalesCostYuan.Equal(d("1000")) {
		t.Errorf("★ 10-31 的销售成本应是 1000 元(10kg × 上期单价 100), got %s", belly.SalesCostYuan)
	}
	if belly.CostIncomplete {
		t.Errorf("★ 有链传下来的上期单价 ⇒ 成本算得出来,不该标 cost_incomplete")
	}
	// ② 对账恒等式仍然闭合。
	ssAssertReconBalanced(t, rep, "2026-10-31")
	// 当日总量短卖 = 10kg(卖 10 − 可售 0)。
	if !rep.Total.ShortSellKg.Equal(d("10")) {
		t.Errorf("★ 10-31 的当日短卖应是 10kg, got %s", rep.Total.ShortSellKg)
	}
}
