// Package service / cut_split_algo.go —— 部位级「建议分割 / 实际分割 / 调整后权重」(2026-10-29)。
//
// # 为什么要有这三个数
//
// 鲜肉毛利看板的「部位明细」卡片要同时给门店看三行:
//
//	建议分割(S_c)   —— AI/算法给的**参考**:这头猪这个部位**应该**出多少。
//	实际分割(A_c)   —— 门店**实际**出了多少(优先用 pig_cuts 实录;没有实录才倒推)。
//	调整后权重(W_c) —— **成本基准**:销售成本按它算。
//
// # 口径(owner 问卷定案 2026-10-29)
//
//	① 建议分割 = **纯算法建议**:统计基线 p50 优先、LLM 兜底,**不含 pig_cuts 实录**。
//	   (实录属于"实际"那一侧;建议必须是"一条录入都没有时也能给"的数。)
//	② 实际分割 = **实录优先**(当日进猪的猪的 pig_cuts,按部位求和);
//	   没有实录才倒推,**两分支**(见 deriveActualSplits 的注释)。
//	③ 实际分割超出建议时:其余部位**先按「可压缩量(建议−实际)」扣,不够再按建议比例摊**。
//
// # 与成本引擎的关系
//
//	W_c 取代「当日入库量」进移动加权循环 ⇒ 部位单价变 ⇒ 销售成本/期末金额/毛利自动重算。
//
// ⚠ **W 每天现算,绝不物化成快照常量** —— 盘点补录后 A_c 变 ⇒ W 变 ⇒ 成本跟着变
//
//	(owner:「实盘数变了,你仍然需要用那个算法去重新计算」)。
//
// ⚠ 本文件与仓库里已有的 `cut_split.go`(部位级 ⇄ SKU 级拆分,给审计/预售结转用)
//
//	**不是一回事**,名字挨着而已,别混。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
)

// 实际分割的来源标记(对外 wire:`actual_split_source`)。
const (
	// ActualSplitSourceRecorded = **实录**(当日 pig_cuts)—— 最准的来源,总是有值。
	ActualSplitSourceRecorded = "recorded"
	// ActualSplitSourceDerived = **倒推**(有实盘 = 全店恒等式;无实盘 = 台面流量近似)。
	ActualSplitSourceDerived = "derived"
)

// stocktakeCutKg 当日盘点单读出来的部位级数量(kg)。
//
// # ⚠ 「实盘」在全仓**只有一个含义**:当日**猪肉台**上的盘点数
//
//	= `CutSnapshot.ActualRemainKg`(= model/model.go:367-378 写死的语义)。
//
// # ⚠ **冷库量不是实盘**
//
//	它是盘点单里**另一个字段**(`CutSnapshot.ColdStoreKg`,盘点**那一刻**的冷库余额),
//	报表上也是**单独一列**(`cold_store_kg`)⇒ 别把两者混成"实盘"。
type stocktakeCutKg struct {
	// CounterActual 是**完整**盘点单(is_complete=true)里的**猪肉台实盘**(按部位求和)。
	CounterActual map[string]decimal.Decimal
	// Cold 是当日**最新那张**盘点单里的**冷库量**(按部位求和)。
	//
	// ⚠ **不求和**:同一天若有多张盘点单,求和会**重复计数**
	//   (半盘 + 全盘 ⇒ 同一个部位被算两遍)⇒ 取**最新一张**即可。
	//   (盘点单主键 = surrogateID("SK", branch, 归属日)⇒ 正常情况下一天只有一张。)
	Cold map[string]decimal.Decimal
	// HasComplete = 当日有没有**完整**盘点单。
	HasComplete bool
	// HasAny = 当日有没有**任何**盘点单(含半盘)。
	HasAny bool
}

