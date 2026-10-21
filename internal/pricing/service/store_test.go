package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	_ "modernc.org/sqlite" // 注册驱动名 "sqlite"(纯 Go 驱动,无需 CGO)

	"github.com/YunBright/supertrade/internal/pricing/model"
)

// newTestDB 构造 SQLite 内存 DB + AutoMigrate。
//
// ⚠️ SQLite 与生产 PG 的差异(decimal 存 TEXT、时间存 datetime)不影响本文件
// 断言的性质:本文件守的是**幂等 / 模式语义 / 生效窗口**,
// 那些是纯逻辑,不是方言问题。
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dialector := sqlite.Dialector{DriverName: "sqlite", DSN: ":memory:?_pragma=foreign_keys(1)"}
	db, err := gorm.Open(dialector, &gorm.Config{})
	require.NoError(t, err, "open sqlite")
	require.NoError(t, db.AutoMigrate(&model.PriceList{}, &model.Promotion{}), "auto migrate")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return db
}

func fixedClock() func() time.Time {
	return func() time.Time { return time.Date(2026, 9, 17, 10, 0, 0, 0, time.Local) }
}

// ---- 售价 ----

// TestUpsertPrices_Idempotent 锁"重复导入不产生新行"。
//
// 症状:每晚跑一次同步,price_lists 的行数**逐晚增长**,
// 而每个商品其实只有一行 —— 查价时取到的是"最早那一行"还是"最新那一行"
// 取决于数据库的返回顺序,不可预测。
func TestUpsertPrices_Idempotent(t *testing.T) {
	db := newTestDB(t)
	svc := NewPriceListService(db)
	svc.SetClock(fixedClock())
	ctx := context.Background()

	rows := []PriceInput{{ProductID: "P-0001", PriceYuan: "9.90"}}
	c1, _, err := svc.UpsertPrices(ctx, testBranch, ModeOverwrite, rows)
	require.NoError(t, err)
	assert.Equal(t, 1, c1.Inserted, "第一次应插入")

	c2, _, err := svc.UpsertPrices(ctx, testBranch, ModeOverwrite, rows)
	require.NoError(t, err)
	assert.Equal(t, 0, c2.Inserted, "第二次不应再插入")
	assert.Equal(t, 1, c2.Updated, "第二次应是更新")

	var n int64
	require.NoError(t, db.Model(&model.PriceList{}).Count(&n).Error)
	assert.Equal(t, int64(1), n, "同一商品在同一价目表里只应有一行")
}

// TestUpsertPrices_IncrementalSkipsExisting 锁"增量模式跳过已存在"。
func TestUpsertPrices_IncrementalSkipsExisting(t *testing.T) {
	db := newTestDB(t)
	svc := NewPriceListService(db)
	svc.SetClock(fixedClock())
	ctx := context.Background()

	_, _, err := svc.UpsertPrices(ctx, testBranch, ModeOverwrite,
		[]PriceInput{{ProductID: "P-0001", PriceYuan: "9.90"}})
	require.NoError(t, err)

	c, _, err := svc.UpsertPrices(ctx, testBranch, ModeIncremental,
		[]PriceInput{{ProductID: "P-0001", PriceYuan: "1.00"}})
	require.NoError(t, err)
	assert.Equal(t, 1, c.Skipped, "增量模式应跳过")
	assert.Equal(t, 0, c.Updated, "增量模式不应更新")

	got, err := svc.ActivePrice(ctx, testBranch, "", "P-0001", time.Now())
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "9.9", got.PriceYuan.String(), "增量模式下原值不该被改")
}

// TestUpsertPrices_SkipsUnpricedRow 锁"没维护价不建行"。
//
// ⚠️ 这是 POS 的核心语义:"没维护售价"要印「未维护售价」,
// 而"售价 0 元"要印 ¥0.00。两者混为一谈 ⇒ 顾客看到错误的价。
func TestUpsertPrices_SkipsUnpricedRow(t *testing.T) {
	db := newTestDB(t)
	svc := NewPriceListService(db)
	svc.SetClock(fixedClock())
	ctx := context.Background()

	c, errs, err := svc.UpsertPrices(ctx, testBranch, ModeOverwrite,
		[]PriceInput{{ProductID: "P-NO-PRICE", PriceYuan: ""}})
	require.NoError(t, err)
	assert.Empty(t, errs, "没维护价不是错误")
	assert.Equal(t, 1, c.Skipped)

	got, err := svc.ActivePrice(ctx, testBranch, "", "P-NO-PRICE", time.Now())
	require.NoError(t, err)
	assert.Nil(t, got, "没维护价 ⇒ 查不到行(而不是有一行、价是 0)")
}

