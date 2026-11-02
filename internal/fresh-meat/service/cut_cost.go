// Package service / cut_cost.go —— **部位级**移动加权成本引擎(2026-10-27)。
//
// # 为什么从 SKU 级改成部位级
//
// 门店口径一直就是部位(见 model.go / CutStockDaily / SkuStockOpening 的注释):
//
//	「鲜肉只管部位数据,不管 SKU;SKU 映射只是为了把销售、数量等数据聚合到其部位……
//	  日终盘点也是记录部位。库存,大宗结算都是部位。」
//
// 而旧的成本链(`moving_average.go` 的 `computeSkuDailyCosts`)是 **SKU 主键** ⇒ 两套粒度打架:
//
//  1. `overlayStockOpenings` 把**部位**的期初结转 100% 记到该部位的「主 SKU」,
//     同部位其它 SKU 的期初被清零 ⇒ 那个货号账面恒为 0;
//  2. 账面 0 的货号卖出 ⇒ 移动加权单价 0 ⇒ **销售成本 0 ⇒ 毛利率 100%**(毛利虚高);
//  3. `expectedCloseKg` 负数裁 0 ⇒ 缺口被抹掉 ⇒ 部位"应有"反而变正
//     ⇒ 门店看到"进货 4.7、销售 16.9、应有 5.0"这种**自相矛盾**的行。
//
// ⇒ 本文件把**成本计算单元**换成部位:库存 / 结余 / 期初 / 成本链全是部位,
// SKU 只承载「卖了多少、卖了多少钱」。
//
// # 部位级引擎的计算式
//
//	期初(部位)   = 上一营业日的部位级成本快照结余
//	              → 回退: closingForDay(D−1) 部位结余 kg × 部位移动加权单价
//	              → 回退: sku_stock_openings 期初结转(**按部位直接用**,不再落 SKU)
//	入库(部位)   = 今日分割产出 kg + 其猪成本分摊额
//	可售金额     = 期初金额 + 入库金额
//	可售数量     = 期初 kg + 入库 kg
//	移动加权单价 = 可售金额 / 可售数量
//	期末(部位)   = 有盘点 → closingForDay(D)(柜台 + 冷库)
//	              无盘点 → 期初 + 入库 − 销售 − 统收 − 报损    ← **不裁 0**
//	期末金额     = 期末 kg × 移动加权单价
//	销售成本     = 可售金额 − 期末金额 − 统收成本 − 报损成本      ← **不裁 0**
//	毛利         = 收入 − 销售成本 − 统收成本 − 报损成本 + 预期毛利
//
// ⚠️ **两处"不裁 0"是本次改造的核心**:
//   - 期末为负 ⇒ 该部位**账面短卖**(卖超了库存)⇒ 短卖部分照摊成本 ⇒ 毛利不再虚高;
//   - 销售成本为负 ⇒ 只可能是"期末实盘 > 可售金额"(盘盈)⇒ 裁 0 并记日志。
//
// # 恒等式
//
//	Σ部位期初金额 + Σ部位入库金额 == Σ销售成本 + Σ统收成本 + Σ报损成本 + Σ期末存货金额
//
// ⚠️ 期末金额**可以为负**(账面欠货)⇒ 对账行里也会是负数 ⇒ 这是真相,不是 bug。
package service

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
)

// CutDailyCost 某店某营业日**某部位**的移动加权成本明细(成本链的计算单元)。
type CutDailyCost struct {
	BranchID string
	CutType  model.CutType

	OpeningKg     decimal.Decimal
	OpeningAmount decimal.Decimal
	// ProducedKg 是**入库量**(= 调整后权重 W_c,见 cut_split_algo.go)。
	ProducedKg decimal.Decimal
	// ProducedAmount 是**入库金额**(当日可摊猪成本按 W 的比例重分)。
	ProducedAmount decimal.Decimal
	SoldKg         decimal.Decimal
	SoldAmount     decimal.Decimal
	BulkSoldKg     decimal.Decimal
	BulkSoldAmount decimal.Decimal
	WasteKg        decimal.Decimal
	WasteAmount    decimal.Decimal
	// GiftKg 是**搭赠**的量(kg)—— 2026-10-31 owner 拍板。
	//
	// ⚠️ 搭赠是「有成本、无收入」的出库(买肉送猪肝这类**没过称、没过账**的):
	//	它是链上的**第四个出口**(销售/统收/报损之外),
	//	要① 从可售量里扣掉(否则被当成盘亏/短卖)② 单独计成本(扣毛利、单列一行)。
	GiftKg       decimal.Decimal
	GiftCostYuan decimal.Decimal
	// GiftGapKg 是**账面应有 − 实盘**的缺口(kg)—— **疑似搭赠**(2026-10-31)。
	//
	// ⚠️ 判据是**该部位被盘过**(盘点单里出现过)⇒ 没盘过的部位没有缺口可言。
	// ⚠️ 它 = 搭赠 + 真盘亏 ⇒ **必须让门店确认**(见 service.ListGiftSuggestions)。
	//   确认成搭赠 ⇒ 记一条 waste_logs(waste_type='gift')⇒ 缺口归零 ⇒ 不再提示。
	GiftGapKg decimal.Decimal
	// GiftGapActualKg 是上面那个缺口对应的**实盘量**(kg,柜台实盘 + 冷库量)。
	GiftGapActualKg decimal.Decimal

	// ExpectedClosingKg 是**账面应有**期末量(**不裁 0** ⇒ 负数 = 账面短卖)。
	//
	// ⚠️ 它是**账面推演**,永远可能是负数 ⇒ 报表用它提示"卖超了账面库存"。
	//   但**期末量(ClosingKg)不裁 0** —— 链上永远是 ≥0(见下)。
	ExpectedClosingKg decimal.Decimal
	// ClosingKg 是期末量(有盘点 = 实盘;无盘点 = 账面推演**裁 0**)。
	//
	// ⚠️ **不为负**(owner 2026-10-30:「期初不能有负数」):
	//   盘到的部位(含 0 kg)用实盘;没盘到的用 max(账面推演, 0)
	//   ⇒ 链上永远是 ≥0 ⇒ 下一天的期初不会是负数。
	ClosingKg     decimal.Decimal
	ClosingAmount decimal.Decimal

	// ---- 短卖(2026-10-30 owner 口径)----
	//
	// CutShortSellKg 是**该部位账面不足**的量(kg)—— 只作提示,**不进链**。
	//
	// = max(0, 卖出+统收+报损 + 期末 − 可售量)
	// ⚠️ 典型场景(生产实测 店 00 2026-10-29):AI 的建议分割给 fat 只分了 5.073kg,
	//   门店当天卖了 8.383kg ⇒ 账面差 3.311kg ⇒ **但总量够**(进货 181.7 ≥ 卖出 178.1)
	//   ⇒ 这是**分摊口径**问题,不是真短卖 ⇒ 不该传递到下一天。
	CutShortSellKg decimal.Decimal
	// ShortSellKg 是**当日总量**短卖中分摊到该部位的量(kg)——
	// 口径 = max(0, Σ卖出+统收+报损 − Σ期初+进货)(owner:「总量够就不算短卖」)。
	//
	// ⚠️ 生产实测 店 00:10-29 = 0(总量够);10-30 = 62.512(账面只剩 20.9kg 却卖了 83.4kg)。
	// ⇒ >0 ⇒ ① 那部分货**必须计成本**(否则销售成本 = 0 ⇒ 毛利虚高)
	//   ② 结转给下一天(累计短卖)。
	//
	// ⚠️ 2026-11-01 owner：**短卖拦截规则废止**
	//   (原「任何一日出现短卖都不允许审批结算」—— 它的拦截点随审计端点一起下线)
	//   ⇒ 不再拦结算、不再描红。
	ShortSellKg decimal.Decimal
	// CarriedShortSellKg 是**从上一期结转过来**的短卖(kg)—— 累计口径。
	//
	// ⚠️ **绝不回写成负库存**:短卖只进"累计短卖"这个数,期初仍然是 ≥0。
	CarriedShortSellKg decimal.Decimal

	MovingAvgUnitCost decimal.Decimal
	SalesCostYuan     decimal.Decimal
	BulkCostYuan      decimal.Decimal
	WasteCostYuan     decimal.Decimal
	RevenueYuan       decimal.Decimal
	GrossMarginYuan   decimal.Decimal
	GrossMarginPct    float64

	PresaleMarginYuan          decimal.Decimal
	PresaleExpectedRevenueYuan decimal.Decimal
	// PresaleExpectedUnitYuan 是该部位的**预期销售单价**(元/kg)—— 加权出来的。
	PresaleExpectedUnitYuan decimal.Decimal

	// DataSource:actual | partial | estimated
	DataSource string

	// CostUncomputable = true ⇒ 该部位当期**成本算不出来**(既没有期初库存、
	// 当天也没有进货,且没有任何可用单价)⇒ 销售成本必然是 0。
	//
	// ⚠️ **这不是"成本为 0",是"不知道成本"** —— 两者必须分开:
	//   报成 0 ⇒ 门店看到"毛利 100%" ⇒ 会以为赚疯了。
	//   (2026-10-27 生产实测:店 00 从 9 月起没有任何期初结转、也没有进货,
	//    整月成本恒 0 ⇒ 期间毛利看起来 = 期间营收。)
	//
	// ⇒ 报表层要**显式标注**这种天/部位,别让"算不出来"伪装成"成本是 0"。
	CostUncomputable bool

	// ---- 部位级分割数据(2026-10-29;owner 定案)----
	//
	// SuggestedSplitKg 是**建议分割** S_c(kg):纯算法建议(基线 p50 → LLM 兜底),
	// **不含 pig_cuts 实录**。
	SuggestedSplitKg decimal.Decimal
	// ActualSplitKg 是**实际分割** A_c(kg);nil ⇒ 按显示规则隐藏(前端显示空)。
	//
	// ⚠ **内部值**在引擎里是全量(隐藏与否只影响报表展示层),
	//   但重分配用的是**内部值**(owner:「用内部的 A_c,不是显示值」)。
	ActualSplitKg *decimal.Decimal
	// ActualSplitSource:recorded(实录)| derived(倒推)。
	ActualSplitSource string
	// AdjustedSplitKg 是**调整后权重** W_c(kg)—— 成本基准(= 入库量)。
	AdjustedSplitKg decimal.Decimal
	// CounterActualKg 是该部位当日的**猪肉台实盘**(kg)(完整盘点单口径)。
	CounterActualKg decimal.Decimal
	// ColdStoreKg 是该部位当日的**冷库量**(kg)。
	ColdStoreKg decimal.Decimal
	// CounterLoadKg 是该部位当日**搬上台**的重量(kg)。
	//
	// ⚠️ 2026-10-30 owner 口径:毛利看板那张表**整张都是猪肉台的** ⇒
	//   「期初」那一列不该显示账面链值(可以是负数 = 账面短卖,门店看不懂),
	//   而该显示**今天搬上台的量**(门店自己搬的,一眼认得)。
	CounterLoadKg decimal.Decimal
	// SuggestedUnitCostYuan 是**建议单价**(元/kg)——
	// 「如果今天的猪按**建议分割**切,这刀货的成本单价是多少」。
	//
	// 口径 = (期初金额 + 今日入库金额按**建议**比例摊到的部分) ÷ (期初kg + 建议分割kg)
	// ⚠️ nil = **没有建议分割**(当天没进猪 ⇒ Σ建议=0)⇒ 前端显示「—」,不显示 0。
	SuggestedUnitCostYuan *decimal.Decimal
	// SuggestedCostUnitPriceYuan 是**建议成本单价**(元/kg)——
	// 「按**单价基线**切出来的这刀货,每个部位应该值多少钱」。
	//
	// ⚠️ 口径(2026-10-31 owner:「分割基线加入单价」):
	//
	//	单价基线(部位进货单价) × (这头猪的进价 ÷ 基线的参考猪进价)
	//	⇒ 基线是 14.6/kg 的猪、今天这头是 16/kg ⇒ 各部位单价按比例上涨。
	//
	// ⚠️ **与 [SuggestedUnitCostYuan] 不是一回事**:
	//	后者是"按建议分割摊出来的**移动加权**成本单价(含期初)";
	//	前者是"基线算出来的**部位进货单价**"(owner 要的新列)。
	//
	// ⚠️ nil = 这个部位**取不到价**(没有基线 / 整店均价 / LLM 缓存都没有)
	//   ⇒ 前端显示「—」(不是 0)。
	SuggestedCostUnitPriceYuan *decimal.Decimal
	// SuggestedCostPriceSource 是建议成本单价的**来源**
	// (baseline | store_avg | llm_cache | none)—— 门店要看得见这个价怎么来的。
	SuggestedCostPriceSource string
	// ActualCostUnitPriceYuan 是**实际成本单价**(元/kg)= 销售成本 ÷ 销量。
	//
	// ⚠️ 分母用**总销量**(零售 + 统收)⇒ 与「零售单价」同一分母
	//   ⇒ 门店能自己验算:成本单价 × 销量 = 销售成本。
	//
	// ⚠️ nil = 当天这个部位**没卖**⇒ 前端显示「—」。
	ActualCostUnitPriceYuan *decimal.Decimal
	// ActualAvgUnitPriceYuan 是**实际均价**(元/kg)—— 当日这个部位**实际卖出**的平均单价。
	//
	// 口径 = 营收 ÷ 销量(零售来自思迅日汇总表 t_rm_daysum;统收是本地登记,一并计入)
	// ⇒ **与「营收」列自洽**(均价 × 销量 = 营收),门店能自己验算。
	// ⚠️ nil = 当天这个部位**没卖**(没销量)⇒ 前端显示「—」。
	ActualAvgUnitPriceYuan *decimal.Decimal
	// SplitWeightNote 是当天的**分割权重告警**(空 = 没有告警)。
	SplitWeightNote string
}

