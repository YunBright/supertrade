// 本文件锁住「图7 的 11 个销售报表」的分流、菜单与过滤口径。
//
// # 为什么要有这组用例
//
// 1. **11 个 code 必须都实现**:图7 的左侧菜单项一个都不能少
//    (前端按 /reports/sales/menu-items 渲染菜单,少一个 = 少一个报表)。
// 2. 三态分流:问不到 ⇒ 503,**不许**回落成本地
//    (回落 = 把一次链路故障显示成"这家店这段时间没生意")。
// 3. 过滤真的生效:断言命中数变化,而不是"200 + 有字段"。
// 4. **costed_rows 必须出现在汇总里**:成本是估算值,
//    没有覆盖率,调用方会把"成本全为 0"当成"毛利率 100%"。
package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cuberead/cubereadfake"
	"github.com/YunBright/supertrade/internal/cubesource"
)

// reportFakeSource 替身 cubesource.Resolver(三态)。
type reportFakeSource struct {
	src cubesource.Source
	err error
}

func (f *reportFakeSource) Resolve(_ context.Context, _ string) (cubesource.Source, error) {
	if f.err != nil {
		return cubesource.Source{}, f.err
	}
	return f.src, nil
}

// newReportSvc 构造报表服务(解析器 + cube 读端)。
func newReportSvc(t *testing.T, src cubesource.Resolver) (*ReportsService, *cubereadfake.Client) {
	t.Helper()
	reader := cubereadfake.New()
	reader.Handler = func(m string, q cubeclient.CubeQuery) ([]map[string]any, int, error) {
		switch m {
		case "sales_report":
			// ⚠️ 按查询里的商品过滤(模拟 cube 的 filter 语义)——
			// 否则"按一个不存在的商品过滤"也会返回行,过滤收窄的断言就废了。
			for _, f := range q.Filters {
				if f.Member != "sales_report.item_no" {
					continue
				}
				want := ""
				if len(f.Values) > 0 {
					want, _ = f.Values[0].(string)
				}
				if want != "" && want != "00001" {
					return nil, 0, nil
				}
			}
			rows := []map[string]any{
				{
					"sales_report.item_no":     "00001",
					"sales_report.oper_date":   "2026-10-22",
					"sales_report.supcust_no":  "084",
					"sales_report.total_net_amount": "52.80",
					"sales_report.total_net_qty":    "11.09",
					"sales_report.total_ref_cost":   "30.00",
					"sales_report.costed_rows":      "1",
					"sales_report.count":            "1",
					"item.name":                     "茄子",
					"item.unit":                     "kg",
					"item.brand_name":               "PM",
					"item.clsno":                    "010201",
					"item_cls.id":                   "010201",
					"item_cls.name":                 "叶菜类",
					"item_cls_parent.id":            "01",
					"item_cls_parent.name":          "生鲜联营",
				},
			}
			return rows, len(rows), nil
		case "sale_day":
			rows := []map[string]any{
				{
					"sale_day.branch_id":              "0001",
					"sale_day.product_id":             "00001",
					"sale_day.business_date":          "2026-10-22",
					"sale_day.net_sale_amount_yuan":   "52.80",
					"sale_day.net_sale_quantity":      "11.09",
					"sale_day.total_cost_yuan":        "30.00",
					"sale_day.count":                  "1",
				},
			}
			return rows, len(rows), nil
		case "product":
			return []map[string]any{
				{"product.id": "00001", "product.name": "茄子", "product.unit": "kg",
					"product.category_id": "010201"},
			}, 1, nil
		case "category":
			return []map[string]any{
				{"category.id": "010201", "category.name": "叶菜类", "category.parent_id": "01"},
				{"category.id": "01", "category.name": "生鲜联营", "category.parent_id": ""},
			}, 2, nil
		case "supplier":
			return []map[string]any{
				{"supplier.id": "084", "supplier.name": "水果(自营)"},
			}, 1, nil
		case "sale_detail":
			return []map[string]any{
				{"sale_detail.product_id": "00001"},
			}, 1, nil
		}
		return nil, 0, nil
	}
	svc := NewReportsService(reader, nil)
	svc.SetSourceResolver(src)
	return svc, reader
}

