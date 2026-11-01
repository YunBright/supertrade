// Package service / gross_margin.go —— 按门店 + 营业日汇总鲜肉毛利。
//
// # 口径(2026-10-23 改:移动加权平均)
//
// **逐 SKU** 用移动加权平均算本期销售成本,再按部位汇总:
//
//	期初 = 昨日日终实盘(全店)× 昨日移动加权单价
//	入库 = 今日分割产出 kg + 其对应的猪成本分摊额
//	移动加权单价 = (期初金额 + 入库金额) / (期初 kg + 入库 kg)
//	期末金额 = 今日日终实盘(全店)× 本期移动加权单价
//	本期销售成本 = 期初金额 + 入库金额 − 期末金额 − 统收成本 − 报损成本
//	本期毛利 = 本期销售收入 − 本期销售成本
//
// 逐 SKU 的计算在 service/moving_average.go(computeSkuDailyCosts)。
//
// # 为什么换口径
//
// 旧口径把"今天卖的钱"按"今天进场的猪"摊成本 ⇒ **今天卖昨天剩的货时成本为 0、
// 毛利率 100%**(今天没进猪 / 今天的猪没出该部位 ⇒ costForCut=0 而 ratio=1)。
// 旧口径里**没有"昨天"的概念**。详见 moving_average.go 的包注释。
//
// # 全店口径
//
// 毛利用**全店**(猪肉台 + 冷库)。位置只影响"可售量"查询,不影响成本:
// 转库只是物理位置变化,成本跟着货走。
//
// # 对账行
//
// 报告带一行 **Reconciliation**:期初 + 入库 == 销售 + 统收 + 报损 + 期末。
// 差额只允许来自**显式化的损耗**(切分损耗 / 隔夜失重·盘亏),两者都在 Losses 里单列。
// ⇒ 毛利准不准一眼可判,不再靠人肉对账。
//
// # data_source
//
//   - actual    当日有 is_complete=true 的盘点单
//   - partial   有盘点单但 is_complete=false
//   - estimated 当日无盘点单
//
// # 退货
//
// OrderStatus='R' 或 qty<0 的行**不计入** revenue / sold / 成本。
//
// 全程不引入 tenant_id。
package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
)

// 盘点口径三态(2026-10-26 收敛成常量 —— 之前是散落的字面量,
// 很容易在新增分支时写错/漏判,导致"推演值"被当成"实盘")。
const (
	// dataSourceActual = 当日有**完整**盘点单(is_complete=true)⇒ 期末量是实盘。
	dataSourceActual = "actual"
	// dataSourcePartial = 有盘点单但 is_complete=false ⇒ 期末量是**部分实盘**。
	dataSourcePartial = "partial"
	// dataSourceEstimated = 当日无盘点单 ⇒ 期末量是**账面推演**。
	dataSourceEstimated = "estimated"
)

// GrossMarginReport 单店单日毛利总览。
type GrossMarginReport struct {
	BranchID            string `json:"branch_id"`
	Date                string `json:"date"` // YYYY-MM-DD(bizTZ)
	IsStocktakeComplete bool   `json:"is_stocktake_complete"`
	// DataSource 是**盘点**口径(actual | partial | estimated)——
	// 它说的是"期末剩余量是实盘还是推演",**与销售来源无关**。
	DataSource string `json:"data_source"`
	// SalesSource 是**销售**口径 —— 这次的收入/销量是从哪读出来的。
	//
	// ⚠️ 为什么必须显式给出(2026-10-26 门店反馈):
	//   有源门店(店 00/01)的销售在**它自己的 cube 源**里;
	//   无源门店(店 10)的销售在本地 PG(本地 POS 的 sale.paid 事件)。
	//   ⇒ 门店看到"没有销售数据"时,第一反应是"今天真没卖"——
	//   而真相可能是"数据源没读到" ⇒ **必须让门店看见来源**。
	//
	// 取值:cube(有源,读 cube sales_report)| local(无源,读本地 line_sales_by_pigs)。
	SalesSource string `json:"sales_source"`
	// SalesSourceNote 是给门店看的**人话**说明(前端直接显示)。
	SalesSourceNote string `json:"sales_source_note,omitempty"`
	// PigsRecorded 是当天(营业日)**进场猪的头数**。
	//
	// ⚠️ 为什么前端需要它(2026-10-30 owner):
	//   「今天还没录猪」时,这张表的数字是**临时的**:
	//   入库量按 0 算、销售成本按**昨天**的移动加权单价估、期末是账面短卖(负数)
	//   ⇒ 门店必须先知道"今天还没录猪",别把临时数当成结论。
	//
	// ⚠️ 前端**不能靠入库量判断**:录了猪但没补录分割时,入库量同样是 0
	//   ⇒ 只能由后端给出"今天到底有没有进猪"。
	//
	// ⇒ 0 = 当天没录猪 ⇒ 前端显示那段解释;>0 ⇒ 隐藏(已经录了就不用再提醒)。
	PigsRecorded int                 `json:"pigs_recorded"`
	PerCut       []PerCutGrossMargin `json:"per_cut"`
	// PerSku 是**货号级销售明细** —— 只承载"货号粒度真实存在"的量
	// (产出 / 销量 / 营收 / 统收 / 报损)。
	//
	// ⚠️ 2026-10-27:成本链改**部位级** ⇒ **计算单元是部位行**(`per_cut`),
	//   库存 / 成本 / 毛利**不在货号行里**(那些键是**缺失**,不是 0)
	//   ⇒ 门店要看成本看部位行,货号行只回答"这个货号卖了多少、卖了多少钱"。
	PerSku []PerSkuGrossMargin `json:"per_sku"`
	Total  TotalGrossMargin    `json:"total"`
	// CostIncomplete = true ⇒ 当天**有部位的成本算不出来**(既没有期初库存、
	// 当天也没有进货、又没有任何可用单价)⇒ 那部分销售成本必然是 0。
	//
	// ⚠️ **这不是"成本为 0",是"不知道成本"** —— 两者必须分开:
	//   报成 0 ⇒ 门店看到"毛利 96%"⇒ 会以为赚疯了。
	//   (2026-10-27 生产实测:店 00 从 9 月起没有期初结转也没有进货,
	//    那些天的成本恒 0 ⇒ 毛利看起来 100%。)
	//
	// ⇒ 前端要**显式提示**,别让"算不出来"伪装成"成本是 0"。
	CostIncomplete bool `json:"cost_incomplete,omitempty"`
	// CostIncompleteNote 是给门店看的**人话**说明(前端直接显示)。
	CostIncompleteNote string `json:"cost_incomplete_note,omitempty"`
	// Reconciliation 成本对账行。
	Reconciliation ReconciliationRow `json:"reconciliation"`
	// Losses 显式化的损耗(以前这些 kg 完全不在任何表里,成本凭空消失)。
	Losses LossSummary `json:"losses"`
	// PromoAllocation 特价的**日终缺口分摊**明细。
	//
	// ⚠️ 归一后的**部位行已经含**分摊进来的特价收入/重量
	// (分摊成功时 promo 桶不再单列,避免重复计收入);
	// 分摊失败(IsEstimated=true)时特价整体留在 promo 桶里,金额重量仍然可见。
	PromoAllocation *PromoAllocation `json:"promo_allocation,omitempty"`
	// Alerts 是**红字告警**(门店口径:「修改值造成负库存时,需要在每日毛利的 alert 上
	// 红字显示」)⇒ 前端直接标红,不要吞掉。
	Alerts []MarginAlert `json:"alerts,omitempty"`
	// ⚠️ 2026-11-01 owner：预期毛利(预售结转)端点下线 ⇒ `presale` 明细与 `presale_hint`
	//   悬浮提示两节随之删除(它们的数据源是已退役的预售结转视图)。
	//   部位行 / 汇总行上的 `presale_margin_yuan`、`presale_expected_revenue_yuan`、
	//   `presale_expected_unit_yuan` **仍然保留** —— 那是账面字段(期末结余按预期售价重估的结果)，
	//   不是预售结转视图；下一张卡才随账面链一起清理。
	GeneratedAt time.Time `json:"generated_at"`
}

