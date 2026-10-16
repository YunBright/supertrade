package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubeclient/cubeclientfake"
)

// fakePrice 固定基准日的 PriceService。
func fakePrice(t *testing.T) (*PriceService, *cubeclientfake.Client) {
	t.Helper()
	f := cubeclientfake.New()
	s := NewPriceService(f)
	s.SetClock(func() time.Time { return time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC) })
	return s, f
}

// 有生效促销 → promo 非空,且取**最低价**那条。
func TestQuote_PicksLowestActivePromo(t *testing.T) {
	s, _ := fakePrice(t)
	q, err := s.Quote(context.Background(), "P-1001", "2026-09-17")
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.Promo == nil {
		t.Fatal("P-1001 在 2026-09-17 有两条生效促销,promo 不该为 nil")
	}
	if got := q.Promo.Price; got != 9.90 {
		t.Errorf("应取最低促销价 9.90,实际 %v", got)
	}
	if q.Promo.PlanNo != "PS-002" {
		t.Errorf("最低价那条是 PS-002,实际 %q", q.Promo.PlanNo)
	}
}

// 促销已过期 → 返 promo=nil 且**是正常结论**(不是 error)。
//
// 这是最容易写错的一条:返 404 / 返 error 会让前端把"绝大多数商品本来就没促销"
// 显示成"查询失败"。
func TestQuote_ExpiredPromoIsNotActive(t *testing.T) {
	s, _ := fakePrice(t)
	// P-1002 的促销 2026-08-01~2026-08-31,9 月已过期。
	q, err := s.Quote(context.Background(), "P-1002", "2026-09-17")
	if err != nil {
		t.Fatalf("过期促销不应是错误,实际 %v", err)
	}
	if q.Promo != nil {
		t.Fatalf("过期促销不该生效,实际 %+v", q.Promo)
	}
	// 零售价仍要有 —— 没促销不等于没售价。
	if q.RetailPrice == nil {
		t.Error("无促销时零售价仍应返出")
	}
}

// 零售价 nil(未维护)与 0 元必须能区分。
func TestQuote_DistinguishesUnsetFromZeroPrice(t *testing.T) {
	s, f := fakePrice(t)
	zero := decimal.NewFromInt(0)
	f.UpsertProductPrice("P-3001", &zero)

	q, err := s.Quote(context.Background(), "P-3001", "2026-09-17")
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.RetailPrice == nil {
		t.Fatal("售价为 0 时不能是 nil(那是'未维护')")
	}
	if *q.RetailPrice != 0 {
		t.Errorf("应为 0,实际 %v", *q.RetailPrice)
	}

	nilPrice := (*decimal.Decimal)(nil)
	f.UpsertProductPrice("P-3001", nilPrice)
	q2, _ := s.Quote(context.Background(), "P-3001", "2026-09-17")
	if q2.RetailPrice != nil {
		t.Errorf("未维护售价必须是 nil,实际 %v", *q2.RetailPrice)
	}
}

// 商品档案缺失 **不** 让整次查询失败 —— 促销可能仍然查得到。
func TestQuote_ProductMissingStillQueriesPromo(t *testing.T) {
	s, f := fakePrice(t)
	orig := decimal.NewFromFloat(10.0)
	f.UpsertPromotions("P-NOT-IN-CATALOG", cubeclient.PromotionDTO{
		PlanNo: "PS-900", Price: decimal.NewFromFloat(5.0),
		OrigPrice: &orig, BeginDay: "2026-09-01", EndDay: "2026-09-30",
	})
	q, err := s.Quote(context.Background(), "P-NOT-IN-CATALOG", "2026-09-17")
	if err != nil {
		t.Fatalf("档案缺失不应报错,实际 %v", err)
	}
	if q.Promo == nil {
		t.Fatal("档案缺失时促销仍应查得到")
	}
	if q.RetailPrice != nil {
		t.Errorf("档案缺失时 retail_price 应缺省,实际 %v", *q.RetailPrice)
	}
}

// 促销价 <= 0 的单不算"生效折扣"。
//
// 没有这条,一条 0 元促销会把有效促销盖掉,价签印出 ¥0.00。
func TestQuote_SkipsNonPositivePromoPrice(t *testing.T) {
	s, f := fakePrice(t)
	orig := decimal.NewFromFloat(3.5)
	f.UpsertPromotions("P-2001",
		cubeclient.PromotionDTO{
			PlanNo: "PS-BAD", Price: decimal.Zero, OrigPrice: &orig,
			BeginDay: "2026-09-01", EndDay: "2026-09-30",
		},
		cubeclient.PromotionDTO{
			PlanNo: "PS-OK", Price: decimal.NewFromFloat(1.5), OrigPrice: &orig,
			BeginDay: "2026-09-01", EndDay: "2026-09-30",
		},
	)
	q, err := s.Quote(context.Background(), "P-2001", "2026-09-17")
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.Promo == nil || q.Promo.PlanNo != "PS-OK" {
		t.Fatalf("0 元促销应被跳过,实际 %+v", q.Promo)
	}
}

func TestQuote_RejectsShortBarcode(t *testing.T) {
	s, _ := fakePrice(t)
	if _, err := s.Quote(context.Background(), "1234", ""); !errors.Is(err, ErrInvalidBarcode) {
		t.Fatalf("4 位条码应 ErrInvalidBarcode,实际 %v", err)
	}
}

func TestQuote_TrimSpaceAndBadDateFallback(t *testing.T) {
	s, _ := fakePrice(t)
	// 前后空格不应导致"条码长度不足"。
	q, err := s.Quote(context.Background(), "  P-1001  ", "not-a-date")
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.Barcode != "P-1001" {
		t.Errorf("应去掉首尾空格,实际 %q", q.Barcode)
	}
	// 非法 on_date 回落到今天(= 注入的 2026-09-17)。
	if q.AsOf != "2026-09-17" {
		t.Errorf("非法 on_date 应回落到今天 2026-09-17,实际 %q", q.AsOf)
	}
	if q.Promo == nil {
		t.Error("回落到今天后 P-1001 的促销应生效")
	}
}

func TestQuote_NoCubeClient(t *testing.T) {
	s := NewPriceService(nil)
	if _, err := s.Quote(context.Background(), "P-1001", ""); !errors.Is(err, ErrNoCube) {
		t.Fatalf("cube=nil 应 ErrNoCube,实际 %v", err)
	}
}