// TestUpsertPrices_InvalidMoneyIsReported 锁"非法金额必须报出来"。
//
// ⚠️ 非法金额当 0 处理 ⇒ 一条脏数据变成"售价 0 元",POS 会照印出去。
// 而"这行没导进去"只出现在报告里 —— 后者才是可接受的失败方式。
func TestUpsertPrices_InvalidMoneyIsReported(t *testing.T) {
	db := newTestDB(t)
	svc := NewPriceListService(db)
	svc.SetClock(fixedClock())
	ctx := context.Background()

	c, errs, err := svc.UpsertPrices(ctx, testBranch, ModeOverwrite,
		[]PriceInput{{ProductID: "P-BAD-01", PriceYuan: "abc"}})
	require.NoError(t, err)
	require.Len(t, errs, 1, "非法金额必须逐行报出")
	assert.Contains(t, errs[0].Error, "售价解析失败")
	assert.Equal(t, 1, c.Skipped)

	got, err := svc.ActivePrice(ctx, testBranch, "", "P-BAD-01", time.Now())
	require.NoError(t, err)
	assert.Nil(t, got, "非法金额的行不应落库")
}

// TestUpsertPrices_RevivesSoftDeleted 锁"软删的行能被重新导入救回"。
//
// ⚠️ 少了这条:软删过的价格行 upsert 后**仍然是已删除**,
// 查价永远查不到,而导入报告说"成功 N 条"。
func TestUpsertPrices_RevivesSoftDeleted(t *testing.T) {
	db := newTestDB(t)
	svc := NewPriceListService(db)
	svc.SetClock(fixedClock())
	ctx := context.Background()

	_, _, err := svc.UpsertPrices(ctx, testBranch, ModeOverwrite,
		[]PriceInput{{ProductID: "P-0001", PriceYuan: "9.90"}})
	require.NoError(t, err)

	deleted, err := svc.DeletePrice(ctx, testBranch, "", "P-0001")
	require.NoError(t, err)
	require.True(t, deleted)

	got, err := svc.ActivePrice(ctx, testBranch, "", "P-0001", time.Now())
	require.NoError(t, err)
	assert.Nil(t, got, "软删后查不到")

	_, _, err = svc.UpsertPrices(ctx, testBranch, ModeOverwrite,
		[]PriceInput{{ProductID: "P-0001", PriceYuan: "9.90"}})
	require.NoError(t, err)

	got, err = svc.ActivePrice(ctx, testBranch, "", "P-0001", time.Now())
	require.NoError(t, err)
	require.NotNil(t, got, "软删过的行必须能被重新导入救回")
}

// TestActivePrice_RespectsEffectiveWindow 锁"生效窗口"。
func TestActivePrice_RespectsEffectiveWindow(t *testing.T) {
	db := newTestDB(t)
	svc := NewPriceListService(db)
	svc.SetClock(fixedClock())
	ctx := context.Background()

	future := time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local)
	_, _, err := svc.UpsertPrices(ctx, testBranch, ModeOverwrite, []PriceInput{{
		ProductID: "P-FUT-01", PriceYuan: "9.90", EffectiveAt: &future,
	}})
	require.NoError(t, err)

	got, err := svc.ActivePrice(ctx, testBranch, "", "P-FUT-01", time.Date(2026, 9, 17, 0, 0, 0, 0, time.Local))
	require.NoError(t, err)
	assert.Nil(t, got, "未来才生效的价,现在查不到")

	got, err = svc.ActivePrice(ctx, testBranch, "", "P-FUT-01", time.Date(2026, 10, 2, 0, 0, 0, 0, time.Local))
	require.NoError(t, err)
	assert.NotNil(t, got, "生效之后应查得到")
}

// ---- 促销 ----

// TestUpsertPromotions_Idempotent 锁"重复导入不产生新行"。
func TestUpsertPromotions_Idempotent(t *testing.T) {
	db := newTestDB(t)
	svc := NewPromotionService(db)
	svc.SetClock(fixedClock())
	ctx := context.Background()

	rows := []PromotionInput{{
		ID: "PS-0001", ProductID: "P-0001", PlanName: "特价",
		PriceYuan: "8.80", OrigPriceYuan: "9.90",
		BeginDay: "2026-09-01", EndDay: "2026-09-30",
	}}
	c1, _, err := svc.UpsertPromotions(ctx, testBranch, ModeOverwrite, "2026-09-17", rows, false)
	require.NoError(t, err)
	assert.Equal(t, 1, c1.Inserted)

	c2, _, err := svc.UpsertPromotions(ctx, testBranch, ModeOverwrite, "2026-09-17", rows, false)
	require.NoError(t, err)
	assert.Equal(t, 1, c2.Updated)
	assert.Equal(t, 0, c2.Inserted)

	var n int64
	require.NoError(t, db.Model(&model.Promotion{}).Count(&n).Error)
	assert.Equal(t, int64(1), n)
}

