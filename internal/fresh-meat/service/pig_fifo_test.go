package service_test

// 2026-10-24:**鲜肉域的库存按日台账 + 毛利按 FIFO 回归到猪**。
//
// 门店口径(用户原话):
//
//	「盘点时将剩余的鲜肉商品按 sku 打包过秤然后登记,每个袋子夹上标签,
//	  方便次日搬上猪肉台继续售卖和毛利回归。」
//	「每袋有自己的产生日,这个产生日就是盘点归属日(也就是补录指定日),
//	  不装袋的 sku+kg 也根据盘点归属日做记录。」
//	「每天都有盘点单,没有任何剩余也要创建一个空的,标记为『售罄』」
//	「当天既没有卖猪、也没有搬冷库商品上猪肉台」⇒「鲜肉休市」
//	「做毛利回归到具体猪(如无标签,则按结转(日终盘点)的先后顺序归摊。)」
//
// ⇒ 六条不变量(每条一个用例):
//
//	① 同一天重复提交 ⇒ **仍然只有一张盘点单**;
//	② 空盘点 ⇒ **售罄**标记;
//	③ 当天无进猪无搬台 ⇒ **休市**标记;
//	④ 不装袋的 SKU **也有按归属日的结转记录**;
//	⑤ FIFO 归摊把一笔销售的收入**摊回多头猪**(一头猪一行,总量守恒);
//	⑥ 重投同一笔销售**不会重复吃 FIFO**(幂等)。

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/shopspring/decimal"
)

// fmFifoSetup 起服务 + 部位映射(belly → P-BELLY),时钟停在 10-01。
func fmFifoSetup(t *testing.T) (*service.Service, context.Context, string) {
	t.Helper()
	svc, _ := setupTestService(t)
	ctx := context.Background()
	svc.SetClock(fmClock(fmDay1))
	if _, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-BELLY"}); err != nil {
		t.Fatalf("CreateBranchCutMapping: %v", err)
	}
	return svc, ctx, "P-BELLY"
}

// fmSeedPig 录一头猪(当天进场),返回 pig_id。
func fmSeedPig(t *testing.T, svc *service.Service, earTag, grossKg string) string {
	t.Helper()
	return fmSeedPigAt(t, svc, earTag, grossKg, fmDay1)
}

// fmSeedPigAt 录一头猪(指定进场时刻),返回 pig_id。
func fmSeedPigAt(t *testing.T, svc *service.Service, earTag, grossKg string, arrivedAt time.Time) string {
	t.Helper()
	pig, err := svc.RecordWholePig(context.Background(), "S001", model.RecordWholePigInput{
		EarTag: earTag, GrossWeightKg: d(grossKg), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: arrivedAt,
	}, "u-1")
	if err != nil {
		t.Fatalf("RecordWholePig(%s): %v", earTag, err)
	}
	return pig.ID
}

// fmSeedPigCut 录一条分割补录(权威的"这头猪产出了这个部位多少 kg")。
func fmSeedPigCut(t *testing.T, svc *service.Service, pigID, sku, kg string) {
	t.Helper()
	if _, err := svc.RecordPigCut(context.Background(), "S001", model.RecordPigCutInput{
		PigID: pigID, CutType: model.CutBelly, Barcode: "B-" + pigID,
		WeightKg: d(kg), ExpiresAt: fmDay1.Add(48 * time.Hour), CubeProductID: sku,
	}, "u-1"); err != nil {
		t.Fatalf("RecordPigCut(%s): %v", pigID, err)
	}
}

// fmStocktakes 查某店的全部盘点单。
func fmStocktakes(t *testing.T) []model.PorkCutsStocktake {
	t.Helper()
	var rows []model.PorkCutsStocktake
	if err := testDBSingleton.Where("branch_id = ?", "S001").
		Order("taken_at ASC").Find(&rows).Error; err != nil {
		t.Fatalf("query pork_cuts_stocktakes: %v", err)
	}
	return rows
}

// fmBags 查某店的全部结转记录(袋子 + 不装袋的散货)。
func fmBags(t *testing.T) []model.StockBag {
	t.Helper()
	var rows []model.StockBag
	if err := testDBSingleton.Where("branch_id = ?", "S001").
		Order("origin_date ASC, id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("query stock_bags: %v", err)
	}
	return rows
}