// cutSplitWeights 某营业日的部位级分割数据(S / A / W)。
type cutSplitWeights struct {
	// Suggested 是**建议分割** S_c(kg):纯算法建议(基线 p50 → LLM 兜底),不含 pig_cuts 实录。
	Suggested map[string]decimal.Decimal
	// Actual 是**实际分割** A_c(kg)—— **内部值**(含按显示规则被隐藏的那些)。
	Actual map[string]decimal.Decimal
	// Source 是每个部位的实际分割来源:recorded | derived。
	Source map[string]string
	// Adjusted 是**调整后权重** W_c(kg)—— 成本基准。
	Adjusted map[string]decimal.Decimal
	// CounterActual 是**猪肉台实盘**(kg,按部位;来自完整盘点单)。
	CounterActual map[string]decimal.Decimal
	// Cold 是当日盘点单里的**冷库量**(kg,按部位)——
	// 「全店期末结余 = 猪肉台实盘 + 冷库量」用它。
	//
	// ⚠ 冷库量**不是实盘**:它是盘点单里的另一个字段,报表上单独一列。
	Cold map[string]decimal.Decimal
	// CounterLoad 是当日**搬上台**的重量(kg,按部位)。
	//
	// ⚠ 2026-10-30 owner 口径:毛利看板那张表**整张都是猪肉台的**
	//   ⇒ 「期初」列改成显示这个数(门店自己搬的量,一眼认得),
	//   而不是账面链值(可以是负数 = 账面短卖 ⇒ 门店看不懂"为什么期初是负的")。
	CounterLoad map[string]decimal.Decimal
	// HasStocktake = 当日有没有**完整**盘点单(is_complete=true)—— 决定实际分割显不显示。
	HasStocktake bool
	// HasAnyStocktake = 当日有没有**任何**盘点单(含半盘)——
	// 决定「冷库量」是取盘点单还是退回引擎链值。
	HasAnyStocktake bool
	// Note 是告警文案(空 = 没有告警)。
	Note string
}

// loadSuggestedSplitKg 算各部位的**建议分割** S_c(kg)。
//
// # 口径
//
//	对当日**每一头猪**:
//	  ① 该猪体重段的统计基线 p50(样本够)⇒ 该部位的建议值;
//	  ② 基线没覆盖到的部位 ⇒ 用这头猪的 LLM 建议(expected_kg)。
//
// ⚠ **不含 pig_cuts 实录** —— owner 定案:建议是"AI/算法"的数,实录是"实际"那一侧。
//
//	(这与 `computeProducedBySku` 的产出优先级**故意不同** ⇒ 别把那段逻辑搬过来。)
//
// ⚠ 基线是**按体重段 × 部位**分层的(见 forecast.go)⇒ 换体重段建议就变。
func (s *Service) loadSuggestedSplitKg(ctx context.Context, branchID string, pigs []model.WholePig) (
	map[string]decimal.Decimal, error) {
	out := map[string]decimal.Decimal{}
	if len(pigs) == 0 {
		return out, nil
	}
	baselines, err := s.loadForecastBaselines(ctx, branchID)
	if err != nil {
		return nil, err
	}
	for _, p := range pigs {
		band := WeightBandOf(p.GrossWeightKg)
		// ① 统计基线 p50 优先(基线是从真实补录样本拟合出来的 ⇒ 比 LLM 准)。
		covered := map[model.CutType]bool{}
		for _, b := range baselines {
			if b.WeightBand != band || !b.Usable() || !b.P50Kg.IsPositive() {
				continue
			}
			out[string(b.CutType)] = out[string(b.CutType)].Add(b.P50Kg)
			covered[b.CutType] = true
		}
		// ② LLM 兜底(基线没覆盖到的部位)。
		if len(p.LLMAdviceJSON) == 0 {
			continue
		}
		var advice struct {
			Cuts []PredictedCut `json:"cuts"`
		}
		if err := json.Unmarshal(p.LLMAdviceJSON, &advice); err != nil {
			continue
		}
		for _, c := range advice.Cuts {
			if covered[c.CutType] || !c.ExpectedKg.IsPositive() {
				continue
			}
			out[string(c.CutType)] = out[string(c.CutType)].Add(c.ExpectedKg)
		}
	}
	return out, nil
}

