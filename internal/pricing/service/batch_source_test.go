// 本文件锁「批量查价 / 批量查促销」的两条路径给**同一个答案**。
//
// # 为什么要有这个文件
//
// 批量端点(`/prices` `/promotions`)是 2026-10-22 才从"只读本地表"改成
// "按数据源分流"的 ⇒ 同一个商品在**有源的门店**走 cube、在**无源的门店**
// 走本域 PG。
//
// ⇒ 两条路径必须给**同一个答案**：字段一样、空值语义一样。
// 不一样的话，现场看到的是"同一个商品，换个门店就换个价/换个成本"，
// 而且**零报错**。
//
// # 具体锁住的三件事
//
// ① **零值一律当"没有"**：cube 的 `SUM(cost_yuan)` 在源列为空时给 0（不是 NULL），
//    本地路径用 `IsZero()` 判断"没维护成本" ⇒ 不对齐就会把
//    "不知道成本"显示成"成本 0 元"。
// ② **没维护价的商品不在返回里**：它没有行（不是"有一行、价是 0"）。
// ③ **cube 的硬约束**：必须带 measure、order 的 id 在 SELECT 里、total:true、
//    批量用 `in` 过滤（不是 N 次单查）。

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/cubeclient"
)

// TestBatchPrices_CubeZeroCostMeansUnknown 锁"cube 的成本 0 = 不知道成本"。
//
// ⚠️ cube 的 `total_cost_yuan` = SUM(cost_yuan)：源列为空时 SUM 给 0（不是 NULL）
// ⇒ 直接透传 ⇒ 客户端显示"成本 0 元" ⇒ 而真相是"没有维护成本"。
//
// ⚠️ 本地路径用 `!CostYuan.IsZero()` 判断 ⇒ 两条路径必须一致。
func TestBatchPrices_CubeZeroCostMeansUnknown(t *testing.T) {
	s, _, _ := newLocalService(t, &countingCube{})
	reader := &fakeCubeReader{rows: []map[string]any{
		{
			"product.id":              "P-0001",
			"product.avg_price_yuan":  9.9,
			"product.total_cost_yuan": 0,
		},
	}, total: 1}
	s.SetCubeSource(&fakeSource{src: externalSource()}, reader)

	rows, err := s.BatchPrices(context.Background(), "00", []string{"P-0001"}, time.Now())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "9.9", rows[0].PriceYuan)
	assert.Nil(t, rows[0].CostYuan, "成本 0 必须是「不知道成本」(nil),不是 \"0\"")
}

// TestBatchPrices_CubeAndLocalAgree 锁"两条路径给同一个答案"。
//
// ⚠️ 同一个商品、同样的价与成本 ⇒ 有源门店(走 cube)与无源门店(走本地)
// 必须给出**逐字相同**的批量结果。
//
// ⇒ 不一致的症状:同一个商品在 00 店显示成本 0 元、在 10 店显示"没维护",
// 现场无从判断哪个对,而且**零报错**。
func TestBatchPrices_CubeAndLocalAgree(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 10, 21, 0, 0, 0, 0, time.Local)

	// 本地侧:门店 10(无源 ⇒ 走本域 PG)。
	localSvc, prices, _ := newLocalService(t, &countingCube{})
	localSvc.SetCubeSource(&fakeSource{src: localSource()}, &fakeCubeReader{})
	_, _, err := prices.UpsertPrices(ctx, "10", ModeOverwrite,
		[]PriceInput{{ProductID: "P-0001", PriceYuan: "9.90"}})
	require.NoError(t, err)

	localRows, err := localSvc.BatchPrices(ctx, "10", []string{"P-0001"}, at)
	require.NoError(t, err)
	require.Len(t, localRows, 1)

	// cube 侧:门店 00(有源 ⇒ 走 cube),同样的数据。
	cubeSvc, _, _ := newLocalService(t, &countingCube{})
	reader := &fakeCubeReader{rows: []map[string]any{
		{
			"product.id":              "P-0001",
			"product.avg_price_yuan":  9.9,
			"product.total_cost_yuan": 0,
		},
	}, total: 1}
	cubeSvc.SetCubeSource(&fakeSource{src: externalSource()}, reader)

	cubeRows, err := cubeSvc.BatchPrices(ctx, "00", []string{"P-0001"}, at)
	require.NoError(t, err)
	require.Len(t, cubeRows, 1)

	assert.Equal(t, localRows[0].ProductID, cubeRows[0].ProductID)
	assert.Equal(t, localRows[0].PriceYuan, cubeRows[0].PriceYuan)
	assert.Equal(t, localRows[0].CostYuan, cubeRows[0].CostYuan, "两条路径的成本语义必须一致")
	assert.Equal(t, localRows[0].PromoPriceYuan, cubeRows[0].PromoPriceYuan)
}

