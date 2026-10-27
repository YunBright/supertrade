package service

// internal/fresh-meat/service/presale.go —— **预期毛利(预售)结转**。
//
// # 为什么需要(2026-10-24 门店口径)
//
// 「审计则进行结转……审计后,产生结转(即快照)」
// +「本期产生结余,结余部分库存未进行毛利归属计算」
//
// ⇒ 审计日(D)的**结余**如果按**成本价**留在账上,它的毛利就永远没被归属:
//   下一期真按市价卖出去时毛利才出现 ⇒ **本期毛利被低估**、跨期归属错位。
//
// ⇒ 审计时按**预期销售单价**把这批结余**结转**掉(= 预售):
//
//	预期收入 = 结余 kg × 预期销售单价(有促销价按促销价)
//	结余成本 = 结余 kg × 当日移动加权单价(成本口径)
//	预售毛利 = 预期收入 − 结余成本
//
// # 两期总毛利不变(门店口径:「只要两期总体毛利没算错就行」)
//
//	审计期(D):  毛利 += 预售毛利(= 预期收入 − 结余成本)
//	下一期(D+1):期初单价 = 预期销售单价 ⇒ 按市价卖时成本 = 预期售价 ⇒ 毛利 ≈ 0
//	             实际价 ≠ 预期价的差额 ⇒ 落在 D+1(门店自己承担)
//
// ⇒ 两期毛利之和 = 实际毛利(不多算、不少算)✔
// ⇒ **不实现**"价格差调整回补上一期"(门店明确不要)。
//
// # 单价来源(门店口径 2.2)
//
// 「取 SKU 的**当前售价**(有促销价按促销价)」
// ⇒ 走 cube 的 product(零售价)+ promotion(当前生效促销价)两条批量查询,
//   **不查近期 POS 成交价**(那会扫太多流水、影响别的功能)。
//
// # 归猪规则
//
// ① **FIFO**:结余里的货按产生日回溯到**产出它的那头猪**
//   (与 service/pig_fifo.go 的零售毛利回归同一套"袋子归属快照");
// ② FIFO 摊不完的部分 ⇒ **兜底**按本次审计范围内各猪的**毛重比例**摊。
//
// ⇒ 无论走哪条,Σ(按猪) == Σ(按 SKU)—— 总数守恒(摊不掉的会显式报出来)。
//
// # 门店口径(2026-10-24 第四轮补充)
//
// 「实现了预售计提后,不存在归到上期的猪的情况」
// 「理论上审计+预售计提后所有猪的毛利已结算完毕,不会留给下一期,
//   预售计提就是为解决'上期的猪的毛利没算足'这个问题」
//
// ⇒ 往日压在中冷库的货,它的预期毛利在**本期审计**一次性结清,
//   且**归回产出它的那头猪**(不是挪给本期的猪)
//   ⇒ 那头猪的毛利在审计当期就**算足**,不留尾巴给下一期。
//
// # 单价拿不到 / 单价为 0 时怎么办
//
// 门店口径:「仍然做预售,负毛利也要将本期归属算干净」
//
//	有促销价       ⇒ 预期价 = 促销价
//	只有零售价     ⇒ 预期价 = 零售价
//	维护为 0 元    ⇒ 预期价 = 0 ⇒ 预售毛利 = −结余成本(**负毛利照做**,写减记)
//	取不到这个货号 ⇒ 预期价 = **成本口径单价** ⇒ 预售毛利 = 0
//	                (「取不到」是**不知道**,不是「卖 0 元」
//	                 ⇒ 当 0 处理会在取价源没同步全时把整批库存写减记到 0)

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm/clause"
)

// 预售单价的来源(写进 cut_presale_settlements.price_source)。
const (
	// PresalePriceSourcePromo 单价取自**促销价**。
	PresalePriceSourcePromo = "promo"
	// PresalePriceSourceRetail 单价取自**零售价**。
	PresalePriceSourceRetail = "retail"
	// PresalePriceSourceZero 货号**维护了价但就是 0 元** ⇒ 按 0 价结转。
	//
	// ⚠️ 这会把该 SKU 的结余**写减记到 0**(预售毛利 = −结余成本)。
	//   门店口径:「仍然做预售,负毛利也要将本期归属算干净」⇒ 不跳过。
	PresalePriceSourceZero = "zero"
	// PresalePriceSourceMissing **取不到这个货号的价**(未维护 / cube 里没有)
	//   ⇒ **也按 0 价结转**(门店口径:「取不到也按 0 生成」)。
	//
	// ⚠️ 门店会**经过一轮人工修改**修正这些 0(门店审计人负责)
	//   ⇒ 修正窗口 = 审计**之后**(见 presale_edit.go 的 PUT /stock/presale):
	//   预售价是**审计结转的产物**(审计人拍板"这批货按什么价卖掉"),
	//   改它是修正审计结论,不是改被冻结的源数据。
	PresalePriceSourceMissing = "missing"
	// PresalePriceSourceManual 单价是**人工改**的(门店审计人改的)。
	PresalePriceSourceManual = "manual"
)

