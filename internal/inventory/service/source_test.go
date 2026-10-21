package service_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/inventory/model"
	"github.com/YunBright/supertrade/internal/inventory/service"
)

// ---- 测试替身 ----

// fakeSource 替身 SourceResolver(cubesource.Resolver)。
type fakeSource struct {
	src cubesource.Source
	err error
	// calls 记录解析次数。
	calls int
}

func (f *fakeSource) Resolve(_ context.Context, _ string) (cubesource.Source, error) {
	f.calls++
	if f.err != nil {
		return cubesource.Source{}, f.err
	}
	return f.src, nil
}

// fakeCubeReader 替身 CubeStockReader。
type fakeCubeReader struct {
	rows  []map[string]any
	total int
	err   error
	// queries 记录每次 cube 查询(用来断言 measure / dimension / filter / order / 分页)。
	queries []cubeclient.CubeQuery
	// model 记录最近一次查询的 model 名。
	model string
	// stock 是 GetStock 的返回值(nil ⇒ 返"没有这条库存")。
	stock *cubeclient.StockSnapshotDTO
	// stockErr 是 GetStock 的错误。
	stockErr error
	// stockCalls 记录 GetStock 被调了几次。
	stockCalls int
}

func (f *fakeCubeReader) LoadCubeQueryPage(_ context.Context, modelName string, q cubeclient.CubeQuery) ([]map[string]any, int, error) {
	f.model = modelName
	f.queries = append(f.queries, q)
	if f.err != nil {
		return nil, 0, f.err
	}
	return f.rows, f.total, nil
}

func (f *fakeCubeReader) GetStock(_ context.Context, branchID, productID string) (*cubeclient.StockSnapshotDTO, error) {
	f.stockCalls++
	if f.stockErr != nil {
		return nil, f.stockErr
	}
	if f.stock == nil {
		return nil, fmt.Errorf("%w: branch_id=%s product_id=%s",
			cubeclient.ErrStockNotFound, branchID, productID)
	}
	return f.stock, nil
}

// localSource 是"没配数据源"的门店(自营店 10)。
func localSource() cubesource.Source {
	return cubesource.Source{BranchID: "10"}
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

// ---- 读路径的三态 ----

// TestListLevels_BranchWithCubeSourceReadsFromCube 锁"配了数据源的门店走 cube"。
//
// ⚠️ 这条是本次要修的**症状本体**：不路由 ⇒ 列表恒空
// ⇒ 用户看到「该门店没有库存」，而真相是"库存���外部系统里"。
func TestListLevels_BranchWithCubeSourceReadsFromCube(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)
	// 本域 PG 里**没有**这个门店的行(外部 ERP 的事实源不在本地)。
	seedLevels(t, svc, "10", "P001")

	reader := &fakeCubeReader{
		rows: []map[string]any{
			{"stock.product_id": "6957583900828", "stock.total_quantity": 261.0, "stock.avg_cost": 4.96},
		},
		total: 4128,
	}
	svc.SetCubeSource(&fakeSource{src: externalSource()}, reader)

	rows, total, err := svc.ListLevels(context.Background(), service.StockLevelQuery{
		BranchID: "00", Page: 1, PageSize: 20,
	})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "6957583900828", rows[0].ProductID)
	assert.Equal(t, "00", rows[0].BranchID)
	assert.Equal(t, "261", rows[0].Qty.String())
	assert.Equal(t, int64(4128), total, "total 必须来自 cube(不受 limit 影响)")

	// ⚠️ **查询怎么拼**必须被锁住(cube 的硬约束:measure 必带 / order 的 id 在 SELECT 里 /
	//   offset>0 必须给 order / 要总数必须 total:true)。
	require.Len(t, reader.queries, 1)
	q := reader.queries[0]
	assert.Equal(t, "stock", reader.model)
	assert.Equal(t, []string{"stock.total_quantity", "stock.avg_cost"}, q.Measures,
		"必须带 measure(cube 拒收没有 measure 的查询)")
	assert.Equal(t, []string{"stock.product_id"}, q.Dimensions)
	assert.NotEmpty(t, q.Order, "offset>0 时必须给 order")
	assert.Equal(t, "stock.product_id", q.Order[0].ID, "order 的 id 必须在 SELECT 的 dimensions 里")
	assert.True(t, q.Total, "要总数必须 total:true")
	assert.Equal(t, 20, q.Limit)
	assert.Equal(t, 0, q.Offset)
	assert.NotNil(t, q.Filters, "filters 必须是非 nil 的空切片(否则 cube 收到 null)")
}

