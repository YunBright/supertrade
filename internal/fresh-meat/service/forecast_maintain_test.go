package service_test

// 2026-10-30:基线**维护**(owner 第 3 条)——删脏基线 + 按某一天生成新基线。
//
// # 锁的是什么(口径,不是实现细节)
//
//	① 生成基线**不设 ≥5 门槛** —— 那天的数据(录入/搬台/销售/盘点)齐全就能生成;
//	   生成的行 manual_generated = true ⇒ **可用**。
//	② 前置校验:那一天缺哪一项就 400 并**点名缺哪一项**(门店才知道要补什么)。
//	③ overwrite = false ⇒ 已有分层**不动**(报 skipped)。
//	④ 删除只删**本店**那一行。
//
// ⚠ 这些用例都走**公开 API** + 真实 SQLite ⇒ 引擎接线错了也会红。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/shopspring/decimal"
)

// fmGenDate 是本文件用的营业日(与 splitDay 同一天:2026-10-29)。
const fmGenDate = "2026-10-29"

// fmSeedCompleteDay 铺一个"数据完整的一天":录入 + 搬台 + 销售 + 完整盘点。
//
// ⚠️ 搬台用 **0 kg 的「今天不搬台」标记** —— 门店口径:有时整日不搬台,
//
//	但"这一步已确认"仍要有流水(owner 2026-10-30:「含 0 搬台」)。
func fmSeedCompleteDay(t *testing.T, svc *service.Service, ctx context.Context, sku string) string {
	t.Helper()
	// ① 录入:当天进猪。
	pigID := estPig(t, svc, ctx, "S001", "EB-GEN", "200", splitDay)
	// ② 搬台:0 kg 的标记行(有流水 = 搬过这一步)。
	_, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		Lines: []service.CounterLoadLine{{QtyKg: decimal.Zero}},
	}, "u-1")
	mustNoErr(t, err, "搬台标记")
	// ③ 销售:一笔零售。
	splitSell(t, svc, sku, "L-GEN", "10", "200")
	// ④ 盘点:完整盘点单。
	estStocktake(t, svc, ctx, "S001", sku, "5", "")
	return pigID
}

// ---- ① 前置校验:缺哪一项点哪一项 ----

// TestGenerateBaselines_MissingAnyItem_400NamesIt 锁「数据不完整 ⇒ 400 且点名缺什么」。
//
// ⚠️ 报错必须**点名**缺哪一项 —— 只说"数据不完整"门店没法行动。
func TestGenerateBaselines_MissingAnyItem_400NamesIt(t *testing.T) {
	cases := []struct {
		name string
		skip string // 故意不铺哪一项
		want string // 期望被点名的关键词
	}{
		{"缺录入(当天没进猪)", "录入", "录入"},
		{"缺搬台", "搬台", "搬台"},
		{"缺销售", "销售", "销售"},
		{"缺盘点(只有半盘也不行)", "盘点", "盘点"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, ctx, sku := splitSetup(t)
			if tc.skip != "录入" {
				estPig(t, svc, ctx, "S001", "EB-GEN", "200", splitDay)
			}
			if tc.skip != "搬台" {
				_, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
					Lines: []service.CounterLoadLine{{QtyKg: decimal.Zero}},
				}, "u-1")
				mustNoErr(t, err, "搬台标记")
			}
			if tc.skip != "销售" {
				splitSell(t, svc, sku, "L-GEN", "10", "200")
			}
			if tc.skip != "盘点" {
				estStocktake(t, svc, ctx, "S001", sku, "5", "")
			}

			_, err := svc.GenerateForecastBaselines(ctx, "S001", service.ForecastBaselineGenerateInput{
				Date: fmGenDate, Overwrite: true,
			})
			if err == nil {
				t.Fatalf("★ 缺[%s]时不该生成成功", tc.skip)
			}
			if !errors.Is(err, service.ErrInvalidInput) {
				t.Errorf("应是输入校验错(400),got %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("★ 报错要点名缺[%s](门店才知道要补什么),got %v", tc.want, err)
			}
		})
	}
}

// ---- ② 数据齐全 ⇒ 写出可用(绕过 ≥5)的手动基线 ----

