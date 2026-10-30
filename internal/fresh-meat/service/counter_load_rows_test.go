package service_test

// 2026-10-24(门店口径第二轮):**期初结转**(鲜肉-设置)+ **搬台行级增删改** +
// **负库存红字告警**。
//
// 门店原话:
//
//	「期初转结可以放在鲜肉-设置里」
//	「一个 sku 在当天搬台里可以有多条记录,多个标签袋,早上一袋,卖完了,中午加一袋,
//	  可以随时修改,甚至过一天修改(但修改值造成负库存时,需要在每日毛利的 alert 上
//	  红字显示)」
//
// ⇒ 三条不变量:
//
//	① 期初结转 = 按营业日推导的冷库余额链的**起点**;
//	② 搬台流水**一行一个动作**:不带 id = 新增(中午加一袋),带 id = 覆盖,
//	   还能改一行 / 撤一行(过一天也能改);
//	③ 改出来的**负库存不拦**,但要在每日毛利的 alert 里**红字**显示。

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

// TestOpening_SeedsTheDailyBalanceChain 锁 ①。
//
// ⇒ 「鲜肉-设置」里录的期初结转 = 链的起点 ⇒ 上线前就在冷库里的货不再算丢。
func TestOpening_SeedsTheDailyBalanceChain(t *testing.T) {
	svc, ctx, sku := fmBizSetup(t)
	fmBizSeedDay1(t, svc, ctx, sku)

	// 链的起点 = 0 ⇒ 10-01 的冷库 = 10(盘点盘出来的 10kg)。
	_, cold := fmMarginClosing(t, "S001", fmDay1, model.CutBelly)
	if !cold.Equal(d("10")) {
		t.Fatalf("期初为 0 时 10-01 冷库余额应是 10,got %s", cold)
	}

	// 「鲜肉-设置」里录期初:冷库 100kg(上线前就在冷库里的货)。
	// ⚠️ 期初**按部位**录(与每日毛利的部位口径一致)⇒ belly 部位。
	view, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-01", ColdStoreKg: d("100"),
			UnitPriceYuan: d("18"), Note: "上线期初"},
	})
	mustNoErr(t, err, "SaveStockOpenings")
	if len(view.Openings) != 1 || !view.Openings[0].ColdStoreKg.Equal(d("100")) {
		t.Fatalf("期初结转没存上: %+v", view.Openings)
	}
	if view.Openings[0].BizDate != "2026-10-01" {
		t.Errorf("期初生效日应是 2026-10-01,got %s", view.Openings[0].BizDate)
	}
	if view.Openings[0].CutType != model.CutBelly {
		t.Errorf("期初应按部位记录,got %q", view.Openings[0].CutType)
	}

	// ⇒ 链从期初起算:10-01 的冷库 = 期初 100 + 结转 10 = 110。
	_, cold2 := fmMarginClosing(t, "S001", fmDay1, model.CutBelly)
	if !cold2.Equal(d("110")) {
		t.Errorf("改期初后 10-01 冷库余额应是 110(期初 100 + 结转 10),got %s", cold2)
	}

	// 读端:期初清单能查回来。
	got, err := svc.ListStockOpenings(ctx, "S001")
	mustNoErr(t, err, "ListStockOpenings")
	if len(got.Openings) != 1 {
		t.Fatalf("期初清单应有 1 条,got %d", len(got.Openings))
	}
}

