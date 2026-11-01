// Package service / cut_chain.go —— **成本链的逐日接续**(2026-10-27)。
//
// # 为什么需要它(生产实测踩到的坑)
//
// 成本链的权威载体是 `cut_cost_daily`(上一营业日的部位结余)。
// 但那张表**只在审计结转时被固化** ⇒ 日常没有快照 ⇒
// 如果每天各自从"门店期初结转"起算,链就**每天断一次** ⇒
// 成本塌成"期初那点残值" ⇒ 毛利虚高。
//
// 生产实测(店 00,2026-10-27,区间毛利):
//
//	09-01 ~ 09-30  成本恒 0     ⇒ 期间毛利看起来 = 期间营收(100% 毛利)
//	10-01          成本 120.41  ⇒ 只剩"期初结转那 9.23kg"的残值
//	10-02          成本 3072.17 ⇒ 碰巧当天有整猪入库(3169.66)
//	10-03 ~ 10-27  成本 ~100    ⇒ 链又断回期初残值(毛利率 97%)
//
// ⇒ **链必须逐日接续**:今天的期初 = 昨天的期末(**算出来的,不必落库**)。
//
// # 链的起点与锚点
//
//	起点(start):链从**最早有基线**的营业日起算。
//	  基线 = ① 门店期初结转(sku_stock_openings,按部位)
//	        ② 已落库的成本日快照(cut_cost_daily)
//
//	  起点之前的天**没有基线** ⇒ 成本算不出来(≠"成本为 0")⇒ 显式标注
//	  (见 CutDailyCost.CostUncomputable 与期间毛利报告的 cost_incomplete 标记)。
//
//	锚点(anchor):**上一营业日(或更早)最近一个已落库快照**的日子。
//	  ⇒ 它的结余可以直接当今天的期初 ⇒ 不用回溯 ⇒ O(1) 快路径。
//	  ⚠️ 锚点必须**早于**要算的那天,否则接不上。
//
// # 物化(materializeCostChain)
//
// 链式回溯是 O(天) 的 ⇒ 每次查都回溯会慢。
// ⇒ 把链**逐日落库**(cut_cost_daily)⇒ 下次算任意一天,期初直接读前一日快照(O(1))。
// ⇒ 落库**不要求当天有盘点单** —— 盘点只是"实盘锚定",不是"链的开关"。
package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
)

// cutChainDay 链上一天的计算结果。
type cutChainDay struct {
	day time.Time
	// dateKey 是营业日(YYYY-MM-DD)。
	dateKey string
	cutRows []CutDailyCost
	skuRows []SkuDailyCost
	// recon / losses 是当天的成本对账行与损耗汇总(每日毛利报表要用)。
	recon  *ReconciliationRow
	losses *LossSummary
	// closing 是当天算出来的**部位期末**(kg + 结转口径金额)⇒ 喂给下一天当期初。
	closing *cutOpeningOverride
	// shortSellKg 是当天的**总量短卖**(kg)—— owner 口径(口径见 CutDailyCost.ShortSellKg)。
	//
	// ⚠️ 分部位的账面不足**不算**在这里(那是 CutShortSellKg)⇒
	//   >0 ⇒ ① 那部分货**必须计成本** ② 结转给下一天(累计短卖)。
	//
	// ⚠️ 2026-11-01 owner：**短卖拦截规则废止** ⇒ 不再拦结算、不再描红。
	shortSellKg decimal.Decimal
	// costIncomplete = true ⇒ 当天有部位的成本**算不出来**(不是成本为 0)。
	costIncomplete bool
	// costUncomputableAll = true ⇒ 当天**所有**部位的成本都算不出来
	// (既无期初库存也无进货、又没有可用单价)⇒ 这天的成本**完全算不出来**。
	//
	// ⚠️ 与 costIncomplete 的区别很重要:
	//   costIncomplete   ⇒ 有一部分算不出来(成本被低估)
	//   costUncomputableAll ⇒ 整天的成本都算不出来(那一行是纯营收)
	// ⇒ 报告级"多少天算不出成本"要用**后者**,否则每天只差 1 个部位也会被算成"整天缺基线"。
	costUncomputableAll bool
}

