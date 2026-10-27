// Package service / per_sku.go —— `per_sku` 行的**对外 DTO**(2026-10-27)。
//
// # 为什么要有这个 DTO(而不是直接返引擎的 `SkuDailyCost`)
//
// 成本链改**部位级**之后,`SkuDailyCost` 只剩"卖了多少、卖了多少钱"是真的;
// 库存 / 成本 / 毛利是**部位口径** ⇒ 挂在货号行上就是**假数据**
// (门店会拿它跟盘点对账 —— 那正是这次改造要消灭的坑)。
//
// ⇒ 对外的货号行必须**没有**那些键。
//
// # 「值 0」→「键缺失」
//
// ⚠️ `decimal.Decimal` 是 **struct**,encoding/json 的 `omitempty` 对 struct **无效**
//   ⇒ 用值类型时,零值照样序列化出 `"opening_kg":"0"`
//   ⇒ 前端拿到 0 就会显示"期初 0"(门店会误读成"我盘出来是 0 kg")。
//
// ⇒ 库存 / 成本 / 毛利这一组字段一律用 **指针** `*decimal.Decimal` + `omitempty`
//   ⇒ nil ⇒ **整个键缺失** ⇒ 前端显示「—」,而不是冒充一个 0。
//
// ⚠️ 销售量 / 营收 / 产出这些**货号粒度真实存在**的量保持**值类型**
//   ⇒ 恒有键(前端可以放心按"这个货号卖了 8kg"来渲染)。
//
// # 键名不变
//
// 对外 JSON 的**键名一个都不改**(前端不用动)⇒ 只是"有值"变成"键缺失"。
package service

import (
	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
)

// PerSkuGrossMargin 单个货号的**销售明细**行(`per_sku`)。
//
// # 它只承载"货号粒度真实存在"的量
//
//	产出 kg / 卖出 kg / 卖出金额 / 统收 / 报损 / 营收
//
// # 它**不承载**库存 / 成本 / 毛利
//
//	那些是**部位口径**(见 `PerCutGrossMargin`)⇒ 这一组的键**缺失**。
type PerSkuGrossMargin struct {
	BranchID        string        `json:"branch_id"`
	CubeProductID   string        `json:"cube_product_id"`
	CubeProductName string        `json:"cube_product_name,omitempty"`
	CutType         model.CutType `json:"cut_type"`

	// ---- 销售口径(值类型 ⇒ 恒有键)----

	// ProducedKg 是该货号当天分割产出的 kg(货号粒度真实存在)。
	ProducedKg decimal.Decimal `json:"produced_kg"`
	// SoldKg / SoldAmount 是零售销量与零售金额。
	SoldKg     decimal.Decimal `json:"sold_kg"`
	SoldAmount decimal.Decimal `json:"sold_amount"`
	// BulkSoldKg / BulkSoldAmount 是统收(卖给酒店的大宗)。
	BulkSoldKg     decimal.Decimal `json:"bulk_sold_kg"`
	BulkSoldAmount decimal.Decimal `json:"bulk_sold_amount"`
	// WasteKg 是该货号当天的报损量。
	WasteKg decimal.Decimal `json:"waste_kg"`
	// RevenueYuan 是该货号当天的营收(零售 + 统收)。
	RevenueYuan decimal.Decimal `json:"revenue_yuan"`

	// ---- 库存 / 成本 / 毛利(指针 ⇒ nil ⇒ **键缺失**)----

	// OpeningKg / OpeningAmount 是**期初** —— 库存是部位的,货号粒度没有 ⇒ 键缺失。
	OpeningKg     *decimal.Decimal `json:"opening_kg,omitempty"`
	OpeningAmount *decimal.Decimal `json:"opening_amount,omitempty"`
	// ExpectedClosingKg / ClosingKg / ClosingAmount 是**期末** —— 同上,键缺失。
	ExpectedClosingKg *decimal.Decimal `json:"expected_closing_kg,omitempty"`
	ClosingKg         *decimal.Decimal `json:"closing_kg,omitempty"`
	ClosingAmount     *decimal.Decimal `json:"closing_amount,omitempty"`
	// CounterKg / ColdStoreKg 是期末两位置分布 —— 同上,键缺失。
	CounterKg   *decimal.Decimal `json:"counter_kg,omitempty"`
	ColdStoreKg *decimal.Decimal `json:"cold_store_kg,omitempty"`
	// ProducedAmount / WasteAmount 是**成本口径**的金额 —— 键缺失。
	ProducedAmount *decimal.Decimal `json:"produced_amount,omitempty"`
	WasteAmount    *decimal.Decimal `json:"waste_amount,omitempty"`
	// MovingAvgUnitCost / SalesCostYuan / BulkCostYuan / WasteCostYuan 是**成本** —— 键缺失。
	MovingAvgUnitCost *decimal.Decimal `json:"moving_avg_unit_cost,omitempty"`
	SalesCostYuan     *decimal.Decimal `json:"sales_cost_yuan,omitempty"`
	BulkCostYuan      *decimal.Decimal `json:"bulk_cost_yuan,omitempty"`
	WasteCostYuan     *decimal.Decimal `json:"waste_cost_yuan,omitempty"`
	// GrossMarginYuan / GrossMarginPct 是**毛利** —— 键缺失。
	GrossMarginYuan *decimal.Decimal `json:"gross_margin_yuan,omitempty"`
	GrossMarginPct  *float64         `json:"gross_margin_pct,omitempty"`
	// 预售(预期毛利)结转的三列同样是**毛利口径** ⇒ 键缺失。
	PresaleMarginYuan          *decimal.Decimal `json:"presale_margin_yuan,omitempty"`
	PresaleExpectedRevenueYuan *decimal.Decimal `json:"presale_expected_revenue_yuan,omitempty"`
	PresaleExpectedUnitYuan    *decimal.Decimal `json:"presale_expected_unit_yuan,omitempty"`

	// DataSource:actual | partial | estimated(盘点口径)。
	DataSource string `json:"data_source"`
}

// perSkuGrossMarginsOf 把引擎内部的 SKU 行转成**对外 DTO**。
//
// ⚠️ 只带"货号粒度真实存在"的量;库存 / 成本 / 毛利**一律不带**
//   ⇒ 那几个字段是 nil ⇒ JSON 里**键缺失**(而不是 0)。
func perSkuGrossMarginsOf(rows []SkuDailyCost) []PerSkuGrossMargin {
	out := make([]PerSkuGrossMargin, 0, len(rows))
	for _, r := range rows {
		out = append(out, PerSkuGrossMargin{
			BranchID:        r.BranchID,
			CubeProductID:   r.CubeProductID,
			CubeProductName: r.CubeProductName,
			CutType:         r.CutType,
			ProducedKg:      r.ProducedKg,
			SoldKg:          r.SoldKg,
			SoldAmount:      r.SoldAmount,
			BulkSoldKg:      r.BulkSoldKg,
			BulkSoldAmount:  r.BulkSoldAmount,
			WasteKg:         r.WasteKg,
			RevenueYuan:     r.RevenueYuan,
			DataSource:      r.DataSource,
		})
	}
	return out
}
