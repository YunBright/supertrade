package service

// value_coefficient_test.go —— **价值系数 / 产出估值**的判别力测试。
//
// ⚠️ 这些是**内部测试**（package service）而不是 package service_test：
//   因为 applyValueCoefficients 是**纯函数**且未导出，
//   而 export_test.go 是 CRLF 存量文件 ⇒ 不去动它（避免整文件行尾漂移）。

import (
	"testing"

	"github.com/shopspring/decimal"
)

// vcRow 造一个「有产出 + 有价」的部位行。
func vcRow(kg, price string) PerCutGrossMargin {
	k := dsv(kg)
	p := dsv(price)
	return PerCutGrossMargin{
		ActualSplitKg:          &k,
		ActualAvgUnitPriceYuan: &p,
	}
}

// vcRowNoPrice 造一个「有产出、但当天没卖过(取不到零售均价)」的部位行。
func vcRowNoPrice(kg string) PerCutGrossMargin {
	k := dsv(kg)
	return PerCutGrossMargin{ActualSplitKg: &k}
}

// ① 价值系数 / 产出估值的口径。
//
// 生产实测（店 00 2026-10-30 的三个部位）：肋排 / 五花 / 猪蹄。
func TestValueCoefficient_CoefficientsAndOutputValue(t *testing.T) {
	rows := []PerCutGrossMargin{
		vcRow("21.876", "45.7441945511062"),  // 肋排：营收 1000.70
		vcRow("27.6569", "15.9598508871204"), // 五花：营收 441.40
		vcRow("15.2143", "20.4150043051603"), // 猪蹄：营收 310.60
	}
	res := applyValueCoefficients(rows)

	if res.OutputValueYuan == nil || res.AvgUnitValueYuan == nil {
		t.Fatal("三个部位都有产出有价 ⇒ 产出估值与平均单位价值必须算得出来")
	}
	// 产出估值 = Σ(kg×price) = 1000.70 + 441.40 + 310.60
	if got := res.OutputValueYuan.Sub(decimal.RequireFromString("1752.70")).Abs(); got.GreaterThan(decimal.RequireFromString("0.01")) {
		t.Fatalf("产出估值应 = 1752.70, got %s（差 %s）", res.OutputValueYuan.String(), got.String())
	}
	// 平均单位价值 = 1752.70 ÷ 64.7472 = 27.0699
	if got := res.AvgUnitValueYuan.Sub(decimal.RequireFromString("27.0699")).Abs(); got.GreaterThan(decimal.RequireFromString("0.0001")) {
		t.Fatalf("平均单位价值应 = 27.0699, got %s（差 %s）", res.AvgUnitValueYuan.String(), got.String())
	}
	// 部位系数 = 该部位零售均价 ÷ 平均单位价值
	for i, want := range []string{"1.6899", "0.5896", "0.7542"} {
		if rows[i].ValueCoefficient == nil {
			t.Fatalf("第 %d 行必须有价值系数", i)
		}
		if got := rows[i].ValueCoefficient.Sub(decimal.RequireFromString(want)).Abs(); got.GreaterThan(decimal.RequireFromString("0.001")) {
			t.Fatalf("第 %d 行价值系数应 ≈ %s, got %s", i, want, rows[i].ValueCoefficient.String())
		}
	}
}

// ② ⚠️ owner 原话里的「加权价值产出」按字面算是**恒等式** ⇒ 锁住它，
//
//	免得以后又有人把它当成新指标端到页面上（它恒等于产出总重，不含新信息）。
func TestValueCoefficient_WeightedOutputIsIdentity(t *testing.T) {
	rows := []PerCutGrossMargin{
		vcRow("21.876", "45.7441945511062"),
		vcRow("27.6569", "15.9598508871204"),
		vcRow("15.2143", "20.4150043051603"),
	}
	applyValueCoefficients(rows)

	var sumKg, weighted decimal.Decimal
	for _, r := range rows {
		if r.ValueCoefficient == nil || r.ActualSplitKg == nil {
			t.Fatal("前置条件不满足")
		}
		sumKg = sumKg.Add(*r.ActualSplitKg)
		weighted = weighted.Add(r.ActualSplitKg.Mul(*r.ValueCoefficient))
	}
	if got := weighted.Sub(sumKg).Abs(); got.GreaterThan(decimal.RequireFromString("0.0001")) {
		t.Fatalf("Σ(产出kg × 价值系数) 必须恒等于产出总重（%s），got %s", sumKg.String(), weighted.String())
	}
}

