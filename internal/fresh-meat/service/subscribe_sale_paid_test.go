package service_test

// 2026-10-24:**`sale.paid`(POS 真正发的那条 topic)**的回归锁。
//
// # 它守的是什么
//
// POS 发的 topic 是 `sale.paid`(internal/pos/service/events.go:TopicSalePaid),
// 而鲜肉服务此前订阅的是 `sale.completed` ⇒ **POS 的销售一条都没进来过**
// (生产日志实测:到 2026-10-24 为止,零条真实 sale 事件)。
//
// ⇒ 零售猪肉的整猪归属(以及之后的部位毛利)全都等在这条上。
//
// # 两种事件形状的差别
//
//	sale.completed:lines[] 有 `sku_id` + `fresh_type` + `completed_at`
//	sale.paid      :lines[] 有 `product_id`(= 思迅货号)且**没有 fresh_type**,
//	                顶层用 `paid_at`
//
// ⇒ 没有 fresh_type 时,"这是不是猪肉行"只能靠**我们自己的表**判定
// (部位映射 / 特价档)。
//
// ⚠️ 2026-10-24:大宗(冷库里的板油被整批处理掉)**不再由 POS 自动采信**
// (门店口径:大宗结算手工登记,大宗品项不再绑 POS 货号)
// ⇒ POS 事件只做**零售**归属。

import (
	"context"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

var (
	fmLoc  = time.FixedZone("Asia/Shanghai", 8*3600)
	fmDay1 = time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC) // = 10-01 14:00 +08
	fmDay2 = time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC) // = 10-02 14:00 +08
	fmClock = func(d time.Time) func() time.Time {
		return func() time.Time { return d.In(fmLoc) }
	}
)

// TestSalePaid_CapturesPorkLinesOnly 锁"只收我们管的货号"。
func TestSalePaid_CapturesPorkLinesOnly(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()
	day := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)
	svc.SetClock(fmClock(day))

	// 五花肉有映射(⇒ 是我们的猪肉);可可乐没有(⇒ 不是本域的事)。
	_, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-BELLY"})
	mustNoErr(t, err, "CreateBranchCutMapping belly")

	pig, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-1", GrossWeightKg: d("200"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: day,
	}, "u-1")
	mustNoErr(t, err, "RecordWholePig")

	// sale.paid 形状:product_id + paid_at,**没有 fresh_type**。
	n, err := svc.OnSaleCompleted(ctx, service.SaleCompletedPayload{
		OpID: "op-1", SaleID: "SO-1", BranchID: "S001",
		PaidAt:      day.Add(2 * time.Hour),
		OrderStatus: "S",
		Lines: []service.SaleLine{
			{LineID: "L-BELLY", ProductID: "P-BELLY", Qty: d("2"),
				UnitPriceYuan: d("18"), AmountYuan: d("36")},
			{LineID: "L-COLA", ProductID: "P-COLA", Qty: d("1"),
				UnitPriceYuan: d("3"), AmountYuan: d("3")},
		},
	})
	mustNoErr(t, err, "OnSaleCompleted sale.paid")
	if n != 1 {
		t.Fatalf("只应收 1 行(五花肉),可乐不是本域的货, got %d", n)
	}

	rows, err := svc.ListLineSalesByPig(ctx, "S001", pig.ID, nil, time.Time{}, time.Time{})
	mustNoErr(t, err, "ListLineSalesByPig")
	if len(rows) != 1 {
		t.Fatalf("应只有 1 行销售, got %d: %+v", len(rows), rows)
	}
	if rows[0].CubeProductID != "P-BELLY" {
		t.Errorf("cube_product_id = %s, want P-BELLY", rows[0].CubeProductID)
	}
	if rows[0].CutType != model.CutBelly {
		t.Errorf("cut_type = %s, want belly", rows[0].CutType)
	}
	if !rows[0].QtyKg.Equal(d("2")) {
		t.Errorf("qty = %s, want 2", rows[0].QtyKg)
	}
}
