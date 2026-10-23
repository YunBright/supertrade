package insight

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/YunBright/supertrade/internal/salesinsight/fact"
)

// 本文件是「结论里不得出现全店级绝对金额」的**确定性闸门**。
//
// # 为什么需要它（prompt 叮嘱是不够的）
//
// 运营明确要求（2026-10-23）：「不要报告这类全店总数的具体值，
// 可保留百分比，可保留小类目统计值」。
//
// 生产实测当天，**月报**正文没有全店总额，**周报**却有：
//
//	> 销售额108626.17元，环比-8.5%
//	> 参考毛利16927.17元，毛利率15.6%
//
// 同一套 prompt、同一套规则，一次遵守一次不遵守 ——
// 这说明「在 prompt 里写清楚」只能提高概率，**不能保证**。
//
// # 三道防线（缺一不可）
//
//  1. **不把绝对值放进 prompt**（见 FactDigest）：
//     模型看不到的数字，它写不出来。这是最强的一道。
//  2. **规则降级路径也不写**（见 RuleInsightsFromFact）：
//     LLM 挂掉时走的那条路，同样不许出现全店总额。
//  3. **本文件的清洗闸**：对 LLM 的自由文本做**值级**判定。
//
// # 清洗闸的判定顺序（顺序本身就是设计）
//
//	① 命中「小类目金额白名单」 → **保留**
//	   （品类/商品/供应商/已排除渠道的金额 —— 用户明确允许保留）
//	② 命中「全店金额黑名单」 → **抹掉**
//	   （本期销售额、上期销售额、总毛利）
//	③ 既不在名单里、前面又紧跟着「销售额/毛利」这类词 → **抹掉**
//	   （兜住"上期遗留摘要"里的旧总额：它不在本期黑名单里）
//	④ 其余 → 保留（百分比、日期、排名、商品名里的数字……）
//
// ③ 为什么必要：黑名单只能覆盖**本期**的总额，
// 而上一期存下来的摘要里可能有**更早**的总额（口径改过就会发生）。
// 词 + 紧跟数字的判定能把它兜住，
// 且**不会**误伤「毛利率15.6%」——"率"挡在中间，它不是金额。

// RedactedMark 是被抹掉的金额留下的占位符。
//
// ⚠️ 用「—」而不是空串或「0」：
//
//   - 空串会让句子变成"销售额，环比-8.5%"，读起来像漏字；
//   - "0"是一个**假的数**（店长会以为本期真的卖了 0 元）；
//   - "—"明确表示"这里有金额，但按口径不报"。
const RedactedMark = "—"

// storeTotalTermsAlways 是**永远**指全店的金额词：
// 它们后面跟的金额一律抹掉，即使那个数值碰巧等于某个品类金额。
//
// ⚠️ 为什么单独分一类：模型说"总销售额4162.4元"时，
//
//	哪怕 4162.4 恰好是"蛋类"的金额，这句话**仍然在报全店总额**
//	（而且报错了）。"总销售额"这三个字不会用来描述一个细分项。
var storeTotalTermsAlways = []string{
	"销售总额",
	"总销售额",
	"全店销售额",
	"全店营业额",
	"营业额",
	"总毛利",
	"毛利额",
}

// storeTotalTermsMaybe 是**可能**指全店、也可能指细分的词。
//
// "销售额"可以用来描述品类（"蛋类销售额4162.4元"），
// "毛利"在事实包里**只有全店口径**（没有品类级毛利），但保守起见
// 与"销售额"同级处理 —— 靠数值白名单区分。
//
// ⚠️ 判定条件是「这个词必须**紧跟**数字」，不是「出现这个词」：
//
//	"毛利率15.6%" —— "毛利"后面跟的是"率"，不是数字 ⇒ 不命中 ⇒ 百分比保留。
//	"销售额108626.17元" —— "销售额"后面紧跟数字 ⇒ 命中。
var storeTotalTermsMaybe = []string{
	"销售额",
	"毛利",
}

// numberRe 匹配文本里的数字。
//
// ⚠️ 允许千分位逗号：模型会把 108626.17 写成 108,626.17，
// 那时"值相同、字符串不同"，只按字符串比会漏掉。
var numberRe = regexp.MustCompile(`\d[\d,]*(?:\.\d+)?`)

// yuan 是「元」。金额文本里的单位用它做**字节级**匹配
// （len 取的是字节数，与 s 的下标一致）。
const yuan = "元"

// RedactStoreTotals 抹掉结论里的全店级绝对金额。
//
// ⚠️ 它**就地**修改 in，并返回被抹掉的原始数字（便于测试与排查）。
//
//	调用点必须覆盖**所有**产出结论的路径 ——
//	LLM 路径（engine.go）与规则降级路径（prompt.go 的 RuleInsightsFromFact）。
//	只改一条路径等于没改：LLM 挂掉时那张规则版卡片照样会带全店总额。
func RedactStoreTotals(f fact.Fact, in *Insight) []string {
	forbidden := forbiddenAmounts(f)
	allowed := allowedAmounts(f)

	var removed []string
	redact := func(s string) string {
		out, hit := redactText(s, forbidden, allowed)
		if hit {
			removed = append(removed, s)
		}
		return out
	}

	in.Summary = redact(in.Summary)
	for i := range in.Highlights {
		in.Highlights[i] = redact(in.Highlights[i])
	}
	for i := range in.Actions {
		in.Actions[i] = redact(in.Actions[i])
	}
	for i := range in.Warnings {
		in.Warnings[i] = redact(in.Warnings[i])
	}
	return removed
}