// cutOpeningOverride 期初覆盖(链式接续用)。
//
// # 为什么需要它
//
// 成本链的权威载体是 `cut_cost_daily`(上一营业日的部位结余)。
// 但那张表**只在审计结转时被固化** ⇒ 日常没有快照 ⇒
// 如果每天各自从"门店期初结转"起算,链就**每天断一次** ⇒
// 成本塌成"期初那点残值" ⇒ 毛利虚高(2026-10-27 生产实测:10-03 起成本 ≈ 100 元)。
//
// ⇒ 实时计算时,把**上一天算出来的期末**直接喂给下一天当期初
//
//	⇒ 链接续 ⇒ 数字自洽(不依赖任何落库快照)。
//
// ⚠️ amount 用的是**结转口径**的期末金额(与 cut_cost_daily.ClosingAmount 同义):
//
//	成本口径期末金额 + 预售重估 ⇒ 下一期的期初单价自动 = 预期销售单价。
type cutOpeningOverride struct {
	kg     map[string]decimal.Decimal
	amount map[string]decimal.Decimal
	// unit 是上一天算出来的**移动加权单价**(元/kg)—— 链式接续的第三根支柱。
	//
	// ⚠️ **为什么必须有它**(2026-10-27 生产实测):
	//
	//	链接上之后,某部位的**账面结余可能是负的**(短卖)⇒
	//	今天的"可售数量" = 期初(负) + 产出 ⇒ **分母 ≤ 0**
	//	⇒ 移动加权单价算不出来(除以非正数)
	//	⇒ 若没有兜底价 ⇒ 单价 0 ⇒ **卖出去的货零成本 ⇒ 毛利 100%(虚高)**。
	//
	//	实测:店 00 的 10-03 起销售成本恒 0、毛利率 100%、对账差额 −335.70。
	//
	// ⇒ 链必须把**上期的加权成本**一起传下去 ⇒ 短卖那部分照上期成本结转
	//   (与"短卖按当期移动加权成本计入成本"的口径一致)。
	unit map[string]decimal.Decimal
	// shortSell 是**累计短卖**(kg)—— 从上一期结转过来的真短卖(口径见 ShortSellKg)。
	//
	// ⚠️ **它不是库存**:短卖不写回负库存(期初仍 ≥0)⇒ 它只是给门店看的
	//   "这段时间一共短卖了多少"⇒ 链带着它往下传,次日报告显示「累计短卖」。
	shortSell decimal.Decimal
}

// has 某个部位有没有覆盖值。
func (o *cutOpeningOverride) has(cut string) bool {
	if o == nil || o.kg == nil {
		return false
	}
	_, ok := o.kg[cut]
	return ok
}

// cutAmounts 部位的量与金额(内部累加用)。
type cutAmounts struct {
	kg     decimal.Decimal
	amount decimal.Decimal
}

// loadStocktakeFlags 取某营业日**有没有盘点单**与**盘点口径三态**。
//
// ⚠️ 只打索引列(branch_id + taken_at),不扫全表。
func (s *Service) loadStocktakeFlags(ctx context.Context, branchID string, startOfDay, endOfDay time.Time) (
	hasStocktake bool, dataSource string, err error,
) {
	var nAll, nComplete int64
	if err := s.db.WithContext(ctx).
		Model(&model.PorkCutsStocktake{}).
		Where("branch_id = ? AND taken_at >= ? AND taken_at < ?", branchID, startOfDay, endOfDay).
		Count(&nAll).Error; err != nil {
		return false, "", fmt.Errorf("count stocktake: %w", err)
	}
	if nAll == 0 {
		return false, dataSourceEstimated, nil
	}
	if err := s.db.WithContext(ctx).
		Model(&model.PorkCutsStocktake{}).
		Where("branch_id = ? AND taken_at >= ? AND taken_at < ? AND is_complete = ?",
			branchID, startOfDay, endOfDay, true).
		Count(&nComplete).Error; err != nil {
		return false, "", fmt.Errorf("count complete stocktake: %w", err)
	}
	if nComplete == 0 {
		return true, dataSourcePartial, nil
	}
	return true, dataSourceActual, nil
}

// clampNonNegative 把"负的期末"裁成 0(kg 与金额**一起**裁)。
//
// # 为什么读快照时也要裁(2026-10-30 加)
//
//	新口径(R1)规定:**期末量永远 ≥0**(盘到的部位用实盘,没盘到的用 max(账面推演, 0))
//	⇒ 链上永远是 ≥0 ⇒ 次日期初不会是负数。
//
//	⚠️ 但**存量快照**是旧口径写的 —— 里面真的有负数
//	(生产实测 店 00 2026-10-29:8 个部位的 closing_kg 为负,最大 −3.311kg)。
//	⇒ 新代码从快照/锚点读期末时若不裁,负数就**从快照漏回链里**
//	⇒ owner 说的"期初不能有负数"又破功(而且只有存量门店会犯 ⇒ 新店测不出来)。
//
// # 为什么金额要**一起**裁
//
//	kg = 0 而金额是负数 ⇒ 可售金额为负 ⇒ 移动加权单价(金额÷kg)算不出来
//	⇒ 又变成"成本算不出来 ⇒ 毛利虚高"的老坑。
//
//	⚠️ 裁掉的那部分**不会凭空消失**:它会变成当天的**短卖分量**
//	(见 CutShortSellKg:可售量被裁小 ⇒ 账面不足变大 ⇒ 按当期单价补回成本)
//	⇒ 对账恒等式仍然闭合(diff = 0)。
func clampNonNegative(kg, amount decimal.Decimal) (decimal.Decimal, decimal.Decimal) {
	if kg.IsNegative() {
		return decimal.Zero, decimal.Zero
	}
	return kg, amount
}

// clampMarginPct 把毛利率**钳进列能装下的范围**。
//
// # 为什么需要它(2026-10-31 实测)
//
//	`cut_cost_daily.gross_margin_pct` 的列类型是 `numeric(8,4)`
//	⇒ 只能装 **±9999.9999**。
//	而毛利率 = 毛利 ÷ 营收×100 ⇒ 当**营收很小、成本很大**时会算出任意大的百分比。
//
//	实测(契约测试门店):某部位营收 30 元、成本 4000 元
//	⇒ 毛利率 = -13233.33% ⇒ **超出 numeric(8,4) 的上限**
//	⇒ PG 报 `numeric field overflow (SQLSTATE 22003)`
//	⇒ **整条成本链物化失败**(不只是这一行)⇒ 门店看到 500。
//
//	⚠️ 这不是理论风险:切「只用基线」时,引擎把**整头猪的成本**摊到
//	  **基线覆盖的那几 kg** 上(基线常常只覆盖一小部分)
//	⇒ 那些部位的 kg 很小、成本很重 ⇒ 毛利率必然爆。
//	  (生产实测店 00 2026-10-30 已出现 -1482.69%,离上限只差一个量级。)
//
// # 为什么钳制而不是改列
//
//	改列类型要 DDL + owner 批准;而**数值本身在业务上也没有意义**
//	(毛利率 -13233% 对门店只是"这刀货卖得极亏",不是精确数字)
//	⇒ 钳到边界内更稳,且不依赖迁移。
//
// # 钳的方向
//
//	**双向**钳:超上限取上限,超下限取下限
//	⇒ 保证写库永远成功,同时也保留"极度亏损"的信号(仍然是很大的负数)。
const (
	// marginPctMax / marginPctMin 是 numeric(8,4) 的安全边界。
	//
	// ⚠️ 留一点余量(9999.99 而不是 9999.9999)⇒ 避免四舍五入后又超。
	marginPctMax = 9999.99
	marginPctMin = -9999.99
)