// 预售毛利按猪分摊的归属规则。
const (
	// PresaleAllocFIFO 按袋子归属快照回溯到产出它的猪。
	PresaleAllocFIFO = "fifo"
	// PresaleAllocScopeFallback FIFO 摊不完 ⇒ 按审计范围内各猪毛重比例摊。
	PresaleAllocScopeFallback = "scope_fallback"
)

// PresaleNote 预售结转的说明(告诉门店"这笔钱是预期值、不是现金")。
const PresaleNote = "预售结转:按当前售价(有促销价按促销价)把结余的预期毛利结转到本期;" +
	"实际售价与预期售价的差额落在下一期(门店自行承担)。"

// PresaleSkuRow SKU 级预售明细(对外 JSON)。
type PresaleSkuRow struct {
	CubeProductID   string          `json:"cube_product_id"`
	CubeProductName string          `json:"cube_product_name,omitempty"`
	CutType         model.CutType   `json:"cut_type"`
	QtyKg           decimal.Decimal `json:"qty_kg"`
	// CostUnitYuan 是成本口径单价(审计日的移动加权单价)。
	CostUnitYuan decimal.Decimal `json:"cost_unit_yuan"`
	// CostAmountYuan 是结余成本。
	CostAmountYuan decimal.Decimal `json:"cost_amount_yuan"`
	// ExpectedUnitYuan 是**预期销售单价**(下一期期初单价就是它)。
	ExpectedUnitYuan   decimal.Decimal `json:"expected_unit_yuan"`
	ExpectedRevenueYuan decimal.Decimal `json:"expected_revenue_yuan"`
	// MarginYuan 是**预售毛利**(可为负:预期售价低于成本 ⇒ 预估亏本,不拦)。
	MarginYuan decimal.Decimal `json:"margin_yuan"`
	// PriceSource:promo | retail。
	PriceSource string `json:"price_source"`
}

// PresaleCutRow 部位级预售汇总(对外 JSON)。
type PresaleCutRow struct {
	CutType  model.CutType `json:"cut_type"`
	CutLabel string        `json:"cut_label"`
	QtyKg    decimal.Decimal `json:"qty_kg"`
	// CostAmountYuan 是该部位的结余成本合计。
	CostAmountYuan decimal.Decimal `json:"cost_amount_yuan"`
	// ExpectedUnitYuan 是**加权平均**预期售价(= 预期收入 ÷ 结余 kg)。
	ExpectedUnitYuan    decimal.Decimal `json:"expected_unit_yuan"`
	ExpectedRevenueYuan decimal.Decimal `json:"expected_revenue_yuan"`
	MarginYuan          decimal.Decimal `json:"margin_yuan"`
	Skus                []PresaleSkuRow `json:"skus"`
}

// PresaleView 一次预售结转的完整视图(对外 JSON)。
type PresaleView struct {
	BranchID string `json:"branch_id"`
	// BizDate 是结转日(= 审计截止日)。
	BizDate string `json:"biz_date"`
	// Rows 是按部位的汇总(升序)。
	Rows []PresaleCutRow `json:"rows"`
	TotalQtyKg             decimal.Decimal `json:"total_qty_kg"`
	TotalCostAmountYuan    decimal.Decimal `json:"total_cost_amount_yuan"`
	TotalExpectedRevenueYuan decimal.Decimal `json:"total_expected_revenue_yuan"`
	// TotalMarginYuan 是**预售毛利合计**(= 预期收入 − 结余成本)。
	TotalMarginYuan decimal.Decimal `json:"total_margin_yuan"`
	// MissingSkus 是**没维护售价**的 SKU ⇒ 这部分结余不做预售(毛利不结转)。
	MissingSkus []string `json:"missing_skus,omitempty"`
	// PriceError 是取价失败的说明(正常流程恒为空 —— 取价失败是**硬失败**,
	// 审计会直接失败,不会带着"没有价"把数据冻结掉)。
	PriceError string `json:"price_error,omitempty"`
	// UnattributedMarginYuan 是**摊不到任何猪**的预售毛利(显式报出来,不静默丢)。
	UnattributedMarginYuan decimal.Decimal `json:"unattributed_margin_yuan,omitempty"`
	// PigMargins 是按猪的分摊(与"正常 FIFO 毛利"并列相加 = 这头猪的总毛利)。
	PigMargins []PigPresaleMarginView `json:"pig_margins,omitempty"`
	// Note 是给门店看的说明。
	Note string `json:"note,omitempty"`
}

// PigPresaleMarginView 按猪的预售毛利分摊(对外 JSON)。
type PigPresaleMarginView struct {
	PigID string `json:"pig_id"`
	// CubeProductID 是这行分摊对应的 SKU(展示合并后可能为空)。
	CubeProductID string        `json:"cube_product_id,omitempty"`
	CutType       model.CutType `json:"cut_type"`
	QtyKg         decimal.Decimal `json:"qty_kg"`
	ExpectedRevenueYuan decimal.Decimal `json:"expected_revenue_yuan"`
	CostAmountYuan     decimal.Decimal `json:"cost_amount_yuan"`
	MarginYuan         decimal.Decimal `json:"margin_yuan"`
	// AllocRule:fifo | scope_fallback。
	AllocRule string `json:"alloc_rule"`
}

