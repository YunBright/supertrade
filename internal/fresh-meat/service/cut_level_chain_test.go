// Package service_test —— 部位级成本链的**新不变量**(2026-10-27 收尾)。
//
// # 四条
//
//	① 多货号部位:两个货号都卖货 ⇒ **两笔销售都带成本**
//	  (旧口径把整个部位的结余记在「主 SKU」上 ⇒ 另一个货号卖出 = 零成本
//	   = 毛利率 100%)+ 部位恒等式闭合。
//	② 账面短卖:卖超账面库存 ⇒「应有」为负、**销售成本 > 0**、毛利不高估。
//	③ 期初结转**直接按部位进链**(不再落「主 SKU」)⇒ 快照落 `cut_cost_daily`,
//	  旧的 `sku_cost_daily` 不再写。
//	④ 对外 JSON:`per_sku` 行**不带**库存/成本键(**键缺失**,不是 0)。
package service_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cuberead/cubereadfake"
	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/shopspring/decimal"
)

// fmDay2Key 是测试里的"营业日"字符串(销售只在这一天发生)。
const fmDay2Key = "2026-10-02"

// fmCubeOperDate 从 cube 查询里取出"要哪天"的 oper_date 过滤值。
//
// ⚠️ 成本链**逐日回溯**时,每一天都会带自己的 oper_date 来问 cube
//   ⇒ 假身必须按这个值过滤,否则同一批销售会被算进每一天。
func fmCubeOperDate(q cubeclient.CubeQuery) string {
	for _, f := range q.Filters {
		if f.Member != "sales_report.oper_date" {
			continue
		}
		for _, v := range f.Values {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}

// fmMultiSkuSetup 起一个"有源门店"的毛利环境:**belly 一个部位挂多个货号**。
//
// ⚠️ 销售走 cube 替身(有源门店的销售在 cube 里,见 cube_sales_test.go);
//   期初/盘点/分割走本地 PG ⇒ 两边都能造出"多货号部位"的形态。
func fmMultiSkuSetup(t *testing.T, cubeRows []map[string]any, skus ...string) (*service.Service, context.Context) {
	t.Helper()
	svc, _ := setupTestService(t)
	ctx := context.Background()
	for _, sku := range skus {
		if _, err := svc.CreateBranchCutMapping(ctx, "S001",
			model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: sku}); err != nil {
			t.Fatalf("CreateBranchCutMapping(%s): %v", sku, err)
		}
	}
	cube := &cubereadfake.Client{}
	cube.Handler = func(m string, q cubeclient.CubeQuery) ([]map[string]any, int, error) {
		if m != "sales_report" {
			return nil, 0, nil
		}
		// ⚠️ **必须按 oper_date 过滤**(2026-10-27 链式接续后暴露):
		//   成本链现在**逐日回溯**(基线日 → 今天),每一天都会问一次 cube。
		//   假身若对任何日期都返回同一批销售 ⇒ 销售会被**重复计入每一天**
		//   ⇒ 链的期初被算成"卖了两遍"的残值 ⇒ 账面应有/成本全错。
		if d := fmCubeOperDate(q); d != "" && d != fmDay2Key {
			return nil, 0, nil
		}
		return cubeRows, len(cubeRows), nil
	}
	svc.SetCubeReader(cube)
	svc.SetSourceResolver(fmFakeResolver{src: cubesource.Source{
		BranchID: "S001", Configured: true, Enabled: true, CubeSourceName: "cube-sixun-ysx-fb",
	}})
	svc.SetClock(fmClock(fmDay2))
	return svc, ctx
}

// fmSaveBellyOpening 录一条**按部位**的期初结转(门店口径:鲜肉只管部位)。
func fmSaveBellyOpening(t *testing.T, svc *service.Service, ctx context.Context, kg, price string) {
	t.Helper()
	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-01",
			ColdStoreKg: d(kg), UnitPriceYuan: d(price)},
	}); err != nil {
		t.Fatalf("SaveStockOpenings: %v", err)
	}
}