// fmSales 查某店的全部按猪销售行。
func fmSales(t *testing.T) []model.LineSalesByPig {
	t.Helper()
	var rows []model.LineSalesByPig
	if err := testDBSingleton.Where("branch_id = ?", "S001").
		Order("pig_id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("query line_sales_by_pigs: %v", err)
	}
	return rows
}

// TestStocktake_SameDayRepeatedSubmitKeepsOneStocktake 锁 ①:同一天重复提交 ⇒ 仍然只有一张盘点单。
//
// ⇒ 同一 (门店, 归属日) 只有一张盘点单(确定性主键 + upsert);
//
//	重复提交 = **覆盖**,不是新增(多一张 ⇒ 报表重复计数)。
//
// ⇒ 补录往日也是同一张(归属日 = 指定日,不是提交那天)。
func TestStocktake_SameDayRepeatedSubmitKeepsOneStocktake(t *testing.T) {
	svc, ctx, sku := fmFifoSetup(t)

	submit := func(takenDate, kg string) *model.PorkCutsStocktake {
		t.Helper()
		st, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
			IsComplete: true,
			TakenDate:  takenDate,
			Cuts: []model.CutInput{
				{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d(kg)},
			},
		}, "u-1")
		mustNoErr(t, err, "RecordPorkCutsStocktake")
		return st
	}

	// ① 今天:提交两次(内容不同)。
	st1 := submit("", "10")
	st2 := submit("", "8")
	if st1.ID != st2.ID {
		t.Errorf("同一天重复提交应是同一张盘点单, got %s vs %s", st1.ID, st2.ID)
	}
	rows := fmStocktakes(t)
	if len(rows) != 1 {
		t.Fatalf("同一天重复提交后应只有 1 张盘点单, got %d: %+v", len(rows), rows)
	}
	// 内容被**覆盖**成第二次的值(不是新增一张)。
	//
	// ⚠️ 注意:盘点单里存的是**全店实盘**(柜台实盘 + 冷库余额),
	// 而重提交会再过一次夜搬运 ⇒ 冷库那半会变 ⇒ 这里比"存的就是第二次提交的内容",
	// 不写死 kg 数(余额按营业日推导落地后这里会变)。
	var snaps, latest []model.CutSnapshot
	mustNoErr(t, json.Unmarshal(rows[0].Cuts, &snaps), "unmarshal cuts(db)")
	mustNoErr(t, json.Unmarshal(st2.Cuts, &latest), "unmarshal cuts(返回体)")
	if len(snaps) != len(latest) {
		t.Fatalf("盘点单内容应被覆盖成最后一次提交, got %+v", snaps)
	}
	for i := range snaps {
		if !snaps[i].ActualRemainKg.Equal(latest[i].ActualRemainKg) {
			t.Errorf("盘点单内容应被覆盖成最后一次提交, got %+v want %+v", snaps[i], latest[i])
		}
	}

	// ② 补录往日:也是同一张(归属日 = 指定日,不是提交那天)。
	back1 := submit("2026-09-30", "4")
	back2 := submit("2026-09-30", "6")
	if back1.ID != back2.ID {
		t.Errorf("同一归属日应是同一张盘点单, got %s vs %s", back1.ID, back2.ID)
	}
	if back1.TakenDate != "2026-09-30" {
		t.Errorf("taken_date = %q, want 2026-09-30", back1.TakenDate)
	}
	rows = fmStocktakes(t)
	if len(rows) != 2 {
		t.Fatalf("应有两张盘点单(今天 + 09-30), got %d: %+v", len(rows), rows)
	}
	// 两张单的归属日必须各自落在自己的营业日窗口里。
	for _, r := range rows {
		day := r.TakenAt.In(fmLoc).Format("2006-01-02")
		if day != "2026-10-01" && day != "2026-09-30" {
			t.Errorf("盘点单的 taken_at 应落在自己的归属日窗口内, got %s", r.TakenAt)
		}
	}
}