// presaleQuote 一条 SKU 的"当前售价"结论(内部用)。
type presaleQuote struct {
	// unit 是预期销售单价(有促销价按促销价)。
	unit decimal.Decimal
	// source 是来源:promo | retail。
	source string
}

// presaleRow 一个 SKU 的预售结转**计算结果**(内部用)。
type presaleRow struct {
	sku          string
	cut          model.CutType
	name         string
	qtyKg        decimal.Decimal
	costUnit     decimal.Decimal
	costAmount   decimal.Decimal
	expectedUnit decimal.Decimal
	expectedRev  decimal.Decimal
	margin       decimal.Decimal
	priceSource  string
}

// CutPresaleID 预售结转行的**确定性**代理主键。
func CutPresaleID(branchID, bizDate, cubeProductID string) string {
	return surrogateID("CPS", branchID, bizDate, cubeProductID)
}

// PigPresaleMarginID 按猪分摊行的**确定性**代理主键。
func PigPresaleMarginID(branchID, bizDate, cubeProductID, pigID string) string {
	return surrogateID("PPM", branchID, bizDate, cubeProductID, pigID)
}

// CutPresaleEditID 预售价修改痕迹的**确定性**代理主键。
//
// ⚠️ 含**时间戳** ⇒ 同一秒内改同一个货号两次会撞主键 ⇒ 用 upsert 覆盖
//   （保留最后一次的旧价/新价），不会丢痕迹。
func CutPresaleEditID(branchID, bizDate, cubeProductID string, at time.Time) string {
	return surrogateID("CPE", branchID, bizDate, cubeProductID, at.UTC().Format(time.RFC3339Nano))
}

// buildPresalePlan 算某个截止日的**预售结转方案**(不落库)。
//
// 步骤:
//  1. 取该日的**成本日快照**(per-SKU 的期末 kg / 期末成本 / 移动加权单价);
//  2. 取这批 SKU 的**当前售价**(零售价 + 促销价);
//  3. 逐 SKU 算:预期收入 / 结余成本 / 预售毛利。
//
// ⚠️ **没维护售价的 SKU 不做预售**(它的结余成本照旧留在账上)
//   ⇒ 它的毛利不结转、下一期期初单价也不改 ⇒ **显式列在 MissingSkus**。
func (s *Service) buildPresalePlan(ctx context.Context, branchID string, cutoff time.Time) (*PresaleView, error) {
	view := &PresaleView{
		BranchID: branchID,
		BizDate:  cutoff.In(s.bizTZOrUTC()).Format("2006-01-02"),
		Rows:     []PresaleCutRow{},
		Note:     PresaleNote,
	}
	rows, err := s.presaleSkuRows(ctx, branchID, cutoff)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return view, nil
	}
	_, names, _ := s.loadSkuCatalog(ctx, branchID, nil)
	skus := make([]string, 0, len(rows))
	for _, r := range rows {
		skus = append(skus, r.sku)
	}
	quotes, priceErr := s.currentPrices(ctx, skus)
	if priceErr != nil {
		// ⚠️ **取价服务不可用** ⇒ 硬失败(不静默跳过):
		//   审计是**不可逆**的一刀(截止日及之前全部冻结)⇒ 预售没算出来就冻结,
		//   门店再也补不了 ⇒ 宁可让审计失败、等价格服务恢复后重试。
		//
		//   (某个 SKU **没维护售价**是另一回事:那是业务事实,进 MissingSkus 继续。)
		return nil, priceErr
	}
	if quotes == nil {
		// 没有价格客户端(这个部署根本没接价格源)⇒ 本次不做预售(不猜价)。
		view.PriceError = "价格客户端未装配(取不到当前售价),本次不做预售结转"
		return view, nil
	}
	byCut := map[model.CutType][]PresaleSkuRow{}
	for _, r := range rows {
		q, ok := quotes[r.sku]
		if !ok {
			// ⚠️ **取不到这个货号的价** ⇒ **按 0 价结转**(门店口径:「取不到也按 0 生成」)
			//   ⇒ 预售毛利 = −结余成本(负毛利照做,本期归属结清)。
			//   ⇒ 会经过一轮人工修改(门店审计人负责)⇒ 修改窗口 = 审计之后(PUT /stock/presale)。
			view.MissingSkus = append(view.MissingSkus, r.sku)
			q = presaleQuote{unit: decimal.Zero, source: PresalePriceSourceMissing}
		}
		row := PresaleSkuRow{
			CubeProductID:    r.sku,
			CubeProductName:  names[r.sku],
			CutType:          r.cut,
			QtyKg:            r.qtyKg,
			CostUnitYuan:     r.costUnit,
			CostAmountYuan:   r.costAmount,
			ExpectedUnitYuan: q.unit,
			PriceSource:      q.source,
		}
		row.ExpectedRevenueYuan = r.qtyKg.Mul(q.unit)
		// ⚠️ 预售毛利**可以为负**(预期售价低于成本)⇒ 门店口径:
		//   「负毛利也要将本期归属算干净」⇒ 照做,不跳过。
		row.MarginYuan = row.ExpectedRevenueYuan.Sub(r.costAmount)
		byCut[r.cut] = append(byCut[r.cut], row)
		view.TotalQtyKg = view.TotalQtyKg.Add(r.qtyKg)
		view.TotalCostAmountYuan = view.TotalCostAmountYuan.Add(r.costAmount)
		view.TotalExpectedRevenueYuan = view.TotalExpectedRevenueYuan.Add(row.ExpectedRevenueYuan)
		view.TotalMarginYuan = view.TotalMarginYuan.Add(row.MarginYuan)
	}
	cuts := make([]model.CutType, 0, len(byCut))
	for c := range byCut {
		cuts = append(cuts, c)
	}
	sort.Slice(cuts, func(i, j int) bool { return cuts[i] < cuts[j] })
	for _, c := range cuts {
		skus := byCut[c]
		sort.Slice(skus, func(i, j int) bool { return skus[i].CubeProductID < skus[j].CubeProductID })
		agg := PresaleCutRow{CutType: c, CutLabel: model.CutLabel(c), Skus: skus}
		for _, r := range skus {
			agg.QtyKg = agg.QtyKg.Add(r.QtyKg)
			agg.CostAmountYuan = agg.CostAmountYuan.Add(r.CostAmountYuan)
			agg.ExpectedRevenueYuan = agg.ExpectedRevenueYuan.Add(r.ExpectedRevenueYuan)
			agg.MarginYuan = agg.MarginYuan.Add(r.MarginYuan)
		}
		if agg.QtyKg.IsPositive() {
			agg.ExpectedUnitYuan = agg.ExpectedRevenueYuan.Div(agg.QtyKg)
		}
		view.Rows = append(view.Rows, agg)
	}
	sort.Strings(view.MissingSkus)
	return view, nil
}

