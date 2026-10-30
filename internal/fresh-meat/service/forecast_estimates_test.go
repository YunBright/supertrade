package service_test

// 2026-10-30:分割基线的**估计样本**通道(owner 选的 A 方案)。
//
// # 锁的是什么(口径,不是实现细节)
//
//	① **倒推值按各猪毛重比例摊到每头猪** ⇒ 每头猪每个部位一条估计样本;
//	② **只信"有完整日终盘点"的倒推** —— 没有完整盘点时倒推只是"台面流量近似",噪声太大;
//	③ **只喂 derived 部位** —— 有 pig_cuts 实录的部位,实录本身就是更准的样本;
//	④ **独立通道** ⇒ 估计样本**不写进 pig_cuts**(不污染 FIFO 摊销 / 不锁死删猪);
//	⑤ **基线口径**:实录样本 ≥ 5 用实录,不够才用估计 ⇒ 用哪个池子记在 sample_source;
//	⑥ **门店隔离**:每店只用自己的样本 ⇒ A 店的样本不进 B 店的基线。
//
// ⚠ 这些用例都走**公开 API** + 真实 SQLite ⇒ 引擎接线错了也会红。

import (
	"context"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/shopspring/decimal"
)

// estPig 录一头猪(返回猪 ID);arrivedAt 决定它属于哪个营业日。
func estPig(t *testing.T, svc *service.Service, ctx context.Context, branchID, earTag, grossKg string,
	arrivedAt time.Time) string {
	t.Helper()
	pig, err := svc.RecordWholePig(ctx, branchID, model.RecordWholePigInput{
		EarTag: earTag, GrossWeightKg: d(grossKg), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: arrivedAt,
	}, "u-1")
	mustNoErr(t, err, "RecordWholePig "+earTag)
	return pig.ID
}

// estPigCut 给某头猪补录一条分割**实录**(⇒ 该部位来源是 recorded,不产生估计样本)。
func estPigCut(t *testing.T, svc *service.Service, ctx context.Context, branchID, pigID, sku, barcode, kg string) {
	t.Helper()
	_, err := svc.RecordPigCut(ctx, branchID, model.RecordPigCutInput{
		PigID: pigID, CutType: model.CutBelly, Barcode: barcode,
		WeightKg: d(kg), ExpiresAt: splitDay.Add(48 * time.Hour), CubeProductID: sku,
	}, "u-1")
	mustNoErr(t, err, "RecordPigCut "+barcode)
}

// estStocktake 提交一张**完整**日终盘点(归属日 = takenDate;空 = 时钟那天)。
func estStocktake(t *testing.T, svc *service.Service, ctx context.Context, branchID, sku, remainKg, takenDate string) {
	t.Helper()
	_, err := svc.RecordPorkCutsStocktake(ctx, branchID, model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d(remainKg)},
		},
		TakenDate: takenDate,
	}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake")
}

// estPartialStocktake 提交一张**半盘**(is_complete=false)。
//
// ⚠️ 半盘**不算**实盘 ⇒ 盘点口径是 partial ⇒ 倒推仍走"台面流量近似"。
func estPartialStocktake(t *testing.T, svc *service.Service, ctx context.Context, branchID, sku, remainKg string) {
	t.Helper()
	_, err := svc.RecordPorkCutsStocktake(ctx, branchID, model.RecordPorkCutsStocktakeInput{
		IsComplete: false,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d(remainKg)},
		},
	}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake(半盘)")
}

// estBaselineSeed 直接写一条统计基线(省掉"先造 5 头历史猪"的麻烦)。
func estBaselineSeed(t *testing.T, branchID string, cut model.CutType, band, p50 string) {
	t.Helper()
	mustNoErr(t, testDBSingleton.Create(&model.PigForecastBaseline{
		ID: "PFB-" + branchID + "-" + string(cut) + "-" + band, BranchID: branchID,
		WeightBand: band, CutType: cut, SampleCount: 5, SampleSource: service.SampleSourceRecorded,
		MeanKg: d(p50), P10Kg: d(p50), P50Kg: d(p50), P90Kg: d(p50),
		ComputedAt: time.Now(),
	}).Error, "插统计基线")
}