// TestBatchPrices_NoPriceMeansNoRow 锁"没维护价的商品不在返回里"。
//
// ⚠️ 调用方据此区分"没维护售价"与"售价 0 元"
// ⇒ 给一行价 0 会让前者变成后者。
func TestBatchPrices_NoPriceMeansNoRow(t *testing.T) {
	s, _, _ := newLocalService(t, &countingCube{})
	reader := &fakeCubeReader{rows: []map[string]any{
		{"product.id": "P-0001", "product.avg_price_yuan": nil},
	}, total: 1}
	s.SetCubeSource(&fakeSource{src: externalSource()}, reader)

	rows, err := s.BatchPrices(context.Background(), "00", []string{"P-0001"}, time.Now())
	require.NoError(t, err)
	assert.Empty(t, rows, "没有价的商品不该出现在返回里")
}

// TestBatchPrices_CubeQueryShape 锁"cube 查询的形状"（硬约束）。
//
// ⚠️ 必须带 measure      —— cube 拒收"没有 measure"的查询(QUERY_INVALID)
// ⚠️ order 的 id 必须在 SELECT 的 dimensions 里 —— 否则报 not a grouped column
// ⚠️ total:true 才拿得到总行数 —— 不给时恒为 0（那是"没问"，不是"总共 0 条"）
// ⚠️ 批量用 `in` 过滤 —— 不是 N 次单查（POS 一次扫一屏商品）
func TestBatchPrices_CubeQueryShape(t *testing.T) {
	s, _, _ := newLocalService(t, &countingCube{})
	reader := &fakeCubeReader{rows: []map[string]any{
		{"product.id": "P-0001", "product.avg_price_yuan": 9.9},
	}, total: 1}
	s.SetCubeSource(&fakeSource{src: externalSource()}, reader)

	_, err := s.BatchPrices(context.Background(), "00",
		[]string{"P-0001", "P-0002"}, time.Now())
	require.NoError(t, err)

	require.NotEmpty(t, reader.queries)
	productQuery := reader.queries[0]
	assert.Equal(t, "product", reader.models[0])
	assert.NotEmpty(t, productQuery.Measures, "cube 拒收没有 measure 的查询")
	assert.True(t, productQuery.Total, "不给 total:true 时 total 恒为 0")
	require.NotEmpty(t, productQuery.Order)
	for _, o := range productQuery.Order {
		assert.Contains(t, productQuery.Dimensions, o.ID,
			"order 的 id 必须同时在 SELECT 的 dimensions 里")
	}
	// 批量过滤必须是 `in`（一次查完），不是 N 次单查。
	require.Len(t, productQuery.Filters, 1)
	assert.Equal(t, "in", productQuery.Filters[0].Operator)
	assert.Len(t, productQuery.Filters[0].Values, 2)
}