// TestCounterLoadRows_AppendEditDelete 锁 ②。
//
// ⇒ 一个 SKU 当天可以有多条搬台记录(早上一袋、中午加一袋);
// ⇒ 带 id 重复提交 = 覆盖(不重复搬);改一行 / 撤一行都按差额调整余额。
func TestCounterLoadRows_AppendEditDelete(t *testing.T) {
	svc, ctx, sku := fmBizSetup(t)

	// 10-01:盘点两行(一袋 20kg + 散货 10kg)⇒ 冷库 30kg。
	_, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("20"), Label: "BAG-A"},
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("10")},
		},
	}, "u-1")
	mustNoErr(t, err, "10-01 盘点")

	// 10-02 早上:搬一袋(BAG-A)+ 搬 2kg。
	svc.SetClock(fmClock(fmDay2))
	_, err = svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		Lines: []service.CounterLoadLine{
			{Label: "BAG-A"},
			{CubeProductID: sku, QtyKg: d("2")},
		},
	}, "u-1")
	mustNoErr(t, err, "10-02 早上搬台")

	view, err := svc.ListCounterLoads(ctx, "S001", fmDay2)
	mustNoErr(t, err, "ListCounterLoads")
	if len(view.Lines) != 2 {
		t.Fatalf("早上应有 2 条搬台流水,got %d", len(view.Lines))
	}
	if !view.TotalKg.Equal(d("22")) {
		t.Fatalf("早上搬台总量应是 22,got %s", view.TotalKg)
	}

	// 中午:卖完了,再加一袋(**不带 id = 新增一行**)。
	_, err = svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		Lines: []service.CounterLoadLine{{CubeProductID: sku, QtyKg: d("3")}},
	}, "u-1")
	mustNoErr(t, err, "10-02 中午加搬")
	view, err = svc.ListCounterLoads(ctx, "S001", fmDay2)
	mustNoErr(t, err, "ListCounterLoads 2")
	if len(view.Lines) != 3 {
		t.Fatalf("中午加搬后应有 3 条流水,got %d", len(view.Lines))
	}
	if !view.TotalKg.Equal(d("25")) {
		t.Errorf("加搬后总量应是 25,got %s", view.TotalKg)
	}
	cold, counter := fmPositions(t, "S001", sku)
	if !cold.Equal(d("5")) || !counter.Equal(d("25")) {
		t.Errorf("加搬后应是冷库 5 / 柜台 25,got %s / %s", cold, counter)
	}

	// 带 id 重复提交同一行 ⇒ **覆盖**(不重复搬)。
	var rowID string
	for _, l := range view.Lines {
		if l.QtyKg.Equal(d("3")) {
			rowID = l.ID
		}
	}
	if rowID == "" {
		t.Fatalf("找不到刚加的那一行")
	}
	_, err = svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		Lines: []service.CounterLoadLine{{ID: rowID, CubeProductID: sku, QtyKg: d("3")}},
	}, "u-1")
	mustNoErr(t, err, "带 id 重复提交")
	view, err = svc.ListCounterLoads(ctx, "S001", fmDay2)
	mustNoErr(t, err, "ListCounterLoads 3")
	if len(view.Lines) != 3 {
		t.Fatalf("带 id 重复提交不该新增行,got %d", len(view.Lines))
	}
	cold2, counter2 := fmPositions(t, "S001", sku)
	if !cold2.Equal(cold) || !counter2.Equal(counter) {
		t.Errorf("带 id 重复提交不该改余额:冷库 %s→%s 柜台 %s→%s", cold, cold2, counter, counter2)
	}

	// 改那一行(3kg → 5kg)⇒ 余额按差额调整。
	_, err = svc.UpdateCounterLoad(ctx, "S001", rowID, service.CounterLoadUpdateInput{QtyKg: d("5")})
	mustNoErr(t, err, "UpdateCounterLoad")
	cold3, counter3 := fmPositions(t, "S001", sku)
	if !cold3.Equal(d("3")) || !counter3.Equal(d("27")) {
		t.Errorf("改 5kg 后应是冷库 3 / 柜台 27,got %s / %s", cold3, counter3)
	}

	// 撤掉那一行 ⇒ 货搬回冷库。
	_, err = svc.DeleteCounterLoad(ctx, "S001", rowID)
	mustNoErr(t, err, "DeleteCounterLoad")
	view, err = svc.ListCounterLoads(ctx, "S001", fmDay2)
	mustNoErr(t, err, "ListCounterLoads 4")
	if len(view.Lines) != 2 {
		t.Fatalf("撤掉后应剩 2 条流水,got %d", len(view.Lines))
	}
	cold4, counter4 := fmPositions(t, "S001", sku)
	if !cold4.Equal(d("8")) || !counter4.Equal(d("22")) {
		t.Errorf("撤掉后应是冷库 8 / 柜台 22,got %s / %s", cold4, counter4)
	}
}

// TestCounterLoadRows_EditPastDayAndNegativeAlert 锁 ③。
//
// ⇒ **过一天修改**允许;改出来的**负库存不拦**,
//   但要在每日毛利的 alert 上**红字**显示。
func TestCounterLoadRows_EditPastDayAndNegativeAlert(t *testing.T) {
	svc, ctx, sku := fmBizSetup(t)

	// 10-01:盘点 5kg ⇒ 冷库 5kg。
	_, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("5")},
		},
	}, "u-1")
	mustNoErr(t, err, "10-01 盘点")

	// 10-02:搬 4kg ⇒ 冷库 1kg。
	svc.SetClock(fmClock(fmDay2))
	_, err = svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		Lines: []service.CounterLoadLine{{CubeProductID: sku, QtyKg: d("4")}},
	}, "u-1")
	mustNoErr(t, err, "10-02 搬台")

	view, err := svc.ListCounterLoads(ctx, "S001", fmDay2)
	mustNoErr(t, err, "ListCounterLoads")
	if len(view.Lines) != 1 {
		t.Fatalf("10-02 应有 1 条流水,got %d", len(view.Lines))
	}
	rowID := view.Lines[0].ID

	// 10-03(今天):**过一天修改** 10-02 的那一行(4kg → 10kg)⇒ 冷库变成负数。
	day3 := fmDay1.Add(48 * time.Hour)
	svc.SetClock(fmClock(day3))
	_, err = svc.UpdateCounterLoad(ctx, "S001", rowID, service.CounterLoadUpdateInput{QtyKg: d("10")})
	if err != nil {
		t.Fatalf("过一天修改应允许(负库存交给告警显示):%v", err)
	}

	// ⇒ 10-02 的按日结余变负(5 − 10 = −5)。
	_, cold := fmMarginClosing(t, "S001", fmDay2, model.CutBelly)
	if !cold.Equal(d("-5")) {
		t.Errorf("10-02 冷库余额应是 -5(改出来负库存),got %s", cold)
	}

	// ⇒ 每日毛利的 alert 里必须看得见这个负数(部位级)。
	//
	// ⚠️ 2026-10-27:成本链改**部位级** ⇒ 告警指向**部位**(不再指向 SKU)。
	//
	// ⚠️ **"负库存"的两种含义要分清**:
	//   - **冷库余额为负**(搬台超了冷库里的货)⇒ 这是**位置**层面的负数
	//     ⇒ 走"当前余额与按营业日推导的结余不一致"那条告警(带负数);
	//   - **账面短卖**(卖超了库存)⇒ 那是**成本链**层面的负数
	//     ⇒ 走红的"负库存"告警(见 cut_level_cost_test.go 的用例)。
	rep, err := svc.ComputeGrossMargin(ctx, "S001", fmDay2)
	mustNoErr(t, err, "ComputeGrossMargin")
	found := false
	for _, a := range rep.Alerts {
		if a.CubeProductID != string(model.CutBelly) {
			continue
		}
		if !strings.Contains(a.Message, "-5") {
			t.Errorf("告警文案里应看得见负结余 -5kg,got %q", a.Message)
		}
		found = true
	}
	if !found {
		t.Errorf("负库存应在每日毛利的 alerts 里显示(部位级),got %+v", rep.Alerts)
	}
}

