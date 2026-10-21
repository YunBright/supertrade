// 本文件锁"查价走哪条路"的**判据**。
//
// # 判据:「这个门店配了数据源吗」
//
//	配了 ⇒ cube(该门店的实时价与促销)
//	没配 ⇒ 本域 PG
//
// ⚠️ **不是**「本地有没有价格数据」(旧判据,2026-10-21 生产实测已证伪):
// 门店 00 的价格已被主同步实例(00→00)灌进本地 PG(27565 行,快照 2026-10-20 19:24)
// ⇒ 旧判据命中本地 ⇒ 返回**两天前的快照价**,而不是思迅的当前价。
//
// ⇒ 本文件里"本地有行 + 配了源 ⇒ 仍走 cube"那条,是本次修改的核心回归。
package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/pricing/model"
)

// countingCube 记录 cube 被调用了几次 ——
// 「配了源 ⇒ 走 cube」这条规则的核心断言就是**cube 有没有被调用**。
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

// fakeSource 替身 SourceResolver(cubesource.Resolver)。
type fakeSource struct {
	src cubesource.Source
	err error
	// calls 记录解析次数(用来断言"判据只问一次"或"没问")。
	calls int
}

func (f *fakeSource) Resolve(_ context.Context, _ string) (cubesource.Source, error) {
	f.calls++
	if f.err != nil {
		return cubesource.Source{}, f.err
	}
	return f.src, nil
}

// externalSource 是"配了数据源"的门店(富百家 00)。
func externalSource() cubesource.Source {
	return cubesource.Source{
		BranchID:       "00",
		Configured:     true,
		Enabled:        true,
		CubeSourceName: "cube-sixun-ysx-fb",
	}
}

// localSource 是"没配数据源"的门店(自营店 10)。
func localSource() cubesource.Source {
	return cubesource.Source{BranchID: "10"}
}

// decimalPtr 把 decimal 变成指针(零值 = nil)。
func decimalPtr(d decimal.Decimal) *decimal.Decimal {
	if d.IsZero() {
		return nil
	}
	return &d
}

// newLocalService 造一个"本地表 + cube"的 PriceService。
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

// TestQuote_ConfiguredBranchUsesCubeEvenWhenLocalHasRows 锁"判据是『配没配数据源』"。
//
// ⚠️ 这是 2026-10-21 生产实测的那个症状本体:
//
//	门店 00 的价格已被主同步实例(00→00)灌进本地 PG(27565 行,快照 2026-10-20 19:24)
//	⇒ 旧判据("本地有没有数据")命中本地 ⇒ 返回**两天前的快照价**。
//
// ⇒ 配了源的门店**即使本地有行**也必须走 cube。
func TestQuote_ConfiguredBranchUsesCubeEvenWhenLocalHasRows(t *testing.T) {
	cube := &countingCube{product: &cubeclient.ProductDTO{
		ID: "P-0001", PriceYuan: decimalPtr(decimal.RequireFromString("77.00")),
	}}
	s, prices, _ := newLocalService(t, cube)
	s.SetCubeSource(&fakeSource{src: externalSource()}, nil)
	ctx := context.Background()

	// 本地**有**这个门店的行(旧快照价 9.90)。
	if _, _, err := prices.UpsertPrices(ctx, "00", ModeOverwrite,
		[]PriceInput{{ProductID: "P-0001", PriceYuan: "9.90"}}); err != nil {
		t.Fatalf("写本地价失败: %v", err)
	}

	q, err := s.Quote(ctx, "00", "P-0001", "2026-09-17")
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.Source != SourceCube {
		t.Errorf("配了数据源的门店必须走 cube,实际 source=%q", q.Source)
	}
	if q.RetailPrice == nil || *q.RetailPrice != "77" {
		t.Errorf("必须取 cube 的实时价 77,实际 %v", q.RetailPrice)
	}
	if cube.productCalls != 1 {
		t.Errorf("配了源的门店应查 cube 一次,实际 %d 次", cube.productCalls)
	}
}

