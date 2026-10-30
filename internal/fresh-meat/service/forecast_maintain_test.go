package service_test

// 2026-10-30:基线**维护** —— 删脏基线行。
//
// # ⚠️ 2026-10-30 owner 重新定义模型：**样本库是一等公民，基线是派生结果**
//
//	「按一天生成基线」把**样本采集**和**基线拟合**混成了一个动作 ⇒ 已下线
//	（改用「按一天加入样本」+「重算」，见 forecast_samples_test.go）。
//	⇒ 本文件只保留**删基线行**这条低层动作。
//
// # 锁的是什么（口径，不是实现细节）
//
//	① 删除只删**本店**那一行（别家店同一分层必须还在）；
//	② 删除的参数不全 ⇒ 400（别一把删光）。
//
// ⚠ 这些用例都走**公开 API** + 真实 SQLite ⇒ 引擎接线错了也会红。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/shopspring/decimal"
)

// fmGenDate 是本文件用的营业日（与 splitDay 同一天：2026-10-29）。
const fmGenDate = "2026-10-29"

// fmSeedCompleteDay 铺一个"数据完整的一天"：录入 + 搬台 + 销售 + 完整盘点。
//
// ⚠️ 搬台用 **0 kg 的「今天不搬台」标记** —— 门店口径：有时整日不搬台，
//
//	但"这一步已确认"仍要有流水（owner 2026-10-30：「含 0 搬台」）。
func fmSeedCompleteDay(t *testing.T, svc *service.Service, ctx context.Context, sku string) string {
	t.Helper()
	// ① 录入：当天进猪。
	pigID := estPig(t, svc, ctx, "S001", "EB-GEN", "200", splitDay)
	// ② 搬台：0 kg 的标记行（有流水 = 搬过这一步）。
	_, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		Lines: []service.CounterLoadLine{{QtyKg: decimal.Zero}},
	}, "u-1")
	mustNoErr(t, err, "搬台标记")
	// ③ 销售：一笔零售。
	splitSell(t, svc, sku, "L-GEN", "10", "200")
	// ④ 盘点：完整盘点单。
	estStocktake(t, svc, ctx, "S001", sku, "5", "")
	return pigID
}

// ---- ① 删除：只删本店那一行 ----

// TestDeleteForecastBaseline_OnlyOwnBranch 锁「删除只删本店的行」。
//
// ⚠️ 别家店同一分层的基线**必须还在** —— 否则一个店能删掉另一个店的数据。
func TestDeleteForecastBaseline_OnlyOwnBranch(t *testing.T) {
	svc, ctx, _ := splitSetup(t)
	fmBorrowSeed(t, "S001", "200-249", model.CutBelly, 5, "10")
	fmBorrowSeed(t, "S002", "200-249", model.CutBelly, 5, "20")

	n, err := svc.DeleteForecastBaseline(ctx, "S001", "200-249", string(model.CutBelly))
	mustNoErr(t, err, "DeleteForecastBaseline")
	if n != 1 {
		t.Errorf("★ 应删掉 1 行,got %d", n)
	}
	if _, ok := estBaseline(t, "S001", "200-249", model.CutBelly); ok {
		t.Errorf("★ 本店那一行应被删掉")
	}
	if _, ok := estBaseline(t, "S002", "200-249", model.CutBelly); !ok {
		t.Errorf("★ 别家店的行不该被删")
	}
}

// TestDeleteForecastBaseline_RequiresBandAndCut 锁「参数不全 ⇒ 400（别一把删光）」。
func TestDeleteForecastBaseline_RequiresBandAndCut(t *testing.T) {
	svc, ctx, _ := splitSetup(t)
	for _, tc := range []struct{ band, cut string }{
		{"", string(model.CutBelly)},
		{"200-249", ""},
		{"", ""},
	} {
		_, err := svc.DeleteForecastBaseline(ctx, "S001", tc.band, tc.cut)
		if err == nil {
			t.Errorf("★ band=%q cut=%q 时应报错(别把整店基线删光)", tc.band, tc.cut)
		}
		if !errors.Is(err, service.ErrInvalidInput) {
			t.Errorf("应是输入校验错(400),got %v", err)
		}
	}
}

// fmSellBranch 记一笔零售销售（指定门店）。
//
// ⚠️ splitSell 写死了 S001 ⇒ 跨店用例要自己铺。
func fmSellBranch(t *testing.T, svc *service.Service, branchID, sku, lineID string) {
	t.Helper()
	_, err := svc.OnSaleCompleted(context.Background(), service.SaleCompletedPayload{
		SaleID: "SO-" + lineID, BranchID: branchID, CompletedAt: splitDay.Add(time.Hour),
		Lines: []service.SaleLine{{
			LineID: lineID, SKUID: sku, FreshType: "meat",
			Qty: d("10"), AmountYuan: d("200"), PigID: "PIG-" + branchID, CutType: model.CutBelly,
		}},
	})
	mustNoErr(t, err, "OnSaleCompleted "+branchID)
}

// fmStocktakeBranch 提交一张**完整**日终盘点（指定门店）。
func fmStocktakeBranch(t *testing.T, svc *service.Service, branchID, sku string) {
	t.Helper()
	_, err := svc.RecordPorkCutsStocktake(context.Background(), branchID, model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("5")},
		},
		TakenDate: "",
	}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake "+branchID)
}

// fmBorrowSeed 直接写一条基线行（省掉"先造 5 头历史猪"的麻烦）。
func fmBorrowSeed(t *testing.T, branchID, band string, cut model.CutType, sampleCount int, p50 string) {
	t.Helper()
	mustNoErr(t, testDBSingleton.Create(&model.PigForecastBaseline{
		ID: "PFB-" + branchID + "-" + string(cut) + "-" + band, BranchID: branchID,
		WeightBand: band, CutType: cut, SampleCount: sampleCount,
		SampleSource: service.SampleSourceRecorded, SourceBranchID: branchID,
		MeanKg: d(p50), P10Kg: d(p50), P50Kg: d(p50), P90Kg: d(p50),
		ComputedAt: time.Now(),
	}).Error, "插统计基线")
}
