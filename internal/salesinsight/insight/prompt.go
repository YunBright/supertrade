package insight

import (
	"fmt"
	"strings"

	"github.com/YunBright/supertrade/internal/salesinsight/fact"
)

// SystemPrompt 是 system prompt。
//
// # 它承担三件事，缺一不可
//
//  1. **角色与读者**：写给谁看。写"给店长"而不是"分析数据"，
//     模型的输出风格会完全不同 —— 前者给建议，后者给报告。
//  2. **硬约束**：不许改数字、不许编数据。这是铁律一。
//  3. **输出格式**：严格 JSON，无 markdown。
//
// ⚠️ 约束必须写成**可验证的禁令**（"不要重新计算任何数字"），
//   而不是含糊的期望（"请基于数据"）。后者模型会照做，
//   但它同样会"顺手"把 15929.65 四舍五入成 16000，
//   而店长会以为报表口径变了。
const SystemPrompt = `你是一名资深零售门店运营顾问，正在为**单店店长**写一份销售简报。

读者是店长本人，他每天早上要据此决定：今天补什么货、下什么架、找哪个供应商。

## 硬性约束（违反其中任何一条都算失败）

1. **绝对不要重新计算、修改或估算任何数字。** 所有金额、占比、环比都已算好，直接引用。
   不要做四舍五入到"整百/整千"的处理，不要换算成"万元"（除非原文已是万元）。
2. **不要编造数据里没有的信息。** 如果某个判断依据不足，就不要说那个判断。
3. **不要使用"可能""也许""大概"来修饰你确定的事实** —— 依据充分时直接说。
4. **缺货就是缺货，不是卖不动。** 事实包里已标注"需要补货"的商品，它们卖不出去是因为没货，
   你必须表述为"缺货导致未销售"，绝不能说成"销量下滑"或"滞销"。这个区分对店长最重要。
5. 若毛利被标注为不可计算，**不要给出任何毛利率数字**，也不要说"毛利表现良好"。
6. **不要报告全店销售总额与总毛利的具体金额。** 只给**百分比**（环比、占比、毛利率）
   与**细分项**的金额（品类、商品、供应商）。事实包不会给你全店金额，
   你也**不要**把细分项相加去凑一个总数 —— 那是估算，不是数据。

## 输出要求

- summary：一句话，说清今天最值得店长知道的一件事。不超过 40 字。
- highlights：3~5 条，每条说明一个现象及其影响。每条不超过 30 字。
- actions：1~3 条，每条是**今天可执行**的动作（如"补货 X""观察 Y 两天再决定"）。不超过 25 字。
- warnings：0~3 条，仅在有数据质量或可比性问题时给出。没有就给空数组。

## 输出格式

只输出一个 JSON 对象，不要 markdown 代码块，不要任何解释文字：

{"summary":"...","highlights":["..."],"actions":["..."],"warnings":["..."]}`

// BuildUserPrompt 拼 user prompt。
//
// ⚠️ 上期结论摘要是**可选**注入的，且必须带硬上限：
//
//	用户明确要求"新的一天只把结论放入新一轮推理的上下文，
//	而不是上一轮上下文直接搬过来"，理由是月报环比不会撑爆上下文。
//
//	本实现把摘要单独存在 state，每次只取上一期的结论文本，
//	长度由 MaxPrevSummaryChars 强制截断 —— 于是上下文大小
//	**与历史深度无关**，跑一年和跑一天一样大。
func BuildUserPrompt(f fact.Fact, prevSummary *PrevSummary) string {
	var b strings.Builder

	b.WriteString(FactDigest(f))
	b.WriteString("\n\n")

	// ---- 上期结论（可选）----
	//
	// ⚠️ 没有上期摘要时**明确告诉模型**，而不是默默省略 ——
	// 模型看到"环比 -15%"会自己编一个"相比上期下滑"的故事，
	// 而它并不知道有没有上期可参照。
	if prevSummary == nil || strings.TrimSpace(prevSummary.Summary) == "" {
		b.WriteString("【上期情况】\n没有上一期的结论记录。请勿编造上期情况，")
		b.WriteString("只依据上面的本期数据与环比数字作答。\n")
	} else {
		b.WriteString("【上期结论（供延续性叙述）】\n")
		if s := strings.TrimSpace(prevSummary.Summary); s != "" {
			b.WriteString("- 上期要点：" + truncate(s, MaxSummaryChars) + "\n")
		}
		for _, h := range prevSummary.Highlights {
			if s := strings.TrimSpace(h); s != "" {
				b.WriteString("- " + truncate(s, MaxHighlightChars) + "\n")
				if len(prevSummary.Highlights) >= 4 {
					break
				}
			}
		}
		b.WriteString("（仅供理解上期情况，**不要**照抄，也不要与之对比数字）\n")
	}

	b.WriteString("\n请按 system 要求的 JSON 格式输出。")
	return b.String()
}

