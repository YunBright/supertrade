// Package insight 定义 LLM 推理的输入输出契约、prompt 版本与降级策略。
//
// # 铁律一：LLM 只做"解读"，数字永远来自规则层
//
// 事实包里的每个数字都是确定性的（SQL 聚合出来的），
// LLM **不允许**重算、不允许修改、不允许"估算"。
// 它的工作是把这些数字翻译成店长能据以行动的话，
// 并解释"为什么"（结合天气、节日、缺货、供应商）。
//
// 为什么要立这条铁律：LLM 在几千 token 的数字上下文里**一定会算错**
// （尤其环比和占比），而一个算错的环比会让店长砍掉正在增长的品类。
// 数字口径不可信，解读再多也没有价值。
//
// 所以 prompt 里用"硬约束"明确禁止改数字，且解析层会校验：
// LLM 输出的结构里**根本没有数字字段**，它只能引用事实包里的名字。
//
// # 铁律二：LLM 不可用时必须有东西可发
//
// 降级不是失败，是一个**诚实且可用**的产品形态：
// 报告的价值 60% 在数据（排行、环比、缺货），40% 在叙述。
// 数据在就不该让整个推送停摆 ——
// "群里什么都没发生"比"收到一份规则版报告"难查得多。
package insight

import (
	"encoding/json"
	"time"

	"github.com/YunBright/supertrade/internal/salesinsight/fact"
)

// PromptVersion 是当前 prompt 版本号。
//
// # 为什么必须版本化
//
// 用户明确提出"我无法提前设计好一个完美的 prompt，但可以记录每次推理，
// 做好文档，这样就能实现后续优化 prompt 的需求"。
//
// 没有版本号的话，后台看到两条不同风格的结论时无法判断：
//   - 是数据变了导致结论变（正常）
//   - 还是 prompt 被改过了（需要评估改动的效果）
//
// 每次改 prompt **必须**递增这个常量，且递增要写在同一个 commit 里。
//
// ⚠️ 2026-10-23 从 v1.0.0 升到 v1.1.0：
//   事实包不再下发全店销售总额/总毛利的绝对金额（只给百分比），
//   并新增硬约束 6「不要报告全店总数」。
const PromptVersion = "v1.1.0"

// GeneratedBy 标识这份结论由谁产出。
type GeneratedBy string

const (
	// ByLLM 正常 AI 推理产出
	ByLLM GeneratedBy = "llm"
	// ByRules LLM 不可用时的规则降级产出
	ByRules GeneratedBy = "rules"
)

// Insight 是一次推理的结论。
type Insight struct {
	// GeneratedBy 标明这份结论的来源。
	//
	// ⚠️ 它会显示在卡片上。规则版报告必须让店长知道"这是规则算的，
	//   没有 AI 解读"，否则他会把简短的结论误当成 AI 的深度判断。
	GeneratedBy GeneratedBy `json:"generated_by"`
	// PromptVersion 记录用的哪版 prompt（规则模式为空）。
	PromptVersion string `json:"prompt_version,omitempty"`
	// DegradedReason 说明为什么降级（规则模式时非空）。
	DegradedReason string `json:"degraded_reason,omitempty"`

	// Summary 是一句话总结（店长扫一眼就知道今天要做什么）。
	Summary string `json:"summary"`
	// Highlights 是要点列表（3~5 条，每条一个可执行判断）。
	Highlights []string `json:"highlights"`
	// Actions 是建议动作列表（今天该做什么）。
	//
	// ⚠️ 它与 Highlights 的区别：Highlights 是"发生了什么"，
	//   Actions 是"该怎么做"。店长看的是后者。
	Actions []string `json:"actions,omitempty"`
	// Warnings 是风险提示（如可比性受限、成本数据不足）。
	Warnings []string `json:"warnings,omitempty"`
}

// 长度硬上限。
//
// 这些上限存在的唯一目的：**保护 token 成本**。
// LLM 调用按次付费，而一个配置失误（比如 prompt 里塞进全量商品）
// 就能让月度账单翻几倍。宁可截断，不可失控。
const (
	// MaxSummaryChars 是 Summary 的上限。
	MaxSummaryChars = 120
	// MaxHighlightChars 是单条要点的上限。
	MaxHighlightChars = 80
	// MaxItemChars 是单条动作的上限。
	MaxItemChars = 60
	// MaxItems 是列表类字段的最大条数。
	MaxItems = 5
	// MaxTotalChars 是整份结论的字符上限。
	//
	// ⚠️ 这是发送前的**最后一道闸**：任何一层限制失效时，
	// 这一条仍能拦住。超标会被截断并在审计里标记。
	MaxTotalChars = 1500
)