// estRows 读某店的估计样本行。
func estRows(t *testing.T, branchID string) []model.PigCutEstimate {
	t.Helper()
	var rows []model.PigCutEstimate
	mustNoErr(t, testDBSingleton.Where("branch_id = ?", branchID).
		Order("pig_id ASC, cut_type ASC").Find(&rows).Error, "读 pig_cut_estimates")
	return rows
}

// estKgByPig 某店某个部位上,每头猪的估计 kg。
func estKgByPig(t *testing.T, branchID string, cut model.CutType) map[string]decimal.Decimal {
	t.Helper()
	out := map[string]decimal.Decimal{}
	for _, r := range estRows(t, branchID) {
		if r.CutType == cut {
			out[r.PigID] = r.WeightKg
		}
	}
	return out
}

// estBaseline 读某店某分层的基线行。
func estBaseline(t *testing.T, branchID, band string, cut model.CutType) (model.PigForecastBaseline, bool) {
	t.Helper()
	var row model.PigForecastBaseline
	err := testDBSingleton.Where("branch_id = ? AND weight_band = ? AND cut_type = ?", branchID, band, cut).
		First(&row).Error
	if err != nil {
		return model.PigForecastBaseline{}, false
	}
	return row, true
}

// ---- ① 倒推值按毛重比例摊到每头猪 ----

// TestForecastEstimates_SplitByGrossWeight 锁「按毛重比例摊 + Σ 守恒」。
//
// 两头猪:200kg / 100kg(同一营业日)⇒ 估计值之比应是 2:1。
// ⚠️ 并且 Σ 各头猪的估计值 **恰好等于** 当天该部位的倒推实际分割(尾差补给最大的那头)。
func TestForecastEstimates_SplitByGrossWeight(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	estBaselineSeed(t, "S001", model.CutBelly, "200-249", "20")

	pigBig := estPig(t, svc, ctx, "S001", "EB-BIG", "200", splitDay)
	pigSmall := estPig(t, svc, ctx, "S001", "EB-SMALL", "100", splitDay)
	estStocktake(t, svc, ctx, "S001", sku, "5", "")

	// 这一天算完了 ⇒ 落成本快照 ⇒ 顺带重建估计样本。
	_, err := svc.PersistCutCostDaily(ctx, "S001", splitDay)
	mustNoErr(t, err, "PersistCutCostDaily")

	byPig := estKgByPig(t, "S001", model.CutBelly)
	if len(byPig) != 2 {
		t.Fatalf("★ 应有 2 头猪的估计样本(每个部位一条), got %d: %+v", len(byPig), byPig)
	}
	big, ok := byPig[pigBig]
	if !ok {
		t.Fatalf("★ 大猪 %s 应有估计样本: %+v", pigBig, byPig)
	}
	small, ok := byPig[pigSmall]
	if !ok {
		t.Fatalf("★ 小猪 %s 应有估计样本: %+v", pigSmall, byPig)
	}
	if !big.GreaterThan(small) {
		t.Errorf("★ 毛重大的猪估计值应更多: big=%s small=%s", big, small)
	}
	// 比例 = 毛重比例 200:100 = 2:1。
	ratio := big.Div(small)
	if ratio.Sub(d("2")).Abs().GreaterThan(d("0.0001")) {
		t.Errorf("★ 估计值之比应是毛重比例 2:1, got %s", ratio)
	}

	// ⚠️ Σ 恰好等于当天的倒推实际分割(报表上那个数)。
	rep, err := svc.ComputeGrossMargin(ctx, "S001", splitDay)
	mustNoErr(t, err, "ComputeGrossMargin")
	belly := findPerCut(t, rep.PerCut, model.CutBelly)
	if belly.ActualSplitKg == nil {
		t.Fatalf("★ 有完整日终盘点 ⇒ 实际分割必须有值")
	}
	// ⚠️ Σ 应**等于**倒推实际分割。
	//
	//	误差判据 = 0.01kg(= 列精度 decimal(10,3) 的 1 个最小单位):
	//	样本按列精度落库 ⇒ Σ 只能"约等于";PG 上会被舍入到 3 位小数,
	//	SQLite 上 decimal 列更是按 float64 存 ⇒ 会有 1e-16 级微差。
	//	(基线是**统计拟合**(p50)⇒ 这个量级的舍入对样本没有影响。)
	total := big.Add(small)
	if diff := total.Sub(*belly.ActualSplitKg).Abs(); diff.GreaterThan(d("0.01")) {
		t.Errorf("★ Σ 估计样本应约等于倒推实际分割 %s(容差 0.01), got %s", *belly.ActualSplitKg, total)
	}
}

