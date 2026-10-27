package insight

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	dapr "github.com/dapr/go-sdk/client"
	"google.golang.org/protobuf/types/known/structpb"

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
	// PrevSummaries 是**上期摘要候选**（每一期一份，2026-10-27 起）。
	//
	// ⚠️ 为什么由本包挑：哪一份才是"上一期报告"是**口径问题**
	//   （日报：上一期=昨天，对比基准=上周同一天），
	//   而口径只有本包一处真相（calendar.ResolvePeriod 的 PrevReport）。
	//   调用方自己挑 ⇒ 两份口径迟早分叉 ⇒
	//   症状是"某一期报告突然不接上期的话"，且**没有任何报错**。
	PrevSummaries []*PrevSummary
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
//
// # 它现在是一个**带工具的 agent**
//
//	模型在第一轮看到事实包 + 工具清单；它可以：
//	  - 直接回答（不需要天气），
//	  - 或发起一次工具调用（查今天/明天的天气）。
//	工具结果回灌后，模型给出最终结论。
//
//	⇒ 天气**不再**无条件出现在 prompt 里（那曾让日报写出
//	  "80分钟后开始下小雨"这种与报告期无关的句子）。
type Engine struct {
	// converse 是与 LLM 的对话。为 nil 时一律降级。
	converse ConverseFunc
	// tools 是暴露给模型的工具目录。
	tools []ToolSpec
	// runner 执行模型发起的工具调用。为 nil 时不发工具（模型只能直接回答）。
	runner ToolRunner
	// timeout 是**整次推理**（含所有轮次与工具调用）的总预算。
	timeout time.Duration
	// now 注入用于测试。
	now func() time.Time
	// enabled 为 false 时一律降级（运营可以只想要规则版）。
	enabled bool
}

// DefaultTimeout 是单次推理的总超时。
//
// 取 45s 而不是 30s：日报要在推送时间点准时发出，
// 而 DeepSeek 在高峰期偶尔会到 20~30s。宁可多等一会儿，
// 也不要因为"差 3 秒"而把一份完整的 AI 报告降级成规则版。
//
// ⚠️ 它是**整个 agent 循环**的预算，不是"每轮"的预算：
//
//	一轮 LLM 约 8s，工具调用约 1s，两轮合计约 17s，
//	远小于 45s。若按"每轮 45s"计，最坏情况会拖到 90s+，
//	而推送是 22:50 准点的 —— 报告迟到比降级更糟。
const DefaultTimeout = 45 * time.Second

// MaxToolRounds 是"模型发起工具调用"的最大轮数。
//
// 取 2 的理由：工具只有两个（今天/明天），一轮就能把需要的都查完；
// 第二轮留给"查完仍不够"的情形。第三轮说明模型在乱转，
// 继续转只是烧钱和时间。
const MaxToolRounds = 2

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

// WithTools 注入工具目录与执行器。
func (e *Engine) WithTools(specs []ToolSpec, runner ToolRunner) *Engine {
	e.tools = specs
	e.runner = runner
	return e
}

// ConverseFunc 是与 LLM 一次往返的窄接口。
//
// ⚠️ 它比原来多带了三样东西：消息列表、工具目录、工具调用请求 ——
// 因为 agent 循环需要"把上一轮的工具结果回灌给模型"。
//
// 传 []Message 而不是单条 user 文本，是为了让**同一份对话**贯穿多轮：
// 模型看到自己的工具调用与结果，推理才能真正连续
// （否则它每轮都被当成新对话，"我不知道刚才查到了什么"）。
type ConverseFunc func(ctx context.Context, c Conversation) (ConversationResult, error)

// Conversation 是一次 LLM 调用的输入。
type Conversation struct {
	// System 是 system prompt。
	System string
	// Messages 是完整对话历史（user → assistant(tool_calls) → tool → …）。
	Messages []Message
	// Tools 是本次暴露给模型的工具（可为空）。
	Tools []ToolSpec
}

// MessageRole 是消息角色。
type MessageRole string

const (
	// RoleUser 用户消息（事实包）。
	RoleUser MessageRole = "user"
	// RoleAssistant 模型消息（可能带工具调用请求）。
	RoleAssistant MessageRole = "assistant"
	// RoleTool 工具执行结果。
	RoleTool MessageRole = "tool"
)

// Message 是一条对话消息。
type Message struct {
	Role    MessageRole
	Content string
	// ToolCalls 只有 assistant 消息会有。
	ToolCalls []ToolCall
	// ToolCallID / ToolName 只有 tool 消息会有。
	ToolCallID string
	ToolName   string
}