// MarginAlert 每日毛利的**红字告警**。
type MarginAlert struct {
	// Level:error(红字)|warning(黄字)。
	Level string `json:"level"`
	// Code:negative_inventory(负库存)|balance_mismatch(余额与按日推导不一致)。
	Code    string `json:"code"`
	Message string `json:"message"`
	// CubeProductID / Kg 便于前端定位到具体 SKU。
	CubeProductID string          `json:"cube_product_id,omitempty"`
	Kg            decimal.Decimal `json:"kg,omitempty"`
}

// 告警级别。
const (
	// MarginAlertLevelError 红字(必须处理)。
	MarginAlertLevelError = "error"
	// MarginAlertLevelWarning 黄字(需要看一眼)。
	MarginAlertLevelWarning = "warning"
)

// 告警码。
const (
	// MarginAlertNegativeInventory 库存为负(负库存)。
	MarginAlertNegativeInventory = "negative_inventory"
	// MarginAlertBalanceMismatch 当前余额与"按营业日推导"的余额不一致。
	MarginAlertBalanceMismatch = "balance_mismatch"
	// MarginAlertSplitWeightUnbalanced 分割权重**没能重分配**:
	// 实际分割超出建议,但其它部位没有可压缩量(没处可挪)⇒ 今天仍按建议分割算成本。
	//
	// ⚠ 必须让门店看见:否则门店会以为"今天的进货就是实际切出来的量"。
	MarginAlertSplitWeightUnbalanced = "split_weight_unbalanced"
)

