package insight

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"
	"unicode"
)

// newRunID 生成一次推理的唯一标识。
//
// # 为什么要随机 ID 而不是时间戳
//
// 时间戳 ID 在**同一分钟内跑两次**（例如手动重跑 + 定时触发撞车）
// 会产生相同 ID，两条审计记录互相覆盖 ——
// 而"手动重跑"恰恰是排查问题时的常见操作，
// 覆盖掉的正是最需要保留的那次推理。
func newRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand 失败极罕见；退回时间戳也比返回空 ID 好 ——
		// 空 ID 会让审计记录无法定位。
		return "r" + time.Now().Format("20060102150405.000")
	}
	return "r" + hex.EncodeToString(b[:])
}

// EstimateTokens 估算一段文本的 token 数。
//
// # 为什么需要估算
//
// ✅ 实测 ConverseAlpha1 不返回 Usage（见 engine.go 的 usageOf 注释），
//   所以后台的 token 栏对 AI 推理恒为 0。
//
// # 换算依据
//
// 中文场景的经验值：**1 个汉字 ≈ 1 token**（部分模型 1 汉字 ≈ 0.6~1.5 token）。
// 英文/数字约 4 字符 ≈ 1 token。
//
// ⚠️ 这只是**估算**，后台必须标注为"估算值"，
//   不能拿它做精确的成本核算。它的用途是：
//   - 发现异常（某期 prompt 字符数突然翻倍）
//   - 相对比较（哪一期更贵）
//
// 两者都不要求绝对准确。
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	var cjk, other int
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) {
			cjk++
		} else {
			other++
		}
	}
	// 其它字符按 4 个 ≈ 1 token，且至少算 1（避免大量标点算成 0）
	otherTokens := (other + 3) / 4
	if otherTokens == 0 {
		otherTokens = 1
	}
	return cjk + otherTokens
}

// EstimatedUsage 是一次推理的估算用量。
type EstimatedUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// Based 标明这是估算值（后台据此显示"估算"标签）。
	Based string `json:"based_on"`
}

// Estimate 估算一次推理的用量。
func (a *Audit) Estimate() EstimatedUsage {
	p := EstimateTokens(a.Prompt) + EstimateTokens(SystemPrompt)
	c := EstimateTokens(a.Insight.Summary) +
		listLen(a.Insight.Highlights) + listLen(a.Insight.Actions) + listLen(a.Insight.Warnings)
	// 用实际字符数更准：CompletionTokens 按原文估算，而不是解析后的结论，
	// 因为模型可能返回了带 markdown 包裹的原文。
	if a.RawOutput != "" {
		c = EstimateTokens(a.RawOutput)
	}
	return EstimatedUsage{
		PromptTokens:     p,
		CompletionTokens: c,
		TotalTokens:      p + c,
		Based:            "字符数估算（dapr ConverseAlpha1 不返回 Usage）",
	}
}

// IsAllZero 判断内容是否全为空白。
//
// 用于审计里判断"这次推理有没有真的产出内容" ——
// 一份全是空的审计记录意味着引擎走了降级但没标 Degraded，
// 那是一个需要暴露而不是隐藏的状态。
func IsAllZero(parts ...string) bool {
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			return false
		}
	}
	return true
}
