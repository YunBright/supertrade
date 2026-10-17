// Package fact 定义"喂给 LLM 的结构化事实包"。
//
// # 为什么是"事实包"而不是让 LLM 自己查
//
// 本包的 Fact 是**唯一**进入 LLM 上下文的业务数据。它是确定性的、
// 一次查询拼装好的、长度受硬上限约束的。LLM 只负责"解读"，不负责"查询"。
//
// 理由（这是本设计最核心的权衡，不要"顺手"改成 function calling）：
//
//	 1. **token 可预测**。事实包大小随 TopN 线性增长，可预算。
//	    function calling 下 LLM 可能调 1 次也可能调 20 次，无法预算，
//	    而 LLM 调用是**按次付费**的（用户明确要求控制 token 成本）。
//	 2. **结构上不可能拉全量**。事实包的每一行都经过 Limit 截断，
//	    不存在"模型判断失误于是查了 10 万行"的可能。
//	    function calling 里，模型误判一次就是一次真实事故。
//	 3. **可审计**。事实包原文可直接落库，后台能逐字回看
//	    "这份结论是基于哪些数据得出的"。
//	 4. **延迟可控**。一次查询 vs 5~10 轮串行往返。
//
// # 保留的扩展位
//
// Fact.DataSufficiency 明确标出"哪些数据不够"，
// 将来若确实需要补查，可以基于这个字段加受限 function，
// 而不必重构整个链路。
package fact

import (
	"github.com/YunBright/supertrade/internal/salesinsight/calendar"
	"github.com/YunBright/supertrade/internal/salesinsight/weather"
)

// MinCostCoverage 是"成本覆盖率低于此值就不展示毛利"的阈值。
//
// ⚠️ 与 salesagg.MinCostCoverage 同值（0.5），但**独立定义**。
//   两处不共享常量是刻意的：salesagg 是展示层的既有行为（改了要同步
//   前端文案），本包是喂给 LLM 的事实。共享会让"为了 AI 调整阈值"
//   意外改变用户已在用的报表。改这里时**不要**顺手改那边，反之亦然。
const MinCostCoverage = 0.5

// Fact 是一次报告推理的完整输入。
type Fact struct {
	Meta Meta `json:"meta"`
	// Context 是日历与天气。
	Context Context `json:"context"`
	// Totals 是本期大盘。
	Totals Totals `json:"totals"`
	// Deltas 是与上期的对比。
	Deltas Deltas `json:"deltas"`
	// Categories 按销售额倒序，最多 DefaultTopN 条。
	Categories []Category `json:"categories"`
	// TopProducts 按销售额倒序，最多 DefaultTopN 条。
	TopProducts []TopProduct `json:"top_products"`
	// Suppliers 按销售额倒序，已剔除自用供应商，最多 DefaultTopN 条。
	Suppliers []Supplier `json:"suppliers"`
	// Stockouts 是"卖得好但没货"的商品，需要紧急补货。
	Stockouts []TopProduct `json:"stockouts"`
	// ExcludedSuppliers 是被排除的自用/客户类记录。
	//
	// ⚠️ 它们**保留在事实包里**而不是直接丢弃：
	//   后台要能看到"排除了谁、为什么排除"，
	//   而 LLM 也需要知道存在自采渠道，否则它会把
	//   "乐园公司直供生鲜"误读成"供应商 X 供货能力强"。
	ExcludedSuppliers []ExcludedSupplier `json:"excluded_suppliers,omitempty"`
	// DataSufficiency 说明数据是否够支撑结论。
	DataSufficiency Sufficiency `json:"data_sufficiency"`
	// RuleInsights 是**规则计算**的结论（确定性，可复现）。
	//
	// ⚠️ 它与 LLM 的产出并存而不是被替代：
	//   LLM 不可用时，报告仍然有内容（降级模式）；
	//   LLM 可用时，它给这份确定性结论"加解释"，而不是重算一遍。
	//   数字口径**永远**来自这里，不来自 LLM —— 见 prompt 里的硬约束。
	RuleInsights RuleInsights `json:"rule_insights"`
}