// ---- 菜单 ----

// TestReportMenu_HasAllElevenCodes 锁"11 个 code 一个都不能少"。
//
// ⚠️ 图7 的左侧菜单是**全部实现**的承诺(PLAN §0 的 E 项)。
// 少一个 code = 少一个报表,而菜单是前端按本接口渲染的。
func TestReportMenu_HasAllElevenCodes(t *testing.T) {
	menu := Menu()
	require.Len(t, menu.Items, 11)
	want := []string{
		ReportProductBarcodeGroup, ReportProductPriceGroup, ReportProductBranch,
		ReportCategoryBranch, ReportBranch, ReportLocation, ReportCategoryLarge,
		ReportCategory, ReportDailyAverage, ReportProductDetail, ReportSupplier,
	}
	got := map[string]bool{}
	for _, it := range menu.Items {
		got[it.Code] = true
		assert.NotEmpty(t, it.Label, "菜单项必须有中文标签(前端不硬编码文案)")
	}
	for _, c := range want {
		assert.True(t, got[c], "缺少菜单项 %s", c)
	}
}

// ---- 三态 ----

func TestReports_ConfiguredSourceReadsCube(t *testing.T) {
	src := &reportFakeSource{src: cubesource.Source{
		BranchID: "00", Configured: true, Enabled: true,
	}}
	svc, _ := newReportSvc(t, src)

	rep, err := svc.Run(context.Background(), "00", ReportQuery{
		Code: ReportProductDetail, From: "2026-10-22", To: "2026-10-22", Page: 1, PageSize: 100,
	})
	require.NoError(t, err)
	require.Len(t, rep.Rows, 1)
	assert.Equal(t, "cube", rep.DataSource)
	assert.Equal(t, "52.80", rep.Rows[0]["net_amount_yuan"])
	// ⚠️ 成本是估算值 ⇒ 汇总里**必须**带覆盖率。
	require.NotNil(t, rep.Summary.CostedRows)
	require.NotNil(t, rep.Summary.TotalRows)
	assert.NotEmpty(t, rep.Summary.CostCoverage)
}

func TestReports_NoSourceReadsLocal(t *testing.T) {
	src := &reportFakeSource{src: cubesource.Source{BranchID: "10"}}
	svc, _ := newReportSvc(t, src)

	rep, err := svc.Run(context.Background(), "10", ReportQuery{
		Code: ReportProductDetail, Page: 1, PageSize: 100,
	})
	require.NoError(t, err)
	// ⚠️ 本地路径**明确**返回"没有数据源",而不是给出与 cube 口径不同的数字。
	assert.Equal(t, "local_unavailable", rep.DataSource)
	assert.Empty(t, rep.Rows)
	// 形状仍然与 cube 路径一致(同一个壳)。
	assert.NotEmpty(t, rep.Columns)
}

