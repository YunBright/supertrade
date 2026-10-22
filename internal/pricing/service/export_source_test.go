// 本文件锁"导出"的两条出口与**同一套判据**。
//
//	GET /archive/export  归档导出(archivedto 契约)
//	GET /prices/export   离线价格包(POS)
//
// ⚠️ 数据来源的判据与查价**完全一致**:配了数据源 ⇒ cube;没配 ⇒ 本域 PG。
//
// ⚠️ **配了源但源里没有 ⇒ 导出空数据,不许回落本地**:
// 回落 = 把"源库没有"说成"本域有旧数据",而真相是"那边没有"。
package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/archivedto"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/pricing/model"
)

// fakeCubeReader 替身 CubeQueryReader。
type fakeCubeReader struct {
	rows  []map[string]any
	total int
	err   error
	// queries 记录每次查询条件(用来断言 measure / order / total / 时间窗确实传下去了)。
	queries []cubeclient.CubeQuery
	// models 记录每次查询的 model 名。
	models []string
}

func (f *fakeCubeReader) LoadCubeQueryPage(_ context.Context, modelName string, q cubeclient.CubeQuery) ([]map[string]any, int, error) {
	f.queries = append(f.queries, q)
	f.models = append(f.models, modelName)
	if f.err != nil {
		return nil, 0, f.err
	}
	return f.rows, f.total, nil
}

// ---- 归档导出 ----

// TestExportArchive_ConfiguredBranchReadsCube 锁"有源 ⇒ 从 cube 取数"。
func TestExportArchive_ConfiguredBranchReadsCube(t *testing.T) {
	s, _, _ := newLocalService(t, &countingCube{})
	reader := &fakeCubeReader{
		rows: []map[string]any{{
			"product.id":              "P-0001",
			"product.avg_price_yuan":  9.9,
			"product.total_cost_yuan": 6.5,
		}},
		total: 1,
	}
	s.SetCubeSource(&fakeSource{src: externalSource()}, reader)

	page, err := s.ExportArchive(context.Background(), "00", archivedto.ScopePrices, 0, 100, "2026-10-21", "")
	require.NoError(t, err)
	assert.Equal(t, archivedto.ScopePrices, page.Scope)
	assert.Equal(t, "00", page.BranchID)
	assert.Equal(t, "cube-sixun-ysx-fb", page.CubeSource)
	require.Len(t, page.Rows, 1)
	assert.Equal(t, 1, page.Count)
	assert.Equal(t, 1, page.Total)

	row := page.Rows[0]
	assert.Equal(t, "P-0001", row["product_id"])
	// ⚠️ 金额是**字符串**(JSON number 到 Dart 侧会变 double,精度当场丢)。
	assert.Equal(t, "9.9", row["price_yuan"])
	assert.Equal(t, "6.5", row["cost_yuan"])

	// ⚠️ cube 的硬约束:必须带 measure、order 的 id 在 SELECT 里、total:true。
	require.Len(t, reader.queries, 1)
	q := reader.queries[0]
	assert.NotEmpty(t, q.Measures, "cube 拒收没有 measure 的查询")
	assert.NotEmpty(t, q.Dimensions)
	assert.True(t, q.Total, "不给 total:true 时 cube 的 total 恒为 0")
	require.NotEmpty(t, q.Order)
	for _, o := range q.Order {
		assert.Contains(t, q.Dimensions, o.ID, "order 的 id 必须同时在 SELECT 的 dimensions 里")
	}
}

// TestExportArchive_UnconfiguredBranchReadsLocal 锁"没源 ⇒ 从本域 PG 取数"。
func TestExportArchive_UnconfiguredBranchReadsLocal(t *testing.T) {
	s, prices, _ := newLocalService(t, &countingCube{})
	reader := &fakeCubeReader{rows: []map[string]any{{"product.id": "P-CUBE"}}, total: 1}
	s.SetCubeSource(&fakeSource{src: localSource()}, reader)
	ctx := context.Background()

	_, _, err := prices.UpsertPrices(ctx, "10", ModeOverwrite,
		[]PriceInput{{ProductID: "P-LOCAL", PriceYuan: "15.00"}})
	require.NoError(t, err)

	page, err := s.ExportArchive(ctx, "10", archivedto.ScopePrices, 0, 100, "2026-10-21", "")
	require.NoError(t, err)
	require.Len(t, page.Rows, 1)
	assert.Equal(t, "P-LOCAL", page.Rows[0]["product_id"], "必须来自本地表")
	assert.Equal(t, "15", page.Rows[0]["price_yuan"])
	assert.Empty(t, reader.queries, "没配数据源时不该查 cube")
}