// PrevSummaryMatches 判断上期摘要描述的区间，是否就是本期的「上期」。
//
// # 为什么必须校验区间（而不是只校验周期类型）
//
// 上期摘要是**按周期类型**存的（aireport.PrevKey 含 kind），
// 看起来"周报只会拿到周报的上一期"。但 2026-10-23 生产实测推翻了它：
//
//	周报正文出现「上期结论写销售额60585.35元，与本期引用的118653.51元不一致」
//
// 真相是：那份"上一期周报"是在**周期口径改动之前**生成的
// （改口径前周报算的是别的区间），它与本期的"上期"根本不是同一段。
// **类型相同 ≠ 区间相同** —— 这正是周报/月报从"未实现"变成"已实现"时
// 才会暴露的跨期问题：类型对了，区间没对。
//
// # 判据
//
//	摘要的 from/to 必须**逐字等于**事实包的 PrevFrom/PrevTo。
//	调用方没给区间（from/to 为空）时**不拦** —— 不猜、不误伤。
//
// ⚠️ 不匹配时的处置是**整份丢弃**（当作"没有上期"），
//   而不是"照用但提醒一句"：模型拿到两份口径不同的数字，
//   必然会自己编一个对比，而那个对比是错的。
func PrevSummaryMatches(f fact.Fact, p *PrevSummary) bool {
	if p == nil {
		return true
	}
	from, to := strings.TrimSpace(p.From), strings.TrimSpace(p.To)
	if from == "" || to == "" {
		return true // 调用方没给区间 → 信任调用方
	}
	return from == f.Meta.PrevFrom && to == f.Meta.PrevTo
}

// MaxPrevSummaryChars 是注入的上期摘要长度上限。
//
// 取 600 的理由：足够装"上期结论 + 3 条要点"，
// 而这已经能让模型理解上期发生过什么；再多只会增加成本，
// 且长文本里模型更容易把细节记混（把上期的事说成这期的）。
const MaxPrevSummaryChars = 600

// PrevSummary 是上一期结论的摘要。
type PrevSummary struct {
	PeriodKind string   `json:"period_kind"`
	From       string   `json:"from"`
	To         string   `json:"to"`
	Summary    string   `json:"summary"`
	Highlights []string `json:"highlights,omitempty"`
}

// FromInsight 从一份结论抽出上期摘要（供下一期注入）。
//
// # 为什么只保留 Summary + Highlights，丢掉 Actions/Warnings
//
// Actions 是"该做什么"，是**本期特有**的；把上期的动作带进来会让
// 模型重复建议昨天已经做过的事。
// Warnings 是数据质量提示，与本期数据无关，保留只会增加噪声。
//
// ⚠️ 这是"只注入结论、不搬运上下文"的具体落实：
//   跨期传递的是**判断**，不是**过程**。
func FromInsight(kind, from, to string, in Insight) *PrevSummary {
	s := PrevSummary{
		PeriodKind: kind,
		From:       from,
		To:         to,
		Summary:    truncate(in.Summary, MaxSummaryChars),
	}
	for i, h := range in.Highlights {
		if i >= 3 {
			break
		}
		if t := strings.TrimSpace(h); t != "" {
			s.Highlights = append(s.Highlights, truncate(t, MaxHighlightChars))
		}
	}
	if strings.TrimSpace(s.Summary) == "" && len(s.Highlights) == 0 {
		return nil // 上期没有可用结论，不注入
	}
	return &s
}