// presaleSkuRows 取该日**有结余**的 SKU 行(结余 kg + 成本口径单价)。
//
// # 数据来源 = 该营业日的**部位级**成本日快照(cut_cost_daily)
//
// ⚠️ 2026-10-27:成本链改**部位级** ⇒ 快照里**没有 SKU 粒度**
//   (cut_cost_daily 的唯一键是 branch_id + biz_date + cut_type)
//   ⇒ 结余 kg 与结余成本先按**部位**取,再按该部位各货号的**当前冷库余额占比**
//   拆到货号(见 cut_split.go 的 splitCutClosingToSkus)
//   ⇒ 预期售价是**货号级**的 ⇒ 拆开之后每个货号按**自己的**售价结转。
//
// ⇒ 结余 kg 与结余成本必须来自**同一部位的同一行**(否则 kg 与金额对不上)。
func (s *Service) presaleSkuRows(ctx context.Context, branchID string, cutoff time.Time) ([]presaleRow, error) {
	start, end := s.businessDayBounds(cutoff)
	var snaps []model.CutCostDaily
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date >= ? AND biz_date < ?", branchID, start, end).
		Order("cut_type ASC").
		Find(&snaps).Error; err != nil {
		return nil, fmt.Errorf("query cut_cost_daily(presale): %w", err)
	}
	cutOf, _, err := s.loadSkuCatalog(ctx, branchID, nil)
	if err != nil {
		return nil, err
	}
	// 部位 → 货号列表(拆开用)。
	skusOfCut := map[model.CutType][]string{}
	for sku, cut := range cutOf {
		if cut == "" || sku == "" {
			continue
		}
		skusOfCut[cut] = append(skusOfCut[cut], sku)
	}
	out := make([]presaleRow, 0, len(snaps))
	for _, sn := range snaps {
		if !sn.ClosingKg.IsPositive() {
			continue
		}
		cut := sn.CutType
		if cut == "" {
			// 没有部位映射的桶 ⇒ 不参与预售(部位口径之外的桶)。
			continue
		}
		costUnit := sn.MovingAvgUnitCost
		if !costUnit.IsPositive() {
			// 没有成本口径(从没进过成本池)⇒ 预售无从谈起。
			continue
		}
		skus := skusOfCut[cut]
		if len(skus) == 0 {
			continue
		}
		// 部位结余按货号拆(当前冷库余额占比;拆不动给主 SKU)。
		cutClosing := map[string]decimal.Decimal{string(cut): sn.ClosingKg}
		split := s.splitCutClosingToSkus(ctx, branchID, cutClosing)
		for _, sku := range skus {
			qty := split[sku]
			if !qty.IsPositive() {
				continue
			}
			out = append(out, presaleRow{
				sku:        sku,
				cut:        cut,
				qtyKg:      qty,
				costUnit:   costUnit,
				costAmount: qty.Mul(costUnit),
			})
		}
	}
	return out, nil
}