// TestStocktake_EmptyIsSoldOut 锁 ②:空盘点 ⇒ **售罄**标记。
//
// ⇒ 「每天都有盘点单,没有任何剩余也要创建一个空的,标记为『售罄』」
// ⇒ 全部行的 actual_remain_kg 都是 0 也算售罄。
//
// ⚠️ 2026-10-24(门店口径):**休市与售罄互斥** —— 休市日没有货可盘
//   ⇒ 标『休市』不标『售罄』⇒ 本用例先录一头猪(当天不是休市日)再测售罄。
func TestStocktake_EmptyIsSoldOut(t *testing.T) {
	svc, ctx, sku := fmFifoSetup(t)

	// 当天进猪 ⇒ 不是休市日(否则空盘点会被标成休市)。
	fmSeedPigAt(t, svc, "EB-SOLD", "180", fmDay1)

	// ① cuts 传空数组 ⇒ 仍然创建盘点单,并标记售罄。
	st, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts:       []model.CutInput{},
	}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake(空盘点)")
	if !st.SoldOut {
		t.Errorf("空盘点应标记 sold_out=true")
	}
	if st.RestDay {
		t.Errorf("当天有进猪 ⇒ 不该是休市")
	}
	if st.ID == "" {
		t.Errorf("空盘点也要创建盘点单")
	}
	if len(fmStocktakes(t)) != 1 {
		t.Errorf("空盘点应留下 1 张盘点单, got %d", len(fmStocktakes(t)))
	}

	// ② 所有行都是 0kg ⇒ 也是售罄。
	st2, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		TakenDate:  "2026-09-30",
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("0")},
		},
	}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake(全 0)")
	// ⚠️ 09-30 既没进猪也没搬台 ⇒ 那是**休市日** ⇒ 标休市、不标售罄。
	if !st2.RestDay {
		t.Errorf("09-30 无进猪无搬台 ⇒ 应标 rest_day=true")
	}
	if st2.SoldOut {
		t.Errorf("休市与售罄互斥:休市日不该同时标 sold_out")
	}

	// ③ 有剩余 ⇒ 不是售罄。
	st3, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		TakenDate:  "2026-09-29",
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("3")},
		},
	}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake(有剩余)")
	if st3.SoldOut {
		t.Errorf("有剩余时不应标记售罄")
	}

	// ④ cuts 字段**缺失**(不是空数组)⇒ 400(不能把"忘了传"当成售罄)。
	_, err = svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		TakenDate:  "2026-09-28",
	}, "u-1")
	if err == nil {
		t.Errorf("cuts 缺失应报错(不能当成售罄)")
	}
}

// TestStocktake_RestDayWhenNoPigAndNoMove 锁 ③:当天无进猪无搬台 ⇒ **休市**标记。
//
// ⇒ 「当天既没有卖猪、也没有搬冷库商品上猪肉台」⇒「鲜肉休市」。
// ⇒ 与「售罄」是**两个不同的状态**(wire 上要能区分)。
func TestStocktake_RestDayWhenNoPigAndNoMove(t *testing.T) {
	svc, ctx, sku := fmFifoSetup(t)

	// ① 当天没有进猪、也没有搬台 ⇒ 鲜肉休市。
	st, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("5")},
		},
	}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake(休市日)")
	if !st.RestDay {
		t.Errorf("当天无进猪无搬台 ⇒ 应标记 rest_day=true")
	}
	if st.SoldOut {
		t.Errorf("有剩余(5kg)时不应同时标售罄(两个状态要能区分)")
	}

	// ② 当天**进了猪** ⇒ 不是休市。
	//
	// ⚠️ 2026-10-24(营业日状态机 + 跨日顺序校验):录 10-03 的猪之前,
	// **10-02 必须已盘点**(「不补录昨天,就无法录今天」)⇒ 先补录 10-02 的盘点。
	svc.SetClock(fmClock(fmDay1.Add(24 * time.Hour))) // 10-02
	_, err = svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		TakenDate:  "2026-10-02",
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("5")},
		},
	}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake(补录 10-02)")

	fmSeedPigAt(t, svc, "EB-R1", "180", fmDay1.Add(48*time.Hour)) // 10-03 进场
	svc.SetClock(fmClock(fmDay1.Add(48 * time.Hour)))             // 10-03
	st2, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		TakenDate:  "2026-10-03",
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("5")},
		},
	}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake(有进猪)")
	if st2.RestDay {
		t.Errorf("当天有进猪 ⇒ 不应标记休市")
	}
}

