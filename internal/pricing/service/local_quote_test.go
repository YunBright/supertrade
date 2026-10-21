package service

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/cubeclient"
)

// countingCube 记录 cube 被调用了几次 ——
// "本地优先"这条规则的核心断言就是**cube 不该被调用**。
type countingCube struct {
	cubeclient.Client
	productCalls int
	promoCalls   int
	product      *cubeclient.ProductDTO
	promo        *cubeclient.PromotionDTO
}

func (c *countingCube) GetProduct(_ context.Context, _ string) (*cubeclient.ProductDTO, error) {
	c.productCalls++
	return c.product, nil
}

func (c *countingCube) GetActivePromotion(_ context.Context, _, _ string) (*cubeclient.PromotionDTO, error) {
	c.promoCalls++
	return c.promo, nil
}

// decimalPtr 把 decimal 变成指针(零值 = nil)。
func decimalPtr(d decimal.Decimal) *decimal.Decimal {
	if d.IsZero() {
		return nil
	}
	return &d
}

// newLocalService 造一个"本地表 + cube 回落"的 PriceService。
func newLocalService(t *testing.T, cube cubeclient.Client) (*PriceService, *PriceListService, *PromotionService) {
	t.Helper()
	db := newTestDB(t)
	prices := NewPriceListService(db)
	prices.SetClock(fixedClock())
	promos := NewPromotionService(db)
	promos.SetClock(fixedClock())
	s := NewPriceService(cube).WithStore(prices, promos)
	s.SetClock(fixedClock())
	return s, prices, promos
}

// TestQuote_LocalPriceWinsOverCube 锁"本地价优先"。
//
// 门店 10 的价在本地表里;cube 那条是"门店 00 的价"。
// 混用 ⇒ 门店 10 的价签印出别的门店的价。
func TestQuote_LocalPriceWinsOverCube(t *testing.T) {
	cube := &countingCube{product: &cubeclient.ProductDTO{
		ID: "P-0001", PriceYuan: decimalPtr(decimal.RequireFromString("77.00")),
	}}
	s, prices, _ := newLocalService(t, cube)
	ctx := context.Background()

	_, _, err := prices.UpsertPrices(ctx, "10", ModeOverwrite,
		[]PriceInput{{ProductID: "P-0001", PriceYuan: "9.90"}})
	if err != nil {
		t.Fatalf("写本地价失败: %v", err)
	}

	q, err := s.Quote(ctx, "10", "P-0001", "2026-09-17")
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.Source != SourceLocal {
		t.Errorf("有本地数据时 source 应为 local,实际 %q", q.Source)
	}
	if q.RetailPrice == nil || *q.RetailPrice != "9.9" {
		t.Errorf("应取本地价 9.9,实际 %v", q.RetailPrice)
	}
	if cube.productCalls != 0 {
		t.Errorf("本地有价时**不该**调 cube,实际调了 %d 次", cube.productCalls)
	}
}

// TestQuote_FallsBackToCubeWhenBranchHasNoLocalData 锁"过渡期回落"。
//
// 门店 00 还没迁移 ⇒ 本地无数据 ⇒ 必须回落 cube,
// 否则门店 00 的价签全印「未维护售价」。
func TestQuote_FallsBackToCubeWhenBranchHasNoLocalData(t *testing.T) {
	cube := &countingCube{product: &cubeclient.ProductDTO{
		ID: "P-0001", PriceYuan: decimalPtr(decimal.RequireFromString("77.00")),
	}}
	s, prices, _ := newLocalService(t, cube)
	ctx := context.Background()

	// 门店 00 没有本地数据,但门店 10 有 ⇒ 回落判定必须**按门店**。
	if _, _, err := prices.UpsertPrices(ctx, "10", ModeOverwrite,
		[]PriceInput{{ProductID: "P-0001", PriceYuan: "9.90"}}); err != nil {
		t.Fatalf("写本地价失败: %v", err)
	}

	q, err := s.Quote(ctx, "00", "P-0001", "2026-09-17")
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.Source != SourceCube {
		t.Errorf("该门店本地无数据时应回落 cube,实际 source=%q", q.Source)
	}
	if q.RetailPrice == nil || *q.RetailPrice != "77" {
		t.Errorf("应取 cube 的价 77,实际 %v", q.RetailPrice)
	}
	if cube.productCalls != 1 {
		t.Errorf("回落时应调 cube 一次,实际 %d 次", cube.productCalls)
	}
}