// currentPrices 取一批 SKU 的**当前售价**(有促销价按促销价)。
//
// 返回里没有的 SKU = **取不到价**(未维护 / cube 里没这个货号)
// ⇒ 调用方按**成本口径**结转(见 buildPresalePlan)。
//
// # 维护为 0 元的货号
//
// cube 返回 0 元 ⇒ **照 0 处理**(price_source=zero)⇒ 预售毛利 = −结余成本
// ⇒ 门店口径:「负毛利也要将本期归属算干净」⇒ 不跳过、不当"没维护"糊弄过去。
//
// # 两种"没有价"要分开
//
// ① **没有价格客户端**(这个部署没接价格源)⇒ 返 (nil, nil)
//   ⇒ 调用方**跳过**预售(不猜价),但**不是错误**;
// ② **价格源查询失败**(cube 挂了 / 超时)⇒ 返 (nil, err)
//   ⇒ **硬失败**:审计是不可逆的一刀,宁可失败也不能带着"没算出来的预售"把数据冻结。
func (s *Service) currentPrices(ctx context.Context, skus []string) (map[string]presaleQuote, error) {
	if s.cube == nil {
		return nil, nil
	}
	quotes, err := s.cube.BatchCurrentPrices(ctx, skus, "")
	if err != nil {
		return nil, fmt.Errorf("查当前售价失败: %w", err)
	}
	out := make(map[string]presaleQuote, len(quotes))
	for id, q := range quotes {
		unit := decimal.Zero
		source := PresalePriceSourceRetail
		switch {
		case q.PromoPriceYuan != nil && q.PromoPriceYuan.IsPositive():
			unit = *q.PromoPriceYuan
			source = PresalePriceSourcePromo
		case q.PromoPriceYuan != nil:
			// 有促销价但 **0 元** ⇒ 促销价优先(哪怕是 0)。
			unit = *q.PromoPriceYuan
			source = PresalePriceSourceZero
		case q.PriceYuan != nil:
			unit = *q.PriceYuan
			if !unit.IsPositive() {
				source = PresalePriceSourceZero
			}
		default:
			// 两边都没有价 ⇒ 交给调用方按成本口径处理。
			continue
		}
		out[id] = presaleQuote{unit: unit, source: source}
	}
	return out, nil
}

// settlePresale 把预售结转**落库**(审计时调用)。
//
// 落三样东西:
//  1. `cut_presale_settlements` —— 预售结转行(单价是"下一期期初单价"的持久事实);
//  2. 该营业日的**成本日快照**:期末金额改成**预期售价口径**
//     ⇒ 下一期的期初金额自动按预期售价进来(链式,不用额外逻辑);
//  3. `pig_presale_margins` —— 预售毛利按猪分摊。
func (s *Service) settlePresale(ctx context.Context, branchID string, cutoff time.Time, plan *PresaleView) error {
	if plan == nil || len(plan.Rows) == 0 {
		return nil
	}
	now := s.now()
	bizDate := datatypesDate(cutoff.In(s.bizTZOrUTC()))
	// 同一 (门店, 归属日) 只留本次这一批 ⇒ 重跑覆盖。
	//
	// ⚠️ 用**营业日窗口**(不是 ±24 小时):窗口再宽一点就会把**前一天**的
	//   结转行也删掉(上一次审计的预售就没了)。
	dayStart, dayEnd := s.businessDayBounds(cutoff)
	if err := s.db.WithContext(ctx).Where("branch_id = ? AND biz_date >= ? AND biz_date < ?",
		branchID, dayStart, dayEnd).
		Delete(&model.CutPresale{}).Error; err != nil {
		return fmt.Errorf("清理旧的预售结转行: %w", err)
	}
	if err := s.db.WithContext(ctx).Where("branch_id = ? AND biz_date >= ? AND biz_date < ?",
		branchID, dayStart, dayEnd).
		Delete(&model.PigPresaleMargin{}).Error; err != nil {
		return fmt.Errorf("清理旧的预售分摊行: %w", err)
	}
	for _, cutRow := range plan.Rows {
		for _, r := range cutRow.Skus {
			rec := &model.CutPresale{
				ID:                 CutPresaleID(branchID, plan.BizDate, r.CubeProductID),
				BranchID:           branchID,
				BizDate:            bizDate,
				CubeProductID:      r.CubeProductID,
				CutType:            r.CutType,
				QtyKg:              r.QtyKg,
				CostUnitYuan:       r.CostUnitYuan,
				CostAmountYuan:     r.CostAmountYuan,
				ExpectedUnitYuan:   r.ExpectedUnitYuan,
				ExpectedRevenueYuan: r.ExpectedRevenueYuan,
				MarginYuan:         r.MarginYuan,
				PriceSource:        r.PriceSource,
				Note:               PresaleNote,
				CreatedAt:          now,
				UpdatedAt:          now,
			}
			if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "id"}},
				DoUpdates: clause.Assignments(map[string]any{
					"cut_type":              rec.CutType,
					"qty_kg":                rec.QtyKg,
					"cost_unit_yuan":        rec.CostUnitYuan,
					"cost_amount_yuan":      rec.CostAmountYuan,
					"expected_unit_yuan":    rec.ExpectedUnitYuan,
					"expected_revenue_yuan": rec.ExpectedRevenueYuan,
					"margin_yuan":           rec.MarginYuan,
					"price_source":          rec.PriceSource,
					"note":                  rec.Note,
					"updated_at":            rec.UpdatedAt,
				}),
			}).Create(rec).Error; err != nil {
				return fmt.Errorf("upsert cut_presale_settlements %s: %w", rec.ID, err)
			}
		}
	}
	// ② **不动**成本日快照(那是冻结的成本口径)⇒ 下一期期初金额的重估
	//    由 loadSkuOpening 按预售表现算(见 moving_average.go)。
	// ③ 按猪分摊。
	return s.writePigAllocations(ctx, branchID, cutoff, plan)
}