// PerCutGrossMargin 单部位的毛利明细(由其下各 SKU 汇总)。
type PerCutGrossMargin struct {
	CutType   model.CutType   `json:"cut_type"`
	OpeningKg decimal.Decimal `json:"opening_kg"`
	// PurchasedKg 是**本期入库量**(kg)—— 2026-10-29 起 = **调整后权重 W_c**
	// (成本基准,见 cut_split_algo.go;语义仍是"进货")。
	PurchasedKg decimal.Decimal `json:"purchased_kg"`
	SoldKg      decimal.Decimal `json:"sold_kg"`
	BulkSoldKg  decimal.Decimal `json:"bulk_sold_kg"`
	WasteKg     decimal.Decimal `json:"waste_kg"`
	// ActualRemainKg 是**猪肉台(柜台)实盘**(kg);**nil = 当天没有完整实盘**。
	//
	// ⚠️ 必须用**指针**:decimal.Decimal 是 struct,encoding/json 的 omitempty
	// 对 struct **无效** ⇒ 用值类型时零值也会序列化出 "actual_remain_kg":"0"
	// ⇒ 前端拿到 0 就会显示"实盘剩余 0"(门店会误读成"我盘出来是 0 kg")。
	// 指针 nil ⇒ 整个键缺失 ⇒ 前端显示「—」。
	ActualRemainKg *decimal.Decimal `json:"actual_remain_kg,omitempty"`
	// ExpectedRemainKg 是**柜台口径的账面应有**(kg)—— 任何口径下都有值。
	//
	// ⚠️ 2026-10-29 owner 口径变更:以前是**全店**应有(引擎 ExpectedClosingKg),
	//   现在 = **全店应有 − 冷库量** ⇒ 与 ActualRemainKg(柜台实盘)**同口径**
	//   ⇒ variance_kg = 实盘 − 应有 才不是"柜台减全店"的胡说。
	//
	// ⚠️ 引擎内部的 ExpectedClosingKg(全店)**没动**(成本/短卖告警/对账都靠它)。
	ExpectedRemainKg decimal.Decimal `json:"expected_remain_kg"`
	// CutShortSellKg 是该部位**账面不足**的量(kg)—— 只作提示,**不进链**。
	//
	// = max(0, 卖出+统收+报损 + 期末 − 可售量)
	// ⚠️ 它**不等于**"这天真短卖":真短卖看的是**总量口径**(见 Total.ShortSellKg)。
	//   生产实测 店 00 2026-10-29:fat 等 8 个部位各有账面不足,但总量够 ⇒ 不是短卖。
	CutShortSellKg decimal.Decimal `json:"cut_short_sell_kg"`
	// VarianceKg = 实盘 − 应有;**nil = 当天没有完整实盘**(恒 0 是**假的**"对上了")。
	VarianceKg *decimal.Decimal `json:"variance_kg,omitempty"`
	// ---- 部位级分割数据(2026-10-29;owner 定案,前端按这些键渲染)----
	//
	// SuggestedSplitKg 是该部位的**建议分割**(kg)—— AI/算法建议
	// (统计基线 p50 优先、LLM 兜底),**不含 pig_cuts 实录**。
	SuggestedSplitKg decimal.Decimal `json:"suggested_split_kg"`
	// ActualSplitKg 是该部位的**实际分割**(kg);nil ⇒ 前端显示空。
	//
	// ⚠️ 必须用**指针**(同上:omitempty 对 decimal.Decimal 无效)。
	// ⚠️ 显示规则(owner 定案):
	//	recorded(实录)⇒ 总是给值;
	//	derived(倒推)⇒ 有完整日终盘点 ⇒ 给值;
	//	无完整日终盘点 且 实际 < 建议 ⇒ 留空(前端显示「—」)。
	ActualSplitKg *decimal.Decimal `json:"actual_split_kg,omitempty"`
	// ActualSplitSource:recorded(当日 pig_cuts 实录)| derived(倒推)。
	ActualSplitSource string `json:"actual_split_source"`
	// AdjustedSplitKg 是**调整后权重**(kg)—— 成本基准(= 本期入库量)。
	AdjustedSplitKg decimal.Decimal `json:"adjusted_split_kg"`
	// BulkRevenueYuan 是该部位当日的**统收金额**(元)—— 营收角标气泡用。
	//
	// ⚠️ `revenue_yuan` **已经含**它(零售 + 统收)⇒ 别重复加。
	BulkRevenueYuan decimal.Decimal `json:"bulk_revenue_yuan"`
	// CounterActualKg 是该部位当日的**猪肉台实盘**(kg)
	// (来自完整盘点单;没有完整盘点 ⇒ 0)。
	CounterActualKg decimal.Decimal `json:"counter_actual_kg"`
	// ColdStoreKg 是该部位当日的**冷库量**(kg)
	// (盘点单的冷库余额;没有盘点单 ⇒ 退回引擎链值)。
	ColdStoreKg decimal.Decimal `json:"cold_store_kg"`
	// ColdStoreUnitCostYuan 是**冷库库存部位单价**(元/kg)
	// = 该部位的**移动加权单价**(owner:「冷库单价不是新的部位单价,是移动加权单价」)。
	ColdStoreUnitCostYuan decimal.Decimal `json:"cold_store_unit_cost_yuan"`
	// ---- 猪肉台口径的三件新数据(2026-10-30 owner 定案)----
	//
	// ⚠️ 背景:owner 看报表后说「这张表(甚至整个鲜肉-毛利看板页)都是**猪肉台**的数据」
	//   ⇒ ① 「期初」列(账面链值,可以是负数 = 账面短卖)看不懂 ⇒ 改成「搬上台」;
	//     ② 「冷库单价」这一列不涉及冷库 ⇒ 换成「单价」(建议单价 / 实际均价 两行)。
	//
	// CounterLoadKg 是该部位当日**搬上台**的重量(kg)—— 门店自己搬的量。
	CounterLoadKg decimal.Decimal `json:"counter_load_kg"`
	// SuggestedUnitCostYuan 是**建议单价**(元/kg):
	// 「如果今天的猪按**建议分割**切,这刀货的成本单价是多少」。
	//
	// ⚠️ nil = 当天**没有建议分割**(没进猪)⇒ 前端显示「—」(不是 0)。
	SuggestedUnitCostYuan *decimal.Decimal `json:"suggested_unit_cost_yuan,omitempty"`
	// ActualAvgUnitPriceYuan 是**实际均价**(元/kg):
	// 当日这个部位**实际卖出**的平均单价 = 营收 ÷ 销量
	// (零售来自思迅日汇总表 t_rm_daysum;统收是本地登记,一并计入
	//  ⇒ 与「营收」列**自洽**:均价 × 销量 = 营收,门店能自己验算)。
	//
	// ⚠️ nil = 当天这个部位**没卖** ⇒ 前端显示「—」。
	ActualAvgUnitPriceYuan *decimal.Decimal `json:"actual_avg_unit_price_yuan,omitempty"`
	// SuggestedCostUnitPriceYuan 是**建议成本单价**(元/kg)——
	// 「按**分割基线的单价**切出来的这刀货,这个部位应该值多少钱」。
	//
	// ⚠️ 口径(2026-10-31 owner:「分割基线加入单价」):
	//
	//	单价基线(部位进货单价) × (这头猪的进价 ÷ 基线的参考猪进价)
	//	⇒ 基线是 14.6/kg 的猪、今天这头是 16/kg ⇒ 各部位单价按比例上涨。
	//
	// ⚠️ nil = 这个部位**取不到价**(没有单价基线、也没有整店均价、也没有 LLM 缓存)
	//   ⇒ 前端显示「—」(不是 0)。
	SuggestedCostUnitPriceYuan *decimal.Decimal `json:"suggested_cost_unit_price_yuan,omitempty"`
	// SuggestedCostPriceSource 是建议成本单价的来源:
	// baseline(单价基线)| store_avg(门店整店加权平均价)| llm_cache(LLM 缓存单价)| none(没有)。
	//
	// ⚠️ 门店要看得见这个价**怎么来的** ⇒ 前端写在悬浮提示里。
	SuggestedCostPriceSource string `json:"suggested_cost_price_source,omitempty"`
	// ActualCostUnitPriceYuan 是**实际成本单价**(元/kg)= 销售成本 ÷ 销量。
	//
	// ⚠️ 分母是**总销量**(零售 + 统收)⇒ 与「零售单价」同一分母
	//   ⇒ 门店能自己验算:成本单价 × 销量 = 销售成本。
	//
	// ⚠️ nil = 当天这个部位**没卖** ⇒ 前端显示「—」。
	ActualCostUnitPriceYuan *decimal.Decimal `json:"actual_cost_unit_price_yuan,omitempty"`
	// ---- 日终剩余 / 售罄率 / 建议毛利（2026-10-31 owner 定案）----
	//
	// DayEndRemainKg 是该部位**猪肉台日终盘点的剩余量**(kg)。
	//
	// ⚠️ **nil = 当天没有完整日终盘点** ⇒ 前端显示「—」
	//   （不是 0:0 会被门店误读成"我盘出来是 0 kg"）。
	// ⚠️ 口径 = **柜台(猪肉台)实盘**,**不含冷库**(owner 知情选定)。
	DayEndRemainKg *decimal.Decimal `json:"day_end_remain_kg,omitempty"`
	// DayEndRemainYuan 是该部位**日终剩余的金额**(元)—— 「猪肉台日终盘点总值」。
	//
	// ⚠️ 折算口径(owner 亲定):**零售均价 ÷ (1 + 今日毛利率)** ⇒ 倒推出进货单价,
	//   再 × 日终剩余 kg ⇒ 柜台上还压着多少钱。
	// ⚠️ 与单价日样本的倒推公式**同一条**(见 forecast_price.go)
	//   ⇒ 「日终剩余估值」与「单价基线」不会各算各的。
	//
	// ⚠️ nil = 取不到折算价(没卖过 + 全店也算不出来) ⇒ 前端显示「—」。
	DayEndRemainYuan *decimal.Decimal `json:"day_end_remain_yuan,omitempty"`
	// SellThroughRate 是该部位的**售罄率**(0~1)。
	//
	// ⚠️ 口径(2026-10-31 owner 改):**销售 ÷ (销售 + 猪肉台日终剩余)**。
	//   旧口径 sold/purchased 会 >100%(生产实测 店 00 10-30 = 100.75%)
	//   ⇒ 因为销售里含**冷库**货、当天进货里没有 ⇒ 新口径用**日终盘点**兜住分母
	//   ⇒ 结果恒 ≤ 100%。
	//
	// ⚠️ nil = 当天没有完整日终盘点(分母算不出来) ⇒ 前端显示「—」。
	SellThroughRate *float64 `json:"sell_through_rate,omitempty"`
	// ---- 参考口径（2026-11-01 owner 终稿）----
	//
	// ⚠️ **owner 2026-11-01 明确要求删掉「建议毛利 / 建议毛利率」**:
	//	它们的定义就是「用建议成本单价再倒推一遍毛利」——而单价列已经给了,
	//	且数学上 m 进去、m′ 出来 ⇒ **永远对不上 header** ⇒ 纯属重复且误导。
	//	⇒ 部位毛利改成**参考口径**(下面三个字段)。
	//
	// ReferenceCostUnitPriceYuan 是该部位的**参考成本单价**(元/kg)
	// = **零售均价 ÷ (1 + 当日总毛利率)**(owner 亲定)。
	//
	// ⚠️ **完全不需要分割重量假设** ⇒ 切割不准也不会污染它
	//	(账面口径的移动加权单价依赖期初快照 ⇒ 一次历史漏记会被放大成假短卖)。
	//
	// ⚠️ nil = 取不到价(既没卖过、全店也没有采购均价)⇒ 前端显示「—」。
	ReferenceCostUnitPriceYuan *decimal.Decimal `json:"reference_cost_unit_price_yuan,omitempty"`
	// ReferenceCostPriceSource 是参考成本单价的来源:
	// retail_backout(零售均价÷(1+m))| purchase_avg(全店采购均价兜底)| none(没有)。
	//
	// ⚠️ 门店要看得见这个价**怎么来的** ⇒ 前端写在悬浮提示里。
	ReferenceCostPriceSource string `json:"reference_cost_price_source,omitempty"`
	// ReferenceGrossMarginYuan 是该部位的**参考毛利**(元):
	//	营收 − 成本单价×销量 − 搭赠成本 − 报损成本。
	//
	// ⚠️ **与全店 header 的毛利不必相等**(owner:「header不需要与部位同口径」):
	//	部位的**产出是基线估算的**,而 header 是总采/总售/总剩余算出来的
	//	⇒ 两者本就不该有恒等关系。
	// ⚠️ nil = 这个部位取不到成本单价 ⇒ 算不出来 ⇒ 前端显示「—」。
	ReferenceGrossMarginYuan *decimal.Decimal `json:"reference_gross_margin_yuan,omitempty"`
	// ReferenceGrossMarginPct 是**参考毛利率**(%)—— 分母 = 营收。
	ReferenceGrossMarginPct *float64 `json:"reference_gross_margin_pct,omitempty"`
	// ValueCoefficient 是该部位的**价值系数**(2026-11-01 owner 拍板):
	//	该部位的单位重量市场价值 ÷ 整扇猪分割后所有可售部位的平均单位重量价值。
	//
	// ⚠️ >1 = 这个部位比全店平均**更值钱**
	//	(生产实测 店 00 10-30:猪肚 2.998 / 肋排 2.294 / 五花 0.800 / 猪油 0.301)。
	// ⚠️ nil = 算不出来(当天没有「有产出且有价」的部位可作分母,
	//   或该部位当天没卖过、取不到零售均价)⇒ 前端显示「—」(不是 0)。
	//
	// ⚠️ 分母是**产出加权**的平均 ⇒ 详见 value_coefficient.go 里的恒等式说明
	//   (owner 原话里的「加权价值产出」按字面算恒等于产出总重 ⇒ 已改成产出估值)。
	ValueCoefficient *decimal.Decimal `json:"value_coefficient,omitempty"`
	RevenueYuan      decimal.Decimal `json:"revenue_yuan"`
	// ⚠️ 改名(2026-10-23):旧字段 `purchase_cost_yuan` 是"按消耗比例摊销的猪采购成本";
	// 新字段是**本期销售成本**(移动加权结转)—— 两者语义不同,不是同一个数。
	// 旧的"摊销采购成本"在"今天没进猪的部位今天在卖"时会算成 0 ⇒ 毛利虚高。
	SalesCostYuan decimal.Decimal `json:"sales_cost_yuan"`
	// BulkCostYuan 是**统收成本**(卖给酒店的大宗品项的结转成本)。
	//
	// ⚠️ 2026-10-23 增:原来它没进毛利,导致"卖板油给酒店"被算成**零成本收入**
	// (毛利率 100%)—— 实测:卖 10kg 收 60 元、成本 1000 元,毛利显示 +60,
	// 真实应是 **−940**。现在它显式成列,毛利 = 收入 − 销售成本 − 统收成本 − 报损成本。
	BulkCostYuan decimal.Decimal `json:"bulk_cost_yuan"`
	// WasteCostYuan 是**报损成本**(扔掉的货的成本)。
	//
	// ⚠️ 同上:原来报损的成本**凭空消失**(不进损益)⇒ 报损不体现为损失。
	WasteCostYuan decimal.Decimal `json:"waste_cost_yuan"`
	// GiftKg 是**搭赠**的量(kg)—— 2026-10-31 owner 拍板。
	//
	// ⚠️ 搭赠 = 买肉送猪肝这类「有成本、无收入」的出库(**没过称、没过账**):
	//	它是链上的**第四个出口** ⇒ 不记它,账上就成了"盘亏/短卖"。
	GiftKg decimal.Decimal `json:"gift_kg"`
	// GiftCostYuan 是**搭赠成本**(元)。
	//
	// ⚠️ 它**不在** SalesCostYuan 里(销售成本只含"真卖掉的货"的成本)
	//	⇒ 毛利 = 营收 − 销售成本 − 统收成本 − 报损成本 − **搭赠成本**。
	GiftCostYuan    decimal.Decimal `json:"gift_cost_yuan"`
	GrossMarginYuan decimal.Decimal `json:"gross_margin_yuan"`
	GrossMarginPct  float64         `json:"gross_margin_pct"`
	// CostIncomplete = true ⇒ 该部位当天**成本算不出来**
	// (既没有期初库存、当天也没有进货、又没有任何可用单价)
	// ⇒ 那部分销售成本必然是 0 ⇒ **毛利被高估**。
	//
	// ⚠️ 这是"不知道成本",不是"成本为 0" ⇒ 前端必须显式提示。
	CostIncomplete bool `json:"cost_incomplete,omitempty"`
	// PresaleMarginYuan 是该部位的**预期毛利(预售结转)**金额。
	//
	// ⚠️ 它**已经含在 GrossMarginYuan 里**(审计日的结余按预期售价重估),
	//   这一列只是把它**单独列出来**给门店看。
	// ⚠️ 它是**预期值**(没有现金进来),不是"多卖出来的钱"。
	PresaleMarginYuan decimal.Decimal `json:"presale_margin_yuan,omitempty"`
	// PresaleExpectedRevenueYuan 是预售的**预期收入**(同样不是真实现金收入)。
	PresaleExpectedRevenueYuan decimal.Decimal `json:"presale_expected_revenue_yuan,omitempty"`
	// MovingAvgUnitCost 是该部位的加权平均成本(元/kg)。
	MovingAvgUnitCost decimal.Decimal `json:"moving_avg_unit_cost"`
}

