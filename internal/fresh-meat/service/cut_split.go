// Package service / cut_split.go —— **部位级 ⇄ SKU 级**的拆分(2026-10-27)。
//
// # 为什么还需要拆分
//
// 成本链改**部位级**之后,有两处仍然需要"把部位的量摊到货号上":
//
//	① **审计的余额校准**:位置表(sku_stock_positions)是 **SKU 级**的,
//	   而"按营业日推导的结余"是**部位级**的 ⇒ 校准前要把部位结余拆到货号。
//	② **预售结转方案**:预期售价是**货号级**的(门店按货号填),
//	   而结余 kg 与结余成本是**部位级**的 ⇒ 结转行也要按货号拆。
//
// # 拆分的口径
//
//	按该部位各货号**当前冷库余额**占比拆
//	⇒ 各货号份额之和 = 部位结余(**部位总额精确**)
//	⇒ 该部位所有货号当前冷库余额都是 0(还没进冷库)⇒ 全给该部位的**主 SKU**
//	  (确定性,不猜)。
//
// # ⚠️ 为什么不能沿用旧的 `splitCutClosing`(2026-10-27 已删)
//
//	旧实现按 `primarySkuPerCut` 拆,而那张表**每个部位只有一个货号**
//	⇒ 整个部位的结余都落在「主 SKU」上
//	⇒ 同部位其它货号的账面恒为 0 ⇒ 卖出即零成本 ⇒ 毛利率 100%
//	(= 本次改造要修的那个坑的另一半)。
//
// ⇒ 现在按**该部位的全部货号**拆。
package service

import (
	"context"
	"sort"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
)

// skusPerCut 取该店每个部位下的**全部**货号(部位 → 货号列表,按货号升序)。
//
// ⚠️ 与 `primarySkuPerCut` 的区别:那张表**每个部位只有一个货号**(主 SKU),
//   这张表给的是**全部**(多货号部位要用)。
func (s *Service) skusPerCut(ctx context.Context, branchID string) (map[model.CutType][]string, error) {
	cutOf, _, err := s.loadSkuCatalog(ctx, branchID, nil)
	if err != nil {
		return nil, err
	}
	out := map[model.CutType][]string{}
	for sku, cut := range cutOf {
		if cut == "" || sku == "" {
			continue
		}
		out[cut] = append(out[cut], sku)
	}
	for cut := range out {
		sort.Strings(out[cut])
	}
	return out, nil
}

// splitCutClosingToSkus 把**部位级**的日终结余拆成 **SKU 级**(部位 → SKU → kg)。
//
// # 口径
//
//	按该部位各货号**当前冷库余额**占比拆 ⇒ 份额之和 = 部位结余(**总额精确**)
//	⇒ 拆不动(该部位没有货号 / 当前冷库余额全为 0)⇒ 全给该部位的**主 SKU**。
//
// # 用途
//
//	① 审计的余额校准(位置表是 SKU 级的)
//	② 预售结转方案(结余 kg 与结余成本按货号拆给各货号的预期售价)
func (s *Service) splitCutClosingToSkus(ctx context.Context, branchID string,
	cutClosing map[string]decimal.Decimal) map[string]decimal.Decimal {
	out := map[string]decimal.Decimal{}
	if len(cutClosing) == 0 {
		return out
	}
	cutToSku, err := s.primarySkuPerCut(ctx, branchID)
	if err != nil {
		return out
	}
	skusOfCut, err := s.skusPerCut(ctx, branchID)
	if err != nil {
		return out
	}
	_, liveCold, err := s.loadSkuPositions(ctx, branchID)
	if err != nil {
		return out
	}
	for cut, v := range cutClosing {
		c := model.CutType(cut)
		primary := cutToSku[c]
		if primary == "" {
			// 该部位没配货号 ⇒ 无处安放(不猜、不静默写错)。
			continue
		}
		skus := skusOfCut[c]
		if len(skus) <= 1 {
			out[primary] = out[primary].Add(v)
			continue
		}
		var total decimal.Decimal
		for _, sku := range skus {
			total = total.Add(liveCold[sku])
		}
		if !total.IsPositive() {
			out[primary] = out[primary].Add(v)
			continue
		}
		for _, sku := range skus {
			out[sku] = out[sku].Add(v.Mul(liveCold[sku]).Div(total))
		}
	}
	return out
}