// cutChainRunner 逐日推进一条成本链(内存链)。
//
// ⚠️ **链是按部位独立接续的**:部位 A 的库存变动不影响部位 B。
//
//	⇒ 某个部位一旦"入链"(有过库存或进货),它的期末就逐日传给下一天;
//	  没入链的部位(既无期初结转也无进货)**不接续** ——
//	  否则"无基线期间卖出的货"会变成负库存,一路污染到有基线的日子。
type cutChainRunner struct {
	svc      *Service
	branchID string
	// inChain 是"已入链"的部位集合。
	inChain map[string]bool
	// prev 是上一天的期末(只含已入链的部位)。
	prev *cutOpeningOverride
}

func newCutChainRunner(svc *Service, branchID string) *cutChainRunner {
	return &cutChainRunner{
		svc:      svc,
		branchID: branchID,
		inChain:  map[string]bool{},
		// ⚠️ **链的起点也要有一个状态**(不是 nil):
		//   nil 会让引擎改从"上一营业日的快照"读累计短卖
		//   ⇒ 而链起点的上一期不属于这条链 ⇒ 累计值必须是 0。
		prev: &cutOpeningOverride{
			kg:     map[string]decimal.Decimal{},
			amount: map[string]decimal.Decimal{},
			unit:   map[string]decimal.Decimal{},
		},
	}
}

// day 算链上的一天,并把期末接给下一天。
func (r *cutChainRunner) day(ctx context.Context, d time.Time) (cutChainDay, error) {
	cutRows, skuRows, recon, losses, err := r.svc.computeCutDailyCostsChained(ctx, r.branchID, d, r.prev)
	if err != nil {
		return cutChainDay{}, err
	}
	out := cutChainDay{
		day:     d,
		dateKey: d.In(r.svc.bizTZOrUTC()).Format("2006-01-02"),
		cutRows: cutRows,
		skuRows: skuRows,
		recon:   recon,
		losses:  losses,
	}
	next := &cutOpeningOverride{
		kg:     map[string]decimal.Decimal{},
		amount: map[string]decimal.Decimal{},
		unit:   map[string]decimal.Decimal{},
	}
	// ★ 累计短卖(R4):今天之前结转过来的 + 今天的当日短卖。
	//
	//	⚠️ **它不是库存**:短卖**绝不回写成负库存** ⇒ 期初仍然是 ≥0
	//	  (期末量裁 0,见 cut_cost.go 的 R1)⇒ 链上带着的只有这个"提示数"。
	carriedShortSell := decimal.Zero
	if r.prev != nil {
		carriedShortSell = r.prev.shortSell
	}
	dayShortSell := decimal.Zero
	totalCuts := 0
	uncomputableCuts := 0
	for _, row := range cutRows {
		cut := string(row.CutType)
		totalCuts++
		dayShortSell = dayShortSell.Add(row.ShortSellKg)
		if row.CostUncomputable {
			out.costIncomplete = true
			uncomputableCuts++
		}
		if !r.inChain[cut] {
			// 该部位今天才第一次有货(期初或进货)⇒ 从今天起入链。
			// ⚠️ 判据用"期初 + 产出 > 0":只有卖出、没有库存的部位**不入链**
			//   (它的缺口是"没基线",不是"短卖",不该往后面传)。
			if !row.OpeningKg.Add(row.ProducedKg).IsPositive() {
				continue
			}
			r.inChain[cut] = true
		}
		// 结转口径的期末金额(成本口径 + 预售重估)—— 与 cut_cost_daily.ClosingAmount 同义。
		next.kg[cut] = row.ClosingKg
		next.amount[cut] = row.ClosingAmount.Add(row.PresaleMarginYuan)
		// ★ 上期的移动加权单价也要传下去:短卖时"可售数量 ≤ 0" ⇒ 单价算不出来
		//   ⇒ 没有它,卖超的部分会变成**零成本** ⇒ 毛利 100%(虚高)。
		if row.MovingAvgUnitCost.IsPositive() {
			next.unit[cut] = row.MovingAvgUnitCost
		}
	}
	next.shortSell = carriedShortSell.Add(dayShortSell)
	out.shortSellKg = dayShortSell
	r.prev = next
	// 当天**所有**部位都算不出来 ⇒ 这天的成本完全算不出来。
	out.costUncomputableAll = totalCuts > 0 && uncomputableCuts == totalCuts
	return out, nil
}

