// Package service_test —— 部位级成本链的**期初定价**回归(2026-10-27)。
//
// # 为什么单独锁这一条
//
// 上线第一天 `cut_cost_states`(部位级成本状态)是**空表** ⇒ 没有历史部位单价。
// 而生产真实形态是:店 00/01 各只有**一条**期初结转
// (trotter,2026-10-01:店 00 = 9.23kg @8元,店 01 = 42kg @6.9元)。
//
// ⇒ 如果期初只拿到 kg、拿不到金额 ⇒ 移动加权单价被稀释
//   ⇒ **成本低估、毛利虚高** —— 正是这次改造要消灭的坑,换个位置又冒出来。
//
// ⇒ 这里锁死:
//   ① 期初金额由**门店给的进价**兜住(不是 0);
//   ② 没有门店进价时,由**今日产出单价**兜住;
//   ③ 兜底后**恒等式仍然闭合**(对账差额 = 0)。
package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"gorm.io/datatypes"
)

// TestCutLevelOpening_PricedFromStoreOpening 锁"期初按**门店给的进价**定价"。
//
// 场景 = 生产真实形态:店 00 只有一条期初结转(trotter 9.23kg @8元,生效 2026-10-01),
// 且 `cut_cost_states` 没有任何历史部位单价(新表,第一天)。
//
// ⇒ 期初金额必须是 9.23 × 8 = 73.84 元 ⇒ 移动加权单价 = 8 元/kg。
// ⇒ 若按 0 元入账:移动加权单价 = 0 ⇒ 那批货零成本 ⇒ 毛利虚高。
func TestCutLevelOpening_PricedFromStoreOpening(t *testing.T) {
	svc, ctx := fmCutLevelSetup(t)

	// 门店在「鲜肉-设置」录的期初结转。
	if err := testDBSingleton.Create(&model.SkuStockOpening{
		ID:              "SSO-S001-TROTTER",
		BranchID:        "S001",
		CutType:         model.CutTrotter,
		ColdStoreKg:     d("9.23"),
		UnitPriceYuan:   d("8"),
		TotalAmountYuan: d("73.84"),
		BizDate:         datatypes.Date(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)),
	}).Error; err != nil {
		t.Fatalf("录期初结转: %v", err)
	}

	rep, err := svc.ComputeGrossMargin(ctx, "S001", fmDay2)
	mustNoErr(t, err, "ComputeGrossMargin(10-02)")

	row := fmCutLevelPerCut(t, rep.PerCut, model.CutTrotter)
	if row == nil {
		t.Fatalf("部位行应含 trotter: %+v", rep.PerCut)
	}
	if !row.OpeningKg.Equal(d("9.23")) {
		t.Errorf("trotter 期初应是 9.23kg,got %s", row.OpeningKg)
	}
	if !row.MovingAvgUnitCost.Equal(d("8")) {
		t.Errorf("trotter 移动加权单价应是 8(期初按门店给的进价定价),got %s", row.MovingAvgUnitCost)
	}
	if !row.ExpectedRemainKg.Equal(d("9.23")) {
		t.Errorf("trotter 应有应是 9.23kg(没卖没产出),got %s", row.ExpectedRemainKg)
	}
	// 恒等式:期初金额 + 入库金额 == 销售成本 + 统收 + 报损 + 期末存货。
	if rep.Reconciliation.DiffYuan.Abs().GreaterThan(d("0.01")) {
		t.Errorf("对账差额应是 0,got %s", rep.Reconciliation.DiffYuan)
	}
}

