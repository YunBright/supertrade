package service_test

// 2026-10-25：**期初结转的单价**必须进成本链。
//
// # 为什么要有这个文件
//
// 期初是**上线前就在冷库里的货** —— 它的成本**不在本系统的流水里**
// （本系统 2026-10 才有进猪/搬台流水）。
//
// ⇒ 若成本链的第一天期初**金额**取 0 ⇒ 那批货**零成本**
// ⇒ 卖出去全是利润 ⇒ **毛利虚高**（"卖 10kg 收 300，毛利 300"）。
//
// ⇒ 门店盘冷库时知道自己那批货当时**多少钱一斤** ⇒ 让他填单价
//
//	（鲜肉-设置 → 期初结转 → 重量 / 单价 / 总价 三列，总价 = 重量 × 单价）。
//
// # 三条不变量
//
//	① **单价必填**：有货(kg > 0)却没有单价 ⇒ 400（别静默按 0 存）。
//	② **总价 = 重量 × 单价**：服务端算、不可填、落库作快照。
//	③ **成本链认这个单价**：期初当天该 SKU 的
//	   期初 kg / 期初金额 / 加权单价 = 门店录的那组数（**不是 0**）。
//
// # 跑在哪
//
// ⇒ **服务层（内存 SQLite）** ⇒ 不碰生产、零压力。

import (
	"context"
	"strings"
	"testing"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/shopspring/decimal"
)

// TestStockOpening_UnitPriceRequired 锁 ①：有货没单价 ⇒ 400。
//
// ⚠️ 为什么不静默按 0 存：单价 0 ⇒ 那批货零成本 ⇒ 毛利虚高
//
//	⇒ 门店会以为"卖得越多赚得越多"，而真相是**成本没进账**。
func TestStockOpening_UnitPriceRequired(t *testing.T) {
	svc, ctx, _ := fmBizSetup(t)

	// kg > 0 但没有单价 ⇒ 拒绝。
	_, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-01", ColdStoreKg: d("100")},
	})
	if err == nil {
		t.Fatalf("★ 有货(kg>0)却没有单价必须被拒 —— 否则那批货零成本、毛利虚高")
	}
	if !strings.Contains(err.Error(), "unit_price_yuan") {
		t.Errorf("★ 报错要点名 unit_price_yuan(让门店知道填哪个), got %v", err)
	}

	// 单价 ≤ 0 ⇒ 同样拒绝。
	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-01", ColdStoreKg: d("100"), UnitPriceYuan: d("0")},
	}); err == nil {
		t.Fatalf("★ 单价 0 必须被拒（零成本 ⇒ 毛利虚高）")
	}

	// kg = 0（明确清零）⇒ 单价可省（金额也是 0）。
	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-01", ColdStoreKg: d("0")},
	}); err != nil {
		t.Fatalf("kg=0 的行不该要单价: %v", err)
	}
}

// TestStockOpening_TotalIsComputed 锁 ②：总价 = 重量 × 单价（服务端算）。
func TestStockOpening_TotalIsComputed(t *testing.T) {
	svc, ctx, _ := fmBizSetup(t)

	out, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-01", ColdStoreKg: d("12.5"), UnitPriceYuan: d("18.4")},
	})
	mustNoErr(t, err, "SaveStockOpenings")
	if len(out.Openings) != 1 {
		t.Fatalf("应有 1 条期初, got %d", len(out.Openings))
	}
	row := out.Openings[0]
	if !row.UnitPriceYuan.Equal(d("18.4")) {
		t.Errorf("单价应读回 18.4, got %s", row.UnitPriceYuan)
	}
	// 12.5 × 18.4 = 230
	if !row.TotalAmountYuan.Equal(d("230")) {
		t.Errorf("总价应是 12.5 × 18.4 = 230, got %s", row.TotalAmountYuan)
	}
}