func TestReports_ResolveFailureIsNotEmptyList(t *testing.T) {
	src := &reportFakeSource{err: errors.New("cube-router 不可达")}
	svc, _ := newReportSvc(t, src)

	_, err := svc.Run(context.Background(), "10", ReportQuery{
		Code: ReportProductDetail, Page: 1, PageSize: 100,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrReportSourceUnavailable)
}

func TestReports_SourceDisabledIsNotLocalFallback(t *testing.T) {
	src := &reportFakeSource{src: cubesource.Source{
		BranchID: "00", Configured: true, Enabled: false,
	}}
	svc, _ := newReportSvc(t, src)

	_, err := svc.Run(context.Background(), "00", ReportQuery{
		Code: ReportProductDetail, Page: 1, PageSize: 100,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrReportSourceDisabled)
}

func TestReports_UnknownCodeIsBadRequest(t *testing.T) {
	src := &reportFakeSource{src: cubesource.Source{BranchID: "00", Configured: true, Enabled: true}}
	svc, _ := newReportSvc(t, src)

	_, err := svc.Run(context.Background(), "00", ReportQuery{Code: "nope"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnknownReportCode)
}

// ---- 每个 code 都要能跑出真实行 ----

// TestReports_EveryCodeReturnsRows 锁"11 个 code 都能跑出真实行"。
//
// ⚠️ PLAN §0 的 E 项写的是「左侧 11 个菜单项**全部实现**」。
// 只断言"不报错"不够 —— 必须真的有行。
func TestReports_EveryCodeReturnsRows(t *testing.T) {
	src := &reportFakeSource{src: cubesource.Source{
		BranchID: "00", Configured: true, Enabled: true,
	}}
	svc, _ := newReportSvc(t, src)
	for _, item := range Menu().Items {
		rep, err := svc.Run(context.Background(), "00", ReportQuery{
			Code: item.Code, From: "2026-10-22", To: "2026-10-22", Page: 1, PageSize: 100,
		})
		require.NoError(t, err, "code=%s", item.Code)
		assert.NotEmpty(t, rep.Columns, "code=%s 必须有列描述", item.Code)
		assert.NotEmpty(t, rep.Rows, "code=%s 必须返回真实行", item.Code)
		assert.Equal(t, item.Code, rep.Code)
	}
}

// ---- 过滤真的生效 ----

// TestReports_FilterNarrowsResult 锁"过滤真的收窄命中数"。
//
// ⚠️ 只断言"200 + 有字段"**抓不到**一个被静默忽略的过滤器。
func TestReports_FilterNarrowsResult(t *testing.T) {
	src := &reportFakeSource{src: cubesource.Source{
		BranchID: "00", Configured: true, Enabled: true,
	}}
	svc, _ := newReportSvc(t, src)

	all, err := svc.Run(context.Background(), "00", ReportQuery{
		Code: ReportProductDetail, From: "2026-10-22", To: "2026-10-22", Page: 1, PageSize: 100,
	})
	require.NoError(t, err)
	require.Len(t, all.Rows, 1)

	filtered, err := svc.Run(context.Background(), "00", ReportQuery{
		Code: ReportProductDetail, From: "2026-10-22", To: "2026-10-22",
		ProductID: "00001", Page: 1, PageSize: 100,
	})
	require.NoError(t, err)
	require.Len(t, filtered.Rows, 1, "命中同一件商品")

	// 不存在的商品 ⇒ 命中 0 行(**不是**"不过滤")。
	none, err := svc.Run(context.Background(), "00", ReportQuery{
		Code: ReportProductDetail, From: "2026-10-22", To: "2026-10-22",
		ProductID: "NOPE", Page: 1, PageSize: 100,
	})
	require.NoError(t, err)
	assert.Empty(t, none.Rows)
}

// TestReports_CubeQueryCarriesFilters 锁"过滤条件真的进了 query"。
func TestReports_CubeQueryCarriesFilters(t *testing.T) {
	src := &reportFakeSource{src: cubesource.Source{
		BranchID: "00", Configured: true, Enabled: true,
	}}
	svc, reader := newReportSvc(t, src)

	_, err := svc.Run(context.Background(), "00", ReportQuery{
		Code: ReportProductDetail, From: "2026-10-22", To: "2026-10-22",
		BrandName: "PM", SupplierID: "084", Page: 1, PageSize: 100,
	})
	require.NoError(t, err)
	q, ok := reader.LastQuery("sales_report")
	require.True(t, ok)
	assert.True(t, q.HasFilter("sales_report.oper_date"), "日期区间必须进 query")
	assert.True(t, q.HasFilter("item.brand_name"), "品牌过滤必须进 query")
	assert.True(t, q.HasFilter("sales_report.supcust_no"), "供应商过滤必须进 query")
}