// TestCounterLoadRows_LineCarriesCutTypeAndProductName 锁 2026-10-29 的前端契约。
//
// 门店原话:「搬台无法看到已录列表,找不到修改、删除入口。」
// ⇒ 前端要列"今日已搬台",每一行得看得见**部位**与**商品名**。
//
// ⚠️ json tag 就是前后端契约(不是装饰)⇒ 本域出参必须 snake_case。
func TestCounterLoadRows_LineCarriesCutTypeAndProductName(t *testing.T) {
	svc, ctx, sku := fmBizSetup(t)

	// 10-01:盘点 5kg ⇒ 冷库 5kg。
	_, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("5")},
		},
	}, "u-1")
	mustNoErr(t, err, "10-01 盘点")

	// 10-02:搬 2.5kg。
	svc.SetClock(fmClock(fmDay2))
	_, err = svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		Lines: []service.CounterLoadLine{{CubeProductID: sku, QtyKg: d("2.5")}},
	}, "u-1")
	mustNoErr(t, err, "10-02 搬台")

	view, err := svc.ListCounterLoads(ctx, "S001", fmDay2)
	mustNoErr(t, err, "ListCounterLoads")
	if len(view.Lines) != 1 {
		t.Fatalf("10-02 应有 1 条流水,got %d", len(view.Lines))
	}
	// ⇒ 部位直接取搬台流水表那一列(落库时就记了)。
	if view.Lines[0].CutType != model.CutBelly {
		t.Errorf("流水行的部位应是 belly,got %q", view.Lines[0].CutType)
	}

	// ⇒ wire 形状:snake_case,且**没有名字时不出现**这个 key。
	raw, err := json.Marshal(view.Lines[0])
	mustNoErr(t, err, "json.Marshal")
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	for _, key := range []string{"id", "branch_id", "biz_date", "seq",
		"cube_product_id", "qty_kg", "source", "operator_id", "created_at", "cut_type"} {
		if _, ok := got[key]; !ok {
			t.Errorf("搬台流水行缺少 %s 这个 wire 字段: %s", key, raw)
		}
	}
	if got["cut_type"] != string(model.CutBelly) {
		t.Errorf("cut_type 应是 belly,got %v", got["cut_type"])
	}
	// ⚠️ 该店没给这个货号登记商品名(名称是 seed 写的)⇒ 留空 ⇒ 不出这个 key。
	if _, ok := got["cube_product_name"]; ok {
		t.Errorf("查不到商品名时不该出现 cube_product_name 这个 key: %s", raw)
	}

	// ⇒ 有名字时必须带出来(前端显示"货号(商品名)")。
	line := service.CounterLoadLineView{
		ID: "CLD-1", BranchID: "S001", BizDate: "2026-10-02", Seq: 0,
		CubeProductID: "01409", QtyKg: d("2.5"),
		Source: service.CounterLoadSourceSKU, OperatorID: "u-1",
		CreatedAt: time.Now(), CutType: model.CutBelly, CubeProductName: "五花肉",
	}
	raw, err = json.Marshal(line)
	mustNoErr(t, err, "json.Marshal 2")
	got = map[string]any{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("json.Unmarshal 2: %v", err)
	}
	if got["cube_product_name"] != "五花肉" {
		t.Errorf("cube_product_name 应是 五花肉,got %v", got["cube_product_name"])
	}
}