// clampMarginPct 把毛利率钳进 numeric(8,4) 装得下的范围。
func clampMarginPct(pct float64) float64 {
	if pct > marginPctMax {
		return marginPctMax
	}
	if pct < marginPctMin {
		return marginPctMin
	}
	return pct
}

// loadCutOpening 取某营业日的**部位级期初**(kg + 金额)。
//
// # kg 与金额**分开定价**(2026-10-27 修)
//
//	kg 来源:① 上一营业日的部位级成本日快照(cut_cost_daily)
//	        → ② 上一营业日的**实时余额链**日终结余(closingForDay)
//	        → ③ 门店期初结转(sku_stock_openings,按部位录)
//	单价:   ① 快照里的金额(直接用,可以是负数)
//	        → ② cut_cost_states 的部位移动加权单价
//	        → ③ 门店期初结转里**门店给的进价**
//	        → ④ 今日该部位的产出单价(产出金额 ÷ 产出 kg)
//
// ⚠️ **绝不能"有 kg 没金额"**:
//
//	移动加权单价 = 可售金额 ÷ 可售数量 ⇒ 期初 10kg、金额 0 ⇒ 单价被稀释
//	⇒ 成本低估 ⇒ **毛利虚高** —— 正是这次改造要消灭的那个坑,换个位置又冒出来。
//
//	⇒ **上线第一天最危险**:此时 cut_cost_states 还没有历史单价
//	   (生产:真实门店 00/01 只有一条期初结转 ⇒ trotter 9.23kg @8元)
//	   ⇒ 金额必须由 ③/④ 兜住,否则期初按 0 元入账 ⇒ 整条链的毛利虚高。
//
// ⚠️ 第 ③ 步是这次改造的关键:旧引擎把它落到"主 SKU"上 ⇒ 另一个货号账面恒为 0
//
//	⇒ 卖出零成本 ⇒ 毛利率 100%。
//
// # override(链式接续,2026-10-27)
//
//	override 命中时**kg 与金额都直接取**(与快照同语义:金额可以是负数 = 账面短卖)。
//	它排在最前 ⇒ 实时算一条链时,上一天的期末直接接成今天的期初,
//	**不依赖 cut_cost_daily 里有没有落库快照**。
func (s *Service) loadCutOpening(ctx context.Context, branchID string, bizDate time.Time,
	producedKg, producedAmount map[string]decimal.Decimal,
	histUnit map[string]decimal.Decimal,
	override *cutOpeningOverride,
) (kg map[string]decimal.Decimal, amount map[string]decimal.Decimal, err error) {
	kg = map[string]decimal.Decimal{}
	amount = map[string]decimal.Decimal{}

	// ---- ① 上一营业日的部位级成本日快照 ----
	prevStart, prevEnd := s.previousBizDayBounds(bizDate)
	var prev []model.CutCostDaily
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date >= ? AND biz_date < ?", branchID, prevStart, prevEnd).
		Find(&prev).Error; err != nil {
		return nil, nil, fmt.Errorf("query cut_cost_daily(prev): %w", err)
	}
	snapKg := map[string]decimal.Decimal{}
	snapAmt := map[string]decimal.Decimal{}
	hasSnap := map[string]bool{}
	for _, r := range prev {
		c := string(r.CutType)
		// ⚠️ **该部位当天成本算不出来** ⇒ 它的期末金额是"0"(单价 0 × kg),
		//   而 0 不是它的真实价值,是"不知道"。
		//   ⇒ 不能拿来当今天的期初:否则可售金额被抹平、可售数量被压小
		//     ⇒ **移动加权单价爆炸** ⇒ 成本变成天文数字
		//     (2026-10-27 生产实测:10-02 成本 3061.57 → 10151.24,毛利率 −191%)。
		//
		// ⇒ 链在这个部位上自然断开(它本来就没有成本链)⇒ 数字安全。
		if r.CostUncomputable {
			continue
		}
		// ⚠️ **负的期末要裁成 0**(2026-10-30 加,见 clampNonNegative 的说明):
		//   旧口径把负的账面推演写进了存量快照 ⇒ 不裁的话负期初又漏回链里。
		snapKg[c], snapAmt[c] = clampNonNegative(r.ClosingKg, r.ClosingAmount)
		hasSnap[c] = true
	}

	// ---- ② 上一营业日的实时余额链日终结余(部位) ----
	chainKg := map[string]decimal.Decimal{}
	hasChain := map[string]bool{}
	prevCounter, prevCold, ok, cerr := s.closingForDay(ctx, branchID, prevStart)
	if cerr != nil {
		s.pubLogger.Warn("loadCutOpening: 读上一营业日的部位结余失败(只走快照/期初结转)",
			"branch_id", branchID, "biz_date", bizDate.In(s.bizTZOrUTC()).Format("2006-01-02"), "err", cerr)
	} else if ok {
		for cut, v := range prevCold {
			chainKg[cut] = v.Add(prevCounter[cut])
			hasChain[cut] = true
		}
	}

	// ---- ③ 门店期初结转(按部位) ----
	openings, err := s.loadOpenings(ctx, branchID)
	if err != nil {
		return nil, nil, err
	}
	key := bizDate.In(s.bizTZOrUTC()).Format("2006-01-02")

	// ⚠️ **基线日是「天」级别的,不是「部位」级别的**(2026-10-30 加)。
	//
	//	门店在生效日**重新定义了整天的期初** ⇒ 那天**所有**部位的期初都来自门店:
	//	录了的 = 门店录的量;没录的 = 0。
	//
	//	# 为什么必须是天级(2026-10-30 生产实测,店 00)
	//
	//	旧实现是 `baselineDay := hasOp && op.dateKey == key` —— **按部位**判断:
	//	只有「门店在这个部位上录了期初」的那个部位才让位给门店的值,
	//	**其余部位照旧接链外的旧结余** ⇒ 链外的负账面值顺着「没录的部位」
	//	又漏回了链里(实测:10-28 的猪耳 −1.279 / 猪肺 −17.64 / 猪手 −9.2 kg
	//	—— 门店压根没录过这三个部位)。
	//	⇒ 代码自己的注释写的就是「生效日当天:快照/链式接续/实时结余链
	//	  **一律让位**给门店录的值」—— 承诺是天级,实现却是部位级。
	//
	//	⇒ 这里取该店期初结转的**最早生效日**(= 链的起点,见 cutChainBaseline)
	//	  作为基线日;其余日子照旧链式接续。
	chainStartDay := ""
	for _, op := range openings {
		if chainStartDay == "" || op.dateKey < chainStartDay {
			chainStartDay = op.dateKey
		}
	}
	baselineDay := chainStartDay != "" && chainStartDay == key

	// 今日产出单价(兜底价):该部位的产出金额 ÷ 产出 kg。
	producedUnit := map[string]decimal.Decimal{}
	for cut, k := range producedKg {
		if !k.IsPositive() {
			continue
		}
		if a := producedAmount[cut]; a.IsPositive() {
			producedUnit[cut] = a.Div(k)
		}
	}

	// ---- 合并:kg 定来源,金额**单独定价** ----
	cuts := map[string]struct{}{}
	for c := range snapKg {
		cuts[c] = struct{}{}
	}
	for c := range chainKg {
		cuts[c] = struct{}{}
	}
	for c := range openings {
		cuts[c] = struct{}{}
	}
	for c := range cuts {
		// 生效日在**今天之后**的期初结转还没开始 ⇒ 不参与(那是给未来那天准备的)。
		op, hasOp := openings[c]
		if hasOp && op.dateKey > key {
			hasOp = false
		}
		// ⚠️ **期初结转生效日 = 链的起点**(2026-10-27 链式接续后必须显式处理)。
		//
		//	门店在生效日**重新定义了**这个部位的库存与进价 ⇒ 这一天的期初
		//	必须用**门店录的值**,不能接昨天的结余。
		//	否则门店录了 100kg、报表写 95kg(昨天卖剩的量)⇒ 门店会以为系统算错了。
		//
		//	⇒ 生效日当天:快照 / 链式接续 / 实时结余链 **一律让位**给门店录的值。
		//	  ⚠️ 但「让位」是**整天**的(见上面的 chainStartDay):
		//	  没录的部位 = 0,**不接**链外的旧结余。
		switch {
		case baselineDay:
			// 门店重定义了这一天 ⇒ 整天的期初都来自门店(录了的用录的,没录的 = 0)。
			// ⚠️ 这里**不接**快照/链式接续/实时结余链 ——
			//   否则链外的旧账(上线前物化的负账面值)会顺着"没录的部位"漏回链里。
			if !hasOp {
				continue // 门店没录 ⇒ 这个部位当天期初 = 0
			}
			kg[c] = op.kg
		case override.has(c):
			// 链式接续:上一天的期末直接接成今天的期初(与快照同语义)。
			kg[c] = override.kg[c]
			amount[c] = override.amount[c]
			// ⚠️ **有量无价**(2026-10-27 链式接续后暴露):上一批货没定价
			//   (基线日门店只录了 kg、没填进价)⇒ 金额 0 会**一路传下去**
			//   ⇒ 那批货永远零成本 ⇒ 毛利虚高。
			//   ⇒ 不直接用 0,落到下面的定价兜底(今日产出单价等)重新估。
			if !amount[c].IsZero() || !kg[c].IsPositive() {
				continue
			}
		case hasSnap[c]:
			// 快照是链的权威 ⇒ kg 与金额都直接用(金额可能是负数 = 账面短卖)。
			kg[c] = snapKg[c]
			amount[c] = snapAmt[c]
			continue
		case hasChain[c]:
			kg[c] = chainKg[c]
		case hasOp:
			kg[c] = op.kg
		default:
			continue
		}
		// 定价:历史部位单价 → 门店给的进价 → 今日产出单价。
		//
		// ⚠️ **基线日**(期初结转生效日)上,**门店给的进价优先于历史单价** ——
		//   否则历史单价会覆盖门店刚录的价格 ⇒ 门店录的价白录了。
		unit := decimal.Zero
		switch {
		case baselineDay && op.unitPrice.IsPositive():
			unit = op.unitPrice
		case histUnit[c].IsPositive():
			unit = histUnit[c]
		case hasOp && op.unitPrice.IsPositive():
			unit = op.unitPrice
		case producedUnit[c].IsPositive():
			unit = producedUnit[c]
		}
		if unit.IsPositive() {
			amount[c] = kg[c].Mul(unit)
			continue
		}
		// ⚠️ 有量无价 ⇒ 成本会被稀释 ⇒ 必须让人知道(别让它悄悄变成 0 成本)。
		if !kg[c].IsZero() {
			s.pubLogger.Warn("loadCutOpening: 期初有量但无价(成本会被稀释),请补录期初进价或盘点",
				"branch_id", branchID, "biz_date", key, "cut_type", c, "opening_kg", kg[c].String())
		}
		amount[c] = decimal.Zero
	}
	// ⚠️ **期初永远 ≥0**(owner 2026-10-30:「期初不能有负数」)。
	//
	//	期初有三个来源,**都可能是负数**:
	//	  ① 上一天的链值(override)—— 旧引擎会把负的期末传下来;
	//	  ② 上一营业日的**快照**(历史数据里有负的 closing_kg);
	//	  ③ **实时结余链**(closingForDay)—— 余额链算出来的冷库余额可以是负的
	//	    (搬台超过库存时)⇒ 实测:10-30 卖出 50kg、账上只有 20kg
	//	    ⇒ 余额链 = −30kg ⇒ 期初 −30 ⇒ 门店看到"期初有负数"。
	//
	//	⇒ 统一在这里裁 0(金额同步归零)⇒ **门店永远看不到负期初**,
	//	  而短卖由「当日短卖 / 账面不足」这两个数显式表达(不靠负库存暗示)。
	for c := range kg {
		if kg[c].IsNegative() {
			kg[c] = decimal.Zero
			amount[c] = decimal.Zero
		}
		if amount[c].IsNegative() {
			amount[c] = decimal.Zero
		}
	}
	return kg, amount, nil
}

