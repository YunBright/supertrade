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
	BranchID            string              `json:"branch_id"`
	Date                string              `json:"date"` // YYYY-MM-DD(bizTZ)
	IsStocktakeComplete bool                `json:"is_stocktake_complete"`
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
	PerCut              []PerCutGrossMargin `json:"per_cut"`
	// PerSku 是**货号级销售明细** —— 只承载"货号粒度真实存在"的量
	// (产出 / 销量 / 营收 / 统收 / 报损)。
	//
	// ⚠️ 2026-10-27:成本链改**部位级** ⇒ **计算单元是部位行**(`per_cut`),
	//   库存 / 成本 / 毛利**不在货号行里**(那些键是**缺失**,不是 0)
	//   ⇒ 门店要看成本看部位行,货号行只回答"这个货号卖了多少、卖了多少钱"。
	PerSku []PerSkuGrossMargin `json:"per_sku"`
	Total  TotalGrossMargin   `json:"total"`
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
	Alerts      []MarginAlert `json:"alerts,omitempty"`
	// Presale 是**预期毛利(预售结转)**明细(审计日才有;其余日子不返)。
	//
	// ⚠️ 它的金额**已经含在** PerCut / Total 的毛利里(期末结余按预期售价重估)
	//   ⇒ 这一节是"把预期毛利单独列出来给门店看",不是再加一次。
	Presale *PresaleView `json:"presale,omitempty"`
	// PresaleHint 是**悬浮提示**用的数据(前端直接显示,不用自己拼)。
	//
	// 门店口径:「显示全部毛利(含预期毛利),鼠标悬浮时显示含多少预期毛利」
	// ⇒ 页面上的毛利 = Total.GrossMarginYuan(**已含**预期毛利);
	//   悬浮提示 = 本 Hint 里的 Text(含金额)。
	PresaleHint *PresaleHint `json:"presale_hint,omitempty"`
	GeneratedAt time.Time     `json:"generated_at"`
}

// PresaleHint 毛利里的**预期毛利**占比(悬浮提示用)。
type PresaleHint struct {
	// GrossMarginYuan 是本期毛利(**已含**预期毛利)。
	GrossMarginYuan decimal.Decimal `json:"gross_margin_yuan"`
	// PresaleMarginYuan 是其中**预期毛利**的金额(负数 = 预估亏本)。
	PresaleMarginYuan decimal.Decimal `json:"presale_margin_yuan"`
	// ExpectedRevenueYuan 是预售的**预期收入**(不是现金收入)。
	ExpectedRevenueYuan decimal.Decimal `json:"expected_revenue_yuan"`
	// Text 是可直接显示的一句话。
	Text string `json:"text"`
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
)