// ToolCall 是模型发起的一次工具调用。
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// ConversationResult 是一次 LLM 调用的结果。
type ConversationResult struct {
	// Content 是模型的文本输出（最后一轮就是最终结论的 JSON）。
	Content string
	// ToolCalls 是模型请求调用的工具（为空 = 它要直接回答了）。
	ToolCalls []ToolCall
	// PromptTokens / CompletionTokens 是 token 用量（可能为 0）。
	PromptTokens     int
	CompletionTokens int
}

// ToolRunner 执行模型发起的工具调用。
//
// fact 是本次报告的事实包 —— 工具需要它来知道"今天是哪天"。
type ToolRunner interface {
	Run(ctx context.Context, call ToolCall, f fact.Fact) (string, error)
}

// Analyze 执行一次推理，永远返回可用的 Insight。
func (e *Engine) Analyze(ctx context.Context, in AnalyzeInput) (Insight, Audit) {
	// ⚠ 上期摘要必须与事实包的"上一期报告"区间一致，否则**整份丢弃**。
	//
	//	这一步在构造审计**之前**做，是为了让 audit.Context 与真正发给模型的
	//	prompt 保持一致 —— 审计要能还原"模型当时到底看到了什么"，
	//	若审计记着有摘要、prompt 里却没有，那份审计就是假的。
	//
	// ⚠️ 候选列表（每一期一份）由本包按区间挑，
	//   而不是调用方挑（见 AnalyzeInput.PrevSummaries 的注释）。
	prev := PickPrevSummary(in.Fact, in.PrevSummaries, in.PrevSummary)

	audit := Audit{
		RunID:         newRunID(),
		PeriodKind:    in.Fact.Meta.PeriodKind,
		From:          in.Fact.Meta.From,
		To:            in.Fact.Meta.To,
		PrevFrom:      in.Fact.Meta.PrevFrom,
		PrevTo:        in.Fact.Meta.PrevTo,
		PrevLabel:     in.Fact.Meta.PrevLabel,
		BranchID:      in.Fact.Meta.BranchID,
		CreatedAt:     e.now(),
		Context:       JSONCompact(in.Fact),
		PromptVersion: PromptVersion,
	}

	// 上期摘要要写进 Context —— 否则后台看到的"上下文"是不完整的，
	// 而"模型当时到底看到了什么"正是审计要回答的问题。
	if prev != nil {
		audit.Context += "|prev_summary=" + JSONCompact(prev)
	}

	// ---- 快速降级：根本不该调 LLM 的情况 ----
	if !e.enabled {
		return e.degrade(in, audit, ReasonDisabled)
	}
	if e.converse == nil {
		return e.degrade(in, audit, ReasonNoClient)
	}

	user := BuildUserPrompt(in.Fact, prev)
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

	// ---- agent 循环 ----
	//
	// ⚠️ 整段共用**一个** deadline：
	//   多轮 + 工具调用必须共享总预算，否则最坏情况会拖成 2 倍超时。
	callCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	msgs := []Message{{Role: RoleUser, Content: user}}
	var raw string
	var pt, ct int

	for round := 0; round <= MaxToolRounds; round++ {
		start := e.now()
		res, err := e.converse(callCtx, Conversation{
			System:   SystemPrompt,
			Messages: msgs,
			Tools:    e.tools,
		})
		audit.LatencyMS += e.now().Sub(start).Milliseconds()
		pt += res.PromptTokens
		ct += res.CompletionTokens

		if err != nil {
			reason := ReasonCallError
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(callCtx.Err(), context.DeadlineExceeded) {
				reason = ReasonTimeout
			}
			audit.Error = err.Error()
			return e.degrade(in, audit, reason)
		}

		if len(res.ToolCalls) == 0 {
			raw = res.Content
			break
		}

		// 模型要调工具：先把它自己的消息记进历史，再回灌工具结果。
		msgs = append(msgs, Message{
			Role:      RoleAssistant,
			Content:   res.Content,
			ToolCalls: res.ToolCalls,
		})
		for _, tc := range res.ToolCalls {
			out := e.runTool(callCtx, tc, in.Fact)
			audit.ToolCalls = append(audit.ToolCalls, ToolCallTrace{
				Name: tc.Name, Args: tc.Arguments, Result: out,
			})
			msgs = append(msgs, Message{
				Role:       RoleTool,
				ToolCallID: tc.ID,
				ToolName:   tc.Name,
				Content:    out,
			})
		}

		if round == MaxToolRounds {
			// ⚠️ 轮数用尽仍要调工具 ⇒ 让模型自己收尾。
			//   再循环下去只是在烧预算；这一轮的内容会是最终答案。
			if strings.TrimSpace(res.Content) == "" {
				raw = ""
			} else {
				raw = res.Content
			}
			break
		}
	}

	audit.PromptTokens = pt
	audit.CompletionTokens = ct
	audit.RawOutput = raw

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
	// ⚠️ LLM 路径必须过清洗闸：prompt 里已经没有全店金额了，
	//   但模型仍可能从别处（品类相加、上期摘要）凑出一个总额。
	//   规则降级路径在 RuleInsightsFromFact 里已经过了同一道闸。
	RedactStoreTotals(in.Fact, parsed)

	audit.Insight = *parsed
	audit.Degraded = false
	audit.Trim()
	return *parsed, audit
}