// loadRecordedSplitKg 算各部位的**实际分割实录**(kg)。
//
// # 口径
//
//	当日**进猪**的猪,其 `pig_cuts` 里该部位(cut_type)的 weight_kg 之和。
//
// ⚠ 按 **cut_type** 汇总(不是按 SKU)⇒ **没有 cube_product_id 的补录也算在内**
//
//	(那部分在 `computeProducedBySku` 里叫 pendingByCut,漏掉它 = 实录少算)。
func (s *Service) loadRecordedSplitKg(ctx context.Context, branchID string, startOfDay, endOfDay time.Time) (
	map[string]decimal.Decimal, error) {
	var pigCuts []model.PigCut
	if err := s.db.WithContext(ctx).
		Joins("JOIN whole_pigs ON whole_pigs.id = pig_cuts.pig_id").
		Where("pig_cuts.branch_id = ? AND whole_pigs.arrived_at >= ? AND whole_pigs.arrived_at < ?",
			branchID, startOfDay, endOfDay).
		Find(&pigCuts).Error; err != nil {
		return nil, fmt.Errorf("query pig_cuts(split): %w", err)
	}
	out := map[string]decimal.Decimal{}
	for _, c := range pigCuts {
		out[string(c.CutType)] = out[string(c.CutType)].Add(c.WeightKg)
	}
	return out, nil
}

// loadStocktakeCutKg 读当日**盘点单**里各部位的**猪肉台实盘**与**冷库量**(kg)。
//
// # ⚠ `CutSnapshot.ActualRemainKg` = **猪肉台(柜台)实盘**(不含冷库)
//
//	`CutSnapshot.ColdStoreKg`   = **冷库量**(盘点那一刻的冷库余额)
//
// ⚠ 柜台实盘只认**完整**盘点单(is_complete=true);半盘/草稿不算实盘。
//
// ⚠ 同一天有多张盘点单时**只取最新一张**(按 taken_at,再按 created_at):
//
//	求和会重复计数(半盘 + 全盘 ⇒ 同一部位算两遍)。
func (s *Service) loadStocktakeCutKg(ctx context.Context, branchID string, startOfDay, endOfDay time.Time) (
	stocktakeCutKg, error) {
	out := stocktakeCutKg{
		CounterActual: map[string]decimal.Decimal{},
		Cold:          map[string]decimal.Decimal{},
	}
	var rows []model.PorkCutsStocktake
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND taken_at >= ? AND taken_at < ?", branchID, startOfDay, endOfDay).
		Find(&rows).Error; err != nil {
		return out, fmt.Errorf("query pork_cuts_stocktakes(split): %w", err)
	}
	if len(rows) == 0 {
		return out, nil
	}
	out.HasAny = true
	// 取**最新一张**(同一天多张时不重复计数)。
	latest := rows[0]
	for _, r := range rows[1:] {
		if r.TakenAt.After(latest.TakenAt) ||
			(r.TakenAt.Equal(latest.TakenAt) && r.CreatedAt.After(latest.CreatedAt)) {
			latest = r
		}
	}
	if len(latest.Cuts) == 0 {
		return out, nil
	}
	var snaps []model.CutSnapshot
	if err := json.Unmarshal(latest.Cuts, &snaps); err != nil {
		return out, nil
	}
	// 冷库量:完整/半盘都给(它是盘点单里的**另一个字段**,不是实盘)。
	for _, sn := range snaps {
		out.Cold[string(sn.CutType)] = out.Cold[string(sn.CutType)].Add(sn.ColdStoreKg)
	}
	if !latest.IsComplete {
		return out, nil // 半盘/草稿:不算实盘
	}
	out.HasComplete = true
	for _, sn := range snaps {
		out.CounterActual[string(sn.CutType)] = out.CounterActual[string(sn.CutType)].Add(sn.ActualRemainKg)
	}
	return out, nil
}

