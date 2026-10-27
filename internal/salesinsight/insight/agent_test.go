package insight

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/YunBright/supertrade/internal/salesinsight/fact"
	"github.com/YunBright/supertrade/internal/salesinsight/weather"
)

// ⚠️ **agent 循环**：模型发起工具调用 → 结果回灌 → 模型给最终结论。
//
// 这条用例钉住 2026-10-26 的核心改动：
// 天气不再是"事实包里的一段文本"，而是模型**按需调用**的工具。
//
// 反证：把 runTool 改成直接返回固定文本（不回灌），
// 下面"模型必须看到自己查到的结果"这条断言会红。
func TestEngineRunsToolAndFeedsResultBack(t *testing.T) {
	rounds := 0
	var secondRoundMsgs []Message

	e := NewEngine(true, 0, func(ctx context.Context, c Conversation) (ConversationResult, error) {
		rounds++
		if rounds == 1 {
			// 第一轮：模型要查今天的天气。
			if len(c.Tools) == 0 {
				t.Error("第一轮必须带上工具目录，否则模型无从调用")
			}
			return ConversationResult{
				Content: "我先查一下今天的降雨。",
				ToolCalls: []ToolCall{{
					ID: "call_1", Name: ToolTodayRainfall, Arguments: "{}",
				}},
			}, nil
		}
		secondRoundMsgs = c.Messages
		return ConversationResult{
			Content: `{"summary":"今天下雨，客流下降","highlights":["雨具销量上升"],` +
				`"actions":["明天多备雨具"],"warnings":[]}`,
		}, nil
	})

	runner := &fakeRunner{result: "今天（2026-10-15）小雨，日降水量 3.2 毫米，有降水。"}
	e.WithTools([]ToolSpec{{Name: ToolTodayRainfall, Description: "查今天"}}, runner)

	in, audit := e.Analyze(context.Background(), AnalyzeInput{Fact: sampleFact()})

	if rounds != 2 {
		t.Fatalf("应往返两轮（第一轮调工具，第二轮给结论），实际 %d 轮", rounds)
	}
	if in.GeneratedBy != ByLLM {
		t.Errorf("应走 LLM 路径，实际 %q", in.GeneratedBy)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("应执行一次工具，实际 %d 次", len(runner.calls))
	}
	if runner.calls[0].Name != ToolTodayRainfall {
		t.Errorf("工具名不对: %q", runner.calls[0].Name)
	}

	// ⚠️ 模型必须看到**自己**那次工具调用的结果，
	//   否则它无从引用"今天下了 3.2 毫米的雨"。
	var toolMsg *Message
	for i := range secondRoundMsgs {
		if secondRoundMsgs[i].Role == RoleTool {
			toolMsg = &secondRoundMsgs[i]
		}
	}
	if toolMsg == nil {
		t.Fatal("第二轮的对话里必须有工具结果消息")
	}
	if !strings.Contains(toolMsg.Content, "3.2 毫米") {
		t.Errorf("工具结果必须回灌给模型: %q", toolMsg.Content)
	}
	if toolMsg.ToolCallID != "call_1" {
		t.Errorf("工具结果必须带上 tool_call_id（模型靠它对齐）：%q", toolMsg.ToolCallID)
	}
	// ⚠️ assistant 的 toolCalls 也必须在历史里 ——
	//   否则模型看到结果却看不到自己请求的是哪个工具，会张冠李戴。
	var assistantMsg *Message
	for i := range secondRoundMsgs {
		if secondRoundMsgs[i].Role == RoleAssistant {
			assistantMsg = &secondRoundMsgs[i]
		}
	}
	if assistantMsg == nil || len(assistantMsg.ToolCalls) != 1 {
		t.Fatal("第二轮的对话里必须有带 toolCalls 的 assistant 消息")
	}

	// 审计必须记录这次工具调用 —— "天气到底有没有被用上"的唯一证据。
	if len(audit.ToolCalls) != 1 {
		t.Fatalf("审计应记录一次工具调用，实际 %d 次", len(audit.ToolCalls))
	}
	if audit.ToolCalls[0].Name != ToolTodayRainfall {
		t.Errorf("审计里的工具名不对: %q", audit.ToolCalls[0].Name)
	}
	if !strings.Contains(audit.ToolCalls[0].Result, "3.2 毫米") {
		t.Errorf("审计必须原样保存工具结果: %q", audit.ToolCalls[0].Result)
	}
}

