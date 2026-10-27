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
	"sort"
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
	// costIncomplete = true ⇒ 当天有部位的成本**算不出来**(不是成本为 0)。
	costIncomplete bool
}

// cutChainRunner 逐日推进一条成本链(内存链)。
//
// ⚠️ **链是按部位独立接续的**:部位 A 的库存变动不影响部位 B。
//   ⇒ 某个部位一旦"入链"(有过库存或进货),它的期末就逐日传给下一天;
//     没入链的部位(既无期初结转也无进货)**不接续** ——
//     否则"无基线期间卖出的货"会变成负库存,一路污染到有基线的日子。
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
	for _, row := range cutRows {
		cut := string(row.CutType)
		if row.CostUncomputable {
			out.costIncomplete = true
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
	r.prev = next
	return out, nil
}

// cutChainBaseline 取该店**最早有基线**的营业日(YYYY-MM-DD);空 = 没有基线。
func (s *Service) cutChainBaseline(ctx context.Context, branchID string) (string, error) {
	loc := s.bizTZOrUTC()
	best := ""
	// ① 门店期初结转(按部位)的最早生效日。
	openings, err := s.loadOpenings(ctx, branchID)
	if err != nil {
		return "", err
	}
	for _, op := range openings {
		if best == "" || op.dateKey < best {
			best = op.dateKey
		}
	}
	// ② 已落库的成本日快照的最早日。
	var firstSnap model.CutCostDaily
	if err := s.db.WithContext(ctx).
		Select("biz_date").
		Where("branch_id = ?", branchID).
		Order("biz_date ASC").
		Limit(1).
		Find(&firstSnap).Error; err != nil {
		return "", fmt.Errorf("query cut_cost_daily(first): %w", err)
	}
	if firstSnap.ID != "" {
		k := time.Time(firstSnap.BizDate).In(loc).Format("2006-01-02")
		if best == "" || k < best {
			best = k
		}
	}
	return best, nil
}

// cutChainAnchor 找链的**锚点**:早于 from 的最近一个已落库快照的日子。
//
// 返回 (锚点那天的日期, 锚点的结余)。没有锚点时返回 (零值, nil)。
func (s *Service) cutChainAnchor(ctx context.Context, branchID string,
	from time.Time,
) (time.Time, *cutOpeningOverride, error) {
	loc := s.bizTZOrUTC()
	before := datatypesDate(from.In(loc))
	var latest model.CutCostDaily
	if err := s.db.WithContext(ctx).
		Select("biz_date").
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
		prev.kg[string(r.CutType)] = r.ClosingKg
		prev.amount[string(r.CutType)] = r.ClosingAmount
		// ★ 锚点也要带上移动加权单价(短卖时的兜底价)。
		if r.MovingAvgUnitCost.IsPositive() {
			prev.unit[string(r.CutType)] = r.MovingAvgUnitCost
		}
	}
	anchorDate, err := time.ParseInLocation("2006-01-02", anchorDay, loc)
	if err != nil {
		return time.Time{}, nil, err
	}
	return anchorDate, prev, nil
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
//   (引擎的 loadCutOpening ① 就是读前一日快照的结余)。
//
// ⚠️ 链必须**从早到晚**落:后一天要读前一天的快照。
func (s *Service) materializeCostChain(ctx context.Context, branchID string, fromDate time.Time) (int, error) {
	loc := s.bizTZOrUTC()
	// ⚠️ 用 s.now()(可注入时钟),不用 time.Now() ⇒ 单测里"今天"跟着测试时钟走。
	today := s.now().In(loc)
	chain, err := s.deriveColdChain(ctx, branchID, "", nil)
	if err != nil {
		return 0, err
	}
	fromKey := fromDate.In(loc).Format("2006-01-02")
	dates := make([]string, 0, len(chain))
	for d := range chain {
		if d >= fromKey && d <= today.Format("2006-01-02") {
			dates = append(dates, d)
		}
	}
	sort.Strings(dates)
	total := 0
	for _, d := range dates {
		n, err := s.persistCutCostDaily(ctx, branchID, mustParseDateInLoc(d, loc), false)
		if err != nil {
			return total, fmt.Errorf("物化 %s 的成本日快照: %w", d, err)
		}
		total += n
	}
	return total, nil
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
//   再套自己的超时 ⇒ 物化不会因为客户端断开而半途而废。
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
//   否则门店看到的成本是旧的。
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
