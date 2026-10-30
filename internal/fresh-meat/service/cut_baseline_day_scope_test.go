// 2026-10-30:锁「**基线日整天由门店定义**」+「**纯基线日不结转业务**」。
//
// # 背景(生产实测:店 00 / 2026-10-30)
//
//	门店口径(owner 原话):「我把 2026-10-28 作为期初,
//	2026-10-29 作为第一天正常开始使用这套鲜肉子系统」。
//	⇒ 期初生效日 = 链的起点 = **门店重新定义库存的那一天**。
//
// # 两条规则
//
//	① **基线日是「天」级别的**:门店在生效日重定义了**整天**的库存
//	  ⇒ 那天的期初**全部**来自门店(录了 = 门店录的量;没录 = 0)
//	  ⇒ **不接**快照 / 链式接续 / 实时结余链(链外的负账面值不许漏回来)。
//
//	② **纯基线日不结转业务**(owner 2026-10-30 决策):
//	  纯基线日 = 期初生效日 **且** 那天**没有任何营业日动作**(没录猪/没搬台/没盘点)
//	  ⇒ 那天的 POS 销售是**上线前那套账**的货:系统里没有对应的进货/搬台/盘点
//	  ⇒ 成本**算不出来**,而且那天的销售**根本不该在这条链里**
//	  ⇒ 纯基线日**只带期初**:销售/统收/报损一律不进链
//	  ⇒ 它的结余 = 期初 ⇒ 链的第一天的期初 = 门店录的值 ⇒ **不再短卖**。
//
// # 判别力
//
//	① 把基线日改回按部位判断(旧实现)⇒ 基线日接上链外的脏快照
//	  ⇒ 出现负期初 ⇒ TestCutOpening_BaselineDay_WholeDayFromStore 立刻红。
//	② 去掉「纯基线日清空销售」⇒ 基线日当天短卖 ⇒ 结余变负
//	  ⇒ TestCutCost_PureBaselineDay_SalesNotInChain 立刻红。
package service_test

import (
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

// TestCutOpening_BaselineDay_WholeDayFromStore 锁「**基线日整天由门店定义**」。
//
// # 场景
//
//	期初(基线日) = 2026-10-05:门店**只录了 belly** 100kg @ 18 元。
//	**上一营业日**(10-04)有一份**链外的脏快照**(猪手结余 −9.2kg / −146.51 元)
//	—— 生产实测(店 00 / 2026-10-30):10-28 的猪耳 −1.279 / 猪肺 −17.64 / 猪手 −9.2 kg
//	**都不是**上一日快照带来的(那些快照已被清掉,报表照样显示负值)
//	⇒ 泄漏源是**实时结余链**(冷链)与**旧链的脏快照**。
//	⇒ 这里用**脏快照**当泄漏源(它能活过"改期初同事务清链外旧账"的清理:
//	   清理只删**链起点之前**的记录,而这份快照是**清完之后**才落库的 —— 生产上也是这个次序)。
//
// # 期望
//
//	基线日当天**任何部位**的期初都**不是负数**
//	⇒ 链外的脏快照没漏回链里;belly 的期初 = 门店录的 100kg。
//
// # ⚠️ 为什么断言「没有任何负期初」而不是「猪手这一行 = 0」
//
//	纯基线日**不结转业务**(见文件头规则②)⇒ 基线日当天的销售被清空
//	⇒ 猪手当天**可能根本没有行**(没期初、没销售、没产出)
//	⇒ 断言「猪手这一行的期初 = 0」在正确实现下会**误报**(行不存在)。
//	⇒ 改成:**只要有行,期初就不许是负数** —— 漏回时猪手会带着 −9.2 出现 ⇒ 一样红。
//
// # 判别力
//
//	把基线日改回按部位判断(旧实现)⇒ 猪手没录期初 ⇒ 接上一日的脏快照
//	⇒ 猪手期初 = −9.2kg ⇒ 本用例立刻红。
func TestCutOpening_BaselineDay_WholeDayFromStore(t *testing.T) {
	baseDay := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)

	svc, ctx := fmChainSetup(t, map[string][]map[string]any{
		"2026-10-05": {fmCubeSalesRow("P-BELLY", "10", "180")},
	})
	// ⚠️ 时钟要先拨到目标日之后:补录(期初)不许填**未来**日期。
	svc.SetClock(fmClock(baseDay.AddDate(0, 0, 1)))
	// 门店的期初结转(基线日 = 2026-10-05):**只录 belly**。
	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-05",
			ColdStoreKg: d("100"), UnitPriceYuan: d("18")},
	}); err != nil {
		t.Fatalf("SaveStockOpenings: %v", err)
	}
	// ⚠ 链外的脏快照(**上一营业日** 10-04):期末是负的账面短卖。
	//   门店压根没录过猪手 ⇒ 基线日**不许**把它接成猪手的期初。
	fmChainSeedSnapshot(t, "S001", "2026-10-04", model.CutTrotter, "-9.2", "-146.51", "15.9249")

	rep, err := svc.ComputeGrossMargin(ctx, "S001", baseDay)
	mustNoErr(t, err, "ComputeGrossMargin(基线日)")

	for _, r := range rep.PerCut {
		if r.OpeningKg.IsNegative() {
			t.Errorf("★ 基线日整天由门店定义 ⇒ 任何部位的期初都不该是负数"+
				"(链外的脏快照漏回链里了): %s 期初 = %s", r.CutType, r.OpeningKg)
		}
	}
	belly := fmCutRowFor(t, rep, model.CutBelly)
	if !belly.OpeningKg.Equal(d("100")) {
		t.Errorf("★ belly 的期初应是门店录的 100kg, got %s", belly.OpeningKg)
	}
}

