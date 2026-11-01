package service_test

// 「LLM 只补**缺销售数据**的部位」(2026-11-01 owner D8)的判据测试。
//
// # 这条契约守的是什么
//
//	owner 原话:「LLM 只补缺销售数据的部位(不再产整猪切割方案),不进成本。」
//
//	⇒ **可观测面**是提示词:焦点部位非空时,提示词里**只应出现那几个部位**。
//	⇒ 破坏它的表征是"LLM 又开始问整副猪" —— **没人看得出来**
//	(只是慢一点、贵一点、幻觉多一点)⇒ 只能靠直接锁提示词。
//
// # 为什么「缺销售数据」= 基线没覆盖
//
//	基线是从**样本库**拟合的,样本库 2026-11-01 起**只从销售数据采集**
//	⇒ 基线覆盖 (体重段 × 部位) ⇔ 该部位在该体重段有销售数据。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

// focusMapping 插一条部位映射(本店在售部位)。
func focusMapping(t *testing.T, id string, cut model.CutType, sku string) {
	t.Helper()
	mustNoErr(t, testDBSingleton.Create(&model.BranchCutMapping{
		ID: id, BranchID: "S001", CutType: cut, CubeProductID: sku,
	}).Error, "插部位映射 "+id)
}

// focusBaseline 插一条统计基线(该体重段该部位"有销售数据")。
func focusBaseline(t *testing.T, cut model.CutType, band, p50 string) {
	t.Helper()
	mustNoErr(t, testDBSingleton.Create(&model.PigForecastBaseline{
		ID: "PFB-" + string(cut) + "-" + band, BranchID: "S001",
		WeightBand: band, CutType: cut, SampleCount: 5,
		MeanKg: d(p50), P10Kg: d(p50), P50Kg: d(p50), P90Kg: d(p50),
		ComputedAt: time.Now(),
	}).Error, "插统计基线 "+string(cut))
}

// TestFocusCuts_OnlyMissingParts 锁「焦点 = 在售 − 基线已覆盖」。
func TestFocusCuts_OnlyMissingParts(t *testing.T) {
	svc, _ := setupTestService(t)
	focusMapping(t, "BCM-F1", model.CutBelly, "SKU-BELLY")
	focusMapping(t, "BCM-F2", model.CutRib, "SKU-RIB")
	focusMapping(t, "BCM-F3", model.CutLiver, "SKU-LIVER")
	// belly 在 100-149 段**有销售数据**(基线覆盖)⇒ 不该再问 LLM。
	focusBaseline(t, model.CutBelly, "100-149", "12")

	focus, err := service.FocusCutsForPigForTest(context.Background(), svc,
		"S001", "100-149", model.AdviceModeAuto)
	mustNoErr(t, err, "FocusCutsForPigForTest")
	if len(focus) != 2 {
		t.Fatalf("焦点部位应只有 2 个(rib/liver),got %d: %v", len(focus), focus)
	}
	got := map[model.CutType]bool{}
	for _, c := range focus {
		got[c] = true
	}
	if !got[model.CutRib] || !got[model.CutLiver] {
		t.Errorf("焦点应含 rib/liver,got %v", focus)
	}
	if got[model.CutBelly] {
		t.Errorf("★ belly 有基线(=有销售数据)⇒ 不该再问 LLM,got %v", focus)
	}
}

// TestFocusCuts_NoMapping_Unlimited 锁「判定不出缺口 ⇒ 不限(不退化报错)」。
func TestFocusCuts_NoMapping_Unlimited(t *testing.T) {
	svc, _ := setupTestService(t)
	focus, err := service.FocusCutsForPigForTest(context.Background(), svc,
		"S001", "100-149", model.AdviceModeAuto)
	mustNoErr(t, err, "FocusCutsForPigForTest")
	if focus != nil {
		t.Errorf("★ 本店还没配部位 ⇒ 应退回 nil(不限),got %v", focus)
	}
}

// TestFocusCuts_AdviceModeLLM_AllSellParts 锁「只用 AI ⇒ 本店在售部位全要」。
func TestFocusCuts_AdviceModeLLM_AllSellParts(t *testing.T) {
	svc, _ := setupTestService(t)
	focusMapping(t, "BCM-F1", model.CutBelly, "SKU-BELLY")
	focusMapping(t, "BCM-F2", model.CutRib, "SKU-RIB")
	focusBaseline(t, model.CutBelly, "100-149", "12")

	focus, err := service.FocusCutsForPigForTest(context.Background(), svc,
		"S001", "100-149", model.AdviceModeLLM)
	mustNoErr(t, err, "FocusCutsForPigForTest")
	if len(focus) != 2 {
		t.Fatalf("「只用 AI」⇒ 本店在售部位(2 个)全要,got %d: %v", len(focus), focus)
	}
}

// TestBuildSystemPrompt_OnlyFocusParts ⭐ 锁 owner D8 的**可观测面**。
func TestBuildSystemPrompt_OnlyFocusParts(t *testing.T) {
	prompt := service.BuildSystemPromptForTest([]model.CutType{model.CutLiver})
	if !strings.Contains(prompt, string(model.CutLiver)) {
		t.Errorf("提示词应包含焦点部位 liver: %s", prompt)
	}
	// ★ 焦点之外的部位**不许出现** ⇒ 否则 LLM 又在产整猪方案。
	for _, c := range model.CarcassCutTypes {
		if c == model.CutLiver {
			continue
		}
		if strings.Contains(prompt, string(c)) {
			t.Errorf("★ 提示词不应出现非焦点部位 %s: %s", c, prompt)
		}
	}
	if !strings.Contains(prompt, "只") || !strings.Contains(prompt, "部位") {
		t.Errorf("提示词应说明「只补这几个部位」: %s", prompt)
	}
}

// TestBuildSystemPrompt_Nil_ListsAll 锁「nil(不限)⇒ 仍然列全集」(退化行为)。
func TestBuildSystemPrompt_Nil_ListsAll(t *testing.T) {
	prompt := service.BuildSystemPromptForTest(nil)
	for _, c := range model.CarcassCutTypes {
		if !strings.Contains(prompt, string(c)) {
			t.Errorf("focus=nil 时提示词应列全部部位,缺 %s", c)
		}
	}
}

// TestBuildUserPrompt_NamesFocusParts 锁「user 侧点名要补哪几个部位」。
func TestBuildUserPrompt_NamesFocusParts(t *testing.T) {
	pig := &model.WholePig{
		ID: "WP-1", EarTag: "EB-1", GrossWeightKg: d("120"),
		PurchaseUnitPriceYuan: d("14.6"), SupplierID: "SUP-1",
	}
	prompt := service.BuildUserPromptForTest(pig, nil, []model.CutType{model.CutLiver})
	if !strings.Contains(prompt, string(model.CutLiver)) {
		t.Errorf("user prompt 应点名 liver: %s", prompt)
	}
	if !strings.Contains(prompt, "120") {
		t.Errorf("user prompt 应带猪重: %s", prompt)
	}
}