// computeCutDailyCosts 算某店某营业日的**部位级**移动加权成本。
//
// 返回 (部位明细, SKU 明细, 成本对账行, 损耗汇总)。
//
// ⚠️ **SKU 明细只承载"卖了多少、卖了多少钱"**:
//
//	销售 kg / 营收 / 分割产出是 SKU 级的事实;
//	**期初 / 期末 / 成本 / 毛利都是部位口径** ⇒ SKU 行里不返(留 0)。
//	⇒ 这正是门店要的口径(2026-10-27):
//	「以部位为主(分割仍然是部位的重量),销售、营收等和 SKU 有关的数据分列」。
func (s *Service) computeCutDailyCosts(ctx context.Context, branchID string, day time.Time) (
	[]CutDailyCost, []SkuDailyCost, *ReconciliationRow, *LossSummary, error,
) {
	return s.computeCutDailyCostsChained(ctx, branchID, day, nil)
}

// computeCutDailyCostsChained 算某店某营业日的**部位级**移动加权成本,
// 并允许把**上一天的期末**接成今天的期初(链式接续)。
//
// ⚠️ override 只影响"期初"这一步;其余口径与 computeCutDailyCosts 完全一致
//
//	⇒ 单日毛利与长期毛利用同一个引擎,不会算出两套数。
func (s *Service) computeCutDailyCostsChained(ctx context.Context, branchID string, day time.Time,
	override *cutOpeningOverride,
) (
	[]CutDailyCost, []SkuDailyCost, *ReconciliationRow, *LossSummary, error,
) {
	startOfDay, endOfDay := s.businessDayBounds(day)
	bizDate := day.In(s.bizTZOrUTC())

	// ---- 1. 今日进场的猪 + 成本 ----
	var pigs []model.WholePig
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND arrived_at >= ? AND arrived_at < ?", branchID, startOfDay, endOfDay).
		Find(&pigs).Error; err != nil {
		return nil, nil, nil, nil, fmt.Errorf("query whole_pigs: %w", err)
	}
	pigCost := make(map[string]decimal.Decimal, len(pigs))
	for _, p := range pigs {
		pigCost[p.ID] = p.PurchaseCostYuan
	}

	// ---- 2. 今日产出(分割)→ 部位口径 ----
	producedKg, producedByPigKg, unallocatedPigCost, cuttingLossKg, err :=
		s.computeProducedBySku(ctx, branchID, startOfDay, endOfDay, pigs, pigCost)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	cutOfSku, nameOfSku, err := s.loadSkuCatalog(ctx, branchID, nil)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	// ---- 2c. **外采入库**(2026-11-03 owner)----
	//
	// 外采 = 额外采购某个部位的货 ⇒ 它是「本期入库」的**另一个来源**
	// (与猪的分割产出并列):本期入库量 += 外采 kg;本期入库金额 += 外采金额。
	//
	// ⚠️ 这里**只取数**;真正叠加在 applySplitWeights **之后**(见 8c-2)。
	extProducedKg, extProducedAmount, err := s.loadExternalPurchaseTotals(ctx, branchID, startOfDay, endOfDay)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	// ---- 3. 卖出 / 统收 / 报损 / 搭赠(SKU 级读,后面汇总到部位) ----
	soldKg, soldAmount, bulkKg, bulkAmount, wasteKg, giftKg, cutFromSales, err :=
		s.loadSkuFlows(ctx, branchID, startOfDay, endOfDay)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	// ---- 3a. ⚠️ **纯基线日**:只提供期初,当天的销售**不进这条链**(2026-10-30 owner 定案)----
	//
	//	owner 原话:「我把 2026-10-28 作为期初,2026-10-29 作为第一天正常开始使用这套鲜肉子系统」
	//
	//	⇒ **纯基线日** = 期初生效日 **且** 那天没有任何营业日动作
	//	  (没录猪、没搬台、没盘点 ⇒ 见 isBaselineDay)
	//	  ⇒ 那天的 POS 销售是**上线前那套账**的货:系统里没有对应的进货/搬台/盘点
	//	  ⇒ 成本**算不出来**,而且那天的销售**根本不该在这条链里**
	//	  (生产实测 店 00:10-28 卖了 172kg / 营收 3632.70 元,而期初只有"后腿 10kg"
	//	   ⇒ 账面短卖 ⇒ 结余 −8.24kg ⇒ 传给 10-29 当期初 ⇒ 门店看到"期初为什么有负数")。
	//
	//	⇒ 纯基线日**只带期初**:销售/统收/报损一律不进链
	//	  ⇒ 它的结余 = 期初 ⇒ 下一天(链的第一天)的期初 = 门店录的值 ⇒ **不再短卖**。
	//
	//	⚠️ **只对"纯基线日"生效**:那天**有**营业日动作的基线日(例如门店当天录了猪)
	//	  ⇒ 那天是**真营业日** ⇒ 它的销售必须进链(否则链会断、货会凭空消失)。
	//	  ⇒ 店 01 的期初生效日 10-01 有营业日动作 ⇒ 它的数字**不受影响**。
	//
	//	⚠️ 在**汇总到部位之前**清(SKU 级源头)⇒ 部位行、SKU 行、营收全部一致。
	baselineKey := bizDate.In(s.bizTZOrUTC()).Format("2006-01-02")
	if pureBaseline, err := s.isBaselineDay(ctx, branchID, baselineKey); err != nil {
		s.pubLogger.Warn("computeCutDailyCosts: 判纯基线日失败(按普通营业日算)",
			"branch_id", branchID, "biz_date", baselineKey, "err", err)
	} else if pureBaseline {
		soldKg = map[string]decimal.Decimal{}
		soldAmount = map[string]decimal.Decimal{}
		bulkKg = map[string]decimal.Decimal{}
		bulkAmount = map[string]decimal.Decimal{}
		wasteKg = map[string]decimal.Decimal{}
		s.pubLogger.Info("纯基线日:只带期初,当天的销售不进成本链",
			"branch_id", branchID, "biz_date", baselineKey)
	}

	// ---- 2b. 入库量兜底 ⚠️ 已下线(2026-11-02 owner:标签号玩法整套下线)----
	//
	// 那条兜底是"门店没补录分割时,用日终盘点重量倒推当天产出",
	// 它的两个输入(**带标签号的结转行** + **按标签整批搬**写下的 moved_at)
	// 都来自"贴标签"这套机制 ⇒ 标签号下线 ⇒ 它恒不生效 ⇒ 整条删除。
	//
	// ⚠️ **不要**把它改成"算全部结转行":那等于启用一条从未跑过的路径
	//   ⇒ "门店没补录分割"那几天的成本口径会突变。
	//
	// (FIFO 归摊用的是**全部**结转行,不受影响 —— 见 service/pig_fifo.go。)

	// ⚠️ **必须先兜底再汇总到部位**:兜底改的是 SKU 级的 producedKg / producedByPigKg,
	//   汇总之后就没法把兜底量算进部位了。
	partProducedKg := map[string]decimal.Decimal{}
	for sku, v := range producedKg {
		cut := string(cutOfSku[sku])
		if cut == "" {
			continue
		}
		partProducedKg[cut] = partProducedKg[cut].Add(v)
	}
	// 猪成本按**部位**分摊(逐猪摊:该猪该部位的 kg 占该猪总产出的比例)。
	partProducedAmount := map[string]decimal.Decimal{}
	for pigID, perPig := range producedByPigKg {
		pigTotal := s.pigTotalProducedKg(perPig)
		if !pigTotal.IsPositive() {
			continue
		}
		partKgInPig := map[string]decimal.Decimal{}
		for sku, kg := range perPig {
			cut := string(cutOfSku[sku])
			if cut == "" {
				continue
			}
			partKgInPig[cut] = partKgInPig[cut].Add(kg)
		}
		for cut, kg := range partKgInPig {
			partProducedAmount[cut] = partProducedAmount[cut].
				Add(pigCost[pigID].Mul(kg).Div(pigTotal))
		}
	}

	// ---- 3b. 汇总到部位 ----
	partSoldKg := map[string]decimal.Decimal{}
	partSoldAmount := map[string]decimal.Decimal{}
	partBulkKg := map[string]decimal.Decimal{}
	partBulkAmount := map[string]decimal.Decimal{}
	partWasteKg := map[string]decimal.Decimal{}
	// partGiftKg 是**搭赠**的量(kg,按部位)—— 2026-10-31 owner 拍板。
	//
	// ⚠️ 搭赠是「有成本、无收入」的出库 ⇒ 链上的**第四个出口**:
	//	从可售量里扣掉(否则账面推演比实盘多 ⇒ 被当成盘亏/短卖),
	//	并且**单独计成本**(搭赠成本)⇒ 扣毛利但单列一行。
	partGiftKg := map[string]decimal.Decimal{}
	cutOf := func(sku string) string {
		if c := string(cutOfSku[sku]); c != "" {
			return c
		}
		return string(cutFromSales[sku])
	}
	for sku, v := range soldKg {
		partSoldKg[cutOf(sku)] = partSoldKg[cutOf(sku)].Add(v)
	}
	for sku, v := range soldAmount {
		partSoldAmount[cutOf(sku)] = partSoldAmount[cutOf(sku)].Add(v)
	}
	for sku, v := range bulkKg {
		partBulkKg[cutOf(sku)] = partBulkKg[cutOf(sku)].Add(v)
	}
	for sku, v := range bulkAmount {
		partBulkAmount[cutOf(sku)] = partBulkAmount[cutOf(sku)].Add(v)
	}
	for sku, v := range wasteKg {
		partWasteKg[cutOf(sku)] = partWasteKg[cutOf(sku)].Add(v)
	}
	for sku, v := range giftKg {
		partGiftKg[cutOf(sku)] = partGiftKg[cutOf(sku)].Add(v)
	}

	// ---- 4. 期初(部位) ----
	//
	// ⚠️ 部位的历史移动加权单价先取出来:**期初定价**要用它(见 loadCutOpening),
	//   "可售数量为 0 时给短卖补提成本"也要用它。
	var states []model.CutCostState
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Find(&states).Error; err != nil {
		return nil, nil, nil, nil, fmt.Errorf("query cut_cost_states: %w", err)
	}
	histUnit := map[string]decimal.Decimal{}
	todayKey := bizDate.In(s.bizTZOrUTC()).Format("2006-01-02")
	for _, st := range states {
		// ⚠️ **只用"更早的营业日"的状态**(2026-10-27 修:自引用漂移)。
		//
		//	`cut_cost_states` 是"当前成本状态"——今天的快照落库后,它记的就是**今天**的
		//	移动加权单价。若拿它给**今天**的期初定价 ⇒ 今天刚算出来的成本
		//	反过来给今天的期初重新定价 ⇒ 同一份数据被算两遍(且随调用顺序漂移)。
		//
		//	⇒ 只有 `last_biz_date` **早于今天**的状态才能当"上期单价"用。
		if time.Time(st.LastBizDate).In(s.bizTZOrUTC()).Format("2006-01-02") >= todayKey {
			continue
		}
		histUnit[string(st.CutType)] = st.MovingAvgUnitCost
	}
	// ⚠️ **链式接续的上期单价优先于 cut_cost_states**(2026-10-27 生产实测)。
	//
	//	`cut_cost_states` 只在**盘点**时刷新 ⇒ 零盘点门店它是空的
	//	⇒ 短卖导致"可售数量 ≤ 0"时,兜底价 = 0 ⇒ **销售成本 0 ⇒ 毛利 100%**。
	//
	//	⇒ 链把上一天算出来的移动加权单价带下来 ⇒ 短卖那部分照上期成本结转。
	if override != nil {
		for cut, u := range override.unit {
			if u.IsPositive() {
				histUnit[cut] = u
			}
		}
	}
	openKg, openAmount, err := s.loadCutOpening(ctx, branchID, bizDate,
		partProducedKg, partProducedAmount, histUnit, override)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	// ---- 6. 期末(部位)与盘点口径 ----
	hasStocktake, dataSource, err := s.loadStocktakeFlags(ctx, branchID, startOfDay, endOfDay)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	closeKg := map[string]decimal.Decimal{}
	// chainColdKg 是**引擎链的日终冷库余额**(按部位)。
	//
	// ⚠ 报表的「冷库量」在**没有盘点单**时退回它(owner 口径)⇒ 这里**每天**都要读一次链。
	//   (链的推导是"按门店的流水"量级,不是按天量级 ⇒ 多读一次的代价可控。)
	chainColdKg := map[string]decimal.Decimal{}
	counter, cold, ok, derr := s.closingForDay(ctx, branchID, bizDate)
	if derr != nil {
		s.pubLogger.Warn("computeCutDailyCosts: 读取按营业日的部位结余失败(回退账面推演)",
			"branch_id", branchID, "biz_date", bizDate.Format("2006-01-02"), "err", derr)
	} else if ok {
		for cut, v := range cold {
			chainColdKg[cut] = v
			// ⚠ 期末量**只在有盘点单时**用链值;没有盘点单 ⇒ 期末走账面推演(行为不变)。
			if hasStocktake {
				closeKg[cut] = v.Add(counter[cut])
			}
		}
	}

	// ---- 7. 预售(预期售价):SKU 价 → 部位加权价 ----
	presaleUnits, err := s.loadPresaleUnits(ctx, branchID, bizDate)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	partPresaleUnit := s.cutPresaleUnits(presaleUnits, cutOfSku, soldAmount, soldKg)

	// ---- 8. 逐部位算移动加权 ----
	recon := &ReconciliationRow{}
	out := make([]CutDailyCost, 0, len(partProducedKg))
	cuts := map[string]struct{}{}
	for c := range partProducedKg {
		cuts[c] = struct{}{}
	}
	for c := range partSoldKg {
		cuts[c] = struct{}{}
	}
	for c := range partBulkKg {
		cuts[c] = struct{}{}
	}
	for c := range partWasteKg {
		cuts[c] = struct{}{}
	}
	for c := range partGiftKg {
		cuts[c] = struct{}{}
	}
	for c := range openKg {
		cuts[c] = struct{}{}
	}
	for c := range closeKg {
		cuts[c] = struct{}{}
	}
	// ⚠️ 2026-11-03 owner:**外采的部位**也要出行
	//   (那天只外采、没进猪也没卖 ⇒ 它是当天唯一有货的部位)。
	for c := range extProducedKg {
		cuts[c] = struct{}{}
	}
	for c := range presaleUnits {
		// 预售按 SKU 记 ⇒ 它的部位也必须有行(哪怕当期没有销售)。
		if cut := string(cutOfSku[c]); cut != "" {
			cuts[cut] = struct{}{}
		}
	}
	cutList := make([]string, 0, len(cuts))
	for c := range cuts {
		cutList = append(cutList, c)
	}
	sort.Strings(cutList)

	// ---- 8b. 部位级分割数据(建议 S / 实际 A / 调整后权重 W)----
	//
	// ⚠ W 只在**要出行的部位**上算 ⇒ 每个 W > 0 的部位都有一行 ⇒ 入库金额不会"摊丢"。
	//
	// ⚠ W **每天现算**(不落快照)⇒ 盘点补录后 A_c 变 ⇒ W 变 ⇒ 成本跟着变。
	cutSet := make(map[string]bool, len(cutList))
	for _, c := range cutList {
		cutSet[c] = true
	}
	split, err := s.computeCutSplit(ctx, branchID, startOfDay, endOfDay, bizDate, pigs,
		cutOfSku, partSoldKg, partBulkKg, partWasteKg, openKg, cutSet)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	// ---- 8b-2. 建议成本单价(2026-10-31 owner:「分割基线加入单价」)----
	//
	// ⚠️ **逐猪**算:每头猪的进价不同 ⇒ 各部位的单价要按比例缩放
	//   (owner:「基线是 14.6/kg 的,那 16/kg 的猪每个部位单价都要按比例上涨」)
	//   ⇒ 再按**建议分割 kg** 加权汇总成部位口径。
	//
	// ⚠️ 单价基线只用**该营业日之前**的日样本 ⇒ 今天的建议不依赖今天的链
	//   ⇒ 结构性断环(见 forecast_price.go 的包注释)。
	//
	// ⚠️ 取不到价的部位**不出现在结果里** ⇒ 报表显示「—」(不是 0)。
	//
	// ⚠️ 封顶标志(capped)在这里**丢弃**:报表是**多猪汇总**口径,
	//   "哪头猪被封顶了"只有单猪视角(预览/录猪页)才讲得清 ⇒ 不在这里误导门店。
	suggestedCostUnit, suggestedCostSource, _, err := s.suggestedCutUnitPrices(ctx, branchID, pigs,
		split.SuggestedPerPig, bizDate)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	suggestedCostUnitPtr := map[string]*decimal.Decimal{}
	for c, v := range suggestedCostUnit {
		if !v.IsPositive() {
			continue
		}
		val := v
		suggestedCostUnitPtr[c] = &val
	}

	// ---- 8c. 入库量/入库金额按 W 重算 ----
	//
	// ⚠ ΣW == 0(没有建议分割)⇒ 完全不接管 ⇒ 与今天逐字一致(见 applySplitWeights)。
	prodKg, prodAmount := applySplitWeights(split, partProducedKg, partProducedAmount)

	// ---- 8c-2. **外采入库叠加**(2026-11-03 owner)----
	//
	// ⚠ **必须在 applySplitWeights 之后**:
	//   那个函数把入库量整体**替换成分割权重 W**、并把入库金额按 W 的**比例重分**
	//   ⇒ 外采放进去会被 W 顶掉(kg 丢了)、金额会被摊到别的部位。
	//
	// ⇒ 这里叠加的是「本期入库」的另一个来源:
	//   本期入库量   += 外采 kg(按部位)
	//   本期入库金额 += 外采金额(kg × 门店给的采购单价)
	//
	// ⇒ 卖掉的那些外采 ⇒ 成本进销售成本 ⇒ **毛利下降**;
	//   还没卖掉的 ⇒ 留在期末存货里 ⇒ 不影响当天的毛利。
	for c, v := range extProducedKg {
		prodKg[c] = prodKg[c].Add(v)
	}
	for c, v := range extProducedAmount {
		prodAmount[c] = prodAmount[c].Add(v)
	}

	// 今日可摊猪成本合计(元)= Σ 入库金额(按 W 重分后**总量不变**)。
	//
	// ⚠️ 「建议单价」要按**建议分割**摊同一笔钱 ⇒ 必须有这份总额。
	var allocatable decimal.Decimal
	for _, v := range prodAmount {
		allocatable = allocatable.Add(v)
	}
	// Σ 建议分割(kg)—— 建议单价的分摊基数。
	var totalSuggestedKg decimal.Decimal
	for _, v := range split.Suggested {
		totalSuggestedKg = totalSuggestedKg.Add(v)
	}

	// ---- 8d. 报表口径:冷库量 + 实际分割的显隐 ----
	//
	// 冷库量:盘点单优先(盘点**那一刻**的冷库余额);**没有盘点单** ⇒ 退回引擎链值
	//   (owner 口径:「没有盘点单时退回 closingForDay 的 cold 那份」)。
	//
	// ⚠ 冷库量**不是实盘**:实盘 = 猪肉台实盘(ActualRemainKg)⇒ 两者分开出参。
	coldStoreKg := split.Cold
	if !split.HasAnyStocktake {
		coldStoreKg = chainColdKg
	}
	//
	// 实际分割的**显示规则**(owner 定案):
	//	recorded ⇒ 总是给值;
	//	derived  ⇒ 有完整日终盘点 ⇒ 给值;
	//	           无完整日终盘点 且 A_c < S_c ⇒ 给 nil(前端显示空)。
	//
	// ⚠ 显隐**只影响展示**;重分配用的永远是**内部值**(split.Actual)。
	splitActualKg := map[string]*decimal.Decimal{}
	splitSource := map[string]string{}
	for _, c := range cutList {
		src := split.Source[c]
		if src == "" {
			src = ActualSplitSourceDerived
		}
		splitSource[c] = src
		actual := split.Actual[c]
		if src == ActualSplitSourceDerived && !split.HasStocktake && actual.LessThan(split.Suggested[c]) {
			continue // 隐藏 ⇒ 前端显示空
		}
		v := actual
		splitActualKg[c] = &v
	}

	// 部位的历史移动加权单价(上面已查)。
	//
	// ⚠️ 兜底价:该部位今日的产出单价(产出金额 ÷ 产出 kg)——
	//   期初没有历史单价、又没有门店给的进价时,用今天的进价给那批货估值。
	fallbackUnit := func(cut string) decimal.Decimal {
		if histUnit[cut].IsPositive() {
			return histUnit[cut]
		}
		k := prodKg[cut]
		if !k.IsPositive() {
			return decimal.Zero
		}
		if a := prodAmount[cut]; a.IsPositive() {
			return a.Div(k)
		}
		return decimal.Zero
	}

	// ---- 从上一期结转过来的**累计短卖**(R4)----
	//
	//	链状态带 shortSellKg(累计)⇒ 次日报告显示「累计短卖」。
	//	⚠️ **绝不回写成负库存**:短卖只进这个数,期初仍然是 ≥0(见上面的期末量口径)。
	carriedShortSellKg := decimal.Zero
	if override != nil {
		carriedShortSellKg = override.shortSell
	} else {
		// 非链式调用(逐日物化:persistCutCostDaily)⇒ 从**上一营业日的快照**读累计值。
		v, err := s.carriedShortSellFromSnapshot(ctx, branchID, bizDate)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		carriedShortSellKg = v
	}

	// 当日总量口径(R3)的两个累加项:Σ(期初+进货) 与 Σ(卖出+统收+报损)。
	availTotalKg := decimal.Zero
	outTotalKg := decimal.Zero

	for _, c := range cutList {
		cut := model.CutType(c)
		openK := openKg[c]
		openA := openAmount[c]
		// ⚠ 入库量/入库金额 = **调整后权重 W** 与按 W 比例重分的猪成本
		//   (ΣW == 0 时完全沿用今天的值 ⇒ 行为逐字一致)。
		prodK := prodKg[c]
		prodA := prodAmount[c]
		availAmt := openA.Add(prodA)
		availKg := openK.Add(prodK)
		movingAvg := decimal.Zero
		switch {
		case availKg.IsPositive() && availAmt.IsPositive():
			movingAvg = availAmt.Div(availKg)
		case availKg.IsPositive():
			// ⚠️ **有量无价**:可售金额为 0(期初没定价)⇒ 加权单价会算成 0
			//   ⇒ 那批货零成本 ⇒ **毛利虚高**(我们正在消灭的那个坑)。
			//   ⇒ 用兜底价(历史部位单价 / 今日产出单价)计成本。
			movingAvg = fallbackUnit(c)
			if movingAvg.IsPositive() {
				s.pubLogger.Warn("computeCutDailyCosts: 可售金额为 0 但有货,已用兜底单价计成本",
					"branch_id", branchID, "biz_date", bizDate.In(s.bizTZOrUTC()).Format("2006-01-02"),
					"cut_type", c, "avail_kg", availKg.String(), "fallback_unit", movingAvg.String())
			}
		case fallbackUnit(c).IsPositive():
			// ⚠️ 本期既没有期初也没有产出 ⇒ 可售数量为 0 ⇒ 加权单价算不出来。
			//   但"短卖"的那部分货**仍要有成本**(否则虚高毛利)⇒ 用兜底单价。
			movingAvg = fallbackUnit(c)
		}
		// ⚠️ **成本算不出来** ≠ **成本为 0**(2026-10-27 加)。
		//
		//	当期既没有库存(期初)也没有进货(产出)、又没有任何可用单价
		//	⇒ 卖出去的那部分货只能是 0 成本 ⇒ 毛利虚高到 100%。
		//	⇒ 显式标记,让报表层能说"这天的**成本不知道**",
		//	  而不是把 0 当成成本报给门店。
		costUncomputable := false
		// ⚠️ **搭赠计入"流出"**(2026-10-31 owner 拍板)。
		//
		//	搭赠的货**从库存里出去了**(只是没过称、没过账)
		//	⇒ 它是**正常出库**,不是"卖超了账面库存"
		//	⇒ 必须计入 soldTotal ⇒ 否则:
		//	  ① 账面推演比实盘多 ⇒ 被当成**盘亏**;
		//	  ② 缺口被算成"卖超了库存" ⇒ 白白多出一块成本(货其实是自己送出去的)。
		//
		//	生产实测(店 00 2026-10-30 猪肝):产出 3.030kg 全部搭赠、
		//	没进任何销售记录 ⇒ 盘成 0 ⇒ 账上成了"短卖 3.03kg"。
		soldTotal := partSoldKg[c].Add(partBulkKg[c]).Add(partWasteKg[c]).Add(partGiftKg[c])
		// 当日总量口径(R3)的累加:Σ(期初+进货) / Σ(卖出+统收+报损+搭赠)。
		availTotalKg = availTotalKg.Add(availKg)
		outTotalKg = outTotalKg.Add(soldTotal)
		if !movingAvg.IsPositive() && soldTotal.IsPositive() {
			costUncomputable = true
		}
		expectedCloseKg := availKg.
			Sub(partSoldKg[c]).Sub(partBulkKg[c]).Sub(partWasteKg[c]).Sub(partGiftKg[c])
		// ---- 疑似搭赠的缺口(2026-10-31)----
		//
		//	缺口 = 账面应有(全店) − 实盘(柜台实盘 + 冷库量)
		//	⇒ **只有"被盘过的部位"才有意义**(没盘过 ⇒ 无从比较)。
		//
		//	⚠️ 缺口 = 搭赠 + 真盘亏 ⇒ 它只是**建议值**,必须让门店确认。
		//	⚠️ expectedCloseKg 已经减掉了**已确认的搭赠** ⇒
		//	  确认完搭赠再算 ⇒ 缺口归零 ⇒ 不再提示(不会反复催)。
		giftGapKg := decimal.Zero
		giftGapActualKg := decimal.Zero
		if _, counted := split.CounterActual[c]; counted {
			giftGapActualKg = split.CounterActual[c].Add(split.Cold[c])
			giftGapKg = expectedCloseKg.Sub(giftGapActualKg)
			if giftGapKg.IsNegative() {
				giftGapKg = decimal.Zero
			}
		}
		// ---- 期末量(R1,2026-10-30 owner:「期初不能有负数」)----
		//
		//	① **盘到的部位(含 0 kg)** ⇒ 期末量 = **实盘**(盘点单里的数);
		//	   ⚠️ 判据是"**盘点单里出现过这个部位**",**不是**"链的余额 map 里有没有它"
		//	     —— 余额链(`dailyBalanceTerms.add`)只记**正数**重量
		//	     ⇒ 盘到 0 kg(售罄)的部位压根不在 map 里
		//	     ⇒ 旧实现以为"没盘到" ⇒ 用了**负的账面推演**当期末量
		//	     ⇒ 负数写进 cut_cost_daily.closing_kg ⇒ 链把它当次日期初
		//	     ⇒ 门店看到"期初为什么有负数"(生产实测 店 00 2026-10-29 的 8 个部位)。
		//	② 其它部位 / 没有完整盘点单 ⇒ 期末量 = max(账面推演, 0)。
		//
		//	⇒ **链上永远是 ≥0** ⇒ 下一天的期初不会有负数。
		closeK := expectedCloseKg
		if hasStocktake {
			if v, ok := closeKg[c]; ok {
				closeK = v
			} else if _, counted := split.CounterActual[c]; counted {
				// 链余额里没有它(盘到 0 kg ⇒ 余额链只记正数)⇒ 用盘点单的实盘。
				closeK = split.CounterActual[c].Add(split.Cold[c])
			}
		}
		if closeK.IsNegative() {
			// 兜底:链上永远 ≥0(绝不让负期末流到次日期初)。
			closeK = decimal.Zero
		}
		closeAmtBasis := closeK.Mul(movingAvg)
		bulkCost := partBulkKg[c].Mul(movingAvg)
		wasteCost := partWasteKg[c].Mul(movingAvg)
		// ---- 搭赠成本(2026-10-31 owner 拍板)----
		//
		//	搭赠是「有成本、无收入」⇒ 它的成本**必须计上去**(否则毛利虚高),
		//	但要**单列一行**、并从「销售成本」里**剥出去**
		//	(销售成本只该含"真卖掉的那些货"的成本)。
		giftCost := partGiftKg[c].Mul(movingAvg)
		// ---- 账面不足(R2)与"短卖必须计成本"(R5)----
		//
		//	cut_short_sell_kg = max(0, 卖出+统收+报损 + 期末 − 可售量)
		//	⇒ 这部分货是**卖超了账面库存**的 ⇒ 它的成本必须计上,
		//	  否则销售成本算成 0 ⇒ **毛利虚高**(生产实测 店 00 2026-10-30:
		//	  14 个部位 sales_cost_yuan = 0 ⇒ 毛利率接近 100%)。
		//
		//	⚠️ 单价用**当期可用单价**(移动加权单价);当期算不出来时
		//	  movingAvg 已经是**链传下来的上期单价 / 兜底价**(见上面的 switch)。
		cutShortSellKg := soldTotal.Add(closeK).Sub(availKg)
		if cutShortSellKg.IsNegative() {
			cutShortSellKg = decimal.Zero
		}
		shortSellCost := cutShortSellKg.Mul(movingAvg)
		// ⚠️ **减掉搭赠成本**(2026-10-31):搭赠不是"卖掉的",
		//	它的成本不该被算成「销售成本」⇒ 销售成本只留"真卖掉的"。
		//	⇒ 搭赠成本单独成列(见 giftCost),并在毛利里扣掉。
		salesCost := availAmt.Sub(closeAmtBasis).Sub(bulkCost).Sub(wasteCost).Sub(giftCost).Add(shortSellCost)
		if salesCost.IsNegative() {
			// 期末实盘 > 可售金额(盘盈)⇒ 成本不能为负,裁到 0 并记日志。
			s.pubLogger.Warn("computeCutDailyCosts: 销售成本为负(盘盈),已裁 0",
				"branch_id", branchID, "biz_date", bizDate.Format("2006-01-02"),
				"cut_type", c, "sales_cost", salesCost.String())
			salesCost = decimal.Zero
		}
		revenue := partSoldAmount[c].Add(partBulkAmount[c])
		// 预期毛利(预售结转):期末结余按**部位预期售价**重估。
		//
		// ⚠️ 判据是"有没有预售结转行",不是"单价是否大于 0":
		//   维护为 **0 元**的货号也要结转(负毛利照做)⇒ 单价 0 一样要重估。
		revaluation := decimal.Zero
		presaleRevenue := decimal.Zero
		presaleUnit := decimal.Zero
		if unit, ok := partPresaleUnit[c]; ok && closeK.IsPositive() {
			presaleUnit = unit
			revaluation = closeK.Mul(unit.Sub(movingAvg))
			presaleRevenue = closeK.Mul(unit)
		}
		// ⚠️ 毛利里**扣掉搭赠成本**(owner 2026-10-31 拍板:扣毛利、单列一行)。
		gm := revenue.Sub(salesCost).Sub(bulkCost).Sub(wasteCost).Sub(giftCost).Add(revaluation)
		var gmPct float64
		gmBase := revenue.Add(presaleRevenue)
		if !gmBase.IsZero() {
			f, _ := gm.Mul(decimal.NewFromInt(100)).Div(gmBase).Float64()
			gmPct = f
		}

		// ---- 2026-10-30 owner 口径:这张表**整张都是猪肉台的** ----
		//
		// ① 搬上台的量(报表上的「搬上台」列,取代"期初"列):
		//    门店自己搬了多少上台,一眼认得;
		//    账面链值(期初)可以是负数(= 账面短卖)⇒ 门店看不懂"为什么期初是负的"。
		counterLoadKg := split.CounterLoad[c]
		// ② 建议单价(元/kg):「如果今天的猪按**建议分割**切,这刀货的成本单价」。
		//    口径 = (期初金额 + 今日入库金额按**建议**比例摊到的部分) ÷ (期初kg + 建议分割kg)
		//    ⚠️ nil = 当天**没有建议分割**(没进猪)⇒ 前端显示「—」,不显示 0。
		var suggestedUnitCost *decimal.Decimal
		if totalSuggestedKg.IsPositive() {
			suggestedAmount := allocatable.Mul(split.Suggested[c]).Div(totalSuggestedKg)
			suggestedKg := openK.Add(split.Suggested[c])
			if suggestedKg.IsPositive() {
				v := openA.Add(suggestedAmount).Div(suggestedKg)
				suggestedUnitCost = &v
			}
		}
		// ③ 实际均价(元/kg):当日这个部位**实际卖出**的平均单价 = 营收 ÷ 销量。
		//    (零售来自思迅日汇总表 t_rm_daysum;统收是本地登记,一并计入
		//     ⇒ 与「营收」列**自洽**:均价 × 销量 = 营收,门店能自己验算。)
		//    ⚠️ nil = 当天这个部位**没卖**(没销量)⇒ 前端显示「—」。
		var actualAvgUnitPrice *decimal.Decimal
		soldTotalKg := partSoldKg[c].Add(partBulkKg[c])
		if soldTotalKg.IsPositive() {
			v := revenue.Div(soldTotalKg)
			actualAvgUnitPrice = &v
		}
		// ④ 实际成本单价(元/kg)= 销售成本 ÷ 总销量(零售 + 统收)。
		//
		//	⚠️ 分母与「零售单价」一致 ⇒ 门店能自己验算:
		//	  零售单价 × 销量 = 营收;成本单价 × 销量 = 销售成本。
		//	⚠️ nil = 当天这个部位**没卖**⇒ 前端显示「—」(不是 0)。
		var actualCostUnit *decimal.Decimal
		if soldTotalKg.IsPositive() {
			v := salesCost.Div(soldTotalKg)
			actualCostUnit = &v
		}
		out = append(out, CutDailyCost{
			BranchID:       branchID,
			CutType:        cut,
			OpeningKg:      openK,
			OpeningAmount:  openA,
			ProducedKg:     prodK,
			ProducedAmount: prodA,
			SoldKg:         partSoldKg[c],
			SoldAmount:     partSoldAmount[c],
			BulkSoldKg:     partBulkKg[c],
			BulkSoldAmount: partBulkAmount[c],
			WasteKg:        partWasteKg[c],
			WasteAmount:    partWasteKg[c].Mul(movingAvg),
			// ---- 搭赠(2026-10-31)----
			GiftKg:                     partGiftKg[c],
			GiftCostYuan:               giftCost,
			GiftGapKg:                  giftGapKg,
			GiftGapActualKg:            giftGapActualKg,
			ExpectedClosingKg:          expectedCloseKg,
			ClosingKg:                  closeK,
			ClosingAmount:              closeAmtBasis,
			MovingAvgUnitCost:          movingAvg,
			SalesCostYuan:              salesCost,
			BulkCostYuan:               bulkCost,
			WasteCostYuan:              wasteCost,
			RevenueYuan:                revenue,
			GrossMarginYuan:            gm,
			// ⚠️ **必须钳制**:列是 numeric(8,4) ⇒ 装不下超过 ±9999.99 的毛利率
			//   ⇒ 不钳会直接写库失败、**打挂整条成本链物化**(见 clampMarginPct)。
			GrossMarginPct:             clampMarginPct(gmPct),
			PresaleMarginYuan:          revaluation,
			PresaleExpectedRevenueYuan: presaleRevenue,
			PresaleExpectedUnitYuan:    presaleUnit,
			DataSource:                 dataSource,
			CostUncomputable:           costUncomputable,
			// ---- 部位级分割数据(owner 定案)----
			SuggestedSplitKg: split.Suggested[c],
			ActualSplitKg:    splitActualKg[c],
			// ⚠ 调整后权重 = **成本实际用的那个权重**(= 入库量 prodK):
			//   守卫①(没有建议分割)时引擎**完全不接管** ⇒ 成本用的仍是今天的产出量
			//   ⇒ 这里报 prodK 而不是 0 ⇒ 与 purchased_kg 永远一致(前端两边对得上)。
			ActualSplitSource: splitSource[c],
			AdjustedSplitKg:   prodK,
			CounterActualKg:   split.CounterActual[c],
			ColdStoreKg:       coldStoreKg[c],
			// ---- 2026-10-30:猪肉台口径的三件新数据 ----
			CounterLoadKg:          counterLoadKg,
			SuggestedUnitCostYuan:  suggestedUnitCost,
			ActualAvgUnitPriceYuan: actualAvgUnitPrice,
			// ---- 2026-10-31:成本单价(建议 = 单价基线;实际 = 销售成本÷销量)----
			SuggestedCostUnitPriceYuan: suggestedCostUnitPtr[c],
			SuggestedCostPriceSource:   suggestedCostSource[c],
			ActualCostUnitPriceYuan:    actualCostUnit,
			SplitWeightNote:            split.Note,
			// ---- 短卖(2026-10-30)----
			CutShortSellKg:     cutShortSellKg,
			CarriedShortSellKg: carriedShortSellKg,
			// ⚠️ ShortSellKg(当日总量短卖的分摊)在**循环之后**回填
			//   (当日总量要等所有部位都算完才知道)。
		})
		recon.OpeningInventoryCostYuan = recon.OpeningInventoryCostYuan.Add(openA)
		recon.ProducedCostYuan = recon.ProducedCostYuan.Add(prodA)
		recon.SalesCostYuan = recon.SalesCostYuan.Add(salesCost)
		recon.BulkCostYuan = recon.BulkCostYuan.Add(bulkCost)
		recon.WasteCostYuan = recon.WasteCostYuan.Add(wasteCost)
		recon.GiftCostYuan = recon.GiftCostYuan.Add(giftCost)
		recon.ClosingInventoryCostYuan = recon.ClosingInventoryCostYuan.Add(closeAmtBasis)
		recon.RevaluationYuan = recon.RevaluationYuan.Add(revaluation)
		// ⚠️ 对账行的**短卖分量** = 各部位"账面不足"的合计(= 实际计入成本的那部分)。
		//   它必须与 SalesCostYuan 里多计的那笔**完全一致**,否则对账恒等式会破。
		recon.ShortSellKg = recon.ShortSellKg.Add(cutShortSellKg)
		recon.ShortSellYuan = recon.ShortSellYuan.Add(shortSellCost)
	}

	// ---- 当日**总量**短卖(R3)----
	//
	//	口径(owner 2026-10-30):**当天卖出的总量超过了账面可售的总量**
	//	⇒ 那部分货没有成本来源 ⇒ 才是"真短卖"。
	//
	//	⚠️ 分部位的账面不足**不算**:那是"建议分割与门店实际卖的不匹配"的**分摊口径**问题
	//	  (生产实测 店 00 2026-10-29:总量 191.709kg ≥ 卖出 178.149kg ⇒ 短卖 = 0;
	//	  但 fat 等 8 个部位的账面不足共 8.871kg ⇒ 只作提示)。
	//
	//	⚠️ 口径里**不含期末**:期末是"卖完之后还剩多少",它不影响"卖超了没有"
	//	  (生产实测 店 00 2026-10-30:账面只剩 20.879kg 却卖了 83.391kg ⇒ 短卖 62.512kg)。
	shortSellKg := outTotalKg.Sub(availTotalKg)
	if shortSellKg.IsNegative() {
		shortSellKg = decimal.Zero
	}
	// 分摊到各部位(按各部位自己的账面不足比例)⇒ Σ = 当日短卖。
	// ⚠️ 分摊只用于**报表**(告诉门店"哪个部位短卖了");计成本用的是各部位自己的账面不足。
	if shortSellKg.IsPositive() {
		totalGap := decimal.Zero
		for _, r := range out {
			totalGap = totalGap.Add(r.CutShortSellKg)
		}
		if totalGap.IsPositive() {
			for i := range out {
				gap := out[i].CutShortSellKg
				if !gap.IsPositive() {
					continue
				}
				out[i].ShortSellKg = shortSellKg.Mul(gap).Div(totalGap)
			}
		}
	}

	// 对账行:差额 = (期初 + 入库 + **短卖**) − (销售 + 统收 + 报损 + **搭赠** + 期末)
	//
	// ⚠️ **加了短卖分量后恒等式必须仍然闭合**(owner 2026-10-30):
	//	短卖的那部分货**照当期可用单价计了成本**(进了 SalesCostYuan)
	//	⇒ 左边必须把同一笔金额补回来 ⇒ 差额仍然是 0。
	//
	// ⚠️ **搭赠同理,但在右边**(2026-10-31):
	//	搭赠成本**已经从 SalesCostYuan 里减掉了**(销售成本只含"真卖掉的")
	//	⇒ 它是**流出侧**的一个独立分量 ⇒ 必须从右边减掉才能闭合。
	//	⚠️ 放错边(加到左边)会让差额变成 **2 × 搭赠成本**。
	//
	// ⚠️ **要 round**:移动加权单价是"金额 ÷ kg"除出来的 ⇒ 尾数 1e-16
	//   ⇒ 显示成 "-0.0000000000000001" 会让人以为**对账不平**。
	recon.PigPurchaseCostYuan = decimal.Zero
	for _, p := range pigs {
		recon.PigPurchaseCostYuan = recon.PigPurchaseCostYuan.Add(p.PurchaseCostYuan)
	}
	recon.UnallocatedPigCostYuan = unallocatedPigCost
	recon.DiffYuan = recon.OpeningInventoryCostYuan.Add(recon.ProducedCostYuan).
		Add(recon.ShortSellYuan).
		Sub(recon.SalesCostYuan).Sub(recon.BulkCostYuan).
		Sub(recon.WasteCostYuan).Sub(recon.GiftCostYuan).
		Sub(recon.ClosingInventoryCostYuan).
		Round(4)
	recon.CuttingLossKg = cuttingLossKg

	losses, err := s.computeCutOvernightShrink(ctx, branchID, bizDate, out)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	recon.OvernightShrinkKg = losses.OvernightShrinkKg

	// ---- 9. SKU 级明细行(只有销售 / 营收 / 分割产出) ----
	skuRows := buildSkuSalesRows(branchID, cutOfSku, nameOfSku, cutOf, out,
		producedKg, soldKg, soldAmount, bulkKg, bulkAmount, wasteKg)

	return out, skuRows, recon, losses, nil
}

