package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/archivedto"
	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/inventory/model"
	"github.com/YunBright/supertrade/internal/inventory/service"
)

// fakeExporter 替身 ArchiveExporter(转发 cube-router 的 /v1/export)。
type fakeExporter struct {
	// body 是被调方返回的响应体(**原样**)。
	body []byte
	// err 是转发错误。
	err error
	// in 记录最后一次转发的入参。
	in service.ExportInput
	// calls 记录转发次数。
	calls int
}

func (f *fakeExporter) ExportRaw(_ context.Context, in service.ExportInput) ([]byte, error) {
	f.calls++
	f.in = in
	if f.err != nil {
		return nil, f.err
	}
	return f.body, nil
}

// cubeRouterBody 造一份 cube-router 的导出响应体(archivedto 契约 + as_of)。
func cubeRouterBody(rows int) []byte {
	page := map[string]any{
		"scope":       archivedto.ScopeStock,
		"branch_id":   "00",
		"cube_source": "cube-sixun-ysx-fb",
		"as_of":       "2026-10-21",
		"rows":        []map[string]any{},
		"count":       rows,
		"total":       rows,
		"offset":      0,
		"limit":       5000,
	}
	raw, _ := json.Marshal(page)
	return raw
}

// TestExportStock_LocalBranch 锁"没配数据源 ⇒ 从本域 stock_levels 导出"。
func TestExportStock_LocalBranch(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)
	seedLevels(t, svc, "10", "P001", "P002")

	exp := &fakeExporter{body: cubeRouterBody(0)}
	svc.SetCubeSource(&fakeSource{src: localSource()}, &fakeCubeReader{})
	svc.SetArchiveExporter(exp)

	raw, err := svc.ExportStock(context.Background(), service.ExportInput{
		BranchID: "10", Scope: archivedto.ScopeStock, Offset: 0, Limit: 5000,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, exp.calls, "没配数据源 ⇒ 不该转发给 cube-router")

	var page struct {
		Scope   string           `json:"scope"`
		Branch  string           `json:"branch_id"`
		Rows    []map[string]any `json:"rows"`
		Count   int              `json:"count"`
		Total   int              `json:"total"`
		Offset  int              `json:"offset"`
		Limit   int              `json:"limit"`
		CubeSrc string           `json:"cube_source"`
		AsOf    string           `json:"as_of"`
	}
	require.NoError(t, json.Unmarshal(raw, &page))
	assert.Equal(t, archivedto.ScopeStock, page.Scope)
	assert.Equal(t, "10", page.Branch)
	assert.Equal(t, 2, page.Count)
	assert.Equal(t, 2, page.Total)
	assert.Empty(t, page.CubeSrc, "本域导出没有 cube 实例")
	require.Len(t, page.Rows, 2)

	// ⚠️ **数量与金额一律字符串**(archivedto 契约:JSON number 到 Dart 侧会变 double)。
	qty, isString := page.Rows[0]["qty"].(string)
	assert.True(t, isString, "qty 在 JSON 里应是字符串,实际 %T", page.Rows[0]["qty"])
	assert.NotEmpty(t, qty)
	_, isString = page.Rows[0]["avg_cost_yuan"].(string)
	assert.True(t, isString, "avg_cost_yuan 在 JSON 里应是字符串")
	assert.Equal(t, "P001", page.Rows[0]["product_id"])
}

// TestExportStock_ExternalBranchForwardsToCubeRouter 锁"有源 ⇒ 转发 cube-router(body 原样)"。
func TestExportStock_ExternalBranchForwardsToCubeRouter(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)
	// 本域里**有**这个门店的行 ⇒ 回落本地就会导出一份"没人看的库存"。
	seedLevels(t, svc, "00", "P001")

	body := cubeRouterBody(1)
	exp := &fakeExporter{body: body}
	svc.SetCubeSource(&fakeSource{src: externalSource()}, &fakeCubeReader{})
	svc.SetArchiveExporter(exp)

	raw, err := svc.ExportStock(context.Background(), service.ExportInput{
		BranchID: "00", Scope: archivedto.ScopeStock, Offset: 100, Limit: 50,
		AsOf:   "2026-10-21",
		Bearer: "Bearer token-abc",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, exp.calls, "有源 ⇒ 必须转发给 cube-router")
	assert.Equal(t, archivedto.ScopeStock, exp.in.Scope)
	assert.Equal(t, 100, exp.in.Offset)
	assert.Equal(t, 50, exp.in.Limit)
	assert.Equal(t, "Bearer token-abc", exp.in.Bearer, "JWT 必须透传(cube-router 要用它解析 per-branch scope)")
	assert.Equal(t, "00", exp.in.BranchID)
	assert.JSONEq(t, string(body), string(raw), "响应体必须**原样**透传")
}