// loadCounterLoadKgByCut 读当日**搬上台**的重量(kg,按部位)。
//
// ⚠ 搬上台的是**冷库 → 猪肉台的老货**(早就分割过、成本早就摊过)
//
//	⇒ 无实盘时倒推实际分割**必须扣掉**,否则同一批成本被摊两次、今天的产出被算大。
//
// ⚠ 老数据可能没记 cut_type ⇒ 按 SKU 反查部位;查不到就跳过(**不猜**)。
func (s *Service) loadCounterLoadKgByCut(ctx context.Context, branchID string, bizDate time.Time,
	cutOfSku map[string]model.CutType) (map[string]decimal.Decimal, error) {
	start, end := s.businessDayBounds(bizDate)
	var rows []model.CounterLoadRecord
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date >= ? AND biz_date < ?", branchID, start, end).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query counter_loads(split): %w", err)
	}
	out := map[string]decimal.Decimal{}
	for _, r := range rows {
		cut := string(r.CutType)
		if cut == "" {
			if c, ok := cutOfSku[r.CubeProductID]; ok {
				cut = string(c)
			}
		}
		if cut == "" {
			continue
		}
		out[cut] = out[cut].Add(r.QtyKg)
	}
	return out, nil
}

// deriveActualSplits 算各部位的**实际分割** A_c 与来源标记。
//
// # 口径(owner 定案 2026-10-29)
//
//	① **实录优先**:当日 pig_cuts 有该部位的补录 ⇒ A_c = 实录之和,来源 `recorded`。
//	   (只有部分猪录了也**整体**取实录值,**不与倒推混算**。)
//
//	② 否则**有日终实盘**(完整盘点单):
//
//	   A_c = 全店期末结余 + 销售 + 统收 + 报损 − 全店期初
//	         其中 全店期末结余 = 猪肉台实盘 + 冷库量(两者都取盘点单)
//
//	   ⇒ 这是**全店恒等式**解出来的"今日新分割产出":
//	     期初 + 今日分割 + 大宗净 − 销售 − 统收 − 报损 = 期末
//
//	③ 否则**无日终实盘**(台面流量倒推的**近似**):
//
//	   A_c = max(0, 销售 + 统收 + 报损 − 全店期初 − 搬上台)
//
// # ⚠ 为什么**必须用全店口径**(很容易被"优化"回台面口径 ⇒ 这段别删)
//
//	台面恒等式里**根本没有"今日分割"**:
//	  今天的分割先进冷库、再搬上台;台面只看得到"搬上台"
//	  ⇒ 台面期末 = 台面期初 + 搬上台 − 搬回 − 销售 − 统收 − 报损
//	  ⇒ 从台面**推不出今日分割**(搬上台是几天的老货,不是今天的)。
//
//	⇒ "今日新分割"只能从**全店**恒等式解 ⇒ **必须把冷库量加回来**。
//
//	⇒ 这也正是"**搬上台的老货不算今日分割**"能成立的原因:
//	   搬上台在全店口径里是冷库↔台面的**内部转移**,天然抵消 ✔
//	   (owner:「搬上台的老货不能算为今日分割」)
//
// # ⚠ 命名:「实盘」= **当日猪肉台上的盘点数**(ActualRemainKg),**不含冷库**;
//
//	冷库量(ColdStoreKg)是**另一个字段**。「全店期末结余」≠「全店期末实盘」。
//
// # ⚠ 三个分支的结果都**不小于 0**(分割量不可能是负数)。
func deriveActualSplits(recorded map[string]decimal.Decimal, hasStocktake bool,
	soldKg, bulkKg, wasteKg map[string]decimal.Decimal,
	counterActual, coldKg, openKg, counterLoadKg map[string]decimal.Decimal) (
	map[string]decimal.Decimal, map[string]string) {
	actual := map[string]decimal.Decimal{}
	source := map[string]string{}
	cuts := map[string]struct{}{}
	for _, m := range []map[string]decimal.Decimal{
		recorded, soldKg, bulkKg, wasteKg, counterActual, coldKg, openKg, counterLoadKg,
	} {
		for c := range m {
			cuts[c] = struct{}{}
		}
	}
	for c := range cuts {
		if rec := recorded[c]; rec.IsPositive() {
			actual[c] = rec
			source[c] = ActualSplitSourceRecorded
			continue
		}
		var derived decimal.Decimal
		if hasStocktake {
			// 有实盘:全店恒等式解"今日新分割"(搬上台在全店口径里天然抵消)。
			// ⚠ 全店期末**结余** = 猪肉台实盘 + 冷库量(**不是**"全店期末实盘")。
			closingBalance := counterActual[c].Add(coldKg[c])
			derived = closingBalance.Add(soldKg[c]).Add(bulkKg[c]).Add(wasteKg[c]).Sub(openKg[c])
		} else {
			// 无实盘:台面流量倒推(**近似**)。搬上台是老货 ⇒ 扣掉。
			derived = soldKg[c].Add(bulkKg[c]).Add(wasteKg[c]).Sub(openKg[c]).Sub(counterLoadKg[c])
		}
		if derived.IsNegative() {
			derived = decimal.Zero
		}
		actual[c] = derived
		source[c] = ActualSplitSourceDerived
	}
	return actual, source
}

