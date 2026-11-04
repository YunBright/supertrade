// Package service / long_term_margin.go —— **长期毛利(全店口径)** + 冷库量。
//
// # 为什么要有这一页(2026-11-01 owner 拍板)
//
//	owner:「冷库余额链是存在的,不能退,但它在长期毛利里」
//	owner:「当日『总剩余』**不含冷库**」⇒ 冷库量**只在长期毛利页单独展示**
//	owner:「**专门建一个**」⇒ 不复用现有的区间/期间毛利页(那个是账面口径,P1 会退休)
//
// # 为什么单日毛利必须配一个长期口径
//
//	单日口径**故意不含冷库**(期初台面 = 0:夜里柜台清空、货回冷库)
//	⇒ 后果:今天卖昨天结转的货时,那笔成本昨天已扣 ⇒ **单日毛利偏高**,跨期才平。
//	owner 知情选定:「十分准确的销售成本基本算不出来」。
//
// ⇒ 这一页就是把**冷库**接回来,让期间数自洽:
//
//	期间毛利 = 期间营收 − 期间采购 + (期末冷库量 − 期初冷库量) × 期间采购均价
//
// # 每日行用哪个口径(2026-11-04 owner:「长期毛利页的也切」)
//
//	每日行 = **当日口径**(营收 − 当日采购 − 搬出估算成本 + 期末台面剩余×采购均价)
//	⇒ 与「每日毛利看板」的**表头**同一个数(**同一个公式**:day_caliber_margin.go)
//	⇒ ⚠️ 与看板的**部位参考毛利**(各部位参考毛利之和)**不是同一个数**。
//
// # 冷库量为什么能代表"全店结余"
//
//	冷库链(deriveColdChain)的口径是:
//		冷库_end = 冷库_start + **猪肉台日终实盘** − 搬上台 − 统收 − 搭赠
//	⇒ **日终冷库余额里已经含了当天夜里从柜台搬回去的那部分**
//	⇒ 所以"全店结余" = 冷库量(柜台那份已经进去了),**不能再加一次台面实盘**。
//
// # 冷库量是"账面推算值",不是实盘
//
//	冷库**不在每日盘点范围内** ⇒ 它的准确性**完全依赖门店登记搬台**
//	⇒ 报表必须标出来(前端原样显示),否则店长会把它当成盘出来的数。
//
// # 与每日毛利看板的关系
//
//	每日行的毛利 = **当日口径**(与每日毛利看板的**表头**同一个数)
//	             ⇒ ⚠️ 与看板的**部位参考毛利**(各部位之和)不是同一个数
//	期间毛利     = **全店口径**(含冷库变动)
//	⇒ 两者**不会完全相等**,差额 ≈ 冷库变动 × 采购均价 ⇒ note 里写清楚。
package service

