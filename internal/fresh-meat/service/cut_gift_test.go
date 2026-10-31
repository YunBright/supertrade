package service_test

// 2026-10-31(owner 拍板):**搭赠**(买肉送猪肝这类「有成本、无收入、没过称」的出库)。
//
// # 背景(生产实测 店 00,已查证 —— 别重查)
//
//	10-29:进猪切出猪肝 3.030kg(移动加权单价 13.1689)⇒ 期末 3.030kg / 39.90 元
//	10-30:盘点猪肝 = **0 kg**、`line_sales_by_pigs` 里 liver 销售行 = **0 条**
//	      ⇒ 这 3.03kg 是**搭赠**出去的(没过称、没过账)
//	⇒ 账上:销售成本 39.90 元(但猪肝营收 0)⇒ 报表像"卖猪肝亏了 39.9 元"
//	⇒ 而且这 3.03kg 进了当天的短卖口径。
//
// # 锁的口径(owner 2026-10-31 拍板)
//
//	G1 搭赠是链上的**第四个出口**(销售/统收/报损之外)
//	   ⇒ 从可售量里扣掉(否则账面推演比实盘多 ⇒ 被当成盘亏);
//	G2 搭赠成本**单列一行**、并**扣毛利**(销售成本只含"真卖掉的货"的成本);
//	G3 重量**没过称** ⇒ 用「**倒推建议值 + 门店确认**」(缺口 = 账面应有 − 实盘);
//	G4 确认搭赠**不改变总毛利**(只改变归因)—— 这是本轮最容易改错的地方;
//	G5 搭赠**不是损耗** ⇒ 不混进报损(waste_type 区分)。
//
// # 为什么这些用例必须走**公开 API + 真实 SQLite**
//
//	引擎接线错了(搭赠没进链/建议值算错/恒等式破了)也会红
//	⇒ 锁的是**口径**,不是实现细节。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

// 本文件用的两个营业日(2026-10-29 / 10-30 14:00 +08)。
var (
	gfDay1 = time.Date(2026, 10, 29, 6, 0, 0, 0, time.UTC)
	gfDay2 = time.Date(2026, 10, 30, 6, 0, 0, 0, time.UTC)
)

// gfLiverSKU 猪肝的货号。
const gfLiverSKU = "P-LIVER"

// gfSetup 起服务 + 登记猪肝的货号映射。
func gfSetup(t *testing.T) (*service.Service, context.Context) {
	t.Helper()
	svc, _ := setupTestService(t)
	ctx := context.Background()
	_, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutLiver, CubeProductID: gfLiverSKU})
	mustNoErr(t, err, "CreateBranchCutMapping")
	return svc, ctx
}

// gfSeedLiverPig 10-29 进一头猪(100kg @ 2 元/kg ⇒ 总成本 200 元),切出猪肝 10kg。
//
// ⇒ 猪肝的入库金额 = 200 × 10/10 = **200 元** ⇒ 入库单价 **20 元/kg**。
//
// ⚠️ 分割建议(基线/LLM)在这个环境里是空的 ⇒ 引擎不接管入库量
// ⇒ 入库量 = 这里补录的重量 ⇒ 用例里的数是确定的。
func gfSeedLiverPig(t *testing.T, svc *service.Service) string {
	t.Helper()
	pig, err := svc.RecordWholePig(context.Background(), "S001", model.RecordWholePigInput{
		EarTag: "GF-1", GrossWeightKg: d("100"), PurchaseUnitPriceYuan: d("2"),
		SupplierID: "SUP-1", ArrivedAt: gfDay1,
	}, "u-1")
	mustNoErr(t, err, "RecordWholePig")
	_, err = svc.RecordPigCut(context.Background(), "S001", model.RecordPigCutInput{
		PigID: pig.ID, CutType: model.CutLiver, Barcode: "B-GF-1",
		WeightKg: d("10"), ExpiresAt: gfDay1.Add(48 * time.Hour), CubeProductID: gfLiverSKU,
	}, "u-1")
	mustNoErr(t, err, "RecordPigCut")
	return pig.ID
}