// TestListLevels_CubeQueryCarriesFilters 锁"过滤条件原样传给 cube"。
func TestListLevels_CubeQueryCarriesFilters(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)
	reader := &fakeCubeReader{}
	svc.SetCubeSource(&fakeSource{src: externalSource()}, reader)

	_, _, err := svc.ListLevels(context.Background(), service.StockLevelQuery{
		BranchID: "00", ProductID: "P002", Keyword: "P00", Page: 2, PageSize: 10,
	})
	require.NoError(t, err)
	require.Len(t, reader.queries, 1)
	q := reader.queries[0]
	require.Len(t, q.Filters, 2, "product_id 精确过滤 + keyword 模糊过滤")
	assert.Equal(t, "stock.product_id", q.Filters[0].Member)
	assert.Equal(t, cubeclient.OperatorEquals, q.Filters[0].Operator)
	assert.Equal(t, []any{"P002"}, q.Filters[0].Values)
	assert.Equal(t, "stock.product_id", q.Filters[1].Member)
	assert.Equal(t, cubeclient.OperatorContains, q.Filters[1].Operator)
	assert.Equal(t, []any{"P00"}, q.Filters[1].Values)
	assert.Equal(t, 10, q.Offset, "第 2 页 ⇒ offset = (page-1)*page_size")
}

// TestListLevels_BranchWithoutCubeSourceReadsLocal 锁"没配数据源的门店读本域 PG"。
func TestListLevels_BranchWithoutCubeSourceReadsLocal(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)
	seedLevels(t, svc, "10", "P001", "P002")

	reader := &fakeCubeReader{}
	svc.SetCubeSource(&fakeSource{src: localSource()}, reader)

	rows, total, err := svc.ListLevels(context.Background(), service.StockLevelQuery{
		BranchID: "10", Page: 1, PageSize: 20,
	})
	require.NoError(t, err)
	require.Len(t, rows, 2, "必须真的返回本域的行")
	assert.Equal(t, int64(2), total)
	assert.Empty(t, reader.queries, "没配数据源的门店**不该**打 cube")
}

// TestListLevels_SourceResolverFailureIsNotEmptyList 锁"问不到数据源 ⇒ 报错,不是空列表"。
//
// ⚠️ 这是本次修复里最要紧的一条:回落成空列表 = 把一次链路故障
// 显示成「该门店没有库存」—— 根因与结论完全无关。
func TestListLevels_SourceResolverFailureIsNotEmptyList(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)
	seedLevels(t, svc, "10", "P001")

	reader := &fakeCubeReader{}
	svc.SetCubeSource(&fakeSource{err: errors.New("cube-router 不可达")}, reader)

	_, _, err := svc.ListLevels(context.Background(), service.StockLevelQuery{BranchID: "00"})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCubeSourceUnavailable)
	assert.Empty(t, reader.queries, "问不到路由时不该打 cube")
}

// TestListLevels_SourceDisabledIsNotLocalFallback 锁"数据源被停用 ⇒ 503,不是回落本地"。
func TestListLevels_SourceDisabledIsNotLocalFallback(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)
	seedLevels(t, svc, "00", "P001")

	disabled := externalSource()
	disabled.Enabled = false
	reader := &fakeCubeReader{}
	svc.SetCubeSource(&fakeSource{src: disabled}, reader)

	_, _, err := svc.ListLevels(context.Background(), service.StockLevelQuery{BranchID: "00"})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCubeSourceDisabled)
	assert.Empty(t, reader.queries)
}

