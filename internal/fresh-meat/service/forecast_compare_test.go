package service_test

// 2026-10-30:**多门店分级基线对比**(owner 第 1 条)。
//
// # 锁的是什么(口径,不是实现细节)
//
//	① 按 (体重段, 部位) 分组,多家店的基线**并排**;
//	② **只列请求的门店** —— 别的店的数据一点都看不到;
//	③ 桶内给出 min/max/avg 的 p50 与"最大最小相差百分之几"(spread_pct)。
//
// ⚠ 这些用例都走**公开 API** + 真实 SQLite。

import (
	"testing"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

// TestCompareForecastBaselines_OnlyRequestedBranches 锁「只列请求的门店」。
func TestCompareForecastBaselines_OnlyRequestedBranches(t *testing.T) {
	svc, ctx, _ := splitSetup(t)
	fmBorrowSeed(t, "S001", "200-249", model.CutBelly, 5, "10")
	fmBorrowSeed(t, "S002", "200-249", model.CutBelly, 5, "12")
	fmBorrowSeed(t, "S003", "200-249", model.CutBelly, 5, "30") // 没被请求 ⇒ 不该出现

	out, err := svc.CompareForecastBaselines(ctx, []string{"S001", "S002"})
	mustNoErr(t, err, "CompareForecastBaselines")
	if len(out.Branches) != 2 || out.Branches[0] != "S001" || out.Branches[1] != "S002" {
		t.Fatalf("★ branches 应只含请求的门店,got %+v", out.Branches)
	}
	if len(out.Buckets) != 1 {
		t.Fatalf("★ 应只有 1 个分层桶,got %d: %+v", len(out.Buckets), out.Buckets)
	}
	bucket := out.Buckets[0]
	if bucket.WeightBand != "200-249" || bucket.CutType != model.CutBelly {
		t.Errorf("★ 分层键不对: %+v", bucket)
	}
	if len(bucket.Baselines) != 2 {
		t.Fatalf("★ 桶里应有 2 家店的行,got %d: %+v", len(bucket.Baselines), bucket.Baselines)
	}
	for _, r := range bucket.Baselines {
		if r.BranchID == "S003" {
			t.Errorf("★ 没被请求的门店不该出现: %+v", r)
		}
		if r.BranchID != "S001" && r.BranchID != "S002" {
			t.Errorf("★ 出现了意料之外的门店: %+v", r)
		}
		if !r.Usable {
			t.Errorf("★ 5 个样本的基线应可用: %+v", r)
		}
	}
	// min/max/avg/spread。
	if bucket.MinP50Kg != "10.000" {
		t.Errorf("★ min_p50_kg 应是 10.000,got %q", bucket.MinP50Kg)
	}
	if bucket.MaxP50Kg != "12.000" {
		t.Errorf("★ max_p50_kg 应是 12.000,got %q", bucket.MaxP50Kg)
	}
	if bucket.AvgP50Kg != "11.000" {
		t.Errorf("★ avg_p50_kg 应是 11.000,got %q", bucket.AvgP50Kg)
	}
	if bucket.SpreadPct < 19.99 || bucket.SpreadPct > 20.01 {
		t.Errorf("★ spread_pct 应是 20,got %v", bucket.SpreadPct)
	}
}

// TestCompareForecastBaselines_GroupsByBandAndCut 锁「按 (体重段, 部位) 分组」。
func TestCompareForecastBaselines_GroupsByBandAndCut(t *testing.T) {
	svc, ctx, _ := splitSetup(t)
	fmBorrowSeed(t, "S001", "200-249", model.CutBelly, 5, "10")
	fmBorrowSeed(t, "S001", "200-249", model.CutRib, 5, "20")
	fmBorrowSeed(t, "S001", "300-349", model.CutBelly, 5, "30")

	out, err := svc.CompareForecastBaselines(ctx, []string{"S001"})
	mustNoErr(t, err, "CompareForecastBaselines")
	if len(out.Buckets) != 3 {
		t.Fatalf("★ 三个分层应是三个桶,got %d: %+v", len(out.Buckets), out.Buckets)
	}
	// 排序:体重段升序,同段按部位。
	if out.Buckets[0].WeightBand != "200-249" || out.Buckets[0].CutType != model.CutBelly {
		t.Errorf("★ 第一个桶应是 200-249×belly,got %+v", out.Buckets[0])
	}
	if out.Buckets[1].CutType != model.CutRib {
		t.Errorf("★ 第二个桶应是 200-249×rib,got %+v", out.Buckets[1])
	}
	if out.Buckets[2].WeightBand != "300-349" {
		t.Errorf("★ 第三个桶应是 300-349,got %+v", out.Buckets[2])
	}
}

// TestCompareForecastBaselines_EmptyBranch_400 锁「没给门店 ⇒ 400」。
func TestCompareForecastBaselines_EmptyBranch_400(t *testing.T) {
	svc, ctx, _ := splitSetup(t)
	if _, err := svc.CompareForecastBaselines(ctx, nil); err == nil {
		t.Errorf("★ 没给门店应报错")
	}
}

// TestCompareForecastBaselines_ShowsManualAndBorrowFlags 锁「对比里能看出手动生成/借来的」。
func TestCompareForecastBaselines_ShowsManualAndBorrowFlags(t *testing.T) {
	svc, ctx, _ := splitSetup(t)
	// 一条手动生成的行(样本只有 1 个 ⇒ 不靠样本数也可用)。
	mustNoErr(t, testDBSingleton.Create(&model.PigForecastBaseline{
		ID: "PFB-manual-S001", BranchID: "S001", WeightBand: "200-249", CutType: model.CutBelly,
		SampleCount: 1, SampleSource: service.SampleSourceEstimated, SourceBranchID: "S002",
		ManualGenerated: true, GeneratedFromDate: "2026-10-29",
		MeanKg: d("10"), P10Kg: d("8"), P50Kg: d("10"), P90Kg: d("12"),
		ComputedAt: splitDay,
	}).Error, "插手动基线")

	out, err := svc.CompareForecastBaselines(ctx, []string{"S001"})
	mustNoErr(t, err, "CompareForecastBaselines")
	if len(out.Buckets) != 1 || len(out.Buckets[0].Baselines) != 1 {
		t.Fatalf("★ 应有一行: %+v", out.Buckets)
	}
	r := out.Buckets[0].Baselines[0]
	if !r.ManualGenerated {
		t.Errorf("★ manual_generated 应为 true: %+v", r)
	}
	if !r.Usable {
		t.Errorf("★ 手动生成的行应可用: %+v", r)
	}
	if r.SourceBranchID != "S002" {
		t.Errorf("★ source_branch_id 应是被借方 S002,got %q", r.SourceBranchID)
	}
	if r.P10Kg != "8.000" || r.P90Kg != "12.000" {
		t.Errorf("★ 重量应是固定 3 位小数的字符串: %+v", r)
	}
}