// PerCutGrossMargin 单部位的毛利明细(由其下各 SKU 汇总)。
type PerCutGrossMargin struct {
	CutType           model.CutType   `json:"cut_type"`
	OpeningKg         decimal.Decimal `json:"opening_kg"`
	PurchasedKg       decimal.Decimal `json:"purchased_kg"` // 本期入库 kg(语义同旧:进货)
	SoldKg            decimal.Decimal `json:"sold_kg"`
	BulkSoldKg        decimal.Decimal `json:"bulk_sold_kg"`
	WasteKg           decimal.Decimal `json:"waste_kg"`
	// ActualRemainKg 是**实盘**剩余(kg);**nil = 当天没有实盘**。
	//
	// ⚠️ 必须用**指针**:decimal.Decimal 是 struct,encoding/json 的 omitempty
	// 对 struct **无效** ⇒ 用值类型时零值也会序列化出 "actual_remain_kg":"0"
	// ⇒ 前端拿到 0 就会显示"实盘剩余 0"(门店会误读成"我盘出来是 0 kg")。
	// 指针 nil ⇒ 整个键缺失 ⇒ 前端显示「—」。
	ActualRemainKg *decimal.Decimal `json:"actual_remain_kg,omitempty"`
	// ExpectedRemainKg 是**账面应有**剩余(kg)—— 任何口径下都有值。
	ExpectedRemainKg decimal.Decimal `json:"expected_remain_kg"`
	// VarianceKg = 实盘 − 应有;**nil = 当天没有实盘**(恒 0 是**假的**"对上了")。
	VarianceKg *decimal.Decimal `json:"variance_kg,omitempty"`
	RevenueYuan       decimal.Decimal `json:"revenue_yuan"`
	// ⚠️ 改名(2026-10-23):旧字段 `purchase_cost_yuan` 是"按消耗比例摊销的猪采购成本";
	// 新字段是**本期销售成本**(移动加权结转)—— 两者语义不同,不是同一个数。
	// 旧的"摊销采购成本"在"今天没进猪的部位今天在卖"时会算成 0 ⇒ 毛利虚高。
	SalesCostYuan     decimal.Decimal `json:"sales_cost_yuan"`
	// BulkCostYuan 是**统收成本**(卖给酒店的大宗品项的结转成本)。
	//
	// ⚠️ 2026-10-23 增:原来它没进毛利,导致"卖板油给酒店"被算成**零成本收入**
	// (毛利率 100%)—— 实测:卖 10kg 收 60 元、成本 1000 元,毛利显示 +60,
	// 真实应是 **−940**。现在它显式成列,毛利 = 收入 − 销售成本 − 统收成本 − 报损成本。
	BulkCostYuan   decimal.Decimal `json:"bulk_cost_yuan"`
	// WasteCostYuan 是**报损成本**(扔掉的货的成本)。
	//
	// ⚠️ 同上:原来报损的成本**凭空消失**(不进损益)⇒ 报损不体现为损失。
	WasteCostYuan    decimal.Decimal `json:"waste_cost_yuan"`
	GrossMarginYuan   decimal.Decimal `json:"gross_margin_yuan"`
	GrossMarginPct    float64         `json:"gross_margin_pct"`
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
	BulkCostYuan     decimal.Decimal `json:"bulk_cost_yuan"`
	WasteCostYuan    decimal.Decimal `json:"waste_cost_yuan"`
	GrossMarginYuan  decimal.Decimal `json:"gross_margin_yuan"`
	GrossMarginPct   float64         `json:"gross_margin_pct"`
	// PresaleMarginYuan / PresaleExpectedRevenueYuan 与部位行同义(预期毛利结转)。
	PresaleMarginYuan          decimal.Decimal `json:"presale_margin_yuan,omitempty"`
	PresaleExpectedRevenueYuan decimal.Decimal `json:"presale_expected_revenue_yuan,omitempty"`
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

	perCut, total := rollUpPerCutCuts(cutRows, promoAlloc)

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

	// 预期毛利(预售结转):只有做过结转的营业日才有 ⇒ 没有就不挂这一节。
	var presale *PresaleView
	if pv, err := s.ListPresale(ctx, branchID, day); err == nil && len(pv.Rows) > 0 {
		presale = pv
	}
	// 悬浮提示:毛利里的预期毛利占比。
	var hint *PresaleHint
	if presale != nil && !total.PresaleMarginYuan.IsZero() {
		hint = &PresaleHint{
			GrossMarginYuan:       total.GrossMarginYuan,
			PresaleMarginYuan:     total.PresaleMarginYuan,
			ExpectedRevenueYuan:   total.PresaleExpectedRevenueYuan,
			Text: fmt.Sprintf("毛利 %s 元中含预期毛利 %s 元(结余按当前售价结转;实际售价与预期售价的差额落在下一期)",
				total.GrossMarginYuan.String(), total.PresaleMarginYuan.String()),
		}
	}

	// 销售来源(2026-10-26):让门店看得见"收入是从哪读出来的"。
	salesSource, salesSourceNote, err := s.salesSourceOf(ctx, branchID)
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
		PerCut:              perCut,
		// ⚠️ SKU 明细**只承载销售 / 营收 / 分割产出**(2026-10-27 口径):
		//   库存 / 成本 / 毛利是**部位口径** ⇒ SKU 行里**不返**(键缺失)。
		PerSku: perSkuGrossMarginsOf(skuRows),
		Total:               total,
		Reconciliation:      *recon,
		Losses:              *losses,
		PromoAllocation:     promoAlloc,
		Alerts:              s.buildCutMarginAlerts(ctx, branchID, day, cutRows),
		Presale:             presale,
		PresaleHint:         hint,
		GeneratedAt:         s.now(),
	}, nil
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