// TestStocktake_RestDayFalseWhenMovedToCounter 锁 ③ 的另一半:当天**搬了冷库商品上台** ⇒ 不是休市。
func TestStocktake_RestDayFalseWhenMovedToCounter(t *testing.T) {
	svc, ctx, sku := fmFifoSetup(t)

	// 10-01:盘一袋(带标签号)⇒ 袋子进冷库。
	_, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("12"), Label: "BAG-R1"},
		},
	}, "u-1")
	mustNoErr(t, err, "10-01 盘点")

	// 10-02:按标签整袋搬上猪肉台(⇒ 当天有"搬冷库商品上猪肉台")。
	svc.SetClock(fmClock(fmDay2))
	if _, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		Lines: []service.CounterLoadLine{{Label: "BAG-R1"}},
	}, "u-1"); err != nil {
		t.Fatalf("CounterLoad(按标签): %v", err)
	}

	// 10-02 的盘点:没有进猪,但**搬了台** ⇒ 不是休市。
	st, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		TakenDate:  "2026-10-02",
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("12")},
		},
	}, "u-1")
	mustNoErr(t, err, "10-02 盘点")
	if st.RestDay {
		t.Errorf("当天搬了冷库商品上猪肉台 ⇒ 不应标记休市")
	}
}

// TestStocktake_CarryOverRowWithoutLabel 锁 ④:不装袋的 SKU **也有按归属日的结转记录**。
//
// ⇒ 「不装袋的 sku+kg 也根据盘点归属日做记录」
// ⇒ 盘点时**每一行**都要有一行记录(带 label = 一袋;无 label = 散货结转)。
func TestStocktake_CarryOverRowWithoutLabel(t *testing.T) {
	svc, ctx, sku := fmFifoSetup(t)

	// ① 不装袋的行 ⇒ 一条 label 为空的结转记录。
	_, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("12")},
		},
	}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake")

	bags := fmBags(t)
	if len(bags) != 1 {
		t.Fatalf("不装袋的行也要有结转记录, got %d 行: %+v", len(bags), bags)
	}
	bg := bags[0]
	if bg.Label != "" {
		t.Errorf("不装袋的结转行 label 应为空, got %q", bg.Label)
	}
	if bg.CubeProductID != sku {
		t.Errorf("sku = %q, want %q", bg.CubeProductID, sku)
	}
	if !bg.QtyKg.Equal(d("12")) {
		t.Errorf("结转重量 = %s, want 12", bg.QtyKg)
	}
	if time.Time(bg.OriginDate).Format("2006-01-02") != "2026-10-01" {
		t.Errorf("origin_date = %s, want 2026-10-01(归属日)", time.Time(bg.OriginDate).Format("2006-01-02"))
	}
	if !bg.ConsumedKg.IsZero() {
		t.Errorf("新登记的结转行 consumed_kg 应为 0, got %s", bg.ConsumedKg)
	}

	// ② **补录往日**的结转记录也记在**归属日**那天(时间线连续)。
	_, err = svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		TakenDate:  "2026-09-30",
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("7")},
		},
	}, "u-1")
	mustNoErr(t, err, "补录 09-30")
	bags = fmBags(t)
	if len(bags) != 2 {
		t.Fatalf("补录往日应再加一条结转记录, got %d: %+v", len(bags), bags)
	}
	back := bags[0]
	if time.Time(back.OriginDate).Format("2006-01-02") != "2026-09-30" {
		t.Errorf("补录的结转记录 origin_date = %s, want 2026-09-30(归属日)",
			time.Time(back.OriginDate).Format("2006-01-02"))
	}
	if !back.QtyKg.Equal(d("7")) {
		t.Errorf("补录的结转重量 = %s, want 7", back.QtyKg)
	}
}