// ---- ② 只信"有完整日终盘点"的倒推 ----

// TestForecastEstimates_NoCompleteStocktake_NoSamples 锁「没完整盘点 ⇒ 不产样本」。
//
// ⚠️ 没有完整盘点时,倒推走的是"台面流量近似"(搬上台是老货 ⇒ 只能扣掉估计)
// ⇒ 噪声太大 ⇒ **不能**喂基线(会把基线教歪)。
//
// # ⚠️ 这个用例必须真的**造得出倒推值**,否则是假绿
//
//	无实盘口径:A_c = 销售 + 统收 + 报损 − 期初 − 搬上台
//	⇒ 造不出 A_c > 0 ⇒ **就算撤掉**过滤条件,也不会有样本 ⇒ 用例测不出任何东西。
//
//	⇒ 这里造:卖 10kg belly(⇒ A_belly = 10 > 0)且**不给**统计基线
//	  (建议分割 S_belly = 0 ⇒ 不会被显示规则隐藏成 nil)。
//
//	⇒ 两个分支都验:完全没有盘点单(口径 estimated)、只有半盘(口径 partial)。
func TestForecastEstimates_NoCompleteStocktake_NoSamples(t *testing.T) {
	for _, tc := range []struct {
		name        string
		partialTake bool
	}{
		{name: "完全没有盘点单", partialTake: false},
		{name: "只有半盘(不是完整盘点)", partialTake: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, ctx, sku := splitSetup(t)
			// 1 头猪(当天进场)⇒ "按毛重摊到每头猪"才有对象。
			estPig(t, svc, ctx, "S001", "EB-1", "200", splitDay)
			// ⚠️ **不**给统计基线 ⇒ 建议分割 S_belly = 0(单测里 LLM 兜底不出数)。
			// 卖 10kg belly ⇒ 台面近似倒推出 A_belly = 10kg > 0。
			splitSell(t, svc, sku, "L-1", "10", "200")
			if tc.partialTake {
				estPartialStocktake(t, svc, ctx, "S001", sku, "5")
			}
			_, err := svc.PersistCutCostDaily(ctx, "S001", splitDay)
			mustNoErr(t, err, "PersistCutCostDaily")

			// ---- 前提:报表上**确实有** derived 的实际分割 ----
			// (否则就算撤掉过滤条件也测不出东西 ⇒ 假绿)
			rep, err := svc.ComputeGrossMargin(ctx, "S001", splitDay)
			mustNoErr(t, err, "ComputeGrossMargin")
			belly := findPerCut(t, rep.PerCut, model.CutBelly)
			if belly.ActualSplitKg == nil || !belly.ActualSplitKg.IsPositive() {
				t.Fatalf("★ 前提:没有完整盘点时也应倒推出实际分割(台面近似), got %v", belly.ActualSplitKg)
			}

			if rows := estRows(t, "S001"); len(rows) != 0 {
				t.Errorf("★ 没有完整日终盘点 ⇒ 不该产生估计样本, got %d 行: %+v", len(rows), rows)
			}
		})
	}
}

// ---- ③ 只喂 derived 部位(有实录的部位不估) ----

// TestForecastEstimates_RecordedCutNotEstimated 锁「有实录的部位不产生估计样本」。
func TestForecastEstimates_RecordedCutNotEstimated(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	estBaselineSeed(t, "S001", model.CutBelly, "200-249", "20")
	pigID := estPig(t, svc, ctx, "S001", "EB-1", "200", splitDay)
	// 门店手工补录了 belly 的分割 ⇒ 该部位来源是 recorded ⇒ 不该被倒推覆盖。
	estPigCut(t, svc, ctx, "S001", pigID, sku, "B-1", "30")
	estStocktake(t, svc, ctx, "S001", sku, "5", "")

	_, err := svc.PersistCutCostDaily(ctx, "S001", splitDay)
	mustNoErr(t, err, "PersistCutCostDaily")

	if rows := estRows(t, "S001"); len(rows) != 0 {
		t.Errorf("★ 有 pig_cuts 实录的部位不该产生估计样本, got %d 行: %+v", len(rows), rows)
	}
}

