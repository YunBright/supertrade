// purge.go —— fresh-meat 的兜底清理(契约测试第二层 sweep)。
//
// # 为什么需要
//
// 第一层清理挂在每个测试的 t.Cleanup 上,但**进程被 Ctrl-C / kill / panic 时一行都不执行**
// —— 而契约测试打的是生产,于是那一次跑造出来的行就永久留下了。
//
// fresh-meat 的契约测试会写:
//   - promo_skus      (特价档货号登记;DELETE 是**停用**不是物理删)
//   - bulk_products   (大宗品项登记;DELETE 也是停用)
//   - bulk_productions / bulk_settlements / bulk_settlement_allocations
//
// ⇒ 停用型删除**清不掉行** ⇒ 每跑一次契约测试就多留一行 ⇒ 必须有 sweep。
//
// # 免鉴权端点的公网可达性
//
// 与 stocktake 同款:nginx `location ^~ /api/v1/fresh-meat/internal/ { return 404; }`
// 把公网挡死,注册在 auth 中间件之前的窗口,调用方走 dapr 内网 invoke。
package handler

import (
	"github.com/YunBright/core/purge"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
)

// PurgeRegistry 是 fresh-meat 的可清理实体清单。
//
// ⚠️ 顺序不是正确性要求(每张表独立记账、独立删),但**从表排前面**能让报告里的
// "删了几行"更准(主表先删会让从表数出 0)。
func PurgeRegistry() *purge.Registry {
	return purge.NewRegistry(
		// 从表
		purge.Model{Table: "bulk_settlement_allocations", Model: &model.BulkSettlementAllocation{}},
		purge.Model{Table: "bulk_settlements", Model: &model.BulkSettlement{}},
		purge.Model{Table: "bulk_settlement_items", Model: &model.BulkSettlementItem{}},
		purge.Model{Table: "bulk_productions", Model: &model.BulkProduction{}},
		// 主表
		purge.Model{Table: "bulk_products", Model: &model.BulkProduct{}},
		purge.Model{Table: "promo_skus", Model: &model.PromoSku{}},
		// 业务表(契约测试会写 pig_cuts / pork_cuts_stocktakes / line_sales_by_pigs 等)
		purge.Model{Table: "pig_cuts", Model: &model.PigCut{}},
		purge.Model{Table: "pork_cuts_stocktakes", Model: &model.PorkCutsStocktake{}},
		purge.Model{Table: "line_sales_by_pigs", Model: &model.LineSalesByPig{}},
		purge.Model{Table: "waste_logs", Model: &model.WasteLog{}},
		purge.Model{Table: "whole_pigs", Model: &model.WholePig{}},
		// 登记的表必须有**单列主键**(复合主键进不了 PurgeRegistry ⇒ 永远清不掉)。
		// 停写的历史表(sku_cost_daily / sku_cost_states)仍保留登记,以清理残留行。
		purge.Model{Table: "stock_bags", Model: &model.StockBag{}},
		purge.Model{Table: "sku_stock_positions", Model: &model.SkuStockPosition{}},
		purge.Model{Table: "sku_cost_states", Model: &model.SkuCostState{}},
		purge.Model{Table: "sku_cost_daily", Model: &model.SkuCostDaily{}},
		purge.Model{Table: "cut_cost_daily", Model: &model.CutCostDaily{}},
		purge.Model{Table: "cut_cost_states", Model: &model.CutCostState{}},
		// 营业日状态机 + 搬台流水 + 按营业日的结余快照。
		//
		// ⚠️ 这三张表都有**单列代理主键**(确定性哈希)⇒ 能进 PurgeRegistry
		// ⇒ 契约测试 / 单测写在上面的行**清得掉**。
		purge.Model{Table: "business_days", Model: &model.BusinessDay{}},
		purge.Model{Table: "counter_loads", Model: &model.CounterLoadRecord{}},
		purge.Model{Table: "cut_stock_daily", Model: &model.CutStockDaily{}},
		purge.Model{Table: "sku_stock_openings", Model: &model.SkuStockOpening{}},
		// ⚠️ 2026-11-01 owner：审计结转端点(`/stock/audit`)已下线 ⇒ 这张表不再有新行写入，
		//   但**保留 purge 登记** —— 它还存着老店的"最后一次审计截止日"，
		//   那是余额链基线锚点 + 期初结转清理的水位线(只读，见 service/audit_history.go)。
		//   ⇒ 必须能清得掉，否则契约测试/单测写进去的行永远留在生产库。
		purge.Model{Table: "biz_audits", Model: &model.BizAudit{}},
		// ⚠️ 2026-11-01 owner：预期毛利(预售结转)端点(`/stock/presale`)已下线
		//   ⇒ 这三张表同样只剩历史存量(结转写路径已退役)，
		//   但账面期末重估仍要读 `cut_presale_settlements.expected_unit_yuan`
		//   ⇒ 保留登记，等下一张卡随账面链一起彻底移除。
		purge.Model{Table: "cut_presale_settlements", Model: &model.CutPresale{}},
		purge.Model{Table: "pig_presale_margins", Model: &model.PigPresaleMargin{}},
		purge.Model{Table: "cut_presale_edits", Model: &model.CutPresaleEdit{}},
		// 装配/配置类:seed 写的行**没有 run_id** ⇒ 不会被记账 ⇒ 本次运行删不到它们
		// ⇒ 不会被误删。登记上只是兜底。
		purge.Model{Table: "branch_cut_mappings", Model: &model.BranchCutMapping{}},
		purge.Model{Table: "sku_cost_daily", Model: &model.SkuCostDaily{}},
		// 分割基线 + 估计样本 + 跨店借用预留(2026-10-30 A 方案)。
		//
		// ⚠️ 这三张表**现在都有单列代理主键**(`id`):
		//   - pig_forecast_baselines.id = "PFB" + 体重段 + 部位 + 门店号
		//   - pig_cut_estimates.id      = surrogateID("PCE", 店, 猪, 部位)
		//   - pig_forecast_baseline_borrows.id = surrogateID(...)
		// ⇒ purge 按**单列**主键定位 ⇒ **进得了 PurgeRegistry** ⇒ 清得掉。
		//
		// (旧注释说"pig_forecast_baselines 主键是复合的 ⇒ 登记不了" —— 那是代理主键
		//  改造**之前**的情况,现在已不成立 ⇒ 留着它 = 契约测试在生产留下的
		//  基线/估计样本行永远清不掉。)
		purge.Model{Table: "pig_forecast_baselines", Model: &model.PigForecastBaseline{}},
		purge.Model{Table: "pig_cut_estimates", Model: &model.PigCutEstimate{}},
		purge.Model{Table: "pig_forecast_baseline_borrows", Model: &model.PigForecastBaselineBorrow{}},
	)
}

// RegisterPurgeRoutes 挂 POST /internal/datastore/purge。
//
// 免鉴权,且公网**不可达**(见文件头)。
func RegisterPurgeRoutes(r *gin.Engine, db *gorm.DB) {
	r.POST("/internal/datastore/purge", purge.Handler(db, PurgeRegistry()))
}