// TestCutLevelOpening_PricedFromProducedUnit 锁"没有门店进价时,用**今日产出单价**兜底"。
//
// 场景:门店录了期初 kg 但**没填进价**(unit_price_yuan = 0)⇒
//
//	旧:期初金额 0 ⇒ 移动加权单价被稀释 ⇒ 成本低估 ⇒ 毛利虚高。
//	新:用该部位**今日产出单价**给这批货估值(最接近的可用成本)。
//
// 算式(故意取整):
//
//	猪 50kg × 20 元/kg = 1000 元;今日切 trotter 10kg ⇒ 产出 10kg / 1000 元 ⇒ 100 元/kg
//	期初 9.23kg(无价)⇒ 按 100 元/kg 估值 = 923 元
//	可售 = (9.23 + 10)kg / (923 + 1000)元 ⇒ 移动加权单价 = 100 元/kg
func TestCutLevelOpening_PricedFromProducedUnit(t *testing.T) {
	svc, ctx := fmCutLevelSetup(t)

	// 期初有 kg、**没有**进价。
	if err := testDBSingleton.Create(&model.SkuStockOpening{
		ID:              "SSO-S001-TROTTER",
		BranchID:        "S001",
		CutType:         model.CutTrotter,
		ColdStoreKg:     d("9.23"),
		UnitPriceYuan:   d("0"),
		TotalAmountYuan: d("0"),
		BizDate:         datatypes.Date(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)),
	}).Error; err != nil {
		t.Fatalf("录期初结转: %v", err)
	}

	// 今日进猪并切出 trotter 10kg(⇒ 该部位今日产出单价 100 元/kg)。
	fmCutLevelSeedPig(t, svc, "TRO-1", "50", fmDay2)
	fmCutLevelSeedPigCut(t, svc, "TRO-1", model.CutTrotter, "P-TROTTER", "10")

	rep, err := svc.ComputeGrossMargin(ctx, "S001", fmDay2)
	mustNoErr(t, err, "ComputeGrossMargin(10-02)")

	row := fmCutLevelPerCut(t, rep.PerCut, model.CutTrotter)
	if row == nil {
		t.Fatalf("部位行应含 trotter: %+v", rep.PerCut)
	}
	if !row.OpeningKg.Equal(d("9.23")) {
		t.Errorf("trotter 期初应是 9.23kg,got %s", row.OpeningKg)
	}
	if !row.MovingAvgUnitCost.Equal(d("100")) {
		t.Errorf("trotter 移动加权单价应是 100(期初按今日产出单价兜底),got %s", row.MovingAvgUnitCost)
	}
	if !row.ExpectedRemainKg.Equal(d("19.23")) {
		t.Errorf("trotter 应有应是 19.23kg(9.23 + 10),got %s", row.ExpectedRemainKg)
	}
	if rep.Reconciliation.DiffYuan.Abs().GreaterThan(d("0.01")) {
		t.Errorf("对账差额应是 0,got %s", rep.Reconciliation.DiffYuan)
	}
}

// ---- 脚手架 ----

// fmCutLevelSetup 起服务 + 建一个 trotter 的货号映射。
func fmCutLevelSetup(t *testing.T) (*service.Service, context.Context) {
	t.Helper()
	svc, _ := setupTestService(t)
	ctx := context.Background()
	svc.SetClock(fmClock(fmDay2))
	if _, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutTrotter, CubeProductID: "P-TROTTER"}); err != nil {
		t.Fatalf("CreateBranchCutMapping: %v", err)
	}
	return svc, ctx
}

// fmCutLevelPerCut 找某个部位的部位行(找不到返 nil)。
func fmCutLevelPerCut(t *testing.T, rows []service.PerCutGrossMargin, cut model.CutType) *service.PerCutGrossMargin {
	t.Helper()
	for i := range rows {
		if rows[i].CutType == cut {
			return &rows[i]
		}
	}
	return nil
}

// fmCutLevelSeedPig 录一头猪(50kg × 20 元/kg = 1000 元)。
func fmCutLevelSeedPig(t *testing.T, svc *service.Service, earTag, grossKg string, arrivedAt time.Time) string {
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

// fmCutLevelSeedPigCut 录一条分割补录(权威的"这头猪产出了这个部位多少 kg")。
func fmCutLevelSeedPigCut(t *testing.T, svc *service.Service, earTag string, cut model.CutType, sku, kg string) {
	t.Helper()
	var pig model.WholePig
	if err := testDBSingleton.Where("branch_id = ? AND ear_tag = ?", "S001", earTag).
		First(&pig).Error; err != nil {
		t.Fatalf("查猪 %s: %v", earTag, err)
	}
	if _, err := svc.RecordPigCut(context.Background(), "S001", model.RecordPigCutInput{
		PigID: pig.ID, CutType: cut, Barcode: "B-" + pig.ID,
		WeightKg: d(kg), ExpiresAt: fmDay2.Add(48 * time.Hour), CubeProductID: sku,
	}, "u-1"); err != nil {
		t.Fatalf("RecordPigCut(%s): %v", earTag, err)
	}
}
