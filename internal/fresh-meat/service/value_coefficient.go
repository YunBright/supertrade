package service

// internal/fresh-meat/service/value_coefficient.go —— **价值系数 / 产出估值**
// (2026-11-01 owner 拍板)。
//
// # owner 要的是什么
//
//	「价值系数 = 该部位的单位重量市场价值 ÷ 整扇猪分割后所有可售部位的平均单位重量价值」
//	「加权价值产出 = Σ(某部位实际产出重量 × 该部位价值系数)」
//
// # ⚠️ 一个必须知道的恒等式：owner 原话的第二项按字面算是**空指标**
//
//	因为「平均单位重量价值」本身就是**产出加权**的平均：
//
//	    平均单位价值 = Σ(kg_c × price_c) ÷ Σ(kg_c)
//
//	⇒ Σ(kg_c × 系数_c) = Σ(kg_c × price_c ÷ 平均单位价值)
//	                  = Σ(kg_c × price_c) ÷ 平均单位价值
//	                  = 平均单位价值 × Σ(kg_c) ÷ 平均单位价值
//	                  = **Σ(kg_c)**（= 产出总重）
//
//	⇒ 「加权价值产出」**恒等于产出总重**，不含任何新信息。
//	  生产实测 店 00：10-29 = 197.899 / 10-30 = 238.726 / 10-31 = 215.596 kg
//	    —— 与同三天的产出总重**完全一致**（三个数逐一相同）。
//
//	⇒ owner 2026-11-01 拍板：这一项改成 **产出估值（¥）**：
//
//	    产出估值 = Σ(kg_c × price_c)   （= 平均单位价值 × 产出总重）
//
//	    含义：**今天切出来的货，按零售价算值多少钱**（含还没卖掉的、损耗的、搭赠的）。
//	    与当日营收对照 ⇒ 差值就是「切出来但还没变成现金」的那部分价值。
//
// # 「价值系数」的三个输入
//
//	① 某部位的单位重量市场价值 = 该部位当天实际卖出均价（零售均价，含统收）
//	   —— 取不到（当天没卖过）⇒ 该部位**不参与**任何计算
//	     （owner 2026-11-01 拍板：缺价部位**不计入也不提示**）
//	② 该部位实际产出重量 = `actual_split_kg`
//	③ 全店平均单位价值 = Σ(kg × price) ÷ Σ(kg)
//
// # 为什么分母只用「有产出 且 有价」的部位
//
//	「整扇猪分割后所有可售部位」= **切出来的货** ⇒ 没切出来的部位不进分母；
//	没卖过的部位**取不到市场价值** ⇒ 也不进分母
//	（否则只能用成本价冒充价值 ⇒ 口径就脏了：成本 < 价值 ⇒ 系数被系统性压低）。
//
// # ⚠️ 一个必须写明的前提
//
//	产出重量 `actual_split_kg` 在多数日子是**倒推值**（derived）
//	⇒ 价值系数与产出估值都带着分割估算的误差：切得不准 ⇒ 这两个数会跟着偏。
//	（真实切分产出（台面恒等式解）目前只有**全店合计**、没有部位级
//	  ⇒ 部位级只能用 `actual_split_kg`。）

import (
	"github.com/shopspring/decimal"
)

// ValueCoefficientResult 价值系数计算的**全店级**结果。
type ValueCoefficientResult struct {
	// AvgUnitValueYuan 是**整扇猪分割后所有可售部位的平均单位重量价值**(元/kg)
	// = Σ(kg×price) ÷ Σ(kg) —— 也就是「价值系数」那个分母。
	//
	// ⚠️ nil = 算不出来(当天没有「有产出且有价」的部位)。
	AvgUnitValueYuan *decimal.Decimal
	// OutputValueYuan 是**产出估值**(元) = Σ(kg×price)。
	//
	// ⚠️ nil = 算不出来(同上)。
	OutputValueYuan *decimal.Decimal
}

// applyValueCoefficients 算部位级**价值系数**与全店级**产出估值**。
//
// # mutates rows
//
//	就地写回每行的 [PerCutGrossMargin.ValueCoefficient]（切片共享底层数组 ⇒ 调用方可见）。
//
// # 「算不出来」的两种情况（都返回两个 nil）
//
//	① 没有任何部位同时满足「有实际产出重量 且 有零售均价」⇒ 没有分母
//	② Σ(kg) = 0（当天没切出东西）⇒ 没有分母
//
// ⚠️ **有零售均价但没有产出重量的部位，仍然会拿到价值系数**
//   （价值系数是**部位的市场价值倍数**，与当天切了多少无关）；
//   但它**不进**分母（分母只认「切出来的货」）。
func applyValueCoefficients(rows []PerCutGrossMargin) ValueCoefficientResult {
	var res ValueCoefficientResult

	var sumValue, sumKg decimal.Decimal
	for i := range rows {
		kg := pricedOutputKgOf(&rows[i])
		if kg == nil {
			continue
		}
		sumValue = sumValue.Add(kg.Mul(*rows[i].ActualAvgUnitPriceYuan))
		sumKg = sumKg.Add(*kg)
	}
	if !sumKg.IsPositive() {
		return res
	}

	avg := sumValue.Div(sumKg)
	out := sumValue
	for i := range rows {
		if rows[i].ActualAvgUnitPriceYuan == nil || !rows[i].ActualAvgUnitPriceYuan.IsPositive() {
			continue
		}
		coef := rows[i].ActualAvgUnitPriceYuan.Div(avg)
		rows[i].ValueCoefficient = &coef
	}

	res.AvgUnitValueYuan = &avg
	res.OutputValueYuan = &out
	return res
}

// pricedOutputKgOf 该部位**参与价值系数计算**的产出重量；nil = 不参与。
//
// ⚠️ 必须**同时**满足：
//	① 当天卖过（有零售均价）⇒ 否则取不到「市场价值」
//	② 有实际产出重量 ⇒ 否则它不是「整扇猪分割后的货」
func pricedOutputKgOf(row *PerCutGrossMargin) *decimal.Decimal {
	if row.ActualAvgUnitPriceYuan == nil || !row.ActualAvgUnitPriceYuan.IsPositive() {
		return nil
	}
	if row.ActualSplitKg == nil || !row.ActualSplitKg.IsPositive() {
		return nil
	}
	return row.ActualSplitKg
}