// TestFifo_SaleSplitAcrossPigs 锁 ⑤:FIFO 归摊把收入**摊回多头猪**。
//
// 场景:
//
//	D1: 猪 A(200kg)与猪 B(100kg)各分割出五花 10kg(都有分割补录)
//	    → 日终盘点五花剩 20kg(不装袋)
//	D2: 卖五花 15kg @ 10 元/kg = 150 元
//
// ⇒ 收入必须**摊回 A 与 B**(各 7.5kg / 75 元),而不是"今天碰巧在场的猪"。
// ⇒ 总量守恒:Σ 重量 = 15、Σ 金额 = 150。
func TestFifo_SaleSplitAcrossPigs(t *testing.T) {
	svc, ctx, sku := fmFifoSetup(t)
	pigA := fmSeedPig(t, svc, "EB-A", "200")
	pigB := fmSeedPig(t, svc, "EB-B", "100")
	fmSeedPigCut(t, svc, pigA, sku, "10")
	fmSeedPigCut(t, svc, pigB, sku, "10")

	// D1 日终盘点:五花 20kg,**不装袋**。
	_, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("20")},
		},
	}, "u-1")
	mustNoErr(t, err, "D1 盘点")

	// D2 卖 15kg。
	svc.SetClock(fmClock(fmDay2))
	_, err = svc.OnSaleCompleted(ctx, service.SaleCompletedPayload{
		SaleID: "SO-FIFO", BranchID: "S001", CompletedAt: fmDay2,
		Lines: []service.SaleLine{
			{LineID: "L-FIFO", SKUID: sku, Qty: d("15"), UnitPriceYuan: d("10"), AmountYuan: d("150")},
		},
	})
	mustNoErr(t, err, "OnSaleCompleted D2")

	rows := fmSales(t)
	if len(rows) != 2 {
		t.Fatalf("一笔销售摊到两头猪 ⇒ 应落 2 行(一头猪一行), got %d: %+v", len(rows), rows)
	}
	sumKg := decimal.Zero
	sumAmt := decimal.Zero
	byPig := map[string]model.LineSalesByPig{}
	for _, r := range rows {
		sumKg = sumKg.Add(r.QtyKg)
		sumAmt = sumAmt.Add(r.AmountYuan)
		byPig[r.PigID] = r
	}
	if !sumKg.Equal(d("15")) {
		t.Errorf("Σ 重量 = %s, want 15(总量守恒)", sumKg)
	}
	if !sumAmt.Equal(d("150")) {
		t.Errorf("Σ 金额 = %s, want 150(总量守恒)", sumAmt)
	}
	// A 与 B 各产出了 10kg ⇒ 各摊一半。
	for _, pigID := range []string{pigA, pigB} {
		r, ok := byPig[pigID]
		if !ok {
			t.Fatalf("猪 %s 没有被摊到: %+v", pigID, rows)
		}
		if !r.QtyKg.Equal(d("7.5")) {
			t.Errorf("猪 %s 摊到的重量 = %s, want 7.5", pigID, r.QtyKg)
		}
		if !r.AmountYuan.Equal(d("75")) {
			t.Errorf("猪 %s 摊到的金额 = %s, want 75", pigID, r.AmountYuan)
		}
		if r.CutType != model.CutBelly {
			t.Errorf("猪 %s 的行 cut_type = %q, want belly", pigID, r.CutType)
		}
	}
	// 被吃掉的 FIFO 重量**落库**。
	bags := fmBags(t)
	if len(bags) != 1 {
		t.Fatalf("应有 1 条结转记录, got %d", len(bags))
	}
	if !bags[0].ConsumedKg.Equal(d("15")) {
		t.Errorf("consumed_kg = %s, want 15(卖掉的部分必须落库)", bags[0].ConsumedKg)
	}
}