// TestQuote_UnconfiguredBranchReadsLocal 锁"没配数据源 ⇒ 本域 PG"。
func TestQuote_UnconfiguredBranchReadsLocal(t *testing.T) {
	cube := &countingCube{product: &cubeclient.ProductDTO{
		ID: "P-0001", PriceYuan: decimalPtr(decimal.RequireFromString("77.00")),
	}}
	s, prices, _ := newLocalService(t, cube)
	s.SetCubeSource(&fakeSource{src: localSource()}, nil)
	ctx := context.Background()

	if _, _, err := prices.UpsertPrices(ctx, "10", ModeOverwrite,
		[]PriceInput{{ProductID: "P-0001", PriceYuan: "9.90"}}); err != nil {
		t.Fatalf("写本地价失败: %v", err)
	}

	q, err := s.Quote(ctx, "10", "P-0001", "2026-09-17")
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.Source != SourceLocal {
		t.Errorf("没配数据源的门店应走本地,实际 source=%q", q.Source)
	}
	if q.RetailPrice == nil || *q.RetailPrice != "9.9" {
		t.Errorf("应取本地价 9.9,实际 %v", q.RetailPrice)
	}
	if cube.productCalls != 0 {
		t.Errorf("没配数据源的门店**不该**调 cube,实际调了 %d 次", cube.productCalls)
	}
}

// TestQuote_ResolveFailureIsUnavailable 锁"问不到路由 ⇒ 503,不回落本地"。
//
// ⚠️ 回落本地 = 把一次链路故障显示成「这个商品没维护售价」——
// 而真相是"路由问不到"。两个真相 = 两个错法。
//
// ⚠️ 本地**有**旧快照价时也不能回落(那正是生产上出问题的场景)。
func TestQuote_ResolveFailureIsUnavailable(t *testing.T) {
	cube := &countingCube{product: &cubeclient.ProductDTO{
		ID: "P-0001", PriceYuan: decimalPtr(decimal.RequireFromString("77.00")),
	}}
	s, prices, _ := newLocalService(t, cube)
	s.SetCubeSource(&fakeSource{err: errors.New("dapr 不可达")}, nil)
	ctx := context.Background()

	if _, _, err := prices.UpsertPrices(ctx, "00", ModeOverwrite,
		[]PriceInput{{ProductID: "P-0001", PriceYuan: "9.90"}}); err != nil {
		t.Fatalf("写本地价失败: %v", err)
	}

	q, err := s.Quote(ctx, "00", "P-0001", "2026-09-17")
	if !errors.Is(err, model.ErrCubeSourceUnavailable) {
		t.Fatalf("问不到路由应返 ErrCubeSourceUnavailable,实际 %v", err)
	}
	if q != nil {
		t.Errorf("失败时不该返回价格结论: %+v", q)
	}
	if cube.productCalls != 0 {
		t.Errorf("问不到路由时不该去查 cube(那是把故障说成业务结论),实际调了 %d 次", cube.productCalls)
	}
}

// TestQuote_DisabledSourceIsUnavailable 锁"配了但被停用 ⇒ 503"。
//
// ⚠️ 与"没配数据源"是两件事:回落成"没配"会让用户看到一份空结果,
// 而真相是"这个门店现在查不了"。
func TestQuote_DisabledSourceIsUnavailable(t *testing.T) {
	cube := &countingCube{product: &cubeclient.ProductDTO{
		ID: "P-0001", PriceYuan: decimalPtr(decimal.RequireFromString("77.00")),
	}}
	s, prices, _ := newLocalService(t, cube)
	disabled := externalSource()
	disabled.Enabled = false
	s.SetCubeSource(&fakeSource{src: disabled}, nil)
	ctx := context.Background()

	if _, _, err := prices.UpsertPrices(ctx, "00", ModeOverwrite,
		[]PriceInput{{ProductID: "P-0001", PriceYuan: "9.90"}}); err != nil {
		t.Fatalf("写本地价失败: %v", err)
	}

	q, err := s.Quote(ctx, "00", "P-0001", "2026-09-17")
	if !errors.Is(err, model.ErrCubeSourceDisabled) {
		t.Fatalf("数据源被停用应返 ErrCubeSourceDisabled,实际 %v", err)
	}
	if q != nil {
		t.Errorf("失败时不该返回价格结论: %+v", q)
	}
	if cube.productCalls != 0 {
		t.Errorf("数据源被停用时不该去查 cube,实际调了 %d 次", cube.productCalls)
	}
}

