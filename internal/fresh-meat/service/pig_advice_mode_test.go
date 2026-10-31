package service_test

// 2026-10-31 owner:「提交录入并生成分割建议时,要在提交**前**选择用什么方式生成,
// 有基线时默认使用基线,其次 LLM。」
//
// # 锁的是**门店选的那个模式真的生效**
//
//	① 录猪能带 advice_mode 落库(空 = auto,兼容老前端);
//	② 「只用基线」⇒ **不调 LLM**(不花钱、不让门店等)且状态不是"生成中";
//	③ 非法值 ⇒ 400(别静默当 auto);
//	④ 成本链**逐猪**按模式取数:
//	   auto     = 基线覆盖的部位用基线 + 没覆盖的部位用 LLM;
//	   baseline = 只有基线;
//	   llm      = 只有 LLM(完全不查基线);
//	⑤ 「只用基线」的猪**不能**手动重生成 AI 建议
//	   (否则"AI 建议生成好了"但成本链根本不用 ⇒ 门店以为生效了)。
//
// # ⚠️ 为什么必须逐猪判、不能按天统一
//
//	同一天可能一头用基线、一头用 AI ⇒ 按天统一取数会让**选错的那头**生效。

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"gorm.io/datatypes"
)

// modeBand 是本文件用的基线体重段(对应 111kg 那头猪 ⇒ 见 WeightBandOf)。
const modeBand = "100-149"

// modePig 造一头带「建议生成方式」的猪(不落库,直接喂给引擎)。
func modePig(t *testing.T, mode string, adviceJSON string) model.WholePig {
	t.Helper()
	pig := model.WholePig{
		ID: "WP-MODE", BranchID: "S001", GrossWeightKg: d("111"),
		PurchaseUnitPriceYuan: d("14.6"), PurchaseCostYuan: d("1620.6"),
		ArrivedAt:  time.Date(2026, 10, 31, 6, 0, 0, 0, time.UTC),
		AdviceMode: mode,
	}
	if adviceJSON != "" {
		pig.LLMAdviceJSON = datatypes.JSON(adviceJSON)
	}
	return pig
}

// TestRecordWholePig_AdviceModePersisted 锁①:模式落库;空 = auto。
func TestRecordWholePig_AdviceModePersisted(t *testing.T) {
	svc, _ := setupTestService(t)
	fake := &fakePredictFn{}
	svc.SetPredictFn(fake.Predict)

	pig, err := svc.RecordWholePig(context.Background(), "S001",
		model.RecordWholePigInput{
			EarTag: "EB-MODE", GrossWeightKg: d("111"), PurchaseUnitPriceYuan: d("14.6"),
			SupplierID: "SUP-1", ArrivedAt: time.Date(2026, 10, 31, 6, 0, 0, 0, time.UTC),
			AdviceMode: model.AdviceModeLLM,
		}, "u-1")
	mustNoErr(t, err, "RecordWholePig")
	if pig.AdviceMode != model.AdviceModeLLM {
		t.Errorf("★ advice_mode 应落库为 llm,got %q", pig.AdviceMode)
	}

	// 空 ⇒ auto(老前端不带这个字段也不能坏)。
	pig2, err := svc.RecordWholePig(context.Background(), "S001",
		model.RecordWholePigInput{
			EarTag: "EB-MODE2", GrossWeightKg: d("111"), PurchaseUnitPriceYuan: d("14.6"),
			SupplierID: "SUP-1", ArrivedAt: time.Date(2026, 10, 31, 6, 0, 0, 0, time.UTC),
		}, "u-1")
	mustNoErr(t, err, "RecordWholePig(不带 advice_mode)")
	if pig2.AdviceMode != model.AdviceModeAuto {
		t.Errorf("★ 不带 advice_mode 应落成 auto,got %q", pig2.AdviceMode)
	}
}