// writePigAllocations 把预售毛利**按猪**分摊落库(结转与改价共用)。
//
// ⚠️ 同一 (门店, 归属日, SKU, 猪) 只有一行 ⇒ 重跑覆盖 ⇒ 改价后重算是干净的。
func (s *Service) writePigAllocations(ctx context.Context, branchID string, cutoff time.Time, plan *PresaleView) error {
	if plan == nil || len(plan.Rows) == 0 {
		return nil
	}
	now := s.now()
	bizDate := datatypesDate(cutoff.In(s.bizTZOrUTC()))
	alloc, unattributed, err := s.allocatePresaleToPigs(ctx, branchID, cutoff, plan)
	if err != nil {
		return err
	}
	plan.UnattributedMarginYuan = unattributed
	// 展示用:同一头猪的同一部位合并成一行(库里仍按 SKU 分行存)。
	plan.PigMargins = mergePigPresale(alloc)
	for _, a := range alloc {
		rec := &model.PigPresaleMargin{
			ID:                   PigPresaleMarginID(branchID, plan.BizDate, a.CubeProductID, a.PigID),
			BranchID:             branchID,
			BizDate:              bizDate,
			CubeProductID:        a.CubeProductID,
			PigID:                a.PigID,
			CutType:              a.CutType,
			QtyKg:                a.QtyKg,
			ExpectedRevenueYuan: a.ExpectedRevenueYuan,
			CostAmountYuan:      a.CostAmountYuan,
			MarginYuan:          a.MarginYuan,
			AllocRule:            a.AllocRule,
			CreatedAt:            now,
		}
		if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "id"}},
			DoUpdates: clause.Assignments(map[string]any{
				"cut_type":              rec.CutType,
				"qty_kg":                rec.QtyKg,
				"expected_revenue_yuan": rec.ExpectedRevenueYuan,
				"cost_amount_yuan":      rec.CostAmountYuan,
				"margin_yuan":           rec.MarginYuan,
				"alloc_rule":            rec.AllocRule,
			}),
		}).Create(rec).Error; err != nil {
			return fmt.Errorf("upsert pig_presale_margins %s: %w", rec.ID, err)
		}
	}
	return nil
}