// TestGetLevel_BranchWithCubeSourceReadsFromCube 锁"单商品库存走 cube"。
func TestGetLevel_BranchWithCubeSourceReadsFromCube(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)

	reader := &fakeCubeReader{stock: &cubeclient.StockSnapshotDTO{
		ProductID:   "P001",
		BranchID:    "00",
		Quantity:    decimal.NewFromInt(261),
		AvgCostYuan: decimal.RequireFromString("4.96"),
	}}
	svc.SetCubeSource(&fakeSource{src: externalSource()}, reader)

	lvl, err := svc.GetLevel(context.Background(), "00", "P001")
	require.NoError(t, err)
	assert.Equal(t, "P001", lvl.ProductID)
	assert.Equal(t, "00", lvl.BranchID)
	assert.Equal(t, "261", lvl.Qty.String())
	assert.Equal(t, "4.96", lvl.AvgCostYuan.String())
	assert.Equal(t, 1, reader.stockCalls)
}

// TestGetLevel_CubeNotFoundIs404 锁"cube 里确实没有这条库存 ⇒ 404(不是 503)"。
func TestGetLevel_CubeNotFoundIs404(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)
	svc.SetCubeSource(&fakeSource{src: externalSource()}, &fakeCubeReader{})

	_, err := svc.GetLevel(context.Background(), "00", "NOPE")
	assert.ErrorIs(t, err, model.ErrStockLevelNotFound)
}

// TestGetLevel_BranchWithoutCubeSourceReadsLocal 锁"没配数据源 ⇒ 读本域 PG"。
func TestGetLevel_BranchWithoutCubeSourceReadsLocal(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)
	seedLevels(t, svc, "10", "P001")

	reader := &fakeCubeReader{stock: &cubeclient.StockSnapshotDTO{ProductID: "P001", Quantity: decimal.NewFromInt(999)}}
	svc.SetCubeSource(&fakeSource{src: localSource()}, reader)

	lvl, err := svc.GetLevel(context.Background(), "10", "P001")
	require.NoError(t, err)
	assert.Equal(t, "P001", lvl.ProductID)
	assert.Equal(t, 0, reader.stockCalls, "没配数据源 ⇒ 不该打 cube")
}

// TestGetLevel_CubeLiveDecimalIsBase64Decoded 锁"思迅 live 源的 base64 数值能被解出来"。
//
// ⚠️ cube 的 stock 是 **live**(MSSQL)源 ⇒ decimal 列经 go-mssqldb 以 []byte 返回
// ⇒ JSON 序列化成 base64 ⇒ 不解码就是"库存恒 0"(一个看起来正常的错数)。
func TestGetLevel_CubeLiveDecimalIsBase64Decoded(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)

	reader := &fakeCubeReader{rows: []map[string]any{{
		"stock.product_id":     "P001",
		"stock.total_quantity": base64.StdEncoding.EncodeToString([]byte("261.0000")),
		"stock.avg_cost":       base64.StdEncoding.EncodeToString([]byte("4.9600")),
	}}}
	svc.SetCubeSource(&fakeSource{src: externalSource()}, reader)

	rows, _, err := svc.ListLevels(context.Background(), service.StockLevelQuery{BranchID: "00"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	// ⚠️ decimal 的 String() 会去掉末尾的 0("261.0000" → "261")。
	assert.Equal(t, "261", rows[0].Qty.String(), "base64 数值必须被解码")
	assert.Equal(t, "4.96", rows[0].AvgCostYuan.String())
}

// TestListMoves_BranchWithCubeSourceDoesNotReadLocal 锁"有源门店的流水**不读本域**"。
//
// ⚠️ cube 的 stock model 是 live 快照，**没有流水** ⇒ 有源门店的流水不在本域。
// 回落读本域 = 把"本域没有这个门店的流水"说成"这个门店没有库存变动"。
func TestListMoves_BranchWithCubeSourceDoesNotReadLocal(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)
	// 本域里**有**这个门店的流水(历史导入留下的)⇒ 回落本地就会把它读出来。
	seedLevels(t, svc, "00", "P001")

	svc.SetCubeSource(&fakeSource{src: externalSource()}, &fakeCubeReader{})

	rows, total, err := svc.ListMoves(context.Background(), service.StockMoveQuery{BranchID: "00"})
	require.NoError(t, err)
	assert.Empty(t, rows, "有源门店不该回落到本域流水")
	assert.Equal(t, int64(0), total)
}