// Meta 是元信息。
type Meta struct {
	PeriodKind string `json:"period_kind"`
	PeriodLabel string `json:"period_label"`
	From        string `json:"from"`
	To          string `json:"to"`
	PrevFrom    string `json:"prev_from"`
	PrevTo      string `json:"prev_to"`
	BranchID    string `json:"branch_id"`
	BranchName  string `json:"branch_name,omitempty"`
	// GeneratedAt 是本事实包的生成时刻（RFC3339）。
	GeneratedAt string `json:"generated_at"`
	// CalendarDataVersion 让后台能看出节假日表是否需要更新。
	CalendarDataVersion string `json:"calendar_data_version"`
}

// Context 是日历与天气上下文。
type Context struct {
	From calendar.Day `json:"from"`
	To   calendar.Day `json:"to"`
	// PrevFrom 是上期首日的性质（用于环比叙述）。
	PrevFrom     calendar.Day `json:"prev_from"`
	Comparability calendar.Comparability `json:"comparability"`
	Weather      weather.Info `json:"weather"`
}

// Totals 是本期大盘。
type Totals struct {
	NetAmount float64 `json:"net_amount"`
	NetQty    float64 `json:"net_qty"`
	RefCost   float64 `json:"ref_cost"`
	// RefMargin 仅在 MarginAvailable=true 时有意义，否则是 0。
	RefMargin float64 `json:"ref_margin"`
	// RefMarginPct 同上；为 0 且 MarginAvailable=false 时**不可解读**。
	RefMarginPct float64 `json:"ref_margin_pct"`
	CostCoverage float64 `json:"cost_coverage"`
	// MarginAvailable 是调用方展示毛利的前提。
	//
	// ⚠️ hbposv10 实测覆盖率仅 25.3%，按 cost=0 算会显示"毛利率 100%"。
	//   一个"看起来完全正常、实际毫无意义"的数比不显示更坏 ——
	//   看到的人会据此判断"这个品类在赚钱"。
	MarginAvailable bool `json:"margin_available"`
}

// Deltas 是与上期的对比。
type Deltas struct {
	PrevNetAmount float64 `json:"prev_net_amount"`
	PrevNetQty    float64 `json:"prev_qty"`
	AmountAbs     float64 `json:"amount_abs"`
	// AmountPct 是环比百分比。
	//
	// ⚠️ 上期为 0 时它取 0 而不是 +Inf：
	//   一个无意义的巨大百分比会让卡片显示 "+∞%"，
	//   而它在 JSON 里会让下游解析失败或算出垃圾数字。
	//   此时应看 AmountAbs —— 这也是 Context.Comparability 存在的意义。
	AmountPct float64 `json:"amount_pct"`
	// HasPrev 为 false 表示**没有上期数据**（首次运行/新店）。
	//
	// ⚠️ 必须与 AmountPct=0 区分开：两者数字都是 0，
	//   但含义完全相反（"没跌" vs "没有可比对象"）。
	//   月报缺上期时不做叙事环比，就是靠这个字段。
	HasPrev bool `json:"has_prev"`
}

// Category 是一个品类的表现。
type Category struct {
	Name   string  `json:"name"`
	Amount float64 `json:"amount"`
	// AmountShare 是占大盘的比重（0~1，不是百分数）。
	AmountShare  float64 `json:"amount_share"`
	Rank         int     `json:"rank"`
	PrevAmount   float64 `json:"prev_amount"`
	DeltaAbs     float64 `json:"delta_abs"`
	DeltaPct     float64 `json:"delta_pct"`
	HasPrev      bool    `json:"has_prev"`
	PrevRank     int     `json:"prev_rank,omitempty"`
	RankChanged  bool    `json:"rank_changed,omitempty"`
}

// DeclineCause 是"销量下降"的归因。
//
// ⚠️ 这是本产品**最核心**的判断。把缺货说成滞销会导致店长砍掉爆款，
// 这是真实的经营损失，所以取值必须由**数据**决定，不能让 LLM 自由发挥。
type DeclineCause string