// TestGenerateBaselines_Complete_WritesUsableManualRow 锁「数据齐全 ⇒ manual_generated 且可用」。
//
// ⚠️ **不设 ≥5 门槛**(owner 明确要求)⇒ 这一行只有 1 个样本也必须**能用**。
func TestGenerateBaselines_Complete_WritesUsableManualRow(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	pigID := fmSeedCompleteDay(t, svc, ctx, sku)
	// 一条实录(当天这头猪的分割补录)⇒ 实录池 1 个样本(远不到 5)。
	estPigCut(t, svc, ctx, "S001", pigID, sku, "B-GEN", "30")

	out, err := svc.GenerateForecastBaselines(ctx, "S001", service.ForecastBaselineGenerateInput{
		Date: fmGenDate, Overwrite: true,
	})
	mustNoErr(t, err, "GenerateForecastBaselines")
	if out.Written != 1 {
		t.Fatalf("★ 应写出 1 行基线,got %d: %+v", out.Written, out)
	}
	if out.Skipped != 0 {
		t.Errorf("★ 第一次生成不该有 skipped,got %d", out.Skipped)
	}
	if out.Date != fmGenDate {
		t.Errorf("★ 返回的 date 应是 %s,got %s", fmGenDate, out.Date)
	}

	row, ok := estBaseline(t, "S001", "200-249", model.CutBelly)
	if !ok {
		t.Fatalf("★ 应有 200-249 段的 belly 基线")
	}
	if !row.ManualGenerated {
		t.Errorf("★ 手动生成的行 manual_generated 必须为 true")
	}
	if row.GeneratedFromDate != fmGenDate {
		t.Errorf("★ generated_from_date 应是 %s,got %q", fmGenDate, row.GeneratedFromDate)
	}
	if row.SampleSource != service.SampleSourceRecorded {
		t.Errorf("★ 实录池优先,sample_source=%q", row.SampleSource)
	}
	if !row.P50Kg.Equal(d("30")) {
		t.Errorf("★ p50 应是那天的实录 30,got %s", row.P50Kg)
	}

	// ⚠️ **可用**(绕过 ≥5 门槛)—— 这是 owner 的核心要求。
	base, ok2, err := svc.ForecastFor(ctx, "S001", d("200"), model.CutBelly)
	mustNoErr(t, err, "ForecastFor")
	if !ok2 {
		t.Fatalf("★ 手动生成的基线必须可用(样本只有 %d,但 owner 要求不设 ≥5 门槛)", row.SampleCount)
	}
	if !base.ManualGenerated {
		t.Errorf("★ 读回来的基线应带 manual_generated 标记")
	}
}

// ---- ③ overwrite = false ⇒ 已有分层不动 ----

// TestGenerateBaselines_OverwriteFalse_KeepsExisting 锁「overwrite=false ⇒ 已有分层不被改」。
func TestGenerateBaselines_OverwriteFalse_KeepsExisting(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	pigID := fmSeedCompleteDay(t, svc, ctx, sku)
	estPigCut(t, svc, ctx, "S001", pigID, sku, "B-GEN", "30")

	_, err := svc.GenerateForecastBaselines(ctx, "S001", service.ForecastBaselineGenerateInput{
		Date: fmGenDate, Overwrite: true,
	})
	mustNoErr(t, err, "GenerateForecastBaselines(第一次)")

	// 管理员手工改过这一行(模拟"这一行是特意调过的")。
	mustNoErr(t, testDBSingleton.Model(&model.PigForecastBaseline{}).
		Where("branch_id = ? AND weight_band = ? AND cut_type = ?", "S001", "200-249", model.CutBelly).
		Update("p50_kg", decimal.NewFromInt(99)).Error, "改 p50")

	out, err := svc.GenerateForecastBaselines(ctx, "S001", service.ForecastBaselineGenerateInput{
		Date: fmGenDate, Overwrite: false,
	})
	mustNoErr(t, err, "GenerateForecastBaselines(overwrite=false)")
	if out.Written != 0 {
		t.Errorf("★ overwrite=false 不该写任何行,got %d", out.Written)
	}
	if out.Skipped != 1 {
		t.Errorf("★ 已有分层应报 skipped=1,got %d", out.Skipped)
	}
	row, ok := estBaseline(t, "S001", "200-249", model.CutBelly)
	if !ok {
		t.Fatalf("★ 原有基线不该被删")
	}
	if !row.P50Kg.Equal(d("99")) {
		t.Errorf("★ overwrite=false ⇒ 已有分层不许被改,p50 应仍是 99,got %s", row.P50Kg)
	}
}