// TestRecordWholePig_AdviceModeBaseline_SkipsLLM 锁②:只用基线 ⇒ 不调 LLM。
//
// ⚠️ 判别力:把「跳过 LLM」那段去掉(fake.calls 会变成 1)⇒ 这条必须红。
func TestRecordWholePig_AdviceModeBaseline_SkipsLLM(t *testing.T) {
	svc, _ := setupTestService(t)
	// 这个体重段**有**基线(否则录猪会被拦:见下一条用例)。
	splitBaseline(t, model.CutBelly, modeBand, "30")
	fake := &fakePredictFn{}
	svc.SetPredictFn(fake.Predict)

	pig, err := svc.RecordWholePig(context.Background(), "S001",
		model.RecordWholePigInput{
			EarTag: "EB-BASE", GrossWeightKg: d("111"), PurchaseUnitPriceYuan: d("14.6"),
			SupplierID: "SUP-1", ArrivedAt: time.Date(2026, 10, 31, 6, 0, 0, 0, time.UTC),
			AdviceMode: model.AdviceModeBaseline,
		}, "u-1")
	mustNoErr(t, err, "RecordWholePig")

	if fake.calls != 0 {
		t.Errorf("★ 「只用基线」不该调 LLM,got %d 次调用", fake.calls)
	}
	// 状态不能停在"生成中" ⇒ 前端会一直转圈等一个永远不会来的建议。
	if pig.AdviceStatus != model.AdviceStatusNone {
		t.Errorf("★ 「只用基线」不该是 generating,got %q", pig.AdviceStatus)
	}
}

// TestRecordWholePig_AdviceModeBaseline_NoBaselineForBand 锁「只用基线」但没基线 ⇒ 400。
//
// ⚠️ 为什么必须拦:这头猪会**一条建议都没有** ⇒ 当天产出凭空少一整头猪
//
//	⇒ 到处是「缺口 / 短卖 / 幻影搭赠」⇒ 门店会以为系统算错了。
func TestRecordWholePig_AdviceModeBaseline_NoBaselineForBand(t *testing.T) {
	svc, _ := setupTestService(t)
	fake := &fakePredictFn{}
	svc.SetPredictFn(fake.Predict)

	// ⚠️ **不**插基线 ⇒ 111kg 落在 100-149 段 ⇒ 该段没有基线。
	_, err := svc.RecordWholePig(context.Background(), "S001",
		model.RecordWholePigInput{
			EarTag: "EB-NOBASE", GrossWeightKg: d("111"), PurchaseUnitPriceYuan: d("14.6"),
			SupplierID: "SUP-1", ArrivedAt: time.Date(2026, 10, 31, 6, 0, 0, 0, time.UTC),
			AdviceMode: model.AdviceModeBaseline,
		}, "u-1")
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Errorf("★ 没有基线时选「只用基线」应被拒,got %v", err)
	}
}

// TestRecordWholePig_AdviceModeInvalid 锁③:非法值 ⇒ 400(别静默当 auto)。
func TestRecordWholePig_AdviceModeInvalid(t *testing.T) {
	svc, _ := setupTestService(t)
	_, err := svc.RecordWholePig(context.Background(), "S001",
		model.RecordWholePigInput{
			EarTag: "EB-BAD", GrossWeightKg: d("111"), PurchaseUnitPriceYuan: d("14.6"),
			SupplierID: "SUP-1", ArrivedAt: time.Date(2026, 10, 31, 6, 0, 0, 0, time.UTC),
			AdviceMode: "ai",
		}, "u-1")
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Errorf("★ 非法 advice_mode 应返 ErrInvalidInput,got %v", err)
	}
}

// TestSuggestedSplitKg_AdviceModes 锁④:三种模式逐猪取数。
//
// 基线:belly p50 = 30;LLM 建议:belly 20.5(基线已覆盖)+ rib 15(基线没覆盖)。
//
//	auto     ⇒ belly 30(基线赢) + rib 15(LLM 兜底);
//	baseline ⇒ belly 30,**没有 rib**(不用 LLM);
//	llm      ⇒ belly 20.5 + rib 15(**不查基线**)。
func TestSuggestedSplitKg_AdviceModes(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()
	// 基线:belly @ 100-149 = 30kg(借 splitBaseline 的写库方式)。
	splitBaseline(t, model.CutBelly, modeBand, "30")

	advice := `{"cuts":[{"cut_type":"belly","expected_kg":20.5},{"cut_type":"rib","expected_kg":15}]}`

	cases := []struct {
		mode    string
		wantBel string
		wantRib string // 空 = 该部位不该出现
	}{
		{model.AdviceModeAuto, "30", "15"},
		{model.AdviceModeBaseline, "30", ""},
		{model.AdviceModeLLM, "20.5", "15"},
	}
	for _, c := range cases {
		got, err := service.SuggestedSplitKgForTest(ctx, svc, "S001",
			[]model.WholePig{modePig(t, c.mode, advice)})
		mustNoErr(t, err, "loadSuggestedSplitKg")
		if !got[string(model.CutBelly)].Equal(d(c.wantBel)) {
			t.Errorf("★ mode=%s belly 应是 %s,got %s", c.mode, c.wantBel, got[string(model.CutBelly)])
		}
		if c.wantRib == "" {
			if v, ok := got[string(model.CutRib)]; ok {
				t.Errorf("★ mode=%s 不该有 rib(LLM 没用上),got %s", c.mode, v)
			}
		} else if !got[string(model.CutRib)].Equal(d(c.wantRib)) {
			t.Errorf("★ mode=%s rib 应是 %s,got %s", c.mode, c.wantRib, got[string(model.CutRib)])
		}
	}
}

