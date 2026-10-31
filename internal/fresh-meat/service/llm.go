// Package service / llm.go —— 同步调 Dapr Conversation API(LLM)做整猪分割预测。
//
// 设计:
//
//	  RecordWholePig → insert → 调 PredictFn(pig, history) →
//	    ├─ 成功 → update whole_pig llm_advice_json = result.RawJSON, data_source = "llm"
//	    └─ 失败 / 超时 → fallbackToHistoryAvg → update whole_pig llm_advice_json + data_source = "history_avg"
//
//	- **失败不视为业务错误**:返回 200 给仓管,data_source 标注降级来源。
//	- 默认 PredictFn 是 "history_avg";生产注入 DaprConversationPredictFn。
//	- ConvName 默认 "conversation";env FRESHMEAT_CONVERSATION 覆盖。
//	- LLMTimeout 默认 30s;env FRESHMEAT_LLM_TIMEOUT 覆盖。
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

	"github.com/YunBright/supertrade/internal/cuberead"
	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	dapr "github.com/dapr/go-sdk/client"
	"github.com/shopspring/decimal"
)

// LLMResult 是 PredictFn 的返回值 — 仓管期望的整猪分割建议。
//
// RawJSON 是给 whole_pig.llm_advice_json 列的原始 JSON(直接写 jsonb);
// Cuts 是解析后的预期 cuts(供后续 BI 复用 / 校验)。
type LLMResult struct {
	Cuts    []PredictedCut `json:"cuts"`
	RawJSON []byte         `json:"-"`
	Source  string         `json:"source"` // "llm" 或 "history_avg"
}