// ⚠️ 模型**不**调工具时，报告照常出 —— 工具是增强项，不是前提。
func TestEngineWorksWithoutToolCalls(t *testing.T) {
	rounds := 0
	e := NewEngine(true, 0, func(ctx context.Context, c Conversation) (ConversationResult, error) {
		rounds++
		return ConversationResult{
			Content: `{"summary":"今天持平","highlights":["蛋类稳定"],"actions":["明天补货西红柿"],"warnings":[]}`,
		}, nil
	})
	runner := &fakeRunner{result: "不该被调用"}
	e.WithTools([]ToolSpec{{Name: ToolTodayRainfall}}, runner)

	in, audit := e.Analyze(context.Background(), AnalyzeInput{Fact: sampleFact()})
	if rounds != 1 {
		t.Fatalf("模型不调工具时只应一轮，实际 %d 轮", rounds)
	}
	if len(runner.calls) != 0 {
		t.Errorf("模型没要求调工具就不该执行，实际执行了 %d 次", len(runner.calls))
	}
	if in.GeneratedBy != ByLLM {
		t.Errorf("应走 LLM 路径，实际 %q", in.GeneratedBy)
	}
	if len(audit.ToolCalls) != 0 {
		t.Errorf("审计里不应有工具调用记录: %v", audit.ToolCalls)
	}
}

// ⚠️ 工具失败**不能**让整份报告失败 ——
// 模型完全可以不用天气就写出一份好报告（那正是工具化的意义）。
func TestEngineSurvivesToolFailure(t *testing.T) {
	rounds := 0
	var toolResult string
	e := NewEngine(true, 0, func(ctx context.Context, c Conversation) (ConversationResult, error) {
		rounds++
		if rounds == 1 {
			return ConversationResult{
				ToolCalls: []ToolCall{{ID: "c1", Name: ToolTomorrowForecast}},
			}, nil
		}
		for _, m := range c.Messages {
			if m.Role == RoleTool {
				toolResult = m.Content
			}
		}
		return ConversationResult{
			Content: `{"summary":"正常出报告","highlights":["x"],"actions":["明天补货"],"warnings":[]}`,
		}, nil
	})
	e.WithTools([]ToolSpec{{Name: ToolTomorrowForecast}}, &fakeRunner{err: errors.New("天气服务挂了")})

	in, _ := e.Analyze(context.Background(), AnalyzeInput{Fact: sampleFact()})
	if in.GeneratedBy != ByLLM {
		t.Errorf("工具失败后仍应走 LLM 路径，实际 %q", in.GeneratedBy)
	}
	if !strings.Contains(toolResult, "工具调用失败") {
		t.Errorf("工具失败必须转成模型能理解的文本: %q", toolResult)
	}
}

// ⚠️ 轮数用尽仍要调工具 ⇒ 停下来，不再烧预算。
func TestEngineStopsAfterMaxToolRounds(t *testing.T) {
	rounds := 0
	e := NewEngine(true, 0, func(ctx context.Context, c Conversation) (ConversationResult, error) {
		rounds++
		return ConversationResult{
			Content:   "我再查一次",
			ToolCalls: []ToolCall{{ID: "c", Name: ToolTodayRainfall}},
		}, nil
	})
	e.WithTools([]ToolSpec{{Name: ToolTodayRainfall}}, &fakeRunner{result: "x"})

	_, audit := e.Analyze(context.Background(), AnalyzeInput{Fact: sampleFact()})
	// MaxToolRounds=2 ⇒ 最多 3 次调用（第一轮 + 两轮工具）。
	if rounds > MaxToolRounds+1 {
		t.Errorf("轮数应被限制在 %d 次内，实际 %d 次", MaxToolRounds+1, rounds)
	}
	// 最后一次的内容不是 JSON ⇒ 解析失败 ⇒ 降级为规则版（仍可发）。
	if !audit.Degraded {
		t.Errorf("模型一直调工具且没给结论时应降级为规则版")
	}
}

// fakeRunner 是可控的工具执行器。
type fakeRunner struct {
	result string
	err    error
	calls  []ToolCall
}

func (r *fakeRunner) Run(ctx context.Context, call ToolCall, f fact.Fact) (string, error) {
	r.calls = append(r.calls, call)
	if r.err != nil {
		return "", r.err
	}
	return r.result, nil
}

// fakeWeatherProvider 是可控的天气源。
type fakeWeatherProvider struct {
	daily map[string]weather.Daily
	err   error
}

func (p *fakeWeatherProvider) Daily(ctx context.Context, date string, loc weather.Location) (weather.Daily, error) {
	if p.err != nil {
		return weather.Daily{}, p.err
	}
	if d, ok := p.daily[date]; ok {
		return d, nil
	}
	return weather.Daily{Available: false, DegradedReason: "没有这一天"}, nil
}