// buildSkuSalesRows 由部位级引擎产出 **SKU 级**明细行。
//
// ⚠️ **只填 SKU 粒度真实存在的量**:销售 kg / 销售金额 / 统收 / 分割产出 / 营收。
//
// ⚠️ **期初 / 期末 / 销售成本 / 毛利一律留 0** —— 它们是**部位口径**:
//
//	库存是部位的(门店口径)⇒ SKU 粒度的库存是**假的**
//	⇒ 填一个数进去就是继续制造同一个坑(门店会拿它跟盘点对账)。
//
//	⇒ 门店要看 SKU 的销售/营收分列(2026-10-27),成本/毛利看部位行。
func buildSkuSalesRows(
	branchID string,
	cutOfSku map[string]model.CutType,
	nameOfSku map[string]string,
	cutOf func(sku string) string,
	cutRows []CutDailyCost,
	producedKg, soldKg, soldAmount, bulkKg, bulkAmount, wasteKg map[string]decimal.Decimal,
) []SkuDailyCost {
	// 部位 → 它的行(取 dataSource / 单价)。
	cutRow := map[string]CutDailyCost{}
	for _, r := range cutRows {
		cutRow[string(r.CutType)] = r
	}
	// SKU 集合:有流水的都算。
	skuSet := map[string]struct{}{}
	for sku := range producedKg {
		skuSet[sku] = struct{}{}
	}
	for sku := range soldKg {
		skuSet[sku] = struct{}{}
	}
	for sku := range soldAmount {
		skuSet[sku] = struct{}{}
	}
	for sku := range bulkKg {
		skuSet[sku] = struct{}{}
	}
	for sku := range wasteKg {
		skuSet[sku] = struct{}{}
	}
	list := make([]string, 0, len(skuSet))
	for sku := range skuSet {
		list = append(list, sku)
	}
	sort.Strings(list)

	out := make([]SkuDailyCost, 0, len(list))
	for _, sku := range list {
		cut := cutOf(sku)
		if cut == "" {
			continue
		}
		row := cutRow[cut]
		revenue := soldAmount[sku].Add(bulkAmount[sku])
		out = append(out, SkuDailyCost{
			BranchID:        branchID,
			CubeProductID:   sku,
			CubeProductName: nameOfSku[sku],
			CutType:         model.CutType(cut),
			ProducedKg:      producedKg[sku],
			SoldKg:          soldKg[sku],
			SoldAmount:      soldAmount[sku],
			BulkSoldKg:      bulkKg[sku],
			BulkSoldAmount:  bulkAmount[sku],
			WasteKg:         wasteKg[sku],
			RevenueYuan:     revenue,
			// ⚠️ 期初/期末/成本/毛利是**部位口径** ⇒ 这里不返(留 0)。
			DataSource: row.DataSource,
		})
	}
	return out
}