// allocatePresaleToPigs 把预售结余按 FIFO 归到产出它的猪。
//
// ① FIFO:按**产生日**回溯结转记录(袋子),用袋子里的**归属快照**分摊;
// ② 摊不完的部分 ⇒ 兜底:按**本次审计范围内**各猪的毛重比例摊。
//
// 返回 (分摊明细, 摊不到任何猪的金额)。
func (s *Service) allocatePresaleToPigs(ctx context.Context, branchID string, cutoff time.Time, plan *PresaleView) (
	[]PigPresaleMarginView, decimal.Decimal, error) {
	out := []PigPresaleMarginView{}
	unattributed := decimal.Zero
	pigCache := map[string][]model.WholePig{}
	cutoffDate := datatypesDate(cutoff.In(s.bizTZOrUTC()))
	scopeFallback := map[string]decimal.Decimal{}
	scopeLoaded := false

	for _, cutRow := range plan.Rows {
		for _, r := range cutRow.Skus {
			need := r.QtyKg
			// ---- ① FIFO ----
			var records []model.StockBag
			if err := s.db.WithContext(ctx).
				Where("branch_id = ? AND cube_product_id = ? AND origin_date <= ?",
					branchID, r.CubeProductID, cutoffDate).
				Order("origin_date ASC, id ASC").
				Find(&records).Error; err != nil {
				return nil, decimal.Zero, fmt.Errorf("query stock_bags(presale fifo): %w", err)
			}
			for _, rec := range records {
				if !need.IsPositive() {
					break
				}
				remaining := rec.QtyKg.Sub(rec.ConsumedKg)
				if !remaining.IsPositive() {
					continue
				}
				shares, err := s.fifoRecordShares(ctx, s.db, rec, pigCache)
				if err != nil {
					return nil, decimal.Zero, err
				}
				if len(shares) == 0 {
					continue // 这批算不出归属 ⇒ 不吃(摊不出去的货不能白吃)
				}
				total := decimal.Zero
				for _, sh := range shares {
					total = total.Add(sh.kg)
				}
				if !total.IsPositive() {
					continue
				}
				take := remaining
				if take.GreaterThan(need) {
					take = need
				}
				for _, sh := range shares {
					kg := take.Mul(sh.kg).Div(total)
					if !kg.IsPositive() {
						continue
					}
					out = append(out, PigPresaleMarginView{
						PigID:               sh.pigID,
						CubeProductID:       r.CubeProductID,
						CutType:             r.CutType,
						QtyKg:               kg,
						ExpectedRevenueYuan: kg.Mul(r.ExpectedUnitYuan),
						CostAmountYuan:      kg.Mul(r.CostUnitYuan),
						MarginYuan:          kg.Mul(r.ExpectedUnitYuan.Sub(r.CostUnitYuan)),
						AllocRule:           PresaleAllocFIFO,
					})
				}
				need = need.Sub(take)
			}
			// ---- ② 兜底:审计范围内的猪按毛重比例 ----
			if need.IsPositive() {
				if !scopeLoaded {
					pigs, err := s.pigsInAuditScope(ctx, branchID, cutoff)
					if err != nil {
						return nil, decimal.Zero, err
					}
					var totalW decimal.Decimal
					for _, p := range pigs {
						totalW = totalW.Add(p.GrossWeightKg)
						scopeFallback[p.ID] = decimal.Zero
					}
					if totalW.IsPositive() {
						for _, p := range pigs {
							scopeFallback[p.ID] = p.GrossWeightKg
						}
					}
					scopeLoaded = true
				}
				var totalW decimal.Decimal
				for _, w := range scopeFallback {
					totalW = totalW.Add(w)
				}
				if !totalW.IsPositive() {
					// 摊不到任何猪 ⇒ 显式报出来(不静默丢)。
					unattributed = unattributed.Add(need.Mul(r.ExpectedUnitYuan.Sub(r.CostUnitYuan)))
					continue
				}
				for _, p := range sortedPigIDs(scopeFallback) {
					w := scopeFallback[p]
					if !w.IsPositive() {
						continue
					}
					kg := need.Mul(w).Div(totalW)
					if !kg.IsPositive() {
						continue
					}
					out = append(out, PigPresaleMarginView{
						PigID:               p,
						CubeProductID:       r.CubeProductID,
						CutType:             r.CutType,
						QtyKg:               kg,
						ExpectedRevenueYuan: kg.Mul(r.ExpectedUnitYuan),
						CostAmountYuan:      kg.Mul(r.CostUnitYuan),
						MarginYuan:          kg.Mul(r.ExpectedUnitYuan.Sub(r.CostUnitYuan)),
						AllocRule:           PresaleAllocScopeFallback,
					})
				}
			}
		}
	}
	// 同一个 pig 可能分到多行(不同 SKU / 不同规则)⇒ 按 pig+cut 合并展示。
	out = mergePigPresale(out)
	return out, unattributed, nil
}

// mergePigPresale 把同一头猪的同一部位的多行合并成一行(展示用)。
func mergePigPresale(rows []PigPresaleMarginView) []PigPresaleMarginView {
	type key struct {
		pig string
		cut model.CutType
	}
	merged := map[key]*PigPresaleMarginView{}
	order := []key{}
	for _, r := range rows {
		k := key{r.PigID, r.CutType}
		m, ok := merged[k]
		if !ok {
			cp := r
			cp.CubeProductID = "" // 合并后不再指向单个 SKU
			merged[k] = &cp
			order = append(order, k)
			continue
		}
		m.QtyKg = m.QtyKg.Add(r.QtyKg)
		m.ExpectedRevenueYuan = m.ExpectedRevenueYuan.Add(r.ExpectedRevenueYuan)
		m.CostAmountYuan = m.CostAmountYuan.Add(r.CostAmountYuan)
		m.MarginYuan = m.MarginYuan.Add(r.MarginYuan)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].pig != order[j].pig {
			return order[i].pig < order[j].pig
		}
		return order[i].cut < order[j].cut
	})
	out := make([]PigPresaleMarginView, 0, len(order))
	for _, k := range order {
		out = append(out, *merged[k])
	}
	return out
}

// pigsInAuditScope 取**本次审计范围内**进场的猪(兜底分摊用)。
//
// 范围 = 上次审计截止日之后 ~ 本次截止日(没审计过 ⇒ 从开业起)。
func (s *Service) pigsInAuditScope(ctx context.Context, branchID string, cutoff time.Time) ([]model.WholePig, error) {
	lastCutoff, err := s.auditCutoff(ctx, branchID)
	if err != nil {
		return nil, err
	}
	start, end := s.businessDayBounds(cutoff)
	if lastCutoff != "" {
		start = mustParseDateInLoc(lastCutoff, s.bizTZOrUTC()).AddDate(0, 0, 1)
	}
	var pigs []model.WholePig
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND arrived_at >= ? AND arrived_at < ?", branchID, start, end).
		Order("arrived_at ASC, id ASC").
		Find(&pigs).Error; err != nil {
		return nil, fmt.Errorf("query whole_pigs(audit scope): %w", err)
	}
	return pigs, nil
}