// TotalGrossMargin 全店汇总。
type TotalGrossMargin struct {
	RevenueYuan   decimal.Decimal `json:"revenue_yuan"`
	SalesCostYuan decimal.Decimal `json:"sales_cost_yuan"`
	// BulkCostYuan / WasteCostYuan 与部位行同义(统收成本 / 报损成本)。
	BulkCostYuan  decimal.Decimal `json:"bulk_cost_yuan"`
	WasteCostYuan decimal.Decimal `json:"waste_cost_yuan"`
	// GiftCostYuan 是**搭赠成本**(元)—— 2026-10-31 owner 拍板(扣毛利、单列一行)。
	GiftCostYuan    decimal.Decimal `json:"gift_cost_yuan"`
	GrossMarginYuan decimal.Decimal `json:"gross_margin_yuan"`
	GrossMarginPct  float64         `json:"gross_margin_pct"`
	// PresaleMarginYuan / PresaleExpectedRevenueYuan 与部位行同义(预期毛利结转)。
	PresaleMarginYuan          decimal.Decimal `json:"presale_margin_yuan,omitempty"`
	PresaleExpectedRevenueYuan decimal.Decimal `json:"presale_expected_revenue_yuan,omitempty"`
	// ---- 短卖(2026-10-30 owner 口径)----
	//
	// ShortSellKg 是**当日总量短卖**(kg)——
	// 口径 = max(0, Σ卖出+统收+报损 − Σ期初+进货)(owner:「总量够就不算短卖」)。
	//
	// ⚠️ **分部位的账面不足不算**(那是分摊口径问题)⇒
	//   生产实测 店 00:10-29 = 0(总量 191.7kg ≥ 卖出 178.1kg);
	//   10-30 = 62.512kg(账面只剩 20.9kg 却卖了 83.4kg)。
	// ⇒ >0 ⇒ ① 那部分货**必须计成本**(否则销售成本 = 0 ⇒ 毛利虚高)
	//   ② 结转给下一天(累计短卖)。
	//
	// ⚠️ 2026-11-01 owner：**短卖拦截规则废止**
	//   (原「任何一日出现短卖都不允许审批结算」—— 它的拦截点随审计端点一起下线)
	//   ⇒ 不再拦结算、不再描红 ⇒ 这里只留**成本**与**传递**两件事。
	ShortSellKg decimal.Decimal `json:"short_sell_kg"`
	// CarriedShortSellKg 是**从上一期结转过来的累计短卖**(kg)。
	//
	// ⚠️ 短卖**绝不回写成负库存**(期初仍 ≥0)⇒ 它只是链上的累计口径,不是库存。
	CarriedShortSellKg decimal.Decimal `json:"carried_short_sell_kg"`
	// ---- 日终剩余 / 售罄率 / 建议毛利（2026-10-31 owner 定案）----
	//
	// DayEndRemainKg 是**猪肉台日终盘点**的全店剩余量(kg)—— 各部位之和。
	//
	// ⚠️ 口径 = **柜台实盘**,**不含冷库**(owner 知情选定)。
	// ⚠️ **nil = 当天没有完整日终盘点** ⇒ 前端显示「—」
	//   (不是 0:0 会被门店误读成"柜台上一点货都没有")。
	DayEndRemainKg *decimal.Decimal `json:"day_end_remain_kg,omitempty"`
	// DayEndRemainYuan 是**日终剩余总值**(元)—— 各部位金额之和。
	//
	// ⚠️ 折算口径(owner 亲定):**零售均价 ÷ (1 + 今日毛利率)** ⇒ 倒推进货单价 × kg。
	// ⚠️ nil = 没有完整盘点(或取不到折算价)⇒ 前端显示「—」。
	DayEndRemainYuan *decimal.Decimal `json:"day_end_remain_yuan,omitempty"`
	// SellThroughRate 是全店**售罄率**(0~1)= 销售 ÷ (销售 + 日终剩余)。
	//
	// ⚠️ nil = 当天没有完整日终盘点 ⇒ 前端显示「—」(不是 100%)。
	SellThroughRate *float64 `json:"sell_through_rate,omitempty"`
	// ---- 参考口径（2026-11-01 owner 终稿）----
	//
	// ⚠️ **owner 2026-11-01 明确要求删掉「建议毛利 / 建议毛利率」** ⇒ 见下面这一组。
	//
	// ReferenceGrossMarginYuan 是**全店参考毛利**(元):
	//
	//	营收 − 当日采购总额 + 期末猪肉台剩余成本
	//
	// ⚠️ **期初剩余 = 0**:夜里柜台清空、货回冷库 ⇒ **冷库才是跨日结转的位置**
	//   ⇒ 冷库归「长期毛利」(owner:「冷库余额链…在长期毛利里」),单日口径不含它。
	// ⚠️ **剩余 → 0 时 m = (营收 − 采购)/营收 ⇒ 精确**(owner 的收敛判据)。
	// ⚠️ **与各部位参考毛利之和不相等**(owner:「header不需要与部位同口径」)⇒ 正常。
	// ⚠️ nil = 当天没有完整日终盘点(剩余算不出来)⇒ 前端显示「—」(不是 0)。
	ReferenceGrossMarginYuan *decimal.Decimal `json:"reference_gross_margin_yuan,omitempty"`
	// ReferenceGrossMarginPct 是**全店参考毛利率**(%)。
	ReferenceGrossMarginPct *float64 `json:"reference_gross_margin_pct,omitempty"`
	// ReferenceMarginRate 是**当日总毛利率** m(小数,0.22 = 22%)——
	// 倒推部位成本单价的那个输入。nil = 算不出来(同上)。
	ReferenceMarginRate *decimal.Decimal `json:"reference_margin_rate,omitempty"`
	// ReferenceCostPartial = true ⇒ **有部位取不到参考成本单价**
	// ⇒ 各部位参考毛利是**不完整**的(前端要提示"部分部位未计入")。
	ReferenceCostPartial bool `json:"reference_cost_partial,omitempty"`
	// ---- 参考口径的输入(让门店能自己验算)----
	//
	// PurchasedKg / PurchasedYuan 是当日进店的猪的合计(总采)。
	PurchasedKg   decimal.Decimal `json:"purchased_kg"`
	PurchasedYuan decimal.Decimal `json:"purchased_yuan"`
	// PurchaseAvgYuan 是**采购均价**(元/kg)= 采购金额 ÷ 采购重量
	// ⇒ 它是**期末剩余成本**的估值价(owner:「可以用采购均价计算」)。
	PurchaseAvgYuan decimal.Decimal `json:"purchase_avg_yuan"`
	// ReferenceRemainKg / ReferenceRemainYuan 是**期末猪肉台剩余**(kg/元)
	// —— **不含冷库**(owner:「总剩余不含冷库」)。
	ReferenceRemainKg   decimal.Decimal `json:"reference_remain_kg"`
	ReferenceRemainYuan decimal.Decimal `json:"reference_remain_yuan"`
	// YieldRate 是**出成率** = 真实切分产出 ÷ 当日进货毛重。
	//
	// ⚠️ **> 100% ⇒ 有问题,要去查**(owner 2026-11-01):
	//	真实产出是**台面恒等式**解出来的(纯事实:台面实盘 + 销售 + 搭赠 + 统收 + 报损 − 搬上台)
	//	⇒ >100% 说明**搬台没登记**或**盘点漏了部位**(生产实测 店 00 10-30 = 106.4%)。
	// ⚠️ nil = 算不出来(没有完整日终盘点 / 当天没进猪)⇒ 前端显示「—」。
	YieldRate *float64 `json:"yield_rate,omitempty"`
	// ---- 价值系数 / 产出估值(2026-11-01 owner)----
	//
	// OutputValueYuan 是**产出估值**(元) = Σ(该部位实际产出重量 × 该部位零售均价)。
	//
	// 含义:**今天切出来的货,按零售价算值多少钱**(含还没卖掉的、损耗的、搭赠的)
	//	⇒ 与当日营收对照,差值就是「切出来但还没变成现金」的那部分价值。
	// 生产实测 店 00:10-29 = 3892.61 / 10-30 = 4761.13 / 10-31 = 4341.13。
	//
	// ⚠️ nil = 算不出来(当天没有「有产出且有价」的部位)⇒ 前端显示「—」。
	OutputValueYuan *decimal.Decimal `json:"output_value_yuan,omitempty"`
	// OutputAvgUnitValueYuan 是**整扇猪分割后所有可售部位的平均单位重量价值**(元/kg)
	// = Σ(kg×price) ÷ Σ(kg) —— 也就是「价值系数」那个分母。
	//
	// 生产实测 店 00 10-30 = 19.944 元/kg。
	// ⚠️ nil = 算不出来(同 OutputValueYuan)⇒ 前端显示「—」。
	OutputAvgUnitValueYuan *decimal.Decimal `json:"output_avg_unit_value_yuan,omitempty"`
}

