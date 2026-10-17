package ignore

import (
	"sort"
	"strings"

	"github.com/YunBright/supertrade/internal/salesinsight/fact/supplier"
)

// Suggestion 是一条推荐（维护名单时的便利工具）。
type Suggestion struct {
	// Code / Name 来自供应商字典。
	Code string `json:"code"`
	Name string `json:"name"`
	// Reason 是人话理由，直接展示给运营看。
	Reason string `json:"reason"`
	// Rule 是命中依据（cust_flag / type / name_keyword），便于人工复核。
	Rule string `json:"rule"`
	// Confidence 是置信度 0~1，仅用于排序与提示，**不参与判定**。
	Confidence float64 `json:"confidence"`
}

// SuggestKeywords 是名称关键词匹配。
//
// # 判定只用 Name，不用 Code
//
// 编码在思迅里是 supcust_no（如 "019"、"038"），没有业务含义。
// 运营看到的是名称，所以按名称给推荐更符合"看懂了再勾"的流程。
var SuggestKeywords = []string{
	"自购", // 自购蔬菜
	"自营", // 水果（自营）
	"自产", // 自产
	"乐园", // 乐园公司（集团/门店内部）
	"内部", // 内部周转
	"自采",
	"厂配",
	"集采",
}

// Suggest 对一批字典记录给出推荐。
//
// # ⚠️ 它**只是工具**，不参与运行时判定
//
// 保留它的理由：人工从几百条供应商里手工挑出自用单元很费劲，
// 而这份推荐能把候选缩到十几条，人只需复核与勾选。
//
// 但**不能**让它在每次生成报告时自动生效 —— 那正是我们要去掉的
// 每次都跑的智能活（见 package 注释）。
//
// ⚠️ 顺序有讲究：先判"是不是供应商"，再判"是不是自用"。
//
// 一个 cust_flag='C' 的客户若名字里恰好有"自"（例如"自营物业"），
// 先跑关键词会把它标成自用，而真实原因是"它根本不是供应商" ——
// 两者对运营的解释完全不同。
//
// # 保守方向
//
// 排除一条真供应商（丢失优质货源线索）的代价 > 多推荐一条让人多看一眼。
// 所以不确定的一律不推荐（不猜），只把明确的推出来。
func Suggest(recs []supplier.Record) []Suggestion {
	out := make([]Suggestion, 0, 8)
	for _, r := range recs {
		if s, ok := suggestOne(r); ok {
			out = append(out, s)
		}
	}
	// 置信度高��前 —— 运营先看最可能的那几条。
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Confidence != out[j].Confidence {
			return out[i].Confidence > out[j].Confidence
		}
		return out[i].Code < out[j].Code
	})
	return out
}

// suggestOne 对单条记录给出推荐。
func suggestOne(r supplier.Record) (Suggestion, bool) {
	code := normalizeCode(r.Code)
	name := r.Name
	if code == "" {
		return Suggestion{}, false
	}

	// ---- 先判：它到底是不是供应商 ----
	//
	// ⚠️ 只认 'C'。'A' 不短路：实测唯一一条 A 是 special/内部周转，
	//   它同时 type='xx'，语义明确是内部周转；
	//   若 A 直接放行，会退化成"名字里有个内部"这种脆弱判据。
	if strings.ToUpper(strings.TrimSpace(r.CustFlag)) == "C" {
		return Suggestion{
			Code: code, Name: name, Rule: "cust_flag",
			Reason:     "这是向我们采购的客户，不是供货方",
			Confidence: 0.95,
		}, true
	}

	// ---- 再判：是不是自用/直供 ----
	//
	// ⚠️ 用 != '1' 而不是 == '5'：实测存在 'xx' 异常值
	//   （special/内部周转），把它当普通供应商是错的。
	switch typ := strings.TrimSpace(r.Type); typ {
	case "1":
		// 普通供应商，继续看名称
	case "":
		// 类型缺失：不猜。
		return Suggestion{}, false
	default:
		reason := "非常规供应商类型（" + typ + "），建议按自用处理"
		if typ == "5" {
			reason = "自用/直供渠道（门店或集团内部采购）"
		}
		return Suggestion{
			Code: code, Name: name, Rule: "type",
			Reason: reason, Confidence: 0.9,
		}, true
	}

	// ---- 最后：名称关键词 ----
	//
	// ⚠️ 这一层置信度最低：它是纯字符串匹配，
	//   实测 `自购蔬菜` type='1'（普通供应商）却语义上是自营，
	//   但换个门店"自营"可能只是个品牌名。
	for _, k := range SuggestKeywords {
		if k == "" {
			continue
		}
		if strings.Contains(name, k) {
			return Suggestion{
				Code: code, Name: name, Rule: "name_keyword",
				Reason:     "名称含自采标识（" + k + "），可能是门店自营采购单元",
				Confidence: 0.6,
			}, true
		}
	}
	return Suggestion{}, false
}