// ---- ③b overwrite = true ⇒ 覆盖 ----

// TestGenerateBaselines_OverwriteTrue_Replaces 锁「overwrite=true ⇒ 覆盖已有分层」。
func TestGenerateBaselines_OverwriteTrue_Replaces(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	pigID := fmSeedCompleteDay(t, svc, ctx, sku)
	estPigCut(t, svc, ctx, "S001", pigID, sku, "B-GEN", "30")

	_, err := svc.GenerateForecastBaselines(ctx, "S001", service.ForecastBaselineGenerateInput{
		Date: fmGenDate, Overwrite: true,
	})
	mustNoErr(t, err, "GenerateForecastBaselines(第一次)")
	mustNoErr(t, testDBSingleton.Model(&model.PigForecastBaseline{}).
		Where("branch_id = ? AND weight_band = ? AND cut_type = ?", "S001", "200-249", model.CutBelly).
		Update("p50_kg", decimal.NewFromInt(99)).Error, "改 p50")

	out, err := svc.GenerateForecastBaselines(ctx, "S001", service.ForecastBaselineGenerateInput{
		Date: fmGenDate, Overwrite: true,
	})
	mustNoErr(t, err, "GenerateForecastBaselines(第二次)")
	if out.Written != 1 {
		t.Errorf("★ overwrite=true 应写 1 行,got %d", out.Written)
	}
	row, ok := estBaseline(t, "S001", "200-249", model.CutBelly)
	if !ok {
		t.Fatalf("★ 应有基线行")
	}
	if !row.P50Kg.Equal(d("30")) {
		t.Errorf("★ overwrite=true ⇒ 旧值应被覆盖成 30,got %s", row.P50Kg)
	}
}

// ---- ④ 删除:只删本店那一行 ----

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

// TestDeleteForecastBaseline_RequiresBandAndCut 锁「参数不全 ⇒ 400(别一把删光)」。
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

// fmSellBranch 记一笔零售销售(指定门店)。
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

// fmStocktakeBranch 提交一张**完整**日终盘点(指定门店)。
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

// ---- ⑤ 手动跨店选用:用别店那一天的数据生成到本店的基线表 ----

// TestGenerateBaselines_FromOtherBranch_RecordsSourceBranch 锁「手动跨店选用」。
//
// source_branch_id = 别店 ⇒ 用**别店那一天**的数据生成到**本店的基线表**
// ⇒ 本店那一行的 source_branch_id 记**被借方**。
func TestGenerateBaselines_FromOtherBranch_RecordsSourceBranch(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	// S002 也要有映射(它自己那天的数据)。
	_, err := svc.CreateBranchCutMapping(ctx, "S002",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: sku})
	mustNoErr(t, err, "CreateBranchCutMapping(S002)")

	// S002 的那一天:进猪 + 搬台 + 销售 + 盘点 + 实录。
	svc.SetClock(fmClock(splitDay))
	pigID := estPig(t, svc, ctx, "S002", "EB-S002", "220", splitDay)
	estPigCut(t, svc, ctx, "S002", pigID, sku, "B-S002", "18")
	_, err = svc.CounterLoad(ctx, "S002", service.CounterLoadInput{
		Lines: []service.CounterLoadLine{{QtyKg: decimal.Zero}},
	}, "u-1")
	mustNoErr(t, err, "S002 搬台标记")
	fmSellBranch(t, svc, "S002", sku, "L-S002")
	fmStocktakeBranch(t, svc, "S002", sku)

	out, err := svc.GenerateForecastBaselines(ctx, "S001", service.ForecastBaselineGenerateInput{
		Date: fmGenDate, SourceBranchID: "S002", Overwrite: true,
	})
	mustNoErr(t, err, "GenerateForecastBaselines(跨店)")
	if out.Written != 1 {
		t.Fatalf("★ 应写出 1 行,got %d", out.Written)
	}
	if out.SourceBranchID != "S002" {
		t.Errorf("★ 返回的 source_branch_id 应回显 %q,got %q", "S002", out.SourceBranchID)
	}
	row, ok := estBaseline(t, "S001", "200-249", model.CutBelly)
	if !ok {
		t.Fatalf("★ 本店应有一行基线(用别店的数据)")
	}
	if row.SourceBranchID != "S002" {
		t.Errorf("★ 本店那一行的 source_branch_id 应记被借方 S002,got %q", row.SourceBranchID)
	}
	if !row.P50Kg.Equal(d("18")) {
		t.Errorf("★ p50 应是 S002 那天的 18,got %s", row.P50Kg)
	}
	if !row.ManualGenerated {
		t.Errorf("★ 手动生成的行 manual_generated 必须为 true")
	}
}

