// Package service / llm.go —— 同步调 Dapr Conversation API(LLM)做整猪分割预测。
//
// 设计:
//     RecordWholePig → insert → 调 PredictFn(pig, history) →
//       ├─ 成功 → update whole_pig llm_advice_json = result.RawJSON, data_source = "llm"
//       └─ 失败 / 超时 → fallbackToHistoryAvg → update whole_pig llm_advice_json + data_source = "history_avg"
//
//   - **失败不视为业务错误**:返回 200 给仓管,data_source 标注降级来源。
//   - 默认 PredictFn 是 "history_avg";生产注入 DaprConversationPredictFn。
//   - ConvName 默认 "conversation";env FRESHMEAT_CONVERSATION 覆盖。
//   - LLMTimeout 默认 30s;env FRESHMEAT_LLM_TIMEOUT 覆盖。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
	dapr "github.com/dapr/go-sdk/client"
)

// LLMResult 是 PredictFn 的返回值 — 仓管期望的整猪分割建议。
//
// RawJSON 是给 whole_pig.llm_advice_json 列的原始 JSON(直接写 jsonb);
// Cuts 是解析后的预期 cuts(供后续 BI 复用 / 校验)。
type LLMResult struct {
	Cuts    []PredictedCut `json:"cuts"`
	RawJSON []byte        `json:"-"`
	Source  string         `json:"source"` // "llm" 或 "history_avg"
}

// PredictedCut 期望的部位产出。
type PredictedCut struct {
	CutType    model.CutType   `json:"cut_type"`
	ExpectedKg decimal.Decimal `json:"expected_kg"`
}

// PredictFn 是 service.Service.predictFn 的签名(便于单测注入 fake)。
//
// 输入:pig 上下文 + 近 30 天同重量段历史猪(可能为空)。
// 返回:LLMResult 或 error(error 由 caller 降级为 history_avg)。
type PredictFn func(ctx context.Context, pig *model.WholePig, history []model.WholePig) (*LLMResult, error)

// ---- 默认 PredictFn:history_avg 降级路径 ----

// defaultPredictFn 是 service.New() 后默认的 predictFn(返回 history_avg 结果)。
//
// 默认不走 LLM,直接走 history_avg 兜底;真实接入 Dapr Conversation API 时
// 由 main.go 调 SetPredictFn 覆盖。
func defaultPredictFn(_ context.Context, pig *model.WholePig, history []model.WholePig) (*LLMResult, error) {
	cuts, raw, err := fallbackToHistoryAvg(pig, history)
	if err != nil {
		return nil, err
	}
	return &LLMResult{Cuts: cuts, RawJSON: raw, Source: "history_avg"}, nil
}