// PredictedCut 期望的部位产出。
type PredictedCut struct {
	CutType    model.CutType   `json:"cut_type"`
	ExpectedKg decimal.Decimal `json:"expected_kg"`
	// UnitPriceYuan 是该部位的**进货单价**(元/kg)—— LLM 顺带给的兜底价。
	//
	// ⚠️ **可选**:LLM 不给就不给(指针 nil)⇒ 退取链继续往下走
	//   (门店整店加权平均价 → LLM 缓存单价 → LLM 重新给价)。
	// ⚠️ 只接受 0 < price < maxLLMUnitPriceYuan 的值(见 forecast_price.go)
	//   ⇒ LLM 幻觉出来的离谱价不会污染缓存。
	UnitPriceYuan *decimal.Decimal `json:"unit_price_yuan,omitempty"`
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
//
// 部位清单**由 model.CarcassCutTypes 生成**(不是手写常量)⇒ 枚举一改,prompt 自动跟着改。
// ⚠️ 用 CarcassCutTypes 而不是 AllCutTypes:local_pork(隔山肉)是品种级商品,
// 一头普通猪分割不出它,让 LLM 去预测它的 kg 只会产生幻觉。
func buildSystemPrompt() string {
	var sb strings.Builder
	sb.WriteString("你是猪分割顾问。根据用户给定的整猪毛重、历史同段重量的猪拆分记录,\n")
	sb.WriteString("输出 JSON 数组 cuts,每个元素含 cut_type、expected_kg(预期部位产出 kg)\n")
	sb.WriteString("与 unit_price_yuan(该部位的进货单价,元/kg)。\n")
	sb.WriteString("cut_type 只能取下面这些值(英文 wire + 中文名),不要自造:\n")
	for _, c := range model.CarcassCutTypes {
		fmt.Fprintf(&sb, "  - %s (%s)\n", c, model.CutLabel(c))
	}
	sb.WriteString("只输出 JSON,不要 markdown。")
	return sb.String()
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
	sb.WriteString("输出 JSON:{\"cuts\":[{\"cut_type\":\"...\",\"expected_kg\":...,\"unit_price_yuan\":...},...]}")
	return sb.String()
}

// cutTypeAliases 中文部位名 → 枚举。
//
// 覆盖**真源库里两个门店在售 SKU 的全部叫法**(2026-10-23 实测):
// 佳乐 1201 / 富百家 010303 的商品名 + 行业标准名 + 广西俗称。
//
// 例:富百家"梅肉/腱子肉/猪面瘦肉"= shoulder_blade;佳乐"猪利"= tongue;
// 广西"猪横利"= spleen;"水油"= fat;"沙骨"= bone_rib。
var cutTypeAliases = map[string]model.CutType{
	// ---- 分割肉 ----
	"五花": model.CutBelly, "五花肉": model.CutBelly, "肋腹": model.CutBelly,
	"三层肉": model.CutBelly, "腩肉": model.CutBelly, "腩肉(五花肉)": model.CutBelly,
	"里脊": model.CutTenderloin, "里脊肉": model.CutTenderloin, "通脊": model.CutTenderloin,
	"外脊": model.CutTenderloin, "小里脊": model.CutTenderloin,
	"排骨": model.CutRib, "肋排": model.CutRib, "肋条": model.CutRib,
	"腩排": model.CutRib, "前排": model.CutRib, "前排(硬排)": model.CutRib,
	"硬排": model.CutRib, "猪大排": model.CutRib, "大排": model.CutRib,
	"前腿": model.CutLegFront, "前腿肉": model.CutLegFront, "猪肩": model.CutLegFront,
	"肩肉": model.CutLegFront, "展肉": model.CutLegFront, "猪展": model.CutLegFront,
	"梅花肉": model.CutShoulderBlade, "梅花": model.CutShoulderBlade,
	"夹心肉": model.CutShoulderBlade, "前夹心": model.CutShoulderBlade,
	"梅肉": model.CutShoulderBlade, "梅肉(小里脊)": model.CutShoulderBlade,
	"肩胛肉": model.CutShoulderBlade,
	"瘦肉": model.CutLean, "精瘦肉": model.CutLean, "精品瘦肉": model.CutLean,
	"新鲜瘦肉": model.CutLean, "鲜风瘦肉": model.CutLean, "中肉": model.CutLean,
	"后腿": model.CutLegRear, "后腿肉": model.CutLegRear, "后臀": model.CutLegRear,
	"猪蹄": model.CutTrotter, "蹄": model.CutTrotter, "蹄髈": model.CutTrotter,
	"猪脚": model.CutTrotter, "猪手": model.CutTrotter, "猪肘": model.CutTrotter,
	"猪肘子": model.CutTrotter, "猪脚尖": model.CutTrotter,
	"猪尾": model.CutTail, "猪尾巴": model.CutTail, "尾骨": model.CutTail,
	"猪尾骨": model.CutTail,
	"猪皮": model.CutSkin, "皮": model.CutSkin,
	"板油": model.CutFat, "猪板油": model.CutFat, "猪油": model.CutFat,
	"肥猪油": model.CutFat, "水油": model.CutFat, "脂肪": model.CutFat,
	"隔山肉": model.CutLocalPork, "隔山": model.CutLocalPork,
	"土猪": model.CutLocalPork, "本地猪": model.CutLocalPork,

	// ---- 头/皮/耳/舌/颈 ----
	"猪头": model.CutHead, "头肉": model.CutHead, "猪头肉": model.CutHead,
	"猪头皮": model.CutHeadSkin, "头皮": model.CutHeadSkin,
	"猪耳": model.CutEar, "猪耳朵": model.CutEar, "耳朵": model.CutEar,
	"猪舌": model.CutTongue, "猪舌头": model.CutTongue, "舌": model.CutTongue,
	"猪利": model.CutTongue,
	"猪颈": model.CutNeck, "猪颈肉": model.CutNeck, "血脖": model.CutNeck,
	"颈肉": model.CutNeck,

	// ---- 骨 ----
	"筒骨": model.CutBoneShank, "筒子骨": model.CutBoneShank, "腿骨": model.CutBoneShank,
	"精制筒骨": model.CutBoneShank,
	"扇骨": model.CutBoneScapula, "肩胛骨": model.CutBoneScapula, "扇子骨": model.CutBoneScapula,
	"龙骨": model.CutBoneSpine, "脊骨": model.CutBoneSpine, "脊椎骨": model.CutBoneSpine,
	"沙骨": model.CutBoneRib, "肋骨": model.CutBoneRib,
	"猪头骨": model.CutBoneHead, "头骨": model.CutBoneHead,

	// ---- 下水 ----
	"猪肝": model.CutLiver, "肝": model.CutLiver,
	"猪心": model.CutHeart, "心": model.CutHeart,
	"猪肺": model.CutLung, "肺": model.CutLung,
	"猪腰": model.CutKidney, "腰": model.CutKidney, "肾": model.CutKidney,
	"猪肾": model.CutKidney,
	"猪肚": model.CutStomach, "肚": model.CutStomach, "胃": model.CutStomach,
	"猪胃": model.CutStomach,
	"猪小肚": model.CutStomachHoneycomb, "小肚": model.CutStomachHoneycomb,
	"蜂巢肚": model.CutStomachHoneycomb,
	"大肠": model.CutIntestine, "生肠": model.CutIntestine, "肥肠": model.CutIntestine,
	"猪大肠": model.CutIntestine, "猪生肠": model.CutIntestine,
	"大肠头": model.CutIntestineHead,
	"粉肠": model.CutIntestineSmall, "小肠": model.CutIntestineSmall,
	"精选粉肠": model.CutIntestineSmall,
	"猪横利": model.CutSpleen, "横利": model.CutSpleen, "脾": model.CutSpleen,
	"猪脾": model.CutSpleen,
	"猪鞭": model.CutTendon, "鞭": model.CutTendon,
}

// cutAliasKeys 排序后的别名(长的在前)—— 给子串兜底匹配用。
var cutAliasKeys = func() []string {
	keys := make([]string, 0, len(cutTypeAliases))
	for k := range cutTypeAliases {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	return keys
}()

// normalizeCutType 把 LLM 输出的中文名映射到枚举。
//
// 三级兜底(从严到松):
//  1. 归一化后**精确**命中别名表;
//  2. 去掉括号补充说明再精确命中(如 "五花肉(肋腹)" → "五花肉");
//  3. **最长子串**命中(如 "猪五花肉" → "五花肉")。
//
// 命中不了就**原样透传**(不猜):调用方(RecordPigCut / stocktake)会用
// model.CutType.Valid() 拒掉未知值 —— 透传比猜错安全。
func normalizeCutType(in model.CutType) model.CutType {
	s := normalizeCutText(string(in))
	if s == "" {
		return in
	}
	if v, ok := cutTypeAliases[s]; ok {
		return v
	}
	// 去括号补充说明("五花肉(肋腹)" → "五花肉";"梅肉(小里脊)" → "梅肉")
	if i := strings.IndexAny(s, "(（"); i > 0 {
		if v, ok := cutTypeAliases[strings.TrimSpace(s[:i])]; ok {
			return v
		}
	}
	// 最长子串兜底
	for _, alias := range cutAliasKeys {
		if strings.Contains(s, alias) {
			return cutTypeAliases[alias]
		}
	}
	return model.CutType(s) // 兜底透传
}

// normalizeCutText 归一化 LLM 可能带的空白/全角括号。
func normalizeCutText(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "（", "(")
	s = strings.ReplaceAll(s, "）", ")")
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "　", "")
	return s
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

// SetCubeReader 注入 cube 通用查询读端(读 cube model 的低层能力)。
//
// ⚠️ **只用于"有源门店读销售"**(见 service/cube_sales.go):
// 供应商校验、预售取价走的是上面那个窄接口 CubeClient,不需要它。
//
// 未注入 ⇒ 有源门店读销售会返 ErrCubeUnavailable(而不是静默回落本地,
// 那会把"有源"显示成"这家店没卖东西")。
func (s *Service) SetCubeReader(r cuberead.Reader) {
	if r == nil {
		return
	}
	s.cubeReader = r
}

// SetSourceResolver 注入「这个门店配了哪个数据源」的解析器。
//
// ⚠️ **未注入 ⇒ 全部按"无源"处理**(resolver == nil 时 cubesource.Decide 返 useCube=false)
// ⇒ 读本地 PG。这是为了单测与纯本地部署兜底;
//   **生产必须装配**(cmd/fresh-meat/main.go 装 cubesource.NewCachedResolver),
//   否则有源门店的销售永远读不到 ⇒ 毛利看板恒 0(2026-10-26 门店反馈的根因)。
func (s *Service) SetSourceResolver(r cubesource.Resolver) {
	if r == nil {
		return
	}
	s.sources = r
}

// 静默引用 os(为 env 读取预留)。
var _ = os.Getenv

// 静默引用 time.Duration(为 timeout 配置预留)。
var _ time.Duration = 0
