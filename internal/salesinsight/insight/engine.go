package insight

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	dapr "github.com/dapr/go-sdk/client"

	"github.com/YunBright/supertrade/internal/salesinsight/fact"
)

// Analyzer 是"从事实包得到结论"的依赖面。
//
// 收窄成一个方法的理由同本仓其它服务（salesagg.CubeQuerier、
// wecomreport.ReportFetcher）：接口越窄，单测越能证明
// "到底往 LLM 发了什么、拿到了什么"。
type Analyzer interface {
	// Analyze 产出结论。它**不应**抛错 ——
	// LLM 的任何失败都应当降级为规则版结论并如实标注，
	// 而不是让整个日报失败。真正需要抛错的只有"事实包本身无效"。
	Analyze(ctx context.Context, in AnalyzeInput) (Insight, Audit)
}

// AnalyzeInput 是一次推理的输入。
type AnalyzeInput struct {
	Fact fact.Fact
	// PrevSummary 是上一期结论摘要，可为 nil。
	PrevSummary *PrevSummary
}

// Reason 是降级原因。
type Reason string

const (
	// ReasonDisabled 显式关闭了 AI
	ReasonDisabled Reason = "AI 解读已关闭"
	// ReasonNoClient 没有 dapr client
	ReasonNoClient Reason = "LLM 客户端未就绪"
	// ReasonNoComponent 未配置 conversation 组件
	ReasonNoComponent Reason = "未配置 dapr conversation 组件"
	// ReasonTimeout 调用超时
	ReasonTimeout Reason = "LLM 调用超时"
	// ReasonCallError 调用失败
	ReasonCallError Reason = "LLM 调用失败"
	// ReasonEmptyOutput 返回为空
	ReasonEmptyOutput Reason = "LLM 返回为空"
	// ReasonParseFailed 返回无法解析
	ReasonParseFailed Reason = "LLM 返回无法解析为 JSON"
)

// Engine 是推理引擎。
type Engine struct {
	// converse 是 dapr conversation 调用。为 nil 时一律降级。
	converse func(ctx context.Context, system, user string) (raw string, pt, ct int, err error)
	// timeout 是单次调用超时。
	timeout time.Duration
	// now 注入用于测试。
	now func() time.Time
	// enabled 为 false 时一律降级（运营可以只想要规则版）。
	enabled bool
}

// DefaultTimeout 是单次 LLM 调用超时。
//
// 取 45s 而不是 30s：日报要在推送时间点准时发出，
// 而 DeepSeek 在高峰期偶尔会到 20~30s。宁可多等一会儿，
// 也不要因为"差 3 秒"而把一份完整的 AI 报告降级成规则版。
const DefaultTimeout = 45 * time.Second

// NewEngine 构造引擎。
//
// converse 为 nil 时引擎仍可用 —— 每次调用都会降级，
// 且降级原因会写进审计。这是有意的：**引擎本身不因缺依赖而无法启动**，
// 否则运维改错一个环境变量就会让整个服务起不来，
// 而日报这件事比 AI 更重要。
func NewEngine(enabled bool, timeout time.Duration, converse ConverseFunc) *Engine {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Engine{
		converse: converse,
		timeout:  timeout,
		now:      time.Now,
		enabled:  enabled,
	}
}

// ConverseFunc 是 dapr conversation 调用的窄接口。
type ConverseFunc func(ctx context.Context, system, user string) (raw string, promptTokens, completionTokens int, err error)

// Analyze 执行一次推理，永远返回可用的 Insight。
func (e *Engine) Analyze(ctx context.Context, in AnalyzeInput) (Insight, Audit) {
	audit := Audit{
		RunID:        newRunID(),
		PeriodKind:   in.Fact.Meta.PeriodKind,
		From:         in.Fact.Meta.From,
		To:           in.Fact.Meta.To,
		BranchID:     in.Fact.Meta.BranchID,
		CreatedAt:    e.now(),
		Context:      JSONCompact(in.Fact),
		PromptVersion: PromptVersion,
	}

	// 上期摘要要写进 Context —— 否则后台看到的"上下文"是不完整的，
	// 而"模型当时到底看到了什么"正是审计要回答的问题。
	if in.PrevSummary != nil {
		audit.Context += "|prev_summary=" + JSONCompact(in.PrevSummary)
	}

	// ---- 快速降级：根本不该调 LLM 的情况 ----
	if !e.enabled {
		return e.degrade(in, audit, ReasonDisabled)
	}
	if e.converse == nil {
		return e.degrade(in, audit, ReasonNoClient)
	}

	user := BuildUserPrompt(in.Fact, in.PrevSummary)
	audit.Prompt = user

	// ---- 自检：prompt 太长就别调了 ----
	//
	// ⚠️ 这是保护 token 预算的最后一道闸。
	//   正常的日报 prompt 只有 2~4K 字符；超过 MaxPromptChars
	//   说明某个配置出了问题（比如 TopN 传了超大值，
	//   或上期摘要没被截断）。此时调 LLM 会浪费一次付费调用，
	//   而且必然超预算。
	if len([]rune(user)) > MaxPromptChars {
		return e.degrade(in, audit, Reason(fmt.Sprintf(
			"prompt 长度 %d 字符，超过上限 %d（配置异常）",
			len([]rune(user)), MaxPromptChars)))
	}

	// ---- 调用 ----
	callCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	start := e.now()
	raw, pt, ct, err := e.converse(callCtx, SystemPrompt, user)
	audit.LatencyMS = e.now().Sub(start).Milliseconds()
	audit.PromptTokens = pt
	audit.CompletionTokens = ct
	audit.RawOutput = raw

	if err != nil {
		reason := ReasonCallError
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			reason = ReasonTimeout
		}
		audit.Error = err.Error()
		return e.degrade(in, audit, reason)
	}
	if strings.TrimSpace(raw) == "" {
		return e.degrade(in, audit, ReasonEmptyOutput)
	}

	parsed, perr := ParseInsight(raw)
	if perr != nil {
		audit.Error = perr.Error()
		// 解析失败是最需要看 raw 的场景 —— 它保留在审计里，
		// 便于判断是 prompt 没写清格式，还是模型换了个输出风格。
		return e.degrade(in, audit, ReasonParseFailed)
	}

	parsed.GeneratedBy = ByLLM
	parsed.PromptVersion = PromptVersion
	parsed.Sanitize()

	audit.Insight = *parsed
	audit.Degraded = false
	audit.Trim()
	return *parsed, audit
}