// TestCutLevel_MultiSkuSameCut_BothSalesCarryCost 锁 ①。
//
// # 场景
//
//	belly 部位挂两个货号 P-BELLY-A / P-BELLY-B;期初 100kg @ 18 元/kg;
//	10-02 卖:A 5kg 收 100 元、B 3kg 收 60 元。
//
// # 期望
//
//	部位行:销量 8kg、营收 160 元、**销售成本 144 元**(= 8kg × 18)
//	⇒ **两个货号的销售都带成本**。
//
// ⚠️ 旧口径(部位级改造前):`overlayStockOpenings` 把部位的期初与结余
//   100% 记到该部位的「主 SKU」上 ⇒ 另一个货号账面恒为 0
//   ⇒ 它卖出的 3kg **零成本** ⇒ 毛利率 100%(毛利虚高)。
//   ⇒ 那时部位的销售成本只有 5kg × 18 = 90 元。
func TestCutLevel_MultiSkuSameCut_BothSalesCarryCost(t *testing.T) {
	svc, ctx := fmMultiSkuSetup(t, []map[string]any{
		fmCubeSalesRow("P-BELLY-A", "5", "100"),
		fmCubeSalesRow("P-BELLY-B", "3", "60"),
	}, "P-BELLY-A", "P-BELLY-B")
	fmSaveBellyOpening(t, svc, ctx, "100", "18")

	rep, err := svc.ComputeGrossMargin(ctx, "S001", fmDay2)
	mustNoErr(t, err, "ComputeGrossMargin(10-02)")
	cut := fmCutRowFor(t, rep, model.CutBelly)

	// ① 两个货号的销售都进部位行(5 + 3 = 8kg;100 + 60 = 160 元)。
	if !cut.SoldKg.Equal(d("8")) {
		t.Errorf("★ 部位销量应是 5 + 3 = 8kg, got %s", cut.SoldKg)
	}
	if !cut.RevenueYuan.Equal(d("160")) {
		t.Errorf("★ 部位营收应是 100 + 60 = 160 元, got %s", cut.RevenueYuan)
	}

	// ② ★ 两笔销售**都带成本**:移动加权单价 18 ⇒ 8kg × 18 = 144 元。
	//
	// (旧口径只会给「主 SKU」那 5kg 计成本 ⇒ 90 元 ⇒ 另一个货号白送。)
	if !cut.MovingAvgUnitCost.Equal(d("18")) {
		t.Errorf("★ 部位移动加权单价应是门店给的 18, got %s", cut.MovingAvgUnitCost)
	}
	if !cut.SalesCostYuan.Equal(d("144")) {
		t.Errorf("★ 销售成本应是 8kg × 18 = 144(两个货号都带成本), got %s —— "+
			"90 说明只有「主 SKU」那笔带成本 ⇒ 另一个货号零成本 ⇒ 毛利虚高",
			cut.SalesCostYuan)
	}

	// ③ 部位恒等式:期初 100 + 产出 0 − 销售 8 = 应有 92 ⇒ 对账差额 0。
	if !cut.ExpectedRemainKg.Equal(d("92")) {
		t.Errorf("★ 账面应有应是 100 − 8 = 92kg, got %s", cut.ExpectedRemainKg)
	}
	if !rep.Reconciliation.DiffYuan.Equal(decimal.Zero) {
		t.Errorf("★ 成本对账差额应是 0, got %s", rep.Reconciliation.DiffYuan)
	}

	// ④ 货号行两行都在(前端按货号分列销售/营收)。
	seen := map[string]bool{}
	for _, r := range rep.PerSku {
		seen[r.CubeProductID] = true
	}
	for _, sku := range []string{"P-BELLY-A", "P-BELLY-B"} {
		if !seen[sku] {
			t.Errorf("per_sku 应含 %s 这一行(销售按货号分列), got %+v", sku, rep.PerSku)
		}
	}
}

// TestCutLevel_ShortSell_NegativeExpectedAndCostNotZero 锁 ②。
//
// # 场景
//
//	期初只有 10kg @ 18 元/kg;10-02 卖了 20kg 收 200 元 ⇒ **卖超账面库存 10kg**。
//
// # 期望
//
//	① 「应有」= −10kg(负数 = 账面短卖,**不裁 0**)
//	② **销售成本 = 360 元**(20kg × 18)—— 短卖的那 10kg **照摊成本**
//	③ 毛利 = 200 − 360 = −160 元(**不高估**:不是 +200)
//	④ 红字告警(negative_inventory)必须出现 ⇒ 门店看得见短卖
//	⑤ 恒等式仍闭合(差额 0)
//
// ⚠️ 旧口径:「应有」被裁成 0、且卖超的部分**零成本** ⇒ 毛利虚高。
func TestCutLevel_ShortSell_NegativeExpectedAndCostNotZero(t *testing.T) {
	svc, ctx := fmMultiSkuSetup(t, []map[string]any{
		fmCubeSalesRow("P-BELLY-A", "20", "200"),
	}, "P-BELLY-A")
	fmSaveBellyOpening(t, svc, ctx, "10", "18")

	rep, err := svc.ComputeGrossMargin(ctx, "S001", fmDay2)
	mustNoErr(t, err, "ComputeGrossMargin(10-02)")
	cut := fmCutRowFor(t, rep, model.CutBelly)

	// ① 「应有」为负(短卖 10kg)。
	if !cut.ExpectedRemainKg.Equal(d("-10")) {
		t.Errorf("★ 账面应有应是 10 − 20 = −10kg(负数 = 账面短卖,不许裁 0), got %s",
			cut.ExpectedRemainKg)
	}

	// ② ★ 销售成本 > 0:短卖的部分照摊成本(18 元/kg)⇒ 20kg × 18 = 360 元。
	if !cut.SalesCostYuan.Equal(d("360")) {
		t.Errorf("★ 销售成本应是 20kg × 18 = 360(短卖也照摊成本), got %s —— "+
			"0 ⇒ 卖超的部分白送 ⇒ 毛利虚高", cut.SalesCostYuan)
	}

	// ③ 毛利不高估:收 200、成本 360 ⇒ −160 元。
	if !cut.GrossMarginYuan.Equal(d("-160")) {
		t.Errorf("★ 毛利应是 200 − 360 = −160(短卖不吃毛利), got %s", cut.GrossMarginYuan)
	}

	// ④ 红字告警:短卖必须看得见。
	var redAlerts []service.MarginAlert
	for _, a := range rep.Alerts {
		if a.Code == service.MarginAlertNegativeInventory && a.Level == service.MarginAlertLevelError {
			redAlerts = append(redAlerts, a)
		}
	}
	if len(redAlerts) == 0 {
		t.Errorf("★ 账面短卖必须出红字告警(negative_inventory), got %+v", rep.Alerts)
	}

	// ⑤ 恒等式仍闭合(差额 0)。
	if !rep.Reconciliation.DiffYuan.Equal(decimal.Zero) {
		t.Errorf("★ 成本对账差额应是 0, got %s", rep.Reconciliation.DiffYuan)
	}
}

