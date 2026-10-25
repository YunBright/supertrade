// internal/fresh-meat/model/model_test.go —— 部位枚举的契约测试。
//
// 这个文件的作用是**锁住 2026-10-23 的那次枚举扩张**:
// 真源库(佳乐 1201 / 富百家 010303)的在售 SKU 需要映射到部位,
// 枚举是那套映射的**词表** —— 词表一改,历史盘点单/销售宽表里的
// cut_type 字符串就对不上了 ⇒ 这里把 wire 名单**逐字**钉死。
package model_test

import (
	"testing"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
)

// TestAllCutTypesWireList 钉死枚举的**顺序 + wire 名单**。
//
// 顺序 = 分组顺序(整猪 → 分割肉 → 骨 → 下水),前端下拉按同序渲染
// (Flutter: FreshMeatCutType.values)。改名单必须同时改 Flutter 那边。
func TestAllCutTypesWireList(t *testing.T) {
	want := []string{
		// A. 整猪
		"whole_pig",
		// B. 分割肉
		"head", "head_skin", "ear", "tongue", "neck",
		"leg_front", "shoulder_blade", "belly", "rib", "tenderloin",
		"lean", "leg_rear", "trotter", "tail", "skin", "fat", "local_pork", "promo",
		// C. 骨
		"bone_shank", "bone_scapula", "bone_spine", "bone_rib", "bone_head",
		// D. 下水
		"liver", "heart", "lung", "kidney", "stomach", "stomach_honeycomb",
		"intestine", "intestine_head", "intestine_small", "spleen", "tendon",
	}
	got := make([]string, 0, len(model.AllCutTypes))
	for _, c := range model.AllCutTypes {
		got = append(got, string(c))
	}
	if len(got) != len(want) {
		t.Fatalf("AllCutTypes 长度 = %d, want %d\n got=%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("AllCutTypes[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestLegacyCutTypesPreserved 锁住**向后兼容**契约:
// 2026-10-23 之前已有的 12 个 wire 值一个都不能改名/删除,
// 否则历史 pig_cuts / pork_cuts_stocktakes / line_sales_by_pigs 里的
// cut_type 字符串会变成"未知值"(前端显示原始串、盘点校验直接拒)。
func TestLegacyCutTypesPreserved(t *testing.T) {
	legacy := []model.CutType{
		model.CutBelly, model.CutTenderloin, model.CutRib,
		model.CutLegFront, model.CutLegRear, model.CutTrotter,
		model.CutLiver, model.CutIntestine, model.CutHeart,
		model.CutLung, model.CutKidney, model.CutStomach,
	}
	for _, c := range legacy {
		if !c.Valid() {
			t.Errorf("旧 wire %q 必须仍然有效", c)
		}
	}
}

// TestNoDuplicateWires 防"两个常量同一个 wire"—— 那会让 AllCutTypes 里
// 出现重复项,ListBranchCutMappings 的 ORDER BY cut_type 与前端下拉都会出岔子。
func TestNoDuplicateWires(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range model.AllCutTypes {
		if seen[string(c)] {
			t.Errorf("重复的 wire: %q", c)
		}
		seen[string(c)] = true
	}
}

// TestCutLabelsCoverAll 保证每个枚举值都有中文名(否则前端显示原始 wire)。
func TestCutLabelsCoverAll(t *testing.T) {
	for _, c := range model.AllCutTypes {
		if model.CutLabels[c] == "" {
			t.Errorf("%q 缺中文名", c)
		}
		if model.CutLabel(c) == string(c) {
			t.Errorf("%q 的 CutLabel 没有命中映射(回退成了 wire 本身)", c)
		}
	}
}

// TestOffalSet 锁住下水集合:11 类(肝/心/肺/肾/肚/小肚/大肠/大肠头/粉肠/脾/鞭)。
//
// 场景二"半头猪带全猪下水"靠它判断 —— 少一个就少算一头猪的下水产出。
func TestOffalSet(t *testing.T) {
	want := []model.CutType{
		model.CutLiver, model.CutHeart, model.CutLung, model.CutKidney,
		model.CutStomach, model.CutStomachHoneycomb, model.CutIntestine,
		model.CutIntestineHead, model.CutIntestineSmall, model.CutSpleen,
		model.CutTendon,
	}
	if len(model.OffalCutTypes) != len(want) {
		t.Fatalf("OffalCutTypes 长度 = %d, want %d", len(model.OffalCutTypes), len(want))
	}
	for _, c := range want {
		if !model.IsOffal(c) {
			t.Errorf("%q 应算下水", c)
		}
	}
	for _, c := range model.AllCutTypes {
		isOffalInWant := false
		for _, w := range want {
			if c == w {
				isOffalInWant = true
				break
			}
		}
		if model.IsOffal(c) != isOffalInWant {
			t.Errorf("%q 的 isOffal = %v, want %v", c, model.IsOffal(c), isOffalInWant)
		}
	}
}

// TestCarcassCutTypesExcludesLocalPork 锁住一条**语义**约束:
// local_pork(隔山肉)是品种级商品、promo(特价档)是促销档位 —— 都不是解剖部位
// ⇒ 不进整猪分割推演。混进去会让 LLM 去预测"这头猪能分割出多少隔山肉/特价肉"
// ⇒ 纯幻觉。
func TestCarcassCutTypesExcludesLocalPork(t *testing.T) {
	for _, c := range model.CarcassCutTypes {
		if c == model.CutLocalPork {
			t.Errorf("local_pork 不该出现在 CarcassCutTypes 里")
		}
		if c == model.CutPromo {
			t.Errorf("promo 不该出现在 CarcassCutTypes 里")
		}
	}
	if len(model.CarcassCutTypes) != len(model.AllCutTypes)-2 {
		t.Errorf("CarcassCutTypes = %d, want %d", len(model.CarcassCutTypes), len(model.AllCutTypes)-2)
	}
}

// TestValidRejectsUnknown 保证未知值被拒(RecordPigCut / stocktake 的第一道闸)。
func TestValidRejectsUnknown(t *testing.T) {
	if model.CutType("not_a_real_cut").Valid() {
		t.Errorf("未知 cut 应无效")
	}
	if model.CutType("").Valid() {
		t.Errorf("空 cut 应无效")
	}
}
