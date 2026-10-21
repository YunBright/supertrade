package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/inventory/model"
	"github.com/YunBright/supertrade/internal/inventory/service"
)

// seedLevels 造几条库存快照。
func seedLevels(t *testing.T, svc *service.StockService, branchID string, productIDs ...string) {
	t.Helper()
	ctx := context.Background()
	for _, pid := range productIDs {
		p := salePaidPayload("op-"+pid, line("L1", pid, "1", "10.00", "10.00"))
		p.BranchID = branchID
		_, err := svc.ApplySalePaid(ctx, p)
		require.NoError(t, err)
	}
}

func TestStockService_ListLevels_Pagination(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)
	seedLevels(t, svc, "10", "P001", "P002", "P003", "P004", "P005")

	rows, total, err := svc.ListLevels(context.Background(), service.StockLevelQuery{
		BranchID: "10", Page: 1, PageSize: 2,
	})
	require.NoError(t, err)
	assert.Len(t, rows, 2, "第 1 页应只有 2 条")
	assert.Equal(t, int64(5), total, "total 是过滤后的总条数")

	rows, total, err = svc.ListLevels(context.Background(), service.StockLevelQuery{
		BranchID: "10", Page: 3, PageSize: 2,
	})
	require.NoError(t, err)
	assert.Len(t, rows, 1, "第 3 页应只有 1 条")
	assert.Equal(t, int64(5), total)

	// 越界参数被兜底(而不是算成负 offset)。
	rows, _, err = svc.ListLevels(context.Background(), service.StockLevelQuery{
		BranchID: "10", Page: 0, PageSize: 0,
	})
	require.NoError(t, err)
	assert.Len(t, rows, 5, "page/page_size 越界应回落到默认")
}

func TestStockService_ListLevels_FilterByProductAndKeyword(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)
	seedLevels(t, svc, "10", "P001", "P002", "P003")

	rows, total, err := svc.ListLevels(context.Background(), service.StockLevelQuery{
		BranchID: "10", ProductID: "P002",
	})
	require.NoError(t, err)
	assert.Len(t, rows, 1)
	assert.Equal(t, int64(1), total)
	assert.Equal(t, "P002", rows[0].ProductID)

	rows, _, err = svc.ListLevels(context.Background(), service.StockLevelQuery{
		BranchID: "10", Keyword: "P00",
	})
	require.NoError(t, err)
	assert.Len(t, rows, 3, "keyword 应模糊命中 product_id")
}

func TestStockService_ListLevels_BranchIsolation(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)
	seedLevels(t, svc, "10", "P001")
	seedLevels(t, svc, "20", "P001", "P002")

	rows, _, err := svc.ListLevels(context.Background(), service.StockLevelQuery{BranchID: "10"})
	require.NoError(t, err)
	assert.Len(t, rows, 1, "门店 10 只应看到自己的 1 条")
}

func TestStockService_ListLevels_BranchRequired(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)

	_, _, err := svc.ListLevels(context.Background(), service.StockLevelQuery{})
	assert.ErrorIs(t, err, model.ErrInvalidInput)
}

func TestStockService_GetLevel_NotFound(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)

	_, err := svc.GetLevel(context.Background(), "10", "NOPE")
	assert.ErrorIs(t, err, model.ErrStockLevelNotFound)
}

func TestStockService_ListMoves_Filters(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewStockService(db)
	ctx := context.Background()

	seedLevels(t, svc, "10", "P001", "P002")

	rows, total, err := svc.ListMoves(ctx, service.StockMoveQuery{
		BranchID: "10", ProductID: "P001",
	})
	require.NoError(t, err)
	assert.Len(t, rows, 1)
	assert.Equal(t, int64(1), total)

	rows, _, err = svc.ListMoves(ctx, service.StockMoveQuery{
		BranchID: "10", MoveType: "sale",
	})
	require.NoError(t, err)
	assert.Len(t, rows, 2, "两条都是 sale")

	rows, _, err = svc.ListMoves(ctx, service.StockMoveQuery{
		BranchID: "10", MoveType: "refund",
	})
	require.NoError(t, err)
	assert.Len(t, rows, 0, "没有退货流水")

	// 流水按商品隔离。
	rows, _, err = svc.ListMoves(ctx, service.StockMoveQuery{
		BranchID: "20", ProductID: "P001",
	})
	require.NoError(t, err)
	assert.Len(t, rows, 0, "门店 20 没有流水")
}