// gfStocktakeLiver 录 10-30 的日终盘点:猪肝 **0 kg**(全送出去了)。
//
// ⚠️ 归属日 = 服务端时钟所在那天 ⇒ 调用前必须把时钟拨到 10-30。
func gfStocktakeLiver(t *testing.T, svc *service.Service) {
	t.Helper()
	_, err := svc.RecordPorkCutsStocktake(context.Background(), "S001",
		model.RecordPorkCutsStocktakeInput{
			IsComplete: true,
			Cuts: []model.CutInput{{
				CutType: model.CutLiver, CubeProductID: gfLiverSKU, ActualRemainKg: d("0"),
			}},
		}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake")
}

// gfSell 记一笔零售销售(本地 POS 事件)。
//
// ⚠️ **必须带 pigID**:POS 事件在"当天找不到猪"时会把整行**丢掉**
// ⇒ 那样销售根本进不了账 ⇒ 用例会"莫名其妙地没有短卖"。
func gfSell(t *testing.T, svc *service.Service, day time.Time, pigID string,
	cut model.CutType, qty, amount string,
) {
	t.Helper()
	_, err := svc.OnSaleCompleted(context.Background(), service.SaleCompletedPayload{
		SaleID:      "SO-GF-" + string(cut) + "-" + day.Format("0102") + "-" + qty,
		BranchID:    "S001",
		CompletedAt: day.Add(time.Hour),
		Lines: []service.SaleLine{{
			LineID: "L-GF-" + string(cut) + "-" + qty, SKUID: gfLiverSKU, FreshType: "meat",
			Qty: d(qty), AmountYuan: d(amount), PigID: pigID, CutType: cut,
		}},
	})
	mustNoErr(t, err, "OnSaleCompleted "+string(cut))
}

// gfMaterializeDay1 物化 10-29 的成本日快照。
//
// ⚠️ 为什么必须物化:成本链是**逐日接续**的(今天的期初 = 昨天的期末),
//
//	而接续靠**上一营业日的快照当锚点**(见 cutChainAnchor)
//	⇒ 不物化 ⇒ 10-30 会被当成"链的第一天"算 ⇒ 猪肝的期初 = 0
//	⇒ 后面所有断言都会失真。
//
// ⇒ 生产上也是这样:每天的 cut_cost_daily 快照落库,次日直接接。
func gfMaterializeDay1(t *testing.T, svc *service.Service, ctx context.Context) {
	t.Helper()
	if _, err := svc.MaterializeCostChain(ctx, "S001", "2026-10-29"); err != nil {
		t.Fatalf("MaterializeCostChain(10-29): %v", err)
	}
}

// gfGift 记一条**搭赠**出库(10-30)。
func gfGift(t *testing.T, svc *service.Service, qty string) {
	t.Helper()
	_, err := svc.RecordWasteLog(context.Background(), "S001", model.RecordWasteLogInput{
		CutType:   model.CutLiver,
		QtyKg:     d(qty),
		Reason:    "搭赠",
		WasteType: model.WasteTypeGift,
	}, "u-1")
	mustNoErr(t, err, "RecordWasteLog(gift)")
}

// gfAssertReconBalanced 断言对账行仍然闭合(加了搭赠分量之后也不能破)。
func gfAssertReconBalanced(t *testing.T, rep *service.GrossMarginReport, day string) {
	t.Helper()
	if rep.Reconciliation.DiffYuan.Abs().GreaterThan(d("0.01")) {
		t.Errorf("★ %s 对账差额应仍为 0(加了搭赠分量之后), got %s", day, rep.Reconciliation.DiffYuan)
	}
}

// TestGift_NotRecorded_ShowsAsVariance 锁 **G1 的反面**:没记搭赠 ⇒ 门店看到"盘亏"。
//
// # 场景(复刻生产 店 00 2026-10-30 猪肝)
//
//	10-29:进猪切猪肝 10kg(单价 20 元/kg)⇒ 10-30 期初 10kg / 200 元;
//	10-30:盘点猪肝 = 0(搭赠出去了),没有任何销售记录。
//
// # 期望
//
//	① **盘亏 10kg**(variance = −10)—— 门店会以为自己盘错了/丢货了;
//	② 搭赠成本 = 0(还没记);
//	③ 销售成本 = 200(把整批货的成本都算成了"卖掉的成本")⇒ 营收 0、毛利 −200;
//	④ 对账仍然闭合(差额 0)—— 因为差额是恒等式,不是"有没有盘亏"的检测器;
//	⑤ **建议值 = 10kg**(账面应有 10 − 实盘 0)。
//
// # 判别力
//
//	把"搭赠缺口"的计算去掉 ⇒ ⑤ 立刻红(建议列表为空)。
func TestGift_NotRecorded_ShowsAsVariance(t *testing.T) {
	svc, ctx := gfSetup(t)
	svc.SetClock(fmClock(gfDay1))
	gfSeedLiverPig(t, svc)
	svc.SetClock(fmClock(gfDay2))
	gfMaterializeDay1(t, svc, ctx)
	gfStocktakeLiver(t, svc)

	rep, err := svc.ComputeGrossMargin(ctx, "S001", gfDay2)
	mustNoErr(t, err, "ComputeGrossMargin(10-30)")
	liver := findPerCut(t, rep.PerCut, model.CutLiver)

	// ① 盘亏 10kg。
	if liver.VarianceKg == nil {
		t.Fatalf("★ 有完整盘点 ⇒ 猪肝应有 variance_kg")
	}
	if !liver.VarianceKg.Equal(d("-10")) {
		t.Errorf("★ 没记搭赠时猪肝应显示盘亏 10kg(实盘 0 − 应有 10), got %s", liver.VarianceKg)
	}
	// ② 还没记 ⇒ 搭赠成本 0。
	if !liver.GiftCostYuan.IsZero() || !liver.GiftKg.IsZero() {
		t.Errorf("★ 没记搭赠时搭赠量/成本应是 0, got %s kg / %s 元", liver.GiftKg, liver.GiftCostYuan)
	}
	// ③ 销售成本把整批货都算进去了(营收 0、毛利 −200)。
	if !liver.SalesCostYuan.Equal(d("200")) {
		t.Errorf("★ 没记搭赠时销售成本应是 200(整批 10kg@20), got %s", liver.SalesCostYuan)
	}
	if !liver.GrossMarginYuan.Equal(d("-200")) {
		t.Errorf("★ 没记搭赠时毛利应是 −200(营收 0 − 成本 200), got %s", liver.GrossMarginYuan)
	}
	gfAssertReconBalanced(t, rep, "2026-10-30")

	// ⑤ 建议值 = 10kg(账面应有 10 − 实盘 0)。
	sugg, err := svc.ListGiftSuggestions(ctx, "S001", gfDay2)
	mustNoErr(t, err, "ListGiftSuggestions")
	if sugg.Count != 1 || len(sugg.Suggestions) != 1 {
		t.Fatalf("★ 应有 1 条搭赠建议(猪肝), got %d", sugg.Count)
	}
	s := sugg.Suggestions[0]
	if s.CutType != model.CutLiver {
		t.Errorf("★ 建议的部位应是 liver, got %s", s.CutType)
	}
	if !s.GapKg.Equal(d("10")) {
		t.Errorf("★ 建议缺口应是 10kg(应有 10 − 实盘 0), got %s", s.GapKg)
	}
	if !s.ExpectedKg.Equal(d("10")) || !s.ActualKg.Equal(d("0")) {
		t.Errorf("★ 建议应带上应有 10kg / 实盘 0kg, got %s / %s", s.ExpectedKg, s.ActualKg)
	}
	if !s.SuggestCostYuan.Equal(d("200")) {
		t.Errorf("★ 建议成本应是 200(10kg @ 20), got %s", s.SuggestCostYuan)
	}
	if s.CubeProductID != gfLiverSKU {
		t.Errorf("★ 建议应带上主货号 %s(门店不认货号 ⇒ 服务端替它补), got %q", gfLiverSKU, s.CubeProductID)
	}
}

// TestGift_Confirmed_SeparatesCostAndClearsVariance 锁 **G1 + G2 + G4**。
//
// # 场景:同上,但门店**确认了**这 10kg 是搭赠。
//
// # 期望
//
//	① **盘亏消失**(variance = 0)—— 账面与实物对上了;
//	② **搭赠成本单列** = 200 元;**销售成本 = 0**(成本被剥到搭赠那一行);
//	③ **总毛利不变**(仍是 −200)—— 确认搭赠只改归因,不改钱(G4);
//	④ 对账仍然闭合;
//	⑤ **建议值消失**(缺口归零 ⇒ 不会反复催门店);
//	⑥ 短卖 = 0(搭赠是正常出库,不是"卖超了库存")。
//
// # 判别力
//
//   - 把搭赠从"流出"里去掉 ⇒ ⑥ 立刻红(短卖会算成 10kg);
//   - 不从销售成本里减搭赠成本 ⇒ ② 立刻红(销售成本还是 200);
//   - 不减搭赠成本 ⇒ ③ 立刻红(毛利变成 −400)。
func TestGift_Confirmed_SeparatesCostAndClearsVariance(t *testing.T) {
	svc, ctx := gfSetup(t)
	svc.SetClock(fmClock(gfDay1))
	gfSeedLiverPig(t, svc)
	svc.SetClock(fmClock(gfDay2))
	gfMaterializeDay1(t, svc, ctx)
	gfStocktakeLiver(t, svc)

	// 确认前的总毛利(用来比 G4)。
	before, err := svc.ComputeGrossMargin(ctx, "S001", gfDay2)
	mustNoErr(t, err, "ComputeGrossMargin(before)")

	gfGift(t, svc, "10")

	rep, err := svc.ComputeGrossMargin(ctx, "S001", gfDay2)
	mustNoErr(t, err, "ComputeGrossMargin(after)")
	liver := findPerCut(t, rep.PerCut, model.CutLiver)

	// ① 盘亏消失。
	if liver.VarianceKg == nil {
		t.Fatalf("★ 有完整盘点 ⇒ 猪肝应有 variance_kg")
	}
	if !liver.VarianceKg.Equal(d("0")) {
		t.Errorf("★ 确认搭赠后猪肝不该再报盘亏, got %s", liver.VarianceKg)
	}
	// ② 搭赠成本单列 + 销售成本剥干净。
	if !liver.GiftKg.Equal(d("10")) {
		t.Errorf("★ 搭赠量应是 10kg, got %s", liver.GiftKg)
	}
	if !liver.GiftCostYuan.Equal(d("200")) {
		t.Errorf("★ 搭赠成本应是 200(10kg @ 20), got %s", liver.GiftCostYuan)
	}
	if !liver.SalesCostYuan.IsZero() {
		t.Errorf("★ 销售成本应是 0(猪肝没卖;成本已剥到搭赠那一行), got %s", liver.SalesCostYuan)
	}
	if !rep.Total.GiftCostYuan.Equal(d("200")) {
		t.Errorf("★ 全店搭赠成本应是 200, got %s", rep.Total.GiftCostYuan)
	}
	// ③ 总毛利不变(G4)。
	if !rep.Total.GrossMarginYuan.Equal(before.Total.GrossMarginYuan) {
		t.Errorf("★ 确认搭赠只改归因、不改毛利:确认前 %s,确认后 %s",
			before.Total.GrossMarginYuan, rep.Total.GrossMarginYuan)
	}
	if !rep.Total.GrossMarginYuan.Equal(d("-200")) {
		t.Errorf("★ 总毛利应是 −200(白送 10kg 猪肝的成本), got %s", rep.Total.GrossMarginYuan)
	}
	// ④ 对账闭合。
	gfAssertReconBalanced(t, rep, "2026-10-30")
	// ⑤ 建议值消失。
	sugg, err := svc.ListGiftSuggestions(ctx, "S001", gfDay2)
	mustNoErr(t, err, "ListGiftSuggestions")
	if sugg.Count != 0 || len(sugg.Suggestions) != 0 {
		t.Errorf("★ 确认搭赠后不该再提示(缺口已归零), got %d 条", sugg.Count)
	}
	// ⑥ 短卖 = 0。
	if !rep.Total.ShortSellKg.IsZero() {
		t.Errorf("★ 搭赠是正常出库,不该算短卖, got %s", rep.Total.ShortSellKg)
	}
}

// TestGift_GiftIsNotLoss 锁 **G5**:搭赠**不是损耗** ⇒ 不混进报损。
//
// # 期望
//
//	① 记一条报损(loss)⇒ 报损成本 = 它的量 × 单价,搭赠成本 = 0;
//	② 记一条搭赠(gift)⇒ 搭赠成本 = 它的量 × 单价,报损成本 = 0。
func TestGift_GiftIsNotLoss(t *testing.T) {
	svc, ctx := gfSetup(t)
	svc.SetClock(fmClock(gfDay1))
	gfSeedLiverPig(t, svc)
	svc.SetClock(fmClock(gfDay2))
	gfMaterializeDay1(t, svc, ctx)
	// 一条真报损 2kg。
	_, err := svc.RecordWasteLog(context.Background(), "S001", model.RecordWasteLogInput{
		CutType: model.CutLiver, CubeProductID: gfLiverSKU, QtyKg: d("2"), Reason: "破损",
	}, "u-1")
	mustNoErr(t, err, "RecordWasteLog(loss)")

	rep, err := svc.ComputeGrossMargin(ctx, "S001", gfDay2)
	mustNoErr(t, err, "ComputeGrossMargin")
	liver := findPerCut(t, rep.PerCut, model.CutLiver)
	if !liver.WasteKg.Equal(d("2")) {
		t.Errorf("★ 报损量应是 2kg, got %s", liver.WasteKg)
	}
	if !liver.WasteCostYuan.Equal(d("40")) {
		t.Errorf("★ 报损成本应是 40(2kg @ 20), got %s", liver.WasteCostYuan)
	}
	if !liver.GiftKg.IsZero() || !liver.GiftCostYuan.IsZero() {
		t.Errorf("★ 只记了报损 ⇒ 搭赠应是 0, got %s kg / %s 元", liver.GiftKg, liver.GiftCostYuan)
	}
	gfAssertReconBalanced(t, rep, "2026-10-30")
}

// TestGift_ShortSellCountsGift 锁「**搭赠计入总流出**」⇒ 短卖反映**真实缺口**。
//
// # 场景
//
//	10-29:进猪切猪肝 10kg(入库 10kg @ 20 元/kg)⇒ 物化快照;
//	10-30:卖出 **12kg**(账面只有 10kg ⇒ 本来就短 2kg)+ 搭赠 **5kg**;
//	      盘点猪肝 = 0(柜台和冷库都没了)。
//
// # 期望
//
//	当天总流出 = 12(卖) + 5(搭赠) = 17kg,总可售 = 10kg
//	⇒ **真实缺口 = 7kg** ⇒ short_sell_kg = 7。
//
//	⚠️ 如果搭赠**不**计入总流出 ⇒ 短卖只算 12 − 10 = 2kg
//	⇒ 那 5kg 的搭赠就"藏"掉了 ⇒ 门店看到的缺口比真实的小 ⇒ **漏报**。
//
// # 判别力
//
//	把搭赠从 soldTotal 里去掉 ⇒ 短卖变 2kg ⇒ 用例立刻红。
func TestGift_ShortSellCountsGift(t *testing.T) {
	svc, ctx := gfSetup(t)
	svc.SetClock(fmClock(gfDay1))
	gfSeedLiverPig(t, svc)
	svc.SetClock(fmClock(gfDay2))
	gfMaterializeDay1(t, svc, ctx)

	// 卖 12kg(账面只有 10kg ⇒ 本来就短 2kg)。
	gfSell(t, svc, gfDay2, "GF-1", model.CutLiver, "12", "360")
	// 搭赠 5kg。
	gfGift(t, svc, "5")
	gfStocktakeLiver(t, svc)

	rep, err := svc.ComputeGrossMargin(ctx, "S001", gfDay2)
	mustNoErr(t, err, "ComputeGrossMargin(10-30)")
	if !rep.Total.ShortSellKg.Equal(d("7")) {
		t.Errorf("★ 总流出 17kg(卖 12 + 搭赠 5)− 可售 10kg = 真实缺口 7kg, got %s"+
			"(搭赠不计入流出 ⇒ 只算 12 − 10 = 2kg ⇒ 那 5kg 被藏掉 ⇒ 漏报)", rep.Total.ShortSellKg)
	}
	gfAssertReconBalanced(t, rep, "2026-10-30")
}

// TestGift_ColdChainReducesBalance 锁「**搭赠要扣冷库余额链**」。
//
// # 场景
//
//	10-29(= 链的起点):期初结转猪肝 10kg @ 20 元/kg(全在冷库里);
//	当天从冷库直接送出 3kg 当搭赠(没过称、没过账);盘点柜台实盘 0。
//
// # 为什么必须扣
//
//	搭赠的货**真的离店了** ⇒ 冷库余额必须减
//	⇒ 不减 ⇒ 链上一直留着这批货 ⇒ **次日期初虚高**
//	  ⇒ 那 3kg 会一直"存在"店里,越滚越大 ⇒ 成本链被污染。
//
// # 期望
//
//	10-29 的期末(链上的冷库余额)= 10 − 3 = **7kg**,不是 10kg。
//
// # 判别力
//
//	把搭赠从冷库链里去掉 ⇒ 期末会变回 10kg ⇒ 用例立刻红。
func TestGift_ColdChainReducesBalance(t *testing.T) {
	svc, ctx := gfSetup(t)
	svc.SetClock(fmClock(gfDay1))
	// 期初结转(链的起点):猪肝 10kg @ 20 元/kg。
	_, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{{
		CutType: model.CutLiver, BizDate: "2026-10-29",
		ColdStoreKg: d("10"), UnitPriceYuan: d("20"),
	}})
	mustNoErr(t, err, "SaveStockOpenings")
	// 当天搭赠 3kg(从冷库直接送出)。
	gfGift(t, svc, "3")
	// 盘点:柜台实盘 0(货不在柜台上)。
	gfStocktakeLiver(t, svc)
	// 物化 10-29 的成本日快照 ⇒ 看链上的期末(冷库余额)。
	if _, err := svc.MaterializeCostChain(ctx, "S001", "2026-10-29"); err != nil {
		t.Fatalf("MaterializeCostChain(10-29): %v", err)
	}
	snap, ok := fmCutCostDaily(t, "S001", "2026-10-29", model.CutLiver)
	if !ok {
		t.Fatal("★ 10-29 应该有猪肝的成本日快照")
	}
	if !snap.ClosingKg.Equal(d("7")) {
		t.Errorf("★ 搭赠 3kg 后冷库余额应是 7kg(10 − 3), got %s"+
			"(不减 ⇒ 那 3kg 一直留在链上 ⇒ 次日期初虚高)", snap.ClosingKg)
	}
}