// redistributeSplitWeights 算各部位的**调整后权重** W_c(kg)—— 成本基准。
//
// # 口径(owner 定案)
//
//	O = { c : A_c > S_c }              // 用**内部** A_c(含被显示规则隐藏的那些)
//	若 O 为空: W_c = S_c
//	否则:
//	    E = Σ_{c∈O} (A_c − S_c)
//	    c ∈ O: W_c = A_c               // 该部位直接取实际分割
//	    c ∉ O: 第一轮按**可压缩量**扣:pool_c = max(0, S_c − A_c),
//	           take_c = min(pool_c, E × pool_c / Σpool),W_c = S_c − take_c
//	           第二轮(还不够)按**当前 W 的比例**摊:
//	           take_c = min(W_c, rest × W_c / ΣW),W_c −= take_c
//	           rest 仍 > 0 ⇒ 记告警(不许静默)
//
// # 两条守卫(2026-10-29 生产实测踩到的退化场景)
//
//	守卫①:ΣS_c <= 0(当天没有进猪 / 没有建议分割)⇒ **不重分配**,W_c = S_c。
//	守卫②:可压缩量总量为 0(**无处可挪**)⇒ **不重分配**,W_c = S_c,并记告警。
//
// # 为什么必须有守卫(生产实测 2026-10-29,店 00)
//
//	当天没有进猪 ⇒ S_c 全是 0;但销量 27.58kg ⇒ 倒推出的 A_c = 27.58 > S_c = 0
//	⇒ 若拿 A_c 当入库量,账面的"短卖"(应有 = −27.58)会被撑成 0
//	⇒ **把短卖掩盖掉了**(而且入库金额为 0 ⇒ 走兜底价 ⇒ 成本算成 0 ⇒ 毛利 100%)。
//
// ⇒ 守卫①的行为与今天**逐字一致**;守卫②**要出告警**(门店看得见)。
func redistributeSplitWeights(suggested, actual map[string]decimal.Decimal) (map[string]decimal.Decimal, string) {
	cuts := map[string]struct{}{}
	for c := range suggested {
		cuts[c] = struct{}{}
	}
	for c := range actual {
		cuts[c] = struct{}{}
	}
	ordered := make([]string, 0, len(cuts))
	for c := range cuts {
		ordered = append(ordered, c)
	}
	sort.Strings(ordered)

	weights := map[string]decimal.Decimal{}
	var totalS decimal.Decimal
	for _, c := range ordered {
		weights[c] = suggested[c]
		totalS = totalS.Add(suggested[c])
	}
	// 守卫①:没有建议分割(ΣS <= 0)⇒ 不重分配。
	if !totalS.IsPositive() {
		return weights, ""
	}

	// O = { c : A_c > S_c }。
	over := map[string]bool{}
	var excess decimal.Decimal
	for _, c := range ordered {
		if actual[c].GreaterThan(suggested[c]) {
			over[c] = true
			excess = excess.Add(actual[c].Sub(suggested[c]))
		}
	}
	if len(over) == 0 {
		return weights, ""
	}

	// 可压缩量:其余部位最多能让出多少。
	pool := map[string]decimal.Decimal{}
	var poolTotal decimal.Decimal
	for _, c := range ordered {
		if over[c] {
			continue
		}
		p := suggested[c].Sub(actual[c])
		if p.IsNegative() {
			p = decimal.Zero
		}
		pool[c] = p
		poolTotal = poolTotal.Add(p)
	}
	// 守卫②:无处可挪 ⇒ 不重分配 + 告警。
	if !poolTotal.IsPositive() {
		return weights, "分割权重无法重分配:实际分割超出建议,但其它部位没有可压缩量(今天仍按建议分割算成本)"
	}

	// 第一轮:按可压缩量扣。
	rest := excess
	round1 := map[string]decimal.Decimal{}
	for _, c := range ordered {
		if over[c] || !pool[c].IsPositive() {
			continue
		}
		t := excess.Mul(pool[c]).Div(poolTotal)
		if t.GreaterThan(pool[c]) {
			t = pool[c]
		}
		round1[c] = t
		rest = rest.Sub(t)
	}
	for _, c := range ordered {
		weights[c] = weights[c].Sub(round1[c])
	}

	// 第二轮:还不够 ⇒ 按**当前 W** 的比例摊。
	if rest.IsPositive() {
		var sumW decimal.Decimal
		for _, c := range ordered {
			if !over[c] {
				sumW = sumW.Add(weights[c])
			}
		}
		if sumW.IsPositive() {
			round2 := map[string]decimal.Decimal{}
			for _, c := range ordered {
				if over[c] {
					continue
				}
				w := weights[c]
				if !w.IsPositive() {
					continue
				}
				t := rest.Mul(w).Div(sumW)
				if t.GreaterThan(w) {
					t = w
				}
				round2[c] = t
			}
			var taken2 decimal.Decimal
			for _, c := range ordered {
				taken2 = taken2.Add(round2[c])
				weights[c] = weights[c].Sub(round2[c])
			}
			rest = rest.Sub(taken2)
		}
	}

	// O 里的部位:直接取实际分割。
	for _, c := range ordered {
		if over[c] {
			weights[c] = actual[c]
		}
	}

	note := ""
	if rest.IsPositive() {
		note = fmt.Sprintf("分割权重无法守恒:实际分割超出建议 %s kg,其它部位已无可压缩量", rest.String())
	}
	return weights, note
}