// TestActivePromotions_PicksLowestPrice 锁"多个促销单取最低价"。
//
// 与 cube promotion model 的 min_promo_price 语义一致:
// 「用户实际能买到的最低价」。
func TestActivePromotions_PicksLowestPrice(t *testing.T) {
	db := newTestDB(t)
	svc := NewPromotionService(db)
	svc.SetClock(fixedClock())
	ctx := context.Background()

	_, _, err := svc.UpsertPromotions(ctx, testBranch, ModeOverwrite, "2026-09-17", []PromotionInput{
		{ID: "PS-A", ProductID: "P-0001", PriceYuan: "9.90", BeginDay: "2026-09-01", EndDay: "2026-09-30"},
		{ID: "PS-B", ProductID: "P-0001", PriceYuan: "8.80", BeginDay: "2026-09-01", EndDay: "2026-09-30"},
	}, false)
	require.NoError(t, err)

	got, err := svc.ActivePromotion(ctx, testBranch, "P-0001",
		time.Date(2026, 9, 17, 0, 0, 0, 0, time.Local))
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "PS-B", got.ID, "应取促销价最低的那条")
	assert.Equal(t, "8.8", got.PriceYuan.String())
}

// TestActivePromotions_RespectsDateWindow 锁"促销的生效区间"。
func TestActivePromotions_RespectsDateWindow(t *testing.T) {
	db := newTestDB(t)
	svc := NewPromotionService(db)
	svc.SetClock(fixedClock())
	ctx := context.Background()

	_, _, err := svc.UpsertPromotions(ctx, testBranch, ModeOverwrite, "2026-09-17", []PromotionInput{{
		ID: "PS-1", ProductID: "P-0001", PriceYuan: "8.80",
		BeginDay: "2026-09-01", EndDay: "2026-09-30",
	}}, false)
	require.NoError(t, err)

	// 活动当天仍生效(端点含)。
	got, err := svc.ActivePromotion(ctx, testBranch, "P-0001",
		time.Date(2026, 9, 30, 0, 0, 0, 0, time.Local))
	require.NoError(t, err)
	assert.NotNil(t, got, "活动截止日当天仍应生效")

	// 第二天失效。
	got, err = svc.ActivePromotion(ctx, testBranch, "P-0001",
		time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local))
	require.NoError(t, err)
	assert.Nil(t, got, "过了截止日应失效")

	// 活动开始前一天不生效。
	got, err = svc.ActivePromotion(ctx, testBranch, "P-0001",
		time.Date(2026, 8, 31, 0, 0, 0, 0, time.Local))
	require.NoError(t, err)
	assert.Nil(t, got, "活动开始前不应生效")
}

// TestUpsertPromotions_RejectsBadDate 锁"日期解析不了必须拒绝"。
//
// ⚠️ "尽力解析"会把促销放到**错误的日期**上生效,
// 于是价签印出低于真价的促销价 —— 这是真金白银的错误。
func TestUpsertPromotions_RejectsBadDate(t *testing.T) {
	db := newTestDB(t)
	svc := NewPromotionService(db)
	svc.SetClock(fixedClock())
	ctx := context.Background()

	c, errs, err := svc.UpsertPromotions(ctx, testBranch, ModeOverwrite, "2026-09-17", []PromotionInput{{
		ID: "PS-1", ProductID: "P-0001", PriceYuan: "8.80",
		BeginDay: "Jul  1 2026 12:00AM", EndDay: "2026-09-30",
	}}, false)
	require.NoError(t, err)
	require.Len(t, errs, 1, "非 ISO 日期必须逐行报出")
	assert.Contains(t, errs[0].Error, "活动起始日解析失败")
	assert.Equal(t, 1, c.Skipped)

	got, err := svc.ActivePromotion(ctx, testBranch, "P-0001",
		time.Date(2026, 9, 17, 0, 0, 0, 0, time.Local))
	require.NoError(t, err)
	assert.Nil(t, got, "日期不法的促销不应落库")
}

// TestParseISODay_RejectsNonISO 锁日期解析的严格性。
func TestParseISODay_RejectsNonISO(t *testing.T) {
	for _, bad := range []string{"", "2026-9-1", "20260901", "2026/09/01", "2026-13-45x"} {
		if _, err := parseISODay(bad); err == nil {
			t.Errorf("%q 必须被拒", bad)
		}
	}
	got, err := parseISODay("2026-09-01")
	if err != nil {
		t.Fatalf("合法日期不该报错: %v", err)
	}
	if got.Format("2006-01-02") != "2026-09-01" {
		t.Errorf("解析结果不对: %v", got)
	}
}