// TestQuote_LocalBranchMissingProductDoesNotFallBack 锁"数据缺口不回落"。
//
// ⚠️ 自营店(门店 10)没有 cube 源 —— 若某个商品没导到价却回落 cube,
// 拿到的必然是 503,于是"这个商品没维护价"被翻译成「库存服务暂不可用」,
// 排查方向完全跑偏(真因是导数据时漏了它)。
func TestQuote_LocalBranchMissingProductDoesNotFallBack(t *testing.T) {
	cube := &countingCube{product: &cubeclient.ProductDTO{
		ID: "P-OTHER-1", PriceYuan: decimalPtr(decimal.RequireFromString("77.00")),
	}}
	s, prices, _ := newLocalService(t, cube)
	s.SetCubeSource(&fakeSource{src: localSource()}, nil)
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
		t.Errorf("自营门店不该走 cube,实际 source=%q", q.Source)
	}
	if q.RetailPrice != nil {
		t.Errorf("没导到价的商品应返回'未维护'(nil),实际 %q", *q.RetailPrice)
	}
	if cube.productCalls != 0 {
		t.Errorf("自营门店不该回落 cube,实际调了 %d 次", cube.productCalls)
	}
}

// TestQuote_LocalPromoWins 锁"本地促销取本地的"。
func TestQuote_LocalPromoWins(t *testing.T) {
	cube := &countingCube{
		product: &cubeclient.ProductDTO{ID: "P-0001"},
		promo: &cubeclient.PromotionDTO{
			PlanNo: "PS-CUBE", Price: decimal.RequireFromString("5.00"),
			BeginDay: "2026-09-01", EndDay: "2026-09-30",
		},
	}
	s, prices, promos := newLocalService(t, cube)
	s.SetCubeSource(&fakeSource{src: localSource()}, nil)
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

// ---- 写侧闸门(导入端点用) ----

// TestEnsureLocalSource_ExternalBranchIsReadOnly 锁"有源门店只读"。
func TestEnsureLocalSource_ExternalBranchIsReadOnly(t *testing.T) {
	s, _, _ := newLocalService(t, &countingCube{})
	s.SetCubeSource(&fakeSource{src: externalSource()}, nil)

	err := s.EnsureLocalSource(context.Background(), "00")
	if !errors.Is(err, model.ErrExternalSourceReadOnly) {
		t.Fatalf("配了数据源的门店应拒写,实际 %v", err)
	}
	// ⚠️ message 不能泄漏内部 app-id(那是运维细节,不是终端用户需要的信息)。
	if msg := err.Error(); strings.Contains(msg, "cube-sixun-ysx-fb") {
		t.Errorf("409 文案泄漏了内部 app-id: %s", msg)
	}
}

// TestEnsureLocalSource_LocalBranchIsWritable 锁"没配数据源的门店照常写"。
func TestEnsureLocalSource_LocalBranchIsWritable(t *testing.T) {
	s, _, _ := newLocalService(t, &countingCube{})
	s.SetCubeSource(&fakeSource{src: localSource()}, nil)

	if err := s.EnsureLocalSource(context.Background(), "10"); err != nil {
		t.Fatalf("自营门店不该被拒写,实际 %v", err)
	}
}

// TestEnsureLocalSource_ResolveFailureIsAllowed 锁"问不到路由 ⇒ 放行"。
//
// ⚠️ 写侧放行是**有意的**:导入路径自己会兜底(本域 PG 里没有这个门店的行
// ⇒ 导入 0 条),那是诚实的失败;而读侧放行会把故障说成业务结论。
func TestEnsureLocalSource_ResolveFailureIsAllowed(t *testing.T) {
	s, _, _ := newLocalService(t, &countingCube{})
	s.SetCubeSource(&fakeSource{err: errors.New("dapr 不可达")}, nil)

	if err := s.EnsureLocalSource(context.Background(), "00"); err != nil {
		t.Fatalf("问不到路由时写侧应放行,实际 %v", err)
	}
}
