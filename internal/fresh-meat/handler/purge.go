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
		// 库存位置 + 成本状态 + 成本日快照(2026-10-23 补登记)。
		//
		// ⚠️ 前两张原来是**复合主键** ⇒ 进不了 PurgeRegistry("模型没有主键")
		// ⇒ 契约测试写在上面的行**永远清不掉** ⇒ 生产库里会一天天攒下
		// "0 kg 的幽灵行",库存查询里会冒出根本不存在的 SKU。
		// ⇒ 已改成"单列代理主键 + 业务唯一索引"(见 model / service.surrogate_id.go),
		// 这里才能登记进来。
		purge.Model{Table: "stock_bags", Model: &model.StockBag{}},
		purge.Model{Table: "sku_stock_positions", Model: &model.SkuStockPosition{}},
		purge.Model{Table: "sku_cost_states", Model: &model.SkuCostState{}},
		purge.Model{Table: "sku_cost_daily", Model: &model.SkuCostDaily{}},
		// 2026-10-27:成本链改**部位级** ⇒ 新增两张部位级成本表。
		//
		// ⚠️ 两张表都是**单列代理主键**(确定性哈希)⇒ 能进 PurgeRegistry
		// ⇒ 契约测试 / 单测写在上面的行**清得掉**。
		//
		// ⚠️ 旧的 `sku_cost_daily` / `sku_cost_states` **停写**(历史保留只读)
		//   ⇒ 仍留在清单里(清掉历史残留),但不会再有新行。
		purge.Model{Table: "cut_cost_daily", Model: &model.CutCostDaily{}},
		purge.Model{Table: "cut_cost_states", Model: &model.CutCostState{}},
		// 2026-10-24:营业日状态机 + 搬台流水 + 按营业日的结余快照。
		//
		// ⚠️ 这三张表都有**单列代理主键**(确定性哈希)⇒ 能进 PurgeRegistry
		// ⇒ 契约测试 / 单测写在上面的行**清得掉**。
		purge.Model{Table: "business_days", Model: &model.BusinessDay{}},
		purge.Model{Table: "counter_loads", Model: &model.CounterLoadRecord{}},
		purge.Model{Table: "cut_stock_daily", Model: &model.CutStockDaily{}},
		purge.Model{Table: "sku_stock_openings", Model: &model.SkuStockOpening{}},
		purge.Model{Table: "biz_audits", Model: &model.BizAudit{}},
		// 2026-10-24:预期毛利(预售)结转 + 按猪分摊(单列代理主键 ⇒ 可登记)。
		purge.Model{Table: "cut_presale_settlements", Model: &model.CutPresale{}},
		purge.Model{Table: "pig_presale_margins", Model: &model.PigPresaleMargin{}},
		purge.Model{Table: "cut_presale_edits", Model: &model.CutPresaleEdit{}},
		// 装配/配置类:seed 写的行**没有 run_id** ⇒ 不会被记账 ⇒ 本次运行删不到它们
		// ⇒ 不会被误删。登记上只是兜底。
		purge.Model{Table: "branch_cut_mappings", Model: &model.BranchCutMapping{}},
		purge.Model{Table: "sku_cost_daily", Model: &model.SkuCostDaily{}},
		//
		// ⚠️ **不登记** `pig_forecast_baselines`:主键是复合的(体重段×部位),
		// purge 按**单列**主键定位 ⇒ 报"模型没有主键"。
		// 而它是**派生状态**(由日快照按业务键 upsert 出来的统计基线)
		// ⇒ 不是"每次跑都多一行"的测试垃圾,本来就该留在库里。
	)
}

// RegisterPurgeRoutes 挂 POST /internal/datastore/purge。
//
// 免鉴权,且公网**不可达**(见文件头)。
func RegisterPurgeRoutes(r *gin.Engine, db *gorm.DB) {
	r.POST("/internal/datastore/purge", purge.Handler(db, PurgeRegistry()))
}