// cutChainBaseline 取该店**链的起点**(YYYY-MM-DD);空 = 没有基线。
//
// # ⚠️ 链的起点 = **期初结转的最早生效日**(门店口径,不要被更早的快照日拉低)
//
//	2026-10-30 生产实测(店 00)踩过的坑:
//	门店是 10-29 晚上才补录期初的(生效日 10-28 = 鲜肉子系统正式启用前一天),
//	而 10-01~10-27 的**上线前旧账**早就物化成了成本日快照。
//	⇒ 若这里取 min(期初生效日, 最早快照日) = 10-01,
//	  链就被拉回**上线前** ⇒ 那些天的旧账(账面短卖的负链值)被当成链内来算
//	  ⇒ 负数一路带进启用第一天(实测:猪耳 −1.279 / 猪肺 −17.64 / 猪手 −9.2 kg)
//	  ⇒ 门店看到"期初为什么有负数"。
//
//	⇒ **期初生效日就是链的起点**(见 loadCutOpening 的 baselineDay 分支:
//	   "生效日当天:快照/链式接续/实时结余链 一律让位给门店录的值")。
//	⇒ 只有**没有期初结转**的店才退回"最早快照日"(链从第一期营业日起算)。
func (s *Service) cutChainBaseline(ctx context.Context, branchID string) (string, error) {
	loc := s.bizTZOrUTC()
	best := ""
	// ① 门店期初结转(按部位)的最早生效日 —— **链的起点**。
	openings, err := s.loadOpenings(ctx, branchID)
	if err != nil {
		return "", err
	}
	for _, op := range openings {
		if best == "" || op.dateKey < best {
			best = op.dateKey
		}
	}
	if best != "" {
		return best, nil
	}
	// ② 没有期初结转 ⇒ 退回"已落库的最早快照日"。
	//
	// ⚠️ 2026-11-01 修：**必须把 id 一起查出来**。
	//
	//	下面用 `firstSnap.ID != ""` 判断"到底查到没查到行",而
	//	`Select("biz_date")` **只查了日期列** ⇒ ID 永远是零值
	//	⇒ 这个"没有期初结转"的兜底分支**永远进不去**
	//	⇒ 没有期初结转的店,链起点恒为空(链退化成"只算被查的那一天")。
	var firstSnap model.CutCostDaily
	if err := s.db.WithContext(ctx).
		Select("id", "biz_date").
		Where("branch_id = ?", branchID).
		Order("biz_date ASC").
		Limit(1).
		Find(&firstSnap).Error; err != nil {
		return "", fmt.Errorf("query cut_cost_daily(first): %w", err)
	}
	if firstSnap.ID != "" {
		best = time.Time(firstSnap.BizDate).In(loc).Format("2006-01-02")
	}
	return best, nil
}