// ---- ④ 独立通道:不污染 pig_cuts ----

// TestForecastEstimates_DoesNotTouchPigCuts 锁「估计样本不写进 pig_cuts」。
//
// ⚠️ pig_cuts 同时是「按猪摊成本」的输入 + 删猪时的"有补录"锁
//
//	⇒ 把估计值混进去 ⇒ 污染 FIFO 摊销 + 锁死删猪。
func TestForecastEstimates_DoesNotTouchPigCuts(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	estBaselineSeed(t, "S001", model.CutBelly, "200-249", "20")
	estPig(t, svc, ctx, "S001", "EB-1", "200", splitDay)
	estStocktake(t, svc, ctx, "S001", sku, "5", "")

	_, err := svc.PersistCutCostDaily(ctx, "S001", splitDay)
	mustNoErr(t, err, "PersistCutCostDaily")

	var n int64
	mustNoErr(t, testDBSingleton.Model(&model.PigCut{}).Count(&n).Error, "count pig_cuts")
	if n != 0 {
		t.Errorf("★ 估计样本不得写进 pig_cuts(会污染 FIFO 摊销 / 锁死删猪), got %d 行", n)
	}
	if rows := estRows(t, "S001"); len(rows) == 0 {
		t.Errorf("★ 估计样本应落在 pig_cut_estimates(独立通道)")
	}
}

// ---- ⑤ 基线口径:实录优先 / 估计兜底 ----

// TestForecastBaselines_RecordedWinsOverEstimated 锁「实录样本够 ⇒ 用实录」。
//
// 同一个分层(200-249 × belly)里**两个池子的样本都够(各 5 条)**:
//
//   - 实录 5 条(前一天 5 头猪各自补录 belly 10kg);
//   - 估计 5 条(当天 5 头猪**不**补录 ⇒ 倒推摊出来的)。
//
// ⇒ 基线必须取**实录**的 p50 = 10(不是估计的 p50)。
//
// ⚠️ 两个池子都必须 ≥ MinForecastSamples ⇒ 才可以真正区分"谁优先"。
func TestForecastBaselines_RecordedWinsOverEstimated(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	prevDay := splitDay.AddDate(0, 0, -1)

	// 前一天:5 头猪 + 各自实录 belly 10kg ⇒ 实录池 5 个样本(都在 200-249 段)。
	for i := 0; i < 5; i++ {
		tag := string(rune('A' + i))
		pigID := estPig(t, svc, ctx, "S001", "EB-"+tag, "200", prevDay)
		estPigCut(t, svc, ctx, "S001", pigID, sku, "B-"+tag, "10")
	}
	// 前一天也要有盘点单(跨日顺序校验:不补录昨天,今天录不了猪)。
	estStocktake(t, svc, ctx, "S001", sku, "5", prevDay.Format("2006-01-02"))

	// 当天:5 头猪(**不**补录)⇒ 倒推值摊到它们身上 ⇒ 估计池 5 个样本。
	for i := 0; i < 5; i++ {
		estPig(t, svc, ctx, "S001", "EB-T"+string(rune('A'+i)), "200", splitDay)
	}
	estStocktake(t, svc, ctx, "S001", sku, "5", "")
	_, err := svc.PersistCutCostDaily(ctx, "S001", splitDay)
	mustNoErr(t, err, "PersistCutCostDaily")

	if rows := estRows(t, "S001"); len(rows) < 5 {
		t.Fatalf("★ 当天 5 头猪 ⇒ 至少 5 条估计样本, got %d", len(rows))
	}

	n, err := svc.RecomputeForecastBaselines(ctx, "S001")
	mustNoErr(t, err, "RecomputeForecastBaselines")
	if n == 0 {
		t.Fatalf("基线应算出至少 1 行")
	}
	row, ok := estBaseline(t, "S001", "200-249", model.CutBelly)
	if !ok {
		t.Fatalf("应有 200-249 段的 belly 基线")
	}
	if row.SampleSource != service.SampleSourceRecorded {
		t.Errorf("★ 实录样本够(≥5)⇒ 基线应取实录池, got sample_source=%q", row.SampleSource)
	}
	if !row.P50Kg.Equal(d("10")) {
		t.Errorf("★ p50 应是实录的 10, got %s", row.P50Kg)
	}
	if row.RecordedSampleCount != 5 {
		t.Errorf("★ recorded_sample_count 应是 5, got %d", row.RecordedSampleCount)
	}
	if row.EstimatedSampleCount != 5 {
		t.Errorf("★ estimated_sample_count 应是 5(留痕), got %d", row.EstimatedSampleCount)
	}
}