const (
	// CauseNone 没有下降
	CauseNone DeclineCause = "none"
	// CauseStockout **缺货**：有销量下滑但当前库存为 0。
	// 这是最重要的一个，也是最容易被误判成滞销的。
	CauseStockout DeclineCause = "stockout"
	// CauseLowStock 库存极低（低于 min_qty），可能已影响销售。
	CauseLowStock DeclineCause = "low_stock"
	// CauseMarketWide 整体下滑：该品类/全店同期也在跌，不是单品问题。
	CauseMarketWide DeclineCause = "market_wide"
	// CauseDemandDrop 单纯需求下降：有货、品类稳定，只有它在跌。
	CauseDemandDrop DeclineCause = "demand_drop"
	// CauseNewOrUnknown 数据不足（新商品/首次出现），不下结论。
	CauseNewOrUnknown DeclineCause = "unknown"
)

// StockStatus 是库存状态。
type StockStatus string

const (
	StockOK   StockStatus = "ok"
	StockLow  StockStatus = "low"
	StockOut  StockStatus = "out"
	StockNone StockStatus = "unknown" // 查不到库存记录
)

// TopProduct 是一个排行商品及其归因。
type TopProduct struct {
	ItemNo string `json:"item_no"`
	Name   string `json:"name"`
	Amount float64 `json:"amount"`
	Qty    float64 `json:"qty"`
	Rank   int    `json:"rank"`

	PrevAmount float64 `json:"prev_amount"`
	DeltaPct   float64 `json:"delta_pct"`
	HasPrev    bool    `json:"has_prev"`

	// CurrentStock 是当前库存；HasStock=false 时为 0 且不可解读。
	CurrentStock float64     `json:"current_stock"`
	HasStock     bool         `json:"has_stock"`
	StockStatus  StockStatus `json:"stock_status"`
	// MinStock 是安全库存线（源库 min_qty）。
	MinStock float64 `json:"min_stock"`

	// Category 是它所属的父类别 —— 归因"整体下滑"需要它。
	Category string `json:"category,omitempty"`

	// DeclineCause 与 DeclineEvidence 是**规则判定**的结果。
	//
	// ⚠️ Evidence 是**给人看的白话证据**，例如
	//   "当前库存 0，低于安全库存 12，近 7 天有 3 天缺货"。
	//   它存在的意义是让店长（和后台审计）能核对判断依据，
	//   而不是只看到一个 LLM 给出的标签。
	DeclineCause    DeclineCause `json:"decline_cause,omitempty"`
	DeclineEvidence string       `json:"decline_evidence,omitempty"`
}

// Supplier 是一个供应商的表现。
type Supplier struct {
	Code string `json:"code"`
	Name string `json:"name"`
	// Amount 是本期该供应商贡献的销售额。
	Amount float64 `json:"amount"`
	// Qty 是本期销量（称重重量）。
	Qty float64 `json:"qty"`
	// ProductCount 是本期卖过的商品数。
	ProductCount int `json:"product_count"`
	// PrevAmount 与 DeltaPct 用于判断供应商稳定性。
	PrevAmount float64 `json:"prev_amount"`
	DeltaPct   float64 `json:"delta_pct"`
	HasPrev    bool    `json:"has_prev"`
	// TopProducts 是该供应商贡献最高的几个商品（最多 SupplierTopN）。
	TopProducts []TopProduct `json:"top_products,omitempty"`
}

// ExcludedSupplier 是被排除的供应商及原因。
type ExcludedSupplier struct {
	Code string `json:"code"`
	Name string `json:"name"`
	// Reason 是给店长看的白话，例如 "自用/直供渠道，不参与供应商排名"。
	Reason string `json:"reason"`
	// Rule 是触发的规则标识（自用类型 / 客户身份 / 名称关键词）。
	Rule string `json:"rule"`
	// Amount 是它本期贡献的销售额 —— 保留它是为了让店长知道
	// "这部分销量走了自采渠道"，而不是从总量里凭空消失。
	Amount float64 `json:"amount"`
}

// ExclusionRule 是自用/客户供应商的排除规则标识。
type ExclusionRule string