// ComputeGrossMargin 按 (branchID, day) 算毛利。
//
// day 是任一时刻;取 day 在 bizTZ 下的日期作为营业日。
func (s *Service) ComputeGrossMargin(ctx context.Context, branchID string, day time.Time) (*GrossMarginReport, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	// ⚠️ **走同一条链**(2026-10-27):期初 = 昨天的期末(链式接续)⇒
	//   与长期毛利同口径 ⇒ 「长期毛利 = 每日毛利之和」这个不变量成立。
	//   有已物化快照时直接用前一日快照当期初 ⇒ O(1);没有就回溯到链起点。
	chainDays, err := s.computeCutChainDays(ctx, branchID, day, day)
	if err != nil {
		return nil, err
	}
	if len(chainDays) == 0 {
		return nil, fmt.Errorf("%w: 该营业日无数据", ErrInvalidInput)
	}
	cd := chainDays[len(chainDays)-1]
	cutRows, skuRows := cd.cutRows, cd.skuRows
	recon, losses := cd.recon, cd.losses
	promoAlloc := allocatePromo(promoRowsOf(cutRows))

	// ---- 参考口径的当日输入(总采 + 期末猪肉台剩余)----
	//
	// ⚠️ 这两项**都来自分割之外的来源**(猪 / 盘点单)⇒ 切割不准也不影响参考毛利。
	// ⚠️ 采集失败**不阻断**报表(账面口径那一套还能出数)⇒ 只降级成"参考口径不可算"。
	refTotals, refErr := s.referenceDayTotalsOf(ctx, branchID, day, cutRows)
	if refErr != nil {
		s.pubLogger.Warn("ComputeGrossMargin: 采集参考口径输入失败(参考毛利显示「—」)",
			"branch_id", branchID, "biz_date", day.In(s.bizTZOrUTC()).Format("2006-01-02"), "err", refErr)
		refTotals = ReferenceDayTotals{}
	}

	perCut, total := rollUpPerCutCuts(cutRows, promoAlloc, refTotals)

	// ---- 价值系数 / 产出估值(2026-11-01 owner)----
	//
	// ⚠️ 就地写回 perCut 每行的 ValueCoefficient(切片共享底层数组 ⇒ 可见)。
	// ⚠️ 这两个数**依赖分割产出**(actual_split_kg 多为倒推值)⇒ 切不准会跟着偏,
	//   详见 value_coefficient.go 顶部的前提说明。
	vc := applyValueCoefficients(perCut)
	total.OutputAvgUnitValueYuan = vc.AvgUnitValueYuan
	total.OutputValueYuan = vc.OutputValueYuan

	dataSource := dataSourceEstimated
	isComplete := false
	for _, r := range cutRows {
		if r.DataSource == dataSourceActual {
			dataSource = dataSourceActual
			isComplete = true
			break
		}
		if r.DataSource == dataSourcePartial && dataSource != dataSourceActual {
			dataSource = dataSourcePartial
		}
	}

	// ⚠️ 2026-11-01 owner：预期毛利(预售结转)端点下线 ⇒ 这里不再挂「预售明细」与
	//   「悬浮提示」两节(它们的唯一数据源 `ListPresale` 已随端点退役)。
	//   账面口径不变：部位行/汇总行上的 `presale_*` 字段仍按预期售价重估填充。

	// 销售来源(2026-10-26):让门店看得见"收入是从哪读出来的"。
	salesSource, salesSourceNote, err := s.salesSourceOf(ctx, branchID)
	if err != nil {
		return nil, err
	}

	// ⚠️ "成本算不出来"(≠"成本为 0")要显式告诉门店。
	//
	//	某部位既没有期初库存、当天也没有进货、又没有任何可用单价
	//	⇒ 那部分销售成本必然是 0 ⇒ **毛利被高估**
	//	⇒ 若不提示,店长看到"毛利 96%"会以为赚疯了(生产实测:店 00 的 10-01)。
	costIncomplete := false
	var incompleteCuts []string
	for _, r := range cutRows {
		if r.CostUncomputable {
			costIncomplete = true
			incompleteCuts = append(incompleteCuts, string(r.CutType))
		}
	}
	costIncompleteNote := ""
	if costIncomplete {
		sort.Strings(incompleteCuts)
		shown := incompleteCuts
		if len(shown) > 6 {
			shown = shown[:6]
		}
		costIncompleteNote = fmt.Sprintf(
			"有 %d 个部位的成本**算不出来**(既没有期初库存、当天也没有进货)⇒ 这部分成本按 0 计,毛利被高估;补录期初结转或当天进货后才准。部位:%s",
			len(incompleteCuts), strings.Join(shown, "、"))
	}

	// 当天进猪头数(2026-10-30):前端据此提示"今天还没录猪 ⇒ 数字是临时的",
	// 录了猪就隐藏(见 GrossMarginReport.PigsRecorded 的说明)。
	pigsRecorded, err := s.countPigsForDay(ctx, branchID, day)
	if err != nil {
		return nil, err
	}

	return &GrossMarginReport{
		BranchID:            branchID,
		Date:                day.In(s.bizTZOrUTC()).Format("2006-01-02"),
		IsStocktakeComplete: isComplete,
		DataSource:          dataSource,
		SalesSource:         salesSource,
		SalesSourceNote:     salesSourceNote,
		PigsRecorded:        pigsRecorded,
		PerCut:              perCut,
		// ⚠️ SKU 明细**只承载销售 / 营收 / 分割产出**(2026-10-27 口径):
		//   库存 / 成本 / 毛利是**部位口径** ⇒ SKU 行里**不返**(键缺失)。
		PerSku:             perSkuGrossMarginsOf(skuRows),
		Total:              total,
		CostIncomplete:     costIncomplete,
		CostIncompleteNote: costIncompleteNote,
		Reconciliation:     *recon,
		Losses:             *losses,
		PromoAllocation:    promoAlloc,
		Alerts:             s.buildCutMarginAlerts(ctx, branchID, day, cutRows),
		GeneratedAt:        s.now(),
	}, nil
}