// cutChainAnchor 找链的**锚点**:早于 from 的最近一个已落库快照的日子。
//
// # ⚠️ 锚点必须**可信**(2026-10-27 生产实测)
//
//	锚点的 closing 是"链算到那天的结果" ⇒ 只有当**从基线到锚点日每一天都有快照**
//	时它才是完整的。若中间断了(例如物化只覆盖了后半段)⇒
//	锚点的 closing 是**断链**算出来的 ⇒ 接上它 ⇒ 数字错得更离谱
//	(实测:成本 8.4 万 → 19.4 万、毛利率 −98.71%)。
//
// ⇒ 这里做**连续性校验**:基线 ~ 锚点日,每一天都要有快照,否则**不用锚点**
//
//	(宁可慢一点回溯,也不能拿断链的结余当期初)。
//
// 返回 (锚点那天的日期, 锚点的结余)。没有可信锚点时返回 (零值, nil)。
func (s *Service) cutChainAnchor(ctx context.Context, branchID string,
	from time.Time,
) (time.Time, *cutOpeningOverride, error) {
	loc := s.bizTZOrUTC()
	before := datatypesDate(from.In(loc))
	// ⚠️ 2026-11-01 修：**必须把 id 一起查出来**。
	//
	//	下面用 `latest.ID == ""` 判断"到底查到没查到锚点行",而
	//	`Select("biz_date")` **只查了日期列** ⇒ ID 永远是零值
	//	⇒ **锚点永远被判为"不存在"** ⇒ `computeCutChainDays` 每次都
	//	  从链起点**逐日重算**整条链(每多一天 = 一次远程销售查询)
	//	  ⇒ 毛利看板的耗时随日期**线性增长**(生产实测 店 00:
	//	    10-30 ≈1.2s / 10-31 ≈1.6s / 11-01 ≈2.0s,每天 +~0.4s)。
	//	⇒ 这就是"锚点快路径"从来没生效过的原因。
	var latest model.CutCostDaily
	if err := s.db.WithContext(ctx).
		Select("id", "biz_date").
		Where("branch_id = ? AND biz_date < ?", branchID, before).
		Order("biz_date DESC").
		Limit(1).
		Find(&latest).Error; err != nil {
		return time.Time{}, nil, fmt.Errorf("query cut_cost_daily(anchor): %w", err)
	}
	if latest.ID == "" {
		return time.Time{}, nil, nil
	}
	anchorDay := time.Time(latest.BizDate).In(loc).Format("2006-01-02")

	// ---- 连续性校验:基线 ~ 锚点日,每一天都要有快照 ----
	baseline, err := s.cutChainBaseline(ctx, branchID)
	if err != nil {
		return time.Time{}, nil, err
	}
	if baseline == "" {
		return time.Time{}, nil, nil
	}
	bs, err := time.ParseInLocation("2006-01-02", baseline, loc)
	if err != nil {
		return time.Time{}, nil, err
	}
	anchorDate, err := time.ParseInLocation("2006-01-02", anchorDay, loc)
	if err != nil {
		return time.Time{}, nil, err
	}
	if bs.After(anchorDate) {
		return time.Time{}, nil, nil
	}
	// ⚠️ **锚点必须晚于基线日**(2026-10-30 加)。
	//
	//	基线日(= 期初生效日)当天必须由**链自己算**(它的期初 = 门店录的值),
	//	不能用快照接续 —— 否则会把"基线日之前物化的旧快照"当成基线日的结余,
	//	旧账(含账面短卖的负链值)就又接回链里了。
	if !bs.Before(anchorDate) {
		return time.Time{}, nil, nil
	}
	var nSnap int64
	if err := s.db.WithContext(ctx).
		Model(&model.CutCostDaily{}).
		Where("branch_id = ? AND biz_date >= ? AND biz_date <= ?",
			branchID, datatypesDate(bs), datatypesDate(anchorDate)).
		Distinct("biz_date").
		Count(&nSnap).Error; err != nil {
		return time.Time{}, nil, fmt.Errorf("count cut_cost_daily(anchor continuity): %w", err)
	}
	wantDays := int(anchorDate.Sub(bs).Hours()/24) + 1
	if int(nSnap) < wantDays {
		// 链是断的 ⇒ 锚点不可信 ⇒ 宁可回溯。
		return time.Time{}, nil, nil
	}

	var rows []model.CutCostDaily
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date >= ? AND biz_date < ?",
			branchID, latest.BizDate, datatypesDate(time.Time(latest.BizDate).In(loc).AddDate(0, 0, 1))).
		Find(&rows).Error; err != nil {
		return time.Time{}, nil, fmt.Errorf("query cut_cost_daily(anchor rows): %w", err)
	}
	prev := &cutOpeningOverride{
		kg:     map[string]decimal.Decimal{},
		amount: map[string]decimal.Decimal{},
		unit:   map[string]decimal.Decimal{},
	}
	for _, r := range rows {
		// ⚠️ 该部位当天成本算不出来 ⇒ 它的期末金额是"0"而不是真实价值
		//   ⇒ 不能当锚点(会抹平可售金额、压小可售数量 ⇒ 单价爆炸)。
		if r.CostUncomputable {
			continue
		}
		// ⚠️ **负的期末要裁成 0**(2026-10-30 加,见 clampNonNegative 的说明):
		//   锚点读的是**已落库**的快照 ⇒ 存量快照里可能还有旧口径写的负数
		//   ⇒ 不裁 ⇒ 负期初从锚点漏回链里 ⇒ owner 说的"期初不能有负数"又破功。
		kg, amount := clampNonNegative(r.ClosingKg, r.ClosingAmount)
		prev.kg[string(r.CutType)] = kg
		prev.amount[string(r.CutType)] = amount
		// ★ 锚点也要带上移动加权单价(短卖时的兜底价)。
		if r.MovingAvgUnitCost.IsPositive() {
			prev.unit[string(r.CutType)] = r.MovingAvgUnitCost
		}
	}
	// ★ 累计短卖(R4):锚点那天"结转过来的累计" + "锚点当天的当日短卖"。
	//
	//	⚠️ **这里要把所有行都算上**(包括成本算不出来的部位):
	//	  短卖是**实物事实**,不是成本推导出来的 ⇒ 不能因为成本算不出来就不计。
	carried := decimal.Zero
	for _, r := range rows {
		carried = carried.Add(r.ShortSellKg)
	}
	if len(rows) > 0 {
		carried = carried.Add(rows[0].CarriedShortSellKg)
	}
	prev.shortSell = carried
	return anchorDate, prev, nil
}