// TestCutLevel_OpeningGoesIntoCutChain_NotPrimarySku 锁 ③。
//
// # 场景
//
//	belly 挂两个货号;期初**按部位**录 100kg @ 18(不是按货号录);
//	10-02 只有「非主货号」P-BELLY-B 卖了 5kg。
//
// # 期望
//
//	① 期初整笔进**部位**链:部位行期初 100kg、单价 18;
//	   成本日快照落在 `cut_cost_daily`(opening 100kg / 1800 元)。
//	② ★ 旧的 `sku_cost_daily` **一行都没有** ⇒ 期初**不再落「主 SKU」**
//	   (落主 SKU ⇒ 另一个货号账面恒 0 ⇒ 卖出零成本 ⇒ 毛利率 100%)。
//	③ 货号行**不带**库存键(期初/期末是部位口径)。
func TestCutLevel_OpeningGoesIntoCutChain_NotPrimarySku(t *testing.T) {
	svc, ctx := fmMultiSkuSetup(t, []map[string]any{
		fmCubeSalesRow("P-BELLY-B", "5", "100"),
	}, "P-BELLY-A", "P-BELLY-B")
	fmSaveBellyOpening(t, svc, ctx, "100", "18")

	rep, err := svc.ComputeGrossMargin(ctx, "S001", fmDay2)
	mustNoErr(t, err, "ComputeGrossMargin(10-02)")
	cut := fmCutRowFor(t, rep, model.CutBelly)

	// ① 期初按部位进链。
	if !cut.OpeningKg.Equal(d("100")) {
		t.Errorf("★ 部位期初应是门店录的 100kg, got %s", cut.OpeningKg)
	}
	if !cut.MovingAvgUnitCost.Equal(d("18")) {
		t.Errorf("★ 部位移动加权单价应是门店给的 18, got %s", cut.MovingAvgUnitCost)
	}

	// 落当日成本快照(部位级)。
	if _, err := svc.PersistCutCostDaily(ctx, "S001", fmDay2); err != nil {
		t.Fatalf("PersistCutCostDaily: %v", err)
	}
	snap, ok := fmCutCostDaily(t, "S001", "2026-10-02", model.CutBelly)
	if !ok {
		t.Fatalf("★ 应有 10-02 的部位级成本日快照(cut_cost_daily)")
	}
	if !snap.OpeningKg.Equal(d("100")) {
		t.Errorf("★ 快照期初应是 100kg, got %s", snap.OpeningKg)
	}
	if !snap.OpeningAmount.Equal(d("1800")) {
		t.Errorf("★ 快照期初金额应是 100 × 18 = 1800 元, got %s", snap.OpeningAmount)
	}
	if !snap.SoldKg.Equal(d("5")) {
		t.Errorf("★ 快照销量应是 5kg, got %s", snap.SoldKg)
	}
	if !snap.SalesCostYuan.Equal(d("90")) {
		t.Errorf("★ 快照销售成本应是 5kg × 18 = 90 元, got %s", snap.SalesCostYuan)
	}

	// ② ★ 旧 SKU 级成本日快照**不再写** ⇒ 期初不再落「主 SKU」。
	var n int64
	if err := testDBSingleton.Model(&model.SkuCostDaily{}).
		Where("branch_id = ?", "S001").Count(&n).Error; err != nil {
		t.Fatalf("count sku_cost_daily: %v", err)
	}
	if n != 0 {
		t.Errorf("★ 旧 SKU 级成本日快照不该再写(期初不再落主 SKU), got %d 行", n)
	}

	// ③ 货号行不带库存/成本键(期初、期末是部位口径)。
	row, ok := fmPerSkuRow(t, rep, "P-BELLY-B")
	if !ok {
		t.Fatalf("per_sku 应含 P-BELLY-B, got %+v", rep.PerSku)
	}
	if row.OpeningKg != nil || row.ClosingKg != nil || row.CounterKg != nil || row.ColdStoreKg != nil {
		t.Errorf("★ 货号行不该带库存/成本(那是部位口径), got %+v", row)
	}
	if row.SalesCostYuan != nil || row.GrossMarginYuan != nil {
		t.Errorf("★ 货号行不该带成本/毛利(那是部位口径), got %+v", row)
	}
	if !row.SoldKg.Equal(d("5")) {
		t.Errorf("★ 货号行的销量应是 5kg, got %s", row.SoldKg)
	}
}