import (
	"context"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// LongTermMarginDay 长期毛利里的**一天**。
type LongTermMarginDay struct {
	// Date 是营业日(YYYY-MM-DD,bizTZ)。
	Date string `json:"date"`
	// RevenueYuan 是当天营收(零售 + 统收)。
	RevenueYuan decimal.Decimal `json:"revenue_yuan"`
	// PurchasedKg / PurchasedYuan 是当天进店的**进货合计**(进猪 + **外采**)。
	//
	// ⚠️ owner 2026-11-03 拍板:「长期毛利的期间采购**要含外采**」
	//   ⇒ 外采的货也是真金白银进的货 ⇒ 必须进采购,否则毛利虚高。
	//
	// ⚠️ 外采的**明细**单列(ExternalKg / ExternalYuan)⇒ 门店看得见"其中外采多少"。
	PurchasedKg   decimal.Decimal `json:"purchased_kg"`
	PurchasedYuan decimal.Decimal `json:"purchased_yuan"`
	// ExternalKg / ExternalYuan 是当天**外采**的合计(kg / 元)—— 从 Purchased* 里单列出来。
	ExternalKg   decimal.Decimal `json:"external_kg"`
	ExternalYuan decimal.Decimal `json:"external_yuan"`
	// PurchaseAvgYuan 是当天**采购均价**(元/kg)= 采购金额 ÷ 采购重量。
	//
	// ⚠️ 它是当天**剩余成本**的估值价(owner:「可以用采购均价计算」)。
	// ⚠️ owner 2026-11-03:它是**含外采**的均价(总金额 ÷ 总重量)—— 即「求均值」。
	PurchaseAvgYuan decimal.Decimal `json:"purchase_avg_yuan"`
	// CounterStockKg 是当天**猪肉台日终实盘**(kg)—— 不含冷库。
	CounterStockKg decimal.Decimal `json:"counter_stock_kg"`
	// ColdStoreKg 是当天**日终冷库量**(kg)—— 账面推算值。
	ColdStoreKg decimal.Decimal `json:"cold_store_kg"`
	// HasStocktake = false ⇒ 当天**没有完整日终盘点** ⇒ 当天的毛利算不出来。
	HasStocktake bool `json:"has_stocktake"`
	// HasPurchase = false ⇒ 当天**没有进货** ⇒ 当天的毛利也算不出来。
	//
	// ⚠️ 2026-11-01 补:没有进货 ⇒ 没有采购均价 ⇒ 期末剩余的成本估不出来;
	//   而公式会退化成 m = (营收 − 0 + 0)/营收 = **100%** ⇒ 门店会以为赚疯了。
	//   ⇒ 这种日子只能显示「—」(算不出来)。
	HasPurchase bool `json:"has_purchase"`
	// GrossMarginYuan 是当天**毛利**(元)—— **当日口径**。
	//
	// ⚠️ 2026-11-04 owner:「长期毛利页的也切」⇒ 与每日毛利看板的**表头**同一个数
	//   (同一个公式:day_caliber_margin.go 的 dayCaliberMargin)。
	//
	// ⚠️ **算不出来时给 0**是错的(会被误读成"当天一点货都没剩、还亏了本")
	//   ⇒ 用 HasStocktake / HasPurchase(以及"当天没营收")表达"算不出来",
	//     前端显示「—」。
	GrossMarginYuan decimal.Decimal `json:"gross_margin_yuan"`
	// GrossMarginPct 是当天毛利率(%)—— 与 GrossMarginYuan 同一口径。
	GrossMarginPct float64 `json:"gross_margin_pct"`
	// YieldRate 是**出成率** = 真实切分产出 ÷ 当天进货毛重。
	//
	// ⚠️ **> 100% ⇒ 有问题,要去查**(owner 2026-11-01)。
	// nil = 算不出来(没有完整日终盘点 / 当天没进猪)⇒ 前端显示「—」。
	YieldRate *float64 `json:"yield_rate,omitempty"`
}

// LongTermMarginTotal 期间的汇总。
type LongTermMarginTotal struct {
	// RevenueYuan 是期间营收(零售 + 统收)。
	RevenueYuan decimal.Decimal `json:"revenue_yuan"`
	// PurchasedKg / PurchasedYuan 是期间总采(**含外采**,owner 2026-11-03)。
	PurchasedKg   decimal.Decimal `json:"purchased_kg"`
	PurchasedYuan decimal.Decimal `json:"purchased_yuan"`
	// ExternalKg / ExternalYuan 是期间**外采**的合计(kg / 元)—— 从 Purchased* 里单列出来。
	ExternalKg   decimal.Decimal `json:"external_kg"`
	ExternalYuan decimal.Decimal `json:"external_yuan"`
	// PurchaseAvgYuan 是**期间采购均价**(元/kg)= 期间采购金额 ÷ 期间采购重量。
	//
	// ⚠️ 期间库存一律按**这一个**均价估值 ⇒ 每日行能加总回期间数(口径自洽)。
	PurchaseAvgYuan decimal.Decimal `json:"purchase_avg_yuan"`
	// OpeningColdStoreKg / ClosingColdStoreKg 是**期初 / 期末冷库量**(kg)。
	OpeningColdStoreKg decimal.Decimal `json:"opening_cold_store_kg"`
	ClosingColdStoreKg decimal.Decimal `json:"closing_cold_store_kg"`
	// ColdStoreDeltaKg = 期末 − 期初(kg)。正数 = 冷库变重(囤货了)。
	ColdStoreDeltaKg decimal.Decimal `json:"cold_store_delta_kg"`
	// ColdStoreValueDeltaYuan 是**冷库变动的计价**(元)= 冷库变动 kg × 期间采购均价。
	//
	// ⚠️ 这是**桥接项**:期间毛利 = 期间营收 − 期间采购 + 这一项 ⇒ 店长能自己验算。
	ColdStoreValueDeltaYuan decimal.Decimal `json:"cold_store_value_delta_yuan"`
	// OpeningStockKg / ClosingStockKg 是**期初 / 期末全店结余**(kg)
	// —— 冷库量(已含夜里搬回柜台的那部分)。
	OpeningStockKg decimal.Decimal `json:"opening_stock_kg"`
	ClosingStockKg decimal.Decimal `json:"closing_stock_kg"`
	// GrossMarginYuan 是**期间毛利**(元)—— 全店口径:
	//	期间营收 − 期间采购 + (期末冷库 − 期初冷库) × 期间采购均价。
	GrossMarginYuan decimal.Decimal `json:"gross_margin_yuan"`
	// GrossMarginPct 是期间毛利率(%)。
	GrossMarginPct float64 `json:"gross_margin_pct"`
	// DailySumGrossMarginYuan 是**每日毛利之和**(当日口径)。
	//
	// ⚠️ 它**不等于** GrossMarginYuan:期间是**全店口径**(含冷库变动),
	//   每日是**当日口径**(不含冷库结转)⇒ 差额来自冷库结转、搬出估算成本
	//   与单日/期间采购均价的差异 ⇒ 两个数都给,让店长能自己看出差在哪。
	DailySumGrossMarginYuan decimal.Decimal `json:"daily_sum_gross_margin_yuan"`
	// DaysWithStocktake / DaysWithoutStocktake 是有/没有完整日终盘点的天数。
	//
	// ⚠️ 没有完整盘点 ⇒ 那天的毛利算不出来 ⇒ 期间数也**不完整**。
	DaysWithStocktake    int `json:"days_with_stocktake"`
	DaysWithoutStocktake int `json:"days_without_stocktake"`
	// CostIncomplete = true ⇒ 区间里有天的毛利**算不出来** ⇒ 期间毛利被低估。
	CostIncomplete bool `json:"cost_incomplete,omitempty"`
	// CostIncompleteNote 是给门店看的**人话**说明(前端原样显示)。
	CostIncompleteNote string `json:"cost_incomplete_note,omitempty"`
}

// LongTermMarginReport 长期毛利报告。
type LongTermMarginReport struct {
	BranchID string `json:"branch_id"`
	FromDate string `json:"from_date"`
	ToDate   string `json:"to_date"`
	// Days 是区间天数(含首尾)。
	Days  int                 `json:"days"`
	Total LongTermMarginTotal `json:"total"`
	Rows  []LongTermMarginDay `json:"rows"`
	// Note 说明口径。
	Note string `json:"note,omitempty"`
	// ColdStoreNote 说明**冷库量是账面推算值**(不是实盘)。
	ColdStoreNote string `json:"cold_store_note,omitempty"`
}

// ComputeLongTermMargin 算某店某期间的**长期毛利**(全店口径)+ 冷库量。
//
// # 为什么"专门建一个"(与已下线的账面期间毛利的区别)
//
//	账面期间毛利(`/gross-margin/period`,移动加权链口径)⇒ 依赖分割重量
//	  ⇒ 已于 2026-11-01 owner 拍板随账面链一起退休;
//	长期毛利(`/long-term-margin`) = **全店口径**(营收/采购/冷库)⇒ 零分割假设 ⇒ 长期有效。
func (s *Service) ComputeLongTermMargin(ctx context.Context, branchID, from, to string) (*LongTermMarginReport, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	loc := s.bizTZOrUTC()
	fromT, err := time.ParseInLocation("2006-01-02", from, loc)
	if err != nil {
		return nil, fmt.Errorf("%w: from 格式应为 YYYY-MM-DD", ErrInvalidInput)
	}
	toT, err := time.ParseInLocation("2006-01-02", to, loc)
	if err != nil {
		return nil, fmt.Errorf("%w: to 格式应为 YYYY-MM-DD", ErrInvalidInput)
	}
	if toT.Before(fromT) {
		return nil, fmt.Errorf("%w: to 不能早于 from", ErrInvalidInput)
	}

	// 逐日的**事实**(营收/销量/盘点/搬台/报损/搭赠)由链式计算产出。
	//
	// ⚠️ 这里借的是链的"事实"部分(销售与库存流水),不是它的"账面成本"部分
	//   ⇒ 账面成本(P1 退休)不影响这一页。
	chainDays, err := s.computeCutChainDays(ctx, branchID, fromT, toT)
	if err != nil {
		return nil, err
	}
	byDate := make(map[string][]CutDailyCost, len(chainDays))
	for _, cd := range chainDays {
		byDate[cd.dateKey] = cd.cutRows
	}

	// 冷库余额链(实时推算,不需要重物化)。
	coldChain, err := s.deriveColdChain(ctx, branchID, "", nil)
	if err != nil {
		return nil, err
	}

	rows := make([]LongTermMarginDay, 0, 8)
	for d := fromT; !d.After(toT); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		cutRows := byDate[key]

		var revenue decimal.Decimal
		for _, r := range cutRows {
			revenue = revenue.Add(r.RevenueYuan)
		}
		// 当日总采 + 期末猪肉台实盘(判据 = 当天有没有**完整**盘点单)。
		ref, err := s.referenceDayTotalsOf(ctx, branchID, d, cutRows)
		if err != nil {
			return nil, fmt.Errorf("query day totals(%s): %w", key, err)
		}
		day := LongTermMarginDay{
			Date:        key,
			RevenueYuan: revenue,
			// ⚠️ owner 2026-11-03:「期间采购要含外采」⇒ 这里用**含外采**的合计与均价
			//   (= 进猪 + 外采;均价 = 总金额 ÷ 总重量,即「求均值」)。
			PurchasedKg:     ref.TotalPurchasedKg(),
			PurchasedYuan:   ref.TotalPurchasedYuan(),
			PurchaseAvgYuan: ref.TotalPurchaseAvgYuan(),
			ExternalKg:      ref.ExternalKg,
			ExternalYuan:    ref.ExternalYuan,
			CounterStockKg:  ref.RemainKg,
			ColdStoreKg:     sumCutKg(coldChain[key]),
			HasStocktake:    ref.HasRemain,
			HasPurchase:     ref.TotalPurchasedKg().IsPositive(),
		}
		// ⚠️ 没有完整盘点 **或** 没有进货 **或** 当天没营收 ⇒ 当天毛利**算不出来**(不给 0)。
		//   详见 day_caliber_margin.go 的三个守卫。
		//
		// ⚠️⚠️ **这里的每日毛利 = 当日口径**(2026-11-04 owner:「长期毛利页的也切」)
		//
		//	⇒ 与「每日毛利看板」的**表头**同一个数(**同一个公式**:dayCaliberMargin)
		//	⇒ 原来这里是「猪肉台口径」(营收 − 采购 + 台面剩余×采购均价),
		//	  与看板表头**只差「搬出估算成本」一项**
		//	  (生产实测 店 00:10-29 差 1.58 元、11-01 差 114.84 元)
		//	⇒ 这一页的**期间数**仍然是**全店口径**(含冷库变动)—— 页面自洽的那一条不变
		//	  ⇒ 每日之和与期间毛利**不相等**,差额在 note 里写清楚。
		loadCost := decimal.Zero
		for _, r := range cutRows {
			loadCost = loadCost.Add(r.CounterLoadKg.Mul(r.MovingAvgUnitCost))
		}
		if gm, pct, ok := dayCaliberMargin(ref, revenue, loadCost); ok {
			day.GrossMarginYuan = gm
			day.GrossMarginPct = pct
		}
		// ⚠️ 没有完整盘点 ⇒ 剩余算不出来 ⇒ 毛利**算不出来**(不给 0)。
		// ⚠️ 2026-11-03 owner:出成率分子**减外采** ⇒ 与毛利看板同一口径。
		if rate, ok := referenceYieldRate(cutRows, ref.PurchasedKg, ref.ExternalKg); ok {
			f, _ := rate.Mul(decimal.NewFromInt(100)).Float64()
			day.YieldRate = &f
		}
		rows = append(rows, day)
	}

	// 期初 = from 的**前一天**;期末 = to 当天。
	openingCold := sumCutKg(chainValueAt(coldChain, fromT.AddDate(0, 0, -1).Format("2006-01-02")))
	closingCold := sumCutKg(chainValueAt(coldChain, toT.Format("2006-01-02")))

	return buildLongTermMarginReport(branchID, from, to, rows, openingCold, closingCold), nil
}