// ⚠️ 天气工具的结果必须**带绝对日期**。
//
// 否则模型会把"明天的雨"写成"今天下雨"——
// 它没有别的时间锚，而店长会照着相反的天气改备货。
func TestWeatherToolResultCarriesAbsoluteDate(t *testing.T) {
	p := &fakeWeatherProvider{daily: map[string]weather.Daily{
		"2026-10-15": {Available: true, Date: "2026-10-15", Text: "小雨", PrecipMM: 3.2},
		"2026-10-16": {Available: true, Date: "2026-10-16", Text: "中雨", PrecipMM: 12.0},
	}}
	tools := WeatherTools{Provider: p, Location: weather.Location{Name: "岑溪", Lat: 24.7, Lon: 111.0}}

	today, err := tools.Run(context.Background(), ToolCall{Name: ToolTodayRainfall}, sampleFact())
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if !strings.Contains(today, "2026-10-15") {
		t.Errorf("今天的结果必须带绝对日期: %q", today)
	}
	if !strings.Contains(today, "3.2 毫米") {
		t.Errorf("必须带降水量（店长要量级）: %q", today)
	}
	if !strings.Contains(today, "今天") {
		t.Errorf("必须带相对标签（今天）: %q", today)
	}

	tomorrow, err := tools.Run(context.Background(), ToolCall{Name: ToolTomorrowForecast}, sampleFact())
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if !strings.Contains(tomorrow, "2026-10-16") {
		t.Errorf("明天的结果必须带绝对日期: %q", tomorrow)
	}
	if !strings.Contains(tomorrow, "明天") {
		t.Errorf("必须带相对标签（明天）: %q", tomorrow)
	}
	// ⚠️ 明天的结果里**不能**出现"今天" —— 那是串日的第一征兆。
	if strings.Contains(tomorrow, "今天") {
		t.Errorf("明天的结果不应出现「今天」: %q", tomorrow)
	}
}

// ⚠️ 天气不可用时**如实说不可用**，绝不编一个晴天。
//
// 编出来的晴天会被模型当事实，产出无法证伪的因果叙述。
func TestWeatherToolSaysUnavailableInsteadOfFaking(t *testing.T) {
	tools := WeatherTools{
		Provider: &fakeWeatherProvider{daily: map[string]weather.Daily{}},
		Location: weather.Location{Name: "岑溪", Lat: 24.7, Lon: 111.0},
	}
	out, err := tools.Run(context.Background(), ToolCall{Name: ToolTodayRainfall}, sampleFact())
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if !strings.Contains(out, "不可用") {
		t.Errorf("天气不可用时必须如实说明: %q", out)
	}
	for _, fake := range []string{"晴", "无降水", "0 毫米"} {
		if strings.Contains(out, fake) {
			t.Errorf("不可用时绝不能编造天气（出现 %q）: %q", fake, out)
		}
	}
}

// ⚠️ 没有位置时**不发起查询**，直接说不可用。
func TestWeatherToolWithoutLocationDoesNotQuery(t *testing.T) {
	p := &fakeWeatherProvider{daily: map[string]weather.Daily{
		"2026-10-15": {Available: true, Date: "2026-10-15", Text: "晴", PrecipMM: 0},
	}}
	tools := WeatherTools{Provider: p, Location: weather.Location{}}
	out, _ := tools.Run(context.Background(), ToolCall{Name: ToolTodayRainfall}, sampleFact())
	if !strings.Contains(out, "不可用") {
		t.Errorf("没有门店位置时必须说不可用: %q", out)
	}
}

// ⚠️ **事实包里绝不能再有天气**（2026-10-26）。
//
// 原实现在组装阶段无条件取天气，日报因此写出过
//「当前晴，22℃，北风3级；80分钟后开始下小雨」——
// 那段话描述的是营业结束之后的天气，与本期无关。
func TestFactDigestHasNoWeatherAnymore(t *testing.T) {
	d := FactDigest(sampleFact())
	for _, banned := range []string{"天气：", "80分钟后", "未来两小时", "分钟"} {
		if strings.Contains(d, banned) {
			t.Errorf("digest 不应再包含天气文本 %q:\n%s", banned, d)
		}
	}
}

// ⚠️ digest 必须带**未来两天** ——
// 这是"建议不滞后"的技术前提（模型要知道明天是周末）。
func TestFactDigestCarriesTomorrowAndDayAfter(t *testing.T) {
	d := FactDigest(sampleFact())
	if !strings.Contains(d, "明天 2026-10-16") {
		t.Errorf("digest 必须给出明天是哪天:\n%s", d)
	}
	if !strings.Contains(d, "后天 2026-10-17") {
		t.Errorf("digest 必须给出后天是哪天:\n%s", d)
	}
	// 且要带上客流基准性质（周末/工作日/假日）。
	if !strings.Contains(d, "周末") {
		t.Errorf("未来两天必须带日期性质（周末/工作日/法定假日）:\n%s", d)
	}
}