// TestQuote_LocalBranchWithMissingProductDoesNotFallBack 锁"数据缺口不回落"。
//
// ⚠️ 这是本文件最要紧的一条。门店 10 是自营店,**没有 cube 源** ——
// 若某个商品没导到价却回落 cube,拿到的必然是 503,
// 于是"这个商品没维护价"被翻译成「库存服务暂不可用」,
// 排查方向完全跑偏(真因是导数据时漏了它)。
func TestQuote_LocalBranchWithMissingProductDoesNotFallBack(t *testing.T) {
	cube := &countingCube{product: &cubeclient.ProductDTO{
		ID: "P-OTHER-1", PriceYuan: decimalPtr(decimal.RequireFromString("77.00")),
	}}
	s, prices, _ := newLocalService(t, cube)
	ctx := context.Background()

	// 门店 10 有本地数据(但不是 P-MISSING 的)。
	if _, _, err := prices.UpsertPrices(ctx, "10", ModeOverwrite,
		[]PriceInput{{ProductID: "P-OTHER-1", PriceYuan: "9.90"}}); err != nil {
		t.Fatalf("写本地价失败: %v", err)
	}

	q, err := s.Quote(ctx, "10", "P-MISS-01", "2026-09-17")
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.Source != SourceLocal {
		t.Errorf("该门店有本地数据时不应回落 cube,实际 source=%q", q.Source)
	}
	if q.RetailPrice != nil {
		t.Errorf("没导到价的商品应返回'未维护'(nil),实际 %q", *q.RetailPrice)
	}
	if cube.productCalls != 0 {
		t.Errorf("自营门店不该回落 cube,实际调了 %d 次", cube.productCalls)
	}
}

// TestQuote_LocalPromoWins 锁"本地促销优先"。
func TestQuote_LocalPromoWins(t *testing.T) {
	cube := &countingCube{
		product: &cubeclient.ProductDTO{ID: "P-0001"},
		promo: &cubeclient.PromotionDTO{
			PlanNo: "PS-CUBE", Price: decimal.RequireFromString("5.00"),
			BeginDay: "2026-09-01", EndDay: "2026-09-30",
		},
	}
	s, prices, promos := newLocalService(t, cube)
	ctx := context.Background()

	if _, _, err := prices.UpsertPrices(ctx, "10", ModeOverwrite,
		[]PriceInput{{ProductID: "P-0001", PriceYuan: "9.90"}}); err != nil {
		t.Fatalf("写本地价失败: %v", err)
	}
	if _, _, err := promos.UpsertPromotions(ctx, "10", ModeOverwrite, "2026-09-17", []PromotionInput{{
		ID: "PS-LOCAL", ProductID: "P-0001", PriceYuan: "8.80",
		BeginDay: "2026-09-01", EndDay: "2026-09-30",
	}}, false); err != nil {
		t.Fatalf("写本地促销失败: %v", err)
	}

	q, err := s.Quote(ctx, "10", "P-0001", "2026-09-17")
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.Promo == nil {
		t.Fatal("本地促销应生效")
	}
	if q.Promo.PlanNo != "PS-LOCAL" {
		t.Errorf("应取本地促销,实际 %q", q.Promo.PlanNo)
	}
	if q.Promo.Price != "8.8" {
		t.Errorf("促销价应是字符串 8.8,实际 %q", q.Promo.Price)
	}
	if cube.promoCalls != 0 {
		t.Errorf("本地有促销时不该调 cube,实际调了 %d 次", cube.promoCalls)
	}
}

// TestQuote_NoBranchSkipsLocal 锁"没有门店就只走 cube"。
func TestQuote_NoBranchSkipsLocal(t *testing.T) {
	cube := &countingCube{product: &cubeclient.ProductDTO{
		ID: "P-0001", PriceYuan: decimalPtr(decimal.RequireFromString("77.00")),
	}}
	s, prices, _ := newLocalService(t, cube)
	ctx := context.Background()

	if _, _, err := prices.UpsertPrices(ctx, "10", ModeOverwrite,
		[]PriceInput{{ProductID: "P-0001", PriceYuan: "9.90"}}); err != nil {
		t.Fatalf("写本地价失败: %v", err)
	}

	q, err := s.Quote(ctx, "", "P-0001", "2026-09-17")
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.Source != SourceCube {
		t.Errorf("没有门店时不该查本地表,实际 source=%q", q.Source)
	}
}