// buildLongTermMarginReport 把**每日事实**汇总成长期毛利报告(**纯计算**,不做 IO)。
//
// # 期间毛利(全店口径)
//
//	期间毛利 = 期间营收 − 期间采购 + (期末冷库量 − 期初冷库量) × 期间采购均价
//
// ⚠️ 库存一律按**期间**采购均价估值(一个价到底)⇒ 口径统一、可自洽。
//
// # 为什么与"每日之和"不相等(这是**口径差**,不是 bug)
//
//	每日行 = **当日口径**(与每日毛利看板的**表头**同一个数;不含冷库结转)
//	期间   = **全店口径**(含冷库变动)
//	⇒ 差额 = 冷库变动的计价 − 每日台面剩余的计价(再叠加搬出估算成本/统收/搭赠的影响)
//	⇒ 两个数都给 ⇒ 店长能自己核对差额从哪来。
func buildLongTermMarginReport(branchID, from, to string, rows []LongTermMarginDay,
	openingCold, closingCold decimal.Decimal) *LongTermMarginReport {
	var totRevenue, totPurchasedKg, totPurchasedYuan, dailySum decimal.Decimal
	var totExternalKg, totExternalYuan decimal.Decimal
	daysWithST, daysWithoutST := 0, 0
	lastPurchaseAvg := decimal.Zero
	for _, d := range rows {
		totRevenue = totRevenue.Add(d.RevenueYuan)
		totPurchasedKg = totPurchasedKg.Add(d.PurchasedKg)
		totPurchasedYuan = totPurchasedYuan.Add(d.PurchasedYuan)
		totExternalKg = totExternalKg.Add(d.ExternalKg)
		totExternalYuan = totExternalYuan.Add(d.ExternalYuan)
		if d.PurchaseAvgYuan.IsPositive() {
			lastPurchaseAvg = d.PurchaseAvgYuan
		}
		if d.HasStocktake {
			daysWithST++
			// ⚠️ **没有进货的那天不算进"每日之和"** —— 那天的毛利算不出来
			//   (公式会退化成 100%,见 cost_reference.go)。
			//   ⇒ 每日之和 < 期间毛利 时,差额里就含着这些天 ⇒ 店长能自己核对。
			if d.HasPurchase {
				dailySum = dailySum.Add(d.GrossMarginYuan)
			}
		} else {
			daysWithoutST++
		}
	}

	// 期间采购均价:优先用期间自己的;期间没进货时退回**区间内最后一次**有进货那天的均价。
	periodAvg := decimal.Zero
	if totPurchasedKg.IsPositive() {
		periodAvg = totPurchasedYuan.Div(totPurchasedKg)
	} else {
		periodAvg = lastPurchaseAvg
	}

	total := LongTermMarginTotal{
		RevenueYuan:             totRevenue,
		PurchasedKg:             totPurchasedKg,
		PurchasedYuan:           totPurchasedYuan,
		ExternalKg:              totExternalKg,
		ExternalYuan:            totExternalYuan,
		PurchaseAvgYuan:         periodAvg,
		OpeningColdStoreKg:      openingCold,
		ClosingColdStoreKg:      closingCold,
		ColdStoreDeltaKg:        closingCold.Sub(openingCold),
		OpeningStockKg:          openingCold,
		ClosingStockKg:          closingCold,
		DailySumGrossMarginYuan: dailySum,
		DaysWithStocktake:       daysWithST,
		DaysWithoutStocktake:    daysWithoutST,
	}
	// 冷库变动的计价(桥接项)= 冷库变动 kg × 期间采购均价。
	total.ColdStoreValueDeltaYuan = total.ColdStoreDeltaKg.Mul(periodAvg)
	// 期间毛利(全店口径)= 期间营收 − 期间采购 + 冷库变动的计价。
	total.GrossMarginYuan = totRevenue.Sub(totPurchasedYuan).Add(total.ColdStoreValueDeltaYuan)
	if totRevenue.IsPositive() {
		pct, _ := total.GrossMarginYuan.Mul(decimal.NewFromInt(100)).Div(totRevenue).Float64()
		total.GrossMarginPct = pct
	}
	if daysWithoutST > 0 {
		total.CostIncomplete = true
		total.CostIncompleteNote = fmt.Sprintf(
			"⚠ 本区间有 %d 天没有完整日终盘点 ⇒ 这 %d 天的剩余算不出来 ⇒ 期间毛利被低估",
			daysWithoutST, daysWithoutST)
	}

	// ⚠️ 2026-11-01 owner 口径切 B 之后,以及 2026-11-04 owner「长期毛利页的也切」之后:
	//
	//	每日行的毛利现在是**当日口径**(与每日毛利看板的**表头**同一个数),
	//	而看板的**部位参考毛利**是「各部位参考毛利之和」⇒ **两者仍不是同一个数**。
	//	⇒ 这里必须说清楚"哪个是哪个",否则门店对着两页对不上 ⇒ 以为页面坏了。
	note := "期间毛利 = 期间营收 − 期间采购 + (期末冷库量 − 期初冷库量) × 期间采购均价(全店口径);" +
		"期间采购**含外采**(进猪 + 外采),采购均价 = 总金额 ÷ 总重量;" +
		"库存按**期间采购均价**估值(不含毛利 ⇒ 不成环)。" +
		"每日行的毛利是**当日口径**(营收 − 当日采购(含外采) − 搬出估算成本 + 期末台面剩余×采购均价)" +
		"⇒ **与「每日毛利看板」表头的毛利是同一个数**;" +
		"⚠️ 但它与看板的**部位参考毛利**(各部位参考毛利之和)不是同一个数。" +
		"⚠️ 每日之和与期间毛利**不相等**:期间是**全店口径**(含冷库变动),每日是**当日口径**" +
		"(不含冷库结转)⇒ 差额来自冷库结转、搬出估算成本与单日/期间采购均价的差异;两个数都给了,可自行核对。"
	coldStoreNote := "冷库量是**账面推算值**(依赖门店登记搬台),不是实盘 —— 冷库不在每日盘点范围内。"
	if !periodAvg.IsPositive() {
		note += ";⚠ 期间**取不到采购均价**(区间内没有进货)⇒ 冷库变动未计价,期间毛利偏高。"
	}
	// ⚠️ 区间里有"每日毛利算不出来"的天 ⇒ 每日之和不含它们。
	//
	//	判据 = **没有完整盘点** 或 **没有进货**(见上面每日行的计算条件)⇒ 两个都要说。
	noPurchaseDays := 0
	noStocktakeDays := 0
	for _, d := range rows {
		if !d.HasPurchase {
			noPurchaseDays++
		}
		if !d.HasStocktake {
			noStocktakeDays++
		}
	}
	if noPurchaseDays > 0 {
		note += fmt.Sprintf(";⚠ 区间里有 %d 天**没有进货** ⇒ 那天的每日毛利算不出来(显示「—」)⇒ 每日之和不含它们。",
			noPurchaseDays)
	}
	if noStocktakeDays > 0 {
		note += fmt.Sprintf(";⚠ 区间里有 %d 天**没有完整日终盘点** ⇒ 台面剩余算不出来 ⇒ 那天的每日毛利也算不出来(显示「—」)。",
			noStocktakeDays)
	}

	return &LongTermMarginReport{
		BranchID:      branchID,
		FromDate:      from,
		ToDate:        to,
		Days:          len(rows),
		Total:         total,
		Rows:          rows,
		Note:          note,
		ColdStoreNote: coldStoreNote,
	}
}

// sumCutKg 把"部位 → kg"的映射加总成总 kg。
func sumCutKg(m map[string]decimal.Decimal) decimal.Decimal {
	var out decimal.Decimal
	for _, v := range m {
		out = out.Add(v)
	}
	return out
}