// forbiddenAmounts 是本期事实包里**全店级**的金额。
//
// ⚠️ 三个取舍：
//
//  1. **只收 > 0 的值**：RefMargin 在毛利不可算时是 0，
//     而 0 是文本里最常见的数字（"库存 0"），收进来会把整张卡片的 0 抹掉。
//  2. **RefMargin 只在毛利可算时收**：不可算时它根本不是数（见 fact.Totals 注释）。
//  3. **带上"取整"形态**：模型可能把 108626.17 写成 108626 或 108626.2，
//     那些也是同一个总额。
func forbiddenAmounts(f fact.Fact) map[float64]bool {
	out := map[float64]bool{}
	add := func(v float64) {
		if v <= 0 {
			return
		}
		out[v] = true
		out[roundTo(v, 1)] = true
		out[roundTo(v, 0)] = true
	}
	add(f.Totals.NetAmount)
	add(f.Deltas.PrevNetAmount)
	if f.Totals.MarginAvailable {
		add(f.Totals.RefMargin)
	}
	return out
}

// allowedAmounts 是**允许保留**的细分项金额（品类/商品/供应商/已排除渠道）。
//
// ⚠️ 白名单**优先**于黑名单：单品类门店里，品类金额恰好等于全店总额，
//
//	此时保留它 —— 用户明确允许保留"小类目统计值"，
//	而把它抹掉会让那张卡片一个金额都不剩。
//
//	例外：数字前面跟着"总销售额"这类**永远**指全店的词时，仍然抹掉
//	（那句话本身就是在报总额，哪怕数值恰好等于某个品类）。
func allowedAmounts(f fact.Fact) map[float64]bool {
	out := map[float64]bool{}
	add := func(v float64) {
		if v > 0 {
			out[v] = true
		}
	}
	for _, c := range f.Categories {
		add(c.Amount)
		add(c.PrevAmount)
	}
	for _, p := range f.TopProducts {
		add(p.Amount)
		add(p.PrevAmount)
	}
	for _, s := range f.Stockouts {
		add(s.Amount)
		add(s.PrevAmount)
	}
	for _, s := range f.Suppliers {
		add(s.Amount)
		add(s.PrevAmount)
		for _, tp := range s.TopProducts {
			add(tp.Amount)
			add(tp.PrevAmount)
		}
	}
	for _, e := range f.ExcludedSuppliers {
		add(e.Amount)
	}
	for _, sp := range f.RuleInsights.BestSupplierProducts {
		add(sp.Amount)
	}
	return out
}

// redactText 对一段文本做金额清洗。
//
// 它**从后往前**替换：否则前面的替换会让后面的下标失效。
func redactText(s string, forbidden, allowed map[float64]bool) (string, bool) {
	if s == "" {
		return s, false
	}
	locs := numberRe.FindAllStringIndex(s, -1)
	if len(locs) == 0 {
		return s, false
	}
	out := s
	changed := false
	for i := len(locs) - 1; i >= 0; i-- {
		start, end := locs[i][0], locs[i][1]

		if isPercent(s, end) || isDatePart(s, start, end) {
			continue
		}
		v, ok := parseAmount(s[start:end])
		if !ok {
			continue
		}
		before := s[:start]
		always := precededByTerm(before, storeTotalTermsAlways)
		if allowed[v] && !always {
			continue // ① 小类目金额：保留
		}
		hit := forbidden[v] // ② 本期/上期的全店总额
		if !hit {
			// ③ 词 + 紧跟数字（兜住"上期遗留摘要"里的旧总额：
			//    它不在本期黑名单里，只能靠词规则认出来）
			hit = always || precededByTerm(before, storeTotalTermsMaybe)
		}
		if !hit {
			continue // ④ 其余保留
		}
		out = out[:start] + RedactedMark + out[amountEnd(s, end):]
		changed = true
	}
	return out, changed
}

// isPercent 判断这个数字是不是百分比（"15.6%"）。
//
// ⚠️ 百分比是**要保留**的，不能参与金额判定 ——
//
//	否则"占比 15.6%"可能被当成某个总额抹掉。
func isPercent(s string, end int) bool {
	return end < len(s) && s[end] == '%'
}

// isDatePart 判断这个数字是不是日期/区间的一部分（"2026-10-15"）。
//
// ⚠️ 门店规模小的时候，销售额可能就是 "2026"，
//
//	而结论里出现日期是常事 —— 不挡掉就会把日期抹成"—-10-15"。
func isDatePart(s string, start, end int) bool {
	before := byte(' ')
	if start > 0 {
		before = s[start-1]
	}
	after := byte(' ')
	if end < len(s) {
		after = s[end]
	}
	return (before == '-' && start >= 2 && isDigit(s[start-2])) ||
		(after == '-' && end+1 < len(s) && isDigit(s[end+1]))
}

// precededByTerm 判断数字前面是否**紧跟**列表里的某个词。
func precededByTerm(before string, terms []string) bool {
	b := strings.TrimRight(before, " \t:：,，")
	for _, t := range terms {
		if strings.HasSuffix(b, t) {
			return true
		}
	}
	return false
}

// amountEnd 返回金额的结束下标：数字后面若跟着空格 + "元"，一并算进去。
//
// ⚠️ "元" 是多字节字符，不能按 byte 比较 —— 这里按 UTF-8 前缀判断。
func amountEnd(s string, end int) int {
	i := end
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	if i < len(s) && strings.HasPrefix(s[i:], yuan) {
		return i + len(yuan)
	}
	return end
}

// parseAmount 解析金额文本（允许千分位逗号）。
func parseAmount(tok string) (float64, bool) {
	clean := strings.ReplaceAll(tok, ",", "")
	v, err := strconv.ParseFloat(clean, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// roundTo 把金额四舍五入到指定小数位。
func roundTo(v float64, digits int) float64 {
	s := strconv.FormatFloat(v, 'f', digits, 64)
	out, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return v
	}
	return out
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }
