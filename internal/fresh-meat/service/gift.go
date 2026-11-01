// Package service / gift.go —— **搭赠**(买肉送猪肝这类「有成本、无收入」的出库)。
//
// # 为什么需要它(2026-10-31 owner)
//
//	门店把猪肝当**搭赠**送给顾客 —— **没过称、也没过账**
//	⇒ 销售流水里没有它、报损里也没有它
//	⇒ 链上看就是「账面有、实盘没有」⇒ 被当成**盘亏**
//	⇒ 更糟的是它会落进**短卖** ⇒ 白白多出一块成本(货其实是自己送出去的)。
//
//	生产实测(店 00 2026-10-30 猪肝):
//		10-29 产出 3.030kg(单价 13.1689)⇒ 10-30 盘点实盘 0kg
//		⇒ 销售成本被算成 39.90 元(但猪肝营收 0 ⇒ 报表上像"卖亏了")
//		⇒ 且这 3.03kg 进了当天的短卖。
//
// # 口径(owner 2026-10-31 拍板)
//
//	① **搭赠是链上的第四个出口**(销售 / 统收 / 报损之外)
//	   ⇒ 从可售量里扣掉 + **单独一条成本线**(搭赠成本)
//	② **扣毛利、单列一行**(不是混进销售成本)
//	③ 重量**没过称** ⇒ 用「**倒推建议值 + 门店确认**」(见下)
//
// # 倒推建议值是怎么来的
//
//	缺口 = 账面应有(全店) − 实盘(柜台实盘 + 冷库量)
//	     = **搭赠 + 真盘亏**   ← 两者混在一起 ⇒ 必须门店确认
//
//	⇒ 系统给建议值,门店点确认:
//	  - 确认是搭赠 ⇒ 记一条 waste_logs(waste_type='gift')⇒ **缺口归零、不再提示**;
//	  - 确实是盘亏   ⇒ 不记录 ⇒ 缺口留在账上(走原有的盘亏/短卖处理)。
//
//	⚠️ **不用过称**:缺口是恒等式的余项,盘完就知道差多少。
package service

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
)

// GiftSuggestion 一条**疑似搭赠**的部位级建议。
//
// ⚠️ 它是**建议**,不是事实 —— 缺口也可能是真盘亏(偷、损、盘错)
// ⇒ 前端必须让门店**确认**,不能直接当成搭赠。
type GiftSuggestion struct {
	CutType  model.CutType `json:"cut_type"`
	CutLabel string        `json:"cut_label"`
	// CubeProductID 是该部位的**主货号** —— 门店确认时直接带上它
	// (门店不认货号 ⇒ 服务端替门店补,见 RecordWasteLog)。
	CubeProductID string `json:"cube_product_id"`
	// ExpectedKg 是**账面应有**期末量(kg,全店口径)。
	ExpectedKg decimal.Decimal `json:"expected_kg"`
	// ActualKg 是**实盘**量(kg,柜台实盘 + 冷库量)。
	ActualKg decimal.Decimal `json:"actual_kg"`
	// GapKg 是缺口(kg)= 账面应有 − 实盘 = 搭赠 + 真盘亏。
	GapKg decimal.Decimal `json:"gap_kg"`
	// RecordedGiftKg 是**已经确认过**的搭赠量(kg)。
	//
	// ⚠️ 它**已经**从账面应有里减掉了 ⇒ 缺口本身就只剩"还没解释的那部分"。
	RecordedGiftKg decimal.Decimal `json:"recorded_gift_kg"`
	// UnitCostYuan 是当日该部位的**移动加权单价**(元/kg)—— 估成本用。
	UnitCostYuan decimal.Decimal `json:"unit_cost_yuan"`
	// SuggestCostYuan 是按建议量估的**搭赠成本**(元)。
	SuggestCostYuan decimal.Decimal `json:"suggest_cost_yuan"`
	// DataSource:actual | partial | estimated —— 当天盘点的可信度。
	//
	// ⚠️ estimated(当天没盘点单)⇒ **不会**产生建议(没盘过就无从比较)。
	DataSource string `json:"data_source"`
}

