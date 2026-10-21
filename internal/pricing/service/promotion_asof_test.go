package service

import (
	"context"
	"testing"

	"github.com/YunBright/supertrade/internal/pricing/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 基准日（促销是时效数据 ⇒ 导入按它过滤）。
const asOfDay = "2026-10-21"

func promoRow(planNo, productID, beginDay, endDay, price string) PromotionInput {
	return PromotionInput{
		ID:            planNo,
		ProductID:     productID,
		PlanName:      planNo,
		PriceYuan:     price,
		OrigPriceYuan: "13.96",
		BeginDay:      beginDay,
		EndDay:        endDay,
	}
}

// TestUpsertPromotions_AsOfSkipsExpiredRows —— 过期促销**不落库**。
//
// # 为什么（生产实测的事实）
//
// 思迅源表 `t_pub_plan_flow` 有 **31371 行**，覆盖 2021-09-16 ~ 2026-10-21，
// 而**对 2026-10-21 有效的只有 741 行**。
//
// ⇒ 导入时把 3 万条历史也写进库，是**纯浪费**：它们永远不会被查价命中，
// 却让"库里躺着 3 万条促销"变成常态（且每次同步都要搬一遍）。
//
// ⚠️ 过滤必须**在服务端做** —— 不能指望每个调用方都记得传 as_of。
func TestUpsertPromotions_AsOfSkipsExpiredRows(t *testing.T) {
	db := newTestDB(t)
	svc := NewPromotionService(db)
	svc.SetClock(fixedClock())
	ctx := context.Background()

	counts, _, err := svc.UpsertPromotions(ctx, testBranch, ModeOverwrite, asOfDay,
		[]PromotionInput{
			promoRow("PS-ACTIVE", "P-1", "2026-10-21", "2026-10-31", "9.96"),
			promoRow("PS-EXPIRED", "P-2", "2024-03-06", "2024-03-28", "1.00"),
			promoRow("PS-FUTURE", "P-3", "2026-11-01", "2026-11-30", "2.00"),
		}, false)
	require.NoError(t, err)

	assert.Equal(t, 1, counts.Inserted, "只有对基准日有效的行该落库")
	assert.Equal(t, 2, counts.Skipped, "过期与未开始的都该跳过")

	var n int64
	require.NoError(t, db.Model(&model.Promotion{}).Count(&n).Error)
	assert.EqualValues(t, 1, n, "库里只该有 1 条促销")

	var p model.Promotion
	require.NoError(t, db.First(&p, "branch_id = ? AND product_id = ?", testBranch, "P-1").Error)
	assert.Equal(t, "PS-ACTIVE", p.ID)
}

// TestUpsertPromotions_OverwriteReplacesScope —— overwrite 把**失效的历史行清掉**。
//
// ⚠️ 这是"同步一次就把历史促销从库里抹掉"的判据。
//
//	不清 ⇒ 库里永远留着上一轮导的历史（库里 3 万条历史就是这么攒出来的）。
func TestUpsertPromotions_OverwriteReplacesScope(t *testing.T) {
	db := newTestDB(t)
	svc := NewPromotionService(db)
	svc.SetClock(fixedClock())
	ctx := context.Background()

	// 先塞一条**早已失效**的历史行（模拟上一轮同步留下的）。
	_, _, err := svc.UpsertPromotions(ctx, testBranch, ModeOverwrite, "2024-03-21",
		[]PromotionInput{promoRow("PS-OLD", "P-OLD", "2024-03-06", "2024-03-28", "1.00")}, false)
	require.NoError(t, err)
	var n int64
	require.NoError(t, db.Model(&model.Promotion{}).Count(&n).Error)
	require.EqualValues(t, 1, n, "前置条件：库里有一条历史行")

	// 本轮同步：replaceScope=true ⇒ 历史行应被清掉。
	counts, _, err := svc.UpsertPromotions(ctx, testBranch, ModeOverwrite, asOfDay,
		[]PromotionInput{promoRow("PS-NEW", "P-NEW", "2026-10-21", "2026-10-31", "9.96")}, true)
	require.NoError(t, err)
	assert.Equal(t, 1, counts.Inserted)
	assert.GreaterOrEqual(t, counts.Deleted, 1, "失效的历史行应被清掉")

	var left []model.Promotion
	require.NoError(t, db.Find(&left).Error)
	for _, p := range left {
		assert.NotEqual(t, "PS-OLD", p.ID, "历史促销不该留在库里")
	}
}

// TestUpsertPromotions_IncrementalDoesNotDelete —— incremental **不删**。
//
// ⚠️ incremental 的语义是"只加不改不删" ⇒ 清历史是 overwrite 才有的动作。
//
//	两者混了 ⇒ 一次增量同步就把别人的促销抹了。
func TestUpsertPromotions_IncrementalDoesNotDelete(t *testing.T) {
	db := newTestDB(t)
	svc := NewPromotionService(db)
	svc.SetClock(fixedClock())
	ctx := context.Background()

	_, _, err := svc.UpsertPromotions(ctx, testBranch, ModeOverwrite, "2024-03-21",
		[]PromotionInput{promoRow("PS-OLD", "P-OLD", "2024-03-06", "2024-03-28", "1.00")}, false)
	require.NoError(t, err)

	counts, _, err := svc.UpsertPromotions(ctx, testBranch, ModeIncremental, asOfDay,
		[]PromotionInput{promoRow("PS-NEW", "P-NEW", "2026-10-21", "2026-10-31", "9.96")}, false)
	require.NoError(t, err)
	assert.Equal(t, 0, counts.Deleted, "incremental 不该删任何行")

	var n int64
	require.NoError(t, db.Model(&model.Promotion{}).Count(&n).Error)
	assert.EqualValues(t, 2, n, "历史行应原样保留")
}

// TestUpsertPromotions_ReplaceScopeFalseKeepsHistory —— 没让清历史 ⇒ 不清。
//
// ⚠️ 这条对应"本次导入的 scope 里**没有** promotions"
//
//	（比如只导 prices）⇒ 清空促销表就是**数据丢失**。
func TestUpsertPromotions_ReplaceScopeFalseKeepsHistory(t *testing.T) {
	db := newTestDB(t)
	svc := NewPromotionService(db)
	svc.SetClock(fixedClock())
	ctx := context.Background()

	_, _, err := svc.UpsertPromotions(ctx, testBranch, ModeOverwrite, "2024-03-21",
		[]PromotionInput{promoRow("PS-OLD", "P-OLD", "2024-03-06", "2024-03-28", "1.00")}, false)
	require.NoError(t, err)

	counts, _, err := svc.UpsertPromotions(ctx, testBranch, ModeOverwrite, asOfDay,
		[]PromotionInput{promoRow("PS-NEW", "P-NEW", "2026-10-21", "2026-10-31", "9.96")}, false)
	require.NoError(t, err)
	assert.Equal(t, 0, counts.Deleted, "没让清历史就不该清")

	var n int64
	require.NoError(t, db.Model(&model.Promotion{}).Count(&n).Error)
	assert.EqualValues(t, 2, n)
}

// TestUpsertPromotions_RejectsBadAsOf —— 非法基准日报错，**不静默回落到今天**。
//
// ⚠️ 静默回落会让"这批数据是哪一天的"变得不可知。
func TestUpsertPromotions_RejectsBadAsOf(t *testing.T) {
	db := newTestDB(t)
	svc := NewPromotionService(db)
	svc.SetClock(fixedClock())

	_, _, err := svc.UpsertPromotions(context.Background(), testBranch, ModeOverwrite, "2026/10/21",
		[]PromotionInput{promoRow("PS-X", "P-1", "2026-10-21", "2026-10-31", "9.96")}, false)
	require.Error(t, err)
}

// TestUpsertPromotions_DoesNotTouchOtherBranch —— 别的门店的促销不动。
func TestUpsertPromotions_DoesNotTouchOtherBranch(t *testing.T) {
	db := newTestDB(t)
	svc := NewPromotionService(db)
	svc.SetClock(fixedClock())
	ctx := context.Background()

	// ⚠️ 用**显式**门店号（testBranch 在本包里是 "00"，用它两段会撞成同一门店）。
	const branchA, branchB = "10", "20"

	_, _, err := svc.UpsertPromotions(ctx, branchA, ModeOverwrite, asOfDay,
		[]PromotionInput{promoRow("PS-A", "P-1", "2026-10-21", "2026-10-31", "9.96")}, false)
	require.NoError(t, err)

	counts, _, err := svc.UpsertPromotions(ctx, branchB, ModeOverwrite, asOfDay,
		[]PromotionInput{promoRow("PS-B", "P-1", "2026-10-21", "2026-10-31", "9.96")}, true)
	require.NoError(t, err)
	assert.Equal(t, 0, counts.Deleted, "清历史只能清本门店的")

	var n int64
	require.NoError(t, db.Model(&model.Promotion{}).
		Where("branch_id = ?", branchA).Count(&n).Error)
	assert.EqualValues(t, 1, n, "门店 10 的促销不该被门店 20 的导入清掉")
}