// carriedShortSellFromSnapshot 读**上一营业日**的累计短卖(kg)。
//
// # 为什么需要它
//
// 逐日物化(`persistCutCostDaily`)是**一次算一天、不带链状态**的
// ⇒ 累计短卖只能从**上一营业日的快照**读回来。
//
// ⚠️ 快照里存的是:
//   - `short_sell_kg`      = 当日**总量**短卖**分摊到各部位**的值(Σ = 当日短卖);
//   - `carried_short_sell_kg` = 从上一期结转过来的累计值。
//
// ⇒ 累计(到上一营业日为止) = carried + 当日短卖。
//
// ⚠️ 查询用**纯日期字符串**区间(不用 time.Time 等值):
//
//	datatypes.Date 落库保留时区表示 ⇒ 字符串区间能同时罩住两种形态。
func (s *Service) carriedShortSellFromSnapshot(ctx context.Context, branchID string, bizDate time.Time) (decimal.Decimal, error) {
	loc := s.bizTZOrUTC()
	prevStart, _ := s.previousBizDayBounds(bizDate)
	prevKey := prevStart.In(loc).Format("2006-01-02")
	nextKey := prevStart.In(loc).AddDate(0, 0, 1).Format("2006-01-02")
	var rows []model.CutCostDaily
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date >= ? AND biz_date < ?", branchID, prevKey, nextKey).
		Find(&rows).Error; err != nil {
		return decimal.Zero, fmt.Errorf("query cut_cost_daily(carried short sell): %w", err)
	}
	if len(rows) == 0 {
		return decimal.Zero, nil
	}
	total := rows[0].CarriedShortSellKg
	for _, r := range rows {
		total = total.Add(r.ShortSellKg)
	}
	return total, nil
}