// TestExportStock_ExternalBranchWithoutExporterDoesNotFallBackToLocal 锁"装配漏了转发器 ⇒ 报错"。
func TestExportStock_ExternalBranchWithoutExporterDoesNotFallBackToLocal(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)
	seedLevels(t, svc, "00", "P001")

	svc.SetCubeSource(&fakeSource{src: externalSource()}, &fakeCubeReader{})
	// ⚠️ **不注入**转发器。

	_, err := svc.ExportStock(context.Background(), service.ExportInput{
		BranchID: "00", Scope: archivedto.ScopeStock,
	})
	require.Error(t, err, "有源却导不出 ⇒ 必须报错,不许回落本地导出一份假数据")
	assert.ErrorIs(t, err, model.ErrCubeSourceUnavailable)
}

// TestExportStock_UnsupportedScope 锁"scope 不归本服务 ⇒ 400"。
func TestExportStock_UnsupportedScope(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)
	svc.SetCubeSource(&fakeSource{src: localSource()}, &fakeCubeReader{})

	_, err := svc.ExportStock(context.Background(), service.ExportInput{
		BranchID: "10", Scope: archivedto.ScopeProducts,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrUnsupportedExportScope)
}

// TestExportStock_SourceResolverFailure 锁"问不到路由 ⇒ 503(不回落本地)"。
func TestExportStock_SourceResolverFailure(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)
	seedLevels(t, svc, "10", "P001")

	exp := &fakeExporter{body: cubeRouterBody(0)}
	svc.SetCubeSource(&fakeSource{err: errors.New("cube-router 不可达")}, &fakeCubeReader{})
	svc.SetArchiveExporter(exp)

	_, err := svc.ExportStock(context.Background(), service.ExportInput{
		BranchID: "10", Scope: archivedto.ScopeStock,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCubeSourceUnavailable)
	assert.Equal(t, 0, exp.calls)
}

// TestExportStock_SourceDisabled 锁"数据源被停用 ⇒ 503"。
func TestExportStock_SourceDisabled(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)

	disabled := externalSource()
	disabled.Enabled = false
	svc.SetCubeSource(&fakeSource{src: disabled}, &fakeCubeReader{})

	_, err := svc.ExportStock(context.Background(), service.ExportInput{
		BranchID: "00", Scope: archivedto.ScopeStock,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCubeSourceDisabled)
}

// TestParseExportPaging 锁"导出分页参数的兜底"。
func TestParseExportPaging(t *testing.T) {
	off, lim := service.ParseExportPaging("10", "100")
	assert.Equal(t, 10, off)
	assert.Equal(t, 100, lim)

	// 非法 / 缺省 ⇒ 兜底(不报错,导出是"拉全量"的循环)。
	off, lim = service.ParseExportPaging("abc", "0")
	assert.Equal(t, 0, off)
	assert.Equal(t, service.ExportDefaultLimit, lim)

	// 负 offset ⇒ 0;超上限 ⇒ 截断。
	off, lim = service.ParseExportPaging("-5", "999999")
	assert.Equal(t, 0, off)
	assert.Equal(t, service.ExportMaxLimit, lim)
}

// ---- cubesource.Source 的形状(防止判据被改坏) ----

// TestExternalSourceShape 锁"外部源的三要素"。
func TestExternalSourceShape(t *testing.T) {
	src := cubesource.Source{BranchID: "00", Configured: true, Enabled: true, CubeSourceName: "cube-sixun-ysx-fb"}
	assert.True(t, src.Configured)
	assert.True(t, src.Enabled)
	assert.Equal(t, "cube-sixun-ysx-fb", src.CubeSourceName)
}