// computeCutSplit 算某营业日的部位级分割数据(S / A / W)。
//
// # cuts 是引擎要出行的部位集合
//
//	W 只在这些部位上算 ⇒ **每个 W > 0 的部位都有一行** ⇒ 入库金额不会"摊丢"。
//
// ⚠ W **每天现算**,不落快照 ⇒ 盘点补录后自然重算(owner:「实盘数变了要重新算」)。
func (s *Service) computeCutSplit(ctx context.Context, branchID string,
	startOfDay, endOfDay time.Time, bizDate time.Time,
	pigs []model.WholePig,
	cutOfSku map[string]model.CutType,
	soldKg, bulkKg, wasteKg, openKg map[string]decimal.Decimal,
	cuts map[string]bool) (*cutSplitWeights, error) {
	out := &cutSplitWeights{
		Suggested:     map[string]decimal.Decimal{},
		Actual:        map[string]decimal.Decimal{},
		Source:        map[string]string{},
		Adjusted:      map[string]decimal.Decimal{},
		CounterActual: map[string]decimal.Decimal{},
		Cold:          map[string]decimal.Decimal{},
		CounterLoad:   map[string]decimal.Decimal{},
	}
	keep := func(c string) bool {
		if cuts == nil {
			return true
		}
		return cuts[c]
	}

	suggested, err := s.loadSuggestedSplitKg(ctx, branchID, pigs)
	if err != nil {
		return nil, err
	}
	for c, v := range suggested {
		if keep(c) {
			out.Suggested[c] = v
		}
	}
	recorded, err := s.loadRecordedSplitKg(ctx, branchID, startOfDay, endOfDay)
	if err != nil {
		return nil, err
	}
	stocktake, err := s.loadStocktakeCutKg(ctx, branchID, startOfDay, endOfDay)
	if err != nil {
		return nil, err
	}
	out.HasStocktake = stocktake.HasComplete
	out.HasAnyStocktake = stocktake.HasAny
	for c, v := range stocktake.CounterActual {
		if keep(c) {
			out.CounterActual[c] = v
		}
	}
	for c, v := range stocktake.Cold {
		if keep(c) {
			out.Cold[c] = v
		}
	}
	counterLoad, err := s.loadCounterLoadKgByCut(ctx, branchID, bizDate, cutOfSku)
	if err != nil {
		return nil, err
	}

	// 只在引擎要出行的部位上算(其余部位不进这张表)。
	recorded = pickCutMap(recorded, keep)
	soldKg = pickCutMap(soldKg, keep)
	bulkKg = pickCutMap(bulkKg, keep)
	wasteKg = pickCutMap(wasteKg, keep)
	openKg = pickCutMap(openKg, keep)
	counterLoad = pickCutMap(counterLoad, keep)
	// ⚠ 搬上台的量**单独存一份**:毛利看板的「搬上台」列要用它
	//   (它是猪肉台口径的数,不是账面链值)。
	out.CounterLoad = counterLoad
	actual, source := deriveActualSplits(recorded, out.HasStocktake,
		soldKg, bulkKg, wasteKg, out.CounterActual, out.Cold, openKg, counterLoad)
	out.Actual = actual
	out.Source = source

	weights, note := redistributeSplitWeights(out.Suggested, out.Actual)
	out.Adjusted = weights
	out.Note = note
	if note != "" {
		s.pubLogger.Warn("computeCutSplit: 分割权重告警",
			"branch_id", branchID,
			"biz_date", bizDate.In(s.bizTZOrUTC()).Format("2006-01-02"),
			"note", note)
	}
	return out, nil
}

