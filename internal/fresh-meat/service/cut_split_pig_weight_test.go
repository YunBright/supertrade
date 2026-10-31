package service

// **建议分割不得超过猪重**(2026-10-31 生产实测,店 00)。
//
// # 现场
//
//	门店 00 只录了 111kg 一头猪 ⇒ 毛利看板「进货」显示 **130.473kg**。
//	⇒ 多出来的 19.473kg 是**账面凭空造的货**:
//	  ① 产出量虚高 ⇒ 每个部位「应有」虚高 ⇒ 到处是幻影缺口(搭赠/盘亏);
//	  ② 入库金额仍是猪的真实成本 ⇒ 每公斤入库成本被摊薄
//	     (1620.6 ÷ 130.473 = 12.42 元/kg,进价却是 14.6)⇒ **毛利虚高**;
//	  ③ `切分损耗 = 猪重 − 产出` 变负数 ⇒ 被裁成 0 ⇒ 这笔货完全隐形。
//
// # 根因
//
//	基线 P50 是**绝对 kg**(隐含历史样本猪的重量:店 00 的 100-149 段 ⇒ 119.273kg),
//	今天的猪只有 111kg ⇒ 直接当绝对 kg 用 ⇒ 建议之和 > 猪重。
//
// # 本文件锁住
//
//	① 建议之和 > 猪重 ⇒ **按比例缩到恰好等于猪重**;
//	② 建议之和 ≤ 猪重 ⇒ **原样不动**(差额是真实的切分损耗,不能往上撑);
//	③ 缩放只改**总量**,不改各部位的**相对占比**。
import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestScaleSplitToPigWeight_ScalesDownToPigWeight(t *testing.T) {
	// Σ = 130 > 猪重 100 ⇒ 必须缩到 100。
	perPig := kgMap("belly", "60", "rib", "70")
	if !scaleSplitToPigWeight(perPig, dsv("100")) {
		t.Fatal("★ 建议之和 130 > 猪重 100 ⇒ 应触发缩放")
	}
	if got := sumKg(perPig); !got.Equal(dsv("100")) {
		t.Errorf("★ 缩放后 Σ 应**恰好等于猪重** 100, got %s", got)
	}
}

func TestScaleSplitToPigWeight_KeepsRelativeShares(t *testing.T) {
	perPig := kgMap("belly", "60", "rib", "70")
	scaleSplitToPigWeight(perPig, dsv("100"))
	// belly 占比 60/130 ⇒ 缩放后 100 × 60/130 = 46.1538...
	wantBelly := dsv("100").Mul(dsv("60")).Div(dsv("130"))
	if perPig["belly"].Sub(wantBelly).Abs().GreaterThan(dsv("0.0001")) {
		t.Errorf("★ 缩放只改总量,belly 的相对占比应不变: got %s want %s",
			perPig["belly"], wantBelly)
	}
}

func TestScaleSplitToPigWeight_NeverScalesUp(t *testing.T) {
	// Σ = 70 < 猪重 100 ⇒ 差额是**真实的切分损耗** ⇒ 原样不动。
	perPig := kgMap("belly", "30", "rib", "40")
	if scaleSplitToPigWeight(perPig, dsv("100")) {
		t.Error("★ 建议之和 70 < 猪重 100 ⇒ 不该往上撑(那等于凭空造货)")
	}
	if got := sumKg(perPig); !got.Equal(dsv("70")) {
		t.Errorf("★ 不该动: got Σ=%s", got)
	}
}

func TestScaleSplitToPigWeight_NoopOnEmptyOrZeroPig(t *testing.T) {
	perPig := kgMap("belly", "60")
	if scaleSplitToPigWeight(perPig, dsv("0")) {
		t.Error("★ 猪重为 0 ⇒ 不该缩放")
	}
	if !perPig["belly"].Equal(dsv("60")) {
		t.Errorf("★ 猪重为 0 时不该动: got %s", perPig["belly"])
	}
	empty := map[string]decimal.Decimal{}
	if scaleSplitToPigWeight(empty, dsv("100")) {
		t.Error("★ 空建议 ⇒ 不该缩放")
	}
}