// countPigsForDay 数某店当天(营业日窗口内)**进场的猪**。
//
// ⚠️ 口径与引擎一致:营业日窗口 = [当天 00:00 bizTZ, +24h)(见 businessDayBounds)
//
//	⇒ 猪按**到店时间**归属,不用 UTC 日期硬比。
//
// # 为什么要有它(2026-10-30 owner)
// 「今天还没录猪」时报表的数字是**临时的**(入库=0、成本按昨天的进价估、
// 期末是账面短卖)⇒ 前端要据此给门店一句提示,录了猪就隐藏。
//
// ⚠️ 前端**不能靠入库量判断**:录了猪但没补录分割时,入库量同样是 0
//
//	⇒ 只能由后端告诉它"今天到底有没有进猪"。
func (s *Service) countPigsForDay(ctx context.Context, branchID string, day time.Time) (int, error) {
	start, end := s.businessDayBounds(day)
	var n int64
	if err := s.db.WithContext(ctx).Model(&model.WholePig{}).
		Where("branch_id = ? AND arrived_at >= ? AND arrived_at < ?", branchID, start, end).
		Count(&n).Error; err != nil {
		return 0, fmt.Errorf("count whole_pigs(gross margin): %w", err)
	}
	return int(n), nil
}

// sortPerCut 按 cut_type 字符串升序(输出稳定,便于 e2e 断言)。
func sortPerCut(rows []PerCutGrossMargin) {
	for i := 1; i < len(rows); i++ {
		j := i
		for j > 0 && string(rows[j-1].CutType) > string(rows[j].CutType) {
			rows[j-1], rows[j] = rows[j], rows[j-1]
			j--
		}
	}
}