const (
	// ExcludeNotSupplier cust_flag != 'S'：是客户不是供应商
	ExcludeNotSupplier ExclusionRule = "not_supplier"
	// ExcludeSelfUseType sup_type != '1'：自用/直供
	ExcludeSelfUseType ExclusionRule = "self_use_type"
	// ExcludeNameKeyword 名称命中自用关键词（可配置）
	ExcludeNameKeyword ExclusionRule = "name_keyword"
	// ExcludeConfigured 运营在配置里手动排除
	ExcludeConfigured ExclusionRule = "configured"
	// ExcludeManualList 命中本门店的**人工忽略名单**。
	//
	// ⚠️ 自 2026-10-17 起，这是运行时**唯一**生效的排除依据。
	//   上面三个规则（not_supplier / self_use_type / name_keyword）
	//   已降级为"维护名单时的推荐工具"（见 salesinsight/ignore.Suggest），
	//   不再在每次生成报告时执行 —— 那次"每次都跑的智能活"
	//   正是这次改造要省掉的开销。
	ExcludeManualList ExclusionRule = "manual_list"
)

// Sufficiency 说明数据是否够支撑结论。
type Sufficiency struct {
	Sufficient bool     `json:"sufficient"`
	Issues     []string `json:"issues,omitempty"`
}

// RuleInsights 是规则计算的确定性结论。
//
// # 为什么 LLM 不重算这些数字
//
// 因为 LLM 会算错。金额对比、环比、占比 —— 这些是算术，
// 而算术恰恰是 LLM 最容易出错的领域（尤其在几千个数字的上下文里）。
// 所以口径**永远**来自本结构，LLM 只负责把它翻译成人话并给出解释。
type RuleInsights struct {
	// StockoutProducts 是缺货商品的名字（Top3），用于卡片醒目提示。
	StockoutProducts []string `json:"stockout_products,omitempty"`
	// FastMovers 是销量增长最快的商品（Top3）。
	FastMovers []string `json:"fast_movers,omitempty"`
	// GrowingCategories 是销售额增长的品类（Top3）。
	GrowingCategories []string `json:"growing_categories,omitempty"`
	// DecliningCategories 是销售额下滑的品类（Top3）。
	DecliningCategories []string `json:"declining_categories,omitempty"`
	// BestSuppliers 是贡献最高的供应商（Top3，已排除自用）。
	BestSuppliers []string `json:"best_suppliers,omitempty"`
	// BestSupplierProducts 是"供应商单品更好"的直接证据（Top3）。
	BestSupplierProducts []SupplierProduct `json:"best_supplier_products,omitempty"`
}

// SupplierProduct 是"某个供应商的某个商品"这一对组合。
type SupplierProduct struct {
	Supplier string  `json:"supplier"`
	ItemNo   string  `json:"item_no"`
	Name     string  `json:"name"`
	Amount   float64 `json:"amount"`
	Qty      float64 `json:"qty"`
	// PrevAmount/DeltaPct 用于判断这个单品是否稳定。
	PrevAmount float64 `json:"prev_amount"`
	DeltaPct   float64 `json:"delta_pct"`
	HasPrev    bool    `json:"has_prev"`
}

// DefaultTopN 是事实包默认保留的排行条数。
//
// ⚠️ 取 10 的理由与 wecomreport.DefaultCardMaxRows 同源：
//   企业微信 markdown 卡片在手机上超过约 20 行就是一张要上下滑很久的图，
//   而 LLM 上下文越多成本越高且质量未必更好（长上下文里中间信息容易被忽略）。
//   20 是实测可接受的上限，10 留出汇总与结论的余量。
const DefaultTopN = 10

// MaxTopN 是硬上限。超过它的配置一律夹到这里。
//
// 它存在的意义是**防呆**：配置写错成 1000 时，
// 一次月报可能把整年商品塞进 prompt，账单会直接失控。
const MaxTopN = 20

// SupplierTopN 是每个供应商下钻保留的商品数。
const SupplierTopN = 3

// SummaryItemN 是 RuleInsights 各摘要列表的条数。
//
// ⚠️ 与 insight.MaxItems **刻意不共享常量**：
//   那是"输出给 LLM 的结论"的条数上限，这是"事实包里的确定性摘要"的条数。
//   两者可能需要不同步调整（改 prompt 的条数限制不该牵动数据层）。
//   两者都是 3，但含义不同 —— 这正是分开定义的理由。
const SummaryItemN = 3

// TopN 归一 TopN 配置。
func TopN(n int) int {
	if n <= 0 {
		return DefaultTopN
	}
	if n > MaxTopN {
		return MaxTopN
	}
	return n
}