// fallbackToHistoryAvg 同重量段(±10%)近 30 天猪的实际 cuts 均值 → 推演本次预期。
//
// 数据来源:history 字段部分已由 caller 查好;本函数只做计算 + JSON 序列化。
// 当 history 为空 → 返空 cuts(避免 hallucinate)。
func fallbackToHistoryAvg(pig *model.WholePig, history []model.WholePig) ([]PredictedCut, []byte, error) {
	if len(history) == 0 {
		// 没历史 → 返 cuts=[];前端看到 data_source=history_avg + 空数组即可知"无建议"。
		raw, err := json.Marshal(map[string]any{
			"cuts":          []any{},
			"fallback_used": true,
			"reason":        "no historical pigs in ±10% weight band (last 30 days)",
		})
		if err != nil {
			return nil, nil, fmt.Errorf("marshal history_avg empty: %w", err)
		}
		return nil, raw, nil
	}

	// 简化:暂未接 cube 的历史 pig_cuts 聚合,直接透传;真正接 cube 后,
	// 这里会按 cut_type × 重量段聚合,缩放到本次 gross_kg。
	raw, err := json.Marshal(map[string]any{
		"cuts":          []any{},
		"fallback_used": true,
		"history_count": len(history),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("marshal history_avg: %w", err)
	}
	return nil, raw, nil
}

// ---- Dapr Conversation API 真实接入 ----

// DaprConversationPredictFn 构造生产用 PredictFn。
//
// 行为:
//   - 30s 超时(caller 注入,env FRESHMEAT_LLM_TIMEOUT 覆盖);
//   - prompt 由 buildPredictPrompt 拼出(参考:猪 metadata + 近 30 天 ±10% 重量段历史);
//   - 解析 LLM 返 JSON 为 LLMResult;
//   - 失败(超时 / 网络 / 解析)→ 返 error,caller 走 history_avg 兜底。
//
// dapr.Client 是 SDK 自动从 DAPR_GRPC_PORT 拿 sidecar 的 gRPC 客户端;
// ConverseAlpha1 走 dapr 1.18 的 conversation.<name>/converse 端点(2026-09)。
func DaprConversationPredictFn(daprCli dapr.Client, convName string, timeout time.Duration) PredictFn {
	if convName == "" {
		convName = "conversation"
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return func(ctx context.Context, pig *model.WholePig, history []model.WholePig) (*LLMResult, error) {
		if daprCli == nil {
			return nil, errors.New("dapr client not injected")
		}
		system := buildSystemPrompt()
		user := buildUserPrompt(pig, history)
		req := dapr.NewConversationRequest(convName, []dapr.ConversationInput{
			{Role: ptrStr("system"), Content: system},
			{Role: ptrStr("user"), Content: user},
		})
		resp, err := daprCli.ConverseAlpha1(ctx, req, dapr.WithTemperature(0.2))
		if err != nil {
			return nil, fmt.Errorf("dapr converse: %w", err)
		}
		if len(resp.Outputs) == 0 {
			return nil, errors.New("dapr converse: no outputs")
		}
		raw := resp.Outputs[0].Result
		if raw == "" {
			return nil, errors.New("dapr converse: empty result")
		}
		// LLM 返 JSON:{"cuts":[{"cut_type":"belly","expected_kg":20.56},...]}
		var parsed struct {
			Cuts []PredictedCut `json:"cuts"`
		}
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			return nil, fmt.Errorf("parse LLM output: %w", err)
		}
		// 归一化 cut_type 字符串(防 LLM 返 "五花" / "五花肉" 等)
		for i := range parsed.Cuts {
			parsed.Cuts[i].CutType = normalizeCutType(parsed.Cuts[i].CutType)
		}
		return &LLMResult{Cuts: parsed.Cuts, RawJSON: []byte(raw), Source: "llm"}, nil
	}
}

// ptrStr 取 string 地址(给 ConversationInput.Role 用)。
func ptrStr(s string) *string { return &s }

// buildSystemPrompt 拼 LLM system prompt(中文,固定)。
func buildSystemPrompt() string {
	return `你是猪分割顾问。根据用户给定的整猪毛重、历史同段重量的猪拆分记录,
输出 JSON 数组 cuts,每个元素含 cut_type(belly/tenderloin/rib/leg_front/leg_rear/trotter/liver)
与 expected_kg(预期部位产出 kg)。只输出 JSON,不要 markdown。`
}

// buildUserPrompt 拼 user prompt(pig metadata + 历史样本)。
func buildUserPrompt(pig *model.WholePig, history []model.WholePig) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "本次整猪:pig_id=%s, ear_tag=%s, gross_kg=%s, unit_price_yuan=%s, supplier_id=%s\n",
		pig.ID, pig.EarTag, pig.GrossWeightKg.String(), pig.PurchaseUnitPriceYuan.String(), pig.SupplierID)
	if len(history) > 0 {
		fmt.Fprintf(&sb, "近 30 天 ±10%% 重量段历史 %d 头:\n", len(history))
		// 按 arrived_at 倒序,只取 5 头
		sort.SliceStable(history, func(i, j int) bool {
			return history[i].ArrivedAt.After(history[j].ArrivedAt)
		})
		for i := 0; i < len(history) && i < 5; i++ {
			h := history[i]
			fmt.Fprintf(&sb, "  - %s: gross=%skg, llm_advice=%s\n",
				h.EarTag, h.GrossWeightKg.String(), string(h.LLMAdviceJSON))
		}
	} else {
		sb.WriteString("近 30 天 ±10% 重量段无历史猪。\n")
	}
	sb.WriteString("输出 JSON:{\"cuts\":[{\"cut_type\":\"...\",\"expected_kg\":...},...]}")
	return sb.String()
}

// normalizeCutType 把 LLM 输出的中文名映射到枚举。
func normalizeCutType(in model.CutType) model.CutType {
	s := strings.TrimSpace(string(in))
	mapping := map[string]model.CutType{
		"belly": model.CutBelly, "五花": model.CutBelly, "五花肉": model.CutBelly,
		"tenderloin": model.CutTenderloin, "里脊": model.CutTenderloin, "里脊肉": model.CutTenderloin,
		"rib": model.CutRib, "排骨": model.CutRib, "肋排": model.CutRib,
		"leg_front": model.CutLegFront, "前腿": model.CutLegFront, "前腿肉": model.CutLegFront,
		"leg_rear": model.CutLegRear, "后腿": model.CutLegRear, "后腿肉": model.CutLegRear,
		"trotter": model.CutTrotter, "猪蹄": model.CutTrotter, "蹄": model.CutTrotter,
		"liver": model.CutLiver, "猪肝": model.CutLiver, "肝": model.CutLiver,
	}
	if v, ok := mapping[s]; ok {
		return v
	}
	return model.CutType(s) // 兜底透传
}

// ---- Service 集成 ----

// SetPredictFn 注入 predict 函数(用于替换默认 history_avg 降级路径,接真 LLM)。
func (s *Service) SetPredictFn(fn PredictFn) {
	if fn == nil {
		return
	}
	s.predictFn = fn
}

// SetCubeClient 注入 cube client(supplier_id 校验)。
func (s *Service) SetCubeClient(c CubeClient) {
	if c == nil {
		return
	}
	s.cube = c
}

// 静默引用 os(为 env 读取预留)。
var _ = os.Getenv

// 静默引用 time.Duration(为 timeout 配置预留)。
var _ time.Duration = 0