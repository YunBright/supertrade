// Package service / bulk_carryover.go —— 大宗品项的**记账 SKU** 与**成本快照**。
//
// # 2026-10-25 口径修正:大宗台账(bulk_productions)已**停写**
//
// 旧实现里,日终盘点会往台账写一条"结转记录"(bulk_productions),
// 而**结转记录(袋子)**(stock_bags)也记同一件事 ⇒ 同一事实两份账
// ⇒ 结算按台账扣、零售按袋子扣 ⇒ 同一批货**能被卖两遍**。
//
// ⇒ 现在只有**一个**记录:结转记录(袋子)。
//	- 进冷库:日终盘点自动登记袋子(registerStockBags)/
//	          大宗手工兜底也登记袋子(RecordBulkProduction);
//	- 出冷库:零售与大宗结算**共用同一个 FIFO 队列**、同一个 consumed_kg 水位;
//	- 成本:入冷库那一刻的移动加权单价**钉在袋子上**(unit_cost_yuan)
//	      ⇒ 冷库压几天后结算,成本仍是入库那天的口径。
//
// # 三条口径,各管一件事
//
//	库存/结存  ← 位置表(sku_stock_positions)主 SKU 的**冷库余额**   ← 物理真相
//	归属/批次  ← 结转记录(stock_bags:袋子 + 散货行)                ← FIFO 归到哪头猪
//	收入/成本  ← line_sales_by_pigs(cube_product_id = **主 SKU**)   ← 移动加权自动匹配
//
// ⚠️ **库存与收入必须记在真实部位 SKU 上**,不能记在 `bulk:<code>` 伪 SKU 上:
// 伪 SKU 不在部位映射里 ⇒ 没有入库金额 ⇒ 移动加权单价恒为 0
// ⇒ 统收成本算成 0 ⇒ 卖板油的毛利率是 **100%**。
package service

import (
	"context"
	"fmt"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// bulkCutSku 取大宗品项对应部位的**主 SKU**。
//
// ⚠️ 大宗库存与收入必须记在真实部位 SKU 上(不是 `bulk:<code>` 伪 SKU):
// 伪 SKU 不在部位映射里 ⇒ 没有入库金额 ⇒ 移动加权单价恒为 0
// ⇒ 统收成本 0 ⇒ 毛利率 100%(2026-10-23 实测)。
func (s *Service) bulkCutSku(ctx context.Context, branchID string, cut model.CutType) (string, error) {
	return s.bulkCutSkuFrom(ctx, nil, branchID, cut)
}

// bulkCutSkuFrom 同 bulkCutSku,但可指定事务。
//
// ⚠️ **事务里必须用它**(2026-10-30):SQLite 单测库只开 1 条连接
//   ⇒ 事务内再走 s.db 读 ⇒ 拿不到连接 ⇒ **死锁**。
func (s *Service) bulkCutSkuFrom(ctx context.Context, tx *gorm.DB, branchID string, cut model.CutType) (string, error) {
	m, err := s.primarySkuPerCutFrom(ctx, tx, branchID)
	if err != nil {
		return "", err
	}
	sku := m[cut]
	if sku == "" {
		return "", fmt.Errorf("%w: 部位 %s 没有映射任何 cube SKU —— 大宗库存要有地方记账(先配部位映射)", ErrInvalidInput, cut)
	}
	return sku, nil
}

// bulkStorageSKU 取大宗品项的**库存记账 SKU**(部位主 SKU)。
//
// ⚠️ 为什么按**部位主 SKU**记:盘点是 per-SKU 的,但**冷库里的油是混在一起的**
// —— 仓管盘"油还有 80kg"时,不会区分那是板油还是水油(同一个油桶)。
// ⇒ 冷库必须按**部位**归集,否则"盘点填 00032 / POS 卖掉 03808"会让冷库变负数。
//
// ⇒ 进冷库 / 出冷库 / 结存 / 收入**全记在部位主 SKU 上**(00032),三者自洽。
//
// (2026-10-24:大宗品项不再绑 POS 货号 ⇒ 这个口径只由"部位"决定。)
func (s *Service) bulkStorageSKU(ctx context.Context, branchID string, prod *model.BulkProduct) (string, error) {
	return s.bulkCutSku(ctx, branchID, prod.CutType)
}

// bulkUnitCost 取某**部位**当前的移动加权单价(元/kg)。
//
// ⚠️ 2026-10-27:成本链改**部位级** ⇒ 大宗成本单价也按**部位**取
// (大宗本来就是按部位记的:bulk_settlement_allocations.cut_type
//  与 bulk_products.cut_type ⇒ 口径更一致)。
//
// 优先用已落库的**部位级成本状态**(cut_cost_states,每次盘点后刷新);
// 没有就用当日实时算的。
// 拿不到就返回 0(调用方要能容忍 0 —— 别把"没有成本"当成"成本是 0"来静默通过)。
func (s *Service) bulkUnitCost(ctx context.Context, branchID string, cut model.CutType, day time.Time) decimal.Decimal {
	if cut == "" {
		return decimal.Zero
	}
	var st model.CutCostState
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND cut_type = ?", branchID, cut).
		First(&st).Error; err == nil && st.MovingAvgUnitCost.IsPositive() {
		return st.MovingAvgUnitCost
	}
	rows, _, _, _, err := s.computeCutDailyCosts(ctx, branchID, day)
	if err != nil {
		return decimal.Zero
	}
	for _, r := range rows {
		if r.CutType == cut && r.MovingAvgUnitCost.IsPositive() {
			return r.MovingAvgUnitCost
		}
	}
	return decimal.Zero
}

// bulkUnitCostMap **一次性**取多个**部位**的移动加权单价(元/kg)。
//
// ⚠️ 为什么必须批量:兜底路径 computeCutDailyCosts 会算**一整天**的成本,
//
//	逐个部位调用 ⇒ N 倍开销(一次盘点几十个部位 ⇒ 几十遍全量成本计算)。
//
// ⇒ 这里先查**成本状态**(便宜),只对**缺的**那批算**一次**当天成本。
func (s *Service) bulkUnitCostMap(ctx context.Context, branchID string, day time.Time, cuts []model.CutType) map[model.CutType]decimal.Decimal {
	out := map[model.CutType]decimal.Decimal{}
	if len(cuts) == 0 {
		return out
	}
	var states []model.CutCostState
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND cut_type IN ?", branchID, cuts).
		Find(&states).Error; err == nil {
		for _, st := range states {
			if st.MovingAvgUnitCost.IsPositive() {
				out[st.CutType] = st.MovingAvgUnitCost
			}
		}
	}
	var missing []model.CutType
	for _, cut := range cuts {
		if _, ok := out[cut]; !ok {
			missing = append(missing, cut)
		}
	}
	if len(missing) == 0 {
		return out
	}
	rows, _, _, _, err := s.computeCutDailyCosts(ctx, branchID, day)
	if err != nil {
		s.pubLogger.Warn("bulkUnitCostMap: 当天成本算不出来,成本快照按 0 记",
			"branch_id", branchID, "err", err)
		return out
	}
	for _, r := range rows {
		if r.MovingAvgUnitCost.IsPositive() {
			out[r.CutType] = r.MovingAvgUnitCost
		}
	}
	return out
}