// TestExportArchive_UnsupportedScope 锁"不支持的资料域 ⇒ 明确报错"。
func TestExportArchive_UnsupportedScope(t *testing.T) {
	s, _, _ := newLocalService(t, &countingCube{})
	s.SetCubeSource(&fakeSource{src: localSource()}, nil)

	_, err := s.ExportArchive(context.Background(), "10", "products", 0, 100, "2026-10-21", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrUnsupportedExportScope)
}

// TestExportArchive_ResolveFailureDoesNotFallBack 锁"问不到路由 ⇒ 报错,不回落本地"。
//
// ⚠️ 回落本地 = 把一次链路故障显示成"该门店没有价格"。
func TestExportArchive_ResolveFailureDoesNotFallBack(t *testing.T) {
	s, prices, _ := newLocalService(t, &countingCube{})
	s.SetCubeSource(&fakeSource{err: errors.New("dapr 不可达")}, nil)
	ctx := context.Background()

	_, _, err := prices.UpsertPrices(ctx, "00", ModeOverwrite,
		[]PriceInput{{ProductID: "P-0001", PriceYuan: "9.90"}})
	require.NoError(t, err)

	page, err := s.ExportArchive(ctx, "00", archivedto.ScopePrices, 0, 100, "2026-10-21", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCubeSourceUnavailable)
	assert.Nil(t, page, "失败时不该返回一页数据")
}

// TestExportArchive_DisabledSourceIsUnavailable 锁"数据源被停用 ⇒ 报错"。
func TestExportArchive_DisabledSourceIsUnavailable(t *testing.T) {
	s, _, _ := newLocalService(t, &countingCube{})
	disabled := externalSource()
	disabled.Enabled = false
	s.SetCubeSource(&fakeSource{src: disabled}, nil)

	_, err := s.ExportArchive(context.Background(), "00", archivedto.ScopePrices, 0, 100, "2026-10-21", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCubeSourceDisabled)
}

// TestExportArchive_PromotionsUseTimeWindow 锁"促销按基准日过滤(inDateRange)"。
//
// ⚠️ 时间窗必须用 inDateRange:gte/lte 是字符串比较,对 datetime 恒不匹配
// ⇒ 返 0 行**且不报错**。
func TestExportArchive_PromotionsUseTimeWindow(t *testing.T) {
	s, _, _ := newLocalService(t, &countingCube{})
	reader := &fakeCubeReader{}
	s.SetCubeSource(&fakeSource{src: externalSource()}, reader)

	_, err := s.ExportArchive(context.Background(), "00", archivedto.ScopePromotions, 0, 100, "2026-10-21", "")
	require.NoError(t, err)
	require.Len(t, reader.queries, 1)
	q := reader.queries[0]
	require.Len(t, q.Filters, 2, "促销是时效数据 ⇒ 必须有两条时间窗过滤")
	for _, f := range q.Filters {
		assert.Equal(t, "inDateRange", f.Operator, "时间窗必须用 inDateRange(字符串比较对 datetime 恒不匹配)")
	}
}