// cutPresaleUnits 把**SKU 级**的预期售价加权成**部位级**预期售价。
//
// # 口径(2026-10-27 决议 R4)
//
// 预期售价是**货号级**的(门店按货号填),而期末结余是**部位级**的
// ⇒ 部位预期售价 = Σ(SKU 预期售价 × SKU 权重) / Σ(SKU 权重)。
//
// ⚠️ 权重优先用**当日营收**(SKU 粒度里最可靠的量:销售是实打实的);
//
//	某货号当日没营收 ⇒ 退回**当日销量**;部位当期完全没动销 ⇒ 用该部位
//	预售货号的**简单平均**(权重无意义时至少给个可解释的数)。
func (s *Service) cutPresaleUnits(
	presaleUnits map[string]decimal.Decimal,
	cutOfSku map[string]model.CutType,
	skuSoldAmount, skuSoldKg map[string]decimal.Decimal,
) map[string]decimal.Decimal {
	if len(presaleUnits) == 0 {
		return map[string]decimal.Decimal{}
	}
	weightOf := func(sku string) decimal.Decimal {
		if v := skuSoldAmount[sku]; v.IsPositive() {
			return v
		}
		return skuSoldKg[sku]
	}
	num := map[string]decimal.Decimal{}
	den := map[string]decimal.Decimal{}
	simpleSum := map[string]decimal.Decimal{}
	simpleCnt := map[string]int{}
	for sku, unit := range presaleUnits {
		cut := string(cutOfSku[sku])
		if cut == "" {
			continue
		}
		w := weightOf(sku)
		num[cut] = num[cut].Add(unit.Mul(w))
		den[cut] = den[cut].Add(w)
		simpleSum[cut] = simpleSum[cut].Add(unit)
		simpleCnt[cut]++
	}
	out := map[string]decimal.Decimal{}
	for cut, d := range den {
		if d.IsPositive() {
			out[cut] = num[cut].Div(d)
			continue
		}
		if simpleCnt[cut] > 0 {
			out[cut] = simpleSum[cut].Div(decimal.NewFromInt(int64(simpleCnt[cut])))
		}
	}
	return out
}