// TestFifo_BulkSettlementConsumesCarryOver 锁:大宗结算出库也扣**同一批按日结转记录**。
//
// ⇒ 门店口径(2026-10-24):「大宗结算可以直接从冷库出,不必先搬上猪肉台」
// ⇒ 冷库里的货(那些按日结转记录)被结算出库消耗掉
// ⇒ 不扣结转记录 ⇒ 零售会把**同一批货再卖一遍**。
func TestFifo_BulkSettlementConsumesCarryOver(t *testing.T) {
	svc, ctx, sku := fmFifoSetup(t)

	// 登记大宗品项(部位 belly ⇒ 大宗库存/收入记在部位主 SKU P-BELLY 上)。
	if _, err := svc.CreateBulkProduct(ctx, "S001",
		service.BulkProductInput{CutType: model.CutBelly}); err != nil {
		t.Fatalf("CreateBulkProduct: %v", err)
	}
	fmSeedPig(t, svc, "EB-B1", "200")

	// 日终盘点:belly 20kg(不装袋)⇒ 一条按日结转记录 + 一条大宗台账。
	_, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("20")},
		},
	}, "u-1")
	mustNoErr(t, err, "盘点")

	// 大宗结算:直接从冷库出 8kg。
	if _, err := svc.CreateBulkSettlement(ctx, "S001", service.BulkSettlementInput{
		SettleDate:   "2026-10-01",
		Counterparty: "某酒店",
		Mode:         service.BulkModeDelivery,
		Items: []service.BulkSettlementItemInput{
			{ProductCode: "belly", QtyKg: d("8"), UnitPriceYuan: d("5")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("CreateBulkSettlement: %v", err)
	}

	// 结转记录的 consumed_kg 必须被扣掉 8kg ⇒ 零售不会再卖这批货。
	bags := fmBags(t)
	if len(bags) != 1 {
		t.Fatalf("应有 1 条按日结转记录, got %d: %+v", len(bags), bags)
	}
	if !bags[0].ConsumedKg.Equal(d("8")) {
		t.Errorf("consumed_kg = %s, want 8(大宗结算出库必须扣同一批结转记录)", bags[0].ConsumedKg)
	}
}

// ⇒ dapr 是 at-least-once ⇒ 同一笔销售会被重投。
// ⇒ 重投时 FIFO 状态已变(上次扣过 consumed_kg)⇒ 不做幂等检查就会把同一批货摊两遍。
func TestFifo_ReplayDoesNotConsumeTwice(t *testing.T) {
	svc, ctx, sku := fmFifoSetup(t)
	pigA := fmSeedPig(t, svc, "EB-A", "200")
	pigB := fmSeedPig(t, svc, "EB-B", "100")
	fmSeedPigCut(t, svc, pigA, sku, "10")
	fmSeedPigCut(t, svc, pigB, sku, "10")

	_, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("20")},
		},
	}, "u-1")
	mustNoErr(t, err, "D1 盘点")

	svc.SetClock(fmClock(fmDay2))
	payload := service.SaleCompletedPayload{
		SaleID: "SO-REPLAY", BranchID: "S001", CompletedAt: fmDay2,
		Lines: []service.SaleLine{
			{LineID: "L-REPLAY", SKUID: sku, Qty: d("15"), UnitPriceYuan: d("10"), AmountYuan: d("150")},
		},
	}
	if _, err := svc.OnSaleCompleted(ctx, payload); err != nil {
		t.Fatalf("第一次投递: %v", err)
	}
	firstRows := fmSales(t)
	firstConsumed := fmBags(t)[0].ConsumedKg

	// 重投(dapr at-least-once)。
	if _, err := svc.OnSaleCompleted(ctx, payload); err != nil {
		t.Fatalf("重投: %v", err)
	}

	rows := fmSales(t)
	if len(rows) != len(firstRows) {
		t.Errorf("重投不应新增行: %d → %d", len(firstRows), len(rows))
	}
	sumAmt := decimal.Zero
	for _, r := range rows {
		sumAmt = sumAmt.Add(r.AmountYuan)
	}
	if !sumAmt.Equal(d("150")) {
		t.Errorf("重投后 Σ 金额 = %s, want 150(收入不能翻倍)", sumAmt)
	}
	if got := fmBags(t)[0].ConsumedKg; !got.Equal(firstConsumed) {
		t.Errorf("重投后 consumed_kg = %s, want %s(FIFO 不能被吃两遍)", got, firstConsumed)
	}
}
