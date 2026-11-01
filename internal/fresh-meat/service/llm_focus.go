// Package service / llm_focus.go —— 「LLM 只补**缺销售数据**的部位」(2026-11-01 owner D8)。
//
// # 为什么要有"焦点部位"这个概念
//
//	旧口径:LLM 每次都产**整猪切割方案**(问 model.CarcassCutTypes 全集)。
//	⇒ ① 问了一堆**本店根本不卖**的部位(纯浪费 token + 诱发幻觉);
//	  ② 门店真正缺数据的那几个部位,**淹没在 30 多个部位里**。
//
//	新口径(owner 原话:「LLM 只补缺销售数据的部位(不再产整猪切割方案)」):
//	提示词**只列缺销售数据的那几个部位** ⇒ LLM 不再产整猪方案。
//
// # 「缺销售数据」怎么判定
//
//	**统计基线覆盖 (体重段 × 部位) ⇔ 该部位在该体重段有销售数据。**
//
//	理由:基线是从**样本库**拟合出来的,而样本库 2026-11-01 起
//	**只从销售数据采集**(见 forecast_estimates.go 的 [EstimateSourceSalesDerived])
//	⇒ 基线覆盖 = 有销售数据;基线没覆盖 = 缺销售数据。
//
//	⚠️ 这条等价关系**依赖 P3 的样本源改动** —— 若样本又退回"盘点倒推",
//	  "基线覆盖"就变成"盘过这个部位"而不是"卖过这个部位" ⇒ 判定口径会漂。
//
// # 为什么先减掉"本店不卖的部位"
//
//	本店在售部位 = branch_cut_mappings 里的 cut_type 集合。
//	问 LLM 要一个本店根本不卖的部位 ⇒ 纯浪费,且必然是幻觉。
package service

import (
	"context"
	"fmt"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
)

// focusCutsForPig 算「这头猪需要 LLM 补哪些部位」。
//
// # 口径
//
//	focus = (本店在售部位) − (该体重段统计基线已覆盖的部位)
//
//	三种退化情形都**退回"不限"(nil)**，让 LLM 保持旧行为：
//
//	| 情形 | focus | 为什么 |
//	|---|---|---|
//	| 本店还没登记任何部位映射 | nil | 门店什么都还没配 ⇒ 判定不出"缺哪些" |
//	| 基线**覆盖了**本店全部在售部位 | nil | 没有缺口 ⇒ 不该问 AI；但「只用 AI」模式仍要 |
//	| 「只用 AI」模式 | 本店在售部位**全部** | 门店明确不要基线 ⇒ AI 得给全量 |
//
// ⚠️ focus = nil ⇒ 提示词**不限制部位**（问全集）⇒ LLM 仍会产整猪方案。
//   这是**故意**的：上面三种情形下我们**不知道**该问哪几个部位，
//   猜一个集合 ⇒ 门店会拿到"缺了一堆部位"的建议（比全量更糟）。
//
// # 返回值
//
//	空切片（非 nil）表示"一个部位都不用问"；nil 表示"不限"。
func (s *Service) focusCutsForPig(ctx context.Context, branchID, band, mode string) ([]model.CutType, error) {
	// ① 本店在售部位。
	_, cutOf, err := s.mappedSkus(ctx, branchID)
	if err != nil {
		return nil, err
	}
	sell := map[model.CutType]bool{}
	for _, c := range cutOf {
		sell[c] = true
	}
	if len(sell) == 0 {
		return nil, nil // 本店还没配部位 ⇒ 判定不出来 ⇒ 不限
	}

	// ② 「只用 AI」模式 ⇒ 本店在售部位**全部**要 AI（不看基线）。
	if mode == model.AdviceModeLLM {
		return sortedCutTypeSet(sell), nil
	}

	// ③ 基线已覆盖的部位（该体重段）。
	baselines, err := s.loadForecastBaselines(ctx, branchID)
	if err != nil {
		return nil, err
	}
	covered := map[model.CutType]bool{}
	for _, b := range baselines {
		if b.WeightBand != band || !b.Usable() || !b.P50Kg.IsPositive() {
			continue
		}
		covered[b.CutType] = true
	}

	// ④ 缺口 = 在售 − 已覆盖。
	focus := map[model.CutType]bool{}
	for c := range sell {
		if !covered[c] {
			focus[c] = true
		}
	}
	if len(focus) == 0 {
		return nil, nil // 没有缺口 ⇒ 不限（保持旧行为，别猜）
	}
	return sortedCutTypeSet(focus), nil
}

// sortedCutTypeSet 把部位集合按 wire 值排好序（提示词稳定 ⇒ 便于比对）。
func sortedCutTypeSet(m map[model.CutType]bool) []model.CutType {
	out := make([]model.CutType, 0, len(m))
	for c := range m {
		out = append(out, c)
	}
	for i := 1; i < len(out); i++ {
		j := i
		for j > 0 && string(out[j-1]) > string(out[j]) {
			out[j-1], out[j] = out[j], out[j-1]
			j--
		}
	}
	return out
}

// focusCutsLabel 把焦点部位渲染成给人看的清单（进提示词）。
func focusCutsLabel(focus []model.CutType) string {
	out := ""
	for i, c := range focus {
		if i > 0 {
			out += "、"
		}
		out += fmt.Sprintf("%s(%s)", c, model.CutLabel(c))
	}
	return out
}