// runTool 执行一次工具调用，并把失败也变成**模型能理解的文本**。
//
// ⚠️ 工具失败**不能**中断整个推理：
//
//	模型完全可以不用天气就写出一份好报告（那正是工具化的意义）。
//	所以这里把错误转成一句话回灌，让模型自己决定要不要用。
func (e *Engine) runTool(ctx context.Context, tc ToolCall, f fact.Fact) string {
	if e.runner == nil {
		return "工具不可用：本次推理未装配工具执行器。"
	}
	out, err := e.runner.Run(ctx, tc, f)
	if err != nil {
		return "工具调用失败：" + err.Error()
	}
	if strings.TrimSpace(out) == "" {
		return "工具调用失败：工具没有返回内容。"
	}
	return out
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
//
//	所以那个服务的 dapr 组件配置可以直接照抄（连 secretstore 依赖都一样）。
//	换组件名会让运维以为要新建配置，而实际重建一个同名组件反而会
//	让两个服务的配置分叉。
//
// # ⚠️ 为什么用 Alpha2 而不是 Alpha1
//
//	只有 Alpha2 的请求里有 `tools` 与 `toolChoice` 字段 ——
//	Alpha1 没有，工具根本传不到模型。
//	代价是 Alpha2 的响应结构变了（Choices[].Message.Content），
//	与 fresh-meat / settlement-auditor 两个服务的用法不同。
//	为了一份工具能力让本服务成为**唯一**用不同 API 的服务，是划算的：
//	"天气不该无条件出现在日报里"是产品硬要求，不是优化项。
//
// ⚠️ 实测（本机 Dapr 1.18 + conversation.openai + DeepSeek）：
//
//	带 tools 的请求会返回 choices[].message.toolCalls，
//	模型确实会调用工具；tool_choice 必须用 "auto"
//	（该模型是 thinking 模式，"required" 会被上游拒 400）。
func DaprConverseFn(daprCli dapr.Client, convName string) ConverseFunc {
	if daprCli == nil {
		return nil
	}
	if convName == "" {
		convName = "conversation"
	}
	return func(ctx context.Context, c Conversation) (ConversationResult, error) {
		req := dapr.ConversationRequestAlpha2{
			Name: convName,
			Inputs: []*dapr.ConversationInputAlpha2{{
				Messages: toDaprMessages(c),
			}},
		}
		if len(c.Tools) > 0 {
			req.Tools = toDaprTools(c.Tools)
			// ⚠️ 必须是 "auto"：
			//   "required" 会强制模型调工具（即使它不需要），
			//   而 DeepSeek 的 thinking 模式模型对 "required" 直接返回 400。
			choice := dapr.ToolChoiceAutoAlpha2
			req.ToolChoice = &choice
		}
		// temperature=0.3 而不是 0：
		// 0 会让"归因措辞"变得非常机械重复，而日报需要一点措辞变化
		// 才不至于每天读起来像系统告警。0.3 在"稳定"与"可读"之间。
		temp := 0.3
		req.Temperature = &temp

		resp, err := daprCli.ConverseAlpha2(ctx, req)
		if err != nil {
			return ConversationResult{}, fmt.Errorf("dapr converse: %w", err)
		}
		if len(resp.Outputs) == 0 {
			return ConversationResult{}, nil
		}
		out := resp.Outputs[0]
		res := ConversationResult{}
		if len(out.Choices) > 0 {
			choice := out.Choices[0]
			if choice.Message != nil {
				res.Content = choice.Message.Content
				for _, tc := range choice.Message.ToolCalls {
					if tc.ToolTypes.Name == "" {
						continue
					}
					res.ToolCalls = append(res.ToolCalls, ToolCall{
						ID:        tc.ID,
						Name:      tc.ToolTypes.Name,
						Arguments: tc.ToolTypes.Arguments,
					})
				}
			}
		}
		if out.Usage != nil {
			res.PromptTokens = int(out.Usage.PromptTokens)
			res.CompletionTokens = int(out.Usage.CompletionTokens)
		}
		return res, nil
	}
}

// toDaprMessages 把内部对话转成 dapr 的 alpha2 消息。
//
// ⚠️ **system prompt 必须在这里发出去**（2026-10-26 现场教训）。
//
//	第一版实现只把 user/assistant/tool 消息转过去，system 被丢掉了 ——
//	而 system prompt 里正是**输出格式**与**全部硬约束**的所在。
//	后果（生产实测）：模型完全不知道该输出
//	{"summary","highlights","actions","warnings"}，
//	自己写了一份 {"headline","key_points","category_analysis",...} 的报告，
//	解析失败 ⇒ 整份 AI 报告降级成规则版（"销售额环比 -4.4%"）。
//
//	症状极具欺骗性：工具调用是好的、天气事实也用上了，
//	只有"AI 解读不可用：LLM 返回无法解析为 JSON"这一行提示不对。
//
// ⚠️ assistant 消息**必须**带上它的 toolCalls ——
//
//	否则模型看到"工具结果"却看不到"我刚才请求的是哪个工具"，
//	会把结果张冠李戴。
func toDaprMessages(c Conversation) []*dapr.ConversationMessageAlpha2 {
	out := make([]*dapr.ConversationMessageAlpha2, 0, len(c.Messages)+1)

	// ---- system ----
	if s := strings.TrimSpace(c.System); s != "" {
		out = append(out, &dapr.ConversationMessageAlpha2{
			ConversationMessageOfSystem: &dapr.ConversationMessageOfSystemAlpha2{
				Content: []*dapr.ConversationMessageContentAlpha2{textContent(s)},
			},
		})
	}

	for _, m := range c.Messages {
		switch m.Role {
		case RoleUser:
			out = append(out, &dapr.ConversationMessageAlpha2{
				ConversationMessageOfUser: &dapr.ConversationMessageOfUserAlpha2{
					Content: []*dapr.ConversationMessageContentAlpha2{textContent(m.Content)},
				},
			})
		case RoleAssistant:
			msg := &dapr.ConversationMessageOfAssistantAlpha2{
				Content: []*dapr.ConversationMessageContentAlpha2{textContent(m.Content)},
			}
			for _, tc := range m.ToolCalls {
				msg.ToolCalls = append(msg.ToolCalls, &dapr.ConversationToolCallsAlpha2{
					ID: tc.ID,
					ToolTypes: dapr.ConversationToolAlpha2{
						Name:      tc.Name,
						Arguments: tc.Arguments,
					},
				})
			}
			out = append(out, &dapr.ConversationMessageAlpha2{
				ConversationMessageOfAssistant: msg,
			})
		case RoleTool:
			name := m.ToolName
			out = append(out, &dapr.ConversationMessageAlpha2{
				ConversationMessageOfTool: &dapr.ConversationMessageOfToolAlpha2{
					ToolID:  &m.ToolCallID,
					Name:    &name,
					Content: []*dapr.ConversationMessageContentAlpha2{textContent(m.Content)},
				},
			})
		}
	}
	return out
}

// textContent 造一个单段文本内容。
func textContent(s string) *dapr.ConversationMessageContentAlpha2 {
	t := s
	return &dapr.ConversationMessageContentAlpha2{Text: &t}
}

// toDaprTools 把工具声明转成 dapr 的 alpha2 工具。
func toDaprTools(specs []ToolSpec) []*dapr.ConversationToolsAlpha2 {
	out := make([]*dapr.ConversationToolsAlpha2, 0, len(specs))
	for _, s := range specs {
		desc := s.Description
		t := &dapr.ConversationToolsAlpha2{
			Name:        s.Name,
			Description: &desc,
		}
		if s.Parameters != nil {
			if st, err := structpbStruct(s.Parameters); err == nil {
				t.Parameters = st
			}
		}
		out = append(out, t)
	}
	return out
}

// structpbStruct 把 map 转成 protobuf 的 Struct（工具的 JSON Schema）。
func structpbStruct(m map[string]any) (*structpb.Struct, error) {
	return structpb.NewStruct(m)
}

// MaxPromptChars 是 prompt 字符数上限（超了就降级，不做付费调用）。
//
// 取 16000 的依据：正常日报约 2~4K；周报/月报因排行更多，
// 大约 5~8K。16000 是一个"配置明显错了"的信号，
// 而不是"正常的大报告"。定得太宽松就失去了拦截意义。
const MaxPromptChars = 16000