// TestForecastBaselines_EstimatedFallback 锁「实录不够 ⇒ 用估计」。
//
// 实录 0 条 + 估计 5 条(5 头猪)⇒ 基线必须取**估计**的 p50,且 sample_source=estimated。
func TestForecastBaselines_EstimatedFallback(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	// 5 头猪(都没有补录分割)⇒ 倒推值按毛重摊到 5 头猪 ⇒ 估计池 5 个样本。
	for i := 0; i < 5; i++ {
		estPig(t, svc, ctx, "S001", "EB-"+string(rune('A'+i)), "200", splitDay)
	}
	estStocktake(t, svc, ctx, "S001", sku, "5", "")
	_, err := svc.PersistCutCostDaily(ctx, "S001", splitDay)
	mustNoErr(t, err, "PersistCutCostDaily")

	if rows := estRows(t, "S001"); len(rows) < 5 {
		t.Fatalf("★ 5 头猪 ⇒ 至少 5 条估计样本, got %d", len(rows))
	}

	_, err = svc.RecomputeForecastBaselines(ctx, "S001")
	mustNoErr(t, err, "RecomputeForecastBaselines")
	row, ok := estBaseline(t, "S001", "200-249", model.CutBelly)
	if !ok {
		t.Fatalf("应有 200-249 段的 belly 基线")
	}
	if row.SampleSource != service.SampleSourceEstimated {
		t.Errorf("★ 实录不够 ⇒ 基线应取估计池, got sample_source=%q", row.SampleSource)
	}
	if row.SampleCount != 5 {
		t.Errorf("★ sample_count 应是采用的估计池样本数 5, got %d", row.SampleCount)
	}
	if row.RecordedSampleCount != 0 {
		t.Errorf("★ recorded_sample_count 应是 0, got %d", row.RecordedSampleCount)
	}
	if !row.P50Kg.IsPositive() {
		t.Errorf("★ 估计池的 p50 应 > 0, got %s", row.P50Kg)
	}
	// 本轮没有跨店借用 ⇒ 样本来源门店 = 本店。
	if row.SourceBranchID != "S001" {
		t.Errorf("★ source_branch_id 本轮应等于本店, got %q", row.SourceBranchID)
	}
}

// ---- ⑥ 门店隔离 ----