// TestPerSku_RowJSONHasNoStockKeys 锁 ④(**序列化后的 JSON**)。
//
// # 为什么要在 JSON 层锁
//
// `decimal.Decimal` 是 struct ⇒ `omitempty` 对它**无效**
// ⇒ 用值类型时零值照样序列化成 `"opening_kg":"0"`
// ⇒ 前端会显示"期初 0"(门店会误读成"我盘出来是 0 kg")。
//
// ⇒ 库存/成本/毛利必须用**指针** ⇒ nil ⇒ **键缺失**。
//
// ⚠️ 键名一个都不改(前端不用动)⇒ 只是"有值"变成"键缺失"。
func TestPerSku_RowJSONHasNoStockKeys(t *testing.T) {
	svc, ctx := fmMultiSkuSetup(t, []map[string]any{
		fmCubeSalesRow("P-BELLY-A", "5", "100"),
		fmCubeSalesRow("P-BELLY-B", "3", "60"),
	}, "P-BELLY-A", "P-BELLY-B")
	fmSaveBellyOpening(t, svc, ctx, "100", "18")

	rep, err := svc.ComputeGrossMargin(ctx, "S001", fmDay2)
	mustNoErr(t, err, "ComputeGrossMargin(10-02)")
	if len(rep.PerSku) != 2 {
		t.Fatalf("per_sku 应有 2 行, got %d", len(rep.PerSku))
	}

	raw, err := json.Marshal(rep.PerSku)
	mustNoErr(t, err, "json.Marshal(per_sku)")
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("unmarshal per_sku: %v", err)
	}

	// 库存 / 成本 / 毛利 ⇒ **键缺失**(不是 0)。
	mustAbsent := []string{
		"opening_kg", "opening_amount",
		"expected_closing_kg", "closing_kg", "closing_amount",
		"counter_kg", "cold_store_kg",
		"produced_amount", "waste_amount",
		"moving_avg_unit_cost", "sales_cost_yuan", "bulk_cost_yuan", "waste_cost_yuan",
		"gross_margin_yuan", "gross_margin_pct",
		"presale_margin_yuan", "presale_expected_revenue_yuan", "presale_expected_unit_yuan",
	}
	// 销售 / 营收 / 产出 ⇒ **恒有键**(货号粒度真实存在的量)。
	mustPresent := []string{
		"branch_id", "cube_product_id", "cut_type",
		"produced_kg", "sold_kg", "sold_amount",
		"bulk_sold_kg", "bulk_sold_amount", "waste_kg", "revenue_yuan", "data_source",
	}
	for i, row := range rows {
		for _, k := range mustAbsent {
			if _, ok := row[k]; ok {
				t.Errorf("★ per_sku[%d] 不该有 %q 键 —— 库存/成本/毛利是部位口径;"+
					"值 0 会让门店误读成「盘出来是 0」", i, k)
			}
		}
		for _, k := range mustPresent {
			if _, ok := row[k]; !ok {
				t.Errorf("★ per_sku[%d] 该有 %q 键 —— 销售/营收是货号粒度真实存在的量", i, k)
			}
		}
	}
}

// fmPerSkuRow 从每日毛利报告里取某个货号的明细行(找不到返 false)。
func fmPerSkuRow(t *testing.T, rep *service.GrossMarginReport, sku string) (service.PerSkuGrossMargin, bool) {
	t.Helper()
	for _, r := range rep.PerSku {
		if r.CubeProductID == sku {
			return r, true
		}
	}
	return service.PerSkuGrossMargin{}, false
}