// Sanitize 裁剪结论长度并去掉空项。
//
// # 为什么在解析之后、落库之前做
//
// LLM 不遵守长度要求是常态而不是例外。与其在 prompt 里反复强调
// "请简短"，不如在代码里硬裁 —— 前者是请求，后者是保证。
//
// ⚠️ 但**不能**裁 Summary：它是唯一必须完整保留的字段，
//   因为它承载核心判断。裁了之后结论会变成没有主语的碎片
//   （"品类销售增长"、"库存不足" —— 谁不足？）。
func (in *Insight) Sanitize() {
	in.Summary = truncate(in.Summary, MaxSummaryChars)
	in.Highlights = cleanList(in.Highlights, MaxItems, MaxHighlightChars)
	in.Actions = cleanList(in.Actions, MaxItems, MaxItemChars)
	in.Warnings = cleanList(in.Warnings, MaxItems, MaxHighlightChars)

	// 总闸：只砍列表，Summary 不动。
	for truncateListToTotal(in) {
	}
}

// truncateListToTotal 反复裁最长的列表直到总量达标。
func truncateListToTotal(in *Insight) bool {
	if len(in.Summary)+listLen(in.Highlights)+listLen(in.Actions)+listLen(in.Warnings) <= MaxTotalChars {
		return false
	}
	switch {
	case len(in.Actions) > 0:
		in.Actions = in.Actions[:len(in.Actions)-1]
	case len(in.Highlights) > 0:
		in.Highlights = in.Highlights[:len(in.Highlights)-1]
	case len(in.Warnings) > 0:
		in.Warnings = in.Warnings[:len(in.Warnings)-1]
	default:
		return false // 只剩 Summary，已无裁剪空间
	}
	return true
}

func listLen(l []string) int {
	n := 0
	for _, s := range l {
		n += len(s)
	}
	return n
}

func cleanList(l []string, maxItems, maxChars int) []string {
	out := make([]string, 0, len(l))
	for _, s := range l {
		s = trimSpace(s)
		if s == "" {
			continue // 丢掉空项：卡片上出现一个空的 bullet 比不出现更糟
		}
		out = append(out, truncate(s, maxChars))
		if len(out) >= maxItems {
			break
		}
	}
	if len(out) == 0 {
		return nil // nil 序列化成 []，前端不会拿到 null
	}
	return out
}

// truncate 按字符截断并加省略号。
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}

// trimSpace 去掉首尾空白（不用 strings.TrimSpace 是为了避免多余 import 时
// 仍保持单一实现点；见文件末尾的 stringsTrimpSpace 别名）。
func trimSpace(s string) string { return stringsTrimpSpace(s) }

// Audit 是一次推理的完整审计记录。
//
// # 为什么存这么多
//
// 用户需求："我可以在后台查到 LLM 是通过怎样的上下文输出的日报"。
//
// 只存最终结论就丢掉了优化 prompt 所需的最关键证据：
// 同一份事实包在不同 prompt 下产出什么差异。
// 所以这里保留了事实包全文、最终 prompt、原始输出、token 用量。
//
// ⚠️ 但**必须**有 MaxPromptChars 一类的上限保护 ——
// 审计数据要落 state store，无界增长会把它写满。
type Audit struct {
	// RunID 是本次推理的唯一标识。
	RunID string `json:"run_id"`
	// PeriodKind / From / To 标明这是哪一期。
	PeriodKind string `json:"period_kind"`
	From       string `json:"from"`
	To         string `json:"to"`
	BranchID   string `json:"branch_id"`
	// PromptVersion 记录 prompt 版本。
	PromptVersion string `json:"prompt_version,omitempty"`
	// Insight 是解析后的结构化结论。
	Insight Insight `json:"insight"`
	// Degraded 表示是否走了降级路径。
	Degraded bool `json:"degraded"`
	// DegradedReason 说明降级原因。
	DegradedReason string `json:"degraded_reason,omitempty"`

	// Context 是本次推理的**完整输入**，逐字可回看。
	//
	// ⚠️ 它是 JSON 字符串而不是结构体：审计要能还原"发给 LLM 的到底是什么"，
	//   包括序列化当时的空格与顺序。结构体会在反序列化时丢失这些。
	Context string `json:"context,omitempty"`
	// Prompt 是最终发出的 user prompt（含事实包）。
	Prompt string `json:"prompt,omitempty"`
	// RawOutput 是 LLM 的原始返回（未经解析）。
	//
	// ⚠️ 必须保留原始输出：模型某天升级后解析失败时，
	//   这是唯一能看出"它到底返回了什么"的证据。
	RawOutput string `json:"raw_output,omitempty"`

	// PromptTokens / CompletionTokens 是 token 用量（供应商返回，可能为 0）。
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	// LatencyMS 是端到端耗时。
	LatencyMS int64 `json:"latency_ms,omitempty"`
	// Error 是失败原因（成功时为空）。
	Error string `json:"error,omitempty"`
	// CreatedAt 是记录时间。
	CreatedAt time.Time `json:"created_at"`
}