// GiftSuggestionList 搭赠建议列表的出参。
//
// ⚠️ 为什么用**对象包裹**而不是裸数组:
//
//	仓里其它列表端点(`GET /forecast-samples` 等)都是对象
//	⇒ 前端统一走 `_expectMap` ⇒ 裸数组会让前端多写一套解析分支。
type GiftSuggestionList struct {
	BranchID string `json:"branch_id"`
	// Date 是这份建议对应的营业日(YYYY-MM-DD)。
	Date string `json:"date"`
	// Count 是建议条数(== len(Suggestions))。
	Count int `json:"count"`
	// Suggestions 按缺口从大到小排。
	Suggestions []GiftSuggestion `json:"suggestions"`
}

// ListGiftSuggestions 列出某店某营业日**疑似搭赠**的部位(按缺口从大到小)。
//
// day 取其 bizTZ 下的日期作为营业日(与毛利报表同一口径)。
//
// # 判据
//
//	① 该部位**被盘过**(盘点单里出现过)⇒ 否则不比较;
//	② 缺口 = 账面应有 − 实盘 > 0 ⇒ 否则没有可建议的;
//	③ 缺口里**已经扣掉**了已确认的搭赠 ⇒ 确认过的不再提示。
func (s *Service) ListGiftSuggestions(ctx context.Context, branchID string, day time.Time) (*GiftSuggestionList, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	dateKey := day.In(s.bizTZOrUTC()).Format("2006-01-02")
	out := &GiftSuggestionList{BranchID: branchID, Date: dateKey, Suggestions: []GiftSuggestion{}}
	// ⚠️ 走**同一条链**(与毛利报表一致)⇒ 建议值和报表里的数字永远对得上。
	chainDays, err := s.computeCutChainDays(ctx, branchID, day, day)
	if err != nil {
		return nil, err
	}
	var rows []CutDailyCost
	for _, cd := range chainDays {
		if cd.dateKey != dateKey {
			continue
		}
		rows = cd.cutRows
	}
	if len(rows) == 0 {
		return out, nil
	}
	// 盘点口径三态(与毛利报表同一个来源)。
	startOfDay, endOfDay := s.businessDayBounds(day)
	_, dataSource, err := s.loadStocktakeFlags(ctx, branchID, startOfDay, endOfDay)
	if err != nil {
		return nil, err
	}

	// 部位主货号(门店不认货号 ⇒ 服务端替它补上)。
	primary, err := s.primarySkuPerCut(ctx, branchID)
	if err != nil {
		return nil, err
	}

	for _, r := range rows {
		if !r.GiftGapKg.IsPositive() {
			continue
		}
		s := GiftSuggestion{
			CutType:       r.CutType,
			CutLabel:      model.CutLabel(r.CutType),
			CubeProductID: primary[r.CutType],
			// ⚠️ ExpectedKg = 引擎当前的**账面应有**(已减掉已确认的搭赠)
			//	⇒ ExpectedKg − ActualKg == GapKg(**自洽**,门店能自己验算)。
			ExpectedKg:      r.ExpectedClosingKg,
			ActualKg:        r.GiftGapActualKg,
			GapKg:           r.GiftGapKg,
			RecordedGiftKg:  r.GiftKg,
			UnitCostYuan:    r.MovingAvgUnitCost,
			SuggestCostYuan: r.GiftGapKg.Mul(r.MovingAvgUnitCost),
			DataSource:      dataSource,
		}
		out.Suggestions = append(out.Suggestions, s)
	}
	// 缺口大的排前面(门店先处理影响最大的)。
	sort.Slice(out.Suggestions, func(i, j int) bool {
		if !out.Suggestions[i].GapKg.Equal(out.Suggestions[j].GapKg) {
			return out.Suggestions[i].GapKg.GreaterThan(out.Suggestions[j].GapKg)
		}
		return out.Suggestions[i].CutType < out.Suggestions[j].CutType
	})
	out.Count = len(out.Suggestions)
	return out, nil
}