// TestCutCost_PureBaselineDay_SalesNotInChain 锁「**纯基线日不结转业务**」。
//
// # 场景(复刻生产:店 00)
//
//	期初(基线日) = 2026-10-28:门店录 **后腿 10kg @ 16 元**。
//	当天 POS 卖了 **172kg**(营收 3632.70 元)—— 那是**上线前那套账**的货:
//	系统里**没有**对应的进猪 / 搬台 / 盘点 ⇒ 那天**没有任何营业日动作**
//	⇒ 纯基线日。
//	10-29(链的第一天)正常卖了 10kg。
//
// # 期望
//
//	① 基线日当天的报表里**没有销售**:销售 kg / 营收都是 0。
//	② 基线日的**结余 = 期初**(10kg)⇒ **不是** 10 − 172 = −162kg 的短卖。
//	③ 链的第一天(10-29)的期初 = **10kg**(门店录的值)⇒ 门店不再看到负数。
//
// # 判别力
//
//	去掉「纯基线日清空销售」⇒ 基线日当天短卖 ⇒ 结余 = −162kg
//	⇒ ②③ 立刻红(10-29 的期初变成 −162kg)。
func TestCutCost_PureBaselineDay_SalesNotInChain(t *testing.T) {
	baseDay := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	nextDay := baseDay.AddDate(0, 0, 1) // 2026-10-29

	svc, ctx := fmChainSetup(t, map[string][]map[string]any{
		"2026-10-28": {fmCubeSalesRow("P-LEG", "172", "3632.70")},
		"2026-10-29": {fmCubeSalesRow("P-LEG", "10", "210")},
	})
	// 后腿的货号映射(fmChainSetup 默认只登记 belly)。
	if _, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{
			CutType: model.CutLegRear, CubeProductID: "P-LEG"}); err != nil {
		t.Fatalf("CreateBranchCutMapping leg_rear: %v", err)
	}
	// ⚠️ 时钟拨到链的第一天:期初不许填**未来**日期,物化也不许跑到未来。
	svc.SetClock(fmClock(nextDay))
	// 门店的期初结转(基线日 = 2026-10-28):只有后腿 10kg。
	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutLegRear, BizDate: "2026-10-28",
			ColdStoreKg: d("10"), UnitPriceYuan: d("16")},
	}); err != nil {
		t.Fatalf("SaveStockOpenings: %v", err)
	}

	// ---- ① 基线日当天的销售不进链 ----
	rep, err := svc.ComputeGrossMargin(ctx, "S001", baseDay)
	mustNoErr(t, err, "ComputeGrossMargin(基线日)")
	for _, r := range rep.PerCut {
		if !r.SoldKg.IsZero() || !r.BulkSoldKg.IsZero() ||
			!r.WasteKg.IsZero() || !r.RevenueYuan.IsZero() {
			t.Errorf("★ 纯基线日只带期初,当天的销售不进链: %s 销量=%s 营收=%s",
				r.CutType, r.SoldKg, r.RevenueYuan)
		}
	}
	leg := fmCutRowFor(t, rep, model.CutLegRear)
	if !leg.OpeningKg.Equal(d("10")) {
		t.Errorf("★ 基线日的期初应是门店录的 10kg, got %s", leg.OpeningKg)
	}

	// ---- ② 基线日的结余 = 期初(不短卖) ----
	if _, err := svc.MaterializeCostChain(ctx, "S001", "2026-10-28"); err != nil {
		t.Fatalf("MaterializeCostChain: %v", err)
	}
	snap, ok := fmCutCostDaily(t, "S001", "2026-10-28", model.CutLegRear)
	if !ok {
		t.Fatal("★ 基线日的成本日快照必须有后腿这一行")
	}
	if !snap.ClosingKg.Equal(d("10")) {
		t.Errorf("★ 纯基线日的结余应 = 期初 10kg(销售不进链), got %s"+
			"(销售进了链 ⇒ 当天短卖 ⇒ 负数被传给链的第一天)", snap.ClosingKg)
	}

	// ---- ③ 链的第一天:期初 = 门店录的值 ----
	next, ok := fmCutCostDaily(t, "S001", "2026-10-29", model.CutLegRear)
	if !ok {
		t.Fatal("★ 链的第一天必须有后腿这一行")
	}
	if !next.OpeningKg.Equal(d("10")) {
		t.Errorf("★ 链的第一天(10-29)的期初应是门店录的 10kg, got %s", next.OpeningKg)
	}
}