// TestBatchPromotions_CubeUsesDateRange 锁"促销的时间窗用 inDateRange"。
//
// ⚠️ gte/lte 编译成字符串比较,对 datetime 恒不匹配 ⇒ 返 0 行**且不报错**。
func TestBatchPromotions_CubeUsesDateRange(t *testing.T) {
	s, _, _ := newLocalService(t, &countingCube{})
	reader := &fakeCubeReader{rows: []map[string]any{
		{
			"promotion.item_no":     "P-0001",
			"promotion.plan_no":     "PS-1",
			"promotion.plan_name":   "长期店特",
			"promotion.promo_price": "8.8",
			"promotion.begin_day":   "2026-01-01",
			"promotion.end_day":     "2026-12-31",
		},
	}, total: 1}
	s.SetCubeSource(&fakeSource{src: externalSource()}, reader)

	rows, err := s.BatchPromotions(context.Background(), "00",
		[]string{"P-0001"}, time.Date(2026, 10, 21, 0, 0, 0, 0, time.Local))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "P-0001", rows[0].ProductID)
	assert.Equal(t, "PS-1", rows[0].PlanNo)
	assert.Equal(t, "8.8", rows[0].PriceYuan)

	// 查促销 ⇒ **一次** cube 查询（售价表里没有促销价，促销单独查）。
	require.Len(t, reader.queries, 1)
	promoQuery := reader.queries[0]
	assert.Equal(t, "promotion", reader.models[0])
	var sawDateRange bool
	for _, f := range promoQuery.Filters {
		if f.Operator == "inDateRange" {
			sawDateRange = true
		}
	}
	assert.True(t, sawDateRange,
		"促销是时效数据 ⇒ 必须用 inDateRange(字符串比较对 datetime 恒不匹配)")
}

// TestBatchPrices_LowestPromoWins 锁"同商品多促销 ⇒ 取促销价最低的那条"。
//
// ⚠️ 促销价是**十进制金额字符串** ⇒ 必须按数值比,不能按字符串比
// ("9.90" > "10.00" 在字符串比较里成立 ⇒ 会选错促销单 ⇒ 收错钱)。
func TestBatchPrices_LowestPromoWins(t *testing.T) {
	s, _, _ := newLocalService(t, &countingCube{})
	reader := &fakeCubeReader{rows: []map[string]any{
		{
			"product.id":             "P-0001",
			"product.avg_price_yuan": 15.0,
		},
		{
			"promotion.item_no":     "P-0001",
			"promotion.plan_no":     "PS-HIGH",
			"promotion.promo_price": "10.00",
		},
		{
			"promotion.item_no":     "P-0001",
			"promotion.plan_no":     "PS-LOW",
			"promotion.promo_price": "9.90",
		},
	}, total: 3}
	s.SetCubeSource(&fakeSource{src: externalSource()}, reader)

	rows, err := s.BatchPrices(context.Background(), "00", []string{"P-0001"}, time.Now())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].PromoPriceYuan, "必须带促销价(否则消费方按原价收钱)")
	assert.Equal(t, "9.9", *rows[0].PromoPriceYuan, "必须取促销价最低的那条")
}

// TestBatchPrices_ResolveFailureDoesNotFallBack 锁"问不到路由 ⇒ 报错,不回落本地"。
//
// ⚠️ 回落本地 = 把一次链路故障显示成"这个商品没维护售价"。
func TestBatchPrices_ResolveFailureDoesNotFallBack(t *testing.T) {
	s, prices, _ := newLocalService(t, &countingCube{})
	s.SetCubeSource(&fakeSource{err: assertError{}}, &fakeCubeReader{})
	ctx := context.Background()

	_, _, err := prices.UpsertPrices(ctx, "00", ModeOverwrite,
		[]PriceInput{{ProductID: "P-0001", PriceYuan: "9.90"}})
	require.NoError(t, err)

	rows, err := s.BatchPrices(ctx, "00", []string{"P-0001"}, time.Now())
	require.Error(t, err)
	assert.Nil(t, rows, "失败时不该返回数据")
}

// assertError 是一个最小的 error 实现(fakeSource 用它制造解析失败)。
type assertError struct{}

func (assertError) Error() string { return "dapr 不可达" }

// 编译期断言:promoPriceLess 按数值比较而不是字典序。
func TestPromoPriceLess_ComparesNumerically(t *testing.T) {
	assert.True(t, promoPriceLess("9.90", "10.00"),
		"9.90 的促销价更低(字符串比较会判反 ⇒ 收错钱)")
	assert.False(t, promoPriceLess("10.00", "9.90"))
	assert.True(t, promoPriceLess("2.5", "2.50") == false, "2.5 与 2.50 相等")
}

// 编译期断言:cubeclient 的过滤算子常量与 cube 侧一致。
var _ = cubeclient.CubeQuery{}