// RuleInsightsFromFact 在**不调用 LLM** 的情况下产出结论。
//
// # 这不是"失败时的兜底"，它是一个独立的产品形态
//
// 它的价值在于：日报 60% 的价值是数据（排行、环比、缺货清单），
// 而这部分**不需要任何 AI**。LLM 挂了不该让店长早上看不到该补什么货。
//
// 措辞刻意写得"像人话但更机械" —— 它不该假装自己是 AI 推理，
// 因为 GeneratedBy=rules 会明确标注，卡片上也会显示"规则模式"。
func RuleInsightsFromFact(f fact.Fact, degradedReason string) Insight {
	in := Insight{
		GeneratedBy:    ByRules,
		DegradedReason: degradedReason,
	}
	if degradedReason != "" {
		in.Warnings = append(in.Warnings, "AI 解读不可用："+truncate(degradedReason, MaxHighlightChars))
	}

	// ---- summary：本期最重要的一件事 ----
	in.Summary = ruleSummary(f)

	// ---- highlights：现象 ----
	for _, n := range f.RuleInsights.StockoutProducts {
		in.Highlights = append(in.Highlights, n+" 缺货，未卖出部分销量")
	}
	for _, n := range f.RuleInsights.FastMovers {
		in.Highlights = append(in.Highlights, n+" 销量增长明显")
	}
	for _, n := range f.RuleInsights.GrowingCategories {
		in.Highlights = append(in.Highlights, "品类 "+n+" 销售额上升")
	}
	for _, n := range f.RuleInsights.DecliningCategories {
		in.Highlights = append(in.Highlights, "品类 "+n+" 销售额下降")
	}
	if len(in.Highlights) == 0 {
		in.Highlights = append(in.Highlights, "本期无显著异常，按常规经营即可")
	}

	// ---- actions：今天该做什么 ----
	//
	// ⚠️ 动作必须来自**已判定的数据**，不能来自推测。
	//   缺货清单来自库存实况；"观察"类建议只在上期结论可比时给。
	for _, s := range f.Stockouts {
		in.Actions = append(in.Actions, "补货 "+s.Name)
	}
	if f.Context.Comparability.Kind == "limited" {
		in.Actions = append(in.Actions, "本期与上期不可直接比较，暂缓据此下架")
	}

	in.Sanitize()
	// ⚠️ 规则版同样不许出现全店总额（见 RedactStoreTotals 的三道防线）。
	//   LLM 挂掉时走的是这条路，而"降级"不该意味着口径变松。
	RedactStoreTotals(f, &in)
	return in
}

// ruleSummary 生成规则版的一句话总结。
//
// # 缺货信息与可比性警告**都要**出现在摘要里
//
// ⚠️ 这是一个真实的取舍点，测试已经钉死它：
//   最初的实现是"缺货优先，摘要只说缺货"，结果可比性提示被吞掉了。
//   但店长看到的"环比 -20%"会被当成经营结论 ——
//   而当天可能恰好是假期结束后的第一个工作日，那个 -20% 是日历造成的。
//   两者都必须说：缺货要马上处理，环比先别当真。
//
// 做法是**先说缺货（要立刻行动的事），再补一句可比性（读数据时的注意点）**，
// 而不是二选一。摘要有长度上限，两句都写得尽量短。
func ruleSummary(f fact.Fact) string {
	var b strings.Builder

	if n := len(f.Stockouts); n > 0 {
		b.WriteString(fmt.Sprintf("有 %d 个商品缺货", n))
		if names := f.RuleInsights.StockoutProducts; len(names) > 0 {
			b.WriteString("（" + joinCN(names) + "）")
		}
		b.WriteString("，建议优先补货")
	} else if f.Deltas.HasPrev {
		// ⚠ **只报百分比，不报全店销售额**（口径要求见 RedactStoreTotals）。
		//
		//	降级版不是"放松版"：LLM 挂掉时这张卡片照样要守同一条口径。
		//	店长在「每日销售报表」里已经能看到全店金额，重复一遍没有增量价值，
		//	却会让群里的汇报变成一张只有大数字的流水账。
		b.WriteString(fmt.Sprintf("销售额环比 %s", fmtPct(f.Deltas.AmountPct)))
	} else {
		// ⚠ 无上期数据时**不能出现"环比"二字**（它会暗示存在可比对象）。
		b.WriteString("本期无上期数据，销售情况待对比")
	}

	// 可比性提示必须**独立追加**，不能只在"没有缺货"分支里出现。
	if f.Context.Comparability.Kind == "limited" {
		b.WriteString("；与上期不可直接比较")
	}
	return truncate(b.String(), MaxSummaryChars)
}