// ---- ⑤ 分割补录:没有可拟合的分割重量 ⇒ 点名 ----

// TestGenerateBaselines_NoSplitSamples_400NamesIt 锁「没有分割重量 ⇒ 400 点名」。
//
// ⚠️ 为什么这一项必须有(2026-10-30 门店实测):
//
//	没有分割重量 ⇒ 生成结果是 **written=0**
//	⇒ 门店点「生成」只看到"已生成 0 条基线" ⇒ 以为按钮坏了,却不知道要做什么。
//	⇒ 宁可 400 点名"缺:分割补录",也不要给一个**空的成功**。
//
// ⚠️ 造"没有分割重量"的手法:盘点会**自动**倒推出估计样本(A 方案)
//
//	⇒ 这里把自动产生的估计样本删掉,模拟**旧账日**
//	(生产实况:店 00 的 10-29 盘点发生在 A 方案上线之前 ⇒ 那天没有任何分割重量)。
func TestGenerateBaselines_NoSplitSamples_400NamesIt(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	// 四项齐全(录入/搬台/销售/盘点)。
	estPig(t, svc, ctx, "S001", "EB-NOSAMPLE", "200", splitDay)
	_, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		Lines: []service.CounterLoadLine{{QtyKg: decimal.Zero}},
	}, "u-1")
	mustNoErr(t, err, "搬台标记")
	splitSell(t, svc, sku, "L-NOSAMPLE", "10", "200")
	estStocktake(t, svc, ctx, "S001", sku, "5", "")

	// 模拟旧账日:把盘点自动倒推出的估计样本删掉(当天没有补录分割 ⇒ 真的没有分割重量)。
	mustNoErr(t, testDBSingleton.Where("branch_id = ?", "S001").
		Delete(&model.PigCutEstimate{}).Error, "删估计样本")

	_, err = svc.GenerateForecastBaselines(ctx, "S001", service.ForecastBaselineGenerateInput{
		Date: fmGenDate, Overwrite: true,
	})
	if err == nil {
		t.Fatalf("★ 没有分割重量时不该返回空成功(written=0 会让门店以为按钮坏了)")
	}
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Errorf("应是输入校验错(400),got %v", err)
	}
	if !strings.Contains(err.Error(), "分割补录") {
		t.Errorf("★ 报错要点名缺[分割补录](门店要补录分割才知道下一步),got %v", err)
	}
}

// TestGenerateBaselines_NoPigs_NoSplitSampleGap 锁「没进猪那天**不**额外报分割补录」。
//
// ⚠️ 没猪 ⇒ 天然没有分割重量 ⇒ 那是"缺录入"的**后果**,不是另一个独立缺口
// ⇒ 一次报全 ≠ 报一堆重复项(别让门店以为要补两遍)。
func TestGenerateBaselines_NoPigs_NoSplitSampleGap(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	// 不进猪;其余三项都铺齐。
	_, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		Lines: []service.CounterLoadLine{{QtyKg: decimal.Zero}},
	}, "u-1")
	mustNoErr(t, err, "搬台标记")
	splitSell(t, svc, sku, "L-NOPIG", "10", "200")
	estStocktake(t, svc, ctx, "S001", sku, "5", "")

	_, err = svc.GenerateForecastBaselines(ctx, "S001", service.ForecastBaselineGenerateInput{
		Date: fmGenDate, Overwrite: true,
	})
	if err == nil {
		t.Fatalf("★ 没进猪那天该报错")
	}
	if strings.Contains(err.Error(), "分割补录") {
		t.Errorf("★ 没进猪那天不该额外报[分割补录](那是缺录入的后果),got %v", err)
	}
	if !strings.Contains(err.Error(), "录入") {
		t.Errorf("★ 该点名缺[录入],got %v", err)
	}
}

// fmBorrowSeed 直接写一条基线行(省掉"先造 5 头历史猪"的麻烦)。
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