// TestForecastBaselines_StoreIsolation 锁「每店只用自己的样本」。
//
// ⚠️ owner 2026-10-30:「每个门店样本要单独」⇒ A 店的样本**不能**进 B 店的基线。
// ⇒ 同一个库里放两家店 ⇒ 才真的验到"查询按 branch_id 过滤"。
func TestForecastBaselines_StoreIsolation(t *testing.T) {
	svc, ctx, sku := splitSetup(t)

	// S001:5 头猪 ⇒ 5 条估计样本。
	estBaselineSeed(t, "S001", model.CutBelly, "200-249", "20")
	for i := 0; i < 5; i++ {
		estPig(t, svc, ctx, "S001", "EB-"+string(rune('A'+i)), "200", splitDay)
	}
	estStocktake(t, svc, ctx, "S001", sku, "5", "")
	_, err := svc.PersistCutCostDaily(ctx, "S001", splitDay)
	mustNoErr(t, err, "PersistCutCostDaily")
	_, err = svc.RecomputeForecastBaselines(ctx, "S001")
	mustNoErr(t, err, "RecomputeForecastBaselines")

	// S002:自己的映射 + 1 头猪 ⇒ 只有 1 条估计样本。
	_, err = svc.CreateBranchCutMapping(ctx, "S002",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-BELLY"})
	mustNoErr(t, err, "CreateBranchCutMapping(S002)")
	estBaselineSeed(t, "S002", model.CutBelly, "200-249", "20")
	estPig(t, svc, ctx, "S002", "EB-B1", "200", splitDay)
	estStocktake(t, svc, ctx, "S002", sku, "5", "")
	_, err = svc.PersistCutCostDaily(ctx, "S002", splitDay)
	mustNoErr(t, err, "PersistCutCostDaily")
	_, err = svc.RecomputeForecastBaselines(ctx, "S002")
	mustNoErr(t, err, "RecomputeForecastBaselines")

	rowB, ok := estBaseline(t, "S002", "200-249", model.CutBelly)
	if !ok {
		t.Fatalf("S002 应有自己的基线行")
	}
	if rowB.EstimatedSampleCount != 1 {
		t.Errorf("★ S002 的基线只该用它自己的 1 条估计样本, got %d"+
			"(S001 的 5 条漏进来了 ⇒ 门店隔离破了)", rowB.EstimatedSampleCount)
	}
	// S001 的基线也不能被 S002 的重算改掉。
	rowA, ok := estBaseline(t, "S001", "200-249", model.CutBelly)
	if !ok {
		t.Fatalf("S001 应有自己的基线行")
	}
	if rowA.EstimatedSampleCount != 5 {
		t.Errorf("★ S001 的基线应仍是它自己的 5 条估计样本, got %d", rowA.EstimatedSampleCount)
	}
}

// ---- ⑦ 幂等 ----

// TestForecastEstimates_Idempotent 锁「重复重建结果一致」。
func TestForecastEstimates_Idempotent(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	estBaselineSeed(t, "S001", model.CutBelly, "200-249", "20")
	estPig(t, svc, ctx, "S001", "EB-1", "200", splitDay)
	estPig(t, svc, ctx, "S001", "EB-2", "240", splitDay)
	estStocktake(t, svc, ctx, "S001", sku, "5", "")

	_, err := svc.PersistCutCostDaily(ctx, "S001", splitDay)
	mustNoErr(t, err, "PersistCutCostDaily")
	first := estKgByPig(t, "S001", model.CutBelly)

	_, err = svc.PersistCutCostDaily(ctx, "S001", splitDay)
	mustNoErr(t, err, "PersistCutCostDaily")
	second := estKgByPig(t, "S001", model.CutBelly)

	if len(first) != len(second) {
		t.Fatalf("★ 重复重建后行数应不变: %d → %d", len(first), len(second))
	}
	for pig, kg := range first {
		got, ok := second[pig]
		if !ok {
			t.Fatalf("★ 重复重建后 %s 的样本不见了", pig)
		}
		if !got.Equal(kg) {
			t.Errorf("★ 重复重建后 %s 的估计值应不变: %s → %s", pig, kg, got)
		}
	}
}

// ---- ⑧ 读取侧:基线列表按 branch_id 过滤 ----

// TestForecastBaselines_ListIsStoreScoped 锁「列基线只列本店的」。
func TestForecastBaselines_ListIsStoreScoped(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	estBaselineSeed(t, "S001", model.CutBelly, "200-249", "20")
	for i := 0; i < 5; i++ {
		estPig(t, svc, ctx, "S001", "EB-"+string(rune('A'+i)), "200", splitDay)
	}
	estStocktake(t, svc, ctx, "S001", sku, "5", "")
	_, err := svc.PersistCutCostDaily(ctx, "S001", splitDay)
	mustNoErr(t, err, "PersistCutCostDaily")
	_, err = svc.RecomputeForecastBaselines(ctx, "S001")
	mustNoErr(t, err, "RecomputeForecastBaselines")

	rows, err := svc.ListForecastBaselines(ctx, "S001")
	mustNoErr(t, err, "ListForecastBaselines")
	if len(rows) == 0 {
		t.Fatalf("本店的基线应能列出来")
	}
	for _, r := range rows {
		if r.BranchID != "S001" {
			t.Errorf("★ 列表里不该出现别店的基线: %+v", r)
		}
	}
	other, err := svc.ListForecastBaselines(ctx, "S002")
	mustNoErr(t, err, "ListForecastBaselines(S002)")
	for _, r := range other {
		if r.BranchID != "S002" {
			t.Errorf("★ S002 的列表里不该出现别店的基线: %+v", r)
		}
	}
}