// TestListMoves_BranchWithoutCubeSourceReadsLocal 锁"没配数据源 ⇒ 读本域流水"。
func TestListMoves_BranchWithoutCubeSourceReadsLocal(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)
	seedLevels(t, svc, "10", "P001")

	svc.SetCubeSource(&fakeSource{src: localSource()}, &fakeCubeReader{})

	rows, total, err := svc.ListMoves(context.Background(), service.StockMoveQuery{BranchID: "10"})
	require.NoError(t, err)
	assert.Len(t, rows, 1)
	assert.Equal(t, int64(1), total)
}

// ---- 写端点的只读闸门 ----

// TestEnsureLocalSource 锁"有源门店只读(导入闸门)"。
func TestEnsureLocalSource(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)
	ctx := context.Background()

	// 没配数据源 ⇒ 放行。
	svc.SetCubeSource(&fakeSource{src: localSource()}, &fakeCubeReader{})
	require.NoError(t, svc.EnsureLocalSource(ctx, "10"))

	// 配了数据源 ⇒ 拒(该门店的库存在外部系统里)。
	svc.SetCubeSource(&fakeSource{src: externalSource()}, &fakeCubeReader{})
	err := svc.EnsureLocalSource(ctx, "00")
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrExternalSourceReadOnly)
	// ⚠️ **message 不暴露内部 app-id**。
	assert.NotContains(t, err.Error(), "cube-sixun-ysx-fb")

	// 数据源被停用 ⇒ **仍然只读**(源已经配了 ⇒ 这家店的数据在外部系统里
	// ⇒ 往本域 PG 写一份"没人看的库存"就是制造假数据)。
	// 与 internal/archiveroute 的 DecideImport 同口径。
	disabled := externalSource()
	disabled.Enabled = false
	svc.SetCubeSource(&fakeSource{src: disabled}, &fakeCubeReader{})
	err = svc.EnsureLocalSource(ctx, "00")
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrExternalSourceReadOnly)

	// 问不到路由 ⇒ **放行**(导入路径自己兜底;不能因为路由故障挡住自营店)。
	svc.SetCubeSource(&fakeSource{err: errors.New("cube-router 不可达")}, &fakeCubeReader{})
	require.NoError(t, svc.EnsureLocalSource(ctx, "10"))
}

// TestImportStock_ExternalBranchIsRejected 锁"有源门店的导入被拒(一行都不写)"。
func TestImportStock_ExternalBranchIsRejected(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)
	svc.SetCubeSource(&fakeSource{src: externalSource()}, &fakeCubeReader{})

	_, err := svc.ImportStock(context.Background(), service.StockImportInput{
		SourceBranchID: "00",
		TargetBranchID: "00",
		Mode:           "overwrite",
		Data:           stockRows("P001", "10", "1.0"),
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrExternalSourceReadOnly)

	// ⚠️ 一行都不许写(写进去就是"没人看的库存")。
	var n int64
	require.NoError(t, db.Model(&model.StockLevel{}).Count(&n).Error)
	assert.Equal(t, int64(0), n)
}