// TestSuggestedSplitKg_ModesArePerPig 锁④的"逐猪"性质。
//
// ⚠️ 同一天两头猪:一头 baseline、一头 llm ⇒ 两头各自按自己的模式取数
//
//	(按天统一取数 ⇒ 其中一头必然拿错)。
func TestSuggestedSplitKg_ModesArePerPig(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()
	splitBaseline(t, model.CutBelly, modeBand, "30")

	advice := `{"cuts":[{"cut_type":"belly","expected_kg":20.5}]}`
	basePig := modePig(t, model.AdviceModeBaseline, advice)
	basePig.ID = "WP-A"
	llmPig := modePig(t, model.AdviceModeLLM, advice)
	llmPig.ID = "WP-B"

	got, err := service.SuggestedSplitKgForTest(ctx, svc, "S001",
		[]model.WholePig{basePig, llmPig})
	mustNoErr(t, err, "loadSuggestedSplitKg")
	// belly:基线 30 + LLM 20.5 = 50.5(两头各按自己的模式取数)。
	if !got[string(model.CutBelly)].Equal(d("50.5")) {
		t.Errorf("★ 两头猪应各自按模式取数(30 + 20.5 = 50.5),got %s", got[string(model.CutBelly)])
	}
}

// TestRegeneratePigAdvice_BaselineModeRejected 锁⑤:只用基线的猪不生成 AI 建议。
func TestRegeneratePigAdvice_BaselineModeRejected(t *testing.T) {
	svc, _ := setupTestService(t)
	splitBaseline(t, model.CutBelly, modeBand, "30")
	fake := &fakePredictFn{}
	svc.SetPredictFn(fake.Predict)

	pig, err := svc.RecordWholePig(context.Background(), "S001",
		model.RecordWholePigInput{
			EarTag: "EB-BASE2", GrossWeightKg: d("111"), PurchaseUnitPriceYuan: d("14.6"),
			SupplierID: "SUP-1", ArrivedAt: time.Date(2026, 10, 31, 6, 0, 0, 0, time.UTC),
			AdviceMode: model.AdviceModeBaseline,
		}, "u-1")
	mustNoErr(t, err, "RecordWholePig")

	_, err = svc.RegeneratePigAdvice(context.Background(), "S001", pig.ID)
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Errorf("★ 「只用基线」的猪重生成 AI 建议应被拒,got %v", err)
	}
	if fake.calls != 0 {
		t.Errorf("★ 被拒也不该调 LLM,got %d 次调用", fake.calls)
	}
}

// TestWholePigJSON_AdviceModeExposed 锁:模式会出现在 JSON 里(前端要靠它显示)。
func TestWholePigJSON_AdviceModeExposed(t *testing.T) {
	svc, _ := setupTestService(t)
	splitBaseline(t, model.CutBelly, modeBand, "30")
	fake := &fakePredictFn{}
	svc.SetPredictFn(fake.Predict)

	pig, err := svc.RecordWholePig(context.Background(), "S001",
		model.RecordWholePigInput{
			EarTag: "EB-JSON", GrossWeightKg: d("111"), PurchaseUnitPriceYuan: d("14.6"),
			SupplierID: "SUP-1", ArrivedAt: time.Date(2026, 10, 31, 6, 0, 0, 0, time.UTC),
			AdviceMode: model.AdviceModeBaseline,
		}, "u-1")
	mustNoErr(t, err, "RecordWholePig")

	b, err := json.Marshal(pig)
	mustNoErr(t, err, "marshal pig")
	var m map[string]any
	mustNoErr(t, json.Unmarshal(b, &m), "unmarshal pig")
	if m["advice_mode"] != model.AdviceModeBaseline {
		t.Errorf("★ JSON 里应带 advice_mode=baseline,got %v", m["advice_mode"])
	}
}