// loadPresaleUnits 取某营业日的**预期销售单价**(per-SKU)。
//
// 用途:毛利引擎算该营业日的期末估值时,用它把结余**重估**到预期售价口径。
//
// ⚠️ 没有预售结转的日子 ⇒ 空 map ⇒ 期末仍按成本口径(行为不变)。
func (s *Service) loadPresaleUnits(ctx context.Context, branchID string, bizDate time.Time) (map[string]decimal.Decimal, error) {
	start, end := s.businessDayBounds(bizDate)
	var rows []model.CutPresale
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date >= ? AND biz_date < ?", branchID, start, end).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query cut_presale_settlements: %w", err)
	}
	out := map[string]decimal.Decimal{}
	for _, r := range rows {
		out[r.CubeProductID] = r.ExpectedUnitYuan
	}
	return out, nil
}

// ListPresale 查某营业日的**预售结转**(读端点)。
func (s *Service) ListPresale(ctx context.Context, branchID string, bizDate time.Time) (*PresaleView, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	start, end := s.businessDayBounds(bizDate)
	var rows []model.CutPresale
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date >= ? AND biz_date < ?", branchID, start, end).
		Order("cube_product_id ASC").
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query cut_presale_settlements: %w", err)
	}
	view := &PresaleView{
		BranchID: branchID,
		BizDate:  bizDate.In(s.bizTZOrUTC()).Format("2006-01-02"),
		Rows:     []PresaleCutRow{},
	}
	if len(rows) == 0 {
		return view, nil
	}
	_, names, _ := s.loadSkuCatalog(ctx, branchID, nil)
	byCut := map[model.CutType][]PresaleSkuRow{}
	for _, r := range rows {
		row := PresaleSkuRow{
			CubeProductID:      r.CubeProductID,
			CubeProductName:    names[r.CubeProductID],
			CutType:            r.CutType,
			QtyKg:              r.QtyKg,
			CostUnitYuan:       r.CostUnitYuan,
			CostAmountYuan:     r.CostAmountYuan,
			ExpectedUnitYuan:   r.ExpectedUnitYuan,
			ExpectedRevenueYuan: r.ExpectedRevenueYuan,
			MarginYuan:         r.MarginYuan,
			PriceSource:        r.PriceSource,
		}
		byCut[r.CutType] = append(byCut[r.CutType], row)
		view.TotalQtyKg = view.TotalQtyKg.Add(r.QtyKg)
		view.TotalCostAmountYuan = view.TotalCostAmountYuan.Add(r.CostAmountYuan)
		view.TotalExpectedRevenueYuan = view.TotalExpectedRevenueYuan.Add(r.ExpectedRevenueYuan)
		view.TotalMarginYuan = view.TotalMarginYuan.Add(r.MarginYuan)
	}
	cuts := make([]model.CutType, 0, len(byCut))
	for c := range byCut {
		cuts = append(cuts, c)
	}
	sort.Slice(cuts, func(i, j int) bool { return cuts[i] < cuts[j] })
	for _, c := range cuts {
		skus := byCut[c]
		sort.Slice(skus, func(i, j int) bool { return skus[i].CubeProductID < skus[j].CubeProductID })
		agg := PresaleCutRow{CutType: c, CutLabel: model.CutLabel(c), Skus: skus}
		for _, r := range skus {
			agg.QtyKg = agg.QtyKg.Add(r.QtyKg)
			agg.CostAmountYuan = agg.CostAmountYuan.Add(r.CostAmountYuan)
			agg.ExpectedRevenueYuan = agg.ExpectedRevenueYuan.Add(r.ExpectedRevenueYuan)
			agg.MarginYuan = agg.MarginYuan.Add(r.MarginYuan)
		}
		if agg.QtyKg.IsPositive() {
			agg.ExpectedUnitYuan = agg.ExpectedRevenueYuan.Div(agg.QtyKg)
		}
		view.Rows = append(view.Rows, agg)
	}
	// 按猪分摊。
	var pigs []model.PigPresaleMargin
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date >= ? AND biz_date < ?", branchID, start, end).
		Order("pig_id ASC, cut_type ASC").
		Find(&pigs).Error; err != nil {
		return nil, fmt.Errorf("query pig_presale_margins: %w", err)
	}
	for _, p := range pigs {
		view.PigMargins = append(view.PigMargins, PigPresaleMarginView{
			PigID:               p.PigID,
			CutType:             p.CutType,
			QtyKg:               p.QtyKg,
			ExpectedRevenueYuan: p.ExpectedRevenueYuan,
			CostAmountYuan:      p.CostAmountYuan,
			MarginYuan:          p.MarginYuan,
			AllocRule:           p.AllocRule,
		})
	}
	view.Note = PresaleNote
	return view, nil
}