// computeCutChainDays 逐日链式算一条链,返回 **start ~ to** 每一天的结果。
//
// # start 怎么定
//
//	① **有锚点**(from 之前最近一个已落库快照)⇒ start = 锚点 + 1 天
//	   ⇒ 锚点的结余直接当期初 ⇒ 不用回溯 ⇒ 快路径。
//	② 没有锚点 ⇒ start = min(from, 最早基线日)
//	   (基线早于 from 时必须从基线算起 ⇒ 区间第一天的期初才是对的)
func (s *Service) computeCutChainDays(ctx context.Context, branchID string,
	from, to time.Time,
) ([]cutChainDay, error) {
	loc := s.bizTZOrUTC()
	start := from
	runner := newCutChainRunner(s, branchID)
	anchorDate, prev, err := s.cutChainAnchor(ctx, branchID, from)
	if err != nil {
		return nil, err
	}
	if prev != nil {
		// 锚点接续:锚点那天的结余就是链的状态 ⇒ 直接往下算。
		start = anchorDate.AddDate(0, 0, 1)
		runner.prev = prev
		for cut := range prev.kg {
			runner.inChain[cut] = true
		}
	} else {
		baseline, err := s.cutChainBaseline(ctx, branchID)
		if err != nil {
			return nil, err
		}
		if baseline != "" {
			bs, perr := time.ParseInLocation("2006-01-02", baseline, loc)
			if perr != nil {
				return nil, perr
			}
			if bs.Before(start) {
				start = bs
			}
		}
	}
	out := make([]cutChainDay, 0, 8)
	// ⚠️ **先预取整条链的销售**(2026-11-01 性能优化):
	//   下面的日循环每一天都会问一次 cube「这天卖了什么」⇒ N 次远程往返
	//   ⇒ 这里一次查回 [start..to] 的销售,日循环直接按天取 ⇒ 1 次远程往返。
	//
	//   语义与现在**逐字一致**:
	//     - 走不走 cube 由 salesReadPath 判定(问不到 / 被停用 ⇒ 原样上抛);
	//     - 预取失败(非权限类)⇒ Warn + 不挂预取 ⇒ 日循环照旧逐日查(错误照旧产生);
	//     - 预取的数据**不可信**(被 limit 截断 / 有行归不到某一天)
	//       ⇒ **整份丢弃** ⇒ 日循环照旧逐日查。
	//       (⚠️ 绝不"只丢那一行" —— 那会把「这天没卖」当成事实 ⇒ 毛利虚高。)
	ctx, err = s.prefetchCubeSalesRange(ctx, branchID, start, to)
	if err != nil {
		return nil, err
	}
	for d := start; !d.After(to); d = d.AddDate(0, 0, 1) {
		row, err := runner.day(ctx, d)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// computeCutChainDaysFull 逐日链式算一条链,**不走锚点**(一定从链的起点重算)。
//
// # 为什么审计要这样(而不是用 computeCutChainDays)
//
//	锚点 = 上一营业日的**已落库快照** ⇒ 它会把锚点之前的天**整段跳过**
//	⇒ 那些天的短卖就查不到 ⇒ 审计会**漏放**。
//	⇒ 审计要检查**截止日及之前的每一天** ⇒ 必须逐日重算。
//
// ⚠️ 代价:O(天) 的重算(与审计本身的成本重算同量级)⇒ 审计本来就是重操作,可以接受。
func (s *Service) computeCutChainDaysFull(ctx context.Context, branchID string,
	from, to time.Time,
) ([]cutChainDay, error) {
	loc := s.bizTZOrUTC()
	start := from
	baseline, err := s.cutChainBaseline(ctx, branchID)
	if err != nil {
		return nil, err
	}
	if baseline != "" {
		bs, perr := time.ParseInLocation("2006-01-02", baseline, loc)
		if perr != nil {
			return nil, perr
		}
		if bs.Before(start) {
			start = bs
		}
	}
	runner := newCutChainRunner(s, branchID)
	out := make([]cutChainDay, 0, 8)
	// ⚠️ **先预取整条链的销售**(2026-11-01 性能优化)—— 同 computeCutChainDays。
	//   审计要逐日重算(每一天都要问一次 cube)⇒ 同样吃"天数 × 330ms"的亏。
	ctx, err = s.prefetchCubeSalesRange(ctx, branchID, start, to)
	if err != nil {
		return nil, err
	}
	for d := start; !d.After(to); d = d.AddDate(0, 0, 1) {
		row, err := runner.day(ctx, d)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// materializeCostChain 逐日**物化**成本链(落 `cut_cost_daily`),**不要求当天有盘点单**。
//
// # 为什么落库不绑盘点
//
// 盘点是"**实盘锚定**"(用实盘校正期末),不是"链的开关"。
// 把链的推进绑在盘点上 ⇒ 零盘点门店的链**每天断一次** ⇒ 成本塌成期初残值
// (2026-10-27 生产实测:店 00 从 10-03 起成本 ≈ 100 元、毛利率 97%)。
//
// ⇒ 这里逐日重算 fromDate → 今天并落库;**链靠"前一日快照"自动接续**
//
//	(引擎的 loadCutOpening ① 就是读前一日快照的结余)。
//
// ⚠️ 链必须**从早到晚**落:后一天要读前一天的快照。
func (s *Service) materializeCostChain(ctx context.Context, branchID string, fromDate time.Time) (int, error) {
	loc := s.bizTZOrUTC()
	// ⚠️ 用 s.now()(可注入时钟),不用 time.Now() ⇒ 单测里"今天"跟着测试时钟走。
	today := s.now().In(loc)
	// ⚠️ **必须从链起点(基线)开始**(2026-10-27 生产实测):
	//   只从某个中间的日子物化 ⇒ 那天的链是**断的**(接不到基线)
	//   ⇒ 落下来的快照 closing 不完整 ⇒ 锚点接到它 ⇒ 数字错得更离谱
	//   (实测:成本从 8.4 万变成 19.4 万、毛利率 −98.71%)。
	baseline, err := s.cutChainBaseline(ctx, branchID)
	if err != nil {
		return 0, err
	}
	start := fromDate.In(loc)
	if baseline != "" {
		bs, perr := time.ParseInLocation("2006-01-02", baseline, loc)
		if perr != nil {
			return 0, perr
		}
		if bs.Before(start) {
			start = bs
		}
	}
	// ⚠️ **按日历逐日**,不用 deriveColdChain 的日期集合 ——
	//   那个集合只含"有流水记录的日子" ⇒ 会漏天 ⇒ 链被截断。
	//   (引擎的 loadCutOpening ① 读前一日快照 ⇒ 逐日落库时链自动接续 ✓)
	total := 0
	for d := start; !d.After(today); d = d.AddDate(0, 0, 1) {
		n, err := s.persistCutCostDaily(ctx, branchID, d, false)
		if err != nil {
			return total, fmt.Errorf("物化 %s 的成本日快照: %w", d.Format("2006-01-02"), err)
		}
		total += n
	}
	return total, nil
}

// MaterializeCostChain 逐日物化成本链(对外入口)。
//
// # 干什么
//
// 把 fromDate → 今天每一天的**部位级成本日快照**重算并落库(`cut_cost_daily`),
// **不要求当天有盘点单** —— 盘点只是"实盘锚定",不是"链的开关"。
//
// # 为什么需要它
//
// 长期毛利要快 ⇒ 得有现成的日序列可读。
// 链式回溯是 O(天) 的 ⇒ 没物化时,查 26 天要 ~9s、查一个月更久。
// 物化之后 ⇒ 前一日快照直接当期初 ⇒ O(1)。
//
// # 幂等
//
// 逐日 upsert(`cut_cost_daily` 的唯一键 = branch_id + biz_date + cut_type)
// ⇒ 重复调用安全;它是**派生数据**,不改业务事实(不像审计那样不可逆)。
func (s *Service) MaterializeCostChain(ctx context.Context, branchID, fromDate string) (int, error) {
	if branchID == "" {
		return 0, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	loc := s.bizTZOrUTC()
	from := s.now().In(loc)
	if fromDate != "" {
		parsed, err := time.ParseInLocation("2006-01-02", fromDate, loc)
		if err != nil {
			return 0, fmt.Errorf("%w: from 格式应为 YYYY-MM-DD", ErrInvalidInput)
		}
		from = parsed
	}
	return s.materializeCostChain(ctx, branchID, from)
}

// chainJob 一个门店的物化任务状态。
type chainJob struct {
	mu sync.Mutex
	// running = true ⇒ 正在物化。
	running bool
	// pending = true ⇒ 物化期间又来了新变更 ⇒ 这一轮结束后要再跑一轮。
	pending bool
	// from 是本轮要重算的起点(多次触发取**最早**的那个)。
	from time.Time
}

// ScheduleChainMaterialize 异步物化成本链(写路径触发)。
//
// # 为什么异步
//
// 物化是 O(天) 的(整条链重算一遍)⇒ 放在写路径里同步跑 ⇒
// 录一头猪要等几秒 ⇒ 门店体验不可接受。
// ⇒ 后台跑 + **不继承请求 ctx**(请求返回后 ctx 会被取消 ⇒ 物化会中途夭折)。
//
// ⚠️ ctx 语义:用 context.WithoutCancel 保留 trace/值,但**去掉取消**;
//
//	再套自己的超时 ⇒ 物化不会因为客户端断开而半途而废。
//
// # 为什么要"合并"
//
// 门店连续录猪时,每个写操作都会触发一次物化 ⇒ 若不合并,后台会排一串
// 全链重算(每个都是 O(天))⇒ CPU 被吃光、且**最后一次变更可能没被覆盖**。
// ⇒ 物化期间的新触发只记一个 pending ⇒ 本轮结束后再跑一轮(起点取最早的)。
//
// # 为什么需要它
//
// 成本链是**逐日接续**的 ⇒ 任何一天的产出/库存变了,后面所有天的成本都要跟着变。
// ⇒ 写路径(录猪/删猪/大宗/搬上台/期初结转)之后必须把链重算一遍,
//
//	否则门店看到的成本是旧的。
func (s *Service) ScheduleChainMaterialize(ctx context.Context, branchID string, fromDate time.Time) {
	if branchID == "" {
		return
	}
	v, _ := s.chainMaterializeGuard.LoadOrStore(branchID, &chainJob{})
	job := v.(*chainJob)
	job.mu.Lock()
	if job.running {
		// 已经在跑 ⇒ 记一笔 pending(起点取最早)⇒ 本轮结束后再跑。
		job.pending = true
		if job.from.IsZero() || fromDate.Before(job.from) {
			job.from = fromDate
		}
		job.mu.Unlock()
		return
	}
	job.running = true
	job.from = fromDate
	job.mu.Unlock()
	s.chainMaterializer(func() { s.runChainMaterialize(ctx, branchID, job) })
}

// runChainMaterialize 跑物化循环(直到没有 pending)。
func (s *Service) runChainMaterialize(ctx context.Context, branchID string, job *chainJob) {
	for {
		job.mu.Lock()
		from := job.from
		job.pending = false
		job.mu.Unlock()

		// 不继承请求的取消 => 客户端断开也不会让物化夭折。
		base := context.Background()
		if ctx != nil {
			base = context.WithoutCancel(ctx)
		}
		bgCtx, cancel := context.WithTimeout(base, 10*time.Minute)
		n, err := s.materializeCostChain(bgCtx, branchID, from)
		cancel()
		if err != nil {
			s.pubLogger.Error("物化成本链失败",
				"branch_id", branchID, "from_date", from.In(s.bizTZOrUTC()).Format("2006-01-02"), "err", err)
		} else {
			s.pubLogger.Info("已物化成本链",
				"branch_id", branchID,
				"from_date", from.In(s.bizTZOrUTC()).Format("2006-01-02"),
				"rows", n)
		}

		job.mu.Lock()
		if job.pending {
			job.mu.Unlock()
			continue // 还有新变更 ⇒ 再跑一轮(起点已在 pending 合并时取过最早)。
		}
		job.running = false
		job.mu.Unlock()
		return
	}
}