// TestStockOpening_UnitPriceEntersCostChain 锁 ③：成本链认门店给的单价。
//
// 这是本轮最核心的一条：**期初当天**该**部位**的
//
//	期初 kg      = 门店录的重量
//	期初金额    = 重量 × 门店录的单价
//	加权单价    = 门店录的单价
//
// ⇒ 断链之前：期初金额 0、加权单价 0 ⇒ 那批货零成本 ⇒ 毛利虚高。
//
// ⚠️ 2026-10-27:成本链改**部位级** ⇒ 断言从 per_sku 改成 **per_cut**
//   (库存/成本/毛利都是部位口径;per_sku 只承载销售/营收/产出)。
func TestStockOpening_UnitPriceEntersCostChain(t *testing.T) {
	svc, ctx, _ := fmBizSetup(t)

	// 期初：五花 100kg @ 18 元/kg（2026-10-01 生效）⇒ 总价 1800。
	out, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-01", ColdStoreKg: d("100"), UnitPriceYuan: d("18")},
	})
	mustNoErr(t, err, "SaveStockOpenings")
	if !out.Openings[0].TotalAmountYuan.Equal(d("1800")) {
		t.Fatalf("总价应是 100 × 18 = 1800, got %s", out.Openings[0].TotalAmountYuan)
	}

	// 当天的每日毛利（实时算）。
	rep, err := service.New(testDBSingleton).ComputeGrossMargin(ctx, "S001", fmDay1)
	mustNoErr(t, err, "ComputeGrossMargin")
	row := fmCutRowFor(t, rep, model.CutBelly)
	if !row.OpeningKg.Equal(d("100")) {
		t.Errorf("★ 期初 kg 应是门店录的 100, got %s", row.OpeningKg)
	}
	// 期初金额 = 100 × 18 = 1800(部位级 ⇒ 走对账行的期初存货金额)。
	if !rep.Reconciliation.OpeningInventoryCostYuan.Equal(d("1800")) {
		t.Errorf("★ 期初金额应是 100 × 18 = 1800, got %s —— "+
			"金额 0 ⇒ 那批货零成本 ⇒ 毛利虚高", rep.Reconciliation.OpeningInventoryCostYuan)
	}
	if !row.MovingAvgUnitCost.Equal(d("18")) {
		t.Errorf("★ 加权单价应是门店给的 18, got %s", row.MovingAvgUnitCost)
	}
	if !row.ExpectedRemainKg.Equal(d("100")) {
		t.Errorf("账面应有应是 100kg(没卖没产出), got %s", row.ExpectedRemainKg)
	}
	// 成本恒等式：期初 + 入库 = 销售 + 统收 + 报损 + 期末 ⇒ 差额 0。
	if !rep.Reconciliation.DiffYuan.Equal(decimal.Zero) {
		t.Errorf("成本对账差额应是 0, got %s", rep.Reconciliation.DiffYuan)
	}
}

// TestStockOpening_UnitPriceInThreePlaces 锁「三处一致」（链 + 位置表 + 队列）。
//
// 期初的货必须同时在：
//
//	① 成本链（上面的用例已验）；② 位置表(冷库)；③ FIFO 队列（袋子）。
//
// 且**队列那一行的成本快照 = 门店给的单价** ⇒ 大宗结算领用它时成本不为 0。
func TestStockOpening_UnitPriceInThreePlaces(t *testing.T) {
	svc, ctx, sku := fmBizSetup(t)

	_, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-01", ColdStoreKg: d("100"), UnitPriceYuan: d("18")},
	})
	mustNoErr(t, err, "SaveStockOpenings")

	// ② 位置表（冷库）= 100kg。
	cold, _ := fmPositions(t, "S001", sku)
	if !cold.Equal(d("100")) {
		t.Errorf("★ 位置表(冷库)应是 100kg, got %s —— 门店在库存查询里看不到这批货", cold)
	}

	// ③ 队列（袋子）：source='opening'、100kg、成本快照 = 18 元/kg。
	bags, err := svc.ListStockBags(ctx, "S001", "", "", "")
	mustNoErr(t, err, "ListStockBags")
	var openBag *model.StockBag
	for i := range bags {
		if bags[i].Source == "opening" {
			openBag = &bags[i]
		}
	}
	if openBag == nil {
		t.Fatalf("★ 队列里应有期初那一条(source='opening'), got %+v", bags)
	}
	if !openBag.QtyKg.Equal(d("100")) {
		t.Errorf("★ 队列里期初应是 100kg, got %s", openBag.QtyKg)
	}
	if !openBag.UnitCostYuan.Equal(d("18")) {
		t.Errorf("★ 队列那一行的成本快照应是门店给的 18 元/kg, got %s —— "+
			"0 ⇒ 大宗结算领用它时成本为 0 ⇒ 毛利率 100%%", openBag.UnitCostYuan)
	}
}

// contains 小工具（避免为一处判断引入 strings 依赖冲突）。
// 编译期提醒：context 用到了（防止误删 import）。
var _ = context.Background