// ③ 当天没卖过的部位（取不到零售均价）⇒ **不计入、不提示**（owner 拍板）。
func TestValueCoefficient_MissingPriceExcluded(t *testing.T) {
	rows := []PerCutGrossMargin{
		vcRow("21.876", "45.7441945511062"),
		vcRowNoPrice("0.65"), // 生产实测 店 00 10-30 猪肾：有产出、没卖过
	}
	res := applyValueCoefficients(rows)

	// 分母与产出估值都**不受**缺价部位影响
	if res.OutputValueYuan == nil {
		t.Fatal("有价部位存在 ⇒ 产出估值必须算得出来")
	}
	if got := res.OutputValueYuan.Sub(decimal.RequireFromString("1000.70")).Abs(); got.GreaterThan(decimal.RequireFromString("0.01")) {
		t.Fatalf("缺价部位不得进入产出估值, got %s", res.OutputValueYuan.String())
	}
	if got := res.AvgUnitValueYuan.Sub(decimal.RequireFromString("45.7441945511062")).Abs(); got.GreaterThan(decimal.RequireFromString("0.0001")) {
		t.Fatalf("缺价部位不得进入平均单位价值, got %s", res.AvgUnitValueYuan.String())
	}
	// 缺价部位本身**没有**价值系数（前端显示「—」）
	if rows[1].ValueCoefficient != nil {
		t.Fatalf("缺价部位不该有价值系数, got %s", rows[1].ValueCoefficient.String())
	}
}

// ④ 有零售均价、但当天没有产出 ⇒ 仍然给价值系数（它是部位的市场价值倍数），
//
//	但**不进**分母（分母只认「切出来的货」）。
func TestValueCoefficient_PriceWithoutOutput(t *testing.T) {
	rows := []PerCutGrossMargin{
		vcRow("21.876", "45.7441945511062"),
		vcRow("0", "15.9598508871204"),
	}
	res := applyValueCoefficients(rows)

	if res.OutputValueYuan == nil {
		t.Fatal("产出估值必须算得出来")
	}
	if got := res.OutputValueYuan.Sub(decimal.RequireFromString("1000.70")).Abs(); got.GreaterThan(decimal.RequireFromString("0.01")) {
		t.Fatalf("零产出的部位不得进入产出估值, got %s", res.OutputValueYuan.String())
	}
	if rows[1].ValueCoefficient == nil {
		t.Fatal("有零售均价 ⇒ 即使零产出也该给价值系数")
	}
	if got := rows[1].ValueCoefficient.Sub(decimal.RequireFromString("0.3489")).Abs(); got.GreaterThan(decimal.RequireFromString("0.001")) {
		t.Fatalf("零产出部位的价值系数应 ≈ 0.3489, got %s", rows[1].ValueCoefficient.String())
	}
}

// ⑤ 算不出来 ⇒ 两个值都是 nil（**不是 0**：0 会被门店误读成"当天没价值"）。
func TestValueCoefficient_NotComputable(t *testing.T) {
	cases := [][]PerCutGrossMargin{
		{vcRowNoPrice("0.65")},          // 只有缺价部位
		{vcRow("0", "45.7441945511062")}, // 只有零产出
		{},                               // 空
	}
	for i, rows := range cases {
		res := applyValueCoefficients(rows)
		if res.OutputValueYuan != nil || res.AvgUnitValueYuan != nil {
			t.Fatalf("第 %d 组：算不出来时两个值都必须是 nil, got %v / %v",
				i, res.OutputValueYuan, res.AvgUnitValueYuan)
		}
		for j := range rows {
			if rows[j].ValueCoefficient != nil {
				t.Fatalf("第 %d 组第 %d 行不该有价值系数", i, j)
			}
		}
	}
}
