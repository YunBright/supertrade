package service

// internal/fresh-meat/service/bulk_fifo.go —— 大宗结算的 FIFO 队列 = **结转记录队列**。
//
// # 2026-10-25 口径修正(为什么改)
//
// 旧实现:大宗结算 FIFO 吃的是 **bulk_productions 台账**,而零售/搬台吃的是
// **carryover_records 结转记录** ⇒ 同一批冷库货有**两份"还剩多少"的账**
// ⇒ 结算按台账扣、零售按结转记录扣 ⇒ 同一批货**能被卖两遍**
// (结算说"还有 10kg 可结",零售把那 10kg 也卖了 ⇒ 收入算两遍、库存算负)。
//
// ⇒ 改成**一个队列**(carryover_records):
//
//	结算按产生日(origin_date)FIFO 领用**结转记录**;
//	领用就扣 consumed_kg ⇒ 与零售**共用同一个消耗水位** ⇒ 不可能卖两遍;
//	猪归属用**同一套**:结转记录的 PigAlloc 归属快照 → 没有才按当日各猪毛重兜底。
//
// ⇒ 「结算的领用」与「结转记录」从此是**同一件事**(门店口径:结转记录就是账)。
//
// # 位置口径
//
// 结算领用时货可能在**冷库**,也可能在**猪肉台**(搬上台过的)
// ⇒ 库存按**领用时那批货所在的位置**扣(冷库扣冷库、柜台扣柜台)
// ⇒ 日余额链也按位置拆(冷库出库才减冷库链),见 service/daily_balance.go。

import (
	"context"
	"fmt"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// bulkCarryoverTake 结算从某一条结转记录上领用的一笔。
type bulkCarryoverTake struct {
	// Record 是被领用的那条结转记录。
	Record model.CarryoverRecord
	// QtyKg 是从它上面领用了多少。
	QtyKg decimal.Decimal
	// Shares 是这批货的**猪份额**(摊到哪几头猪、各多少 kg)。
	Shares []fifoShare
}

// allocateBulkByBIFO 按 FIFO 从**结转记录队列**领用 qtyKg。
//
// # 队列口径
//
//	该 SKU 的 **active** 结转记录(在冷库还是在猪肉台都算)
//	+ origin_date <= uptoDate(只能领用结算日之前(含当天)就结转出来的货)
//	+ 排序 origin_date ASC, id ASC(先进先出)
//
// ⚠️ 与零售 FIFO **完全同一套队列** ⇒ 两边共用 consumed_kg 这个"已消耗水位"
// ⇒ 同一批货**不可能被零售卖一遍、又被大宗结一遍**。
//
// # 返回
//
//	(领用明细, 还差多少)
//	⇒ 还差 > 0 ⇒ 队列里不够 ⇒ 调用方报错(不落半张单)。
func (s *Service) allocateBulkByBIFO(
	ctx context.Context, tx *gorm.DB,
	branchID, cubeProductID string, uptoDate datatypes.Date, qtyKg decimal.Decimal,
) ([]bulkCarryoverTake, decimal.Decimal, error) {
	if !qtyKg.IsPositive() {
		return nil, decimal.Zero, nil
	}
	var records []model.CarryoverRecord
	if err := tx.WithContext(ctx).
		Where("branch_id = ? AND cube_product_id = ? AND status = ? AND origin_date <= ?",
			branchID, cubeProductID, CarryoverStatusActive, uptoDate).
		Order("origin_date ASC, id ASC").
		Find(&records).Error; err != nil {
		return nil, decimal.Zero, fmt.Errorf("query carryover_records(bulk fifo): %w", err)
	}
	need := qtyKg
	out := make([]bulkCarryoverTake, 0, len(records))
	pigCache := map[string][]model.WholePig{}
	for _, rec := range records {
		if !need.IsPositive() {
			break
		}
		remaining := rec.QtyKg.Sub(rec.ConsumedKg)
		if !remaining.IsPositive() {
			continue // 这批已经卖完了(零售或大宗结过)
		}
		take := remaining
		if take.GreaterThan(need) {
			take = need
		}
		shares, err := s.bulkCarryoverShares(ctx, tx, rec, uptoDate, pigCache)
		if err != nil {
			return nil, decimal.Zero, err
		}
		out = append(out, bulkCarryoverTake{Record: rec, QtyKg: take, Shares: shares})
		need = need.Sub(take)
	}
	return out, need, nil
}

// bulkCarryoverShares 这批货的**猪份额**(结算收入摊到哪几头猪)。
//
// # 三级归属(前两级与零售 FIFO 完全一致)
//
// ① 结转记录的**归属快照**(PigAlloc):盘点那一刻钉死的 —— 首选;
// ② 那天的各猪**毛重比例**:盘点那天没补录分割 ⇒ 算不出精确归属;
// ③ **结算当天的**各猪毛重比例:这批货来自更早(甚至期初),
//
//	那天根本没有猪 ⇒ 摊给"结算时在场的猪"(比丢钱好)。
//
// ⚠️ 三级都没有 ⇒ 返回空 ⇒ 调用方记一行**无归属猪**的收入
//
//	(钱不能凭空消失,但也不能硬塞给某头猪)。
func (s *Service) bulkCarryoverShares(
	ctx context.Context, tx *gorm.DB,
	rec model.CarryoverRecord, settleDate datatypes.Date, pigCache map[string][]model.WholePig,
) ([]fifoShare, error) {
	shares, err := s.fifoRecordShares(ctx, tx, rec, pigCache)
	if err != nil {
		return nil, err
	}
	if len(shares) > 0 {
		return shares, nil
	}
	// 兜底③:结算当天的猪。
	pigs, err := s.pigsOfBizDay(ctx, tx, rec.BranchID, settleDate, pigCache)
	if err != nil {
		return nil, err
	}
	totalW := decimal.Zero
	for _, p := range pigs {
		totalW = totalW.Add(p.GrossWeightKg)
	}
	if !totalW.IsPositive() {
		return nil, nil
	}
	out := make([]fifoShare, 0, len(pigs))
	for _, p := range pigs {
		out = append(out, fifoShare{pigID: p.ID, kg: p.GrossWeightKg})
	}
	return out, nil
}

// bulkCarryoverLocation 结算领用时这批货所在的位置(空 ⇒ 冷库)。
func bulkCarryoverLocation(rec model.CarryoverRecord) model.StockLocation {
	if rec.Location == string(model.StockLocationCounter) {
		return model.StockLocationCounter
	}
	return model.StockLocationColdStore
}