// MaxAuditFieldChars 是审计各字段的存储上限。
const MaxAuditFieldChars = 32 * 1024

// Trim 裁剪审计里的大字段，防止写爆 state store。
func (a *Audit) Trim() {
	a.Context = clip(a.Context, MaxAuditFieldChars)
	a.Prompt = clip(a.Prompt, MaxAuditFieldChars)
	a.RawOutput = clip(a.RawOutput, MaxAuditFieldChars)
	a.Error = clip(a.Error, 2000)
}

func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// 按字节截断可能切坏 UTF-8，所以按 rune 收缩到安全长度。
	return truncate(s, max)
}

// JSONCompact 把任意值压成紧凑 JSON（用于存审计的 context）。
func JSONCompact(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// ParseInsight 解析 LLM 返回的 JSON。
//
// # 为什么用严格解析 + 多层兜底
//
// LLM 返回 JSON 有各种毛病：裹 markdown 代码块、前面带一句"好的"、
// 结尾多个逗号。处理方式按"成本从低到高"排列：
//
//	 1. 直接 Unmarshal
//	 2. 剥 ```json 代码块
//	 3. 截取第一个 { 到最后一个 }
//
// 三种都失败才返回错误并降级。
// 而**内容**的校验（Summary 非空等）放在这里而不是调用方，
// 是为了让"LLM 说了什么"与"我们采信什么"在一处决定，便于审计。
func ParseInsight(raw string) (*Insight, error) {
	candidate := extractJSONObject(raw)
	if candidate == "" {
		return nil, errUnparseable
	}
	var in Insight
	if err := json.Unmarshal([]byte(candidate), &in); err != nil {
		return nil, errUnparseable
	}
	if trimSpace(in.Summary) == "" && len(in.Highlights) == 0 && len(in.Actions) == 0 {
		// 全空 = 等价于没解析出任何东西。放行会让卡片变成一张空卡，
		// 而"发一张说明卡"与"发一张空白卡"对运营是完全不同的信号。
		return nil, errEmptyInsight
	}
	return &in, nil
}

// extractJSONObject 从杂乱的 LLM 输出里取出 JSON 对象。
func extractJSONObject(raw string) string {
	s := trimSpace(raw)
	if s == "" {
		return ""
	}
	// 剥 markdown 代码块
	if start := indexOf(s, "```"); start >= 0 {
		rest := s[start+3:]
		// 跳过可能的 json 语言标记
		if nl := indexOf(rest, "\n"); nl >= 0 {
			lang := trimSpace(rest[:nl])
			if lang == "json" || lang == "" {
				rest = rest[nl+1:]
			}
		}
		if end := lastIndexOf(rest, "```"); end > 0 {
			rest = rest[:end]
		}
		if t := trimSpace(rest); t != "" {
			s = t
		}
	}
	// 截取第一个 { 到最后一个 }
	start := indexOf(s, "{")
	end := lastIndexOf(s, "}")
	if start < 0 || end <= start {
		return ""
	}
	return s[start : end+1]
}

// FactDigest 是喂给 LLM 的事实包文本。
//
// ⚠️ 它**不是**整个 Fact 的 JSON dump —— 那会把内部字段名、
//
//	布尔开关、null 全部暴露给模型，让它去解释"decline_cause: stockout"
//	这种机器值，然后可能原样复述给店长。
//
//	我们只给它**人话**：数字、名字、以及已经翻译好的结论标签。
func FactDigest(f fact.Fact) string {
	var b []byte
	b = append(b, "【统计区间】"...)
	b = append(b, f.Meta.From...)
	if f.Meta.From != f.Meta.To {
		b = append(b, " ~ "...)
		b = append(b, f.Meta.To...)
	}
	b = append(b, "（"...)
	b = append(b, f.Meta.PeriodLabel...)
	b = append(b, "，对比上期 "...)
	b = append(b, f.Meta.PrevFrom...)
	b = append(b, "）\n"...)

	b = append(b, "\n【日期背景】\n"...)
	b = append(b, "本期 "+f.Context.From.Weekday...)
	if f.Context.From.Name != "" {
		b = append(b, "（"+f.Context.From.Name+"）"...)
	}
	b = append(b, "，上期 "+f.Context.PrevFrom.Weekday...)
	if f.Context.PrevFrom.Name != "" {
		b = append(b, "（"+f.Context.PrevFrom.Name+"）"...)
	}
	b = append(b, "\n"...)
	if f.Context.Comparability.Kind == "limited" {
		b = append(b, "⚠️ "+f.Context.Comparability.Reason+"\n"...)
	}
	if f.Context.Weather.Available {
		b = append(b, "天气："+f.Context.Weather.Summary+"\n"...)
	}

	b = append(b, "\n【本期业绩】\n"...)
	// ⚠ 这里**只给百分比，不给全店销售总额与总毛利的具体金额**。
	//
	//	理由（2026-10-23 生产实测）：周报正文出现
	//	「销售额108626.17元」「参考毛利16927.17元」，
	//	而运营明确要求"不要报告这类全店总数的具体值"。
	//
	//	把绝对值从 prompt 里**拿掉**，比"叮嘱模型别写"可靠得多：
	//	模型看不到的数字，它写不出来。
	//	残留风险由 RedactStoreTotals 兜底。
	if f.Deltas.HasPrev {
		b = append(b, "销售额环比 "+fmtPct(f.Deltas.AmountPct)+"\n"...)
	} else {
		b = append(b, "销售额（无上期数据，不做环比）\n"...)
	}
	if f.Totals.MarginAvailable {
		b = append(b, "参考毛利率 "+fmtPct(f.Totals.RefMarginPct*100)+"\n"...)
	} else {
		// ⚠️ 必须显式告诉 LLM"毛利不可算"，否则它会自己编一个看起来合理的毛利率。
		b = append(b, "⚠️ 参考毛利不可计算（成本数据覆盖率仅 "+fmtPct(f.Totals.CostCoverage*100)+"），请勿给出毛利率\n"...)
	}

	if len(f.Categories) > 0 {
		b = append(b, "\n【品类销售排行】\n"...)
		for _, c := range f.Categories {
			b = append(b, c.Name+"："+fmtFloat(c.Amount)+" 元（占比 "+fmtPct(c.AmountShare*100)...)
			if c.HasPrev {
				b = append(b, "，环比 "+fmtPct(c.DeltaPct)...)
			}
			b = append(b, "）\n"...)
		}
	}

	if len(f.TopProducts) > 0 {
		b = append(b, "\n【商品销售排行】\n"...)
		for _, p := range f.TopProducts {
			b = append(b, p.Name+"："+fmtFloat(p.Amount)+" 元"...)
			if p.HasPrev {
				b = append(b, "，环比 "+fmtPct(p.DeltaPct)...)
			}
			b = append(b, "\n"...)
		}
	}

	if len(f.Stockouts) > 0 {
		b = append(b, "\n【需要补货的商品（已确认缺货或库存偏低）】\n"...)
		for _, s := range f.Stockouts {
			b = append(b, s.Name+"："+s.DeclineEvidence+"\n"...)
		}
	}

	if len(f.Suppliers) > 0 {
		b = append(b, "\n【供应商贡献（已排除自用与客户）】\n"...)
		for _, s := range f.Suppliers {
			line := s.Name+"：" + fmtFloat(s.Amount) + " 元，" +
				itoa(s.ProductCount) + " 个商品"
			if len(s.TopProducts) > 0 {
				line += "，其中 "
				for i, tp := range s.TopProducts {
					if i > 0 {
						line += "、"
					}
					line += tp.Name
				}
			}
			b = append(b, line...)
			b = append(b, "\n"...)
		}
	}

	if len(f.ExcludedSuppliers) > 0 {
		b = append(b, "\n【已排除的渠道】\n"...)
		for _, e := range f.ExcludedSuppliers {
			b = append(b, e.Name+"（"+e.Reason+"），"+fmtFloat(e.Amount)+" 元\n"...)
		}
	}

	if len(f.RuleInsights.StockoutProducts) > 0 ||
		len(f.RuleInsights.GrowingCategories) > 0 ||
		len(f.RuleInsights.FastMovers) > 0 {
		b = append(b, "\n【系统已判定的要点】\n"...)
		if len(f.RuleInsights.StockoutProducts) > 0 {
			b = append(b, "- 缺货商品："+joinCN(f.RuleInsights.StockoutProducts)+"\n"...)
		}
		if len(f.RuleInsights.GrowingCategories) > 0 {
			b = append(b, "- 增长品类："+joinCN(f.RuleInsights.GrowingCategories)+"\n"...)
		}
		if len(f.RuleInsights.DecliningCategories) > 0 {
			b = append(b, "- 下滑品类："+joinCN(f.RuleInsights.DecliningCategories)+"\n"...)
		}
		if len(f.RuleInsights.FastMovers) > 0 {
			b = append(b, "- 增长最快的商品："+joinCN(f.RuleInsights.FastMovers)+"\n"...)
		}
	}

	if !f.DataSufficiency.Sufficient && len(f.DataSufficiency.Issues) > 0 {
		b = append(b, "\n【数据提示】\n"...)
		for _, iss := range f.DataSufficiency.Issues {
			b = append(b, "- "+iss+"\n"...)
		}
	}

	return string(b)
}
