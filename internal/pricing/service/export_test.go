package service

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/pricing/model"
)

// exportDay 造一个"当天零点"(价格与促销都是日粒度)。
func exportDay(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.Local)
}

// seedPriceRow 直接写一行售价(绕开导入路径,便于精确控制生效窗口)。
func seedPriceRow(t *testing.T, db *gorm.DB, listID, branchID, productID, price string, effectiveAt time.Time, expiredAt *time.Time) {
	t.Helper()
	err := db.Create(&model.PriceList{
		ID: listID, BranchID: branchID, ProductID: productID,
		PriceYuan:   decimal.RequireFromString(price),
		EffectiveAt: effectiveAt, ExpiredAt: expiredAt, Status: "active",
		CreatedBy: "test", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}).Error
	require.NoError(t, err)
}

// TestActivePricesAll_SameAnswerAsActivePrices 锁"全量与批量查价给**同一个答案**。
//
// ⚠️ 导出与查价的 WHERE 条件必须**逐字相同** ——
//
//	否则"批量查价说这个商品没价、导出说有价"
//	⇒ 客户端取哪个都不对,而且**零报错**。
func TestActivePricesAll_SameAnswerAsActivePrices(t *testing.T) {
	db := newTestDB(t)
	svc := NewPriceListService(db)
	ctx := context.Background()
	at := exportDay(2026, 10, 21)

	expired := exportDay(2026, 9, 1)
	seedPriceRow(t, db, "default", "10", "P1", "15.00", exportDay(2026, 1, 1), nil)
	seedPriceRow(t, db, "default", "10", "P2", "16.00", exportDay(2026, 12, 1), nil)
	seedPriceRow(t, db, "default", "10", "P3", "17.00", exportDay(2026, 1, 1), &expired)
	// 别的价目表 ⇒ 不在导出范围内(当前只有 default 一套)。
	seedPriceRow(t, db, "vip", "10", "P4", "18.00", exportDay(2026, 1, 1), nil)
	// 别的门店 ⇒ 不在导出范围内。
	seedPriceRow(t, db, "default", "01", "P5", "19.00", exportDay(2026, 1, 1), nil)

	all, err := svc.ActivePricesAll(ctx, "10", "", at)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, r := range all {
		ids[r.ProductID] = true
	}
	assert.Equal(t, map[string]bool{"P1": true}, ids,
		"只有生效中、属于 default 价目表、本门店的行")

	// 与批量查价逐一对齐。
	byID, err := svc.ActivePrices(ctx, "10", "", []string{"P1", "P2", "P3", "P4", "P5"}, at)
	require.NoError(t, err)
	assert.Len(t, byID, len(all), "全量与批量必须给同样多的行")
	for _, r := range all {
		_, ok := byID[r.ProductID]
		assert.True(t, ok, "%s 在批量查价里也必须命中", r.ProductID)
	}
}

// TestActivePromotionsAll_SameAnswerAsActivePromotions 锁"全量与批量促销给**同一个答案**。
//
// ⚠️ 同商品多促销 ⇒ 取**最低价**(cube promotion model 的 min_promo_price:
//
//	「用户实际能买到的最低价」)⇒ 导出与查价不能给出两个价。
func TestActivePromotionsAll_SameAnswerAsActivePromotions(t *testing.T) {
	db := newTestDB(t)
	svc := NewPromotionService(db)
	ctx := context.Background()
	at := exportDay(2026, 10, 21)

	for _, p := range []struct {
		id, productID, price string
		begin, end           time.Time
	}{
		{"PS-EXPENSIVE", "P1", "12.00", exportDay(2026, 1, 1), exportDay(2026, 12, 31)},
		{"PS-CHEAP", "P1", "8.80", exportDay(2026, 1, 1), exportDay(2026, 12, 31)},
		{"PS-EXPIRED", "P2", "5.00", exportDay(2026, 1, 1), exportDay(2026, 9, 30)},
		{"PS-FUTURE", "P3", "6.00", exportDay(2026, 12, 1), exportDay(2026, 12, 31)},
	} {
		require.NoError(t, db.Create(&model.Promotion{
			ID: p.id, BranchID: "10", ProductID: p.productID,
			PlanName:  "促销单" + p.id,
			PriceYuan: decimal.RequireFromString(p.price),
			BeginAt:   p.begin, EndAt: p.end, Status: "active",
			CreatedBy: "test", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}).Error)
	}
	// 别的门店 ⇒ 不在导出范围内。
	require.NoError(t, db.Create(&model.Promotion{
		ID: "PS-OTHER", BranchID: "01", ProductID: "P4",
		PlanName:  "别家店的促销",
		PriceYuan: decimal.RequireFromString("1.00"),
		BeginAt:   exportDay(2026, 1, 1), EndAt: exportDay(2026, 12, 31), Status: "active",
		CreatedBy: "test", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}).Error)

	all, err := svc.ActivePromotionsAll(ctx, "10", at)
	require.NoError(t, err)
	require.Len(t, all, 1, "只有 P1 在这个基准日生效")
	assert.Equal(t, "PS-CHEAP", all["P1"].ID, "同商品多促销取最低价那条")
	assert.Equal(t, "8.8", all["P1"].PriceYuan.String())

	byID, err := svc.ActivePromotions(ctx, "10", []string{"P1", "P2", "P3", "P4"}, at)
	require.NoError(t, err)
	assert.Len(t, byID, len(all), "全量与批量必须给同样多的行")
	for pid, p := range all {
		batch, ok := byID[pid]
		require.True(t, ok, "%s 在批量查促销里也必须命中", pid)
		assert.Equal(t, p.ID, batch.ID, "同商品选中的促销单必须一致")
	}
}