// TestGift_Validation 锁搭赠录入的**验参**。
//
//	① 既没部位也没货号 ⇒ 400(不然这行会**静默丢失**:链按货号归集);
//	② waste_type 非法 ⇒ 400;
//	③ 只给部位、不给货号 ⇒ **自动补主货号** ⇒ 进得了链(门店不认货号)。
func TestGift_Validation(t *testing.T) {
	svc, ctx := gfSetup(t)
	svc.SetClock(fmClock(gfDay1))
	gfSeedLiverPig(t, svc)
	svc.SetClock(fmClock(gfDay2))

	// ① 既没部位也没货号。
	_, err := svc.RecordWasteLog(context.Background(), "S001", model.RecordWasteLogInput{
		QtyKg: d("1"), Reason: "搭赠", WasteType: model.WasteTypeGift,
	}, "u-1")
	if err == nil {
		t.Fatalf("★ 搭赠既没部位也没货号 ⇒ 应被拒(否则这行静默丢失)")
	}
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Errorf("★ 应是输入校验错(400), got %v", err)
	}

	// ② waste_type 非法。
	_, err = svc.RecordWasteLog(context.Background(), "S001", model.RecordWasteLogInput{
		CutType: model.CutLiver, QtyKg: d("1"), Reason: "x", WasteType: "gift-ish",
	}, "u-1")
	if err == nil {
		t.Fatalf("★ waste_type 非法 ⇒ 应被拒")
	}
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Errorf("★ 应是输入校验错(400), got %v", err)
	}

	// ③ 只给部位 ⇒ 自动补货号。
	out, err := svc.RecordWasteLog(context.Background(), "S001", model.RecordWasteLogInput{
		CutType: model.CutLiver, QtyKg: d("3"), Reason: "搭赠", WasteType: model.WasteTypeGift,
	}, "u-1")
	mustNoErr(t, err, "RecordWasteLog(gift, 只给部位)")
	if out.CubeProductID != gfLiverSKU {
		t.Errorf("★ 只给部位时应自动补主货号 %s, got %q", gfLiverSKU, out.CubeProductID)
	}
	if out.CutType != model.CutLiver {
		t.Errorf("★ 部位应保留 liver, got %s", out.CutType)
	}

	// 进得了链(搭赠量 = 3kg)。
	rep, err := svc.ComputeGrossMargin(ctx, "S001", gfDay2)
	mustNoErr(t, err, "ComputeGrossMargin")
	liver := findPerCut(t, rep.PerCut, model.CutLiver)
	if !liver.GiftKg.Equal(d("3")) {
		t.Errorf("★ 自动补货号后搭赠量应进链 = 3kg, got %s", liver.GiftKg)
	}
}