// pickCutMap 只保留 keep 认可的部位。
func pickCutMap(in map[string]decimal.Decimal, keep func(string) bool) map[string]decimal.Decimal {
	out := make(map[string]decimal.Decimal, len(in))
	for c, v := range in {
		if keep(c) {
			out[c] = v
		}
	}
	return out
}

// applySplitWeights 用**调整后权重 W** 重算当日的入库量与入库金额。
//
// # 口径
//
//	入库量   prodK_c = W_c
//	入库金额 prodA_c = allocatable × W_c / ΣW
//	  (allocatable = Σ 当日可摊猪成本 ⇒ **当日总摊销不变**)
//
// ⚠ **不能按原比例同比缩放**:量与金额同比缩放 ⇒ 单价不变 ⇒ 毛利不变(等于没做)。
//
//	金额必须按 W 的**比例**重分 ⇒ 比例变了单价才会变。
//
// ⚠ ΣW == 0(没有建议分割 ⇒ 守卫①)⇒ **完全不接管**:入库量/金额保持今天的值。
//
// ⚠ 返回的 kg/amt 的键 = 入库量 ∪ 入库金额 ∪ W 的键 —— 调用方必须保证这些部位都有行。
func applySplitWeights(split *cutSplitWeights, producedKg, producedAmount map[string]decimal.Decimal) (
	map[string]decimal.Decimal, map[string]decimal.Decimal) {
	kg := map[string]decimal.Decimal{}
	amt := map[string]decimal.Decimal{}
	for c, v := range producedKg {
		kg[c] = v
	}
	for c, v := range producedAmount {
		amt[c] = v
	}
	var totalW, allocatable decimal.Decimal
	for _, v := range split.Adjusted {
		totalW = totalW.Add(v)
	}
	for _, v := range producedAmount {
		allocatable = allocatable.Add(v)
	}
	// 守卫①:没有建议分割 ⇒ 与今天**逐字一致**(不接管入库量/金额)。
	if !totalW.IsPositive() {
		return kg, amt
	}
	all := map[string]struct{}{}
	for c := range kg {
		all[c] = struct{}{}
	}
	for c := range amt {
		all[c] = struct{}{}
	}
	for c := range split.Adjusted {
		all[c] = struct{}{}
	}
	for c := range all {
		w := split.Adjusted[c]
		if !w.IsPositive() {
			kg[c] = decimal.Zero
			amt[c] = decimal.Zero
			continue
		}
		kg[c] = w
		amt[c] = allocatable.Mul(w).Div(totalW)
	}
	return kg, amt
}