// ⚠️ prompt 必须明确要求建议指向**明天/后天**。
//
// 旧版只说"今天可执行的动作"，而日报 22:50 发 ——
// "今天"已经结束，于是模型写出"明天复核"这种把判断推迟一天的话。
func TestSystemPromptDemandsForwardLookingActions(t *testing.T) {
	for _, kw := range []string{
		"明天",
		"后天",
		"建议必须指向未来",
		"不要把现象当建议",
		"天气只能用工具返回的事实",
		`不要用"环比"这个词`,
	} {
		if !strings.Contains(SystemPrompt, kw) {
			t.Errorf("system prompt 缺少关键约束 %q", kw)
		}
	}
}

// ⚠️ **system prompt 必须真的发出去**（2026-10-26 现场教训）。
//
//	第一版 alpha2 实现只转发了 user/assistant/tool 消息，system 被丢掉 ——
//	而输出格式与全部硬约束都在 system 里。
//	生产症状：模型自己写了一份 {"headline","key_points",...} 的报告，
//	解析失败 ⇒ 整份 AI 报告降级成规则版。
//
// 反证：把 toDaprMessages 里的 system 分支删掉 → 本用例变红。
func TestDaprMessagesIncludeSystemPrompt(t *testing.T) {
	msgs := toDaprMessages(Conversation{
		System:   "SYSTEM-RULES-HERE",
		Messages: []Message{{Role: RoleUser, Content: "user body"}},
	})
	if len(msgs) == 0 {
		t.Fatal("消息列表为空")
	}
	if msgs[0].ConversationMessageOfSystem == nil {
		t.Fatal("第一条必须是 system 消息")
	}
	if msgs[0].ConversationMessageOfSystem.Content[0].Text == nil ||
		*msgs[0].ConversationMessageOfSystem.Content[0].Text != "SYSTEM-RULES-HERE" {
		t.Errorf("system 内容不对: %+v", msgs[0].ConversationMessageOfSystem)
	}
	// 之后才是 user 消息。
	if msgs[1].ConversationMessageOfUser == nil {
		t.Fatal("system 之后必须是 user 消息")
	}
}

// ⚠️ 形状有出入时**尽量取用**而不是整份丢弃。
//
// 生产实测：模型（带工具的 thinking 模式）自己写了一份
// {"headline","key_points","actions":[{"date","theme","items"}],...}，
// 而我们的 actions 是 []string ⇒ 严格解析失败 ⇒ 整份 AI 报告降级成规则版。
//
// 那份分析其实比规则版好得多，不能因为一个字段形状被整个丢掉。
func TestParseInsightToleratesModelInventedShape(t *testing.T) {
	raw := `{
      "report_date": "2026-10-26",
      "headline": "销售额较上周同期 -4.4%，百货类单点爆发",
      "key_points": ["蛋类 -14.4% 拖累全店", "百货类 +222.3%"],
      "risks": ["蛋类单一供应商依赖"],
      "actions": [
        {"date": "2026-10-27", "theme": "稳住刚需盘", "items": ["补齐鸡蛋到货", "巡检调味类货架"]},
        {"date": "2026-10-28", "theme": "承接换季需求", "items": ["复核炒锅动销"]}
      ]
    }`
	in, err := ParseInsight(raw)
	if err != nil {
		t.Fatalf("形状有出入时不应整份失败: %v", err)
	}
	if in.Summary == "" {
		t.Error("headline 应被取用为 summary")
	}
	if len(in.Highlights) != 2 {
		t.Errorf("key_points 应被取用为 highlights，实际 %d 条", len(in.Highlights))
	}
	if len(in.Warnings) != 1 {
		t.Errorf("risks 应被取用为 warnings，实际 %d 条", len(in.Warnings))
	}
	if len(in.Actions) != 3 {
		t.Fatalf("actions 应被拍平成 3 条，实际 %d 条: %v", len(in.Actions), in.Actions)
	}
	// ⚠️ **日期必须留下** —— 读者要能分清明天与后天。
	if !strings.Contains(in.Actions[0], "2026-10-27") {
		t.Errorf("动作必须带日期: %q", in.Actions[0])
	}
	if !strings.Contains(in.Actions[2], "2026-10-28") {
		t.Errorf("后天动作必须带日期: %q", in.Actions[2])
	}
}
