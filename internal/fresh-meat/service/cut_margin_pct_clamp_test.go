package service_test

// 2026-10-31:毛利率**写入前的数值边界** —— `numeric(8,4)` 装不下就写库失败。
//
// # 现场(实测,不是推演)
//
//	切「只用基线」后,某部位营收 **30 元**、成本 **4000 元**
//	⇒ 毛利率 = (30 − 4000) ÷ 30 × 100 = **−13233.33%**
//	⇒ `cut_cost_daily.gross_margin_pct` 的列类型是 `numeric(8,4)`
//	(= 上限 9999.9999)⇒ PG 报 `numeric field overflow (SQLSTATE 22003)`
//	⇒ **整条成本链物化失败** ⇒ 门店看到 500。
//
// # 为什么要单独锁
//
//	「毛利率爆表」在端到端场景里很难稳定构造(要同时满足营收极小 + 成本极大)
//	⇒ 直接锁判据(clampMarginPct)才可靠。

import (
	"testing"

	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

// TestClampMarginPct_ClampsToColumnRange 锁「毛利率永远落在 numeric(8,4) 装得下的范围」。
func TestClampMarginPct_ClampsToColumnRange(t *testing.T) {
	// 实测爆表值。
	if got := service.ClampMarginPctForTest(-13233.333333333334); got > -9999.99 {
		t.Errorf("★ 超过下限的毛利率应被钳到 -9999.99,got %v", got)
	}
	// 超上限。
	if got := service.ClampMarginPctForTest(99999.99); got < 9999.99 {
		t.Errorf("★ 超过上限的毛利率应被钳到 9999.99,got %v", got)
	}
	// 正常值**不动**(钳制不能污染正常数据)。
	for _, v := range []float64{0, 21.53, -12.5, 100, 9999.99, -9999.99} {
		if got := service.ClampMarginPctForTest(v); got != v {
			t.Errorf("★ 正常范围内的毛利率不该被改(%v → %v)", v, got)
		}
	}
}

// TestClampMarginPct_AlwaysFitsColumn 锁「任何输入都装得下」。
//
// ⚠️ 这是判别力的核心:不是"某些值被钳",而是"**任何**值都不会让写库失败"。
func TestClampMarginPct_AlwaysFitsColumn(t *testing.T) {
	inputs := []float64{
		-1e9, -13233.333333333334, -9999.9999, -9999.99,
		0, 9999.99, 9999.9999, 13233.333333333334, 1e9,
	}
	for _, v := range inputs {
		got := service.ClampMarginPctForTest(v)
		if got > 9999.99 || got < -9999.99 {
			t.Errorf("★ 毛利率 %v 钳完仍超出 numeric(8,4) 范围: %v", v, got)
		}
	}
}