// degrade 走规则降级。
func (e *Engine) degrade(in AnalyzeInput, audit Audit, why Reason) (Insight, Audit) {
	ins := RuleInsightsFromFact(in.Fact, string(why))
	audit.Insight = ins
	audit.Degraded = true
	audit.DegradedReason = string(why)
	audit.Trim()
	return ins, audit
}

// DaprConverseFn 构造经 dapr conversation 的调用实现。
//
// ⚠️ 与 fresh-meat / settlement-auditor 用**同一个组件名** `conversation`，
//   所以那个服务的 dapr 组件配置可以直接照抄（连 secretstore 依赖都一样）。
//   换组件名会让运维以为要新建配置，而实际重建一个同名组件反而会
//   让两个服务的配置分叉。
func DaprConverseFn(daprCli dapr.Client, convName string) ConverseFunc {
	if daprCli == nil {
		return nil
	}
	if convName == "" {
		convName = "conversation"
	}
	return func(ctx context.Context, system, user string) (string, int, int, error) {
		roleSystem, roleUser := "system", "user"
		req := dapr.NewConversationRequest(convName, []dapr.ConversationInput{
			{Role: &roleSystem, Content: system},
			{Role: &roleUser, Content: user},
		})
		// temperature=0.3 而不是 0：
		// 0 会让"归因措辞"变得非常机械重复，而日报需要一点措辞变化
		// 才不至于每天读起来像系统告警。0.3 在"稳定"与"可读"之间。
		resp, err := daprCli.ConverseAlpha1(ctx, req, dapr.WithTemperature(0.3))
		if err != nil {
			return "", 0, 0, fmt.Errorf("dapr converse: %w", err)
		}
		if len(resp.Outputs) == 0 {
			return "", 0, 0, nil
		}
		raw := resp.Outputs[0].Result
		pt, ct := usageOf(resp)
		return raw, pt, ct, nil
	}
}

// usageOf 提取 token 用量。
//
// # ⚠️ ConverseAlpha1 的响应**没有** Usage 字段
//
// ✅ 实测 SDK v1.15.0：ConversationResponse 只有 ContextID + Outputs[]，
//   而 ConversationResult 只有 Result + Parameters。
//   Usage 只在 **Alpha2**（ConversationResultAlpha2.Usage）上。
//
// 所以本函数恒返回 (0, 0)。
//
// # 为什么不改用 Alpha2
//
// Alpha2 的返回结构变了（Choices[].Message.Content），
// 与既有 fresh-meat / settlement-auditor 两个服务的用法不同，
// 为了一份 token 统计而让本服务成为**唯一**用不同 API 的服务，
// 不划算 —— 组件配置、错误处理、降级路径都要重新验证。
//
// 代价是：后台的 token 用量一栏对 AI 推理恒为 0。
// 这是**已知限制**而不是 bug。若将来要精确成本，
// 正确做法是给 conversation 组件加 usage 回调或在审计里
// 用字符数估算（约 1 个 token ≈ 1.5 个中文字符），
// 而不是为此切换 API 版本。
func usageOf(resp *dapr.ConversationResponse) (promptTokens, completionTokens int) {
	return 0, 0
}

// MaxPromptChars 是 prompt 字符数上限（超了就降级，不做付费调用）。
//
// 取 16000 的依据：正常日报约 2~4K；周报/月报因排行更多，
// 大约 5~8K。16000 是一个"配置明显错了"的信号，
// 而不是"正常的大报告"。定得太宽松就失去了拦截意义。
const MaxPromptChars = 16000