// computeCutOvernightShrink 估"隔夜失重·盘亏"(部位口径)。
//
// 口径:昨日全店 − 今日全店 − (今日产出 − 今日卖出 − 今日报损)
//
// ⚠️ **负数不计入**(2026-10-27):部位期末为负 = **账面短卖**(卖超了库存),
//
//	那不是"货凭空消失",而是**卖多了** ⇒ 计进"隔夜失重"会把销售问题
//	误报成损耗问题 ⇒ 门店会去查损耗,而不是查销售。
//	⇒ 负余额(无论是今天的还是昨天的)一律按 0 参与合计,
//	  短卖本身由「账面短卖」红字告警暴露。
func (s *Service) computeCutOvernightShrink(ctx context.Context, branchID string, bizDate time.Time,
	rows []CutDailyCost) (*LossSummary, error) {
	out := &LossSummary{ShrinkNote: "期初沿用昨日日终重量,未做隔夜失重修正;实测销量可能被高估。"}
	prevStart, _ := s.previousBizDayBounds(bizDate)
	prevCounter, prevCold, ok, err := s.closingForDay(ctx, branchID, prevStart)
	if err != nil || !ok {
		return out, nil
	}
	prevTotal := decimal.Zero
	for _, v := range prevCold {
		if v.IsPositive() {
			prevTotal = prevTotal.Add(v)
		}
	}
	for _, v := range prevCounter {
		if v.IsPositive() {
			prevTotal = prevTotal.Add(v)
		}
	}
	if prevTotal.IsZero() {
		return out, nil
	}
	curTotal := decimal.Zero
	produced := decimal.Zero
	sold := decimal.Zero
	waste := decimal.Zero
	for _, r := range rows {
		if r.ClosingKg.IsPositive() {
			curTotal = curTotal.Add(r.ClosingKg)
		}
		produced = produced.Add(r.ProducedKg)
		sold = sold.Add(r.SoldKg).Add(r.BulkSoldKg)
		waste = waste.Add(r.WasteKg)
	}
	diff := prevTotal.Sub(curTotal).Sub(produced.Sub(sold.Sub(waste)))
	if diff.IsPositive() {
		out.OvernightShrinkKg = diff
		out.OvernightShrinkPct = 0
		if prevTotal.IsPositive() {
			f, _ := diff.Mul(decimal.NewFromInt(100)).Div(prevTotal).Float64()
			out.OvernightShrinkPct = f
		}
	}
	return out, nil
}