// TestExportArchive_RowKeysFollowContract 锁"导出的列名与 archivedto 契约一致"。
//
// ⚠️ 字段名不一致 ⇒ 导入端**静默丢数据**(JSON 对不认识的字段静默忽略),
// 症状是"导出了 27565 条、导入后价格全是空的",而且**没有一行报错**。
func TestExportArchive_RowKeysFollowContract(t *testing.T) {
	for _, scope := range []string{archivedto.ScopePrices, archivedto.ScopePromotions} {
		t.Run(scope, func(t *testing.T) {
			s, _, _ := newLocalService(t, &countingCube{})
			reader := &fakeCubeReader{
				rows: []map[string]any{{
					"product.id":              "P-0001",
					"product.avg_price_yuan":  9.9,
					"product.total_cost_yuan": 6.5,
					"promotion.item_no":       "P-0001",
					"promotion.plan_no":       "PS-1",
					"promotion.plan_name":     "促销",
					"promotion.promo_price":   "8.8",
					"promotion.orig_price":    "9.9",
					"promotion.begin_day":     "2026-01-01",
					"promotion.end_day":       "2026-12-31",
				}},
				total: 1,
			}
			s.SetCubeSource(&fakeSource{src: externalSource()}, reader)

			page, err := s.ExportArchive(context.Background(), "00", scope, 0, 100, "2026-10-21", "")
			require.NoError(t, err)
			require.Len(t, page.Rows, 1)

			allowed := map[string]bool{}
			for _, col := range archivedto.ExportColumns[scope] {
				allowed[col] = true
			}
			for key := range page.Rows[0] {
				assert.True(t, allowed[key], "列 %q 不在 archivedto.ExportColumns[%s] 里", key, scope)
			}
			// 契约里的列必须**都在**(少一列 = 导入端拿不到那个字段)。
			for col := range allowed {
				assert.Contains(t, page.Rows[0], col, "契约里的列 %q 必须在导出行里", col)
			}
		})
	}
}

// ---- 离线价格包(POS) ----

// TestExportPriceRows_ConfiguredBranchUsesCube 锁"POS 价格包也跟着换源"。
//
// ⚠️ 门店 00 的价会因此**从本地快照切成 cube 实时** —— 值可能变。
// 这是修正(快照是两天前的),不是回归。
func TestExportPriceRows_ConfiguredBranchUsesCube(t *testing.T) {
	s, prices, _ := newLocalService(t, &countingCube{})
	reader := &fakeCubeReader{rows: []map[string]any{
		{"product.id": "P-0001", "product.avg_price_yuan": 77.0},
	}, total: 1}
	s.SetCubeSource(&fakeSource{src: externalSource()}, reader)
	ctx := context.Background()

	// 本地有旧快照价 9.90。
	_, _, err := prices.UpsertPrices(ctx, "00", ModeOverwrite,
		[]PriceInput{{ProductID: "P-0001", PriceYuan: "9.90"}})
	require.NoError(t, err)

	rows, err := s.ExportPriceRows(ctx, "00", time.Now())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "P-0001", rows[0].ProductID)
	assert.Equal(t, "77", rows[0].PriceYuan, "必须取 cube 的实时价")
	assert.Nil(t, rows[0].PromoPriceYuan)
}

// TestExportPriceRows_UnconfiguredBranchUsesLocal 锁"没源 ⇒ 价格包来自本地表"。
func TestExportPriceRows_UnconfiguredBranchUsesLocal(t *testing.T) {
	s, prices, promos := newLocalService(t, &countingCube{})
	reader := &fakeCubeReader{rows: []map[string]any{
		{"product.id": "P-CUBE", "product.avg_price_yuan": 77.0},
	}, total: 1}
	s.SetCubeSource(&fakeSource{src: localSource()}, reader)
	ctx := context.Background()

	_, _, err := prices.UpsertPrices(ctx, "10", ModeOverwrite,
		[]PriceInput{{ProductID: "P-LOCAL", PriceYuan: "15.00"}})
	require.NoError(t, err)
	_, _, err = promos.UpsertPromotions(ctx, "10", ModeOverwrite, "2026-10-21", []PromotionInput{{
		ID: "PS-1", ProductID: "P-LOCAL", PriceYuan: "9.90",
		BeginDay: "2026-01-01", EndDay: "2026-12-31",
	}}, false)
	require.NoError(t, err)

	rows, err := s.ExportPriceRows(ctx, "10", time.Date(2026, 10, 21, 0, 0, 0, 0, time.Local))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "P-LOCAL", rows[0].ProductID)
	assert.Equal(t, "15", rows[0].PriceYuan)
	require.NotNil(t, rows[0].PromoPriceYuan, "必须带促销价(否则消费方按原价收钱)")
	assert.Equal(t, "9.9", *rows[0].PromoPriceYuan)
	assert.Empty(t, reader.queries, "没配数据源时不该查 cube")
}
